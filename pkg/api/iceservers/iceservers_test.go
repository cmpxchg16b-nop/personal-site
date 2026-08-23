package iceservers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestParseURLs(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"stun:a:3478", []string{"stun:a:3478"}},
		{"stun:a:3478,turn:b:3478", []string{"stun:a:3478", "turn:b:3478"}},
		{" stun:a:3478 , , turn:b:3478 ", []string{"stun:a:3478", "turn:b:3478"}},
	} {
		if got := ParseURLs(tc.in); !slices.Equal(got, tc.want) {
			t.Errorf("ParseURLs(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func testHandler() *IceServersHandler {
	return NewIceServersHandler([]IceServerEntry{
		// No allowedOrigin: matches every request origin.
		{URLs: []string{"stun:stun.global.example:3478"}},
		// Two entries share an origin: their URLs are concatenated, and
		// the duplicate global URL is dropped.
		{URLs: []string{"stun:stun.global.example:3478", "stun:stun.a.example:3478"}, AllowedOrigin: "http://a.example"},
		{URLs: []string{"stun:stun.b.example:3478"}, AllowedOrigin: "http://b.example"},
	})
}

func getURLs(t *testing.T, h *IceServersHandler, r *http.Request) []string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body responseBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return body.URLs
}

func TestIceServersHandler(t *testing.T) {
	h := testHandler()

	for _, tc := range []struct {
		name string
		req  *http.Request
		want []string
	}{
		// The Origin header drives the match.
		{"origin a", httptest.NewRequest(http.MethodGet, "http://a.example/api/iceServers", nil),
			[]string{"stun:stun.global.example:3478", "stun:stun.a.example:3478"}},
		{"origin b", httptest.NewRequest(http.MethodGet, "http://b.example/api/iceServers", nil),
			[]string{"stun:stun.global.example:3478", "stun:stun.b.example:3478"}},
		// An unknown origin gets only the unrestricted entry.
		{"unknown origin", httptest.NewRequest(http.MethodGet, "http://stranger.example/api/iceServers", nil),
			[]string{"stun:stun.global.example:3478"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := getURLs(t, h, tc.req); !slices.Equal(got, tc.want) {
				t.Errorf("urls = %v, want %v", got, tc.want)
			}
		})
	}

	// httptest.NewRequest sets no Origin header; set one explicitly: the
	// browser sends it on cross-site requests.
	r := httptest.NewRequest(http.MethodGet, "http://server.example/api/iceServers", nil)
	r.Header.Set("Origin", "http://a.example")
	if got, want := getURLs(t, h, r), []string{"stun:stun.global.example:3478", "stun:stun.a.example:3478"}; !slices.Equal(got, want) {
		t.Errorf("Origin-header request: urls = %v, want %v", got, want)
	}

	// Only GET is allowed.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/iceServers", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

// TestIceServersHandlerEmpty checks that an unconfigured handler answers
// an empty JSON array, never null.
func TestIceServersHandlerEmpty(t *testing.T) {
	h := NewIceServersHandler(nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/iceServers", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := string(body["urls"]); got != "[]" {
		t.Errorf("urls = %s, want []", got)
	}
}
