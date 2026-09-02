package audiosource

// This file is the source's sample data as a stream: lazily loaded
// (nothing is touched until Open), never held as a whole (a consumer
// reads on demand), and rewindable (the project's players loop their
// music indefinitely — a filesystem or inline source rewinds by seeking
// back to the first byte, an http source by re-fetching its URL).
//
// Two layers compose into a Stream: a rawReader over the source's
// (possibly compressed) bytes — inline, filesystem, or http — and the
// decode layer above it: the passthrough of an uncompressed source, or
// the on-demand FLAC decoding of a compressed one. When the source's
// total sample count is known, both layers check it at end-of-data: a
// stream that ends early (or late) is an error, not a short song.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/mewkiz/flac"
	"github.com/mewkiz/flac/frame"
)

// Format describes an opened stream's sample format: for an
// uncompressed source it is the declared metadata; for a compressed
// source it is the stream info encoded in the file header, which takes
// precedence over the declared metadata.
type Format struct {
	SampleFormatType string
	BitDepth         int
	NumericType      string
	NumChannels      int
	SampleRate       int
	// NumTotalSamples is the stream's total sample count, using the
	// inter-channel definition (see AudioSourceData); 0 denotes a
	// streaming source of unknown length.
	NumTotalSamples int
}

// Stream is one open read of a source's decoded sample data, positioned
// at the first sample. Reading decodes on demand; the samples are
// little-endian, interleaved channel by channel when the source has
// several channels. A stream is single-use but rewindable, and each
// Open call returns an independent one — sources need no
// synchronization and carry no open-stream state.
type Stream interface {
	io.Reader
	io.Closer

	// Rewind repositions the stream at the first sample, so a consumer
	// can play the source indefinitely: a filesystem or inline source
	// seeks its bytes back to the start, an http source re-fetches its
	// URL (fetching honors ctx). A source whose length is unknown
	// (NumTotalSamples 0) rewinds just the same — a streaming source
	// restarts from its beginning.
	Rewind(ctx context.Context) error

	// Format reports the stream's effective sample format — the
	// declared metadata, save that a compressed source reports the
	// stream info of its file header, which takes precedence.
	Format() Format
}

// Open starts a fresh stream of the source's decoded sample data,
// positioned at the first sample. This is the source's first touch:
// sample data is loaded lazily, and an http(s) source's fetch happens
// here (honoring ctx). For a compressed source the header's stream
// info takes precedence over the declared metadata; the effective
// format it describes must be one of the supported combinations.
func (s *AudioSourceData) Open(ctx context.Context) (Stream, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	raw := &rawReader{src: s}
	if err := raw.open(ctx); err != nil {
		return nil, err
	}
	if s.Compression == CompressionFLAC {
		fs := &flacStream{raw: raw}
		if err := fs.parse(); err != nil {
			raw.Close()
			return nil, err
		}
		return fs, nil
	}
	return &pcmStream{raw: raw, format: s.declaredFormat()}, nil
}

// declaredFormat is the format the metadata declares. It is what an
// uncompressed stream holds (Validate has checked it).
func (s *AudioSourceData) declaredFormat() Format {
	return Format{
		SampleFormatType: s.SampleFormatType,
		BitDepth:         s.BitDepth,
		NumericType:      s.NumericType,
		NumChannels:      s.NumChannels,
		SampleRate:       s.SampleRate,
		NumTotalSamples:  s.NumTotalSamples,
	}
}

// validateFormat checks that a format (declared or effective) is one of
// the supported combinations.
func validateFormat(f Format) error {
	switch {
	case f.SampleFormatType == SampleLinearPCM && f.SampleRate == 48000 && f.NumChannels == 2:
	case f.SampleFormatType == SampleMuLaw && f.SampleRate == 8000 && f.NumChannels == 1:
	default:
		return fmt.Errorf("unsupported combination %s/%dHz/%dch", f.SampleFormatType, f.SampleRate, f.NumChannels)
	}
	switch f.BitDepth {
	case 8, 16, 32:
	default:
		return fmt.Errorf("bit depth %d is not one of 8, 16, 32", f.BitDepth)
	}
	return nil
}

