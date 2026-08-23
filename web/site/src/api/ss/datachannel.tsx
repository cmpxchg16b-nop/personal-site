"use client";

/**
 * Data-channel messaging between channel members, over the signalling
 * server.
 *
 * useDataChannel grabs the SSProxy from React context (like
 * useSignalling) and brings up one RTCPeerConnection per fellow member
 * of the current user's channel, driving each with a PerfectNegotiator
 * (see negotiate.ts): SDP offers/answers and trickled ICE candidates
 * ride the SS's client-to-client relay, so once the channel listing
 * shows a peer, negotiation — and with it the messaging data channel —
 * follows automatically. Exactly one data channel exists per pair,
 * created by the polite peer (the lexicographically smaller subscriber
 * id — the same rule PerfectNegotiator derives the role from) and
 * received via ondatachannel on the other.
 *
 * Messages are DCMsgs. DCMsg is agnostic to the on-the-wire format the
 * data channel (SCTP) carries — the codec (encodeDCMsg/decodeDCMsg) is
 * the only place that knows it; JSON today, but it could be XML or
 * anything else. A data channel does not echo its own messages back,
 * so the recipient bounces every message it accepts back to the sender
 * with the echo flag set: the sender's own messages arrive as echoes
 * over the same channel.
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
import type {
  ChannelId,
  ChatChannel,
  ChatUser,
  MsgId,
  SubscriberId,
} from "./types";

// DC_MSG_MIME_VERSION is the DCMsg format version, MIME-Version style.
export const DC_MSG_MIME_VERSION = "1.0";

// DC_MSG_MIME_PLAINTEXT is the only supported body kind for now.
export const DC_MSG_MIME_PLAINTEXT = "text/plain";

// DCMsg is a message carried over a WebRTC data channel.
export interface DCMsg {
  /** version of the message format, MIME-Version style */
  mimeVersion: string;
  /** the channel the two subscribers live in */
  channelId: ChannelId;
  fromSubscriberId: SubscriberId;
  toSubscriberId: SubscriberId;
  /** Unix seconds, matching ChatMessage.timestamp */
  creationTimestamp: number;
  msgId: MsgId;
  inReplyTo?: MsgId;
  /**
   * true when this frame is an echo: the recipient's copy of the
   * message bounced back to its sender, so the sender sees its own
   * message — a data channel does not echo on its own. Absent on the
   * original send; echoes are never echoed again.
   */
  echo?: boolean;
  /** the body kind; only DC_MSG_MIME_PLAINTEXT is supported for now */
  mimeType: string;
  /** the message body, when mimeType is text/plain */
  plaintext: string;
}

// DCMsgs maps channelId → sender subscriber id → the list of messages
// received from that sender over the data channel, oldest first. The
// own subscriber id's list holds the echoes of the messages we sent
// (echo set), bounced back by their recipients.
export type DCMsgs = Record<ChannelId, Record<SubscriberId, DCMsg[]>>;

// newDCMsg builds a plaintext DCMsg to toSubscriberId, minting a fresh
// msg id and stamping the creation time.
export function newDCMsg(
  channelId: ChannelId,
  fromSubscriberId: SubscriberId,
  toSubscriberId: SubscriberId,
  plaintext: string,
  inReplyTo?: MsgId,
): DCMsg {
  return {
    mimeVersion: DC_MSG_MIME_VERSION,
    channelId,
    fromSubscriberId,
    toSubscriberId,
    creationTimestamp: Date.now() / 1000,
    msgId: crypto.randomUUID(),
    inReplyTo,
    mimeType: DC_MSG_MIME_PLAINTEXT,
    plaintext,
  };
}

// encodeDCMsg serializes a DCMsg for the wire. The data channel's
// on-the-wire format is a private detail of this codec — JSON today, but
// it could be XML or anything else; DCMsg itself stays agnostic to it.
function encodeDCMsg(msg: DCMsg): string {
  return JSON.stringify(msg);
}

// decodeDCMsg parses one data-channel frame back into a DCMsg, returning
// null for non-text or malformed frames (dropped silently, mirroring the
// SS's rule for malformed events).
function decodeDCMsg(data: unknown): DCMsg | null {
  if (typeof data !== "string") {
    return null;
  }
  let msg: DCMsg;
  try {
    msg = JSON.parse(data) as DCMsg;
  } catch {
    return null;
  }
  if (
    typeof msg.channelId !== "string" ||
    typeof msg.fromSubscriberId !== "string" ||
    typeof msg.toSubscriberId !== "string" ||
    typeof msg.msgId !== "string" ||
    typeof msg.plaintext !== "string" ||
    (msg.echo !== undefined && typeof msg.echo !== "boolean")
  ) {
    return null;
  }
  return msg;
}

// DATA_CHANNEL_LABEL is the label of the messaging data channel every
// pair of peers brings up.
const DATA_CHANNEL_LABEL = "dcmsg";

// peerKey indexes the session of one (channel, peer subscriber) pair.
function peerKey(channelId: ChannelId, subscriberId: SubscriberId): string {
  return `${channelId}:${subscriberId}`;
}

// PeerSession is one peer connection plus its messaging data channel and
// the negotiation task driving it.
interface PeerSession {
  pc: RTCPeerConnection;
  // The messaging data channel — assigned as soon as it exists (created
  // by the polite peer, received by the other), possibly still
  // connecting.
  dc: RTCDataChannel | null;
  close: () => void;
}

