package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	pkgapicommon "personal-site/pkg/api/common"
	pkgutils "personal-site/pkg/utils"

	"github.com/golang-jwt/jwt/v5"
)

type CustomClaimType struct {
	jwt.RegisteredClaims
	Username string `json:"username,omitempty"`
	Email    string `json:"email,omitempty"`
}

type JWTIssuer interface {
	IssueToken(ctx context.Context, mapClaims jwt.MapClaims) (string, error)
}

type JWTValidator interface {
	ValidateToken(ctx context.Context, token string) (valid bool, reason string, err error)

	// the second return value is claim of custom claim type
	ParseToken(ctx context.Context, token string) (*jwt.RegisteredClaims, any, error)
}

type StaticKeyJWTValidator struct {
	secretProvider SecretProvider
	blacklist      BlackListProvider
	rejectVisitor  bool
}

func NewStaticKeyJWTValidator(secretProvider SecretProvider, blacklist BlackListProvider, rejectVisitor bool) *StaticKeyJWTValidator {
	return &StaticKeyJWTValidator{
		secretProvider: secretProvider,
		blacklist:      blacklist,
		rejectVisitor:  rejectVisitor,
	}
}

func (s *StaticKeyJWTValidator) ParseToken(ctx context.Context, tokenString string) (*jwt.RegisteredClaims, any, error) {
	_, claims, _, err := s.doValidateToken(ctx, tokenString)
	if err != nil {
		return nil, nil, fmt.Errorf("internal error, can not parse token: %w", err)
	}
	if claims == nil {
		return nil, nil, fmt.Errorf("token is nil or invalid")
	}
	return &claims.RegisteredClaims, claims, nil
}

func (s *StaticKeyJWTValidator) checkBL(ctx context.Context, subj string) bool {
	hit, err := s.blacklist.CheckBlackList(ctx, subj)
	if err != nil {
		log.Printf("blacklist provider returned an error: %v", err)
		return true
	}

	return hit
}

// returns: (valid, claims, reason, error)
func (s *StaticKeyJWTValidator) doValidateToken(ctx context.Context, tokenString string) (*jwt.Token, *CustomClaimType, string, error) {
	token, err := jwt.ParseWithClaims(tokenString, &CustomClaimType{}, s.secretProvider.GetSecret, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))
	if err != nil {
		return nil, nil, fmt.Sprintf("Invalid token: %s", err.Error()), nil
	}
	if token == nil || !token.Valid {
		return nil, nil, "Invalid token: token is nil or invalid", nil
	}

	if claims, ok := token.Claims.(*CustomClaimType); ok && claims != nil {
		if s.checkBL(ctx, claims.Subject) {
			return nil, nil, fmt.Sprintf("Token is blacklisted, subj=%s", claims.Subject), nil
		}
		if s.rejectVisitor && strings.HasPrefix(claims.Subject, VisitorSubjectPrefix) {
			return nil, nil, fmt.Sprintf("Visitor sessions are not allowed, subj=%s", claims.Subject), nil
		}

		return token, claims, "", nil
	}

	if claims, ok := token.Claims.(*jwt.RegisteredClaims); ok && claims != nil {
		if s.checkBL(ctx, claims.Subject) {
			return nil, nil, fmt.Sprintf("Token is blacklisted, subj=%s", claims.Subject), nil
		}
		if s.rejectVisitor && strings.HasPrefix(claims.Subject, VisitorSubjectPrefix) {
			return nil, nil, fmt.Sprintf("Visitor sessions are not allowed, subj=%s", claims.Subject), nil
		}

		return token, &CustomClaimType{RegisteredClaims: *claims}, "", nil
	}

	return nil, nil, "Invalid token, can not parse a *jwt.RegisteredClaims", nil
}

