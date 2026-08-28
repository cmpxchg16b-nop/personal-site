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
 * over the same channel. The one exception is the call protocol
 * (application/x-sip): like real SIP, a dialog message is never
 * bounced — the sender records its own copy when it sends, so a
 * call's behavior never depends on an echo.
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

// DC_MSG_MIME_SIP: one message of the phone (voice or video call)
// session protocol (body in the sip field) — a SIP subset with the SDP
// body stripped. Every message of a call's dialog is one of these, and
// every one is behavior: the caller's INVITE opens the dialog (stored
// and rendered as the call's log entry; its arrival is what rings the
// callee), the callee answers it with a response (200 OK / 603
// Decline), the caller aborts the ring with a CANCEL, either party
// hangs an established call up with a BYE. Each end folds the dialog's
// messages into its session state (see usePhoneCalls); the log entry's
// displayed status only follows the dialog (via chat-control amends of
// the INVITE), never leads it.
//
// The subset and its deliberate omissions:
//
// - The SDP body is stripped. This system's actual SDP offer/answer
//   lives elsewhere: it rides the SS's client-to-client relay between
//   the two browsers' PerfectNegotiators (see peersessions.tsx) when
//   the pair's peer connection negotiates and renegotiates. These
//   messages are pure dialog verbs.
// - No ACK, no CSeq, and no echo: SCTP delivery is ordered and
//   reliable, and the fold is order-independent (a precedence
//   maximum). Unlike every other DCMsg kind, a dialog message is never
//   bounced back to its sender — like real SIP, each end advances its
//   own state from its own sends (recorded locally at send time, see
//   useDataChannel) and the peer's sends (received), never from an
//   echo.
// - From / To live on the DCMsg envelope (fromSubscriberId /
//   toSubscriberId); the dialog identifier (SIP's Call-ID) is in the
//   body.
// - What cannot be expressed in standard SIP terms travels as SIP-style
//   extension headers (X-*), see DCSip.
export const DC_MSG_MIME_SIP = "application/x-sip";

// DC_CALL_STATUSES are the lifecycle states of a phone call's dialog.
export const DC_CALL_STATUSES = [
  "inviting",
  "accepted",
  "rejected",
  "cancelled",
  "ended",
] as const;

// DCCallStatus is one lifecycle state of a phone call: "inviting" (the
// caller's INVITE is ringing), "accepted" (the callee answered 200 OK —
// media flows), "rejected" (the callee answered 603 Decline),
// "cancelled" (the caller CANCELed before an answer), "ended" (an
// accepted call was hung up with a BYE).
export type DCCallStatus = (typeof DC_CALL_STATUSES)[number];

// DC_CALL_KINDS are the kinds of a phone call.
export const DC_CALL_KINDS = ["voice", "video"] as const;

// DCCallKind is what a call carries: "voice" attaches the microphones
// only, "video" additionally the cameras.
export type DCCallKind = (typeof DC_CALL_KINDS)[number];

// DC_SIP_METHODS are the request methods of the SIP subset: the caller
// opens the dialog with an INVITE and aborts the ring with a CANCEL
// (before a final response); either party hangs an established dialog
// up with a BYE.
export const DC_SIP_METHODS = ["INVITE", "CANCEL", "BYE"] as const;

// DCSipMethod is one request method of the SIP subset.
export type DCSipMethod = (typeof DC_SIP_METHODS)[number];

// DC_SIP_RESPONSE_OK / DC_SIP_RESPONSE_DECLINE are the INVITE's final
// responses: the callee picks up (200 OK) or declines (603 Decline).
export const DC_SIP_RESPONSE_OK = { code: 200, phrase: "OK" } as const;
export const DC_SIP_RESPONSE_DECLINE = {
  code: 603,
  phrase: "Decline",
} as const;

// DCSipResponse is a SIP status line: the response code and its reason
// phrase, as one of the well-known pairs above.
export type DCSipResponse =
  typeof DC_SIP_RESPONSE_OK | typeof DC_SIP_RESPONSE_DECLINE;

