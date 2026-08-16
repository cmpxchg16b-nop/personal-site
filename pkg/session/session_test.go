package session_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"personal-site/pkg/session"
	pkgutils "personal-site/pkg/utils"
)

// interface conformance: the on-memory manager must satisfy SessionManager.
var _ session.SessionManager = (*session.OnMemorySessionManager)(nil)

// interface conformance: the static visitor manager must satisfy
// SessionManager.
var _ session.SessionManager = (*session.StaticVisitorSessionManager)(nil)

func TestStaticVisitorSessionManager(t *testing.T) {
	sm := session.NewStaticVisitorSessionManager()

	// Any context — even an empty one — yields the same visitor session.
	sess, ok := sm.GetSessionFromContext(context.Background())
	if !ok {
		t.Fatal("empty context: got no session, want the static visitor session")
	}
	if sess.Id() != "visitor" || sess.SubjectId() != "visitor" || sess.Username() != "Visitor" {
		t.Errorf("session = {id:%q subject:%q username:%q}, want the hard-coded visitor",
			sess.Id(), sess.SubjectId(), sess.Username())
	}

	// WithSession is a no-op: an attached session must not shadow the visitor.
	ctx := sm.WithSession(context.Background(), &session.Session{})
	got, ok := sm.GetSessionFromContext(ctx)
	if !ok || got != sess {
		t.Errorf("after WithSession: got (%p, %v), want the same visitor session (%p, true)", got, ok, sess)
	}
}

// sessionFromDownstream returns a handler that resolves the session from the
// request context and reports what it found through the returned pointers.
func sessionFromDownstream(found **session.Session, ok *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sm := session.NewOnMemorySessionManager()
		*found, *ok = sm.GetSessionFromContext(r.Context())
	})
}

func TestGetSessionFromContextMiss(t *testing.T) {
	sm := session.NewOnMemorySessionManager()

	if sess, ok := sm.GetSessionFromContext(context.Background()); ok || sess != nil {
		t.Errorf("empty context: got (%v, %v), want (nil, false)", sess, ok)
	}

	// A context carrying unrelated values must not be mistaken for a session.
	ctx := context.WithValue(context.Background(), pkgutils.CtxKeySubjectId, "subject-1")
	if sess, ok := sm.GetSessionFromContext(ctx); ok || sess != nil {
		t.Errorf("context with unrelated values: got (%v, %v), want (nil, false)", sess, ok)
	}
}

func TestWithSessionRoundTrip(t *testing.T) {
	sm := session.NewOnMemorySessionManager()
	sess := &session.Session{}

	ctx := sm.WithSession(context.Background(), sess)

	got, ok := sm.GetSessionFromContext(ctx)
	if !ok {
		t.Fatal("attached session was not found in the derived context")
	}
	if got != sess {
		t.Errorf("got session %p, want the exact pointer %p", got, sess)
	}
}

func TestWithSessionDoesNotMutateParentContext(t *testing.T) {
	sm := session.NewOnMemorySessionManager()

	parent := context.Background()
	_ = sm.WithSession(parent, &session.Session{})

	if _, ok := sm.GetSessionFromContext(parent); ok {
		t.Error("parent context gained a session; context derivation should be immutable")
	}
}

func TestWithSessionLaterAttachmentShadowsEarlier(t *testing.T) {
	sm := session.NewOnMemorySessionManager()
	first := &session.Session{}
	second := &session.Session{}

	ctx := sm.WithSession(context.Background(), first)
	ctx = sm.WithSession(ctx, second)

	got, ok := sm.GetSessionFromContext(ctx)
	if !ok || got != second {
		t.Errorf("got (%p, %v), want the later session %p", got, ok, second)
	}
}

func TestWithSessionIdCopiesAllContextValues(t *testing.T) {
	sm := session.NewOnMemorySessionManager()

	var got *session.Session
	var ok bool
	h := session.WithSessionId(sessionFromDownstream(&got, &ok), sm)

	r := httptest.NewRequest(http.MethodGet, "/me", nil)
	ctx := r.Context()
	ctx = context.WithValue(ctx, pkgutils.CtxKeySessionId, "sess-1")
	ctx = context.WithValue(ctx, pkgutils.CtxKeySubjectId, "subject-1")
	ctx = context.WithValue(ctx, pkgutils.CtxKeyUsername, "alice")
	ctx = context.WithValue(ctx, pkgutils.CtxKeyEmail, "alice@example.com")
	ctx = context.WithValue(ctx, pkgutils.CtxKeySessionTTLSecs, int64(1700000000))
	h.ServeHTTP(httptest.NewRecorder(), r.WithContext(ctx))

	if !ok {
		t.Fatal("downstream handler found no session")
	}
	if got.Id() != "sess-1" {
		t.Errorf("Id() = %q, want sess-1", got.Id())
	}
	if got.SubjectId() != "subject-1" {
		t.Errorf("SubjectId() = %q, want subject-1", got.SubjectId())
	}
	if got.Username() != "alice" {
		t.Errorf("Username() = %q, want alice", got.Username())
	}
	if got.Email() != "alice@example.com" {
		t.Errorf("Email() = %q, want alice@example.com", got.Email())
	}
	if want := time.Unix(1700000000, 0); !got.Expiry().Equal(want) {
		t.Errorf("Expiry() = %v, want %v", got.Expiry(), want)
	}
}

