"use client";

import {
  Box,
  IconButton,
  Typography,
  type SxProps,
  type Theme,
} from "@mui/material";
import ArrowBackIcon from "@mui/icons-material/ArrowBack";
import TagIcon from "@mui/icons-material/Tag";
import { useTranslation } from "react-i18next";
import MessageInput from "./MessageInput";
import MessageList from "./MessageList";
import UserAvatar from "./UserAvatar";
import {
  conversationKey,
  type ChatMessage,
  type ChatUser,
  type Conversation,
} from "./types";

type ConversationViewProps = {
  conversation: Conversation;
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

  const ref =
    conversation.kind === "channel"
      ? { kind: "channel" as const, channelId: conversation.channel.id }
      : { kind: "dm" as const, userId: conversation.user.id };
  const target =
    conversation.kind === "channel"
      ? `#${conversation.channel.name}`
      : `@${conversation.user.name}`;

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
      {/* Conversation header: back button (phones only), then the channel's
          name+topic or the DM partner's avatar+name+presence. */}
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
        {conversation.kind === "channel" ? (
          <>
            <Box
              sx={{
                width: 36,
                height: 36,
                borderRadius: 2,
                bgcolor: "action.hover",
                display: "flex",
                alignItems: "center",
                justifyContent: "center",
                flexShrink: 0,
              }}
            >
              <TagIcon sx={{ color: "text.secondary" }} />
            </Box>
            <Box sx={{ minWidth: 0 }}>
              <Typography variant="subtitle1" noWrap sx={{ fontWeight: 600 }}>
                {conversation.channel.name}
              </Typography>
              {conversation.channel.topic && (
                <Typography variant="caption" color="text.secondary" noWrap>
                  {conversation.channel.topic}
                </Typography>
              )}
            </Box>
          </>
        ) : (
          <>
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
          </>
        )}
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
