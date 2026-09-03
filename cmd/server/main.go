package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	personalsite "personal-site"
	pkgapicomments "personal-site/pkg/api/comments"
	pkgapidyn "personal-site/pkg/api/dyn"
	pkgapiiceservers "personal-site/pkg/api/iceservers"
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
	"personal-site/pkg/models/audiosource"
	pkgmodelscomment "personal-site/pkg/models/comment"
	pkgmodelsdyn "personal-site/pkg/models/dyn"
	pkgmodelsserverconfig "personal-site/pkg/models/serverconfig"
	pkgmodelsshortlink "personal-site/pkg/models/shortlink"
	pkgmodelsss "personal-site/pkg/models/ss"
	"personal-site/pkg/rtc"
	"personal-site/pkg/rtc/echobot"
	"personal-site/pkg/rtc/musicbot"
	pkgsession "personal-site/pkg/session"

	"github.com/alecthomas/kong"
	"github.com/joho/godotenv"
	"github.com/pion/webrtc/v4"
)

// logger is the application-wide structured logger used by the HTTP logging
// middleware. It defaults to slog.Default() (text handler on stderr).
var logger = slog.Default()

// CLI is the command line of the server binary: two subcommands sharing
// the JWT secret indirection. Root flags must precede the subcommand word.
type CLI struct {
	JWTAuthSecretFromEnv  string `name:"jwt-auth-secret-from-env" help:"Name of the environment variable that contains the JWT secret" default:"JWT_SECRET"`
	JWTAuthSecretFromFile string `name:"jwt-auth-secret-from-file" help:"Path to the file that contains the JWT secret"`
	JWTIssuer             string `help:"The issuer of the JWT token" default:"personal-site"`

	Serve ServeCmd `cmd:"" help:"Run the server."`
	Sign  SignCmd  `cmd:"" help:"Sign a session JWT with the deployment's secret, print it on stdout, and exit."`
}

// ServeCmd is the "serve" subcommand: the server itself. Its flag set is
// what the binary took before the subcommand split.
type ServeCmd struct {
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
	SubjectBlacklistTxtPath     string        `name:"subj-blacklist-path" help:"Path to the blacklist text file, one subject id per a line"`
	RejectVisitor               bool          `name:"reject-visitor" help:"Reject requests from visitors (subjects with the 'visitor:' prefix)" default:"false"`
	VisitorSessionValidity      time.Duration `name:"validity-of-visitor-session" help:"Validity of visitor session" default:"168h"`
	VisitorSessionTicketGenIntv time.Duration `name:"visitor-jwt-ticket-gen-intv" help:"We issue visitor token based on some ticket generator, this is the interval of how fast it generate tickets" default:"1s"`
	NonceLifespan               time.Duration `name:"nonce-lifespan" help:"Lifespan of the OAuth nonce." default:"10m"`
}

