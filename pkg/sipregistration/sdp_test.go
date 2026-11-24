package sipregistration

import (
	"errors"
	"fmt"
	"net"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

func testSDP(media ...string) []byte {
	head := "v=0\r\n" +
		"o=- 1234 1 IN IP4 192.0.2.10\r\n" +
		"s=-\r\n" +
		"c=IN IP4 192.0.2.10\r\n" +
		"t=0 0\r\n"
	return []byte(head + strings.Join(media, "\r\n") + "\r\n")
}

func TestSDPParseDynamicCodecsAndMediaOverrides(t *testing.T) {
	model, err := parseSDP(testSDP(
		"a=sendonly",
		"m=audio 4000 RTP/AVP 111 0 101 13 120",
		"c=IN IP4 198.51.100.20",
		"a=recvonly",
		"a=rtpmap:111 opus/48000/2",
		"a=fmtp:111 minptime=10;useinbandfec=1;stereo=1",
		"a=rtpmap:101 telephone-event/8000",
		"a=fmtp:101 0-16",
		"a=rtpmap:13 CN/8000",
		"a=rtpmap:120 X-Experimental/16000/2",
		"a=fmtp:120 mode=compact",
		"m=video 4002 RTP/AVP 96 97 98 99 100 108",
		"a=rtpmap:96 H264/90000",
		"a=fmtp:96 packetization-mode=1;profile-level-id=42e01f",
		"a=rtpmap:97 rtx/90000",
		"a=fmtp:97 apt=96",
		"a=rtpmap:98 red/90000",
		"a=rtpmap:99 ulpfec/90000",
		"a=rtpmap:100 VP9/90000",
		"a=rtpmap:108 CN/90000",
	))
	if err != nil {
		t.Fatal(err)
	}
	if model.Audio == nil || model.Video == nil {
		t.Fatalf("missing media sections: %+v", model)
	}
	if model.Audio.Port != 4000 || model.Audio.Protocol != "RTP/AVP" || model.Audio.Direction != "recvonly" {
		t.Fatalf("audio section = %+v", model.Audio)
	}
	if !model.Audio.ConnectionIP.Equal(net.ParseIP("198.51.100.20")) {
		t.Fatalf("audio connection = %v", model.Audio.ConnectionIP)
	}
	wantAudio := []rtpCodec{
		{PayloadType: 111, Name: "opus", ClockRate: 48000, Channels: 2, Fmtp: "minptime=10;useinbandfec=1;stereo=1"},
		{PayloadType: 0, Name: "PCMU", ClockRate: 8000, Channels: 1},
		{PayloadType: 120, Name: "X-Experimental", ClockRate: 16000, Channels: 2, Fmtp: "mode=compact"},
	}
	if !reflect.DeepEqual(model.Audio.Codecs, wantAudio) {
		t.Fatalf("audio codecs = %#v, want %#v", model.Audio.Codecs, wantAudio)
	}
	wantDTMF := []rtpCodec{{PayloadType: 101, Name: "telephone-event", ClockRate: 8000, Channels: 1, Fmtp: "0-16"}}
	if !reflect.DeepEqual(model.Audio.TelephoneEvents, wantDTMF) {
		t.Fatalf("audio telephone-events = %#v, want %#v", model.Audio.TelephoneEvents, wantDTMF)
	}
	if model.Video.Direction != "sendonly" || !model.Video.ConnectionIP.Equal(net.ParseIP("192.0.2.10")) {
		t.Fatalf("video inheritance = %+v", model.Video)
	}
	wantVideo := []rtpCodec{
		{PayloadType: 96, Name: "H264", ClockRate: 90000, Fmtp: "packetization-mode=1;profile-level-id=42e01f"},
		{PayloadType: 100, Name: "VP9", ClockRate: 90000},
	}
	if !reflect.DeepEqual(model.Video.Codecs, wantVideo) {
		t.Fatalf("video codecs = %#v, want %#v", model.Video.Codecs, wantVideo)
	}
}

func TestSDPParseStaticPayloadsAndRejectedVersusAbsentVideo(t *testing.T) {
	model, err := parseSDP(testSDP(
		"m=audio 4000 RTP/AVP 0 8 9 13 110",
		"m=video 0 RTP/AVP 96 97",
		"a=rtpmap:96 VP8/90000",
		"a=rtpmap:97 rtx/90000",
		"a=fmtp:97 apt=96",
	))
	if err != nil {
		t.Fatal(err)
	}
	wantAudio := []rtpCodec{
		{PayloadType: 0, Name: "PCMU", ClockRate: 8000, Channels: 1},
		{PayloadType: 8, Name: "PCMA", ClockRate: 8000, Channels: 1},
		{PayloadType: 9, Name: "G722", ClockRate: 8000, Channels: 1},
	}
	if !reflect.DeepEqual(model.Audio.Codecs, wantAudio) {
		t.Fatalf("static audio = %#v, want %#v", model.Audio.Codecs, wantAudio)
	}
	if model.Video == nil || model.Video.Port != 0 {
		t.Fatalf("rejected video = %+v, want a present port-0 section", model.Video)
	}
	if !reflect.DeepEqual(model.Video.Formats, []string{"96", "97"}) {
		t.Fatalf("rejected video formats = %v", model.Video.Formats)
	}

	model, err = parseSDP(testSDP("m=audio 4000 RTP/AVP 0"))
	if err != nil {
		t.Fatal(err)
	}
	if model.Video != nil {
		t.Fatalf("absent video = %+v, want nil", model.Video)
	}
}

func TestSDPParseAcceptsSDESSRTP(t *testing.T) {
	model, err := parseSDP(testSDP(
		"m=audio 4000 RTP/SAVP 0 101",
		"a=crypto:1 AES_CM_128_HMAC_SHA1_80 inline:8Dlz/SyzlAKCZwH49w5DX8S4pDa7Lw0n3LTI4t6Z",
		"a=rtpmap:101 telephone-event/8000",
	))
	if err != nil {
		t.Fatal(err)
	}
	if model.Audio == nil || model.Audio.Protocol != "RTP/SAVP" {
		t.Fatalf("parsed SRTP audio = %+v", model.Audio)
	}
}

func TestSDPParseSignalWireAudioProfileAlternatives(t *testing.T) {
	media := []string{
		"m=audio 15348 RTP/SAVP 102 9 0 8 18 103 101",
		"a=rtpmap:102 opus/48000/2",
		"a=fmtp:102 useinbandfec=1; maxaveragebitrate=30000; maxplaybackrate=48000; ptime=20; minptime=10; maxptime=40",
		"a=rtpmap:9 G722/8000",
		"a=rtpmap:0 PCMU/8000",
		"a=rtpmap:8 PCMA/8000",
		"a=rtpmap:18 G729/8000",
		"a=fmtp:18 annexb=no",
		"a=rtpmap:103 telephone-event/48000",
		"a=fmtp:103 0-15",
		"a=rtpmap:101 telephone-event/8000",
		"a=fmtp:101 0-15",
		"a=crypto:7 AES_CM_128_HMAC_SHA1_80 inline:8Dlz/SyzlAKCZwH49w5DX8S4pDa7Lw0n3LTI4t6Z",
		"a=ptime:20",
	}
	alternative := append([]string(nil), media...)
	alternative[0] = "m=audio 15348 RTP/AVP 102 9 0 8 18 103 101"
	alternative = append(alternative[:len(alternative)-2], alternative[len(alternative)-1])
	model, err := parseSDP(testSDP(append(media, alternative...)...))
	if err != nil {
		t.Fatal(err)
	}
	if model.Audio == nil || model.Audio.Protocol != "RTP/SAVP" || len(model.AudioAlternatives) != 1 ||
		model.AudioAlternatives[0].Protocol != "RTP/AVP" {
		t.Fatalf("SignalWire alternatives = %+v, %+v", model.Audio, model.AudioAlternatives)
	}

	independent := append(append([]string(nil), media...), "m=audio 15350 RTP/AVP 0")
	if _, err := parseSDP(testSDP(independent...)); err == nil {
		t.Fatal("independent second audio stream was accepted as a profile alternative")
	}
}

func TestSDPParseRejectsNonPlainRTPProfiles(t *testing.T) {
	for _, protocol := range []string{"RTP/SAVPF", "UDP/TLS/RTP/SAVPF", "TCP/RTP/AVP", "raw"} {
		t.Run(protocol, func(t *testing.T) {
			if _, err := parseSDP(testSDP("m=audio 4000 " + protocol + " 0")); err == nil {
				t.Fatalf("parse protocol %q succeeded, want rejection", protocol)
			}
		})
	}

	// Non-media sections, such as a WebRTC data channel, do not change the
	// plaintext audio/video capability parser's profile restriction.
	if _, err := parseSDP(testSDP(
		"m=audio 4000 RTP/AVP 0",
		"m=application 9 UDP/DTLS/SCTP webrtc-datachannel",
	)); err != nil {
		t.Fatalf("unrelated application section rejected: %v", err)
	}
}

func TestSDPParseBoundsPrimaryCodecCount(t *testing.T) {
	lines := make([]string, 0, maxSDPCodecsPerMedia+2)
	formats := make([]string, 0, maxSDPCodecsPerMedia+1)
	for index := 0; index <= maxSDPCodecsPerMedia; index++ {
		// Static-range numbers with explicit rtpmap attributes let the fixture
		// exceed the primary-codec bound without exceeding RTP's 0..127 range.
		payloadType := 20 + index
		formats = append(formats, strconv.Itoa(payloadType))
		lines = append(lines, fmt.Sprintf("a=rtpmap:%d X-BOUND/8000", payloadType))
	}
	media := append([]string{"m=audio 4000 RTP/AVP " + strings.Join(formats, " ")}, lines...)
	if _, err := parseSDP(testSDP(media...)); err == nil || !strings.Contains(err.Error(), "too many primary codecs") {
		t.Fatalf("codec bound error = %v", err)
	}
}

func TestIntersectCodecsUsesActualBrowserListAndPreservesBothSides(t *testing.T) {
	browser := []rtpCodec{
		{PayloadType: 120, Name: "X-One", ClockRate: 16000, Channels: 2, Fmtp: "mode=Compact"},
		{PayloadType: 121, Name: "X-Two", ClockRate: 32000},
		{PayloadType: 126, Name: "telephone-event", ClockRate: 8000},
	}
	sip := []rtpCodec{
		{PayloadType: 98, Name: "x-two", ClockRate: 32000},
		{PayloadType: 97, Name: "X-ONE", ClockRate: 16000, Channels: 2, Fmtp: "mode=compact;extra=yes"},
		{PayloadType: 101, Name: "telephone-event", ClockRate: 8000},
	}
	pairs := intersectCodecs(browser, sip)
	want := []codecPair{{Browser: browser[0], SIP: sip[1]}, {Browser: browser[1], SIP: sip[0]}}
	if !reflect.DeepEqual(pairs, want) {
		t.Fatalf("pairs = %#v, want %#v", pairs, want)
	}
}

func TestIntersectCodecsOpusChannelsAndFMTP(t *testing.T) {
	browser := rtpCodec{
		PayloadType: 111,
		Name:        "opus",
		ClockRate:   48000,
		Channels:    2,
		Fmtp:        "minptime=10;useinbandfec=1;stereo=1;sprop-stereo=1",
	}
	for _, test := range []struct {
		name string
		sip  rtpCodec
		want int
	}{
		{
			name: "compatible actual PT and parameter order",
			sip:  rtpCodec{PayloadType: 96, Name: "OPUS", ClockRate: 48000, Channels: 2, Fmtp: "sprop-stereo=1;stereo=1;useinbandfec=1;minptime=20"},
			want: 1,
		},
		{
			name: "channel mismatch",
			sip:  rtpCodec{PayloadType: 96, Name: "opus", ClockRate: 48000, Channels: 1, Fmtp: browser.Fmtp},
		},
		{
			name: "stereo mismatch",
			sip:  rtpCodec{PayloadType: 96, Name: "opus", ClockRate: 48000, Channels: 2, Fmtp: "useinbandfec=1;stereo=0;sprop-stereo=1"},
		},
		{
			name: "sprop stereo mismatch",
			sip:  rtpCodec{PayloadType: 96, Name: "opus", ClockRate: 48000, Channels: 2, Fmtp: "useinbandfec=1;stereo=1;sprop-stereo=0"},
		},
		{
			name: "inband FEC mismatch",
			sip:  rtpCodec{PayloadType: 96, Name: "opus", ClockRate: 48000, Channels: 2, Fmtp: "useinbandfec=0;stereo=1;sprop-stereo=1"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := len(intersectCodecs([]rtpCodec{browser}, []rtpCodec{test.sip})); got != test.want {
				t.Fatalf("matches = %d, want %d", got, test.want)
			}
		})
	}

	zeroDefaults := rtpCodec{PayloadType: 96, Name: "opus", ClockRate: 48000, Channels: 2, Fmtp: "minptime=20;useinbandfec=0;stereo=0;sprop-stereo=0"}
	if pairs := intersectCodecs([]rtpCodec{{PayloadType: 111, Name: "opus", ClockRate: 48000, Channels: 2, Fmtp: "minptime=10"}}, []rtpCodec{zeroDefaults}); len(pairs) != 1 {
		t.Fatal("omitted Opus booleans did not match their zero defaults")
	}
}

func TestIntersectCodecsH264FMTP(t *testing.T) {
	browser := rtpCodec{PayloadType: 102, Name: "H264", ClockRate: 90000, Fmtp: "profile-level-id=42E01F;packetization-mode=1"}
	levelAsymmetry := rtpCodec{PayloadType: 97, Name: "h264", ClockRate: 90000, Fmtp: "level-asymmetry-allowed=1; packetization-mode=01; profile-level-id=42e01f"}
	pairs := intersectCodecs([]rtpCodec{browser}, []rtpCodec{levelAsymmetry})
	if len(pairs) != 1 || pairs[0].Browser.PayloadType != 102 || pairs[0].SIP.PayloadType != 97 {
		t.Fatalf("level-asymmetry-compatible pair = %#v", pairs)
	}

	for _, test := range []struct {
		name string
		fmtp string
	}{
		{name: "packetization mismatch", fmtp: "packetization-mode=0;profile-level-id=42e01f"},
		{name: "profile mismatch", fmtp: "packetization-mode=1;profile-level-id=640c1f"},
		{name: "level remains exact", fmtp: "packetization-mode=1;profile-level-id=42e020;level-asymmetry-allowed=1"},
		{name: "missing profile mismatch", fmtp: "packetization-mode=1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			sip := rtpCodec{PayloadType: 97, Name: "H264", ClockRate: 90000, Fmtp: test.fmtp}
			if pairs := intersectCodecs([]rtpCodec{browser}, []rtpCodec{sip}); len(pairs) != 0 {
				t.Fatalf("incompatible H264 matched: %#v", pairs)
			}
		})
	}

	defaultPacketization := rtpCodec{PayloadType: 103, Name: "H264", ClockRate: 90000, Fmtp: "profile-level-id=42e01f"}
	explicitZero := rtpCodec{PayloadType: 97, Name: "H264", ClockRate: 90000, Fmtp: "packetization-mode=0;profile-level-id=42E01F"}
	if pairs := intersectCodecs([]rtpCodec{defaultPacketization}, []rtpCodec{explicitZero}); len(pairs) != 1 {
		t.Fatal("omitted packetization-mode did not match its zero default")
	}
}

