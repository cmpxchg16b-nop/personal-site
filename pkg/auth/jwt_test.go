package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	pkgutils "personal-site/pkg/utils"

	"github.com/golang-jwt/jwt/v5"
)

// fakeValidator is a controllable JWTValidator used to exercise the middleware
// without depending on signing/verification machinery. Only ParseToken is used
// by the middleware, but the interface requires both methods.
type fakeValidator struct {
	claims       *jwt.RegisteredClaims
	customClaims any
	err          error
	parseCalls   int
	lastToken    string
}

func (f *fakeValidator) ValidateToken(context.Context, string) (bool, string, error) {
	return true, "", nil
}

func (f *fakeValidator) ParseToken(_ context.Context, token string) (*jwt.RegisteredClaims, any, error) {
	f.parseCalls++
	f.lastToken = token
	return f.claims, f.customClaims, f.err
}

// okHandler records whether it was invoked and replies 200 "ok".
func okHandler(called *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*called = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

func bearerRequest(method, path, token string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

// decodeErr unmarshals the recorder body as an ErrorResponse.
func decodeErr(t *testing.T, rec *httptest.ResponseRecorder) pkgutils.ErrorResponse {
	t.Helper()
	var er pkgutils.ErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&er); err != nil {
		t.Fatalf("decoding error response: %v", err)
	}
	return er
}

func TestWhiteListAuth_PanicsOnNilValidator(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for nil jwtValidator")
		}
	}()
	_ = WithWhiteListJWTAuth(okHandler(new(bool)), nil, []string{"/login"}, nil)
}

func TestWhiteListAuth_WhitelistedPathSkipsValidation(t *testing.T) {
	v := &fakeValidator{err: context.DeadlineExceeded} // would reject if called
	called := false
	h := WithWhiteListJWTAuth(okHandler(&called), v, []string{"/login"}, nil)

	rec := httptest.NewRecorder()
	// No token at all; whitelisted paths must not require one.
	h.ServeHTTP(rec, bearerRequest(http.MethodGet, "/login", ""))

	if !called {
		t.Fatal("nextHandler not invoked for whitelisted path")
	}
	if v.parseCalls != 0 {
		t.Fatalf("ParseToken must not be called for whitelisted path; got %d calls", v.parseCalls)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestWhiteListAuth_NonWhitelistedRequiresValidToken(t *testing.T) {
	v := &fakeValidator{
		claims: &jwt.RegisteredClaims{ID: "sess-1", Subject: "user-1"},
	}
	called := false
	h := WithWhiteListJWTAuth(okHandler(&called), v, []string{"/login"}, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, bearerRequest(http.MethodGet, "/api/data", "good-token"))

	if !called {
		t.Fatal("nextHandler not invoked for valid token on protected path")
	}
	if v.parseCalls != 1 {
		t.Fatalf("ParseToken calls: got %d, want 1", v.parseCalls)
	}
	if v.lastToken != "good-token" {
		t.Fatalf("parsed token: got %q, want %q", v.lastToken, "good-token")
	}
}

func TestWhiteListAuth_InvalidTokenRejectedWith401(t *testing.T) {
	v := &fakeValidator{err: context.DeadlineExceeded}
	called := false
	h := WithWhiteListJWTAuth(okHandler(&called), v, []string{"/login"}, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, bearerRequest(http.MethodGet, "/api/data", "bad-token"))

	if called {
		t.Fatal("nextHandler must not be invoked when token is invalid")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	er := decodeErr(t, rec)
	if er.Error == "" {
		t.Fatal("expected non-empty Unauthorized error message")
	}
}

func TestWhiteListAuth_NilClaimsRejected(t *testing.T) {
	v := &fakeValidator{claims: nil} // ParseToken returns (nil, nil, nil)
	called := false
	h := WithWhiteListJWTAuth(okHandler(&called), v, []string{"/login"}, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, bearerRequest(http.MethodGet, "/api/data", "some-token"))

	if called {
		t.Fatal("nextHandler must not be invoked for nil claims")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestWhiteListAuth_RejectDelegatesToOnRejectHandler(t *testing.T) {
	v := &fakeValidator{err: context.DeadlineExceeded}
	called := false
	rejectCalled := false
	rejectHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rejectCalled = true
		w.WriteHeader(http.StatusTeapot) // distinctive code to prove handoff
	})
	h := WithWhiteListJWTAuth(okHandler(&called), v, []string{"/login"}, rejectHandler)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, bearerRequest(http.MethodGet, "/api/data", "bad-token"))

	if called {
		t.Fatal("nextHandler must not be invoked on rejection")
	}
	if !rejectCalled {
		t.Fatal("onRejectHandler must be invoked when provided")
	}
	if rec.Code != http.StatusTeapot {
		t.Fatalf("status: got %d, want %d (from onRejectHandler)", rec.Code, http.StatusTeapot)
	}
}

func TestWhiteListAuth_ContextIsEnriched(t *testing.T) {
	v := &fakeValidator{
		claims:       &jwt.RegisteredClaims{ID: "sess-1", Subject: "subj-1"},
		customClaims: &CustomClaimType{Username: "alice", Email: "alice@example.com"},
	}
	var gotUsername, gotEmail, gotSession, gotSubject any
	capture := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		gotUsername = ctx.Value(pkgutils.CtxKeyUsername)
		gotEmail = ctx.Value(pkgutils.CtxKeyEmail)
		gotSession = ctx.Value(pkgutils.CtxKeySessionId)
		gotSubject = ctx.Value(pkgutils.CtxKeySubjectId)
		w.WriteHeader(http.StatusOK)
	})
	h := WithWhiteListJWTAuth(capture, v, nil, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, bearerRequest(http.MethodGet, "/anything", "tok"))

	if gotUsername != "alice" {
		t.Fatalf("username in context: got %v, want %q", gotUsername, "alice")
	}
	if gotEmail != "alice@example.com" {
		t.Fatalf("email in context: got %v, want %q", gotEmail, "alice@example.com")
	}
	if gotSession != "sess-1" {
		t.Fatalf("session id in context: got %v, want %q", gotSession, "sess-1")
	}
	if gotSubject != "subj-1" {
		t.Fatalf("subject id in context: got %v, want %q", gotSubject, "subj-1")
	}
}

