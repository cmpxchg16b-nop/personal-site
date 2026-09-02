package audiosource

// The audiosource tests: the metadata model's validation, and the
// streaming of the sample data — inline, filesystem, http and flac —
// with lazily loading, rewinding, and the end-of-data checks. The flac
// vectors are encoded in-test with mewkiz/flac's encoder, so the
// package's decoding is proven against an independent encoder over
// round trips.

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mewkiz/flac"
	"github.com/mewkiz/flac/frame"
	"github.com/mewkiz/flac/meta"
)

// muLawSource is a minimal valid mu-law source carrying data inline.
func muLawSource(data []byte) *AudioSourceData {
	if data == nil {
		data = []byte{0x00, 0x7F, 0x80, 0xFF}
	}
	return &AudioSourceData{
		Id:               "test-source",
		Name:             "test-source",
		SampleFormatType: SampleMuLaw,
		BitDepth:         8,
		NumericType:      NumUnsignedInt,
		NumChannels:      1,
		Interleaved:      true,
		InlineData:       data,
		Compression:      CompressionNone,
		SampleRate:       8000,
		NumTotalSamples:  len(data),
	}
}

// pcmSource is a minimal valid linear PCM source carrying data inline.
func pcmSource(data []byte) *AudioSourceData {
	return &AudioSourceData{
		Id:               "test-source",
		Name:             "test-source",
		SampleFormatType: SampleLinearPCM,
		BitDepth:         16,
		NumericType:      NumSignedInt,
		NumChannels:      2,
		Interleaved:      true,
		InlineData:       data,
		Compression:      CompressionNone,
		SampleRate:       48000,
		NumTotalSamples:  len(data) / 4,
	}
}

func TestValidate(t *testing.T) {
	valid := func(mutate func(*AudioSourceData)) *AudioSourceData {
		s := pcmSource([]byte{0, 0, 0, 0})
		if mutate != nil {
			mutate(s)
		}
		return s
	}
	for _, tc := range []struct {
		name string
		src  *AudioSourceData
		want string // a fragment of the expected error; "" for valid
	}{
		{"the linear pcm combination", valid(nil), ""},
		{"the mu-law combination", muLawSource(nil), ""},
		{"empty id", valid(func(s *AudioSourceData) { s.Id = "" }), "empty id"},
		{"an uppercase name", valid(func(s *AudioSourceData) { s.Name = "Ooops" }), "dns-label"},
		{"a leading hyphen name", valid(func(s *AudioSourceData) { s.Name = "-oops" }), "dns-label"},
		{"an unknown sample format", valid(func(s *AudioSourceData) { s.SampleFormatType = "vorbis" }), "sample format"},
		{"an unknown numeric type", valid(func(s *AudioSourceData) { s.NumericType = "fixed" }), "numeric type"},
		{"an unknown compression", valid(func(s *AudioSourceData) { s.Compression = "ogg" }), "compression"},
		{"a bit depth of 24", valid(func(s *AudioSourceData) { s.BitDepth = 24 }), "bit depth"},
		{"float at 16 bits", valid(func(s *AudioSourceData) { s.NumericType = NumFloat }), "float"},
		{"no channels", valid(func(s *AudioSourceData) { s.NumChannels = 0 }), "channel count"},
		{"non-interleaved", valid(func(s *AudioSourceData) { s.Interleaved = false }), "interleaved"},
		{"no sample rate", valid(func(s *AudioSourceData) { s.SampleRate = 0 }), "sample rate"},
		{"a negative sample count", valid(func(s *AudioSourceData) { s.NumTotalSamples = -1 }), "negative"},
		{"neither inline data nor url", valid(func(s *AudioSourceData) { s.InlineData = nil }), "neither"},
		{"a mu-law source with flac compression", func() *AudioSourceData {
			s := valid(nil)
			s.SampleFormatType = SampleMuLaw
			s.SampleRate = 8000
			s.NumChannels = 1
			s.BitDepth = 8
			s.Compression = CompressionFLAC
			return s
		}(), "flac source decodes"},
		{"a mu-law source at 16 bits", func() *AudioSourceData {
			s := muLawSource(nil)
			s.BitDepth = 16
			return s
		}(), "8-bit"},
		{"a sample rate the combination excludes", valid(func(s *AudioSourceData) { s.SampleRate = 44100 }), "unsupported combination"},
		{"a channel count the combination excludes", valid(func(s *AudioSourceData) { s.NumChannels = 1 }), "unsupported combination"},
		{"a streaming source (sample count 0)", valid(func(s *AudioSourceData) { s.NumTotalSamples = 0 }), ""},
	} {
		err := tc.src.Validate()
		if tc.want == "" && err != nil {
			t.Errorf("%s: Validate: unexpected error %v", tc.name, err)
		}
		if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: Validate error = %v, want one mentioning %q", tc.name, err, tc.want)
		}
	}
}

