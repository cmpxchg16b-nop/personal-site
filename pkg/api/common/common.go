package common

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

const DefaultJWTCookieKey = "jwt"
const DefaultNonceCookieKey = "nonce"

// DefaultRedirectIfSucceedCookieKey is the cookie that carries the URL the
// OAuth2/OIDC login callbacks redirect to after a successful login. The
// login start handlers set it from the redirect_if_succeed query parameter
// (same name), which the login page appends to its options' start URLs.
const DefaultRedirectIfSucceedCookieKey = "redirect_if_succeed"

// IsAllowedPostLoginRedirect reports whether target is a safe post-login
// redirect target: either a site-relative path (starts with "/" but not
// "//", which would be a protocol-relative URL leaving the site), or an
// absolute http(s) URL whose origin ("scheme://host") appears in
// allowedOrigins. The login handlers apply it to the client-supplied
// redirect_if_succeed value — both when stashing it (/start) and when
// consuming it (/auth), since the cookie is client-controllable — so the
// post-login redirect cannot be pointed off-site (an open redirect).
func IsAllowedPostLoginRedirect(target string, allowedOrigins []string) bool {
	if strings.HasPrefix(target, "/") {
		return !strings.HasPrefix(target, "//")
	}
	u, err := url.Parse(target)
	if err != nil || u.Host == "" {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	return slices.Contains(allowedOrigins, u.Scheme+"://"+u.Host)
}

// RequestOrigin determines the externally-visible origin ("scheme://host") of
// an incoming request. The Origin header is preferred (browsers send it on
// cross-site and non-GET requests); top-level navigations typically carry no
// Origin header, so the origin is then reconstructed from the request's TLS
// state (or the X-Forwarded-Proto header set by a trusted reverse proxy) and
// the Host header.
func RequestOrigin(r *http.Request) string {
	if origin := r.Header.Get("Origin"); origin != "" && origin != "null" {
		return origin
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	// A reverse proxy may terminate TLS; honor its forwarded scheme. Only the
	// first hop's value is used, and only well-known schemes are accepted.
	if fwd := r.Header.Get("X-Forwarded-Proto"); fwd != "" {
		if i := strings.IndexByte(fwd, ','); i >= 0 {
			fwd = strings.TrimSpace(fwd[:i])
		}
		if fwd == "http" || fwd == "https" {
			scheme = fwd
		}
	}
	return scheme + "://" + r.Host
}

// ResolveRedirectURL turns a configured OAuth/OIDC redirect URL into the
// absolute URL sent to the identity provider. Absolute URLs are returned
// unchanged. A relative URL (starting with "/") is resolved against the
// origin of the incoming request, so a single configuration serves every
// origin the deployment is reachable under (e.g. "http://localhost:8080"
// during development and "https://app.example.com" in production).
//
// Because the request's origin is attacker-controllable (Host header), it is
// only trusted when it appears in allowedOrigins; otherwise an error is
// returned and the login attempt must be aborted. An empty allowedOrigins
// list therefore disables relative redirect URLs entirely.
func ResolveRedirectURL(redirectURL string, allowedOrigins []string, r *http.Request) (string, error) {
	if !strings.HasPrefix(redirectURL, "/") {
		return redirectURL, nil
	}
	origin := RequestOrigin(r)
	if !slices.Contains(allowedOrigins, origin) {
		return "", fmt.Errorf("origin %q is not in the list of allowed origins", origin)
	}
	return origin + redirectURL, nil
}
