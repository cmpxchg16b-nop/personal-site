// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package media

import (
	"bytes"
	"io"
	"net"
	"testing"

	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/sipgo/fakes"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMediaPortRange(t *testing.T) {
	RTPPortStart = 5000
	RTPPortEnd = 5010

	sessions := []*MediaSession{}
	for i := RTPPortStart; i < RTPPortEnd; i += 2 {
		require.Equal(t, i-RTPPortStart, int(rtpPortOffset.Load()))
		mess, err := NewMediaSession(net.IPv4(127, 0, 0, 1), 0)
		t.Log(mess.rtpConn.LocalAddr(), mess.rtcpConn.LocalAddr())
		require.NoError(t, err)
		sessions = append(sessions, mess)
	}

	for _, s := range sessions {
		s.Close()
	}

}

func TestDTMFEncodeDecode(t *testing.T) {
	// Example payload for DTMF digit '1' with volume 10 and duration 1000
	// Event: 0x01 (DTMF digit '1')
	// E bit: 0x80 (End of Event)
	// Volume: 0x0A (Volume 10)
	// Duration: 0x03E8 (Duration 1000)
	payload := []byte{0x01, 0x8A, 0x03, 0xE8}

	event := DTMFEvent{}
	err := DTMFDecode(payload, &event)
	if err != nil {
		t.Fatalf("Error decoding payload: %v", err)
	}

	if event.Event != 0x01 {
		t.Errorf("Unexpected Event. got: %v, want: %v", event.Event, 0x01)
	}

	if event.EndOfEvent != true {
		t.Errorf("Unexpected EndOfEvent. got: %v, want: %v", event.EndOfEvent, true)
	}

	if event.Volume != 0x0A {
		t.Errorf("Unexpected Volume. got: %v, want: %v", event.Volume, 0x0A)
	}

	if event.Duration != 0x03E8 {
		t.Errorf("Unexpected Duration. got: %v, want: %v", event.Duration, 0x03E8)
	}

	encoded := DTMFEncode(event)
	require.Equal(t, payload, encoded)
}

func TestReadRTCP(t *testing.T) {
	session := &MediaSession{}
	reader, writer := io.Pipe()
	session.rtcpConn = &fakes.UDPConn{
		Reader: reader,
	}

	go func() {
		pkts := []rtcp.Packet{
			&rtcp.SenderReport{},
			&rtcp.ReceiverReport{},
		}
		data, err := rtcp.Marshal(pkts)
		if err != nil {
			return
		}
		writer.Write(data)
	}()
	pkts := make([]rtcp.Packet, 5)
	n, err := session.ReadRTCP(make([]byte, 1600), pkts)
	require.NoError(t, err)
	require.Equal(t, 2, n)
	require.IsType(t, &rtcp.SenderReport{}, pkts[0])
	require.IsType(t, &rtcp.ReceiverReport{}, pkts[1])

}

func TestMediaSessionExternalIP(t *testing.T) {
	m := &MediaSession{
		Laddr:      net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)},
		Mode:       sdp.ModeSendrecv,
		ExternalIP: net.IPv4(1, 1, 1, 1),
	}

	data := m.LocalSDP()
	sd := sdp.SessionDescription{}
	err := sdp.Unmarshal(data, &sd)
	require.NoError(t, err)

	connInfo, err := sd.ConnectionInformation()
	require.NoError(t, err)
	assert.NotEmpty(t, connInfo.IP.To4())
	assert.Equal(t, m.ExternalIP.To4(), connInfo.IP.To4())
}

func TestMediaSessionForkPreservesExternalIP(t *testing.T) {
	m := &MediaSession{
		Laddr:      net.UDPAddr{IP: net.IPv4(10, 0, 0, 1)},
		Mode:       sdp.ModeSendrecv,
		ExternalIP: net.IPv4(203, 0, 113, 10),
	}

	fork := m.Fork()
	sd := sdp.SessionDescription{}
	require.NoError(t, sdp.Unmarshal(fork.LocalSDP(), &sd))

	connInfo, err := sd.ConnectionInformation()
	require.NoError(t, err)
	assert.Equal(t, m.ExternalIP.To4(), connInfo.IP.To4())
}

