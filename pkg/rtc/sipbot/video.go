package sipbot

// This file is the video half of the relay: the /video-call command's and
// the inbound video call's sip-leg mechanics (docs/sip-bot.md §8). A video
// call is the audio call's twin with a second m-line — the bot's SDP offer
// carries audio + video (muxed on ONE RTP port), and an inbound offer's
// video m-line is answered in kind. Media stays transcoding-free: the
// callee's video packets cross to the browser byte for byte (the track
// re-stamps only SSRC/PT), and the browser's camera crosses back
// re-packetized at the sip-leg's negotiated video payload type.
//
// What lives here:
//
//   - the presumed WebRTC video codec list — the sip-leg offer's video
//     codec list and the inbound selection order, parsed from the
//     configuration's comma-separated names (the audio list's mirror);
//   - sipVideoCodecByName / webrtcVideoCodecParams: a video codec's diago
//     and pion shapes (the pin of the call's webrtc-leg video track);
//   - the video SDP reads: selectVideoFromAnswer (the callee's pick and
//     address, outbound) and selectVideoFromOffer (the browser-preferred
//     selection at the offer's PT, inbound) — diago's own parser;
//   - the SDP generators replicating diago's generateSDPForAudio shape,
//     extended with the video section (one shared port; the answer's
//     port-0 rejection when no video codec matched);
//   - the hand-built media session injection: diago's own exported seams
//     (MediaSession.Init → our SDP → InitWithSDP → the dialog's
//     InitMediaSession) put OUR audio+video SDP on the wire where diago's
//     own generator speaks audio only — no diago patch (docs/sip-bot.md
//     §8, "the static-SDP injection seam");
//   - the browser→SIP video writers: the muxed one (the dialog's own
//     socket, MediaSession.WriteRTPRaw) and the demuxed one (a dedicated
//     socket, for a peer that answered video on another port).

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/sipgo/sip"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// defaultWebRTCVideoCodecs is the presumed WebRTC video codec list when
// the configuration names none: VP8 first (every browser, royalty-free,
// no fmtp), then H.264 (the SIP world's common denominator).
var defaultWebRTCVideoCodecs = []string{"vp8", "h264"}

// ParseWebRTCVideoCodecs parses the webrtcVideoCodecs configuration
// attribute: a comma-separated list of video codec names ("vp8,h264" —
// case-insensitive), empty selecting the default. An unknown or empty
// name, or an empty resulting list, is an error — the ParseWebRTCCodecs
// discipline.
func ParseWebRTCVideoCodecs(csv string) ([]string, error) {
	if strings.TrimSpace(csv) == "" {
		return defaultWebRTCVideoCodecs, nil
	}
	names := strings.Split(csv, ",")
	out := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.ToLower(strings.TrimSpace(name))
		if _, ok := sipVideoCodecByName(name); !ok {
			return nil, fmt.Errorf("unknown webrtc video codec %q (known: %s)", name, strings.Join(defaultWebRTCVideoCodecs, ","))
		}
		out = append(out, name)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("the webrtc video codec list is empty")
	}
	return out, nil
}

// sipVideoCodecByName is the configuration name → the sip-leg's video
// codec (diago's media.Codec). The payload types 102+ are clear of the
// audio offer's 0/8/9/96/101 (a customized audio list must keep that
// true — docs/sip-bot.md §10). SampleDur is cosmetic (the video writer's
// ticker is disabled — the camera pump feeds explicit timestamp deltas),
// set to the usual 33 ms frame.
func sipVideoCodecByName(name string) (media.Codec, bool) {
	switch name {
	case "vp8":
		return media.Codec{Name: "VP8", PayloadType: 102, SampleRate: 90000, SampleDur: 33 * time.Millisecond}, true
	case "h264":
		return media.Codec{Name: "H264", PayloadType: 103, SampleRate: 90000, SampleDur: 33 * time.Millisecond, Fmtp: "packetization-mode=1;profile-level-id=42e01f"}, true
	}
	return media.Codec{}, false
}