// DCSip is the body of a SIP-subset DCMsg: one message of a call's
// dialog, addressed to the dialog's other party by callId. The messages
// ARE the phone session protocol — they drive each end's session state.
// They travel the data channel like any DCMsg, but — unlike any other
// kind — are never echoed: the sender folds its own send in at send
// time (see useDataChannel), so both ends fold the same messages into
// their session state without any bounce. Only the INVITE is rendered
// (the call's log entry) and only the INVITE is amendable (its
// X-Call-Status, via chat control).
export interface DCSip {
  /** the dialog's identifier — SIP's Call-ID header; minted by the
      caller with the INVITE, named by every later message of the
      dialog */
  callId: string;
  /** the start line of a request: its method. Exactly one of method /
      response is set — a SIP start line is a request line XOR a
      status line */
  method?: DCSipMethod;
  /** the start line of a response: its status line */
  response?: DCSipResponse;
  /** extension header, INVITE only: what the dialog carries — "voice"
      or "video" — standing in for the stripped SDP body's m= lines;
      absent means "voice" */
  "X-Media"?: DCCallKind;
  /** extension header, INVITE only: the dialog's logged UI state — the
      status the log entry and the status indicators display. Not a
      SIP notion, and a dependent variable: it changes only when the
      dialog's messages caused it, the amendment riding chat control
      (and only the INVITE's author — the caller — amends) */
  "X-Call-Status"?: DCCallStatus;
}

// DCChatControl is the body of a chat-control DCMsg. For "delete" only
// targetMessageId is set. For "amend" exactly one of text /
// fileTransfer / sip carries the target's new content; the
// amendment keeps the target's original id and timestamp (see
// applyChatControlDCMsg below).
export interface DCChatControl {
  subtype: "delete" | "amend";
  targetMessageId: MsgId;
  /** amended content, when the target is a text message */
  text?: string;
  /** amended status, when the target is a file-transfer status message */
  fileTransfer?: DCFileTransfer;
  /** amended content, when the target is a call's INVITE; only the
      INVITE's author (the caller) amends it, reporting the dialog's
      new UI state (X-Call-Status) after its messages unfolded */
  sip?: DCSip;
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
   * true marks the sender's own copy of the message. For echoed kinds
   * the copy arrives bounced back by the recipient (a data channel
   * does not echo on its own; echoes are never echoed again). A SIP
   * message (application/x-sip) is never bounced: the sender records
   * its own copy at send time. Absent on the original wire frame.
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
  /** the SIP-subset message when mimeType is DC_MSG_MIME_SIP */
  sip?: DCSip;
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

// newSipDCMsg builds the DCMsg carrying one message of a call's
// SIP-subset dialog to toSubscriberId — an INVITE (the call's log
// entry, stored on both ends via the echo), a response to it, a
// CANCEL, or a BYE — minting a fresh msg id and stamping the creation
// time.
export function newSipDCMsg(
  channelId: ChannelId,
  fromSubscriberId: SubscriberId,
  toSubscriberId: SubscriberId,
  sip: DCSip,
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
    mimeType: DC_MSG_MIME_SIP,
    plaintext: "",
    sip,
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

// isWellFormedSip reports whether sip is a structurally valid DCSip: a
// callId, a start line that is a request line XOR a status line, and
// the extension headers on the INVITE alone (X-Call-Status required
// there, X-Media optional).
function isWellFormedSip(sip: DCSip): boolean {
  if (typeof sip.callId !== "string") return false;
  // SIP's start line is a request line XOR a status line.
  if ((sip.method === undefined) === (sip.response === undefined)) {
    return false;
  }
  // Extension headers belong to the INVITE alone.
  if (
    sip.method !== "INVITE" &&
    (sip["X-Media"] !== undefined || sip["X-Call-Status"] !== undefined)
  ) {
    return false;
  }
  if (sip.response !== undefined) {
    const r = sip.response;
    return (
      (r.code === DC_SIP_RESPONSE_OK.code &&
        r.phrase === DC_SIP_RESPONSE_OK.phrase) ||
      (r.code === DC_SIP_RESPONSE_DECLINE.code &&
        r.phrase === DC_SIP_RESPONSE_DECLINE.phrase)
    );
  }
  const method = sip.method;
  if (
    method === undefined ||
    !(DC_SIP_METHODS as readonly string[]).includes(method)
  ) {
    return false;
  }
  if (method !== "INVITE") return true;
  const status = sip["X-Call-Status"];
  const media = sip["X-Media"];
  return (
    typeof status === "string" &&
    (DC_CALL_STATUSES as readonly string[]).includes(status) &&
    (media === undefined ||
      (DC_CALL_KINDS as readonly string[]).includes(media))
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
        !isWellFormedFileTransfer(cc.fileTransfer)) ||
      (cc.sip !== undefined && !isWellFormedSip(cc.sip))
    ) {
      return null;
    }
  }
  if (
    msg.mimeType === DC_MSG_MIME_SIP &&
    (msg.sip === undefined || !isWellFormedSip(msg.sip))
  ) {
    return null;
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
// (plaintext for a text message, fileTransfer for a file-transfer status,
// sip for a call's INVITE), keeping the target's
// msgId and creationTimestamp — an amendment never moves or reattributes
// a message. A chat control only ever mutates UI state: the actions that
// CAUSED the new state are their own frames (the file-transfer
// acknowledgements, the dialog's SIP messages), never chat controls.
// Controls targeting another sender's message, an unknown message, or a
// mismatched body kind are no-ops; an INVITE amend must also be an
// INVITE naming the target dialog's own callId.
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
  } else if (
    cc.sip !== undefined &&
    target.mimeType === DC_MSG_MIME_SIP &&
    target.sip !== undefined &&
    target.sip.method === "INVITE" &&
    cc.sip.method === "INVITE" &&
    target.sip.callId === cc.sip.callId
  ) {
    amended = { ...target, sip: cc.sip };
  }
  if (amended === null) return prev;
  const next = [...list];
  next[index] = amended;
  return { ...prev, [channelId]: { ...byPeer, [senderId]: next } };
}

