"use client";

/**
 * Peer sessions between channel members, over the signalling server.
 *
 * usePeerSessions grabs the SSProxy from React context (like
 * useSignalling) and brings up one RTCPeerConnection per fellow member
 * of the current user's channel, driving each with a PerfectNegotiator
 * (see negotiate.ts): SDP offers/answers and trickled ICE candidates
 * ride the SS's client-to-client relay, so once the channel listing
 * shows a peer, negotiation follows automatically. Sessions track the
 * membership: they appear as the listing discovers members and
 * disappear when members drop out. A signalling reconnect keeps the
 * sessions and their media alive; an identity change tears every session
 * down and rebuilds from the current listing.
 *
 * What a session carries is decided by the consumers, by data-channel
 * label: useDataChannel subscribes "dcmsg" (JSON DCMsgs),
 * useBinaryDataChannel subscribes "dcbin" (compact binary frames) —
 * siblings over this one primitive, neither knowing the other. Every
 * subscribed label gets one channel per session, created by the polite
 * peer (the lexicographically smaller subscriber id — the same rule
 * PerfectNegotiator derives the role from) and received via
 * ondatachannel on the other, dispatched by label. All channels ride
 * the one peer connection per pair: a second connection per pair would
 * cross-wire the negotiators, which filter client-to-client signalling
 * by (channelId, from, to) only.
 *
 * Media tracks ride the same connection: consumers add and remove local
 * tracks (addTrack/removeTrack) and subscribe remote track arrivals
 * (subscribeTracks); the PerfectNegotiator renegotiates on its own. SIP-
 * subset call dialogs and chatControl.sip amendments have a separate
 * subscribeCall/sendCall path on the same ordered per-peer WS output;
 * they never enter the negotiator or a DataChannel.
 *
 * The ICE servers for the peer connections come from GET
 * /api/iceServers (the <iceServer/> entries of the server configuration
 * document, matched to this origin) via the useICEServers query; peer
 * sessions are not brought up before that query has settled, so every
 * connection negotiates with the configured servers.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import { useICEServers } from "@/hooks/useICEServers";
import {
  createPeerDiagnostics,
  summarizeSessionDescription,
} from "./diagnostics";
import { sameEndpointIdentity, type EndpointIdentity } from "./identity";
import { PerfectNegotiator } from "./negotiate";
import type { SSProxy } from "./proxy";
import { useSSProxy } from "./react";
import { WELLKNOWN_SVC_ID_SS } from "./types";
import type {
  ChannelId,
  ChatChannel,
  ChatUser,
  ClientToClientCallEv,
  SignallingEvent,
  SubscriberId,
} from "./types";

// peerKey indexes the session of one (channel, peer subscriber) pair.
function peerKey(channelId: ChannelId, subscriberId: SubscriberId): string {
  return `${channelId}:${subscriberId}`;
}

// inboxKey includes the recipient so pre-registration traffic for different
// incarnations cannot share an inbox.
function inboxKey(
  channelId: ChannelId,
  self: SubscriberId,
  peer: SubscriberId,
): string {
  return JSON.stringify([channelId, self, peer]);
}

// PeerInbox decouples a peer's input from the session's lifetime: an early
// offer and membership can create the inbox in either order, and the eventual
// negotiator reads every queued event from that one stream.
interface PeerInbox {
  readonly channelId: ChannelId;
  readonly self: SubscriberId;
  readonly peer: SubscriberId;
  readonly stream: ReadableStream<SignallingEvent>;
  absentListings: number;
  enqueue(event: SignallingEvent): void;
  close(): void;
}

function createPeerInbox(
  channelId: ChannelId,
  self: SubscriberId,
  peer: SubscriberId,
): PeerInbox {
  let controller!: ReadableStreamDefaultController<SignallingEvent>;
  let closed = false;
  const stream = new ReadableStream<SignallingEvent>({
    start: (c) => {
      controller = c;
    },
    cancel: () => {
      closed = true;
    },
  });
  return {
    channelId,
    self,
    peer,
    stream,
    absentListings: 0,
    enqueue(event) {
      if (!closed) controller.enqueue(event);
    },
    close() {
      if (closed) return;
      closed = true;
      controller.close();
    },
  };
}

// PendingCalls holds call events until both a matching subscriber incarnation
// and its peer session exist. The self address is part of the key so events
// accepted before registration cannot leak into a different incarnation.
interface PendingCalls {
  channelId: ChannelId;
  self: SubscriberId;
  peer: SubscriberId;
  events: SignallingEvent[];
  absentListings: number;
}

// A peer's signalling output outlives any one proxy. Writes made while the
// WebSocket is down stay ordered and resolve once a replacement proxy sends
// them, so PerfectNegotiator does not lose SDP or candidates during reconnect.
interface PeerSignallingOutput {
  stream: WritableStream<SignallingEvent>;
  write(event: SignallingEvent): Promise<void>;
  setProxy(proxy: SSProxy | null): void;
  close(): void;
}

function createPeerSignallingOutput(
  initialProxy: SSProxy | null,
): PeerSignallingOutput {
  interface PendingWrite {
    event: SignallingEvent;
    resolve: () => void;
    reject: (err: Error) => void;
  }

  let proxy = initialProxy;
  let closed = false;
  let flushing = false;
  let flushAgain = false;
  let cancelWrite: (() => void) | null = null;
  const pending: PendingWrite[] = [];

  const flush = async () => {
    if (flushing) return;
    flushing = true;
    try {
      while (!closed && proxy !== null && pending.length > 0) {
        const next = pending[0];
        const writer = proxy.getWriteStream().getWriter();
        const cancelled = new Promise<void>((_resolve, reject) => {
          cancelWrite = () =>
            reject(new Error("peersessions: signalling proxy changed"));
        });
        try {
          // A stale network write may never settle. setProxy rejects this race
          // so its replacement is not blocked behind the old transport.
          await Promise.race([writer.write(next.event), cancelled]);
        } catch {
          // Keep the event at the head of the queue. A proxy swap retries it;
          // later writes cannot overtake it.
          break;
        } finally {
          cancelWrite = null;
          writer.releaseLock();
        }
        pending.shift();
        next.resolve();
      }
    } finally {
      flushing = false;
      if (flushAgain && !closed) {
        flushAgain = false;
        void flush();
      }
    }
  };

  const write = (event: SignallingEvent): Promise<void> => {
    if (closed) {
      return Promise.reject(
        new Error("peersessions: signalling output is closed"),
      );
    }
    return new Promise<void>((resolve, reject) => {
      pending.push({ event, resolve, reject });
      void flush();
    });
  };
  const stream = new WritableStream<SignallingEvent>({ write });

  return {
    stream,
    write,
    setProxy(next) {
      if (closed) return;
      const changed = proxy !== next;
      proxy = next;
      if (flushing) {
        if (changed) {
          flushAgain = true;
          cancelWrite?.();
        }
        return;
      }
      void flush();
    },
    close() {
      if (closed) return;
      closed = true;
      proxy = null;
      flushAgain = false;
      cancelWrite?.();
      const err = new Error("peersessions: signalling output is closed");
      for (const write of pending) write.reject(err);
      pending.length = 0;
    },
  };
}

// PeerChannelHandler is called as a session's channel of the subscribed
// label appears — created locally by the polite peer, or received via
// ondatachannel — so the consumer can wire its handlers on it. The
// channel may still be connecting.
export type PeerChannelHandler = (
  channelId: ChannelId,
  peer: SubscriberId,
  dc: RTCDataChannel,
) => void;

// PeerTrackHandler is called when a remote track arrives on a session
// (pc.ontrack), so media consumers — the voice-call feature — can pick
// up the peer's stream. Adding the local track (addTrack) triggers a
// renegotiation automatically: the sessions are PerfectNegotiator-driven.
export type PeerTrackHandler = (
  channelId: ChannelId,
  peer: SubscriberId,
  ev: RTCTrackEvent,
) => void;

// PeerCallHandler receives one call-signalling event before the WebRTC
// negotiator's SDP/ICE demux. Call events never enter the negotiator.
export type PeerCallHandler = (event: SignallingEvent) => void;

// SessionResetHandler is notified when peer sessions go away: `dropped`
// names one closed session, or null when every session was torn down
// (an identity change or the whole hook unmounting).
export type SessionResetHandler = (
  dropped: { channelId: ChannelId; peer: SubscriberId } | null,
) => void;

// PeerConnectionStates is the live connection state of every peer
// session, by channel and peer subscriber id. A session's entry appears
// with its first state change and goes with the session's teardown; a
// missing entry reads as "new".
export type PeerConnectionStates = Record<
  ChannelId,
  Record<SubscriberId, RTCPeerConnectionState>
>;

// PeerSessions is the peer-connection and c2c plumbing the consumers build
// on: one connection per pair, DataChannels by label, and an independent
// WebSocket call-signalling path. The object is stable across renders.
export interface PeerSessions {
  /**
   * The session's channel of `label`, whatever its readyState; null
   * when the session or the channel does not exist (yet).
   */
  getChannel(
    label: string,
    channelId: ChannelId,
    peer: SubscriberId,
  ): RTCDataChannel | null;
  /**
   * Resolves with the session's open channel of `label`, waiting for
   * the session to appear and the channel to open; resolves with null
   * when the session goes away first (the peer dropped out, or every
   * session was torn down). A peer that never appears at all leaves the
   * promise pending until the next teardown.
   */
  whenOpenChannel(
    label: string,
    channelId: ChannelId,
    peer: SubscriberId,
  ): Promise<RTCDataChannel | null>;
  /**
   * Subscribes to the channels of `label`: the callback fires as each
   * session's channel appears — existing sessions included — and every
   * session brought up later gets one. The polite peer creates the
   * channel (the first creation starts negotiation), the other receives
   * it via ondatachannel. Returns the unsubscribe function; the
   * channels themselves live and die with their session.
   */
  subscribeChannel(label: string, cb: PeerChannelHandler): () => void;
  /**
   * Subscribes to SIP-subset call dialogs and call-status amendments arriving
   * on the WebSocket c2c relay. Returns the unsubscribe function.
   */
  subscribeCall(cb: PeerCallHandler): () => void;
  /**
   * Sends call signalling over this peer session's ordered WebSocket output.
   * Rejects when the current identity or peer session is unavailable.
   */
  sendCall(
    channelId: ChannelId,
    peer: SubscriberId,
    call: ClientToClientCallEv,
  ): Promise<void>;
  /**
   * Subscribes to session teardowns; returns the unsubscribe function.
   */
  subscribeReset(cb: SessionResetHandler): () => void;
  /**
   * Adds a local media track to the session's peer connection, to be
   * sent to the peer; the PerfectNegotiator renegotiates automatically
   * (onnegotiationneeded), so no signalling work is needed here.
   * Returns the sender to pass to removeTrack, or null when the session
   * does not exist (any more).
   */
  addTrack(
    channelId: ChannelId,
    peer: SubscriberId,
    track: MediaStreamTrack,
    ...streams: MediaStream[]
  ): RTCRtpSender | null;
  /**
   * Removes a track previously added with addTrack; a no-op when the
   * session is gone. Removing renegotiates like adding does.
   */
  removeTrack(
    channelId: ChannelId,
    peer: SubscriberId,
    sender: RTCRtpSender,
  ): void;
  /** Resolves a retained sender handle to the current transport's sender. */
  resolveSender(
    channelId: ChannelId,
    peer: SubscriberId,
    sender: RTCRtpSender,
  ): RTCRtpSender | null;
  /**
   * Subscribes to remote track arrivals (pc.ontrack) of every session;
   * returns the unsubscribe function.
   */
  subscribeTracks(cb: PeerTrackHandler): () => void;
}