// Run serves. cli carries the root flags shared with the other subcommands.
func (cmd *ServeCmd) Run(cli *CLI) error {
	if cmd.HealthzProbe {
		return runHealthzProbe(cmd.Addr)
	}

	ctx := context.Background()

	// The global server configuration document is loaded once here; its
	// sections are consumed by the wiring below (login options, generic OIDC
	// login providers, allowed origins). The dyn blog data and short link
	// providers below re-read the same document on every request instead, so
	// their edits apply without a restart.
	var serverCfg *pkgmodelsserverconfig.ServerConfigXML
	if cmd.ConfigXML != "" {
		cfg, err := pkgmodelsserverconfig.LoadServerConfig(cmd.ConfigXML)
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
	if blTxtPath := cmd.SubjectBlacklistTxtPath; blTxtPath != "" {
		txtblProvider, err := pkgauth.NewTextBasedBlackListProvider(blTxtPath)
		if err != nil {
			return fmt.Errorf("failed to load blacklist file: %v", err)
		}
		blProvider = txtblProvider
	} else {
		blProvider = pkgauth.NewNullBlackListProvider()
	}
	tokenIssuer := pkgauth.NewStaticKeyJWTIssuer(keyProvider, cli.JWTIssuer)
	tickIssuer := pkgauth.NewSharedTickingTicketGenerator(cmd.VisitorSessionTicketGenIntv)
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
	if cmd.ConfigXML != "" {
		dynProvider = pkgmodelsdyn.NewFSBasedDynBlogData(cmd.ConfigXML)
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
		context.Background(), pkgmodelsss.NewSimpleOnMemorySSProviderWithAging(cmd.SSAging), sm)
	ssHandler.Upgrader.CheckOrigin = pkgapiwebsocketss.CheckOriginAllowing(allowedOrigins)
	muxHandlerDyn.Handle("/api/ss/ws", ssHandler)

	// The ICE servers endpoint serves the <iceServer/> entries of the
	// configuration document: visitors need them to establish the WebRTC
	// sessions the signalling endpoint above brokers. Registered
	// unconditionally — an empty list when unconfigured — and reachable
	// without a session (it is on the JWT whitelist below).
	var iceServerEntries []pkgapiiceservers.IceServerEntry
	if serverCfg != nil {
		for _, e := range serverCfg.IceServers {
			iceServerEntries = append(iceServerEntries, pkgapiiceservers.IceServerEntry{
				URLs:          pkgapiiceservers.ParseURLs(e.URLs),
				AllowedOrigin: e.AllowedOrigin,
			})
		}
	}
	muxHandlerDyn.Handle("GET /api/iceServers", pkgapiiceservers.NewIceServersHandler(iceServerEntries))

	// /api/logout is on the JWT whitelist below, so the handler also runs for
	// requests whose token is already expired or invalid — clearing cookies
	// must never depend on a still-valid session.
	muxHandlerDyn.Handle("/api/logout", pkgapilogout.NewLogoutHandler(""))

	// Visitor login issues a short-setup anonymous session: it waits for the
	// shared ticking ticket generator (which paces registrations), signs a
	// visitor JWT and sets it as the session cookie.
	visitorLoginHandler := pkgapiloginvisitor.NewVisitorLoginHandler(
		tokenIssuer,
		cmd.VisitorSessionValidity,
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
			NonceLifespan:  cmd.NonceLifespan,
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
			NonceLifespan:  cmd.NonceLifespan,
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

	jwtValidator := pkgauth.NewStaticKeyJWTValidator(keyProvider, blProvider, cmd.RejectVisitor)

	// The built-in bots: headless RTC peers (pkg/rtc/echobot,
	// pkg/rtc/musicbot) living in this process as plain signalling
	// clients of the WebSocket endpoint their <echoBot/> or <musicBot/>
	// configuration points at — typically this server's own /api/ss/ws.
	// Wired only when the element carries a url and a jwt (so a sample
	// element with empty values can ship in the document unused); the
	// token doubles as the bot's whole identity: sub/jti become its
	// signalling address, and the username claim becomes its display name
	// — the endpoint stamps it onto the bot's registration, like every
	// client's. The bots live for the process lifetime, re-establishing
	// the connection when it drops.
	if serverCfg != nil && serverCfg.EchoBot != nil &&
		serverCfg.EchoBot.URL != "" && serverCfg.EchoBot.JWT != "" {
		if err := startEchoBot(ctx, serverCfg.EchoBot); err != nil {
			return fmt.Errorf("echo bot: %w", err)
		}
	}
	if serverCfg != nil && serverCfg.MusicBot != nil &&
		serverCfg.MusicBot.URL != "" && serverCfg.MusicBot.JWT != "" {
		if err := startMusicBot(ctx, serverCfg.MusicBot, filepath.Dir(cmd.ConfigXML)); err != nil {
			return fmt.Errorf("music bot: %w", err)
		}
	}

	var authRejectHandler http.Handler = nil

	// The JWT whitelist: paths that must stay reachable without a session.
	// The login endpoints and logout obviously so; healthz (the container
	// probe carries no credentials); the ICE server list is public WebRTC
	// bootstrap data; the public blog data keeps its current
	// unauthenticated behavior. Comments are whitelisted for reads only:
	// appending one requires a session.
	whList := []string{
		"/api/login",
		"/api/login/",
		"/api/logout",
		"/api/healthz",
		"GET /api/iceServers",
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
	if cmd.ConfigXML != "" {
		shortLinkProvider = pkgmodelsshortlink.NewFsShortLinkDataProvider(cmd.ConfigXML)
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

	logger.Info("listening", "addr", cmd.Addr)
	return http.ListenAndServe(cmd.Addr, handler)
}

func (cli *CLI) getJWTSecret() ([]byte, error) {
	return getJWTSecFromSomewhere(cli.JWTAuthSecretFromEnv, cli.JWTAuthSecretFromFile)
}

// defaultBotReconnectInterval is how long a bot waits before
// re-establishing a lost signalling connection, unless the
// configuration's bot element says otherwise.
const defaultBotReconnectInterval = 5 * time.Second

// startEchoBot wires the built-in echo bot (pkg/rtc/echobot).
func startEchoBot(ctx context.Context, cfg *pkgmodelsserverconfig.BotClientXML) error {
	return startBotClient(ctx, "echo bot", cfg, nil, func(client *rtc.HeadlessRTCClient) {
		echobot.New(client, echobot.Configuration{Logger: logger})
	})
}

// startMusicBot wires the built-in music bot (pkg/rtc/musicbot). The
// element's audioSource children become the bot's songbook: each entry
// is converted to the audiosource model (configDir is what a relative
// url resolves against — the configuration document's directory) and
// validated; an entry the model rejects fails the startup. The sample
// data itself loads lazily, when a call first plays the song.
func startMusicBot(ctx context.Context, cfg *pkgmodelsserverconfig.MusicBotXML, configDir string) error {
	sources := make([]*audiosource.AudioSourceData, 0, len(cfg.AudioSources))
	for _, entry := range cfg.AudioSources {
		src, err := entry.AudioSourceData(configDir)
		if err != nil {
			return err
		}
		sources = append(sources, src)
	}
	return startBotClient(ctx, "music bot", &cfg.BotClientXML, stereoOpusPCFactory(pkgapiiceservers.ParseURLs(cfg.IceServers)), func(client *rtc.HeadlessRTCClient) {
		musicbot.New(client, musicbot.Configuration{Logger: logger, AudioSources: sources})
	})
}

// stereoOpusPCFactory builds the music bot's peer-connection factory:
// pion's default codecs, save that opus negotiates the stereo=1 fmtp
// parameter (RFC 7587). Without it the peer connections advertise
// mono-only reception, so a browser receiving the bot's stereo music
// configures a mono opus decoder and downmixes the song on the fly —
// wide stereo mixes lose content to the collapse. With stereo=1 the
// offer carries it and every browser answers it, so the song keeps its
// channels. The music bot declines video calls, yet the default video
// codecs stay registered so a video m-line the peer proposes still
// negotiates instead of being rejected out of hand.
func stereoOpusPCFactory(iceServers []string) func() (*webrtc.PeerConnection, error) {
	return func() (*webrtc.PeerConnection, error) {
		m := &webrtc.MediaEngine{}
		// The audio defaults, opus upgraded to stereo; the video defaults
		// verbatim (pion's list, mirrored here because it cannot be amended
		// after registration).
		audio := []webrtc.RTPCodecParameters{
			{RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
				SDPFmtpLine: "minptime=10;useinbandfec=1;stereo=1",
			}, PayloadType: 111},
			{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeG722, ClockRate: 8000}, PayloadType: 9},
			{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: 8000}, PayloadType: 0},
			{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMA, ClockRate: 8000}, PayloadType: 8},
		}
		for _, c := range audio {
			if err := m.RegisterCodec(c, webrtc.RTPCodecTypeAudio); err != nil {
				return nil, fmt.Errorf("register audio codec %s: %w", c.MimeType, err)
			}
		}
		if err := m.RegisterDefaultCodecs(); err != nil {
			// The video codecs: the audio ones above are already
			// registered, and registration is idempotent per payload type.
			return nil, fmt.Errorf("register default codecs: %w", err)
		}
		api := webrtc.NewAPI(webrtc.WithMediaEngine(m))
		config := webrtc.Configuration{}
		if len(iceServers) > 0 {
			config.ICEServers = []webrtc.ICEServer{{URLs: iceServers}}
		}
		return api.NewPeerConnection(config)
	}
}

