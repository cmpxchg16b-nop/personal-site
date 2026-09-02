//go:build cgo

package musicbot

// This file is the opus encoder of the linear PCM path: libopus through
// github.com/hraban/opus, a cgo binding — which is why it lives behind
// the cgo build tag. A build without cgo (the cross-compiled binaries
// and the container image) compiles the stub instead: its music bot
// plays μ-law sources, and a linear PCM source reports the limitation
// when a call tries to prepare it.

import (
	"fmt"

	"github.com/hraban/opus"
)

// opusBitrate is the music's encoding bitrate: stereo music over the
// phone line, 96 kbit/s — transparent enough for a chiptune and kind to
// the wire.
const opusBitrate = 96000

// musicEncoder is one track's opus encoder.
type musicEncoder struct {
	enc *opus.Encoder
}

// newMusicEncoder creates an encoder for the given stream shape. The
// player feeds it 20 ms frames (opusSamplesPerFrame samples per
// channel), the frame duration every WebRTC stack handles best.
func newMusicEncoder(sampleRate, channels int) (*musicEncoder, error) {
	enc, err := opus.NewEncoder(sampleRate, channels, opus.AppAudio)
	if err != nil {
		return nil, fmt.Errorf("musicbot: create the opus encoder: %w", err)
	}
	if err := enc.SetBitrate(opusBitrate); err != nil {
		return nil, fmt.Errorf("musicbot: set the opus bitrate: %w", err)
	}
	return &musicEncoder{enc: enc}, nil
}

// encode encodes one frame of interleaved signed 16-bit samples into
// buf, returning the packet's length.
func (e *musicEncoder) encode(pcm []int16, buf []byte) (int, error) {
	return e.enc.Encode(pcm, buf)
}

// reset clears the encoder's inter-frame state: a mid-call source
// switch starts a new piece of music, not a continuation of the old
// one's prediction.
func (e *musicEncoder) reset() {
	_ = e.enc.Reset()
}
