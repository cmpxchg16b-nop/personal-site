package google_test

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
	"personal-site/pkg/api/login/oauth2/google"
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
	testRedirectURI = "http://localhost:8080/api/login/oauth2/google/auth"
)

// newTestHandler builds a handler wired with a real JWT issuer (signed with a
// static test secret) and the real cookie builder; only the nonce issuer is
// faked so tests can script nonce outcomes.
func newTestHandler(nonceIssuer pkgauth.NonceIssuer) *google.GoogleOAuthLoginHandler {
	keyProvider := pkgauth.NewStaticSecretProvider([]byte(testJWTSecret))
	return google.NewGoogleOAuthLoginHandler(
		24*time.Hour,
		"test-client-id",
		"test-client-secret",
		testRedirectURI,
		"", // default google login page
		"", // default scope ("openid profile email")
		"", // default google token endpoint
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

// TestServeHTTP_NotFound covers the fallthrough branch: paths ending in
// neither /start nor /auth get a 404 with a JSON error body.
func TestServeHTTP_NotFound(t *testing.T) {
	h := newTestHandler(&stubNonceIssuer{})

	for _, path := range []string{
		"/api/login/oauth2/google",
		"/api/login/oauth2/google/bogus",
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
// google authorize URL with the expected query params, and the failure paths.
func TestHandleStart(t *testing.T) {
	tests := []struct {
		name        string
		nonceIssuer *stubNonceIssuer
		loginPage   string // overrides GoogleOAuthLoginPage when non-empty
		scope       string // overrides GoogleOAuthScope when non-empty
		wantStatus  int
		check       func(t *testing.T, rr *httptest.ResponseRecorder)
	}{
		{
			name:        "redirects to google authorize page with nonce cookie and default scope",
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
				if u.Host != "accounts.google.com" || u.Path != "/o/oauth2/v2/auth" {
					t.Errorf("redirect = %q, want https://accounts.google.com/o/oauth2/v2/auth", u)
				}
				q := u.Query()
				for k, want := range map[string]string{
					"client_id":     "test-client-id",
					"redirect_uri":  testRedirectURI,
					"response_type": "code",
					"scope":         "openid profile email",
					"state":         "nonce-123",
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
			loginPage:   "https://idp.example.com/o/oauth2/v2/auth",
			scope:       "openid email",
			wantStatus:  http.StatusTemporaryRedirect,
			check: func(t *testing.T, rr *httptest.ResponseRecorder) {
				u, err := url.Parse(rr.Header().Get("Location"))
				if err != nil {
					t.Fatalf("Location %q does not parse: %v", rr.Header().Get("Location"), err)
				}
				if u.Host != "idp.example.com" {
					t.Errorf("redirect host = %q, want idp.example.com", u.Host)
				}
				if got := u.Query().Get("scope"); got != "openid email" {
					t.Errorf("scope = %q, want %q", got, "openid email")
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
			h.GoogleOAuthLoginPage = tc.loginPage
			h.GoogleOAuthScope = tc.scope

			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/login/oauth2/google/start", nil))

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
			target:         "/api/login/oauth2/google/auth?error=access_denied&error_description=User+denied+access",
			wantStatus:     http.StatusBadRequest,
			wantBodySubstr: "access_denied: User denied access",
		},
		{
			name:           "missing nonce cookie responds 400",
			target:         "/api/login/oauth2/google/auth?state=nonce-1&code=code-1",
			wantStatus:     http.StatusBadRequest,
			wantBodySubstr: "Nonce is not found from the cookies",
		},
		{
			name:           "state param not matching the nonce cookie responds 401",
			target:         "/api/login/oauth2/google/auth?state=nonce-1&code=code-1",
			cookie:         nonceCookie("other-nonce"),
			wantStatus:     http.StatusUnauthorized,
			wantBodySubstr: "does not match",
		},
		{
			name:   "nonce validation error responds 401",
			target: "/api/login/oauth2/google/auth?state=nonce-1&code=code-1",
			cookie: nonceCookie("nonce-1"),
			validateFunc: func(string) (bool, error) {
				return false, errors.New("nonce store unavailable")
			},
			wantStatus:     http.StatusUnauthorized,
			wantBodySubstr: "Invalid nonce",
		},
		{
			name:   "rejected nonce responds 401",
			target: "/api/login/oauth2/google/auth?state=nonce-1&code=code-1",
			cookie: nonceCookie("nonce-1"),
			validateFunc: func(string) (bool, error) {
				return false, nil
			},
			wantStatus:     http.StatusUnauthorized,
			wantBodySubstr: "Invalid nonce",
		},
		{
			name:           "missing authorization code responds 401",
			target:         "/api/login/oauth2/google/auth?state=nonce-1",
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

// TestHandleAuthorizationCode_TokenExchangeFailures covers the token endpoint
// failure branches: non-200 responses (with and without a decodable google
// error body), undecodable success bodies, and the granted-scope checks. All
// of these short-circuit before the profile lookup and token revocation, so
// only the token endpoint is faked.
func TestHandleAuthorizationCode_TokenExchangeFailures(t *testing.T) {
	tests := []struct {
		name           string
		tokenStatus    int
		tokenBody      string
		wantBodySubstr string
	}{
		{
			name:           "non-200 with google error body reports the error",
			tokenStatus:    http.StatusBadRequest,
			tokenBody:      `{"error":"invalid_grant","error_description":"The code is bad"}`,
			wantBodySubstr: "Token exchange failed: invalid_grant: The code is bad",
		},
		{
			name:           "non-200 with undecodable body reports the status",
			tokenStatus:    http.StatusInternalServerError,
			tokenBody:      "this is not json",
			wantBodySubstr: "Token exchange failed with status 500",
		},
		{
			name:           "200 with undecodable body responds 401",
			tokenStatus:    http.StatusOK,
			tokenBody:      "this is not json",
			wantBodySubstr: "Failed to decode google token api response",
		},
		{
			name:           "empty granted scope responds 401",
			tokenStatus:    http.StatusOK,
			tokenBody:      `{"access_token":"ya29_testtoken","token_type":"Bearer"}`,
			wantBodySubstr: "No scopes were granted by Google",
		},
		{
			name:           "scope without email or profile responds 401",
			tokenStatus:    http.StatusOK,
			tokenBody:      `{"access_token":"ya29_testtoken","token_type":"Bearer","scope":"openid"}`,
			wantBodySubstr: "Required scopes (email or profile) were not granted",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.tokenStatus)
				io.WriteString(w, tc.tokenBody)
			}))
			defer tokenSrv.Close()

			h := newTestHandler(&stubNonceIssuer{}) // accepts any nonce
			h.GoogleOAuthTokenEndpoint = tokenSrv.URL

			r := httptest.NewRequest(http.MethodGet, "/api/login/oauth2/google/auth?state=nonce-1&code=code-1", nil)
			r.AddCookie(&http.Cookie{Name: pkgapicommon.DefaultNonceCookieKey, Value: "nonce-1"})
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, r)

			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d (body %q)", rr.Code, http.StatusUnauthorized, rr.Body.String())
			}
			if body := rr.Body.String(); !strings.Contains(body, tc.wantBodySubstr) {
				t.Errorf("body = %q, want it to contain %q", body, tc.wantBodySubstr)
			}
		})
	}
}

// TestHandleAuthorizationCode_HappyPath runs the whole /auth flow against a
// fake token endpoint (httptest) and stubbed google API hosts: the profile and
// token-revocation URLs are hardcoded in pkg/google, so http.DefaultClient is
// swapped for the duration of the test. It verifies the exchange request, the
// nonce cookie clearing, the redirect target, the issued JWT claims, and the
// token revocation call.
func TestHandleAuthorizationCode_HappyPath(t *testing.T) {
	// Fake google token endpoint: capture the exchange request to assert on
	// it afterwards, and answer with a token and the granted scopes.
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
		io.WriteString(w, `{"access_token":"ya29_testtoken","token_type":"Bearer","expires_in":3599,"scope":"openid profile email"}`)
	}))
	defer tokenSrv.Close()

	// The profile lookup (pkg/google.GetGoogleProfileByToken) and the deferred
	// token revocation both go through http.DefaultClient with hardcoded
	// google API URLs; intercept those and pass everything else (the httptest
	// token endpoint) through to the real transport.
	var revokeForm url.Values
	origClient := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch {
		case req.Method == http.MethodGet && req.URL.Host == "openidconnect.googleapis.com" && req.URL.Path == "/v1/userinfo":
			if got, want := req.Header.Get("Authorization"), "Bearer ya29_testtoken"; got != want {
				t.Errorf("profile request Authorization = %q, want %q", got, want)
			}
			return cannedResponse(req, http.StatusOK, `{"sub":"118234567890","email":"octo@gmail.com","name":"Octo Cat","verified_email":true}`), nil
		case req.Method == http.MethodPost && req.URL.Host == "oauth2.googleapis.com" && req.URL.Path == "/revoke":
			bodyBytes, err := io.ReadAll(req.Body)
			if err != nil {
				t.Errorf("revoke request: cannot read body: %v", err)
			}
			revokeForm, _ = url.ParseQuery(string(bodyBytes))
			return cannedResponse(req, http.StatusOK, ""), nil
		}
		// origClient.Transport is nil (http.DefaultClient is &Client{}); the
		// stdlib falls back to http.DefaultTransport in that case, and so do we.
		return http.DefaultTransport.RoundTrip(req)
	})}
	t.Cleanup(func() { http.DefaultClient = origClient })

	h := newTestHandler(&stubNonceIssuer{}) // accepts any nonce
	h.GoogleOAuthTokenEndpoint = tokenSrv.URL

	r := httptest.NewRequest(http.MethodGet, "/api/login/oauth2/google/auth?state=nonce-1&code=code-1", nil)
	r.AddCookie(&http.Cookie{Name: pkgapicommon.DefaultNonceCookieKey, Value: "nonce-1"})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want %d (body %q)", rr.Code, http.StatusTemporaryRedirect, rr.Body.String())
	}
	if got := rr.Header().Get("Location"); got != "/welcome" {
		t.Errorf("Location = %q, want %q", got, "/welcome")
	}

	// The nonce cookie must be cleared once the nonce has been validated.
	if c := findCookie(rr, pkgapicommon.DefaultNonceCookieKey); c == nil {
		t.Error("nonce cookie clearing header is not set")
	} else if c.Value != "" || c.MaxAge >= 0 {
		t.Errorf("nonce cookie = %q (MaxAge %d), want empty value with negative MaxAge", c.Value, c.MaxAge)
	}

	// The token exchange must carry the app credentials, the authz code and
	// the authorization_code grant type.
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
		"grant_type":    "authorization_code",
		"redirect_uri":  testRedirectURI,
	} {
		if got := tokenReq.form.Get(k); got != want {
			t.Errorf("token endpoint form %s = %q, want %q", k, got, want)
		}
	}

	// The session cookie must hold a JWT whose claims describe the google user.
	claims := parseIssuedClaims(t, rr)
	if got, want := claims.Subject, "google:118234567890"; got != want {
		t.Errorf("sub = %q, want %q", got, want)
	}
	if got, want := claims.Username, "octo@gmail.com"; got != want {
		t.Errorf("username = %q, want %q", got, want)
	}
	if got, want := claims.Email, "octo@gmail.com"; got != want {
		t.Errorf("email = %q, want %q", got, want)
	}

	// The handler defers revocation of the google access token once the
	// session JWT is issued.
	if got := revokeForm.Get("token"); got != "ya29_testtoken" {
		t.Errorf("revoke form token = %q, want %q", got, "ya29_testtoken")
	}
}