// startPeerSession brings up one peer connection to peer and starts its
// perfect negotiation over the proxy's streams. Inbound DCMsgs that
// decode cleanly and name this pair go to onMessage.
function startPeerSession(
  proxy: SSProxy,
  channelId: ChannelId,
  self: SubscriberId,
  peer: SubscriberId,
  iceServers: RTCIceServer[],
  onMessage: (msg: DCMsg) => void,
): PeerSession {
  const pc = new RTCPeerConnection({ iceServers });
  const session: PeerSession = { pc, dc: null, close: () => {} };

  const wire = (dc: RTCDataChannel) => {
    session.dc = dc;
    dc.onmessage = (e) => {
      const msg = decodeDCMsg(e.data);
      // The data channel is bound to this pair by construction; a
      // message claiming otherwise is dropped.
      if (msg === null || msg.channelId !== channelId) {
        return;
      }
      if (msg.echo === true) {
        // An echo is one of our own messages bounced back by the peer:
        // it must name us as the sender and the peer as the recipient,
        // and it is never echoed again.
        if (msg.fromSubscriberId !== self || msg.toSubscriberId !== peer) {
          return;
        }
        onMessage(msg);
        return;
      }
      if (msg.fromSubscriberId !== peer) {
        return;
      }
      onMessage(msg);
      // Bounce the message back so the sender sees its own message.
      dc.send(encodeDCMsg({ ...msg, echo: true }));
    };
  };
  // Exactly one data channel per pair: the polite peer (smaller
  // subscriber id) creates it — which also triggers the initial
  // negotiation — the impolite peer receives it via ondatachannel.
  if (self < peer) {
    wire(pc.createDataChannel(DATA_CHANNEL_LABEL));
  } else {
    pc.ondatachannel = (e) => wire(e.channel);
  }

  const negotiator = new PerfectNegotiator(pc, {
    channelId,
    selfSubscriber: self,
    peerSubscriber: peer,
  });
  const inStream = proxy.getReadStream();
  void negotiator
    .negotiate(inStream, proxy.getWriteStream())
    .catch((err) =>
      console.error(`datachannel: negotiation with ${peer} failed`, err),
    );

  session.close = () => {
    // Cancelling the inbound stream ends negotiate(), which releases its
    // stream locks; closing the connection stops ICE and its events.
    void inStream.cancel();
    pc.close();
  };
  return session;
}

function appendDCMsg(prev: DCMsgs, msg: DCMsg): DCMsgs {
  const byPeer = prev[msg.channelId] ?? {};
  const list = byPeer[msg.fromSubscriberId] ?? [];
  return {
    ...prev,
    [msg.channelId]: {
      ...byPeer,
      [msg.fromSubscriberId]: [...list, msg],
    },
  };
}

export interface UseDataChannelResult {
  /**
   * Messages received from data channels so far: first key the channel
   * id, second key the sender's subscriber id, oldest first. Our own
   * messages come back as echoes (echo set) under our own subscriber
   * id.
   */
  dcMsgs: DCMsgs;
  /**
   * Sends one DCMsg on the data channel to msg.toSubscriberId. While no
   * channel to that peer is open — not negotiated yet, or the peer is
   * gone — the message is dropped with a warning.
   */
  sendTo: (msg: DCMsg) => void;
}

/**
 * Brings up a WebRTC data channel to every fellow member of the current
 * user's channel and exchanges DCMsgs over it. `me` and `channels` are
 * the useSignalling state: sessions appear as the listing discovers
 * members and disappear when members drop out; a proxy swap (reconnect)
 * or an identity change tears every session down and rebuilds from the
 * current listing.
 *
 * The ICE servers are loaded via the useICEServers query; no session is
 * brought up before the query settles, so every peer connection is
 * created with the configured servers (with none when the query fails —
 * local-network peers still connect).
 */
export function useDataChannel(
  me: ChatUser | null,
  channels: ChatChannel[],
): UseDataChannelResult {
  const proxy = useSSProxy();
  const [dcMsgs, setDcMsgs] = useState<DCMsgs>({});
  const sessionsRef = useRef(new Map<string, PeerSession>());

  // The ICE servers of the peer connections, cached by react-query; null
  // until the query settles, holding back every session until then.
  const iceServersQuery = useICEServers();
  useEffect(() => {
    if (iceServersQuery.isError) {
      console.error(
        "datachannel: cannot load /api/iceServers, using no ICE servers",
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
    };
  }, [proxy, me]);

  // Reconcile the sessions with the current member listing: open one
  // session per newly seen member of the own channel, close the ones
  // whose peer dropped out. Kept sessions are never touched, so a
  // membership change elsewhere never renegotiates an existing pair.
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
          sessions.set(
            key,
            startPeerSession(proxy, channel.id, self, peer, iceServers, (msg) =>
              setDcMsgs((prev) => appendDCMsg(prev, msg)),
            ),
          );
        }
      }
    }
    for (const [key, session] of sessions) {
      if (!want.has(key)) {
        session.close();
        sessions.delete(key);
      }
    }
  }, [proxy, me, channels, iceServers]);

  const sendTo = useCallback((msg: DCMsg) => {
    const session = sessionsRef.current.get(
      peerKey(msg.channelId, msg.toSubscriberId),
    );
    if (session?.dc?.readyState !== "open") {
      console.warn(
        `datachannel: no open data channel to subscriber ` +
          `${msg.toSubscriberId} in channel ${msg.channelId}; ` +
          `message ${msg.msgId} dropped`,
      );
      return;
    }
    session.dc.send(encodeDCMsg(msg));
  }, []);

  return { dcMsgs, sendTo };
}
