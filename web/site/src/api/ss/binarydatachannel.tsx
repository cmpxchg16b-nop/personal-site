"use client";

/**
 * Binary file transfer between channel members, over the binary data
 * channel (dcbin).
 *
 * useBinaryDataChannel is one of the two consumers of the peer sessions
 * usePeerSessions brings up (see peersessions.tsx) — decoupled from the
 * other (useDataChannel): both build on the same peer connections,
 * neither knowing the other. It subscribes the binary data channel
 * (dcbin) of every session — the polite peer creates it, the other
 * receives it via ondatachannel — and moves compact binary frames (the
 * BinaryFrame codec of binaryframes.ts) between peers. Sending a file
 * streams it as FILE frames — 16 KiB chunks, up to SEND_WINDOW_FRAMES
 * unacknowledged in flight (a sliding window over the per-file frame
 * sequence) — while the receiver acknowledges every accepted frame
 * with a FACK frame: ack_seq is the cumulative next expected seq,
 * acked_bytes the cumulative contiguous byte count. SCTP is ordered
 * and reliable, so the receiver reassembles by strict concatenation; a
 * gap, an overlap or a mismatched total marks the stream corrupt and
 * the transfer is dropped.
 *
 * sendFile returns a reader over a ReadableStream of the transfer's
 * status (a DCFileTransfer, the same shape the messaging channel's
 * file-transfer-status messages carry): it yields pending first, then
 * running updates as the receiver's FACKs advance — throttled to one
 * per STATUS_UPDATE_INTERVAL_MS — and finally done, so the progress the
 * UI shows is what the receiver actually holds, not what the sender
 * merely wrote to the channel. The reader never yields an error status;
 * a broken transfer rejects the pending read.
 *
 * The transfer's status travels on the messaging channel, not here:
 * sendFile's caller forwards the yielded statuses as chat-control
 * amends of the original file-transfer-status message, and the echo
 * applies them to the sender's own history (see chat/page.tsx), so both
 * UIs stay identical. Completed files are kept in memory and handed out
 * by getFileByFileId — the receiver's reassembled Blob, or the sender's
 * original File under the same fileId.
 */

import { useCallback, useEffect, useRef, useState } from "react";

import {
  BINARY_FRAME_TYPE_FILE_ACK,
  BINARY_FRAME_TYPE_FILE_TRANSFER,
  bytesToUuid,
  decodeBinaryFrame,
  encodeBinaryFrame,
  uuidToBytes,
  type FileAckFrame,
} from "./binaryframes";
import type { DCFileTransfer, DCFileTransferKind } from "./datachannel";
import type { PeerSessions } from "./peersessions";
import type { ChannelId, SubscriberId } from "./types";

// BINARY_DATA_CHANNEL_LABEL is the label of the binary data channel
// every pair of peers brings up alongside the messaging one, on the
// same peer connection (see peersessions.tsx).
const BINARY_DATA_CHANNEL_LABEL = "dcbin";

// FILE_CHUNK_SIZE is the payload size of one FILE frame.
const FILE_CHUNK_SIZE = 16 * 1024;

// SEND_WINDOW_FRAMES is the sliding-window size: the most frames sent
// but not yet acknowledged. With 16 KiB chunks it caps the in-flight
// (and thus SCTP-buffered) bytes at 1 MiB per transfer.
const SEND_WINDOW_FRAMES = 64;

// STATUS_UPDATE_INTERVAL_MS throttles the running statuses sendFile's
// reader yields: every one of them becomes a chat-control message on
// the messaging channel, so they are bounded to a few per second.
const STATUS_UPDATE_INTERVAL_MS = 250;

// InboundTransfer is the reassembly state of one incoming file: the
// frames received so far, in order. SCTP's ordered, reliable delivery
// means the next frame must continue the contiguous prefix exactly.
interface InboundTransfer {
  /** the whole file's size in bytes (Number(frame.total)) */
  total: number;
  /** contiguously received bytes so far (the next expected offset) */
  received: number;
  /** the next expected seq */
  nextSeq: number;
  /** the received payloads, in order */
  chunks: Uint8Array<ArrayBuffer>[];
}

