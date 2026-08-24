"use client";

import { useEffect } from "react";
import {
  DC_FILE_TRANSFER_STATUSES,
  newChatControlDCMsg,
  newDCMsg,
  newFileTransferDCMsg,
  useDataChannel,
} from "@/api/ss/datachannel";
import { useSignalling } from "@/api/ss/react";
import ChatApp from "@/components/chat/ChatApp";
import { buildMagicControl, parseMagicCommand } from "@/components/chat/magic";
import { conversationKey } from "@/components/chat/types";
import { useChatMessages } from "@/components/chat/useChatMessages";
import { useChatUsers } from "@/components/chat/useChatUsers";
import { useConversationNavigation } from "@/components/chat/useConversationNavigation";

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

  // All known users by id, including the current user (see useChatUsers).
  const users = useChatUsers(me, channels, dcMsgs);

  // The open conversation and its selection live in the ?conversation=
  // query param (see useConversationNavigation).
  const { selected, select } = useConversationNavigation(me, users, channels);

  // The conversations' messages, oldest first, keyed by conversation key
  // (see useChatMessages).
  const messages = useChatMessages(dcMsgs);

  const handleSend = (content: string) => {
    // The composer only exists while a conversation is open, so a null
    // selection (or a missing registration) here is unreachable. The
    // message shows up in the conversation when its echo comes back
    // over the data channel.
    const ref = selected;
    if (ref === null || me?.subscriberId === undefined) return;
    const magic = parseMagicCommand(content);
    if (magic !== null) {
      // A magic command never becomes a text message: it is turned into
      // the corresponding chat-control message and sent like any other;
      // the echo applies its effect to our own history.
      const history = messages[conversationKey(ref)] ?? [];
      const control = buildMagicControl(magic, history);
      if (control === null) return;
      sendTo(
        newChatControlDCMsg(
          ref.channelId,
          me.subscriberId,
          ref.userId,
          control,
        ),
      );
      return;
    }
    sendTo(newDCMsg(ref.channelId, me.subscriberId, ref.userId, content));
  };

  // handleAttachFile sends each picked file as a file-transfer status
  // message over the data channel — only the status travels, never the
  // file's bytes. The message shows up in the conversation when its echo
  // comes back, like a text message. The status is picked at random and
  // the progress with it (a pending card shows nothing transferred, a
  // done card the full size): the demo exercises every
  // FileTransferStatusItem state until a real transfer drives them.
  const handleAttachFile = (files: File[]) => {
    const ref = selected;
    if (ref === null || me?.subscriberId === undefined) return;
    for (const file of files) {
      const status =
        DC_FILE_TRANSFER_STATUSES[
          Math.floor(Math.random() * DC_FILE_TRANSFER_STATUSES.length)
        ];
      sendTo(
        newFileTransferDCMsg(ref.channelId, me.subscriberId, ref.userId, {
          // The demo's file id is a fresh UUID; a real transfer would mint
          // it when the file starts moving.
          fileId: crypto.randomUUID(),
          filename: file.name,
          // The File API leaves type empty for unrecognized extensions.
          fileMIMEType: file.type || "application/octet-stream",
          fileSizeTotalBytes: file.size,
          fileSizeTransferred:
            status === "pending"
              ? 0
              : status === "done"
                ? file.size
                : Math.floor(Math.random() * (file.size + 1)),
          fileTransferStatus: status,
        }),
      );
    }
  };

  // handleRequestFile is the click handler of a completed file-transfer
  // card. Fetching the bytes over the data channel isn't wired yet.
  const handleRequestFile = (fileId: string) => {
    alert(`[chat] file requested: ${fileId}`);
  };

  return (
    <ChatApp
      channels={channels}
      users={users}
      currentUserId={me?.id ?? ""}
      selected={selected}
      onSelect={select}
      messages={messages}
      unread={NO_UNREAD}
      onSend={handleSend}
      onAttachFile={handleAttachFile}
      onRequestFile={handleRequestFile}
    />
  );
}