func TestMediaSessionUpdateCodec(t *testing.T) {
	newM := func() *MediaSession {
		return &MediaSession{
			Codecs: []Codec{
				CodecAudioUlaw, CodecAudioAlaw, CodecTelephoneEvent8000,
			},
		}
	}

	m := newM()
	m.updateRemoteCodecs([]Codec{CodecAudioAlaw, CodecAudioUlaw}, false)
	assert.Equal(t, []Codec{CodecAudioAlaw, CodecAudioUlaw}, m.filterCodecs)

	m = newM()
	m.updateRemoteCodecs([]Codec{CodecAudioAlaw}, false)
	assert.Equal(t, []Codec{CodecAudioAlaw}, m.filterCodecs)

	m = newM()
	m.updateRemoteCodecs([]Codec{}, false)
	assert.Equal(t, []Codec{}, m.filterCodecs)

	m = newM()
	m.updateRemoteCodecs([]Codec{{Name: "NonExisting"}}, false)
	assert.Equal(t, []Codec{}, m.filterCodecs)
}

func TestCodecsMatch(t *testing.T) {
	upperOpus := CodecAudioOpus
	upperOpus.Name = "OPUS"
	upperOpus.Fmtp = "useinbandfec=1;stereo=1"
	differentName := upperOpus
	differentName.Name = "opus2"
	differentPayload := upperOpus
	differentPayload.PayloadType++
	differentRate := upperOpus
	differentRate.SampleRate++
	differentDuration := upperOpus
	differentDuration.SampleDur++
	differentChannels := upperOpus
	differentChannels.NumChannels++

	tests := []struct {
		name string
		a    Codec
		b    Codec
		want bool
	}{
		{name: "name case and fmtp ignored", a: CodecAudioOpus, b: upperOpus, want: true},
		{name: "different name", a: CodecAudioOpus, b: differentName, want: false},
		{name: "different payload type", a: CodecAudioOpus, b: differentPayload, want: false},
		{name: "different sample rate", a: CodecAudioOpus, b: differentRate, want: false},
		{name: "different sample duration", a: CodecAudioOpus, b: differentDuration, want: false},
		{name: "different channels", a: CodecAudioOpus, b: differentChannels, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, codecsMatch(test.a, test.b))
		})
	}
}

func TestMediaSessionUpdateCodecCaseInsensitiveName(t *testing.T) {
	local := CodecAudioOpus
	local.Fmtp = "useinbandfec=1;stereo=1"
	remote := CodecAudioOpus
	remote.Name = "OPUS"

	m := MediaSession{Codecs: []Codec{local}}
	require.Equal(t, 1, m.updateRemoteCodecs([]Codec{remote}, false))
	assert.Equal(t, []Codec{local}, m.filterCodecs)
}

