"use client";

import { useRef, useState } from "react";
import { Box, IconButton, TextField } from "@mui/material";
import AttachFileIcon from "@mui/icons-material/AttachFile";
import SendIcon from "@mui/icons-material/Send";
import { useTranslation } from "react-i18next";

type MessageInputProps = {
  // Display name of the conversation target ("#日常闲聊", "@阿福"), used in
  // the placeholder.
  target: string;
  onSend: (content: string) => void;
  // onAttachFile reports the files picked through the attach button.
  onAttachFile: (files: File[]) => void;
};

// MessageInput is the composer at the bottom of a conversation: a borderless
// multiline input filling the whole bar, separated from the history by a
// single hairline, with the attach button docked at its start and the send
// button at its end. Enter sends; Shift+Enter inserts a newline. Enter is
// ignored while an IME composition is in progress so CJK input isn't cut
// off mid-conversion.
export default function MessageInput({
  target,
  onSend,
  onAttachFile,
}: MessageInputProps) {
  const { t } = useTranslation();
  const [content, setContent] = useState("");
  const sendable = content.trim() !== "";
  const fileInputRef = useRef<HTMLInputElement>(null);

  const submit = () => {
    const trimmed = content.trim();
    if (trimmed === "") return;
    onSend(trimmed);
    setContent("");
  };

  return (
    <Box
      sx={{
        display: "flex",
        alignItems: "flex-end",
        gap: 1,
        px: 1,
        py: 1,
        borderTop: 1,
        borderColor: "divider",
      }}
    >
      <IconButton
        onClick={() => fileInputRef.current?.click()}
        aria-label={t("chat.attach")}
      >
        <AttachFileIcon fontSize="small" />
      </IconButton>
      {/* Hidden file input driven by the attach button; the value is
          cleared after each pick so choosing the same file again still
          fires onChange. */}
      <input
        type="file"
        hidden
        multiple
        ref={fileInputRef}
        onChange={(e) => {
          const files = Array.from(e.target.files ?? []);
          if (files.length > 0) onAttachFile(files);
          e.target.value = "";
        }}
      />
      <TextField
        placeholder={t("chat.message", { target })}
        value={content}
        onChange={(e) => setContent(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "Enter" && !e.shiftKey && !e.nativeEvent.isComposing) {
            e.preventDefault();
            submit();
          }
        }}
        multiline
        maxRows={8}
        fullWidth
        variant="standard"
        sx={{ py: 0.5 }}
        slotProps={{
          input: { disableUnderline: true },
          htmlInput: { "aria-label": t("chat.message", { target }) },
        }}
      />
      {/* While empty the button is a quiet disabled icon; once there is
          text it fills with the primary color as a send affordance. */}
      <IconButton
        color="primary"
        disabled={!sendable}
        onClick={submit}
        aria-label={t("chat.send")}
        sx={
          sendable
            ? {
                bgcolor: "primary.main",
                color: "primary.contrastText",
                "&:hover": { bgcolor: "primary.dark" },
              }
            : undefined
        }
      >
        <SendIcon sx={{ transform: "rotate(-45deg)" }} fontSize="small" />
      </IconButton>
    </Box>
  );
}