// webrtcVideoCodecParams is the video codec's webrtc-leg shape (pion's
// RTPCodecParameters) for the codec pin: a zero PayloadType means the
// peer connection's MediaEngine assignment, exactly as the audio pin
// (webrtcCodecParams). The H.264 pin names the constrained-baseline
// packetization-mode-1 entry the bot's factory registers (pion's PT-106
// variant), so the negotiated profile is decodable on both legs.
func webrtcVideoCodecParams(codec media.Codec) (webrtc.RTPCodecParameters, bool) {
	switch codec.Name {
	case "VP8":
		return webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeVP8,
			ClockRate: 90000,
		}}, true
	case "H264":
		return webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH264,
			ClockRate:   90000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
		}}, true
	}
	return webrtc.RTPCodecParameters{}, false
}

// videoOfferSelection is selectVideoFromOffer's read of the inbound
// INVITE's video section.
type videoOfferSelection struct {
	// offered says the offer carries a video m-line at all (the answer
	// must then carry one too — RFC 3264's m-line parity); formats keeps
	// the offered format list for the rejection line when nothing
	// matched. ok says a presumed video codec matched on a live port:
	// codec is the selection — the presumed entry at the OFFER's payload
	// type and fmtp (the answer echoes them) — and raddr the offerer's
	// video address.
	codec   media.Codec
	raddr   *net.UDPAddr
	formats []string
	offered bool
	ok      bool
}

// selectVideoFromOffer reads the caller's offered video section for the
// inbound call: the first PRESUMED codec the offer carries (the presumed
// list's order is the browsers' preference order — the audio selection's
// mirror), the offer's payload type adopted so the answer echoes it (RFC
// 3264), and the offered fmtp adopted so the answer echoes that too (the
// H.264 profile the caller promised to decode).
func selectVideoFromOffer(body []byte, presumed []media.Codec) videoOfferSelection {
	var sel videoOfferSelection
	sd := sdp.SessionDescription{}
	if err := sdp.Unmarshal(body, &sd); err != nil {
		return sel
	}
	md, err := sd.MediaDescription("video")
	if err != nil {
		return sel // no video m-line — an audio-only call
	}
	sel.offered = true
	sel.formats = md.Formats
	if md.Port == 0 {
		return sel // a disabled video section stays rejected
	}
	ci, err := sd.ConnectionInformation()
	if err != nil {
		return sel
	}
	sel.raddr = &net.UDPAddr{IP: ci.IP, Port: md.Port}
	codecs := make([]media.Codec, len(md.Formats))
	n, _ := media.CodecsFromSDPRead(md.Formats, sd.Values("a"), codecs)
	for _, p := range presumed {
		for _, o := range codecs[:n] {
			if strings.EqualFold(p.Name, o.Name) && p.SampleRate == o.SampleRate {
				p.PayloadType = o.PayloadType
				if fmtp := sdpFmtpOf(sd.Values("a"), o.PayloadType); fmtp != "" {
					p.Fmtp = fmtp
				}
				sel.codec, sel.ok = p, true
				return sel
			}
		}
	}
	return sel
}