func TestWithSessionIdAttachesASessionEvenWithoutContextValues(t *testing.T) {
	sm := session.NewOnMemorySessionManager()

	var got *session.Session
	var ok bool
	h := session.WithSessionId(sessionFromDownstream(&got, &ok), sm)

	// No JWT middleware ran beforehand: no context values at all. The
	// middleware still attaches a (zero-value) session rather than none.
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/anon", nil))

	if !ok {
		t.Fatal("downstream handler found no session; the middleware should attach a zero-value one")
	}
	if got.Id() != "" || got.SubjectId() != "" || got.Username() != "" || got.Email() != "" {
		t.Errorf("session = {id:%q subject:%q username:%q email:%q}, want all empty",
			got.Id(), got.SubjectId(), got.Username(), got.Email())
	}
	if want := time.Unix(0, 0); !got.Expiry().Equal(want) {
		t.Errorf("Expiry() = %v, want the zero Unix time %v", got.Expiry(), want)
	}
}

func TestWithSessionIdCopiesOnlyPresentValues(t *testing.T) {
	sm := session.NewOnMemorySessionManager()

	var got *session.Session
	var ok bool
	h := session.WithSessionId(sessionFromDownstream(&got, &ok), sm)

	r := httptest.NewRequest(http.MethodGet, "/partial", nil)
	ctx := context.WithValue(r.Context(), pkgutils.CtxKeySubjectId, "subject-only")
	h.ServeHTTP(httptest.NewRecorder(), r.WithContext(ctx))

	if !ok {
		t.Fatal("downstream handler found no session")
	}
	if got.SubjectId() != "subject-only" {
		t.Errorf("SubjectId() = %q, want subject-only", got.SubjectId())
	}
	if got.Id() != "" || got.Username() != "" || got.Email() != "" {
		t.Errorf("unseeded fields = {id:%q username:%q email:%q}, want empty",
			got.Id(), got.Username(), got.Email())
	}
	if want := time.Unix(0, 0); !got.Expiry().Equal(want) {
		t.Errorf("Expiry() = %v, want the zero Unix time %v", got.Expiry(), want)
	}
}

// TestWithSessionIdRejectsMistypedContextValues verifies that a context value
// of the wrong type (a misconfigured upstream middleware) produces a 500 in
// the project's standard error shape instead of panicking, and that the
// downstream handler is never called with a partially populated session.
func TestWithSessionIdRejectsMistypedContextValues(t *testing.T) {
	tests := []struct {
		name    string
		key     pkgutils.CtxKey
		value   any
		wantMsg string
	}{
		{name: "session id", key: pkgutils.CtxKeySessionId, value: 42, wantMsg: `"session_id" has unexpected type int`},
		{name: "subject id", key: pkgutils.CtxKeySubjectId, value: 42, wantMsg: `"subject_id" has unexpected type int`},
		{name: "username", key: pkgutils.CtxKeyUsername, value: 42, wantMsg: `"username" has unexpected type int`},
		{name: "email", key: pkgutils.CtxKeyEmail, value: 42, wantMsg: `"email" has unexpected type int`},
		{name: "ttl secs", key: pkgutils.CtxKeySessionTTLSecs, value: "tomorrow", wantMsg: `"session_ttl_secs" has unexpected type string`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sm := session.NewOnMemorySessionManager()

			downstreamCalled := false
			h := session.WithSessionId(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				downstreamCalled = true
			}), sm)

			r := httptest.NewRequest(http.MethodGet, "/bad", nil)
			ctx := context.WithValue(r.Context(), tc.key, tc.value)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, r.WithContext(ctx))

			if downstreamCalled {
				t.Error("downstream handler was called despite the mistyped context value")
			}
			if rr.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want %d (body %q)", rr.Code, http.StatusInternalServerError, rr.Body.String())
			}
			if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
				t.Errorf("Content-Type = %q, want it to contain application/json", ct)
			}
			var body pkgutils.ErrorResponse
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
				t.Fatalf("response body is not valid JSON: %v (body %q)", err, rr.Body.String())
			}
			if !strings.Contains(body.Error, tc.wantMsg) {
				t.Errorf("error = %q, want it to contain %q", body.Error, tc.wantMsg)
			}
		})
	}
}

// TestWithSessionIdForwardsTheRequest verifies the wrapped handler receives
// the request intact (method, path) and its response passes through.
func TestWithSessionIdForwardsTheRequest(t *testing.T) {
	sm := session.NewOnMemorySessionManager()

	h := session.WithSessionId(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/submit" {
			t.Errorf("downstream saw %s %s, want POST /submit", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusTeapot)
	}), sm)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/submit", nil))

	if rr.Code != http.StatusTeapot {
		t.Errorf("status = %d, want %d (response must pass through untouched)", rr.Code, http.StatusTeapot)
	}
}
