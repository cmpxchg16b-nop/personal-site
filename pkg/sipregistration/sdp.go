package sipregistration

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/emiago/diago/media"
	pionsdp "github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
)

const (
	maxSDPFormatsPerMedia = 128
	maxSDPCodecsPerMedia  = 32
)

var errNoCommonAudio = errors.New("no common audio codec")

// rtpCodec is one primary RTP payload description. PayloadType and Fmtp are
// deliberately retained per side: SDP answers use the offerer's values, while
// the other leg may use a different dynamic payload type for the same codec.
type rtpCodec struct {
	PayloadType uint8
	Name        string
	ClockRate   uint32
	Channels    uint16
	Fmtp        string
}

type mediaSection struct {
	Kind            string
	Port            int
	Protocol        string
	Codecs          []rtpCodec
	TelephoneEvents []rtpCodec
	ConnectionIP    net.IP

	// Formats preserves the m-line for an answer that must reject the section.
	// Direction is the media direction after inheriting the session default.
	Formats   []string
	Direction string
}

type sdpModel struct {
	Audio *mediaSection
	Video *mediaSection
}

type codecPair struct {
	Browser rtpCodec
	SIP     rtpCodec
}

// negotiatedSelection contains matches in browser preference order. The raw
// offer/answer selection functions retain exactly one audio pair and at most
// one video pair. SIPAudio and SIPVideo retain the selected remote media
// addresses for RTP relay and policy checks; RejectVideoFormats preserves an
// active SIP video offer that must be answered with port zero.
type negotiatedSelection struct {
	Audio              []codecPair
	Video              []codecPair
	DTMF               *codecPair
	SIPAudio           *mediaSection
	SIPVideo           *mediaSection
	RejectVideoFormats []string
}

// normalizeRTPCodec returns the comparison form without changing a retained
// offer or answer. An omitted channel count means one for audio (RFC 8866).
func normalizeRTPCodec(codec rtpCodec, kind string) rtpCodec {
	codec.Name = strings.TrimSpace(codec.Name)
	codec.Fmtp = strings.TrimSpace(codec.Fmtp)
	if codec.Channels == 0 && !strings.EqualFold(kind, "video") {
		codec.Channels = 1
	}
	return codec
}

func (c rtpCodec) diagoCodec(kind string) media.Codec {
	normalized := normalizeRTPCodec(c, kind)
	codec := media.Codec{
		Name:        normalized.Name,
		PayloadType: normalized.PayloadType,
		SampleRate:  normalized.ClockRate,
		NumChannels: int(normalized.Channels),
		Fmtp:        normalized.Fmtp,
	}
	if strings.EqualFold(kind, "audio") {
		// diago needs a duration for its PCM packet reader. This is a transport
		// default, not a restriction on the codecs that can be negotiated.
		codec.SampleDur = 20 * time.Millisecond
	}
	return codec
}

func (c rtpCodec) webRTCCapability(kind string) webrtc.RTPCodecCapability {
	normalized := normalizeRTPCodec(c, kind)
	return webrtc.RTPCodecCapability{
		MimeType:    kind + "/" + normalized.Name,
		ClockRate:   normalized.ClockRate,
		Channels:    normalized.Channels,
		SDPFmtpLine: normalized.Fmtp,
	}
}

func rtpCodecFromWebRTC(params webrtc.RTPCodecParameters) rtpCodec {
	name := strings.TrimSpace(params.MimeType)
	if slash := strings.LastIndexByte(name, '/'); slash >= 0 {
		name = name[slash+1:]
	}
	return rtpCodec{
		PayloadType: uint8(params.PayloadType),
		Name:        name,
		ClockRate:   params.ClockRate,
		Channels:    params.Channels,
		Fmtp:        strings.TrimSpace(params.SDPFmtpLine),
	}
}

// parseSDP parses the one-audio/one-video plaintext RTP profile supported by
// the SIP leg. A missing video m-line leaves Video nil; a rejected video m-line
// leaves a non-nil section with Port zero and its original Formats.
func parseSDP(raw []byte) (sdpModel, error) {
	return parseSDPWithProfiles(raw, false)
}

