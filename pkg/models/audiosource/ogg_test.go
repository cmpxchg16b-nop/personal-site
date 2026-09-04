package audiosource

// The ogg opus stream tests: the demuxed packets, their granule-derived
// durations, the header discipline (skipped headers, chained segments),
// the format precedence, the looping and the end-of-data checks. The
// streams are built in-test page by page — header, lacing and checksum
// included — so the package's demuxing is proven against an independent
// construction.

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// oggCRC is the Ogg pages' checksum (RFC 3533's): CRC-32, polynomial
// 0x04C11DB7, no reflection, no pre- or post-conditioning.
var oggCRCTable [256]uint32

func init() {
	for n := range oggCRCTable {
		r := uint32(n) << 24
		for i := 0; i < 8; i++ {
			if r&(1<<31) != 0 {
				r = (r << 1) ^ 0x04c11db7
			} else {
				r <<= 1
			}
		}
		oggCRCTable[n] = r
	}
}

func oggCRC(b []byte) uint32 {
	var crc uint32
	for _, v := range b {
		crc = crc<<8 ^ oggCRCTable[byte(crc>>24)^v]
	}
	return crc
}

// The ogg page header type flags the builder below uses.
const (
	oggFlagContinued = 0x01
	oggFlagFirst     = 0x02
)

// oggBuilder assembles an ogg stream page by page.
type oggBuilder struct {
	buf    bytes.Buffer
	serial uint32
	seq    uint32
}

// page writes one page carrying the given packets, each laced out in
// full (see rawPage for the fragments a spanning packet splits into).
func (b *oggBuilder) page(granule uint64, flags byte, packets ...[]byte) {
	var segs []byte
	var body bytes.Buffer
	for _, pkt := range packets {
		n := len(pkt)
		body.Write(pkt)
		for n >= 255 {
			segs = append(segs, 255)
			n -= 255
		}
		segs = append(segs, byte(n))
	}
	b.rawPage(granule, flags, segs, body.Bytes())
}

// rawPage writes one page with the segment table and the body given
// directly — the fragments a packet spanning pages splits into.
func (b *oggBuilder) rawPage(granule uint64, flags byte, segs []byte, body []byte) {
	var hdr [27]byte
	copy(hdr[:4], "OggS")
	hdr[4] = 0
	hdr[5] = flags
	binary.LittleEndian.PutUint64(hdr[6:14], granule)
	binary.LittleEndian.PutUint32(hdr[14:18], b.serial)
	binary.LittleEndian.PutUint32(hdr[18:22], b.seq)
	b.seq++
	hdr[26] = byte(len(segs))

	var page bytes.Buffer
	page.Write(hdr[:])
	page.Write(segs)
	page.Write(body)
	out := page.Bytes()
	binary.LittleEndian.PutUint32(out[22:26], oggCRC(out))
	b.buf.Write(out)
}

// opusHead builds an ID header packet (RFC 7845 §5.1).
func opusHead(channels uint8, preSkip uint16, rate uint32) []byte {
	pkt := make([]byte, 19)
	copy(pkt, opusHeadMagic)
	pkt[8] = 1 // the version
	pkt[9] = channels
	binary.LittleEndian.PutUint16(pkt[10:12], preSkip)
	binary.LittleEndian.PutUint32(pkt[12:16], rate)
	return pkt
}

// opusTags builds an empty comment header packet (RFC 7845 §5.2).
func opusTags() []byte {
	pkt := make([]byte, 16)
	copy(pkt, opusTagsMagic)
	return pkt // vendor length 0, no user comments
}

// oggOpusSource assembles an ogg opus stream around the given audio
// pages and wraps it in an opus source: an ID header page, a comment
// page, then the pages body builds.
func oggOpusSource(channels uint8, rate uint32, preSkip uint16, body func(b *oggBuilder)) *AudioSourceData {
	var b oggBuilder
	b.serial = 0xABCD
	b.page(0, oggFlagFirst, opusHead(channels, preSkip, rate))
	b.page(0, 0, opusTags())
	body(&b)
	return &AudioSourceData{
		Id:               "test-ogg",
		Name:             "test-ogg",
		SampleFormatType: SampleOpus,
		NumChannels:      int(channels),
		Interleaved:      true,
		InlineData:       b.buf.Bytes(),
		Compression:      CompressionOgg,
		SampleRate:       int(rate),
	}
}

