package sipbot

// This file is the relay's codec vocabulary: one audio codec per call,
// negotiated end to end between the browser and the SIP UA (docs/sip-
// bot.md §8), plus telephone-event alongside (which rides the signalling
// of both legs and needs no mapping here — rtc.TelephoneEventCodec on
// the webrtc leg, media.CodecTelephoneEvent8000 on the sip leg). What
// lives here is the mapping between the two SDP dialects' spellings of
// that vocabulary:
//
//   - the presumed WebRTC codec list — the outbound SIP offer's codec
//     list and the inbound call's selection order among the caller's
//     offer — parsed from the configuration's comma-separated names;
//   - sipCodecByName / webrtcCodecParams: a codec's diago (media.Codec)
//     and pion (webrtc.RTPCodecParameters) shapes, so the negotiated
//     codec pins both the call's webrtc-leg track and its transceiver
//     preferences;
//   - audioCodecsFromSDP: the inbound INVITE's offered codec list, read
//     with diago's own parser;
//   - selectCodec: the end-to-end selection — the first presumed codec
//     the other leg's list carries;
//   - packetDuration: one payload's media duration for the browser-
//     bound track's timestamps (the opus packet's own TOC; the 64 kbps
//     8 kHz codecs by length).

import (
	"fmt"
	"strings"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/diago/media/sdp"
	"github.com/pion/webrtc/v4"
)

// defaultWebRTCCodecs is the presumed WebRTC codec list when the
// configuration names none: every audio codec a browser offers, in the
// browsers' own preference order.
var defaultWebRTCCodecs = []string{"opus", "g722", "pcmu", "pcma"}

// ptimeDefault is the duration a payload falls back to when its own
// shape cannot say — the ptime the SDP world defaults to.
const ptimeDefault = 20 * time.Millisecond

// ParseWebRTCCodecs parses the webrtcCodecs configuration attribute: a
// comma-separated list of codec names ("opus,g722,pcmu,pcma" — case-
// insensitive), empty selecting the default. An unknown or empty name,
// or an empty resulting list, is an error — a misconfiguration the
// wiring fails startup on (the ParseIPPreference discipline).
func ParseWebRTCCodecs(csv string) ([]string, error) {
	if strings.TrimSpace(csv) == "" {
		return defaultWebRTCCodecs, nil
	}
	names := strings.Split(csv, ",")
	out := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.ToLower(strings.TrimSpace(name))
		if _, ok := sipCodecByName(name); !ok {
			return nil, fmt.Errorf("unknown webrtc codec %q (known: %s)", name, strings.Join(defaultWebRTCCodecs, ","))
		}
		out = append(out, name)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("the webrtc codec list is empty")
	}
	return out, nil
}

// sipCodecByName is the configuration name → the sip-leg's codec (diago's
// media.Codec). Opus maps to the bot's stereo-annotated constant (the
// webrtc-leg's stereo negotiation mirrored onto the SIP side — see
// sipbot.go).
func sipCodecByName(name string) (media.Codec, bool) {
	switch name {
	case "opus":
		return codecAudioOpusStereo, true
	case "g722":
		return media.CodecAudioG722, true
	case "pcmu":
		return media.CodecAudioUlaw, true
	case "pcma":
		return media.CodecAudioAlaw, true
	}
	return media.Codec{}, false
}

// webrtcCodecParams is the codec's webrtc-leg shape (pion's
// RTPCodecParameters) for the codec pin: a zero PayloadType means the
// peer connection's MediaEngine assignment — pion substitutes it when
// the preferences are read (the pin validates against the MediaEngine,
// which the bot's factory stocks with exactly this vocabulary).
func webrtcCodecParams(codec media.Codec) (webrtc.RTPCodecParameters, bool) {
	switch codec.Name {
	case media.CodecAudioOpus.Name:
		return webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeOpus,
			ClockRate:   48000,
			Channels:    2,
			SDPFmtpLine: "minptime=10;useinbandfec=1;stereo=1",
		}}, true
	case media.CodecAudioG722.Name:
		return webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeG722,
			ClockRate: 8000,
		}}, true
	case media.CodecAudioUlaw.Name:
		return webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypePCMU,
			ClockRate: 8000,
		}}, true
	case media.CodecAudioAlaw.Name:
		return webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypePCMA,
			ClockRate: 8000,
		}}, true
	}
	return webrtc.RTPCodecParameters{}, false
}

