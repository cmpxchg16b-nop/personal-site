package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	personalsite "personal-site"
	pkgapicomments "personal-site/pkg/api/comments"
	pkgapidyn "personal-site/pkg/api/dyn"
	pkgapilinks "personal-site/pkg/api/links"
	pkgapiloginoauth2github "personal-site/pkg/api/login/oauth2/github"
	pkgapiloginoidcgeneral "personal-site/pkg/api/login/oidc/general"
	pkgapiloginvisitor "personal-site/pkg/api/login/visitor"
	pkgapiloginoptions "personal-site/pkg/api/loginoptions"
	pkgapilogout "personal-site/pkg/api/logout"
	pkgapiprofile "personal-site/pkg/api/profile"
	pkgapiwebsocketss "personal-site/pkg/api/websocket_ss"
	pkgauth "personal-site/pkg/auth"
	pkgcookie "personal-site/pkg/cookie"
	pkglog "personal-site/pkg/log"
	pkgmodelscomment "personal-site/pkg/models/comment"
	pkgmodelsdyn "personal-site/pkg/models/dyn"
	pkgmodelsserverconfig "personal-site/pkg/models/serverconfig"
	pkgmodelsshortlink "personal-site/pkg/models/shortlink"
	pkgmodelsss "personal-site/pkg/models/ss"
	pkgsession "personal-site/pkg/session"

	"github.com/alecthomas/kong"
	"github.com/joho/godotenv"
)

// logger is the application-wide structured logger used by the HTTP logging
// middleware. It defaults to slog.Default() (text handler on stderr).
var logger = slog.Default()

type CLI struct {
	Addr      string `name:"addr" help:"Listening address." env:"ADDR" default:":8080"`
	ConfigXML string `name:"config-xml" help:"Path to the server configuration XML document (see serverConfig.xsd)." env:"CONFIG_XML" type:"existingfile"`
	// SSAging is the subscriber aging interval of the in-memory signalling
	// provider: a registration expires when the subscriber is idle longer
	// than this.
	SSAging time.Duration `name:"ss-aging" help:"Signalling subscriber aging interval." env:"SS_AGING" default:"10s"`
	// HealthzProbe makes the process a health probe instead of a server: it
	// GETs the running server's /api/healthz on the loopback address and
	// exits 0 on success, non-zero otherwise. The container image's
	// HEALTHCHECK uses it, because the scratch-based image has no shell or
	// curl to probe with.
	HealthzProbe                bool          `name:"healthz-probe" help:"Probe the server's /api/healthz endpoint and exit."`
	JWTAuthSecretFromEnv        string        `name:"jwt-auth-secret-from-env" help:"Name of the environment variable that contains the JWT secret" default:"JWT_SECRET"`
	JWTAuthSecretFromFile       string        `name:"jwt-auth-secret-from-file" help:"Path to the file that contains the JWT secret"`
	SubjectBlacklistTxtPath     string        `name:"subj-blacklist-path" help:"Path to the blacklist text file, one subject id per a line"`
	RejectVisitor               bool          `name:"reject-visitor" help:"Reject requests from visitors (subjects with the 'visitor:' prefix)" default:"false"`
	VisitorSessionValidity      time.Duration `name:"validity-of-visitor-session" help:"Validity of visitor session" default:"168h"`
	VisitorSessionTicketGenIntv time.Duration `name:"visitor-jwt-ticket-gen-intv" help:"We issue visitor token based on some ticket generator, this is the interval of how fast it generate tickets" default:"1s"`
	JWTIssuer                   string        `help:"The issuer of the JWT token" default:"personal-site"`
	NonceLifespan               time.Duration `name:"nonce-lifespan" help:"Lifespan of the OAuth nonce." default:"10m"`
}