func TestMediaSessionUpdateSDP(t *testing.T) {
	sd := `v=0
o=- 3948988145 3948988145 IN IP4 192.168.178.54
s=Sip Go Media
c=IN IP4 192.168.178.54
t=0 0
m=audio 34391 RTP/AVP 0 8 96 101
a=rtpmap:0 PCMU/8000
a=rtpmap:8 PCMA/8000
a=rtpmap:96 opus/48000/2
a=rtpmap:101 telephone-event/8000
a=fmtp:101 0-16
a=ptime:20
a=maxptime:20
a=sendrecv`

	m := MediaSession{
		Codecs: []Codec{
			CodecAudioAlaw, CodecAudioUlaw, CodecAudioOpus, CodecTelephoneEvent8000,
		},
		Laddr: net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234},
		Mode:  "sendrecv",
	}
	require.NoError(t, m.Init())
	err := m.RemoteSDP([]byte(sd))
	require.NoError(t, err)

	require.Len(t, m.filterCodecs, 4)
	assert.Equal(t, CodecAudioUlaw, m.filterCodecs[0])
	assert.Equal(t, CodecAudioAlaw, m.filterCodecs[1])
	assert.Equal(t, CodecAudioOpus, m.filterCodecs[2])
	assert.Equal(t, CodecTelephoneEvent8000, m.filterCodecs[3])

	lsdp := m.LocalSDP()
	lsd := sdp.SessionDescription{}
	sdp.Unmarshal(lsdp, &lsd)

	// Check that order is preserved from offerrer
	assert.Equal(t, "audio 1234 RTP/AVP 0 8 96 101", lsd.Value("m"))

	// Test forking
	{
		sd = `v=0
o=- 3948988145 3948988145 IN IP4 192.168.178.54
s=Sip Go Media
c=IN IP4 192.168.178.54
t=0 0
m=audio 34391 RTP/AVP 0 96 101
a=rtpmap:0 PCMU/8000
a=rtpmap:96 opus/48000/2
a=rtpmap:101 telephone-event/8000
a=fmtp:101 0-16
a=ptime:20
a=maxptime:20
a=sendrecv`

		m = *m.Fork()
		err = m.RemoteSDP([]byte(sd))
		require.NoError(t, err)

		require.Len(t, m.filterCodecs, 3)
		assert.Equal(t, CodecAudioUlaw, m.filterCodecs[0])
		assert.Equal(t, CodecAudioOpus, m.filterCodecs[1])
		assert.Equal(t, CodecTelephoneEvent8000, m.filterCodecs[2])
	}
}

func TestRemoteSDPAudioVideoPayloadTypeCollision(t *testing.T) {
	// FreeSWITCH offer: dynamic payload types are scoped to each media section.
	// Audio 96/97/98/99 and video 96/97/98/99 deliberately have different codecs.
	sd := `v=0
o=1002 2205 1425 IN IP4 192.168.10.201
s=Talk
c=IN IP4 192.168.10.201
t=0 0
a=rtcp-xr:rcvr-rtt=all:10000 stat-summary=loss,dup,jitt,TTL voip-metrics
a=group:BUNDLE as vs
a=record:off
m=audio 55234 RTP/AVP 96 97 98 0 8 9 18 101 99 100
a=rtpmap:96 opus/48000/2
a=fmtp:96 useinbandfec=1;stereo=1
a=rtpmap:97 speex/16000
a=fmtp:97 vbr=on
a=rtpmap:98 speex/8000
a=fmtp:98 vbr=on
a=fmtp:18 annexb=yes
a=rtpmap:101 telephone-event/48000
a=rtpmap:99 telephone-event/16000
a=rtpmap:100 telephone-event/8000
a=rtcp-mux
a=mid:as
a=extmap:1 urn:ietf:params:rtp-hdrext:sdes:mid
a=rtcp:55819
a=rtcp-fb:* trr-int 1000
a=rtcp-fb:* ccm tmmbr
m=video 58426 RTP/AVP 96 97 98 99
a=rtpmap:96 AV1/90000
a=rtpmap:97 H265/90000
a=rtpmap:98 H264/90000
a=fmtp:98 profile-level-id=42801F
a=rtpmap:99 VP8/90000
a=rtcp-mux
a=mid:vs
a=extmap:1 urn:ietf:params:rtp-hdrext:sdes:mid
a=rtcp:51160
a=rtcp-fb:* trr-int 1000
a=rtcp-fb:* ccm tmmbr
a=rtcp-fb:96 nack pli
a=rtcp-fb:96 ccm fir
a=rtcp-fb:97 nack pli
a=rtcp-fb:97 ccm fir
a=rtcp-fb:98 nack pli
a=rtcp-fb:98 ccm fir
a=rtcp-fb:99 nack pli
a=rtcp-fb:99 nack sli
a=rtcp-fb:99 ack rpsi
a=rtcp-fb:99 ccm fir
`

	m := MediaSession{
		Codecs: []Codec{CodecAudioOpus, CodecAudioUlaw, CodecAudioAlaw, CodecAudioG722},
		Laddr:  net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234},
		Mode:   sdp.ModeSendrecv,
	}
	require.NoError(t, m.RemoteSDP([]byte(sd)))
	assert.Equal(t, []Codec{CodecAudioOpus, CodecAudioUlaw, CodecAudioAlaw, CodecAudioG722}, m.CommonCodecs())
}

