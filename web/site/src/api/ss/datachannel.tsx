"use client";

/**
 * Chat/call message state between channel members.
 *
 * useDataChannel owns the shared message store, but the transports are
 * deliberately split: ordinary chat and file metadata use each peer
 * session's dcmsg DataChannel, while SIP-subset call dialogs and their
 * chatControl.sip history amendments use the WebSocket c2c call relay.
 * useBinaryDataChannel owns dcbin independently.
 *
 * A data channel does not echo its own business messages back, so the
 * recipient bounces every accepted dcmsg message with echo set. Call
 * signalling has no bounce: the sender records its local copy after the
 * WebSocket write succeeds, and the receiver folds the relayed event
 * directly. Legacy call frames arriving on dcmsg are dropped rather than
 * forming a second signalling stack.
 */

import { useCallback, useEffect, useState } from "react";

import type { PeerSessions } from "./peersessions";
import type {
  ChannelId,
  ChatUser,
  ClientToClientCallEv,
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

// DCSip is the body of a SIP-subset call message: one message of a
// dialog, addressed to the other party by callId. These messages ARE the
// phone session protocol and travel only on the WebSocket c2c call relay.
// There is no network echo: each sender folds its own successful write,
// so both ends fold the same messages without a bounce. Only the INVITE
// is rendered (the call's log entry) and amendable (X-Call-Status).
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

// DCMsg is the application's historical message envelope. Business kinds
// travel on dcmsg; the call-only subset is embedded in c2CEv.call on WS.
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

// DCMsgs maps channelId → sender subscriber id → messages, oldest first.
// The own list holds business echoes bounced by recipients and local
// copies of successful WebSocket call writes (echo set).
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

// isCallSignalling identifies the two message shapes that belong on the
// WebSocket call relay: the dialog itself and its INVITE history amendments.
// Text/file chat control remains dcmsg business data.
function isCallSignalling(msg: DCMsg): boolean {
  return (
    msg.mimeType === DC_MSG_MIME_SIP ||
    (msg.mimeType === DC_MSG_MIME_CHAT_CONTROL &&
      msg.chatControl?.sip !== undefined)
  );
}

// decodeCallSignalling validates a c2c call payload with the same codec as
// the historical message envelope. Only call signalling is accepted.
function decodeCallSignalling(call: ClientToClientCallEv): DCMsg | null {
  const msg = decodeDCMsg(JSON.stringify(call));
  return msg !== null && isCallSignalling(msg) ? msg : null;
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
   * Messages received from dcmsg and the WebSocket call relay. The own
   * subscriber list holds business echoes and local call copies (echo
   * set). Chat controls amend/delete their targets instead of appearing
   * as separate entries.
   */
  dcMsgs: DCMsgs;
  /**
   * Sends one message to msg.toSubscriberId. Business kinds use dcmsg;
   * SIP and chatControl.sip use the peer session's ordered WebSocket
   * output. A successful call write is recorded locally (echo set).
   */
  sendTo: (msg: DCMsg) => void;
}

/**
 * Owns the message store and routes business messages over dcmsg and
 * call signalling over WebSocket. `me` is the useSignalling identity;
 * the business echo check needs it.
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
          // dcmsg is business data only. Legacy call frames are deliberately
          // dropped rather than kept as a second signalling stack.
          if (
            msg === null ||
            msg.channelId !== channelId ||
            isCallSignalling(msg)
          ) {
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
          // Bounce business data so the sender sees its own message.
          dc.send(encodeDCMsg({ ...msg, echo: true }));
        };
      },
    );
  }, [sessions, me?.subscriberId]);

  // Call signalling has no DataChannel echo: accepted inbound events fold
  // straight into the same store, while sendTo records the local copy after
  // the WebSocket write succeeds. Validate both layers of addressing because
  // the shared socket is not inherently bound to one peer pair.
  useEffect(() => {
    return sessions.subscribeCall((event) => {
      const c2c = event.c2CEv;
      if (c2c?.call === undefined) return;
      const msg = decodeCallSignalling(c2c.call);
      if (
        msg === null ||
        msg.echo === true ||
        msg.channelId !== c2c.channelId ||
        msg.fromSubscriberId !== c2c.fromSubscriber ||
        msg.toSubscriberId !== c2c.toSubscriber
      ) {
        return;
      }
      setDcMsgs((prev) => applyDCMsg(prev, msg));
    });
  }, [sessions]);

  const sendTo = useCallback(
    (msg: DCMsg) => {
      if (isCallSignalling(msg)) {
        void sessions
          .sendCall(msg.channelId, msg.toSubscriberId, msg)
          .then(() => {
            setDcMsgs((prev) => applyDCMsg(prev, { ...msg, echo: true }));
          })
          .catch((err) => {
            console.warn(
              `datachannel: call signalling ${msg.msgId} not sent`,
              err,
            );
          });
        return;
      }
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
