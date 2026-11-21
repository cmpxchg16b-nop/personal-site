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
  diagnosticError,
  sessionDescriptionDtlsFingerprints,
  summarizeSessionDescription,
  type PeerDiagnostics,
  type RtcDiagnosticLevel,
} from "./diagnostics";
import { IceRestartController } from "./ice_restart";

const GLARE_RETRY_MS = 2_000;
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

  /** Optional structured console diagnostics owned by the peer session. */
  diagnostics?: PeerDiagnostics;
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
  private pendingOffer?: Promise<MsgId | undefined>;
  private glareRetryTimer?: ReturnType<typeof setTimeout>;
  private iceRestart?: IceRestartController;
  private detachCurrent?: () => void;
  private handlingSignalling = false;
  private readonly retiredRemoteFingerprints = new Set<string>();

  /**
   * @param pc the peer connection to negotiate on. The caller keeps
   *   ownership of it — tracks, `ontrack`, data channels, closing, etc.
   *   PerfectNegotiator only takes over `onnegotiationneeded`,
   *   `onicecandidate`, and `oniceconnectionstatechange` for the duration of
   *   negotiate(). It separately subscribes to `connectionstatechange` with
   *   an event listener so ICE recovery also tracks aggregate DTLS health.
   */
  constructor(
    private pc: RTCPeerConnection,
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

    this.attach();

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
      this.detach();
      this.stopGlareRetryTimer();
      reader.releaseLock();
      writer.releaseLock();
      this.writer = undefined;
    }
  }

  /** Whether the current transport can be swapped without interrupting SDP. */
  public canReplacePeerConnection(): boolean {
    return (
      this.writer !== undefined &&
      !this.makingOffer &&
      !this.handlingSignalling &&
      this.pc.signalingState === "stable"
    );
  }

  /**
   * Adopts a stocked fresh PC for recovery from a terminal DTLS transport.
   * The signalling streams and logical peer session stay in place. The caller
   * closes the old PC after this synchronous, generation-safe swap.
   */
  public replacePeerConnection(
    next: RTCPeerConnection,
    diagnostics?: PeerDiagnostics,
  ): boolean {
    if (!this.canReplacePeerConnection() || next === this.pc) return false;
    for (const description of [
      this.pc.currentRemoteDescription,
      this.pc.remoteDescription,
    ]) {
      for (const fingerprint of sessionDescriptionDtlsFingerprints(
        description,
      )) {
        this.retiredRemoteFingerprints.add(fingerprint);
      }
    }
    this.detach();
    this.pc = next;
    this.options.diagnostics = diagnostics;
    this.pendingOffer = undefined;
    this.ignoreOffer = false;
    this.isSettingRemoteAnswerPending = false;
    this.stopGlareRetryTimer();
    this.attach();
    // Adding tracks/channels before the negotiator attached need not produce a
    // fresh negotiationneeded event. Explicitly start the replacement offer;
    // an event already queued by the browser makes this a no-op in stable only
    // after the first offer has begun.
    queueMicrotask(() => {
      if (
        this.pc === next &&
        next.signalingState === "stable" &&
        !this.makingOffer
      ) {
        next.onnegotiationneeded?.(new Event("negotiationneeded"));
      }
    });
    return true;
  }

  private attach(): void {
    const pc = this.pc;
    const isCurrent = () => this.pc === pc;
    // Local descriptions and trickled ICE candidates carry the identity of
    // their source PC: an old async continuation must not write into its
    // replacement's negotiation.
    pc.onnegotiationneeded = async () => {
      if (!isCurrent()) return;
      try {
        this.makingOffer = true;
        this.log("negotiationneeded");
        await pc.setLocalDescription();
        if (!isCurrent()) return;
        const description = this.localDescription();
        this.logDescription(
          "sdp:local:created",
          description,
          "local description",
        );
        if (description !== undefined) {
          const msgId = await this.sendOffer(description);
          if (!isCurrent()) return;
          this.logDescription("sdp:offer:sent", description, "send offer", {
            msgId: msgId ?? null,
          });
        }
      } catch (err) {
        if (isCurrent()) {
          this.log("sdp:offer:error", { error: diagnosticError(err) }, "error");
        }
      } finally {
        if (isCurrent()) this.makingOffer = false;
      }
    };
    pc.onicecandidate = ({ candidate }) => {
      if (!isCurrent()) return;
      // A null candidate means end-of-candidates; on the wire it is an
      // empty-candidate init, which addIceCandidate() treats the same.
      this.sendToPeer({
        rtcICECandidate: candidate ?? { candidate: "" },
      })
        .then((msgId) => {
          if (isCurrent()) {
            this.log(
              "ice:candidate:sent",
              { msgId: msgId ?? null, endOfCandidates: candidate === null },
              "debug",
            );
          }
        })
        .catch((err) => {
          if (isCurrent()) {
            this.log(
              "ice:candidate:send:error",
              { error: diagnosticError(err) },
              "error",
            );
          }
        });
    };
    const onRestartError = (err: unknown) => {
      if (isCurrent()) {
        this.log("ice:restart:error", { error: diagnosticError(err) }, "error");
      }
    };
    const iceRestart = new IceRestartController(pc, {
      // Only the polite peer originates automated restart offers. Letting both
      // sides offer together would create glare on every shared outage.
      restart: () => {
        if (!isCurrent()) return;
        this.log("ice:restart:execute", {
          mode: this.polite ? "local-offer" : "remote-request",
        });
        if (this.polite) {
          pc.restartIce();
        } else {
          this.sendToPeer({ iceRestartRequest: true })
            .then((msgId) => {
              if (isCurrent()) {
                this.log("ice:restart:request:sent", { msgId: msgId ?? null });
              }
            })
            .catch(onRestartError);
        }
        // restartIce() need not fire negotiationneeded while an offer is
        // unanswered. Retry the actual signalling transaction as well, or a
        // lost offer/answer leaves every later watchdog tick doing nothing.
        void this.retransmitPendingOffer().catch(onRestartError);
      },
      onRestartError,
      onEvent: (event, details) => {
        if (!isCurrent()) return;
        this.log(event, details);
        if (event === "restart:attempt") {
          this.options.diagnostics?.captureStats("restart:attempt");
        }
      },
    });
    this.iceRestart = iceRestart;
    pc.oniceconnectionstatechange = () => {
      if (isCurrent()) iceRestart.handleState(pc.iceConnectionState);
    };
    const onConnectionStateChange = () => {
      if (isCurrent()) iceRestart.handleConnectionState(pc.connectionState);
    };
    pc.addEventListener("connectionstatechange", onConnectionStateChange);
    this.detachCurrent = () => {
      pc.onnegotiationneeded = null;
      pc.onicecandidate = null;
      pc.oniceconnectionstatechange = null;
      pc.removeEventListener("connectionstatechange", onConnectionStateChange);
      iceRestart.close();
      if (this.iceRestart === iceRestart) this.iceRestart = undefined;
    };
    iceRestart.handleState(pc.iceConnectionState);
    iceRestart.handleConnectionState(pc.connectionState);
  }

  private detach(): void {
    const detach = this.detachCurrent;
    this.detachCurrent = undefined;
    detach?.();
  }

  /**
   * Re-sends the current local offer on an ICE retry or replacement signalling
   * transport. A socket may have accepted the original write without delivering it;
   * repeated restartIce() calls need not produce another negotiationneeded
   * while this offer is pending. Reading localDescription now also includes
   * candidates gathered since the original emission.
   */
  public async retransmitPendingOffer(): Promise<void> {
    if (this.pc.signalingState !== "have-local-offer") {
      this.log(
        "sdp:offer:retransmit:skip",
        {
          reason: "no-local-offer",
        },
        "debug",
      );
      return;
    }
    const description = this.localDescription();
    if (description === undefined) return;
    const msgId = await this.sendOffer(description);
    this.logDescription(
      "sdp:offer:retransmitted",
      description,
      "retransmit offer",
      { msgId: msgId ?? null, coalesced: msgId === undefined },
    );
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

    this.handlingSignalling = true;
    try {
      if (c2c.sessionDesc) {
        const description = c2c.sessionDesc;
        const fingerprints = sessionDescriptionDtlsFingerprints(description);
        if (
          fingerprints.some((fingerprint) =>
            this.retiredRemoteFingerprints.has(fingerprint),
          )
        ) {
          // Ordered signalling can still contain an old offer retransmission or
          // its answer after the fresh PC is adopted. Such SDP must never win a
          // fresh-PC glare or complete the fresh offer with retired credentials.
          this.ignoreOffer = description.type === "offer";
          this.logDescription(
            "sdp:stale:ignored",
            description,
            "ignore retired transport description",
            { msgId: ev.msgId },
          );
          return;
        }
        const readyForOffer =
          !this.makingOffer &&
          (this.pc.signalingState === "stable" ||
            this.isSettingRemoteAnswerPending);
        const offerCollision = description.type === "offer" && !readyForOffer;

        this.ignoreOffer = !this.polite && offerCollision;
        if (this.ignoreOffer) {
          this.logDescription(
            "sdp:offer:ignored",
            description,
            "ignore offer",
            { msgId: ev.msgId, collision: true },
          );
          // The polite peer cannot yield until it receives our pending offer.
          // Retry only a glare that stays unresolved: immediate duplicates can
          // themselves keep the polite peer rolling back an ordinary glare.
          this.scheduleGlareRetryTimer();
          return;
        }
        this.logDescription(
          `sdp:${description.type}:apply:start`,
          description,
          `apply remote ${description.type}`,
          {
            msgId: ev.msgId,
            collision: offerCollision,
            makingOffer: this.makingOffer,
            isSettingRemoteAnswerPending: this.isSettingRemoteAnswerPending,
          },
        );
        this.isSettingRemoteAnswerPending = description.type === "answer";
        await this.pc.setRemoteDescription(description);
        this.isSettingRemoteAnswerPending = false;
        this.logDescription(
          `sdp:${description.type}:apply:ok`,
          description,
          `applied remote ${description.type}`,
          { msgId: ev.msgId },
        );
        if (this.pc.signalingState === "stable") {
          this.stopGlareRetryTimer();
        }
        if (description.type === "offer") {
          // setLocalDescription() with no argument answers the offer;
          // for the polite peer on a collision it also implicitly rolls
          // back the offer it was making.
          await this.pc.setLocalDescription();
          const answer = this.localDescription();
          const msgId = await this.sendToPeer({ sessionDesc: answer });
          this.logDescription("sdp:answer:sent", answer, "send answer", {
            msgId: msgId ?? null,
            inReplyTo: ev.msgId,
          });
        }
      } else if (c2c.rtcICECandidate) {
        try {
          await this.pc.addIceCandidate(c2c.rtcICECandidate);
          this.log(
            "ice:candidate:applied",
            {
              msgId: ev.msgId,
              endOfCandidates: c2c.rtcICECandidate.candidate === "",
            },
            "debug",
          );
        } catch (err) {
          // Candidates belonging to an offer we chose to ignore are
          // expected to fail; everything else is a real error.
          if (!this.ignoreOffer) {
            throw err;
          }
          this.log(
            "ice:candidate:ignored",
            { msgId: ev.msgId, error: diagnosticError(err) },
            "debug",
          );
        }
      } else if (c2c.iceRestartRequest && this.polite) {
        this.log("ice:restart:request:received", { msgId: ev.msgId });
        // restartIce() latches through any in-flight negotiation. Also repeat
        // an unanswered offer: the requester's original answer may be the
        // message that was lost, and restartIce() alone need not emit SDP.
        this.pc.restartIce();
        await this.retransmitPendingOffer();
      }
    } catch (err) {
      this.logDescription(
        "signalling:handle:error",
        c2c.sessionDesc,
        "FAILED",
        { msgId: ev.msgId, error: diagnosticError(err) },
        "error",
      );
    } finally {
      this.handlingSignalling = false;
    }
  }

  private scheduleGlareRetryTimer(): void {
    if (
      this.writer === undefined ||
      this.glareRetryTimer !== undefined ||
      this.pc.signalingState !== "have-local-offer"
    ) {
      return;
    }
    this.glareRetryTimer = setTimeout(() => {
      this.glareRetryTimer = undefined;
      void this.retransmitPendingOffer()
        .catch((err) =>
          this.log(
            "glare:retry:error",
            { error: diagnosticError(err) },
            "error",
          ),
        )
        .finally(() => this.scheduleGlareRetryTimer());
    }, GLARE_RETRY_MS);
    this.log("glare:retry:scheduled", { delayMs: GLARE_RETRY_MS });
  }

  private stopGlareRetryTimer(): void {
    if (this.glareRetryTimer !== undefined) {
      clearTimeout(this.glareRetryTimer);
      this.glareRetryTimer = undefined;
      this.log("glare:retry:stopped");
    }
  }

  // sendOffer coalesces retransmission with the current local-offer write.
  // Candidate gathering can change SDP while that write is pending, but those
  // candidates already use trickle. Once the write settles (including on a
  // stale socket), reconnect may retransmit the latest localDescription.
  private async sendOffer(
    description: RTCSessionDescriptionInit,
  ): Promise<MsgId | undefined> {
    if (this.pendingOffer !== undefined) {
      await this.pendingOffer;
      return undefined;
    }
    const write = this.sendToPeer({ sessionDesc: description });
    this.pendingOffer = write;
    try {
      return await write;
    } finally {
      if (this.pendingOffer === write) {
        this.pendingOffer = undefined;
      }
    }
  }

  private localDescription(): RTCSessionDescriptionInit | undefined {
    return this.pc.localDescription ?? undefined;
  }

  private log(
    event: string,
    details: Record<string, unknown> = {},
    level: RtcDiagnosticLevel = "info",
  ): void {
    if (this.options.diagnostics) {
      this.options.diagnostics.log(event, details, level);
    } else {
      console[level](this.tag(), event, details);
    }
  }

  private logDescription(
    event: string,
    description: RTCSessionDescriptionInit | null | undefined,
    fallbackWhat: string,
    details: Record<string, unknown> = {},
    level: RtcDiagnosticLevel = "info",
  ): void {
    if (this.options.diagnostics) {
      this.options.diagnostics.log(
        event,
        {
          ...details,
          description: summarizeSessionDescription(description),
        },
        level,
      );
    } else {
      logSdp(this.tag(), fallbackWhat, description ?? undefined, details);
    }
  }

  /** The log tag identifying this negotiation pair. */
  private tag(): string {
    return `[negotiate ${this.options.selfSubscriber}→${this.options.peerSubscriber}${this.polite ? " polite" : ""}]`;
  }

  /** Wraps a c2c payload into the SS envelope and queues it for sending. */
  private async sendToPeer(
    payload: Pick<
      ClientToClientEv,
      "sessionDesc" | "rtcICECandidate" | "iceRestartRequest"
    >,
  ): Promise<MsgId | undefined> {
    if (!this.writer) {
      return undefined;
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
    return ev.msgId;
  }
}

// newMsgId mints a message id uniquely, statelessly, independently,
// mirroring the server side.
function newMsgId(): MsgId {
  return crypto.randomUUID();
}

// logSdp logs one session description's wire-relevant shape: its type,
// and per m-section the mid and the a=setup role — the attributes a
// "Failed to set SSL role" failure is about.
function logSdp(
  tag: string,
  what: string,
  desc: RTCSessionDescriptionInit | undefined,
  extra?: Record<string, unknown>,
): void {
  if (!desc) {
    console.log(tag, what, "(no description)", extra ?? "");
    return;
  }
  const sections: string[] = [];
  let mid = "?";
  for (const line of desc.sdp?.split("\r\n") ?? []) {
    if (line.startsWith("a=mid:")) {
      mid = line.slice("a=mid:".length);
    } else if (line.startsWith("a=setup:")) {
      sections.push(`mid=${mid} setup=${line.slice("a=setup:".length)}`);
    }
  }
  console.log(tag, what, desc.type, `[${sections.join("; ")}]`, extra ?? "");
}