// PeerSession is one peer connection, its data channels by label, and
// the negotiation task driving it.
interface PeerSession {
  pc: RTCPeerConnection;
  channelId: ChannelId;
  self: SubscriberId;
  peer: SubscriberId;
  // true when this end creates the data channels (the smaller
  // subscriber id); false when they arrive via ondatachannel.
  polite: boolean;
  // The session's data channels by label — assigned as each appears,
  // possibly still connecting.
  channels: Map<string, RTCDataChannel>;
  // Creates the session's channel of a label — polite sessions only;
  // a label already present is a no-op.
  createChannel: (label: string) => void;
  sendSignalling: (event: SignallingEvent) => Promise<void>;
  setProxy: (proxy: SSProxy | null) => void;
  addTrack: (
    track: MediaStreamTrack,
    ...streams: MediaStream[]
  ) => RTCRtpSender;
  removeTrack: (sender: RTCRtpSender) => void;
  resolveSender: (sender: RTCRtpSender) => RTCRtpSender | null;
  close: () => void;
}

interface SessionLocalTrack {
  track: MediaStreamTrack;
  streams: MediaStream[];
  sender: RTCRtpSender;
  // Callers keep the sender returned when they joined. A transport rebuild
  // changes the sender, but the original handle must still remove the track.
  handles: Set<RTCRtpSender>;
}

