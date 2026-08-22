package visitor_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	pkgapicommon "personal-site/pkg/api/common"
	"personal-site/pkg/api/login/visitor"
	pkgauth "personal-site/pkg/auth"
	pkgcookie "personal-site/pkg/cookie"
)

// stubTicketGenerator is a test double for pkgauth.TicketGenerator returning
// scripted values without any waiting.
type stubTicketGenerator struct {
	ticket string
	err    error
}

var _ pkgauth.TicketGenerator = (*stubTicketGenerator)(nil)

func (s *stubTicketGenerator) GetTicket(context.Context) (string, error) {
	return s.ticket, s.err
}

const (
	testJWTSecret = "test-jwt-secret"
	testIssuer    = "test-issuer"
	testValidity  = 2 * time.Hour
)

// newTestHandler builds a handler wired like cmd/server/main.go does: a real
// JWT issuer (signed with a static test secret) and the real cookie builder;
// only the ticket generator is faked so tests don't wait on ticks.
func newTestHandler(ticketGen pkgauth.TicketGenerator, issuerName string) *visitor.VisitorLoginHandler {
	keyProvider := pkgauth.NewStaticSecretProvider([]byte(testJWTSecret))
	return visitor.NewVisitorLoginHandler(
		pkgauth.NewStaticKeyJWTIssuer(keyProvider, issuerName),
		testValidity,
		ticketGen,
		nil, // no allowed origins; tests use site-relative redirect targets
		&pkgcookie.SimpleCookieBuilder{},
	)
}

// findCookie returns the named response cookie, or nil when absent.
func findCookie(rr *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rr.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// parseIssuedClaims extracts and validates the session JWT from the response's
// jwt cookie, returning its custom claims.
func parseIssuedClaims(t *testing.T, rr *httptest.ResponseRecorder) *pkgauth.CustomClaimType {
	t.Helper()
	jwtCookie := findCookie(rr, pkgapicommon.DefaultJWTCookieKey)
	if jwtCookie == nil {
		t.Fatal("jwt cookie is not set")
	}
	keyProvider := pkgauth.NewStaticSecretProvider([]byte(testJWTSecret))
	validator := pkgauth.NewStaticKeyJWTValidator(keyProvider, pkgauth.NewNullBlackListProvider(), false)
	_, customClaimsAny, err := validator.ParseToken(context.Background(), jwtCookie.Value)
	if err != nil {
		t.Fatalf("cannot parse issued jwt: %v", err)
	}
	claims, ok := customClaimsAny.(*pkgauth.CustomClaimType)
	if !ok || claims == nil {
		t.Fatalf("issued jwt claims have unexpected type %T", customClaimsAny)
	}
	return claims
}

var visitorUsernamePattern = regexp.MustCompile(`^visitor-\d{5}$`)

// assertVisitorClaims checks the claims every successful visitor login must
// produce, regardless of how the handler was invoked.
func assertVisitorClaims(t *testing.T, claims *pkgauth.CustomClaimType) {
	t.Helper()
	if !strings.HasPrefix(claims.Subject, pkgauth.VisitorSubjectPrefix) {
		t.Errorf("sub = %q, want the %q prefix", claims.Subject, pkgauth.VisitorSubjectPrefix)
	}
	if !visitorUsernamePattern.MatchString(claims.Username) {
		t.Errorf("username = %q, want it to match %q", claims.Username, visitorUsernamePattern)
	}
	if claims.ID == "" {
		t.Error("jti is empty, want a uuid")
	}
	if len(claims.Audience) != 1 || claims.Audience[0] != pkgauth.AudSession {
		t.Errorf("aud = %v, want [%q]", claims.Audience, pkgauth.AudSession)
	}
	if claims.NotBefore == nil || claims.ExpiresAt == nil {
		t.Fatalf("nbf/exp must be set, got nbf=%v exp=%v", claims.NotBefore, claims.ExpiresAt)
	}
	if got, want := claims.ExpiresAt.Sub(claims.NotBefore.Time), testValidity; got != want {
		t.Errorf("exp - nbf = %v, want validity %v", got, want)
	}
}

// TestServeHTTP_Success covers the happy path: a ticket is available, a
// visitor JWT is issued into the session cookie, and the browser is
// redirected to the root page.
func TestServeHTTP_Success(t *testing.T) {
	h := newTestHandler(&stubTicketGenerator{ticket: "tick-1"}, testIssuer)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/login/visitor", nil))

	if rr.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want %d (body %q)", rr.Code, http.StatusTemporaryRedirect, rr.Body.String())
	}
	if got := rr.Header().Get("Location"); got != "/" {
		t.Errorf("Location = %q, want %q", got, "/")
	}
	assertVisitorClaims(t, parseIssuedClaims(t, rr))
}

