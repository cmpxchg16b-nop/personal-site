"use client";

import {
  Box,
  IconButton,
  Typography,
  type SxProps,
  type Theme,
} from "@mui/material";
import ArrowBackIcon from "@mui/icons-material/ArrowBack";
import CallEndIcon from "@mui/icons-material/CallEnd";
import CallIcon from "@mui/icons-material/Call";
import ForumOutlinedIcon from "@mui/icons-material/ForumOutlined";
import { useTranslation } from "react-i18next";
import type { ChatUser } from "@/api/ss/types";
import { CallPanel } from "./CallPanel";
import MessageInput from "./MessageInput";
import MessageList from "./MessageList";
import UserAvatar from "./UserAvatar";
import {
  conversationKey,
  type ActivePhoneCall,
  type ChatMessage,
  type Conversation,
  type TransferKind,
} from "./types";

type ConversationViewProps = {
  // The open conversation, or null when none is selected — a placeholder
  // fills the pane then.
  conversation: Conversation | null;
  messages: ChatMessage[];
  usersById: Record<string, ChatUser>;
  currentUserId: string;
  onSend: (content: string) => void;
  // onAttachFile reports the files picked in the composer's attach menu,
  // with the picker's kind.
  onAttachFile: (files: File[], kind: TransferKind) => void;
  // onRequestFile asks for a completed transfer's bytes by fileId.
  onRequestFile: (fileId: string) => void;
  // getFileByFileId resolves a completed transfer's bytes locally; the
  // media cards render from it (see MediaMessageItem).
  getFileByFileId: (fileId: string) => Blob | undefined;
  // The live voice call with this conversation's peer, or null (see
  // usePhoneCalls).
  call: ActivePhoneCall | null;
  // Rings the peer (the header's call button).
  onStartCall: () => void;
  // Hangs the live call up — a cancel while ringing, an end in call.
  onEndCall: () => void;
  // FFT taps of the two voices while in call (see useCallMedia).
  localAnalyser: AnalyserNode | null;
  remoteAnalyser: AnalyserNode | null;
  // onBack returns to the channel list; only reachable on phone-sized
  // viewports where the sidebar and the conversation don't share the screen.
  onBack: () => void;
  // Responsive visibility is controlled by the parent (ChatApp) through sx.
  sx?: SxProps<Theme>;
};

// ConversationView is the right-hand side of the chat: a header naming the
// conversation, the scrollable message history, and the composer.
export default function ConversationView({
  conversation,
  messages,
  usersById,
  currentUserId,
  onSend,
  onAttachFile,
  onRequestFile,
  getFileByFileId,
  call,
  onStartCall,
  onEndCall,
  localAnalyser,
  remoteAnalyser,
  onBack,
  sx,
}: ConversationViewProps) {
  const { t } = useTranslation();

  if (conversation === null) {
    // No conversation selected: a centered placeholder fills the pane.
    // Same colors as MessageList's empty state (text.secondary on the
    // container, icon dimmed via opacity) so the two placeholders match.
    // On phones the placeholder gets the conversation header's back
    // button too — without it the channel list is unreachable when
    // nothing is selected. The whole header is hidden on sm and up,
    // where the sidebar is always visible.
    return (
      <Box
        sx={[
          {
            flexGrow: 1,
            minWidth: 0,
            minHeight: 0,
            display: "flex",
            flexDirection: "column",
          },
          ...(Array.isArray(sx) ? sx : [sx]),
        ]}
      >
        <Box
          sx={{
            display: { xs: "flex", sm: "none" },
            alignItems: "center",
            px: 2,
            py: 1.25,
            borderBottom: 1,
            borderColor: "divider",
            flexShrink: 0,
          }}
        >
          <IconButton onClick={onBack} aria-label={t("chat.back")}>
            <ArrowBackIcon />
          </IconButton>
        </Box>
        <Box
          sx={{
            flexGrow: 1,
            minHeight: 0,
            display: "flex",
            flexDirection: "column",
            alignItems: "center",
            justifyContent: "center",
            gap: 1,
            color: "text.secondary",
          }}
        >
          <ForumOutlinedIcon sx={{ fontSize: 40, opacity: 0.6 }} />
          <Typography variant="body2">
            {t("chat.selectConversation")}
          </Typography>
        </Box>
      </Box>
    );
  }

  const ref = {
    kind: "dm" as const,
    channelId: conversation.channelId,
    userId: conversation.user.id,
  };
  const target = `@${conversation.user.name}`;

  return (
    <Box
      sx={[
        {
          flexGrow: 1,
          minWidth: 0,
          display: "flex",
          flexDirection: "column",
          minHeight: 0,
        },
        ...(Array.isArray(sx) ? sx : [sx]),
      ]}
    >
      {/* Conversation header: back button (phones only), then the DM
          partner's avatar+name+presence. */}
      <Box
        sx={{
          display: "flex",
          alignItems: "center",
          gap: 1.5,
          px: 2,
          py: 1.25,
          borderBottom: 1,
          borderColor: "divider",
          flexShrink: 0,
        }}
      >
        <IconButton
          onClick={onBack}
          aria-label={t("chat.back")}
          sx={{ display: { xs: "inline-flex", sm: "none" } }}
        >
          <ArrowBackIcon />
        </IconButton>
        <UserAvatar user={conversation.user} size={36} showPresence />
        <Box sx={{ minWidth: 0 }}>
          <Typography variant="subtitle1" noWrap sx={{ fontWeight: 600 }}>
            {conversation.user.name}
          </Typography>
          <Typography
            variant="caption"
            color="text.secondary"
            noWrap
            sx={{ display: "flex", alignItems: "center", gap: 0.5 }}
          >
            {t(conversation.user.online ? "chat.online" : "chat.offline")}
          </Typography>
        </Box>
        {/* The call button, at the header's right end: it rings the peer,
            or hangs the live call up. Incoming calls are answered from
            the global popup, so while one rings this button just shows
            the ringing state. */}
        {call === null ? (
          <IconButton
            onClick={onStartCall}
            disabled={!conversation.user.online}
            aria-label={t("chat.call.start")}
            sx={{ ml: "auto" }}
          >
            <CallIcon />
          </IconButton>
        ) : call.incoming && call.status === "inviting" ? (
          <IconButton
            disabled
            aria-label={t("chat.call.incoming")}
            sx={{ ml: "auto" }}
          >
            <CallIcon />
          </IconButton>
        ) : (
          <IconButton
            onClick={onEndCall}
            aria-label={t("chat.call.end")}
            sx={{ ml: "auto", color: "error.main" }}
          >
            <CallEndIcon />
          </IconButton>
        )}
      </Box>
      {call !== null && (
        <CallPanel
          call={call}
          peerName={conversation.user.name}
          localAnalyser={localAnalyser}
          remoteAnalyser={remoteAnalyser}
          onEnd={onEndCall}
        />
      )}
      <MessageList
        messages={messages}
        usersById={usersById}
        currentUserId={currentUserId}
        conversationKey={conversationKey(ref)}
        onRequestFile={onRequestFile}
        getFileByFileId={getFileByFileId}
      />
      <MessageInput
        target={target}
        onSend={onSend}
        onAttachFile={onAttachFile}
      />
    </Box>
  );
}
