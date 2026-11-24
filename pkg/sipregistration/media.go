package sipregistration

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/emiago/diago"
	"github.com/emiago/diago/media"
	diagosdp "github.com/emiago/diago/media/sdp"
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

const (
	// A SIP offer travels inside the INVITE, and sipgo rejects UDP packets over
	// its MTU. Four primary audio codecs retain the usual Opus/G722/PCMU/PCMA
	// fallback set without serializing Chrome's entire capability vocabulary.
	maxSIPOfferAudioCodecs = 4
	// Two primary video codecs retain Chrome's VP8 + H264 interoperability path.
	// Helper/repair payloads are removed before either bound is applied.
	maxSIPOfferVideoCodecs = 2
)

func primaryRTPCodecs(codecs []rtpCodec) []rtpCodec {
	primary := make([]rtpCodec, 0, len(codecs))
	for _, codec := range codecs {
		if codec.Name == "" || codec.ClockRate == 0 || nonPrimaryRTPCodec(codec.Name) {
			continue
		}
		primary = append(primary, codec)
	}
	return primary
}

func sipOfferPrimaryCodecs(codecs []rtpCodec, maximum int) []rtpCodec {
	primary := primaryRTPCodecs(codecs)
	if len(primary) > maximum {
		primary = primary[:maximum]
	}
	return primary
}

func diagoCodecList(kind string, codecs []rtpCodec) []media.Codec {
	converted := make([]media.Codec, 0, len(codecs))
	for _, codec := range codecs {
		converted = append(converted, codec.diagoCodec(kind))
	}
	return converted
}

func initializeSIPMediaSession(bindIP net.IP, audio, video []rtpCodec) (*media.MediaSession, error) {
	if bindIP == nil {
		return nil, errors.New("SIP media bind IP is required")
	}
	if len(audio) == 0 {
		return nil, errNoCommonAudio
	}
	for _, audioCodec := range audio {
		for _, videoCodec := range video {
			if audioCodec.PayloadType == videoCodec.PayloadType {
				return nil, fmt.Errorf("shared SIP RTP socket cannot use payload type %d for both audio and video", audioCodec.PayloadType)
			}
		}
	}

	// diago has one RTP transport. Keep video in Codecs as well so its RTP
	// statistics layer does not discard video packets on the shared socket.
	codecs := diagoCodecList("audio", audio)
	codecs = append(codecs, diagoCodecList("video", video)...)
	allowedNATSources := make(map[uint8]bool, len(audio))
	for _, codec := range audio {
		allowedNATSources[codec.PayloadType] = true
	}
	session := &media.MediaSession{
		Codecs: codecs,
		Mode:   diagosdp.ModeSendrecv,
		Laddr:  net.UDPAddr{IP: cloneIP(bindIP)},
		// The SDP address may be a PBX's private address (for example a Docker
		// container), not the source address reachable from this runtime. Use
		// diago's symmetric RTP learning so audio and DTMF return to the actual
		// mapped source instead of disappearing into that private route.
		RTPNAT:             media.RTPNATSymetric,
		RTPNATPayloadTypes: allowedNATSources,
	}
	if err := session.Init(); err != nil {
		return nil, fmt.Errorf("initialize SIP media: %w", err)
	}
	return session, nil
}