func TestNegotiatedSelectionRequiresAudioAndMakesVideoOptional(t *testing.T) {
	browserAudio := rtpCodec{PayloadType: 120, Name: "X-Audio", ClockRate: 16000, Channels: 1}
	browser := sdpModel{
		Audio: &mediaSection{Kind: "audio", Port: 5000, Codecs: []rtpCodec{browserAudio}},
		Video: &mediaSection{Kind: "video", Port: 5000, Codecs: []rtpCodec{{PayloadType: 102, Name: "H264", ClockRate: 90000}}},
	}
	sip := sdpModel{
		Audio: &mediaSection{Kind: "audio", Port: 4000, Codecs: []rtpCodec{{PayloadType: 96, Name: "different", ClockRate: 16000, Channels: 1}}},
		Video: &mediaSection{Kind: "video", Port: 4000, Formats: []string{"96", "97", "98"}},
	}
	if _, err := negotiateInboundOffer(browser, sip); !errors.Is(err, errNoCommonAudio) {
		t.Fatalf("no-common-audio error = %v", err)
	}

	sipAudio := rtpCodec{PayloadType: 97, Name: "x-audio", ClockRate: 16000, Channels: 1, Fmtp: "actual=sip"}
	sip.Audio.Codecs = []rtpCodec{sipAudio}
	selection, err := negotiateInboundOffer(browser, sip)
	if err != nil {
		t.Fatal(err)
	}
	if len(selection.Audio) != 1 || len(selection.Video) != 0 {
		t.Fatalf("selection = %+v", selection)
	}
	if !reflect.DeepEqual(selection.RejectVideoFormats, []string{"96", "97", "98"}) {
		t.Fatalf("video rejection = %v", selection.RejectVideoFormats)
	}
	audio, video := selection.sipCodecs()
	if !reflect.DeepEqual(audio, []rtpCodec{sipAudio}) || len(video) != 0 {
		t.Fatalf("SIP answer codecs = %#v, %#v", audio, video)
	}
}

