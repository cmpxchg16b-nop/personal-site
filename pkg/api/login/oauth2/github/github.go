package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	pkgapicommon "personal-site/pkg/api/common"
	pkgauth "personal-site/pkg/auth"
	pkgcookie "personal-site/pkg/cookie"
	pkggithub "personal-site/pkg/github"
	pkgutils "personal-site/pkg/utils"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

type GithubOAuthLoginNonceState struct {
	SessionId   string
	CurrentPage string
}

type GithubOAuthLoginHandler struct {
	SessionLifespan time.Duration

	// Must be specified, get it from Github OAuth app settings page
	GithubOAuthClientId string

	// Must be specified, get it from Github OAuth app settings page
	GithubOAuthAppSecret string

	// Must be specified, get it from Github OAuth app settings page.
	// When it starts with "/", the origin of the incoming request is
	// prepended dynamically (see AllowedOrigins).
	GithubOAuthRedirURL string

	// AllowedOrigins lists the request origins trusted when
	// GithubOAuthRedirURL is relative: the request's origin is prepended to
	// the redirect URL only when it appears here. When empty, relative
	// redirect URLs are rejected.
	AllowedOrigins []string

	// If this is empty, we would use default value (see github docs) for it.
	GithubOAuthLoginPage string

	// If this is empty, we would use default value ("read:user")) for it.
	// Note: we deliberately do not request the broader "user:email" scope;
	// only the user's publicly visible email (part of the profile) is read.
	GithubOAuthScope string

	// If this is empty, we would use default value (see github docs) for it.
	GithubOAuthTokenEndpoint string

	LoginSuccessRedirectURL string

	TokenIssuer   pkgauth.JWTIssuer
	NonceIssuer   pkgauth.NonceIssuer
	cookieBuilder pkgcookie.CookieBuilder
}

// NewGithubOAuthLoginHandler constructs a GithubOAuthLoginHandler, injecting its
// dependencies including the CookieBuilder used to create the session and nonce
// cookies.
func NewGithubOAuthLoginHandler(
	sessionLifespan time.Duration,
	githubOAuthClientId string,
	githubOAuthAppSecret string,
	githubOAuthRedirURL string,
	githubOAuthLoginPage string,
	githubOAuthScope string,
	githubOAuthTokenEndpoint string,
	loginSuccessRedirectURL string,
	allowedOrigins []string,
	tokenIssuer pkgauth.JWTIssuer,
	nonceIssuer pkgauth.NonceIssuer,
	cookieBuilder pkgcookie.CookieBuilder,
) *GithubOAuthLoginHandler {
	return &GithubOAuthLoginHandler{
		SessionLifespan:          sessionLifespan,
		GithubOAuthClientId:      githubOAuthClientId,
		GithubOAuthAppSecret:     githubOAuthAppSecret,
		GithubOAuthRedirURL:      githubOAuthRedirURL,
		GithubOAuthLoginPage:     githubOAuthLoginPage,
		GithubOAuthScope:         githubOAuthScope,
		GithubOAuthTokenEndpoint: githubOAuthTokenEndpoint,
		LoginSuccessRedirectURL:  loginSuccessRedirectURL,
		AllowedOrigins:           allowedOrigins,
		TokenIssuer:              tokenIssuer,
		NonceIssuer:              nonceIssuer,
		cookieBuilder:            cookieBuilder,
	}
}

func (h *GithubOAuthLoginHandler) getGithubLoginPage() string {
	if h.GithubOAuthLoginPage != "" {
		return h.GithubOAuthLoginPage
	}
	return "https://github.com/login/oauth/authorize"
}

func (h *GithubOAuthLoginHandler) getGithubTokenEndpoint() string {
	if h.GithubOAuthTokenEndpoint != "" {
		return h.GithubOAuthTokenEndpoint
	}
	return "https://github.com/login/oauth/access_token"
}

func (h *GithubOAuthLoginHandler) getGithubOAuthScope() string {
	if h.GithubOAuthScope != "" {
		return h.GithubOAuthScope
	}
	scopes := []string{"read:user"}
	return strings.Join(scopes, " ")
}

func (h *GithubOAuthLoginHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if url := r.URL; url != nil {
		if strings.HasSuffix(url.Path, "/start") {
			h.handleStart(w, r)
			return
		} else if strings.HasSuffix(url.Path, "/auth") {
			h.handleAuthorizationCode(w, r)
			return
		}
	}
	h.handleNotFoundForThis(w, r)
}

