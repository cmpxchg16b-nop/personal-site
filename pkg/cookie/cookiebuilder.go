package cookie

import (
	"net/http"

	pkgapicommon "personal-site/pkg/api/common"
)

// CookieBuilder builds the HTTP cookies used by the login handlers.
type CookieBuilder interface {
	// BuildCookieFromToken builds the cookie that carries the session JWT,
	// using the default JWT cookie key (pkgapicommon.DefaultJWTCookieKey)
	// as the cookie name.
	BuildCookieFromToken(token string) *http.Cookie

	// BuildCookieFromKeyValue builds a cookie with an arbitrary name and
	// value, used for non-token cookies such as the OAuth/OIDC nonce cookie.
	BuildCookieFromKeyValue(name, value string) *http.Cookie
}

// SimpleCookieBuilder is the default CookieBuilder implementation. It follows
// the attributes used by the visitor login handler.
type SimpleCookieBuilder struct{}

// compile-time assertion that SimpleCookieBuilder implements CookieBuilder.
var _ CookieBuilder = (*SimpleCookieBuilder)(nil)

// BuildCookieFromToken builds a cookie named after the default JWT cookie key
// that carries the given JWT token.
func (b *SimpleCookieBuilder) BuildCookieFromToken(token string) *http.Cookie {
	cookieObj := &http.Cookie{}
	cookieObj.HttpOnly = true
	cookieObj.Secure = true

	cookieObj.Path = "/"
	cookieObj.Name = pkgapicommon.DefaultJWTCookieKey
	cookieObj.Value = token
	return cookieObj
}

// BuildCookieFromKeyValue builds a cookie with the given name and value.
func (b *SimpleCookieBuilder) BuildCookieFromKeyValue(name, value string) *http.Cookie {
	cookieObj := &http.Cookie{}
	cookieObj.HttpOnly = true
	cookieObj.Secure = true

	cookieObj.Path = "/"
	cookieObj.Name = name
	cookieObj.Value = value
	return cookieObj
}