// selectVideoFromAnswer reads the callee's answered video section for the
// outbound /video-call: the answer's first video codec matching the
// presumed list by name and rate (the callee's pick — the answer's own
// order), at the answer's payload type, and the answer's video address
// (the connection IP plus the video m-line's port — a peer answering on
// another port gets the bot's video sent there). False when the answer
// carries no video m-line or a rejected (port 0) one — the call degrades
// to voice.
func selectVideoFromAnswer(body []byte, presumed []media.Codec) (media.Codec, *net.UDPAddr, bool) {
	sd := sdp.SessionDescription{}
	if err := sdp.Unmarshal(body, &sd); err != nil {
		return media.Codec{}, nil, false
	}
	md, err := sd.MediaDescription("video")
	if err != nil || md.Port == 0 {
		return media.Codec{}, nil, false
	}
	ci, err := sd.ConnectionInformation()
	if err != nil {
		return media.Codec{}, nil, false
	}
	codecs := make([]media.Codec, len(md.Formats))
	n, _ := media.CodecsFromSDPRead(md.Formats, sd.Values("a"), codecs)
	for _, o := range codecs[:n] {
		for _, p := range presumed {
			if strings.EqualFold(p.Name, o.Name) && p.SampleRate == o.SampleRate {
				p.PayloadType = o.PayloadType
				return p, &net.UDPAddr{IP: ci.IP, Port: md.Port}, true
			}
		}
	}
	return media.Codec{}, nil, false
}

// sdpFmtpOf greps the SDP's flattened attribute list for one payload
// type's a=fmtp parameters (CodecsFromSDPRead resolves rtpmap but not
// fmtp).
func sdpFmtpOf(attrs []string, pt uint8) string {
	pref := "fmtp:" + strconv.Itoa(int(pt)) + " "
	for _, a := range attrs {
		if strings.HasPrefix(a, pref) {
			return strings.TrimSpace(a[len(pref):])
		}
	}
	return ""
}

// The SDP generators. They replicate diago's generateSDPForAudio shape
// (media_session.go) — the session head, the audio section with its
// well-known rtpmap/fmtp spellings, the ptime/maxptime tail — so a
// bot-generated SDP is indistinguishable from diago's own save the video
// section it cannot speak. Plain RTP/AVP only: the bot's sip leg never
// does SDES or DTLS (diago's media config for the bot sets neither).

// generateSIPOffer is the outbound INVITE's SDP body: the audio section
// (the presumed audio list plus telephone-event) and the video section
// (the presumed video list) sharing ONE RTP port — the muxed media the
// session's single socket serves.
func generateSIPOffer(originIP, connIP net.IP, port int, audio, video []media.Codec) []byte {
	lines := sdpSessionHead(originIP, connIP)
	lines = append(lines, sdpAudioSection(port, audio)...)
	lines = append(lines, sdpVideoSection(port, video)...)
	return []byte(strings.Join(lines, "\r\n") + "\r\n")
}

// generateSIPAnswer is the inbound call's 200 OK SDP body: the audio
// section from the negotiated filter (exactly the selection, as the
// native answer's — docs/sip-bot.md §7.2), and the video section the
// selected codec at the offer's payload type — or the port-0 rejection
// carrying the offered format list (RFC 3264 keeps it) when no presumed
// video codec matched.
func generateSIPAnswer(originIP, connIP net.IP, port int, audio []media.Codec, sel videoOfferSelection) []byte {
	lines := sdpSessionHead(originIP, connIP)
	lines = append(lines, sdpAudioSection(port, audio)...)
	if sel.ok {
		lines = append(lines, sdpVideoSection(port, []media.Codec{sel.codec})...)
	} else {
		lines = append(lines, fmt.Sprintf("m=video 0 RTP/AVP %s", strings.Join(sel.formats, " ")))
	}
	return []byte(strings.Join(lines, "\r\n") + "\r\n")
}

// sdpSessionHead is the SDP's session-level lines, diago's shape: v=0,
// the o= line with the NTP-seconds session id (the full 64-bit NTP value
// breaks strict peers — diago's own shift), s=Sip Go Media, c=, t=0 0.
func sdpSessionHead(originIP, connIP net.IP) []string {
	sid := media.GetCurrentNTPTimestamp() >> 32
	return []string{
		"v=0",
		fmt.Sprintf("o=- %d %d IN %s %s", sid, sid, sdpIPFamily(originIP), originIP),
		"s=Sip Go Media",
		fmt.Sprintf("c=IN %s %s", sdpIPFamily(connIP), connIP),
		"t=0 0",
	}
}