// parseBrowserSDP extracts the same primary codec capabilities from a normal
// WebRTC offer. ICE, DTLS, BUNDLE, rtcp-mux, mid, extmap, and other transport
// attributes are deliberately irrelevant here and are handled by Pion.
func parseBrowserSDP(raw []byte) (sdpModel, error) {
	return parseSDPWithProfiles(raw, true)
}

func parseSDPWithProfiles(raw []byte, browser bool) (sdpModel, error) {
	var description pionsdp.SessionDescription
	if err := description.Unmarshal(raw); err != nil {
		return sdpModel{}, fmt.Errorf("parse SDP: %w", err)
	}

	sessionIP, err := sdpConnectionIP(description.ConnectionInformation)
	if err != nil {
		return sdpModel{}, fmt.Errorf("session connection: %w", err)
	}
	sessionDirection := sdpDirection(description.Attributes, "sendrecv")

	var model sdpModel
	for _, description := range description.MediaDescriptions {
		kind := strings.ToLower(strings.TrimSpace(description.MediaName.Media))
		if kind != "audio" && kind != "video" {
			continue
		}
		if (kind == "audio" && model.Audio != nil) || (kind == "video" && model.Video != nil) {
			return sdpModel{}, fmt.Errorf("multiple %s media sections are unsupported", kind)
		}

		section, err := parseMediaSectionWithProfile(description, kind, sessionIP, sessionDirection, browser)
		if err != nil {
			return sdpModel{}, err
		}
		if kind == "audio" {
			model.Audio = section
		} else {
			model.Video = section
		}
	}
	return model, nil
}

func parseMediaSection(description *pionsdp.MediaDescription, kind string, sessionIP net.IP, sessionDirection string) (*mediaSection, error) {
	return parseMediaSectionWithProfile(description, kind, sessionIP, sessionDirection, false)
}

func parseMediaSectionWithProfile(description *pionsdp.MediaDescription, kind string, sessionIP net.IP, sessionDirection string, browser bool) (*mediaSection, error) {
	protocol := strings.ToUpper(strings.Join(description.MediaName.Protos, "/"))
	if browser {
		if !browserRTPProfile(protocol) {
			return nil, fmt.Errorf("%s media uses unsupported WebRTC profile %q", kind, protocol)
		}
	} else if protocol != "RTP/AVP" {
		return nil, fmt.Errorf("%s media uses unsupported profile %q: only RTP/AVP is supported", kind, protocol)
	}
	port := description.MediaName.Port.Value
	if port < 0 || port > 65535 {
		return nil, fmt.Errorf("%s media has invalid port %d", kind, port)
	}
	if description.MediaName.Port.Range != nil {
		return nil, fmt.Errorf("%s media port ranges are unsupported", kind)
	}
	if len(description.MediaName.Formats) > maxSDPFormatsPerMedia {
		return nil, fmt.Errorf("%s media has too many formats: %d", kind, len(description.MediaName.Formats))
	}

	connectionIP := sessionIP
	if description.ConnectionInformation != nil {
		var err error
		connectionIP, err = sdpConnectionIP(description.ConnectionInformation)
		if err != nil {
			return nil, fmt.Errorf("%s connection: %w", kind, err)
		}
	}

	mapped, fmtpLines := mediaCodecAttributes(description.Attributes)
	section := &mediaSection{
		Kind:         kind,
		Port:         port,
		Protocol:     protocol,
		ConnectionIP: cloneIP(connectionIP),
		Formats:      append([]string(nil), description.MediaName.Formats...),
		Direction:    sdpDirection(description.Attributes, sessionDirection),
	}

	seenPayloads := make(map[uint8]struct{}, len(description.MediaName.Formats))
	for _, format := range description.MediaName.Formats {
		payload64, err := strconv.ParseUint(format, 10, 7)
		if err != nil {
			return nil, fmt.Errorf("%s media has invalid RTP payload type %q", kind, format)
		}
		payloadType := uint8(payload64)
		if _, seen := seenPayloads[payloadType]; seen {
			continue
		}
		seenPayloads[payloadType] = struct{}{}

		codec, ok := mapped[payloadType]
		if !ok {
			codec, ok = staticRTPCodec(kind, payloadType)
		}
		if !ok {
			continue
		}
		codec.PayloadType = payloadType
		codec.Fmtp = strings.TrimSpace(fmtpLines[payloadType])
		if codec.Channels == 0 && kind == "audio" {
			codec.Channels = 1
		}
		if kind == "audio" && isTelephoneEvent(codec.Name) {
			section.TelephoneEvents = append(section.TelephoneEvents, codec)
			if len(section.TelephoneEvents) > maxSDPCodecsPerMedia {
				return nil, fmt.Errorf("audio media has too many telephone-event codecs")
			}
			continue
		}
		if nonPrimaryRTPCodec(codec.Name) {
			continue
		}
		section.Codecs = append(section.Codecs, codec)
		if len(section.Codecs) > maxSDPCodecsPerMedia {
			return nil, fmt.Errorf("%s media has too many primary codecs", kind)
		}
	}
	return section, nil
}

