package sipregistration

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/diago"
	"github.com/emiago/diago/media"
	diagosdp "github.com/emiago/diago/media/sdp"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

func testRelaySelection() negotiatedSelection {
	browserAudio := rtpCodec{PayloadType: 111, Name: "opus", ClockRate: 48000, Channels: 2, Fmtp: "minptime=10;useinbandfec=1"}
	sipAudio := rtpCodec{PayloadType: 96, Name: "opus", ClockRate: 48000, Channels: 2, Fmtp: "useinbandfec=1;minptime=10"}
	browserVideo := rtpCodec{PayloadType: 96, Name: "VP8", ClockRate: 90000}
	sipVideo := rtpCodec{PayloadType: 97, Name: "VP8", ClockRate: 90000}
	return negotiatedSelection{
		Audio: []codecPair{{Browser: browserAudio, SIP: sipAudio}},
		Video: []codecPair{{Browser: browserVideo, SIP: sipVideo}},
	}
}

func TestNewOutboundSIPMediaUsesActualBrowserCodecs(t *testing.T) {
	browser := sdpModel{
		Audio: &mediaSection{Kind: "audio", Port: 9, Codecs: []rtpCodec{
			{PayloadType: 111, Name: "opus", ClockRate: 48000, Channels: 2, Fmtp: "minptime=10;useinbandfec=1"},
			{PayloadType: 0, Name: "PCMU", ClockRate: 8000, Channels: 1},
		}, TelephoneEvents: []rtpCodec{
			{PayloadType: 110, Name: "telephone-event", ClockRate: 48000, Channels: 1},
			{PayloadType: 101, Name: "telephone-event", ClockRate: 8000, Channels: 1},
		}},
		Video: &mediaSection{Kind: "video", Port: 9, Codecs: []rtpCodec{
			{PayloadType: 96, Name: "VP8", ClockRate: 90000},
			{PayloadType: 97, Name: "rtx", ClockRate: 90000, Fmtp: "apt=96"},
		}},
	}
	session, err := newOutboundSIPMedia(net.ParseIP("127.0.0.1"), browser, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	model, err := parseSDP(session.LocalSDP())
	if err != nil {
		t.Fatal(err)
	}
	if model.Audio.Protocol != "RTP/AVP" || model.Video.Protocol != "RTP/AVP" {
		t.Fatalf("outbound profiles = %q, %q", model.Audio.Protocol, model.Video.Protocol)
	}
	if session.RTPNAT != media.RTPNATSymetric {
		t.Fatalf("SIP media RTPNAT = %d, want symmetric source learning", session.RTPNAT)
	}
	if model.Audio.Direction != "sendrecv" || model.Video.Direction != "sendrecv" {
		t.Fatalf("outbound offer directions = %q, %q", model.Audio.Direction, model.Video.Direction)
	}
	if !reflect.DeepEqual(model.Audio.Codecs, browser.Audio.Codecs) || !reflect.DeepEqual(model.Video.Codecs, browser.Video.Codecs[:1]) {
		t.Fatalf("outbound codecs = %#v, %#v", model.Audio.Codecs, model.Video.Codecs)
	}
	if !reflect.DeepEqual(model.Audio.TelephoneEvents, browser.Audio.TelephoneEvents[1:]) || strings.Contains(strings.ToLower(string(session.LocalSDP())), "rtx") {
		t.Fatalf("outbound DTMF/helper codecs are wrong:\n%s", session.LocalSDP())
	}
	if len(session.Codecs) != 4 || session.Codecs[0].Fmtp != browser.Audio.Codecs[0].Fmtp {
		t.Fatalf("diago codecs were not retained: %+v", session.Codecs)
	}
	if _, err := newOutboundSIPMedia(net.ParseIP("127.0.0.1"), sdpModel{}, false); !errors.Is(err, errNoCommonAudio) {
		t.Fatalf("missing browser audio error = %v", err)
	}
}

func TestNewOutboundSIPMediaOmitsDTMFOnVideoPayloadTypeCollision(t *testing.T) {
	browser := sdpModel{
		Audio: &mediaSection{
			Kind: "audio", Port: 9,
			Codecs:          []rtpCodec{{PayloadType: 0, Name: "PCMU", ClockRate: 8000, Channels: 1}},
			TelephoneEvents: []rtpCodec{{PayloadType: 101, Name: "telephone-event", ClockRate: 8000, Channels: 1}},
		},
		Video: &mediaSection{Kind: "video", Port: 9, Codecs: []rtpCodec{
			{PayloadType: 101, Name: "VP8", ClockRate: 90000},
		}},
	}
	session, err := newOutboundSIPMedia(net.ParseIP("127.0.0.1"), browser, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	offer, err := parseSDP(session.LocalSDP())
	if err != nil {
		t.Fatal(err)
	}
	if len(offer.Audio.TelephoneEvents) != 0 || len(offer.Video.Codecs) != 1 || offer.Video.Codecs[0].PayloadType != 101 {
		t.Fatalf("optional DTMF displaced video:\n%s", session.LocalSDP())
	}
}

func TestNewOutboundSIPMediaBoundsLargeChromeCapabilitiesForUDPMTU(t *testing.T) {
	raw := testSDP(
		"a=group:BUNDLE 0 1",
		"a=msid-semantic: WMS chrome-stream",
		"m=audio 9 UDP/TLS/RTP/SAVPF 111 101 0 13 8 9 18 106",
		"a=mid:0",
		"a=rtcp-mux",
		"a=rtpmap:111 opus/48000/2",
		"a=fmtp:111 minptime=10;useinbandfec=1",
		"a=rtcp-fb:111 transport-cc",
		"a=rtpmap:101 telephone-event/8000",
		"a=fmtp:101 0-16",
		"a=rtpmap:0 PCMU/8000",
		"a=rtpmap:13 CN/8000",
		"a=rtpmap:8 PCMA/8000",
		"a=rtpmap:9 G722/8000",
		"a=rtpmap:18 G729/8000",
		"a=rtpmap:106 red/48000/2",
		"m=video 9 UDP/TLS/RTP/SAVPF 96 97 102 103 104 105 106 107 108 109 110 111 112 113 124 125 126",
		"a=mid:1",
		"a=rtcp-mux",
		"a=extmap:2 urn:ietf:params:rtp-hdrext:toffset",
		"a=rtpmap:96 VP8/90000",
		"a=rtcp-fb:96 goog-remb",
		"a=rtcp-fb:96 transport-cc",
		"a=rtpmap:97 rtx/90000",
		"a=fmtp:97 apt=96",
		"a=rtpmap:102 H264/90000",
		"a=fmtp:102 level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
		"a=rtpmap:103 rtx/90000",
		"a=fmtp:103 apt=102",
		"a=rtpmap:104 VP9/90000",
		"a=fmtp:104 profile-id=0",
		"a=rtpmap:105 rtx/90000",
		"a=fmtp:105 apt=104",
		"a=rtpmap:106 AV1/90000",
		"a=fmtp:106 profile=0;level-idx=5;tier=0",
		"a=rtpmap:107 rtx/90000",
		"a=fmtp:107 apt=106",
		"a=rtpmap:108 H265/90000",
		"a=fmtp:108 profile-id=1;tier-flag=0;level-id=93",
		"a=rtpmap:109 rtx/90000",
		"a=fmtp:109 apt=108",
		"a=rtpmap:110 H264/90000",
		"a=fmtp:110 packetization-mode=0;profile-level-id=42e01f",
		"a=rtpmap:111 rtx/90000",
		"a=fmtp:111 apt=110",
		"a=rtpmap:112 H264/90000",
		"a=fmtp:112 packetization-mode=1;profile-level-id=640c1f",
		"a=rtpmap:113 rtx/90000",
		"a=fmtp:113 apt=112",
		"a=rtpmap:124 red/90000",
		"a=rtpmap:125 ulpfec/90000",
		"a=rtpmap:126 flexfec-03/90000",
	)
	browser, err := parseBrowserSDP(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(browser.Audio.Codecs) != 5 || len(browser.Video.Codecs) != 7 {
		t.Fatalf("Chrome fixture primary codecs = %d audio, %d video", len(browser.Audio.Codecs), len(browser.Video.Codecs))
	}

	session, err := newOutboundSIPMedia(net.ParseIP("127.0.0.1"), browser, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	localSDP := session.LocalSDP()
	if len(localSDP) >= 1200 {
		t.Fatalf("bounded SIP offer is still too large: %d bytes\n%s", len(localSDP), localSDP)
	}
	offer, err := parseSDP(localSDP)
	if err != nil {
		t.Fatal(err)
	}
	wantAudio := []rtpCodec{
		{PayloadType: 111, Name: "opus", ClockRate: 48000, Channels: 2, Fmtp: "minptime=10;useinbandfec=1"},
		{PayloadType: 0, Name: "PCMU", ClockRate: 8000, Channels: 1},
		{PayloadType: 8, Name: "PCMA", ClockRate: 8000, Channels: 1},
		{PayloadType: 9, Name: "G722", ClockRate: 8000, Channels: 1},
	}
	wantVideo := []rtpCodec{
		{PayloadType: 96, Name: "VP8", ClockRate: 90000},
		{PayloadType: 102, Name: "H264", ClockRate: 90000, Fmtp: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"},
	}
	if !reflect.DeepEqual(offer.Audio.Codecs, wantAudio) {
		t.Fatalf("bounded audio = %#v, want %#v", offer.Audio.Codecs, wantAudio)
	}
	if !reflect.DeepEqual(offer.Video.Codecs, wantVideo) {
		t.Fatalf("bounded video = %#v, want %#v", offer.Video.Codecs, wantVideo)
	}
	if len(session.Codecs) != maxSIPOfferAudioCodecs+maxSIPOfferVideoCodecs+1 {
		t.Fatalf("diago codec table was not bounded with DTMF: %+v", session.Codecs)
	}
	wantDTMF := []rtpCodec{{PayloadType: 101, Name: "telephone-event", ClockRate: 8000, Channels: 1, Fmtp: "0-16"}}
	if !reflect.DeepEqual(offer.Audio.TelephoneEvents, wantDTMF) {
		t.Fatalf("bounded offer DTMF = %#v, want %#v", offer.Audio.TelephoneEvents, wantDTMF)
	}
	lowerSDP := strings.ToLower(string(localSDP))
	for _, forbidden := range []string{"cn/", "rtx", "red/", "ulpfec", "flexfec", "vp9", "av1", "h265", "g729"} {
		if strings.Contains(lowerSDP, forbidden) {
			t.Fatalf("bounded offer contains excluded capability %q:\n%s", forbidden, localSDP)
		}
	}
	t.Logf("bounded Chrome offer size: %d bytes", len(localSDP))
}

func testSDPCrypto(t *testing.T, raw []byte) (tag, suite, inline string) {
	t.Helper()
	found := false
	for _, line := range strings.Split(string(raw), "\r\n") {
		if !strings.HasPrefix(line, "a=crypto:") {
			continue
		}
		if found {
			t.Fatal("SDP contains multiple audio crypto attributes")
		}
		found = true
		fields := strings.Fields(strings.TrimPrefix(line, "a=crypto:"))
		if len(fields) != 3 {
			t.Fatalf("crypto attribute has %d fields, want 3", len(fields))
		}
		tag, suite, inline = fields[0], fields[1], fields[2]
	}
	if !found {
		t.Fatal("SDP has no crypto attribute")
	}
	return tag, suite, inline
}

func TestNewInboundSIPMediaSDESSRTPBidirectionalRTP(t *testing.T) {
	remote := &media.MediaSession{
		Codecs:    []media.Codec{media.CodecAudioUlaw},
		Mode:      diagosdp.ModeSendrecv,
		Laddr:     net.UDPAddr{IP: net.ParseIP("127.0.0.1")},
		SecureRTP: 1,
		SRTPAlg:   media.SRTPProfileAes128CmHmacSha1_80,
	}
	if err := remote.Init(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = remote.Close() })
	offer := remote.LocalSDP()
	offerTag, offerSuite, offerInline := testSDPCrypto(t, offer)

	browserOffer := testSDP("m=audio 9 UDP/TLS/RTP/SAVPF 0")
	selection, err := negotiateInboundSelection(browserOffer, offer)
	if err != nil {
		t.Fatal(err)
	}
	request := sip.NewRequest(sip.INVITE, sip.Uri{User: "user", Host: "127.0.0.1"})
	request.SetBody(offer)
	server := &sipgo.DialogServerSession{}
	server.InviteRequest = request
	dialog := &diago.DialogServerSession{DialogServerSession: server}
	session, _, err := newInboundSIPMedia(dialog, net.ParseIP("127.0.0.1"), selection)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dialog.DialogMedia.Close() })

	answer := session.LocalSDP()
	answerModel, err := parseSDP(answer)
	if err != nil {
		t.Fatal(err)
	}
	if answerModel.Audio == nil || answerModel.Audio.Protocol != "RTP/SAVP" {
		t.Fatalf("SRTP answer audio = %+v", answerModel.Audio)
	}
	answerTag, answerSuite, answerInline := testSDPCrypto(t, answer)
	if answerTag != offerTag || answerSuite != offerSuite {
		t.Fatal("SDES answer did not echo the selected offer tag and suite")
	}
	if answerInline == offerInline {
		t.Fatal("SDES answer reused the offerer's sending key")
	}
	if err := remote.RemoteSDP(answer); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name   string
		writer *media.MediaSession
		reader *media.MediaSession
		ssrc   uint32
	}{
		{name: "SIP peer to gateway", writer: remote, reader: session, ssrc: 0x11223344},
		{name: "gateway to SIP peer", writer: session, reader: remote, ssrc: 0x55667788},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.reader.StopRTP(1, 2*time.Second); err != nil {
				t.Fatal(err)
			}
			payload := []byte("arbitrary SRTP payload")
			packet := rtp.Packet{
				Header:  rtp.Header{Version: 2, PayloadType: 0, SequenceNumber: 100, Timestamp: 8000, SSRC: test.ssrc},
				Payload: payload,
			}
			if err := test.writer.WriteRTP(&packet); err != nil {
				t.Fatal(err)
			}
			wire := make([]byte, media.RTPBufSize)
			n, err := test.reader.ReadRTPRaw(wire)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(wire[:n], payload) {
				t.Fatal("RTP payload was sent in plaintext")
			}

			packet.SequenceNumber++
			packet.Timestamp += 160
			if err := test.writer.WriteRTP(&packet); err != nil {
				t.Fatal(err)
			}
			var decoded rtp.Packet
			if _, err := test.reader.ReadRTP(make([]byte, media.RTPBufSize), &decoded); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(decoded.Payload, payload) || decoded.SSRC != test.ssrc {
				t.Fatalf("decrypted RTP = %+v", decoded)
			}
		})
	}
}