// expectedBytes is how many decoded bytes a stream of the format should
// produce in total: the (inter-channel) sample count times every
// channel's sample. A streaming format (sample count 0) expects nothing
// — no end-of-data check applies.
func expectedBytes(f Format) int64 {
	if f.NumTotalSamples <= 0 {
		return 0
	}
	return int64(f.NumTotalSamples) * int64(f.NumChannels) * int64(f.BitDepth/8)
}

// rawReader is the source's raw — possibly compressed — bytes as a
// reader, over whichever location the data lives at: inline bytes, a
// filesystem file, or an http(s) URL. It knows how to rewind each
// kind: inline and file bytes seek back to the start; an http source
// re-fetches.
type rawReader struct {
	src *AudioSourceData

	// body is the current read; exactly one of the three fields below
	// names what it reads.
	body   io.ReadCloser
	inline *bytes.Reader
	file   *os.File
}

// open starts the first read of the source's bytes. Inline data takes
// precedence over the URL.
func (r *rawReader) open(ctx context.Context) error {
	switch {
	case len(r.src.InlineData) > 0:
		r.inline = bytes.NewReader(r.src.InlineData)
		r.body = io.NopCloser(r.inline)
	case strings.HasPrefix(r.src.URL, "http://") || strings.HasPrefix(r.src.URL, "https://"):
		return r.fetch(ctx)
	default:
		f, err := os.Open(r.src.URL)
		if err != nil {
			return fmt.Errorf("audiosource: %s: open %s: %w", r.src.Id, r.src.URL, err)
		}
		r.file = f
		r.body = f
	}
	return nil
}

