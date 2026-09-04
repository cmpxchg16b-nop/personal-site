package audiosource

// This file is the ogg-framed opus source's stream: the packets the Ogg
// container frames (RFC 7845), served whole through ReadPacket. The
// demuxing itself — page parsing and checksums, packet reassembly from
// the segment lacing — is pion's (github.com/pion/opus/pkg/oggreader);
// what lives here is the source model's part: the format the ID header
// declares takes precedence over the metadata, the two header packets
// are skipped (and re-skipped across a chained segment), and the
// end-of-data count is checked.
//
// Nothing here decodes or encodes anything: an opus source is already
// the audio the wire wants, so its stream is packets and only packets —
// a consumer passes them through.
//
// A packet's duration comes from its own table of contents (RFC 6716
// §3.1 — the frame size its configuration codes, times the frame count),
// never from the pages' granule positions: a granule position is only a
// counter, and for a live stream it counts from whenever the encoder
// started — a join mid-stream reads an arbitrary, huge value, and a
// duration derived from it would be hours of silence. (A granule can
// also jump mid-stream when a live encoder flushes its queue; the TOC
// is indifferent to both.)

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/pion/opus/pkg/oggreader"
)

// The opus header packets' magic numbers (RFC 7845 §5): the ID header a
// stream (or a chained segment) begins with, and the comment header
// that follows it. Neither is audio; both are skipped.
var (
	opusHeadMagic = []byte("OpusHead")
	opusTagsMagic = []byte("OpusTags")
)

// opusClockRate is the opus codec's own rate — the one rate the codec
// ever runs at, so the one a duration in samples converts with.
const opusClockRate = 48000

// oggStream is an ogg-framed opus source's stream: pion's OggReader
// over the raw bytes, wrapped in the source model's stream discipline.
type oggStream struct {
	raw *rawReader
	ogg *oggreader.OggReader

	// format is the effective format: the opus family, the ID header's
	// channel count, the codec's own rate, the declared sample count.
	format Format
	// preSkip is the ID header's pre-skip — the encoder's head padding,
	// which the packet durations count but the declared sample count
	// does not; the end-of-data check's tolerance.
	preSkip uint16

	// served is the media time handed out since the start or the last
	// rewind, in samples — the end-of-data count check's measure.
	served int64
}

// parse reads the ID header and derives the stream's effective format
// from it, mirroring the flac path: the header takes precedence over
// the declared metadata, and the combination it describes must be one
// of the supported ones.
func (s *oggStream) parse() error {
	ogg, hdr, err := oggreader.NewWith(s.raw)
	if err != nil {
		return fmt.Errorf("audiosource: %s: parse ogg: %w", s.raw.src.Id, err)
	}
	if err := s.adoptHeader(hdr.Version, hdr.Channels, hdr.PreSkip, hdr.SampleRate); err != nil {
		return err
	}
	s.ogg = ogg
	return nil
}

// adoptHeader validates an ID header's description (the stream's first,
// or a chained segment's) and takes it as the stream's format. The
// sample rate the header declares is the rate the encoder was fed —
// informational per RFC 7845 §5.1, but the one field a source has to
// say 48000 with: the model accepts 48000 Hz opus streams only (the
// codec's own clock is 48000 either way, so nothing is lost by the
// refusal).
func (s *oggStream) adoptHeader(version, channels uint8, preSkip uint16, rate uint32) error {
	if version != 1 {
		return fmt.Errorf("audiosource: %s: the opus id header version is %d, want 1", s.raw.src.Id, version)
	}
	if rate != opusClockRate {
		return fmt.Errorf("audiosource: %s: the opus stream's sample rate is %d, want 48000", s.raw.src.Id, rate)
	}
	format := Format{
		SampleFormatType: SampleOpus,
		NumChannels:      int(channels),
		SampleRate:       opusClockRate,
		NumTotalSamples:  s.raw.src.NumTotalSamples,
	}
	if err := validateFormat(format); err != nil {
		return fmt.Errorf("audiosource: %s: the opus id header says %w", s.raw.src.Id, err)
	}
	s.format = format
	s.preSkip = preSkip
	return nil
}

