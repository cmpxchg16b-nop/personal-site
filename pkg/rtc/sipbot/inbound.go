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
//   - diago's Answer is the 200 OK: it builds the media session from the
//     account's codec list (the same opus-first preference the outbound
//     offer carries), sends the SDP answer, and blocks until the
//     caller's ACK (64·T1 bound) — hence its own goroutine, and the
//     exactly-once arm discipline shared with the outbound setDialog.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/emiago/diago"
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

// serveInbound handles one inbound SIP call: ring the peer's browser,
// and on its accept answer the SIP side and join the legs. It runs on
// the account's diago serve goroutine and blocks until the dialog ends
// — diago hangs up and closes the dialog when its serve handler returns,
// so the return IS the call's end (the ctx's done is the one signal for
// every ending, ours included).
func (h *sipHandler) serveInbound(peer ss.SubscriberId, inDialog *diago.DialogServerSession) {
	caller := callerOf(inDialog)
	h.logger.Info("sipbot: inbound SIP call", "peer", peer, "from", caller)
	w := h.writers.WriterFor(peer)
	track, err := webrtc.NewTrackLocalStaticSample(voiceTrackCodec, "sip-voice", "sipbot")
	if err != nil {
		h.logger.Warn("sipbot: inbound call, track not prepared", "peer", peer, "err", err)
		if rerr := inDialog.Respond(480, "Temporarily Unavailable", nil); rerr != nil {
			h.logger.Warn("sipbot: inbound-call rejection not sent", "peer", peer, "err", rerr)
		}
		return
	}
	ringCtx, ringCancel := context.WithCancel(context.Background())
	c := &peerCall{
		target:        caller,
		inbound:       true,
		serverDialog:  inDialog,
		track:         track,
		w:             w,
		pendingCancel: ringCancel,
		dialog:        inDialog,
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
	// Register for the browser's mic before the INVITE goes out, so no
	// early track is missed (the /call discipline).
	w.OnTrack(func(remote *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		h.onMic(peer, remote, receiver)
	})
	callId, err := w.Invite(msg_handler.MediaVoice)
	if err != nil {
		// The peer has no live session — nobody to ring.
		h.calls.CompareAndDelete(peer, c)
		ringCancel()
		h.logger.Info("sipbot: inbound call to an offline peer", "peer", peer, "from", caller, "err", err)
		if rerr := inDialog.Respond(480, "Temporarily Unavailable", nil); rerr != nil {
			h.logger.Warn("sipbot: offline answer not sent", "peer", peer, "err", rerr)
		}
		return
	}
	c.callId = callId
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

// watchInbound relays the SIP side's own end of an inbound call — the
// caller's CANCEL of the ring, the caller's BYE of the answered call,
// the transaction's death — into the webrtc-leg: the outbound dial's
// callee-hangup watcher's twin, armed at creation (the server dialog's
// ctx exists from the start). The bot's own terminations end the ctx
// too; the exactly-once end filters those out.
func (h *sipHandler) watchInbound(peer ss.SubscriberId, c *peerCall) {
	<-c.serverDialog.Context().Done()
	_, phase, ok := c.end()
	if !ok {
		return // ended from this side already
	}
	h.calls.CompareAndDelete(peer, c)
	h.tellBrowserEnded(c, phase)
	if phase == phaseRinging {
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
	dialog, _, ok := c.endRinging()
	if !ok {
		return
	}
	h.calls.CompareAndDelete(peer, c)
	h.hangupDialog(dialog) // the dialog still rings: a 480
	if err := c.w.Cancel(c.callId); err != nil {
		h.logger.Warn("sipbot: CANCEL not sent", "peer", peer, "err", err)
	}
	h.sayp(c, "No answer — the call was cancelled.")
}

// answerInbound answers the inbound call's SIP side: the browser's 200
// OK arrived (handleResponse), so the caller gets his — diago's Answer
// builds the media session, sends the 200 OK with the SDP answer, and
// blocks until the caller's ACK, hence its own goroutine. On success the
// relay arms and the ring's clock stops; on a failure — or a call that
// ended while the ACK was awaited — the call ends with the browser leg
// told.
func (h *sipHandler) answerInbound(peer ss.SubscriberId, c *peerCall) {
	if err := c.serverDialog.Answer(); err != nil {
		// The usual failure is the caller's CANCEL crossing before the
		// answer landed; the dialog's ctx then ended and the watcher runs
		// the teardown — the end here filters out. Otherwise (a media
		// error, the ACK never arriving) end the call from this side.
		dialog, phase, ok := c.end()
		if !ok {
			return
		}
		h.calls.CompareAndDelete(peer, c)
		h.hangupDialog(dialog)
		h.tellBrowserEnded(c, phase)
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
		// A relay-construction failure: the negotiated codec is not
		// relayable (a pure-Go build facing G.711).
		dialog, phase, ok := c.end()
		if !ok {
			return
		}
		h.calls.CompareAndDelete(peer, c)
		h.hangupDialog(dialog)
		h.tellBrowserEnded(c, phase)
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
