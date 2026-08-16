package logout_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	pkgapicommon "personal-site/pkg/api/common"
	"personal-site/pkg/api/logout"
	pkgutils "personal-site/pkg/utils"
)

// findCookie returns the named response cookie, or nil when absent.
func findCookie(rr *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rr.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// assertClearedCookie checks that the named cookie is present and configured
// to expire immediately (empty value, MaxAge < 0) with the security
// attributes the handler sets.
func assertClearedCookie(t *testing.T, rr *httptest.ResponseRecorder, name string) {
	t.Helper()

	c := findCookie(rr, name)
	if c == nil {
		t.Fatalf("cookie %q is not set", name)
	}
	if c.Value != "" {
		t.Errorf("cookie %q value = %q, want empty", name, c.Value)
	}
	if c.MaxAge >= 0 {
		t.Errorf("cookie %q MaxAge = %d, want < 0 so it expires immediately", name, c.MaxAge)
	}
	if c.Path != "/" {
		t.Errorf("cookie %q Path = %q, want %q", name, c.Path, "/")
	}
	if !c.HttpOnly {
		t.Errorf("cookie %q HttpOnly = false, want true", name)
	}
	if !c.Secure {
		t.Errorf("cookie %q Secure = false, want true", name)
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie %q SameSite = %v, want SameSiteLaxMode", name, c.SameSite)
	}
}

func TestServeHTTPPostClearsCookiesAndRedirects(t *testing.T) {
	tests := []struct {
		name                string
		redirectAfterLogout string
		wantLocation        string
	}{
		{
			name:                "default redirect when empty",
			redirectAfterLogout: "",
			wantLocation:        "/",
		},
		{
			name:                "custom redirect",
			redirectAfterLogout: "/login",
			wantLocation:        "/login",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := logout.NewLogoutHandler(tt.redirectAfterLogout)

			req := httptest.NewRequest(http.MethodPost, "/api/logout", nil)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)

			if rr.Code != http.StatusFound {
				t.Errorf("status = %d, want %d", rr.Code, http.StatusFound)
			}
			if got := rr.Header().Get("Location"); got != tt.wantLocation {
				t.Errorf("Location = %q, want %q", got, tt.wantLocation)
			}

			assertClearedCookie(t, rr, pkgapicommon.DefaultJWTCookieKey)
			assertClearedCookie(t, rr, pkgapicommon.DefaultNonceCookieKey)
		})
	}
}

func TestServeHTTPRejectsNonPostMethods(t *testing.T) {
	methods := []string{
		http.MethodGet,
		http.MethodPut,
		http.MethodPatch,
		http.MethodDelete,
		http.MethodHead,
	}

	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			h := logout.NewLogoutHandler("/login")

			req := httptest.NewRequest(method, "/api/logout", nil)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)

			if rr.Code != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want %d", rr.Code, http.StatusMethodNotAllowed)
			}

			var body pkgutils.ErrorResponse
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
				t.Fatalf("response body is not valid JSON: %v", err)
			}
			want := fmt.Sprintf("method %s not allowed, use POST", method)
			if body.Error != want {
				t.Errorf("error body = %q, want %q", body.Error, want)
			}

			// No cookies should be cleared and no redirect should happen.
			if cookies := rr.Result().Cookies(); len(cookies) != 0 {
				t.Errorf("got %d cookies, want none", len(cookies))
			}
			if loc := rr.Header().Get("Location"); loc != "" {
				t.Errorf("Location = %q, want empty", loc)
			}
		})
	}
}