func (h *GithubOAuthLoginHandler) getGithubOAuthRedirectURL(redirectURL string, nonce string) string {
	urlVals := url.Values{}
	urlVals.Set("client_id", h.GithubOAuthClientId)
	urlVals.Set("redirect_uri", redirectURL)
	urlVals.Set("scope", h.getGithubOAuthScope())
	urlVals.Set("state", nonce)
	urlObj, err := url.Parse(h.getGithubLoginPage())
	if err != nil {
		log.New(os.Stderr, "LoginHandler", 0).Println("Invalid github login page url:", err)
		return ""
	}
	urlObj.RawQuery = urlVals.Encode()
	return urlObj.String()
}

func (h *GithubOAuthLoginHandler) handleStart(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	nonce, err := h.NonceIssuer.IssueNonce(ctx)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(&pkgutils.ErrorResponse{Error: "Failed to issue nonce"})
		return
	}

	cookieObj := h.cookieBuilder.BuildCookieFromKeyValue(pkgapicommon.DefaultNonceCookieKey, nonce)
	http.SetCookie(w, cookieObj)

	// Stash the post-login redirect target (if the login page passed one)
	// alongside the nonce: the /auth callback consumes it after the
	// successful code exchange. A target outside the allowed origins is
	// rejected outright — the cookie is client-controllable, so the callback
	// re-validates too.
	if target := r.URL.Query().Get(pkgapicommon.DefaultRedirectIfSucceedCookieKey); target != "" {
		if !pkgapicommon.IsAllowedPostLoginRedirect(target, h.AllowedOrigins) {
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(&pkgutils.ErrorResponse{Error: fmt.Sprintf("redirect target %q is not in the allowed origins", target)})
			return
		}
		http.SetCookie(w, h.cookieBuilder.BuildCookieFromKeyValue(pkgapicommon.DefaultRedirectIfSucceedCookieKey, target))
	}

	redirectURL, err := pkgapicommon.ResolveRedirectURL(h.GithubOAuthRedirURL, h.AllowedOrigins, r)
	if err != nil {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(&pkgutils.ErrorResponse{Error: err.Error()})
		return
	}

	redirURL := h.getGithubOAuthRedirectURL(redirectURL, nonce)
	if redirURL == "" {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(&pkgutils.ErrorResponse{Error: "Failed to determine redir url (internal error)"})
		return
	}
	http.Redirect(w, r, redirURL, http.StatusTemporaryRedirect)
}