// sdpAudioSection is the m=audio section of the bot's codec list,
// replicating generateSDPForAudio's well-known payload-type spellings
// (PCMU/PCMA/G.722/opus/telephone-event, the fmtp defaults included).
func sdpAudioSection(port int, codecs []media.Codec) []string {
	fmts := make([]string, 0, len(codecs))
	attrs := []string{}
	for _, f := range codecs {
		fmts = append(fmts, strconv.Itoa(int(f.PayloadType)))
		switch f.PayloadType {
		case media.CodecAudioUlaw.PayloadType:
			attrs = append(attrs, "a=rtpmap:0 PCMU/8000")
		case media.CodecAudioAlaw.PayloadType:
			attrs = append(attrs, "a=rtpmap:8 PCMA/8000")
		case media.CodecAudioG722.PayloadType:
			attrs = append(attrs, "a=rtpmap:9 G722/8000")
		case media.CodecAudioOpus.PayloadType:
			attrs = append(attrs, "a=rtpmap:96 opus/48000/2")
			attrs = append(attrs, sdpFmtpLine(f, "useinbandfec=0"))
		case media.CodecTelephoneEvent8000.PayloadType:
			attrs = append(attrs, "a=rtpmap:101 telephone-event/8000")
			attrs = append(attrs, sdpFmtpLine(f, "0-16"))
		default:
			attrs = append(attrs, fmt.Sprintf("a=rtpmap:%d %s/%d/%d", f.PayloadType, f.Name, f.SampleRate, f.NumChannels))
			if f.Fmtp != "" {
				attrs = append(attrs, sdpFmtpLine(f, ""))
			}
		}
	}
	lines := []string{fmt.Sprintf("m=audio %d RTP/AVP %s", port, strings.Join(fmts, " "))}
	lines = append(lines, attrs...)
	return append(lines, "a=ptime:20", "a=maxptime:20", "a=sendrecv")
}

// sdpVideoSection is the m=video section: the codecs' rtpmap lines (a
// video rtpmap carries no channel count), the fmtp lines of those that
// have one (H.264's profile), and sendrecv.
func sdpVideoSection(port int, codecs []media.Codec) []string {
	fmts := make([]string, 0, len(codecs))
	attrs := []string{}
	for _, f := range codecs {
		fmts = append(fmts, strconv.Itoa(int(f.PayloadType)))
		attrs = append(attrs, fmt.Sprintf("a=rtpmap:%d %s/%d", f.PayloadType, f.Name, f.SampleRate))
		if f.Fmtp != "" {
			attrs = append(attrs, sdpFmtpLine(f, ""))
		}
	}
	lines := []string{fmt.Sprintf("m=video %d RTP/AVP %s", port, strings.Join(fmts, " "))}
	lines = append(lines, attrs...)
	return append(lines, "a=sendrecv")
}

// sdpFmtpLine renders a codec's a=fmtp line: the codec's own Fmtp when
// set, otherwise the built-in default — diago's codecFmtpLine behavior
// (its patch 1), mirrored because the helper is not exported.
func sdpFmtpLine(f media.Codec, def string) string {
	params := f.Fmtp
	if params == "" {
		params = def
	}
	return "a=fmtp:" + strconv.Itoa(int(f.PayloadType)) + " " + params
}

// sdpIPFamily is the c=/o= line's address-type token for the address.
func sdpIPFamily(ip net.IP) string {
	if ip.To4() != nil {
		return "IP4"
	}
	return "IP6"
}

