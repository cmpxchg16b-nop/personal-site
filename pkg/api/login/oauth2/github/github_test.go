package github_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	pkgapicommon "personal-site/pkg/api/common"
	"personal-site/pkg/api/login/oauth2/github"
	pkgauth "personal-site/pkg/auth"
	pkgcookie "personal-site/pkg/cookie"
)

// stubNonceIssuer is a test double for pkgauth.NonceIssuer returning scripted
// values. A nil validateFunc accepts every nonce.
type stubNonceIssuer struct {
	nonce        string
	issueErr     error
	validateFunc func(nonce string) (bool, error)
}

var _ pkgauth.NonceIssuer = (*stubNonceIssuer)(nil)

func (s *stubNonceIssuer) IssueNonce(context.Context) (string, error) {
	return s.nonce, s.issueErr
}

func (s *stubNonceIssuer) ValidateNonce(_ context.Context, nonce string) (bool, error) {
	if s.validateFunc != nil {
		return s.validateFunc(nonce)
	}
	return true, nil
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

const (
	testJWTSecret   = "test-jwt-secret"
	testRedirectURI = "http://localhost:8080/api/login/oauth2/github/auth"
)

// newTestHandler builds a handler wired like cmd/server/main.go does: a real
// JWT issuer (signed with a static test secret) and the real cookie builder;
// only the nonce issuer is faked so tests can script nonce outcomes.
func newTestHandler(nonceIssuer pkgauth.NonceIssuer) *github.GithubOAuthLoginHandler {
	keyProvider := pkgauth.NewStaticSecretProvider([]byte(testJWTSecret))
	return github.NewGithubOAuthLoginHandler(
		24*time.Hour,
		"test-client-id",
		"test-client-secret",
		testRedirectURI,
		"", // default github login page
		"", // default scope ("read:user")
		"", // default github token endpoint
		"/welcome",
		nil, // no allowed origins; tests use an absolute redirect URI
		pkgauth.NewStaticKeyJWTIssuer(keyProvider, "test-issuer"),
		nonceIssuer,
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

// cannedResponse builds a minimal *http.Response for intercepted requests.
func cannedResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		StatusCode:    status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": []string{"application/json"}},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

// TestServeHTTP_NotFound covers the fallthrough branch: paths ending in
// neither /start nor /auth get a 404 with a JSON error body.
func TestServeHTTP_NotFound(t *testing.T) {
	h := newTestHandler(&stubNonceIssuer{})

	for _, path := range []string{
		"/api/login/oauth2/github",
		"/api/login/oauth2/github/bogus",
	} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))

		if rr.Code != http.StatusNotFound {
			t.Errorf("GET %s: status = %d, want %d", path, rr.Code, http.StatusNotFound)
		}
		if body := rr.Body.String(); !strings.Contains(body, "has no handler attached") {
			t.Errorf("GET %s: body = %q, want it to mention 'has no handler attached'", path, body)
		}
	}
}