// applyDCMsg folds one inbound DCMsg into the store: a chat-control
// message mutates the sender's earlier message and is never stored;
// anything else appends. SIP messages append like ordinary messages —
// they are the call protocol's record the phone-call state is folded
// from; useChatMessages renders the INVITE (the log entry) and simply
// never renders the rest of the dialog.
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
   * id, second key the sender's subscriber id, oldest first. The own
   * subscriber id's list holds our own sends (echo set): chat kinds
   * arrive as echoes bounced back by their recipients; SIP messages are
   * never echoed, so our own SIP sends are recorded at send time.
   * Chat-control messages are applied on arrival — the target is
   * deleted or amended in place — and never show up in the store.
   */
  dcMsgs: DCMsgs;
  /**
   * Sends one DCMsg on the data channel to msg.toSubscriberId. While no
   * channel to that peer is open — not negotiated yet, or the peer is
   * gone — the message is dropped with a warning. A SIP message that
   * goes out is additionally recorded locally as our own send (echo
   * set): SIP messages are never echoed back, so the call's state must
   * advance from the send itself.
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
          // Bounce the message back so the sender sees its own message
          // — except SIP messages: like real SIP, a dialog message is
          // never bounced; the sender records its own sends (sendTo).
          if (msg.mimeType !== DC_MSG_MIME_SIP) {
            dc.send(encodeDCMsg({ ...msg, echo: true }));
          }
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
      // A SIP message is never echoed back (the receive path skips its
      // bounce): record the sender's own copy locally, echo set, so the
      // dialog's state advances from the send itself — never from a
      // bounce.
      if (msg.mimeType === DC_MSG_MIME_SIP) {
        setDcMsgs((prev) => applyDCMsg(prev, { ...msg, echo: true }));
      }
    },
    [sessions],
  );

  return { dcMsgs, sendTo };
}
