package e2e

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestICEServers covers GET /api/iceServers end to end: it needs no
// session (the path is on the JWT whitelist), and it answers the
// concatenated, deduplicated URLs of the <iceServer/> entries matching
// the request's origin — entries without an allowedOrigin match every
// origin.
func TestICEServers(t *testing.T) {
	cfg := `<?xml version="1.0" encoding="UTF-8"?>
<serverConfig>
  <iceServer urls="stun:stun.global.example:3478" />
  <iceServer urls="stun:stun.global.example:3478,stun:stun.a.example:3478" allowedOrigin="http://a.example" />
  <iceServer urls="stun:stun.b.example:3478" allowedOrigin="http://b.example" />
</serverConfig>
`
	cfgPath := filepath.Join(t.TempDir(), "serverConfig.xml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write the configuration document: %v", err)
	}
	baseURL := startServerWithArgs(t, "--config-xml", cfgPath)

	// get ICE servers; origin sets the Origin header ("" sends none). No
	// session cookie anywhere: the endpoint is on the JWT whitelist.
	get := func(origin string) []string {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, baseURL+"/api/iceServers", nil)
		if err != nil {
			t.Fatalf("build GET /api/iceServers: %v", err)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET /api/iceServers: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /api/iceServers (origin %q): status: got %d, want %d",
				origin, resp.StatusCode, http.StatusOK)
		}
		var body struct {
			URLs []string `json:"urls"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("GET /api/iceServers: decode response: %v", err)
		}
		return body.URLs
	}

	// A matching origin gets the unrestricted entry's URLs plus its own,
	// concatenated and deduplicated.
	if got, want := get("http://a.example"),
		[]string{"stun:stun.global.example:3478", "stun:stun.a.example:3478"}; !slices.Equal(got, want) {
		t.Errorf("origin a.example: urls = %v, want %v", got, want)
	}
	if got, want := get("http://b.example"),
		[]string{"stun:stun.global.example:3478", "stun:stun.b.example:3478"}; !slices.Equal(got, want) {
		t.Errorf("origin b.example: urls = %v, want %v", got, want)
	}
	// An unknown origin, or no Origin header at all, gets only the
	// unrestricted entry.
	for _, origin := range []string{"http://stranger.example", ""} {
		if got, want := get(origin), []string{"stun:stun.global.example:3478"}; !slices.Equal(got, want) {
			t.Errorf("origin %q: urls = %v, want %v", origin, got, want)
		}
	}
}
