package sipbot

// This file is the relay's codec matrix. The webrtc-leg codec is fixed
// (opus 48 kHz); the sip-leg codec is negotiated (opus, PCMU, or PCMA).
// Each direction's bridge converts one sip-leg payload at a time and
// hands the result to its pump's writer through a callback, so no
// intermediate buffer outlives a call:
//
//   - opus sip-leg: a pure passthrough both ways — the codec's own
//     packets cross the SBC byte for byte, the browser-bound direction
//     timestamped by the packets' own TOC durations. No codec runs at
//     all, which is also the only path a pure-Go build needs.
//   - G.711 sip-leg: a transcoding path through mono 48 kHz PCM —
//     G.711 decode and 6:1 resampling browser-bound, opus decode and
//     1:6 resampling plus G.711 encode callee-bound. A small sample
//     accumulator decouples the input's ptime from the output's 20 ms
//     frames, so a peer sending 30 or 60 ms packets still lands on
//     whole frames.
//
// A bridge is owned by its pump goroutine alone — no locks, like the
// music bot's player.

import (
	"fmt"
	"time"

	"github.com/emiago/diago/media"
	"github.com/zaf/g711"
)

// The relay's opus shape: 48 kHz. The PCM junction's shape: mono,
// 48 kHz, 20 ms frames. G.711's own shape: mono, 8 kHz, 160 samples to
// the same 20 ms.
const (
	opusSampleRate = 48000

	frameDuration    = 20 * time.Millisecond
	frameSamples48   = opusSampleRate / 50 // 960
	frameSamples8    = 8000 / 50           // 160
	resampleFactor   = frameSamples48 / frameSamples8
	g711MaxFrameSize = 1500 // one payload's maximum bytes (≈ MTU)

	// opusMaxPacket is one encoded frame's maximum bytes; opusMaxPCM is
	// one decoded packet's maximum interleaved samples (the RFC 6716
	// 120 ms ceiling, stereo).
	opusMaxPacket = 4000
	opusMaxPCM    = opusSampleRate * 120 / 1000 * 2
)

// toBrowserBridge converts the sip-leg's payloads into opus samples for
// the webrtc-leg track: a passthrough for an opus sip-leg, a
// G.711 → PCM → opus transcode for the G.711 twins.
type toBrowserBridge struct {
	pass bool // the opus passthrough: no codecs, no accumulator
	alaw bool // the sip-leg is A-law (μ-law when false)

	enc   *opusEncoder // nil on the passthrough
	acc   pcmAccumulator
	pcm8  []int16 // scratch: one payload decoded to 8 kHz samples
	up    []int16 // scratch: one payload resampled to 48 kHz
	frame []int16 // scratch: one 20 ms frame for the encoder
}

// newToBrowserBridge builds the browser-bound bridge for the negotiated
// sip-leg codec.
func newToBrowserBridge(codec media.Codec) (*toBrowserBridge, error) {
	switch codec.Name {
	case media.CodecAudioOpus.Name:
		return &toBrowserBridge{pass: true}, nil
	case media.CodecAudioUlaw.Name, media.CodecAudioAlaw.Name:
		enc, err := newOpusEncoder()
		if err != nil {
			return nil, err
		}
		return &toBrowserBridge{
			alaw:  codec.Name == media.CodecAudioAlaw.Name,
			enc:   enc,
			pcm8:  make([]int16, 0, g711MaxFrameSize),
			up:    make([]int16, 0, g711MaxFrameSize*resampleFactor),
			frame: make([]int16, frameSamples48),
		}, nil
	}
	return nil, fmt.Errorf("sipbot: the negotiated codec %q is not relayable", codec.Name)
}

// put converts one sip-leg payload, handing each resulting sample to
// out — the pump's track write.
func (b *toBrowserBridge) put(payload []byte, out func(data []byte, dur time.Duration) error) error {
	if b.pass {
		return out(payload, opusPacketDuration(payload))
	}
	b.pcm8 = b.pcm8[:len(payload)]
	for i, c := range payload {
		if b.alaw {
			b.pcm8[i] = g711.DecodeAlawFrame(c)
		} else {
			b.pcm8[i] = g711.DecodeUlawFrame(c)
		}
	}
	b.up = upsample8to48(b.up, b.pcm8)
	b.acc.put(b.up)
	for b.acc.next(b.frame) {
		pkt, err := b.enc.encode(b.frame)
		if err != nil {
			return err
		}
		if err := out(pkt, frameDuration); err != nil {
			return err
		}
	}
	return nil
}

// toSIPBridge converts the browser's mic packets into the sip-leg's
// payloads: a passthrough for an opus sip-leg, an
// opus → PCM → G.711 transcode for the G.711 twins.
type toSIPBridge struct {
	pass bool
	alaw bool

	dec   *opusDecoder // nil on the passthrough
	acc   pcmAccumulator
	mono  []int16 // scratch: one packet decoded and downmixed
	frame []int16 // scratch: one 20 ms frame resampled to 8 kHz
	g711  []byte  // scratch: one frame encoded
}