func TestWhiteListAuth_SubtreePatternMatchesNestedPaths(t *testing.T) {
	v := &fakeValidator{err: context.DeadlineExceeded} // would reject if validated
	called := false
	h := WithWhiteListJWTAuth(okHandler(&called), v, []string{"/public/"}, nil)

	// A nested path under the subtree must be whitelisted (no token).
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, bearerRequest(http.MethodGet, "/public/assets/app.js", ""))
	if !called {
		t.Fatal("nested path under subtree pattern should be whitelisted")
	}
	if v.parseCalls != 0 {
		t.Fatalf("ParseToken must not be called under whitelisted subtree; got %d", v.parseCalls)
	}

	// A sibling path NOT under the subtree must be validated and rejected.
	called = false
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, bearerRequest(http.MethodGet, "/private", ""))
	if called {
		t.Fatal("path outside subtree must be validated, not passed through")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status for non-whitelisted path: got %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestWhiteListAuth_ExactPathDoesNotMatchChildren(t *testing.T) {
	v := &fakeValidator{err: context.DeadlineExceeded}
	called := false
	h := WithWhiteListJWTAuth(okHandler(&called), v, []string{"/ping"}, nil)

	// Exact match is whitelisted.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, bearerRequest(http.MethodGet, "/ping", ""))
	if !called || rec.Code != http.StatusOK {
		t.Fatalf("exact whitelisted path: called=%v status=%d", called, rec.Code)
	}

	// A child path is NOT whitelisted and must be validated.
	called = false
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, bearerRequest(http.MethodGet, "/ping/extra", ""))
	if called {
		t.Fatal("child of exact path pattern must not be whitelisted")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status for child of exact path: got %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestWhiteListAuth_MethodScopedPatternOnlyMatchesThatMethod(t *testing.T) {
	v := &fakeValidator{err: context.DeadlineExceeded}
	called := false
	h := WithWhiteListJWTAuth(okHandler(&called), v, []string{"GET /ping"}, nil)

	// GET matches the whitelist.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, bearerRequest(http.MethodGet, "/ping", ""))
	if !called {
		t.Fatal("GET on method-scoped whitelist entry should be allowed")
	}

	// POST does not match and must be validated (and rejected).
	called = false
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, bearerRequest(http.MethodPost, "/ping", ""))
	if called {
		t.Fatal("POST on a GET-only whitelist entry must be validated, not passed through")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status for non-matching method: got %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestWhiteListAuth_EmptyWhitelistValidatesEverything(t *testing.T) {
	v := &fakeValidator{err: context.DeadlineExceeded}
	called := false
	h := WithWhiteListJWTAuth(okHandler(&called), v, nil, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, bearerRequest(http.MethodGet, "/anything", "bad"))
	if called {
		t.Fatal("with empty whitelist every path must be validated")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}