func TestOpenInline(t *testing.T) {
	data := bytes.Repeat([]byte{0x00, 0x7F, 0x80, 0xFF}, 40)
	st, err := muLawSource(data).Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	got, err := io.ReadAll(st)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("the stream served %d bytes, want the inline data verbatim", len(got))
	}
	f := st.Format()
	if f.SampleFormatType != SampleMuLaw || f.SampleRate != 8000 || f.NumChannels != 1 || f.NumTotalSamples != len(data) {
		t.Fatalf("the format = %+v, want the declared one", f)
	}
}

func TestOpenRewindsInlineBySeeking(t *testing.T) {
	data := bytes.Repeat([]byte{0x2A}, 300)
	st, err := muLawSource(data).Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	first := make([]byte, 100)
	if _, err := io.ReadFull(st, first); err != nil {
		t.Fatalf("first read: %v", err)
	}
	if err := st.Rewind(context.Background()); err != nil {
		t.Fatalf("Rewind: %v", err)
	}
	second := make([]byte, 100)
	if _, err := io.ReadFull(st, second); err != nil {
		t.Fatalf("second read: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("a rewound inline stream does not restart from the first sample")
	}
}

func TestOpenFileRewindsBySeeking(t *testing.T) {
	data := bytes.Repeat([]byte{0x11, 0x22, 0x33}, 100)
	path := filepath.Join(t.TempDir(), "song.ulaw")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write the source file: %v", err)
	}
	src := muLawSource(data)
	src.InlineData = nil
	src.URL = path
	st, err := src.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if got, err := io.ReadAll(st); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("the first pass = (%d bytes, %v), want the file verbatim", len(got), err)
	}
	// EOF reached: rewinding seeks the open file back to its first byte
	// (no close, no re-open) and the stream plays on.
	if err := st.Rewind(context.Background()); err != nil {
		t.Fatalf("Rewind: %v", err)
	}
	if got, err := io.ReadAll(st); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("the second pass = (%d bytes, %v), want the file verbatim again", len(got), err)
	}
}

func TestOpenHTTPRefetchesOnRewind(t *testing.T) {
	data := bytes.Repeat([]byte{0x55}, 256)
	fetches := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches++
		w.Write(data)
	}))
	defer srv.Close()

	src := muLawSource(data)
	src.InlineData = nil
	src.URL = srv.URL
	st, err := src.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	if fetches != 1 {
		t.Fatalf("Open fetched %d times, want 1", fetches)
	}

	if got, err := io.ReadAll(st); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("the first pass = (%d bytes, %v)", len(got), err)
	}
	if err := st.Rewind(context.Background()); err != nil {
		t.Fatalf("Rewind: %v", err)
	}
	if fetches != 2 {
		t.Fatalf("Rewind re-fetched %d times in total, want 2", fetches)
	}
	if got, err := io.ReadAll(st); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("the second pass = (%d bytes, %v)", len(got), err)
	}
}

