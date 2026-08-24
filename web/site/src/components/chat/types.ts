// Shared chat domain types. ChatUser and ChatChannel live with the
// signalling types in @/api/ss/types.

import type { ChatUser } from "@/api/ss/types";

// TextChatMessage is one line of plain-text chat.
export type TextChatMessage = {
  type: "text-chat";
  id: string;
  authorId: string;
  content: string;
  // Unix seconds, matching Comment.creation_time elsewhere in the app.
  timestamp: number;
};

// FileTransferStatusMessage carries the UI state of a file transfer — the
// file's bytes never travel in this message; it exists purely so both ends
// can render the transfer's progress.
export type FileTransferStatusMessage = {
  type: "file-transfer-status";
  id: string;
  authorId: string;
  // Opaque, globally unique identifier of the transferred file; the handle
  // a recipient passes back (e.g. via onRequestFile) to fetch the bytes.
  fileId: string;
  filename: string;
  fileMIMEType: string;
  fileSizeTotalBytes: number;
  fileSizeTransferred: number;
  fileTransferStatus: "pending" | "running" | "done";
  // Unix seconds, matching TextChatMessage.timestamp.
  timestamp: number;
};

// ChatMessage is any message renderable in a conversation.
export type ChatMessage = TextChatMessage | FileTransferStatusMessage;

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
