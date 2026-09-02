//go:build !cgo

package musicbot

// This file is the opus encoder's stub for builds without cgo: the
// linear PCM path needs libopus (see opus_encoder.go), which a pure Go
// build cannot link. A μ-law source plays unaffected; a linear PCM
// source fails when a call tries to prepare it, with the error below
// saying why.

import "errors"

// errNoOpus explains the limitation.
var errNoOpus = errors.New("musicbot: playing a linear pcm source needs the opus encoder — build with cgo and libopus available (CGO_ENABLED=1); mu-law sources play without it")

// musicEncoder mirrors the cgo build's encoder, always failing.
type musicEncoder struct{}

func newMusicEncoder(sampleRate, channels int) (*musicEncoder, error) {
	return nil, errNoOpus
}

func (e *musicEncoder) encode(pcm []int16, buf []byte) (int, error) {
	return 0, errNoOpus
}

func (e *musicEncoder) reset() {}