// returns: (valid, reason, error)
func (s *StaticKeyJWTValidator) ValidateToken(ctx context.Context, tokenString string) (bool, string, error) {
	token, _, reason, err := s.doValidateToken(ctx, tokenString)
	if err != nil {
		return false, "", fmt.Errorf("internal error, can not validate token: %w", err)
	}
	if token == nil {
		return false, reason, nil
	}
	return true, "", nil
}

type SecretProvider interface {
	// `GetSecret` returns the signing key for the given token.
	// Implementations must handle `token` being `nil` (used during signing).
	GetSecret(token *jwt.Token) (any, error)
}

type StaticSecretProvider struct {
	secret []byte
}

func NewStaticSecretProvider(secret []byte) *StaticSecretProvider {
	return &StaticSecretProvider{secret: secret}
}

func (provider *StaticSecretProvider) GetSecret(_ *jwt.Token) (any, error) {
	return provider.secret, nil
}

type StaticKeyJWTIssuer struct {
	issuer         string
	secretProvider SecretProvider
}

// pass a validity of 0 to use default validity
func NewStaticKeyJWTIssuer(secretProvider SecretProvider, issuer string) *StaticKeyJWTIssuer {

	return &StaticKeyJWTIssuer{
		issuer:         issuer,
		secretProvider: secretProvider,
	}
}

const AudSession string = "session"

const VisitorSubjectPrefix = "visitor:"

func (s *StaticKeyJWTIssuer) IssueToken(ctx context.Context, mapClaims jwt.MapClaims) (string, error) {
	if s.issuer == "" {
		return "", errors.New("issuer is not specified, can not sign token")
	}

	secret, err := s.secretProvider.GetSecret(nil)
	if err != nil {
		return "", fmt.Errorf("failed to get secret to sign the token: %w", err)
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, mapClaims)
	return token.SignedString(secret)
}

func NewMapClaims(customClaims *CustomClaimType) (jwt.MapClaims, error) {
	tmpBuf := &bytes.Buffer{}
	if err := json.NewEncoder(tmpBuf).Encode(customClaims); err != nil {
		return nil, fmt.Errorf("failed to encode registered claims: %w", err)
	}

	var mapClaims jwt.MapClaims
	if err := json.NewDecoder(tmpBuf).Decode(&mapClaims); err != nil {
		return nil, fmt.Errorf("failed to unmarshal map claims: %w", err)
	}

	return mapClaims, nil
}






func extractJWTFromRequest(r *http.Request) string {
	tokenFromCtx := r.Context().Value(pkgutils.CtxKeyJustIssuedJWTToken)
	if tokenFromCtx != nil {
		return tokenFromCtx.(string)
	}

	tokenString := r.Header.Get("Authorization")
	tokenString = strings.TrimPrefix(tokenString, "Bearer ")
	tokenString = strings.TrimPrefix(tokenString, "bearer ")

	if tokenString != "" {
		return tokenString
	}

	if cookie, err := r.Cookie(pkgapicommon.DefaultJWTCookieKey); err == nil {
		return cookie.Value
	}

	return ""
}

func WithBearerToken(next http.Handler, bearerToken string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+bearerToken)
		next.ServeHTTP(w, r)
	})
}

func WithCFServiceToken(next http.Handler, cfCLIId string, cfSec string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("CF-Access-Client-Id", cfCLIId)
		r.Header.Set("CF-Access-Client-Secret", cfSec)
		next.ServeHTTP(w, r)
	})
}

// whiteListSentinelHandler is a zero-width, comparable http.Handler used as a
// marker. A request is considered whitelisted when ServeMux.Handler resolves to
// this exact handler value. Empty structs are comparable, so it may be compared
// with == even when stored in an http.Handler interface (unlike func-backed
// handlers such as http.HandlerFunc, which can only be compared to nil).
type whiteListSentinelHandler struct{}

func (whiteListSentinelHandler) ServeHTTP(http.ResponseWriter, *http.Request) {}