// newOutboundSIPMedia creates the plaintext offer for the SIP leg from the
// browser's actual capabilities. The primary codecs stay bounded for UDP MTU;
// one offered 8 kHz telephone-event capability is retained for RFC 4733 DTMF.
func newOutboundSIPMedia(bindIP net.IP, browser sdpModel, wantVideo bool) (*media.MediaSession, error) {
	if !activeMedia(browser.Audio) {
		return nil, errNoCommonAudio
	}
	audio := sipOfferPrimaryCodecs(browser.Audio.Codecs, maxSIPOfferAudioCodecs)
	if len(audio) == 0 {
		return nil, errNoCommonAudio
	}
	offerAudio := append([]rtpCodec(nil), audio...)
	var video []rtpCodec
	if wantVideo && activeMedia(browser.Video) {
		video = sipOfferPrimaryCodecs(browser.Video.Codecs, maxSIPOfferVideoCodecs)
	}
	for _, dtmf := range browser.Audio.TelephoneEvents {
		if dtmf.ClockRate != dtmfClockRate {
			continue
		}
		conflicts := false
		for _, videoCodec := range video {
			if dtmf.PayloadType == videoCodec.PayloadType {
				// DTMF is optional. The shared SIP socket cannot demux equal
				// audio/video PTs, so preserve the otherwise valid video call.
				conflicts = true
				break
			}
		}
		if !conflicts {
			offerAudio = append(offerAudio, dtmf)
			break
		}
	}

	session, err := initializeSIPMediaSession(bindIP, offerAudio, video)
	if err != nil {
		return nil, err
	}
	localSDP := generateSIPSDP(bindIP, session.Laddr.Port, offerAudio, video, nil)
	if err := session.InitWithSDP(localSDP); err != nil {
		_ = session.Close()
		return nil, fmt.Errorf("pin outbound SIP SDP: %w", err)
	}
	// InitWithSDP uses diago's generic codec parser, which does not retain
	// fmtp. Restore the actual capabilities used in the pinned offer and keep
	// video available to the shared RTP demux/statistics path.
	session.Codecs = append(diagoCodecList("audio", offerAudio), diagoCodecList("video", video)...)
	return session, nil
}

// newInboundSIPMedia applies the original plaintext offer, pins the selected
// answer (including a port-zero video rejection), and injects packet-level
// reader/writers into the dialog. Dialog close stops RTCP monitoring and then
// closes the media session through diago's normal DialogMedia cleanup.
func newInboundSIPMedia(dialog *diago.DialogServerSession, bindIP net.IP, selection negotiatedSelection) (*media.MediaSession, *media.RTPSession, error) {
	if dialog == nil || dialog.DialogServerSession == nil || dialog.InviteRequest == nil {
		return nil, nil, errors.New("inbound SIP dialog and INVITE are required")
	}
	rawOffer := dialog.InviteRequest.Body()
	if len(rawOffer) == 0 {
		return nil, nil, errors.New("inbound SIP INVITE has no SDP offer")
	}
	if _, err := parseSDP(rawOffer); err != nil {
		return nil, nil, fmt.Errorf("validate inbound plaintext SIP offer: %w", err)
	}
	if len(selection.Audio) != 1 || len(selection.Video) > 1 {
		return nil, nil, errors.New("inbound selection must contain one audio and at most one video pair")
	}
	audio, video := selection.sipCodecs()
	if selection.DTMF != nil {
		audio = append(audio, selection.DTMF.SIP)
	}
	session, err := initializeSIPMediaSession(bindIP, audio, video)
	if err != nil {
		return nil, nil, err
	}
	cleanupSession := func() {
		_ = session.Close()
	}

	if err := session.RemoteSDP(rawOffer); err != nil {
		cleanupSession()
		return nil, nil, fmt.Errorf("apply inbound SIP offer: %w", err)
	}
	answer := generateSIPSDPWithDirections(
		bindIP,
		session.Laddr.Port,
		audio,
		video,
		selection.RejectVideoFormats,
		sipAnswerDirection(mediaDirection(selection.SIPAudio)),
		sipAnswerDirection(mediaDirection(selection.SIPVideo)),
	)
	if err := session.InitWithSDP(answer); err != nil {
		cleanupSession()
		return nil, nil, fmt.Errorf("pin inbound SIP answer: %w", err)
	}
	session.Codecs = append(diagoCodecList("audio", audio), diagoCodecList("video", video)...)

	rtpSession := media.NewRTPSession(session)
	reader := media.NewRTPPacketReaderSession(rtpSession)
	writer := media.NewRTPPacketWriterSession(rtpSession)
	writer.ClockDisable()
	dialog.InitMediaSession(session, reader, writer)
	dialog.SetAudioReader(reader)
	dialog.SetAudioWriter(writer)
	dialog.OnClose(rtpSession.MonitorClose)
	if err := rtpSession.MonitorBackground(); err != nil {
		_ = rtpSession.MonitorClose()
		cleanupSession()
		return nil, nil, fmt.Errorf("monitor inbound SIP RTP: %w", err)
	}
	return session, rtpSession, nil
}

