package cloudflare_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"personal-site/pkg/api/login/oidc/idTokenHeader/cloudflare"
	pkgutils "personal-site/pkg/utils"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/golang-jwt/jwt/v5"
)

const (
	testTeam   = "testteam"
	testAUD    = "test-aud-tag"
	testIssuer = "https://" + testTeam + ".cloudflareaccess.com"
)

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

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

// recordingHandler stands in for the protected origin handler: it records
// whether it ran and which identity values the middleware injected into the
// request context.
type recordingHandler struct {
	called   bool
	username any
	email    any
}

func (h *recordingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.called = true
	h.username = r.Context().Value(pkgutils.CtxKeyUsername)
	h.email = r.Context().Value(pkgutils.CtxKeyEmail)
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, "origin-ok")
}

func newMiddleware(origin http.Handler) *cloudflare.WithCloudflareJWTValidate {
	return &cloudflare.WithCloudflareJWTValidate{
		CloudflareTeamName: testTeam,
		CloudflareAUD:      testAUD,
		Origin:             origin,
	}
}

// newTestRSAKey generates an RSA key pair and returns it together with the
// JWKS document (containing only the public key) that the fake Cloudflare
// certs endpoint will serve.
func newTestRSAKey(t *testing.T) (key *rsa.PrivateKey, kid, jwksJSON string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	kid = "test-kid"
	jwksJSON = fmt.Sprintf(`{"keys":[{"kty":"RSA","use":"sig","alg":"RS256","kid":%q,"n":%q,"e":%q}]}`,
		kid,
		base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
	)
	return key, kid, jwksJSON
}

// signToken creates an RS256-signed JWT with the given claims, tagged with kid
// so the verifier can match it against the JWKS.
func signToken(t *testing.T, key *rsa.PrivateKey, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = kid
	raw, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return raw
}

// jwksHTTPClient returns an http.Client that answers the Cloudflare certs
// endpoint with jwksJSON and fails every other request, so tests can never
// reach the real network. It is injected via oidc.ClientContext, which the
// middleware's remote key set honors (the key set is built with the request
// context and fetches keys through the context's http client).
func jwksHTTPClient(jwksJSON string) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == testTeam+".cloudflareaccess.com" && req.URL.Path == "/cdn-cgi/access/certs" {
			return cannedResponse(req, http.StatusOK, jwksJSON), nil
		}
		return nil, fmt.Errorf("unexpected outbound request in test: %s", req.URL)
	})}
}

// newAuthRequest builds a request carrying the given header/cookie tokens and
// steers any JWKS fetch to the injected client. An empty token is omitted.
func newAuthRequest(t *testing.T, client *http.Client, headerToken, cookieToken string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/protected", nil)
	if headerToken != "" {
		r.Header.Set(cloudflare.CF_JWT_HEADER, headerToken)
	}
	if cookieToken != "" {
		r.AddCookie(&http.Cookie{Name: cloudflare.CF_AUTH_COOKIE, Value: cookieToken})
	}
	if client != nil {
		r = r.WithContext(oidc.ClientContext(r.Context(), client))
	}
	return r
}

// validClaims returns a claim set that passes verification: issuer matches
// the team domain, audience matches CloudflareAUD, and exp is in the future.
func validClaims() jwt.MapClaims {
	now := time.Now()
	return jwt.MapClaims{
		"iss":                testIssuer,
		"aud":                testAUD,
		"sub":                "user-sub-1",
		"iat":                now.Unix(),
		"exp":                now.Add(time.Hour).Unix(),
		"email":              "user@example.com",
		"name":               "Octo Cat",
		"preferred_username": "octo",
	}
}