func TestMediaRelaySDESSRTPBidirectional(t *testing.T) {
	remote := &media.MediaSession{
		Codecs:    []media.Codec{media.CodecAudioUlaw},
		Mode:      diagosdp.ModeSendrecv,
		Laddr:     net.UDPAddr{IP: net.ParseIP("127.0.0.1")},
		SecureRTP: 1,
		SRTPAlg:   media.SRTPProfileAes128CmHmacSha1_80,
	}
	if err := remote.Init(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = remote.Close() })
	offer := remote.LocalSDP()
	selection, err := negotiateInboundSelection(testSDP("m=audio 9 UDP/TLS/RTP/SAVPF 0"), offer)
	if err != nil {
		t.Fatal(err)
	}
	request := sip.NewRequest(sip.INVITE, sip.Uri{User: "user", Host: "127.0.0.1"})
	request.SetBody(offer)
	server := &sipgo.DialogServerSession{}
	server.InviteRequest = request
	dialog := &diago.DialogServerSession{DialogServerSession: server}
	if _, _, err := newInboundSIPMedia(dialog, net.ParseIP("127.0.0.1"), selection); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dialog.DialogMedia.Close() })
	if err := remote.RemoteSDP(dialog.MediaSession().LocalSDP()); err != nil {
		t.Fatal(err)
	}

	var trackMu sync.Mutex
	var connectedRelay *mediaRelay
	var pendingTracks []*webrtc.TrackRemote
	fixture, browser := connectPionBrowser(t, selection, func(track *webrtc.TrackRemote) {
		trackMu.Lock()
		defer trackMu.Unlock()
		if connectedRelay == nil {
			pendingTracks = append(pendingTracks, track)
			return
		}
		connectedRelay.AddTrack(track)
	})
	relay, err := newMediaRelay(dialog, browser, selection)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = relay.Close() })
	trackMu.Lock()
	connectedRelay = relay
	for _, track := range pendingTracks {
		relay.AddTrack(track)
	}
	pendingTracks = nil
	trackMu.Unlock()
	// Pion may surface the remote track only with its first incoming RTP packet.
	writeTestPacket(t, fixture.audio, 0, 800, false, []byte{0})
	waitRelayRemoteKinds(t, relay, webrtc.RTPCodecTypeAudio)

	browserPayload := []byte{0x01, 0x80, 0x00, 0xff}
	writeTestPacket(t, fixture.audio, 0, 1600, true, browserPayload)
	if err := remote.StopRTP(1, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	var fromBrowser rtp.Packet
	for {
		if _, err := remote.ReadRTP(make([]byte, media.RTPBufSize), &fromBrowser); err != nil {
			t.Fatal(err)
		}
		if fromBrowser.Timestamp == 1600 {
			break
		}
	}
	if fromBrowser.PayloadType != 0 || fromBrowser.Timestamp != 1600 || !fromBrowser.Marker || !bytes.Equal(fromBrowser.Payload, browserPayload) {
		t.Fatalf("browser-to-SRTP relay packet = %+v", fromBrowser)
	}

	sipPayload := []byte{0xde, 0xad, 0xbe, 0xef}
	if err := remote.WriteRTP(&rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 0, SequenceNumber: 50, Timestamp: 3200, Marker: true, SSRC: 0x12345678},
		Payload: sipPayload,
	}); err != nil {
		t.Fatal(err)
	}
	fromSIP := waitObservedRTP(t, fixture.received, webrtc.RTPCodecTypeAudio)
	if fromSIP.PayloadType != 0 || fromSIP.Timestamp != 3200 || !fromSIP.Marker || !bytes.Equal(fromSIP.Payload, sipPayload) {
		t.Fatalf("SRTP-to-browser relay packet = %+v", fromSIP)
	}
}

