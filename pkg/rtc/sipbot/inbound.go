package sipbot

// This file is the inbound call — the reverse direction of the B2BUA:
// someone on the SIP network dials the AOR a chat user registered (their
// own credential, or a loaned pool account while the loan lasts), the
// registrar routes the INVITE to the account's socket, and the bot rings
// the user's browser. The browser needs nothing new: an inbound call is
// a bot-originated INVITE on the webrtc-leg, exactly the verb /call
// already uses — the answer popup, the accept (whose mic attach precedes
// the bot's media), the decline, the hangup are the frontend's generic
// incoming-call path.
//
// The mechanics this file rests on (diago's and sipgo's source):
//
//   - The serve handler owns the dialog's lifetime: diago's OnInvite
//     wrapper hangs up and closes the dialog the moment its serve
//     callback returns, so serveInbound blocks on the dialog's ctx until
//     the call is over, whichever side ended it.
//   - The dialog's ctx is the one "the SIP side ended" signal: sipgo's
//     ReadInvite wires the INVITE transaction's OnCancel into it (a
//     caller's CANCEL of the ring ends it), and BYE, the bot's own final
//     response, and the SIP server's shutdown end it too.
//   - diago's server-side Hangup speaks the dialog's own state: a BYE on
//     a confirmed dialog, a 480 Temporarily Unavailable on a ringing
//     one — so the generic termination paths (the browser's CANCEL/BYE,
//     /hangup, /unregister, a replacing /register, the session's end,
//     the ring timeout) need no answered-flag bookkeeping. The one
//     exception is the user's explicit decline, mirrored to the caller
//     as a 603.
//   - diago's AnswerOptions is the 200 OK: it builds the media session
//     from the per-call codec list (the codec selected at INVITE time
//     plus telephone-event — diago's negotiation intersects it with the
//     caller's offer, so the answer's codec list is exactly the
//     selection), sends the SDP answer, and blocks until the caller's
//     ACK (64·T1 bound) — hence its own goroutine, and the exactly-once
//     arm discipline shared with the outbound setDialog.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/emiago/diago"
	"github.com/emiago/diago/media"
	"github.com/pion/webrtc/v4"

	"personal-site/pkg/models/ss"
	"personal-site/pkg/rtc/msg_handler"
)

// inboundRingTimeout bounds how long an inbound call rings the browser:
// a caller kept waiting longer gets a 480, the browser a CANCEL. The
// normal ring's end is the caller's own CANCEL (the dialog's ctx), not
// this clock.
const inboundRingTimeout = 60 * time.Second

// inboundServe is the account's inbound-call callback (sipStack.open's
// onInbound): one closure per account, binding the owning peer.
func (h *sipHandler) inboundServe(peer ss.SubscriberId) func(inDialog *diago.DialogServerSession) {
	return func(inDialog *diago.DialogServerSession) {
		h.serveInbound(peer, inDialog)
	}
}