// audioCodecsFromSDP reads an SDP body's audio codec list — the inbound
// INVITE's offer — with diago's own parser: the m= line's formats,
// resolved through the rtpmap lines (and the static payload types'
// shorthands, G.722's included).
func audioCodecsFromSDP(body []byte) ([]media.Codec, error) {
	sd := sdp.SessionDescription{}
	if err := sdp.Unmarshal(body, &sd); err != nil {
		return nil, fmt.Errorf("the SDP offer does not parse: %w", err)
	}
	md, err := sd.MediaDescription("audio")
	if err != nil {
		return nil, fmt.Errorf("the SDP offer carries no audio: %w", err)
	}
	codecs := make([]media.Codec, len(md.Formats))
	n, err := media.CodecsFromSDPRead(md.Formats, sd.Values("a"), codecs)
	if n == 0 {
		if err != nil {
			return nil, fmt.Errorf("the SDP offer's codecs do not parse: %w", err)
		}
		return nil, fmt.Errorf("the SDP offer carries no codecs")
	}
	return codecs[:n], nil
}

// selectCodec is the end-to-end selection: the first codec of the
// presumed WebRTC codec list present in the other leg's list, matched by
// name and RTP clock rate. The presumed list's order IS the browsers'
// preference order, so the selection coincides with what the browser
// itself would have picked from the list.
func selectCodec(presumed, offered []media.Codec) (media.Codec, bool) {
	for _, p := range presumed {
		for _, o := range offered {
			if strings.EqualFold(p.Name, o.Name) && p.SampleRate == o.SampleRate {
				return p, true
			}
		}
	}
	return media.Codec{}, false
}

// packetDuration is one relayed payload's media duration — the browser-
// bound track's sample duration. Opus carries its own in the TOC byte
// (RFC 6716 §3.1), so a non-20 ms source still timestamps exactly; the
// 64 kbps 8 kHz codecs (G.711, G.722) run 8 bytes per millisecond.
func packetDuration(codec media.Codec, payload []byte) time.Duration {
	if codec.Name == media.CodecAudioOpus.Name {
		return opusPacketDuration(payload)
	}
	if d := time.Duration(len(payload)/8) * time.Millisecond; d > 0 {
		return d
	}
	return ptimeDefault
}

// opusPacketDuration is the packet's media duration from its TOC byte
// (RFC 6716 §3.1): the configuration's frame duration times the frame
// count (the count rides the payload's second byte for a code-3
// packet). Anything malformed answers 20 ms — the ptime the SDP world
// defaults to.
func opusPacketDuration(pkt []byte) time.Duration {
	if len(pkt) == 0 {
		return ptimeDefault
	}
	config := pkt[0] >> 3
	var frame time.Duration
	switch {
	case config < 12: // SILK: 10, 20, 40, 60 ms
		frame = []time.Duration{10, 20, 40, 60}[config&3] * time.Millisecond
	case config < 16: // hybrid: 10, 20 ms
		frame = []time.Duration{10, 20}[config&1] * time.Millisecond
	default: // CELT: 2.5, 5, 10, 20 ms
		frame = []time.Duration{2500, 5000, 10000, 20000}[config&3] * time.Microsecond
	}
	count := 1
	switch pkt[0] & 0x3 {
	case 1, 2:
		count = 2
	case 3:
		if len(pkt) < 2 {
			return ptimeDefault
		}
		count = int(pkt[1]>>2) & 0x3F
		if count == 0 {
			return ptimeDefault
		}
	}
	d := frame * time.Duration(count)
	if d <= 0 || d > 120*time.Millisecond {
		return ptimeDefault
	}
	return d
}