func TestNewInboundSIPMediaRejectsInvalidSDES(t *testing.T) {
	for _, test := range []struct {
		name   string
		crypto string
	}{
		{name: "missing crypto"},
		{name: "unsupported suite", crypto: "a=crypto:1 AES_CM_256_HMAC_SHA1_80 inline:8Dlz/SyzlAKCZwH49w5DX8S4pDa7Lw0n3LTI4t6Z"},
		{name: "invalid inline key", crypto: "a=crypto:1 AES_CM_128_HMAC_SHA1_80 inline:not-base64"},
		{name: "unsupported session parameters", crypto: "a=crypto:1 AES_CM_128_HMAC_SHA1_80 inline:8Dlz/SyzlAKCZwH49w5DX8S4pDa7Lw0n3LTI4t6Z UNENCRYPTED_SRTP"},
		{name: "unsupported lifetime", crypto: "a=crypto:1 AES_CM_128_HMAC_SHA1_80 inline:8Dlz/SyzlAKCZwH49w5DX8S4pDa7Lw0n3LTI4t6Z|2^20"},
		{name: "invalid tag", crypto: "a=crypto:invalid AES_CM_128_HMAC_SHA1_80 inline:8Dlz/SyzlAKCZwH49w5DX8S4pDa7Lw0n3LTI4t6Z"},
	} {
		t.Run(test.name, func(t *testing.T) {
			lines := []string{"m=audio 4000 RTP/SAVP 0", "c=IN IP4 127.0.0.1"}
			if test.crypto != "" {
				lines = append(lines, test.crypto)
			}
			offer := testSDP(lines...)
			selection, err := negotiateInboundSelection(testSDP("m=audio 9 UDP/TLS/RTP/SAVPF 0"), offer)
			if err != nil {
				t.Fatal(err)
			}
			request := sip.NewRequest(sip.INVITE, sip.Uri{User: "user", Host: "127.0.0.1"})
			request.SetBody(offer)
			server := &sipgo.DialogServerSession{}
			server.InviteRequest = request
			dialog := &diago.DialogServerSession{DialogServerSession: server}
			session, _, err := newInboundSIPMedia(dialog, net.ParseIP("127.0.0.1"), selection)
			if err == nil {
				_ = session.Close()
				t.Fatal("invalid SDES offer succeeded")
			}
			if strings.Contains(err.Error(), "inline:") {
				t.Fatalf("SDES error exposed key material: %v", err)
			}
		})
	}
}

func TestNewInboundSIPMediaAVPFirstProfileAlternative(t *testing.T) {
	offer := testSDP(
		"m=audio 4000 RTP/AVP 0",
		"m=audio 4000 RTP/SAVP 0",
		"a=crypto:1 AES_CM_128_HMAC_SHA1_80 inline:8Dlz/SyzlAKCZwH49w5DX8S4pDa7Lw0n3LTI4t6Z",
	)
	selection, err := negotiateInboundSelection(testSDP("m=audio 9 UDP/TLS/RTP/SAVPF 0"), offer)
	if err != nil {
		t.Fatal(err)
	}
	request := sip.NewRequest(sip.INVITE, sip.Uri{User: "user", Host: "127.0.0.1"})
	request.SetBody(offer)
	server := &sipgo.DialogServerSession{}
	server.InviteRequest = request
	dialog := &diago.DialogServerSession{DialogServerSession: server}
	session, _, err := newInboundSIPMedia(dialog, net.ParseIP("127.0.0.1"), selection)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dialog.DialogMedia.Close() })
	answer := session.LocalSDP()
	if session.SecureRTP != 0 || strings.Contains(string(answer), "a=crypto:") ||
		!strings.Contains(string(answer), "m=audio 0 RTP/SAVP 0\r\n") {
		t.Fatal("AVP-first alternative did not retain plaintext audio and reject SAVP")
	}
}

func TestNewInboundSIPMediaSignalWireSAVPAlternative(t *testing.T) {
	sipOffer := testSDP(
		"m=audio 15348 RTP/SAVP 102 9 0 8 18 103 101",
		"c=IN IP4 127.0.0.1",
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
		"a=crypto:1 AEAD_AES_256_GCM_8 inline:PS1uQCVeeCFCanVmcjkpPywjNWhcYD0mXXtxaVBR",
		"a=crypto:5 AES_256_CM_HMAC_SHA1_80 inline:PS1uQCVeeCFCanVmcjkpPywjNWhcYD0mXXtxaVBR",
		"a=crypto:7 AES_CM_128_HMAC_SHA1_80 inline:8Dlz/SyzlAKCZwH49w5DX8S4pDa7Lw0n3LTI4t6Z",
		"a=crypto:8 AES_256_CM_HMAC_SHA1_32 inline:PS1uQCVeeCFCanVmcjkpPywjNWhcYD0mXXtxaVBR",
		"a=crypto:10 AES_CM_128_HMAC_SHA1_32 inline:PS1uQCVeeCFCanVmcjkpPywjNWhcYD0mXXtxaVBR",
		"a=ptime:20",
		"m=audio 15348 RTP/AVP 102 9 0 8 18 103 101",
		"c=IN IP4 127.0.0.1",
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
		"a=ptime:20",
	)
	browserOffer := testSDP(
		"m=audio 9 UDP/TLS/RTP/SAVPF 111 9 0 8 101",
		"a=rtpmap:111 opus/48000/2",
		"a=fmtp:111 minptime=10;useinbandfec=1",
		"a=rtpmap:9 G722/8000",
		"a=rtpmap:0 PCMU/8000",
		"a=rtpmap:8 PCMA/8000",
		"a=rtpmap:101 telephone-event/8000",
		"a=fmtp:101 0-16",
	)
	selection, err := negotiateInboundSelection(browserOffer, sipOffer)
	if err != nil {
		t.Fatal(err)
	}
	if len(selection.Audio) != 1 || selection.Audio[0].SIP.PayloadType != 102 || len(selection.RejectAudio) != 1 {
		t.Fatalf("SignalWire selection = %+v", selection)
	}
	request := sip.NewRequest(sip.INVITE, sip.Uri{User: "user", Host: "127.0.0.1"})
	request.SetBody(sipOffer)
	server := &sipgo.DialogServerSession{}
	server.InviteRequest = request
	dialog := &diago.DialogServerSession{DialogServerSession: server}
	session, _, err := newInboundSIPMedia(dialog, net.ParseIP("127.0.0.1"), selection)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dialog.DialogMedia.Close() })
	answer := session.LocalSDP()
	tag, suite, _ := testSDPCrypto(t, answer)
	if tag != "7" || suite != sipSDESCryptoSuite {
		t.Fatalf("SignalWire selected crypto tag/suite = %s/%s", tag, suite)
	}
	if strings.Count(string(answer), "m=audio ") != 2 || !strings.Contains(string(answer), "m=audio 0 RTP/AVP 102 9 0 8 18 103 101\r\n") {
		t.Fatal("SignalWire AVP alternative was not rejected in order")
	}
	model, err := parseSDP(answer)
	if err != nil {
		t.Fatal(err)
	}
	if model.Audio.Protocol != "RTP/SAVP" || len(model.AudioAlternatives) != 1 || model.AudioAlternatives[0].Port != 0 {
		t.Fatalf("SignalWire answer profiles = %+v, %+v", model.Audio, model.AudioAlternatives)
	}
}

