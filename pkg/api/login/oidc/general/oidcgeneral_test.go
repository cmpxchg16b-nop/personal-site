package general_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	pkgapicommon "personal-site/pkg/api/common"
	"personal-site/pkg/api/login/oidc/general"
	pkgauth "personal-site/pkg/auth"
	pkgcookie "personal-site/pkg/cookie"

	"github.com/golang-jwt/jwt/v5"
)

const (
	testJWTSecret   = "test-jwt-secret"
	testJWTIssuer   = "test-issuer"
	testRedirectURI = "http://localhost:8080/api/login/oidc/testprov/auth"
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

// fakeIdP is an httptest server playing a complete OIDC provider: discovery
// document, JWKS, token endpoint, userinfo endpoint and revocation endpoint.
// Because oidc.NewProvider works against any issuer URL, the handler under
// test talks to this server for the whole flow with plain http.DefaultClient.
type fakeIdP struct {
	t   *testing.T
	srv *httptest.Server
	key *rsa.PrivateKey
	kid string

	// Behavior knobs (set before the request that triggers them).
	tokenStatus   int           // non-200 forces an exchange error with tokenErrBody
	tokenErrBody  string
	omitIDToken   bool          // token response carries no id_token
	signingKey    *rsa.PrivateKey // defaults to key; swap to simulate an unknown signer
	idTokenClaims jwt.MapClaims // defaults to defaultIDTokenClaims()
	userinfoJSON  string
	noUserinfo    bool // discovery document omits the userinfo endpoint

	// Captures.
	tokenReq struct {
		method       string
		contentType  string
		clientID     string
		clientSecret string
		form         url.Values
	}
	userinfoAuth string
	revokeForm   url.Values
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	p := &fakeIdP{t: t, key: key, kid: "test-kid", userinfoJSON: `{"sub":"user-123"}`}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", p.handleDiscovery)
	mux.HandleFunc("/token", p.handleToken)
	mux.HandleFunc("/keys", p.handleKeys)
	mux.HandleFunc("/userinfo", p.handleUserInfo)
	mux.HandleFunc("/revoke", p.handleRevoke)
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func (p *fakeIdP) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	doc := map[string]any{
		"issuer":                 p.srv.URL,
		"authorization_endpoint": p.srv.URL + "/authorize",
		"token_endpoint":         p.srv.URL + "/token",
		"jwks_uri":               p.srv.URL + "/keys",
		"revocation_endpoint":    p.srv.URL + "/revoke",
	}
	if !p.noUserinfo {
		doc["userinfo_endpoint"] = p.srv.URL + "/userinfo"
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(doc)
}

// defaultIDTokenClaims returns a claim set that passes the handler's ID token
// verification: issuer is the fake IdP URL, audience the client id, exp in
// the future, plus a full identity.
func (p *fakeIdP) defaultIDTokenClaims() jwt.MapClaims {
	now := time.Now()
	return jwt.MapClaims{
		"iss":                p.srv.URL,
		"aud":                "test-client-id",
		"sub":                "user-123",
		"iat":                now.Unix(),
		"exp":                now.Add(time.Hour).Unix(),
		"preferred_username": "octo",
		"email":              "octo@example.com",
		"name":               "Octo Cat",
	}
}

func (p *fakeIdP) signIDToken(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	signer := p.signingKey
	if signer == nil {
		signer = p.key
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = p.kid
	raw, err := tok.SignedString(signer)
	if err != nil {
		t.Fatalf("sign id token: %v", err)
	}
	return raw
}

func (p *fakeIdP) handleToken(w http.ResponseWriter, r *http.Request) {
	p.tokenReq.method = r.Method
	p.tokenReq.contentType = r.Header.Get("Content-Type")
	id, secret, _ := r.BasicAuth()
	p.tokenReq.clientID, p.tokenReq.clientSecret = id, secret
	if err := r.ParseForm(); err != nil {
		p.t.Errorf("token endpoint: cannot parse form: %v", err)
	}
	p.tokenReq.form = r.Form

	w.Header().Set("Content-Type", "application/json")
	if p.tokenStatus != 0 && p.tokenStatus != http.StatusOK {
		w.WriteHeader(p.tokenStatus)
		io.WriteString(w, p.tokenErrBody)
		return
	}
	resp := map[string]any{
		"access_token": "oidc-access-token",
		"token_type":   "Bearer",
		"expires_in":   3600,
	}
	if !p.omitIDToken {
		claims := p.idTokenClaims
		if claims == nil {
			claims = p.defaultIDTokenClaims()
		}
		resp["id_token"] = p.signIDToken(p.t, claims)
	}
	json.NewEncoder(w).Encode(resp)
}

func (p *fakeIdP) handleKeys(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"keys":[{"kty":"RSA","use":"sig","alg":"RS256","kid":%q,"n":%q,"e":%q}]}`,
		p.kid,
		base64.RawURLEncoding.EncodeToString(p.key.N.Bytes()),
		base64.RawURLEncoding.EncodeToString(big.NewInt(int64(p.key.E)).Bytes()),
	)
}

func (p *fakeIdP) handleUserInfo(w http.ResponseWriter, r *http.Request) {
	p.userinfoAuth = r.Header.Get("Authorization")
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, p.userinfoJSON)
}

func (p *fakeIdP) handleRevoke(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		p.t.Errorf("revoke endpoint: cannot read body: %v", err)
	}
	p.revokeForm, _ = url.ParseQuery(string(bodyBytes))
	w.WriteHeader(http.StatusOK)
}

// newTestHandler builds a handler wired with a real JWT issuer (signed with a
// static test secret) and the real cookie builder, against the fake IdP; only
// the nonce issuer is faked so tests can script state outcomes.
func newTestHandler(idp *fakeIdP, providerName string, nonceIssuer pkgauth.NonceIssuer) *general.GenericOIDCLoginHandler {
	keyProvider := pkgauth.NewStaticSecretProvider([]byte(testJWTSecret))
	return general.NewGenericOIDCLoginHandler(
		24*time.Hour,
		providerName,
		idp.srv.URL,
		"test-client-id",
		"test-client-secret",
		testRedirectURI,
		"", // default scope ("openid profile email")
		"/welcome",
		nil, // no allowed origins; tests use an absolute redirect URI
		pkgauth.NewStaticKeyJWTIssuer(keyProvider, testJWTIssuer),
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

// doAuthRequest drives /auth with a matching state cookie and code.
func doAuthRequest(h *general.GenericOIDCLoginHandler) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/api/login/oidc/testprov/auth?state=state-1&code=code-1", nil)
	r.AddCookie(&http.Cookie{Name: pkgapicommon.DefaultNonceCookieKey, Value: "state-1"})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	return rr
}

// TestServeHTTP_NotFound covers the fallthrough branch: paths ending in
// neither /start nor /auth get a 404 with a JSON error body.
func TestServeHTTP_NotFound(t *testing.T) {
	h := newTestHandler(newFakeIdP(t), "testprov", &stubNonceIssuer{})

	for _, path := range []string{
		"/api/login/oidc/testprov",
		"/api/login/oidc/testprov/bogus",
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

// TestHandleStart exercises the /start endpoint: state cookie, redirect to
// the discovered authorize URL with the expected query params, and the
// failure paths (discovery error, state issue error).
func TestHandleStart(t *testing.T) {
	t.Run("redirects to discovered authorize URL with state cookie and default scope", func(t *testing.T) {
		idp := newFakeIdP(t)
		h := newTestHandler(idp, "testprov", &stubNonceIssuer{nonce: "state-123"})

		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/login/oidc/testprov/start", nil))

		if rr.Code != http.StatusTemporaryRedirect {
			t.Fatalf("status = %d, want %d (body %q)", rr.Code, http.StatusTemporaryRedirect, rr.Body.String())
		}
		if c := findCookie(rr, pkgapicommon.DefaultNonceCookieKey); c == nil {
			t.Fatal("state cookie is not set")
		} else if c.Value != "state-123" {
			t.Errorf("state cookie = %q, want %q", c.Value, "state-123")
		}

		u, err := url.Parse(rr.Header().Get("Location"))
		if err != nil {
			t.Fatalf("Location %q does not parse: %v", rr.Header().Get("Location"), err)
		}
		idpURL, _ := url.Parse(idp.srv.URL)
		if u.Host != idpURL.Host || u.Path != "/authorize" {
			t.Errorf("redirect = %q, want %s/authorize", u, idp.srv.URL)
		}
		q := u.Query()
		for k, want := range map[string]string{
			"client_id":     "test-client-id",
			"redirect_uri":  testRedirectURI,
			"response_type": "code",
			"scope":         "openid profile email",
			"state":         "state-123",
		} {
			if got := q.Get(k); got != want {
				t.Errorf("redirect query %s = %q, want %q", k, got, want)
			}
		}
	})

	t.Run("custom scope is honored", func(t *testing.T) {
		idp := newFakeIdP(t)
		h := newTestHandler(idp, "testprov", &stubNonceIssuer{nonce: "state-abc"})
		h.Scope = "openid groups"

		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/login/oidc/testprov/start", nil))

		if rr.Code != http.StatusTemporaryRedirect {
			t.Fatalf("status = %d, want %d (body %q)", rr.Code, http.StatusTemporaryRedirect, rr.Body.String())
		}
		u, err := url.Parse(rr.Header().Get("Location"))
		if err != nil {
			t.Fatalf("Location %q does not parse: %v", rr.Header().Get("Location"), err)
		}
		if got := u.Query().Get("scope"); got != "openid groups" {
			t.Errorf("scope = %q, want %q", got, "openid groups")
		}
	})

	t.Run("state issue failure responds 500", func(t *testing.T) {
		idp := newFakeIdP(t)
		h := newTestHandler(idp, "testprov", &stubNonceIssuer{issueErr: errors.New("signing key unavailable")})

		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/login/oidc/testprov/start", nil))

		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want %d (body %q)", rr.Code, http.StatusInternalServerError, rr.Body.String())
		}
		if body := rr.Body.String(); !strings.Contains(body, "Failed to issue state") {
			t.Errorf("body = %q, want it to mention 'Failed to issue state'", body)
		}
	})

	t.Run("redirect_if_succeed param is stashed as a cookie", func(t *testing.T) {
		idp := newFakeIdP(t)
		h := newTestHandler(idp, "testprov", &stubNonceIssuer{nonce: "state-1"})

		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/login/oidc/testprov/start?redirect_if_succeed=/posts/x", nil))

		if rr.Code != http.StatusTemporaryRedirect {
			t.Fatalf("status = %d, want %d (body %q)", rr.Code, http.StatusTemporaryRedirect, rr.Body.String())
		}
		c := findCookie(rr, pkgapicommon.DefaultRedirectIfSucceedCookieKey)
			if c == nil {
				t.Fatal("redirect_if_succeed cookie is not set")
			}
			if c.Value != "/posts/x" {
				t.Errorf("redirect_if_succeed cookie = %q, want %q", c.Value, "/posts/x")
			}
	})

	t.Run("redirect_if_succeed outside the allowed origins is rejected", func(t *testing.T) {
		idp := newFakeIdP(t)
		h := newTestHandler(idp, "testprov", &stubNonceIssuer{nonce: "state-1"})

		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/login/oidc/testprov/start?redirect_if_succeed=https%3A%2F%2Fevil.example.com%2F", nil))

		if rr.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want %d (body %q)", rr.Code, http.StatusForbidden, rr.Body.String())
		}
		if body := rr.Body.String(); !strings.Contains(body, "is not in the allowed origins") {
			t.Errorf("body = %q, want it to mention 'is not in the allowed origins'", body)
		}
		if c := findCookie(rr, pkgapicommon.DefaultRedirectIfSucceedCookieKey); c != nil {
			t.Errorf("redirect_if_succeed cookie must not be set, got %q", c.Value)
		}
	})

	t.Run("discovery failure responds 500", func(t *testing.T) {
		// Point the handler at an issuer URL where nothing listens: provider
		// discovery fails before any state work.
		keyProvider := pkgauth.NewStaticSecretProvider([]byte(testJWTSecret))
		h := general.NewGenericOIDCLoginHandler(
			24*time.Hour, "testprov", "http://127.0.0.1:1",
			"test-client-id", "test-client-secret", testRedirectURI, "", "/welcome", nil,
			pkgauth.NewStaticKeyJWTIssuer(keyProvider, testJWTIssuer),
			&stubNonceIssuer{nonce: "state-1"},
			&pkgcookie.SimpleCookieBuilder{},
		)

		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/login/oidc/testprov/start", nil))

		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want %d (body %q)", rr.Code, http.StatusInternalServerError, rr.Body.String())
		}
		if body := rr.Body.String(); !strings.Contains(body, "Failed to fetch OIDC provider configuration") {
			t.Errorf("body = %q, want it to mention 'Failed to fetch OIDC provider configuration'", body)
		}
	})
}

// TestHandleAuthorizationCode_ErrorCases walks the /auth guard branches in
// order: oauth error param, state cookie presence, cookie/state match, state
// validity, and authorization code presence. All of these short-circuit
// before the token exchange, so the fake IdP only serves discovery.
func TestHandleAuthorizationCode_ErrorCases(t *testing.T) {
	idp := newFakeIdP(t)
	stateCookie := func(value string) *http.Cookie {
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
			target:         "/api/login/oidc/testprov/auth?error=access_denied&error_description=User+denied+access",
			wantStatus:     http.StatusBadRequest,
			wantBodySubstr: "access_denied: User denied access",
		},
		{
			name:           "missing state cookie responds 400",
			target:         "/api/login/oidc/testprov/auth?state=state-1&code=code-1",
			wantStatus:     http.StatusBadRequest,
			wantBodySubstr: "State not found in cookies",
		},
		{
			name:           "state param not matching the cookie responds 401",
			target:         "/api/login/oidc/testprov/auth?state=state-1&code=code-1",
			cookie:         stateCookie("other-state"),
			wantStatus:     http.StatusUnauthorized,
			wantBodySubstr: "does not match",
		},
		{
			name:   "state validation error responds 401",
			target: "/api/login/oidc/testprov/auth?state=state-1&code=code-1",
			cookie: stateCookie("state-1"),
			validateFunc: func(string) (bool, error) {
				return false, errors.New("nonce store unavailable")
			},
			wantStatus:     http.StatusUnauthorized,
			wantBodySubstr: "Invalid state",
		},
		{
			name:   "rejected state responds 401",
			target: "/api/login/oidc/testprov/auth?state=state-1&code=code-1",
			cookie: stateCookie("state-1"),
			validateFunc: func(string) (bool, error) {
				return false, nil
			},
			wantStatus:     http.StatusUnauthorized,
			wantBodySubstr: "Invalid state",
		},
		{
			name:           "missing authorization code responds 401",
			target:         "/api/login/oidc/testprov/auth?state=state-1",
			cookie:         stateCookie("state-1"),
			wantStatus:     http.StatusUnauthorized,
			wantBodySubstr: "No authorization code",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// A fresh handler per case: provider discovery is cached per
			// handler instance via sync.Once.
			h := newTestHandler(idp, "testprov", &stubNonceIssuer{validateFunc: tc.validateFunc})

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

// TestHandleAuthorizationCode_TokenFailures covers the branches after the
// state checks: exchange errors, a missing id_token, an id_token that fails
// verification, and an id_token without a sub claim.
func TestHandleAuthorizationCode_TokenFailures(t *testing.T) {
	tests := []struct {
		name           string
		configure      func(idp *fakeIdP)
		wantBodySubstr string
	}{
		{
			name: "exchange error is reported as 401",
			configure: func(idp *fakeIdP) {
				idp.tokenStatus = http.StatusBadRequest
				idp.tokenErrBody = `{"error":"invalid_grant","error_description":"The code is bad"}`
			},
			wantBodySubstr: "Failed to exchange token",
		},
		{
			name: "missing id token in token response responds 401",
			configure: func(idp *fakeIdP) {
				idp.omitIDToken = true
			},
			wantBodySubstr: "No ID token in token response",
		},
		{
			name: "id token signed by unknown key responds 401",
			configure: func(idp *fakeIdP) {
				otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
				if err != nil {
					t.Fatalf("generate rsa key: %v", err)
				}
				idp.signingKey = otherKey
			},
			wantBodySubstr: "ID token verification failed",
		},
		{
			name: "id token without sub claim responds 401",
			configure: func(idp *fakeIdP) {
				claims := idp.defaultIDTokenClaims()
				delete(claims, "sub")
				idp.idTokenClaims = claims
			},
			wantBodySubstr: "Failed to get user ID (sub claim)",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			idp := newFakeIdP(t)
			tc.configure(idp)
			h := newTestHandler(idp, "testprov", &stubNonceIssuer{}) // accepts any state

			rr := doAuthRequest(h)

			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d (body %q)", rr.Code, http.StatusUnauthorized, rr.Body.String())
			}
			if body := rr.Body.String(); !strings.Contains(body, tc.wantBodySubstr) {
				t.Errorf("body = %q, want it to contain %q", body, tc.wantBodySubstr)
			}
		})
	}
}

// TestHandleAuthorizationCode_HappyPath runs the whole /auth flow against the
// fake IdP for several identity shapes. It verifies the exchange request, the
// state cookie clearing, the redirect target, the session JWT claims, the
// userinfo call, and the access token revocation.
func TestHandleAuthorizationCode_HappyPath(t *testing.T) {
	tests := []struct {
		name          string
		providerName  string
		configure     func(idp *fakeIdP)
		wantSubject   string
		wantUsername  string
		wantEmail     string
	}{
		{
			name:         "full identity from id token",
			providerName: "testprov",
			configure:    func(idp *fakeIdP) {},
			wantSubject:  "oidc-testprov:user-123",
			wantUsername: "octo",
			wantEmail:    "octo@example.com",
		},
		{
			name:         "identity enriched from userinfo endpoint",
			providerName: "testprov",
			configure: func(idp *fakeIdP) {
				claims := idp.defaultIDTokenClaims()
				delete(claims, "preferred_username")
				delete(claims, "email")
				delete(claims, "name")
				idp.idTokenClaims = claims
				idp.userinfoJSON = `{"sub":"user-123","preferred_username":"info-octo","email":"info@example.com"}`
			},
			wantSubject:  "oidc-testprov:user-123",
			wantUsername: "info-octo",
			wantEmail:    "info@example.com",
		},
		{
			name:         "email becomes username when preferred_username is absent",
			providerName: "testprov",
			configure: func(idp *fakeIdP) {
				claims := idp.defaultIDTokenClaims()
				delete(claims, "preferred_username")
				delete(claims, "name")
				idp.idTokenClaims = claims
			},
			wantSubject:  "oidc-testprov:user-123",
			wantUsername: "octo@example.com",
			wantEmail:    "octo@example.com",
		},
		{
			name:         "name becomes username when preferred_username and email are absent",
			providerName: "testprov",
			configure: func(idp *fakeIdP) {
				claims := idp.defaultIDTokenClaims()
				delete(claims, "preferred_username")
				delete(claims, "email")
				idp.idTokenClaims = claims
			},
			wantSubject:  "oidc-testprov:user-123",
			wantUsername: "Octo Cat",
			wantEmail:    "",
		},
		{
			name:         "empty provider name defaults to oidc",
			providerName: "",
			configure:    func(idp *fakeIdP) {},
			wantSubject:  "oidc-oidc:user-123",
			wantUsername: "octo",
			wantEmail:    "octo@example.com",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			idp := newFakeIdP(t)
			tc.configure(idp)
			h := newTestHandler(idp, tc.providerName, &stubNonceIssuer{}) // accepts any state

			rr := doAuthRequest(h)

			if rr.Code != http.StatusTemporaryRedirect {
				t.Fatalf("status = %d, want %d (body %q)", rr.Code, http.StatusTemporaryRedirect, rr.Body.String())
			}
			if got := rr.Header().Get("Location"); got != "/welcome" {
				t.Errorf("Location = %q, want %q", got, "/welcome")
			}

			// The state cookie must be cleared once the state has been consumed.
			if c := findCookie(rr, pkgapicommon.DefaultNonceCookieKey); c == nil {
				t.Error("state cookie clearing header is not set")
			} else if c.Value != "" || c.MaxAge >= 0 {
				t.Errorf("state cookie = %q (MaxAge %d), want empty value with negative MaxAge", c.Value, c.MaxAge)
			}

			// The exchange must be a form POST carrying the authz code, the
			// grant type and the client credentials (via Basic auth).
			if idp.tokenReq.method != http.MethodPost {
				t.Errorf("token endpoint method = %q, want POST", idp.tokenReq.method)
			}
			if idp.tokenReq.contentType != "application/x-www-form-urlencoded" {
				t.Errorf("token endpoint Content-Type = %q, want application/x-www-form-urlencoded", idp.tokenReq.contentType)
			}
			if idp.tokenReq.clientID != "test-client-id" || idp.tokenReq.clientSecret != "test-client-secret" {
				t.Errorf("token endpoint basic auth = %q/%q, want test-client-id/test-client-secret", idp.tokenReq.clientID, idp.tokenReq.clientSecret)
			}
			for k, want := range map[string]string{
				"grant_type":   "authorization_code",
				"code":         "code-1",
				"redirect_uri": testRedirectURI,
			} {
				if got := idp.tokenReq.form.Get(k); got != want {
					t.Errorf("token endpoint form %s = %q, want %q", k, got, want)
				}
			}

			// The userinfo endpoint is exposed by the fake IdP, so it must
			// have been consulted with the access token.
			if got, want := idp.userinfoAuth, "Bearer oidc-access-token"; got != want {
				t.Errorf("userinfo Authorization = %q, want %q", got, want)
			}

			claims := parseIssuedClaims(t, rr)
			if got := claims.Subject; got != tc.wantSubject {
				t.Errorf("sub = %q, want %q", got, tc.wantSubject)
			}
			if got := claims.Username; got != tc.wantUsername {
				t.Errorf("username = %q, want %q", got, tc.wantUsername)
			}
			if got := claims.Email; got != tc.wantEmail {
				t.Errorf("email = %q, want %q", got, tc.wantEmail)
			}

			// The fake IdP advertises a revocation endpoint, so the handler
			// must have revoked the access token on its way out.
			if got := idp.revokeForm.Get("token"); got != "oidc-access-token" {
				t.Errorf("revoke form token = %q, want %q", got, "oidc-access-token")
			}
		})
	}
}

// TestGetMapClaims verifies the claim set that handleAuthorizationCode signs
// into the session JWT: subject, username/email passthrough, session
// audience, and an expiry one session-lifespan out.
func TestGetMapClaims(t *testing.T) {
	const lifespan = 12 * time.Hour
	h := newTestHandler(newFakeIdP(t), "testprov", &stubNonceIssuer{})
	h.SessionLifespan = lifespan

	before := time.Now()
	claims, err := h.GetMapClaims(
		httptest.NewRequest(http.MethodGet, "/api/login/oidc/testprov/auth", nil),
		"oidc-testprov:42", "octo", "octo@example.com",
	)
	if err != nil {
		t.Fatalf("GetMapClaims: %v", err)
	}
	after := time.Now()

	for k, want := range map[string]any{
		"sub":      "oidc-testprov:42",
		"username": "octo",
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

// TestGenericOIDCLoginHandler_RouteMounted mirrors the wiring in
// cmd/server/main.go (mux.Handle at /api/login/oidc/{provider} and at the
// subtree) to confirm /start requests reach the handler.
func TestGenericOIDCLoginHandler_RouteMounted(t *testing.T) {
	idp := newFakeIdP(t)
	h := newTestHandler(idp, "testprov", &stubNonceIssuer{nonce: "state-route"})

	mux := http.NewServeMux()
	mux.Handle("/api/login/oidc/testprov", h)
	mux.Handle("/api/login/oidc/testprov/", h)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/login/oidc/testprov/start", nil))
	if rr.Code != http.StatusTemporaryRedirect {
		t.Fatalf("/start: status = %d, want %d (body %q)", rr.Code, http.StatusTemporaryRedirect, rr.Body.String())
	}
	if loc := rr.Header().Get("Location"); !strings.Contains(loc, "/authorize?") {
		t.Errorf("/start: Location = %q, want the IdP authorize URL", loc)
	}

	// /auth with an oauth error param short-circuits with 400 — enough to
	// prove the subtree route reaches handleAuthorizationCode.
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/login/oidc/testprov/auth?error=access_denied", nil))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("/auth: status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}
