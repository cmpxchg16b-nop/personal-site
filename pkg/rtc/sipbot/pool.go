package sipbot

// This file is the bot's pool of SIP accounts (docs/sip-bot.md §5): the
// deployment's own credentials — the <sipCredentialPool/> element of
// serverConfig.xml — loaned to chat users who bring none.
//
// The pool is lazy: a range ("1101-1120") is kept as its descriptor
// (from, to, password, server) and a username is materialized into a
// UserSession only when the allocation cursor passes its index — a
// 10 000-account range costs two integers at startup, not 10 000
// structs.
//
// The pool is stateful (it tracks which accounts are on loan), shared
// (one pool serves every chat user of the bot), and safe for concurrent
// use — without a mutex: the fresh-credential cursor is an
// atomic.Uint64 over the descriptor space (explicit entries first, then
// the ranges in configuration order), so two concurrent allocations
// never meet on one index; and returned loans ride a buffered channel
// whose capacity is the pool's size, so a loan's release never blocks.
//
// Allocation is fresh-first: released loans are re-used only once the
// fresh credentials run out, so an account that just failed a
// registration (its loan was released on the failure) sinks to the back
// of the queue instead of failing the very next user too, and the
// registrar sees the load spread across the configured accounts rather
// than hammering one.
//
// Duplication is the operator's business: the pool neither detects nor
// rejects duplicate entries (an explicit credential inside a range's
// span, two overlapping ranges) — SIP allows concurrent registrations
// of one account at the protocol level, and whoever authors the
// configuration owns the consequence.

import (
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/emiago/sipgo/sip"
)

// SIPCredential is one explicit pool entry — the configuration
// document's <sipCredential sipUri="..." password="..."/>: one SIP
// account, the full URI (the sip: scheme is tolerated, not required)
// and its password.
type SIPCredential struct {
	URI      string
	Password string
}

// SIPCredentialRange is one ranged pool entry — the configuration
// document's <sipCredentialRange usernameRange="1101-1120"
// password="..." sipServer="..."/>: every username in UsernameRange
// ("<integer>-<integer>", both ends inclusive) registering on SIPServer
// ("host" or "host:port") with the shared Password.
type SIPCredentialRange struct {
	UsernameRange string
	Password      string
	SIPServer     string
}

// maxRangeAccounts bounds one range's span: a typo'd endpoint must fail
// the startup, not size the pool's release channel absurdly.
const maxRangeAccounts = 100000

// SIPCredentialPool is the bot's pool of SIP accounts; see this file's
// top comment for the design. The method set is the minimum a pool
// needs: Allocate loans, Release returns. A nil pool is a valid empty
// pool — the bot without a configured pool simply loans nothing.
type SIPCredentialPool struct {
	explicit []UserSession // the parsed explicit entries
	ranges   []sipRange    // the parsed range descriptors — never expanded
	total    int           // the descriptor space's size: len(explicit) + Σ span

	cursor   atomic.Uint64    // the next fresh credential's flat index
	released chan UserSession // returned loans, capacity total
}

// sipRange is a range descriptor: the usernames from..to (both ends
// inclusive) registering on host:port with password.
type sipRange struct {
	from, to int
	password string
	host     string
	port     int
}

// NewSIPCredentialPool builds a pool from the configuration's entries:
// the explicit credentials and the range descriptors, both validated —
// an invalid entry (a malformed sipUri; a usernameRange that is not
// "<integer>-<integer>", is reversed, or spans more than
// maxRangeAccounts; an empty or unparseable sipServer) is an error, a
// wiring-time one (the music bot's audioSource discipline). A pool with
// no entries is valid and empty: Allocate always fails.
func NewSIPCredentialPool(explicit []SIPCredential, ranges []SIPCredentialRange) (*SIPCredentialPool, error) {
	p := &SIPCredentialPool{}
	for _, c := range explicit {
		session, err := parseAddressOfRecord(c.URI, c.Password)
		if err != nil {
			return nil, fmt.Errorf("sipCredential %q: %w", c.URI, err)
		}
		p.explicit = append(p.explicit, session)
	}
	for _, r := range ranges {
		from, to, err := parseUsernameRange(r.UsernameRange)
		if err != nil {
			return nil, err
		}
		if r.SIPServer == "" {
			return nil, fmt.Errorf("sipCredentialRange %q: sipServer is required", r.UsernameRange)
		}
		uri := sip.Uri{}
		if err := sip.ParseUri(withSipScheme(r.SIPServer), &uri); err != nil {
			return nil, fmt.Errorf("sipCredentialRange %q: bad sipServer %q: %w", r.UsernameRange, r.SIPServer, err)
		}
		if uri.User != "" {
			return nil, fmt.Errorf("sipCredentialRange %q: the sipServer %q carries a user part — it is a server, host[:port]", r.UsernameRange, r.SIPServer)
		}
		if uri.Host == "" {
			return nil, fmt.Errorf("sipCredentialRange %q: the sipServer %q has no host", r.UsernameRange, r.SIPServer)
		}
		p.ranges = append(p.ranges, sipRange{from: from, to: to, password: r.Password, host: uri.Host, port: uri.Port})
	}
	p.total = len(p.explicit)
	for _, r := range p.ranges {
		p.total += r.to - r.from + 1
	}
	p.released = make(chan UserSession, p.total)
	return p, nil
}

