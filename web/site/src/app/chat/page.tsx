"use client";

import { useEffect, useMemo } from "react";
import { usePathname, useRouter, useSearchParams } from "next/navigation";
import { newDCMsg, useDataChannel } from "@/api/ss/datachannel";
import { useSignalling } from "@/api/ss/react";
import type { ChatUser } from "@/api/ss/types";
import ChatApp from "@/components/chat/ChatApp";
import {
  conversationKey,
  parseConversationKey,
  type ChatMessage,
  type ConversationRef,
} from "@/components/chat/types";

// The chat page: it owns all chat state. The sidebar's channels and
// their members come live from the signalling server (useSignalling),
// and messages travel over WebRTC data channels between the peers
// (useDataChannel); our own messages come back as echoes over the same
// channel. ChatApp under it is a pure controlled component.

// NO_UNREAD is the stable empty unread-count record: the mock badge
// source is gone and nothing computes unread counts yet.
const NO_UNREAD: Record<string, number> = {};

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

  const { dcMsgs, sendTo } = useDataChannel(me, channels);

  // All known users by id, including the current user: the live channel
  // members, plus ourselves once registered, plus every data-channel
  // sender — a message must keep rendering even after its author drops
  // out of the listing (MessageList skips messages by unknown authors).
  const users: Record<string, ChatUser> = useMemo(() => {
    const byId: Record<string, ChatUser> = {};
    if (me !== null) {
      byId[me.id] = me;
      // Messages name their author by subscriber id
      // (DCMsg.fromSubscriberId), which is not me's app-level user id:
      // index ourselves under it too, so our own echoed messages
      // resolve to our username (and to us for isOwn).
      if (me.subscriberId !== undefined) {
        byId[me.subscriberId] = me;
      }
    }
    for (const channel of channels) {
      for (const member of channel.members) {
        byId[member.id] = member;
      }
    }
    for (const bySender of Object.values(dcMsgs)) {
      for (const [senderId, msgs] of Object.entries(bySender)) {
        if (byId[senderId] === undefined && msgs.length > 0) {
          // The sender is no longer in the listing; the id doubles as
          // the display name (it is all that is known).
          byId[senderId] = {
            id: senderId,
            name: senderId,
            online: false,
            channelId: msgs[0].channelId,
            subscriberId: senderId,
          };
        }
      }
    }
    return byId;
  }, [me, channels, dcMsgs]);

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

  // The conversations' messages from the data channels, oldest first:
  // the peers' messages plus the echoes of our own (echo set), keyed by
  // conversation key. Derived from dcMsgs at render time — there is
  // nothing to sync.
  const messages: Record<string, ChatMessage[]> = useMemo(() => {
    const merged: Record<string, ChatMessage[]> = {};
    for (const bySender of Object.values(dcMsgs)) {
      for (const senderMsgs of Object.values(bySender)) {
        for (const m of senderMsgs) {
          // The conversation's peer is the message's other end: its
          // sender, or its recipient for an echo of our own message.
          const key = conversationKey({
            kind: "dm",
            channelId: m.channelId,
            userId: m.echo === true ? m.toSubscriberId : m.fromSubscriberId,
          });
          (merged[key] ??= []).push({
            id: m.msgId,
            authorId: m.fromSubscriberId,
            content: m.plaintext,
            timestamp: m.creationTimestamp,
          });
        }
      }
    }
    // Each sender list is oldest-first, but the senders interleave.
    for (const list of Object.values(merged)) {
      list.sort((a, b) => a.timestamp - b.timestamp);
    }
    return merged;
  }, [dcMsgs]);

  const handleSelect = (ref: ConversationRef) => {
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

  const handleSend = (content: string) => {
    // The composer only exists while a conversation is open, so a null
    // selection (or a missing registration) here is unreachable. The
    // message shows up in the conversation when its echo comes back
    // over the data channel.
    const ref = selected;
    if (ref === null || me?.subscriberId === undefined) return;
    sendTo(newDCMsg(ref.channelId, me.subscriberId, ref.userId, content));
  };

  return (
    <ChatApp
      channels={channels}
      users={users}
      currentUserId={me?.id ?? ""}
      selected={selected}
      onSelect={handleSelect}
      messages={messages}
      unread={NO_UNREAD}
      onSend={handleSend}
    />
  );
}
