// Package audiosource is the unified model of audio source data: what a
// playable piece of audio IS, independent of who plays it. A source is
// an AudioSourceData — metadata describing the samples (the sample
// format, the numeric encoding, the channel layout, the sample rate)
// plus the data's location: inline bytes, or a URL (an http(s) URL or a
// filesystem path).
//
// Sample data is loaded and decoded LAZILY, and never as a whole: a
// source's samples are a stream, accessed through Open — each call
// starts a fresh Stream positioned at the first sample, reading the
// data on demand as the consumer reads. A source whose NumTotalSamples
// is 0 is a streaming source: its length is unknown (or unbounded), and
// no end-of-data checks apply.
//
// Not every format combination is supported; the accepted ones are:
//
//   - linear PCM, 48000 Hz, 2 channels,
//   - G.711 μ-law, 8000 Hz, 1 channel,
//   - Opus, 48000 Hz, 1 or 2 channels, Ogg-framed.
//
// Raw samples of an uncompressed source are little-endian, interleaved
// channel by channel when the source has several channels. When a
// source is compressed, the stream info encoded in the file header
// takes precedence over the declared metadata: an opened stream's
// Format reports what the stream actually holds (see Stream). An Opus
// source holds no samples to describe — its data is already codec
// packets — so an open read of one serves whole packets instead of
// sample bytes (see PacketStream); a consumer plays them by passing
// them through, not by decoding and re-encoding them.
package audiosource

import (
	"fmt"
	"regexp"
)

// The sample format types: the shape of one decoded sample.
const (
	// SampleLinearPCM: samples are linear pulse-code modulation.
	SampleLinearPCM = "linear_pcm"
	// SampleMuLaw: samples are G.711 μ-law companded bytes (one byte
	// per sample — the WebRTC PCMU payload).
	SampleMuLaw = "mu_law"
	// SampleOpus: the data is Opus packets — already-encoded audio. An
	// opus source needs the Ogg framing (see CompressionOgg), and an
	// open read of one serves whole packets (see PacketStream): the
	// codec's own clock is 48000 Hz, whatever rate the encoder was fed.
	SampleOpus = "opus"
)

// The numeric types of a linear PCM sample's encoding.
const (
	// NumUnsignedInt: samples are unsigned integers (zero biased to the
	// format's midpoint).
	NumUnsignedInt = "unsigned_integer"
	// NumSignedInt: samples are two's-complement signed integers.
	NumSignedInt = "signed_integer"
	// NumFloat: samples are IEEE 754 floats in [-1, 1] (32-bit).
	NumFloat = "float"
)

// The compressions a source's data can be stored under.
const (
	// CompressionNone: the data is raw samples in the declared format.
	CompressionNone = "none"
	// CompressionFLAC: the data is a FLAC stream; the decoded samples
	// are linear PCM, and the stream info in the file header takes
	// precedence over the declared metadata (see Stream's Format).
	CompressionFLAC = "flac"
	// CompressionOgg: the data is an Ogg-framed stream of Opus packets
	// (RFC 7845). The ID header's stream description — the channel
	// count and the input sample rate the encoder declares — takes
	// precedence over the declared metadata, and the stream it frames
	// is served as whole packets (see PacketStream).
	CompressionOgg = "ogg"
)

// dnsLabel is the Name's shape: a DNS-label-like string — lowercase
// letters, digits and hyphens, starting and ending alphanumeric, at
// most 63 characters.
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// AudioSourceData is one audio source: the metadata describing its
// samples plus the data's location. Sources are configured values (the
// global configuration document's audioSource entries mirror them);
// their sample data is accessed lazily through Open, and a source is
// shared between consumers by pointer.
type AudioSourceData struct {
	// Id is the source's globally unique identifier.
	Id string
	// Name is the source's descriptive name, a DNS-label-like string —
	// the music bot's CLI addresses songs by it.
	Name string
	// Description is the source's description.
	Description string
	// Author is the source's author.
	Author string
	// SampleFormatType is the sample format: SampleLinearPCM,
	// SampleMuLaw or SampleOpus. For a compressed source it describes
	// the decoded samples — FLAC always decodes to linear PCM.
	SampleFormatType string
	// BitDepth is one sample's size in bits: 8, 16 or 32. G.711 μ-law
	// samples are 8-bit companded bytes. The field describes PCM
	// samples; it does not apply to an opus source and is not checked
	// for one.
	BitDepth int
	// NumericType is a linear PCM sample's numeric encoding:
	// NumUnsignedInt, NumSignedInt or NumFloat (32-bit). Like BitDepth
	// it does not apply to an opus source and is not checked for one.
	NumericType string
	// NumChannels is the number of channels.
	NumChannels int
	// Interleaved says the samples of a multi-channel source are
	// interleaved (frame by frame, channel by channel). Only
	// interleaved sources are supported, so it must be true. The field
	// describes PCM samples; it does not apply to an opus source (the
	// codec carries its channels inside its packets) and is not checked
	// for one.
	Interleaved bool
	// InlineData carries the source's data inline. When present, it
	// takes precedence over URL.
	InlineData []byte
	// URL is where the data lives when InlineData is absent: an
	// http(s) URL, or a filesystem path (absolute, or relative to the
	// consumer's configuration — the server wiring resolves relative
	// paths against the configuration document's directory). When
	// InlineData is present, InlineData takes precedence.
	URL string
	// Compression is the data's storage compression: CompressionNone,
	// CompressionFLAC or CompressionOgg.
	Compression string
	// SampleRate is the sample rate in Hz. For an opus source it is the
	// codec's own rate — 48000, the rate the Ogg granule positions
	// count at — and the ID header's declared input rate must match it.
	SampleRate int
	// NumTotalSamples is the total number of samples, using the
	// inter-channel definition: NOT multiplied by the number of
	// channels — a 2-channel, 1-minute, 48000 Hz source has 48000
	// samples, not 96000. A value of 0 denotes a streaming source: the
	// length is unknown (or unbounded). For an opus source the count is
	// in the codec's samples (at 48000 Hz), what the Ogg granule
	// positions measure.
	NumTotalSamples int
}