func TestNegotiateOutboundAnswerRetainsBrowserPreference(t *testing.T) {
	first := rtpCodec{PayloadType: 120, Name: "X-First", ClockRate: 8000, Channels: 1}
	second := rtpCodec{PayloadType: 121, Name: "X-Second", ClockRate: 8000, Channels: 1}
	browser := sdpModel{Audio: &mediaSection{Kind: "audio", Port: 5000, Codecs: []rtpCodec{first, second}}}
	sip := sdpModel{Audio: &mediaSection{Kind: "audio", Port: 4000, Codecs: []rtpCodec{
		{PayloadType: 97, Name: "x-second", ClockRate: 8000, Channels: 1},
		{PayloadType: 96, Name: "x-first", ClockRate: 8000, Channels: 1},
	}}}
	selection, err := negotiateOutboundAnswer(browser, sip)
	if err != nil {
		t.Fatal(err)
	}
	if len(selection.Audio) != 2 || selection.Audio[0].Browser != first || selection.Audio[1].Browser != second {
		t.Fatalf("outbound selection order = %+v", selection.Audio)
	}
	audio, _ := selection.browserCodecs()
	if !reflect.DeepEqual(audio, []rtpCodec{first, second}) {
		t.Fatalf("browser answer codecs = %#v", audio)
	}
}

