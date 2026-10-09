package sipbot

// This file is one call: the B2BUA's meeting point. A peerCall joins
//
//   - the webrtc-leg — the in-band dialog toward the browser (the
//     callId) and the bot's speaker track, created once the call's codec
//     is known and attached with the codec pin when the browser is rung;
//   - the sip-leg — the SIP dialog toward the network (outbound: the
//     diago INVITE's client session; inbound: the pending server session
//     the call was created from), whose answer (outbound) or offer
//     (inbound) selects the call's codec;
//
// and relays media between them with two pump goroutines — one per
// direction — each a read → write loop at RTP payload granularity: the
// payload crosses byte for byte, the bot re-originating only the
// envelope (transport, crypto, PT/SSRC/sequence/timestamps) per leg —
// there is no transcoding anywhere (docs/sip-bot.md §8). The pumps are
// self-paced by the arriving RTP: no tickers, unlike the music bot there
// is no free-running source. The SIP→browser pump is gated on the
// call's accept: the browser answers the ring-time offer at once, so
// the track binds long before the user picks up, and an answered
// callee's early audio must not be audible in a still-ringing browser —
// it drops at the gate until activate (docs/sip-bot.md §11.5).
//
// Concurrency: the call's fields are either set before the state is
// published (target, w — and callId before its first reader can win the
// call's end) or gathered under mu — the SIP dial goroutine, the
// callee's hangup watcher, and the peer's serialized dcmsg goroutine
// genuinely meet on them, so a small mutex is the honest tool (the music
// bot needs none only because every touch of its state is on the channel
// goroutine).
//
// The ring's exact-once discipline: outbound the browser is rung only
// after the callee answered, inbound the ring is the call's first act —
// either way the ring's steps (the pinned attach, the INVITE, the callId)
// race the call's end paths. Each step re-checks the call is still live
// under mu; an end landing mid-ring leaves the just-sent ring to the
// step that sent it (it Cancels), the end's owner having seen no callId.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"

	"github.com/emiago/diago"
	"github.com/emiago/diago/media"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	pionmedia "github.com/pion/webrtc/v4/pkg/media"

	"personal-site/pkg/rtc"
	"personal-site/pkg/rtc/msg_handler"
)

// callPhase is where the call's webrtc-leg stands (the sip-leg's phase
// is the dialog's own).
type callPhase int

const (
	// phaseRinging: the bot INVITEd the browser and waits for its answer.
	phaseRinging callPhase = iota
	// phaseActive: the browser answered.
	phaseActive
)