// Allocate loans a free credential: the fresh ones first, in
// configuration order; the released loans only once the fresh run out.
// ok is false when the pool is empty or exhausted. The loaned value is
// a full UserSession with Pooled set.
func (p *SIPCredentialPool) Allocate() (UserSession, bool) {
	if p == nil {
		return UserSession{}, false
	}
	// The cursor hands out each fresh credential's index exactly once, so
	// concurrent allocations never meet on one credential.
	if i := int(p.cursor.Add(1)) - 1; i < p.total {
		if c, ok := p.materialize(i); ok {
			return c, true
		}
		// Unreachable: i < p.total, and both derive from the same
		// descriptor lists. Fall through to the released loans.
	}
	// Only returned loans remain.
	select {
	case c := <-p.released:
		return c, true
	default:
		return UserSession{}, false
	}
}

// Release returns a loaned credential to the pool. Releasing one that
// is not on loan hands it out a second time — SIP tolerates the
// concurrent registrations, and the bot releases each loan exactly once
// by construction.
func (p *SIPCredentialPool) Release(credential UserSession) {
	if p == nil {
		return
	}
	select {
	case p.released <- credential:
	default:
		// The channel's capacity is the pool's size, so a loan's release
		// always fits; the default branch is a double release's, ignored.
	}
}

// materialize is the laziness: the flat index i (an explicit entry,
// then the ranges in order) becomes its UserSession on first handout.
// ok is false only when i is out of range, which Allocate's cursor
// discipline rules out.
func (p *SIPCredentialPool) materialize(i int) (UserSession, bool) {
	if i < len(p.explicit) {
		c := p.explicit[i]
		c.Pooled = true
		return c, true
	}
	i -= len(p.explicit)
	for _, r := range p.ranges {
		n := r.to - r.from + 1
		if i >= n {
			i -= n
			continue
		}
		c := UserSession{
			Username: strconv.Itoa(r.from + i),
			Host:     r.host,
			Port:     r.port,
			Password: r.password,
			Pooled:   true,
		}
		c.AddressOfRecord = c.Username + "@" + c.hostPort()
		return c, true
	}
	return UserSession{}, false
}

// parseUsernameRange parses a usernameRange attribute: "<integer>-<integer>",
// both ends inclusive, from ≤ to, the span capped at maxRangeAccounts.
// The integer IS the username, so leading zeros are not preserved.
func parseUsernameRange(expr string) (from, to int, err error) {
	lo, hi, ok := strings.Cut(expr, "-")
	if !ok || !isUintStr(lo) || !isUintStr(hi) {
		return 0, 0, fmt.Errorf("usernameRange %q is not an \"integer-integer\" range", expr)
	}
	if from, err = strconv.Atoi(lo); err != nil {
		return 0, 0, fmt.Errorf("usernameRange %q: %w", expr, err)
	}
	if to, err = strconv.Atoi(hi); err != nil {
		return 0, 0, fmt.Errorf("usernameRange %q: %w", expr, err)
	}
	if from > to {
		return 0, 0, fmt.Errorf("usernameRange %q is reversed (%d > %d)", expr, from, to)
	}
	if to-from+1 > maxRangeAccounts {
		return 0, 0, fmt.Errorf("usernameRange %q spans %d accounts; the maximum is %d", expr, to-from+1, maxRangeAccounts)
	}
	return from, to, nil
}

// isUintStr reports whether s is a non-empty string of decimal digits
// (stricter than strconv.Atoi, which would take a sign).
func isUintStr(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