func TestRTPCodecConversionsDoNotRestrictVocabulary(t *testing.T) {
	codec := rtpCodec{PayloadType: 120, Name: "X-Custom", ClockRate: 16000, Fmtp: "mode=one"}
	diago := codec.diagoCodec("audio")
	if diago.Name != codec.Name || diago.PayloadType != 120 || diago.SampleRate != 16000 || diago.NumChannels != 1 || diago.Fmtp != codec.Fmtp || diago.SampleDur != 20*time.Millisecond {
		t.Fatalf("diago conversion = %+v", diago)
	}
	capability := codec.webRTCCapability("audio")
	if capability.MimeType != "audio/X-Custom" || capability.ClockRate != 16000 || capability.Channels != 1 || capability.SDPFmtpLine != codec.Fmtp {
		t.Fatalf("WebRTC conversion = %+v", capability)
	}
	roundTrip := rtpCodecFromWebRTC(webrtc.RTPCodecParameters{RTPCodecCapability: capability, PayloadType: 120})
	if roundTrip.Name != codec.Name || roundTrip.PayloadType != codec.PayloadType || roundTrip.ClockRate != codec.ClockRate || roundTrip.Channels != 1 || roundTrip.Fmtp != codec.Fmtp {
		t.Fatalf("WebRTC to rtpCodec = %+v", roundTrip)
	}
}

