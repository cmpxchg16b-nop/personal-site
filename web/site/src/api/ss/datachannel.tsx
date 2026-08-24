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
 * follows automatically. Two data channels exist per pair, both riding
 * the one peer connection, both created by the polite peer (the
 * lexicographically smaller subscriber id — the same rule
 * PerfectNegotiator derives the role from) and received via
 * ondatachannel on the other, dispatched by label: the messaging
 * channel (dcmsg) carries JSON DCMsgs, the binary channel (dcbin)
 * carries compact binary frames (see binaryframes.ts) and is handed to
 * consumers — useBinaryDataChannel — as a BinaryTransport.
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

// DC_MSG_MIME_PLAINTEXT: a plain-text chat line.
export const DC_MSG_MIME_PLAINTEXT = "text/plain";

// DC_MSG_MIME_FILE_TRANSFER_STATUS: a file-transfer status update (body in
// the fileTransfer field). The file's bytes never travel in the message —
// it exists so both ends can render the transfer's progress.
export const DC_MSG_MIME_FILE_TRANSFER_STATUS =
  "application/x-file-transfer-status";

// DC_FILE_TRANSFER_STATUSES are the lifecycle states of a file transfer.
export const DC_FILE_TRANSFER_STATUSES = [
  "pending",
  "running",
  "done",
] as const;

// DCFileTransferKind is how a transfer is announced and rendered: a
// plain file download card ("file") or an inline media card ("image",
// "video"). It is the sender's explicit choice — the attach menu's
// picker — never derived from the file's MIME type.
export type DCFileTransferKind = "file" | "image" | "video";

// DCFileTransfer is the body of a file-transfer-status DCMsg: the UI state
// of one file transfer.
export interface DCFileTransfer {
  /** opaque, globally unique identifier of the file */
  fileId: string;
  /** how the transfer is announced and rendered (the sender's choice) */
  kind: DCFileTransferKind;
  filename: string;
  fileMIMEType: string;
  fileSizeTotalBytes: number;
  fileSizeTransferred: number;
  fileTransferStatus: "pending" | "running" | "done";
}

// DC_MSG_MIME_CHAT_CONTROL: a chat control message (body in the
// chatControl field): delete or amend an earlier message.
export const DC_MSG_MIME_CHAT_CONTROL = "application/x-chat-control";

// DCChatControl is the body of a chat-control DCMsg. For "delete" only
// targetMessageId is set. For "amend" exactly one of text / fileTransfer
// carries the target's new content; the amendment keeps the target's
// original id and timestamp (see applyChatControlDCMsg below).
export interface DCChatControl {
  subtype: "delete" | "amend";
  targetMessageId: MsgId;
  /** amended content, when the target is a text message */
  text?: string;
  /** amended status, when the target is a file-transfer status message */
  fileTransfer?: DCFileTransfer;
}

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
  /** the body kind; see DC_MSG_MIME_PLAINTEXT et al. */
  mimeType: string;
  /** the message body when mimeType is text/plain; empty otherwise */
  plaintext: string;
  /** the file-transfer status when mimeType is
      DC_MSG_MIME_FILE_TRANSFER_STATUS */
  fileTransfer?: DCFileTransfer;
  /** the control operation when mimeType is DC_MSG_MIME_CHAT_CONTROL */
  chatControl?: DCChatControl;
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

// newFileTransferDCMsg builds a file-transfer-status DCMsg to
// toSubscriberId, minting a fresh msg id and stamping the creation time.
export function newFileTransferDCMsg(
  channelId: ChannelId,
  fromSubscriberId: SubscriberId,
  toSubscriberId: SubscriberId,
  fileTransfer: DCFileTransfer,
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
    mimeType: DC_MSG_MIME_FILE_TRANSFER_STATUS,
    plaintext: "",
    fileTransfer,
  };
}

// newChatControlDCMsg builds a chat-control DCMsg to toSubscriberId,
// minting a fresh msg id and stamping the creation time.
export function newChatControlDCMsg(
  channelId: ChannelId,
  fromSubscriberId: SubscriberId,
  toSubscriberId: SubscriberId,
  chatControl: DCChatControl,
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
    mimeType: DC_MSG_MIME_CHAT_CONTROL,
    plaintext: "",
    chatControl,
  };
}

// encodeDCMsg serializes a DCMsg for the wire. The data channel's
// on-the-wire format is a private detail of this codec — JSON today, but
// it could be XML or anything else; DCMsg itself stays agnostic to it.
function encodeDCMsg(msg: DCMsg): string {
  return JSON.stringify(msg);
}

