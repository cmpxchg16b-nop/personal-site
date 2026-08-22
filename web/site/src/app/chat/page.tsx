"use client";

import { useEffect, useRef, useState } from "react";
import { useSignalling } from "@/api/ss/react";
import ChatApp from "@/components/chat/ChatApp";
import {
  chatChannels,
  chatUsers,
  CURRENT_USER_ID,
  DEFAULT_CONVERSATION,
  mockMessages,
  mockReplies,
  mockUnread,
} from "@/components/chat/mockData";
import {
  conversationKey,
  type ChatMessage,
  type ConversationRef,
} from "@/components/chat/types";

// The chat page: it owns all chat state and plays backend. Stage 1 is
// frontend-only, so the state is seeded from mockData.ts, and sending a
// message appends it immediately with a canned reply from the DM partner
// following a second or two later — a real API will eventually take over
// both sides of handleSend. ChatApp under it is a pure controlled component.
export default function ChatPage() {
  // Latency monitor: the latest answered ping of the signalling server
  // connection, re-measured once a second.
  const { lastPing } = useSignalling();
  useEffect(() => {
    if (lastPing !== null) {
      console.log(
        `[chat] signalling server RTT: ${lastPing.rtt.toFixed(1)} ms ` +
          `(ping_id=${lastPing.id} ping_seq=${lastPing.seq} ` +
          `at=${new Date(lastPing.at).toISOString()})`,
      );
    }
  }, [lastPing]);

  const [selected, setSelected] =
    useState<ConversationRef>(DEFAULT_CONVERSATION);
  const [messages, setMessages] =
    useState<Record<string, ChatMessage[]>>(mockMessages);
  // The conversation open by default starts already-read.
  const [unread, setUnread] = useState<Record<string, number>>(() => ({
    ...mockUnread,
    [conversationKey(DEFAULT_CONVERSATION)]: 0,
  }));

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

  const handleSelect = (ref: ConversationRef) => {
    setSelected(ref);
    setUnread((prev) => ({ ...prev, [conversationKey(ref)]: 0 }));
  };

  // Stands in for the backend: a canned reply from the DM partner, a second
  // or two after you send something.
  const scheduleMockReply = (ref: ConversationRef) => {
    // Channels have no chat of their own at this moment, so only DMs get a
    // mock reply.
    if (ref.kind !== "dm") return;
    const key = conversationKey(ref);
    const authorId = ref.userId;
    const content = mockReplies[Math.floor(Math.random() * mockReplies.length)];
    const timer = window.setTimeout(
      () => {
        setMessages((prev) => ({
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
    setMessages((prev) => ({
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
    <ChatApp
      channels={chatChannels}
      users={chatUsers}
      currentUserId={CURRENT_USER_ID}
      selected={selected}
      onSelect={handleSelect}
      messages={messages}
      unread={unread}
      onSend={handleSend}
    />
  );
}
