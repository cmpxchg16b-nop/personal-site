package sipbot

// This file is the sip bot's message policy — a
// msg_handler.BotMessageHandler. Everything protocol-shaped (the echo
// discipline, the transfer reassembly, the call log's status amends)
// lives a layer down, in msg_handler's Server; what lives here is what
// the messages MEAN to a session border controller:
//
//   - chat is the bot's CLI (/help, /register, /unregister, /call,
//     /hangup),
//   - attachments are refused with a chat reply,
//   - an incoming call from the browser is declined (the bot is an
//     outbound SBC: a browser-originated call has no SIP meaning),
//   - /call opens both legs of a B2BUA: it phones the user on the
//     webrtc-leg (the bot's own INVITE over dcmsg) and phones the callee
//     on the sip-leg (a diago INVITE), then relays the two legs'
//     signalling and media (see call.go).
//
// State discipline: per-peer accounts and calls live in sync.Maps keyed
// by the peer. Handler invocations for one peer's dcmsg channel are
// serialized (the framework's guarantee); the genuinely asynchronous
// pieces — the SIP dial goroutine, the callee's hangup watcher, the
// session watchers — meet the channel goroutine only through the
// peerCall's small mutex and its exactly-once end, and through
// CompareAndDelete on the maps. Async answers to the chat (the callee's
// progress, hangup, failures) go out on the ResponseWriter the /call
// invocation captured — a writer is a plain value bound to its peer,
// safe to keep (its sends thread on the /call command, which is where
// news of the call belongs).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emiago/diago"
	"github.com/pion/webrtc/v4"

	"personal-site/pkg/models/ss"
	"personal-site/pkg/rtc/msg_handler"
)

// The CLI's answer texts.
const (
	helpText = "I bridge this chat into the SIP telephone network: register your own SIP account, then phone any SIP subscriber — the call rings here, in your browser.\n" +
		"Commands:\n" +
		"/help — show this help\n" +
		"/register <user@host> <password> — register your SIP account (e.g. /register 2001@sip.example.com passW_0rd)\n" +
		"/unregister — drop the registration and the stored credential\n" +
		"/call <user@host> — phone a SIP subscriber (a bare user keeps your account's domain)\n" +
		"/test-call — phone the bot's configured test callee\n" +
		"/hangup — end the current call\n" +
		"No account of your own? Just /call — when the bot's pool has a free account, it lends you one."
	attachmentRefusal = "Attachments are not supported — I'm a SIP bot. Try /help."
	unknownCommand    = "Unrecognized command — try /help."
	registerUsage     = "Usage: /register <user@host> <password> — e.g. /register 2001@sip.example.com passW_0rd."
	callUsage         = "Usage: /call <user@host> — e.g. /call 1001@sip.example.com."
	notRegistered     = "Not registered — /register <user@host> <password> first."
	poolExhausted     = "The bot's pool of SIP accounts is empty — /register <user@host> <password> to use your own account."
	testUnavailable   = "Test call unavailable — the bot has no testSIPContact configured."
)

// hangupDetachDelay is how long the withdrawal of the call's track waits
// after a hangup before going out — the music bot's discipline: the
// peer's own teardown renegotiation crosses within this window, so the
// two offers never meet in flight as a glare the polite yield would
// answer with a peer-connection rebuild a browser cannot survive.
const hangupDetachDelay = 500 * time.Millisecond

// voiceTrackCodec carries the webrtc-leg's audio: opus 48 kHz. The fmtp
// mirrors the server-side peer connections' (cmd/server's
// stereoOpusPCFactory): stereo=1 keeps a stereo opus source audible in
// stereo through the passthrough; the transcoded voice path simply
// encodes mono packets, which are valid RFC 7587 on it.
var voiceTrackCodec = webrtc.RTPCodecCapability{
	MimeType:    webrtc.MimeTypeOpus,
	ClockRate:   48000,
	Channels:    2,
	SDPFmtpLine: "minptime=10;useinbandfec=1;stereo=1",
}

// sipHandler is the sip bot's msg_handler.BotMessageHandler.
type sipHandler struct {
	logger  *slog.Logger
	storage UserSessionStorage
	// pool is the bot's pool of SIP accounts (nil: no pool), loaned to a
	// user who /calls without a credential of their own.
	pool *SIPCredentialPool
	// stack opens each account's own SIP client — one per registered
	// user, never a client shared across users.
	stack  sipStack
	expiry time.Duration

	// testContact is the CLI /test-call command's SIP callee (the
	// Configuration's TestSIPContact); empty disables the command.
	testContact string

	// accounts is the per-peer SIP runtime (registrations); calls the
	// per-peer call state. The maps are shared across peers' channel
	// goroutines; the values' own synchronization is theirs.
	accounts sync.Map // ss.SubscriberId → *account
	calls    sync.Map // ss.SubscriberId → *peerCall
}