// openPackets opens src's stream and asserts it is a packet stream.
func openPackets(t *testing.T, src *AudioSourceData) PacketStream {
	t.Helper()
	st, err := src.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ps, ok := st.(PacketStream)
	if !ok {
		t.Fatalf("the stream of %q serves no packets", src.Id)
	}
	return ps
}

// TestOggOpusPackets covers the demuxing itself: the two header packets
// are skipped, the audio packets come out whole — off one page or many
// per page — and each packet's duration is its own table of contents',
// never the granule position's: the pages here anchor at a live
// stream's huge counter value, and the durations must not care.
func TestOggOpusPackets(t *testing.T) {
	pkt := func(toc, seed byte, n int) []byte {
		b := make([]byte, n)
		b[0] = toc
		for i := 1; i < n; i++ {
			b[i] = seed + byte(i)
		}
		return b
	}
	// 0xFC: config 31 (fullband, 20 ms frames), code 0 (one frame);
	// 0xFA: config 31, code 2 (two frames).
	a, b1, c, d := pkt(0xFC, 0x10, 40), pkt(0xFC, 0xA0, 32), pkt(0xFA, 0x40, 48), pkt(0xFC, 0x70, 24)
	const liveEdge = 771303869 // a live encoder's counter, days in
	src := oggOpusSource(2, 48000, 312, func(b *oggBuilder) {
		b.page(liveEdge, 0, a, b1) // two packets on one page
		b.page(liveEdge+48000, 0, c)
		b.page(liveEdge+96000, 0, d)
	})
	src.NumTotalSamples = 4 * 960
	st := openPackets(t, src)
	defer st.Close()

	f := st.Format()
	if f.SampleFormatType != SampleOpus || f.SampleRate != 48000 || f.NumChannels != 2 || f.NumTotalSamples != 3840 {
		t.Fatalf("the format = %+v, want the opus family at 48000 Hz stereo", f)
	}
	for _, want := range []struct {
		data []byte
		dur  time.Duration
	}{
		{a, 20 * time.Millisecond},
		{b1, 20 * time.Millisecond},
		{c, 40 * time.Millisecond},
		{d, 20 * time.Millisecond},
	} {
		data, dur, err := st.ReadPacket()
		if err != nil {
			t.Fatalf("ReadPacket: %v", err)
		}
		if !bytes.Equal(data, want.data) {
			t.Fatalf("the packet = %d bytes %02x…, want %02x…", len(data), data[:1], want.data[:1])
		}
		if dur != want.dur {
			t.Fatalf("the packet's duration = %v, want %v (the toc's own, not the granule's)", dur, want.dur)
		}
	}
	if _, _, err := st.ReadPacket(); err != io.EOF {
		t.Fatalf("the end of the stream = %v, want io.EOF", err)
	}
}

// TestOggOpusSpanningPacket covers a packet too long for one page: its
// fragments lace across two pages (the first carrying no completion,
// so no granule position) and the packet reassembles whole.
func TestOggOpusSpanningPacket(t *testing.T) {
	big := make([]byte, 600)
	big[0] = 0xFC // a 20 ms packet
	for i := 1; i < len(big); i++ {
		big[i] = byte(i * 3)
	}
	src := oggOpusSource(2, 48000, 312, func(b *oggBuilder) {
		b.rawPage(math.MaxUint64, 0, []byte{255, 255}, big[:510])
		b.rawPage(312+960, oggFlagContinued, []byte{90}, big[510:])
	})
	st := openPackets(t, src)
	defer st.Close()

	data, dur, err := st.ReadPacket()
	if err != nil {
		t.Fatalf("ReadPacket: %v", err)
	}
	if !bytes.Equal(data, big) {
		t.Fatalf("the spanning packet = %d bytes that differ from the fragments", len(data))
	}
	if dur != 20*time.Millisecond {
		t.Fatalf("the packet's duration = %v, want 20ms", dur)
	}
}

