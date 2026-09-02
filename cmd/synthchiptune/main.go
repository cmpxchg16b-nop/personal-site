// Command synthchiptune is the one-off synthesizer of the chiptune
// song the music bot ships with: an eight-bar pentatonic loop — a
// melody voice and a bass voice — rendered to 8 kHz mono PCM with an
// additive sine synth (three partials under a pluck envelope), mixed,
// normalized, and G.711 μ-law encoded once. The composition once lived
// in pkg/rtc/musicbot's source (song.go); the audio source model made
// songs data instead, so this command renders it to the file the
// configuration document's audioSource entry points at:
//
//	go run ./cmd/synthchiptune [-o assets/chiptune.ulaw]
//
// The output is the raw μ-law byte stream (one byte per sample,
// 16000 Hz of nothing — 8000 samples per second), padded with silence
// to a whole number of 20 ms frames so looping players wrap cleanly;
// the file is byte-deterministic, so regenerating it is idempotent.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"math"
	"math/bits"
	"os"
	"time"
)

// The render format: 8 kHz mono — PCMU's native rate, so the μ-law
// bytes are one-to-one the RTP payload.
const (
	sampleRate      = 8000
	songTempo       = 120 // beats per minute
	frameDuration   = 20 * time.Millisecond
	samplesPerFrame = sampleRate / 50 // one frameDuration of samples: 160
)

// note is one composed note: its MIDI note number and its length in beats.
type note struct {
	midi  float64
	beats float64
}

// The chiptune's melody: eight bars of C-major pentatonic.
var chiptuneMelody = []note{
	{76, 0.5}, {79, 0.5}, {81, 1}, {79, 0.5}, {76, 0.5}, {74, 1}, // E5 G5 A5 G5 E5 D5
	{76, 0.5}, {79, 0.5}, {81, 0.5}, {83, 0.5}, {81, 1}, {79, 1}, // E5 G5 A5 B5 A5 G5
	{76, 0.5}, {74, 0.5}, {76, 0.5}, {79, 0.5}, {76, 1}, {74, 1}, // E5 D5 E5 G5 E5 D5
	{72, 0.5}, {74, 0.5}, {76, 0.5}, {74, 0.5}, {72, 2}, // C5 D5 E5 D5 C5
	{76, 0.5}, {79, 0.5}, {81, 1}, {84, 0.5}, {83, 0.5}, {81, 1}, // E5 G5 A5 C6 B5 A5
	{79, 0.5}, {76, 0.5}, {79, 0.5}, {81, 0.5}, {79, 2}, // G5 E5 G5 A5 G5
	{76, 0.5}, {74, 0.5}, {76, 0.5}, {79, 0.5}, {81, 1}, {83, 1}, // E5 D5 E5 G5 A5 B5
	{81, 0.5}, {79, 0.5}, {76, 0.5}, {74, 0.5}, {72, 2}, // A5 G5 E5 D5 C5
}

// The chiptune's bass: one whole-bar note per bar.
var chiptuneBass = []note{
	{48, 4}, {45, 4}, {52, 4}, {43, 4}, // C3 A2 E3 G2
	{45, 4}, {52, 4}, {50, 4}, {48, 4}, // A2 E3 D3 C3
}

// midiFreq maps a MIDI note number to its frequency in Hz (equal
// temperament, A4 = 440).
func midiFreq(midi float64) float64 {
	return 440 * math.Pow(2, (midi-69)/12)
}

// envelope shapes one note: a short linear attack against the onset click,
// an exponential decay (the pluck), and a fade to silence over the note's
// last tenth so note — and loop — boundaries are click-free.
func envelope(t, dur float64) float64 {
	const attack = 0.01 // seconds
	a := math.Min(1, t/attack)
	decay := math.Exp(-3 * t / dur)
	fade := math.Max(0, math.Min(1, (dur-t)/(0.1*dur)))
	return a * decay * fade
}