func TestNewInboundSIPMediaSDESUsesAudioCryptoWithVideoFirstOffer(t *testing.T) {
	offer := testSDP(
		"m=video 4002 RTP/SAVP 96",
		"a=rtpmap:96 VP8/90000",
		"a=crypto:7 AES_CM_128_HMAC_SHA1_80 inline:PS1uQCVeeCFCanVmcjkpPywjNWhcYD0mXXtxaVBR",
		"m=audio 4000 RTP/SAVP 0",
		"c=IN IP4 127.0.0.1",
		"a=crypto:3 AES_CM_128_HMAC_SHA1_80 inline:8Dlz/SyzlAKCZwH49w5DX8S4pDa7Lw0n3LTI4t6Z",
	)
	selection, err := negotiateInboundSelection(testSDP("m=audio 9 UDP/TLS/RTP/SAVPF 0"), offer)
	if err != nil {
		t.Fatal(err)
	}
	if len(selection.Video) != 0 || len(selection.RejectVideoFormats) != 1 {
		t.Fatalf("video-first selection = %+v", selection)
	}
	request := sip.NewRequest(sip.INVITE, sip.Uri{User: "user", Host: "127.0.0.1"})
	request.SetBody(offer)
	server := &sipgo.DialogServerSession{}
	server.InviteRequest = request
	dialog := &diago.DialogServerSession{DialogServerSession: server}
	session, _, err := newInboundSIPMedia(dialog, net.ParseIP("127.0.0.1"), selection)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dialog.DialogMedia.Close() })
	answer := session.LocalSDP()
	tag, _, _ := testSDPCrypto(t, answer)
	if tag != "3" {
		t.Fatalf("audio answer selected crypto tag %s, want audio tag 3", tag)
	}
	model, err := parseSDP(answer)
	if err != nil {
		t.Fatal(err)
	}
	if model.Video == nil || model.Video.Port != 0 || model.Video.Protocol != "RTP/SAVP" {
		t.Fatalf("video-first rejection = %+v", model.Video)
	}
}

func TestNewInboundSIPMediaRejectsActiveSRTPVideo(t *testing.T) {
	for _, audioProtocol := range []string{"RTP/AVP", "RTP/SAVP"} {
		t.Run(audioProtocol, func(t *testing.T) {
			lines := []string{"m=audio 4000 " + audioProtocol + " 0", "c=IN IP4 127.0.0.1"}
			if audioProtocol == "RTP/SAVP" {
				lines = append(lines, "a=crypto:1 AES_CM_128_HMAC_SHA1_80 inline:8Dlz/SyzlAKCZwH49w5DX8S4pDa7Lw0n3LTI4t6Z")
			}
			lines = append(lines,
				"m=video 4002 RTP/SAVP 96",
				"a=rtpmap:96 VP8/90000",
				"a=crypto:2 AES_CM_128_HMAC_SHA1_80 inline:8Dlz/SyzlAKCZwH49w5DX8S4pDa7Lw0n3LTI4t6Z",
			)
			offer := testSDP(lines...)
			browser := testSDP(
				"m=audio 9 UDP/TLS/RTP/SAVPF 0",
				"m=video 9 UDP/TLS/RTP/SAVPF 96",
				"a=rtpmap:96 VP8/90000",
			)
			selection, err := negotiateInboundSelection(browser, offer)
			if err != nil {
				t.Fatal(err)
			}
			request := sip.NewRequest(sip.INVITE, sip.Uri{User: "user", Host: "127.0.0.1"})
			request.SetBody(offer)
			server := &sipgo.DialogServerSession{}
			server.InviteRequest = request
			dialog := &diago.DialogServerSession{DialogServerSession: server}
			if _, _, err := newInboundSIPMedia(dialog, net.ParseIP("127.0.0.1"), selection); err == nil || !strings.Contains(err.Error(), "active video") {
				t.Fatalf("active SRTP video error = %v", err)
			}
		})
	}
}

func TestNewInboundSIPMediaPinsSelectedAnswerAndCleansDialog(t *testing.T) {
	browserOffer := testSDP(
		"m=audio 9 UDP/TLS/RTP/SAVPF 0 101",
		"a=rtpmap:101 telephone-event/8000",
		"a=fmtp:101 0-16",
		"m=video 9 UDP/TLS/RTP/SAVPF 96",
		"a=rtpmap:96 VP8/90000",
	)
	sipOffer := testSDP(
		"m=audio 4000 RTP/AVP 0 101",
		"c=IN IP4 127.0.0.1",
		"a=rtpmap:101 telephone-event/8000",
		"m=video 4002 RTP/AVP 97",
		"a=rtpmap:97 H264/90000",
		"a=fmtp:97 packetization-mode=1;profile-level-id=42e01f",
	)
	selection, err := negotiateInboundSelection(browserOffer, sipOffer)
	if err != nil {
		t.Fatal(err)
	}
	if len(selection.Video) != 0 || !reflect.DeepEqual(selection.RejectVideoFormats, []string{"97"}) {
		t.Fatalf("video rejection selection = %+v", selection)
	}

	request := sip.NewRequest(sip.INVITE, sip.Uri{User: "user", Host: "127.0.0.1"})
	request.SetBody(sipOffer)
	server := &sipgo.DialogServerSession{}
	server.InviteRequest = request
	dialog := &diago.DialogServerSession{DialogServerSession: server}
	session, rtpSession, err := newInboundSIPMedia(dialog, net.ParseIP("127.0.0.1"), selection)
	if err != nil {
		t.Fatal(err)
	}
	if rtpSession == nil || dialog.MediaSession() != session {
		t.Fatal("inbound RTP/media session was not installed")
	}
	reader, err := dialog.AudioReader()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reader.(*media.RTPPacketReader); !ok {
		t.Fatalf("dialog audio reader = %T, want RTP packet reader", reader)
	}
	answer, err := parseSDP(session.LocalSDP())
	if err != nil {
		t.Fatal(err)
	}
	if len(answer.Audio.Codecs) != 1 || answer.Audio.Codecs[0].PayloadType != 0 {
		t.Fatalf("selected inbound audio = %+v", answer.Audio)
	}
	if answer.Video == nil || answer.Video.Port != 0 || !reflect.DeepEqual(answer.Video.Formats, []string{"97"}) {
		t.Fatalf("pinned video rejection = %+v\n%s", answer.Video, session.LocalSDP())
	}
	wantDTMF := []rtpCodec{{PayloadType: 101, Name: "telephone-event", ClockRate: 8000, Channels: 1}}
	if !reflect.DeepEqual(answer.Audio.TelephoneEvents, wantDTMF) {
		t.Fatalf("selected inbound DTMF = %#v, want %#v\n%s", answer.Audio.TelephoneEvents, wantDTMF, session.LocalSDP())
	}
	if err := dialog.DialogMedia.Close(); err != nil {
		t.Fatal(err)
	}
	if err := session.StopRTP(1, 0); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("media socket remains open after dialog close: %v", err)
	}
}

func TestNewInboundSIPMediaOmitsConflictingDTMF(t *testing.T) {
	browserOffer := testSDP(
		"m=audio 9 UDP/TLS/RTP/SAVPF 0 101",
		"a=rtpmap:101 telephone-event/8000",
		"m=video 9 UDP/TLS/RTP/SAVPF 96",
		"a=rtpmap:96 VP8/90000",
	)
	sipOffer := testSDP(
		"m=audio 4000 RTP/AVP 0 101",
		"c=IN IP4 127.0.0.1",
		"a=rtpmap:101 telephone-event/8000",
		"m=video 4002 RTP/AVP 101",
		"a=rtpmap:101 VP8/90000",
	)
	selection, err := negotiateInboundSelection(browserOffer, sipOffer)
	if err != nil {
		t.Fatal(err)
	}
	if selection.DTMF != nil || len(selection.Video) != 1 || selection.Video[0].SIP.PayloadType != 101 {
		t.Fatalf("conflicting DTMF selection = %+v", selection)
	}
	outbound, err := negotiateOutboundSelection(browserOffer, sipOffer)
	if err != nil {
		t.Fatal(err)
	}
	if outbound.DTMF != nil {
		t.Fatalf("outbound answer retained conflicting DTMF: %+v", outbound.DTMF)
	}

	request := sip.NewRequest(sip.INVITE, sip.Uri{User: "user", Host: "127.0.0.1"})
	request.SetBody(sipOffer)
	server := &sipgo.DialogServerSession{}
	server.InviteRequest = request
	dialog := &diago.DialogServerSession{DialogServerSession: server}
	session, _, err := newInboundSIPMedia(dialog, net.ParseIP("127.0.0.1"), selection)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dialog.DialogMedia.Close() })
	answer, err := parseSDP(session.LocalSDP())
	if err != nil {
		t.Fatal(err)
	}
	if len(answer.Audio.TelephoneEvents) != 0 || len(answer.Video.Codecs) != 1 || answer.Video.Codecs[0].PayloadType != 101 {
		t.Fatalf("conflicting DTMF was not omitted from answer:\n%s", session.LocalSDP())
	}
}