function dtlsTransportStates(pc: RTCPeerConnection): RTCDtlsTransportState[] {
  const transports = new Set<RTCDtlsTransport>();
  for (const transceiver of pc.getTransceivers?.() ?? []) {
    if (transceiver.sender.transport) {
      transports.add(transceiver.sender.transport);
    }
    if (transceiver.receiver.transport) {
      transports.add(transceiver.receiver.transport);
    }
  }
  if (pc.sctp?.transport) transports.add(pc.sctp.transport);
  return [...transports].map((transport) => transport.state);
}

// SessionHooks is how a peer session reports to the hook: a channel
// appeared, a channel opened, a remote track arrived, the session closed.
interface SessionHooks {
  onChannel(
    channelId: ChannelId,
    peer: SubscriberId,
    label: string,
    dc: RTCDataChannel,
  ): void;
  onChannelOpen(
    channelId: ChannelId,
    peer: SubscriberId,
    label: string,
    dc: RTCDataChannel,
  ): void;
  onTrack(channelId: ChannelId, peer: SubscriberId, ev: RTCTrackEvent): void;
  onConnectionState(
    channelId: ChannelId,
    peer: SubscriberId,
    state: RTCPeerConnectionState,
  ): void;
  onSessionClosed(channelId: ChannelId, peer: SubscriberId): void;
}