// TestServeHTTP_RedirectIfSucceed covers the redirect_if_succeed query
// parameter: allowed targets (a site-relative path, or an absolute URL whose
// origin is in AllowedOrigins) are honored directly — there is no IdP round
// trip to survive; anything else is rejected with 403.
func TestServeHTTP_RedirectIfSucceed(t *testing.T) {
	tests := []struct {
		name         string
		target       string
		origins      []string // overrides AllowedOrigins when non-nil
		wantStatus   int
		wantLocation string
	}{
		{
			name:         "site-relative target is honored",
			target:       "/api/login/visitor?redirect_if_succeed=/posts/hello-world",
			wantStatus:   http.StatusTemporaryRedirect,
			wantLocation: "/posts/hello-world",
		},
		{
			name:         "absolute target with an allowed origin is honored",
			target:       "/api/login/visitor?redirect_if_succeed=https%3A%2F%2Fapp.example.com%2Fposts%2Fx",
			origins:      []string{"https://app.example.com"},
			wantStatus:   http.StatusTemporaryRedirect,
			wantLocation: "https://app.example.com/posts/x",
		},
		{
			name:         "no param falls back to root",
			target:       "/api/login/visitor",
			wantStatus:   http.StatusTemporaryRedirect,
			wantLocation: "/",
		},
		{
			name:       "absolute target without an allowed origin is rejected",
			target:     "/api/login/visitor?redirect_if_succeed=https%3A%2F%2Fevil.example.com%2F",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "protocol-relative target is rejected",
			target:     "/api/login/visitor?redirect_if_succeed=%2F%2Fevil.example.com%2F",
			wantStatus: http.StatusForbidden,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHandler(&stubTicketGenerator{ticket: "tick-1"}, testIssuer)
			if tc.origins != nil {
				h.AllowedOrigins = tc.origins
			}

			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, tc.target, nil))

			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", rr.Code, tc.wantStatus, rr.Body.String())
			}
			if tc.wantStatus == http.StatusForbidden {
				if body := rr.Body.String(); !strings.Contains(body, "is not in the allowed origins") {
					t.Errorf("body = %q, want it to mention 'is not in the allowed origins'", body)
				}
				return
			}
			if got := rr.Header().Get("Location"); got != tc.wantLocation {
				t.Errorf("Location = %q, want %q", got, tc.wantLocation)
			}
		})
	}
}

// TestServeHTTP_TicketFailure covers the branch where no visitor ticket can
// be obtained (e.g. the ticket generator is shut down): the handler rejects
// with 400 before any claims or token work.
func TestServeHTTP_TicketFailure(t *testing.T) {
	h := newTestHandler(&stubTicketGenerator{err: errors.New("tick generator closed")}, testIssuer)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/login/visitor", nil))

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d (body %q)", rr.Code, http.StatusBadRequest, rr.Body.String())
	}
	if body := rr.Body.String(); !strings.Contains(body, "can't wait for visitor ticket to be generated") {
		t.Errorf("body = %q, want it to mention 'can't wait for visitor ticket to be generated'", body)
	}
	if c := findCookie(rr, pkgapicommon.DefaultJWTCookieKey); c != nil {
		t.Errorf("jwt cookie must not be set on failure, got %q", c.Value)
	}
}

// TestServeHTTP_TokenSignFailure drives the token-signing failure branch by
// using a JWT issuer without an issuer name, which makes IssueToken fail.
func TestServeHTTP_TokenSignFailure(t *testing.T) {
	h := newTestHandler(&stubTicketGenerator{ticket: "tick-1"}, "") // empty issuer: IssueToken always fails

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/login/visitor", nil))

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d (body %q)", rr.Code, http.StatusInternalServerError, rr.Body.String())
	}
	if body := rr.Body.String(); !strings.Contains(body, "failed to sign token") {
		t.Errorf("body = %q, want it to mention 'failed to sign token'", body)
	}
	if c := findCookie(rr, pkgapicommon.DefaultJWTCookieKey); c != nil {
		t.Errorf("jwt cookie must not be set on failure, got %q", c.Value)
	}
}