var _ msg_handler.BotMessageHandler = (*sipHandler)(nil)

func newSipHandler(logger *slog.Logger, storage UserSessionStorage, pool *SIPCredentialPool, stack sipStack, expiry time.Duration, testContact string) *sipHandler {
	return &sipHandler{logger: logger, storage: storage, pool: pool, stack: stack, expiry: expiry, testContact: testContact}
}

// HandleChatMessage is the CLI: parse the line, answer it.
func (h *sipHandler) HandleChatMessage(ctx context.Context, msg *msg_handler.ChatMessage, w msg_handler.ResponseWriter) {
	fields := strings.Fields(msg.Text)
	if len(fields) == 0 {
		h.say(msg.From, w, unknownCommand)
		return
	}
	switch fields[0] {
	case "/help":
		h.say(msg.From, w, helpText)
	case "/register":
		h.register(ctx, msg, w, fields[1:])
	case "/unregister":
		h.unregister(ctx, msg, w)
	case "/call":
		h.call(ctx, msg, w, fields[1:])
	case "/test-call":
		h.testCall(ctx, msg, w)
	case "/hangup":
		h.hangup(ctx, msg, w)
	default:
		h.say(msg.From, w, unknownCommand)
	}
}

// HandleFileChunk does nothing: the Server owns the transfer mechanics;
// the refusal is spoken once, when the attachment completes.
func (h *sipHandler) HandleFileChunk(_ context.Context, _ *msg_handler.FileAnnouncement, _ *msg_handler.FileChunk, _ msg_handler.ResponseWriter) {
}

// HandleAttachment refuses the finished attachment: the sip bot takes
// no files, of any kind.
func (h *sipHandler) HandleAttachment(_ context.Context, _ *msg_handler.FileAnnouncement, attachment *msg_handler.Attachment, w msg_handler.ResponseWriter) {
	if _, err := w.Reply(attachmentRefusal); err != nil {
		h.logger.Warn("sipbot: attachment refusal not sent", "peer", attachment.Peer, "err", err)
	}
}

// HandleCalling is the call policy.
func (h *sipHandler) HandleCalling(ctx context.Context, sip *msg_handler.SipMessage, w msg_handler.ResponseWriter) {
	switch {
	case sip.Method == msg_handler.SipMethodInvite:
		h.handleInvite(sip, w)
	case sip.Response != nil:
		h.handleResponse(sip, w)
	case sip.Method == msg_handler.SipMethodBye || sip.Method == msg_handler.SipMethodCancel:
		h.handleHangup(ctx, sip, w)
	}
}

// HandlePeerSessionStart is a deliberate no-op: allocation from the
// credential pool is on-demand (at /call), never at session start — the
// bot holds a peer session with every online channel member, and
// presence alone must not loan accounts to lurkers who never dial.
func (h *sipHandler) HandlePeerSessionStart(_ context.Context, _ ss.SubscriberId) {
}

// HandlePeerSessionEnd is the bot's session-end teardown — the
// framework's lifecycle hook, replacing what used to be a per-call and
// a per-account ctx-watcher goroutine: the call in progress ends (the
// callee's dialog gets its BYE; the browser leg is gone with the
// session, so there is nothing to tell it), the account stops (its own
// ctx teardown sends the de-REGISTER), and a pooled credential — a loan
// whose scope is the session — is dropped from the store and returned
// to the pool. A manual credential survives in the store by design: the
// next session's /call revives it. Runs on the Server's hub goroutine:
// all of it is fast bookkeeping (the dialog's BYE is sent on its own
// goroutine, as everywhere).
func (h *sipHandler) HandlePeerSessionEnd(_ context.Context, peer ss.SubscriberId) {
	if v, ok := h.calls.Load(peer); ok {
		c := v.(*peerCall)
		if dialog, _, ok := c.end(); ok {
			h.calls.CompareAndDelete(peer, c)
			h.hangupDialog(dialog)
			h.logger.Info("sipbot: the session's end ended the call", "peer", peer, "callId", c.callId)
		}
	}
	if v, ok := h.accounts.Load(peer); ok {
		a := v.(*account)
		a.stop()
		h.accounts.CompareAndDelete(peer, a)
	}
	if session, ok := h.storage.Load(peer); ok && session.Pooled {
		h.storage.Delete(peer)
		h.pool.Release(session)
		h.logger.Info("sipbot: the pooled account is back in the pool", "peer", peer, "aor", session.AddressOfRecord)
	}
}