func TestMediaNegotiaton(t *testing.T) {
	t.Run("UnsupportedRTPProfile", func(t *testing.T) {
		sd := `v=0
o=- 3948988145 3948988145 IN IP4 192.168.178.54
s=Sip Go Media
c=IN IP4 192.168.178.54
t=0 0
m=audio 34391 RTP/UNKNOWN 0 8
a=sendrecv`

		m := MediaSession{
			Codecs: []Codec{
				CodecAudioAlaw, CodecAudioUlaw, CodecAudioOpus, CodecTelephoneEvent8000,
			},
			Laddr: net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234},
			Mode:  "sendrecv",
		}
		err := m.RemoteSDP([]byte(sd))
		require.Error(t, err)
		require.Contains(t, err.Error(), "unsupported media description protocol")
	})

	t.Run("ValidRTPSDP", func(t *testing.T) {
		sd := `v=0
o=- 3948988145 3948988145 IN IP4 192.168.178.54
s=Sip Go Media
c=IN IP4 192.168.178.54
t=0 0
m=audio 34391 RTP/AVP 0 8
a=sendrecv`

		m := MediaSession{
			Codecs: []Codec{
				CodecAudioAlaw, CodecAudioUlaw, CodecAudioOpus, CodecTelephoneEvent8000,
			},
			Laddr: net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234},
			Mode:  "sendrecv",
		}
		err := m.RemoteSDP([]byte(sd))
		require.NoError(t, err)
	})

	t.Run("SecuredWithoutCrypto", func(t *testing.T) {
		sd := `v=0
o=- 3948988145 3948988145 IN IP4 192.168.178.54
s=Sip Go Media
c=IN IP4 192.168.178.54
t=0 0
m=audio 34391 RTP/SAVP 0 8
a=sendrecv`

		m := MediaSession{
			Codecs: []Codec{
				CodecAudioAlaw, CodecAudioUlaw, CodecAudioOpus, CodecTelephoneEvent8000,
			},
			Laddr:     net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234},
			Mode:      "sendrecv",
			SecureRTP: 1,
			SRTPAlg:   SRTPProfileAes128CmHmacSha1_80,
		}
		err := m.RemoteSDP([]byte(sd))
		require.Error(t, err)
		require.Contains(t, err.Error(), "remote requested secure RTP, but no context is created")
	})

	t.Run("Secured", func(t *testing.T) {
		sd := `v=0
o=- 3948988145 3948988145 IN IP4 192.168.178.54
s=Sip Go Media
c=IN IP4 192.168.178.54
t=0 0
m=audio 34391 RTP/SAVP 0 8
a=crypto:1 AES_CM_128_HMAC_SHA1_80 inline:8Dlz/SyzlAKCZwH49w5DX8S4pDa7Lw0n3LTI4t6Z
a=sendrecv`

		m := MediaSession{
			Codecs: []Codec{
				CodecAudioAlaw, CodecAudioUlaw, CodecAudioOpus, CodecTelephoneEvent8000,
			},
			Laddr:     net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234},
			Mode:      "sendrecv",
			SecureRTP: 1,
			SRTPAlg:   SRTPProfileAes128CmHmacSha1_80,
		}
		err := m.RemoteSDP([]byte(sd))
		require.NoError(t, err)
	})

}

