"use client";

/**
 * Data-channel messaging between channel members, over the signalling
 * server.
 *
 * useDataChannel is one of the two consumers of the peer sessions
 * usePeerSessions brings up (see peersessions.tsx) — one
 * RTCPeerConnection per fellow member of the current user's channel,
 * PerfectNegotiator-driven over the SS's client-to-client relay. It
 * subscribes the messaging data channel (dcmsg) of every session and
 * exchanges DCMsgs over it; the binary channel (dcbin) is
 * useBinaryDataChannel's business — the two are siblings over the
 * sessions primitive, decoupled from each other.
 *
 * Messages are DCMsgs. DCMsg is agnostic to the on-the-wire format the
 * data channel (SCTP) carries — the codec (encodeDCMsg/decodeDCMsg) is
 * the only place that knows it; JSON today, but it could be XML or
 * anything else. A data channel does not echo its own messages back,
 * so the recipient bounces every message it accepts back to the sender
 * with the echo flag set: the sender's own messages arrive as echoes
 * over the same channel.
 */

import { useCallback, useEffect, useState } from "react";

import type { PeerSessions } from "./peersessions";
import type { ChannelId, ChatUser, MsgId, SubscriberId } from "./types";

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
}

/**
 * Exchanges DCMsgs over the messaging data channel (dcmsg) of the peer
 * sessions — one of the two consumers of the PeerSessions primitive
 * (see peersessions.tsx), decoupled from the other
 * (useBinaryDataChannel): both build on the same peer connections,
 * neither knowing the other. `me` is the useSignalling identity; the
 * echo check below needs it.
 */
export function useDataChannel(
  me: ChatUser | null,
  sessions: PeerSessions,
): UseDataChannelResult {
  const [dcMsgs, setDcMsgs] = useState<DCMsgs>({});

  // Wire the messaging channel of every session: inbound DCMsgs that
  // decode cleanly and name the session's pair fold into the store, and
  // every accepted message is bounced back to the sender with the echo
  // flag set — a data channel does not echo on its own, so the sender's
  // own messages arrive as echoes over the same channel.
  useEffect(() => {
    const self = me?.subscriberId;
    if (self === undefined) return;
    return sessions.subscribeChannel(
      DATA_CHANNEL_LABEL,
      (channelId, peer, dc) => {
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
            setDcMsgs((prev) => applyDCMsg(prev, msg));
            return;
          }
          if (msg.fromSubscriberId !== peer) {
            return;
          }
          setDcMsgs((prev) => applyDCMsg(prev, msg));
          // Bounce the message back so the sender sees its own message.
          dc.send(encodeDCMsg({ ...msg, echo: true }));
        };
      },
    );
  }, [sessions, me?.subscriberId]);

  const sendTo = useCallback(
    (msg: DCMsg) => {
      const dc = sessions.getChannel(
        DATA_CHANNEL_LABEL,
        msg.channelId,
        msg.toSubscriberId,
      );
      if (dc?.readyState !== "open") {
        console.warn(
          `datachannel: no open data channel to subscriber ` +
            `${msg.toSubscriberId} in channel ${msg.channelId}; ` +
            `message ${msg.msgId} dropped`,
        );
        return;
      }
      dc.send(encodeDCMsg(msg));
    },
    [sessions],
  );

  return { dcMsgs, sendTo };
}
