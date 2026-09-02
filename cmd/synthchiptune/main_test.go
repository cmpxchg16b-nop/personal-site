package main

// The synthesizer's unit tests: the song's shape and determinism, and
// the composition error the renderer catches.

import (
	"bytes"
	"testing"
)

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

// TestRenderDeterministic: the generated song renders byte-identically
// every time (a programmatically generated song is a pure function).
func TestRenderDeterministic(t *testing.T) {
	if !bytes.Equal(render(), render()) {
		t.Fatal("the song rendered differently twice")
	}
	if ulaw := render(); len(ulaw)%samplesPerFrame != 0 {
		t.Fatalf("the encoded song's %d bytes are not a whole number of frames", len(ulaw))
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