// startPeerSession brings up one peer connection to peer and starts its
// perfect negotiation over the proxy's streams. The data channels are
// demanded by label: a polite session creates them on createChannel
// (the first creation triggers the initial negotiation), an impolite
// one receives them via ondatachannel; both are reported through hooks.
function startPeerSession(
  proxy: SSProxy | null,
  channelId: ChannelId,
  self: SubscriberId,
  peer: SubscriberId,
  iceServers: RTCIceServer[],
  inbox: PeerInbox,
  hooks: SessionHooks,
): PeerSession {
  let pc = new RTCPeerConnection({ iceServers });
  const sessionId = crypto.randomUUID();
  const localTracks: SessionLocalTrack[] = [];
  const channelLabels = new Set<string>();
  let closed = false;
  let rebuilding = false;
  let rebuildRetry: ReturnType<typeof setTimeout> | undefined;
  const session: PeerSession = {
    pc,
    channelId,
    self,
    peer,
    polite: self < peer,
    channels: new Map(),
    createChannel: () => {},
    sendSignalling: () =>
      Promise.reject(new Error("peersessions: signalling output not ready")),
    setProxy: () => {},
    addTrack: (track, ...streams) => {
      const sender = pc.addTrack(track, ...streams);
      localTracks.push({
        track,
        streams,
        sender,
        handles: new Set([sender]),
      });
      return sender;
    },
    removeTrack: (sender) => {
      const index = localTracks.findIndex((local) => local.handles.has(sender));
      if (index === -1 || pc.signalingState === "closed") return;
      const local = localTracks[index];
      pc.removeTrack(local.sender);
      localTracks.splice(index, 1);
    },
    resolveSender: (sender) =>
      localTracks.find((local) => local.handles.has(sender))?.sender ?? null,
    close: () => {},
  };
  let diagnostics = createPeerDiagnostics(pc, {
    sessionId,
    channelId,
    self,
    peer,
    polite: session.polite,
  });

  // State diagnostics belong to the session rather than either consumer of
  // the PC: event listeners leave PerfectNegotiator's onice... handler and
  // the media/data-channel handlers untouched. Console entries contain safe
  // summaries; the raw PC and wire SDP are never dumped into the log.
  let previousConnectionState: RTCPeerConnectionState | null = null;
  let previousIceConnectionState: RTCIceConnectionState | null = null;
  let hadHealthyConnection = false;
  let statsTimer: ReturnType<typeof setInterval> | undefined;
  const trackContext = (track: MediaStreamTrack | null) =>
    track === null
      ? null
      : {
          id: track.id,
          kind: track.kind,
          enabled: track.enabled,
          muted: track.muted,
          readyState: track.readyState,
        };
  const syncStatsMonitor = (event: string) => {
    const healthy =
      (pc.iceConnectionState === "connected" ||
        pc.iceConnectionState === "completed") &&
      pc.connectionState === "connected";
    if (healthy) {
      hadHealthyConnection = true;
      if (statsTimer !== undefined) {
        clearInterval(statsTimer);
        statsTimer = undefined;
        diagnostics.captureStats(`state:${event}:recovered`);
      }
      return;
    }
    // Do not sample the initial offer/connection ramp. Once a previously
    // healthy call degrades, retain a compact transport timeline until it
    // either recovers or the session closes.
    if (!hadHealthyConnection) return;
    diagnostics.captureStats(`state:${event}`);
    if (statsTimer === undefined) {
      statsTimer = setInterval(
        () => diagnostics.captureStats("recovery:interval"),
        5_000,
      );
    }
  };
  const logState = (
    event:
      | "created"
      | "connectionstatechange"
      | "iceconnectionstatechange"
      | "icegatheringstatechange"
      | "signalingstatechange",
  ) => {
    diagnostics.log(event, {
      previousConnectionState,
      previousIceConnectionState,
      localDescription: summarizeSessionDescription(pc.localDescription),
      remoteDescription: summarizeSessionDescription(pc.remoteDescription),
      dtlsTransportStates: dtlsTransportStates(pc),
      transceivers: (pc.getTransceivers?.() ?? []).map((transceiver) => ({
        mid: transceiver.mid,
        direction: transceiver.direction,
        currentDirection: transceiver.currentDirection,
        senderTrack: trackContext(transceiver.sender.track),
        receiverTrack: trackContext(transceiver.receiver.track),
      })),
      dataChannels: [...session.channels.values()].map((channel) => ({
        label: channel.label,
        readyState: channel.readyState,
      })),
    });
    syncStatsMonitor(event);
    // Keep each state's previous value on its own event stream. The paired
    // ice.../connection... events then each show a real transition instead of
    // the first event erasing the second event's "previous" value.
    if (event !== "iceconnectionstatechange") {
      previousConnectionState = pc.connectionState;
    }
    if (event !== "connectionstatechange") {
      previousIceConnectionState = pc.iceConnectionState;
    }

    // An ICE event is also a synchronization point for the aggregate state.
    // This covers a browser updating connectionState without delivering its
    // aggregate event, without ever pretending ICE-only recovery is healthy.
    // Skip the pre-offer "new" value to avoid a spurious disconnected flash.
    if (event === "connectionstatechange" || pc.connectionState !== "new") {
      hooks.onConnectionState(channelId, peer, pc.connectionState);
    }
    if (
      hadHealthyConnection &&
      pc.connectionState === "failed" &&
      dtlsTransportStates(pc).includes("failed")
    ) {
      rebuildTransport(pc);
    }
  };
  logState("created");
  const onConnectionStateChange = () => logState("connectionstatechange");
  const onIceConnectionStateChange = () => logState("iceconnectionstatechange");
  const onIceGatheringStateChange = () => logState("icegatheringstatechange");
  const onSignalingStateChange = () => logState("signalingstatechange");
  const onIceCandidateError = (event: RTCPeerConnectionIceErrorEvent) =>
    diagnostics.log(
      "ice:candidate:error",
      { errorCode: event.errorCode, errorText: event.errorText },
      "warn",
    );

  // attach registers one channel of the session and reports it to the
  // label's subscribers; its opening resolves the whenOpenChannel
  // waiters.
  const attach = (label: string, dc: RTCDataChannel) => {
    channelLabels.add(label);
    const source = pc;
    const sourceDiagnostics = diagnostics;
    session.channels.set(label, dc);
    sourceDiagnostics.log("datachannel:attach", {
      label,
      readyState: dc.readyState,
    });
    const onOpen = () => {
      if (pc !== source) return;
      sourceDiagnostics.log("datachannel:open", { label });
      hooks.onChannelOpen(channelId, peer, label, dc);
    };
    if (dc.readyState === "open") {
      onOpen();
    } else {
      dc.addEventListener("open", onOpen, { once: true });
    }
    dc.addEventListener("close", () =>
      sourceDiagnostics.log("datachannel:close", { label }),
    );
    dc.addEventListener("error", () =>
      sourceDiagnostics.log("datachannel:error", { label }, "warn"),
    );
    hooks.onChannel(channelId, peer, label, dc);
  };
  session.createChannel = (label) => {
    channelLabels.add(label);
    if (!session.polite || session.channels.has(label)) return;
    attach(label, pc.createDataChannel(label));
  };
  const onDataChannel = (event: RTCDataChannelEvent) =>
    attach(event.channel.label, event.channel);
  // Remote tracks are multiplexed to the track subscribers; media
  // consumers (voice calls) attach and detach tracks freely — the
  // PerfectNegotiator owns the (re)negotiation they trigger.
  const onTrack = (ev: RTCTrackEvent) => {
    diagnostics.log("track:remote", { track: trackContext(ev.track) });
    hooks.onTrack(channelId, peer, ev);
  };

  function wirePeerConnection(target: RTCPeerConnection): void {
    target.addEventListener("connectionstatechange", onConnectionStateChange);
    target.addEventListener(
      "iceconnectionstatechange",
      onIceConnectionStateChange,
    );
    target.addEventListener(
      "icegatheringstatechange",
      onIceGatheringStateChange,
    );
    target.addEventListener("signalingstatechange", onSignalingStateChange);
    target.addEventListener("icecandidateerror", onIceCandidateError);
    if (!session.polite) {
      target.ondatachannel = (event) => {
        if (pc === target) onDataChannel(event);
      };
    }
    target.ontrack = (event) => {
      if (pc === target) onTrack(event);
    };
  }

  function unwirePeerConnection(target: RTCPeerConnection): void {
    target.removeEventListener(
      "connectionstatechange",
      onConnectionStateChange,
    );
    target.removeEventListener(
      "iceconnectionstatechange",
      onIceConnectionStateChange,
    );
    target.removeEventListener(
      "icegatheringstatechange",
      onIceGatheringStateChange,
    );
    target.removeEventListener("signalingstatechange", onSignalingStateChange);
    target.removeEventListener("icecandidateerror", onIceCandidateError);
    target.ondatachannel = null;
    target.ontrack = null;
  }

  function scheduleRebuildRetry(): void {
    if (closed || rebuildRetry !== undefined) return;
    rebuildRetry = setTimeout(() => {
      rebuildRetry = undefined;
      rebuildTransport(pc);
    }, 100);
  }

  function rebuildTransport(failedPc: RTCPeerConnection): void {
    if (
      closed ||
      rebuilding ||
      failedPc !== pc ||
      !dtlsTransportStates(failedPc).includes("failed")
    ) {
      return;
    }
    if (!negotiator.canReplacePeerConnection()) {
      scheduleRebuildRetry();
      return;
    }
    rebuilding = true;
    let fresh: RTCPeerConnection | undefined;
    let adopted = false;
    try {
      fresh = new RTCPeerConnection({ iceServers });
      const stockedTracks = localTracks.map((local) => ({
        local,
        sender: fresh!.addTrack(local.track, ...local.streams),
      }));
      // The fresh PC is the offerer, so it creates every known channel even if
      // this end was impolite on the original transport. The answering Go peer
      // detects the new DTLS identity and waits for these OnDataChannel events.
      const stockedChannels = [...channelLabels].map((label) => ({
        label,
        channel: fresh!.createDataChannel(label),
      }));
      const nextDiagnostics = createPeerDiagnostics(fresh, {
        sessionId,
        channelId,
        self,
        peer,
        polite: session.polite,
      });
      if (!negotiator.replacePeerConnection(fresh, nextDiagnostics)) {
        fresh.close();
        scheduleRebuildRetry();
        return;
      }
      adopted = true;

      const previousPc = pc;
      const previousDiagnostics = diagnostics;
      unwirePeerConnection(previousPc);
      if (statsTimer !== undefined) clearInterval(statsTimer);
      statsTimer = undefined;
      if (rebuildRetry !== undefined) clearTimeout(rebuildRetry);
      rebuildRetry = undefined;
      pc = fresh;
      session.pc = fresh;
      diagnostics = nextDiagnostics;
      session.channels = new Map();
      for (const { local, sender } of stockedTracks) {
        local.handles.add(sender);
        local.sender = sender;
      }
      for (const { label, channel } of stockedChannels) attach(label, channel);
      previousDiagnostics.log("transport:replaced", { reason: "dtls-failed" });
      previousDiagnostics.close();
      previousPc.close();
      previousConnectionState = null;
      previousIceConnectionState = null;
      diagnostics.log("transport:rebuilt", { reason: "dtls-failed" });
      wirePeerConnection(fresh);
      logState("created");
    } catch (err) {
      diagnostics.log(
        "transport:rebuild:error",
        { error: String(err) },
        "error",
      );
      if (!adopted) {
        fresh?.close();
        scheduleRebuildRetry();
      }
    } finally {
      rebuilding = false;
    }
  }

  wirePeerConnection(pc);

  const output = createPeerSignallingOutput(proxy);
  session.sendSignalling = output.write;
  const negotiator = new PerfectNegotiator(pc, {
    channelId,
    selfSubscriber: self,
    peerSubscriber: peer,
    diagnostics,
  });
  session.setProxy = (next) => {
    diagnostics.log("signalling:proxy", { connected: next !== null });
    output.setProxy(next);
    if (next !== null) {
      void negotiator
        .retransmitPendingOffer()
        .catch((err) =>
          diagnostics.log(
            "signalling:retransmit:error",
            { error: String(err) },
            "error",
          ),
        );
    }
  };
  void negotiator
    .negotiate(inbox.stream, output.stream)
    .catch((err) =>
      diagnostics.log("negotiation:error", { error: String(err) }, "error"),
    );

  session.close = () => {
    if (closed) return;
    closed = true;
    // State listeners go first: close()'s queued "closed" event must not
    // report — a teardown is announced through onSessionClosed. Closing the
    // inbox ends negotiate(), which releases its stream locks; closing the
    // connection stops ICE and its events.
    unwirePeerConnection(pc);
    if (statsTimer !== undefined) clearInterval(statsTimer);
    statsTimer = undefined;
    if (rebuildRetry !== undefined) clearTimeout(rebuildRetry);
    rebuildRetry = undefined;
    diagnostics.log("session:closed");
    diagnostics.close();
    output.close();
    inbox.close();
    pc.close();
    hooks.onSessionClosed(channelId, peer);
  };
  return session;
}

