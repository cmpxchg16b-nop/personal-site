"use client";

import { useState } from "react";
import {
  Alert,
  Box,
  CircularProgress,
  IconButton,
  TextField,
} from "@mui/material";
import SendIcon from "@mui/icons-material/Send";
import { useTranslation } from "react-i18next";
import { ApiError } from "@/hooks/useComments";

type CommentFormProps = {
  // submitting is true while a comment is being posted.
  submitting: boolean;
  // The last post attempt's error, if any; a 409 conflict gets its own
  // message.
  error: Error | null;
  // onSubmit resolves to true when the comment was posted — only then is the
  // text area cleared, so a failed attempt never loses what was written.
  onSubmit: (input: { content: string }) => Promise<boolean>;
};

// CommentForm is the comment editor: the comment box — a rounded, outlined
// container holding a borderless multiline input with the send icon button
// in its bottom-right corner. There is no name field: the comment's author
// is the commenter's session identity, resolved server-side.
export default function CommentForm({
  submitting,
  error,
  onSubmit,
}: CommentFormProps) {
  const { t } = useTranslation();
  const [content, setContent] = useState("");

  const trimmedContent = content.trim();
  const fieldsValid = trimmedContent !== "";

  const handleSubmit = async (event: React.FormEvent) => {
    event.preventDefault();
    // The send button blocks invalid and duplicate submissions already; this
    // guards programmatic submits (e.g. pressing Enter in the field).
    if (submitting || !fieldsValid) {
      return;
    }
    if (await onSubmit({ content: trimmedContent })) {
      setContent("");
    }
  };

  return (
    <Box component="form" onSubmit={handleSubmit} sx={{ mt: 3 }}>
      {/* The comment box. The outer Box carries the border, so the input
          itself stays borderless; :focus-within moves the focus indication
          to the box since the input has no outline of its own. */}
      <Box
        sx={{
          border: 1,
          borderColor: "divider",
          borderRadius: 2,
          px: 2,
          pt: 1,
          pb: 0.5,
          "&:focus-within": { borderColor: "primary.main" },
        }}
      >
        <TextField
          placeholder={t("comments.content")}
          value={content}
          onChange={(e) => setContent(e.target.value)}
          multiline
          minRows={3}
          fullWidth
          variant="standard"
          slotProps={{
            input: { disableUnderline: true },
            htmlInput: { "aria-label": t("comments.content") },
          }}
        />
        <Box sx={{ display: "flex", justifyContent: "flex-end", pb: 1 }}>
          {/* disabled reflects field validity only; while posting, the icon
              swaps to a spinner — a disabled button would gray it out. */}
          <IconButton
            type="submit"
            color="primary"
            disabled={!fieldsValid}
            aria-label={t("comments.submit")}
          >
            {submitting ? (
              <CircularProgress size={20} color="inherit" />
            ) : (
              <SendIcon sx={{ transform: "rotate(-45deg)" }} />
            )}
          </IconButton>
        </Box>
      </Box>
      {error !== null && (
        <Alert severity="warning" sx={{ mt: 2 }}>
          {error instanceof ApiError && error.status === 409
            ? t("comments.conflict")
            : t("comments.postFailed")}
        </Alert>
      )}
    </Box>
  );
}
