package sipbot

// This file is one chat user's SIP account: the runtime of the identity
// a /register stored — the REGISTER that puts the AOR's registration up,
// and the dial path that places the user's outbound calls. The account
// is runtime, not data: it lives in the handler's account map, next to
// (never inside) the UserSessionStorage, which holds only the
// credential.
//
// The account's lifetime is its context: /unregister, a replacing
// /register, or the peer session's end cancels it — the de-REGISTER goes
// out on the way down (SIP's own unregister: Contact "*", Expires 0).
// There is no expiry and no refresh timer: the registration is an event,
// not a lease — the bot's own lifecycle says when to register (the
// account's birth) and when to de-register (its end), which a dumb
// re-REGISTER clock cannot know. (A registrar that expires bindings on
// its own clock would lose the number — the deployment's registrar keeps
// them.)

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/emiago/diago"
	"github.com/emiago/sipgo/sip"
)

// registerTimeout bounds /register's first REGISTER round trip — the
// handler answers from its outcome, so the wait must not stall the
// peer's channel goroutine longer than this.
const registerTimeout = 5 * time.Second

// registerBindRetries / registerBindRetryBackoff bound the first
// REGISTER's tolerance of the diago startup race (see newAccount): the
// listener whose pool entry the first transaction needs appears a few
// instructions after diago reports readiness, so a spurious bind
// conflict gets a handful of cheap retries inside registerTimeout.
const registerBindRetries = 5
const registerBindRetryBackoff = 20 * time.Millisecond

// account is one chat user's SIP runtime: its own SIP client (the
// registration and the dial-out identity run on a diago of the account's
// own — never a client shared with other users, each user brings their
// own credential). An account in the handler's map is registered by
// construction: newAccount publishes only after the first REGISTER
// succeeded.
type account struct {
	logger  *slog.Logger
	dg      *diago.Diago
	session UserSession

	ctx    context.Context
	cancel context.CancelFunc
	// stackStop tears down the account's SIP client. It must outlive the
	// registration by the de-REGISTER's round trip: the teardown watcher
	// calls it once that left, never before.
	stackStop context.CancelFunc
}

// newAccount opens the session's own SIP client and registers its AOR
// against its registrar: the REGISTER runs synchronously (bounded by
// registerTimeout) so its outcome answers the /register command; the
// registration then stands untouched for the account's whole life — no
// expiry, no refresh loop — until the account's ctx ends, which sends
// the de-REGISTER. onInbound is handed the account's inbound SIP calls
// (someone dialing the registered AOR) — see inbound.go.
func newAccount(ctx context.Context, logger *slog.Logger, stack sipStack, session UserSession, onInbound func(inDialog *diago.DialogServerSession)) (*account, error) {
	a := &account{logger: logger, session: session}
	a.ctx, a.cancel = context.WithCancel(ctx)
	// The client's ctx is NOT the account's: the socket must outlive the
	// registration by the de-REGISTER's round trip.
	stackCtx, stackStop := context.WithCancel(context.Background())
	a.stackStop = stackStop
	dg, err := stack.open(stackCtx, session, onInbound)
	if err != nil {
		a.cancel()
		stackStop()
		return nil, err
	}
	a.dg = dg
	recipient := sip.Uri{User: session.Username, Host: session.Host, Port: session.Port}
	regCtx, stop := context.WithTimeout(a.ctx, registerTimeout)
	defer stop()

	// The first REGISTER races diago's own startup: ServeBackground
	// reports the transport ready — and re-pins the client's connection
	// address to the listener's just-bound port — a few instructions
	// BEFORE sipgo pools the listener connection, so the first
	// transaction's pool lookup can still miss and its connection create
	// can try to bind the account's own port (EADDRINUSE). The listener
	// is pooled almost immediately, so the spurious conflict gets a few
	// retries; any other failure is the registrar's real answer.
	var t *diago.RegisterTransaction
	for attempt := 0; ; attempt++ {
		t, err = dg.RegisterTransaction(a.ctx, recipient, diago.RegisterOptions{
			Username: session.Username,
			Password: session.Password,
		})
		if err == nil {
			// diago builds the REGISTER bare — From/To/Via are added at send
			// time, and the From sipgo would add is the socket's own address,
			// not the AOR the credential belongs to. Stamp the user's identity:
			// the From IS the AOR (with the tag sipgo adds only to Froms of its
			// own making), pre-set so the send-time build leaves it alone. The
			// Contact's user part is the account UA's name already, so the
			// registrar sees one consistent identity: AOR, Contact user, and
			// digest username are all the user's.
			from := &sip.FromHeader{
				Address: sip.Uri{User: session.Username, Host: session.Host},
				Params:  sip.NewParams(),
			}
			from.Params.Add("tag", sip.GenerateTagN(16))
			t.Origin.AppendHeader(from)
			err = t.Register(regCtx)
		}
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EADDRINUSE) || attempt+1 >= registerBindRetries {
			a.cancel()
			stackStop()
			return nil, fmt.Errorf("REGISTER rejected: %w", err)
		}
		a.logger.Warn("sipbot: the first REGISTER hit the SIP client's startup race; retrying", "aor", session.AddressOfRecord, "err", err)
		select {
		case <-regCtx.Done():
			a.cancel()
			stackStop()
			return nil, fmt.Errorf("REGISTER rejected: %w", err)
		case <-time.After(registerBindRetryBackoff):
		}
	}
	go a.holdRegistration(t)
	return a, nil
}

