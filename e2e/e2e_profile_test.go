package e2e

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	pkgapiloginvisitor "personal-site/pkg/api/login/visitor"
	pkgapiprofile "personal-site/pkg/api/profile"
	pkgauth "personal-site/pkg/auth"
	pkgcookie "personal-site/pkg/cookie"
	pkgsession "personal-site/pkg/session"
)

// TestE2E_VisitorProfile exercises the /api/profile endpoint through the full
// HTTP stack (JWT auth + session middleware) as a real visitor identity. The
// visitor logs in through /api/login/visitor and then reads back the session and
// subject ids that the auth middleware injected from its JWT claims.
func TestE2E_VisitorProfile(t *testing.T) {
	// --- Wire up a server that mirrors main.go, with /api/profile mounted ----

	jwtSecret := []byte("e2e-profile-secret")
	keyProvider := pkgauth.NewStaticSecretProvider(jwtSecret)
	tokenIssuer := pkgauth.NewStaticKeyJWTIssuer(keyProvider, "e2e-issuer")
	tickIssuer := pkgauth.NewSharedTickingTicketGenerator(5 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tickIssuer.Run(ctx)

	sm := pkgsession.NewOnMemorySessionManager()
	cookieBuilder := &pkgcookie.SimpleCookieBuilder{}
	visitorLoginHandler := pkgapiloginvisitor.NewVisitorLoginHandler(
		tokenIssuer, time.Hour, tickIssuer, nil, cookieBuilder,
	)

	mux := http.NewServeMux()
	mux.Handle("/api/login/visitor", visitorLoginHandler)
	mux.Handle("/api/profile", pkgapiprofile.NewProfileHandler(sm))

	jwtValidator := pkgauth.NewStaticKeyJWTValidator(keyProvider, pkgauth.NewNullBlackListProvider(), false)

	var h http.Handler = mux
	h = pkgsession.WithSessionId(h, sm)
	h = pkgauth.WithWhiteListJWTAuth(h, jwtValidator, []string{"/api/login", "/api/login/", "/api/logout"}, nil)

	ts := newTestServer(t, h)

	// --- Log in as a visitor (reuses the shared e2e helper) -----------------

	jwtCookieValue := loginAsVisitor(t, ts.URL+"/api/login/visitor")
	if jwtCookieValue == "" {
		t.Fatal("visitor login did not set a jwt cookie")
	}

	client := &http.Client{Timeout: 10 * time.Second}

	// --- GET /api/profile as a visitor returns the visitor's identity -------

	t.Run("authenticated visitor profile", func(t *testing.T) {
		body := cookieReq(t, client, ts.URL, http.MethodGet, "/api/profile", "", jwtCookieValue)

		var resp struct {
			SessionID string `json:"session_id"`
			SubjectID string `json:"subject_id"`
			Username  string `json:"username"`
			Email     string `json:"email"`
		}
		decodeOrFail(t, body, &resp)
		t.Logf("profile: session_id=%q subject_id=%q username=%q email=%q", resp.SessionID, resp.SubjectID, resp.Username, resp.Email)

		if resp.SessionID == "" {
			t.Errorf("session_id is empty; the auth middleware should populate it from the JWT jti")
		}
		if !strings.HasPrefix(resp.SubjectID, pkgauth.VisitorSubjectPrefix) {
			t.Errorf("subject_id = %q, want a %q-prefixed visitor id", resp.SubjectID, pkgauth.VisitorSubjectPrefix)
		}
		if !strings.HasPrefix(resp.Username, "visitor-") {
			t.Errorf("username = %q, want a %q-prefixed visitor name", resp.Username, "visitor-")
		}
		if resp.Email != "" {
			t.Errorf("email = %q, want empty for a visitor session (no email claim)", resp.Email)
		}
	})

	// --- GET /api/profile without a cookie is rejected by the auth layer ----

	t.Run("unauthenticated request is rejected", func(t *testing.T) {
		status := rawStatus(client, ts.URL, http.MethodGet, "/api/profile", "")
		if status != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401 (auth middleware should reject a cookieless request)", status)
		}
	})

	// --- GET /api/profile with a tampered cookie is rejected ----------------

	t.Run("tampered cookie is rejected", func(t *testing.T) {
		tampered := jwtCookieValue + "tampered-suffix"
		status := rawStatus(client, ts.URL, http.MethodGet, "/api/profile", tampered)
		if status != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401 for a tampered token", status)
		}
	})

	// --- POST /api/profile is rejected with 405 Allow: GET ------------------

	t.Run("non-GET method returns 405", func(t *testing.T) {
		status, allow := rawStatusWithHeader(client, ts.URL, http.MethodPost, "/api/profile", jwtCookieValue, "Allow")
		if status != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want 405 for POST", status)
		}
		if allow != http.MethodGet {
			t.Errorf("Allow = %q, want GET", allow)
		}
	})
}

// newTestServer wraps httptest.NewServer and registers cleanup.
func newTestServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts
}

// decodeOrFail unmarshals body into v, failing the test on error.
func decodeOrFail(t *testing.T, body []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("decode body %q: %v", body, err)
	}
}

// rawStatus performs a request (optionally carrying a cookie value) and returns
// just the HTTP status code, without failing on non-2xx. Used for the 401/405
// assertions where the shared cookieReq helper would otherwise fatal.
func rawStatus(client *http.Client, baseURL, method, path, cookie string) int {
	status, _ := rawStatusWithHeader(client, baseURL, method, path, cookie, "")
	return status
}

// rawStatusWithHeader is like rawStatus but also returns the value of a named
// response header (empty if headerName is "" or absent).
func rawStatusWithHeader(client *http.Client, baseURL, method, path, cookie, headerName string) (int, string) {
	req, err := http.NewRequest(method, baseURL+path, nil)
	if err != nil {
		return 0, ""
	}
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: "jwt", Value: cookie})
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, ""
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	headerVal := ""
	if headerName != "" {
		headerVal = resp.Header.Get(headerName)
	}
	return resp.StatusCode, headerVal
}
