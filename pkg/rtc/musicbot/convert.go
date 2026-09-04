package musicbot

// This file is the track-side half of playing an audio source: the
// codecs the player's tracks carry, and the streaming conversion a
// decoded linear PCM source goes through on its way to the opus
// encoder — plus the looping packet read the opus family's passthrough
// pump is built on.
//
// The codec follows the source — nothing is downmixed, resampled,
// decoded or re-encoded unless it must be:
//
//   - a μ-law source (8000 Hz, mono) plays as PCMU, its companded bytes
//     becoming the RTP payload byte for byte, exactly as before the
//     audio source model existed;
//   - a linear PCM source (48000 Hz, stereo) plays as opus (48000 Hz,
//     stereo): the decoded samples only change representation —
//     libopus encodes integers and floats alike, so an integer source
//     normalizes to the interleaved signed 16-bit stereo of the integer
//     API (signed 16-bit itself takes a bit-exact fast path), a float
//     source rides the float API in its own shape, and nothing passes
//     through a needless conversion on the way;
//   - an opus source plays as opus untouched: its packets are already
//     the payload the wire wants, so they pass through — read one,
//     write one (see readPacketLooping and the player's packet pump).
//
// Both codecs are among the audio codecs pion's default media engine
// registers, and every browser's WebRTC stack offers.

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/pion/webrtc/v4"

	"personal-site/pkg/models/audiosource"
)

// The codecs of the player's tracks, matched against the negotiated
// ones when a track binds.
var (
	// pcmuTrackCodec carries a μ-law source's bytes as PCMU.
	pcmuTrackCodec = webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypePCMU,
		ClockRate: sampleRate,
		Channels:  1,
	}
	// opusTrackCodec carries a linear PCM source's music as opus. The
	// fmtp mirrors the one the server's peer connections negotiate
	// (cmd/server's stereoOpusPCFactory): stereo=1 is what keeps the
	// receiving browser from collapsing the music to mono (RFC 7587).
	opusTrackCodec = webrtc.RTPCodecCapability{
		MimeType:    webrtc.MimeTypeOpus,
		ClockRate:   opusSampleRate,
		Channels:    opusChannels,
		SDPFmtpLine: "minptime=10;useinbandfec=1;stereo=1",
	}
)

// loopingSource is a readable, rewindable stream — the shape of an
// opened audiosource.Stream and of anything wrapping one.
type loopingSource interface {
	io.Reader
	Rewind(ctx context.Context) error
}

// readLooping fills buf from src, rewinding the stream to its first
// sample whenever it ends — the source plays indefinitely, looping
// whatever it holds. A frame straddling the stream's end takes its tail
// from the stream's beginning; a fill that starts exactly at the
// stream's end rewinds just the same (the previous fill consumed it to
// the last byte without meeting the end).
func readLooping(ctx context.Context, src loopingSource, buf []byte) error {
	n := 0
	progress := 0    // bytes read since the last rewind
	stalled := false // the last rewind yielded nothing
	for n < len(buf) {
		nn, err := src.Read(buf[n:])
		n += nn
		progress += nn
		if err == nil {
			continue
		}
		if err != io.EOF {
			return err
		}
		if progress == 0 {
			// The stream ends without yielding a byte since the last
			// rewind. Once is the boundary case above; twice in a row is an
			// empty source, which would loop here forever.
			if stalled {
				return fmt.Errorf("the audio source holds no samples")
			}
			stalled = true
		} else {
			stalled = false
		}
		if err := src.Rewind(ctx); err != nil {
			return fmt.Errorf("rewind the audio source: %w", err)
		}
		progress = 0
	}
	return nil
}

// packetCloser is an open packet stream: readable packet by packet,
// rewindable, closable — the shape of an opened
// audiosource.PacketStream.
type packetCloser interface {
	packetSource
	io.Closer
}

// packetSource is a readable, rewindable packet stream.
type packetSource interface {
	ReadPacket() (data []byte, duration time.Duration, err error)
	Rewind(ctx context.Context) error
}