// handleInvite declines an incoming call, voice and video alike: the
// bot is an outbound SBC — a browser-originated call has no SIP meaning
// without a routing target.
func (h *sipHandler) handleInvite(sip *msg_handler.SipMessage, w msg_handler.ResponseWriter) {
	h.logger.Info("sipbot: declining an incoming call", "peer", sip.From, "media", sip.Media, "callId", sip.CallId)
	if err := w.Reject(msg_handler.SipCodeDecline, msg_handler.SipPhraseDecline); err != nil {
		h.logger.Warn("sipbot: call decline not sent", "peer", sip.From, "err", err)
	}
}

// handleResponse folds the browser's answer to the bot's INVITE: on 200
// OK the call's opus track goes on the wire; on a decline the call —
// the sip-leg's dial included — is dropped.
func (h *sipHandler) handleResponse(sip *msg_handler.SipMessage, w msg_handler.ResponseWriter) {
	peer := sip.From
	v, ok := h.calls.Load(peer)
	if !ok {
		return // an answer to a call the bot never opened (or already ended)
	}
	c := v.(*peerCall)
	if c.callId != sip.CallId {
		return
	}
	if sip.Response.Code == msg_handler.SipCodeOK {
		if !c.activate() {
			return // a duplicate answer, or the call already ended
		}
		if err := w.AttachMedia(c.track); err != nil {
			// The call stands answered but carries no media toward the
			// browser; the peer can still hang up, which resets the state
			// (the music bot's failure shape).
			h.logger.Warn("sipbot: attach on accepted call failed", "peer", peer, "err", err)
			return
		}
		h.logger.Info("sipbot: the browser answered; the track is on the wire", "peer", peer, "callId", c.callId)
		return
	}
	dialog, _, ok := c.end()
	if !ok {
		return
	}
	h.calls.CompareAndDelete(peer, c)
	h.hangupDialog(dialog)
	h.logger.Info("sipbot: call declined by the user", "peer", peer, "callId", c.callId)
	h.sayp(c, "Call declined.")
}

// handleHangup ends the call the user hung up (BYE) or aborted
// (CANCEL): the sip-leg's dialog goes down with it, the track is
// withdrawn after the peer's teardown offer has passed.
func (h *sipHandler) handleHangup(ctx context.Context, sip *msg_handler.SipMessage, _ msg_handler.ResponseWriter) {
	peer := sip.From
	v, ok := h.calls.Load(peer)
	if !ok {
		return
	}
	c := v.(*peerCall)
	if c.callId != sip.CallId {
		return
	}
	dialog, phase, ok := c.end()
	if !ok {
		return
	}
	h.calls.CompareAndDelete(peer, c)
	h.hangupDialog(dialog)
	if phase == phaseActive {
		h.detachLater(ctx, c)
	}
	h.logger.Info("sipbot: call ended by the user", "peer", peer, "callId", c.callId)
}

// register is the /register command: validate the credential, end any
// call (the identity is changing), REGISTER the AOR — the first
// REGISTER synchronously, so the reply carries the registrar's actual
// answer — and keep the registration alive in the background. A
// previous registration is replaced.
func (h *sipHandler) register(ctx context.Context, msg *msg_handler.ChatMessage, w msg_handler.ResponseWriter, args []string) {
	peer := msg.From
	if len(args) != 2 {
		h.say(peer, w, registerUsage)
		return
	}
	session, err := parseAddressOfRecord(args[0], args[1])
	if err != nil {
		h.say(peer, w, fmt.Sprintf("Could not register: %v.", err))
		return
	}
	if v, ok := h.calls.Load(peer); ok {
		h.endCall(ctx, peer, v.(*peerCall))
		h.say(peer, w, "The call in progress was ended.")
	}
	a, err := newAccount(ctx, h.logger, h.stack, session, h.expiry)
	if err != nil {
		h.say(peer, w, fmt.Sprintf("Registration failed: %v.", err))
		return
	}
	h.setAccount(peer, a)
	// A replaced pool loan returns to the pool; the new credential is the
	// user's own.
	if old, ok := h.storage.Load(peer); ok && old.Pooled {
		h.pool.Release(old)
	}
	h.storage.Store(peer, session)
	h.say(peer, w, fmt.Sprintf("Registered as sip:%s.", session.AddressOfRecord))
}