// TestTokenExtraction covers getCFJWT through the exported surface: a valid
// token presented in either the header or the cookie reaches the origin;
// when both are present the header wins (a garbage cookie must not matter);
// with neither, the request is rejected before any verification.
func TestTokenExtraction(t *testing.T) {
	key, kid, jwks := newTestRSAKey(t)
	validToken := signToken(t, key, kid, validClaims())

	tests := []struct {
		name           string
		headerToken    string
		cookieToken    string
		wantStatus     int
		wantOriginCall bool
		wantBodySubstr string
	}{
		{
			name:           "valid token in header",
			headerToken:    validToken,
			wantStatus:     http.StatusOK,
			wantOriginCall: true,
		},
		{
			name:           "valid token in cookie",
			cookieToken:    validToken,
			wantStatus:     http.StatusOK,
			wantOriginCall: true,
		},
		{
			name:           "header wins over cookie",
			headerToken:    validToken,
			cookieToken:    "garbage-cookie-token",
			wantStatus:     http.StatusOK,
			wantOriginCall: true,
		},
		{
			name:           "no token anywhere",
			wantStatus:     http.StatusUnauthorized,
			wantOriginCall: false,
			wantBodySubstr: "No token on the request",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			origin := &recordingHandler{}
			h := newMiddleware(origin)
			r := newAuthRequest(t, jwksHTTPClient(jwks), tc.headerToken, tc.cookieToken)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, r)

			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", rr.Code, tc.wantStatus, rr.Body.String())
			}
			if origin.called != tc.wantOriginCall {
				t.Errorf("origin called = %v, want %v", origin.called, tc.wantOriginCall)
			}
			if tc.wantBodySubstr != "" && !strings.Contains(rr.Body.String(), tc.wantBodySubstr) {
				t.Errorf("body = %q, want it to contain %q", rr.Body.String(), tc.wantBodySubstr)
			}
		})
	}
}

// TestServeHTTP_InvalidTokens covers the rejection branch: tokens that are
// malformed, expired, mis-issued, mis-addressed, or signed by an unknown key
// all fail verification with 401, and the origin handler never runs. The
// JWKS document keeps serving the legitimate key, so the "unknown key" case
// proves the signature check itself rejects the token.
func TestServeHTTP_InvalidTokens(t *testing.T) {
	key, kid, jwks := newTestRSAKey(t)

	tests := []struct {
		name  string
		token func(t *testing.T) string
	}{
		{
			name:  "malformed jwt",
			token: func(t *testing.T) string { return "not-a-jwt" },
		},
		{
			name: "expired token",
			token: func(t *testing.T) string {
				claims := validClaims()
				claims["iat"] = time.Now().Add(-2 * time.Hour).Unix()
				claims["exp"] = time.Now().Add(-time.Hour).Unix()
				return signToken(t, key, kid, claims)
			},
		},
		{
			name: "wrong audience",
			token: func(t *testing.T) string {
				claims := validClaims()
				claims["aud"] = "some-other-aud-tag"
				return signToken(t, key, kid, claims)
			},
		},
		{
			name: "wrong issuer",
			token: func(t *testing.T) string {
				claims := validClaims()
				claims["iss"] = "https://evil.example.com"
				return signToken(t, key, kid, claims)
			},
		},
		{
			name: "signed by unknown key",
			token: func(t *testing.T) string {
				otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
				if err != nil {
					t.Fatalf("generate rsa key: %v", err)
				}
				return signToken(t, otherKey, kid, validClaims())
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			origin := &recordingHandler{}
			h := newMiddleware(origin)
			r := newAuthRequest(t, jwksHTTPClient(jwks), tc.token(t), "")
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, r)

			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d (body %q)", rr.Code, http.StatusUnauthorized, rr.Body.String())
			}
			if body := rr.Body.String(); !strings.Contains(body, "Invalid token") {
				t.Errorf("body = %q, want it to contain %q", body, "Invalid token")
			}
			if origin.called {
				t.Error("origin handler must not run for rejected tokens")
			}
		})
	}
}