// peerCall is one chat user's call state.
type peerCall struct {
	// Set before the state is published, read-only after.
	//
	// target is the remote party: outbound — the dial target as /call gave
	// it; inbound — the caller's user@host. inbound says which direction
	// the call came from; an inbound call's dialog (a server session) is
	// set here at creation — the pending INVITE IS the sip-leg — where an
	// outbound one's (a client session) lands under mu when the callee
	// answers.
	target  string
	inbound bool
	// serverDialog is the inbound call's concrete server session — the
	// pending INVITE diago handed the serve callback (nil for an outbound
	// call). dialog below carries the same value (as the interface) from
	// creation; the concrete handle is for the verbs only a server dialog
	// speaks: Answer, and the decline's 603.
	serverDialog *diago.DialogServerSession
	w            msg_handler.ResponseWriter // the invocation's writer, captured for the SIP leg's async answers (the /call command outbound; the inbound ring's unsolicited WriterFor)

	// pendingCancel aborts the call's still-pending phase: outbound — the
	// unanswered diago INVITE (the user hung up first, /unregister
	// replaced the account, the session ended); inbound — the ring
	// timeout's clock.
	pendingCancel context.CancelFunc

	// accepted is closed by activate when the browser answers the ring —
	// the SIP→browser pump's gate, which it reads off the relay. Created
	// at construction, closed exactly once (activate's phase guard).
	accepted chan struct{}

	mu       sync.Mutex
	phase    callPhase
	callId   string                         // the webrtc-leg dialog's id — "" until the browser is rung
	codec    media.Codec                    // the call's end-to-end audio codec, once known (outbound: the callee's answer; inbound: the caller's offer, at creation)
	codecOK  bool                           // codec is set
	track    *webrtc.TrackLocalStaticSample // the bot's speaker track toward the browser — created by setCodec
	attached bool                           // the track is on the wire (the pinned attach succeeded)
	dialog   sipLeg                         // outbound: nil until the callee answers; inbound: the pending dialog from the start
	relay    *relay                         // nil until the SIP side is answered
	mic      *webrtc.TrackRemote            // nil until the browser's mic arrives
	micOn    bool                           // the browser→SIP pump is running
	ended    bool                           // an end path ran; later ones no-op

	// The video half. video is the outbound call's intent (/video-call —
	// inbound calls learn theirs from the caller's offer instead); videoOK
	// says a video codec negotiated end to end (outbound: the callee's
	// answer carried one; inbound: the caller's offer shared one with the
	// presumed list). The rest is set by setVideo once it did: the
	// negotiated codec at the sip-leg's payload type, the peer's video
	// address (nil or the audio address: the media is muxed on the
	// dialog's one socket), and the bot's video track toward the browser.
	// customMedia marks the inbound call whose media session was built
	// by hand and injected (video.go) — its Answer RespondSDPs the static
	// answer where the native path builds one.
	video         bool
	videoOK       bool
	videoCodec    media.Codec
	videoRaddr    *net.UDPAddr
	videoTrack    *webrtc.TrackLocalStaticRTP
	attachedVideo bool                // the video track is on the wire (the pinned attach succeeded)
	camera        *webrtc.TrackRemote // nil until the browser's camera arrives
	cameraOn      bool                // the camera→SIP pump is running
	customMedia   bool

	// dtmfPT/dtmfOK are the webrtc-leg's negotiated telephone-event
	// payload type (and whether one negotiated), learned from the mic's
	// receiver at setMic: the pad's DTMF rides the mic's own RTP stream
	// under that PT (dtmf.go). dtmfOK false means the browser leg sends
	// no DTMF at all — the pump dispatches nothing.
	dtmfPT uint8
	dtmfOK bool
}

// sipLeg is the call's SIP side as the relay and the teardown see it: the
// encoded-payload reader/writer both diago dialog types embed (their
// DialogMedia), the dialog's lifetime, and its termination verb —
// diago's Hangup speaks the dialog's own state on either type: the BYE of
// an established dialog, the 480 of a still-ringing server one.
type sipLeg interface {
	AudioReader(opts ...diago.AudioReaderOption) (io.Reader, error)
	AudioWriter(opts ...diago.AudioWriterOption) (io.Writer, error)
	Context() context.Context
	Hangup(ctx context.Context) error
	// MediaSession is the dialog's media session — on a video call it is
	// the bot's hand-built injected one (video.go); the relay's video
	// writers write through it (the muxed port) or read its addresses off
	// it (the demuxed fallback).
	MediaSession() *media.MediaSession
}

// relay is the call's media bridge: the two payload passthroughs and the
// pumps' lifetime. accepted is the call's accept gate — the SIP→browser
// pump drops the callee's audio until it closes (the user picked up).
// stop bars new pump starts; pumps already blocked on a read die with
// their leg (the dialog's close, the track's end) — the music bot's
// drainTrack has the same shape. dtmfCh carries the pad's digits the
// browser→SIP pump parsed to the digit sender (sendDigits): buffered and
// dropped on full, so a flooded pad never stalls the audio pump.
type relay struct {
	dialog   sipLeg
	track    *webrtc.TrackLocalStaticSample
	codec    media.Codec     // the call's codec — the browser-bound payloads' duration key
	audioPT  uint8           // the sip-leg's negotiated audio payload type — anything else on the read stream is not audio (telephone-event — or video)
	accepted <-chan struct{} // the accept gate — activate closes it when the user picks up
	dtmfCh   chan rune
	stopCh   chan struct{}
	stopOnce sync.Once

	// video is nil on a voice-only call. On a video call it joins the
	// video legs: the browser-bound track (the callee's video crosses
	// byte for byte, the track re-stamping SSRC/PT), the sip-leg's
	// negotiated video payload type (the read stream's PT dispatch), and
	// the camera→SIP packet writer with its demuxed socket to close when
	// the answer demuxed the video (video.go's writers).
	video *relayVideo
}