// browserMedia owns the single answerer PeerConnection used for a call. Answer
// is the local answer sent to the browser as its remote description. Candidates
// received before a remote description is installed are queued in order.
type browserMedia struct {
	pc         *webrtc.PeerConnection
	audioTrack *webrtc.TrackLocalStaticRTP
	videoTrack *webrtc.TrackLocalStaticRTP
	Answer     webrtc.SessionDescription

	mu                   sync.Mutex
	remoteDescriptionSet bool
	pendingCandidates    []webrtc.ICECandidateInit
	closed               bool
	closeOnce            sync.Once
	closeErr             error
}

func browserCodecParameters(codec rtpCodec, kind string) webrtc.RTPCodecParameters {
	return webrtc.RTPCodecParameters{
		RTPCodecCapability: codec.webRTCCapability(kind),
		PayloadType:        webrtc.PayloadType(codec.PayloadType),
	}
}

func registerBrowserCodec(engine *webrtc.MediaEngine, codec rtpCodec, kind string, codecType webrtc.RTPCodecType) error {
	parameters := browserCodecParameters(codec, kind)
	if err := engine.RegisterCodec(parameters, codecType); err != nil {
		return fmt.Errorf("register browser %s codec %s/%d: %w", kind, parameters.MimeType, codec.PayloadType, err)
	}
	return nil
}