func TestRemoteSDP_SAVPOfferMirroredWhenSAVPDisabled(t *testing.T) {
	// When RTPProfileSAVPDisable is true (opportunistic mode) and the remote
	// offers RTP/SAVP, the answer must still use RTP/SAVP per RFC 3264.
	old := RTPProfileSAVPDisable
	RTPProfileSAVPDisable = true
	t.Cleanup(func() { RTPProfileSAVPDisable = old })

	offer := `v=0
o=- 3948988145 3948988145 IN IP4 192.168.178.54
s=Sip Go Media
c=IN IP4 192.168.178.54
t=0 0
m=audio 25380 RTP/SAVP 0 8
a=crypto:1 AES_CM_128_HMAC_SHA1_80 inline:8Dlz/SyzlAKCZwH49w5DX8S4pDa7Lw0n3LTI4t6Z
a=sendrecv`

	m := MediaSession{
		Codecs:    []Codec{CodecAudioAlaw, CodecAudioUlaw},
		Laddr:     net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)},
		Mode:      "sendrecv",
		SecureRTP: 1,
		SRTPAlg:   SRTPProfileAes128CmHmacSha1_80,
	}
	require.NoError(t, m.Init())

	err := m.RemoteSDP([]byte(offer))
	require.NoError(t, err)

	answer := string(m.LocalSDP())
	assert.Contains(t, answer, "RTP/SAVP", "answer must mirror RTP/SAVP from offer when RTPProfileSAVPDisable is true")
	assert.NotRegexp(t, `m=audio \d+ RTP/AVP `, answer, "answer must not downgrade to RTP/AVP when offer is RTP/SAVP")
}

func TestRemoteSDP_AVPOfferStaysAVPWhenSAVPDisabled(t *testing.T) {
	// When RTPProfileSAVPDisable is true and the remote offers RTP/AVP,
	// the answer should use RTP/AVP (opportunistic: don't require SRTP).
	old := RTPProfileSAVPDisable
	RTPProfileSAVPDisable = true
	t.Cleanup(func() { RTPProfileSAVPDisable = old })

	offer := `v=0
o=- 3948988145 3948988145 IN IP4 192.168.178.54
s=Sip Go Media
c=IN IP4 192.168.178.54
t=0 0
m=audio 25380 RTP/AVP 0 8
a=sendrecv`

	m := MediaSession{
		Codecs:    []Codec{CodecAudioAlaw, CodecAudioUlaw},
		Laddr:     net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)},
		Mode:      "sendrecv",
		SecureRTP: 1,
		SRTPAlg:   SRTPProfileAes128CmHmacSha1_80,
	}
	require.NoError(t, m.Init())

	err := m.RemoteSDP([]byte(offer))
	require.NoError(t, err)

	answer := string(m.LocalSDP())
	assert.Regexp(t, `m=audio \d+ RTP/AVP `, answer, "answer should stay RTP/AVP when offer is RTP/AVP and SAVP is disabled")
}

func TestMediaSRTP(t *testing.T) {
	m1 := MediaSession{
		Laddr:     net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)},
		Codecs:    []Codec{CodecAudioAlaw},
		SecureRTP: 1,
		SRTPAlg:   SRTPProfileAes128CmHmacSha1_80,
		Mode:      sdp.ModeSendrecv,
	}
	require.NoError(t, m1.Init())

	m2 := MediaSession{
		Laddr:     net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)},
		Codecs:    []Codec{CodecAudioAlaw},
		SecureRTP: 1,
		SRTPAlg:   SRTPProfileAes128CmHmacSha1_80,
		Mode:      sdp.ModeSendrecv,
	}
	require.NoError(t, m2.Init())

	err := m1.RemoteSDP(m2.LocalSDP())
	require.NoError(t, err)
	err = m2.RemoteSDP(m1.LocalSDP())
	require.NoError(t, err)

	require.NotNil(t, m1.remoteCtxSRTP)
	require.NotNil(t, m1.localCtxSRTP)
	require.NotNil(t, m2.remoteCtxSRTP)
	require.NotNil(t, m2.localCtxSRTP)

	pkt := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    96,
			SequenceNumber: 1234,
			Timestamp:      56789,
			SSRC:           0xdeadbeef,
		},
		Payload: []byte("Hello SRTP!"),
	}
	// Test m1 -> m2 Read
	{
		err = m1.WriteRTP(pkt)
		require.NoError(t, err)

		rPkt := rtp.Packet{}
		m2.ReadRTP(make([]byte, 1500), &rPkt)

		assert.Equal(t, pkt.Payload, rPkt.Payload)
	}

	// Test m2 -> m1 Read
	{
		err = m2.WriteRTP(pkt)
		require.NoError(t, err)

		rPkt := rtp.Packet{}
		m1.ReadRTP(make([]byte, 1500), &rPkt)

		assert.Equal(t, pkt.Payload, rPkt.Payload)
	}
}

