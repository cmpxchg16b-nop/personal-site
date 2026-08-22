"use client";

import { useEffect, useMemo, useRef, useState } from "react";
import { Paper } from "@mui/material";
import { useTranslation } from "react-i18next";
import ChatSidebar from "./ChatSidebar";
import ConversationView from "./ConversationView";
import {
  chatChannels,
  chatUsers,
  CURRENT_USER_ID,
  DEFAULT_CONVERSATION,
  mockMessages,
  mockReplies,
  mockUnread,
} from "./mockData";
import {
  conversationKey,
  type ChatMessage,
  type ChatUser,
  type Conversation,
  type ConversationRef,
} from "./types";

// ChatApp is the chat page's root: it owns the shared state (selection,
// messages, unread counts) and lays out the sidebar next to the conversation
// inside one rounded surface filling the viewport below the top bar.
//
// Layout math: the surface is as tall as the viewport minus the 48px dense
// TopBar and the root layout's vertical padding (16px per side on xs, 24px
// on sm and up — see app/layout.tsx).
//
// On phone-sized viewports the sidebar and the conversation take turns
// filling the surface (mobileListOpen picks which); on sm and up both are
// visible side by side.
//
// Sending a message appends it immediately; a mock reply from a channel
// member (or the DM partner) follows a second or two later — stage 1 has no
// backend, so this stands in for one.
export default function ChatApp() {
  const { t } = useTranslation();
  const [selected, setSelected] =
    useState<ConversationRef>(DEFAULT_CONVERSATION);
  const [messagesByConv, setMessagesByConv] =
    useState<Record<string, ChatMessage[]>>(mockMessages);
  // The conversation open by default starts already-read.
  const [unread, setUnread] = useState<Record<string, number>>(() => ({
    ...mockUnread,
    [conversationKey(DEFAULT_CONVERSATION)]: 0,
  }));
  const [mobileListOpen, setMobileListOpen] = useState(false);

  // Mirror of `selected` for the reply timer callbacks, which close over
  // stale state otherwise.
  const selectedRef = useRef(selected);
  useEffect(() => {
    selectedRef.current = selected;
  }, [selected]);

  const idCounter = useRef(0);
  const nextId = () => `local-${Date.now()}-${idCounter.current++}`;

  const replyTimers = useRef<number[]>([]);
  useEffect(
    () => () => {
      replyTimers.current.forEach((t) => window.clearTimeout(t));
    },
    [],
  );

  // The current user's display name is localized ("You"/"我"), so the
  // identity map is rebuilt when the language changes.
  const usersById: Record<string, ChatUser> = useMemo(
    () => ({
      ...chatUsers,
      [CURRENT_USER_ID]: { ...chatUsers[CURRENT_USER_ID], name: t("chat.you") },
    }),
    [t],
  );

  const conversation: Conversation =
    selected.kind === "channel"
      ? {
          kind: "channel",
          channel:
            chatChannels.find((c) => c.id === selected.channelId) ??
            chatChannels[0],
        }
      : {
          kind: "dm",
          user: usersById[selected.userId] ?? usersById[CURRENT_USER_ID],
        };

  const activeKey = conversationKey(selected);
  const messages = messagesByConv[activeKey] ?? [];

  const handleSelect = (ref: ConversationRef) => {
    setSelected(ref);
    setMobileListOpen(false);
    setUnread((prev) => ({ ...prev, [conversationKey(ref)]: 0 }));
  };

  const scheduleMockReply = (ref: ConversationRef) => {
    // Channels have no chat of their own at this moment, so only DMs get a
    // mock reply — from the DM partner.
    if (ref.kind !== "dm") return;
    const key = conversationKey(ref);
    const authorId = ref.userId;
    const content = mockReplies[Math.floor(Math.random() * mockReplies.length)];
    const timer = window.setTimeout(
      () => {
        setMessagesByConv((prev) => ({
          ...prev,
          [key]: [
            ...(prev[key] ?? []),
            { id: nextId(), authorId, content, timestamp: Date.now() / 1000 },
          ],
        }));
        // A reply landing while you look at another conversation counts as
        // unread there.
        if (conversationKey(selectedRef.current) !== key) {
          setUnread((prev) => ({ ...prev, [key]: (prev[key] ?? 0) + 1 }));
        }
      },
      900 + Math.random() * 1200,
    );
    replyTimers.current.push(timer);
  };

  const handleSend = (content: string) => {
    const ref = selected;
    const key = conversationKey(ref);
    setMessagesByConv((prev) => ({
      ...prev,
      [key]: [
        ...(prev[key] ?? []),
        {
          id: nextId(),
          authorId: CURRENT_USER_ID,
          content,
          timestamp: Date.now() / 1000,
        },
      ],
    }));
    scheduleMockReply(ref);
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
        channels={chatChannels}
        unread={unread}
        selected={selected}
        onSelect={handleSelect}
        sx={{ display: { xs: mobileListOpen ? "flex" : "none", sm: "flex" } }}
      />
      <ConversationView
        conversation={conversation}
        messages={messages}
        usersById={usersById}
        currentUserId={CURRENT_USER_ID}
        onSend={handleSend}
        onBack={() => setMobileListOpen(true)}
        sx={{ display: { xs: mobileListOpen ? "none" : "flex", sm: "flex" } }}
      />
    </Paper>
  );
}
