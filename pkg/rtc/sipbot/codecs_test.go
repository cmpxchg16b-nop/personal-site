package sipbot

// The codec vocabulary's units: the presumed-list's parse and
// validation, the pion↔diago mapping, the SDP codec-list read, the
// selection, and the packet-duration helpers (the opus TOC against RFC
// 6716's table).

import (
	"strings"
	"testing"
	"time"

	"github.com/emiago/diago/media"
	"github.com/pion/webrtc/v4"
)

// TestParseWebRTCCodecs covers the presumed-list attribute: the default,
// the custom order preserved, case and whitespace tolerated, and the
// misconfigurations refused.
func TestParseWebRTCCodecs(t *testing.T) {
	got, err := ParseWebRTCCodecs("")
	if err != nil || strings.Join(got, ",") != "opus,g722,pcmu,pcma" {
		t.Fatalf("the default: got (%v, %v)", got, err)
	}
	got, err = ParseWebRTCCodecs(" PCMU ,Opus ")
	if err != nil || strings.Join(got, ",") != "pcmu,opus" {
		t.Fatalf("case and whitespace: got (%v, %v), want [pcmu opus]", got, err)
	}
	for _, bad := range []string{"opus,speex", "opus,,pcmu", "g711", ","} {
		if _, err := ParseWebRTCCodecs(bad); err == nil {
			t.Fatalf("ParseWebRTCCodecs(%q): got no error, want one", bad)
		}
	}
}

// TestCodecMapping covers the two dialects' shapes of the vocabulary:
// every name maps to its diago codec, every diago codec of the
// vocabulary to its pion parameters — and the round trip agrees.
func TestCodecMapping(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mimeType  string
		clockRate uint32
	}{
		{"opus", webrtc.MimeTypeOpus, 48000},
		{"g722", webrtc.MimeTypeG722, 8000},
		{"pcmu", webrtc.MimeTypePCMU, 8000},
		{"pcma", webrtc.MimeTypePCMA, 8000},
	} {
		codec, ok := sipCodecByName(tc.name)
		if !ok {
			t.Fatalf("sipCodecByName(%q): not mapped", tc.name)
		}
		params, ok := webrtcCodecParams(codec)
		if !ok {
			t.Fatalf("webrtcCodecParams(%q): not mapped", codec.Name)
		}
		if params.MimeType != tc.mimeType || params.ClockRate != tc.clockRate {
			t.Fatalf("%s: got (%s, %d), want (%s, %d)", tc.name, params.MimeType, params.ClockRate, tc.mimeType, tc.clockRate)
		}
		if params.PayloadType != 0 {
			t.Fatalf("%s: the pin carries PT %d, want 0 (the engine's assignment)", tc.name, params.PayloadType)
		}
	}
	if _, ok := sipCodecByName("speex"); ok {
		t.Fatal("sipCodecByName(speex): mapped, want not")
	}
	if _, ok := webrtcCodecParams(media.CodecTelephoneEvent8000); ok {
		t.Fatal("webrtcCodecParams(telephone-event): mapped, want not — it rides the pin's auxiliary slot")
	}
	// The stereo fmtp is the opus mapping's (the webrtc-leg's stereo
	// negotiation mirrored onto both the sip leg and the pin).
	if codec, _ := sipCodecByName("opus"); codec.Fmtp != "useinbandfec=1;stereo=1" {
		t.Fatalf("the sip-leg opus fmtp is %q", codec.Fmtp)
	}
	if params, _ := webrtcCodecParams(media.CodecAudioOpus); params.SDPFmtpLine != "minptime=10;useinbandfec=1;stereo=1" || params.Channels != 2 {
		t.Fatalf("the webrtc-leg opus params are %v", params)
	}
}

// TestAudioCodecsFromSDP covers the SDP codec-list read: rtpmap lines
// resolved, the static payload types needing none (G.722's shorthand
// included), the offer's order preserved.
func TestAudioCodecsFromSDP(t *testing.T) {
	body := "v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=x\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\n" +
		"m=audio 4000 RTP/AVP 111 9 0 101\r\n" +
		"a=rtpmap:111 opus/48000/2\r\n" +
		"a=rtpmap:101 telephone-event/8000\r\n"
	codecs, err := audioCodecsFromSDP([]byte(body))
	if err != nil {
		t.Fatalf("audioCodecsFromSDP: %v", err)
	}
	names := make([]string, len(codecs))
	for i, c := range codecs {
		names[i] = c.Name
	}
	if strings.Join(names, ",") != "opus,G722,PCMU,telephone-event" {
		t.Fatalf("got %v, want the offer's order: opus,G722,PCMU,telephone-event", names)
	}
	if _, err := audioCodecsFromSDP([]byte("v=0\r\n")); err == nil {
		t.Fatal("no audio m-line: got no error, want one")
	}
}