// unregister is the /unregister command: the call ends, the
// registration is taken down (a de-REGISTER goes out), the stored
// credential is dropped.
func (h *sipHandler) unregister(ctx context.Context, msg *msg_handler.ChatMessage, w msg_handler.ResponseWriter) {
	peer := msg.From
	if v, ok := h.calls.Load(peer); ok {
		h.endCall(ctx, peer, v.(*peerCall))
	}
	if v, ok := h.accounts.Load(peer); ok {
		a := v.(*account)
		a.stop()
		h.accounts.CompareAndDelete(peer, a)
	}
	if session, ok := h.storage.Delete(peer); !ok {
		h.say(peer, w, notRegistered)
		return
	} else if session.Pooled {
		// The loan returns to the pool with its store entry.
		h.pool.Release(session)
	}
	h.say(peer, w, "Unregistered.")
}

// call is the /call command: with a registration in place and no call
// standing, the bot phones the user on the webrtc-leg and the callee on
// the sip-leg, and joins them.
func (h *sipHandler) call(ctx context.Context, msg *msg_handler.ChatMessage, w msg_handler.ResponseWriter, args []string) {
	peer := msg.From
	if len(args) != 1 {
		h.say(peer, w, callUsage)
		return
	}
	if _, ok := h.calls.Load(peer); ok {
		h.say(peer, w, "Already in a call — /hangup first.")
		return
	}
	a, pooled, err := h.ensureAccount(ctx, peer)
	if err != nil {
		h.say(peer, w, err.Error())
		return
	}
	if pooled {
		// The user is phoning with a loaned account — they should know
		// which identity the callee sees.
		h.say(peer, w, fmt.Sprintf("Registered as sip:%s (an account from the bot's pool).", a.session.AddressOfRecord))
	}
	track, err := webrtc.NewTrackLocalStaticSample(voiceTrackCodec, "sip-voice", "sipbot")
	if err != nil {
		h.say(peer, w, fmt.Sprintf("Could not prepare the call: %v.", err))
		return
	}
	dialCtx, dialCancel := context.WithCancel(ctx)
	c := &peerCall{target: args[0], track: track, w: w, dialCancel: dialCancel}
	h.calls.Store(peer, c)
	// Register for the browser's mic before the INVITE goes out, so no
	// early track is missed (the music bot's discipline).
	w.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		h.onMic(peer, remote)
	})
	callId, err := w.Invite(msg_handler.MediaVoice)
	if err != nil {
		h.calls.CompareAndDelete(peer, c)
		dialCancel()
		h.say(peer, w, fmt.Sprintf("Could not call you: %v.", err))
		return
	}
	c.callId = callId
	h.say(peer, w, fmt.Sprintf("Calling %s…", args[0]))
	// The dial runs on its own goroutine: ringing the PSTN takes
	// seconds, and the peer's channel goroutine never waits on it. The
	// session's end ends the call — the lifecycle hook
	// (HandlePeerSessionEnd) owns that teardown.
	go h.dial(a, peer, c, dialCtx)
}

// testCall is the /test-call command: a /call to the bot's configured
// test callee (a known-good subscriber of the SIP network the
// deployment tests against), unavailable when none is configured.
func (h *sipHandler) testCall(ctx context.Context, msg *msg_handler.ChatMessage, w msg_handler.ResponseWriter) {
	if h.testContact == "" {
		h.say(msg.From, w, testUnavailable)
		return
	}
	h.call(ctx, msg, w, []string{h.testContact})
}

// hangup is the /hangup command: the call's user-side end from chat —
// the browser leg is told (CANCEL while it rings, BYE once answered),
// the sip-leg's dialog goes down.
func (h *sipHandler) hangup(ctx context.Context, msg *msg_handler.ChatMessage, w msg_handler.ResponseWriter) {
	peer := msg.From
	v, ok := h.calls.Load(peer)
	if !ok {
		h.say(peer, w, "No call in progress.")
		return
	}
	c := v.(*peerCall)
	dialog, phase, ok := c.end()
	if !ok {
		return
	}
	h.calls.CompareAndDelete(peer, c)
	h.hangupDialog(dialog)
	if phase == phaseRinging {
		if err := w.Cancel(c.callId); err != nil {
			h.logger.Warn("sipbot: CANCEL not sent", "peer", peer, "err", err)
		}
	} else {
		if err := w.Bye(c.callId); err != nil {
			h.logger.Warn("sipbot: BYE not sent", "peer", peer, "err", err)
		}
		h.detachLater(ctx, c)
	}
	h.say(peer, w, "Hung up.")
}