func browserRTPProfile(protocol string) bool {
	switch protocol {
	case "RTP/AVP", "RTP/AVPF", "RTP/SAVP", "RTP/SAVPF",
		"UDP/TLS/RTP/SAVP", "UDP/TLS/RTP/SAVPF":
		return true
	default:
		return false
	}
}

func mediaCodecAttributes(attributes []pionsdp.Attribute) (map[uint8]rtpCodec, map[uint8]string) {
	mapped := make(map[uint8]rtpCodec)
	fmtpLines := make(map[uint8]string)
	for _, attribute := range attributes {
		switch strings.ToLower(attribute.Key) {
		case "rtpmap":
			payloadType, codec, ok := parseRTPMap(attribute.Value)
			if ok {
				mapped[payloadType] = codec
			}
		case "fmtp":
			payload, params, ok := strings.Cut(strings.TrimSpace(attribute.Value), " ")
			if !ok {
				continue
			}
			payloadType, err := strconv.ParseUint(payload, 10, 7)
			if err == nil {
				fmtpLines[uint8(payloadType)] = strings.TrimSpace(params)
			}
		}
	}
	return mapped, fmtpLines
}

func parseRTPMap(value string) (uint8, rtpCodec, bool) {
	fields := strings.Fields(value)
	if len(fields) != 2 {
		return 0, rtpCodec{}, false
	}
	payloadType, err := strconv.ParseUint(fields[0], 10, 7)
	if err != nil {
		return 0, rtpCodec{}, false
	}
	encoding := strings.Split(fields[1], "/")
	if len(encoding) < 2 || len(encoding) > 3 || strings.TrimSpace(encoding[0]) == "" {
		return 0, rtpCodec{}, false
	}
	clockRate, err := strconv.ParseUint(encoding[1], 10, 32)
	if err != nil || clockRate == 0 {
		return 0, rtpCodec{}, false
	}
	codec := rtpCodec{
		PayloadType: uint8(payloadType),
		Name:        strings.TrimSpace(encoding[0]),
		ClockRate:   uint32(clockRate),
	}
	if len(encoding) == 3 {
		channels, err := strconv.ParseUint(encoding[2], 10, 16)
		if err != nil || channels == 0 {
			return 0, rtpCodec{}, false
		}
		codec.Channels = uint16(channels)
	}
	return uint8(payloadType), codec, true
}

// staticRTPCodec contains only the static RTP assignments needed when their
// optional rtpmap is absent. It is not a preferred or allowable codec list.
func staticRTPCodec(kind string, payloadType uint8) (rtpCodec, bool) {
	if kind != "audio" {
		return rtpCodec{}, false
	}
	switch payloadType {
	case 0:
		return rtpCodec{Name: "PCMU", ClockRate: 8000, Channels: 1}, true
	case 8:
		return rtpCodec{Name: "PCMA", ClockRate: 8000, Channels: 1}, true
	case 9:
		return rtpCodec{Name: "G722", ClockRate: 8000, Channels: 1}, true
	default:
		return rtpCodec{}, false
	}
}

func isTelephoneEvent(name string) bool {
	return strings.EqualFold(strings.TrimSpace(name), "telephone-event")
}

func nonPrimaryRTPCodec(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "telephone-event", "cn", "comfort-noise",
		"rtx", "red", "fec", "ulpfec", "x-ulpfec",
		"flexfec", "flexfec-02", "flexfec-03", "parityfec":
		return true
	default:
		return false
	}
}