func TestNewInboundSIPMediaAudioVideoPayloadTypeCollision(t *testing.T) {
	browserOffer := testSDP(
		"m=audio 9 UDP/TLS/RTP/SAVPF 111",
		"a=rtpmap:111 opus/48000/2",
		"a=fmtp:111 minptime=10;useinbandfec=1;stereo=1",
		"m=video 9 UDP/TLS/RTP/SAVPF 96",
		"a=rtpmap:96 VP8/90000",
	)
	// This is the codec layout from the crashing FreeSWITCH offer. Payload
	// types 96/97/98/99 have independent audio and video mappings.
	sipOffer := testSDP(
		"m=audio 55234 RTP/AVP 96 97 98 0 8 9 18 101 99 100",
		"c=IN IP4 127.0.0.1",
		"a=rtpmap:96 opus/48000/2",
		"a=fmtp:96 useinbandfec=1;stereo=1",
		"a=rtpmap:97 speex/16000",
		"a=rtpmap:98 speex/8000",
		"a=fmtp:18 annexb=yes",
		"a=rtpmap:101 telephone-event/48000",
		"a=rtpmap:99 telephone-event/16000",
		"a=rtpmap:100 telephone-event/8000",
		"m=video 58426 RTP/AVP 96 97 98 99",
		"a=rtpmap:96 AV1/90000",
		"a=rtpmap:97 H265/90000",
		"a=rtpmap:98 H264/90000",
		"a=rtpmap:99 VP8/90000",
	)
	selection, err := negotiateInboundSelection(browserOffer, sipOffer)
	if err != nil {
		t.Fatal(err)
	}
	if len(selection.Audio) != 1 || selection.Audio[0].SIP.PayloadType != 96 ||
		len(selection.Video) != 1 || selection.Video[0].SIP.PayloadType != 99 {
		t.Fatalf("unexpected collision-offer selection: %+v", selection)
	}

	request := sip.NewRequest(sip.INVITE, sip.Uri{User: "user", Host: "127.0.0.1"})
	request.SetBody(sipOffer)
	server := &sipgo.DialogServerSession{}
	server.InviteRequest = request
	dialog := &diago.DialogServerSession{DialogServerSession: server}
	session, _, err := newInboundSIPMedia(dialog, net.ParseIP("127.0.0.1"), selection)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dialog.DialogMedia.Close() })

	answer, err := parseSDP(session.LocalSDP())
	if err != nil {
		t.Fatal(err)
	}
	if len(answer.Audio.Codecs) != 1 || answer.Audio.Codecs[0].PayloadType != 96 || answer.Audio.Codecs[0].Name != "opus" {
		t.Fatalf("answer audio = %+v", answer.Audio)
	}
	if answer.Video == nil || len(answer.Video.Codecs) != 1 || answer.Video.Codecs[0].PayloadType != 99 || answer.Video.Codecs[0].Name != "VP8" {
		t.Fatalf("answer video = %+v", answer.Video)
	}
}

func TestNewInboundSIPMediaAnswerDirections(t *testing.T) {
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
			browserOffer := testSDP(
				"m=audio 9 UDP/TLS/RTP/SAVPF 0",
				"m=video 9 UDP/TLS/RTP/SAVPF 96",
				"a=rtpmap:96 VP8/90000",
			)
			sipOffer := testSDP(
				"m=audio 4000 RTP/AVP 0",
				"c=IN IP4 127.0.0.1",
				"a="+test.offerDirection,
				"m=video 4002 RTP/AVP 96",
				"a=rtpmap:96 VP8/90000",
				"a="+test.offerDirection,
			)
			selection, err := negotiateInboundSelection(browserOffer, sipOffer)
			if err != nil {
				t.Fatal(err)
			}
			request := sip.NewRequest(sip.INVITE, sip.Uri{User: "user", Host: "127.0.0.1"})
			request.SetBody(sipOffer)
			server := &sipgo.DialogServerSession{}
			server.InviteRequest = request
			dialog := &diago.DialogServerSession{DialogServerSession: server}
			session, _, err := newInboundSIPMedia(dialog, net.ParseIP("127.0.0.1"), selection)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = dialog.DialogMedia.Close() })

			answer, err := parseSDP(session.LocalSDP())
			if err != nil {
				t.Fatal(err)
			}
			if answer.Audio.Direction != test.want || answer.Video.Direction != test.want {
				t.Fatalf("answer directions = %q, %q; want %q\n%s", answer.Audio.Direction, answer.Video.Direction, test.want, session.LocalSDP())
			}
			if answer.Audio.Port == 0 || answer.Video.Port == 0 {
				t.Fatalf("negotiated inactive/directional media was rejected: %+v %+v", answer.Audio, answer.Video)
			}
		})
	}
}

type observedRTP struct {
	kind   webrtc.RTPCodecType
	packet rtp.Packet
}

type pionBrowserFixture struct {
	pc        *webrtc.PeerConnection
	audio     *webrtc.TrackLocalStaticRTP
	video     *webrtc.TrackLocalStaticRTP
	received  chan observedRTP
	answerEnd atomic.Bool
	answerNum atomic.Int32
}

func emitTestCandidate(target chan<- webrtc.ICECandidateInit, candidate *webrtc.ICECandidate) {
	if candidate == nil {
		target <- webrtc.ICECandidateInit{Candidate: ""}
		return
	}
	target <- candidate.ToJSON()
}

func forwardTestCandidates(
	source <-chan webrtc.ICECandidateInit,
	add func(webrtc.ICECandidateInit) error,
	done chan<- struct{},
	errors chan<- error,
) {
	go func() {
		defer close(done)
		for candidate := range source {
			if err := add(candidate); err != nil {
				errors <- fmt.Errorf("add ICE candidate: %w", err)
				return
			}
			if candidate.Candidate == "" {
				return
			}
		}
	}()
}

func waitTestCondition(t *testing.T, timeout time.Duration, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func waitCandidateForwarding(t *testing.T, done <-chan struct{}, candidateErrors <-chan error) {
	t.Helper()
	select {
	case err := <-candidateErrors:
		t.Fatal(err)
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for ICE candidate forwarding")
	}
}

// relayDTMFInjector rewrites crafted audio payloads below Pion's track API.
// TrackLocalStaticRTP otherwise forces every packet to the primary audio PT.
type relayDTMFInjector struct {
	interceptor.NoOp
	pt atomic.Int32
}

func (i *relayDTMFInjector) BindLocalStream(_ *interceptor.StreamInfo, writer interceptor.RTPWriter) interceptor.RTPWriter {
	return interceptor.RTPWriterFunc(func(header *rtp.Header, payload []byte, attributes interceptor.Attributes) (int, error) {
		if pt := i.pt.Load(); pt >= 0 && len(payload) == 5 && payload[0] == 0xee {
			header.PayloadType = uint8(pt)
			return writer.Write(header, payload[1:], attributes)
		}
		return writer.Write(header, payload, attributes)
	})
}

type relayInterceptorFactory func(id string) (interceptor.Interceptor, error)

func (f relayInterceptorFactory) NewInterceptor(id string) (interceptor.Interceptor, error) {
	return f(id)
}

func connectPionBrowser(t *testing.T, selection negotiatedSelection, onTrack func(*webrtc.TrackRemote), extraInterceptors ...interceptor.Factory) (*pionBrowserFixture, *browserMedia) {
	t.Helper()
	fixture := &pionBrowserFixture{received: make(chan observedRTP, 32)}
	var pc *webrtc.PeerConnection
	var err error
	if len(extraInterceptors) == 0 && selection.DTMF == nil {
		pc, err = webrtc.NewPeerConnection(webrtc.Configuration{})
	} else {
		engine := &webrtc.MediaEngine{}
		if err := engine.RegisterDefaultCodecs(); err != nil {
			t.Fatal(err)
		}
		if selection.DTMF != nil {
			if err := registerBrowserCodec(engine, selection.DTMF.Browser, "audio", webrtc.RTPCodecTypeAudio); err != nil {
				t.Fatal(err)
			}
		}
		registry := &interceptor.Registry{}
		if err := webrtc.RegisterDefaultInterceptors(engine, registry); err != nil {
			t.Fatal(err)
		}
		for _, factory := range extraInterceptors {
			registry.Add(factory)
		}
		pc, err = webrtc.NewAPI(
			webrtc.WithMediaEngine(engine),
			webrtc.WithInterceptorRegistry(registry),
		).NewPeerConnection(webrtc.Configuration{})
	}
	if err != nil {
		t.Fatal(err)
	}
	fixture.pc = pc
	t.Cleanup(func() { _ = pc.Close() })
	fixture.audio, err = webrtc.NewTrackLocalStaticRTP(selection.Audio[0].Browser.webRTCCapability("audio"), "browser-audio", "browser")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pc.AddTrack(fixture.audio); err != nil {
		t.Fatal(err)
	}
	if len(selection.Video) == 1 {
		fixture.video, err = webrtc.NewTrackLocalStaticRTP(selection.Video[0].Browser.webRTCCapability("video"), "browser-video", "browser")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pc.AddTrack(fixture.video); err != nil {
			t.Fatal(err)
		}
	}
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		go func() {
			for {
				packet, _, err := track.ReadRTP()
				if err != nil {
					return
				}
				cloned := *packet
				cloned.Payload = append([]byte(nil), packet.Payload...)
				fixture.received <- observedRTP{kind: track.Kind(), packet: cloned}
			}
		}()
	})

	peerCandidates := make(chan webrtc.ICECandidateInit, 64)
	answerCandidates := make(chan webrtc.ICECandidateInit, 64)
	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		emitTestCandidate(peerCandidates, candidate)
	})
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	browser, err := newBrowserMedia(offer, selection, nil, func(candidate webrtc.ICECandidateInit) {
		fixture.answerNum.Add(1)
		if candidate.Candidate == "" {
			fixture.answerEnd.Store(true)
		}
		answerCandidates <- candidate
	}, onTrack)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = browser.Close() })
	if err := pc.SetRemoteDescription(browser.Answer); err != nil {
		t.Fatal(err)
	}

	peerDone := make(chan struct{})
	answerDone := make(chan struct{})
	candidateErrors := make(chan error, 2)
	forwardTestCandidates(peerCandidates, browser.AddCandidate, peerDone, candidateErrors)
	forwardTestCandidates(answerCandidates, pc.AddICECandidate, answerDone, candidateErrors)
	waitTestCondition(t, 15*time.Second, "Pion connection", func() bool {
		return pc.ConnectionState() == webrtc.PeerConnectionStateConnected && browser.pc.ConnectionState() == webrtc.PeerConnectionStateConnected
	})
	waitCandidateForwarding(t, peerDone, candidateErrors)
	waitCandidateForwarding(t, answerDone, candidateErrors)
	return fixture, browser
}