func (cli *CLI) Run() error {
	if cli.HealthzProbe {
		return runHealthzProbe(cli.Addr)
	}

	ctx := context.Background()

	// The global server configuration document is loaded once here; its
	// sections are consumed by the wiring below (login options, generic OIDC
	// login providers, allowed origins). The dyn blog data and short link
	// providers below re-read the same document on every request instead, so
	// their edits apply without a restart.
	var serverCfg *pkgmodelsserverconfig.ServerConfigXML
	if cli.ConfigXML != "" {
		cfg, err := pkgmodelsserverconfig.LoadServerConfig(cli.ConfigXML)
		if err != nil {
			return err
		}
		serverCfg = cfg
	}

	// The <allowedOrigin/> entries of the configuration document are shared
	// by every OAuth2/OIDC login handler below: a handler whose redirect URL
	// is relative prepends the request's origin only when it appears here.
	var allowedOrigins []string
	if serverCfg != nil {
		allowedOrigins = serverCfg.AllowedOrigins
	}

	// The JWT secret backs both the session token issuer/validator and the
	// OAuth/OIDC nonce issuer, so it is required at startup.
	jwtSec, err := cli.getJWTSecret()
	if err != nil {
		return fmt.Errorf("failed to get JWT secret: %v", err)
	}
	keyProvider := pkgauth.NewStaticSecretProvider(jwtSec)
	var blProvider pkgauth.BlackListProvider
	if blTxtPath := cli.SubjectBlacklistTxtPath; blTxtPath != "" {
		txtblProvider, err := pkgauth.NewTextBasedBlackListProvider(blTxtPath)
		if err != nil {
			return fmt.Errorf("failed to load blacklist file: %v", err)
		}
		blProvider = txtblProvider
	} else {
		blProvider = pkgauth.NewNullBlackListProvider()
	}
	tokenIssuer := pkgauth.NewStaticKeyJWTIssuer(keyProvider, cli.JWTIssuer)
	tickIssuer := pkgauth.NewSharedTickingTicketGenerator(cli.VisitorSessionTicketGenIntv)
	tickIssuer.Run(ctx)
	cookieBuilder := &pkgcookie.SimpleCookieBuilder{}

	// The session manager resolves the request-scoped session object from the
	// values the JWT auth middleware injects into the request context.
	sm := pkgsession.NewOnMemorySessionManager()

	// API routes. The site is a purely client-rendered static export today;
	// these endpoints are the seed of the dynamic features served by this Go
	// backend (profile, dynamic blog data, …).
	muxHandlerDyn := http.NewServeMux()
	muxHandlerDyn.Handle("GET /api/healthz", pkgapidyn.NewHealthzHandler())

	// The profile endpoint reports the caller's identity. Requests reach it
	// through the JWT-backed session middleware chain (see below), so the
	// session it serves is the one the caller's login produced.
	muxHandlerDyn.Handle("/api/profile", pkgapiprofile.NewProfileHandler(sm))

	// The dynamic blog data endpoints serve the <dynBlogData/> section of the
	// server configuration document (projects, author contacts). The provider
	// re-reads the document on every request, so edits apply without a
	// restart. Registered unconditionally — with no --config-xml the handler
	// serves empty lists — so the frontend can always rely on it.
	var dynProvider pkgmodelsdyn.DynBlogDataProvider
	if cli.ConfigXML != "" {
		dynProvider = pkgmodelsdyn.NewFSBasedDynBlogData(cli.ConfigXML)
	}
	muxHandlerDyn.Handle("/api/dyn/", pkgapidyn.NewDynamicBlogDataHandler(dynProvider))

	// The comments endpoints store and serve channel comments. Reading is
	// open to everyone (the GET routes are on the JWT whitelist below);
	// appending requires a session — the server takes the author from the
	// caller's session identity, never from the request body — and comments
	// live in process memory only, so they are lost on restart.
	muxHandlerDyn.Handle("/api/comments/", pkgapicomments.NewCommentsHandler(pkgmodelscomment.NewOnMemoryCommentProvider(), sm))

	// The signalling endpoint upgrades client connections to WebSocket and
	// bridges them to the signalling service: visitors discover each other
	// and broker WebRTC sessions through it. Client identity comes from the
	// caller's session (the path is NOT on the JWT whitelist below —
	// browsers reach it with the session cookie), and registrations live in
	// process memory only, so they are lost on restart. The provider and
	// hub goroutines run for the process lifetime. The upgrader trusts the
	// configuration's allowed origins alongside same-origin requests: in
	// development the browser's origin is the frontend dev server proxying
	// /api/* here, which the default origin check would reject.
	ssHandler := pkgapiwebsocketss.NewWebSocketSSHandler(
		context.Background(), pkgmodelsss.NewSimpleOnMemorySSProviderWithAging(cli.SSAging), sm)
	ssHandler.Upgrader.CheckOrigin = pkgapiwebsocketss.CheckOriginAllowing(allowedOrigins)
	muxHandlerDyn.Handle("/api/ss/ws", ssHandler)

	// /api/logout is on the JWT whitelist below, so the handler also runs for
	// requests whose token is already expired or invalid — clearing cookies
	// must never depend on a still-valid session.
	muxHandlerDyn.Handle("/api/logout", pkgapilogout.NewLogoutHandler(""))

	// Visitor login issues a short-setup anonymous session: it waits for the
	// shared ticking ticket generator (which paces registrations), signs a
	// visitor JWT and sets it as the session cookie.
	visitorLoginHandler := pkgapiloginvisitor.NewVisitorLoginHandler(
		tokenIssuer,
		cli.VisitorSessionValidity,
		tickIssuer,
		allowedOrigins,
		cookieBuilder,
	)
	muxHandlerDyn.Handle("/api/login/visitor", visitorLoginHandler)

	// The login options endpoint serves the <loginOptions/> section of the
	// server configuration document to the login page. It is registered
	// unconditionally (an empty list when unconfigured) so the frontend can
	// always rely on it; /api/login/ is on the JWT whitelist below, so
	// logged-out visitors can reach it.
	var loginOptions []pkgapiloginoptions.LoginOption
	if serverCfg != nil {
		for _, opt := range serverCfg.LoginOptions.Options {
			loginOptions = append(loginOptions, pkgapiloginoptions.LoginOption{
				Kind:           opt.Kind,
				Name:           opt.Name,
				DisplayName:    opt.DisplayName,
				Label:          opt.Label,
				LoginURL:       opt.LoginURL,
				AllowedOrigins: pkgapiloginoptions.ParseAllowedOrigins(opt.AllowedOrigins),
			})
		}
	}
	muxHandlerDyn.Handle("/api/login/loginoptions", pkgapiloginoptions.NewLoginOptionsHandler(loginOptions))

	// Github OAuth login. The handler is wired only when the configuration
	// document carries a <githubOAuthLogin/> element with a client id, since
	// the OAuth flow requires app credentials and a redirect URL to function.
	// The nonce issuer is signed with the same static key used for JWT auth.
	if serverCfg != nil && serverCfg.GithubOAuthLogin != nil && serverCfg.GithubOAuthLogin.ClientId != "" {
		ghCfg := serverCfg.GithubOAuthLogin
		sessionLifespan, err := pkgmodelsserverconfig.ParseSessionLifespan(ghCfg.SessionLifespan, 168*time.Hour)
		if err != nil {
			return fmt.Errorf("github OAuth login: %w", err)
		}
		nonceIssuer := &pkgauth.StaticKeyNonceIssuer{
			NonceLifespan:  cli.NonceLifespan,
			SecretProvider: keyProvider,
		}
		githubLoginHandler := pkgapiloginoauth2github.NewGithubOAuthLoginHandler(
			sessionLifespan,
			ghCfg.ClientId,
			ghCfg.AppSecret,
			ghCfg.RedirectURL,
			ghCfg.LoginPage,
			ghCfg.Scope,
			ghCfg.TokenEndpoint,
			ghCfg.LoginSuccessRedirectURL,
			allowedOrigins,
			tokenIssuer,
			nonceIssuer,
			cookieBuilder,
		)
		muxHandlerDyn.Handle("/api/login/oauth2/github", githubLoginHandler)
		muxHandlerDyn.Handle("/api/login/oauth2/github/", githubLoginHandler)
		logger.Info("registered Github login handler", "path", "/api/login/oauth2/github/")
	}

	// Generic OIDC providers loaded from the <oidcLoginOptions/> section of
	// the server configuration document (loaded above). Each
	// <oidcLoginOption/> with a non-empty issuerURL registers a
	// GenericOIDCLoginHandler at /api/login/oidc/{providerName}[/...].
	// Entries with an empty issuerURL are skipped so the shipped sample file
	// can be used as-is.
	if serverCfg != nil {
		nonceIssuer := &pkgauth.StaticKeyNonceIssuer{
			NonceLifespan:  cli.NonceLifespan,
			SecretProvider: keyProvider,
		}
		for _, opt := range serverCfg.OIDCLoginOptions.Options {
			if opt.IssuerURL == "" {
				continue
			}
			providerName := opt.ProviderName
			if providerName == "" {
				providerName = "oidc"
			}
			sessionLifespan, err := pkgmodelsserverconfig.ParseSessionLifespan(opt.SessionLifespan, 168*time.Hour)
			if err != nil {
				return fmt.Errorf("OIDC provider %q: %w", providerName, err)
			}
			handler := pkgapiloginoidcgeneral.NewGenericOIDCLoginHandler(
				sessionLifespan,
				opt.ProviderName,
				opt.IssuerURL,
				opt.ClientId,
				opt.ClientSecret,
				opt.RedirectURL,
				opt.Scope,
				opt.LoginSuccessRedirectURL,
				allowedOrigins,
				tokenIssuer,
				nonceIssuer,
				cookieBuilder,
			)
			base := "/api/login/oidc/" + providerName
			muxHandlerDyn.Handle(base, handler)
			muxHandlerDyn.Handle(base+"/", handler)
			logger.Info("registered OIDC login handler", "provider", providerName, "path", base+"/", "issuer", opt.IssuerURL)
		}
	}

	jwtValidator := pkgauth.NewStaticKeyJWTValidator(keyProvider, blProvider, cli.RejectVisitor)

	var authRejectHandler http.Handler = nil

	// The JWT whitelist: paths that must stay reachable without a session.
	// The login endpoints and logout obviously so; healthz (the container
	// probe carries no credentials); the public blog data keeps its current
	// unauthenticated behavior. Comments are whitelisted for reads only:
	// appending one requires a session.
	whList := []string{
		"/api/login",
		"/api/login/",
		"/api/logout",
		"/api/healthz",
		"/api/dyn",
		"/api/dyn/",
		"GET /api/comments",
		"GET /api/comments/",
	}

	// Detailed per-request middleware applies only to dynamic (api) endpoints.
	var dynHandler http.Handler = muxHandlerDyn
	dynHandler = pkglog.WithSessionAwaredLog(logger, sm, dynHandler)
	dynHandler = pkgsession.WithSessionId(dynHandler, sm)
	dynHandler = pkgauth.WithWhiteListJWTAuth(dynHandler, jwtValidator, whList, authRejectHandler)
	dynHandler = pkglog.WithHTTPLog(logger, dynHandler)

	// The short link endpoints redirect /links/{id} to the destinations of
	// the <shortlink/> entries of the server configuration document. Like the
	// dynamic blog data provider, the provider re-reads the document on every
	// request, so edits apply without a restart. Mounted without the /api
	// prefix — the paths are meant to be shared, so they stay outside the
	// JWT-protected subtree — and unconditionally: with no --config-xml
	// every id answers 404.
	var shortLinkProvider pkgmodelsshortlink.ShortLinkDataProvider
	if cli.ConfigXML != "" {
		shortLinkProvider = pkgmodelsshortlink.NewFsShortLinkDataProvider(cli.ConfigXML)
	}

	muxHandlerGeneral := http.NewServeMux()
	muxHandlerGeneral.Handle("/api/", dynHandler)
	muxHandlerGeneral.Handle("/links/", pkgapilinks.NewShortLinkHandler(shortLinkProvider))
	// Everything else is the embedded Next.js static export.
	muxHandlerGeneral.Handle("/", personalsite.Handler())

	// Trace id and overall log wrap the general mux so both static and dynamic
	// requests get a trace id and an overall log line; dynamic requests
	// additionally get the detailed HTTP log above.
	handler := pkglog.WithLogTraceId(pkglog.WithOverallLog(logger, muxHandlerGeneral))

	logger.Info("listening", "addr", cli.Addr)
	return http.ListenAndServe(cli.Addr, handler)
}

