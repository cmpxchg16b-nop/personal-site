"use client";

/**
 * WebRTC session negotiation over the signalling server (SS).
 *
 * RTCNegotiator is the contract; PerfectNegotiator implements the MDN
 * "perfect negotiation" pattern
 * (https://developer.mozilla.org/en-US/docs/Web/API/WebRTC_API/Perfect_negotiation)
 * on top of the SS's client-to-client relay (`c2CEv`). The SS is a dumb
 * relay for this traffic: it never looks at session descriptions or ICE
 * candidates, it just passes c2c events between subscribers.
 *
 * Signalling events flow through the native Streams API
 * (https://developer.mozilla.org/en-US/docs/Web/API/Streams_API): the
 * negotiator reads inbound events from a ReadableStream and writes
 * outbound ones to a WritableStream. A transport (e.g. the SSProxy) is
 * expected to provide both; usePeerSessions (see peersessions.tsx)
 * wires one negotiator per peer to it.
 */

import {
  WELLKNOWN_SVC_ID_SS,
  type ChannelId,
  type ClientToClientEv,
  type MsgId,
  type SignallingEvent,
  type SubscriberId,
} from "./types";

/**
 * RTCNegotiator drives the WebRTC offer/answer + ICE exchange between the
 * local peer and one remote peer, using the signalling server as a relay:
 * it reads inbound signalling events from `ssEvIn` and writes outbound
 * ones to `ssEvOut`.
 */
export interface RTCNegotiator {
  /**
   * Runs the negotiation loop. Resolves when `ssEvIn` closes; rejects
   * when either stream errors (e.g. the transport failed).
   *
   * The streams stay owned by the caller: negotiate() takes a reader on
   * `ssEvIn` and a writer on `ssEvOut` and releases both locks when it
   * returns, but never closes or aborts them. Writes go through
   * `WritableStreamDefaultWriter.write()`, so the writable side's
   * queuing strategy applies backpressure naturally.
   */
  negotiate(
    ssEvIn: ReadableStream<SignallingEvent>,
    ssEvOut: WritableStream<SignallingEvent>,
  ): Promise<void>;
}

export interface PerfectNegotiatorOptions {
  /**
   * The channel both subscribers are registered in. Only c2c events in
   * this channel are negotiation traffic, and outbound events are scoped
   * to it.
   */
  channelId: ChannelId;

  /** Own subscriber id on the signalling channel. */
  selfSubscriber: SubscriberId;

  /** Subscriber id of the remote peer to negotiate with. */
  peerSubscriber: SubscriberId;
}

/**
 * PerfectNegotiator implements the perfect negotiation pattern: the same
 * code runs on both ends of a connection, caller or callee, and the only
 * asymmetry is the polite/impolite role, which resolves offer collisions
 * ("glare") without deadlocking. The role is derived deterministically
 * from the two subscriber ids — the lexicographically smaller id is
 * polite — so both ends agree on the roles without any extra signalling.
 * Roles are per pair of peers: one client may be polite towards one peer
 * and impolite towards another.
 *
 * Only c2c events in `options.channelId` from `options.peerSubscriber`
 * addressed to `options.selfSubscriber` are negotiation traffic; every
 * other event on the input stream is ignored.
 *
 * Don't re-use same negotiator instance in the case when at least one
 * subscriber id has been changed.
 */
export class PerfectNegotiator implements RTCNegotiator {
  // Perfect-negotiation state. `makingOffer` is tracked in a boolean
  // rather than derived from pc.signalingState because the signaling
  // state changes asynchronously and would race with an incoming offer.
  private makingOffer = false;
  private ignoreOffer = false;
  private isSettingRemoteAnswerPending = false;

  /**
   * Whether this peer is the polite one — the lexicographically smaller
   * of the two subscriber ids. Both ends compute this locally from the
   * same pair of ids, so they deterministically land on opposite roles.
   */
  public readonly polite: boolean;

  private writer?: WritableStreamDefaultWriter<SignallingEvent>;

  /**
   * @param pc the peer connection to negotiate on. The caller keeps
   *   ownership of it — tracks, `ontrack`, data channels, closing, etc.
   *   PerfectNegotiator only takes over `onnegotiationneeded` and
   *   `onicecandidate` for the duration of negotiate().
   */
  constructor(
    private readonly pc: RTCPeerConnection,
    private readonly options: PerfectNegotiatorOptions,
  ) {
    // Subscriber ids are unique within a channel (the SS rejects
    // duplicate registrations), so the comparison never ties; equal ids
    // here mean a programming error at the call site.
    if (options.selfSubscriber === options.peerSubscriber) {
      throw new Error("PerfectNegotiator: cannot negotiate with oneself");
    }
    this.polite = options.selfSubscriber < options.peerSubscriber;
  }

