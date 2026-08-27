// Shared chat domain types. ChatUser and ChatChannel live with the
// signalling types in @/api/ss/types.

import type { ChatUser } from "@/api/ss/types";
import type {
  DCFileTransferKind,
  DCPhoneSessionStatus,
} from "@/api/ss/datachannel";

// TransferKind is how a transfer is announced and rendered: a plain file
// download card ("file") or an inline media card ("image", "video"). The
// attach menu's picker chooses it, and it travels in the DCFileTransfer
// body — never derived from the file's MIME type.
export type TransferKind = DCFileTransferKind;

// TextChatMessage is one line of plain-text chat.
export type TextChatMessage = {
  type: "text-chat";
  id: string;
  authorId: string;
  content: string;
  // Unix seconds, matching Comment.creation_time elsewhere in the app.
  timestamp: number;
};

// TransferMessageFields are the fields every transfer-backed message
// carries: the UI state of one file transfer over the binary data
// channel. The file's bytes never travel in the message — the opaque,
// globally unique fileId is the handle getFileByFileId
// (useBinaryDataChannel) resolves them with — the message exists purely
// so both ends can render the transfer's progress.
export type TransferMessageFields = {
  fileId: string;
  filename: string;
  fileMIMEType: string;
  fileSizeTotalBytes: number;
  fileSizeTransferred: number;
  fileTransferStatus: "pending" | "running" | "done";
};

// FileTransferStatusMessage is a generic file transfer, rendered as a
// download card (see FileTransferStatusItem).
export type FileTransferStatusMessage = TransferMessageFields & {
  type: "file-transfer-status";
  id: string;
  authorId: string;
  // Unix seconds, matching TextChatMessage.timestamp.
  timestamp: number;
};

// ImageChatMessage is a file transfer whose payload is an image (its
// fileMIMEType is image/*), rendered as an inline image card with a
// preview dialog once the transfer completes (see MediaMessageItem).
export type ImageChatMessage = TransferMessageFields & {
  type: "image-chat";
  id: string;
  authorId: string;
  timestamp: number;
};

// VideoChatMessage is a file transfer whose payload is a video (its
// fileMIMEType is video/*), rendered as an inline video card with a
// preview dialog once the transfer completes (see MediaMessageItem).
export type VideoChatMessage = TransferMessageFields & {
  type: "video-chat";
  id: string;
  authorId: string;
  timestamp: number;
};

// MediaChatMessage is either transfer-backed media message.
export type MediaChatMessage = ImageChatMessage | VideoChatMessage;

// PhoneSessionStatus is the lifecycle state of a voice call — the wire
// type, re-exported so UI components stay free of api imports.
export type PhoneSessionStatus = DCPhoneSessionStatus;

// PhoneCallMessage is the call log entry of one voice call: the
// invitation's arrival rendered in the history, its phoneStatus moving
// with the session ("inviting" → "accepted" → "ended", …) as the
// parties' amends land. The audio itself never travels in the message —
// it flows over the peer connection while the status says "accepted".
export type PhoneCallMessage = {
  type: "phone-call";
  id: string;
  // The caller (the invitation's author).
  authorId: string;
  sessionId: string;
  phoneStatus: PhoneSessionStatus;
  // Unix seconds of the invitation, matching TextChatMessage.timestamp.
  timestamp: number;
};

// ChatMessage is any message renderable in a conversation.
export type ChatMessage =
  | TextChatMessage
  | FileTransferStatusMessage
  | MediaChatMessage
  | PhoneCallMessage;

// ConversationRef identifies the selected conversation: a direct message
// with one user, scoped to the channel the DM was opened from — opening
// the same person from different channels lands in different
// conversations. (Channels only group people; there is no channel-level
// chat at this moment.)
export type ConversationRef = {
  kind: "dm";
  channelId: string;
  userId: string;
};

// Conversation is a ConversationRef resolved to its display target.
export type Conversation = { kind: "dm"; channelId: string; user: ChatUser };

// ActivePhoneCall is the live view of an ongoing voice call with a
// conversation's peer: ringing ("inviting") or in call ("accepted").
// Terminal states are not here — they live in the history as
// PhoneCallMessages. Derived from the conversation's latest phone-call
// message by usePhoneCalls.
export type ActivePhoneCall = {
  // The conversation the call belongs to.
  ref: ConversationRef;
  // The invitation message's id — the target the parties' status amends
  // point at.
  messageId: string;
  sessionId: string;
  status: "inviting" | "accepted";
  // true when the peer is ringing us (we are the callee).
  incoming: boolean;
  // Unix seconds of the invitation.
  since: number;
};

// conversationKey flattens a ConversationRef into the string key under which
// its messages and unread count are stored.
export function conversationKey(ref: ConversationRef): string {
  return `dm:${ref.channelId}:${ref.userId}`;
}

// parseConversationKey is the inverse of conversationKey: it returns the
// ConversationRef a key string encodes, or null when the string is not a
// valid conversation key (e.g. a tampered URL query value).
export function parseConversationKey(
  key: string | null,
): ConversationRef | null {
  if (key === null) return null;
  const parts = key.split(":");
  if (
    parts.length !== 3 ||
    parts[0] !== "dm" ||
    parts[1] === "" ||
    parts[2] === ""
  ) {
    return null;
  }
  return { kind: "dm", channelId: parts[1], userId: parts[2] };
}

// MessageGroup is the view model for MessageItem: a run of consecutive
// text messages by the same author rendered under one avatar and header
// line.
export type MessageGroup = {
  // Stable React key (the first message's id).
  key: string;
  author: ChatUser;
  // Unix seconds; when the group started (shown in the header line).
  startedAt: number;
  messages: TextChatMessage[];
};
