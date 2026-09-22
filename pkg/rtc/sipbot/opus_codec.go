//go:build cgo

package sipbot

// This file is the relay's opus codec: libopus through
// github.com/hraban/opus, a cgo binding — which is why it lives behind
// the cgo build tag, the same discipline as the music bot's encoder
// (which this package's encoder mirrors). A build without cgo compiles
// the stub instead: opus↔opus calls still relay (passthrough needs no
// codec), and a call whose sip-leg negotiated G.711 fails when the
// relay tries to arm its transcoder, with the stub's error saying why.
//
// The relay needs both directions: the decoder turns the browser's mic
// packets into PCM (toward a G.711 callee), the encoder turns a G.711
// callee's PCM into opus packets (toward the browser). Voice shapes
// both: 48 kHz, mono, VoIP-tuned.

import (
	"fmt"

	"github.com/hraban/opus"
)

// The voice encoder's shapes: mono (a mono packet on the
// stereo-negotiated webrtc leg is valid RFC 7587 and plays as dual
// mono), 32 kbit/s — transparent for speech. The decoder is
// stereo-capable: a browser's mic is mono, but the packets' TOC says
// per frame.
const (
	opusChannels = 1
	opusBitrate  = 32000
)

// opusEncoder is one transcoder's opus encoder: one 20 ms mono frame
// (960 samples) per encode.
type opusEncoder struct {
	enc *opus.Encoder
	out []byte
}

func newOpusEncoder() (*opusEncoder, error) {
	enc, err := opus.NewEncoder(opusSampleRate, opusChannels, opus.AppVoIP)
	if err != nil {
		return nil, fmt.Errorf("sipbot: create the opus encoder: %w", err)
	}
	if err := enc.SetBitrate(opusBitrate); err != nil {
		return nil, fmt.Errorf("sipbot: set the opus bitrate: %w", err)
	}
	return &opusEncoder{enc: enc, out: make([]byte, opusMaxPacket)}, nil
}

// encode encodes one frame of mono signed 16-bit samples, returning the
// packet. The packet's bytes are valid until the next encode — the
// relay's pump hands them to the track immediately (pion copies them
// into the RTP packet).
func (e *opusEncoder) encode(pcm []int16) ([]byte, error) {
	n, err := e.enc.Encode(pcm, e.out)
	if err != nil {
		return nil, err
	}
	return e.out[:n], nil
}

// opusDecoder is one transcoder's opus decoder: one packet per decode,
// producing interleaved stereo samples (the downmix to mono is the
// transcoder's, keeping the decoder agnostic of the packet's channels).
type opusDecoder struct {
	dec *opus.Decoder
	pcm []int16
}

func newOpusDecoder() (*opusDecoder, error) {
	dec, err := opus.NewDecoder(opusSampleRate, 2)
	if err != nil {
		return nil, fmt.Errorf("sipbot: create the opus decoder: %w", err)
	}
	return &opusDecoder{dec: dec, pcm: make([]int16, opusMaxPCM)}, nil
}

// decode decodes one packet into interleaved stereo samples, valid until
// the next decode.
func (d *opusDecoder) decode(pkt []byte) ([]int16, error) {
	n, err := d.dec.Decode(pkt, d.pcm)
	if err != nil {
		return nil, err
	}
	return d.pcm[:n*2], nil
}
