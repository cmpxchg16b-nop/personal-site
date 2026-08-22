package cloudflare

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	pkgoidc "personal-site/pkg/oidc"
	pkgutils "personal-site/pkg/utils"

	"github.com/coreos/go-oidc/v3/oidc"
)

const CF_JWT_HEADER = "Cf-Access-Jwt-Assertion"
const CF_AUTH_COOKIE = "CF_AUTHORIZATION"

type WithCloudflareJWTValidate struct {
	CloudflareTeamName string
	CloudflareAUD      string
	Origin             http.Handler
}

func (withCfJWT *WithCloudflareJWTValidate) mustGetTeam() string {
	if team := withCfJWT.CloudflareTeamName; team != "" {
		return team
	}
	log.Panic("Cloudflare team name not specified")
	return ""
}

func (withCfJWT *WithCloudflareJWTValidate) mustGetPubkeysURL() string {
	team := withCfJWT.mustGetTeam()
	urlStr := fmt.Sprintf("https://%s.cloudflareaccess.com/cdn-cgi/access/certs", team)
	return urlStr
}

func (withCftJWT *WithCloudflareJWTValidate) mustGetAUD() string {
	if aud := withCftJWT.CloudflareAUD; aud != "" {
		return aud
	}
	log.Panic("Cloudflare AUD not specified")
	return ""
}

func (withCfgJWT *WithCloudflareJWTValidate) mustGetVerifier(ctx context.Context) *oidc.IDTokenVerifier {

	config := &oidc.Config{
		ClientID: withCfgJWT.mustGetAUD(),
	}
	keySet := oidc.NewRemoteKeySet(ctx, withCfgJWT.mustGetPubkeysURL())
	teamDomain := fmt.Sprintf("https://%s.cloudflareaccess.com", withCfgJWT.mustGetTeam())
	return oidc.NewVerifier(teamDomain, keySet, config)
}

func (handler *WithCloudflareJWTValidate) getCFJWT(r *http.Request) string {
	if accessJWT := r.Header.Get(CF_JWT_HEADER); accessJWT != "" {
		return accessJWT
	}

	if cookieObj, err := r.Cookie(CF_AUTH_COOKIE); err == nil && cookieObj != nil {
		if accessJWT := cookieObj.Value; accessJWT != "" {
			return accessJWT
		}
	}
	return ""
}

func (handler *WithCloudflareJWTValidate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	accessJWT := handler.getCFJWT(r)
	if accessJWT == "" {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(pkgutils.ErrorResponse{Error: "No token on the request"})
		return
	}

	verifier := handler.mustGetVerifier(ctx)
	idToken, err := verifier.Verify(ctx, accessJWT)
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(pkgutils.ErrorResponse{Error: fmt.Sprintf("Invalid token: %s", err.Error())})
		return
	}

	if idToken == nil {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(pkgutils.ErrorResponse{Error: "IdToken is nil"})
		return
	}

	// Extract the user's identity from the verified id token so downstream
	// handlers can rely on it. Cloudflare Access JWTs carry OIDC standard
	// claims such as "sub" and "email" (see
	// https://developers.cloudflare.com/cloudflare-one/identity/authorization-cookie/validating-json/).
	// Failure to decode is non-fatal: the token has already been verified.
	claims := new(pkgoidc.UserInfoResponse)
	if err := idToken.Claims(claims); err != nil {
		log.Printf("Failed to decode Cloudflare id token claims (non-fatal): %v", err)
	} else {
		username := claims.PreferredUsername
		if username == "" {
			username = claims.Name
		}
		if username == "" {
			username = claims.Email
		}
		if username != "" {
			ctx = context.WithValue(ctx, pkgutils.CtxKeyUsername, username)
		}
		if claims.Email != "" {
			ctx = context.WithValue(ctx, pkgutils.CtxKeyEmail, claims.Email)
		}
		r = r.WithContext(ctx)
	}

	handler.Origin.ServeHTTP(w, r)
}
