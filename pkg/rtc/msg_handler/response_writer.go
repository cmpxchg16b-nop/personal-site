package msg_handler

import (
	"errors"

	"github.com/google/uuid"
	"github.com/pion/webrtc/v4"

	"personal-site/pkg/models/ss"
)

// ResponseWriter carries a bot's answers to the message being handled —
// the counterpart of http.ResponseWriter: the Server hands one to every
// BotMessageHandler invocation, bound to that message (its peer, its
// messaging channel, its threading anchor, its call id), and the handler
// answers through it — or not at all.
//
// Which methods are meaningful depends on the message: Reply and Amend
// send chat-channel messages. The call methods work with a phone call:
// Accept and Reject answer a call's INVITE and are meaningful only from
// HandleCalling for an INVITE (anything else returns ErrNotACall); Invite
// opens an outgoing call to the peer; AttachMedia and DetachMedia offer
// and withdraw local media tracks on the pair's peer connection — the
// caller-side counterpart of Accept's track offer, once the outgoing call
// was accepted — and OnTrack registers for the peer's inbound media.
//
// Media rides the pair's existing peer connection, the way the browser's
// useCallMedia attaches the microphone to it: the tracks offered with
// Accept or AttachMedia are added to the session (pion's
// PeerConnection.AddTrack shape), and the session's negotiator
// renegotiates on its own — no SDP work reaches the handler.
type ResponseWriter interface {
	// Reply sends a fresh plain-text chat message to the peer and returns
	// its msg id — the handle a later Amend rewrites. The reply threads
	// on the handled message (on the transfer's announcement for a chunk
	// or an attachment; on nothing when there is none).
	// ErrNoMessagingChannel is returned when the peer's messaging channel
	// is not up.
	Reply(text string) (ss.MsgId, error)

	// Amend rewrites the plain text of one of the bot's own earlier
	// messages via chat control; the message keeps its msg id on both
	// ends (the peer applies the control on arrival, the bot's own side
	// has no store). ErrNoMessagingChannel is returned when the peer's
	// messaging channel is not up.
	Amend(target ss.MsgId, text string) error

	// Invite opens an outgoing call to the peer: a SIP INVITE carrying
	// the media kind (X-Media) and the "inviting" call status. It returns
	// the dialog's fresh call id; the peer's answer (200 OK or 603
	// Decline) and the dialog's later messages arrive as SipMessages at
	// HandleCalling. The INVITE's logged UI state then follows the dialog
	// on its own — the Server, as the caller's side, amends it (see the
	// Server's doc). ErrNoMessagingChannel is returned when the peer's
	// messaging channel is not up, ErrInvalidMediaKind when media is not
	// MediaVoice or MediaVideo.
	Invite(media MediaKind) (callId string, err error)

	// Accept answers the incoming call's INVITE with 200 OK, offering the
	// given local media tracks to the caller — modeled on
	// webrtc.PeerConnection.AddTrack, whose TrackLocal is the sender's
	// microphone/camera (or a bot's generated stream) on the pair's
	// existing peer connection; the negotiators renegotiate on their own.
	// The tracks are attached before the response is sent, so a failed
	// attach (a gone session) sends nothing. ErrNotACall is returned when
	// the handled message is not a call invitation, ErrNoMessagingChannel
	// when the peer's messaging channel is not up.
	Accept(tracks ...webrtc.TrackLocal) error

	// Reject answers the incoming call's INVITE with a final error
	// response; the protocol defines one rejection, SipCodeDecline
	// (SipPhraseDecline). ErrNotACall is returned when the handled
	// message is not a call invitation, ErrNoMessagingChannel when the
	// peer's messaging channel is not up.
	Reject(code int, phrase string) error

	// AttachMedia offers the given local media tracks to the peer on the
	// pair's peer connection, answering nothing — the caller-side attach
	// once the peer accepted the bot's INVITE (200 OK), where Accept
	// would be wrong (the bot sent no INVITE answer). Returns the first
	// attach error — rtc.ErrNoPeerSession when the session is gone.
	AttachMedia(tracks ...webrtc.TrackLocal) error

	// DetachMedia withdraws tracks previously offered with Accept or
	// AttachMedia (the call ended); a no-op for a track that is not
	// attached. Returns the first detach error.
	DetachMedia(tracks ...webrtc.TrackLocal) error

	// OnTrack registers fn for the peer's inbound media — modeled on
	// webrtc.PeerConnection.OnTrack, which fires with the peer's
	// TrackRemote (and its RTPReceiver) once media flows after the call
	// is accepted. One registration per peer: a later OnTrack replaces
	// the earlier one; the registration lives for the peer session (a
	// glare rebuild keeps it). fn runs on a client goroutine and must not
	// block.
	OnTrack(fn func(remote *webrtc.TrackRemote, receiver *webrtc.RTPReceiver))
}

var (
	// ErrNoMessagingChannel is returned by a ResponseWriter's messaging
	// methods when the peer's messaging channel is not up (the binary
	// channel can carry a file before the messaging one arrives).
	ErrNoMessagingChannel = errors.New("msg_handler: the peer's messaging channel is not up")

	// ErrNotACall is returned by a ResponseWriter's call methods when the
	// handled message is not a call invitation.
	ErrNotACall = errors.New("msg_handler: the handled message is not a call invitation")

	// ErrInvalidMediaKind is returned by Invite when the media kind is not
	// MediaVoice or MediaVideo — anything else would be silently dropped
	// by the peer's codec validation.
	ErrInvalidMediaKind = errors.New("msg_handler: invalid media kind")

	// errEncode is returned when the (fixed-shape) outbound message could
	// not be encoded — in practice unreachable.
	errEncode = errors.New("msg_handler: message could not be encoded")
)

