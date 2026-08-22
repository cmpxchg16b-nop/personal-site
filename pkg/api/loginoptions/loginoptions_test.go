package loginoptions

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestLoginOptionsHandler_ServesOptions(t *testing.T) {
	h := NewLoginOptionsHandler([]LoginOption{
		{Kind: "github", Name: "github", DisplayName: "Github", LoginURL: "/api/login/oauth2/github/start"},
		{Kind: "visitor", Name: "visitor", Label: "Sign in as Visitor", LoginURL: "/api/login/visitor"},
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/login/loginoptions", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type: got %q, want application/json", ct)
	}

	var got []LoginOption
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not a JSON array: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("options count: got %d, want 2", len(got))
	}
	if got[0].Name != "github" || got[1].Label != "Sign in as Visitor" {
		t.Fatalf("unexpected options payload: %+v", got)
	}
}

func TestLoginOptionsHandler_NilOptionsServedAsEmptyArray(t *testing.T) {
	h := NewLoginOptionsHandler(nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/login/loginoptions", nil))

	var got []LoginOption
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not a JSON array: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("nil options must be served as [], got %+v", got)
	}
}

func TestLoginOptionsHandler_FiltersByRequestOrigin(t *testing.T) {
	h := NewLoginOptionsHandler([]LoginOption{
		{Kind: "github", Name: "github", DisplayName: "Github", LoginURL: "/api/login/oauth2/github/start"},
		{
			Kind:           "kioubit",
			Name:           "kioubit",
			DisplayName:    "Kioubit",
			LoginURL:       "/api/login/oidc/kioubit/start",
			AllowedOrigins: []string{"https://exam.edu.dn42", "https://testcenter.edu.dn42"},
		},
		{
			Kind:           "visitor",
			Name:           "visitor",
			Label:          "Sign in as Visitor",
			LoginURL:       "/api/login/visitor",
			AllowedOrigins: []string{},
		},
	})

	optionNames := func(t *testing.T, r *http.Request) []string {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("status: got %d, want %d", rec.Code, http.StatusOK)
		}
		var got []LoginOption
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("response is not a JSON array: %v", err)
		}
		names := make([]string, 0, len(got))
		for _, opt := range got {
			names = append(names, opt.Name)
		}
		return names
	}

	t.Run("matching origin sees the restricted option", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "https://exam.edu.dn42/api/login/loginoptions", nil)
		got := optionNames(t, r)
		want := []string{"github", "kioubit", "visitor"}
		if !slices.Equal(got, want) {
			t.Fatalf("options: got %v, want %v", got, want)
		}
	})

	t.Run("matching Origin header sees the restricted option", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "http://internal/api/login/loginoptions", nil)
		r.Header.Set("Origin", "https://testcenter.edu.dn42")
		got := optionNames(t, r)
		want := []string{"github", "kioubit", "visitor"}
		if !slices.Equal(got, want) {
			t.Fatalf("options: got %v, want %v", got, want)
		}
	})

	t.Run("other origins do not see the restricted option", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "http://localhost:8080/api/login/loginoptions", nil)
		got := optionNames(t, r)
		want := []string{"github", "visitor"}
		if !slices.Equal(got, want) {
			t.Fatalf("options: got %v, want %v", got, want)
		}
	})

	t.Run("unmatched restricted option alone yields an empty array", func(t *testing.T) {
		h := NewLoginOptionsHandler([]LoginOption{
			{Name: "hidden", LoginURL: "/x", AllowedOrigins: []string{"https://elsewhere.example"}},
		})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://localhost:8080/api/login/loginoptions", nil))
		var got []LoginOption
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("response is not a JSON array: %v", err)
		}
		if got == nil || len(got) != 0 {
			t.Fatalf("filtered-out options must be served as [], got %+v", got)
		}
	})
}

func TestParseAllowedOrigins(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{in: "", want: nil},
		{in: "  ", want: nil},
		{in: ",", want: nil},
		{in: "https://a.example", want: []string{"https://a.example"}},
		{
			in:   "https://a.example, https://b.example ,,http://localhost:8080",
			want: []string{"https://a.example", "https://b.example", "http://localhost:8080"},
		},
	}
	for _, tc := range tests {
		if got := ParseAllowedOrigins(tc.in); !slices.Equal(got, tc.want) {
			t.Errorf("ParseAllowedOrigins(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestLoginOptionsHandler_RejectsNonGet(t *testing.T) {
	h := NewLoginOptionsHandler(nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/login/loginoptions", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
	if allow := rec.Header().Get("Allow"); allow != "GET" {
		t.Fatalf("Allow: got %q, want GET", allow)
	}
}
