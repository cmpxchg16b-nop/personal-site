// Magic commands are a debugging backdoor in the composer: a message
// matching one of the patterns below is intercepted on send (see
// handleSend in app/chat/page.tsx) and turned into the corresponding
// chat-control wire message instead of a text line:
//
//   /magic a90926e3-c768-45b7-ab93-4709c5f4aa91 <target_msg_id>
//     delete the target message
//   /magic 1f734b69-9c46-4629-9e73-0aed96166f7c <target_msg_id> <content>
//     amend a text chat message (content may contain spaces)
//   /magic 7f8d9d4e-f41e-4e18-958e-ebc990690666 <target_msg_id> <status>
//         <transferred> <total>
//     amend a file-transfer status message (status is pending, running
//     or done; sizes are byte counts)

import type { DCChatControl } from "@/api/ss/datachannel";
import type { ChatMessage, FileTransferStatusMessage } from "./types";

// MagicCommand is a parsed magic command.
export type MagicCommand =
  | { kind: "delete"; targetMessageId: string }
  | { kind: "amend-text"; targetMessageId: string; content: string }
  | {
      kind: "amend-file-transfer";
      targetMessageId: string;
      fileTransferStatus: FileTransferStatusMessage["fileTransferStatus"];
      fileSizeTransferred: number;
      fileSizeTotalBytes: number;
    };

const DELETE_RE = /^\/magic a90926e3-c768-45b7-ab93-4709c5f4aa91 (\S+)$/;
const AMEND_TEXT_RE =
  /^\/magic 1f734b69-9c46-4629-9e73-0aed96166f7c (\S+)\s+([\s\S]+)$/;
const AMEND_FILE_RE =
  /^\/magic 7f8d9d4e-f41e-4e18-958e-ebc990690666 (\S+) (\S+) (\d+) (\d+)$/;

// parseMagicCommand matches content against the magic command patterns,
// returning null when it matches none (an ordinary message).
export function parseMagicCommand(content: string): MagicCommand | null {
  let m = content.match(DELETE_RE);
  if (m !== null) {
    return { kind: "delete", targetMessageId: m[1] };
  }
  m = content.match(AMEND_TEXT_RE);
  if (m !== null) {
    return { kind: "amend-text", targetMessageId: m[1], content: m[2] };
  }
  m = content.match(AMEND_FILE_RE);
  if (m === null) {
    return null;
  }
  const status = m[2];
  if (status !== "pending" && status !== "running" && status !== "done") {
    return null;
  }
  return {
    kind: "amend-file-transfer",
    targetMessageId: m[1],
    fileTransferStatus: status,
    fileSizeTransferred: Number(m[3]),
    fileSizeTotalBytes: Number(m[4]),
  };
}

// buildMagicControl turns a parsed magic command into the on-the-wire
// chat-control body. Amendments copy the target's immutable fields
// (a file transfer's id, kind, name and MIME type) from the conversation
// history, so an unknown target or a kind mismatch returns null and the
// command goes nowhere.
export function buildMagicControl(
  command: MagicCommand,
  history: ChatMessage[],
): DCChatControl | null {
  switch (command.kind) {
    case "delete":
      return { subtype: "delete", targetMessageId: command.targetMessageId };
    case "amend-text": {
      const target = history.find((m) => m.id === command.targetMessageId);
      if (target === undefined || target.type !== "text-chat") return null;
      return {
        subtype: "amend",
        targetMessageId: command.targetMessageId,
        text: command.content,
      };
    }
    case "amend-file-transfer": {
      const target = history.find((m) => m.id === command.targetMessageId);
      // Only transfer-backed messages carry a transferable file; a phone
      // call's log entry or a text line is not amendable as one.
      if (
        target === undefined ||
        target.type === "text-chat" ||
        target.type === "phone-call"
      ) {
        return null;
      }
      // The wire kind is the target's own: the UI message type is the
      // useChatMessages mapping of it, so this is its exact inverse — a
      // bijection, never a guess from the file's MIME type.
      const kind =
        target.type === "image-chat"
          ? "image"
          : target.type === "video-chat"
            ? "video"
            : "file";
      return {
        subtype: "amend",
        targetMessageId: command.targetMessageId,
        fileTransfer: {
          fileId: target.fileId,
          kind,
          filename: target.filename,
          fileMIMEType: target.fileMIMEType,
          fileSizeTotalBytes: command.fileSizeTotalBytes,
          fileSizeTransferred: command.fileSizeTransferred,
          fileTransferStatus: command.fileTransferStatus,
        },
      };
    }
  }
}
