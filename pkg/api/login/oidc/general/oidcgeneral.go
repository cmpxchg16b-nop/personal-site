package general

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	pkgapicommon "personal-site/pkg/api/common"
	pkgauth "personal-site/pkg/auth"
	pkgcookie "personal-site/pkg/cookie"
	pkgoidc "personal-site/pkg/oidc"
	pkgutils "personal-site/pkg/utils"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"golang.org/x/oauth2"
)

// GenericOIDCLoginHandler implements the OAuth 2.0 Authorization Code flow
// with OpenID Connect extensions against any OIDC-compliant provider.
//
// It builds on top of two standard libraries:
//   - github.com/coreos/go-oidc/v3/oidc for provider discovery and ID token
//     verification;
//   - golang.org/x/oauth2 for the authorize-URL construction and the
//     authorization-code -> token exchange.
//
// The provider's endpoints are discovered automatically from its
// {.well-known/openid-configuration} document.
//
// Register this handler on a path ending with "/", e.g. "/login/as/oidc/".
// Sub-paths "/start" and "/auth" are handled automatically. The "state"
// parameter (a server-issued, cookie-bound nonce) provides CSRF protection
// across the redirect to the IdP.
type GenericOIDCLoginHandler struct {
	SessionLifespan time.Duration

	// A short name identifying the OIDC provider (e.g. "keycloak", "auth0").
	// Used as a prefix in the subject claim: "oidc-{ProviderName}:{sub}".
	// Defaults to "oidc" if empty.
	ProviderName string

	// The issuer URL of the OIDC provider (e.g. "https://auth.example.com/realms/myrealm").
	// The discovery document is fetched from {IssuerURL}/.well-known/openid-configuration.
	IssuerURL string

	// The OAuth 2.0 client ID registered with the OIDC provider.
	ClientId string

	// The OAuth 2.0 client secret registered with the OIDC provider.
	ClientSecret string

	// One of the authorized redirect URIs for the OAuth 2.0 client.
	// When it starts with "/", the origin of the incoming request is
	// prepended dynamically (see AllowedOrigins).
	RedirectURL string

	// AllowedOrigins lists the request origins trusted when RedirectURL is
	// relative: the request's origin is prepended to the redirect URL only
	// when it appears here. When empty, relative redirect URLs are rejected.
	AllowedOrigins []string

	// Space-delimited scopes. Defaults to "openid profile email" if empty.
	Scope string

	LoginSuccessRedirectURL string

	TokenIssuer   pkgauth.JWTIssuer
	NonceIssuer   pkgauth.NonceIssuer
	cookieBuilder pkgcookie.CookieBuilder

	// The OIDC provider, oauth2 config and the (optional) revocation endpoint
	// are resolved lazily on first use and then cached for the lifetime of the
	// handler. oidc.Provider already fetches and caches the discovery document,
	// so there is no need for a separate discovery cache.
	providerInit sync.Once
	provider     *oidc.Provider
	oauth2Config *oauth2.Config
	revokeURL    string
	initErr      error
}

// NewGenericOIDCLoginHandler constructs a GenericOIDCLoginHandler, injecting its
// dependencies including the CookieBuilder used to create the session and state
// cookies.
func NewGenericOIDCLoginHandler(
	sessionLifespan time.Duration,
	providerName string,
	issuerURL string,
	clientId string,
	clientSecret string,
	redirectURL string,
	scope string,
	loginSuccessRedirectURL string,
	allowedOrigins []string,
	tokenIssuer pkgauth.JWTIssuer,
	nonceIssuer pkgauth.NonceIssuer,
	cookieBuilder pkgcookie.CookieBuilder,
) *GenericOIDCLoginHandler {
	return &GenericOIDCLoginHandler{
		SessionLifespan:         sessionLifespan,
		ProviderName:            providerName,
		IssuerURL:               issuerURL,
		ClientId:                clientId,
		ClientSecret:            clientSecret,
		RedirectURL:             redirectURL,
		Scope:                   scope,
		LoginSuccessRedirectURL: loginSuccessRedirectURL,
		AllowedOrigins:          allowedOrigins,
		TokenIssuer:             tokenIssuer,
		NonceIssuer:             nonceIssuer,
		cookieBuilder:           cookieBuilder,
	}
}

const defaultScope = "openid profile email"

func (h *GenericOIDCLoginHandler) getScope() string {
	if h.Scope != "" {
		return h.Scope
	}
	return defaultScope
}

func (h *GenericOIDCLoginHandler) getProviderName() string {
	if h.ProviderName != "" {
		return h.ProviderName
	}
	return "oidc"
}

