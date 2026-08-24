"use client";

import { useMemo, useState } from "react";
import {
  Box,
  CircularProgress,
  Dialog,
  DialogContent,
  DialogTitle,
  Typography,
} from "@mui/material";
import PlayCircleIcon from "@mui/icons-material/PlayCircle";
import { formatDistanceToNow } from "date-fns";
import { useTranslation } from "react-i18next";
import type { ChatUser } from "@/api/ss/types";
import { dateFnsLocaleFor, localeTagFor } from "@/i18n";
import UserAvatar from "./UserAvatar";
import type { MediaChatMessage } from "./types";

type MediaMessageItemProps = {
  message: MediaChatMessage;
  author: ChatUser;
  // isOwn marks the local user's transfers: the author name picks up the
  // primary color, mirroring MessageItem.
  isOwn: boolean;
  // getFileByFileId resolves a completed transfer's bytes from
  // useBinaryDataChannel's registry. Render-safe: the hook re-renders
  // the page whenever a file lands (its filesVersion), so the card swaps
  // from the progress indicator to the media the moment its bytes
  // arrive.
  getFileByFileId: (fileId: string) => Blob | undefined;
};

// objectUrls caches each blob's object URL for the blob's lifetime. The
// registry (useBinaryDataChannel) pins every blob until the page
// unloads, so a URL is created once and never revoked: revoking in an
// effect cleanup races render-time URL creation (an effect replay
// revokes the very URL the render memoized — the card then points at a
// dead blob: URL) while pinning nothing the registry doesn't pin
// already.
const objectUrls = new WeakMap<Blob, string>();

// objectUrlFor returns blob's object URL, creating it on first use.
function objectUrlFor(blob: Blob): string {
  let url = objectUrls.get(blob);
  if (url === undefined) {
    url = URL.createObjectURL(blob);
    objectUrls.set(blob, url);
  }
  return url;
}

// MediaMessageItem renders a transfer-backed media message (an
// ImageChatMessage or a VideoChatMessage). While the transfer runs it is
// a placeholder with a doughnut progress indicator and the percentage;
// once done, it is a borderless media card — clickable, opening a
// borderless preview dialog. The bytes never travel in the message: the
// card resolves them locally by fileId.
export function MediaMessageItem({
  message,
  author,
  isOwn,
  getFileByFileId,
}: MediaMessageItemProps) {
  const { t, i18n } = useTranslation();
  const [previewOpen, setPreviewOpen] = useState(false);
  const sentAt = new Date(message.timestamp * 1000);
  const relative = formatDistanceToNow(sentAt, {
    addSuffix: true,
    locale: dateFnsLocaleFor(i18n.language),
  });
  // Full timestamp for the hover tooltip on the relative time.
  const absolute = sentAt.toLocaleString(localeTagFor(i18n.language));

  const isImage = message.type === "image-chat";
  // The binary channel moves bytes only, so the reassembled blob carries
  // no MIME type; re-wrap it with the message's for the media element.
  const blob = getFileByFileId(message.fileId);
  const typedBlob = useMemo(
    () =>
      blob === undefined
        ? undefined
        : new Blob([blob], { type: message.fileMIMEType }),
    [blob, message.fileMIMEType],
  );
  // The blob's URL outlives the component (see objectUrlFor): no revoke.
  const url = typedBlob === undefined ? undefined : objectUrlFor(typedBlob);

  // The card is complete when the status says done AND the bytes are
  // here — the done status can outrun the last frames (status rides the
  // messaging channel, bytes the binary one), so the doughnut lingers at
  // 100% until the registry catches up.
  const ready = message.fileTransferStatus === "done" && url !== undefined;
  const percent =
    message.fileSizeTotalBytes > 0
      ? Math.min(
          100,
          (message.fileSizeTransferred / message.fileSizeTotalBytes) * 100,
        )
      : 0;
  const openPreview = () => setPreviewOpen(true);

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
        {ready ? (
          /* The completed media card: borderless, clickable, opening the
             preview dialog (pointer cursor, Enter/Space activation). */
          <Box
            onClick={openPreview}
            onKeyDown={(e) => {
              if (e.key === "Enter" || e.key === " ") {
                e.preventDefault();
                openPreview();
              }
            }}
            role="button"
            tabIndex={0}
            aria-label={t("chat.preview")}
            title={message.filename}
            sx={{
              mt: 0.5,
              position: "relative",
              display: "inline-block",
              maxWidth: 320,
              borderRadius: 2,
              overflow: "hidden",
              lineHeight: 0,
              cursor: "pointer",
            }}
          >
            {isImage ? (
              <Box
                component="img"
                src={url}
                alt={message.filename}
                sx={{ display: "block", maxWidth: "100%", maxHeight: 320 }}
              />
            ) : (
              <>
                <Box
                  component="video"
                  src={url}
                  muted
                  preload="metadata"
                  sx={{ display: "block", maxWidth: "100%", maxHeight: 320 }}
                />
                <PlayCircleIcon
                  sx={{
                    position: "absolute",
                    top: "50%",
                    left: "50%",
                    transform: "translate(-50%, -50%)",
                    fontSize: 48,
                    color: "common.white",
                    opacity: 0.85,
                    pointerEvents: "none",
                  }}
                />
              </>
            )}
          </Box>
        ) : (
          /* Transferring (or the bytes still catching up with a done
             status): a doughnut progress indicator with the percentage at
             its center. */
          <Box
            sx={{
              mt: 0.5,
              width: 240,
              height: 180,
              borderRadius: 2,
              bgcolor: "action.hover",
              display: "flex",
              alignItems: "center",
              justifyContent: "center",
            }}
          >
            <Box sx={{ position: "relative", display: "inline-flex" }}>
              <CircularProgress
                variant="determinate"
                value={percent}
                size={48}
              />
              <Box
                sx={{
                  position: "absolute",
                  inset: 0,
                  display: "flex",
                  alignItems: "center",
                  justifyContent: "center",
                }}
              >
                <Typography variant="caption" color="text.secondary">
                  {`${Math.round(percent)}%`}
                </Typography>
              </Box>
            </Box>
          </Box>
        )}
      </Box>
      {url !== undefined && (
        /* The preview dialog: borderless body (no padding — the media is
           edge to edge), titled "Preview". */
        <Dialog
          open={previewOpen}
          onClose={() => setPreviewOpen(false)}
          maxWidth="lg"
        >
          <DialogContent
            sx={{ p: 0, "&:last-child": { pb: 0 }, lineHeight: 0 }}
          >
            {isImage ? (
              <Box
                component="img"
                src={url}
                alt={message.filename}
                sx={{
                  display: "block",
                  mx: "auto",
                  maxWidth: "100%",
                  maxHeight: "75vh",
                }}
              />
            ) : (
              <Box
                component="video"
                src={url}
                controls
                autoPlay
                sx={{
                  display: "block",
                  mx: "auto",
                  maxWidth: "100%",
                  maxHeight: "75vh",
                }}
              />
            )}
          </DialogContent>
        </Dialog>
      )}
    </Box>
  );
}
