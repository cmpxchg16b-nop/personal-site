package musicbot

// The unit tests of the music itself: the μ-law codec's vectors, the
// song's shape and determinism, and the player's frame arithmetic.

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// TestLinearToMuLawVectors pins the encoder to the canonical G.711
// reference values: the two zero regions, both clip ends, and the
// clipping itself.
func TestLinearToMuLawVectors(t *testing.T) {
	for _, tc := range []struct {
		sample int16
		want   byte
	}{
		{0, 0xFF},      // positive zero
		{-1, 0x7F},     // negative zero
		{32635, 0x80},  // positive full scale
		{-32635, 0x00}, // negative full scale
		{32767, 0x80},  // clipped to full scale
		{-32768, 0x00}, // clipped to negative full scale
		{8159, 0x9F},   // a mid-scale step (segment 6, mantissa 0)
		{-8159, 0x1F},  // ...mirrored
	} {
		if got := linearToMuLaw(tc.sample); got != tc.want {
			t.Errorf("linearToMuLaw(%d) = %#02x, want %#02x", tc.sample, got, tc.want)
		}
	}
}

// TestLinearToMuLawMonotonic: the magnitude code grows monotonically with
// the magnitude (the companding's defining property).
func TestLinearToMuLawMonotonic(t *testing.T) {
	prev := linearToMuLaw(0)
	for s := int32(1); s <= muLawClip; s++ {
		got := linearToMuLaw(int16(s))
		if got > prev {
			t.Fatalf("linearToMuLaw is not monotonic at %d: %#02x after %#02x", s, got, prev)
		}
		prev = got
	}
}

// TestSynthesizeSong pins the song's shape: sixteen seconds at 8 kHz (32
// beats at 120 BPM), frame-aligned, non-silent, and normalized below
// clipping.
func TestSynthesizeSong(t *testing.T) {
	pcm := synthesize(songTempo, chiptuneMelody, chiptuneBass)
	wantSamples := 32 * 60 / songTempo * sampleRate // 128000
	if len(pcm) != wantSamples {
		t.Fatalf("the song renders %d samples, want %d (16 s at 8 kHz)", len(pcm), wantSamples)
	}
	if len(pcm)%samplesPerFrame != 0 {
		t.Fatalf("the song's %d samples are not a whole number of %d-sample frames", len(pcm), samplesPerFrame)
	}
	peak := 0
	for _, s := range pcm {
		if v := int(s); v < 0 {
			v = -v
			if v > peak {
				peak = v
			}
		} else if v > peak {
			peak = v
		}
	}
	if peak == 0 {
		t.Fatal("the song is silent")
	}
	if peak > 23000 {
		t.Fatalf("the song clips: peak %d, want at most ~0.7 of full scale (22937)", peak)
	}
}

// TestSongDeterministic: the generated song renders byte-identically
// every time (a programmatically generated song is a pure function).
func TestSongDeterministic(t *testing.T) {
	again := newSong("chiptune", "", songTempo, chiptuneMelody, chiptuneBass)
	if !bytes.Equal(again.ulaw, songChiptune.ulaw) {
		t.Fatal("the song rendered differently twice")
	}
	if len(songChiptune.ulaw)%samplesPerFrame != 0 {
		t.Fatalf("the encoded song's %d bytes are not a whole number of frames", len(songChiptune.ulaw))
	}
}

// TestSynthesizePanicsOnMismatchedVoices: voices that disagree on the
// loop length are a composition error, caught at render.
func TestSynthesizePanicsOnMismatchedVoices(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("synthesize did not panic on mismatched voices")
		}
	}()
	synthesize(songTempo, []note{{69, 4}}, []note{{48, 3}})
}

// TestPlayerFrames covers the pump's frame arithmetic: frames advance one
// frame at a time and wrap around the loop; setSong restarts.
func TestPlayerFrames(t *testing.T) {
	two := &song{name: "two", ulaw: bytes.Repeat([]byte{7}, 2*samplesPerFrame)}
	p, err := newPlayer(two)
	if err != nil {
		t.Fatalf("newPlayer: %v", err)
	}
	if f := p.frame(); len(f) != samplesPerFrame || p.offset != samplesPerFrame {
		t.Fatalf("the first frame advanced the offset to %d, want %d", p.offset, samplesPerFrame)
	}
	if f := p.frame(); len(f) != samplesPerFrame || p.offset != 0 {
		t.Fatalf("the second frame did not wrap: offset %d", p.offset)
	}
	other := &song{name: "other", ulaw: bytes.Repeat([]byte{9}, samplesPerFrame)}
	p.setSong(other)
	if p.offset != 0 || p.song != other {
		t.Fatalf("setSong did not restart: offset %d, song %q", p.offset, p.song.name)
	}
}

// TestPlayerRunStops covers the pump's lifecycle: it writes frames while
// running and stops on stop() (an unbound track swallows the writes, so
// the pump is observable here only through its liveness, not its output).
func TestPlayerRunStops(t *testing.T) {
	p, err := newPlayer(songChiptune)
	if err != nil {
		t.Fatalf("newPlayer: %v", err)
	}
	done := make(chan struct{})
	go func() {
		p.run(context.Background(), testLogger(t), "peer")
		close(done)
	}()
	p.stop()
	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("the pump did not stop")
	}
	// stop is idempotent.
	p.stop()
}