func newBrowserMedia(
	offer webrtc.SessionDescription,
	selection negotiatedSelection,
	iceServers []string,
	onCandidate func(webrtc.ICECandidateInit),
	onTrack func(*webrtc.TrackRemote),
) (*browserMedia, error) {
	if offer.Type != webrtc.SDPTypeOffer {
		return nil, errors.New("browser session description is not an offer")
	}
	if len(selection.Audio) != 1 || len(selection.Video) > 1 {
		return nil, errors.New("browser selection must contain one audio and at most one video pair")
	}

	engine := &webrtc.MediaEngine{}
	if err := registerBrowserCodec(engine, selection.Audio[0].Browser, "audio", webrtc.RTPCodecTypeAudio); err != nil {
		return nil, err
	}
	if selection.DTMF != nil {
		if err := registerBrowserCodec(engine, selection.DTMF.Browser, "audio", webrtc.RTPCodecTypeAudio); err != nil {
			return nil, err
		}
	}
	if len(selection.Video) == 1 {
		if err := registerBrowserCodec(engine, selection.Video[0].Browser, "video", webrtc.RTPCodecTypeVideo); err != nil {
			return nil, err
		}
	}
	interceptors := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(engine, interceptors); err != nil {
		return nil, fmt.Errorf("register browser interceptors: %w", err)
	}
	settings := webrtc.SettingEngine{}
	// An actpass browser offer receives setup:active, allowing answer and DTLS
	// handshake to proceed in parallel. Explicit remote roles still take priority.
	if err := settings.SetAnsweringDTLSRole(webrtc.DTLSRoleClient); err != nil {
		return nil, fmt.Errorf("set browser answering DTLS role: %w", err)
	}
	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(engine),
		webrtc.WithInterceptorRegistry(interceptors),
		webrtc.WithSettingEngine(settings),
	)
	configuration := webrtc.Configuration{}
	if len(iceServers) > 0 {
		configuration.ICEServers = []webrtc.ICEServer{{URLs: append([]string(nil), iceServers...)}}
	}
	pc, err := api.NewPeerConnection(configuration)
	if err != nil {
		return nil, fmt.Errorf("create browser PeerConnection: %w", err)
	}
	browser := &browserMedia{pc: pc}
	succeeded := false
	defer func() {
		if !succeeded {
			_ = pc.Close()
		}
	}()

	if onCandidate != nil {
		pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
			init := webrtc.ICECandidateInit{Candidate: ""}
			if candidate != nil {
				init = candidate.ToJSON()
			}
			browser.mu.Lock()
			closed := browser.closed
			browser.mu.Unlock()
			if !closed {
				// A nil Pion candidate is gathering completion. Its empty candidate
				// string is the browser-facing end-of-candidates signal.
				onCandidate(init)
			}
		})
	}
	if onTrack != nil {
		pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
			onTrack(track)
		})
	}

	browser.audioTrack, err = webrtc.NewTrackLocalStaticRTP(
		selection.Audio[0].Browser.webRTCCapability("audio"), "sip-audio", "sip",
	)
	if err != nil {
		return nil, fmt.Errorf("create browser audio track: %w", err)
	}
	audioSender, err := pc.AddTrack(browser.audioTrack)
	if err != nil {
		return nil, fmt.Errorf("add browser audio track: %w", err)
	}
	if selection.DTMF != nil {
		// AddTrack otherwise narrows the answer to the track's primary codec.
		// Pin the same primary + telephone-event preferences used by sip-bot so
		// the browser keeps its RTCDTMFSender and emits RFC 4733 on this track.
		preferences := []webrtc.RTPCodecParameters{
			browserCodecParameters(selection.Audio[0].Browser, "audio"),
			browserCodecParameters(selection.DTMF.Browser, "audio"),
		}
		pinned := false
		for _, transceiver := range pc.GetTransceivers() {
			if transceiver.Sender() != audioSender {
				continue
			}
			if err := transceiver.SetCodecPreferences(preferences); err != nil {
				return nil, fmt.Errorf("pin browser audio DTMF codec: %w", err)
			}
			pinned = true
			break
		}
		if !pinned {
			return nil, errors.New("browser audio transceiver is missing after AddTrack")
		}
	}
	if len(selection.Video) == 1 {
		browser.videoTrack, err = webrtc.NewTrackLocalStaticRTP(
			selection.Video[0].Browser.webRTCCapability("video"), "sip-video", "sip",
		)
		if err != nil {
			return nil, fmt.Errorf("create browser video track: %w", err)
		}
		if _, err = pc.AddTrack(browser.videoTrack); err != nil {
			return nil, fmt.Errorf("add browser video track: %w", err)
		}
	}

	if err := pc.SetRemoteDescription(offer); err != nil {
		return nil, fmt.Errorf("set browser offer: %w", err)
	}
	browser.mu.Lock()
	browser.remoteDescriptionSet = true
	browser.mu.Unlock()
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return nil, fmt.Errorf("create browser answer: %w", err)
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		return nil, fmt.Errorf("set browser answer: %w", err)
	}
	browser.Answer = answer
	succeeded = true
	return browser, nil
}

func cloneICECandidateInit(candidate webrtc.ICECandidateInit) webrtc.ICECandidateInit {
	cloned := candidate
	if candidate.SDPMid != nil {
		value := *candidate.SDPMid
		cloned.SDPMid = &value
	}
	if candidate.SDPMLineIndex != nil {
		value := *candidate.SDPMLineIndex
		cloned.SDPMLineIndex = &value
	}
	if candidate.UsernameFragment != nil {
		value := *candidate.UsernameFragment
		cloned.UsernameFragment = &value
	}
	return cloned
}

func (b *browserMedia) AddCandidate(candidate webrtc.ICECandidateInit) error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return net.ErrClosed
	}
	if !b.remoteDescriptionSet {
		b.pendingCandidates = append(b.pendingCandidates, cloneICECandidateInit(candidate))
		b.mu.Unlock()
		return nil
	}
	pc := b.pc
	b.mu.Unlock()
	if pc == nil {
		return errors.New("browser PeerConnection is not initialized")
	}
	return pc.AddICECandidate(candidate)
}

func (b *browserMedia) Close() error {
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.closed = true
		b.pendingCandidates = nil
		pc := b.pc
		b.mu.Unlock()
		if pc != nil {
			b.closeErr = pc.Close()
		}
	})
	return b.closeErr
}