func TestBrowserMediaOfferAnswerWithSelectedCodecsAndCandidates(t *testing.T) {
	selection := testRelaySelection()
	selection.DTMF = &codecPair{
		Browser: rtpCodec{PayloadType: 101, Name: "telephone-event", ClockRate: 8000, Channels: 1, Fmtp: "0-16"},
		SIP:     rtpCodec{PayloadType: 120, Name: "telephone-event", ClockRate: 8000, Channels: 1, Fmtp: "0-16"},
	}
	_, browser := connectPionBrowser(t, selection, nil)
	answer, err := parseBrowserSDP([]byte(browser.Answer.SDP))
	if err != nil {
		t.Fatal(err)
	}
	audio, video := selection.browserCodecs()
	if !reflect.DeepEqual(answer.Audio.Codecs, audio) || !reflect.DeepEqual(answer.Video.Codecs, video) {
		t.Fatalf("browser answer codecs = %#v, %#v; want %#v, %#v", answer.Audio.Codecs, answer.Video.Codecs, audio, video)
	}
	if !reflect.DeepEqual(answer.Audio.TelephoneEvents, []rtpCodec{selection.DTMF.Browser}) {
		t.Fatalf("browser answer DTMF = %#v, want %#v\n%s", answer.Audio.TelephoneEvents, selection.DTMF.Browser, browser.Answer.SDP)
	}
	if !strings.Contains(browser.Answer.SDP, "a=setup:active") {
		t.Fatalf("answer is not an active DTLS answerer:\n%s", browser.Answer.SDP)
	}
}

func TestBrowserMediaCandidateEndAndClose(t *testing.T) {
	selection := testRelaySelection()
	selection.Video = nil
	fixture, browser := connectPionBrowser(t, selection, nil)
	if fixture.answerNum.Load() < 2 || !fixture.answerEnd.Load() {
		t.Fatalf("candidate callbacks = %d, end = %v", fixture.answerNum.Load(), fixture.answerEnd.Load())
	}
	if err := browser.AddCandidate(webrtc.ICECandidateInit{Candidate: ""}); err != nil {
		t.Fatal(err)
	}
	if err := browser.Close(); err != nil {
		t.Fatal(err)
	}
	if err := browser.Close(); err != nil {
		t.Fatal(err)
	}
	if err := browser.AddCandidate(webrtc.ICECandidateInit{Candidate: ""}); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("candidate after close error = %v", err)
	}
}

type relayTestDialog struct {
	session *media.MediaSession
	rtp     *media.RTPSession
	reader  *media.RTPPacketReader
}

func (d *relayTestDialog) AudioReader(...diago.AudioReaderOption) (io.Reader, error) {
	return d.reader, nil
}
func (d *relayTestDialog) MediaSession() *media.MediaSession { return d.session }
func (d *relayTestDialog) RTPSession() *media.RTPSession     { return d.rtp }

func listenTestUDP(t *testing.T) *net.UDPConn {
	t.Helper()
	connection, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return connection
}

func writeTestPacket(t *testing.T, connection *webrtc.TrackLocalStaticRTP, payloadType uint8, timestamp uint32, marker bool, payload []byte) {
	t.Helper()
	writeTestPacketWithSequence(t, connection, payloadType, uint16(timestamp), timestamp, marker, payload)
}

func writeTestPacketWithSequence(t *testing.T, connection *webrtc.TrackLocalStaticRTP, payloadType uint8, sequence uint16, timestamp uint32, marker bool, payload []byte) {
	t.Helper()
	if err := connection.WriteRTP(&rtp.Packet{Header: rtp.Header{
		Version:        2,
		PayloadType:    payloadType,
		SequenceNumber: sequence,
		Timestamp:      timestamp,
		Marker:         marker,
		SSRC:           5000,
	}, Payload: payload}); err != nil {
		t.Fatal(err)
	}
}