// TestHandleStart exercises the /start endpoint: nonce cookie, redirect to the
// github authorize URL with the expected query params, and the failure paths.
func TestHandleStart(t *testing.T) {
	tests := []struct {
		name        string
		nonceIssuer *stubNonceIssuer
		loginPage   string // overrides GithubOAuthLoginPage when non-empty
		scope       string // overrides GithubOAuthScope when non-empty
		wantStatus  int
		check       func(t *testing.T, rr *httptest.ResponseRecorder)
	}{
		{
			name:        "redirects to github authorize page with nonce cookie and default scope",
			nonceIssuer: &stubNonceIssuer{nonce: "nonce-123"},
			wantStatus:  http.StatusTemporaryRedirect,
			check: func(t *testing.T, rr *httptest.ResponseRecorder) {
				c := findCookie(rr, pkgapicommon.DefaultNonceCookieKey)
				if c == nil {
					t.Fatal("nonce cookie is not set")
				}
				if c.Value != "nonce-123" {
					t.Errorf("nonce cookie = %q, want %q", c.Value, "nonce-123")
				}

				u, err := url.Parse(rr.Header().Get("Location"))
				if err != nil {
					t.Fatalf("Location %q does not parse: %v", rr.Header().Get("Location"), err)
				}
				if u.Host != "github.com" || u.Path != "/login/oauth/authorize" {
					t.Errorf("redirect = %q, want https://github.com/login/oauth/authorize", u)
				}
				q := u.Query()
				for k, want := range map[string]string{
					"client_id":    "test-client-id",
					"redirect_uri": testRedirectURI,
					"scope":        "read:user",
					"state":        "nonce-123",
				} {
					if got := q.Get(k); got != want {
						t.Errorf("redirect query %s = %q, want %q", k, got, want)
					}
				}
			},
		},
		{
			name:        "custom login page and scope are honored",
			nonceIssuer: &stubNonceIssuer{nonce: "nonce-abc"},
			loginPage:   "https://ghe.example.com/login/oauth/authorize",
			scope:       "read:user user:email",
			wantStatus:  http.StatusTemporaryRedirect,
			check: func(t *testing.T, rr *httptest.ResponseRecorder) {
				u, err := url.Parse(rr.Header().Get("Location"))
				if err != nil {
					t.Fatalf("Location %q does not parse: %v", rr.Header().Get("Location"), err)
				}
				if u.Host != "ghe.example.com" {
					t.Errorf("redirect host = %q, want ghe.example.com", u.Host)
				}
				if got := u.Query().Get("scope"); got != "read:user user:email" {
					t.Errorf("scope = %q, want %q", got, "read:user user:email")
				}
			},
		},
		{
			name:        "nonce issue failure responds 500",
			nonceIssuer: &stubNonceIssuer{issueErr: errors.New("signing key unavailable")},
			wantStatus:  http.StatusInternalServerError,
			check: func(t *testing.T, rr *httptest.ResponseRecorder) {
				if body := rr.Body.String(); !strings.Contains(body, "Failed to issue nonce") {
					t.Errorf("body = %q, want it to mention 'Failed to issue nonce'", body)
				}
			},
		},
		{
			name:        "unparseable login page URL responds 500",
			nonceIssuer: &stubNonceIssuer{nonce: "nonce-1"},
			loginPage:   "http://\x7f.invalid/", // control characters make url.Parse fail
			wantStatus:  http.StatusInternalServerError,
			check: func(t *testing.T, rr *httptest.ResponseRecorder) {
				if body := rr.Body.String(); !strings.Contains(body, "Failed to determine redir url") {
					t.Errorf("body = %q, want it to mention 'Failed to determine redir url'", body)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHandler(tc.nonceIssuer)
			h.GithubOAuthLoginPage = tc.loginPage
			h.GithubOAuthScope = tc.scope

			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/login/oauth2/github/start", nil))

			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", rr.Code, tc.wantStatus, rr.Body.String())
			}
			tc.check(t, rr)
		})
	}
}

// TestHandleAuthorizationCode_ErrorCases walks the /auth guard branches in
// order: oauth error param, nonce cookie presence, cookie/state match, nonce
// validity, and authorization code presence. All of these short-circuit
// before any outbound HTTP call, so no token endpoint is needed.
func TestHandleAuthorizationCode_ErrorCases(t *testing.T) {
	nonceCookie := func(value string) *http.Cookie {
		return &http.Cookie{Name: pkgapicommon.DefaultNonceCookieKey, Value: value}
	}

	tests := []struct {
		name           string
		target         string
		cookie         *http.Cookie
		validateFunc   func(nonce string) (bool, error)
		wantStatus     int
		wantBodySubstr string
	}{
		{
			name:           "oauth error param is reported as 400",
			target:         "/api/login/oauth2/github/auth?error=access_denied&error_description=User+denied+access",
			wantStatus:     http.StatusBadRequest,
			wantBodySubstr: "access_denied: User denied access",
		},
		{
			name:           "missing nonce cookie responds 400",
			target:         "/api/login/oauth2/github/auth?state=nonce-1&code=code-1",
			wantStatus:     http.StatusBadRequest,
			wantBodySubstr: "Nonce is not found from the cookies",
		},
		{
			name:           "state param not matching the nonce cookie responds 401",
			target:         "/api/login/oauth2/github/auth?state=nonce-1&code=code-1",
			cookie:         nonceCookie("other-nonce"),
			wantStatus:     http.StatusUnauthorized,
			wantBodySubstr: "does not match",
		},
		{
			name:   "nonce validation error responds 401",
			target: "/api/login/oauth2/github/auth?state=nonce-1&code=code-1",
			cookie: nonceCookie("nonce-1"),
			validateFunc: func(string) (bool, error) {
				return false, errors.New("nonce store unavailable")
			},
			wantStatus:     http.StatusUnauthorized,
			wantBodySubstr: "Invalid nonce",
		},
		{
			name:   "rejected nonce responds 401",
			target: "/api/login/oauth2/github/auth?state=nonce-1&code=code-1",
			cookie: nonceCookie("nonce-1"),
			validateFunc: func(string) (bool, error) {
				return false, nil
			},
			wantStatus:     http.StatusUnauthorized,
			wantBodySubstr: "Invalid nonce",
		},
		{
			name:           "missing authorization code responds 401",
			target:         "/api/login/oauth2/github/auth?state=nonce-1",
			cookie:         nonceCookie("nonce-1"),
			wantStatus:     http.StatusUnauthorized,
			wantBodySubstr: "No authorization code",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHandler(&stubNonceIssuer{validateFunc: tc.validateFunc})

			r := httptest.NewRequest(http.MethodGet, tc.target, nil)
			if tc.cookie != nil {
				r.AddCookie(tc.cookie)
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, r)

			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", rr.Code, tc.wantStatus, rr.Body.String())
			}
			if body := rr.Body.String(); !strings.Contains(body, tc.wantBodySubstr) {
				t.Errorf("body = %q, want it to contain %q", body, tc.wantBodySubstr)
			}
		})
	}
}