// TestSelectCodec covers the end-to-end selection: the presumed list's
// order wins over the offer's, a name match is case-insensitive, a
// sample-rate mismatch is no match, and nothing shared is false.
func TestSelectCodec(t *testing.T) {
	presumed := []media.Codec{media.CodecAudioOpus, media.CodecAudioG722, media.CodecAudioUlaw, media.CodecAudioAlaw}
	// The offer's preference is the reverse of the presumed one — the
	// presumed (browsers') order still selects.
	got, ok := selectCodec(presumed, []media.Codec{media.CodecAudioUlaw, media.CodecAudioG722})
	if !ok || got.Name != media.CodecAudioG722.Name {
		t.Fatalf("got (%v, %v), want G722", got, ok)
	}
	if _, ok := selectCodec(presumed, []media.Codec{{Name: "speex", SampleRate: 8000}}); ok {
		t.Fatal("a shared nothing offer selected")
	}
	// A lowercase spelling matches (wire spellings vary).
	if got, ok := selectCodec(presumed, []media.Codec{{Name: "g722", SampleRate: 8000, PayloadType: 9}}); !ok || got.Name != media.CodecAudioG722.Name {
		t.Fatalf("lowercase g722: got (%v, %v)", got, ok)
	}
	// A G.722 announced at the wrong clock is not G.722 (RFC 3551's RTP
	// clock is 8000 Hz whatever thertpmap claims).
	if _, ok := selectCodec(presumed, []media.Codec{{Name: "G722", SampleRate: 16000, PayloadType: 9}}); ok {
		t.Fatal("a 16 kHz G722 selected")
	}
}

// TestPacketDuration covers the browser-bound payloads' durations: the
// opus TOC read for opus, the 8-bytes-per-millisecond rule of the 64
// kbps 8 kHz codecs.
func TestPacketDuration(t *testing.T) {
	if got := packetDuration(media.CodecAudioOpus, []byte{0xf8}); got != 20*time.Millisecond {
		t.Fatalf("opus: got %v, want 20 ms", got)
	}
	for _, codec := range []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw, media.CodecAudioG722} {
		if got := packetDuration(codec, make([]byte, 240)); got != 30*time.Millisecond {
			t.Fatalf("%s: got %v, want 30 ms (240 bytes at 64 kbps)", codec.Name, got)
		}
		if got := packetDuration(codec, nil); got != ptimeDefault {
			t.Fatalf("%s empty: got %v, want the ptime default", codec.Name, got)
		}
	}
}

// TestOpusPacketDuration checks the TOC parse against RFC 6716 §3.1's
// shapes: SILK, hybrid and CELT configurations, the frame-count codes
// (including code 3's count byte), and the malformed fallbacks.
func TestOpusPacketDuration(t *testing.T) {
	// The TOC packs the fields: config in bits 3-7, the frame-count code
	// in bits 0-1.
	for _, tc := range []struct {
		name string
		pkt  []byte
		want time.Duration
	}{
		{"empty", nil, ptimeDefault},
		{"code3 without count byte", []byte{0x03}, ptimeDefault},
		{"code3 zero count", []byte{0x03, 0x00}, ptimeDefault},
		{"silk 10ms x1", []byte{0 << 3}, 10 * time.Millisecond},
		{"silk 20ms x1", []byte{1 << 3}, 20 * time.Millisecond},
		{"silk 40ms x2 code1", []byte{2<<3 | 1}, 80 * time.Millisecond},
		{"silk 60ms x1", []byte{3 << 3}, 60 * time.Millisecond},
		{"hybrid 10ms x1", []byte{12 << 3}, 10 * time.Millisecond},
		{"hybrid 20ms x2 code2", []byte{13<<3 | 2}, 40 * time.Millisecond},
		{"celt 2.5ms x1", []byte{16 << 3}, 2500 * time.Microsecond},
		{"celt 20ms x1", []byte{19 << 3}, 20 * time.Millisecond},
		{"celt 20ms x4 code3", []byte{19<<3 | 3, 4 << 2}, 80 * time.Millisecond},
		{"fullband voice packet (0xf8)", []byte{0xf8}, 20 * time.Millisecond},
		{"overlong is refused", []byte{3<<3 | 3, 3 << 2}, ptimeDefault}, // 3 × 60 ms > 120 ms
	} {
		if got := opusPacketDuration(tc.pkt); got != tc.want {
			t.Errorf("%s: opusPacketDuration = %v, want %v", tc.name, got, tc.want)
		}
	}
}