// relayVideo is the relay's video half — see relay.video.
type relayVideo struct {
	track  *webrtc.TrackLocalStaticRTP
	pt     uint8
	writer *media.RTPPacketWriter
	closer io.Closer // the demuxed socket; nil when the video is muxed
}

// errCallEnded is setDialog's and the ring steps' answer when the call
// already ended while the dial or the ring was in flight — the end's
// owner runs the teardown, the in-flight path just stops.
var errCallEnded = errors.New("sipbot: the call ended while the callee was being rung")

// setCodec pins the call's codec: it stores the negotiated selection and
// creates the speaker track with the codec's webrtc-leg capability. An
// unknown codec is not relayable (a misconfigured presumed list would
// say so here); errCallEnded when the call already ended.
func (c *peerCall) setCodec(codec media.Codec) error {
	params, ok := webrtcCodecParams(codec)
	if !ok {
		return fmt.Errorf("sipbot: the negotiated codec %q is not relayable", codec.Name)
	}
	track, err := webrtc.NewTrackLocalStaticSample(params.RTPCodecCapability, "sip-voice", "sipbot")
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ended {
		return errCallEnded
	}
	c.codec, c.codecOK = codec, true
	c.track = track
	return nil
}

// codecParamsLocked is the webrtc-leg's codec pin: the call's codec plus
// telephone-event — the list the transceiver's preferences are set to,
// so the session's offers and answers carry exactly it (docs/sip-bot.md
// §8). mu held; setCodec has validated the mapping.
func (c *peerCall) codecParamsLocked() []webrtc.RTPCodecParameters {
	params, _ := webrtcCodecParams(c.codec)
	return []webrtc.RTPCodecParameters{params, rtc.TelephoneEventCodec}
}

// setVideo pins the call's video: it stores the negotiated selection
// (the codec at the sip-leg's payload type and the peer's video address)
// and creates the video track with the codec's webrtc-leg capability —
// setCodec's video twin. errCallEnded when the call already ended.
func (c *peerCall) setVideo(codec media.Codec, raddr *net.UDPAddr) error {
	params, ok := webrtcVideoCodecParams(codec)
	if !ok {
		return fmt.Errorf("sipbot: the negotiated video codec %q is not relayable", codec.Name)
	}
	track, err := webrtc.NewTrackLocalStaticRTP(params.RTPCodecCapability, "sip-video", "sipbot")
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ended {
		return errCallEnded
	}
	c.videoCodec, c.videoOK, c.videoRaddr, c.videoTrack = codec, true, raddr, track
	return nil
}

// ringMedia is the browser ring's media kind: video when a video codec
// negotiated end to end (the browser rings as a video call), voice
// otherwise — a video call the far end declined video on rings as a
// voice call.
func (c *peerCall) ringMedia() msg_handler.MediaKind {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.videoOK {
		return msg_handler.MediaVideo
	}
	return msg_handler.MediaVoice
}

// attachPinned puts the call's track on the wire with the codec pin —
// the ring's first step: the attach's renegotiation offers the browser
// exactly the pinned codec list. A video call's video track follows
// (attachPinnedVideo): two rapid offers, answered in order by the
// browser's perfect negotiator. errCallEnded when the call already
// ended (or ends while the attach is in flight — the just-attached track
// is then withdrawn from here, the end's owner having seen attached ==
// false).
func (c *peerCall) attachPinned() error {
	c.mu.Lock()
	if c.ended || !c.codecOK {
		c.mu.Unlock()
		return errCallEnded
	}
	track, params := c.track, c.codecParamsLocked()
	c.mu.Unlock()
	if err := c.w.AttachMediaWithCodecPreferences(track, params); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ended {
		// An end landed while the attach was in flight: withdraw the
		// just-attached track — the end's owner saw attached == false and
		// scheduled nothing. A failed withdraw rides the session's own
		// teardown (a detaching peer fails its own answer anyway).
		go func() { _ = c.w.DetachMedia(track) }()
		return errCallEnded
	}
	c.attached = true
	return nil
}

