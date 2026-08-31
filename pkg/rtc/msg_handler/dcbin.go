package msg_handler

// This file is the Go mirror of the browser's binary data channel
// ("dcbin") codec — web/site/src/api/ss/binaryframes.ts is the source of
// truth. Every frame starts with a 4-byte ASCII frame type; all multi-byte
// integers are big-endian. Two frame kinds exist:
//
//	FILE — one (possibly fragmented) block of a file, sender → receiver:
//	a 48-byte header (frame type, 16-byte packed UUID file id, uint32 seq,
//	uint64 offset, uint64 total, uint64 payload size) followed by the
//	payload. A file id is a stream: files multiplex over one channel.
//
//	FACK — receiver → sender acknowledgement of the contiguous prefix
//	received so far; 32 bytes: frame type, file id, cumulative ack_seq
//	(one past the highest contiguous seq) and cumulative acked_bytes.
//
// The Server only ever receives FILE frames and sends FACK frames, so only
// the FACK encoder lives here.

import (
	"encoding/binary"

	"github.com/google/uuid"
)

const (
	// binaryFrameTypeFile / binaryFrameTypeAck are the 4-byte ASCII frame
	// types of a file-block frame and an acknowledgement frame.
	binaryFrameTypeFile = "FILE"
	binaryFrameTypeAck  = "FACK"

	// fileFrameHeaderSize is the size of a FILE frame's fixed header; the
	// payload follows it. ackFrameSize is the exact size of a FACK frame.
	fileFrameHeaderSize = 48
	ackFrameSize        = 32
)

// binaryFrame is one decoded dcbin frame. Sealed: all implementations
// live here.
type binaryFrame interface{ isBinaryFrame() }

// fileFrame is a decoded FILE frame: one block of a file's stream.
type fileFrame struct {
	fileId  uuid.UUID
	seq     uint32
	offset  uint64
	total   uint64
	payload []byte
}

// ackFrame is a decoded FACK frame: a cumulative acknowledgement of a
// file stream's contiguous prefix.
type ackFrame struct {
	fileId     uuid.UUID
	ackSeq     uint32 // the next expected seq
	ackedBytes uint64 // the contiguously received byte count
}

func (*fileFrame) isBinaryFrame() {}
func (*ackFrame) isBinaryFrame()  {}

// decodeBinaryFrame parses one dcbin frame, returning nil for malformed
// frames (dropped silently, mirroring the frontend's decodeBinaryFrame).
func decodeBinaryFrame(data []byte) binaryFrame {
	if len(data) < 4 {
		return nil
	}
	switch string(data[:4]) {
	case binaryFrameTypeFile:
		if len(data) < fileFrameHeaderSize {
			return nil
		}
		// payload_size is redundant with the frame's length — a data
		// channel message is delivered whole — so it doubles as a
		// consistency check.
		if binary.BigEndian.Uint64(data[40:48]) != uint64(len(data)-fileFrameHeaderSize) {
			return nil
		}
		var fileId uuid.UUID
		copy(fileId[:], data[4:20])
		return &fileFrame{
			fileId:  fileId,
			seq:     binary.BigEndian.Uint32(data[20:24]),
			offset:  binary.BigEndian.Uint64(data[24:32]),
			total:   binary.BigEndian.Uint64(data[32:40]),
			payload: data[fileFrameHeaderSize:],
		}
	case binaryFrameTypeAck:
		if len(data) != ackFrameSize {
			return nil
		}
		var fileId uuid.UUID
		copy(fileId[:], data[4:20])
		return &ackFrame{
			fileId:     fileId,
			ackSeq:     binary.BigEndian.Uint32(data[20:24]),
			ackedBytes: binary.BigEndian.Uint64(data[24:32]),
		}
	}
	return nil
}

// encode serializes the FACK frame for the wire.
func (f *ackFrame) encode() []byte {
	out := make([]byte, ackFrameSize)
	copy(out[:4], binaryFrameTypeAck)
	copy(out[4:20], f.fileId[:])
	binary.BigEndian.PutUint32(out[20:24], f.ackSeq)
	binary.BigEndian.PutUint64(out[24:32], f.ackedBytes)
	return out
}
