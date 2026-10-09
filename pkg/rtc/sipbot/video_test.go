package sipbot

// The video half's units: the presumed video list's parse and
// validation, the pion↔diago video mapping, the SDP generators (the
// offer's one-port audio+video shape, the answer's accept and rejection
// shapes), and the two video selections (the callee's pick outbound, the
// browser-preferred echo inbound).

import (
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/emiago/diago/media"
	"github.com/pion/webrtc/v4"
)

// testVideoCodecs is the presumed video list the tables run against —
// the default vocabulary, resolved.
func testVideoCodecs() []media.Codec {
	vp8, _ := sipVideoCodecByName("vp8")
	h264, _ := sipVideoCodecByName("h264")
	return []media.Codec{vp8, h264}
}

// TestParseWebRTCVideoCodecs covers the presumed-list attribute: the
// default, the custom order preserved, case and whitespace tolerated,
// and the misconfigurations refused.
func TestParseWebRTCVideoCodecs(t *testing.T) {
	got, err := ParseWebRTCVideoCodecs("")
	if err != nil || strings.Join(got, ",") != "vp8,h264" {
		t.Fatalf("the default: got (%v, %v)", got, err)
	}
	got, err = ParseWebRTCVideoCodecs(" H264 ,Vp8 ")
	if err != nil || strings.Join(got, ",") != "h264,vp8" {
		t.Fatalf("case and whitespace: got (%v, %v), want [h264 vp8]", got, err)
	}
	for _, bad := range []string{"vp8,vp9", "vp8,,h264", "av1", ","} {
		if _, err := ParseWebRTCVideoCodecs(bad); err == nil {
			t.Fatalf("ParseWebRTCVideoCodecs(%q): got no error, want one", bad)
		}
	}
}

// TestVideoCodecMapping covers the two dialects' shapes of the video
// vocabulary: every name maps to its diago codec (its payload type clear
// of the audio offer's), every diago codec of the vocabulary to its pion
// parameters — and the round trip agrees.
func TestVideoCodecMapping(t *testing.T) {
	for _, tc := range []struct {
		name      string
		pt        uint8
		mimeType  string
		clockRate uint32
		fmtp      string
	}{
		{"vp8", 102, webrtc.MimeTypeVP8, 90000, ""},
		{"h264", 103, webrtc.MimeTypeH264, 90000, "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"},
	} {
		codec, ok := sipVideoCodecByName(tc.name)
		if !ok {
			t.Fatalf("sipVideoCodecByName(%q): not mapped", tc.name)
		}
		if codec.PayloadType != tc.pt || codec.SampleRate != tc.clockRate {
			t.Fatalf("%s: got (pt %d, rate %d), want (pt %d, rate %d)", tc.name, codec.PayloadType, codec.SampleRate, tc.pt, tc.clockRate)
		}
		params, ok := webrtcVideoCodecParams(codec)
		if !ok {
			t.Fatalf("webrtcVideoCodecParams(%q): not mapped", codec.Name)
		}
		if params.MimeType != tc.mimeType || params.ClockRate != tc.clockRate || params.SDPFmtpLine != tc.fmtp {
			t.Fatalf("%s: got (%s, %d, %q), want (%s, %d, %q)", tc.name, params.MimeType, params.ClockRate, params.SDPFmtpLine, tc.mimeType, tc.clockRate, tc.fmtp)
		}
		if params.PayloadType != 0 {
			t.Fatalf("%s: the pin carries PT %d, want 0 (the engine's assignment)", tc.name, params.PayloadType)
		}
	}
	if _, ok := sipVideoCodecByName("vp9"); ok {
		t.Fatal("sipVideoCodecByName(vp9): mapped, want not")
	}
	if _, ok := webrtcVideoCodecParams(media.CodecAudioOpus); ok {
		t.Fatal("webrtcVideoCodecParams(opus): mapped, want not — the vocabularies are disjoint")
	}
}