// isWellFormedFileTransfer reports whether ft is a structurally valid
// DCFileTransfer.
function isWellFormedFileTransfer(ft: DCFileTransfer): boolean {
  return (
    typeof ft.fileId === "string" &&
    (ft.kind === "file" || ft.kind === "image" || ft.kind === "video") &&
    typeof ft.filename === "string" &&
    typeof ft.fileMIMEType === "string" &&
    typeof ft.fileSizeTotalBytes === "number" &&
    typeof ft.fileSizeTransferred === "number" &&
    (ft.fileTransferStatus === "pending" ||
      ft.fileTransferStatus === "running" ||
      ft.fileTransferStatus === "done")
  );
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
    typeof msg.mimeType !== "string" ||
    typeof msg.plaintext !== "string" ||
    (msg.echo !== undefined && typeof msg.echo !== "boolean")
  ) {
    return null;
  }
  if (
    msg.mimeType === DC_MSG_MIME_FILE_TRANSFER_STATUS &&
    (msg.fileTransfer === undefined ||
      !isWellFormedFileTransfer(msg.fileTransfer))
  ) {
    return null;
  }
  if (msg.mimeType === DC_MSG_MIME_CHAT_CONTROL) {
    const cc = msg.chatControl;
    if (
      cc === undefined ||
      (cc.subtype !== "delete" && cc.subtype !== "amend") ||
      typeof cc.targetMessageId !== "string" ||
      (cc.text !== undefined && typeof cc.text !== "string") ||
      (cc.fileTransfer !== undefined &&
        !isWellFormedFileTransfer(cc.fileTransfer))
    ) {
      return null;
    }
  }
  return msg;
}

// DATA_CHANNEL_LABEL is the label of the messaging data channel every
// pair of peers brings up.
const DATA_CHANNEL_LABEL = "dcmsg";

// BINARY_DATA_CHANNEL_LABEL is the label of the binary data channel every
// pair of peers brings up alongside the messaging one, on the same peer
// connection; it carries compact binary frames (see binaryframes.ts).
// This module only moves the bytes — frame semantics live with the
// consumer (binarydatachannel.tsx).
const BINARY_DATA_CHANNEL_LABEL = "dcbin";

// peerKey indexes the session of one (channel, peer subscriber) pair.
function peerKey(channelId: ChannelId, subscriberId: SubscriberId): string {
  return `${channelId}:${subscriberId}`;
}

// BinaryFrameHandler receives one inbound binary frame from a peer's
// binary data channel.
export type BinaryFrameHandler = (
  channelId: ChannelId,
  from: SubscriberId,
  data: ArrayBuffer,
) => void;

// SessionResetHandler is notified when peer sessions go away: `dropped`
// names one closed session, or null when every session was torn down
// (a proxy swap or an identity change).
export type SessionResetHandler = (
  dropped: { channelId: ChannelId; peer: SubscriberId } | null,
) => void;

// BinaryTransport is useDataChannel's binary-channel plumbing, consumed
// by useBinaryDataChannel (binarydatachannel.tsx): it moves binary
// frames between peers, nothing more — frame semantics live with the
// consumer.
export interface BinaryTransport {
  /**
   * Resolves with the open binary data channel to `peer`, waiting for
   * the session to appear and the channel to open; resolves with null
   * when the session goes away first (the peer dropped out, or every
   * session was torn down). A peer that never appears at all leaves the
   * promise pending until the next teardown.
   */
  whenOpenChannel(
    channelId: ChannelId,
    peer: SubscriberId,
  ): Promise<RTCDataChannel | null>;
  /**
   * Subscribes to inbound binary frames from any peer; returns the
   * unsubscribe function.
   */
  subscribeFrames(cb: BinaryFrameHandler): () => void;
  /**
   * Subscribes to session teardowns; returns the unsubscribe function.
   */
  subscribeReset(cb: SessionResetHandler): () => void;
}

// BinarySessionHooks is how a peer session reports its binary channel
// (label dcbin) to useDataChannel's BinaryTransport: the channel opened,
// a frame arrived, the session closed.
interface BinarySessionHooks {
  onBinaryOpen(
    channelId: ChannelId,
    peer: SubscriberId,
    dc: RTCDataChannel,
  ): void;
  onBinaryFrame(
    channelId: ChannelId,
    peer: SubscriberId,
    data: ArrayBuffer,
  ): void;
  onSessionClosed(channelId: ChannelId, peer: SubscriberId): void;
}