// dial rings the callee: one diago INVITE on the account's identity,
// answered when the callee answers. Its endings, all exactly-once
// through the call's end: a dial failure (busy, unreachable — or the
// user hanging up first, which cancels the dial's ctx) terminates the
// browser leg and says why; an answered dialog arms the relay and gets
// its hangup watcher.
func (h *sipHandler) dial(a *account, peer ss.SubscriberId, c *peerCall, dialCtx context.Context) {
	var rang atomic.Bool
	dialog, err := a.dial(dialCtx, c.target, func(code int) {
		if (code == 180 || code == 183) && rang.CompareAndSwap(false, true) {
			h.logger.Info("sipbot: [dbg] ringing, no sayp")
		}
	})
	if err != nil {
		dlg, phase, ok := c.end()
		if !ok {
			return // the call ended while the dial was in flight
		}
		h.calls.CompareAndDelete(peer, c)
		h.hangupDialog(dlg)
		h.tellBrowserEnded(c, phase)
		h.sayp(c, fmt.Sprintf("Call failed: %v.", err))
		return
	}
	if err := c.setDialog(dialog); err != nil {
		h.hangupDialog(dialog)
		if errors.Is(err, errCallEnded) {
			return // the user ended it while the callee was being rung
		}
		_, phase, ok := c.end()
		if !ok {
			return
		}
		h.calls.CompareAndDelete(peer, c)
		h.tellBrowserEnded(c, phase)
		h.sayp(c, fmt.Sprintf("Call failed: %v.", err))
		return
	}
	h.sayp(c, "Callee answered.")
	// The callee's hangup (the dialog's ctx) ends the whole call: the
	// browser leg is told, the state reset.
	go func() {
		<-dialog.Context().Done()
		dlg, phase, ok := c.end()
		if !ok {
			return // ended from this side already
		}
		h.calls.CompareAndDelete(peer, c)
		h.hangupDialog(dlg)
		h.tellBrowserEnded(c, phase)
		h.sayp(c, "Callee hung up.")
	}()
}

// endCall runs the call's full teardown from the peer's channel
// goroutine (/unregister, a replacing /register): the sip-leg's dialog
// is hung up, the browser leg told, the track withdrawn on the hangup
// delay, and the user told.
func (h *sipHandler) endCall(ctx context.Context, peer ss.SubscriberId, c *peerCall) {
	dialog, phase, ok := c.end()
	if !ok {
		return
	}
	h.calls.CompareAndDelete(peer, c)
	h.hangupDialog(dialog)
	if phase == phaseRinging {
		if err := c.w.Cancel(c.callId); err != nil {
			h.logger.Warn("sipbot: CANCEL not sent", "peer", peer, "err", err)
		}
	} else {
		if err := c.w.Bye(c.callId); err != nil {
			h.logger.Warn("sipbot: BYE not sent", "peer", peer, "err", err)
		}
		h.detachLater(ctx, c)
	}
}

// tellBrowserEnded terminates the call's webrtc-leg from the SIP side's
// async paths: CANCEL while the browser still rings, BYE once it
// answered — and in the answered case the track's withdrawal waits out
// the peer's own teardown offer.
func (h *sipHandler) tellBrowserEnded(c *peerCall, phase callPhase) {
	if phase == phaseRinging {
		if err := c.w.Cancel(c.callId); err != nil {
			h.logger.Warn("sipbot: CANCEL not sent", "err", err)
		}
		return
	}
	if err := c.w.Bye(c.callId); err != nil {
		h.logger.Warn("sipbot: BYE not sent", "err", err)
	}
	h.detachLater(context.Background(), c)
}

// hangupDialog sends the answered dialog's BYE without blocking the
// caller; a nil dialog (the dial never answered) needs none — its
// transaction died with the dial's ctx.
func (h *sipHandler) hangupDialog(dialog *diago.DialogClientSession) {
	if dialog == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := dialog.Hangup(ctx); err != nil {
			h.logger.Warn("sipbot: SIP BYE failed", "err", err)
		}
	}()
}

