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
 * disappear when members drop out; a proxy swap (reconnect) or an
 * identity change tears every session down and rebuilds from the
 * current listing.
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
 * (subscribeTracks); the PerfectNegotiator renegotiates on its own, so
 * voice-call media needs no signalling work of its own — and no second
 * peer connection.
 *
 * The ICE servers for the peer connections come from GET
 * /api/iceServers (the <iceServer/> entries of the server configuration
 * document, matched to this origin) via the useICEServers query; peer
 * sessions are not brought up before that query has settled, so every
 * connection negotiates with the configured servers.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import { useICEServers } from "@/hooks/useICEServers";
import { PerfectNegotiator } from "./negotiate";
import type { SSProxy } from "./proxy";
import { useSSProxy } from "./react";
import type { ChannelId, ChatChannel, ChatUser, SubscriberId } from "./types";

// peerKey indexes the session of one (channel, peer subscriber) pair.
function peerKey(channelId: ChannelId, subscriberId: SubscriberId): string {
  return `${channelId}:${subscriberId}`;
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

// SessionResetHandler is notified when peer sessions go away: `dropped`
// names one closed session, or null when every session was torn down
// (a proxy swap or an identity change).
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

// PeerSessions is the peer-connection plumbing the data-channel
// consumers (useDataChannel, useBinaryDataChannel) build on: one peer
// connection per (channel, peer) pair, its data channels demanded by
// label. The object is stable across renders.
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
  close: () => void;
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
  proxy: SSProxy,
  channelId: ChannelId,
  self: SubscriberId,
  peer: SubscriberId,
  iceServers: RTCIceServer[],
  hooks: SessionHooks,
): PeerSession {
  const pc = new RTCPeerConnection({ iceServers });
  const session: PeerSession = {
    pc,
    channelId,
    peer,
    polite: self < peer,
    channels: new Map(),
    createChannel: () => {},
    close: () => {},
  };

  // attach registers one channel of the session and reports it to the
  // label's subscribers; its opening resolves the whenOpenChannel
  // waiters.
  const attach = (label: string, dc: RTCDataChannel) => {
    session.channels.set(label, dc);
    if (dc.readyState === "open") {
      hooks.onChannelOpen(channelId, peer, label, dc);
    } else {
      dc.addEventListener(
        "open",
        () => hooks.onChannelOpen(channelId, peer, label, dc),
        { once: true },
      );
    }
    hooks.onChannel(channelId, peer, label, dc);
  };
  session.createChannel = (label) => {
    if (!session.polite || session.channels.has(label)) return;
    attach(label, pc.createDataChannel(label));
  };
  if (!session.polite) {
    pc.ondatachannel = (e) => attach(e.channel.label, e.channel);
  }
  // Remote tracks are multiplexed to the track subscribers; media
  // consumers (voice calls) attach and detach tracks freely — the
  // PerfectNegotiator owns the (re)negotiation they trigger.
  pc.ontrack = (ev) => hooks.onTrack(channelId, peer, ev);
  // The connection state feeds the UI's presence line.
  pc.onconnectionstatechange = () =>
    hooks.onConnectionState(channelId, peer, pc.connectionState);

  const negotiator = new PerfectNegotiator(pc, {
    channelId,
    selfSubscriber: self,
    peerSubscriber: peer,
  });
  const inStream = proxy.getReadStream();
  void negotiator
    .negotiate(inStream, proxy.getWriteStream())
    .catch((err) =>
      console.error(`peersessions: negotiation with ${peer} failed`, err),
    );

  session.close = () => {
    // The state handler goes first: close()'s queued "closed" event must
    // not report — a teardown is announced through onSessionClosed.
    // Cancelling the inbound stream ends negotiate(), which releases its
    // stream locks; closing the connection stops ICE and its events.
    pc.onconnectionstatechange = null;
    void inStream.cancel();
    pc.close();
    hooks.onSessionClosed(channelId, peer);
  };
  return session;
}

/**
 * Brings up a WebRTC peer connection to every fellow member of the
 * current user's channel. `me` and `channels` are the useSignalling
 * state: sessions appear as the listing discovers members and disappear
 * when members drop out; a proxy swap (reconnect) or an identity change
 * tears every session down and rebuilds from the current listing.
 *
 * The ICE servers are loaded via the useICEServers query; no session is
 * brought up before the query settles, so every peer connection is
 * created with the configured servers (with none when the query fails —
 * local-network peers still connect).
 *
 * The returned connectionStates map carries every session's live
 * RTCPeerConnection.connectionState (channelId → peer → state), for the
 * UI's presence line; a session's entry appears with its first state
 * change and goes with the session's teardown.
 */
export function usePeerSessions(
  me: ChatUser | null,
  channels: ChatChannel[],
): { sessions: PeerSessions; connectionStates: PeerConnectionStates } {
  const proxy = useSSProxy();
  const sessionsRef = useRef(new Map<string, PeerSession>());
  const [connectionStates, setConnectionStates] =
    useState<PeerConnectionStates>({});
  // The consumers' channel subscriptions by label, the teardown
  // subscribers, and the waiters whenOpenChannel parks until a channel
  // opens or its session goes away (peer key → label → resolvers).
  const channelHandlersRef = useRef(new Map<string, Set<PeerChannelHandler>>());
  const trackHandlersRef = useRef(new Set<PeerTrackHandler>());
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
        setConnectionStates((prev) => ({
          ...prev,
          [channelId]: { ...prev[channelId], [peer]: state },
        }));
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

  // Sessions are bound to their connection and to the own subscription:
  // a proxy swap or an identity change tears every session down; the
  // reconcile effect below then rebuilds from the current listing.
  useEffect(() => {
    const sessions = sessionsRef.current;
    return () => {
      for (const session of sessions.values()) {
        session.close();
      }
      sessions.clear();
      // Anything still waiting on a channel is released, and the
      // consumers are told every session is gone.
      for (const byLabel of channelWaitersRef.current.values()) {
        for (const waiters of byLabel.values()) {
          for (const resolve of waiters) resolve(null);
        }
      }
      channelWaitersRef.current.clear();
      for (const cb of resetHandlersRef.current) cb(null);
      setConnectionStates({});
    };
  }, [proxy, me]);

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
      me === null ||
      me.channelId === undefined ||
      me.subscriberId === undefined ||
      iceServers === null
    ) {
      return;
    }
    const self = me.subscriberId;
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
          const session = startPeerSession(
            proxy,
            channel.id,
            self,
            peer,
            iceServers,
            sessionHooks,
          );
          sessions.set(key, session);
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
      }
    }
  }, [proxy, me, channels, iceServers, sessionHooks]);

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
      return session.pc.addTrack(track, ...streams);
    },
    [],
  );
  const removeTrack = useCallback(
    (channelId: ChannelId, peer: SubscriberId, sender: RTCRtpSender) => {
      const session = sessionsRef.current.get(peerKey(channelId, peer));
      if (session === undefined || session.pc.signalingState === "closed") {
        return;
      }
      session.pc.removeTrack(sender);
    },
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
      subscribeReset,
      addTrack,
      removeTrack,
      subscribeTracks,
    }),
    [
      getChannel,
      whenOpenChannel,
      subscribeChannel,
      subscribeReset,
      addTrack,
      removeTrack,
      subscribeTracks,
    ],
  );
  return { sessions, connectionStates };
}
