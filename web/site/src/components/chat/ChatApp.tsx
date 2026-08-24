"use client";

import { useState } from "react";
import { Paper } from "@mui/material";
import type { ChatChannel, ChatUser } from "@/api/ss/types";
import ChatSidebar from "./ChatSidebar";
import ConversationView from "./ConversationView";
import {
  conversationKey,
  type ChatMessage,
  type Conversation,
  type ConversationRef,
} from "./types";

// ChatApp is the chat page's root: it lays out the sidebar next to the
// conversation inside one rounded surface filling the viewport below the top
// bar.
//
// Layout math: the surface is as tall as the viewport minus the 48px dense
// TopBar and the root layout's vertical padding (16px per side on xs, 24px
// on sm and up — see app/layout.tsx).
//
// On phone-sized viewports the sidebar and the conversation take turns
// filling the surface (mobileListOpen picks which); on sm and up both are
// visible side by side.
//
// ChatApp is a pure controlled component: every piece of data — channels,
// users, the selection, messages, unread counts — arrives via props, and
// every user action is reported back through a callback (onSelect, onSend)
// for the parent to handle (see chat/page.tsx). The only state ChatApp owns
// is mobileListOpen, pure viewport chrome.
type ChatAppProps = {
  // The sidebar's channel list; a channel's members are the DM targets.
  channels: ChatChannel[];
  // All known users by id, including the current user.
  users: Record<string, ChatUser>;
  // The local chatter's identity — the author of your sent messages.
  currentUserId: string;
  // The open conversation, or null when none is selected — the pane shows
  // a placeholder then.
  selected: ConversationRef | null;
  onSelect: (ref: ConversationRef) => void;
  // Messages per conversation, keyed by conversationKey (see types.ts),
  // oldest first.
  messages: Record<string, ChatMessage[]>;
  // Unseen-message counts per conversation key.
  unread: Record<string, number>;
  // Reports a message the user sent; the parent owns appending it (and any
  // reply) to `messages`.
  onSend: (content: string) => void;
  // Reports the files the user attached in the composer; the parent owns
  // the resulting messages.
  onAttachFile: (files: File[]) => void;
  // Reports a click on a completed file-transfer card: the user wants the
  // file's bytes.
  onRequestFile: (fileId: string) => void;
};

export default function ChatApp({
  channels,
  users,
  currentUserId,
  selected,
  onSelect,
  messages,
  unread,
  onSend,
  onAttachFile,
  onRequestFile,
}: ChatAppProps) {
  const [mobileListOpen, setMobileListOpen] = useState(false);

  const conversation: Conversation | null =
    selected === null
      ? null
      : {
          kind: "dm",
          channelId: selected.channelId,
          user: users[selected.userId] ?? users[currentUserId],
        };

  const activeKey = selected === null ? null : conversationKey(selected);
  const activeMessages = activeKey === null ? [] : (messages[activeKey] ?? []);

  const handleSelect = (ref: ConversationRef) => {
    onSelect(ref);
    setMobileListOpen(false);
  };

  return (
    <Paper
      elevation={0}
      sx={{
        display: "flex",
        height: { xs: "calc(100dvh - 80px)", sm: "calc(100dvh - 96px)" },
        minHeight: 360,
        border: 1,
        borderColor: "divider",
        // borderRadius: 2 = 24px (2 × theme.shape.borderRadius) — one step
        // rounder than the site's plain cards, quieter than the first cut's
        // 36px.
        borderRadius: 2,
        overflow: "hidden",
      }}
    >
      <ChatSidebar
        channels={channels}
        unread={unread}
        selected={selected}
        onSelect={handleSelect}
        sx={{ display: { xs: mobileListOpen ? "flex" : "none", sm: "flex" } }}
      />
      <ConversationView
        conversation={conversation}
        messages={activeMessages}
        usersById={users}
        currentUserId={currentUserId}
        onSend={onSend}
        onAttachFile={onAttachFile}
        onRequestFile={onRequestFile}
        onBack={() => setMobileListOpen(true)}
        sx={{ display: { xs: mobileListOpen ? "none" : "flex", sm: "flex" } }}
      />
    </Paper>
  );
}