// Validate validates the source: its identity, its declared format, and
// that the format is one of the supported combinations. The sample data
// itself is not touched — validation is a wiring-time concern, loading
// is lazy.
func (s *AudioSourceData) Validate() error {
	if s.Id == "" {
		return fmt.Errorf("audiosource: empty id")
	}
	if !dnsLabel.MatchString(s.Name) {
		return fmt.Errorf("audiosource: %s: name %q is not a dns-label-like string", s.Id, s.Name)
	}
	switch s.SampleFormatType {
	case SampleLinearPCM, SampleMuLaw, SampleOpus:
	default:
		return fmt.Errorf("audiosource: %s: unknown sample format type %q", s.Id, s.SampleFormatType)
	}
	switch s.Compression {
	case CompressionNone, CompressionFLAC, CompressionOgg:
	default:
		return fmt.Errorf("audiosource: %s: unknown compression %q", s.Id, s.Compression)
	}
	// The numeric shape describes PCM samples; it does not apply to an
	// opus source (see the field docs) and is not checked for one.
	if s.SampleFormatType != SampleOpus {
		switch s.NumericType {
		case NumUnsignedInt, NumSignedInt, NumFloat:
		default:
			return fmt.Errorf("audiosource: %s: unknown numeric type %q", s.Id, s.NumericType)
		}
		switch s.BitDepth {
		case 8, 16, 32:
		default:
			return fmt.Errorf("audiosource: %s: bit depth %d is not one of 8, 16, 32", s.Id, s.BitDepth)
		}
		if s.NumericType == NumFloat && s.BitDepth != 32 {
			return fmt.Errorf("audiosource: %s: float samples require a bit depth of 32", s.Id)
		}
		if !s.Interleaved {
			return fmt.Errorf("audiosource: %s: only interleaved samples are supported", s.Id)
		}
	}
	if s.NumChannels < 1 {
		return fmt.Errorf("audiosource: %s: channel count %d is not positive", s.Id, s.NumChannels)
	}
	if s.SampleRate < 1 {
		return fmt.Errorf("audiosource: %s: sample rate %d is not positive", s.Id, s.SampleRate)
	}
	if s.NumTotalSamples < 0 {
		return fmt.Errorf("audiosource: %s: sample count %d is negative", s.Id, s.NumTotalSamples)
	}
	if len(s.InlineData) == 0 && s.URL == "" {
		return fmt.Errorf("audiosource: %s: neither inline data nor a url", s.Id)
	}
	if s.Compression == CompressionFLAC && s.SampleFormatType != SampleLinearPCM {
		return fmt.Errorf("audiosource: %s: a flac source decodes to linear pcm", s.Id)
	}
	if s.Compression == CompressionOgg && s.SampleFormatType != SampleOpus {
		return fmt.Errorf("audiosource: %s: an ogg-framed source holds opus packets", s.Id)
	}
	if s.SampleFormatType == SampleOpus && s.Compression != CompressionOgg {
		return fmt.Errorf("audiosource: %s: an opus source needs the ogg framing", s.Id)
	}
	if s.SampleFormatType == SampleMuLaw && s.BitDepth != 8 {
		return fmt.Errorf("audiosource: %s: mu-law samples are 8-bit", s.Id)
	}
	// The accepted format combinations.
	switch {
	case s.SampleFormatType == SampleLinearPCM && s.SampleRate == 48000 && s.NumChannels == 2:
	case s.SampleFormatType == SampleMuLaw && s.SampleRate == 8000 && s.NumChannels == 1:
	case s.SampleFormatType == SampleOpus && s.SampleRate == 48000 && (s.NumChannels == 1 || s.NumChannels == 2):
	default:
		return fmt.Errorf("audiosource: %s: unsupported combination %s/%dHz/%dch",
			s.Id, s.SampleFormatType, s.SampleRate, s.NumChannels)
	}
	return nil
}
