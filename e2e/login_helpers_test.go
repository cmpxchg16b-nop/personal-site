package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"
)

// loginAsVisitor hits the visitor login endpoint and extracts the JWT cookie
// value from the Set-Cookie header. It uses a client that does NOT follow the
// TemporaryRedirect so the cookie is captured directly from the login response.
func loginAsVisitor(t *testing.T, loginURL string) string {
	t.Helper()
	client := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // don't follow the redirect to "/"
		},
	}
	resp, err := client.Get(loginURL)
	if err != nil && resp == nil {
		t.Fatalf("visitor login request failed: %v", err)
	}
	if resp != nil {
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
	}
	if resp == nil || (resp.StatusCode < 200 || resp.StatusCode >= 400) {
		status := "(nil)"
		if resp != nil {
			status = fmt.Sprintf("%d", resp.StatusCode)
		}
		t.Fatalf("visitor login returned unexpected status %s", status)
	}
	for _, c := range resp.Cookies() {
		if c.Name == "jwt" {
			return c.Value
		}
	}
	return ""
}

// profileIdentity GETs /api/profile with the session cookie and returns
// the session's subject id, session id and username — the identity the
// JWT-backed endpoints (comments, signalling) stamp onto the caller's
// actions.
func profileIdentity(t *testing.T, baseURL, jwtCookie string) (subjectId, sessionId, username string) {
	t.Helper()
	body := cookieReq(t, http.DefaultClient, baseURL, http.MethodGet, "/api/profile", "", jwtCookie)
	var profile struct {
		SessionID string `json:"session_id"`
		SubjectID string `json:"subject_id"`
		Username  string `json:"username"`
	}
	if err := json.Unmarshal(body, &profile); err != nil {
		t.Fatalf("GET /api/profile: decode response: %v", err)
	}
	if profile.SubjectID == "" || profile.SessionID == "" {
		t.Fatalf("GET /api/profile: empty identity: %s", body)
	}
	return profile.SubjectID, profile.SessionID, profile.Username
}

// cookieReq performs an HTTP request carrying the visitor's JWT cookie.
func cookieReq(t *testing.T, client *http.Client, baseURL, method, path, body, jwtCookie string) []byte {
	t.Helper()
	var bodyReader io.Reader
	if body != "" {
		bodyReader = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequest(method, baseURL+path, bodyReader)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	req.AddCookie(&http.Cookie{Name: "jwt", Value: jwtCookie})
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if method == http.MethodDelete {
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("%s %s: status = %d, want %d (body %s)", method, path, resp.StatusCode, http.StatusNoContent, respBody)
		}
		return respBody
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Fatalf("%s %s: status = %d (body %s)", method, path, resp.StatusCode, respBody)
	}
	return respBody
}