// initProvider discovers the OIDC provider (which caches the discovery
// document internally) and builds the oauth2.Config used for authorize-URL
// construction and token exchange. The result is cached for the handler's
// lifetime.
func (h *GenericOIDCLoginHandler) initProvider(ctx context.Context) error {
	h.providerInit.Do(func() {
		provider, err := oidc.NewProvider(ctx, h.IssuerURL)
		if err != nil {
			h.initErr = fmt.Errorf("failed to discover OIDC provider %q: %w", h.IssuerURL, err)
			return
		}
		// Pull the optional revocation endpoint out of the raw discovery claims.
		var claims struct {
			RevocationEndpoint string `json:"revocation_endpoint"`
		}
		if err := provider.Claims(&claims); err != nil {
			h.initErr = fmt.Errorf("failed to decode OIDC discovery claims: %w", err)
			return
		}

		h.provider = provider
		h.revokeURL = claims.RevocationEndpoint
		h.oauth2Config = &oauth2.Config{
			ClientID:     h.ClientId,
			ClientSecret: h.ClientSecret,
			RedirectURL:  h.RedirectURL,
			Endpoint:     provider.Endpoint(),
			Scopes:       strings.Fields(h.getScope()),
		}
	})
	return h.initErr
}

func (h *GenericOIDCLoginHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if reqUrl := r.URL; reqUrl != nil {
		if strings.HasSuffix(reqUrl.Path, "/start") {
			h.handleStart(w, r)
			return
		} else if strings.HasSuffix(reqUrl.Path, "/auth") {
			h.handleAuthorizationCode(w, r)
			return
		}
	}
	h.handleNotFoundForThis(w, r)
}

func (h *GenericOIDCLoginHandler) handleStart(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if err := h.initProvider(ctx); err != nil {
		log.Printf("Failed to initialise OIDC provider %q: %v", h.IssuerURL, err)
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(&pkgutils.ErrorResponse{Error: "Failed to fetch OIDC provider configuration"})
		return
	}

	// Resolve the redirect URL against this request's origin when the
	// configured one is relative; oauth2Config is shared and immutable, so a
	// shallow per-request copy carries the resolved URL.
	redirectURL, err := pkgapicommon.ResolveRedirectURL(h.RedirectURL, h.AllowedOrigins, r)
	if err != nil {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(&pkgutils.ErrorResponse{Error: err.Error()})
		return
	}
	oauth2Config := *h.oauth2Config
	oauth2Config.RedirectURL = redirectURL

	// The state value doubles as a CSRF token: it is stored in a cookie and
	// echoed back by the IdP, then validated in /auth before the code is
	// exchanged.
	state, err := h.NonceIssuer.IssueNonce(ctx)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(&pkgutils.ErrorResponse{Error: "Failed to issue state"})
		return
	}

	http.SetCookie(w, h.cookieBuilder.BuildCookieFromKeyValue(pkgapicommon.DefaultNonceCookieKey, state))

	http.Redirect(w, r, oauth2Config.AuthCodeURL(state), http.StatusTemporaryRedirect)
}

