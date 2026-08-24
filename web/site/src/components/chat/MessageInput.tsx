"use client";

import { useRef, useState, type ChangeEvent, type RefObject } from "react";
import {
  Box,
  IconButton,
  ListItemIcon,
  ListItemText,
  Menu,
  MenuItem,
  TextField,
} from "@mui/material";
import AttachFileIcon from "@mui/icons-material/AttachFile";
import InsertDriveFileOutlinedIcon from "@mui/icons-material/InsertDriveFileOutlined";
import ImageOutlinedIcon from "@mui/icons-material/ImageOutlined";
import VideocamOutlinedIcon from "@mui/icons-material/VideocamOutlined";
import SendIcon from "@mui/icons-material/Send";
import { useTranslation } from "react-i18next";
import type { TransferKind } from "./types";

type MessageInputProps = {
  // Display name of the conversation target ("#日常闲聊", "@阿福"), used in
  // the placeholder.
  target: string;
  onSend: (content: string) => void;
  // onAttachFile reports the files picked through the attach menu, with
  // the picker's kind: how the transfer is announced and rendered (see
  // TransferKind). The kind is the user's explicit choice here — it is
  // never derived from the file's MIME type.
  onAttachFile: (files: File[], kind: TransferKind) => void;
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
  // The attach button opens a menu of three pickers; each picker drives
  // its own hidden file input whose accept filter narrows the file
  // dialog (any file, images only, videos only) and whose kind the pick
  // is reported with. The kind only chooses how the transfer is
  // announced and rendered — the transfer path is identical for all
  // three.
  const [attachAnchor, setAttachAnchor] = useState<HTMLElement | null>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);
  const photoInputRef = useRef<HTMLInputElement>(null);
  const videoInputRef = useRef<HTMLInputElement>(null);

  const submit = () => {
    const trimmed = content.trim();
    if (trimmed === "") return;
    onSend(trimmed);
    setContent("");
  };

  // pickFiles wires every hidden input alike: report the pick with the
  // input's kind, then clear the value so choosing the same file again
  // still fires onChange.
  const pickFiles =
    (kind: TransferKind) => (e: ChangeEvent<HTMLInputElement>) => {
      const files = Array.from(e.target.files ?? []);
      if (files.length > 0) onAttachFile(files, kind);
      e.target.value = "";
    };
  const openPicker = (ref: RefObject<HTMLInputElement | null>) => {
    setAttachAnchor(null);
    ref.current?.click();
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
        onClick={(e) => setAttachAnchor(e.currentTarget)}
        aria-label={t("chat.attach")}
        aria-haspopup="true"
        aria-expanded={attachAnchor !== null ? "true" : undefined}
      >
        <AttachFileIcon fontSize="small" />
      </IconButton>
      <Menu
        anchorOrigin={{
          vertical: "top",
          horizontal: "left",
        }}
        transformOrigin={{
          vertical: "bottom",
          horizontal: "left",
        }}
        anchorEl={attachAnchor}
        open={attachAnchor !== null}
        onClose={() => setAttachAnchor(null)}
      >
        <MenuItem onClick={() => openPicker(fileInputRef)}>
          <ListItemIcon>
            <InsertDriveFileOutlinedIcon fontSize="small" />
          </ListItemIcon>
          <ListItemText>{t("chat.attachMenu.attachment")}</ListItemText>
        </MenuItem>
        <MenuItem onClick={() => openPicker(photoInputRef)}>
          <ListItemIcon>
            <ImageOutlinedIcon fontSize="small" />
          </ListItemIcon>
          <ListItemText>{t("chat.attachMenu.photo")}</ListItemText>
        </MenuItem>
        <MenuItem onClick={() => openPicker(videoInputRef)}>
          <ListItemIcon>
            <VideocamOutlinedIcon fontSize="small" />
          </ListItemIcon>
          <ListItemText>{t("chat.attachMenu.video")}</ListItemText>
        </MenuItem>
      </Menu>
      {/* Hidden file inputs driven by the attach menu's items; the value
          is cleared after each pick so choosing the same file again still
          fires onChange. */}
      <input
        type="file"
        hidden
        multiple
        ref={fileInputRef}
        onChange={pickFiles("file")}
      />
      <input
        type="file"
        hidden
        multiple
        accept="image/*"
        ref={photoInputRef}
        onChange={pickFiles("image")}
      />
      <input
        type="file"
        hidden
        multiple
        accept="video/*"
        ref={videoInputRef}
        onChange={pickFiles("video")}
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