// sipDialog is the packet-level surface shared by diago client and server
// dialogs. Tests and future service code can wrap an inbound dialog with its
// explicitly returned RTPSession when diago's private pointer is not populated.
type sipDialog interface {
	AudioReader(...diago.AudioReaderOption) (io.Reader, error)
	MediaSession() *media.MediaSession
	RTPSession() *media.RTPSession
}

type udpRTPWriter struct {
	conn *net.UDPConn
}

func (w *udpRTPWriter) WriteRTP(packet *rtp.Packet) error {
	raw, err := packet.Marshal()
	if err != nil {
		return err
	}
	n, err := w.conn.Write(raw)
	if err != nil {
		return err
	}
	if n != len(raw) {
		return io.ErrShortWrite
	}
	return nil
}

// relayRTPWriter gives each browser track its own RTP sequence/SSRC writer.
// The shared mutex serializes diago's underlying MediaSession write buffer.
type relayRTPWriter struct {
	mu               *sync.Mutex
	writer           *media.RTPPacketWriter
	clockRate        uint32
	started          bool
	previous         uint32
	dtmfAdvance      uint32
	lastDTMFDuration uint16
}

func newRelayRTPWriter(target media.RTPWriter, codec rtpCodec, kind string, mu *sync.Mutex) *relayRTPWriter {
	converted := codec.diagoCodec(kind)
	if converted.SampleDur <= 0 {
		// diago's constructor creates a ticker even for packet-driven writes.
		// This value is never used for pacing: the clock is stopped immediately.
		converted.SampleDur = 20 * time.Millisecond
	}
	writer := media.NewRTPPacketWriter(target, converted)
	writer.ClockDisable()
	if kind == "audio" {
		if rtpSession, ok := target.(*media.RTPSession); ok {
			// Keep sender reports on the primary audio clock even if the user
			// presses a DTMF key before the first audio packet arrives.
			rtpSession.PrimeWriteStats(converted, writer.SSRC)
		}
	}
	return &relayRTPWriter{mu: mu, writer: writer, clockRate: converted.SampleRate}
}

// advanceDTMF separates consecutive generated event start timestamps. The
// caller holds the shared audio write mutex. Write subtracts this compensation
// when primary audio resumes, preserving that stream's original delta clock.
func (w *relayRTPWriter) advanceDTMF(duration uint16, eventClockRate uint32) {
	if duration == 0 || eventClockRate == 0 || w.clockRate == 0 {
		return
	}
	advance := uint32((uint64(duration)*uint64(w.clockRate) + uint64(eventClockRate) - 1) / uint64(eventClockRate))
	w.writer.DelayTimestamp(advance)
	w.dtmfAdvance += advance
}

func (w *relayRTPWriter) Write(packet *rtp.Packet, sipPayloadType uint8) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	delta := packet.Timestamp
	if w.started {
		delta = packet.Timestamp - w.previous
	}
	// DelayTimestamp advances the timestamp used by the next packet. Passing a
	// zero increment to WriteSamples avoids diago applying the delta one packet
	// late; subtraction deliberately retains uint32 wraparound semantics.
	w.writer.DelayTimestamp(delta - w.dtmfAdvance)
	w.dtmfAdvance = 0
	w.started = true
	if _, err := w.writer.WriteSamples(packet.Payload, 0, packet.Marker, sipPayloadType); err != nil {
		return err
	}
	w.previous = packet.Timestamp
	w.lastDTMFDuration = 0
	return nil
}

// mediaRelay forwards RTP payloads without decoding, packet pacing, or
// transcoding. RTCP is endpoint-local: Pion and diago each generate/consume
// their own reports, and this relay never copies RTCP between the legs.
type mediaRelay struct {
	browser   *browserMedia
	selection negotiatedSelection
	reader    *media.RTPPacketReader
	session   *media.MediaSession

	audioWriter *relayRTPWriter
	dtmfWriter  *media.RTPDtmfWriter
	dtmfCh      chan rune
	videoWriter *relayRTPWriter
	videoConn   *net.UDPConn
	writeMu     *sync.Mutex

	done chan struct{}
	wg   sync.WaitGroup

	mu          sync.Mutex
	closed      bool
	remoteKinds map[webrtc.RTPCodecType]struct{}
	closeOnce   sync.Once
	closeErr    error
}

