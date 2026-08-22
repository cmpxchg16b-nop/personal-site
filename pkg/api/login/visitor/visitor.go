package visitor

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"time"

	pkgapicommon "personal-site/pkg/api/common"
	pkgauth "personal-site/pkg/auth"
	pkgcookie "personal-site/pkg/cookie"
	pkgutils "personal-site/pkg/utils"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

type VisitorLoginHandler struct {
	JWTIssuer       pkgauth.JWTIssuer
	Validity        time.Duration
	TicketGenerator pkgauth.TicketGenerator
	// AllowedOrigins lists the origins trusted as absolute
	// redirect_if_succeed targets (site-relative targets are always
	// allowed); see pkgapicommon.IsAllowedPostLoginRedirect.
	AllowedOrigins []string
	cookieBuilder  pkgcookie.CookieBuilder
}

// NewVisitorLoginHandler constructs a VisitorLoginHandler, injecting its
// dependencies including the CookieBuilder used to create the session cookie.
func NewVisitorLoginHandler(jwtIssuer pkgauth.JWTIssuer, validity time.Duration, ticketGenerator pkgauth.TicketGenerator, allowedOrigins []string, cookieBuilder pkgcookie.CookieBuilder) *VisitorLoginHandler {
	return &VisitorLoginHandler{
		JWTIssuer:       jwtIssuer,
		Validity:        validity,
		TicketGenerator: ticketGenerator,
		AllowedOrigins:  allowedOrigins,
		cookieBuilder:   cookieBuilder,
	}
}

func (h *VisitorLoginHandler) GetMapClaims(r *http.Request) (jwt.MapClaims, error) {
	visitorId := rand.IntN(65536)
	visitorIdStr := fmt.Sprintf("%05d", visitorId)

	customClaims := &pkgauth.CustomClaimType{}
	customClaims.ID = uuid.NewString()
	customClaims.Subject = pkgauth.VisitorSubjectPrefix + uuid.NewString()
	customClaims.Audience = make([]string, 0)
	customClaims.Audience = append(customClaims.Audience, pkgauth.AudSession)
	now := time.Now()
	customClaims.NotBefore = jwt.NewNumericDate(now)
	customClaims.ExpiresAt = jwt.NewNumericDate(now.Add(h.Validity))
	customClaims.Username = "visitor-" + visitorIdStr

	return pkgauth.NewMapClaims(customClaims)
}

func (h *VisitorLoginHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, err := h.TicketGenerator.GetTicket(ctx)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(pkgutils.ErrorResponse{Error: fmt.Errorf("can't wait for visitor ticket to be generated: %w", err).Error()})
		return
	}

	claims, err := h.GetMapClaims(r)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(pkgutils.ErrorResponse{Error: fmt.Sprintf("failed to generate the token claims for you: %v", err)})
		return
	}

	token, err := h.JWTIssuer.IssueToken(ctx, claims)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(pkgutils.ErrorResponse{Error: fmt.Sprintf("failed to sign token: %v", err)})
		return
	}

	subj := ""
	if s, err := claims.GetSubject(); err == nil {
		subj = s
	}
	log.Printf("issued token for remote %s, subject is %s", pkgutils.GetRemoteAddr(r), subj)

	cookieObj := h.cookieBuilder.BuildCookieFromToken(token)

	http.SetCookie(w, cookieObj)

	// Unlike the OAuth handlers there is no IdP round trip here, so the
	// login page's redirect_if_succeed parameter can be honored straight
	// from the query string — no cookie stash needed. A target outside the
	// allowed origins is rejected outright.
	redirUrl := "/"
	if target := r.URL.Query().Get(pkgapicommon.DefaultRedirectIfSucceedCookieKey); target != "" {
		if !pkgapicommon.IsAllowedPostLoginRedirect(target, h.AllowedOrigins) {
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(pkgutils.ErrorResponse{Error: fmt.Sprintf("redirect target %q is not in the allowed origins", target)})
			return
		}
		redirUrl = target
	}
	http.Redirect(w, r, redirUrl, http.StatusTemporaryRedirect)
}