// newVideoMediaSession builds the outbound /video-call's media session:
// diago's own session construction (the account's bind and external IPs,
// the presumed codec list) run by hand so the SDP it puts on the wire is
// OURS — Init opens the RTP/RTCP sockets (assigning the one muxed port),
// the offer is generated from the session's own addresses, InitWithSDP
// pins it as the session's static LocalSDP (what the dialog's INVITE then
// sends), and the video codecs join sess.Codecs so the read path's
// per-SSRC stats gate admits the muxed video packets (it drops an unknown
// payload type on an SSRC switch — every alternating video packet would
// hit it). The dialog gets the session through InitMediaSession before
// Invite; diago's answer path then negotiates and runs its RTPSession on
// it untouched (docs/sip-bot.md §8, "the static-SDP injection seam").
func (s sipStack) newVideoMediaSession(ctx context.Context, session UserSession) (*media.MediaSession, error) {
	sess, err := s.baseVideoSession(ctx, session, s.codecs)
	if err != nil {
		return nil, err
	}
	offer := generateSIPOffer(sess.Laddr.IP, connIPOf(sess), sess.Laddr.Port, s.codecs, s.videoCodecs)
	if err := sess.InitWithSDP(offer); err != nil {
		_ = sess.Close()
		return nil, err
	}
	sess.Codecs = append(sess.Codecs, s.videoCodecs...)
	return sess, nil
}

// newInboundVideoSession builds the inbound video call's media session
// and its RTP session: the answer-side half of the injection seam. The
// session starts from exactly the selected audio codec plus
// telephone-event (today's AnswerOptions pin — the answer's audio list is
// the selection, the local codecs' fmtp annotations kept); the caller's
// offer is applied (RemoteSDP — the audio negotiation and the remote
// address, diago's own answerSession order); the answer is generated
// (video accepted or the port-0 rejection) and pinned with InitWithSDP;
// the video codec joins sess.Codecs for the read gate. The RTPSession
// runs its RTCP monitor (the caller's address is known from the offer).
// The caller injects the trio into the dialog (InitMediaSession with the
// packet reader/writer pair) — the dialog's Answer then RespondSDPs the
// static answer, and the dialog's Close closes the session's sockets.
func (s sipStack) newInboundVideoSession(ctx context.Context, session UserSession, offer []byte, audio media.Codec, sel videoOfferSelection) (*media.MediaSession, *media.RTPSession, error) {
	sess, err := s.baseVideoSession(ctx, session, []media.Codec{audio, media.CodecTelephoneEvent8000})
	if err != nil {
		return nil, nil, err
	}
	fail := func(err error) (*media.MediaSession, *media.RTPSession, error) {
		_ = sess.Close()
		return nil, nil, err
	}
	if err := sess.RemoteSDP(offer); err != nil {
		return fail(fmt.Errorf("the offer did not apply: %w", err))
	}
	answer := generateSIPAnswer(sess.Laddr.IP, connIPOf(sess), sess.Laddr.Port, sess.CommonCodecs(), sel)
	if err := sess.InitWithSDP(answer); err != nil {
		return fail(err)
	}
	if sel.ok {
		sess.Codecs = append(sess.Codecs, sel.codec)
	}
	rtpSess := media.NewRTPSession(sess)
	if err := rtpSess.MonitorBackground(); err != nil {
		_ = rtpSess.Close()
		return fail(err)
	}
	return sess, rtpSess, nil
}

// baseVideoSession is the two builders' shared start: a MediaSession of
// the account's own addresses, Init'd (the sockets bound, the port
// assigned). codecs is the session's initial list — the full offer list
// outbound, the answer pin inbound.
func (s sipStack) baseVideoSession(ctx context.Context, session UserSession, codecs []media.Codec) (*media.MediaSession, error) {
	bindIP, externalIP := s.mediaIPs(ctx, session)
	if bindIP == nil {
		return nil, fmt.Errorf("the account has no local media address")
	}
	sess := &media.MediaSession{
		Codecs:     slices.Clone(codecs),
		Laddr:      net.UDPAddr{IP: bindIP},
		ExternalIP: externalIP,
		Mode:       sdp.ModeSendrecv,
	}
	if err := sess.Init(); err != nil {
		return nil, err
	}
	return sess, nil
}