// holdRegistration keeps the account's registration for the account's
// whole life. There is nothing to do while the account lives — no
// refresh clock: the lifecycle hooks, not a timer, say when the
// registration ends — so the goroutine's one job is the way out: once
// the account's ctx ends, the de-REGISTER goes out, and only then does
// the SIP client go (the socket must outlive the de-REGISTER's round
// trip).
func (a *account) holdRegistration(t *diago.RegisterTransaction) {
	<-a.ctx.Done()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := t.Unregister(ctx); err != nil && !errors.Is(err, context.Canceled) {
		a.logger.Warn("sipbot: the de-REGISTER failed", "aor", a.session.AddressOfRecord, "err", err)
	}
	// The de-REGISTER left — the account's SIP client can go now.
	a.stackStop()
}

// dial places the user's outbound call to target (a SIP URI without the
// scheme — "1001@sip.example.com", or a bare user completed with the
// account's own domain): one diago INVITE transaction carrying the
// account's From and digest credential, answered when the callee
// answers. onProgress reports the provisional responses (180 Ringing).
// The call runs on the caller's goroutine — the dial blocks until the
// final response, the ctx, or an error.
func (a *account) dial(ctx context.Context, target string, onProgress func(code int)) (*diago.DialogClientSession, error) {
	uri := sip.Uri{}
	to := target
	if !hasHost(to) {
		to += "@" + a.session.hostPort()
	}
	if err := sip.ParseUri(withSipScheme(to), &uri); err != nil {
		return nil, fmt.Errorf("bad callee address %q: %w", to, err)
	}
	from := &sip.FromHeader{
		DisplayName: a.session.Username,
		Address:     sip.Uri{User: a.session.Username, Host: a.session.Host, Port: a.session.Port},
		Params:      sip.NewParams(),
	}
	return a.dg.Invite(ctx, uri, diago.InviteOptions{
		Username: a.session.Username,
		Password: a.session.Password,
		Headers:  []sip.Header{from},
		OnResponse: func(res *sip.Response) error {
			if res.StatusCode < 200 && onProgress != nil {
				onProgress(res.StatusCode)
			}
			return nil
		},
	})
}

// hasHost reports whether target carries an @host part.
func hasHost(target string) bool {
	for i := len(target) - 1; i >= 0; i-- {
		if target[i] == '@' {
			return true
		}
	}
	return false
}

// withSipScheme prepends the sip: scheme unless the address already
// carries one.
func withSipScheme(addr string) string {
	if len(addr) >= 4 && strings.EqualFold(addr[:4], "sip:") {
		return addr
	}
	return "sip:" + addr
}

// stop ends the account: the teardown watcher fires (sending the
// de-REGISTER) and dials in progress unwind.
func (a *account) stop() {
	a.cancel()
}

// parseAddressOfRecord splits an AOR argument ("2001@sip.example.com",
// a scheme tolerated) into its session form. The host must be present —
// a registrar-less identity cannot register anywhere.
func parseAddressOfRecord(arg, password string) (UserSession, error) {
	to := arg
	if strings.IndexFunc(to, unicode.IsSpace) >= 0 {
		return UserSession{}, fmt.Errorf("the address %q carries whitespace", arg)
	}
	if !hasHost(to) {
		return UserSession{}, fmt.Errorf("the address %q has no @host — give user@host", arg)
	}
	uri := sip.Uri{}
	if err := sip.ParseUri(withSipScheme(to), &uri); err != nil {
		if strings.Contains(to, "::") {
			return UserSession{}, fmt.Errorf("bad address %q: %w (an IPv6 literal needs brackets, e.g. 1005@[2a0a:4cc0::1])", arg, err)
		}
		return UserSession{}, fmt.Errorf("bad address %q: %w", arg, err)
	}
	if uri.User == "" {
		return UserSession{}, fmt.Errorf("the address %q has no user part", arg)
	}
	session := UserSession{
		Username: uri.User,
		Host:     uri.Host,
		Port:     uri.Port,
		Password: password,
	}
	session.AddressOfRecord = uri.User + "@" + session.hostPort()
	return session, nil
}

// hostPort is the address's host part in text form, with the port when
// one was given — the form a bare /call user is completed with. An IPv6
// literal keeps its brackets (the form sipgo's parser keeps), so it
// renders back into a valid URI.
func (s UserSession) hostPort() string {
	if s.Port > 0 {
		return fmt.Sprintf("%s:%d", s.Host, s.Port)
	}
	return s.Host
}
