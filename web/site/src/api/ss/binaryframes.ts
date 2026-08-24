/**
 * The on-the-wire format of the binary data channel (`dcbin`): compact
 * binary frames, as opposed to the JSON-encoded DCMsgs of the messaging
 * data channel (`dcmsg`, see datachannel.tsx). This module is the source
 * of truth for the binary frame format — it holds the frame types and
 * the codec (encodeBinaryFrame/decodeBinaryFrame) and nothing else; the
 * transfer engine built on it lives in binarydatachannel.tsx.
 *
 * Every frame starts with a 4-byte ASCII frame type and the 16-byte file
 * id (a UUID) it belongs to; multi-byte integers are big-endian (network
 * byte order — the DataView default). Two frame kinds exist:
 *
 * FILE — one (possibly fragmented) block of a file, sender → receiver.
 * A 48-byte header followed by the payload:
 *
 *   offset  size  field
 *   ------  ----  -----------------------------------------------------
 *   0       4     frame_type — ASCII "FILE"
 *   4       16    file_id — the file's UUID, packed into 16 bytes
 *   20      4     seq — uint32, per-file_id frame sequence from 0: the
 *                 file_id identifies a stream, so multiple files
 *                 multiplex over one binary data channel
 *   24      8     offset — uint64 byte offset of the payload in the
 *                 original file (the first frame's offset is always 0)
 *   32      8     total — uint64 size of the whole file in bytes
 *   40      8     payload_size — uint64 size of the payload that
 *                 follows; must equal the remaining frame length
 *   48      var   payload — the (possibly fragmented) file content
 *
 * FACK — receiver → sender acknowledgement of the contiguous prefix of
 * a file's stream received so far (sliding-window flow control; see
 * useBinaryDataChannel). 32 bytes, no payload:
 *
 *   offset  size  field
 *   ------  ----  -----------------------------------------------------
 *   0       4     frame_type — ASCII "FACK"
 *   4       16    file_id — the file's UUID, packed into 16 bytes
 *   20      4     ack_seq — uint32, cumulative: the next expected seq,
 *                 i.e. one past the highest contiguously received seq
 *   24      8     acked_bytes — uint64, cumulative: the contiguously
 *                 received byte count (the next expected offset)
 */

// BINARY_FRAME_TYPE_FILE_TRANSFER is the frame type of a file-block
// frame (sender → receiver).
export const BINARY_FRAME_TYPE_FILE_TRANSFER = "FILE";

// BINARY_FRAME_TYPE_FILE_ACK is the frame type of an acknowledgement
// frame (receiver → sender).
export const BINARY_FRAME_TYPE_FILE_ACK = "FACK";

// FILE_FRAME_HEADER_SIZE is the size of a FILE frame's fixed header in
// bytes; the payload follows it.
export const FILE_FRAME_HEADER_SIZE = 48;

// ACK_FRAME_SIZE is the exact size of a FACK frame in bytes.
export const ACK_FRAME_SIZE = 32;

// FileTransferFrame is the decoded form of a FILE frame.
export interface FileTransferFrame {
  frameType: typeof BINARY_FRAME_TYPE_FILE_TRANSFER;
  /** the file's UUID, canonical (lowercase, dashed) string form */
  fileId: string;
  /** per-file_id frame sequence number, from 0 */
  seq: number;
  /** byte offset of the payload in the original file */
  offset: bigint;
  /** size of the whole file in bytes */
  total: bigint;
  /** the (possibly fragmented) file content of this frame; always
      ArrayBuffer-backed, so decoded frames feed straight into a Blob */
  payload: Uint8Array<ArrayBuffer>;
}

// FileAckFrame is the decoded form of a FACK frame.
export interface FileAckFrame {
  frameType: typeof BINARY_FRAME_TYPE_FILE_ACK;
  /** the file's UUID, canonical (lowercase, dashed) string form */
  fileId: string;
  /** cumulative: the next expected seq (one past the highest
      contiguously received seq) */
  ackSeq: number;
  /** cumulative: the contiguously received byte count */
  ackedBytes: bigint;
}

// BinaryFrame is any frame the binary data channel carries.
export type BinaryFrame = FileTransferFrame | FileAckFrame;

const UUID_PATTERN =
  /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/;