// fetch (re-)starts the http read of the source's URL.
func (r *rawReader) fetch(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.src.URL, nil)
	if err != nil {
		return fmt.Errorf("audiosource: %s: %w", r.src.Id, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("audiosource: %s: fetch %s: %w", r.src.Id, r.src.URL, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return fmt.Errorf("audiosource: %s: fetch %s: %s", r.src.Id, r.src.URL, resp.Status)
	}
	r.body = resp.Body
	return nil
}

func (r *rawReader) Read(p []byte) (int, error) { return r.body.Read(p) }

// Rewind restarts the raw read from the source's first byte: inline and
// filesystem bytes seek back; an http source re-fetches (honoring ctx).
func (r *rawReader) Rewind(ctx context.Context) error {
	switch {
	case r.inline != nil:
		_, err := r.inline.Seek(0, io.SeekStart)
		return err
	case r.file != nil:
		_, err := r.file.Seek(0, io.SeekStart)
		if err != nil {
			return fmt.Errorf("audiosource: %s: rewind %s: %w", r.src.Id, r.src.URL, err)
		}
		return nil
	default:
		if err := r.body.Close(); err != nil {
			// An http body that fails to close is unwound, not fatal:
			// the re-fetch replaces it either way.
			_ = err
		}
		return r.fetch(ctx)
	}
}

func (r *rawReader) Close() error {
	if err := r.body.Close(); err != nil {
		return fmt.Errorf("audiosource: %s: close: %w", r.src.Id, err)
	}
	return nil
}

// pcmStream is an uncompressed source's stream: the raw bytes ARE the
// decoded samples, so the stream passes them through, counting them for
// the end-of-data check when the source's length is known.
type pcmStream struct {
	raw    *rawReader
	format Format
	read   int64 // decoded bytes served since the start or the last rewind
}

func (s *pcmStream) Read(p []byte) (int, error) {
	n, err := s.raw.Read(p)
	s.read += int64(n)
	if err == io.EOF {
		if cerr := s.checkCount(); cerr != nil {
			return n, cerr
		}
	}
	return n, err
}

func (s *pcmStream) Rewind(ctx context.Context) error {
	if err := s.raw.Rewind(ctx); err != nil {
		return err
	}
	s.read = 0
	return nil
}

func (s *pcmStream) Close() error { return s.raw.Close() }

func (s *pcmStream) Format() Format { return s.format }

// checkCount verifies the end-of-data against the expected total: a
// stream that ended early (or ran past) contradicts the metadata and is
// an error. A streaming source (no expected total) checks nothing.
func (s *pcmStream) checkCount() error {
	if want := expectedBytes(s.format); want > 0 && s.read != want {
		return fmt.Errorf("audiosource: %s: the stream holds %d bytes, want %d (%d samples)",
			s.raw.src.Id, s.read, want, s.format.NumTotalSamples)
	}
	return nil
}

// flacStream is a compressed source's stream: FLAC frames decode on
// demand as the stream is read, the decoded samples interleaving into
// the little-endian linear PCM of the model. The file header's stream
// info — parsed once, before the first byte is served — takes
// precedence over the declared metadata and must be a supported
// combination.
type flacStream struct {
	raw    *rawReader
	dec    *flac.Stream
	format Format

	// buf holds the interleaved bytes of the decoded frame being
	// served; pos is the next byte's offset in it.
	buf []byte
	pos int

	read int64 // decoded bytes served since the start or the last rewind
}

// parse reads the FLAC signature and the stream info block and derives
// the stream's effective format from them.
func (s *flacStream) parse() error {
	dec, err := flac.New(s.raw)
	if err != nil {
		return fmt.Errorf("audiosource: %s: parse flac: %w", s.raw.src.Id, err)
	}
	info := dec.Info
	format := Format{
		// A FLAC stream always decodes to signed-integer linear PCM.
		SampleFormatType: SampleLinearPCM,
		BitDepth:         int(info.BitsPerSample),
		NumericType:      NumSignedInt,
		NumChannels:      int(info.NChannels),
		SampleRate:       int(info.SampleRate),
		// The header's sample count uses the inter-channel definition —
		// the model's definition — already; 0 means unknown.
		NumTotalSamples: int(info.NSamples),
	}
	if err := validateFormat(format); err != nil {
		dec.Close()
		return fmt.Errorf("audiosource: %s: the flac stream info says %w", s.raw.src.Id, err)
	}
	s.dec = dec
	s.format = format
	return nil
}

func (s *flacStream) Read(p []byte) (int, error) {
	for s.pos >= len(s.buf) {
		f, err := s.dec.ParseNext()
		if err == io.EOF {
			if cerr := s.checkCount(); cerr != nil {
				return 0, cerr
			}
			return 0, io.EOF
		}
		if err != nil {
			return 0, fmt.Errorf("audiosource: %s: decode flac: %w", s.raw.src.Id, err)
		}
		s.buf = appendInterleaved(s.buf[:0], f, s.format.BitDepth/8)
		s.pos = 0
	}
	n := copy(p, s.buf[s.pos:])
	s.pos += n
	s.read += int64(n)
	return n, nil
}

func (s *flacStream) Rewind(ctx context.Context) error {
	if err := s.raw.Rewind(ctx); err != nil {
		return err
	}
	// A fresh decode over the rewound bytes. The discarded decoder is
	// not closed — closing it would close the raw read it shares.
	dec, err := flac.New(s.raw)
	if err != nil {
		return fmt.Errorf("audiosource: %s: parse flac on rewind: %w", s.raw.src.Id, err)
	}
	s.dec = dec
	s.buf = s.buf[:0]
	s.pos = 0
	s.read = 0
	return nil
}

func (s *flacStream) Close() error {
	if err := s.dec.Close(); err != nil {
		return fmt.Errorf("audiosource: %s: close flac: %w", s.raw.src.Id, err)
	}
	return nil
}

func (s *flacStream) Format() Format { return s.format }

func (s *flacStream) checkCount() error {
	if want := expectedBytes(s.format); want > 0 && s.read != want {
		return fmt.Errorf("audiosource: %s: the flac stream decoded %d bytes, want %d (%d samples)",
			s.raw.src.Id, s.read, want, s.format.NumTotalSamples)
	}
	return nil
}

// appendInterleaved appends one decoded frame's samples to dst as
// little-endian interleaved bytes, channel by channel within each
// sample.
func appendInterleaved(dst []byte, f *frame.Frame, width int) []byte {
	n := len(f.Subframes[0].Samples)
	for i := 0; i < n; i++ {
		for _, sub := range f.Subframes {
			v := sub.Samples[i]
			for b := 0; b < width; b++ {
				dst = append(dst, byte(v>>(8*b)))
			}
		}
	}
	return dst
}