// WithWhiteListJWTAuth wraps nextHandler with a whitelist-based JWT auth
// middleware. Only explicitly permitted paths skip validation; every other
// path must carry a valid JWT.
//
//   - At construction time an http.ServeMux is built and each whiteListPatterns
//     entry is registered against a sentinel handler. Per-request,
//     mux.Handler(r) is consulted; if it resolves to the sentinel the path is
//     whitelisted and passes straight to nextHandler without JWT validation.
//     This reuses Go's built-in mux (1.22+ semantics: exact paths, subtree
//     "/public/", method-scoped "GET /ping", "{wildcards}") rather than a
//     hand-rolled matcher.
//   - Any path not matching the whitelist is validated via
//     jwtValidator.ParseToken (which validates internally) and the resulting
//     claims are injected into the request context (CtxKeyUsername,
//     CtxKeySessionId, CtxKeySubjectId).
//   - On validation failure, if onRejectHandler is nil the request is
//     short-circuited with http.StatusUnauthorized (401) and an
//     "Unauthorized: ..." body; otherwise control is handed to onRejectHandler.
func WithWhiteListJWTAuth(nextHandler http.Handler, jwtValidator JWTValidator, whiteListPatterns []string, onRejectHandler http.Handler) http.Handler {
	if jwtValidator == nil {
		panic("WithWhiteListJWTAuth: jwtValidator must not be nil")
	}

	// Build a ServeMux used purely as a whitelist matcher. Each whitelist
	// pattern is registered against a sentinel handler; a request whose path
	// matches a registered pattern resolves to that sentinel handler via
	// mux.Handler(r). Equality against the sentinel reliably distinguishes a
	// real whitelist hit from the mux's built-in 404/redirect/405 handlers
	// returned for non-matching requests.
	whiteListMux := http.NewServeMux()
	whiteListSentinel := http.Handler(whiteListSentinelHandler{})
	for _, pattern := range whiteListPatterns {
		whiteListMux.Handle(pattern, whiteListSentinel)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Whitelist-based security model: only explicitly permitted paths skip
		// validation. Any path that does not match a whitelist pattern must be
		// validated against a valid JWT.
		if handler, _ := whiteListMux.Handler(r); handler == whiteListSentinel {
			nextHandler.ServeHTTP(w, r)
			return
		}

		tokenString := extractJWTFromRequest(r)

		rejectWithErr := func(additionalMsg string) {
			if onRejectHandler != nil {
				onRejectHandler.ServeHTTP(w, r)
				return
			}

			unAuthErr := pkgutils.ErrorResponse{Error: fmt.Sprintf("Unauthorized: %s", additionalMsg)}
			remote := pkgutils.GetRemoteAddr(r)
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(unAuthErr)
			log.Printf("Remote peer %s is rejected by JWT middleware", remote)
		}

		ctx := r.Context()
		claims, customClaimsAny, err := jwtValidator.ParseToken(ctx, tokenString)
		if err != nil {
			rejectWithErr(err.Error())
			return
		} else if claims == nil {
			rejectWithErr("Got nil token")
			return
		}

		if customClaimsAny != nil {
			if customClaims, ok := customClaimsAny.(*CustomClaimType); ok && customClaims != nil {
				ctx = context.WithValue(ctx, pkgutils.CtxKeyUsername, customClaims.Username)
				if customClaims.Email != "" {
					ctx = context.WithValue(ctx, pkgutils.CtxKeyEmail, customClaims.Email)
				}
			}
		}

		if claims.ID != "" {
			ctx = context.WithValue(ctx, pkgutils.CtxKeySessionId, claims.ID)
		}

		if claims.Subject != "" {
			ctx = context.WithValue(ctx, pkgutils.CtxKeySubjectId, claims.Subject)
		}

		if exp := claims.ExpiresAt; exp != nil{
			ctx = context.WithValue(ctx, pkgutils.CtxKeySessionTTLSecs, exp.Time.Unix())
		}

		r = r.WithContext(ctx)

		nextHandler.ServeHTTP(w, r)
	})
}