func TestInlineDataTakesPrecedenceOverURL(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.Write(bytes.Repeat([]byte{0x99}, 8))
	}))
	defer srv.Close()

	// Both locations carry data; the inline one must win.
	src := muLawSource([]byte{0x01, 0x02, 0x03, 0x04})
	src.URL = srv.URL
	st, err := src.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	got, err := io.ReadAll(st)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if hit {
		t.Error("the url was fetched although inline data is present")
	}
	if !bytes.Equal(got, []byte{0x01, 0x02, 0x03, 0x04}) {
		t.Fatalf("the stream served %v, want the inline bytes", got)
	}
}

func TestOpenIndependentStreams(t *testing.T) {
	src := pcmSource(stereoPCMSamples(100))
	a, err := src.Open(context.Background())
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	defer a.Close()
	b, err := src.Open(context.Background())
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer b.Close()

	// Reading one stream to its end leaves the other untouched at the
	// first sample — each Open is an independent read.
	if _, err := io.ReadAll(a); err != nil {
		t.Fatalf("drain the first stream: %v", err)
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(b, head); err != nil {
		t.Fatalf("read the second stream's head: %v", err)
	}
	if want := stereoPCMSamples(100)[:4]; !bytes.Equal(head, want) {
		t.Fatalf("the second stream's head = %v, want %v", head, want)
	}
}

func TestEndOfDataCountCheck(t *testing.T) {
	// The metadata declares five samples; the data holds ten.
	src := muLawSource(bytes.Repeat([]byte{0x00}, 10))
	src.NumTotalSamples = 5
	st, err := src.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	_, err = io.ReadAll(st)
	if err == nil || !strings.Contains(err.Error(), "holds 10 bytes, want 5") {
		t.Fatalf("ReadAll error = %v, want the end-of-data count mismatch", err)
	}
}

func TestStreamingSourceChecksNothing(t *testing.T) {
	data := bytes.Repeat([]byte{0x77}, 64)
	src := muLawSource(data)
	src.NumTotalSamples = 0 // a streaming source: the length is unknown
	st, err := src.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	got, err := io.ReadAll(st)
	if err != nil {
		t.Fatalf("ReadAll: %v (a streaming source must end with a plain EOF)", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("the stream served %d bytes, want the data verbatim", len(got))
	}
	if f := st.Format(); f.NumTotalSamples != 0 {
		t.Fatalf("the format reports %d samples, want 0 (streaming)", f.NumTotalSamples)
	}
}

// stereoPCMSamples renders n interleaved stereo samples as signed 16-bit
// little-endian PCM: a rising left channel and a falling right one.
func stereoPCMSamples(n int) []byte {
	b := make([]byte, n*4)
	for i := 0; i < n; i++ {
		l := int16(i)
		r := int16(-i)
		b[4*i] = byte(l)
		b[4*i+1] = byte(l >> 8)
		b[4*i+2] = byte(r)
		b[4*i+3] = byte(r >> 8)
	}
	return b
}

// encodeFLAC encodes interleaved stereo 16-bit samples as a one-frame
// FLAC stream whose stream info header carries the given sample rate
// and total sample count (0 leaves the count unknown).
func encodeFLAC(t *testing.T, samples []byte, sampleRate uint32, numTotalSamples int) []byte {
	t.Helper()
	n := len(samples) / 4
	channels := make([][]int32, 2)
	for ch := range channels {
		channels[ch] = make([]int32, n)
	}
	for i := 0; i < n; i++ {
		for ch := 0; ch < 2; ch++ {
			o := 4*i + 2*ch
			channels[ch][i] = int32(uint16(samples[o]) | uint16(samples[o+1])<<8)
		}
	}

	var buf bytes.Buffer
	info := &meta.StreamInfo{
		BlockSizeMin:  uint16(n),
		BlockSizeMax:  uint16(n),
		SampleRate:    sampleRate,
		NChannels:     2,
		BitsPerSample: 16,
		NSamples:      uint64(numTotalSamples),
	}
	enc, err := flac.NewEncoder(&buf, info)
	if err != nil {
		t.Fatalf("flac.NewEncoder: %v", err)
	}
	f := &frame.Frame{
		Header: frame.Header{
			HasFixedBlockSize: true,
			BlockSize:         uint16(n),
			SampleRate:        sampleRate,
			Channels:          frame.ChannelsLR,
			BitsPerSample:     16,
			Num:               0,
		},
	}
	for _, ch := range channels {
		f.Subframes = append(f.Subframes, &frame.Subframe{
			SubHeader: frame.SubHeader{Pred: frame.PredVerbatim},
			Samples:   ch,
			NSamples:  n,
		})
	}
	if err := enc.WriteFrame(f); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("Close the flac encoder: %v", err)
	}
	return buf.Bytes()
}

// flacSource is a valid linear PCM source whose inline data is a flac
// stream encoding the given samples.
func flacSource(t *testing.T, samples []byte, sampleRate uint32, numTotalSamples int) *AudioSourceData {
	src := pcmSource(samples)
	src.Compression = CompressionFLAC
	src.InlineData = encodeFLAC(t, samples, sampleRate, numTotalSamples)
	return src
}

func TestFLACDecodesAndRewinds(t *testing.T) {
	samples := stereoPCMSamples(200)
	src := flacSource(t, samples, 48000, 200)
	st, err := src.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	got, err := io.ReadAll(st)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, samples) {
		t.Fatalf("the flac stream decoded %d bytes that differ from the encoded samples", len(got))
	}
	if err := st.Rewind(context.Background()); err != nil {
		t.Fatalf("Rewind: %v", err)
	}
	if got, err := io.ReadAll(st); err != nil || !bytes.Equal(got, samples) {
		t.Fatalf("the second pass = (%d bytes, %v), want the samples again", len(got), err)
	}
}

