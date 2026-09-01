package musicbot

// This file is the bot's music: a fixed composition synthesized
// programmatically — no assets, no files. The one song is an eight-bar
// pentatonic loop ("chiptune"): a melody voice and a bass voice rendered
// to 8 kHz mono PCM with an additive sine synth (three partials under a
// pluck envelope), mixed, normalized, and μ-law encoded once, so the
// players only ever copy bytes. The loop's boundaries are click-free:
// every note fades to silence, so the wrap from the last sample back to
// the first is inaudible. The song plays indefinitely — the players loop
// it for as long as the call lasts.

import (
	"math"
	"time"
)

// The render format: 8 kHz mono — PCMU's native rate, so the μ-law bytes
// are one-to-one the RTP payload.
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

// song is one rendered song: the metadata the CLI lists, and the whole
// loop μ-law encoded — len(ulaw) is a multiple of samplesPerFrame, so the
// players copy whole frames and wrap with a plain modulo.
type song struct {
	name  string
	blurb string
	ulaw  []byte
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
// composition error caught at init.
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
			panic("musicbot: song voices disagree on the loop length")
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

// newSong renders and encodes one song. The PCM is padded with silence to
// a whole number of frames, so the players' frame arithmetic is exact.
func newSong(name, blurb string, bpm float64, voices ...[]note) *song {
	pcm := synthesize(bpm, voices...)
	if rem := len(pcm) % samplesPerFrame; rem != 0 {
		pcm = append(pcm, make([]int16, samplesPerFrame-rem)...)
	}
	return &song{name: name, blurb: blurb, ulaw: muLawEncode(pcm)}
}

// The songbook: the one initial song, programmatically generated.
var songChiptune = newSong(
	"chiptune",
	"a programmatically generated chiptune loop, playing indefinitely",
	songTempo,
	chiptuneMelody,
	chiptuneBass,
)