func (h *GenericOIDCLoginHandler) handleAuthorizationCode(w http.ResponseWriter, r *http.Request) {
	// See rfc6749 section-4.1.2 and section-4.1.2.1
	// https://datatracker.ietf.org/doc/html/rfc6749#section-4.1.2.1
	if errParam := r.URL.Query().Get("error"); errParam != "" {
		errDesc := r.URL.Query().Get("error_description")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(&pkgutils.ErrorResponse{Error: fmt.Sprintf("%s: %s", errParam, errDesc)})
		return
	}

	ctx := r.Context()
	if err := h.initProvider(ctx); err != nil {
		log.Printf("Failed to initialise OIDC provider %q: %v", h.IssuerURL, err)
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(&pkgutils.ErrorResponse{Error: "Failed to fetch OIDC provider configuration"})
		return
	}

	// Validate the state/CSRF token echoed back by the IdP against the one we
	// stored in a cookie at /start.
	state := r.URL.Query().Get("state")
	stateFromCookie, err := r.Cookie(pkgapicommon.DefaultNonceCookieKey)
	if err != nil || stateFromCookie == nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(pkgutils.ErrorResponse{Error: fmt.Sprintf("State not found in cookies: %v", err)})
		return
	}
	if stateFromCookie.Value != state {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(pkgutils.ErrorResponse{Error: "State from cookie does not match state in request"})
		return
	}
	valid, err := h.NonceIssuer.ValidateNonce(ctx, state)
	if err != nil || !valid {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(&pkgutils.ErrorResponse{Error: fmt.Sprintf("Invalid state: %v", err)})
		return
	}

	// Clear the state cookie now that it has been consumed.
	http.SetCookie(w, &http.Cookie{
		Name:     pkgapicommon.DefaultNonceCookieKey,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})

	code := r.URL.Query().Get("code")
	if code == "" {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(&pkgutils.ErrorResponse{Error: "No authorization code found in request"})
		return
	}

	// The redirect_uri sent with the code exchange must match the one used in
	// /start; resolve it against this request's origin the same way.
	redirectURL, err := pkgapicommon.ResolveRedirectURL(h.RedirectURL, h.AllowedOrigins, r)
	if err != nil {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(&pkgutils.ErrorResponse{Error: err.Error()})
		return
	}
	oauth2Config := *h.oauth2Config
	oauth2Config.RedirectURL = redirectURL

	// Exchange the authorization code for tokens. oauth2 handles the form
	// encoding, the grant_type and the redirect_uri.
	oauth2Token, err := oauth2Config.Exchange(ctx, code)
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(&pkgutils.ErrorResponse{Error: fmt.Sprintf("Failed to exchange token: %v", err)})
		return
	}

	// The ID token is the primary identity artifact in OIDC; extract it from
	// the token response and verify it (signature, issuer, audience, expiry).
	rawIDToken, ok := oauth2Token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(&pkgutils.ErrorResponse{Error: "No ID token in token response from OIDC provider"})
		return
	}

	idToken, err := h.provider.Verifier(&oidc.Config{ClientID: h.ClientId}).Verify(ctx, rawIDToken)
	if err != nil {
		log.Printf("ID token verification failed for OIDC provider %q: %v", h.getProviderName(), err)
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(&pkgutils.ErrorResponse{Error: fmt.Sprintf("ID token verification failed: %v", err)})
		return
	}

	// Revoke the access token if the provider exposes a revocation endpoint.
	if h.revokeURL != "" {
		defer revokeOIDCToken(h.revokeURL, oauth2Token.AccessToken)
	}

	// Extract user identity from the verified ID token claims first.
	idTokenClaims := new(pkgoidc.UserInfoResponse)
	if err := idToken.Claims(idTokenClaims); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(&pkgutils.ErrorResponse{Error: fmt.Sprintf("Failed to extract ID token claims: %v", err)})
		return
	}

	userId := idTokenClaims.Sub
	if userId == "" {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(&pkgutils.ErrorResponse{Error: "Failed to get user ID (sub claim) from ID token"})
		return
	}

	subjectId := fmt.Sprintf("oidc-%s:%s", h.getProviderName(), userId)

	username := idTokenClaims.PreferredUsername
	if username == "" {
		username = idTokenClaims.Email
	}
	if username == "" {
		username = idTokenClaims.Name
	}

	email := idTokenClaims.Email

	// Enrich profile from the userinfo endpoint if the provider exposes one.
	if endpoint := h.provider.UserInfoEndpoint(); endpoint != "" {
		userinfo, err := h.provider.UserInfo(ctx, oauth2.StaticTokenSource(oauth2Token))
		if err != nil {
			log.Printf("Failed to fetch userinfo from %q (non-fatal): %v", endpoint, err)
		} else {
			info := new(pkgoidc.UserInfoResponse)
			if err := userinfo.Claims(info); err != nil {
				log.Printf("Failed to decode userinfo claims (non-fatal): %v", err)
			} else {
				if info.Sub != "" && info.Sub != userId {
					log.Printf("userinfo sub %q differs from ID token sub %q", info.Sub, userId)
				}
				if username == "" {
					username = info.PreferredUsername
				}
				if username == "" {
					username = info.Email
				}
				if username == "" {
					username = info.Name
				}
				if email == "" {
					email = info.Email
				}
			}
		}
	}

	claims, err := h.GetMapClaims(r, subjectId, username, email)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(&pkgutils.ErrorResponse{Error: fmt.Sprintf("Failed to get claims: %v", err)})
		return
	}

	token, err := h.TokenIssuer.IssueToken(ctx, claims)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(&pkgutils.ErrorResponse{Error: fmt.Sprintf("Failed to issue token: %v", err)})
		return
	}

	http.SetCookie(w, h.cookieBuilder.BuildCookieFromToken(token))

	redirUrl := "/"
	if u := h.LoginSuccessRedirectURL; u != "" {
		redirUrl = u
	}

	log.Printf("User %s (id=%s) from OIDC provider %q has been successfully logged in, redirecting to %s", username, userId, h.getProviderName(), redirUrl)
	http.Redirect(w, r, redirUrl, http.StatusTemporaryRedirect)
}

func (h *GenericOIDCLoginHandler) GetMapClaims(r *http.Request, subjectId string, username string, email string) (jwt.MapClaims, error) {

	customClaims := &pkgauth.CustomClaimType{}
	customClaims.ID = uuid.NewString()
	customClaims.Subject = subjectId
	customClaims.Audience = make([]string, 0)
	customClaims.Audience = append(customClaims.Audience, pkgauth.AudSession)
	now := time.Now()
	customClaims.NotBefore = jwt.NewNumericDate(now)
	customClaims.ExpiresAt = jwt.NewNumericDate(now.Add(h.SessionLifespan))
	customClaims.Username = username
	customClaims.Email = email

	return pkgauth.NewMapClaims(customClaims)
}

// revokeOIDCToken revokes the OAuth access token at the provider's revocation endpoint.
// Uses context.Background() because it is typically called via defer after the response
// has already been written.
func revokeOIDCToken(revocationEndpoint, token string) error {
	body := url.Values{}
	body.Set("token", token)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, revocationEndpoint, strings.NewReader(body.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("failed to revoke OIDC token: status code %d", resp.StatusCode)
	}
	return nil
}

func (h *GenericOIDCLoginHandler) handleNotFoundForThis(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNotFound)
	json.NewEncoder(w).Encode(&pkgutils.ErrorResponse{Error: fmt.Sprintf("Path %s has no handler attached", r.URL.Path)})
}