func readTestUDPRTP(t *testing.T, connection *net.UDPConn) rtp.Packet {
	t.Helper()
	if err := connection.SetReadDeadline(time.Now().Add(8 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 65535)
	n, _, err := connection.ReadFromUDP(buffer)
	if err != nil {
		t.Fatal(err)
	}
	var packet rtp.Packet
	if err := packet.Unmarshal(buffer[:n]); err != nil {
		t.Fatal(err)
	}
	return packet
}

func sendTestUDPRTP(t *testing.T, connection *net.UDPConn, target *net.UDPAddr, payloadType uint8, timestamp uint32, marker bool, ssrc uint32, payload []byte) {
	t.Helper()
	raw, err := (&rtp.Packet{Header: rtp.Header{
		Version:        2,
		PayloadType:    payloadType,
		SequenceNumber: 20,
		Timestamp:      timestamp,
		Marker:         marker,
		SSRC:           ssrc,
	}, Payload: payload}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.WriteToUDP(raw, target); err != nil {
		t.Fatal(err)
	}
}

func waitObservedRTP(t *testing.T, source <-chan observedRTP, kind webrtc.RTPCodecType) rtp.Packet {
	t.Helper()
	timer := time.NewTimer(8 * time.Second)
	defer timer.Stop()
	for {
		select {
		case observed := <-source:
			if observed.kind == kind {
				return observed.packet
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for %s RTP", kind.String())
		}
	}
}

func expectNoTestUDPRTP(t *testing.T, connection *net.UDPConn) {
	t.Helper()
	if err := connection.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 65535)
	n, _, err := connection.ReadFromUDP(buffer)
	if err != nil {
		var networkError net.Error
		if errors.As(err, &networkError) && networkError.Timeout() {
			return
		}
		t.Fatal(err)
	}
	var packet rtp.Packet
	if err := packet.Unmarshal(buffer[:n]); err != nil {
		t.Fatal(err)
	}
	t.Fatalf("unexpected RTP forwarded to SIP: %+v payload %x", packet.Header, packet.Payload)
}

func expectNoObservedRTP(t *testing.T, source <-chan observedRTP) {
	t.Helper()
	select {
	case observed := <-source:
		t.Fatalf("unexpected RTP forwarded to browser %s: %+v payload %x", observed.kind, observed.packet.Header, observed.packet.Payload)
	case <-time.After(200 * time.Millisecond):
	}
}

func waitRelayRemoteKinds(t *testing.T, relay *mediaRelay, kinds ...webrtc.RTPCodecType) {
	t.Helper()
	waitTestCondition(t, 8*time.Second, "browser tracks to reach relay", func() bool {
		relay.mu.Lock()
		defer relay.mu.Unlock()
		for _, kind := range kinds {
			if _, ok := relay.remoteKinds[kind]; !ok {
				return false
			}
		}
		return true
	})
}

func TestMediaRelayBidirectionalPayloadPTTimestampAndDemuxCleanup(t *testing.T) {
	selection := testRelaySelection()
	var relayPointer atomic.Pointer[mediaRelay]
	fixture, browser := connectPionBrowser(t, selection, func(track *webrtc.TrackRemote) {
		if relay := relayPointer.Load(); relay != nil {
			relay.AddTrack(track)
		}
	})

	audioPeer := listenTestUDP(t)
	videoPeer := listenTestUDP(t)
	sipAudio, sipVideo := selection.sipCodecs()
	session, err := initializeSIPMediaSession(net.ParseIP("127.0.0.1"), sipAudio, sipVideo)
	if err != nil {
		t.Fatal(err)
	}
	rtpSession := media.NewRTPSession(session)
	t.Cleanup(func() {
		_ = rtpSession.MonitorClose()
		_ = session.Close()
	})
	audioAddress := audioPeer.LocalAddr().(*net.UDPAddr)
	videoAddress := videoPeer.LocalAddr().(*net.UDPAddr)
	session.SetRemoteAddr(audioAddress)
	selection.SIPAudio = &mediaSection{Kind: "audio", Port: audioAddress.Port, ConnectionIP: cloneIP(audioAddress.IP)}
	selection.SIPVideo = &mediaSection{Kind: "video", Port: videoAddress.Port, ConnectionIP: cloneIP(videoAddress.IP)}
	dialog := &relayTestDialog{session: session, rtp: rtpSession, reader: media.NewRTPPacketReaderSession(rtpSession)}
	relay, err := newMediaRelay(dialog, browser, selection)
	if err != nil {
		t.Fatal(err)
	}
	relayPointer.Store(relay)
	t.Cleanup(func() { _ = relay.Close() })
	if relay.videoConn == nil {
		t.Fatal("demuxed video did not allocate its dedicated UDP socket")
	}
	videoConn := relay.videoConn

	audioPayload1 := []byte{0x01, 0x80, 0x00, 0xff}
	writeTestPacket(t, fixture.audio, 111, 1000, true, audioPayload1)
	got := readTestUDPRTP(t, audioPeer)
	if got.PayloadType != 96 || got.Timestamp != 1000 || !got.Marker || !bytes.Equal(got.Payload, audioPayload1) {
		t.Fatalf("browser-to-SIP audio = %+v payload %x", got.Header, got.Payload)
	}
	audioPayload2 := []byte{0xde, 0xad, 0xbe, 0xef}
	writeTestPacket(t, fixture.audio, 111, 1320, false, audioPayload2)
	got = readTestUDPRTP(t, audioPeer)
	if got.PayloadType != 96 || got.Timestamp != 1320 || got.Marker || !bytes.Equal(got.Payload, audioPayload2) {
		t.Fatalf("second browser-to-SIP audio = %+v payload %x", got.Header, got.Payload)
	}

	videoPayload := []byte{0x90, 0x00, 0x7f, 0x42, 0x99}
	writeTestPacket(t, fixture.video, 96, 5000, true, videoPayload)
	got = readTestUDPRTP(t, videoPeer)
	if got.PayloadType != 97 || got.Timestamp != 5000 || !got.Marker || !bytes.Equal(got.Payload, videoPayload) {
		t.Fatalf("browser-to-SIP video = %+v payload %x", got.Header, got.Payload)
	}

	localAddress := &net.UDPAddr{IP: cloneIP(session.Laddr.IP), Port: session.Laddr.Port}
	// An unselected PT on the same socket must not consume or corrupt the next
	// selected audio packet.
	sendTestUDPRTP(t, audioPeer, localAddress, 8, 10, false, 6000, []byte{0x55})
	sipAudioPayload := []byte{0x11, 0x22, 0x00, 0x80}
	sendTestUDPRTP(t, audioPeer, localAddress, 96, 777, true, 6001, sipAudioPayload)
	got = waitObservedRTP(t, fixture.received, webrtc.RTPCodecTypeAudio)
	if got.PayloadType != 111 || got.Timestamp != 777 || !got.Marker || !bytes.Equal(got.Payload, sipAudioPayload) {
		t.Fatalf("SIP-to-browser audio = %+v payload %x", got.Header, got.Payload)
	}

	sipVideoPayload := []byte{0x33, 0x44, 0x55, 0x00, 0xaa}
	// The offer advertised the shared local port, so inbound demuxed video also
	// arrives there even though browser-originated video uses a dedicated writer.
	sendTestUDPRTP(t, videoPeer, localAddress, 97, 9000, true, 7000, sipVideoPayload)
	got = waitObservedRTP(t, fixture.received, webrtc.RTPCodecTypeVideo)
	if got.PayloadType != 96 || got.Timestamp != 9000 || !got.Marker || !bytes.Equal(got.Payload, sipVideoPayload) {
		t.Fatalf("SIP-to-browser video = %+v payload %x", got.Header, got.Payload)
	}

	// Demuxed video must not teach the shared symmetric-RTP session to send
	// subsequent audio/DTMF to videoPeer instead of audioPeer.
	audioPayload3 := []byte{0x55, 0x66, 0x77}
	writeTestPacket(t, fixture.audio, 111, 2000, false, audioPayload3)
	got = readTestUDPRTP(t, audioPeer)
	if got.PayloadType != 96 || got.Timestamp != 2000 || !bytes.Equal(got.Payload, audioPayload3) {
		t.Fatalf("audio after demuxed video = %+v payload %x", got.Header, got.Payload)
	}

	if err := relay.Close(); err != nil {
		t.Fatal(err)
	}
	if err := relay.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := videoConn.Write([]byte{1}); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("dedicated video socket remains open: %v", err)
	}
}

func TestMediaRelayReoriginatesBrowserDTMFOnSIPClock(t *testing.T) {
	selection := testRelaySelection()
	selection.Video = nil
	selection.DTMF = &codecPair{
		Browser: rtpCodec{PayloadType: 101, Name: "telephone-event", ClockRate: 8000, Channels: 1, Fmtp: "0-16"},
		SIP:     rtpCodec{PayloadType: 120, Name: "telephone-event", ClockRate: 8000, Channels: 1, Fmtp: "0-16"},
	}
	injector := &relayDTMFInjector{}
	injector.pt.Store(int32(selection.DTMF.Browser.PayloadType))

	var trackMu sync.Mutex
	var connectedRelay *mediaRelay
	var pendingTracks []*webrtc.TrackRemote
	fixture, browser := connectPionBrowser(t, selection, func(track *webrtc.TrackRemote) {
		trackMu.Lock()
		defer trackMu.Unlock()
		if connectedRelay == nil {
			pendingTracks = append(pendingTracks, track)
			return
		}
		connectedRelay.AddTrack(track)
	}, relayInterceptorFactory(func(string) (interceptor.Interceptor, error) { return injector, nil }))

	audioPeer := listenTestUDP(t)
	sipAudio, _ := selection.sipCodecs()
	sipAudio = append(sipAudio, selection.DTMF.SIP)
	session, err := initializeSIPMediaSession(net.ParseIP("127.0.0.1"), sipAudio, nil)
	if err != nil {
		t.Fatal(err)
	}
	rtpSession := media.NewRTPSession(session)
	t.Cleanup(func() {
		_ = rtpSession.MonitorClose()
		_ = session.Close()
	})
	audioAddress := audioPeer.LocalAddr().(*net.UDPAddr)
	session.SetRemoteAddr(audioAddress)
	selection.SIPAudio = &mediaSection{Kind: "audio", Port: audioAddress.Port, ConnectionIP: cloneIP(audioAddress.IP)}
	dialog := &relayTestDialog{session: session, rtp: rtpSession, reader: media.NewRTPPacketReaderSession(rtpSession)}
	relay, err := newMediaRelay(dialog, browser, selection)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = relay.Close() })
	if stats := rtpSession.WriteStats(); stats.SSRC != relay.audioWriter.writer.SSRC {
		t.Fatalf("primary audio RTCP writer was not primed: %+v", stats)
	}
	trackMu.Lock()
	connectedRelay = relay
	for _, track := range pendingTracks {
		relay.AddTrack(track)
	}
	pendingTracks = nil
	trackMu.Unlock()

	audioBefore := []byte{0x01, 0x80, 0x00, 0xff}
	writeTestPacket(t, fixture.audio, 111, 1000, true, audioBefore)
	waitRelayRemoteKinds(t, relay, webrtc.RTPCodecTypeAudio)
	got := readTestUDPRTP(t, audioPeer)
	if got.PayloadType != 96 || got.Timestamp != 1000 || !bytes.Equal(got.Payload, audioBefore) {
		t.Fatalf("audio before DTMF = %+v payload %x", got.Header, got.Payload)
	}

	eventTimestamp := uint32(4000)
	events := []struct {
		sequence uint16
		marker   bool
		payload  []byte
	}{
		{sequence: 1001, marker: true, payload: []byte{5, 10, 0, 160}},
		{sequence: 1002, payload: []byte{5, 10, 1, 64}},
		{sequence: 1003, payload: []byte{5, 0x8a, 3, 32}},
		{sequence: 1004, payload: []byte{5, 0x8a, 3, 32}},
		{sequence: 1005, payload: []byte{5, 0x8a, 3, 32}},
		{sequence: 1006, marker: true, payload: []byte{5, 10, 0, 160}},
		{sequence: 1007, payload: []byte{5, 10, 1, 64}},
		{sequence: 1008, payload: []byte{5, 0x8a, 3, 32}},
		{sequence: 1009, payload: []byte{5, 0x8a, 3, 32}},
		{sequence: 1010, payload: []byte{5, 0x8a, 3, 32}},
	}
	for index, event := range events {
		if index == 5 {
			eventTimestamp = 4800 // a new press of the same digit, not an END retransmission
		}
		crafted := append([]byte{0xee}, event.payload...)
		writeTestPacketWithSequence(t, fixture.audio, 111, event.sequence, eventTimestamp, event.marker, crafted)
	}

	// The three browser END packets are one key press. As in sip-bot, diago
	// emits a fresh seven-packet series on the SIP audio timeline instead of
	// forwarding the browser's event timestamp through the audio delta clock.
	wantEvents := []struct {
		end      bool
		duration uint16
	}{
		{duration: 160}, {duration: 320}, {duration: 480}, {duration: 640},
		{end: true, duration: 800}, {end: true, duration: 800}, {end: true, duration: 800},
	}
	for press := 0; press < 2; press++ {
		wantTimestamp := uint32(1000)
		if press == 1 {
			// Opus uses 48 kHz: advance 100 ms (800 units at 8 kHz)
			// between the two event starts, not zero between every packet.
			wantTimestamp = 5800
		}
		for index, want := range wantEvents {
			got = readTestUDPRTP(t, audioPeer)
			wantPayload := []byte{5, 10, byte(want.duration >> 8), byte(want.duration)}
			if want.end {
				wantPayload[1] |= 0x80
			}
			if got.PayloadType != 120 || got.Timestamp != wantTimestamp || got.Marker != (index == 0) || !bytes.Equal(got.Payload, wantPayload) {
				t.Fatalf("SIP DTMF press %d packet %d = %+v payload %x, want PT 120 timestamp %d marker %t payload %x", press, index, got.Header, got.Payload, wantTimestamp, index == 0, wantPayload)
			}
		}
	}

	audioAfter := []byte{0xde, 0xad, 0xbe, 0xef}
	writeTestPacket(t, fixture.audio, 111, 12000, false, audioAfter)
	got = readTestUDPRTP(t, audioPeer)
	if got.PayloadType != 96 || got.Timestamp != 12000 || !bytes.Equal(got.Payload, audioAfter) {
		t.Fatalf("audio after DTMF = %+v payload %x", got.Header, got.Payload)
	}
}