// mediaIPs mirrors diago's WithTransport media-address computation (the
// hand-built session must sit on the same addresses diago's own would):
// the bind IP is the account socket's bind address (bindHostFor — the
// route's source toward the registrar), with the interface-resolution
// fallback diago uses for an unspecified bind; the external IP is the
// configured ExternalHost when it names an address, else nil (LocalSDP's
// c= then falls back to the bind IP, diago's behavior too).
func (s sipStack) mediaIPs(ctx context.Context, session UserSession) (bindIP, externalIP net.IP) {
	bindIP = net.ParseIP(s.bindHostFor(ctx, session))
	if bindIP == nil || bindIP.IsUnspecified() {
		network := "ip4"
		if bindIP != nil && bindIP.To4() == nil {
			network = "ip6"
		}
		if ip, _, err := sip.ResolveInterfacesIP(network, nil); err == nil {
			bindIP = ip
		}
	}
	externalIP = net.ParseIP(s.externalHost)
	if externalIP != nil && externalIP.IsUnspecified() {
		externalIP = nil
	}
	return bindIP, externalIP
}

// connIPOf is the SDP c= address: the external IP when one is set, else
// the bind IP — diago's LocalSDP fallback.
func connIPOf(sess *media.MediaSession) net.IP {
	if sess.ExternalIP != nil {
		return sess.ExternalIP
	}
	return sess.Laddr.IP
}

// sessionRTPWriter is the muxed browser→SIP video sink: the packet is
// marshaled and written raw onto the media session's own socket toward
// the session's remote address (WriteRTPRaw) — the video leaves from the
// same port the audio does (the mux the shared m-line port announced).
// WriteRTPRaw touches neither WriteRTP's shared marshal buffer nor its
// mode gate, and UDP writes are datagram-atomic, so it races the audio
// writer safely.
type sessionRTPWriter struct {
	sess *media.MediaSession
}

func (w sessionRTPWriter) WriteRTP(p *rtp.Packet) error {
	data, err := p.Marshal()
	if err != nil {
		return err
	}
	_, err = w.sess.WriteRTPRaw(data)
	return err
}

// udpRTPWriter is the demuxed browser→SIP video sink: a peer that
// answered video on a port of its own gets the bot's video sent there
// from a dedicated socket on the session's local IP (the send's source
// port is unconstrained in RTP — the symmetric-RTP caveat is
// docs/sip-bot.md §11's).
type udpRTPWriter struct {
	conn  *net.UDPConn
	raddr *net.UDPAddr
}

func (w udpRTPWriter) WriteRTP(p *rtp.Packet) error {
	data, err := p.Marshal()
	if err != nil {
		return err
	}
	_, err = w.conn.WriteToUDP(data, w.raddr)
	return err
}

// newVideoWriter picks the camera pump's sink for the negotiated video:
// the muxed session writer when the peer's video address is its audio
// one (or none was parsed), the dedicated-socket writer otherwise. The
// packet writer's clock ticker is disabled — the camera pump feeds
// explicit timestamp deltas (the browser's own 90 kHz steps), not a
// free-running 33 ms pace.
func newVideoWriter(sess *media.MediaSession, codec media.Codec, raddr *net.UDPAddr) (*media.RTPPacketWriter, *net.UDPConn, error) {
	var writer media.RTPWriter = sessionRTPWriter{sess}
	var conn *net.UDPConn
	if raddr != nil && !udpAddrEqual(*raddr, sess.Raddr) {
		c, err := net.ListenUDP("udp", &net.UDPAddr{IP: sess.Laddr.IP})
		if err != nil {
			return nil, nil, err
		}
		writer, conn = udpRTPWriter{conn: c, raddr: raddr}, c
	}
	w := media.NewRTPPacketWriter(writer, codec)
	w.ClockDisable()
	return w, conn, nil
}

// udpAddrEqual reports whether two UDP addresses name the same endpoint.
func udpAddrEqual(a, b net.UDPAddr) bool {
	return a.Port == b.Port && a.IP.Equal(b.IP)
}