export interface UseBinaryDataChannelResult {
  /**
   * Sends file to a peer as FILE frames and returns a reader over the
   * transfer's status stream: pending first, then running updates as
   * the receiver's acknowledgements advance, and done once the last
   * block's acknowledgement is in; the stream closes after done. A
   * transfer that breaks (the channel or the session goes away) rejects
   * the pending read. fileId must be a UUID string — the wire format's
   * file_id field is the packed UUID — and is also the key the peer's
   * getFileByFileId hands the reassembled file out under. kind is the
   * sender's render choice (see DCFileTransfer.kind); every status the
   * reader yields carries it, so amends built from them stay complete.
   */
  sendFile: (
    channelId: ChannelId,
    toSubscriberId: SubscriberId,
    fileId: string,
    file: File,
    kind: DCFileTransferKind,
  ) => ReadableStreamDefaultReader<DCFileTransfer>;
  /**
   * Returns a completed file by its fileId: a reassembled Blob for a
   * received file, the original File for a sent one. Undefined while
   * the transfer is still running or unknown. Render-safe: filesVersion
   * bumps whenever a file lands in the registry, re-rendering the
   * hook's caller, so a component that reads getFileByFileId during
   * render sees a file the moment its bytes arrive (memoized consumers
   * can depend on filesVersion).
   */
  getFileByFileId: (fileId: string) => Blob | undefined;
  /**
   * Bumped whenever a completed file lands in the registry — the
   * mechanism that keeps getFileByFileId reads reactive.
   */
  filesVersion: number;
}

/**
 * Sends and receives files as compact binary frames over the binary
 * data channel (dcbin) of each peer session. `sessions` is the
 * PeerSessions primitive usePeerSessions brings up; see binaryframes.ts
 * for the frame format.
 */