func TestMediaRelayEnforcesNegotiatedDirections(t *testing.T) {
	for _, negotiation := range []struct {
		name      string
		negotiate func([]byte, []byte) (negotiatedSelection, error)
	}{
		{name: "inbound offer", negotiate: negotiateInboundSelection},
		{name: "outbound answer", negotiate: negotiateOutboundSelection},
	} {
		for _, direction := range []struct {
			name        string
			wantReceive bool
			wantSend    bool
		}{
			{name: "sendonly", wantReceive: true},
			{name: "recvonly", wantSend: true},
			{name: "sendrecv", wantReceive: true, wantSend: true},
			{name: "inactive"},
		} {
			t.Run(negotiation.name+"/"+direction.name, func(t *testing.T) {
				audioPeer := listenTestUDP(t)
				videoPeer := listenTestUDP(t)
				audioAddress := audioPeer.LocalAddr().(*net.UDPAddr)
				videoAddress := videoPeer.LocalAddr().(*net.UDPAddr)
				sipDescription := testSDP(
					fmt.Sprintf("m=audio %d RTP/AVP 96", audioAddress.Port),
					"c=IN IP4 127.0.0.1",
					"a=rtpmap:96 opus/48000/2",
					"a=fmtp:96 useinbandfec=1;minptime=10",
					"a="+direction.name,
					fmt.Sprintf("m=video %d RTP/AVP 98", videoAddress.Port),
					"c=IN IP4 127.0.0.1",
					"a=rtpmap:98 VP8/90000",
					"a="+direction.name,
				)
				selection, err := negotiation.negotiate(realisticBrowserOffer(), sipDescription)
				if err != nil {
					t.Fatal(err)
				}

				var trackMu sync.Mutex
				var connectedRelay *mediaRelay
				var pendingTracks []*webrtc.TrackRemote
				fixture, browser := connectPionBrowser(t, selection, func(track *webrtc.TrackRemote) {
					trackMu.Lock()
					defer trackMu.Unlock()
					if connectedRelay == nil {
						pendingTracks = append(pendingTracks, track)
						return
					}
					connectedRelay.AddTrack(track)
				})

				sipAudio, sipVideo := selection.sipCodecs()
				session, err := initializeSIPMediaSession(net.ParseIP("127.0.0.1"), sipAudio, sipVideo)
				if err != nil {
					t.Fatal(err)
				}
				rtpSession := media.NewRTPSession(session)
				t.Cleanup(func() {
					_ = rtpSession.MonitorClose()
					_ = session.Close()
				})
				session.SetRemoteAddr(audioAddress)
				dialog := &relayTestDialog{session: session, rtp: rtpSession, reader: media.NewRTPPacketReaderSession(rtpSession)}
				relay, err := newMediaRelay(dialog, browser, selection)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = relay.Close() })
				trackMu.Lock()
				connectedRelay = relay
				for _, track := range pendingTracks {
					relay.AddTrack(track)
				}
				pendingTracks = nil
				trackMu.Unlock()

				if (relay.audioWriter != nil) != direction.wantSend || (relay.videoWriter != nil) != direction.wantSend {
					t.Fatalf("writer allocation for remote %s = audio %v video %v, want %v", direction.name, relay.audioWriter != nil, relay.videoWriter != nil, direction.wantSend)
				}
				if (relay.videoConn != nil) != direction.wantSend {
					t.Fatalf("demux socket allocation for remote %s = %v, want %v", direction.name, relay.videoConn != nil, direction.wantSend)
				}
				writeTestPacket(t, fixture.audio, 111, 1000, true, []byte{0x11})
				writeTestPacket(t, fixture.video, 96, 5000, true, []byte{0x22})
				waitRelayRemoteKinds(t, relay, webrtc.RTPCodecTypeAudio, webrtc.RTPCodecTypeVideo)
				if direction.wantSend {
					if got := readTestUDPRTP(t, audioPeer); got.PayloadType != 96 {
						t.Fatalf("browser-to-SIP audio payload type = %d", got.PayloadType)
					}
					if got := readTestUDPRTP(t, videoPeer); got.PayloadType != 98 {
						t.Fatalf("browser-to-SIP video payload type = %d", got.PayloadType)
					}
				} else {
					expectNoTestUDPRTP(t, audioPeer)
					expectNoTestUDPRTP(t, videoPeer)
				}

				localAddress := &net.UDPAddr{IP: cloneIP(session.Laddr.IP), Port: session.Laddr.Port}
				sendTestUDPRTP(t, audioPeer, localAddress, 96, 2000, true, 6000, []byte{0x33})
				if direction.wantReceive {
					if got := waitObservedRTP(t, fixture.received, webrtc.RTPCodecTypeAudio); got.PayloadType != 111 {
						t.Fatalf("SIP-to-browser audio payload type = %d", got.PayloadType)
					}
				} else {
					expectNoObservedRTP(t, fixture.received)
				}
				sendTestUDPRTP(t, videoPeer, localAddress, 98, 6000, true, 7000, []byte{0x44})
				if direction.wantReceive {
					if got := waitObservedRTP(t, fixture.received, webrtc.RTPCodecTypeVideo); got.PayloadType != 96 {
						t.Fatalf("SIP-to-browser video payload type = %d", got.PayloadType)
					}
				} else {
					expectNoObservedRTP(t, fixture.received)
				}
			})
		}
	}
}