// PeerSession is one peer connection plus its messaging and binary data
// channels and the negotiation task driving it.
interface PeerSession {
  pc: RTCPeerConnection;
  // The messaging data channel — assigned as soon as it exists (created
  // by the polite peer, received by the other), possibly still
  // connecting.
  dc: RTCDataChannel | null;
  // The binary data channel — same lifecycle as dc.
  bindc: RTCDataChannel | null;
  close: () => void;
}

// startPeerSession brings up one peer connection to peer and starts its
// perfect negotiation over the proxy's streams. Inbound DCMsgs that
// decode cleanly and name this pair go to onMessage; the binary channel
// is reported through binary.
function startPeerSession(
  proxy: SSProxy,
  channelId: ChannelId,
  self: SubscriberId,
  peer: SubscriberId,
  iceServers: RTCIceServer[],
  onMessage: (msg: DCMsg) => void,
  binary: BinarySessionHooks,
): PeerSession {
  const pc = new RTCPeerConnection({ iceServers });
  const session: PeerSession = { pc, dc: null, bindc: null, close: () => {} };

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
  // wireBinary hooks up the binary channel (label dcbin): it carries
  // ArrayBuffer frames only, which are reported to the consumer; its
  // opening resolves the BinaryTransport's waiters.
  const wireBinary = (dc: RTCDataChannel) => {
    session.bindc = dc;
    dc.binaryType = "arraybuffer";
    dc.onmessage = (e) => {
      // Anything but binary frames is dropped silently, mirroring
      // decodeDCMsg's rule for malformed frames.
      if (e.data instanceof ArrayBuffer) {
        binary.onBinaryFrame(channelId, peer, e.data);
      }
    };
    if (dc.readyState === "open") {
      binary.onBinaryOpen(channelId, peer, dc);
    } else {
      dc.addEventListener(
        "open",
        () => binary.onBinaryOpen(channelId, peer, dc),
        { once: true },
      );
    }
  };
  // Two data channels per pair on the one peer connection, both created
  // by the polite peer (the messaging channel's creation also triggers
  // the initial negotiation); the impolite peer receives them via
  // ondatachannel, dispatched by label.
  if (self < peer) {
    wire(pc.createDataChannel(DATA_CHANNEL_LABEL));
    wireBinary(pc.createDataChannel(BINARY_DATA_CHANNEL_LABEL));
  } else {
    pc.ondatachannel = (e) => {
      if (e.channel.label === BINARY_DATA_CHANNEL_LABEL) {
        wireBinary(e.channel);
      } else {
        wire(e.channel);
      }
    };
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
    binary.onSessionClosed(channelId, peer);
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

// applyChatControlDCMsg applies a chat-control message to the sender's own
// stored messages: "delete" drops the target, "amend" rewrites its body
// (plaintext for a text message, fileTransfer for a file-transfer status),
// keeping the target's msgId and creationTimestamp — an amendment never
// moves or reattributes a message. Controls targeting another sender's
// message, an unknown message, or a mismatched body kind are no-ops.
function applyChatControlDCMsg(
  prev: DCMsgs,
  channelId: ChannelId,
  senderId: SubscriberId,
  cc: DCChatControl,
): DCMsgs {
  const byPeer = prev[channelId];
  const list = byPeer?.[senderId];
  if (byPeer === undefined || list === undefined) return prev;
  if (cc.subtype === "delete") {
    const next = list.filter((m) => m.msgId !== cc.targetMessageId);
    if (next.length === list.length) return prev;
    return { ...prev, [channelId]: { ...byPeer, [senderId]: next } };
  }
  const index = list.findIndex((m) => m.msgId === cc.targetMessageId);
  if (index === -1) return prev;
  const target = list[index];
  let amended: DCMsg | null = null;
  if (cc.text !== undefined && target.mimeType === DC_MSG_MIME_PLAINTEXT) {
    amended = { ...target, plaintext: cc.text };
  } else if (
    cc.fileTransfer !== undefined &&
    target.mimeType === DC_MSG_MIME_FILE_TRANSFER_STATUS
  ) {
    amended = { ...target, fileTransfer: cc.fileTransfer };
  }
  if (amended === null) return prev;
  const next = [...list];
  next[index] = amended;
  return { ...prev, [channelId]: { ...byPeer, [senderId]: next } };
}

// applyDCMsg folds one inbound DCMsg into the store: a chat-control
// message mutates the sender's earlier message and is never stored;
// anything else appends.
function applyDCMsg(prev: DCMsgs, msg: DCMsg): DCMsgs {
  const cc =
    msg.mimeType === DC_MSG_MIME_CHAT_CONTROL ? msg.chatControl : undefined;
  if (cc !== undefined) {
    return applyChatControlDCMsg(prev, msg.channelId, msg.fromSubscriberId, cc);
  }
  return appendDCMsg(prev, msg);
}

export interface UseDataChannelResult {
  /**
   * Messages received from data channels so far: first key the channel
   * id, second key the sender's subscriber id, oldest first. Our own
   * messages come back as echoes (echo set) under our own subscriber
   * id. Chat-control messages are applied on arrival — the target is
   * deleted or amended in place — and never show up in the store.
   */
  dcMsgs: DCMsgs;
  /**
   * Sends one DCMsg on the data channel to msg.toSubscriberId. While no
   * channel to that peer is open — not negotiated yet, or the peer is
   * gone — the message is dropped with a warning.
   */
  sendTo: (msg: DCMsg) => void;
  /**
   * The binary-channel plumbing of the same peer sessions: the transport
   * useBinaryDataChannel builds file transfer on. The object is stable
   * across renders.
   */
  binaryTransport: BinaryTransport;
}

/**
 * Brings up a WebRTC peer connection to every fellow member of the
 * current user's channel, each carrying two data channels — the
 * messaging channel (dcmsg) DCMsgs are exchanged over, and the binary
 * channel (dcbin) exposed as binaryTransport. `me` and `channels` are
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
  // The BinaryTransport's plumbing: inbound-frame and session-teardown
  // subscribers, and the waiters whenOpenChannel parks until a binary
  // channel opens or its session goes away.
  const frameHandlersRef = useRef(new Set<BinaryFrameHandler>());
  const resetHandlersRef = useRef(new Set<SessionResetHandler>());
  const channelWaitersRef = useRef(
    new Map<string, Set<(dc: RTCDataChannel | null) => void>>(),
  );

  const binaryHooks = useMemo<BinarySessionHooks>(
    () => ({
      onBinaryOpen: (channelId, peer, dc) => {
        const key = peerKey(channelId, peer);
        const waiters = channelWaitersRef.current.get(key);
        if (waiters === undefined) return;
        channelWaitersRef.current.delete(key);
        for (const resolve of waiters) resolve(dc);
      },
      onBinaryFrame: (channelId, peer, data) => {
        for (const cb of frameHandlersRef.current) cb(channelId, peer, data);
      },
      onSessionClosed: (channelId, peer) => {
        const key = peerKey(channelId, peer);
        const waiters = channelWaitersRef.current.get(key);
        if (waiters !== undefined) {
          channelWaitersRef.current.delete(key);
          for (const resolve of waiters) resolve(null);
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
      // Anything still waiting on a binary channel is released, and the
      // BinaryTransport's consumers are told every session is gone.
      for (const waiters of channelWaitersRef.current.values()) {
        for (const resolve of waiters) resolve(null);
      }
      channelWaitersRef.current.clear();
      for (const cb of resetHandlersRef.current) cb(null);
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
            startPeerSession(
              proxy,
              channel.id,
              self,
              peer,
              iceServers,
              (msg) => setDcMsgs((prev) => applyDCMsg(prev, msg)),
              binaryHooks,
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
  }, [proxy, me, channels, iceServers, binaryHooks]);

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

  const whenOpenChannel = useCallback(
    (channelId: ChannelId, peer: SubscriberId) => {
      const key = peerKey(channelId, peer);
      const bindc = sessionsRef.current.get(key)?.bindc;
      if (bindc?.readyState === "open") {
        return Promise.resolve(bindc);
      }
      // The waiter resolves when the channel opens (BinarySessionHooks),
      // or with null when the session goes away first — a waiter whose
      // session is never created is released by the teardown effect.
      return new Promise<RTCDataChannel | null>((resolve) => {
        const waiters = channelWaitersRef.current.get(key) ?? new Set();
        waiters.add(resolve);
        channelWaitersRef.current.set(key, waiters);
      });
    },
    [],
  );
  const subscribeFrames = useCallback((cb: BinaryFrameHandler) => {
    frameHandlersRef.current.add(cb);
    return () => {
      frameHandlersRef.current.delete(cb);
    };
  }, []);
  const subscribeReset = useCallback((cb: SessionResetHandler) => {
    resetHandlersRef.current.add(cb);
    return () => {
      resetHandlersRef.current.delete(cb);
    };
  }, []);
  const binaryTransport = useMemo<BinaryTransport>(
    () => ({ whenOpenChannel, subscribeFrames, subscribeReset }),
    [whenOpenChannel, subscribeFrames, subscribeReset],
  );

  return { dcMsgs, sendTo, binaryTransport };
}