func mediaAddress(section *mediaSection, fallback net.UDPAddr) (*net.UDPAddr, error) {
	if section == nil {
		if fallback.IP == nil || fallback.Port == 0 {
			return nil, errors.New("SIP media has no remote RTP address")
		}
		address := fallback
		address.IP = cloneIP(fallback.IP)
		return &address, nil
	}
	if section.ConnectionIP == nil || section.Port == 0 {
		return nil, fmt.Errorf("SIP %s media has no remote RTP address", section.Kind)
	}
	return &net.UDPAddr{IP: cloneIP(section.ConnectionIP), Port: section.Port}, nil
}

func sameUDPAddress(a, b *net.UDPAddr) bool {
	return a.Port == b.Port && a.Zone == b.Zone && a.IP.Equal(b.IP)
}

// sipRelayReceiveAllowed reports whether the local SIP endpoint may receive
// from the remote endpoint. sipRelaySendAllowed is the converse local-send
// policy; both use the direction advertised by the remote SIP offer/answer.
func sipRelayReceiveAllowed(section *mediaSection) bool {
	switch mediaDirection(section) {
	case "sendonly", "sendrecv":
		return true
	default:
		return false
	}
}

func sipRelaySendAllowed(section *mediaSection) bool {
	switch mediaDirection(section) {
	case "recvonly", "sendrecv":
		return true
	default:
		return false
	}
}

func ensureSessionCodec(session *media.MediaSession, codec rtpCodec, kind string) error {
	converted := codec.diagoCodec(kind)
	for _, existing := range session.Codecs {
		if existing.PayloadType != converted.PayloadType {
			continue
		}
		if strings.EqualFold(existing.Name, converted.Name) && existing.SampleRate == converted.SampleRate && existing.NumChannels == converted.NumChannels {
			return nil
		}
		return fmt.Errorf("SIP payload type %d is ambiguous across selected media", converted.PayloadType)
	}
	session.Codecs = append(session.Codecs, converted)
	return nil
}

