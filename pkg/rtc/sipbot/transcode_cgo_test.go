//go:build cgo

package sipbot

// This file's tests exercise the transcoding paths, which need libopus
// (cgo); the passthrough matrix is transcode_test.go's, pure Go.

import (
	"math"
	"testing"
	"time"

	"github.com/emiago/diago/media"
	"github.com/zaf/g711"
)

// sine48 renders n samples of a 48 kHz mono 1 kHz tone at the given
// amplitude — smooth enough that the lossy chain stays close.
func sine48(n int, amp float64) []int16 {
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(amp * math.Sin(2*math.Pi*float64(i)/48))
	}
	return out
}

// TestToBrowserBridgeG711 runs the callee→browser transcode: μ-law
// payloads in, opus samples out — one whole 20 ms frame per 160-byte
// payload, a partial one never — decoded back to PCM that tracks the
// μ-law original.
func TestToBrowserBridgeG711(t *testing.T) {
	b, err := newToBrowserBridge(media.CodecAudioUlaw)
	if err != nil {
		t.Skipf("the opus codec is unavailable: %v", err)
	}
	// 20 ms of a 1 kHz tone at 8 kHz, μ-law encoded.
	pcm8 := make([]int16, 160)
	for i := range pcm8 {
		pcm8[i] = int16(10000 * math.Sin(2*math.Pi*float64(i)/8))
	}
	payload := make([]byte, 160)
	for i, s := range pcm8 {
		payload[i] = g711.EncodeUlawFrame(s)
	}

	var packets [][]byte
	var durs []time.Duration
	if err := b.put(payload, func(data []byte, dur time.Duration) error {
		packets = append(packets, append([]byte(nil), data...))
		durs = append(durs, dur)
		return nil
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if len(packets) != 1 || durs[0] != frameDuration {
		t.Fatalf("one 160-byte payload produced %d samples (durs %v), want 1 × 20 ms", len(packets), durs)
	}

	// Decode the opus packet with a fresh decoder (independent of the
	// bridge's state): 20 ms of stereo carrying the mono tone on both
	// channels, tracking the μ-law source's smooth shape.
	dec, err := newOpusDecoder()
	if err != nil {
		t.Fatalf("newOpusDecoder: %v", err)
	}
	stereo, err := dec.decode(packets[0])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(stereo) != 2*frameSamples48 {
		t.Fatalf("decoded %d samples, want %d (20 ms stereo)", len(stereo), 2*frameSamples48)
	}
	// Zero crossings survive the chain (the tone's period is 48 samples
	// at 48 kHz): count sign changes of the left channel past the
	// codec's onset.
	mono := downmixStereo(nil, stereo)
	crossings := 0
	for i := 1 + 48; i < len(mono); i++ {
		if (mono[i-1] < 0) != (mono[i] < 0) {
			crossings++
		}
	}
	if crossings < 10 {
		t.Fatalf("the decoded tone has %d zero crossings, want the 1 kHz shape (≈38)", crossings)
	}

	// A half-size payload (10 ms) yields nothing until its second half
	// arrives — the accumulator's ptime freedom.
	if err := b.put(payload[:80], func(data []byte, dur time.Duration) error {
		t.Fatal("a 10 ms payload produced a frame")
		return nil
	}); err != nil {
		t.Fatalf("put half: %v", err)
	}
	var second int
	if err := b.put(payload[:80], func(data []byte, dur time.Duration) error {
		second++
		return nil
	}); err != nil {
		t.Fatalf("put second half: %v", err)
	}
	if second != 1 {
		t.Fatalf("two 10 ms halves produced %d frames, want 1", second)
	}
}

// TestToSIPBridgeG711 runs the browser→callee transcode: an opus packet
// in, whole 160-byte μ-law frames out — decoded PCM tracking the
// original tone's shape.
func TestToSIPBridgeG711(t *testing.T) {
	b, err := newToSIPBridge(media.CodecAudioUlaw)
	if err != nil {
		t.Skipf("the opus codec is unavailable: %v", err)
	}
	// 20 ms of the 1 kHz tone at 48 kHz, opus-encoded (mono — exactly
	// what the bridge's own encoder emits, so the chain round-trips).
	enc, err := newOpusEncoder()
	if err != nil {
		t.Fatalf("newOpusEncoder: %v", err)
	}
	pkt, err := enc.encode(sine48(frameSamples48, 10000))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	var payloads [][]byte
	if err := b.put(pkt, func(data []byte) error {
		payloads = append(payloads, append([]byte(nil), data...))
		return nil
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if len(payloads) != 1 || len(payloads[0]) != frameSamples8 {
		t.Fatalf("one 20 ms packet produced %d payloads of %v bytes, want 1 × 160",
			len(payloads), lens(payloads))
	}
	// The μ-law decodes to the tone at 8 kHz (period 8): count zero
	// crossings past the codec's onset.
	crossings := 0
	prev := g711.DecodeUlawFrame(payloads[0][0])
	for _, c := range payloads[0][8:] {
		cur := g711.DecodeUlawFrame(c)
		if (prev < 0) != (cur < 0) {
			crossings++
		}
		prev = cur
	}
	if crossings < 10 {
		t.Fatalf("the transcoded tone has %d zero crossings, want the 1 kHz shape (≈38)", crossings)
	}
}

// TestTranscodeRoundTrip chains the two G.711 paths: μ-law → opus →
// μ-law preserves the tone's shape — the two bridges facing each other,
// as they do in a call.
func TestTranscodeRoundTrip(t *testing.T) {
	toBrowser, err := newToBrowserBridge(media.CodecAudioUlaw)
	if err != nil {
		t.Skipf("the opus codec is unavailable: %v", err)
	}
	toSIP, err := newToSIPBridge(media.CodecAudioUlaw)
	if err != nil {
		t.Skipf("the opus codec is unavailable: %v", err)
	}
	pcm8 := make([]int16, 160)
	for i := range pcm8 {
		pcm8[i] = int16(10000 * math.Sin(2*math.Pi*float64(i)/8))
	}
	in := make([]byte, 160)
	for i, s := range pcm8 {
		in[i] = g711.EncodeUlawFrame(s)
	}
	var out []byte
	err = toBrowser.put(in, func(data []byte, dur time.Duration) error {
		return toSIP.put(data, func(back []byte) error {
			out = append(out, back...)
			return nil
		})
	})
	if err != nil {
		t.Fatalf("the round trip: %v", err)
	}
	if len(out) != frameSamples8 {
		t.Fatalf("the round trip produced %d bytes, want %d", len(out), frameSamples8)
	}
	// The shape survives the chain's group delay (libopus's lookahead
	// plus the resamplers' span): the 1 kHz tone's period structure —
	// its zero crossings — is intact, though no index-wise comparison is
	// meaningful.
	crossings := 0
	prev := g711.DecodeUlawFrame(out[0])
	for _, c := range out[8:] {
		cur := g711.DecodeUlawFrame(c)
		if (prev < 0) != (cur < 0) {
			crossings++
		}
		prev = cur
	}
	if crossings < 20 {
		t.Fatalf("the round trip produced %d zero crossings, want the 1 kHz shape (≈38)", crossings)
	}
}

func lens(payloads [][]byte) []int {
	out := make([]int, len(payloads))
	for i, p := range payloads {
		out[i] = len(p)
	}
	return out
}
