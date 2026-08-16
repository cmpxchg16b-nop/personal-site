package session

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	pkgutils "personal-site/pkg/utils"
)


type Session struct {
	id           string
	subjectId    string
	username     string
	email        string
	expiresAtSec    int64
}

func (s *Session) Id() string { return s.id }

func (s *Session) SubjectId() string { return s.subjectId }

func (s *Session) Username() string { return s.username }

func (s *Session) Email() string { return s.email }

func (s *Session) Expiry() time.Time { return time.Unix(s.expiresAtSec, 0) }

type SessionManager interface {
	// GetSessionFromContext returns the Session attached to ctx, if any.
	GetSessionFromContext(ctx context.Context) (*Session, bool)

	// WithSession returns ctx with sess attached so downstream handlers can
	// retrieve it via GetSessionFromContext.
	WithSession(ctx context.Context, sess *Session) context.Context
}

// A completely stateless session manager that out-sourced everything to context
type OnMemorySessionManager struct { }

func NewOnMemorySessionManager() *OnMemorySessionManager {
	return &OnMemorySessionManager{}
}

func (m *OnMemorySessionManager) GetSessionFromContext(ctx context.Context) (*Session, bool) {
	sess, ok := ctx.Value(pkgutils.CtxKeySessionObject).(*Session)
	return sess, ok
}

// WithSession returns ctx with sess attached.
func (m *OnMemorySessionManager) WithSession(ctx context.Context, sess *Session) context.Context {
	return context.WithValue(ctx, pkgutils.CtxKeySessionObject, sess)
}

// StaticVisitorSessionManager is a SessionManager that answers every
// GetSessionFromContext with the same hard-coded visitor session, ignoring
// the context. It exists because the site has no login or account system
// yet: handlers that need a session (e.g. pkg/api/profile's ProfileHandler)
// are supplied this placeholder identity instead.
type StaticVisitorSessionManager struct {
	sess *Session
}

func NewStaticVisitorSessionManager() *StaticVisitorSessionManager {
	return &StaticVisitorSessionManager{sess: &Session{
		id:        "visitor",
		subjectId: "visitor",
		username:  "Visitor",
	}}
}

// GetSessionFromContext returns the static visitor session, unconditionally.
func (m *StaticVisitorSessionManager) GetSessionFromContext(_ context.Context) (*Session, bool) {
	return m.sess, true
}

// WithSession returns ctx unchanged: the static visitor identity is
// unconditional, so there is nothing to attach.
func (m *StaticVisitorSessionManager) WithSession(ctx context.Context, _ *Session) context.Context {
	return ctx
}

// This middleware is expected the be chained before jwt middleware, in another word,
// it is expected that the request is flowed through the jwt middleware before it hits this.
func WithSessionId(h http.Handler, sm *OnMemorySessionManager) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, err := sessionFromContext(r.Context())
		if err != nil {
			// A mistyped context value means the upstream middleware chain is
			// misconfigured. Fail closed with a 500 rather than run downstream
			// handlers with a partially populated session (e.g. an empty
			// subject id scoping operations to a non-existent user).
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(pkgutils.ErrorResponse{Error: err.Error()})
			return
		}

		h.ServeHTTP(w, r.WithContext(sm.WithSession(r.Context(), sess)))
	})
}

// sessionFromContext builds a Session from the values the JWT middleware left
// in ctx. Absent values leave the corresponding field zero. A present value of
// an unexpected type indicates a misconfigured upstream chain and is returned
// as an error instead of panicking.
func sessionFromContext(ctx context.Context) (*Session, error) {
	var sess Session
	var err error

	if sess.id, err = typedContextValue[string](ctx, pkgutils.CtxKeySessionId); err != nil {
		return nil, err
	}
	if sess.subjectId, err = typedContextValue[string](ctx, pkgutils.CtxKeySubjectId); err != nil {
		return nil, err
	}
	if sess.username, err = typedContextValue[string](ctx, pkgutils.CtxKeyUsername); err != nil {
		return nil, err
	}
	if sess.email, err = typedContextValue[string](ctx, pkgutils.CtxKeyEmail); err != nil {
		return nil, err
	}
	if sess.expiresAtSec, err = typedContextValue[int64](ctx, pkgutils.CtxKeySessionTTLSecs); err != nil {
		return nil, err
	}
	return &sess, nil
}

// typedContextValue reads the value stored under key, expecting type T. An
// absent key yields the zero value. A present value of another type is an
// error naming the offending key.
func typedContextValue[T any](ctx context.Context, key pkgutils.CtxKey) (T, error) {
	var zero T
	v := ctx.Value(key)
	if v == nil {
		return zero, nil
	}
	typed, ok := v.(T)
	if !ok {
		return zero, fmt.Errorf("session: context value %q has unexpected type %T", key, v)
	}
	return typed, nil
}