func TestFLACStreamInfoTakesPrecedence(t *testing.T) {
	samples := stereoPCMSamples(300)
	// The header counts 300 samples; the declared metadata claims 999
	// (a valid combination either way — only the count differs).
	src := flacSource(t, samples, 48000, 300)
	src.NumTotalSamples = 999
	st, err := src.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	f := st.Format()
	if f.SampleFormatType != SampleLinearPCM || f.NumericType != NumSignedInt ||
		f.BitDepth != 16 || f.NumChannels != 2 || f.SampleRate != 48000 || f.NumTotalSamples != 300 {
		t.Fatalf("the format = %+v, want the header's stream info", f)
	}
	// The header's count is also what the end-of-data check enforces:
	// the decoded stream ends exactly at it, with no error.
	if _, err := io.ReadAll(st); err != nil {
		t.Fatalf("ReadAll: %v (the check must follow the header, not the declaration)", err)
	}
}

func TestFLACStreamingHeader(t *testing.T) {
	samples := stereoPCMSamples(50)
	// The header's sample count is unknown (0): a streaming flac source.
	src := flacSource(t, samples, 48000, 0)
	st, err := src.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	if f := st.Format(); f.NumTotalSamples != 0 {
		t.Fatalf("the format reports %d samples, want 0 (unknown)", f.NumTotalSamples)
	}
	if got, err := io.ReadAll(st); err != nil || !bytes.Equal(got, samples) {
		t.Fatalf("the stream = (%d bytes, %v), want the decoded samples with a plain EOF", len(got), err)
	}
}

func TestFLACUnsupportedEffectiveFormat(t *testing.T) {
	// A flac stream at 44.1 kHz: the header's stream info takes
	// precedence, and the effective combination is not supported —
	// whatever the declaration says. (The frame holds 100 samples: the
	// flac block size minimum is 16.)
	samples := stereoPCMSamples(100)
	src := flacSource(t, samples, 44100, 100)

	_, err := src.Open(context.Background())
	if err == nil || !strings.Contains(err.Error(), "stream info says") {
		t.Fatalf("Open error = %v, want the unsupported stream info combination", err)
	}
}
