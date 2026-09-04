package musicbot

// The player's unit tests: the looping read, the stereo normalizer's
// sample math, and the player's switch lifecycle. The pump's media
// output is proven by the wire suite; here the pieces are pinned in
// isolation.

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"math"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"personal-site/pkg/models/audiosource"
	"personal-site/pkg/models/ss"
)

// memSource is an in-memory loopingSource: a rewindable byte reader.
type memSource struct {
	r *bytes.Reader
}

func newMemSource(data []byte) *memSource {
	return &memSource{r: bytes.NewReader(data)}
}

func (m *memSource) Read(p []byte) (int, error) { return m.r.Read(p) }

func (m *memSource) Rewind(_ context.Context) error {
	_, err := m.r.Seek(0, io.SeekStart)
	return err
}

func TestReadLooping(t *testing.T) {
	ctx := context.Background()

	// A stream shorter than the buffer loops: the fill continues from
	// the stream's beginning, so the buffer holds the pattern twice.
	buf := make([]byte, 6)
	if err := readLooping(ctx, newMemSource([]byte{1, 2, 3}), buf); err != nil {
		t.Fatalf("readLooping: %v", err)
	}
	if want := []byte{1, 2, 3, 1, 2, 3}; !bytes.Equal(buf, want) {
		t.Fatalf("readLooping filled %v, want %v", buf, want)
	}

	// A stream longer than the buffer reads its head only.
	if err := readLooping(ctx, newMemSource([]byte{9, 8, 7, 6}), buf[:2]); err != nil {
		t.Fatalf("readLooping: %v", err)
	}
	if want := []byte{9, 8}; !bytes.Equal(buf[:2], want) {
		t.Fatalf("readLooping filled %v, want %v", buf[:2], want)
	}

	// An empty stream is an error, not an infinite loop of rewinds.
	if err := readLooping(ctx, newMemSource(nil), buf); err == nil {
		t.Fatal("readLooping over an empty stream did not fail")
	}
}

// memPackets is an in-memory packetSource: a rewindable packet list.
type memPackets struct {
	packets [][]byte
	i       int
}

func (m *memPackets) ReadPacket() ([]byte, time.Duration, error) {
	if m.i >= len(m.packets) {
		return nil, 0, io.EOF
	}
	m.i++
	return m.packets[m.i-1], 20 * time.Millisecond, nil
}

func (m *memPackets) Rewind(context.Context) error {
	m.i = 0
	return nil
}

func TestReadPacketLooping(t *testing.T) {
	ctx := context.Background()

	// A stream of two packets loops: the reads keep coming, restarting
	// at the first packet past the end.
	src := &memPackets{packets: [][]byte{{1, 2}, {3}}}
	for i, want := range [][]byte{{1, 2}, {3}, {1, 2}, {3}} {
		data, _, err := readPacketLooping(ctx, src)
		if err != nil {
			t.Fatalf("readPacketLooping %d: %v", i, err)
		}
		if !bytes.Equal(data, want) {
			t.Fatalf("readPacketLooping %d = %v, want %v", i, data, want)
		}
	}

	// An empty stream is an error, not an infinite loop of rewinds.
	if _, _, err := readPacketLooping(ctx, &memPackets{}); err == nil {
		t.Fatal("readPacketLooping over an empty stream did not fail")
	}
}