// serveInbound handles one inbound SIP call: select the call's codec
// from the caller's offer (the first of the presumed WebRTC codec list
// the offer carries — nothing in common is a 488, the browser never
// rings), ring the peer's browser with the selection pinned, and on its
// accept answer the SIP side and join the legs. It runs on the
// account's diago serve goroutine and blocks until the dialog ends —
// diago hangs up and closes the dialog when its serve handler returns,
// so the return IS the call's end (the ctx's done is the one signal for
// every ending, ours included).
func (h *sipHandler) serveInbound(peer ss.SubscriberId, inDialog *diago.DialogServerSession) {
	caller := callerOf(inDialog)
	h.logger.Info("sipbot: inbound SIP call", "peer", peer, "from", caller)
	w := h.writers.WriterFor(peer)
	// The codec is selected at INVITE time — the presumed browser's
	// selection among the caller's support (docs/sip-bot.md §8). An
	// INVITE without SDP (late-offer inbound stays unsupported), an
	// unparseable one, or one sharing nothing with the presumed list
	// gets a 488 and the browser never rings.
	codec, ok := h.selectInboundCodec(inDialog)
	if !ok {
		h.logger.Info("sipbot: inbound call with no relayable codec", "peer", peer, "from", caller)
		if err := inDialog.Respond(488, "Not Acceptable Here", nil); err != nil {
			h.logger.Warn("sipbot: 488 answer not sent", "peer", peer, "err", err)
		}
		return
	}
	ringCtx, ringCancel := context.WithCancel(context.Background())
	c := &peerCall{
		target:        caller,
		inbound:       true,
		serverDialog:  inDialog,
		w:             w,
		pendingCancel: ringCancel,
		dialog:        inDialog,
	}
	if err := c.setCodec(codec); err != nil {
		// The selected codec would not map to a track (in-memory creation
		// failing is near-impossible) — the caller is told, nobody rings.
		ringCancel()
		h.logger.Warn("sipbot: inbound call, track not prepared", "peer", peer, "err", err)
		if rerr := inDialog.Respond(480, "Temporarily Unavailable", nil); rerr != nil {
			h.logger.Warn("sipbot: inbound-call rejection not sent", "peer", peer, "err", rerr)
		}
		return
	}
	// One call per peer, either direction — and this goroutine races both
	// a second inbound INVITE and the peer's own /call, so the check IS
	// the store (the /call side is equally atomic).
	if _, loaded := h.calls.LoadOrStore(peer, c); loaded {
		ringCancel()
		h.logger.Info("sipbot: inbound call to a busy peer", "peer", peer, "from", caller)
		if err := inDialog.Respond(486, "Busy Here", nil); err != nil {
			h.logger.Warn("sipbot: busy answer not sent", "peer", peer, "err", err)
		}
		return
	}
	// Ring the browser with the selection: the mic registration first
	// (so no early track is missed — the /call discipline), then the
	// pinned attach — the offer carries the SIP-determined selection
	// toward the browser — then the INVITE. Each step re-checks the call
	// lives (the outbound dial's exact-once discipline, mirrored).
	w.OnTrack(func(remote *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		h.onMic(peer, remote, receiver)
	})
	if err := c.attachPinned(); err != nil {
		if errors.Is(err, errCallEnded) {
			return // the call ended as it was being rung — its end's owner tears down
		}
		h.failInboundRing(peer, c, "attach", err)
		return
	}
	callId, err := w.Invite(msg_handler.MediaVoice)
	if err != nil {
		// The peer has no live session — nobody to ring.
		h.failInboundRing(peer, c, "invite", err)
		return
	}
	if !c.setCallId(callId) {
		// An end landed while the INVITE was being sent — its owner saw
		// no callId and sent nothing; cancel the just-sent ring.
		if err := w.Cancel(callId); err != nil {
			h.logger.Warn("sipbot: CANCEL not sent", "peer", peer, "err", err)
		}
		return
	}
	// 180 Ringing to the caller. A failure means the dialog is already
	// dead (the caller's CANCEL crossed) — the watcher runs the teardown.
	if err := inDialog.Ringing(); err != nil {
		h.logger.Warn("sipbot: 180 Ringing not sent", "peer", peer, "err", err)
	}
	h.say(peer, w, fmt.Sprintf("Incoming call from %s…", caller))
	go h.watchInbound(peer, c)
	go h.ringTimeout(peer, c, ringCtx)
	<-inDialog.Context().Done()
}

// selectInboundCodec reads the caller's offered codec list off the
// INVITE's body and answers the end-to-end selection: the first codec
// of the presumed WebRTC codec list present in the offer.
func (h *sipHandler) selectInboundCodec(inDialog *diago.DialogServerSession) (media.Codec, bool) {
	body := inDialog.InviteRequest.Body()
	if body == nil {
		return media.Codec{}, false // late-offer inbound stays unsupported
	}
	offered, err := audioCodecsFromSDP(body)
	if err != nil {
		h.logger.Warn("sipbot: the inbound offer's codecs unreadable", "err", err)
		return media.Codec{}, false
	}
	return selectCodec(h.stack.audioCodecs, offered)
}

// failInboundRing ends an inbound call whose browser leg could not be
// opened (the peer's session is gone): the caller gets the still-ringing
// dialog's 480, the just-attached track withdraws, the state resets.
func (h *sipHandler) failInboundRing(peer ss.SubscriberId, c *peerCall, step string, err error) {
	h.logger.Info("sipbot: inbound call, the browser could not be rung", "peer", peer, "step", step, "err", err)
	e, ok := c.end()
	if !ok {
		return
	}
	h.calls.CompareAndDelete(peer, c)
	h.hangupDialog(e.dialog) // the dialog still rings: a 480
	if e.attached {
		h.detachLater(context.Background(), e.track, c.w)
	}
}