// ReadPacket returns the next whole opus packet and its duration — the
// media time the packet's own table of contents codes. The header
// packets of the stream (and of any chained segment beyond it) are
// skipped; a chained segment's ID header re-validates the stream's
// format. The data slice is valid until the next ReadPacket (see
// PacketStream).
func (s *oggStream) ReadPacket() ([]byte, time.Duration, error) {
	for {
		pkt, _, err := s.ogg.ParseNextPacket()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				// A partial packet in the reader's hands is a truncated
				// tail, discarded — the count check below says whether
				// the stream came up short.
				if cerr := s.checkCount(); cerr != nil {
					return nil, 0, cerr
				}
				return nil, 0, io.EOF
			}
			return nil, 0, fmt.Errorf("audiosource: %s: read ogg: %w", s.raw.src.Id, err)
		}
		if bytes.HasPrefix(pkt, opusHeadMagic) {
			if len(pkt) < 19 {
				return nil, 0, fmt.Errorf("audiosource: %s: a short chained opus id header", s.raw.src.Id)
			}
			if err := s.adoptHeader(
				pkt[8],
				pkt[9],
				binary.LittleEndian.Uint16(pkt[10:12]),
				binary.LittleEndian.Uint32(pkt[12:16]),
			); err != nil {
				return nil, 0, err
			}
			continue
		}
		if bytes.HasPrefix(pkt, opusTagsMagic) {
			continue // the comment header carries no audio
		}
		samples, err := opusPacketSamples(pkt)
		if err != nil {
			return nil, 0, fmt.Errorf("audiosource: %s: %w", s.raw.src.Id, err)
		}
		s.served += samples
		return pkt, time.Duration(samples) * time.Second / opusClockRate, nil
	}
}

// opusPacketSamples is the packet's media time in samples at the
// codec's 48 kHz clock: the frame size its TOC configuration codes
// (RFC 6716 §3.1), times the frame count the TOC's frame code implies
// (the frame-count byte for code 3 packets). All frames of a packet
// share the configuration's frame size — the packet's duration is the
// count times the size (pion's decoder computes it the same way).
func opusPacketSamples(pkt []byte) (int64, error) {
	if len(pkt) == 0 {
		return 0, errors.New("a zero-length opus packet")
	}
	toc := pkt[0]
	var frameSamples int64
	switch toc >> 3 {
	case 16, 20, 24, 28:
		frameSamples = 120 // 2.5 ms
	case 17, 21, 25, 29:
		frameSamples = 240 // 5 ms
	case 0, 4, 8, 12, 14, 18, 22, 26, 30:
		frameSamples = 480 // 10 ms
	case 1, 5, 9, 13, 15, 19, 23, 27, 31:
		frameSamples = 960 // 20 ms
	case 2, 6, 10:
		frameSamples = 1920 // 40 ms
	default: // 3, 7, 11
		frameSamples = 2880 // 60 ms
	}
	var frames int64
	switch toc & 3 {
	case 0:
		frames = 1
	case 1, 2:
		frames = 2
	default: // 3: the frame-count byte's low six bits
		if len(pkt) < 2 {
			return 0, errors.New("a code 3 opus packet with no frame-count byte")
		}
		frames = int64(pkt[1] & 0x3F)
		if frames == 0 {
			return 0, errors.New("a code 3 opus packet with zero frames")
		}
	}
	return frames * frameSamples, nil
}

// Rewind restarts the stream at its first packet: the raw read rewinds,
// and the demuxer restarts fresh over the rewound bytes (a re-fetched
// http body is re-validated by the headers it serves).
func (s *oggStream) Rewind(ctx context.Context) error {
	if err := s.raw.Rewind(ctx); err != nil {
		return err
	}
	ogg, hdr, err := oggreader.NewWith(s.raw)
	if err != nil {
		return fmt.Errorf("audiosource: %s: parse ogg on rewind: %w", s.raw.src.Id, err)
	}
	if err := s.adoptHeader(hdr.Version, hdr.Channels, hdr.PreSkip, hdr.SampleRate); err != nil {
		return err
	}
	s.ogg = ogg
	s.served = 0
	return nil
}

// Close drops the raw read; the demuxer holds nothing of its own.
func (s *oggStream) Close() error { return s.raw.Close() }

func (s *oggStream) Format() Format { return s.format }

// checkCount verifies the served media time against the expected
// total: a stream that ended early (or ran past) contradicts the
// metadata and is an error, not a silently short song. A streaming
// source (no expected total) checks nothing. Unlike the sample
// families' byte-exact check, this one tolerates the padding a real
// encoder adds: the pre-skip at the head and the trimmed frame at the
// tail — the packets' own durations count both, the declared count
// neither.
func (s *oggStream) checkCount() error {
	want := int64(s.format.NumTotalSamples)
	if want <= 0 {
		return nil
	}
	// 5760 samples is 120 ms — the longest packet the codec codes.
	if slack := int64(s.preSkip) + 5760; s.served < want || s.served > want+slack {
		return fmt.Errorf("audiosource: %s: the ogg stream served %d samples, want %d (±%d)",
			s.raw.src.Id, s.served, want, slack)
	}
	return nil
}