export function useBinaryDataChannel(
  sessions: PeerSessions,
): UseBinaryDataChannelResult {
  // Completed files by fileId: reassembled received files and the
  // originals of sent ones.
  const filesRef = useRef(new Map<string, Blob>());
  // Bumped whenever the registry gains a file — the re-render that
  // keeps getFileByFileId reads reactive.
  const [filesVersion, setFilesVersion] = useState(0);
  // Partial inbound transfers, keyed `${channelId}:${from}:${fileId}`.
  const inboundRef = useRef(new Map<string, InboundTransfer>());
  // The acknowledgement callbacks of in-flight sends, keyed
  // `${channelId}:${to}:${fileId}`.
  const sendersRef = useRef(new Map<string, (ack: FileAckFrame) => void>());
  // The open binary channel per peer, resolved lazily for sending FACKs.
  const ackChannelsRef = useRef(new Map<string, RTCDataChannel>());

  // Inbound frames: FILE frames fold into the reassembly (and are
  // acknowledged), FACK frames advance the in-flight send they name.
  useEffect(() => {
    const sendAck = (
      channelId: ChannelId,
      to: SubscriberId,
      ack: FileAckFrame,
    ) => {
      const key = `${channelId}:${to}`;
      const data = encodeBinaryFrame(ack);
      // A failed acknowledgement only stalls the sender's window, which
      // the channel's own close then fails loudly — so a closed channel
      // here is logged and swallowed.
      const cached = ackChannelsRef.current.get(key);
      if (cached !== undefined && cached.readyState === "open") {
        try {
          cached.send(data);
        } catch (err) {
          console.warn("binarydatachannel: acknowledgement not sent", err);
        }
        return;
      }
      void sessions
        .whenOpenChannel(BINARY_DATA_CHANNEL_LABEL, channelId, to)
        .then((dc) => {
          if (dc === null) return;
          ackChannelsRef.current.set(key, dc);
          try {
            dc.send(data);
          } catch (err) {
            console.warn("binarydatachannel: acknowledgement not sent", err);
          }
        });
    };
    const handleFrame = (
      channelId: ChannelId,
      from: SubscriberId,
      data: ArrayBuffer,
    ) => {
      const frame = decodeBinaryFrame(data);
      if (frame === null) {
        return;
      }
      if (frame.frameType === BINARY_FRAME_TYPE_FILE_ACK) {
        sendersRef.current.get(`${channelId}:${from}:${frame.fileId}`)?.(frame);
        return;
      }
      const key = `${channelId}:${from}:${frame.fileId}`;
      let transfer = inboundRef.current.get(key);
      if (transfer === undefined) {
        // The first frame of a stream must start it: seq 0, offset 0,
        // and a total that survives the bigint → number conversion.
        if (
          frame.seq !== 0 ||
          frame.offset !== BigInt(0) ||
          frame.total > BigInt(Number.MAX_SAFE_INTEGER)
        ) {
          console.warn(
            `binarydatachannel: first frame of file ${frame.fileId} ` +
              `from ${from} does not start the stream; frame dropped`,
          );
          return;
        }
        transfer = {
          total: Number(frame.total),
          received: 0,
          nextSeq: 0,
          chunks: [],
        };
        inboundRef.current.set(key, transfer);
      }
      // Delivery is ordered and reliable, so the frame must continue
      // the contiguous prefix exactly; anything else means a corrupt
      // stream and the whole transfer is dropped.
      if (
        frame.total !== BigInt(transfer.total) ||
        frame.seq !== transfer.nextSeq ||
        frame.offset !== BigInt(transfer.received) ||
        frame.offset + BigInt(frame.payload.byteLength) > frame.total
      ) {
        inboundRef.current.delete(key);
        console.warn(
          `binarydatachannel: corrupt frame stream of file ` +
            `${frame.fileId} from ${from}; transfer dropped`,
        );
        return;
      }
      transfer.chunks.push(frame.payload);
      transfer.received += frame.payload.byteLength;
      transfer.nextSeq += 1;
      sendAck(channelId, from, {
        frameType: BINARY_FRAME_TYPE_FILE_ACK,
        fileId: frame.fileId,
        ackSeq: transfer.nextSeq,
        ackedBytes: BigInt(transfer.received),
      });
      if (transfer.received === transfer.total) {
        inboundRef.current.delete(key);
        filesRef.current.set(frame.fileId, new Blob(transfer.chunks));
        setFilesVersion((v) => v + 1);
      }
    };
    return sessions.subscribeChannel(
      BINARY_DATA_CHANNEL_LABEL,
      (channelId, from, dc) => {
        dc.binaryType = "arraybuffer";
        dc.onmessage = (e) => {
          // Anything but binary frames is dropped silently, mirroring
          // decodeDCMsg's rule for malformed frames.
          if (e.data instanceof ArrayBuffer) {
            handleFrame(channelId, from, e.data);
          }
        };
      },
    );
  }, [sessions]);

  // A session teardown drops the partial inbound transfers of the gone
  // peers (in-flight sends notice on their own — their channel closes);
  // completed files survive.
  useEffect(() => {
    return sessions.subscribeReset((dropped) => {
      if (dropped === null) {
        inboundRef.current.clear();
        ackChannelsRef.current.clear();
        return;
      }
      const prefix = `${dropped.channelId}:${dropped.peer}:`;
      for (const key of [...inboundRef.current.keys()]) {
        if (key.startsWith(prefix)) {
          inboundRef.current.delete(key);
        }
      }
      ackChannelsRef.current.delete(`${dropped.channelId}:${dropped.peer}`);
    });
  }, [sessions]);

  const getFileByFileId = useCallback((fileId: string): Blob | undefined => {
    // Registry keys are the canonical UUID form; normalize the same way.
    const bytes = uuidToBytes(fileId);
    return filesRef.current.get(bytes === null ? fileId : bytesToUuid(bytes));
  }, []);

  const sendFile = useCallback(
    (
      channelId: ChannelId,
      toSubscriberId: SubscriberId,
      fileId: string,
      file: File,
      kind: DCFileTransferKind,
    ): ReadableStreamDefaultReader<DCFileTransfer> => {
      const fileIdBytes = uuidToBytes(fileId);
      if (fileIdBytes === null) {
        throw new TypeError(
          `binarydatachannel: fileId must be a UUID string, got ` +
            JSON.stringify(fileId),
        );
      }
      const normalizedFileId = bytesToUuid(fileIdBytes);
      // The sender's own completed card downloads the original file.
      filesRef.current.set(normalizedFileId, file);
      setFilesVersion((v) => v + 1);
      const base = {
        fileId: normalizedFileId,
        kind,
        filename: file.name,
        // The File API leaves type empty for unrecognized extensions.
        fileMIMEType: file.type || "application/octet-stream",
        fileSizeTotalBytes: file.size,
      };
      // cancelled stops the pump (a reader.cancel()); wake parks the
      // pump while the send window is full and an acknowledgement is
      // owed.
      let cancelled = false;
      let wake: (() => void) | null = null;
      const stream = new ReadableStream<DCFileTransfer>({
        start(controller) {
          const emit = (status: DCFileTransfer) => {
            if (!cancelled) {
              controller.enqueue(status);
            }
          };
          const pump = async () => {
            emit({
              ...base,
              fileSizeTransferred: 0,
              fileTransferStatus: "pending",
            });
            const dc = await sessions.whenOpenChannel(
              BINARY_DATA_CHANNEL_LABEL,
              channelId,
              toSubscriberId,
            );
            if (cancelled) return;
            if (dc === null) {
              throw new Error(
                `binarydatachannel: no binary data channel to ` +
                  `subscriber ${toSubscriberId} in channel ${channelId}`,
              );
            }
            const senderKey = `${channelId}:${toSubscriberId}:${normalizedFileId}`;
            const total = file.size;
            let ackedSeq = 0;
            let ackedBytes = 0;
            let failure: Error | null = null;
            const fail = (err: Error) => {
              failure = err;
              wake?.();
            };
            const onChannelClose = () =>
              fail(
                new Error(
                  "binarydatachannel: binary data channel closed " +
                    "mid-transfer",
                ),
              );
            dc.addEventListener("close", onChannelClose, { once: true });
            const offReset = sessions.subscribeReset((dropped) => {
              if (
                dropped === null ||
                (dropped.channelId === channelId &&
                  dropped.peer === toSubscriberId)
              ) {
                fail(
                  new Error(
                    "binarydatachannel: peer session dropped mid-transfer",
                  ),
                );
              }
            });
            sendersRef.current.set(senderKey, (ack) => {
              // Acknowledgements only advance; a stale one is ignored —
              // but still wakes the pump, which re-checks cheaply.
              if (ack.ackSeq > ackedSeq) {
                ackedSeq = ack.ackSeq;
                ackedBytes = Number(ack.ackedBytes);
              }
              wake?.();
            });
            try {
              let seq = 0;
              let offset = 0;
              let lastEmit = Date.now();
              let lastEmitted = 0;
              for (;;) {
                // Fill the send window: up to SEND_WINDOW_FRAMES
                // unacknowledged frames in flight. An empty file is a
                // single FILE frame with an empty payload.
                while (
                  !cancelled &&
                  failure === null &&
                  seq - ackedSeq < SEND_WINDOW_FRAMES &&
                  (offset < total || (total === 0 && seq === 0))
                ) {
                  const end = Math.min(offset + FILE_CHUNK_SIZE, total);
                  const payload = new Uint8Array(
                    await file.slice(offset, end).arrayBuffer(),
                  );
                  if (dc.readyState !== "open") {
                    throw new Error(
                      "binarydatachannel: binary data channel closed " +
                        "mid-transfer",
                    );
                  }
                  dc.send(
                    encodeBinaryFrame({
                      frameType: BINARY_FRAME_TYPE_FILE_TRANSFER,
                      fileId: normalizedFileId,
                      seq,
                      offset: BigInt(offset),
                      total: BigInt(total),
                      payload,
                    }),
                  );
                  seq += 1;
                  offset = end;
                }
                if (cancelled) return;
                if (failure !== null) throw failure;
                // Done once everything sent has been acknowledged.
                if (ackedSeq === seq && ackedBytes === total) break;
                const now = Date.now();
                if (
                  ackedBytes > lastEmitted &&
                  now - lastEmit >= STATUS_UPDATE_INTERVAL_MS
                ) {
                  emit({
                    ...base,
                    fileSizeTransferred: ackedBytes,
                    fileTransferStatus: "running",
                  });
                  lastEmit = now;
                  lastEmitted = ackedBytes;
                }
                await new Promise<void>((resolve) => {
                  wake = resolve;
                });
                wake = null;
              }
              emit({
                ...base,
                fileSizeTransferred: total,
                fileTransferStatus: "done",
              });
              if (!cancelled) {
                controller.close();
              }
            } finally {
              sendersRef.current.delete(senderKey);
              offReset();
              dc.removeEventListener("close", onChannelClose);
            }
          };
          void pump().catch((err) => {
            if (!cancelled) {
              controller.error(err);
            }
          });
        },
        cancel() {
          cancelled = true;
          wake?.();
        },
      });
      return stream.getReader();
    },
    [sessions],
  );

  return { sendFile, getFileByFileId, filesVersion };
}
