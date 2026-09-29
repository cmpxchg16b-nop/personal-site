package sipbot

// This file is the sip bot's message policy — a
// msg_handler.BotMessageHandler. Everything protocol-shaped (the echo
// discipline, the transfer reassembly, the call log's status amends)
// lives a layer down, in msg_handler's Server; what lives here is what
// the messages MEAN to a session border controller:
//
//   - chat is the bot's CLI (/help, /register, /unregister,
//     /my-number, /call, /yellow-page, /test-call, /hangup),
//   - attachments are refused with a chat reply,
//   - an incoming call from the browser is declined (a browser-
//     originated call to the bot has no SIP routing target),
//   - /call opens both legs of a B2BUA: it phones the callee on the
//     sip-leg (a diago INVITE), and once the callee answered — the
//     answer's codec selection in hand — phones the user on the
//     webrtc-leg (the bot's own INVITE over dcmsg, the speaker track's
//     offer pinned to the selection), then relays the two legs'
//     signalling and media (see call.go),
//   - an inbound SIP call to the user's registered AOR does the same in
//     reverse: it rings the user's browser (see inbound.go).
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
	helpText = "I bridge this chat into the SIP telephone network: register your own SIP account, then phone any SIP subscriber — and be phoned back on your number. The call rings here, in your browser.\n" +
		"Commands:\n" +
		"/help — show this help\n" +
		"/register <user@host> <password> — register your SIP account (e.g. /register 2001@sip.example.com passW_0rd)\n" +
		"/unregister — drop the registration and the stored credential\n" +
		"/my-number — show your registered SIP number — the one others can call you on\n" +
		"/call <user@host> — phone a SIP subscriber (a bare user keeps your account's domain)\n" +
		"/yellow-page — list the phone book: the example numbers you can call\n" +
		"/test-call — phone the bot's configured test callee\n" +
		"/hangup — end the current call\n" +
		"No account of your own? When the bot's pool has a free account it lends you one as you arrive (and tells you your number) — or just /call."
	attachmentRefusal = "Attachments are not supported — I'm a SIP bot. Try /help."
	unknownCommand    = "Unrecognized command — try /help."
	registerUsage     = "Usage: /register <user@host> <password> — e.g. /register 2001@sip.example.com passW_0rd."
	callUsage         = "Usage: /call <user@host> — e.g. /call 1001@sip.example.com."
	notRegistered     = "Not registered — /register <user@host> <password> first."
	noNumber          = "Not registered to a SIP registrar yet — /register <user@host> <password> to get a number."
	noNumberPoolHint  = " Or just /call — when the bot's pool has a free account, it lends you one."
	poolExhausted     = "The bot's pool of SIP accounts is empty — /register <user@host> <password> to use your own account."
	testUnavailable   = "Test call unavailable — the bot has no testSIPContact configured."
)

// hangupDetachDelay is how long the withdrawal of the call's track waits
// after a hangup before going out — the music bot's discipline: the
// peer's own teardown renegotiation crosses within this window, so the
// two offers never meet in flight as a glare the polite yield would
// answer with a peer-connection rebuild a browser cannot survive.
const hangupDetachDelay = 500 * time.Millisecond

// sipHandler is the sip bot's msg_handler.BotMessageHandler.
type sipHandler struct {
	logger  *slog.Logger
	storage UserSessionStorage
	// pool is the bot's pool of SIP accounts (nil: no pool), loaned to a
	// user who /calls without a credential of their own.
	pool *SIPCredentialPool
	// stack opens each account's own SIP client — one per registered
	// user, never a client shared across users.
	stack sipStack

	// testContact is the CLI /test-call command's SIP callee (the
	// Configuration's TestSIPContact); empty disables the command.
	testContact string

	// yellowPage is the bot's phone book (the Configuration's
	// YellowPage), listed by the CLI's /yellow-page command.
	yellowPage []YellowPageSection

	// accounts is the per-peer SIP runtime (registrations); calls the
	// per-peer call state. The maps are shared across peers' channel
	// goroutines; the values' own synchronization is theirs.
	accounts sync.Map // ss.SubscriberId → *account
	calls    sync.Map // ss.SubscriberId → *peerCall

	// accountMu serializes the account lifecycle — the session-start
	// loan, /register, /unregister, /call's ensure, the session-end
	// teardown — so the start hook's off-hub registration cannot race a
	// command's own. The critical section includes the bounded first
	// REGISTER (registerTimeout): a command, or the hub's end hook, can
	// wait behind another account's registration for at most that bound.
	// Nothing hub-bound (WriterFor) runs under it — the hub itself takes
	// it in the end hook.
	accountMu sync.Mutex

	// writers is the unsolicited-send path (the msg_handler.Server's
	// WriterFor): a ResponseWriter for a peer outside any message — the
	// inbound SIP call's way to ring the browser. Set by New at wiring
	// time (before the client serves), read-only after.
	writers writerSource
}

