//go:build !cgo

package sipbot

// This file is the opus codec's stub for builds without cgo: the
// transcoding paths need libopus (see opus_codec.go), which a pure Go
// build cannot link. An opus↔opus call relays unaffected (passthrough
// needs no codec); a call whose sip-leg negotiated G.711 fails when the
// relay arms its transcoder, with the error below saying why — the
// music bot's stub discipline, extended to a decoder.

import "errors"

// errNoOpus explains the limitation.
var errNoOpus = errors.New("sipbot: transcoding to or from G.711 needs the opus codec — build with cgo and libopus available (CGO_ENABLED=1); an opus-negotiated call relays without it")

// opusEncoder mirrors the cgo build's encoder, always failing.
type opusEncoder struct{}

func newOpusEncoder() (*opusEncoder, error)               { return nil, errNoOpus }
func (e *opusEncoder) encode(pcm []int16) ([]byte, error) { return nil, errNoOpus }

// opusDecoder mirrors the cgo build's decoder, always failing.
type opusDecoder struct{}

func newOpusDecoder() (*opusDecoder, error)               { return nil, errNoOpus }
func (d *opusDecoder) decode(pkt []byte) ([]int16, error) { return nil, errNoOpus }