func TestGenerateSIPSDPRoundTripWithExplicitTelephoneEvent(t *testing.T) {
	audio := []rtpCodec{
		{PayloadType: 111, Name: "opus", ClockRate: 48000, Channels: 2, Fmtp: "useinbandfec=1;stereo=1"},
		{PayloadType: 0, Name: "PCMU", ClockRate: 8000, Channels: 1},
		{PayloadType: 120, Name: "X-Experimental", ClockRate: 16000, Channels: 2, Fmtp: "mode=compact"},
		{PayloadType: 126, Name: "telephone-event", ClockRate: 8000, Channels: 1, Fmtp: "0-16"},
	}
	video := []rtpCodec{
		{PayloadType: 102, Name: "H264", ClockRate: 90000, Fmtp: "packetization-mode=1;profile-level-id=42e01f"},
		{PayloadType: 121, Name: "VP8", ClockRate: 90000},
		{PayloadType: 122, Name: "rtx", ClockRate: 90000, Fmtp: "apt=102"},
	}
	raw := generateSIPSDP(net.ParseIP("198.51.100.30"), 4100, audio, video, nil)
	text := string(raw)
	for _, forbidden := range []string{"rtx", "comfort-noise", "CN/"} {
		if strings.Contains(strings.ToLower(text), strings.ToLower(forbidden)) {
			t.Fatalf("generated SDP contains helper %q:\n%s", forbidden, text)
		}
	}

	model, err := parseSDP(raw)
	if err != nil {
		t.Fatalf("generated SDP does not parse: %v\n%s", err, text)
	}
	if model.Audio.Port != 4100 || model.Video.Port != 4100 || !model.Audio.ConnectionIP.Equal(net.ParseIP("198.51.100.30")) {
		t.Fatalf("generated media addresses = %+v %+v", model.Audio, model.Video)
	}
	if model.Audio.Direction != "sendrecv" || model.Video.Direction != "sendrecv" {
		t.Fatalf("outbound offer directions = %q, %q", model.Audio.Direction, model.Video.Direction)
	}
	if !reflect.DeepEqual(model.Audio.Codecs, audio[:3]) {
		t.Fatalf("round-trip audio = %#v, want %#v", model.Audio.Codecs, audio[:3])
	}
	if !reflect.DeepEqual(model.Audio.TelephoneEvents, audio[3:]) || strings.Count(text, "telephone-event") != 1 {
		t.Fatalf("round-trip telephone-event = %#v, want %#v\n%s", model.Audio.TelephoneEvents, audio[3:], text)
	}
	if !reflect.DeepEqual(model.Video.Codecs, video[:2]) {
		t.Fatalf("round-trip video = %#v, want %#v", model.Video.Codecs, video[:2])
	}
}

