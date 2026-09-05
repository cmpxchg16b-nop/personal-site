package e2e

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// TestHomeLiveWHEPURL covers GET /api/homeLiveWHEPURL end to end: it needs
// no session (the path is on the JWT whitelist) and answers the url
// attribute of the configuration document's <homeLiveWHEPURL/> element —
// the WHEP endpoint the home page's Live section reads its stream from. A
// document without the element answers an empty url, the frontend's cue to
// show no Live section at all.
func TestHomeLiveWHEPURL(t *testing.T) {
	get := func(baseURL string) string {
		t.Helper()
		resp, err := http.Get(baseURL + "/api/homeLiveWHEPURL")
		if err != nil {
			t.Fatalf("GET /api/homeLiveWHEPURL: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /api/homeLiveWHEPURL: status: got %d, want %d",
				resp.StatusCode, http.StatusOK)
		}
		var body struct {
			URL string `json:"url"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("GET /api/homeLiveWHEPURL: decode response: %v", err)
		}
		return body.URL
	}

	writeCfg := func(content string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "serverConfig.xml")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write the configuration document: %v", err)
		}
		return path
	}

	const header = `<?xml version="1.0" encoding="UTF-8"?>
<serverConfig>
`
	t.Run("configured", func(t *testing.T) {
		cfgPath := writeCfg(header +
			`  <homeLiveWHEPURL url="http://localhost:8889/mystream/whep" />
</serverConfig>
`)
		baseURL := startServerWithArgs(t, "--config-xml", cfgPath)
		if got, want := get(baseURL), "http://localhost:8889/mystream/whep"; got != want {
			t.Errorf("url = %q, want %q", got, want)
		}
	})

	t.Run("unconfigured", func(t *testing.T) {
		cfgPath := writeCfg(header + `</serverConfig>
`)
		baseURL := startServerWithArgs(t, "--config-xml", cfgPath)
		if got := get(baseURL); got != "" {
			t.Errorf("url = %q, want the empty string", got)
		}
	})
}