// TestGenerateSIPOffer covers the outbound offer's shape: diago's own
// head and audio section, the video section on the SAME port (the mux),
// the video rtpmap lines (no channel count), and H.264's fmtp.
func TestGenerateSIPOffer(t *testing.T) {
	ip := net.ParseIP("192.0.2.1")
	body := string(generateSIPOffer(ip, ip, 4000, []media.Codec{codecAudioOpusStereo, media.CodecAudioUlaw, media.CodecTelephoneEvent8000}, testVideoCodecs()))
	audioPort, videoPort := sdpMediaPort(t, body, "audio"), sdpMediaPort(t, body, "video")
	if audioPort != 4000 || videoPort != 4000 {
		t.Fatalf("the media ports are (audio %d, video %d), want both 4000 (the mux):\n%s", audioPort, videoPort, body)
	}
	for _, want := range []string{
		"m=audio 4000 RTP/AVP 96 0 101",
		"a=rtpmap:96 opus/48000/2",
		"a=fmtp:96 useinbandfec=1;stereo=1",
		"a=rtpmap:101 telephone-event/8000",
		"a=ptime:20",
		"m=video 4000 RTP/AVP 102 103",
		"a=rtpmap:102 VP8/90000",
		"a=rtpmap:103 H264/90000",
		"a=fmtp:103 packetization-mode=1;profile-level-id=42e01f",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("the offer lacks %q:\n%s", want, body)
		}
	}
	// The head is diago's own shape.
	for _, want := range []string{"v=0\r\n", "o=- ", "s=Sip Go Media", "c=IN IP4 192.0.2.1", "t=0 0"} {
		if !strings.Contains(body, want) {
			t.Fatalf("the offer's head lacks %q:\n%s", want, body)
		}
	}
}

// TestGenerateSIPAnswer covers the inbound answer's two shapes: the
// accepted video (the selected codec at the offer's PT, its fmtp
// echoed) and the port-0 rejection carrying the offered format list.
func TestGenerateSIPAnswer(t *testing.T) {
	ip := net.ParseIP("192.0.2.1")
	audio := []media.Codec{media.CodecAudioOpus, media.CodecTelephoneEvent8000}

	// The accept: the offer's VP8 rode PT 120 — the answer echoes it.
	sel := videoOfferSelection{ok: true, offered: true, codec: media.Codec{Name: "VP8", PayloadType: 120, SampleRate: 90000}}
	body := string(generateSIPAnswer(ip, ip, 4000, audio, sel))
	for _, want := range []string{
		"m=audio 4000 RTP/AVP 96 101",
		"m=video 4000 RTP/AVP 120",
		"a=rtpmap:120 VP8/90000",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("the accepting answer lacks %q:\n%s", want, body)
		}
	}

	// The H.264 accept echoes the offer's fmtp.
	sel.codec = media.Codec{Name: "H264", PayloadType: 99, SampleRate: 90000, Fmtp: "profile-level-id=640028"}
	body = string(generateSIPAnswer(ip, ip, 4000, audio, sel))
	for _, want := range []string{"m=video 4000 RTP/AVP 99", "a=rtpmap:99 H264/90000", "a=fmtp:99 profile-level-id=640028"} {
		if !strings.Contains(body, want) {
			t.Fatalf("the H.264 answer lacks %q:\n%s", want, body)
		}
	}

	// The rejection: port 0, the offered formats kept.
	sel = videoOfferSelection{offered: true, formats: []string{"31", "32"}}
	body = string(generateSIPAnswer(ip, ip, 4000, audio, sel))
	if !strings.Contains(body, "m=video 0 RTP/AVP 31 32") {
		t.Fatalf("the rejecting answer lacks the port-0 line:\n%s", body)
	}
	if strings.Contains(body, "a=rtpmap:31") {
		t.Fatalf("the rejecting answer carries attributes:\n%s", body)
	}
}

// TestSelectVideoFromAnswer covers the outbound read: the callee's pick
// in the answer's own order, the answer's PT and video address, and the
// degradations (no video m-line, a rejected one, nothing in common).
func TestSelectVideoFromAnswer(t *testing.T) {
	presumed := testVideoCodecs()
	answer := func(videoSection string) []byte {
		return []byte("v=0\r\no=- 0 0 IN IP4 192.0.2.2\r\ns=x\r\nc=IN IP4 192.0.2.2\r\nt=0 0\r\n" +
			"m=audio 5000 RTP/AVP 0 101\r\na=rtpmap:0 PCMU/8000\r\n" + videoSection)
	}

	// The callee picked H.264 first (its own order wins) on the shared
	// port — the muxed case.
	codec, raddr, ok := selectVideoFromAnswer(answer("m=video 5000 RTP/AVP 103 102\r\na=rtpmap:103 H264/90000\r\na=rtpmap:102 VP8/90000\r\n"), presumed)
	if !ok || codec.Name != "H264" || codec.PayloadType != 103 {
		t.Fatalf("got (%v, %v), want H264 at 103", codec, ok)
	}
	if raddr == nil || raddr.Port != 5000 || !raddr.IP.Equal(net.ParseIP("192.0.2.2")) {
		t.Fatalf("the video address is %v, want 192.0.2.2:5000", raddr)
	}

	// The demuxed case: video on a port of its own.
	codec, raddr, ok = selectVideoFromAnswer(answer("m=video 5002 RTP/AVP 102\r\na=rtpmap:102 VP8/90000\r\n"), presumed)
	if !ok || codec.Name != "VP8" || codec.PayloadType != 102 {
		t.Fatalf("demuxed: got (%v, %v), want VP8 at 102", codec, ok)
	}
	if raddr == nil || raddr.Port != 5002 {
		t.Fatalf("the demuxed video address is %v, want port 5002", raddr)
	}

	// The degradations: no video m-line, a rejected one, a codec the
	// presumed list does not name.
	if _, _, ok := selectVideoFromAnswer(answer(""), presumed); ok {
		t.Fatal("no video m-line: selected, want not")
	}
	if _, _, ok := selectVideoFromAnswer(answer("m=video 0 RTP/AVP 102 103\r\n"), presumed); ok {
		t.Fatal("a rejected video m-line: selected, want not")
	}
	if _, _, ok := selectVideoFromAnswer(answer("m=video 5000 RTP/AVP 31\r\na=rtpmap:31 MP4V-ES/90000\r\n"), presumed); ok {
		t.Fatal("an unknown video codec: selected, want not")
	}
}