// attachPinnedVideo is attachPinned's video half: the video track joins
// the wire pinned to the call's negotiated video codec. The browser
// answers the two attaches' offers in order; at accept the mic joins the
// audio transceiver and the camera the video one (pion's kind-based
// reuse). errCallEnded exactly as attachPinned's.
func (c *peerCall) attachPinnedVideo() error {
	c.mu.Lock()
	if c.ended || !c.videoOK {
		c.mu.Unlock()
		return errCallEnded
	}
	track := c.videoTrack
	params, _ := webrtcVideoCodecParams(c.videoCodec)
	c.mu.Unlock()
	if err := c.w.AttachMediaWithCodecPreferences(track, []webrtc.RTPCodecParameters{params}); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ended {
		go func() { _ = c.w.DetachMedia(track) }()
		return errCallEnded
	}
	c.attachedVideo = true
	return nil
}

// setCallId records the webrtc-leg dialog's id — the ring's last step.
// False when the call already ended: the end's owner saw no callId and
// sent nothing, so the caller Cancels the just-sent ring itself.
func (c *peerCall) setCallId(callId string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ended {
		return false
	}
	c.callId = callId
	return true
}

// callIdSnapshot reports the webrtc-leg dialog's id — "" before the
// browser is rung. The dcmsg gates read it under the mutex: the ring's
// INVITE and the peer's instant answer can cross the id's assignment.
func (c *peerCall) callIdSnapshot() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.callId
}

// setDialog arms the answered sip-leg: the relay is built from the
// negotiated codec and the SIP→browser pump starts. errCallEnded when
// the call already ended (the caller hangs the dialog up instead); any
// other error is a relay-construction failure — the dialog's media
// properties never came up — and the caller ends the call saying why.
func (c *peerCall) setDialog(dialog *diago.DialogClientSession) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ended {
		return errCallEnded
	}
	return c.armLocked(dialog)
}

// armRelay is setDialog's inbound twin: the dialog was created with the
// call (the pending INVITE), so answering only arms the relay on it.
func (c *peerCall) armRelay() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ended {
		return errCallEnded
	}
	return c.armLocked(c.dialog)
}

// armLocked builds the relay on leg and starts the SIP→browser pump (and
// the browser→SIP one when the mic already arrived, the camera one when
// the camera did). mu held.
func (c *peerCall) armLocked(leg sipLeg) error {
	r, err := newRelay(leg, c.track, c.codec, c.accepted)
	if err != nil {
		return err
	}
	if c.videoOK {
		v, err := newRelayVideo(leg.MediaSession(), c.videoCodec, c.videoRaddr, c.videoTrack)
		if err != nil {
			return err
		}
		r.video = v
	}
	c.dialog = leg
	c.relay = r
	go r.pumpSIPToBrowser()
	if c.mic != nil {
		c.startMicPumpLocked()
	}
	if c.camera != nil && c.videoOK {
		c.startCameraPumpLocked()
	}
	return nil
}

// micCodecOK reports whether an arriving mic speaks the call's codec —
// the failed-presumption check: the mic's m-line is answered with the
// pin, so anything else means the presumed list did not match the actual
// WebRTC client (docs/sip-bot.md §11.8) — such a mic is drained, never
// bridged.
func (c *peerCall) micCodecOK(mimeType string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.codecOK {
		return false
	}
	params, ok := webrtcCodecParams(c.codec)
	return ok && strings.EqualFold(params.MimeType, mimeType)
}

// setMic notes the browser's mic track and its receiver's negotiated
// telephone-event PT; the browser→SIP pump starts once both it and the
// answered dialog exist.
func (c *peerCall) setMic(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mic = track
	if pt, ok := telephoneEventPT(receiver); ok {
		c.dtmfPT, c.dtmfOK = pt, true
	}
	if c.relay != nil && !c.micOn {
		c.startMicPumpLocked()
	}
}

// startMicPumpLocked starts the browser→SIP pump. mu held.
func (c *peerCall) startMicPumpLocked() {
	c.micOn = true
	go c.relay.pumpBrowserToSIP(c.mic, c.dtmfPT, c.dtmfOK)
}