// writerSource is the handler's unsolicited-send path — the
// msg_handler.Server's WriterFor, behind an interface so tests can
// substitute it.
type writerSource interface {
	WriterFor(peer ss.SubscriberId) msg_handler.ResponseWriter
}

var _ msg_handler.BotMessageHandler = (*sipHandler)(nil)

func newSipHandler(logger *slog.Logger, storage UserSessionStorage, pool *SIPCredentialPool, stack sipStack, testContact string, yellowPage []YellowPageSection) *sipHandler {
	return &sipHandler{logger: logger, storage: storage, pool: pool, stack: stack, testContact: testContact, yellowPage: yellowPage}
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
	case "/my-number":
		h.myNumber(msg, w)
	case "/call":
		h.call(ctx, msg, w, fields[1:])
	case "/yellow-page":
		h.say(msg.From, w, yellowPageText(h.yellowPage))
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

// HandlePeerSessionStart loans and registers a pooled account for a
// peer who brought no credential of their own — early, at the session's
// birth rather than on the first /call: the call then need not bear the
// REGISTER round trip, and the peer's number is callable (inbound.go)
// for the session's whole life. Best-effort: an empty pool or a failed
// registration leaves the peer to /call's on-demand path, which answers
// the failure itself. The work is spawned — the hub never waits on the
// REGISTER's round trip.
func (h *sipHandler) HandlePeerSessionStart(ctx context.Context, peer ss.SubscriberId) {
	if h.pool == nil {
		return
	}
	go h.loanPooledAccount(ctx, peer)
}

// loanPooledAccount is the session start's off-hub work: allocate a
// pooled account for the peer and REGISTER it, exactly like /register's
// outcome, then tell the peer their number (the unsolicited path — the
// /call-time announcement's earlier twin, so the user always knows which
// identity a callee sees). Serialized with the commands' own account
// lifecycle by accountMu: a peer who /registered or /called first (or
// whose session already ended) is left alone.
func (h *sipHandler) loanPooledAccount(ctx context.Context, peer ss.SubscriberId) {
	h.accountMu.Lock()
	if ctx.Err() != nil {
		h.accountMu.Unlock()
		return // the session ended before the loan even started
	}
	if _, ok := h.accounts.Load(peer); ok {
		h.accountMu.Unlock()
		return // an account already stands (a fast /call)
	}
	if _, ok := h.storage.Load(peer); ok {
		h.accountMu.Unlock()
		return // the peer brought their own credential meanwhile
	}
	session, ok := h.pool.Allocate()
	if !ok {
		h.accountMu.Unlock()
		h.logger.Info("sipbot: no pooled account for the session-start loan", "peer", peer)
		return
	}
	a, err := newAccount(ctx, h.logger, h.stack, session, h.inboundServe(peer))
	if err != nil {
		h.accountMu.Unlock()
		h.pool.Release(session)
		h.logger.Warn("sipbot: the session-start loan's registration failed", "peer", peer, "aor", session.AddressOfRecord, "err", err)
		return
	}
	h.setAccount(peer, a)
	h.storage.Store(peer, session)
	h.accountMu.Unlock()
	h.logger.Info("sipbot: loaned a pooled account at the session's start", "peer", peer, "aor", session.AddressOfRecord)
	// The announcement goes after the lock: WriterFor round-trips the hub,
	// which the session-end hook — an accountMu taker — may be occupying.
	h.say(peer, h.writers.WriterFor(peer), fmt.Sprintf("Registered as sip:%s (an account from the bot's pool).", a.session.AddressOfRecord))
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
// goroutine, as everywhere); the accountMu section can wait behind an
// in-flight registration for at most registerTimeout.
func (h *sipHandler) HandlePeerSessionEnd(_ context.Context, peer ss.SubscriberId) {
	if v, ok := h.calls.Load(peer); ok {
		c := v.(*peerCall)
		if e, ok := c.end(); ok {
			h.calls.CompareAndDelete(peer, c)
			h.hangupDialog(e.dialog)
			h.logger.Info("sipbot: the session's end ended the call", "peer", peer, "callId", c.callId)
		}
	}
	h.accountMu.Lock()
	defer h.accountMu.Unlock()
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

// handleInvite declines an incoming call, voice and video alike: a
// browser-originated call to the bot has no SIP routing target. (The
// reverse direction — a SIP subscriber calling the user's registered
// AOR — is inbound.go's, and rings the browser.)
func (h *sipHandler) handleInvite(sip *msg_handler.SipMessage, w msg_handler.ResponseWriter) {
	h.logger.Info("sipbot: declining an incoming call", "peer", sip.From, "media", sip.Media, "callId", sip.CallId)
	if err := w.Reject(msg_handler.SipCodeDecline, msg_handler.SipPhraseDecline); err != nil {
		h.logger.Warn("sipbot: call decline not sent", "peer", sip.From, "err", err)
	}
}

// handleResponse folds the browser's answer to the bot's INVITE: on 200
// OK the call activates (its track went on the wire at ring time — the
// defensive re-attach is a no-op); on a decline the call is dropped —
// outbound the already-answered callee gets the BYE, inbound the caller
// the mirrored 603.
func (h *sipHandler) handleResponse(sip *msg_handler.SipMessage, w msg_handler.ResponseWriter) {
	peer := sip.From
	v, ok := h.calls.Load(peer)
	if !ok {
		return // an answer to a call the bot never opened (or already ended)
	}
	c := v.(*peerCall)
	if c.callIdSnapshot() != sip.CallId {
		return
	}
	if sip.Response.Code == msg_handler.SipCodeOK {
		if !c.activate() {
			return // a duplicate answer, or the call already ended
		}
		if err := c.attachPinned(); err != nil && !errors.Is(err, errCallEnded) {
			// The call stands answered but carries no media toward the
			// browser; the peer can still hang up, which resets the state
			// (the music bot's failure shape).
			h.logger.Warn("sipbot: attach on accepted call failed", "peer", peer, "err", err)
		}
		h.logger.Info("sipbot: the browser answered", "peer", peer, "callId", c.callId)
		if c.inbound {
			// The SIP caller gets his 200 OK now: AnswerOptions builds the
			// media session and awaits the ACK, so it runs on its own
			// goroutine.
			go h.answerInbound(peer, c)
		}
		return
	}
	e, ok := c.end()
	if !ok {
		return
	}
	h.calls.CompareAndDelete(peer, c)
	if c.inbound {
		// The user's own decline, mirrored to the caller in the
		// webrtc-leg's vocabulary (a generic termination would be a 480).
		h.declineInbound(c.serverDialog)
	} else {
		h.hangupDialog(e.dialog)
	}
	if e.attached {
		h.detachLater(context.Background(), e.track, c.w)
	}
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
	if c.callIdSnapshot() != sip.CallId {
		return
	}
	e, ok := c.end()
	if !ok {
		return
	}
	h.calls.CompareAndDelete(peer, c)
	h.hangupDialog(e.dialog)
	if e.attached {
		h.detachLater(ctx, e.track, c.w)
	}
	h.logger.Info("sipbot: call ended by the user", "peer", peer, "callId", c.callId)
}

// register is the /register command: validate the credential, end any
// call (the identity is changing), and REGISTER the AOR — synchronously,
// so the reply carries the registrar's actual answer; the registration
// then stands until the account ends. A previous registration is
// replaced.
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
	h.accountMu.Lock()
	a, err := newAccount(ctx, h.logger, h.stack, session, h.inboundServe(peer))
	if err != nil {
		h.accountMu.Unlock()
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
	h.accountMu.Unlock()
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
	h.accountMu.Lock()
	if v, ok := h.accounts.Load(peer); ok {
		a := v.(*account)
		a.stop()
		h.accounts.CompareAndDelete(peer, a)
	}
	session, ok := h.storage.Delete(peer)
	if ok && session.Pooled {
		// The loan returns to the pool with its store entry.
		h.pool.Release(session)
	}
	h.accountMu.Unlock()
	if !ok {
		h.say(peer, w, notRegistered)
		return
	}
	h.say(peer, w, "Unregistered.")
}

// myNumber is the /my-number command: the user's currently registered
// SIP address — the number others can dial to ring this chat (the
// inbound path, inbound.go) — or the not-registered answer. The answer
// reads the LIVE registration (an account in the map — published only
// once its REGISTER succeeded), not the credential store: a stored
// credential whose registration has not run yet (a fresh session before
// its first /call) is not callable, so it reads as not-registered.
func (h *sipHandler) myNumber(msg *msg_handler.ChatMessage, w msg_handler.ResponseWriter) {
	peer := msg.From
	if v, ok := h.accounts.Load(peer); ok {
		a := v.(*account)
		if a.session.Pooled {
			h.say(peer, w, fmt.Sprintf("Your number is sip:%s (an account from the bot's pool).", a.session.AddressOfRecord))
		} else {
			h.say(peer, w, fmt.Sprintf("Your number is sip:%s.", a.session.AddressOfRecord))
		}
		return
	}
	hint := noNumber
	if h.pool != nil {
		hint += noNumberPoolHint
	}
	h.say(peer, w, hint)
}

// call is the /call command: with a registration in place and no call
// standing, the SIP dial starts at once on its own goroutine — the
// browser is rung only once the callee answered, with the answer's
// codec selection (docs/sip-bot.md §7.1) — and the legs are joined.
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
	dialCtx, dialCancel := context.WithCancel(ctx)
	c := &peerCall{target: args[0], w: w, pendingCancel: dialCancel}
	// The store, not the early check above, is the one-call gate: an
	// inbound ring (its own goroutine) can slip in between the two, so
	// the check IS the store here — the loser of the race backs out
	// before anything rings.
	if _, loaded := h.calls.LoadOrStore(peer, c); loaded {
		dialCancel()
		h.say(peer, w, "Already in a call — /hangup first.")
		return
	}
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
// the browser leg is told (CANCEL while it rings, BYE once answered —
// nothing when it was never rung), the sip-leg's dialog goes down, the
// attached track withdraws on the hangup delay.
func (h *sipHandler) hangup(ctx context.Context, msg *msg_handler.ChatMessage, w msg_handler.ResponseWriter) {
	peer := msg.From
	v, ok := h.calls.Load(peer)
	if !ok {
		h.say(peer, w, "No call in progress.")
		return
	}
	c := v.(*peerCall)
	e, ok := c.end()
	if !ok {
		return
	}
	h.calls.CompareAndDelete(peer, c)
	h.hangupDialog(e.dialog)
	h.endBrowserLeg(ctx, w, e)
	h.say(peer, w, "Hung up.")
}

// dial rings the callee: one diago INVITE on the account's identity,
// answered when the callee answers. The answer's codec selection pins
// the call (the speaker track is created with it), the relay arms, and
// only then is the browser rung — a busy or unreachable callee never
// disturbs it. Its endings, all exactly-once through the call's end: a
// dial failure (busy, unreachable — or the user hanging up first, which
// cancels the dial's ctx) is the chat line alone (no webrtc-leg verbs
// exist yet); an answered dialog watches for the callee's hangup.
func (h *sipHandler) dial(a *account, peer ss.SubscriberId, c *peerCall, dialCtx context.Context) {
	var rang atomic.Bool
	dialog, err := a.dial(dialCtx, c.target, func(code int) {
		if (code == 180 || code == 183) && rang.CompareAndSwap(false, true) {
			h.logger.Info("sipbot: [dbg] ringing, no sayp")
		}
	})
	if err != nil {
		e, ok := c.end()
		if !ok {
			return // the call ended while the dial was in flight
		}
		h.calls.CompareAndDelete(peer, c)
		h.hangupDialog(e.dialog)
		h.sayp(c, fmt.Sprintf("Call failed: %v.", err))
		return
	}
	// The callee answered: the answer's top audio codec is the call's
	// (diago's negotiated codec — docs/sip-bot.md §8).
	props := diago.MediaProps{}
	if _, err := dialog.AudioReader(diago.WithAudioReaderMediaProps(&props)); err != nil {
		h.failDialedCall(peer, c, dialog, fmt.Errorf("the answered dialog's media never came up: %w", err))
		return
	}
	if err := c.setCodec(props.Codec); err != nil {
		if errors.Is(err, errCallEnded) {
			// The user ended it while the callee was being rung: the end's
			// owner has no dialog to hang up — the just-answered callee is
			// this path's.
			h.hangupDialog(dialog)
			return
		}
		h.failDialedCall(peer, c, dialog, err)
		return
	}
	if err := c.setDialog(dialog); err != nil {
		if errors.Is(err, errCallEnded) {
			h.hangupDialog(dialog) // see above
			return
		}
		h.failDialedCall(peer, c, dialog, err)
		return
	}
	// The callee's hangup (the dialog's ctx) ends the whole call: the
	// browser leg is told, the state reset.
	go func() {
		<-dialog.Context().Done()
		e, ok := c.end()
		if !ok {
			return // ended from this side already
		}
		h.calls.CompareAndDelete(peer, c)
		h.hangupDialog(e.dialog)
		h.endBrowserLeg(context.Background(), c.w, e)
		h.sayp(c, "Callee hung up.")
	}()
	// Ring the browser with the selection: the mic registration first
	// (so no early track is missed — the music bot's discipline), then
	// the pinned attach (the offer carries exactly the selected codec),
	// then the INVITE. Each step re-checks the call lives: an end
	// landing mid-ring is its owner's teardown — the ring's sender
	// Cancels the just-sent ring.
	c.w.OnTrack(func(remote *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		h.onMic(peer, remote, receiver)
	})
	if err := c.attachPinned(); err != nil {
		if errors.Is(err, errCallEnded) {
			return
		}
		h.failDialedCall(peer, c, nil, fmt.Errorf("could not call you: %w", err))
		return
	}
	callId, err := c.w.Invite(msg_handler.MediaVoice)
	if err != nil {
		h.failDialedCall(peer, c, nil, fmt.Errorf("could not call you: %w", err))
		return
	}
	if !c.setCallId(callId) {
		// An end landed while the INVITE was being sent — its owner saw
		// no callId and sent nothing; cancel the just-sent ring.
		if err := c.w.Cancel(callId); err != nil {
			h.logger.Warn("sipbot: CANCEL not sent", "peer", peer, "err", err)
		}
		return
	}
	h.sayp(c, "Callee answered.")
}

// failDialedCall ends an outbound call whose callee already answered —
// the codec would not map, the relay would not arm, or the browser
// could not be rung: the answered callee is hung up, the attached
// track withdrawn, the user told why. A just-answered dialog the
// failure path has not hung up yet is passed as hangup.
func (h *sipHandler) failDialedCall(peer ss.SubscriberId, c *peerCall, hangup sipLeg, err error) {
	h.hangupDialog(hangup)
	e, ok := c.end()
	if !ok {
		return // the call ended meanwhile — its owner tears down
	}
	h.calls.CompareAndDelete(peer, c)
	h.hangupDialog(e.dialog)
	if e.attached {
		h.detachLater(context.Background(), e.track, c.w)
	}
	h.sayp(c, fmt.Sprintf("Call failed: %v.", err))
}

// endCall runs the call's full teardown from the peer's channel
// goroutine (/unregister, a replacing /register): the sip-leg's dialog
// is hung up, the browser leg told, the track withdrawn on the hangup
// delay, and the user told.
func (h *sipHandler) endCall(ctx context.Context, peer ss.SubscriberId, c *peerCall) {
	e, ok := c.end()
	if !ok {
		return
	}
	h.calls.CompareAndDelete(peer, c)
	h.hangupDialog(e.dialog)
	h.endBrowserLeg(ctx, c.w, e)
}

// endBrowserLeg terminates the call's webrtc-leg after its end: CANCEL
// while the browser still rings, BYE once it answered — and nothing
// when the browser was never rung (an outbound dial that failed before
// the ring) — the attached track's withdrawal then waiting out the
// peer's own teardown offer.
func (h *sipHandler) endBrowserLeg(ctx context.Context, w msg_handler.ResponseWriter, e callEnd) {
	if e.callId != "" {
		var err error
		verb := "BYE"
		if e.phase == phaseRinging {
			verb = "CANCEL"
			err = w.Cancel(e.callId)
		} else {
			err = w.Bye(e.callId)
		}
		if err != nil {
			h.logger.Warn("sipbot: "+verb+" not sent", "err", err)
		}
	}
	if e.attached {
		h.detachLater(ctx, e.track, w)
	}
}

// hangupDialog sends the dialog's termination without blocking the
// caller — diago's Hangup speaks the dialog's own state on either leg
// (the BYE of an established dialog, the 480 of a still-ringing inbound
// one). A nil dialog (the outbound dial never answered) needs none — its
// transaction died with the dial's ctx.
func (h *sipHandler) hangupDialog(dialog sipLeg) {
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
func (h *sipHandler) detachLater(ctx context.Context, track *webrtc.TrackLocalStaticSample, w msg_handler.ResponseWriter) {
	go func() {
		t := time.NewTimer(hangupDetachDelay)
		defer t.Stop()
		select {
		case <-t.C:
			if err := w.DetachMedia(track); err != nil {
				h.logger.Warn("sipbot: track not detached on hangup", "err", err)
			}
		case <-ctx.Done():
		}
	}()
}

// ensureAccount answers the peer's live registration: the account in
// the map; else a revival of the stored credential; else — for a user
// who never /registered and whose session-start loan did not land — a
// loan from the bot's credential pool, registered and stored exactly
// like a /register's outcome (the pooled flag tells the caller to
// announce it). The revive's and the loan's first REGISTER are
// synchronous and bounded, like /register's. The errors are the
// user-facing answers, each pointing at /register: no credential at all
// (no pool configured), an exhausted pool, or a failed registration — a
// pooled account that fails to register returns to the pool.
func (h *sipHandler) ensureAccount(ctx context.Context, peer ss.SubscriberId) (*account, bool, error) {
	h.accountMu.Lock()
	defer h.accountMu.Unlock()
	if v, ok := h.accounts.Load(peer); ok {
		return v.(*account), false, nil
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
	a, err := newAccount(ctx, h.logger, h.stack, session, h.inboundServe(peer))
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
// answered dialog exist) — but only when it speaks the call's codec:
// the mic's m-line is answered with the codec pin, so anything else is
// the failed presumption (docs/sip-bot.md §11.8) — drained, not
// bridged, the sip-leg hearing silence while the log says why. The
// receiver comes along for the negotiated telephone-event PT — the
// pad's DTMF rides the mic's own read stream (dtmf.go).
func (h *sipHandler) onMic(peer ss.SubscriberId, track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
	h.logger.Info("sipbot: inbound track", "peer", peer, "codec", track.Codec().MimeType)
	v, ok := h.calls.Load(peer)
	if !ok || !v.(*peerCall).micCodecOK(track.Codec().MimeType) {
		if ok {
			h.logger.Warn("sipbot: the mic is not the call's codec; draining it", "peer", peer, "codec", track.Codec().MimeType)
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
	v.(*peerCall).setMic(track, receiver)
}

// say answers the peer with a chat message, logging a failed send.
func (h *sipHandler) say(peer ss.SubscriberId, w msg_handler.ResponseWriter, text string) {
	if _, err := w.Reply(text); err != nil {
		h.logger.Warn("sipbot: reply not sent", "peer", peer, "err", err)
	}
}

// sayp answers on the call's captured writer — the SIP leg's async news
// (progress, answer, hangup, failure): threaded on the /call command for
// an outbound call, unthreaded (the WriterFor writer) for an inbound one.
func (h *sipHandler) sayp(c *peerCall, text string) {
	if _, err := c.w.Reply(text); err != nil {
		h.logger.Warn("sipbot: reply not sent", "err", err)
	}
}