func sdpConnectionIP(connection *pionsdp.ConnectionInformation) (net.IP, error) {
	if connection == nil {
		return nil, nil
	}
	if connection.Address == nil {
		return nil, errors.New("missing connection address")
	}
	ip := net.ParseIP(strings.TrimSpace(connection.Address.Address))
	if ip == nil {
		return nil, fmt.Errorf("invalid connection IP %q", connection.Address.Address)
	}
	addressType := strings.ToUpper(connection.AddressType)
	if addressType == "IP4" {
		ip = ip.To4()
		if ip == nil {
			return nil, fmt.Errorf("connection address is not IPv4: %q", connection.Address.Address)
		}
	} else if addressType == "IP6" {
		ip = ip.To16()
		if ip == nil || ip.To4() != nil {
			return nil, fmt.Errorf("connection address is not IPv6: %q", connection.Address.Address)
		}
	}
	return ip, nil
}

func sdpDirection(attributes []pionsdp.Attribute, inherited string) string {
	direction := inherited
	for _, attribute := range attributes {
		switch strings.ToLower(attribute.Key) {
		case "sendrecv", "sendonly", "recvonly", "inactive":
			direction = strings.ToLower(attribute.Key)
		}
	}
	return direction
}

func cloneIP(ip net.IP) net.IP {
	if ip == nil {
		return nil
	}
	return append(net.IP(nil), ip...)
}

func cloneMediaSection(section *mediaSection) *mediaSection {
	if section == nil {
		return nil
	}
	cloned := *section
	cloned.Codecs = append([]rtpCodec(nil), section.Codecs...)
	cloned.TelephoneEvents = append([]rtpCodec(nil), section.TelephoneEvents...)
	cloned.ConnectionIP = cloneIP(section.ConnectionIP)
	cloned.Formats = append([]string(nil), section.Formats...)
	return &cloned
}

// intersectCodecs matches primary payloads without imposing a codec
// vocabulary. The empty kind uses audio's default channel rule; negotiated
// selection calls the kind-specific form for video.
func intersectCodecs(browser, sip []rtpCodec) []codecPair {
	return intersectCodecsForKind("", browser, sip)
}

func intersectCodecsForKind(kind string, browser, sip []rtpCodec) []codecPair {
	var pairs []codecPair
	for _, browserCodec := range browser {
		if nonPrimaryRTPCodec(browserCodec.Name) {
			continue
		}
		for _, sipCodec := range sip {
			if nonPrimaryRTPCodec(sipCodec.Name) {
				continue
			}
			if rtpCodecsCompatible(kind, browserCodec, sipCodec) {
				// Keep the original values, including each side's dynamic PT and
				// fmtp spelling, rather than either normalized comparison form.
				pairs = append(pairs, codecPair{Browser: browserCodec, SIP: sipCodec})
				break
			}
		}
	}
	return pairs
}

// intersectTelephoneEvents selects the browser-preferred RFC 4733 capability
// shared by the two audio sections. It is separate from primary-codec matching:
// telephone-event rides the audio RTP stream but is never decoded as audio.
func intersectTelephoneEvents(browser, sip []rtpCodec) *codecPair {
	for _, browserCodec := range browser {
		for _, sipCodec := range sip {
			if rtpCodecsCompatible("audio", browserCodec, sipCodec) {
				return &codecPair{Browser: browserCodec, SIP: sipCodec}
			}
		}
	}
	return nil
}

func rtpCodecsCompatible(kind string, a, b rtpCodec) bool {
	a = normalizeRTPCodec(a, kind)
	b = normalizeRTPCodec(b, kind)
	if a.Name == "" || b.Name == "" || !strings.EqualFold(a.Name, b.Name) {
		return false
	}
	if a.ClockRate != b.ClockRate || a.Channels != b.Channels {
		return false
	}

	switch strings.ToLower(a.Name) {
	case "h264":
		return h264FMTPCompatible(a.Fmtp, b.Fmtp)
	case "opus":
		return opusFMTPCompatible(a.Fmtp, b.Fmtp)
	default:
		return genericFMTPCompatible(a.Fmtp, b.Fmtp)
	}
}

