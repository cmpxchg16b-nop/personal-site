package visitor

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"time"

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
	cookieBuilder   pkgcookie.CookieBuilder
}

// NewVisitorLoginHandler constructs a VisitorLoginHandler, injecting its
// dependencies including the CookieBuilder used to create the session cookie.
func NewVisitorLoginHandler(jwtIssuer pkgauth.JWTIssuer, validity time.Duration, ticketGenerator pkgauth.TicketGenerator, cookieBuilder pkgcookie.CookieBuilder) *VisitorLoginHandler {
	return &VisitorLoginHandler{
		JWTIssuer:       jwtIssuer,
		Validity:        validity,
		TicketGenerator: ticketGenerator,
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
	http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
}
