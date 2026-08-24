"use client";

import { Box, Chip, LinearProgress, Typography } from "@mui/material";
import InsertDriveFileOutlinedIcon from "@mui/icons-material/InsertDriveFileOutlined";
import { formatDistanceToNow } from "date-fns";
import { useTranslation } from "react-i18next";
import type { ChatUser } from "@/api/ss/types";
import { dateFnsLocaleFor, localeTagFor } from "@/i18n";
import UserAvatar from "./UserAvatar";
import type { FileTransferStatusMessage } from "./types";

// formatBytes renders a byte count with the largest binary unit that keeps
// the value at or above 1 (e.g. "512 B", "1.5 MiB").
function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  const units = ["KiB", "MiB", "GiB", "TiB"] as const;
  let value = bytes;
  let unit: string = "B";
  for (const u of units) {
    if (value < 1024) break;
    value /= 1024;
    unit = u;
  }
  return `${value >= 10 ? Math.round(value) : value.toFixed(1)} ${unit}`;
}

// STATUS_CHIP_COLOR maps the transfer status onto a chip color.
const STATUS_CHIP_COLOR = {
  pending: "default",
  running: "info",
  done: "success",
} as const;

type FileTransferStatusItemProps = {
  message: FileTransferStatusMessage;
  author: ChatUser;
  // isOwn marks the local user's transfers: the author name picks up the
  // primary color, mirroring MessageItem.
  isOwn: boolean;
  // onRequestFile asks the parent for the file's bytes by fileId; the
  // whole item becomes clickable once the transfer is done.
  onRequestFile?: (fileId: string) => void;
};

// FileTransferStatusItem renders a file-transfer status message: a compact
// card with the file's name, its progress (transferred / total bytes) and
// the transfer's status. The message carries no file bytes — the card is
// pure UI state shared by both ends of the transfer.
export function FileTransferStatusItem({
  message,
  author,
  isOwn,
  onRequestFile,
}: FileTransferStatusItemProps) {
  const { t, i18n } = useTranslation();
  const sentAt = new Date(message.timestamp * 1000);
  const relative = formatDistanceToNow(sentAt, {
    addSuffix: true,
    locale: dateFnsLocaleFor(i18n.language),
  });
  // Full timestamp for the hover tooltip on the relative time.
  const absolute = sentAt.toLocaleString(localeTagFor(i18n.language));
  const percent =
    message.fileSizeTotalBytes > 0
      ? Math.min(
          100,
          (message.fileSizeTransferred / message.fileSizeTotalBytes) * 100,
        )
      : 0;
  // A completed transfer is downloadable: the card acts as a button
  // (pointer cursor, hover/focus fill, Enter/Space activation) and reports
  // the file's id upward.
  const downloadable =
    message.fileTransferStatus === "done" && onRequestFile !== undefined;
  const requestFile = () => onRequestFile?.(message.fileId);

  return (
    <Box sx={{ display: "flex", gap: 1.5, px: 2, py: 0.75, borderRadius: 2 }}>
      <UserAvatar user={author} size={36} />
      <Box sx={{ minWidth: 0, flexGrow: 1 }}>
        <Box sx={{ display: "flex", alignItems: "baseline", gap: 1 }}>
          <Typography
            variant="subtitle2"
            component="span"
            color={isOwn ? "primary.main" : "text.primary"}
          >
            {author.name}
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
        {/* The card: once the transfer is done it acts as a button
            (pointer cursor, hover/focus fill, Enter/Space activation)
            and reports the file's id upward. */}
        <Box
          onClick={downloadable ? requestFile : undefined}
          onKeyDown={
            downloadable
              ? (e) => {
                  if (e.key === "Enter" || e.key === " ") {
                    e.preventDefault();
                    requestFile();
                  }
                }
              : undefined
          }
          role={downloadable ? "button" : undefined}
          tabIndex={downloadable ? 0 : undefined}
          aria-label={
            downloadable
              ? t("chat.fileTransfer.download", { filename: message.filename })
              : undefined
          }
          sx={{
            mt: 0.5,
            p: 1.5,
            maxWidth: 360,
            border: 1,
            borderColor: "divider",
            borderRadius: 2,
            display: "flex",
            flexDirection: "column",
            gap: 1,
            ...(downloadable && {
              cursor: "pointer",
              "&:hover": { bgcolor: "action.hover" },
              "&:focus-visible": { bgcolor: "action.focus", outline: "none" },
            }),
          }}
        >
          <Box
            sx={{ display: "flex", alignItems: "center", gap: 1, minWidth: 0 }}
          >
            <InsertDriveFileOutlinedIcon color="action" />
            <Typography
              variant="body2"
              component="span"
              title={message.fileMIMEType}
              sx={{ overflowWrap: "anywhere" }}
            >
              {message.filename}
            </Typography>
            <Chip
              label={t(`chat.fileTransfer.${message.fileTransferStatus}`)}
              size="small"
              color={STATUS_CHIP_COLOR[message.fileTransferStatus]}
              variant={
                message.fileTransferStatus === "done" ? "filled" : "outlined"
              }
              sx={{ ml: "auto", flexShrink: 0 }}
            />
          </Box>
          {message.fileTransferStatus === "running" && (
            <LinearProgress variant="determinate" value={percent} />
          )}
          <Typography variant="caption" color="text.secondary">
            {formatBytes(message.fileSizeTransferred)} /{" "}
            {formatBytes(message.fileSizeTotalBytes)}
          </Typography>
        </Box>
      </Box>
    </Box>
  );
}
