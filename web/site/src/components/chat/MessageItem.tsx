"use client";

import { Box, Typography } from "@mui/material";
import { formatDistanceToNow } from "date-fns";
import { useTranslation } from "react-i18next";
import { dateFnsLocaleFor, localeTagFor } from "@/i18n";
import UserAvatar from "./UserAvatar";
import type { MessageGroup } from "./types";

type MessageItemProps = {
  group: MessageGroup;
  // isOwn marks the local user's messages: the author name picks up the
  // primary color so your own lines are easy to spot in a busy channel.
  isOwn: boolean;
};

// MessageItem renders one message group: the author's avatar on the left,
// then a header line (name + relative time) above the group's message lines.
// Content keeps the author's line breaks (pre-wrap); long unbroken text
// wraps instead of overflowing.
export default function MessageItem({ group, isOwn }: MessageItemProps) {
  const { i18n } = useTranslation();
  const startedAt = new Date(group.startedAt * 1000);
  const relative = formatDistanceToNow(startedAt, {
    addSuffix: true,
    locale: dateFnsLocaleFor(i18n.language),
  });
  // Full timestamp for the hover tooltip on the relative time.
  const absolute = startedAt.toLocaleString(localeTagFor(i18n.language));

  return (
    <Box
      sx={{
        display: "flex",
        gap: 1.5,
        px: 2,
        py: 0.75,
        borderRadius: 2,
      }}
    >
      <UserAvatar user={group.author} size={36} />
      <Box sx={{ minWidth: 0, flexGrow: 1 }}>
        <Box sx={{ display: "flex", alignItems: "baseline", gap: 1 }}>
          <Typography
            variant="subtitle2"
            component="span"
            color={isOwn ? "primary.main" : "text.primary"}
          >
            {group.author.name}
          </Typography>
          <Typography
            variant="caption"
            color="text.secondary"
            component="span"
            title={absolute}
          >
            {relative}
          </Typography>
        </Box>
        {group.messages.map((message) => (
          <Typography
            key={message.id}
            variant="body2"
            sx={{ whiteSpace: "pre-wrap", overflowWrap: "anywhere", mt: 0.25 }}
          >
            {message.content}
          </Typography>
        ))}
      </Box>
    </Box>
  );
}