// startBotClient wires one built-in bot: a pkg/rtc bot (attached by wire)
// on a pkg/rtc HeadlessRTCClient, driven over a pkg/rtc
// WebSocketSignallingTransport to the endpoint cfg.URL points at, with
// cfg.JWT as the session credential, sent as a bearer token (the
// endpoint is not on the JWT whitelist). The bot never inspects the
// token itself: validating it is the server's job, so a bad token
// surfaces as the reconnect loop's logged 401s. The bot runs until ctx
// is done, re-establishing the signalling connection every
// reconnectInterval when it drops.
func startBotClient(ctx context.Context, name string, cfg *pkgmodelsserverconfig.BotClientXML, pcFactory func() (*webrtc.PeerConnection, error), wire func(client *rtc.HeadlessRTCClient)) error {
	keepAliveInterval, err := pkgmodelsserverconfig.ParseDuration(cfg.KeepAliveInterval, 0)
	if err != nil {
		return fmt.Errorf("keepAliveInterval: %w", err)
	}
	memberListInterval, err := pkgmodelsserverconfig.ParseDuration(cfg.MemberListInterval, 0)
	if err != nil {
		return fmt.Errorf("memberListInterval: %w", err)
	}
	replyTimeout, err := pkgmodelsserverconfig.ParseDuration(cfg.ReplyTimeout, 0)
	if err != nil {
		return fmt.Errorf("replyTimeout: %w", err)
	}
	reconnectInterval, err := pkgmodelsserverconfig.ParseDuration(cfg.ReconnectInterval, defaultBotReconnectInterval)
	if err != nil {
		return fmt.Errorf("reconnectInterval: %w", err)
	}

	// The token doubles as the bot's whole identity: the endpoint stamps
	// its subject/session onto the bot's events and its username claim
	// onto the registration's display name — like every client's. The
	// bot never inspects the token itself: validating it is the server's
	// job, and only the server knows how.
	// An empty iceServers attribute means exactly that: no ICE servers —
	// the bot's peer connections run on host candidates alone.
	iceServers := pkgapiiceservers.ParseURLs(cfg.IceServers)

	client, err := rtc.NewHeadlessRTCClient(rtc.PerfectNegotiatorFactory, rtc.RTCClientConfiguration{
		ChannelId:          pkgmodelsss.ChannelId(cfg.ChannelId),
		SubscriberId:       pkgmodelsss.SubscriberId(cfg.SubscriberId),
		ICEServers:         iceServers,
		KeepAliveInterval:  keepAliveInterval,
		MemberListInterval: memberListInterval,
		ReplyTimeout:       replyTimeout,
		NewPeerConnection:  pcFactory,
		Logger:             logger,
	})
	if err != nil {
		return err
	}
	// The bot registers its dcmsg/dcbin handlers on the client at
	// construction; it needs no further driving.
	wire(client)

	transport := &rtc.WebSocketSignallingTransport{
		URL:    cfg.URL,
		Header: http.Header{"Authorization": []string{"Bearer " + cfg.JWT}},
	}
	go func() {
		for ctx.Err() == nil {
			// One channel pair per attempt: a transport run closes its
			// outbound side when it returns, which is how the client
			// observes the drop.
			toSS := make(chan *pkgmodelsss.SignallingEvent, 64)
			fromSS := make(chan *pkgmodelsss.SignallingEvent, 64)
			attemptCtx, stop := context.WithCancel(ctx)
			transportErr := make(chan error, 1)
			go func() { transportErr <- transport.Run(attemptCtx, toSS, fromSS) }()
			runErr := client.Run(attemptCtx, fromSS, toSS)
			stop()
			terr := <-transportErr
			if ctx.Err() != nil {
				return
			}
			logger.Warn(name+": signalling connection ended, reconnecting",
				"clientErr", runErr, "transportErr", terr, "retryIn", reconnectInterval)
			select {
			case <-time.After(reconnectInterval):
			case <-ctx.Done():
			}
		}
	}()
	logger.Info(name+" wired",
		"url", cfg.URL, "channelId", cfg.ChannelId,
		"subscriberId", cfg.SubscriberId)
	return nil
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
	ctx := kong.Parse(&cli)
	// kong's dispatch: RunNode binds the targets of the selected path, so
	// each subcommand's Run(cli *CLI) receives the root flags.
	ctx.FatalIfErrorf(ctx.Run())
}
