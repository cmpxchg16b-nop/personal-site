"use client";

import { useEffect, useMemo, useRef } from "react";
import { Box, Chip, Divider, Typography } from "@mui/material";
import ForumOutlinedIcon from "@mui/icons-material/ForumOutlined";
import { format, isSameDay, isToday, isYesterday } from "date-fns";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import type { ChatUser } from "@/api/ss/types";
import { dateFnsLocaleFor } from "@/i18n";
import { MessageItem } from "./MessageItem";
import { FileTransferStatusItem } from "./FileTransferStatusItem";
import { MediaMessageItem } from "./MediaMessageItem";
import type {
  ChatMessage,
  FileTransferStatusMessage,
  MediaChatMessage,
  MessageGroup,
} from "./types";

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
  // onRequestFile asks for a completed transfer's bytes by fileId (see
  // FileTransferStatusItem).
  onRequestFile: (fileId: string) => void;
  // getFileByFileId resolves a completed transfer's bytes locally for
  // the media cards (see MediaMessageItem).
  getFileByFileId: (fileId: string) => Blob | undefined;
};

// ListItem is one renderable row of the list: a group of text messages, or
// a standalone transfer card (transfers are never folded into text groups;
// they break a run).
type ListItem =
  | { kind: "group"; group: MessageGroup }
  | {
      kind: "fileTransfer";
      message: FileTransferStatusMessage;
      author: ChatUser;
    }
  | { kind: "media"; message: MediaChatMessage; author: ChatUser };

// itemTimestamp returns the Unix-seconds timestamp an item carries for the
// day-divider logic.
function itemTimestamp(item: ListItem): number {
  return item.kind === "group" ? item.group.startedAt : item.message.timestamp;
}

// foldMessages folds the flat oldest-first list into renderable items: runs
// of consecutive text messages by the same author collapse into one
// MessageGroup; transfer messages stand alone.
function foldMessages(
  messages: ChatMessage[],
  usersById: Record<string, ChatUser>,
): ListItem[] {
  const items: ListItem[] = [];
  for (const message of messages) {
    const author = usersById[message.authorId];
    if (!author) continue;
    if (message.type === "file-transfer-status") {
      items.push({ kind: "fileTransfer", message, author });
      continue;
    }
    if (message.type === "image-chat" || message.type === "video-chat") {
      items.push({ kind: "media", message, author });
      continue;
    }
    const last = items[items.length - 1];
    if (
      last?.kind === "group" &&
      last.group.author.id === author.id &&
      message.timestamp * 1000 - last.group.startedAt * 1000 <
        GROUP_WINDOW_MS &&
      isSameDay(message.timestamp * 1000, last.group.startedAt * 1000)
    ) {
      last.group.messages.push(message);
    } else {
      items.push({
        kind: "group",
        group: {
          key: message.id,
          author,
          startedAt: message.timestamp,
          messages: [message],
        },
      });
    }
  }
  return items;
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
  onRequestFile,
  getFileByFileId,
}: MessageListProps) {
  const { t, i18n } = useTranslation();
  const bottomRef = useRef<HTMLDivElement>(null);
  // null until the first effect run, so the initial mount scrolls instantly
  // (no smooth animation on page load).
  const lastKeyRef = useRef<string | null>(null);

  const items = useMemo(
    () => foldMessages(messages, usersById),
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

  if (items.length === 0) {
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
      {items.map((item, i) => {
        const timestamp = itemTimestamp(item);
        const showDivider =
          i === 0 ||
          !isSameDay(timestamp * 1000, itemTimestamp(items[i - 1]) * 1000);
        return (
          <Box key={item.kind === "group" ? item.group.key : item.message.id}>
            {showDivider && (
              <Divider sx={{ my: 1.5, mx: 2 }}>
                <Chip
                  label={dayLabel(timestamp, t, i18n.language)}
                  size="small"
                  variant="outlined"
                />
              </Divider>
            )}
            {item.kind === "group" ? (
              <MessageItem
                group={item.group}
                isOwn={item.group.author.id === currentUserId}
              />
            ) : item.kind === "fileTransfer" ? (
              <FileTransferStatusItem
                message={item.message}
                author={item.author}
                isOwn={item.author.id === currentUserId}
                onRequestFile={onRequestFile}
              />
            ) : (
              <MediaMessageItem
                message={item.message}
                author={item.author}
                isOwn={item.author.id === currentUserId}
                getFileByFileId={getFileByFileId}
              />
            )}
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