// readPacketLooping reads the next whole packet, rewinding the stream
// to its first packet whenever it ends — the opus family's counterpart
// of readLooping. A packet is atomic, so unlike a byte frame it cannot
// straddle the end: a truncated final packet is the stream's own
// discard, and the loop restarts at the first packet. Two consecutive
// end-of-data reads without a packet between them are an empty source,
// which would loop here forever.
func readPacketLooping(ctx context.Context, src packetSource) ([]byte, time.Duration, error) {
	stalled := false // the last rewind yielded no packet
	for {
		data, dur, err := src.ReadPacket()
		if err == nil {
			return data, dur, nil
		}
		if err != io.EOF {
			return nil, 0, err
		}
		if stalled {
			return nil, 0, fmt.Errorf("the audio source holds no packets")
		}
		stalled = true
		if err := src.Rewind(ctx); err != nil {
			return nil, 0, fmt.Errorf("rewind the audio source: %w", err)
		}
	}
}

// stereoNormalizer adapts one open read of a decoded linear PCM source
// — any supported bit depth and numeric type, 48000 Hz stereo
// interleaved — to the interleaved stereo frame the opus encoder
// consumes: signed 16-bit samples for an integer source (the integer
// API's own shape), float samples for a float one (the float API's). The
// conversion is streaming: each frame reads its own worth of source
// bytes; nothing is held beyond the frame.
type stereoNormalizer struct {
	in loopingSource

	width    int  // one sample's bytes: 1, 2 or 4
	unsigned bool // NumericType is unsigned_integer
	float    bool // NumericType is float (32-bit)

	raw []byte // scratch: one frame's worth of source bytes
}

// newStereoNormalizer wraps the stream of a source whose effective
// format the caller already knows (an opened stream's Format).
func newStereoNormalizer(in loopingSource, bitDepth int, numericType string) *stereoNormalizer {
	n := &stereoNormalizer{
		in:       in,
		width:    bitDepth / 8,
		unsigned: numericType == audiosource.NumUnsignedInt,
		float:    numericType == audiosource.NumFloat,
	}
	n.raw = make([]byte, opusPCMPerFrame*n.width)
	return n
}

// readFrame fills pcm — interleaved signed 16-bit stereo samples —
// with the next frame's worth of an integer source, looping over the
// source's end like the player's other reads.
func (n *stereoNormalizer) readFrame(ctx context.Context, pcm []int16) error {
	if len(n.raw) != len(pcm)*n.width {
		n.raw = make([]byte, len(pcm)*n.width)
	}
	if err := readLooping(ctx, n.in, n.raw); err != nil {
		return err
	}
	if n.width == 2 && !n.unsigned && !n.float {
		// Signed 16-bit is the encoder's own shape: pass the samples
		// through bit-exactly.
		for i := range pcm {
			pcm[i] = int16(binary.LittleEndian.Uint16(n.raw[2*i:]))
		}
		return nil
	}
	for i := range pcm {
		pcm[i] = sampleToInt16(n.raw[i*n.width:(i+1)*n.width], n.unsigned)
	}
	return nil
}

// readFrameFloat32 fills pcm — interleaved float stereo samples — with
// the next frame's worth of a float source: the samples decode in the
// encoder's own shape, so nothing is converted and nothing is lost —
// only clipped, as the integer path clips what runs past [-1, 1].
func (n *stereoNormalizer) readFrameFloat32(ctx context.Context, pcm []float32) error {
	if len(n.raw) != len(pcm)*n.width {
		n.raw = make([]byte, len(pcm)*n.width)
	}
	if err := readLooping(ctx, n.in, n.raw); err != nil {
		return err
	}
	for i := range pcm {
		v := math.Float32frombits(binary.LittleEndian.Uint32(n.raw[i*n.width:]))
		pcm[i] = max(-1, min(1, v))
	}
	return nil
}

// sampleToInt16 converts one little-endian source sample of the given
// integer width and signedness to a signed 16-bit sample, normalizing
// through [-1, 1] and clipping.
func sampleToInt16(b []byte, unsigned bool) int16 {
	var v float64
	if unsigned {
		switch len(b) {
		case 1:
			v = (float64(b[0]) - 128) / 128
		case 2:
			v = (float64(binary.LittleEndian.Uint16(b)) - 32768) / 32768
		case 4:
			v = (float64(binary.LittleEndian.Uint32(b)) - 2147483648) / 2147483648
		}
	} else {
		switch len(b) {
		case 1:
			v = float64(int8(b[0])) / 128
		case 2:
			v = float64(int16(binary.LittleEndian.Uint16(b))) / 32768
		case 4:
			v = float64(int32(binary.LittleEndian.Uint32(b))) / 2147483648
		}
	}
	v = math.Max(-1, math.Min(1, v))
	return int16(math.Round(v * 32767))
}