// TestSelectVideoFromOffer covers the inbound read: the presumed order
// wins over the caller's, the offer's PT and fmtp adopted (the answer
// echoes them), the rejection shapes still reporting the offered format
// list, and the video-less offer.
func TestSelectVideoFromOffer(t *testing.T) {
	presumed := testVideoCodecs()
	offer := func(videoSection string) []byte {
		return []byte("v=0\r\no=- 0 0 IN IP4 192.0.2.3\r\ns=x\r\nc=IN IP4 192.0.2.3\r\nt=0 0\r\n" +
			"m=audio 6000 RTP/AVP 96 101\r\na=rtpmap:96 opus/48000/2\r\n" + videoSection)
	}

	// The caller prefers H.264; the presumed (browsers') order selects
	// VP8 — at the offer's PT.
	sel := selectVideoFromOffer(offer("m=video 6000 RTP/AVP 120 121\r\na=rtpmap:120 VP8/90000\r\na=rtpmap:121 H264/90000\r\n"), presumed)
	if !sel.offered || !sel.ok || sel.codec.Name != "VP8" || sel.codec.PayloadType != 120 {
		t.Fatalf("got %+v, want VP8 at the offer's 120", sel)
	}
	if sel.raddr == nil || sel.raddr.Port != 6000 || !sel.raddr.IP.Equal(net.ParseIP("192.0.2.3")) {
		t.Fatalf("the video address is %v, want 192.0.2.3:6000", sel.raddr)
	}

	// The H.264 selection adopts the offer's fmtp for the echo.
	sel = selectVideoFromOffer(offer("m=video 6000 RTP/AVP 121\r\na=rtpmap:121 H264/90000\r\na=fmtp:121 profile-level-id=640028\r\n"), presumed)
	if !sel.ok || sel.codec.Name != "H264" || sel.codec.Fmtp != "profile-level-id=640028" {
		t.Fatalf("got %+v, want H264 with the offer's fmtp", sel)
	}

	// Nothing in common: the rejection still knows the offered formats.
	sel = selectVideoFromOffer(offer("m=video 6000 RTP/AVP 31 32\r\na=rtpmap:31 MP4V-ES/90000\r\n"), presumed)
	if !sel.offered || sel.ok || strings.Join(sel.formats, " ") != "31 32" {
		t.Fatalf("got %+v, want offered-but-unmatched with formats [31 32]", sel)
	}

	// A disabled video section: offered, never accepted.
	sel = selectVideoFromOffer(offer("m=video 0 RTP/AVP 120\r\n"), presumed)
	if !sel.offered || sel.ok {
		t.Fatalf("got %+v, want offered-but-disabled", sel)
	}

	// No video m-line at all: not offered.
	if sel := selectVideoFromOffer(offer(""), presumed); sel.offered || sel.ok {
		t.Fatalf("no video m-line: got %+v, want not offered", sel)
	}
}

// sdpMediaPort reads one m-line's port off a generated body.
func sdpMediaPort(t *testing.T, body, mediaType string) int {
	t.Helper()
	for line := range strings.Lines(body) {
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "m="+mediaType+" ") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				t.Fatalf("the %s m-line is malformed: %q", mediaType, line)
			}
			port, err := strconv.Atoi(fields[1])
			if err != nil {
				t.Fatalf("the %s m-line's port does not parse: %v", mediaType, err)
			}
			return port
		}
	}
	t.Fatalf("the body carries no %s m-line:\n%s", mediaType, body)
	return 0
}