// cameraCodecOK reports whether an arriving camera speaks the call's
// negotiated video codec — micCodecOK's video twin: the camera's m-line
// is answered with the video pin, so anything else is the failed
// presumption — drained, never bridged.
func (c *peerCall) cameraCodecOK(mimeType string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.videoOK {
		return false
	}
	params, ok := webrtcVideoCodecParams(c.videoCodec)
	return ok && strings.EqualFold(params.MimeType, mimeType)
}

// setCamera notes the browser's camera track; the camera→SIP pump starts
// once both it and the answered dialog's relay exist (setMic's shape).
func (c *peerCall) setCamera(track *webrtc.TrackRemote) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.camera = track
	if c.relay != nil && !c.cameraOn && c.videoOK {
		c.startCameraPumpLocked()
	}
}

// startCameraPumpLocked starts the camera→SIP pump. mu held.
func (c *peerCall) startCameraPumpLocked() {
	c.cameraOn = true
	go c.relay.pumpCameraToSIP(c.camera)
}

// activate marks the webrtc-leg answered, opening the SIP→browser
// pump's gate; false when the call ended meanwhile or the phase was
// already past ringing.
func (c *peerCall) activate() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ended || c.phase != phaseRinging {
		return false
	}
	c.phase = phaseActive
	close(c.accepted)
	return true
}

// callEnd is the end()'s snapshot for the winner's follow-up: the SIP
// dialog to hang up, where the webrtc-leg stood ("" callId — the browser
// was never rung — means no verbs exist to send), and the attached
// track to withdraw. The fields stop mutating the moment ended is set
// (the ring steps and setCodec refuse a dead call), so the winner reads
// them off the snapshot its own end() answered.
type callEnd struct {
	dialog        sipLeg
	callId        string
	phase         callPhase
	track         *webrtc.TrackLocalStaticSample
	attached      bool
	videoTrack    *webrtc.TrackLocalStaticRTP
	attachedVideo bool
}

// end runs the call's end exactly once, answering its snapshot for the
// caller's follow-up (which legs to tell, what to hang up): the pending
// phase is aborted, the relay stopped, the state marked. The dialog's
// own termination verb is the caller's — it knows the context (and must
// not block on it).
func (c *peerCall) end() (callEnd, bool) {
	return c.endIf(func() bool { return true })
}

// endRinging is the ring timeout's end — the call's end, but only while
// the webrtc-leg still rings: an answered call is not the timeout's to
// end, and a same-instant answer wins.
func (c *peerCall) endRinging() (callEnd, bool) {
	return c.endIf(func() bool { return c.phase == phaseRinging })
}

// endIf runs the call's end exactly once when allow (read under the
// lock) permits; the end's own bookkeeping is end's.
func (c *peerCall) endIf(allow func() bool) (callEnd, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ended || !allow() {
		return callEnd{}, false
	}
	c.ended = true
	c.pendingCancel()
	if c.relay != nil {
		c.relay.stop()
	}
	return callEnd{dialog: c.dialog, callId: c.callId, phase: c.phase, track: c.track, attached: c.attached, videoTrack: c.videoTrack, attachedVideo: c.attachedVideo}, true
}

// answerCodecs is the inbound call's SDP answer pin: the selected codec
// plus telephone-event. diago's negotiation intersects it with the
// caller's offer, so telephone-event echoes only when offered, and the
// answer's codec list is exactly the selection (docs/sip-bot.md §7.2).
func (c *peerCall) answerCodecs() []media.Codec {
	c.mu.Lock()
	defer c.mu.Unlock()
	return []media.Codec{c.codec, media.CodecTelephoneEvent8000}
}

// newRelay builds the media bridge for the answered dialog: the
// negotiated wire payload type read off the dialog's media properties
// (anything else on the read stream — telephone-event — is not audio).
// accepted is the call's accept gate for the SIP→browser pump.
func newRelay(dialog sipLeg, track *webrtc.TrackLocalStaticSample, codec media.Codec, accepted <-chan struct{}) (*relay, error) {
	props := diago.MediaProps{}
	if _, err := dialog.AudioReader(diago.WithAudioReaderMediaProps(&props)); err != nil {
		return nil, err
	}
	return &relay{
		dialog:   dialog,
		track:    track,
		codec:    codec,
		audioPT:  props.Codec.PayloadType,
		accepted: accepted,
		dtmfCh:   make(chan rune, 16),
		stopCh:   make(chan struct{}),
	}, nil
}

