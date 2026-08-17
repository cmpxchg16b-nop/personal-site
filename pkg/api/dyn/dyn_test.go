package dyn

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	pkgmodelsdyn "personal-site/pkg/models/dyn"
	pkgutils "personal-site/pkg/utils"
)

// stubProvider is a DynBlogDataProvider returning fixed data (or error).
type stubProvider struct {
	data *pkgmodelsdyn.DynBlogData
	err  error
}

func (s stubProvider) GetDynBlogData() (*pkgmodelsdyn.DynBlogData, error) {
	return s.data, s.err
}

var testData = &pkgmodelsdyn.DynBlogData{
	Posts: []pkgmodelsdyn.PostMetadata{
		{Id: "post-1", Href: "/posts/post-1", Title: "First Post", Description: "The first post.", LastModified: "2026-03-08", Creation: "2026-03-01", Tags: []string{"meta", "example"}},
	},
	Projects: []pkgmodelsdyn.Project{
		{Id: "p1", Name: "Project One", Description: "First.", URL: "https://example.com/one", Tech: []string{"Go", "Next.js"}},
	},
	AuthorContacts: []pkgmodelsdyn.AuthorContact{
		{Id: "c1", Kind: "email", Label: "you@example.com", URL: "mailto:you@example.com"},
	},
}

func TestDynamicBlogDataHandler_ServesPosts(t *testing.T) {
	h := NewDynamicBlogDataHandler(stubProvider{data: testData})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/dyn/posts", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type: got %q, want application/json", ct)
	}

	var got []pkgmodelsdyn.PostMetadata
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not a JSON array: %v", err)
	}
	if len(got) != 1 || got[0].Id != "post-1" || got[0].Creation != "2026-03-01" || got[0].Tags[1] != "example" {
		t.Fatalf("unexpected posts payload: %+v", got)
	}
}

func TestDynamicBlogDataHandler_ServesPostById(t *testing.T) {
	h := NewDynamicBlogDataHandler(stubProvider{data: testData})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/dyn/posts/post-1", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type: got %q, want application/json", ct)
	}

	var got pkgmodelsdyn.PostMetadata
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not a JSON object: %v", err)
	}
	if got.Id != "post-1" || got.Title != "First Post" || got.Creation != "2026-03-01" || len(got.Tags) != 2 {
		t.Fatalf("unexpected post payload: %+v", got)
	}
}

func TestDynamicBlogDataHandler_PostByIdUnknownId(t *testing.T) {
	h := NewDynamicBlogDataHandler(stubProvider{data: testData})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/dyn/posts/nope", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestDynamicBlogDataHandler_ServesProjects(t *testing.T) {
	h := NewDynamicBlogDataHandler(stubProvider{data: testData})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/dyn/projects", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type: got %q, want application/json", ct)
	}

	var got []pkgmodelsdyn.Project
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not a JSON array: %v", err)
	}
	if len(got) != 1 || got[0].Id != "p1" || got[0].Tech[1] != "Next.js" {
		t.Fatalf("unexpected projects payload: %+v", got)
	}
}

func TestDynamicBlogDataHandler_ServesAuthorContacts(t *testing.T) {
	h := NewDynamicBlogDataHandler(stubProvider{data: testData})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/dyn/authorcontacts", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusOK)
	}

	var got []pkgmodelsdyn.AuthorContact
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not a JSON array: %v", err)
	}
	if len(got) != 1 || got[0].Id != "c1" || got[0].Kind != "email" {
		t.Fatalf("unexpected author contacts payload: %+v", got)
	}
}

func TestDynamicBlogDataHandler_NilProviderServesEmptyArrays(t *testing.T) {
	h := NewDynamicBlogDataHandler(nil)

	for _, path := range []string{"/api/dyn/posts", "/api/dyn/projects", "/api/dyn/authorcontacts"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: status: got %d, want %d", path, rec.Code, http.StatusOK)
		}
		if body := rec.Body.String(); body != "[]\n" {
			t.Fatalf("GET %s: body: got %q, want %q", path, body, "[]\n")
		}
	}

	// With no provider configured, every post id answers 404.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/dyn/posts/post-1", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /api/dyn/posts/post-1: status: got %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestDynamicBlogDataHandler_ProviderError(t *testing.T) {
	h := NewDynamicBlogDataHandler(stubProvider{err: errors.New("boom")})

	for _, path := range []string{"/api/dyn/projects", "/api/dyn/posts/post-1"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("GET %s: status: got %d, want %d", path, rec.Code, http.StatusInternalServerError)
		}
		var got pkgutils.ErrorResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("GET %s: response is not an error object: %v", path, err)
		}
		if got.Error != "boom" {
			t.Fatalf("GET %s: unexpected error payload: %+v", path, got)
		}
	}
}

func TestDynamicBlogDataHandler_RejectsNonGet(t *testing.T) {
	h := NewDynamicBlogDataHandler(stubProvider{data: testData})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/dyn/projects", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestDynamicBlogDataHandler_UnknownSubPath(t *testing.T) {
	h := NewDynamicBlogDataHandler(stubProvider{data: testData})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/dyn/nope", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestHealthzHandler(t *testing.T) {
	h := NewHealthzHandler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not a JSON object: %v", err)
	}
	if got["status"] != "ok" {
		t.Fatalf("unexpected payload: %+v", got)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/healthz", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status: got %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}
