package personalsite

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pkgapiloginvisitor "personal-site/pkg/api/login/visitor"
	pkgauth "personal-site/pkg/auth"
	pkgcookie "personal-site/pkg/cookie"
	pkgsession "personal-site/pkg/session"
)

// TestE2E_StaticAssetsPathTraversal guards the /assets/ static file handler —
// wired exactly as in cmd/server/main.go (StripPrefix +
// http.FileServer(http.Dir)) behind the JWT auth middleware — against path
// escape attacks. Dot-dot segments, plain or percent-encoded, must never
// resolve to files outside the assets directory.
func TestE2E_StaticAssetsPathTraversal(t *testing.T) {
	// --- Seed a directory layout with a secret outside the assets root -----

	root := t.TempDir()
	assetsDir := filepath.Join(root, "assets")
	if err := os.Mkdir(assetsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(assetsDir, "public.txt"), []byte("PUBLIC"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "secret.txt"), []byte("SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}

	// --- Wire up the server (mirrors the /assets/ chain in main.go) --------

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sm := pkgsession.NewOnMemorySessionManager()

	mux := http.NewServeMux()
	mux.Handle("/assets/", http.StripPrefix("/assets/", http.FileServer(http.Dir(assetsDir))))

	jwtSecret := []byte("e2e-test-secret")
	keyProvider := pkgauth.NewStaticSecretProvider(jwtSecret)
	tokenIssuer := pkgauth.NewStaticKeyJWTIssuer(keyProvider, "e2e-issuer")
	tickIssuer := pkgauth.NewSharedTickingTicketGenerator(5 * time.Millisecond)
	tickIssuer.Run(ctx)
	visitorLoginHandler := pkgapiloginvisitor.NewVisitorLoginHandler(
		tokenIssuer,
		time.Hour,
		tickIssuer,
		&pkgcookie.SimpleCookieBuilder{},
	)
	mux.Handle("/api/login/visitor", visitorLoginHandler)

	jwtValidator := pkgauth.NewStaticKeyJWTValidator(keyProvider, pkgauth.NewNullBlackListProvider(), false)
	var h http.Handler = mux
	h = pkgsession.WithSessionId(h, sm)
	h = pkgauth.WithWhiteListJWTAuth(h, jwtValidator, []string{"/api/login", "/api/login/", "/api/logout"}, nil)

	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	jwtCookieValue := loginAsVisitor(t, ts.URL+"/api/login/visitor")
	if jwtCookieValue == "" {
		t.Fatal("visitor login did not set a jwt cookie")
	}

	// The client must not follow redirects: a traversal attempt answered with
	// the mux's canonical-path redirect has to be observed raw, and the
	// redirect target then checked manually.
	client := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	get := func(path string) (status int, location, body string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		req.AddCookie(&http.Cookie{Name: "jwt", Value: jwtCookieValue})
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Get("Location"), string(b)
	}

	t.Run("serves files inside the root", func(t *testing.T) {
		status, _, body := get("/assets/public.txt")
		if status != http.StatusOK || body != "PUBLIC" {
			t.Fatalf("GET /assets/public.txt: status = %d, body = %q; want 200 \"PUBLIC\"", status, body)
		}
	})

	t.Run("rejects unauthenticated requests", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, ts.URL+"/assets/public.txt", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET /assets/public.txt: %v", err)
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("GET /assets/public.txt without JWT: status = %d, want %d",
				resp.StatusCode, http.StatusUnauthorized)
		}
	})

	t.Run("dot-dot traversal is neutralized", func(t *testing.T) {
		// The ServeMux canonicalizes paths with literal ".." segments and
		// answers with a redirect to the cleaned path, which no longer matches
		// /assets/ and therefore 404s.
		for _, tc := range []struct{ path, wantLocation string }{
			{"/assets/../secret.txt", "/secret.txt"},
			{"/assets/../../secret.txt", "/secret.txt"},
		} {
			status, location, body := get(tc.path)
			if status != http.StatusMovedPermanently || location != tc.wantLocation {
				t.Errorf("GET %s: status = %d, Location = %q; want 301 to %q",
					tc.path, status, location, tc.wantLocation)
			}
			if strings.Contains(body, "SECRET") {
				t.Errorf("GET %s leaked the secret file", tc.path)
			}

			// Following the redirect must not reach the file either.
			status, _, body = get(location)
			if status != http.StatusNotFound || strings.Contains(body, "SECRET") {
				t.Errorf("GET %s (redirect target): status = %d, body = %q; want 404 without secret",
					location, status, body)
			}
		}
	})

	t.Run("encoded traversal is neutralized", func(t *testing.T) {
		// Percent-encoded dot/slash sequences are decoded into r.URL.Path
		// before routing; http.Dir then pins any ".." to the virtual root, so
		// these resolve under the assets dir and simply miss.
		for _, path := range []string{
			"/assets/%2e%2e/secret.txt",
			"/assets/%2e%2e%2fsecret.txt",
			"/assets/..%2fsecret.txt",
			"/assets/%2e%2e/%2e%2e/secret.txt",
		} {
			status, _, body := get(path)
			if status != http.StatusNotFound {
				t.Errorf("GET %s: status = %d, want %d", path, status, http.StatusNotFound)
			}
			if strings.Contains(body, "SECRET") {
				t.Errorf("GET %s leaked the secret file", path)
			}
		}
	})
}
