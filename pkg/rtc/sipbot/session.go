package sipbot

// This file is the sip bot's user-session store: the SIP credential a
// /register command captured, kept per chat user. The store is an
// interface the bot's caller injects — the shipped wiring uses the
// on-memory implementation below, but any service-shaped key/value
// storage satisfies it, because the session it holds is deliberately
// plain data: the registration's runtime (its cancel func, its diago
// handles) is the handler's affair, never the store's.

import (
	"sync"

	"personal-site/pkg/models/ss"
)

// UserSession is one chat user's SIP identity as the bot keeps it: the
// credential of the user's /register, or a loan from the bot's
// SIPCredentialPool. Plain data, safe to copy — an
// implementation may serialize sessions freely.
type UserSession struct {
	// AddressOfRecord is the SIP AOR exactly as /register gave it —
	// "2001@sip.example.com" (no sip: scheme).
	AddressOfRecord string
	// Username is the digest-auth username (the AOR's user part).
	Username string
	// Host is the registrar the AOR registers against (the AOR's host
	// part, without a port); Port is its port, zero for the default.
	Host string
	Port int
	// Password is the digest-auth password, kept as plain data because
	// SIP digest authentication needs the password itself.
	Password string
	// Pooled is true when the credential is a loan from the bot's
	// credential pool: the loan returns to the pool when the session
	// leaves the store (/unregister, a replacing /register, the peer
	// session's end).
	Pooled bool
}

// UserSessionStorage is the bot's user-session store: a key/value
// service keyed by the chat subscriber id. Implementations must be safe
// for concurrent use — the bot reaches it from many peers' channel
// goroutines.
type UserSessionStorage interface {
	// Load returns the session stored for user; ok is false when none is.
	Load(user ss.SubscriberId) (session UserSession, ok bool)
	// Store associates user with session, replacing any previous one.
	Store(user ss.SubscriberId, session UserSession)
	// Delete drops user's session; ok is false when none was stored.
	Delete(user ss.SubscriberId) (session UserSession, ok bool)
}

// OnMemoryUserSessionStorage is the shipped UserSessionStorage: a plain
// map under a RWMutex, living and dying with the process — a restart
// drops every registration, and every user /registers again.
type OnMemoryUserSessionStorage struct {
	mu       sync.RWMutex
	sessions map[ss.SubscriberId]UserSession
}

var _ UserSessionStorage = (*OnMemoryUserSessionStorage)(nil)

// NewOnMemoryUserSessionStorage returns an empty on-memory store.
func NewOnMemoryUserSessionStorage() *OnMemoryUserSessionStorage {
	return &OnMemoryUserSessionStorage{sessions: make(map[ss.SubscriberId]UserSession)}
}

// Load implements UserSessionStorage.
func (s *OnMemoryUserSessionStorage) Load(user ss.SubscriberId) (UserSession, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	session, ok := s.sessions[user]
	return session, ok
}

// Store implements UserSessionStorage.
func (s *OnMemoryUserSessionStorage) Store(user ss.SubscriberId, session UserSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[user] = session
}

// Delete implements UserSessionStorage.
func (s *OnMemoryUserSessionStorage) Delete(user ss.SubscriberId) (UserSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[user]
	delete(s.sessions, user)
	return session, ok
}