  public async negotiate(
    this: PerfectNegotiator,
    ssEvIn: ReadableStream<SignallingEvent>,
    ssEvOut: WritableStream<SignallingEvent>,
  ): Promise<void> {
    if (this.writer) {
      throw new Error("PerfectNegotiator.negotiate: already running");
    }
    const writer = ssEvOut.getWriter();
    this.writer = writer;
    const reader = ssEvIn.getReader();

    // Outgoing half of the pattern: local descriptions and trickled ICE
    // candidates are pushed to the peer as WebRTC produces them.
    this.pc.onnegotiationneeded = async () => {
      try {
        this.makingOffer = true;
        await this.pc.setLocalDescription();
        await this.sendToPeer({ sessionDesc: this.localDescription() });
      } catch (err) {
        console.error(err);
      } finally {
        this.makingOffer = false;
      }
    };
    this.pc.onicecandidate = ({ candidate }) => {
      // A null candidate means end-of-candidates; on the wire it is an
      // empty-candidate init, which addIceCandidate() treats the same.
      this.sendToPeer({
        rtcICECandidate: candidate ?? { candidate: "" },
      }).catch(console.error);
    };

    // Incoming half: consume signalling events until the stream closes.
    try {
      for (;;) {
        const { done, value } = await reader.read();
        if (done) {
          return;
        }
        await this.handle(value);
      }
    } finally {
      this.pc.onnegotiationneeded = null;
      this.pc.onicecandidate = null;
      reader.releaseLock();
      writer.releaseLock();
      this.writer = undefined;
    }
  }

  /** Handles one inbound signalling event; non-c2c or foreign-peer events are ignored. */
  private async handle(ev: SignallingEvent): Promise<void> {
    const c2c = ev.c2CEv;
    if (
      !c2c ||
      c2c.channelId !== this.options.channelId ||
      c2c.fromSubscriber !== this.options.peerSubscriber ||
      c2c.toSubscriber !== this.options.selfSubscriber
    ) {
      return;
    }

    try {
      if (c2c.sessionDesc) {
        const description = c2c.sessionDesc;
        const readyForOffer =
          !this.makingOffer &&
          (this.pc.signalingState === "stable" ||
            this.isSettingRemoteAnswerPending);
        const offerCollision = description.type === "offer" && !readyForOffer;

        this.ignoreOffer = !this.polite && offerCollision;
        if (this.ignoreOffer) {
          return;
        }
        this.isSettingRemoteAnswerPending = description.type === "answer";
        await this.pc.setRemoteDescription(description);
        this.isSettingRemoteAnswerPending = false;
        if (description.type === "offer") {
          // setLocalDescription() with no argument answers the offer;
          // for the polite peer on a collision it also implicitly rolls
          // back the offer it was making.
          await this.pc.setLocalDescription();
          await this.sendToPeer({ sessionDesc: this.localDescription() });
        }
      } else if (c2c.rtcICECandidate) {
        try {
          await this.pc.addIceCandidate(c2c.rtcICECandidate);
        } catch (err) {
          // Candidates belonging to an offer we chose to ignore are
          // expected to fail; everything else is a real error.
          if (!this.ignoreOffer) {
            throw err;
          }
        }
      }
    } catch (err) {
      console.error(err);
    }
  }

  private localDescription(): RTCSessionDescriptionInit | undefined {
    return this.pc.localDescription ?? undefined;
  }

  /** Wraps a c2c payload into the SS envelope and queues it for sending. */
  private async sendToPeer(
    payload: Pick<ClientToClientEv, "sessionDesc" | "rtcICECandidate">,
  ): Promise<void> {
    if (!this.writer) {
      return;
    }
    const ev: SignallingEvent = {
      // `from` is left empty: the server-side transport populates it from
      // the authenticated session (cookie / JWT).
      from: {},
      // c2c events are addressed to the SS; it rewrites `to` to the
      // address `toSubscriber` resolves to before relaying.
      to: { serviceId: WELLKNOWN_SVC_ID_SS },
      msgId: newMsgId(),
      c2CEv: {
        fromSubscriber: this.options.selfSubscriber,
        toSubscriber: this.options.peerSubscriber,
        channelId: this.options.channelId,
        ...payload,
      },
    };
    await this.writer.write(ev);
  }
}

// newMsgId mints a message id uniquely, statelessly, independently,
// mirroring the server side.
function newMsgId(): MsgId {
  return crypto.randomUUID();
}