// watchInbound relays the SIP side's own end of an inbound call — the
// caller's CANCEL of the ring, the caller's BYE of the answered call,
// the transaction's death — into the webrtc-leg: the outbound dial's
// callee-hangup watcher's twin, armed at creation (the server dialog's
// ctx exists from the start). The bot's own terminations end the ctx
// too; the exactly-once end filters those out.
func (h *sipHandler) watchInbound(peer ss.SubscriberId, c *peerCall) {
	<-c.serverDialog.Context().Done()
	e, ok := c.end()
	if !ok {
		return // ended from this side already
	}
	h.calls.CompareAndDelete(peer, c)
	h.endBrowserLeg(context.Background(), c.w, e)
	if e.phase == phaseRinging {
		h.sayp(c, "Caller gave up.")
	} else {
		h.sayp(c, "Caller hung up.")
	}
}

// ringTimeout bounds an inbound call's ring: a browser left ringing is
// CANCELLed, the caller gets a 480. It loses a same-instant answer
// deliberately — endRinging only ends a still-ringing call.
func (h *sipHandler) ringTimeout(peer ss.SubscriberId, c *peerCall, ringCtx context.Context) {
	t := time.NewTimer(inboundRingTimeout)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ringCtx.Done():
		return // the ring ended — answered, or the call ended
	case <-c.serverDialog.Context().Done():
		return // the SIP side ended — the watcher owns the teardown
	}
	e, ok := c.endRinging()
	if !ok {
		return
	}
	h.calls.CompareAndDelete(peer, c)
	h.hangupDialog(e.dialog) // the dialog still rings: a 480
	if err := c.w.Cancel(e.callId); err != nil {
		h.logger.Warn("sipbot: CANCEL not sent", "peer", peer, "err", err)
	}
	if e.attached {
		h.detachLater(context.Background(), e.track, c.w)
	}
	h.sayp(c, "No answer — the call was cancelled.")
}

// answerInbound answers the inbound call's SIP side: the browser's 200
// OK arrived (handleResponse), so the caller gets his — diago's
// AnswerOptions builds the media session pinned to the selected codec
// (plus telephone-event, echoed when offered), sends the 200 OK with
// the SDP answer, and blocks until the caller's ACK, hence its own
// goroutine. On success the relay arms and the ring's clock stops; on a
// failure — or a call that ended while the ACK was awaited — the call
// ends with the browser leg told.
func (h *sipHandler) answerInbound(peer ss.SubscriberId, c *peerCall) {
	if err := c.serverDialog.AnswerOptions(diago.AnswerOptions{Codecs: c.answerCodecs()}); err != nil {
		// The usual failure is the caller's CANCEL crossing before the
		// answer landed; the dialog's ctx then ended and the watcher runs
		// the teardown — the end here filters out. Otherwise (a media
		// error, the ACK never arriving) end the call from this side.
		e, ok := c.end()
		if !ok {
			return
		}
		h.calls.CompareAndDelete(peer, c)
		h.hangupDialog(e.dialog)
		h.endBrowserLeg(context.Background(), c.w, e)
		h.sayp(c, fmt.Sprintf("Call failed: %v.", err))
		return
	}
	c.pendingCancel() // the ring's clock stops: the ring is answered
	if err := c.armRelay(); err != nil {
		if errors.Is(err, errCallEnded) {
			// The call ended while the ACK was awaited — the just-answered
			// dialog is this path's to hang up (the outbound dial's
			// discipline, mirrored).
			h.hangupDialog(c.serverDialog)
			return
		}
		// A relay-construction failure: the answered dialog's media
		// properties never came up.
		e, ok := c.end()
		if !ok {
			return
		}
		h.calls.CompareAndDelete(peer, c)
		h.hangupDialog(e.dialog)
		h.endBrowserLeg(context.Background(), c.w, e)
		h.sayp(c, fmt.Sprintf("Call failed: %v.", err))
		return
	}
	h.logger.Info("sipbot: the inbound call is answered; the relay is armed", "peer", peer, "callId", c.callId)
}

// declineInbound mirrors the user's own decline onto the inbound call's
// SIP side — a 603, the webrtc-leg's vocabulary — where the generic
// termination (hangupDialog) would be a 480.
func (h *sipHandler) declineInbound(dialog *diago.DialogServerSession) {
	go func() {
		if err := dialog.Respond(603, "Decline", nil); err != nil {
			h.logger.Warn("sipbot: inbound-call decline not sent", "err", err)
		}
	}()
}

// callerOf renders the inbound caller's identity for the chat lines —
// "user@host" off the INVITE's From header. diago's dialog creation has
// already validated the From (the dialog id needs its tag); the guard is
// for the degenerate wire shapes.
func callerOf(inDialog *diago.DialogServerSession) string {
	from := inDialog.InviteRequest.From()
	if from == nil || from.Address.User == "" {
		return "an unknown caller"
	}
	return from.Address.User + "@" + from.Address.Host
}
