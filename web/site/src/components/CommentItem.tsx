"use client";

import { Box, ListItem, Typography } from "@mui/material";
import { formatDistanceToNow } from "date-fns";
import { useTranslation } from "react-i18next";
import { dateFnsLocaleFor } from "@/i18n";
import type { Comment } from "@/hooks/useComments";

type CommentItemProps = {
  comment: Comment;
};

// CommentItem renders a single comment: author and relative creation time on
// one line, then the body. The body keeps the author's line breaks
// (pre-wrap): comments are plain text, so newlines are all the structure
// there is.
export default function CommentItem({ comment }: CommentItemProps) {
  const { i18n } = useTranslation();
  const when = formatDistanceToNow(new Date(comment.creation_time * 1000), {
    addSuffix: true,
    locale: dateFnsLocaleFor(i18n.language),
  });

  return (
    <ListItem disableGutters disablePadding sx={{ py: 1.5, display: "block" }}>
      <Box sx={{ display: "flex", alignItems: "baseline", gap: 1 }}>
        <Typography variant="subtitle2" component="span">
          {comment.user_id}
        </Typography>
        <Typography variant="caption" color="textSecondary">
          {when}
        </Typography>
      </Box>
      <Typography variant="body2" sx={{ whiteSpace: "pre-wrap", mt: 0.5 }}>
        {comment.content}
      </Typography>
    </ListItem>
  );
}
