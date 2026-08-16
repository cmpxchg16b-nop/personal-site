package common_test

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"

	pkgapicommon "personal-site/pkg/api/common"
)

func TestRequestOrigin(t *testing.T) {
	tests := []struct {
		name string
		req  *http.Request
		want string
	}{
		{
			name: "Origin header wins",
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "http://internal.example/start", nil)
				r.Header.Set("Origin", "https://app.example.com")
				return r
			}(),
			want: "https://app.example.com",
		},
		{
			name: "null Origin header falls back to host",
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "http://internal.example/start", nil)
				r.Header.Set("Origin", "null")
				return r
			}(),
			want: "http://internal.example",
		},
		{
			name: "plain http from host",
			req:  httptest.NewRequest(http.MethodGet, "http://localhost:8080/start", nil),
			want: "http://localhost:8080",
		},
		{
			name: "https from TLS state",
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "https://secure.example/start", nil)
				r.TLS = &tls.ConnectionState{}
				return r
			}(),
			want: "https://secure.example",
		},
		{
			name: "X-Forwarded-Proto overrides scheme",
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "http://app.example/start", nil)
				r.Header.Set("X-Forwarded-Proto", "https")
				return r
			}(),
			want: "https://app.example",
		},
		{
			name: "first hop of X-Forwarded-Proto list is used",
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "http://app.example/start", nil)
				r.Header.Set("X-Forwarded-Proto", "https, http")
				return r
			}(),
			want: "https://app.example",
		},
		{
			name: "unknown X-Forwarded-Proto value ignored",
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "http://app.example/start", nil)
				r.Header.Set("X-Forwarded-Proto", "gopher")
				return r
			}(),
			want: "http://app.example",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := pkgapicommon.RequestOrigin(tc.req); got != tc.want {
				t.Errorf("RequestOrigin() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveRedirectURL(t *testing.T) {
	allowed := []string{"http://localhost:8080", "https://app.example.com"}

	tests := []struct {
		name        string
		redirectURL string
		originHdr   string
		want        string
		wantErr     bool
	}{
		{
			name:        "absolute URL returned unchanged",
			redirectURL: "https://fixed.example.com/auth",
			want:        "https://fixed.example.com/auth",
		},
		{
			name:        "relative URL resolved against allowed origin",
			redirectURL: "/api/login/oidc/prov/auth",
			want:        "http://localhost:8080/api/login/oidc/prov/auth",
		},
		{
			name:        "relative URL resolved against allowed Origin header",
			redirectURL: "/auth",
			originHdr:   "https://app.example.com",
			want:        "https://app.example.com/auth",
		},
		{
			name:        "disallowed origin rejected",
			redirectURL: "/auth",
			originHdr:   "https://evil.example.com",
			wantErr:     true,
		},
		{
			name:        "empty allowed list rejects relative URL",
			redirectURL: "/auth",
			want:        "", // resolved below with nil allowed list
			wantErr:     true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			allowedList := allowed
			if tc.name == "empty allowed list rejects relative URL" {
				allowedList = nil
			}
			r := httptest.NewRequest(http.MethodGet, "http://localhost:8080/start", nil)
			if tc.originHdr != "" {
				r.Header.Set("Origin", tc.originHdr)
			}
			got, err := pkgapicommon.ResolveRedirectURL(tc.redirectURL, allowedList, r)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ResolveRedirectURL() = %q, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveRedirectURL() unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("ResolveRedirectURL() = %q, want %q", got, tc.want)
			}
		})
	}
}
