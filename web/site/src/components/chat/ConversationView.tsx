"use client";

import {
  Box,
  IconButton,
  Typography,
  type SxProps,
  type Theme,
} from "@mui/material";
import ArrowBackIcon from "@mui/icons-material/ArrowBack";
import ForumOutlinedIcon from "@mui/icons-material/ForumOutlined";
import { useTranslation } from "react-i18next";
import type { ChatUser } from "@/api/ss/types";
import MessageInput from "./MessageInput";
import MessageList from "./MessageList";
import UserAvatar from "./UserAvatar";
import { conversationKey, type ChatMessage, type Conversation } from "./types";

type ConversationViewProps = {
  // The open conversation, or null when none is selected — a placeholder
  // fills the pane then.
  conversation: Conversation | null;
  messages: ChatMessage[];
  usersById: Record<string, ChatUser>;
  currentUserId: string;
  onSend: (content: string) => void;
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
  onBack,
  sx,
}: ConversationViewProps) {
  const { t } = useTranslation();

  if (conversation === null) {
    // No conversation selected: a centered placeholder fills the pane.
    // Same colors as MessageList's empty state (text.secondary on the
    // container, icon dimmed via opacity) so the two placeholders match.
    return (
      <Box
        sx={[
          {
            flexGrow: 1,
            minWidth: 0,
            minHeight: 0,
            display: "flex",
            flexDirection: "column",
            alignItems: "center",
            justifyContent: "center",
            gap: 1,
            color: "text.secondary",
          },
          ...(Array.isArray(sx) ? sx : [sx]),
        ]}
      >
        <ForumOutlinedIcon sx={{ fontSize: 40, opacity: 0.6 }} />
        <Typography variant="body2">{t("chat.selectConversation")}</Typography>
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
      </Box>
      <MessageList
        messages={messages}
        usersById={usersById}
        currentUserId={currentUserId}
        conversationKey={conversationKey(ref)}
      />
      <MessageInput target={target} onSend={onSend} />
    </Box>
  );
}