// newToSIPBridge builds the callee-bound bridge for the negotiated
// sip-leg codec.
func newToSIPBridge(codec media.Codec) (*toSIPBridge, error) {
	switch codec.Name {
	case media.CodecAudioOpus.Name:
		return &toSIPBridge{pass: true}, nil
	case media.CodecAudioUlaw.Name, media.CodecAudioAlaw.Name:
		dec, err := newOpusDecoder()
		if err != nil {
			return nil, err
		}
		return &toSIPBridge{
			alaw:  codec.Name == media.CodecAudioAlaw.Name,
			dec:   dec,
			mono:  make([]int16, 0, opusMaxPCM/2),
			frame: make([]int16, frameSamples48),
			g711:  make([]byte, frameSamples8),
		}, nil
	}
	return nil, fmt.Errorf("sipbot: the negotiated codec %q is not relayable", codec.Name)
}

// put converts one browser packet, handing each resulting payload to
// out — the pump's dialog write.
func (b *toSIPBridge) put(payload []byte, out func(data []byte) error) error {
	if b.pass {
		return out(payload)
	}
	stereo, err := b.dec.decode(payload)
	if err != nil {
		return err
	}
	b.mono = downmixStereo(b.mono, stereo)
	b.acc.put(b.mono)
	for b.acc.next(b.frame) {
		n := downsample48to8(b.g711, b.frame, b.alaw)
		if err := out(b.g711[:n]); err != nil {
			return err
		}
	}
	return nil
}

// pcmAccumulator is a 48 kHz mono PCM queue decoupling the input's
// packet size from whole 20 ms output frames.
type pcmAccumulator struct {
	buf []int16
}

// put appends one packet's samples.
func (a *pcmAccumulator) put(samples []int16) {
	a.buf = append(a.buf, samples...)
}

// next fills frame with the oldest samples; false while fewer than a
// whole frame is queued.
func (a *pcmAccumulator) next(frame []int16) bool {
	if len(a.buf) < len(frame) {
		return false
	}
	copy(frame, a.buf[:len(frame)])
	a.buf = append(a.buf[:0], a.buf[len(frame):]...)
	return true
}

// upsample8to48 converts 8 kHz mono samples to 48 kHz into dst: every
// input sample opens a six-sample span stepping linearly toward the
// next one (the final sample holds) — telephony-grade interpolation,
// allocation-free per packet. It returns dst resliced to the output.
func upsample8to48(dst, src []int16) []int16 {
	dst = dst[:0]
	for i, s := range src {
		next := s
		if i+1 < len(src) {
			next = src[i+1]
		}
		step := int32(next) - int32(s)
		for k := 0; k < resampleFactor; k++ {
			dst = append(dst, int16(int32(s)+step*int32(k)/int32(resampleFactor)))
		}
	}
	return dst
}

// downsample48to8 converts 48 kHz mono samples to 8 kHz, encoding each
// output sample into dst as G.711 (A-law when alaw, μ-law otherwise):
// every output sample is the mean of the input's next six — a box-car
// anti-alias filter, allocation-free per frame. It returns the number
// of bytes written.
func downsample48to8(dst []byte, src []int16, alaw bool) int {
	n := 0
	for len(src) >= resampleFactor {
		var sum int32
		for k := 0; k < resampleFactor; k++ {
			sum += int32(src[k])
		}
		s := int16(sum / resampleFactor)
		if alaw {
			dst[n] = g711.EncodeAlawFrame(s)
		} else {
			dst[n] = g711.EncodeUlawFrame(s)
		}
		n++
		src = src[resampleFactor:]
	}
	return n
}

// downmixStereo folds interleaved stereo samples to mono into dst
// (mean of each pair) and returns dst resliced.
func downmixStereo(dst, src []int16) []int16 {
	dst = dst[:0]
	for i := 0; i+1 < len(src); i += 2 {
		dst = append(dst, int16((int32(src[i])+int32(src[i+1]))/2))
	}
	return dst
}

// opusPacketDuration is the packet's media duration from its TOC byte
// (RFC 6716 §3.1): the configuration's frame duration times the frame
// count (the count rides the payload's second byte for a code-3
// packet). Anything malformed answers 20 ms — the ptime the SDP world
// defaults to.
func opusPacketDuration(pkt []byte) time.Duration {
	if len(pkt) == 0 {
		return frameDuration
	}
	config := pkt[0] >> 3
	var frame time.Duration
	switch {
	case config < 12: // SILK: 10, 20, 40, 60 ms
		frame = []time.Duration{10, 20, 40, 60}[config&3] * time.Millisecond
	case config < 16: // hybrid: 10, 20 ms
		frame = []time.Duration{10, 20}[config&1] * time.Millisecond
	default: // CELT: 2.5, 5, 10, 20 ms
		frame = []time.Duration{2500, 5000, 10000, 20000}[config&3] * time.Microsecond
	}
	count := 1
	switch pkt[0] & 0x3 {
	case 1, 2:
		count = 2
	case 3:
		if len(pkt) < 2 {
			return frameDuration
		}
		count = int(pkt[1]>>2) & 0x3F
		if count == 0 {
			return frameDuration
		}
	}
	d := frame * time.Duration(count)
	if d <= 0 || d > 120*time.Millisecond {
		return frameDuration
	}
	return d
}