func parseFMTP(line string) map[string]string {
	parameters := make(map[string]string)
	for part := range strings.SplitSeq(line, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, value, _ := strings.Cut(part, "=")
		key = strings.ToLower(strings.TrimSpace(key))
		if key != "" {
			parameters[key] = strings.TrimSpace(value)
		}
	}
	return parameters
}

func normalizedFMTPNumber(parameters map[string]string, key, defaultValue string) string {
	value, ok := parameters[key]
	if !ok {
		return defaultValue
	}
	value = strings.TrimSpace(value)
	if number, err := strconv.ParseUint(value, 10, 64); err == nil {
		return strconv.FormatUint(number, 10)
	}
	return strings.ToLower(value)
}

func h264FMTPCompatible(a, b string) bool {
	aParams, bParams := parseFMTP(a), parseFMTP(b)
	if normalizedFMTPNumber(aParams, "packetization-mode", "0") != normalizedFMTPNumber(bParams, "packetization-mode", "0") {
		return false
	}
	// Profile and level remain an exact normalized comparison for this
	// milestone. level-asymmetry-allowed itself is intentionally not compared.
	aProfile := strings.ToLower(strings.TrimSpace(aParams["profile-level-id"]))
	bProfile := strings.ToLower(strings.TrimSpace(bParams["profile-level-id"]))
	return aProfile == bProfile
}

func opusFMTPCompatible(a, b string) bool {
	aParams, bParams := parseFMTP(a), parseFMTP(b)
	for _, key := range []string{"stereo", "sprop-stereo", "useinbandfec"} {
		if normalizedFMTPNumber(aParams, key, "0") != normalizedFMTPNumber(bParams, key, "0") {
			return false
		}
	}
	return true
}

func genericFMTPCompatible(a, b string) bool {
	aParams, bParams := parseFMTP(a), parseFMTP(b)
	for key, aValue := range aParams {
		if bValue, ok := bParams[key]; ok && !strings.EqualFold(aValue, bValue) {
			return false
		}
	}
	for key, bValue := range bParams {
		if aValue, ok := aParams[key]; ok && !strings.EqualFold(aValue, bValue) {
			return false
		}
	}
	return true
}

// negotiateOutboundAnswer intersects the browser's original offer with the
// SIP answer. The browser ordering remains authoritative even if the answer
// lists its matching formats in another order.
func negotiateOutboundAnswer(browserOffer, sipAnswer sdpModel) (negotiatedSelection, error) {
	return negotiateSDPModels(browserOffer, sipAnswer)
}

// negotiateInboundOffer intersects actual browser capabilities with an
// inbound SIP offer. sipCodecs on the result returns the offerer's PT/fmtp
// values for generateSIPSDP, as required for the SDP answer.
func negotiateInboundOffer(browser, sipOffer sdpModel) (negotiatedSelection, error) {
	return negotiateSDPModels(browser, sipOffer)
}

// negotiateOutboundSelection parses an actual WebRTC offer and plaintext SIP
// answer, then pins the single codec pair used by each relay direction.
func negotiateOutboundSelection(browserOffer, sipAnswer []byte) (negotiatedSelection, error) {
	browser, err := parseBrowserSDP(browserOffer)
	if err != nil {
		return negotiatedSelection{}, fmt.Errorf("parse browser offer: %w", err)
	}
	sip, err := parseSDP(sipAnswer)
	if err != nil {
		return negotiatedSelection{}, fmt.Errorf("parse SIP answer: %w", err)
	}
	selection, err := negotiateOutboundAnswer(browser, sip)
	if err != nil {
		return negotiatedSelection{}, err
	}
	return pinNegotiatedSelection(selection)
}

// negotiateInboundSelection is the inbound-offer counterpart. The returned SIP
// codec values retain the offerer's PT and fmtp for the pinned SDP answer.
func negotiateInboundSelection(browserOffer, sipOffer []byte) (negotiatedSelection, error) {
	browser, err := parseBrowserSDP(browserOffer)
	if err != nil {
		return negotiatedSelection{}, fmt.Errorf("parse browser offer: %w", err)
	}
	sip, err := parseSDP(sipOffer)
	if err != nil {
		return negotiatedSelection{}, fmt.Errorf("parse SIP offer: %w", err)
	}
	selection, err := negotiateInboundOffer(browser, sip)
	if err != nil {
		return negotiatedSelection{}, err
	}
	return pinNegotiatedSelection(selection)
}