func newMediaRelay(dialog sipDialog, browser *browserMedia, selection negotiatedSelection) (*mediaRelay, error) {
	if dialog == nil || browser == nil {
		return nil, errors.New("SIP dialog and browser media are required")
	}
	if len(selection.Audio) != 1 || len(selection.Video) > 1 || browser.audioTrack == nil {
		return nil, errors.New("relay selection must contain one audio and at most one video pair")
	}
	if len(selection.Video) == 1 && browser.videoTrack == nil {
		return nil, errors.New("selected browser video track is missing")
	}
	if len(selection.Video) == 1 && selection.Audio[0].SIP.PayloadType == selection.Video[0].SIP.PayloadType {
		return nil, errors.New("shared SIP RTP socket cannot demux equal audio/video payload types")
	}
	session := dialog.MediaSession()
	if session == nil {
		return nil, errors.New("SIP dialog has no media session")
	}
	if err := ensureSessionCodec(session, selection.Audio[0].SIP, "audio"); err != nil {
		return nil, err
	}
	if selection.DTMF != nil {
		if err := ensureSessionCodec(session, selection.DTMF.SIP, "audio"); err != nil {
			return nil, err
		}
	}
	if len(selection.Video) == 1 {
		if err := ensureSessionCodec(session, selection.Video[0].SIP, "video"); err != nil {
			return nil, err
		}
	}

	audioReader, err := dialog.AudioReader()
	if err != nil {
		return nil, fmt.Errorf("get SIP RTP reader: %w", err)
	}
	reader, ok := audioReader.(*media.RTPPacketReader)
	if !ok || reader == nil {
		return nil, errors.New("SIP audio reader is not an RTP packet reader")
	}

	relay := &mediaRelay{
		browser:     browser,
		selection:   selection,
		reader:      reader,
		session:     session,
		writeMu:     &sync.Mutex{},
		done:        make(chan struct{}),
		remoteKinds: make(map[webrtc.RTPCodecType]struct{}),
	}
	if sipRelaySendAllowed(selection.SIPAudio) {
		var audioTarget media.RTPWriter = session
		if rtpSession := dialog.RTPSession(); rtpSession != nil {
			audioTarget = rtpSession
		}
		relay.audioWriter = newRelayRTPWriter(audioTarget, selection.Audio[0].SIP, "audio", relay.writeMu)
		if selection.DTMF != nil {
			// Match sip-bot: parse/dedupe browser events, then re-originate a
			// fresh RFC 4733 series on the SIP audio RTP timeline. Diago's
			// writer uses the negotiated 8 kHz payload type and shares the
			// primary audio packet writer/SSRC.
			codec := selection.DTMF.SIP.diagoCodec("audio")
			relay.dtmfWriter = media.NewRTPDTMFWriter(codec, relay.audioWriter.writer, nil)
			relay.dtmfCh = make(chan rune, 16)
		}
	}

	if len(selection.Video) == 1 && sipRelaySendAllowed(selection.SIPVideo) {
		audioAddress, err := mediaAddress(selection.SIPAudio, session.Raddr)
		if err != nil {
			return nil, err
		}
		videoAddress, err := mediaAddress(selection.SIPVideo, session.Raddr)
		if err != nil {
			return nil, err
		}
		var videoTarget media.RTPWriter = session
		if !sameUDPAddress(audioAddress, videoAddress) {
			local := &net.UDPAddr{IP: cloneIP(session.Laddr.IP), Zone: session.Laddr.Zone}
			relay.videoConn, err = net.DialUDP("udp", local, videoAddress)
			if err != nil {
				return nil, fmt.Errorf("connect demuxed SIP video RTP: %w", err)
			}
			videoTarget = &udpRTPWriter{conn: relay.videoConn}
		}
		relay.videoWriter = newRelayRTPWriter(videoTarget, selection.Video[0].SIP, "video", relay.writeMu)
	}

	// Start background workers only after every fallible setup step succeeds.
	if relay.dtmfCh != nil {
		relay.wg.Add(1)
		go relay.sendDigits()
	}
	if sipRelayReceiveAllowed(selection.SIPAudio) || (len(selection.Video) == 1 && sipRelayReceiveAllowed(selection.SIPVideo)) {
		relay.wg.Add(1)
		go relay.sipToBrowser()
	}
	return relay, nil
}

func (r *mediaRelay) sipToBrowser() {
	defer r.wg.Done()
	buffer := make([]byte, media.RTPBufSize)
	for {
		select {
		case <-r.done:
			return
		default:
		}
		n, err := r.reader.Read(buffer)
		if err != nil {
			return
		}
		if n == 0 {
			continue
		}

		header := r.reader.PacketHeader
		sipPayloadType := header.PayloadType
		var track *webrtc.TrackLocalStaticRTP
		var browserPayloadType uint8
		switch {
		case sipPayloadType == r.selection.Audio[0].SIP.PayloadType:
			if !sipRelayReceiveAllowed(r.selection.SIPAudio) {
				continue
			}
			track = r.browser.audioTrack
			browserPayloadType = r.selection.Audio[0].Browser.PayloadType
		case len(r.selection.Video) == 1 && sipPayloadType == r.selection.Video[0].SIP.PayloadType:
			if !sipRelayReceiveAllowed(r.selection.SIPVideo) {
				continue
			}
			track = r.browser.videoTrack
			browserPayloadType = r.selection.Video[0].Browser.PayloadType
		default:
			continue
		}
		header.PayloadType = browserPayloadType
		packet := &rtp.Packet{Header: header, Payload: buffer[:n]}
		if err := track.WriteRTP(packet); err != nil {
			return
		}
	}
}

