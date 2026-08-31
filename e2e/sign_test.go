package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	pkgauth "personal-site/pkg/auth"
)

// runSign invokes the server binary's sign subcommand with the throwaway
// JWT secret every e2e server runs with (see startServerOnAddr) and returns
// the issued token (the command's stdout).
func runSign(t *testing.T, args ...string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(serverBin, append([]string{"sign"}, args...)...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = append(os.Environ(), "JWT_SECRET=personal-site-e2e-secret")
	if err := cmd.Run(); err != nil {
		t.Fatalf("sign %v: %v\nstderr: %s", args, err, stderr.String())
	}
	return strings.TrimSpace(stdout.String())
}

// parseSignedToken validates an issued token against the e2e secret and
// returns its claims, failing the test on any rejection.
func parseSignedToken(t *testing.T, token string) (*jwt.RegisteredClaims, *pkgauth.CustomClaimType) {
	t.Helper()
	validator := pkgauth.NewStaticKeyJWTValidator(
		pkgauth.NewStaticSecretProvider([]byte("personal-site-e2e-secret")),
		pkgauth.NewNullBlackListProvider(), false)
	registered, customAny, err := validator.ParseToken(context.Background(), token)
	if err != nil {
		t.Fatalf("the issued token does not validate: %v", err)
	}
	custom, ok := customAny.(*pkgauth.CustomClaimType)
	if !ok {
		t.Fatalf("custom claims = %T, want *auth.CustomClaimType", customAny)
	}
	return registered, custom
}

// TestSignClaims covers the issued claim set: the requested sub/jti/username,
// the session audience, and the default validity (720h) — plus a fresh
// random jti per invocation when none is given.
func TestSignClaims(t *testing.T) {
	token := runSign(t, "--sub", "bot:e2e-sign", "--jti", "e2e-sign-session", "--username", "e2e-signed")
	registered, custom := parseSignedToken(t, token)
	if registered.Subject != "bot:e2e-sign" {
		t.Errorf("sub = %q, want %q", registered.Subject, "bot:e2e-sign")
	}
	if registered.ID != "e2e-sign-session" {
		t.Errorf("jti = %q, want %q", registered.ID, "e2e-sign-session")
	}
	if len(registered.Audience) != 1 || registered.Audience[0] != pkgauth.AudSession {
		t.Errorf("aud = %v, want [%q]", registered.Audience, pkgauth.AudSession)
	}
	if custom.Username != "e2e-signed" {
		t.Errorf("username = %q, want %q", custom.Username, "e2e-signed")
	}
	if registered.ExpiresAt == nil {
		t.Fatal("exp is missing, want the default validity of 720h")
	}
	if ttl := time.Until(registered.ExpiresAt.Time); ttl <= 719*time.Hour || ttl > 720*time.Hour {
		t.Errorf("time to expiry = %s, want within (719h, 720h]", ttl)
	}

	// Without --jti each invocation gets a fresh session id.
	a, _ := parseSignedToken(t, runSign(t, "--sub", "bot:e2e-sign"))
	b, _ := parseSignedToken(t, runSign(t, "--sub", "bot:e2e-sign"))
	if a.ID == "" || a.ID == b.ID {
		t.Errorf("default jti = %q then %q, want two fresh distinct ids", a.ID, b.ID)
	}
}

// TestSignNoExpiry covers --validity 0: the token carries no exp claim and
// still validates (ParseToken would reject it otherwise).
func TestSignNoExpiry(t *testing.T) {
	registered, _ := parseSignedToken(t, runSign(t, "--sub", "bot:e2e-sign", "--validity", "0"))
	if registered.ExpiresAt != nil {
		t.Errorf("exp = %v, want no expiry claim with --validity 0", registered.ExpiresAt)
	}
}

// TestSignRequiresSubject covers the CLI surface: sub is the identity and
// kong rejects the invocation without it.
func TestSignRequiresSubject(t *testing.T) {
	cmd := exec.Command(serverBin, "sign")
	cmd.Env = append(os.Environ(), "JWT_SECRET=personal-site-e2e-secret")
	if err := cmd.Run(); err == nil {
		t.Fatal("sign without --sub succeeded, want a usage error")
	}
}

// TestSignTokenAcceptedByServer closes the loop against the real server: a
// token issued by the subcommand authenticates a request to the JWT-protected
// /api/profile endpoint, and the profile reflects the issued identity.
func TestSignTokenAcceptedByServer(t *testing.T) {
	baseURL := startServer(t)
	token := runSign(t, "--sub", "bot:e2e-sign", "--jti", "e2e-sign-profile-session", "--username", "e2e-signed")

	req, err := http.NewRequest(http.MethodGet, baseURL+"/api/profile", nil)
	if err != nil {
		t.Fatalf("build the profile request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /api/profile: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/profile = %s, want 200 OK", resp.Status)
	}
	var profile struct {
		SessionID string `json:"session_id"`
		SubjectID string `json:"subject_id"`
		Username  string `json:"username"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&profile); err != nil {
		t.Fatalf("decode the profile: %v", err)
	}
	if profile.SubjectID != "bot:e2e-sign" {
		t.Errorf("subject_id = %q, want %q", profile.SubjectID, "bot:e2e-sign")
	}
	if profile.SessionID != "e2e-sign-profile-session" {
		t.Errorf("session_id = %q, want %q", profile.SessionID, "e2e-sign-profile-session")
	}
	if profile.Username != "e2e-signed" {
		t.Errorf("username = %q, want %q", profile.Username, "e2e-signed")
	}
}
