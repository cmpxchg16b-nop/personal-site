"use client";

import { useEffect, useMemo, useRef, useState } from "react";
import { usePathname, useRouter, useSearchParams } from "next/navigation";
import { useSignalling } from "@/api/ss/react";
import type { ChatUser } from "@/api/ss/types";
import ChatApp from "@/components/chat/ChatApp";
import {
  mockMessages,
  mockReplies,
  mockUnread,
} from "@/components/chat/mockData";
import {
  conversationKey,
  parseConversationKey,
  type ChatMessage,
  type ConversationRef,
} from "@/components/chat/types";

// The chat page: it owns all chat state and plays backend. The sidebar's
// channels and their members come live from the signalling server (see
// useSignalling); messages are still mock — sending a message appends it
// immediately with a canned reply from the DM partner following a second
// or two later, until a real API takes over both sides of handleSend.
// ChatApp under it is a pure controlled component.
export default function ChatPage() {
  // Latency monitor: the latest answered ping of the signalling server
  // connection, re-measured once a second.
  const { lastPing, channels, me } = useSignalling();
  useEffect(() => {
    if (lastPing !== null) {
      console.log(
        `[chat] signalling server RTT: ${lastPing.rtt.toFixed(1)} ms ` +
          `(ping_id=${lastPing.id} ping_seq=${lastPing.seq} ` +
          `at=${new Date(lastPing.at).toISOString()})`,
      );
    }
  }, [lastPing]);

  // All known users by id, including the current user: the live channel
  // members plus ourselves once registered.
  const users: Record<string, ChatUser> = useMemo(() => {
    const byId: Record<string, ChatUser> = {};
    if (me !== null) {
      byId[me.id] = me;
    }
    for (const channel of channels) {
      for (const member of channel.members) {
        byId[member.id] = member;
      }
    }
    return byId;
  }, [channels, me]);

  // The open conversation lives in the ?conversation= query param (its
  // conversationKey), so every selection is a browser-history entry and
  // the back button walks back through them. A missing or unknown value
  // means nothing is selected, and the pane shows its placeholder.
  const searchParams = useSearchParams();
  const router = useRouter();
  const pathname = usePathname();
  const conversationParam = searchParams.get("conversation");
  const selected: ConversationRef | null = useMemo(() => {
    const ref = parseConversationKey(conversationParam);
    // Only known users other than yourself, reached from a known
    // channel, are openable. While disconnected the listing is empty and
    // nothing is openable; the query param survives, so the conversation
    // reopens once the listing returns.
    if (
      ref === null ||
      me === null ||
      ref.userId === me.id ||
      users[ref.userId] === undefined ||
      !channels.some((c) => c.id === ref.channelId)
    ) {
      return null;
    }
    return ref;
  }, [conversationParam, users, channels, me]);
  const [messages, setMessages] =
    useState<Record<string, ChatMessage[]>>(mockMessages);
  const [unread, setUnread] = useState<Record<string, number>>(() => ({
    ...mockUnread,
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
    setUnread((prev) => ({ ...prev, [conversationKey(ref)]: 0 }));
    const params = new URLSearchParams(searchParams.toString());
    if (conversationKey(ref) === conversationParam) {
      // Selecting the open conversation again deselects it.
      params.delete("conversation");
    } else {
      params.set("conversation", conversationKey(ref));
    }
    // Push, not replace: each selection change is a history entry, so
    // the browser's back button walks back through them.
    const query = params.toString();
    router.push(query === "" ? pathname : `${pathname}?${query}`, {
      scroll: false,
    });
  };

  // Stands in for the backend: a canned reply from the DM partner, a second
  // or two after you send something.
  const scheduleMockReply = (ref: ConversationRef) => {
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
        // A reply landing while you look at another conversation — or
        // none — counts as unread there.
        const openKey =
          selectedRef.current === null
            ? null
            : conversationKey(selectedRef.current);
        if (openKey !== key) {
          setUnread((prev) => ({ ...prev, [key]: (prev[key] ?? 0) + 1 }));
        }
      },
      900 + Math.random() * 1200,
    );
    replyTimers.current.push(timer);
  };

  const handleSend = (content: string) => {
    // The composer only exists while a conversation is open, so a null
    // selection (or a missing registration) here is unreachable.
    const ref = selected;
    if (ref === null || me === null) return;
    const key = conversationKey(ref);
    setMessages((prev) => ({
      ...prev,
      [key]: [
        ...(prev[key] ?? []),
        {
          id: nextId(),
          authorId: me.id,
          content,
          timestamp: Date.now() / 1000,
        },
      ],
    }));
    scheduleMockReply(ref);
  };

  return (
    <ChatApp
      channels={channels}
      users={users}
      currentUserId={me?.id ?? ""}
      selected={selected}
      onSelect={handleSelect}
      messages={messages}
      unread={unread}
      onSend={handleSend}
    />
  );
}
