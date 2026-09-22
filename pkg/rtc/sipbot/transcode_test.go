package sipbot

import (
	"testing"
	"time"

	"github.com/emiago/diago/media"
	"github.com/zaf/g711"
)

// TestOpusPacketDuration checks the TOC parse against RFC 6716 §3.1's
// shapes: SILK, hybrid and CELT configurations, the frame-count codes
// (including code 3's count byte), and the malformed fallbacks.
func TestOpusPacketDuration(t *testing.T) {
	// The TOC packs the fields: config in bits 3-7, the frame-count code
	// in bits 0-1.
	for _, tc := range []struct {
		name string
		pkt  []byte
		want time.Duration
	}{
		{"empty", nil, frameDuration},
		{"code3 without count byte", []byte{0x03}, frameDuration},
		{"code3 zero count", []byte{0x03, 0x00}, frameDuration},
		{"silk 10ms x1", []byte{0 << 3}, 10 * time.Millisecond},
		{"silk 20ms x1", []byte{1 << 3}, 20 * time.Millisecond},
		{"silk 40ms x2 code1", []byte{2<<3 | 1}, 80 * time.Millisecond},
		{"silk 60ms x1", []byte{3 << 3}, 60 * time.Millisecond},
		{"hybrid 10ms x1", []byte{12 << 3}, 10 * time.Millisecond},
		{"hybrid 20ms x2 code2", []byte{13<<3 | 2}, 40 * time.Millisecond},
		{"celt 2.5ms x1", []byte{16 << 3}, 2500 * time.Microsecond},
		{"celt 20ms x1", []byte{19 << 3}, 20 * time.Millisecond},
		{"celt 20ms x4 code3", []byte{19<<3 | 3, 4 << 2}, 80 * time.Millisecond},
		{"fullband voice packet (0xf8)", []byte{0xf8}, 20 * time.Millisecond},
		{"overlong is refused", []byte{3<<3 | 3, 3 << 2}, frameDuration}, // 3 × 60 ms > 120 ms
	} {
		if got := opusPacketDuration(tc.pkt); got != tc.want {
			t.Errorf("%s: opusPacketDuration = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestUpsample8to48 checks the 1:6 interpolation: the output is six
// times the input, endpoints exact, steps linear.
func TestUpsample8to48(t *testing.T) {
	out := upsample8to48(nil, []int16{0, 600, -600})
	if len(out) != 18 {
		t.Fatalf("len = %d, want 18", len(out))
	}
	// 0 → 600 in steps of 100.
	for k := 0; k < 6; k++ {
		if out[k] != int16(k*100) {
			t.Fatalf("out[%d] = %d, want %d", k, out[k], k*100)
		}
	}
	// 600 → -600 in steps of -200.
	for k := 0; k < 6; k++ {
		if out[6+k] != int16(600-k*200) {
			t.Fatalf("out[%d] = %d, want %d", 6+k, out[6+k], 600-k*200)
		}
	}
	// The final sample holds.
	for k := 0; k < 6; k++ {
		if out[12+k] != -600 {
			t.Fatalf("out[%d] = %d, want -600", 12+k, out[12+k])
		}
	}
}

// TestDownsample48to8 checks the 6:1 box average and the G.711 encoding
// against zaf/g711's own frame functions.
func TestDownsample48to8(t *testing.T) {
	src := make([]int16, 12) // two output samples
	for i := range src[:6] {
		src[i] = 60
	}
	for i := 6; i < 12; i++ {
		src[i] = -120
	}
	dst := make([]byte, 4)
	n := downsample48to8(dst, src, false)
	if n != 2 {
		t.Fatalf("n = %d, want 2", n)
	}
	if dst[0] != g711.EncodeUlawFrame(60) || dst[1] != g711.EncodeUlawFrame(-120) {
		t.Fatalf("payload = %v, want μ-law of the averages", dst[:n])
	}
	n = downsample48to8(dst, src, true)
	if dst[0] != g711.EncodeAlawFrame(60) || dst[1] != g711.EncodeAlawFrame(-120) {
		t.Fatalf("payload = %v, want A-law of the averages", dst[:n])
	}
	// A tail of fewer than six samples is left for the accumulator's
	// next fill: 12 in, 2 out; 5 in, 0 more.
	if n := downsample48to8(dst, src[:5], false); n != 0 {
		t.Fatalf("a 5-sample tail produced %d samples", n)
	}
}

// TestDownmixStereo checks the stereo fold: mean of each pair.
func TestDownmixStereo(t *testing.T) {
	out := downmixStereo(nil, []int16{100, 300, -100, -300, 7})
	if len(out) != 2 || out[0] != 200 || out[1] != -200 {
		t.Fatalf("out = %v, want [200 -200]", out)
	}
}

// TestPCMAccumulator checks the ptime decoupling: arbitrary input chunk
// sizes drain as whole frames only.
func TestPCMAccumulator(t *testing.T) {
	var acc pcmAccumulator
	frame := make([]int16, 960)
	if acc.next(frame) {
		t.Fatal("an empty accumulator yields a frame")
	}
	acc.put(make([]int16, 500))
	if acc.next(frame) {
		t.Fatal("a short accumulator yields a frame")
	}
	acc.put(make([]int16, 500))
	if !acc.next(frame) {
		t.Fatal("960 queued samples yield no frame")
	}
	if acc.next(frame) {
		t.Fatal("the drained remainder (40) yields a frame")
	}
	// The values are the oldest first (a fresh accumulator, so the drain
	// starts at the first sample put).
	var ordered pcmAccumulator
	ordered.put([]int16{1, 2, 3})
	ordered.put(make([]int16, 960))
	if !ordered.next(frame) || frame[0] != 1 || frame[1] != 2 || frame[2] != 3 {
		t.Fatal("the accumulator reordered its samples")
	}
}

// TestToBrowserBridgePassthrough checks the opus↔opus relay: payloads
// cross byte for byte, timestamped by their own TOCs — no codec runs.
func TestToBrowserBridgePassthrough(t *testing.T) {
	b, err := newToBrowserBridge(media.CodecAudioOpus)
	if err != nil {
		t.Fatalf("newToBrowserBridge: %v", err)
	}
	pkt := make([]byte, 40)
	pkt[0] = 0xF8 // a fullband 20 ms frame
	for i := 1; i < len(pkt); i++ {
		pkt[i] = byte(i * 31)
	}
	var calls int
	err = b.put(pkt, func(data []byte, dur time.Duration) error {
		calls++
		if dur != 20*time.Millisecond {
			t.Fatalf("duration = %v, want 20 ms", dur)
		}
		if len(data) != len(pkt) {
			t.Fatalf("payload length = %d, want %d", len(data), len(pkt))
		}
		for i := range data {
			if data[i] != pkt[i] {
				t.Fatalf("payload byte %d changed in the passthrough", i)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if calls != 1 {
		t.Fatalf("one packet in produced %d samples out, want 1", calls)
	}
}

// TestToSIPBridgePassthrough checks the other passthrough direction.
func TestToSIPBridgePassthrough(t *testing.T) {
	b, err := newToSIPBridge(media.CodecAudioOpus)
	if err != nil {
		t.Fatalf("newToSIPBridge: %v", err)
	}
	pkt := []byte{0xF8, 1, 2, 3, 4}
	var calls int
	if err := b.put(pkt, func(data []byte) error {
		calls++
		if len(data) != len(pkt) || data[0] != 0xF8 {
			t.Fatalf("payload changed in the passthrough: %v", data)
		}
		return nil
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if calls != 1 {
		t.Fatalf("one packet in produced %d payloads out, want 1", calls)
	}
}

// TestBridgeUnsupportedCodec checks the relay's refusal of a codec
// outside the matrix.
func TestBridgeUnsupportedCodec(t *testing.T) {
	exotic := media.Codec{Name: "G722", PayloadType: 9, SampleRate: 8000}
	if _, err := newToBrowserBridge(exotic); err == nil {
		t.Fatal("a G722 sip-leg produced a browser-bound bridge")
	}
	if _, err := newToSIPBridge(exotic); err == nil {
		t.Fatal("a G722 sip-leg produced a callee-bound bridge")
	}
}