func TestGenerateSIPSDPWithDirectionsMapsInboundAnswers(t *testing.T) {
	audio := []rtpCodec{{PayloadType: 0, Name: "PCMU", ClockRate: 8000, Channels: 1}}
	video := []rtpCodec{{PayloadType: 96, Name: "VP8", ClockRate: 90000}}
	for _, test := range []struct {
		offerDirection string
		want           string
	}{
		{offerDirection: "sendonly", want: "recvonly"},
		{offerDirection: "recvonly", want: "sendonly"},
		{offerDirection: "sendrecv", want: "sendrecv"},
		{offerDirection: "inactive", want: "inactive"},
	} {
		t.Run(test.offerDirection, func(t *testing.T) {
			raw := generateSIPSDPWithDirections(
				net.ParseIP("192.0.2.20"),
				4100,
				audio,
				video,
				nil,
				sipAnswerDirection(test.offerDirection),
				sipAnswerDirection(test.offerDirection),
			)
			model, err := parseSDP(raw)
			if err != nil {
				t.Fatal(err)
			}
			if model.Audio.Direction != test.want || model.Video.Direction != test.want {
				t.Fatalf("answer directions = %q, %q; want %q\n%s", model.Audio.Direction, model.Video.Direction, test.want, raw)
			}
			if model.Audio.Port != 4100 || model.Video.Port != 4100 {
				t.Fatalf("active answer ports = %d, %d", model.Audio.Port, model.Video.Port)
			}
		})
	}
}

func TestGenerateSIPSDPRejectsOrOmitsVideoWithoutAddingCodecs(t *testing.T) {
	audio := []rtpCodec{{PayloadType: 120, Name: "X-Audio", ClockRate: 8000, Channels: 1}}
	raw := generateSIPSDP(net.ParseIP("192.0.2.20"), 4100, audio, nil, []string{"96", "97", "98"})
	if !strings.Contains(string(raw), "m=video 0 RTP/AVP 96 97 98\r\n") {
		t.Fatalf("missing offered-format video rejection:\n%s", raw)
	}
	model, err := parseSDP(raw)
	if err != nil {
		t.Fatal(err)
	}
	if model.Video == nil || model.Video.Port != 0 || model.Video.Direction != "inactive" || len(model.Video.Codecs) != 0 {
		t.Fatalf("rejected video = %+v", model.Video)
	}

	raw = generateSIPSDP(net.ParseIP("192.0.2.20"), 4100, audio, nil, nil)
	model, err = parseSDP(raw)
	if err != nil {
		t.Fatal(err)
	}
	if model.Video != nil || strings.Contains(string(raw), "m=video") {
		t.Fatalf("audio-only SDP unexpectedly has video: %+v\n%s", model.Video, raw)
	}
	if strings.Count(string(raw), "m=audio") != 1 || len(model.Audio.Codecs) != 1 || model.Audio.Codecs[0] != audio[0] {
		t.Fatalf("audio-only generation changed codecs: %+v\n%s", model.Audio, raw)
	}
}