// TestHandleAuthorizationCode_ProfileVariants covers the profile-derived
// identity fallbacks: the legacy "id" field is used when "sub" is absent, the
// display name becomes the username when the email is empty, and a profile
// without any id is rejected.
func TestHandleAuthorizationCode_ProfileVariants(t *testing.T) {
	tests := []struct {
		name           string
		profileJSON    string
		wantStatus     int
		wantSubject    string // only checked when wantStatus is 307
		wantUsername   string // only checked when wantStatus is 307
		wantBodySubstr string // only checked when wantStatus is not 307
	}{
		{
			name:         "id field is used when sub is absent, name becomes username when email is empty",
			profileJSON:  `{"id":"99887766","name":"Octo Cat"}`,
			wantStatus:   http.StatusTemporaryRedirect,
			wantSubject:  "google:99887766",
			wantUsername: "Octo Cat",
		},
		{
			name:           "profile without any id responds 401",
			profileJSON:    `{"name":"Nobody"}`,
			wantStatus:     http.StatusUnauthorized,
			wantBodySubstr: "Failed to get Id of google user",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"access_token":"ya29_testtoken","token_type":"Bearer","scope":"openid profile email"}`)
			}))
			defer tokenSrv.Close()

			origClient := http.DefaultClient
			http.DefaultClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				switch {
				case req.Method == http.MethodGet && req.URL.Host == "openidconnect.googleapis.com":
					return cannedResponse(req, http.StatusOK, tc.profileJSON), nil
				case req.Method == http.MethodPost && req.URL.Host == "oauth2.googleapis.com":
					return cannedResponse(req, http.StatusOK, ""), nil
				}
				return http.DefaultTransport.RoundTrip(req)
			})}
			t.Cleanup(func() { http.DefaultClient = origClient })

			h := newTestHandler(&stubNonceIssuer{}) // accepts any nonce
			h.GoogleOAuthTokenEndpoint = tokenSrv.URL

			r := httptest.NewRequest(http.MethodGet, "/api/login/oauth2/google/auth?state=nonce-1&code=code-1", nil)
			r.AddCookie(&http.Cookie{Name: pkgapicommon.DefaultNonceCookieKey, Value: "nonce-1"})
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, r)

			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", rr.Code, tc.wantStatus, rr.Body.String())
			}
			if tc.wantStatus == http.StatusTemporaryRedirect {
				claims := parseIssuedClaims(t, rr)
				if got := claims.Subject; got != tc.wantSubject {
					t.Errorf("sub = %q, want %q", got, tc.wantSubject)
				}
				if got := claims.Username; got != tc.wantUsername {
					t.Errorf("username = %q, want %q", got, tc.wantUsername)
				}
			} else if body := rr.Body.String(); !strings.Contains(body, tc.wantBodySubstr) {
				t.Errorf("body = %q, want it to contain %q", body, tc.wantBodySubstr)
			}
		})
	}
}

// TestGetMapClaims verifies the claim set that handleAuthorizationCode signs
// into the session JWT: google subject, username/email passthrough, session
// audience, and an expiry one session-lifespan out.
func TestGetMapClaims(t *testing.T) {
	const lifespan = 12 * time.Hour
	h := newTestHandler(&stubNonceIssuer{})
	h.SessionLifespan = lifespan

	before := time.Now()
	claims, err := h.GetMapClaims(
		httptest.NewRequest(http.MethodGet, "/api/login/oauth2/google/auth", nil),
		"google:42", "octo@gmail.com", "octo@gmail.com",
	)
	if err != nil {
		t.Fatalf("GetMapClaims: %v", err)
	}
	after := time.Now()

	for k, want := range map[string]any{
		"sub":      "google:42",
		"username": "octo@gmail.com",
		"email":    "octo@gmail.com",
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