func pinNegotiatedSelection(selection negotiatedSelection) (negotiatedSelection, error) {
	if len(selection.Audio) == 0 {
		return negotiatedSelection{}, errNoCommonAudio
	}
	selection.Audio = selection.Audio[:1]
	if len(selection.Video) > 1 {
		selection.Video = selection.Video[:1]
	}
	if selection.DTMF != nil && len(selection.Video) == 1 && selection.DTMF.SIP.PayloadType == selection.Video[0].SIP.PayloadType {
		// DTMF must not make an otherwise usable video call fail shared-socket
		// demux. Drop only the optional helper; primary codec collisions remain
		// the relay setup errors they were before DTMF support.
		selection.DTMF = nil
	}
	if selection.SIPAudio == nil || selection.SIPAudio.Port == 0 || selection.SIPAudio.ConnectionIP == nil {
		return negotiatedSelection{}, errors.New("selected SIP audio has no remote RTP address")
	}
	if len(selection.Video) == 1 && (selection.SIPVideo == nil || selection.SIPVideo.Port == 0 || selection.SIPVideo.ConnectionIP == nil) {
		return negotiatedSelection{}, errors.New("selected SIP video has no remote RTP address")
	}
	return selection, nil
}

func negotiateSDPModels(browser, sip sdpModel) (negotiatedSelection, error) {
	var selection negotiatedSelection
	if !activeMedia(browser.Audio) || !activeMedia(sip.Audio) {
		return selection, errNoCommonAudio
	}
	selection.Audio = intersectCodecsForKind("audio", browser.Audio.Codecs, sip.Audio.Codecs)
	if len(selection.Audio) == 0 {
		return negotiatedSelection{}, errNoCommonAudio
	}
	selection.DTMF = intersectTelephoneEvents(browser.Audio.TelephoneEvents, sip.Audio.TelephoneEvents)

	if activeMedia(browser.Video) && activeMedia(sip.Video) {
		selection.Video = intersectCodecsForKind("video", browser.Video.Codecs, sip.Video.Codecs)
	}
	if len(selection.Video) == 0 && activeMedia(sip.Video) {
		selection.RejectVideoFormats = append([]string{}, sip.Video.Formats...)
	}
	selection.SIPAudio = cloneMediaSection(sip.Audio)
	selection.SIPVideo = cloneMediaSection(sip.Video)
	return selection, nil
}

// activeMedia reports whether a media section can be negotiated. An inactive
// section still has a valid answer (also inactive), unlike a rejected section.
func activeMedia(section *mediaSection) bool {
	return section != nil && section.Port != 0
}

func normalizeSDPDirection(direction string) string {
	direction = strings.ToLower(strings.TrimSpace(direction))
	switch direction {
	case "sendonly", "recvonly", "sendrecv", "inactive":
		return direction
	default:
		return "sendrecv"
	}
}

func mediaDirection(section *mediaSection) string {
	if section == nil {
		return "sendrecv"
	}
	return normalizeSDPDirection(section.Direction)
}

// sipAnswerDirection applies RFC 3264's direction mapping from the offerer's
// perspective to the answerer's perspective.
func sipAnswerDirection(offerDirection string) string {
	switch strings.ToLower(strings.TrimSpace(offerDirection)) {
	case "sendonly":
		return "recvonly"
	case "recvonly":
		return "sendonly"
	case "inactive":
		return "inactive"
	default:
		return "sendrecv"
	}
}

func pairCodecs(pairs []codecPair, browserSide bool) []rtpCodec {
	codecs := make([]rtpCodec, 0, len(pairs))
	for _, pair := range pairs {
		if browserSide {
			codecs = append(codecs, pair.Browser)
		} else {
			codecs = append(codecs, pair.SIP)
		}
	}
	return codecs
}

func (s negotiatedSelection) browserCodecs() (audio, video []rtpCodec) {
	return pairCodecs(s.Audio, true), pairCodecs(s.Video, true)
}

func (s negotiatedSelection) sipCodecs() (audio, video []rtpCodec) {
	return pairCodecs(s.Audio, false), pairCodecs(s.Video, false)
}