func (h *GithubOAuthLoginHandler) handleAuthorizationCode(w http.ResponseWriter, r *http.Request) {
	// See rfc6749 section-4.1.2 and section-4.1.2.1
	// https://datatracker.ietf.org/doc/html/rfc6749#section-4.1.2.1
	if err := r.URL.Query().Get("error"); err != "" {
		errDesc := r.URL.Query().Get("error_description")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(&pkgutils.ErrorResponse{Error: fmt.Sprintf("%s: %s", err, errDesc)})
		return
	}

	ctx := r.Context()
	nonce := r.URL.Query().Get("state")
	nonceFromCookie, err := r.Cookie(pkgapicommon.DefaultNonceCookieKey)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(pkgutils.ErrorResponse{Error: fmt.Sprintf("Nonce is not found from the cookies: %v", err)})
		return
	}
	if nonceFromCookie == nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(pkgutils.ErrorResponse{Error: fmt.Sprintf("Nonce is not found from the cookies")})
		return
	}

	if nonceFromCookie.Value != nonce {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(pkgutils.ErrorResponse{Error: fmt.Sprintf("Nonce from the cookie does not match the nonce in the request")})
		return
	}

	valid, err := h.NonceIssuer.ValidateNonce(ctx, nonce)
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(&pkgutils.ErrorResponse{Error: fmt.Sprintf("Invalid nonce: %v", err)})
		return
	}
	if !valid {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(&pkgutils.ErrorResponse{Error: "Invalid nonce"})
		return
	}

	authZCode := r.URL.Query().Get("code")
	if authZCode == "" {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(&pkgutils.ErrorResponse{Error: "No authorization code is found in the request"})
		return
	}

	redirectURL, err := pkgapicommon.ResolveRedirectURL(h.GithubOAuthRedirURL, h.AllowedOrigins, r)
	if err != nil {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(&pkgutils.ErrorResponse{Error: err.Error()})
		return
	}

	bodyVals := url.Values{}
	bodyVals.Set("client_id", h.GithubOAuthClientId)
	bodyVals.Set("client_secret", h.GithubOAuthAppSecret)
	bodyVals.Set("code", authZCode)
	bodyVals.Set("redirect_uri", redirectURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.getGithubTokenEndpoint(), strings.NewReader(bodyVals.Encode()))
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(&pkgutils.ErrorResponse{Error: "Failed to create request to github token endpoint"})
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	cli := http.DefaultClient
	resp, err := cli.Do(req)
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(&pkgutils.ErrorResponse{Error: fmt.Sprintf("Failed to get token from github: %+v", err)})
		return
	}
	defer resp.Body.Close()

	tokenResp := new(pkggithub.GithubTokenResponse)
	if err := json.NewDecoder(resp.Body).Decode(tokenResp); err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(&pkgutils.ErrorResponse{Error: "Failed to decode github token api response"})
		return
	}
	defer revokeGithubToken(h.GithubOAuthClientId, h.GithubOAuthAppSecret, tokenResp.AccessToken)

	profile, err := pkggithub.GetGithubProfileByToken(ctx, tokenResp.AccessToken)
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(&pkgutils.ErrorResponse{Error: fmt.Sprintf("Failed to get Github user profile: %v", err)})
		return
	}

	githubId := profile.Id
	if githubId == nil {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(&pkgutils.ErrorResponse{Error: "Failed to get Id of github user"})
		return
	}

	subjectId := fmt.Sprintf("github:%d", *githubId)
	username := profile.Login

	// Only the user's publicly visible email is used: the "email" field of
	// the profile endpoint is exactly that, and is available under the
	// "read:user" scope. Users who keep their email addresses private will
	// simply have an empty email claim; reading those would require the
	// broader "user:email" scope, which we intentionally do not request.
	email := profile.Email

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

	cookieObj := h.cookieBuilder.BuildCookieFromToken(token)
	http.SetCookie(w, cookieObj)

	// The post-login redirect target: the redirect_if_succeed cookie stashed
	// by /start (consumed and cleared here), else the configured
	// LoginSuccessRedirectURL, else "/". A cookie outside the allowed
	// origins is rejected outright.
	redirUrl := "/"

	if u := h.LoginSuccessRedirectURL; u != "" {
		redirUrl = u
	}

	if c, err := r.Cookie(pkgapicommon.DefaultRedirectIfSucceedCookieKey); err == nil && c.Value != "" {
		if !pkgapicommon.IsAllowedPostLoginRedirect(c.Value, h.AllowedOrigins) {
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(&pkgutils.ErrorResponse{Error: fmt.Sprintf("redirect target %q is not in the allowed origins", c.Value)})
			return
		}
		redirUrl = c.Value
		http.SetCookie(w, &http.Cookie{
			Name:     pkgapicommon.DefaultRedirectIfSucceedCookieKey,
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteLaxMode,
		})
	}

	log.Printf("User %s (id=%d) from github has been successfully logged in, redirecting to %s", username, *githubId, redirUrl)
	http.Redirect(w, r, redirUrl, http.StatusTemporaryRedirect)
}

func (h *GithubOAuthLoginHandler) GetMapClaims(r *http.Request, subjectId string, username string, email string) (jwt.MapClaims, error) {

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

func revokeGithubToken(cliId, cliSec, token string) error {
	u := fmt.Sprintf("https://api.github.com/applications/%s/token", cliId)
	var reqBody bytes.Buffer
	if err := json.NewEncoder(&reqBody).Encode(map[string]string{"access_token": token}); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodDelete, u, &reqBody)
	if err != nil {
		return err
	}
	req.SetBasicAuth(cliId, cliSec)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("failed to revoke token: status code %d", resp.StatusCode)
	}
	return nil
}

func (h *GithubOAuthLoginHandler) handleNotFoundForThis(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNotFound)
	json.NewEncoder(w).Encode(&pkgutils.ErrorResponse{Error: fmt.Sprintf("Path %s has no handler attached", r.URL.Path)})
}
