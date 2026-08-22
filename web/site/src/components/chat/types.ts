// Shared chat domain types. Stage 1 is frontend-only: all data comes from
// mockData.ts, and these types mirror the shapes the chat API will
// eventually serve.

export type ChatUser = {
  id: string;
  name: string;
  online: boolean;
};

export type ChatChannel = {
  id: string;
  name: string;
  topic?: string;
  // The tree's second level: the channel's members (excludes the current
  // user — you don't open a direct message with yourself).
  members: ChatUser[];
};

export type ChatMessage = {
  id: string;
  authorId: string;
  content: string;
  // Unix seconds, matching Comment.creation_time elsewhere in the app.
  timestamp: number;
};

// ConversationRef identifies the selected conversation: a channel room or a
// direct message with one user. A DM is keyed by the user alone, so opening
// the same person from different channels lands in the same conversation.
export type ConversationRef =
  | { kind: "channel"; channelId: string }
  | { kind: "dm"; userId: string };

// Conversation is a ConversationRef resolved to its display target.
export type Conversation =
  | { kind: "channel"; channel: ChatChannel }
  | { kind: "dm"; user: ChatUser };

// conversationKey flattens a ConversationRef into the string key under which
// its messages and unread count are stored.
export function conversationKey(ref: ConversationRef): string {
  return ref.kind === "channel"
    ? `channel:${ref.channelId}`
    : `dm:${ref.userId}`;
}

// MessageGroup is the view model for MessageItem: a run of consecutive
// messages by the same author rendered under one avatar and header line.
export type MessageGroup = {
  // Stable React key (the first message's id).
  key: string;
  author: ChatUser;
  // Unix seconds; when the group started (shown in the header line).
  startedAt: number;
  messages: ChatMessage[];
};