// AddTrack starts exactly one browser-to-SIP goroutine for each negotiated
// kind. Unexpected duplicate tracks and tracks added after Close are ignored.
func (r *mediaRelay) AddTrack(track *webrtc.TrackRemote) {
	if track == nil {
		return
	}
	kind := track.Kind()
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	if _, exists := r.remoteKinds[kind]; exists {
		r.mu.Unlock()
		return
	}
	var writer *relayRTPWriter
	var pair codecPair
	var dtmf *codecPair
	switch kind {
	case webrtc.RTPCodecTypeAudio:
		writer = r.audioWriter
		pair = r.selection.Audio[0]
		dtmf = r.selection.DTMF
	case webrtc.RTPCodecTypeVideo:
		if len(r.selection.Video) != 1 {
			r.mu.Unlock()
			return
		}
		writer = r.videoWriter
		pair = r.selection.Video[0]
	default:
		r.mu.Unlock()
		return
	}
	r.remoteKinds[kind] = struct{}{}
	if writer == nil {
		// The browser may still negotiate a sending track even when the SIP
		// direction prohibits forwarding it.
		r.mu.Unlock()
		return
	}
	r.wg.Add(1)
	r.mu.Unlock()
	go r.browserToSIP(track, writer, pair, dtmf)
}

func (r *mediaRelay) browserToSIP(track *webrtc.TrackRemote, writer *relayRTPWriter, pair codecPair, dtmf *codecPair) {
	defer r.wg.Done()
	var dedup dtmfDeduper
	for {
		select {
		case <-r.done:
			return
		default:
		}
		packet, _, err := track.ReadRTP()
		if err != nil {
			return
		}
		if dtmf != nil && packet.PayloadType == dtmf.Browser.PayloadType {
			event, ok := parseDTMFEvent(packet.Payload)
			if !ok {
				continue
			}
			if !event.end {
				continue
			}
			digit, ok := event.digit()
			if !ok || !dedup.freshEnd(event.code, packet.Timestamp) {
				continue
			}
			select {
			case r.dtmfCh <- digit:
			default: // a flooded pad drops digits, never the audio pump
			}
			continue
		}
		if packet.PayloadType != pair.Browser.PayloadType {
			continue
		}
		if err := writer.Write(packet, pair.SIP.PayloadType); err != nil {
			return
		}
	}
}

// sendDigits serializes fresh DTMF events with primary audio on the SIP RTP
// writer. WriteDTMF takes real time, so it runs outside the browser read pump.
func (r *mediaRelay) sendDigits() {
	defer r.wg.Done()
	for {
		select {
		case digit := <-r.dtmfCh:
			r.writeMu.Lock()
			if r.audioWriter.lastDTMFDuration > 0 {
				r.audioWriter.advanceDTMF(r.audioWriter.lastDTMFDuration, r.selection.DTMF.SIP.ClockRate)
			}
			err := r.dtmfWriter.WriteDTMF(digit)
			if err == nil {
				// diago's standard series ends at 800 units (100 ms at 8 kHz).
				r.audioWriter.lastDTMFDuration = 800
			}
			r.writeMu.Unlock()
			if err != nil {
				return
			}
		case <-r.done:
			return
		}
	}
}

// Close stops all relay goroutines and closes only resources owned by the
// relay. The SIP dialog retains ownership of its MediaSession/RTPSession.
func (r *mediaRelay) Close() error {
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.closed = true
		r.mu.Unlock()
		close(r.done)

		var stopErr, videoErr, browserErr error
		if r.session != nil {
			// Expire only the read deadline to unblock RTPPacketReader. The dialog
			// remains responsible for closing the underlying RTP/RTCP sockets.
			stopErr = r.session.StopRTP(1, 0)
		}
		if r.browser != nil {
			// Closing Pion interrupts every blocked TrackRemote.ReadRTP.
			browserErr = r.browser.Close()
		}
		if r.videoConn != nil {
			videoErr = r.videoConn.Close()
		}
		r.wg.Wait()
		r.closeErr = errors.Join(stopErr, videoErr, browserErr)
	})
	return r.closeErr
}