// TestHandleAuthorizationCode_TokenExchangeFailure covers the branch where the
// github token endpoint answers with an undecodable body: the login must be
// rejected with 401.
func TestHandleAuthorizationCode_TokenExchangeFailure(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "this is not json")
	}))
	defer tokenSrv.Close()

	h := newTestHandler(&stubNonceIssuer{}) // accepts any nonce
	h.GithubOAuthTokenEndpoint = tokenSrv.URL

	r := httptest.NewRequest(http.MethodGet, "/api/login/oauth2/github/auth?state=nonce-1&code=code-1", nil)
	r.AddCookie(&http.Cookie{Name: pkgapicommon.DefaultNonceCookieKey, Value: "nonce-1"})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d (body %q)", rr.Code, http.StatusUnauthorized, rr.Body.String())
	}
	if body := rr.Body.String(); !strings.Contains(body, "Failed to decode github token api response") {
		t.Errorf("body = %q, want it to mention 'Failed to decode github token api response'", body)
	}
}

// TestHandleAuthorizationCode_HappyPath runs the whole /auth flow against a
// fake token endpoint (httptest) and a stubbed api.github.com: the profile and
// token-revocation URLs are hardcoded in pkg/github, so http.DefaultClient is
// swapped for the duration of the test. It verifies the exchange request, the
// redirect target, and the claims of the issued session JWT.
func TestHandleAuthorizationCode_HappyPath(t *testing.T) {
	// Fake github token endpoint: capture the exchange request to assert on
	// it afterwards, and answer with a token.
	var tokenReq struct {
		method      string
		contentType string
		accept      string
		form        url.Values
	}
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenReq.method = r.Method
		tokenReq.contentType = r.Header.Get("Content-Type")
		tokenReq.accept = r.Header.Get("Accept")
		if err := r.ParseForm(); err != nil {
			t.Errorf("token endpoint: cannot parse form: %v", err)
		}
		tokenReq.form = r.Form
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"access_token":"gho_testtoken","token_type":"bearer","scope":"read:user"}`)
	}))
	defer tokenSrv.Close()

	// The profile lookup (pkg/github.GetGithubProfileByToken) and the deferred
	// token revocation both go through http.DefaultClient with hardcoded
	// api.github.com URLs; intercept those and pass everything else (the
	// httptest token endpoint) through to the real transport.
	revoked := false
	origClient := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "api.github.com" {
			switch {
			case req.Method == http.MethodGet && req.URL.Path == "/user":
				if got, want := req.Header.Get("Authorization"), "Bearer gho_testtoken"; got != want {
					t.Errorf("profile request Authorization = %q, want %q", got, want)
				}
				return cannedResponse(req, http.StatusOK, `{"login":"octocat","id":12345,"email":"octo@example.com"}`), nil
			case req.Method == http.MethodDelete && strings.HasPrefix(req.URL.Path, "/applications/"):
				revoked = true
				return cannedResponse(req, http.StatusNoContent, ""), nil
			}
		}
		// origClient.Transport is nil (http.DefaultClient is &Client{}); the
		// stdlib falls back to http.DefaultTransport in that case, and so do we.
		return http.DefaultTransport.RoundTrip(req)
	})}
	t.Cleanup(func() { http.DefaultClient = origClient })

	h := newTestHandler(&stubNonceIssuer{}) // accepts any nonce
	h.GithubOAuthTokenEndpoint = tokenSrv.URL

	r := httptest.NewRequest(http.MethodGet, "/api/login/oauth2/github/auth?state=nonce-1&code=code-1", nil)
	r.AddCookie(&http.Cookie{Name: pkgapicommon.DefaultNonceCookieKey, Value: "nonce-1"})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want %d (body %q)", rr.Code, http.StatusTemporaryRedirect, rr.Body.String())
	}
	if got := rr.Header().Get("Location"); got != "/welcome" {
		t.Errorf("Location = %q, want %q", got, "/welcome")
	}

	// The token exchange must carry the app credentials and the authz code.
	if tokenReq.method != http.MethodPost {
		t.Errorf("token endpoint method = %q, want POST", tokenReq.method)
	}
	if tokenReq.contentType != "application/x-www-form-urlencoded" {
		t.Errorf("token endpoint Content-Type = %q, want application/x-www-form-urlencoded", tokenReq.contentType)
	}
	if tokenReq.accept != "application/json" {
		t.Errorf("token endpoint Accept = %q, want application/json", tokenReq.accept)
	}
	for k, want := range map[string]string{
		"client_id":     "test-client-id",
		"client_secret": "test-client-secret",
		"code":          "code-1",
		"redirect_uri":  testRedirectURI,
	} {
		if got := tokenReq.form.Get(k); got != want {
			t.Errorf("token endpoint form %s = %q, want %q", k, got, want)
		}
	}

	// The session cookie must hold a JWT whose claims describe the github user.
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
	if got, want := claims.Subject, "github:12345"; got != want {
		t.Errorf("sub = %q, want %q", got, want)
	}
	if got, want := claims.Username, "octocat"; got != want {
		t.Errorf("username = %q, want %q", got, want)
	}
	if got, want := claims.Email, "octo@example.com"; got != want {
		t.Errorf("email = %q, want %q", got, want)
	}

	// The handler defers revocation of the github access token once the
	// session JWT is issued.
	if !revoked {
		t.Error("github access token was not revoked after login")
	}
}

// TestGithubOAuthLoginHandler_RouteMounted mirrors the wiring in
// cmd/server/main.go (mux.Handle at the exact path and at the subtree) to
// confirm /start and /auth requests reach the handler.
func TestGithubOAuthLoginHandler_RouteMounted(t *testing.T) {
	h := newTestHandler(&stubNonceIssuer{nonce: "nonce-route"})

	mux := http.NewServeMux()
	mux.Handle("/api/login/oauth2/github", h)
	mux.Handle("/api/login/oauth2/github/", h)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/login/oauth2/github/start", nil))
	if rr.Code != http.StatusTemporaryRedirect {
		t.Fatalf("/start: status = %d, want %d", rr.Code, http.StatusTemporaryRedirect)
	}
	if loc := rr.Header().Get("Location"); !strings.HasPrefix(loc, "https://github.com/") {
		t.Errorf("/start: Location = %q, want a github.com URL", loc)
	}

	// /auth with an oauth error param short-circuits with 400 — enough to
	// prove the subtree route reaches handleAuthorizationCode.
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/login/oauth2/github/auth?error=access_denied", nil))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("/auth: status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

// TestGetMapClaims verifies the claim set that handleAuthorizationCode signs
// into the session JWT: github subject, username/email passthrough, session
// audience, and an expiry one session-lifespan out.
func TestGetMapClaims(t *testing.T) {
	const lifespan = 12 * time.Hour
	h := newTestHandler(&stubNonceIssuer{})
	h.SessionLifespan = lifespan

	before := time.Now()
	claims, err := h.GetMapClaims(
		httptest.NewRequest(http.MethodGet, "/api/login/oauth2/github/auth", nil),
		"github:42", "octocat", "octo@example.com",
	)
	if err != nil {
		t.Fatalf("GetMapClaims: %v", err)
	}
	after := time.Now()

	for k, want := range map[string]any{
		"sub":      "github:42",
		"username": "octocat",
		"email":    "octo@example.com",
	} {
		if got := claims[k]; got != want {
			t.Errorf("claims[%q] = %v, want %v", k, got, want)
		}
	}

	aud, ok := claims["aud"].([]any)
	if !ok || len(aud) != 1 || aud[0] != pkgauth.AudSession {
		t.Errorf("claims[aud] = %v, want [%q]", claims["aud"], pkgauth.AudSession)
	}

	if jti, _ := claims["jti"].(string); jti == "" {
		t.Error("claims[jti] is empty, want a uuid")
	}

	nbf, ok := claims["nbf"].(float64)
	if !ok {
		t.Fatalf("claims[nbf] = %v, want a numeric date", claims["nbf"])
	}
	exp, ok := claims["exp"].(float64)
	if !ok {
		t.Fatalf("claims[exp] = %v, want a numeric date", claims["exp"])
	}
	nbfTime := time.Unix(int64(nbf), 0)
	if nbfTime.Before(before.Add(-time.Minute)) || nbfTime.After(after.Add(time.Minute)) {
		t.Errorf("nbf = %v, want it close to now (%v..%v)", nbfTime, before, after)
	}
	if got, want := time.Unix(int64(exp), 0).Sub(nbfTime), lifespan; got != want {
		t.Errorf("exp - nbf = %v, want session lifespan %v", got, want)
	}
}