// detachLater withdraws the call's track after the hangup delay — see
// the constant's comment; the music bot's handleHangup has the same
// shape, and so does its reason.
func (h *sipHandler) detachLater(ctx context.Context, c *peerCall) {
	go func() {
		t := time.NewTimer(hangupDetachDelay)
		defer t.Stop()
		select {
		case <-t.C:
			if err := c.w.DetachMedia(c.track); err != nil {
				h.logger.Warn("sipbot: track not detached on hangup", "err", err)
			}
		case <-ctx.Done():
		}
	}()
}

// ensureAccount answers the peer's live registration: the account in
// the map when it is believed registered; else a revival of the stored
// credential; else — for a user who never /registered — a loan from the
// bot's credential pool, registered and stored exactly like a
// /register's outcome (the pooled flag tells the caller to announce it).
// The revive's and the loan's first REGISTER are synchronous and
// bounded, like /register's. The errors are the user-facing answers,
// each pointing at /register: no credential at all (no pool
// configured), an exhausted pool, or a failed registration — a pooled
// account that fails to register returns to the pool.
func (h *sipHandler) ensureAccount(ctx context.Context, peer ss.SubscriberId) (*account, bool, error) {
	if v, ok := h.accounts.Load(peer); ok {
		a := v.(*account)
		if a.registered.Load() {
			return a, false, nil
		}
	}
	session, ok := h.storage.Load(peer)
	pooled := false
	if !ok {
		if h.pool == nil {
			return nil, false, errors.New(notRegistered)
		}
		session, ok = h.pool.Allocate()
		if !ok {
			h.logger.Warn("sipbot: the credential pool is exhausted", "peer", peer)
			return nil, false, errors.New(poolExhausted)
		}
		pooled = true
		h.logger.Info("sipbot: loaned a pooled account", "peer", peer, "aor", session.AddressOfRecord)
	}
	a, err := newAccount(ctx, h.logger, h.stack, session, h.expiry)
	if err != nil {
		if pooled {
			h.pool.Release(session)
			return nil, false, fmt.Errorf("the bot's pooled account sip:%s could not be registered (%v) — /register <user@host> <password> to use your own account", session.AddressOfRecord, err)
		}
		return nil, false, fmt.Errorf("registration failed: %w", err)
	}
	h.setAccount(peer, a)
	if pooled {
		h.storage.Store(peer, session)
	}
	return a, pooled, nil
}

// setAccount publishes a fresh account, stopping the one it replaces.
// The account's ctx is the peer session's, and the session-end hook
// (HandlePeerSessionEnd) drops it from the map when the session ends —
// the credential in the store survives a manual one, a pooled one
// returns to the pool there.
func (h *sipHandler) setAccount(peer ss.SubscriberId, a *account) {
	if old, ok := h.accounts.Load(peer); ok {
		old.(*account).stop()
	}
	h.accounts.Store(peer, a)
}

// onMic is the inbound-media callback: the browser's mic track joins
// the call's relay (the browser→SIP pump starts once both it and the
// answered dialog exist). A mic that is not opus — every browser offers
// opus first, so this is the degenerate case — is drained, not
// transcoded: the sip-leg hears silence, and the log says why.
func (h *sipHandler) onMic(peer ss.SubscriberId, track *webrtc.TrackRemote) {
	h.logger.Info("sipbot: inbound track", "peer", peer, "codec", track.Codec().MimeType)
	v, ok := h.calls.Load(peer)
	if !ok || track.Codec().MimeType != webrtc.MimeTypeOpus {
		if ok {
			h.logger.Warn("sipbot: the mic is not opus; draining it", "peer", peer, "codec", track.Codec().MimeType)
		}
		go func() {
			for {
				if _, _, err := track.ReadRTP(); err != nil {
					return
				}
			}
		}()
		return
	}
	v.(*peerCall).setMic(track)
}

// say answers the peer with a chat message, logging a failed send.
func (h *sipHandler) say(peer ss.SubscriberId, w msg_handler.ResponseWriter, text string) {
	if _, err := w.Reply(text); err != nil {
		h.logger.Warn("sipbot: reply not sent", "peer", peer, "err", err)
	}
}

// sayp answers on the call's captured writer — the SIP leg's async news
// (progress, answer, hangup, failure), threaded on the /call command.
func (h *sipHandler) sayp(c *peerCall, text string) {
	if _, err := c.w.Reply(text); err != nil {
		h.logger.Warn("sipbot: reply not sent", "err", err)
	}
}