// uuidToBytes packs a UUID string into its 16 wire bytes, returning null
// when the string is not a UUID.
export function uuidToBytes(uuid: string): Uint8Array | null {
  if (!UUID_PATTERN.test(uuid)) {
    return null;
  }
  const hex = uuid.replaceAll("-", "");
  const bytes = new Uint8Array(16);
  for (let i = 0; i < 16; i++) {
    bytes[i] = Number.parseInt(hex.slice(i * 2, i * 2 + 2), 16);
  }
  return bytes;
}

// bytesToUuid unpacks 16 wire bytes into the canonical (lowercase,
// dashed) UUID string. The input must be exactly 16 bytes — the codec
// only ever calls it on the 16-byte file_id field.
export function bytesToUuid(bytes: Uint8Array): string {
  let hex = "";
  for (const b of bytes) {
    hex += b.toString(16).padStart(2, "0");
  }
  return (
    `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-` +
    `${hex.slice(16, 20)}-${hex.slice(20)}`
  );
}

// writeHeader writes the frame type (4 ASCII bytes) and the file id
// (16 bytes) shared by every frame kind.
function writeHeader(
  out: Uint8Array,
  frameType: string,
  fileId: Uint8Array,
): void {
  for (let i = 0; i < 4; i++) {
    out[i] = frameType.charCodeAt(i);
  }
  out.set(fileId, 4);
}

// encodeBinaryFrame serializes a BinaryFrame for the wire, the
// encodeDCMsg counterpart for binary frames. A fileId that is not a UUID
// is a programming error (the caller mints it) and throws.
export function encodeBinaryFrame(frame: BinaryFrame): ArrayBuffer {
  const fileId = uuidToBytes(frame.fileId);
  if (fileId === null) {
    throw new TypeError(
      `binaryframes: fileId must be a UUID string, got ` +
        JSON.stringify(frame.fileId),
    );
  }
  if (frame.frameType === BINARY_FRAME_TYPE_FILE_TRANSFER) {
    const out = new Uint8Array(
      FILE_FRAME_HEADER_SIZE + frame.payload.byteLength,
    );
    writeHeader(out, frame.frameType, fileId);
    const view = new DataView(out.buffer);
    view.setUint32(20, frame.seq);
    view.setBigUint64(24, frame.offset);
    view.setBigUint64(32, frame.total);
    view.setBigUint64(40, BigInt(frame.payload.byteLength));
    out.set(frame.payload, FILE_FRAME_HEADER_SIZE);
    return out.buffer;
  }
  const out = new Uint8Array(ACK_FRAME_SIZE);
  writeHeader(out, frame.frameType, fileId);
  const view = new DataView(out.buffer);
  view.setUint32(20, frame.ackSeq);
  view.setBigUint64(24, frame.ackedBytes);
  return out.buffer;
}

// decodeBinaryFrame parses one binary data-channel frame back into a
// BinaryFrame, returning null for non-binary or malformed frames
// (dropped silently, mirroring decodeDCMsg's rule). Only ArrayBuffer
// frames are accepted — the binary channel's binaryType is arraybuffer.
export function decodeBinaryFrame(data: unknown): BinaryFrame | null {
  if (!(data instanceof ArrayBuffer)) {
    return null;
  }
  const bytes = new Uint8Array(data);
  if (bytes.byteLength < 4) {
    return null;
  }
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  const frameType = String.fromCharCode(bytes[0], bytes[1], bytes[2], bytes[3]);
  if (frameType === BINARY_FRAME_TYPE_FILE_TRANSFER) {
    if (bytes.byteLength < FILE_FRAME_HEADER_SIZE) {
      return null;
    }
    // payload_size is redundant with the frame's length — a data-channel
    // message is delivered whole — so it doubles as a consistency check.
    if (
      view.getBigUint64(40) !==
      BigInt(bytes.byteLength - FILE_FRAME_HEADER_SIZE)
    ) {
      return null;
    }
    return {
      frameType: BINARY_FRAME_TYPE_FILE_TRANSFER,
      fileId: bytesToUuid(bytes.subarray(4, 20)),
      seq: view.getUint32(20),
      offset: view.getBigUint64(24),
      total: view.getBigUint64(32),
      payload: bytes.subarray(FILE_FRAME_HEADER_SIZE),
    };
  }
  if (frameType === BINARY_FRAME_TYPE_FILE_ACK) {
    if (bytes.byteLength !== ACK_FRAME_SIZE) {
      return null;
    }
    return {
      frameType: BINARY_FRAME_TYPE_FILE_ACK,
      fileId: bytesToUuid(bytes.subarray(4, 20)),
      ackSeq: view.getUint32(20),
      ackedBytes: view.getBigUint64(24),
    };
  }
  return null;
}