func (cli *CLI) getJWTSecret() ([]byte, error) {
	return getJWTSecFromSomewhere(cli.JWTAuthSecretFromEnv, cli.JWTAuthSecretFromFile)
}

// dotEnvFiles are the dotenv files loaded at startup, in decreasing order
// of precedence: godotenv.Load never overrides a variable that is already
// set, so .env.local wins over .env, and both lose to the real environment.
var dotEnvFiles = []string{".env.local", ".env"}

// loadDotEnvFiles loads the conventional dotenv files (.env.local, .env)
// into the process environment. It runs before kong.Parse so that kong's
// env-tagged CLI fields observe the variables defined there. Missing files
// are skipped; a failure to load an existing file is fatal.
func loadDotEnvFiles() {
	var existing []string
	for _, f := range dotEnvFiles {
		if _, err := os.Stat(f); err == nil {
			existing = append(existing, f)
		}
	}
	if len(existing) == 0 {
		return
	}
	if err := godotenv.Load(existing...); err != nil {
		logger.Error("failed to load dot env files", "files", existing, "err", err)
		os.Exit(1)
	}
	logger.Info("loaded dot env files", "files", existing)
}

// runHealthzProbe GETs the server's health endpoint over the loopback
// interface and reports the outcome via the process exit code. addr is the
// server's listening address (same value as --addr); only its port is used.
func runHealthzProbe(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("health probe: invalid listening address %q: %w", addr, err)
	}
	url := "http://127.0.0.1:" + port + "/api/healthz"
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("health probe: %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health probe: %s returned %s", url, resp.Status)
	}
	return nil
}

func getJWTSecFromSomewhere(envVar string, filePath string) ([]byte, error) {
	if envVar != "" {
		secret := os.Getenv(envVar)
		if secret == "" {
			return nil, fmt.Errorf("JWT secret is not set in environment variable %s", envVar)
		}
		return []byte(secret), nil
	}

	if filePath != "" {
		secret, err := os.ReadFile(filePath)
		if err != nil {
			return nil, fmt.Errorf("failed to read JWT secret file %s: %v", filePath, err)
		}
		if len(secret) == 0 {
			return nil, fmt.Errorf("JWT secret file %s is empty", filePath)
		}
		return secret, nil
	}

	return nil, fmt.Errorf("no JWT secret is set")
}

func main() {
	loadDotEnvFiles()

	var cli CLI
	kong.Parse(&cli)
	if err := cli.Run(); err != nil {
		logger.Error("server exited", "error", err)
		os.Exit(1)
	}
}