/**
 * Brings up a WebRTC peer connection to every fellow member of the
 * current user's channel. `me` and `channels` are the useSignalling
 * state: sessions appear as the listing discovers members and disappear
 * when members drop out. A proxy swap keeps existing sessions alive and
 * reconnects their signalling; an identity change tears every session down
 * and rebuilds from the current listing.
 *
 * The ICE servers are loaded via the useICEServers query; no session is
 * brought up before the query settles, so every peer connection is
 * created with the configured servers (with none when the query fails —
 * local-network peers still connect).
 *
 * `listingRevision` advances after every successful listing, including an
 * unchanged one. `completeMembershipChannelIds` limits omission evidence to
 * channels whose member pages and profiles fully resolved. A definitive
 * `registrationFailed` ends the pre-registration pending window.
 *
 * The returned connectionStates map carries every session's live
 * RTCPeerConnection.connectionState (channelId → peer → state), for the
 * UI's presence line; a session's entry appears with its first state
 * change and goes with the session's teardown.
 */
export function usePeerSessions(
  me: ChatUser | null,
  channels: ChatChannel[],
  listingRevision = 0,
  completeMembershipChannelIds: readonly ChannelId[] = [],
  registrationFailed = false,
  registrationReady = true,
): { sessions: PeerSessions; connectionStates: PeerConnectionStates } {
  const proxy = useSSProxy();
  const sessionsRef = useRef(new Map<string, PeerSession>());
  const peerInboxesRef = useRef(new Map<string, PeerInbox>());
  const pendingCallsRef = useRef(new Map<string, PendingCalls>());
  const previousIdentityRef = useRef<EndpointIdentity | null>(null);
  const countedListingRevisionRef = useRef<number | null>(null);
  // The async demux needs the latest address; teardown also uses the latest
  // lifecycle to distinguish initial registration from a real replacement.
  const lifecycleRef = useRef({ proxy, me, registrationFailed });
  lifecycleRef.current = { proxy, me, registrationFailed };
  const [connectionStates, setConnectionStates] =
    useState<PeerConnectionStates>({});
  // The consumers' channel subscriptions by label, the teardown
  // subscribers, and the waiters whenOpenChannel parks until a channel
  // opens or its session goes away (peer key → label → resolvers).
  const channelHandlersRef = useRef(new Map<string, Set<PeerChannelHandler>>());
  const trackHandlersRef = useRef(new Set<PeerTrackHandler>());
  const callHandlersRef = useRef(new Set<PeerCallHandler>());
  const resetHandlersRef = useRef(new Set<SessionResetHandler>());
  const channelWaitersRef = useRef(
    new Map<string, Map<string, Set<(dc: RTCDataChannel | null) => void>>>(),
  );

  const sessionHooks = useMemo<SessionHooks>(
    () => ({
      onChannel: (channelId, peer, label, dc) => {
        for (const cb of channelHandlersRef.current.get(label) ?? []) {
          cb(channelId, peer, dc);
        }
      },
      onChannelOpen: (channelId, peer, label, dc) => {
        const key = peerKey(channelId, peer);
        const byLabel = channelWaitersRef.current.get(key);
        const waiters = byLabel?.get(label);
        if (byLabel === undefined || waiters === undefined) return;
        byLabel.delete(label);
        if (byLabel.size === 0) channelWaitersRef.current.delete(key);
        for (const resolve of waiters) resolve(dc);
      },
      onTrack: (channelId, peer, ev) => {
        for (const cb of trackHandlersRef.current) cb(channelId, peer, ev);
      },
      onConnectionState: (channelId, peer, state) => {
        setConnectionStates((prev) => {
          if (prev[channelId]?.[peer] === state) return prev;
          return {
            ...prev,
            [channelId]: { ...prev[channelId], [peer]: state },
          };
        });
      },
      onSessionClosed: (channelId, peer) => {
        // The session's connection-state entry goes with it: the last
        // reported state must not linger (a re-registered peer would
        // briefly render it until the new session's first transition).
        setConnectionStates((prev) => {
          const byPeer = prev[channelId];
          if (byPeer === undefined || byPeer[peer] === undefined) {
            return prev;
          }
          const nextByPeer = { ...byPeer };
          delete nextByPeer[peer];
          if (Object.keys(nextByPeer).length === 0) {
            const next = { ...prev };
            delete next[channelId];
            return next;
          }
          return { ...prev, [channelId]: nextByPeer };
        });
        const key = peerKey(channelId, peer);
        const byLabel = channelWaitersRef.current.get(key);
        if (byLabel !== undefined) {
          channelWaitersRef.current.delete(key);
          for (const waiters of byLabel.values()) {
            for (const resolve of waiters) resolve(null);
          }
        }
        for (const cb of resetHandlersRef.current) cb({ channelId, peer });
      },
    }),
    [],
  );

  // The ICE servers of the peer connections, cached by react-query; null
  // until the query settles, holding back every session until then.
  const iceServersQuery = useICEServers();
  useEffect(() => {
    if (iceServersQuery.isError) {
      console.error(
        "peersessions: cannot load /api/iceServers, using no ICE servers",
        iceServersQuery.error,
      );
    }
  }, [iceServersQuery.isError, iceServersQuery.error]);
  const iceServers: RTCIceServer[] | null = useMemo(() => {
    const urls = iceServersQuery.data;
    if (urls !== undefined) {
      return urls.length > 0 ? [{ urls }] : [];
    }
    return iceServersQuery.isError ? [] : null;
  }, [iceServersQuery.data, iceServersQuery.isError]);

  // Pre-registration traffic belongs to the endpoint that received it, even
  // though there is no subscriber identity for the lifecycle cleanup below to
  // depend on. Keep the last non-null endpoint across transport outages: only
  // a confirmed pair change clears pending input. Registered sessions, waiters,
  // and reset notifications remain the existing lifecycle cleanup's job.
  useEffect(() => {
    if (proxy === null) return;
    const previous = previousIdentityRef.current;
    previousIdentityRef.current = proxy.identity;
    if (previous === null || sameEndpointIdentity(previous, proxy.identity)) {
      return;
    }
    for (const inbox of peerInboxesRef.current.values()) inbox.close();
    peerInboxesRef.current.clear();
    pendingCallsRef.current.clear();
  }, [proxy]);

  // A polite peer can offer before our membership listing has created the
  // impolite session. Route every inbound c2c event through a per-peer inbox:
  // either that offer or membership creates the inbox first, and both paths
  // then use the same stream, so neither ordering loses SDP or candidates.
  // Answers/candidates without an inbox cannot start a session and are ignored.
  //
  // Do not send c2c traffic on a replacement socket until its roaming
  // registration has succeeded; the server may already have aged out the old
  // registration even though the WebSocket itself is open.
  useEffect(() => {
    const outputProxy = registrationReady ? proxy : null;
    for (const session of sessionsRef.current.values()) {
      session.setProxy(outputProxy);
    }
  }, [proxy, registrationReady]);

  useEffect(() => {
    if (proxy === null) return;
    const inboxes = peerInboxesRef.current;
    const reader = proxy.getReadStream({ replayEarlyC2C: true }).getReader();
    let live = true;
    let readerActive = true;
    void (async () => {
      try {
        for (;;) {
          const { done, value } = await reader.read();
          if (done || !live) return;
          const c2c = value.c2CEv;
          if (c2c === undefined) continue;
          // Ack only after reader.read yields: a StrictMode cleanup that wins
          // before this point leaves the event replayable for the next setup.
          proxy.acknowledgeEarlyC2C(value);
          const current = lifecycleRef.current.me;
          if (lifecycleRef.current.registrationFailed && current === null) {
            continue;
          }
          if (
            current?.subscriberId !== undefined &&
            current.channelId !== undefined &&
            (c2c.channelId !== current.channelId ||
              c2c.toSubscriber !== current.subscriberId)
          ) {
            continue;
          }
          if (c2c.call !== undefined) {
            const session = sessionsRef.current.get(
              peerKey(c2c.channelId, c2c.fromSubscriber),
            );
            if (
              session === undefined ||
              session.self !== c2c.toSubscriber ||
              callHandlersRef.current.size === 0
            ) {
              const key = inboxKey(
                c2c.channelId,
                c2c.toSubscriber,
                c2c.fromSubscriber,
              );
              const pending = pendingCallsRef.current.get(key) ?? {
                channelId: c2c.channelId,
                self: c2c.toSubscriber,
                peer: c2c.fromSubscriber,
                events: [],
                absentListings: 0,
              };
              pending.events.push(value);
              pending.absentListings = 0;
              pendingCallsRef.current.set(key, pending);
            } else {
              for (const cb of callHandlersRef.current) cb(value);
            }
            continue;
          }
          const key = inboxKey(
            c2c.channelId,
            c2c.toSubscriber,
            c2c.fromSubscriber,
          );
          let inbox = inboxes.get(key);
          if (inbox === undefined) {
            if (c2c.sessionDesc?.type !== "offer") continue;
            inbox = createPeerInbox(
              c2c.channelId,
              c2c.toSubscriber,
              c2c.fromSubscriber,
            );
            inboxes.set(key, inbox);
          }
          if (c2c.sessionDesc?.type === "offer") {
            // A fresh offer is newer liveness evidence than earlier listings.
            inbox.absentListings = 0;
          }
          inbox.enqueue(value);
        }
      } catch (err) {
        if (live) console.error("peersessions: signalling read failed", err);
      } finally {
        readerActive = false;
        reader.releaseLock();
      }
    })();
    return () => {
      live = false;
      if (readerActive) void reader.cancel().catch(() => {});
    };
  }, [proxy]);

  // Sessions belong to the subscriber incarnation, not to one WebSocket.
  // A proxy swap therefore leaves them alone; a real identity change or
  // unmount still tears everything down before reconciliation rebuilds.
  const wasUnregistered = me === null;
  const selfChannelId = me?.channelId;
  const selfSubscriberId = me?.subscriberId;
  // A failed attempt with a retained previous identity is a roaming retry,
  // not an incarnation reset. Only failure with no usable identity ends the
  // pre-registration pending window.
  const registrationReset = registrationFailed && wasUnregistered;
  useEffect(() => {
    const sessions = sessionsRef.current;
    const inboxes = peerInboxesRef.current;
    const pendingCalls = pendingCallsRef.current;
    const channelWaiters = channelWaitersRef.current;
    const resetHandlers = resetHandlersRef.current;
    return () => {
      const initialRegistration =
        wasUnregistered && lifecycleRef.current.me !== null;
      // Initial registration must not discard offers accepted while the
      // recipient was unknown. Unmount and genuine identity replacement still
      // tear everything down.
      if (initialRegistration) return;
      countedListingRevisionRef.current = null;
      for (const session of sessions.values()) {
        session.close();
      }
      sessions.clear();
      for (const inbox of inboxes.values()) inbox.close();
      inboxes.clear();
      pendingCalls.clear();
      // Anything still waiting on a channel is released, and the
      // consumers are told every session is gone.
      for (const byLabel of channelWaiters.values()) {
        for (const waiters of byLabel.values()) {
          for (const resolve of waiters) resolve(null);
        }
      }
      channelWaiters.clear();
      for (const cb of resetHandlers) cb(null);
      setConnectionStates({});
    };
  }, [wasUnregistered, selfChannelId, selfSubscriberId, registrationReset]);

  // Reconcile the sessions with the current member listing: open one
  // session per newly seen member of the own channel, close the ones
  // whose peer dropped out. Kept sessions are never touched, so a
  // membership change elsewhere never renegotiates an existing pair. A
  // new session gets a channel per subscribed label (subscribeChannel
  // does the same for sessions that predate the subscription).
  useEffect(() => {
    const sessions = sessionsRef.current;
    if (
      proxy === null ||
      !registrationReady ||
      me === null ||
      me.channelId === undefined ||
      me.subscriberId === undefined
    ) {
      return;
    }
    const self = me.subscriberId;
    const inboxes = peerInboxesRef.current;
    // Registration validates offers accepted before the recipient was known.
    for (const [key, inbox] of inboxes) {
      if (inbox.channelId !== me.channelId || inbox.self !== self) {
        inbox.close();
        inboxes.delete(key);
      }
    }
    for (const [key, pending] of pendingCallsRef.current) {
      if (pending.channelId !== me.channelId || pending.self !== self) {
        pendingCallsRef.current.delete(key);
      }
    }
    if (iceServers === null) return;
    const want = new Set<string>();
    for (const channel of channels) {
      if (channel.id !== me.channelId) {
        continue;
      }
      for (const member of channel.members) {
        const peer = member.subscriberId;
        if (peer === undefined || peer === self) {
          continue;
        }
        const key = peerKey(channel.id, peer);
        want.add(key);
        if (!sessions.has(key)) {
          const id = inboxKey(channel.id, self, peer);
          let inbox = inboxes.get(id);
          if (inbox === undefined) {
            inbox = createPeerInbox(channel.id, self, peer);
            inboxes.set(id, inbox);
          }
          inbox.absentListings = 0;
          const session = startPeerSession(
            proxy,
            channel.id,
            self,
            peer,
            iceServers,
            inbox,
            sessionHooks,
          );
          sessions.set(key, session);
          const pending = pendingCallsRef.current.get(id);
          if (pending !== undefined && callHandlersRef.current.size > 0) {
            for (const event of pending.events) {
              for (const cb of callHandlersRef.current) cb(event);
            }
            pendingCallsRef.current.delete(id);
          }
          for (const label of channelHandlersRef.current.keys()) {
            session.createChannel(label);
          }
        }
      }
    }
    for (const [key, session] of sessions) {
      if (!want.has(key)) {
        session.close();
        sessions.delete(key);
        inboxes.delete(inboxKey(session.channelId, self, session.peer));
      }
    }
    if (!completeMembershipChannelIds.includes(me.channelId)) return;
    // Only a newly completed listing is omission evidence. Proxy, identity
    // object, and ICE-server effect reruns may reuse the same retained listing.
    if (countedListingRevisionRef.current === listingRevision) return;
    countedListingRevisionRef.current = listingRevision;
    // One absent listing may have been assembled before the offer arrived.
    // Retire only a pending inbox omitted by two consecutive own-channel
    // listings; a fresh offer resets this evidence in the demux above.
    for (const [key, inbox] of inboxes) {
      if (sessions.has(peerKey(inbox.channelId, inbox.peer))) {
        inbox.absentListings = 0;
        continue;
      }
      inbox.absentListings += 1;
      if (inbox.absentListings >= 2) {
        inbox.close();
        inboxes.delete(key);
      }
    }
    for (const [key, pending] of pendingCallsRef.current) {
      if (sessions.has(peerKey(pending.channelId, pending.peer))) {
        pending.absentListings = 0;
        continue;
      }
      pending.absentListings += 1;
      if (pending.absentListings >= 2) {
        pendingCallsRef.current.delete(key);
      }
    }
  }, [
    proxy,
    me,
    channels,
    listingRevision,
    completeMembershipChannelIds,
    registrationReady,
    iceServers,
    sessionHooks,
  ]);

  const getChannel = useCallback(
    (label: string, channelId: ChannelId, peer: SubscriberId) =>
      sessionsRef.current.get(peerKey(channelId, peer))?.channels.get(label) ??
      null,
    [],
  );
  const whenOpenChannel = useCallback(
    (label: string, channelId: ChannelId, peer: SubscriberId) => {
      const key = peerKey(channelId, peer);
      const dc = sessionsRef.current.get(key)?.channels.get(label);
      if (dc?.readyState === "open") {
        return Promise.resolve(dc);
      }
      // The waiter resolves when the channel opens (SessionHooks), or
      // with null when the session goes away first — a waiter whose
      // session is never created is released by the teardown effect.
      return new Promise<RTCDataChannel | null>((resolve) => {
        const byLabel = channelWaitersRef.current.get(key) ?? new Map();
        const waiters = byLabel.get(label) ?? new Set();
        waiters.add(resolve);
        byLabel.set(label, waiters);
        channelWaitersRef.current.set(key, byLabel);
      });
    },
    [],
  );
  const subscribeChannel = useCallback(
    (label: string, cb: PeerChannelHandler) => {
      const handlers = channelHandlersRef.current.get(label) ?? new Set();
      handlers.add(cb);
      channelHandlersRef.current.set(label, handlers);
      for (const session of sessionsRef.current.values()) {
        const existing = session.channels.get(label);
        if (existing !== undefined) {
          cb(session.channelId, session.peer, existing);
        } else {
          // A polite session creates the channel on demand; attach()
          // reports it to every subscriber of the label, this one
          // included. An impolite session's channel arrives via
          // ondatachannel once the remote end creates it.
          session.createChannel(label);
        }
      }
      return () => {
        handlers.delete(cb);
      };
    },
    [],
  );
  const subscribeCall = useCallback((cb: PeerCallHandler) => {
    callHandlersRef.current.add(cb);
    for (const [key, pending] of pendingCallsRef.current) {
      if (sessionsRef.current.has(peerKey(pending.channelId, pending.peer))) {
        for (const event of pending.events) cb(event);
        pendingCallsRef.current.delete(key);
      }
    }
    return () => {
      callHandlersRef.current.delete(cb);
    };
  }, []);
  const sendCall = useCallback(
    (
      channelId: ChannelId,
      peer: SubscriberId,
      call: ClientToClientCallEv,
    ): Promise<void> => {
      const me = lifecycleRef.current.me;
      const self = me?.subscriberId;
      const session = sessionsRef.current.get(peerKey(channelId, peer));
      if (
        self === undefined ||
        me?.channelId !== channelId ||
        session === undefined
      ) {
        return Promise.reject(
          new Error(
            `peersessions: no peer session for call signalling to ${peer}`,
          ),
        );
      }
      return session.sendSignalling({
        from: {},
        to: { serviceId: WELLKNOWN_SVC_ID_SS },
        msgId: crypto.randomUUID(),
        c2CEv: {
          fromSubscriber: self,
          toSubscriber: peer,
          channelId,
          call: {
            ...call,
            channelId,
            fromSubscriberId: self,
            toSubscriberId: peer,
          },
        },
      });
    },
    [],
  );
  const subscribeReset = useCallback((cb: SessionResetHandler) => {
    resetHandlersRef.current.add(cb);
    return () => {
      resetHandlersRef.current.delete(cb);
    };
  }, []);
  const addTrack = useCallback(
    (
      channelId: ChannelId,
      peer: SubscriberId,
      track: MediaStreamTrack,
      ...streams: MediaStream[]
    ) => {
      const session = sessionsRef.current.get(peerKey(channelId, peer));
      if (session === undefined) return null;
      return session.addTrack(track, ...streams);
    },
    [],
  );
  const removeTrack = useCallback(
    (channelId: ChannelId, peer: SubscriberId, sender: RTCRtpSender) => {
      const session = sessionsRef.current.get(peerKey(channelId, peer));
      if (session === undefined) return;
      session.removeTrack(sender);
    },
    [],
  );
  const resolveSender = useCallback(
    (
      channelId: ChannelId,
      peer: SubscriberId,
      sender: RTCRtpSender,
    ): RTCRtpSender | null =>
      sessionsRef.current
        .get(peerKey(channelId, peer))
        ?.resolveSender(sender) ?? null,
    [],
  );
  const subscribeTracks = useCallback((cb: PeerTrackHandler) => {
    trackHandlersRef.current.add(cb);
    return () => {
      trackHandlersRef.current.delete(cb);
    };
  }, []);

  const sessions = useMemo<PeerSessions>(
    () => ({
      getChannel,
      whenOpenChannel,
      subscribeChannel,
      subscribeCall,
      sendCall,
      subscribeReset,
      addTrack,
      removeTrack,
      resolveSender,
      subscribeTracks,
    }),
    [
      getChannel,
      whenOpenChannel,
      subscribeChannel,
      subscribeCall,
      sendCall,
      subscribeReset,
      addTrack,
      removeTrack,
      resolveSender,
      subscribeTracks,
    ],
  );
  return { sessions, connectionStates };
}