func TestMediaSessionRTPSymetric(t *testing.T) {
	session := &MediaSession{
		Raddr:  net.UDPAddr{IP: net.IPv4(127, 1, 1, 1), Port: 1234},
		RTPNAT: 1,
	}
	reader, writer := io.Pipe()
	session.rtpConn = &fakes.UDPConn{
		RAddr:  net.UDPAddr{IP: net.IPv4(127, 2, 2, 2), Port: 4321},
		Reader: reader,
		Writers: map[string]io.Writer{
			"127.2.2.2:4321": bytes.NewBuffer([]byte{}),
		},
	}

	go func() {
		var seq uint16 = 0
		for ; seq < 4; seq++ {
			pkt := rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					SequenceNumber: seq,
				},
			}
			data, err := pkt.Marshal()
			if err != nil {
				return
			}
			writer.Write(data)
		}
	}()

	// Writing should fail as new IP is not learned yet
	err := session.WriteRTP(&rtp.Packet{Header: rtp.Header{}, Payload: []byte{}})
	require.Error(t, err) // No writer

	pkt := rtp.Packet{}
	_, err = session.ReadRTP(make([]byte, 1600), &pkt)
	require.NoError(t, err)

	assert.Equal(t, &net.UDPAddr{IP: net.IPv4(127, 2, 2, 2), Port: 4321}, session.learnedRTPFrom.Load())
	// Writing should be successfull
	err = session.WriteRTP(&rtp.Packet{Header: rtp.Header{}, Payload: []byte{}})
	require.NoError(t, err)
	rtpLen := session.rtpConn.(*fakes.UDPConn).Writers["127.2.2.2:4321"].(*bytes.Buffer).Len()
	assert.Greater(t, rtpLen, 0)
}

func TestMediaSessionRTPSymetricPayloadTypeFilter(t *testing.T) {
	mappedSource := net.UDPAddr{IP: net.IPv4(127, 2, 2, 2), Port: 4321}
	reader, writer := io.Pipe()
	go func() {
		for _, payloadType := range []uint8{97, 96} {
			raw, err := (&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: payloadType}}).Marshal()
			if err != nil {
				return
			}
			if _, err := writer.Write(raw); err != nil {
				return
			}
		}
	}()
	session := &MediaSession{
		Raddr:              net.UDPAddr{IP: net.IPv4(127, 1, 1, 1), Port: 1234},
		RTPNAT:             RTPNATSymetric,
		RTPNATPayloadTypes: map[uint8]bool{96: true},
	}
	session.rtpConn = &fakes.UDPConn{RAddr: mappedSource, Reader: reader}

	packet := rtp.Packet{}
	_, err := session.ReadRTP(make([]byte, 1500), &packet)
	require.NoError(t, err)
	assert.Nil(t, session.learnedRTPFrom.Load(), "demuxed video must not become the audio return source")
	_, err = session.ReadRTP(make([]byte, 1500), &packet)
	require.NoError(t, err)
	assert.Equal(t, &mappedSource, session.learnedRTPFrom.Load())
}