// TestOggOpusHeaderDiscipline covers the format precedence and the
// refusal of streams outside the supported combination: the ID header
// decides channels and rate, whatever the metadata declares.
func TestOggOpusHeaderDiscipline(t *testing.T) {
	for _, tc := range []struct {
		name     string
		channels uint8
		rate     uint32
		version  byte
		want     string // a fragment of the expected error; "" for accepted
	}{
		{"the accepted stereo combination", 2, 48000, 1, ""},
		{"a mono stream", 1, 48000, 1, ""},
		{"a 44.1 kHz input rate", 2, 44100, 1, "sample rate is 44100"},
		{"too many channels", 3, 48000, 1, "3ch"},
		{"a future header version", 2, 48000, 2, "version is 2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			head := opusHead(tc.channels, 312, tc.rate)
			head[8] = tc.version
			var b oggBuilder
			b.serial = 7
			b.page(0, oggFlagFirst, head)
			src := oggOpusSource(tc.channels, tc.rate, 312, func(b *oggBuilder) {})
			// The header's word is what counts: the declared metadata is a
			// valid combination either way, and only the header says 44100.
			src.SampleRate = 48000
			src.InlineData = b.buf.Bytes()

			_, err := src.Open(context.Background())
			if tc.want == "" && err != nil {
				t.Fatalf("Open: unexpected error %v", err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("Open error = %v, want one mentioning %q", err, tc.want)
			}
		})
	}
}

// TestOggOpusRewinds covers the looping discipline: a stream read to
// its end rewinds to its first packet — headers re-parsed, the timing
// re-baselined at the pre-skip.
func TestOggOpusRewinds(t *testing.T) {
	pkts := [][]byte{{0x01, 0x02, 0x03}, {0x04, 0x05, 0x06}}
	src := oggOpusSource(2, 48000, 312, func(b *oggBuilder) {
		b.page(312+960, 0, pkts[0])
		b.page(312+960+960, 0, pkts[1])
	})
	st := openPackets(t, src)
	defer st.Close()

	pass := func() {
		for i, want := range pkts {
			data, _, err := st.ReadPacket()
			if err != nil {
				t.Fatalf("ReadPacket %d: %v", i, err)
			}
			if !bytes.Equal(data, want) {
				t.Fatalf("packet %d = %v, want %v", i, data, want)
			}
		}
	}
	pass()
	if _, _, err := st.ReadPacket(); err != io.EOF {
		t.Fatalf("the end of the stream = %v, want io.EOF", err)
	}
	if err := st.Rewind(context.Background()); err != nil {
		t.Fatalf("Rewind: %v", err)
	}
	pass()
}

// TestOggOpusHTTPRefetchesOnRewind covers the remote case the feature
// is for: the source's bytes are an http body's, fetched on open and
// re-fetched on every rewind.
func TestOggOpusHTTPRefetchesOnRewind(t *testing.T) {
	var b oggBuilder
	b.serial = 9
	b.page(0, oggFlagFirst, opusHead(2, 312, 48000))
	b.page(0, 0, opusTags())
	b.page(312+960, 0, []byte{0xFC, 0x22})
	var fetches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		_, _ = w.Write(b.buf.Bytes())
	}))
	defer srv.Close()

	src := oggOpusSource(2, 48000, 312, func(b *oggBuilder) {})
	src.InlineData = nil
	src.URL = srv.URL
	src.NumTotalSamples = 960

	st := openPackets(t, src)
	defer st.Close()
	if got := fetches.Load(); got != 1 {
		t.Fatalf("Open fetched %d times, want 1", got)
	}
	data, _, err := st.ReadPacket()
	if err != nil || !bytes.Equal(data, []byte{0xFC, 0x22}) {
		t.Fatalf("the packet = (%v, %v)", data, err)
	}
	if _, _, err := st.ReadPacket(); err != io.EOF {
		t.Fatalf("the end of the stream = %v, want io.EOF", err)
	}
	if err := st.Rewind(context.Background()); err != nil {
		t.Fatalf("Rewind: %v", err)
	}
	if got := fetches.Load(); got != 2 {
		t.Fatalf("Rewind re-fetched %d times in total, want 2", got)
	}
	if data, _, err := st.ReadPacket(); err != nil || !bytes.Equal(data, []byte{0xFC, 0x22}) {
		t.Fatalf("the re-fetched packet = (%v, %v)", data, err)
	}
}