// TestServeHTTP_RealTicketGenerator wires the handler with the real
// SharedTickingTicketGenerator (as cmd/server/main.go does) to confirm the
// handler blocks until a tick is produced and then completes the login.
func TestServeHTTP_RealTicketGenerator(t *testing.T) {
	gen := pkgauth.NewSharedTickingTicketGenerator(time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	gen.Run(ctx)

	h := newTestHandler(gen, testIssuer)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/login/visitor", nil))

	if rr.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want %d (body %q)", rr.Code, http.StatusTemporaryRedirect, rr.Body.String())
	}
	assertVisitorClaims(t, parseIssuedClaims(t, rr))
}

// TestGetMapClaims verifies the claim set built for visitor sessions and that
// consecutive calls mint distinct identities (unique jti and subject).
func TestGetMapClaims(t *testing.T) {
	h := newTestHandler(&stubTicketGenerator{}, testIssuer)
	r := httptest.NewRequest(http.MethodGet, "/api/login/visitor", nil)

	claims1, err := h.GetMapClaims(r)
	if err != nil {
		t.Fatalf("GetMapClaims: %v", err)
	}
	claims2, err := h.GetMapClaims(r)
	if err != nil {
		t.Fatalf("GetMapClaims: %v", err)
	}

	aud, ok := claims1["aud"].([]any)
	if !ok || len(aud) != 1 || aud[0] != pkgauth.AudSession {
		t.Errorf("claims[aud] = %v, want [%q]", claims1["aud"], pkgauth.AudSession)
	}

	sub1, _ := claims1["sub"].(string)
	if !strings.HasPrefix(sub1, pkgauth.VisitorSubjectPrefix) {
		t.Errorf("sub = %q, want the %q prefix", sub1, pkgauth.VisitorSubjectPrefix)
	}
	username1, _ := claims1["username"].(string)
	if !visitorUsernamePattern.MatchString(username1) {
		t.Errorf("username = %q, want it to match %q", username1, visitorUsernamePattern)
	}
	jti1, _ := claims1["jti"].(string)
	if jti1 == "" {
		t.Error("jti is empty, want a uuid")
	}

	nbf, ok := claims1["nbf"].(float64)
	if !ok {
		t.Fatalf("claims[nbf] = %v, want a numeric date", claims1["nbf"])
	}
	exp, ok := claims1["exp"].(float64)
	if !ok {
		t.Fatalf("claims[exp] = %v, want a numeric date", claims1["exp"])
	}
	if got, want := time.Unix(int64(exp), 0).Sub(time.Unix(int64(nbf), 0)), testValidity; got != want {
		t.Errorf("exp - nbf = %v, want validity %v", got, want)
	}

	// Each call must mint a fresh identity.
	if sub2, _ := claims2["sub"].(string); sub2 == sub1 {
		t.Errorf("two GetMapClaims calls returned the same subject %q, want distinct visitor ids", sub1)
	}
	if jti2, _ := claims2["jti"].(string); jti2 == jti1 {
		t.Errorf("two GetMapClaims calls returned the same jti %q, want distinct uuids", jti1)
	}
}

// TestVisitorLoginHandler_RouteMounted mirrors the wiring in
// cmd/server/main.go (mux.Handle at /api/login/visitor) to confirm requests
// to that path reach the handler.
func TestVisitorLoginHandler_RouteMounted(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle("/api/login/visitor", newTestHandler(&stubTicketGenerator{ticket: "tick-1"}, testIssuer))

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/login/visitor", nil))

	if rr.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want %d (body %q)", rr.Code, http.StatusTemporaryRedirect, rr.Body.String())
	}
	if got := rr.Header().Get("Location"); got != "/" {
		t.Errorf("Location = %q, want %q", got, "/")
	}
	assertVisitorClaims(t, parseIssuedClaims(t, rr))
}
