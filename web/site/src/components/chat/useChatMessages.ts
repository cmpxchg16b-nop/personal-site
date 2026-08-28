import { useMemo } from "react";
import {
  DC_MSG_MIME_FILE_TRANSFER_STATUS,
  DC_MSG_MIME_SIP,
  type DCMsgs,
} from "@/api/ss/datachannel";
import { conversationKey, type ChatMessage } from "./types";

// useChatMessages derives the conversations' renderable messages from the
// raw data-channel store: the peers' messages plus our own sends (echo
// set), mapped onto the chat domain, keyed by conversation key,
// oldest first. Chat-control effects are already applied to the store by
// useDataChannel. Derived at render time — there is nothing to sync.
export function useChatMessages(dcMsgs: DCMsgs): Record<string, ChatMessage[]> {
  return useMemo(() => {
    const merged: Record<string, ChatMessage[]> = {};
    for (const bySender of Object.values(dcMsgs)) {
      for (const senderMsgs of Object.values(bySender)) {
        for (const m of senderMsgs) {
          // A dialog's SIP messages past the INVITE (responses, CANCEL,
          // BYE) are the call protocol's frames, not renderable
          // messages: they drive usePhoneCalls' session state, and the
          // log entry's status follows via chat-control amends of the
          // INVITE.
          if (m.mimeType === DC_MSG_MIME_SIP && m.sip?.method !== "INVITE") {
            continue;
          }
          // The conversation's peer is the message's other end: its
          // sender, or its recipient for our own copy (echo set —
          // bounced back for chat kinds, recorded at send time for
          // SIP).
          const key = conversationKey({
            kind: "dm",
            channelId: m.channelId,
            userId: m.echo === true ? m.toSubscriberId : m.fromSubscriberId,
          });
          let msg: ChatMessage;
          if (m.mimeType === DC_MSG_MIME_SIP && m.sip !== undefined) {
            msg = {
              type: "phone-call",
              id: m.msgId,
              authorId: m.fromSubscriberId,
              callId: m.sip.callId,
              // X-Media stands in for the stripped SDP's m= lines:
              // absent is a voice call.
              kind: m.sip["X-Media"] ?? "voice",
              phoneStatus: m.sip["X-Call-Status"] ?? "inviting",
              timestamp: m.creationTimestamp,
            };
          } else if (
            m.mimeType === DC_MSG_MIME_FILE_TRANSFER_STATUS &&
            m.fileTransfer !== undefined
          ) {
            const transfer = {
              id: m.msgId,
              authorId: m.fromSubscriberId,
              fileId: m.fileTransfer.fileId,
              filename: m.fileTransfer.filename,
              fileMIMEType: m.fileTransfer.fileMIMEType,
              fileSizeTotalBytes: m.fileTransfer.fileSizeTotalBytes,
              fileSizeTransferred: m.fileTransfer.fileSizeTransferred,
              fileTransferStatus: m.fileTransfer.fileTransferStatus,
              timestamp: m.creationTimestamp,
            };
            // The message kind is the sender's explicit choice, carried
            // in the transfer's kind field — never derived from the
            // file's MIME type.
            msg =
              m.fileTransfer.kind === "image"
                ? { type: "image-chat", ...transfer }
                : m.fileTransfer.kind === "video"
                  ? { type: "video-chat", ...transfer }
                  : { type: "file-transfer-status", ...transfer };
          } else {
            msg = {
              type: "text-chat",
              id: m.msgId,
              authorId: m.fromSubscriberId,
              content: m.plaintext,
              timestamp: m.creationTimestamp,
            };
          }
          (merged[key] ??= []).push(msg);
        }
      }
    }
    // Each sender list is oldest-first, but the senders interleave.
    for (const list of Object.values(merged)) {
      list.sort((a, b) => a.timestamp - b.timestamp);
    }
    return merged;
  }, [dcMsgs]);
}
