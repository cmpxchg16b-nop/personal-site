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
  onSubmit: (input: { userId: string; content: string }) => Promise<boolean>;
};

// CommentForm is the comment editor: a name field, then the comment box — a
// rounded, outlined container holding a borderless multiline input with the
// send icon button in its bottom-right corner. Both fields are required; the
// name is typed fresh each time — there is no authentication, so a comment's
// author is whatever name the commenter enters.
export default function CommentForm({
  submitting,
  error,
  onSubmit,
}: CommentFormProps) {
  const { t } = useTranslation();
  const [name, setName] = useState("");
  const [content, setContent] = useState("");
  // touched gates the name field's error state: an untouched form shows no
  // error; once the commenter types into or leaves either field, a
  // trimmed-empty name is flagged live.
  const [touched, setTouched] = useState(false);

  const userId = name.trim();
  const trimmedContent = content.trim();
  const fieldsValid = userId !== "" && trimmedContent !== "";
  const nameError = touched && userId === "";

  const handleSubmit = async (event: React.FormEvent) => {
    event.preventDefault();
    // The send button blocks invalid and duplicate submissions already; this
    // guards programmatic submits (e.g. pressing Enter in a field), flagging
    // the name field when an invalid submit is attempted.
    if (submitting) {
      return;
    }
    if (!fieldsValid) {
      setTouched(true);
      return;
    }
    if (await onSubmit({ userId, content: trimmedContent })) {
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
          onChange={(e) => {
            setContent(e.target.value);
            setTouched(true);
          }}
          onBlur={() => setTouched(true)}
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
      {/* The name field sits below the comment box: its error helper text
          appearing pushes only whatever is below the form, so the box never
          jumps. */}
      <TextField
        label={t("comments.name")}
        value={name}
        onChange={(e) => {
          setName(e.target.value);
          setTouched(true);
        }}
        onBlur={() => setTouched(true)}
        error={nameError}
        helperText={nameError ? t("comments.nameRequired") : undefined}
        size="small"
        sx={{ mt: 2, maxWidth: 320 }}
        fullWidth
        variant="standard"
      />
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