// TestServeHTTP_ValidTokenInjectsIdentity verifies that after a token passes
// verification, the origin handler runs with the Cloudflare identity claims
// (username and email) available in the request context.
func TestServeHTTP_ValidTokenInjectsIdentity(t *testing.T) {
	key, kid, jwks := newTestRSAKey(t)
	token := signToken(t, key, kid, validClaims())

	origin := &recordingHandler{}
	h := newMiddleware(origin)
	r := newAuthRequest(t, jwksHTTPClient(jwks), token, "")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %q)", rr.Code, http.StatusOK, rr.Body.String())
	}
	if !origin.called {
		t.Fatal("origin handler did not run")
	}
	if origin.username != "octo" {
		t.Errorf("ctx username = %v, want %q (preferred_username)", origin.username, "octo")
	}
	if origin.email != "user@example.com" {
		t.Errorf("ctx email = %v, want %q", origin.email, "user@example.com")
	}
}

// TestServeHTTP_UsernameFallback covers the username selection order
// (preferred_username, then name, then email) and the case where no
// username-ish claim exists at all.
func TestServeHTTP_UsernameFallback(t *testing.T) {
	key, kid, jwks := newTestRSAKey(t)

	tests := []struct {
		name         string
		mutate       func(claims jwt.MapClaims)
		wantUsername any // nil means the ctx key must be unset
		wantEmail    any
	}{
		{
			name:         "preferred_username wins over name and email",
			mutate:       func(claims jwt.MapClaims) {},
			wantUsername: "octo",
			wantEmail:    "user@example.com",
		},
		{
			name: "name is used when preferred_username is absent",
			mutate: func(claims jwt.MapClaims) {
				delete(claims, "preferred_username")
			},
			wantUsername: "Octo Cat",
			wantEmail:    "user@example.com",
		},
		{
			name: "email is used when preferred_username and name are absent",
			mutate: func(claims jwt.MapClaims) {
				delete(claims, "preferred_username")
				delete(claims, "name")
			},
			wantUsername: "user@example.com",
			wantEmail:    "user@example.com",
		},
		{
			name: "no identity claims leaves the context keys unset",
			mutate: func(claims jwt.MapClaims) {
				delete(claims, "preferred_username")
				delete(claims, "name")
				delete(claims, "email")
			},
			wantUsername: nil,
			wantEmail:    nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			claims := validClaims()
			tc.mutate(claims)
			token := signToken(t, key, kid, claims)

			origin := &recordingHandler{}
			h := newMiddleware(origin)
			r := newAuthRequest(t, jwksHTTPClient(jwks), token, "")
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, r)

			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d (body %q)", rr.Code, http.StatusOK, rr.Body.String())
			}
			if !origin.called {
				t.Fatal("origin handler did not run")
			}
			if origin.username != tc.wantUsername {
				t.Errorf("ctx username = %v, want %v", origin.username, tc.wantUsername)
			}
			if origin.email != tc.wantEmail {
				t.Errorf("ctx email = %v, want %v", origin.email, tc.wantEmail)
			}
		})
	}
}

// TestMustGetVerifier_MissingConfigPanics documents the must* accessors'
// behavior: building a verifier without a team name or AUD is a programming
// error and panics (via log.Panic) before any verification happens.
func TestMustGetVerifier_MissingConfigPanics(t *testing.T) {
	tests := []struct {
		name string
		team string
		aud  string
	}{
		{name: "missing AUD", team: testTeam, aud: ""},
		{name: "missing team name", team: "", aud: testAUD},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := &cloudflare.WithCloudflareJWTValidate{
				CloudflareTeamName: tc.team,
				CloudflareAUD:      tc.aud,
				Origin:             &recordingHandler{},
			}
			defer func() {
				if r := recover(); r == nil {
					t.Error("expected a panic, got none")
				}
			}()
			// Any non-empty token drives ServeHTTP into mustGetVerifier.
			r := newAuthRequest(t, jwksHTTPClient(`{"keys":[]}`), "some-token", "")
			h.ServeHTTP(httptest.NewRecorder(), r)
		})
	}
}
