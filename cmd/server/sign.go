package main

import (
	"context"
	"fmt"
	"time"

	pkgauth "personal-site/pkg/auth"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// SignCmd is the "sign" subcommand: it issues a session JWT with the
// deployment's own secret — read through the same --jwt-auth-secret-from-env
// / --jwt-auth-secret-from-file indirection as the server — and prints it on
// stdout. It is the operator-facing way to produce the token a machine
// client's configuration carries, e.g. the <echoBot/> element's jwt
// attribute. The claim set mirrors what the login handlers sign (see e.g.
// VisitorLoginHandler.GetMapClaims): sub and jti are the identity (the
// signalling endpoint rejects identity-less tokens), aud is the session
// audience, and the username claim becomes the client's display name.
type SignCmd struct {
	Subject   string        `name:"sub" required:"" help:"Subject claim: the identity the token represents (e.g. \"bot:echo\")."`
	SessionId string        `name:"jti" help:"Session id claim; a fresh random one is generated when empty."`
	Username  string        `name:"username" help:"Username claim: the display name other clients show for this identity."`
	Validity  time.Duration `name:"validity" default:"720h" help:"How long the token stays valid; 0 issues a token without an expiry claim."`
}

func (cmd *SignCmd) Run(cli *CLI) error {
	if cmd.Validity < 0 {
		return fmt.Errorf("validity must not be negative (%s)", cmd.Validity)
	}
	jwtSec, err := cli.getJWTSecret()
	if err != nil {
		return fmt.Errorf("failed to get JWT secret: %v", err)
	}
	issuer := pkgauth.NewStaticKeyJWTIssuer(pkgauth.NewStaticSecretProvider(jwtSec), cli.JWTIssuer)

	sessionId := cmd.SessionId
	if sessionId == "" {
		sessionId = uuid.NewString()
	}

	customClaims := &pkgauth.CustomClaimType{}
	customClaims.ID = sessionId
	customClaims.Subject = cmd.Subject
	customClaims.Audience = []string{pkgauth.AudSession}
	now := time.Now()
	customClaims.NotBefore = jwt.NewNumericDate(now)
	if cmd.Validity > 0 {
		customClaims.ExpiresAt = jwt.NewNumericDate(now.Add(cmd.Validity))
	}
	customClaims.Username = cmd.Username

	mapClaims, err := pkgauth.NewMapClaims(customClaims)
	if err != nil {
		return fmt.Errorf("failed to build the claims: %w", err)
	}
	token, err := issuer.IssueToken(context.Background(), mapClaims)
	if err != nil {
		return fmt.Errorf("failed to sign the token: %w", err)
	}

	// The token alone goes to stdout so the command composes in a shell;
	// the summary of what was issued goes to stderr.
	expiry := "never"
	if customClaims.ExpiresAt != nil {
		expiry = customClaims.ExpiresAt.Time.Format(time.RFC3339)
	}
	logger.Info("signed a session token",
		"sub", cmd.Subject, "jti", sessionId, "username", cmd.Username, "expires", expiry)
	fmt.Println(token)
	return nil
}
