"use client";

import { useEffect } from "react";
import { useAudioGraph } from "@/api/audio/audiograph";
import {
  newChatControlDCMsg,
  newDCMsg,
  newFileTransferDCMsg,
  useDataChannel,
} from "@/api/ss/datachannel";
import { useBinaryDataChannel } from "@/api/ss/binarydatachannel";
import { usePeerSessions } from "@/api/ss/peersessions";
import { useSignalling } from "@/api/ss/react";
import ChatApp from "@/components/chat/ChatApp";
import { buildMagicControl, parseMagicCommand } from "@/components/chat/magic";
import { conversationKey, type TransferKind } from "@/components/chat/types";
import { useCallMedia } from "@/components/chat/useCallMedia";
import { useCallVolumes } from "@/components/chat/useCallVolumes";
import { useChatMessages } from "@/components/chat/useChatMessages";
import { useChatUsers } from "@/components/chat/useChatUsers";
import { useConversationNavigation } from "@/components/chat/useConversationNavigation";
import { usePhoneCalls } from "@/components/chat/usePhoneCalls";

// The chat page: it owns all chat state. The sidebar's channels and
// their members come live from the signalling server (useSignalling),
// the peer connections to fellow members are usePeerSessions', and the
// three data-channel consumers build on them — messages travel as DCMsgs
// (useDataChannel), file bytes as binary frames (useBinaryDataChannel),
// and voice-call media rides the same connections' tracks (useCallMedia)
// while the calls themselves are invitation DCMsgs (usePhoneCalls). Our
// own messages come back as echoes over the same channel. ChatApp under
// it is a pure controlled component.

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

  const sessions = usePeerSessions(me, channels);
  const { dcMsgs, sendTo } = useDataChannel(me, sessions);
  const { sendFile, getFileByFileId } = useBinaryDataChannel(sessions);

  // All known users by id, including the current user (see useChatUsers).
  const users = useChatUsers(me, channels, dcMsgs);

  // The open conversation and its selection live in the ?conversation=
  // query param (see useConversationNavigation).
  const { selected, select } = useConversationNavigation(me, users, channels);

  // The conversations' messages, oldest first, keyed by conversation key
  // (see useChatMessages).
  const messages = useChatMessages(dcMsgs);

  // The voice calls: the session protocol (invitations and session
  // events) rides the messaging channel and usePhoneCalls folds it into
  // the per-pair call states — the cause — while the log entries'
  // statuses follow via chat-control amends — the UI state. Media of
  // the accepted calls rides the peer connections' tracks through the
  // audio graph (useCallMedia); the two volumes are the graph's gain
  // nodes (useCallVolumes).
  const audio = useAudioGraph();
  const { calls, startCall, acceptCall, rejectCall, hangupCall } =
    usePhoneCalls(me, dcMsgs, sessions, sendTo, audio);
  const { localAnalyser, remoteAnalyserFor } = useCallMedia(
    sessions,
    audio,
    calls,
  );
  const { localVolume, remoteVolume, setLocalVolume, setRemoteVolume } =
    useCallVolumes(audio);

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

  // handleAttachFile sends each picked file: first a file-transfer
  // status message (pending) over the messaging channel announces the
  // transfer to both ends, then sendFile streams the file's bytes over
  // the binary channel. kind is the attach-menu picker's explicit
  // choice (file / image / video): how the transfer is announced and
  // rendered. Every status the transfer's reader yields amends the
  // announce via a chat-control message — the receiver's echo applies
  // it to our own history — so both UIs render the progress the
  // receiver actually acknowledges, in real time.
  const handleAttachFile = (files: File[], kind: TransferKind) => {
    const ref = selected;
    const selfId = me?.subscriberId;
    if (ref === null || selfId === undefined) return;
    for (const file of files) {
      const fileId = crypto.randomUUID();
      const announce = newFileTransferDCMsg(ref.channelId, selfId, ref.userId, {
        fileId,
        kind,
        filename: file.name,
        // The File API leaves type empty for unrecognized extensions.
        fileMIMEType: file.type || "application/octet-stream",
        fileSizeTotalBytes: file.size,
        fileSizeTransferred: 0,
        fileTransferStatus: "pending",
      });
      sendTo(announce);
      void (async () => {
        try {
          const reader = sendFile(
            ref.channelId,
            ref.userId,
            fileId,
            file,
            kind,
          );
          for (;;) {
            const { done, value } = await reader.read();
            if (done) break;
            sendTo(
              newChatControlDCMsg(ref.channelId, selfId, ref.userId, {
                subtype: "amend",
                targetMessageId: announce.msgId,
                fileTransfer: value,
              }),
            );
          }
        } catch (err) {
          console.error(`[chat] file transfer failed (fileId=${fileId})`, err);
        }
      })();
    }
  };

  // handleRequestFile is the click handler of a completed file-transfer
  // card: it downloads the file the binary channel transferred. The
  // binary channel moves bytes only, so the download's filename comes
  // from the status message in the history.
  const handleRequestFile = (fileId: string) => {
    const blob = getFileByFileId(fileId);
    if (blob === undefined) {
      // The sender's "done" can outrun the last binary frames — status
      // rides the messaging channel, bytes the binary one — so the file
      // may simply not be fully here yet.
      alert(`[chat] file not available yet: ${fileId}`);
      return;
    }
    const status = Object.values(messages)
      .flat()
      .find((m) => m.type === "file-transfer-status" && m.fileId === fileId);
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download =
      (status?.type === "file-transfer-status" && status.filename) || fileId;
    a.click();
    // Revoke asynchronously: revoking in the click handler itself can
    // race the download's start.
    setTimeout(() => URL.revokeObjectURL(url), 1000);
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
      getFileByFileId={getFileByFileId}
      calls={calls}
      onStartCall={startCall}
      onAcceptCall={acceptCall}
      onRejectCall={rejectCall}
      onEndCall={hangupCall}
      localAnalyser={localAnalyser}
      remoteAnalyserFor={remoteAnalyserFor}
      localVolume={localVolume}
      remoteVolume={remoteVolume}
      onLocalVolumeChange={setLocalVolume}
      onRemoteVolumeChange={setRemoteVolume}
    />
  );
}