// newRelayVideo builds the relay's video half for the answered dialog:
// the camera→SIP writer picked by the answer's video address (the muxed
// session writer, or the demuxed socket — video.go's newVideoWriter). A
// demuxed socket that will not open fails the relay's construction (the
// call's failure path reports it); the muxed writer is infallible.
func newRelayVideo(sess *media.MediaSession, codec media.Codec, raddr *net.UDPAddr, track *webrtc.TrackLocalStaticRTP) (*relayVideo, error) {
	writer, conn, err := newVideoWriter(sess, codec, raddr)
	if err != nil {
		return nil, err
	}
	v := &relayVideo{track: track, pt: codec.PayloadType, writer: writer}
	if conn != nil {
		v.closer = conn
	}
	return v, nil
}

// stop ends the relay; safe to call repeatedly.
func (r *relay) stop() {
	r.stopOnce.Do(func() {
		close(r.stopCh)
		if r.video != nil && r.video.closer != nil {
			// The demuxed-answer socket goes with the call (the muxed writer
			// owns no socket — the dialog's Close closes the session's).
			_ = r.video.closer.Close()
		}
	})
}

// stopped reports whether the relay was stopped — a pump start checks
// it to not run on a dead call.
func (r *relay) stopped() bool {
	select {
	case <-r.stopCh:
		return true
	default:
		return false
	}
}

// pumpSIPToBrowser relays the callee's audio to the browser: sip-leg
// payloads read off the dialog, written to the call's track — byte for
// byte, the track's timestamps the payloads' own durations. The pump is
// self-paced by the arriving RTP. Until the user accepts the ring the
// payloads drop at the accept gate: the browser's negotiator answers
// the ring-time offer at once (it is call-state-blind), so the track
// binds long before the accept — without the gate a callee who starts
// talking at once (an IVR greeting, a "hello?") would be audible in a
// still-ringing browser (docs/sip-bot.md §11.5). pion's drop of writes
// to the not-yet-bound track (the music bot's "empty room") remains
// for the residual window where the pump outruns the attach. The pump
// dies on the dialog's close (the callee's BYE, the call's hangup).
// Payloads whose payload type is not the negotiated audio codec's —
// telephone-event, the one other PT the sip-leg can legitimately carry
// — are dropped, never bridged: a 4-byte DTMF event written to the
// audio track is a garbage blip. (The SIP→browser DTMF direction is
// deliberately not transported — docs/sip-bot.md §8.)
func (r *relay) pumpSIPToBrowser() {
	if r.stopped() {
		return
	}
	reader, err := r.dialog.AudioReader()
	if err != nil {
		return
	}
	// The concrete reader exposes the last packet's header (safe in the
	// reading goroutine, which the pump is); diago does no PT filtering
	// of its own. A nil pktReader (a future diago wrapping the reader)
	// degrades to no filtering — today's behavior, never a crash.
	pktReader, _ := reader.(*media.RTPPacketReader)
	buf := make([]byte, media.RTPBufSize)
	for {
		n, err := reader.Read(buf)
		if err != nil {
			return // the dialog ended
		}
		if r.stopped() {
			return
		}
		if n == 0 {
			continue // a keep-alive or a read-stats gate drop; the header is stale
		}
		payload := buf[:n]
		if pktReader != nil && pktReader.PacketHeader.PayloadType != r.audioPT {
			// Not audio: on a video call the muxed read stream also carries
			// the callee's video — it crosses to the browser's video track
			// byte for byte (the track re-stamps SSRC/PT, the packet's own
			// sequence/timestamp/marker preserved). Anything else
			// (telephone-event) drops as today.
			if r.video == nil || pktReader.PacketHeader.PayloadType != r.video.pt {
				continue
			}
			select {
			case <-r.accepted:
			default:
				continue // the accept gate covers video too
			}
			pkt := &rtp.Packet{Header: pktReader.PacketHeader, Payload: payload}
			if err := r.video.track.WriteRTP(pkt); err != nil {
				return
			}
			continue
		}
		select {
		case <-r.accepted:
		default:
			continue // the accept gate: the browser still rings — drop
		}
		if err := r.track.WriteSample(pionmedia.Sample{Data: payload, Duration: packetDuration(r.codec, payload)}); err != nil {
			return
		}
	}
}