func TestSampleToInt16(t *testing.T) {
	be := func(bs ...byte) []byte { return bs }
	le16 := func(v int16) []byte { b := make([]byte, 2); binary.LittleEndian.PutUint16(b, uint16(v)); return b }
	for _, tc := range []struct {
		name     string
		b        []byte
		unsigned bool
		want     int16
	}{
		{"u8 midpoint", be(128), true, 0},
		{"u8 full", be(255), true, 32511}, // (255-128)/128 of full scale
		{"u8 floor", be(0), true, -32767},
		{"s16 zero", le16(0), false, 0},
		{"s16 full", le16(32767), false, 32766}, // 32767/32768 of full scale
		{"s16 floor", le16(-32768), false, -32767},
	} {
		if got := sampleToInt16(tc.b, tc.unsigned); got != tc.want {
			t.Errorf("%s: sampleToInt16 = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestStereoNormalizer covers the linear PCM normalization: signed
// 16-bit samples pass through (the opus encoder's own shape), and the
// other widths and numeric types land correctly.
func TestStereoNormalizer(t *testing.T) {
	ctx := context.Background()

	// Signed 16-bit stereo: identity.
	s16 := newStereoNormalizer(newMemSource([]byte{
		0x00, 0x7F, 0x00, 0x00, // 32512, 0
		0x01, 0x00, 0xFF, 0xFF, // 1, -1
	}), 16, audiosource.NumSignedInt)
	pcm := make([]int16, 4)
	if err := s16.readFrame(ctx, pcm); err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if pcm[0] != 32512 || pcm[1] != 0 || pcm[2] != 1 || pcm[3] != -1 {
		t.Fatalf("the s16 normalization = %v", pcm)
	}

	// Unsigned 8-bit stereo: the bias shifts to signed.
	u8 := newStereoNormalizer(newMemSource([]byte{128, 255, 0, 128}), 8, audiosource.NumUnsignedInt)
	if err := u8.readFrame(ctx, pcm); err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if pcm[0] != 0 || pcm[1] != 32511 || pcm[2] != -32767 || pcm[3] != 0 {
		t.Fatalf("the u8 normalization = %v", pcm)
	}

	// Float 32-bit stereo: the samples ride the encoder's own float
	// shape — no 16-bit conversion — with only the past-full-scale ones
	// clipped, as the integer path clips.
	f32 := newStereoNormalizer(newMemSource(float32StereoSamples(0, 0.5, -1, 2)), 32, audiosource.NumFloat)
	fpcm := make([]float32, 4)
	if err := f32.readFrameFloat32(ctx, fpcm); err != nil {
		t.Fatalf("readFrameFloat32: %v", err)
	}
	if fpcm[0] != 0 || fpcm[1] != 0.5 || fpcm[2] != -1 || fpcm[3] != 1 {
		t.Fatalf("the f32 passthrough = %v", fpcm)
	}
}

// float32StereoSamples renders n interleaved stereo samples as 32-bit
// float little-endian PCM.
func float32StereoSamples(values ...float32) []byte {
	b := make([]byte, len(values)*4)
	for i, v := range values {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(v))
	}
	return b
}

// TestPlayerSetSource covers the switch lifecycle: a same-family switch
// is applied by the pump and answered; a stopped player answers the
// stopped error instead.
func TestPlayerSetSource(t *testing.T) {
	ctx := context.Background()
	p, err := newPlayer(ctx, testLogger(t), ss.SubscriberId("1-user"), muLawSong("one", 4))
	if err != nil {
		t.Fatalf("newPlayer: %v", err)
	}
	if err := p.setSource(muLawSong("two", 4)); err != nil {
		t.Fatalf("setSource: %v", err)
	}
	if !p.accepts(muLawSong("another", 4)) {
		t.Fatal("a mu-law player refused a mu-law source")
	}
	if p.accepts(pcmSong("stereo", 8)) {
		t.Fatal("a mu-law player accepted a linear pcm source — the families must not mix")
	}
	p.stop()
	if err := p.setSource(muLawSong("three", 4)); err == nil {
		t.Fatal("a stopped player switched songs")
	}
	// stop is idempotent.
	p.stop()
}

// TestPlayerRunStops covers the pump's lifecycle: it ticks while
// running (an unbound track swallows the writes, so the pump is
// observable here only through its liveness) and stops on stop().
func TestPlayerRunStops(t *testing.T) {
	p, err := newPlayer(context.Background(), testLogger(t), ss.SubscriberId("1-user"), muLawSong("one", 4))
	if err != nil {
		t.Fatalf("newPlayer: %v", err)
	}
	time.Sleep(100 * time.Millisecond) // a few ticks
	p.stop()
	// The switch path reports the stop rather than hanging.
	done := make(chan error, 1)
	go func() { done <- p.setSource(muLawSong("two", 4)) }()
	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("setSource on a stopped player did not answer")
	}
}

// livePackets is a self-paced packet source: every read blocks longer
// than the packet's duration before answering, as a live radio stream
// does — the pump's pacing wait is never positive with one.
type livePackets struct{ dur time.Duration }

func (l *livePackets) ReadPacket() ([]byte, time.Duration, error) {
	time.Sleep(l.dur + l.dur/2)
	return []byte{0xF8, 0x01}, l.dur, nil
}

func (l *livePackets) Rewind(context.Context) error { return nil }

func (l *livePackets) Close() error { return nil }

// TestPlayerPacketPumpLiveStream covers the opus pump's control
// discipline on a self-paced source: even when the pacing wait never
// opens (the source's reads arrive at real time, so there is nothing
// to wait out), a mid-call switch is answered and a stop ends the
// pump — a pump that listened only inside the wait would do neither.
func TestPlayerPacketPumpLiveStream(t *testing.T) {
	track, err := webrtc.NewTrackLocalStaticSample(opusTrackCodec, "music-opus", "musicbot")
	if err != nil {
		t.Fatalf("NewTrackLocalStaticSample: %v", err)
	}
	p := &player{
		track:    track,
		pkts:     &livePackets{dur: frameDuration},
		switches: make(chan switchReq),
		stopCh:   make(chan struct{}),
	}
	done := make(chan struct{})
	go func() {
		p.runPackets(context.Background(), testLogger(t), ss.SubscriberId("1-user"))
		close(done)
	}()
	time.Sleep(3 * frameDuration) // a few packets

	// A switch mid-call is answered: retune opens the new song and the
	// pump plays it. (An unresponsive pump would deadlock the caller.)
	song, _ := opusSong("live", 2)
	if err := p.setSource(song); err != nil {
		t.Fatalf("setSource: %v", err)
	}
	if p.src != song {
		t.Fatal("the switch was not applied")
	}

	p.stop()
	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("the pump did not stop on a self-paced source")
	}
}
