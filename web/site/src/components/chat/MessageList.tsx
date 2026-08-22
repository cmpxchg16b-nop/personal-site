"use client";

import { useEffect, useMemo, useRef } from "react";
import { Box, Chip, Divider, Typography } from "@mui/material";
import ForumOutlinedIcon from "@mui/icons-material/ForumOutlined";
import { format, isSameDay, isToday, isYesterday } from "date-fns";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { dateFnsLocaleFor } from "@/i18n";
import MessageItem from "./MessageItem";
import type { ChatMessage, ChatUser, MessageGroup } from "./types";

// GROUP_WINDOW_MS: consecutive messages by the same author within this gap
// collapse into one group (one avatar, one header line).
const GROUP_WINDOW_MS = 5 * 60 * 1000;

type MessageListProps = {
  messages: ChatMessage[];
  usersById: Record<string, ChatUser>;
  currentUserId: string;
  // Key of the conversation being shown; a change resets the scroll position
  // instantly instead of animating.
  conversationKey: string;
};

// groupMessages folds the flat oldest-first list into per-author runs.
function groupMessages(
  messages: ChatMessage[],
  usersById: Record<string, ChatUser>,
): MessageGroup[] {
  const groups: MessageGroup[] = [];
  for (const message of messages) {
    const author = usersById[message.authorId];
    if (!author) continue;
    const last = groups[groups.length - 1];
    if (
      last &&
      last.author.id === author.id &&
      message.timestamp * 1000 - last.startedAt * 1000 < GROUP_WINDOW_MS &&
      isSameDay(message.timestamp * 1000, last.startedAt * 1000)
    ) {
      last.messages.push(message);
    } else {
      groups.push({
        key: message.id,
        author,
        startedAt: message.timestamp,
        messages: [message],
      });
    }
  }
  return groups;
}

// dayLabel renders a divider's date: "Today"/"Yesterday" when applicable,
// otherwise a locale-formatted date.
function dayLabel(timestamp: number, t: TFunction, lng: string) {
  const date = new Date(timestamp * 1000);
  if (isToday(date)) return t("chat.today");
  if (isYesterday(date)) return t("chat.yesterday");
  return format(date, "PP", { locale: dateFnsLocaleFor(lng) });
}

// MessageList is the scrollable message history: date dividers between days,
// grouped messages, and an empty state. It keeps itself pinned to the bottom
// — instantly on a conversation switch, smoothly when a new message arrives
// in the conversation already being viewed.
export default function MessageList({
  messages,
  usersById,
  currentUserId,
  conversationKey,
}: MessageListProps) {
  const { t, i18n } = useTranslation();
  const bottomRef = useRef<HTMLDivElement>(null);
  // null until the first effect run, so the initial mount scrolls instantly
  // (no smooth animation on page load).
  const lastKeyRef = useRef<string | null>(null);

  const groups = useMemo(
    () => groupMessages(messages, usersById),
    [messages, usersById],
  );

  useEffect(() => {
    const switched = lastKeyRef.current !== conversationKey;
    lastKeyRef.current = conversationKey;
    bottomRef.current?.scrollIntoView({
      behavior: switched ? "auto" : "smooth",
      block: "end",
    });
  }, [conversationKey, messages.length]);

  if (groups.length === 0) {
    return (
      <Box
        sx={{
          flexGrow: 1,
          minHeight: 0,
          py: 6,
          display: "flex",
          flexDirection: "column",
          alignItems: "center",
          justifyContent: "center",
          gap: 1,
          color: "text.secondary",
        }}
      >
        <ForumOutlinedIcon sx={{ fontSize: 40, opacity: 0.6 }} />
        <Typography variant="body2">{t("chat.empty")}</Typography>
      </Box>
    );
  }

  return (
    <Box sx={{ flexGrow: 1, minHeight: 0, overflowY: "auto", py: 1.5 }}>
      {groups.map((group, i) => {
        const showDivider =
          i === 0 ||
          !isSameDay(group.startedAt * 1000, groups[i - 1].startedAt * 1000);
        return (
          <Box key={group.key}>
            {showDivider && (
              <Divider sx={{ my: 1.5, mx: 2 }}>
                <Chip
                  label={dayLabel(group.startedAt, t, i18n.language)}
                  size="small"
                  variant="outlined"
                />
              </Divider>
            )}
            <MessageItem
              group={group}
              isOwn={group.author.id === currentUserId}
            />
          </Box>
        );
      })}
      {/* Scroll anchor pinned at the list's end. The height must be a CSS
          length — a numeric sx height is a sizing fraction (1 = 100%), not
          pixels. */}
      <Box ref={bottomRef} sx={{ height: "1px", flexShrink: 0 }} />
    </Box>
  );
}
