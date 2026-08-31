package msg_handler

import (
	"errors"

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
// send chat-channel messages; Accept, Reject, and OnTrack work with a
// phone call (voice, video, or both) and are meaningful only from
// HandleCalling for an INVITE — calling them for anything else returns
// ErrNotACall.
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

	// Accept answers the incoming call's INVITE with 200 OK, offering the
	// given local media tracks to the caller — modeled on
	// webrtc.PeerConnection.AddTrack, whose TrackLocal is the sender's
	// microphone/camera (or a bot's looped file or generated stream) on
	// the pair's existing peer connection; the negotiators renegotiate on
	// their own. ErrNotACall is returned when the handled message is not
	// a call invitation.
	//
	// The current Server has no media support — HeadlessRTCClient does
	// not attach tracks yet — so Accept always returns
	// ErrMediaUnsupported and sends nothing; the signature fixes the
	// shape for when the client grows track support.
	Accept(tracks ...webrtc.TrackLocal) error

	// Reject answers the incoming call's INVITE with a final error
	// response; the protocol defines one rejection, SipCodeDecline
	// (SipPhraseDecline). ErrNotACall is returned when the handled
	// message is not a call invitation, ErrNoMessagingChannel when the
	// peer's messaging channel is not up.
	Reject(code int, phrase string) error

	// OnTrack registers fn for the call's inbound media — modeled on
	// webrtc.PeerConnection.OnTrack, which fires with the caller's
	// TrackRemote (and its RTPReceiver) once media flows after an Accept.
	// Like Accept, it is inert with the current Server: fn never fires.
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

	// ErrMediaUnsupported is returned by Accept: the Server has no media
	// support yet (see ResponseWriter.Accept).
	ErrMediaUnsupported = errors.New("msg_handler: media is not supported")

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

	onTrack func(remote *webrtc.TrackRemote, receiver *webrtc.RTPReceiver)
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

// Accept implements ResponseWriter.
func (w *responseWriter) Accept(_ ...webrtc.TrackLocal) error {
	if w.callId == "" {
		return ErrNotACall
	}
	return ErrMediaUnsupported
}

// Reject implements ResponseWriter.
func (w *responseWriter) Reject(code int, phrase string) error {
	if w.callId == "" {
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

// OnTrack implements ResponseWriter.
func (w *responseWriter) OnTrack(fn func(remote *webrtc.TrackRemote, receiver *webrtc.RTPReceiver)) {
	w.onTrack = fn
}