func realisticBrowserOffer() []byte {
	lines := []string{
		"v=0",
		"o=- 4611731400430051336 2 IN IP4 127.0.0.1",
		"s=-",
		"t=0 0",
		"a=group:BUNDLE 0 1",
		"a=extmap-allow-mixed",
		"a=msid-semantic: WMS browser-stream",
		"m=audio 9 UDP/TLS/RTP/SAVPF 111 0 101 106",
		"c=IN IP4 0.0.0.0",
		"a=rtcp:9 IN IP4 0.0.0.0",
		"a=ice-ufrag:abcd",
		"a=ice-pwd:abcdefghijklmnopqrstuvwxyz",
		"a=ice-options:trickle",
		"a=fingerprint:sha-256 00:01:02:03:04:05:06:07:08:09:0A:0B:0C:0D:0E:0F:10:11:12:13:14:15:16:17:18:19:1A:1B:1C:1D:1E:1F",
		"a=setup:actpass",
		"a=mid:0",
		"a=extmap:1 urn:ietf:params:rtp-hdrext:ssrc-audio-level",
		"a=sendrecv",
		"a=rtcp-mux",
		"a=rtcp-rsize",
		"a=rtpmap:111 opus/48000/2",
		"a=rtcp-fb:111 transport-cc",
		"a=fmtp:111 minptime=10;useinbandfec=1",
		"a=rtpmap:0 PCMU/8000",
		"a=rtpmap:101 telephone-event/8000",
		"a=fmtp:101 0-16",
		"a=rtpmap:106 red/48000/2",
		"a=ssrc:1000 cname:browser-audio",
		"m=video 9 UDP/TLS/RTP/SAVPF 96 97 102 98 99",
		"c=IN IP4 0.0.0.0",
		"a=rtcp:9 IN IP4 0.0.0.0",
		"a=ice-ufrag:abcd",
		"a=ice-pwd:abcdefghijklmnopqrstuvwxyz",
		"a=setup:actpass",
		"a=mid:1",
		"a=extmap:2 urn:ietf:params:rtp-hdrext:toffset",
		"a=sendrecv",
		"a=rtcp-mux",
		"a=rtpmap:96 VP8/90000",
		"a=rtcp-fb:96 goog-remb",
		"a=rtpmap:97 rtx/90000",
		"a=fmtp:97 apt=96",
		"a=rtpmap:102 H264/90000",
		"a=fmtp:102 level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
		"a=rtpmap:98 rtx/90000",
		"a=fmtp:98 apt=102",
		"a=rtpmap:99 red/90000",
		"a=ssrc:2000 cname:browser-video",
	}
	return []byte(strings.Join(lines, "\r\n") + "\r\n")
}

