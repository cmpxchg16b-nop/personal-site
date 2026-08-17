package links

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	pkgmodelsshortlink "personal-site/pkg/models/shortlink"
	pkgutils "personal-site/pkg/utils"
)

// stubProvider is a ShortLinkDataProvider returning fixed destinations (or
// error).
type stubProvider struct {
	hrefs map[string]string
	err   error
}

func (s stubProvider) GetShortLinkById(_ context.Context, shortLinkId string) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	if href, ok := s.hrefs[shortLinkId]; ok {
		return href, nil
	}
	return "", fmt.Errorf("short link %q: %w", shortLinkId, pkgmodelsshortlink.ErrShortLinkNotFound)
}

var testHrefs = map[string]string{
	"gh":         "https://github.com/your-handle",
	"first-post": "/posts/your-first-post",
}

func TestShortLinkHandler_Redirects(t *testing.T) {
	h := NewShortLinkHandler(stubProvider{hrefs: testHrefs})

	for _, tc := range []struct {
		path       string
		wantLocation string
	}{
		{"/links/gh", "https://github.com/your-handle"},
		{"/links/first-post", "/posts/your-first-post"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))

		if rec.Code != http.StatusFound {
			t.Fatalf("GET %s: status: got %d, want %d", tc.path, rec.Code, http.StatusFound)
		}
		if loc := rec.Header().Get("Location"); loc != tc.wantLocation {
			t.Fatalf("GET %s: Location: got %q, want %q", tc.path, loc, tc.wantLocation)
		}
	}
}

func TestShortLinkHandler_UnknownId(t *testing.T) {
	h := NewShortLinkHandler(stubProvider{hrefs: testHrefs})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/links/nope", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestShortLinkHandler_NilProviderServesNotFound(t *testing.T) {
	h := NewShortLinkHandler(nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/links/gh", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestShortLinkHandler_ProviderError(t *testing.T) {
	h := NewShortLinkHandler(stubProvider{err: errors.New("boom")})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/links/gh", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	var got pkgutils.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not an error object: %v", err)
	}
	if got.Error != "boom" {
		t.Fatalf("unexpected error payload: %+v", got)
	}
}

func TestShortLinkHandler_RejectsNonGet(t *testing.T) {
	h := NewShortLinkHandler(stubProvider{hrefs: testHrefs})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/links/gh", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestShortLinkHandler_NoId(t *testing.T) {
	h := NewShortLinkHandler(stubProvider{hrefs: testHrefs})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/links/", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusNotFound)
	}
}