// pumpBrowserToSIP relays the browser's mic to the callee: packets read
// off the mic's remote track, their payloads written to the dialog —
// byte for byte, diago's writer re-stamping PT/SSRC/seq and pacing the
// pump on the codec's 20 ms ticker. It dies on the track's end (the
// peer's teardown, the session's close). The webrtc-leg's DTMF surfaces
// in the same read stream — telephone-event shares the mic's SSRC, only
// its negotiated PT (dtmfPT, when dtmfOK) tells it from audio — and is
// parsed per RFC 4733 (dtmf.go): a fresh END packet's digit queues onto
// the sip-leg's DTMF writer.
func (r *relay) pumpBrowserToSIP(mic *webrtc.TrackRemote, dtmfPT uint8, dtmfOK bool) {
	if r.stopped() {
		return
	}
	// The dialog's writer is DTMF-aware: audio passes through it under
	// its lock, and WriteDTMF injects the event series onto the same RTP
	// stream (PT 101, the sip-leg's telephone-event) — diago's own
	// interleaving discipline.
	dtmfWriter := diago.DTMFWriter{}
	writer, err := r.dialog.AudioWriter(diago.WithAudioWriterDTMF(&dtmfWriter))
	if err != nil {
		return
	}
	go r.sendDigits(&dtmfWriter)
	var dedup dtmfDeduper
	for {
		pkt, _, err := mic.ReadRTP()
		if err != nil {
			return // the track or the connection ended
		}
		if r.stopped() {
			return
		}
		if dtmfOK && pkt.PayloadType == dtmfPT {
			ev, ok := parseDTMFEvent(pkt.Payload)
			if !ok || !ev.end {
				continue // start/update packets are not the cue; a complete press is
			}
			digit, ok := ev.digit()
			if !ok || !dedup.freshEnd(ev.code, pkt.Timestamp) {
				continue
			}
			select {
			case r.dtmfCh <- digit:
			default: // a flooded pad drops digits, never stalls the pump
			}
			continue
		}
		if _, err := writer.Write(pkt.Payload); err != nil {
			return
		}
	}
}

// pumpCameraToSIP relays the browser's camera to the sip-leg: packets
// read off the camera's remote track, re-packetized onto the call's
// video writer (the muxed session socket, or the demuxed one —
// video.go) at the negotiated video payload type. The payload crosses
// byte for byte; the browser's own 90 kHz timestamp steps carry over as
// the deltas (the writer's ticker is disabled — the camera is the
// clock), the marker bit (the frame boundary) with them. It dies on the
// track's end, exactly as the mic pump.
func (r *relay) pumpCameraToSIP(camera *webrtc.TrackRemote) {
	if r.stopped() {
		return
	}
	var lastTS uint32
	first := true
	for {
		pkt, _, err := camera.ReadRTP()
		if err != nil {
			return // the track or the connection ended
		}
		if r.stopped() {
			return
		}
		var delta uint32
		if first {
			first = false
		} else {
			delta = pkt.Timestamp - lastTS
		}
		lastTS = pkt.Timestamp
		if _, err := r.video.writer.WriteSamples(pkt.Payload, delta, pkt.Marker, r.video.pt); err != nil {
			return
		}
	}
}

// sendDigits drains the pad's digit queue onto the sip-leg: diago's
// WriteDTMF emits the RFC 4733 event series (≈140 ms of real time per
// digit, holding the audio writer's lock — the audio's pause is the
// price of sharing one RTP stream), so it runs off the audio pump. It
// dies on the relay's stop or a failed write (the dialog is gone then).
func (r *relay) sendDigits(w *diago.DTMFWriter) {
	for {
		select {
		case digit := <-r.dtmfCh:
			if err := w.WriteDTMF(digit); err != nil {
				return
			}
		case <-r.stopCh:
			return
		}
	}
}