func TestParseBrowserSDPAcceptsWebRTCProfileAndIgnoresTransportAttributes(t *testing.T) {
	raw := realisticBrowserOffer()
	model, err := parseBrowserSDP(raw)
	if err != nil {
		t.Fatal(err)
	}
	if model.Audio == nil || model.Audio.Protocol != "UDP/TLS/RTP/SAVPF" || model.Audio.Port != 9 {
		t.Fatalf("browser audio = %+v", model.Audio)
	}
	wantAudio := []rtpCodec{
		{PayloadType: 111, Name: "opus", ClockRate: 48000, Channels: 2, Fmtp: "minptime=10;useinbandfec=1"},
		{PayloadType: 0, Name: "PCMU", ClockRate: 8000, Channels: 1},
	}
	if !reflect.DeepEqual(model.Audio.Codecs, wantAudio) {
		t.Fatalf("browser audio codecs = %#v, want %#v", model.Audio.Codecs, wantAudio)
	}
	wantDTMF := []rtpCodec{{PayloadType: 101, Name: "telephone-event", ClockRate: 8000, Channels: 1, Fmtp: "0-16"}}
	if !reflect.DeepEqual(model.Audio.TelephoneEvents, wantDTMF) {
		t.Fatalf("browser telephone-events = %#v, want %#v", model.Audio.TelephoneEvents, wantDTMF)
	}
	wantVideo := []rtpCodec{
		{PayloadType: 96, Name: "VP8", ClockRate: 90000},
		{PayloadType: 102, Name: "H264", ClockRate: 90000, Fmtp: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"},
	}
	if model.Video == nil || !reflect.DeepEqual(model.Video.Codecs, wantVideo) {
		t.Fatalf("browser video = %+v, want codecs %#v", model.Video, wantVideo)
	}
	if _, err := parseSDP(raw); err == nil {
		t.Fatal("SIP plaintext parser accepted a secure WebRTC profile")
	}
}

func TestNegotiateRawSelectionsPinBrowserPreferenceAndSIPAddresses(t *testing.T) {
	sip := testSDP(
		"m=audio 4000 RTP/AVP 0 96 101",
		"c=IN IP4 198.51.100.10",
		"a=rtpmap:96 opus/48000/2",
		"a=fmtp:96 useinbandfec=1;minptime=10",
		"a=rtpmap:101 telephone-event/8000",
		"m=video 4002 RTP/AVP 97 98 99",
		"c=IN IP4 198.51.100.20",
		"a=rtpmap:97 H264/90000",
		"a=fmtp:97 packetization-mode=1;profile-level-id=42e01f;level-asymmetry-allowed=1",
		"a=rtpmap:98 VP8/90000",
		"a=rtpmap:99 rtx/90000",
		"a=fmtp:99 apt=98",
	)
	for _, test := range []struct {
		name      string
		negotiate func([]byte, []byte) (negotiatedSelection, error)
	}{
		{name: "outbound answer", negotiate: negotiateOutboundSelection},
		{name: "inbound offer", negotiate: negotiateInboundSelection},
	} {
		t.Run(test.name, func(t *testing.T) {
			selection, err := test.negotiate(realisticBrowserOffer(), sip)
			if err != nil {
				t.Fatal(err)
			}
			if len(selection.Audio) != 1 || len(selection.Video) != 1 {
				t.Fatalf("selection sizes = %d audio, %d video", len(selection.Audio), len(selection.Video))
			}
			audio := selection.Audio[0]
			if audio.Browser.PayloadType != 111 || audio.SIP.PayloadType != 96 {
				t.Fatalf("audio pair = %+v", audio)
			}
			if selection.DTMF == nil || selection.DTMF.Browser.PayloadType != 101 || selection.DTMF.SIP.PayloadType != 101 {
				t.Fatalf("DTMF pair = %+v", selection.DTMF)
			}
			video := selection.Video[0]
			if video.Browser.PayloadType != 96 || video.Browser.Name != "VP8" || video.SIP.PayloadType != 98 {
				t.Fatalf("video pair = %+v", video)
			}
			if selection.SIPAudio == nil || selection.SIPAudio.Port != 4000 || !selection.SIPAudio.ConnectionIP.Equal(net.ParseIP("198.51.100.10")) {
				t.Fatalf("SIP audio address = %+v", selection.SIPAudio)
			}
			if selection.SIPVideo == nil || selection.SIPVideo.Port != 4002 || !selection.SIPVideo.ConnectionIP.Equal(net.ParseIP("198.51.100.20")) {
				t.Fatalf("SIP video address = %+v", selection.SIPVideo)
			}
		})
	}
}

func TestNegotiateTelephoneEventUsesSeparateDynamicPTAndAllowsAbsence(t *testing.T) {
	browserAudio := rtpCodec{PayloadType: 111, Name: "opus", ClockRate: 48000, Channels: 2}
	sipAudio := rtpCodec{PayloadType: 96, Name: "opus", ClockRate: 48000, Channels: 2}
	browser := sdpModel{Audio: &mediaSection{
		Kind: "audio", Port: 9, Codecs: []rtpCodec{browserAudio},
		TelephoneEvents: []rtpCodec{
			{PayloadType: 110, Name: "telephone-event", ClockRate: 48000, Channels: 1},
			{PayloadType: 126, Name: "telephone-event", ClockRate: 8000, Channels: 1, Fmtp: "0-16"},
		},
	}}
	sip := sdpModel{Audio: &mediaSection{
		Kind: "audio", Port: 4000, Codecs: []rtpCodec{sipAudio},
		TelephoneEvents: []rtpCodec{
			{PayloadType: 110, Name: "telephone-event", ClockRate: 48000, Channels: 1},
			{PayloadType: 120, Name: "telephone-event", ClockRate: 8000, Channels: 1, Fmtp: "0-15"},
		},
	}}
	selection, err := negotiateSDPModels(browser, sip)
	if err != nil {
		t.Fatal(err)
	}
	if selection.DTMF == nil || selection.DTMF.Browser.PayloadType != 126 || selection.DTMF.SIP.PayloadType != 120 {
		t.Fatalf("dynamic DTMF pair = %+v", selection.DTMF)
	}

	sip.Audio.TelephoneEvents = nil
	selection, err = negotiateSDPModels(browser, sip)
	if err != nil {
		t.Fatal(err)
	}
	if selection.DTMF != nil {
		t.Fatalf("DTMF negotiated without a SIP capability: %+v", selection.DTMF)
	}

	browser.Audio.TelephoneEvents = browser.Audio.TelephoneEvents[:1]
	sip.Audio.TelephoneEvents = []rtpCodec{{PayloadType: 110, Name: "telephone-event", ClockRate: 48000, Channels: 1}}
	selection, err = negotiateSDPModels(browser, sip)
	if err != nil {
		t.Fatal(err)
	}
	if selection.DTMF != nil {
		t.Fatalf("non-8kHz DTMF negotiated without sip-bot-compatible generation: %+v", selection.DTMF)
	}
}

func TestNegotiateRawSelectionsNoCommonAudio(t *testing.T) {
	browser := testSDP(
		"m=audio 9 UDP/TLS/RTP/SAVPF 111",
		"a=rtpmap:111 opus/48000/2",
	)
	sip := testSDP("m=audio 4000 RTP/AVP 0")
	for name, negotiate := range map[string]func([]byte, []byte) (negotiatedSelection, error){
		"outbound": negotiateOutboundSelection,
		"inbound":  negotiateInboundSelection,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := negotiate(browser, sip); !errors.Is(err, errNoCommonAudio) {
				t.Fatalf("no-common-audio error = %v", err)
			}
		})
	}
}