// responseWriter is the Server's ResponseWriter, bound to one handled
// message: the messaging channel it answers on (nil when the peer's
// messaging channel never came up), the threading anchor for Reply, and
// the dialog's call id when the handled message is a call message.
type responseWriter struct {
	server    *Server
	dc        *webrtc.DataChannel
	channelId ss.ChannelId
	self      ss.SubscriberId
	peer      ss.SubscriberId
	inReplyTo ss.MsgId
	callId    string
	isInvite  bool // the handled message is a call's INVITE
}

// Reply implements ResponseWriter.
func (w *responseWriter) Reply(text string) (ss.MsgId, error) {
	if w.dc == nil {
		return "", ErrNoMessagingChannel
	}
	msg := newDCMsgOut(w.channelId, w.self, w.peer)
	msg.MimeType = dcMsgMimePlaintext
	msg.Plaintext = text
	msg.InReplyTo = w.inReplyTo
	if err := w.server.sendText(w.dc, msg.encode()); err != nil {
		return "", err
	}
	return msg.MsgId, nil
}

// Amend implements ResponseWriter.
func (w *responseWriter) Amend(target ss.MsgId, text string) error {
	if w.dc == nil {
		return ErrNoMessagingChannel
	}
	msg := newDCMsgOut(w.channelId, w.self, w.peer)
	msg.MimeType = dcMsgMimeChatControl
	msg.ChatControl = &dcChatControlBody{
		Subtype:         "amend",
		TargetMessageId: target,
		Text:            text,
	}
	return w.server.sendText(w.dc, msg.encode())
}

// Invite implements ResponseWriter.
func (w *responseWriter) Invite(media MediaKind) (string, error) {
	if w.dc == nil {
		return "", ErrNoMessagingChannel
	}
	if media != MediaVoice && media != MediaVideo {
		return "", ErrInvalidMediaKind
	}
	callId := uuid.NewString()
	msg := newDCMsgOut(w.channelId, w.self, w.peer)
	msg.MimeType = dcMsgMimeSip
	msg.Sip = &dcSipBody{
		CallId:      callId,
		Method:      sipMethodInvite,
		XMedia:      string(media),
		XCallStatus: callStatusInviting,
	}
	if err := w.server.sendText(w.dc, msg.encode()); err != nil {
		return "", err
	}
	// Record the outgoing call: the Server owns the caller's duty of
	// amending the INVITE's logged UI status as the dialog unfolds.
	w.server.serviceChan <- inviteSentNote{
		peer:        w.peer,
		channelId:   w.channelId,
		self:        w.self,
		callId:      callId,
		inviteMsgId: msg.MsgId,
		media:       media,
	}
	return callId, nil
}

// Accept implements ResponseWriter.
func (w *responseWriter) Accept(tracks ...webrtc.TrackLocal) error {
	if !w.isInvite {
		return ErrNotACall
	}
	if w.dc == nil {
		return ErrNoMessagingChannel
	}
	// The tracks first: a failed attach (a gone session) sends no OK.
	for _, track := range tracks {
		if _, err := w.server.client.AddTrack(w.peer, track); err != nil {
			return err
		}
	}
	msg := newDCMsgOut(w.channelId, w.self, w.peer)
	msg.MimeType = dcMsgMimeSip
	msg.Sip = &dcSipBody{
		CallId:   w.callId,
		Response: &dcSipResponse{Code: sipResponseOKCode, Phrase: sipResponseOKPhrase},
	}
	return w.server.sendText(w.dc, msg.encode())
}

// Reject implements ResponseWriter.
func (w *responseWriter) Reject(code int, phrase string) error {
	if !w.isInvite {
		return ErrNotACall
	}
	if w.dc == nil {
		return ErrNoMessagingChannel
	}
	msg := newDCMsgOut(w.channelId, w.self, w.peer)
	msg.MimeType = dcMsgMimeSip
	msg.Sip = &dcSipBody{
		CallId:   w.callId,
		Response: &dcSipResponse{Code: code, Phrase: phrase},
	}
	return w.server.sendText(w.dc, msg.encode())
}

// AttachMedia implements ResponseWriter.
func (w *responseWriter) AttachMedia(tracks ...webrtc.TrackLocal) error {
	for _, track := range tracks {
		if _, err := w.server.client.AddTrack(w.peer, track); err != nil {
			return err
		}
	}
	return nil
}

// DetachMedia implements ResponseWriter.
func (w *responseWriter) DetachMedia(tracks ...webrtc.TrackLocal) error {
	for _, track := range tracks {
		if err := w.server.client.RemoveTrack(w.peer, track); err != nil {
			return err
		}
	}
	return nil
}

// OnTrack implements ResponseWriter.
func (w *responseWriter) OnTrack(fn func(remote *webrtc.TrackRemote, receiver *webrtc.RTPReceiver)) {
	w.server.setOnTrack(w.peer, fn)
}