// generateSIPSDP emits a sendrecv plaintext RTP/AVP offer using exactly the
// supplied codec descriptions. A non-nil rejectVideoFormats emits a port-0
// video m-line; nil omits video entirely. Audio telephone-event is emitted only
// when explicitly supplied; comfort-noise, repair, and preferred codecs are not.
func generateSIPSDP(bindIP net.IP, rtpPort int, audio, video []rtpCodec, rejectVideoFormats []string) []byte {
	return generateSIPSDPWithDirections(bindIP, rtpPort, audio, video, rejectVideoFormats, "sendrecv", "sendrecv")
}

func generateSIPSDPWithDirections(bindIP net.IP, rtpPort int, audio, video []rtpCodec, rejectVideoFormats []string, audioDirection, videoDirection string) []byte {
	ip, addressType := sdpBindAddress(bindIP)
	sessionID := uint64(time.Now().Unix())
	lines := []string{
		"v=0",
		fmt.Sprintf("o=- %d %d IN %s %s", sessionID, sessionID, addressType, ip),
		"s=-",
		fmt.Sprintf("c=IN %s %s", addressType, ip),
		"t=0 0",
	}
	lines = append(lines, generateSDPMediaSectionWithDirection("audio", rtpPort, audio, audioDirection)...)
	if len(video) > 0 {
		lines = append(lines, generateSDPMediaSectionWithDirection("video", rtpPort, video, videoDirection)...)
	} else if rejectVideoFormats != nil {
		lines = append(lines, "m=video 0 RTP/AVP"+sdpFormatsSuffix(rejectVideoFormats), "a=inactive")
	}
	return []byte(strings.Join(lines, "\r\n") + "\r\n")
}

func generateSDPMediaSection(kind string, port int, codecs []rtpCodec) []string {
	return generateSDPMediaSectionWithDirection(kind, port, codecs, "sendrecv")
}

func generateSDPMediaSectionWithDirection(kind string, port int, codecs []rtpCodec, direction string) []string {
	if len(codecs) == 0 {
		port = 0
	}
	formats := make([]string, 0, len(codecs))
	attributes := make([]string, 0, len(codecs)*2+1)
	for _, original := range codecs {
		codec := normalizeRTPCodec(original, kind)
		isAudioDTMF := kind == "audio" && isTelephoneEvent(codec.Name)
		if (nonPrimaryRTPCodec(codec.Name) && !isAudioDTMF) || codec.Name == "" || codec.ClockRate == 0 {
			continue
		}
		payloadType := strconv.Itoa(int(codec.PayloadType))
		formats = append(formats, payloadType)
		encoding := codec.Name + "/" + strconv.FormatUint(uint64(codec.ClockRate), 10)
		if kind == "audio" && codec.Channels > 1 {
			encoding += "/" + strconv.FormatUint(uint64(codec.Channels), 10)
		}
		attributes = append(attributes, "a=rtpmap:"+payloadType+" "+encoding)
		if codec.Fmtp != "" {
			attributes = append(attributes, "a=fmtp:"+payloadType+" "+singleSDPLine(codec.Fmtp))
		}
	}
	if len(formats) == 0 {
		port = 0
	}
	lines := []string{"m=" + kind + " " + strconv.Itoa(port) + " RTP/AVP" + sdpFormatsSuffix(formats)}
	lines = append(lines, attributes...)
	if port == 0 {
		lines = append(lines, "a=inactive")
	} else {
		lines = append(lines, "a="+normalizeSDPDirection(direction))
	}
	return lines
}

func sdpFormatsSuffix(formats []string) string {
	clean := make([]string, 0, len(formats))
	for _, format := range formats {
		if format = strings.TrimSpace(singleSDPLine(format)); format != "" {
			clean = append(clean, format)
		}
	}
	if len(clean) == 0 {
		return ""
	}
	return " " + strings.Join(clean, " ")
}

func singleSDPLine(value string) string {
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	return strings.TrimSpace(value)
}

func sdpBindAddress(ip net.IP) (string, string) {
	if ip4 := ip.To4(); ip4 != nil {
		return ip4.String(), "IP4"
	}
	if ip16 := ip.To16(); ip16 != nil {
		return ip16.String(), "IP6"
	}
	return net.IPv4zero.String(), "IP4"
}