// TestOggOpusChainedSegment covers a stream that starts a new logical
// segment mid-way: the second ID header re-baselines the timing at its
// own pre-skip, and the packets play on.
func TestOggOpusChainedSegment(t *testing.T) {
	src := oggOpusSource(2, 48000, 312, func(b *oggBuilder) {
		b.page(312+960, 0, []byte{0x21})
		// A chained segment: its own headers, its own pre-skip.
		b.page(0, oggFlagFirst, opusHead(2, 0, 48000))
		b.page(0, 0, opusTags())
		b.page(960, 0, []byte{0x22})
	})
	st := openPackets(t, src)
	defer st.Close()

	for i, want := range []byte{0x21, 0x22} {
		data, dur, err := st.ReadPacket()
		if err != nil {
			t.Fatalf("ReadPacket %d: %v", i, err)
		}
		if !bytes.Equal(data, []byte{want}) {
			t.Fatalf("packet %d = %v, want %02x", i, data, want)
		}
		if dur != 20*time.Millisecond {
			t.Fatalf("packet %d's duration = %v, want 20ms", i, dur)
		}
	}
}

// TestOggOpusCountCheck covers the end-of-data discipline: the served
// media time must match the declared total — a truncated stream is an
// error, not a short song. (The packet durations being the toc's own,
// the check counts them, tolerating the encoder's pre-skip and tail
// padding.)
func TestOggOpusCountCheck(t *testing.T) {
	src := oggOpusSource(2, 48000, 312, func(b *oggBuilder) {
		b.page(312+960, 0, []byte{0xFC, 0x31})
		b.page(312+960+960, 0, []byte{0xFC, 0x32})
	})
	src.NumTotalSamples = 7993 // the stream serves only 1920
	st := openPackets(t, src)
	defer st.Close()

	if _, _, err := st.ReadPacket(); err != nil {
		t.Fatalf("the first packet: %v", err)
	}
	if _, _, err := st.ReadPacket(); err != nil {
		t.Fatalf("the second packet: %v", err)
	}
	if _, _, err := st.ReadPacket(); err == nil || !strings.Contains(err.Error(), "served 1920 samples, want 7993") {
		t.Fatalf("the third read = %v, want the count mismatch", err)
	}
}

// TestOggOpusMalformed covers the malformed streams the demuxer must
// refuse: a broken checksum and a zero-length packet.
func TestOggOpusMalformed(t *testing.T) {
	t.Run("a broken checksum", func(t *testing.T) {
		src := oggOpusSource(2, 48000, 312, func(b *oggBuilder) {
			b.page(312+960, 0, []byte{0xFC, 0x41})
		})
		// The stream's last byte is the audio page's single payload byte.
		src.InlineData[len(src.InlineData)-1] ^= 0xFF
		st := openPackets(t, src)
		defer st.Close()
		if _, _, err := st.ReadPacket(); err == nil || !strings.Contains(err.Error(), "checksum") {
			t.Fatalf("ReadPacket = %v, want the checksum mismatch", err)
		}
	})
	t.Run("a zero-length packet", func(t *testing.T) {
		src := oggOpusSource(2, 48000, 312, func(b *oggBuilder) {
			b.page(312+960, 0, []byte{})
		})
		st := openPackets(t, src)
		defer st.Close()
		if _, _, err := st.ReadPacket(); err == nil || !strings.Contains(err.Error(), "zero-length") {
			t.Fatalf("ReadPacket = %v, want the zero-length refusal", err)
		}
	})
}