// synthesize renders the voices in parallel (all start at beat zero; they
// must agree on the total length) and mixes them into one normalized PCM
// buffer. It panics when the voices disagree on the loop's length — a
// composition error caught at render.
func synthesize(bpm float64, voices ...[]note) []int16 {
	var totalBeats float64
	for i, voice := range voices {
		var beats float64
		for _, n := range voice {
			beats += n.beats
		}
		if i == 0 {
			totalBeats = beats
		} else if beats != totalBeats {
			panic("synthchiptune: song voices disagree on the loop length")
		}
	}
	totalSamples := int(math.Round(totalBeats * 60 / bpm * sampleRate))
	mix := make([]float64, totalSamples)
	for _, voice := range voices {
		pos := 0
		for _, n := range voice {
			dur := n.beats * 60 / bpm
			nSamples := int(math.Round(dur * sampleRate))
			f := midiFreq(n.midi)
			for i := 0; i < nSamples && pos+i < totalSamples; i++ {
				t := float64(i) / sampleRate
				// Three additive partials: the fundamental, plus the
				// octave and the twelfth above it for the chiptune bite.
				mix[pos+i] += envelope(t, dur) * (math.Sin(2*math.Pi*f*t) +
					0.3*math.Sin(2*math.Pi*2*f*t) +
					0.15*math.Sin(2*math.Pi*3*f*t))
			}
			pos += nSamples
		}
	}
	peak := 0.0
	for _, s := range mix {
		peak = math.Max(peak, math.Abs(s))
	}
	pcm := make([]int16, totalSamples)
	if peak > 0 {
		scale := 0.7 * 32767 / peak
		for i, s := range mix {
			pcm[i] = int16(s * scale)
		}
	}
	return pcm
}

// G.711 μ-law constants of the canonical ITU-T/Sun reference
// implementation: the segment bias and the clip threshold.
const (
	muLawBias = 0x84 // 132
	muLawClip = 32635
)

// linearToMuLaw encodes one 16-bit linear PCM sample as its μ-law byte:
// sign, the segment (exponent) of the biased magnitude, and the mantissa,
// complemented. (0 encodes to 0xFF, the μ-law positive zero.)
func linearToMuLaw(sample int16) byte {
	sign := 0
	s := int32(sample)
	if s < 0 {
		sign = 0x80
		s = -s
	}
	if s > muLawClip {
		s = muLawClip
	}
	s += muLawBias
	// After the bias the magnitude is at most 0x7FFF, so s>>7 fits a byte
	// and its highest set bit is the segment.
	exponent := 7 - bits.LeadingZeros8(uint8(s>>7))
	mantissa := (s >> (exponent + 3)) & 0x0F
	return byte(^(sign | (exponent << 4) | int(mantissa)))
}

// muLawEncode encodes a whole PCM buffer.
func muLawEncode(pcm []int16) []byte {
	out := make([]byte, len(pcm))
	for i, sample := range pcm {
		out[i] = linearToMuLaw(sample)
	}
	return out
}

// render renders the chiptune loop and μ-law encodes it. The PCM is
// padded with silence to a whole number of frames, so players' frame
// arithmetic is exact.
func render() []byte {
	pcm := synthesize(songTempo, chiptuneMelody, chiptuneBass)
	if rem := len(pcm) % samplesPerFrame; rem != 0 {
		pcm = append(pcm, make([]int16, samplesPerFrame-rem)...)
	}
	return muLawEncode(pcm)
}

func main() {
	out := flag.String("o", "assets/chiptune.ulaw", "the output file to write the μ-law song to")
	flag.Parse()

	ulaw := render()
	if err := os.WriteFile(*out, ulaw, 0o644); err != nil {
		log.Fatalf("write %s: %v", *out, err)
	}
	sum := sha256.Sum256(ulaw)
	fmt.Printf("wrote %s: %d bytes (%d samples, %s at %d Hz), sha256 %s\n",
		*out, len(ulaw), len(ulaw), time.Duration(len(ulaw)/sampleRate)*time.Second,
		sampleRate, hex.EncodeToString(sum[:]))
}
