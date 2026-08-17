package main

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	personalsite "personal-site"
	pkgapidyn "personal-site/pkg/api/dyn"
	pkgapilinks "personal-site/pkg/api/links"
	pkgapiprofile "personal-site/pkg/api/profile"
	pkglog "personal-site/pkg/log"
	pkgmodelsdyn "personal-site/pkg/models/dyn"
	pkgmodelsshortlink "personal-site/pkg/models/shortlink"
	pkgsession "personal-site/pkg/session"

	"github.com/alecthomas/kong"
)

// logger is the application-wide structured logger used by the HTTP logging
// middleware. It defaults to slog.Default() (text handler on stderr).
var logger = slog.Default()

type CLI struct {
	Addr      string `name:"addr" help:"Listening address." env:"ADDR" default:":8080"`
	ConfigXML string `name:"config-xml" help:"Path to the server configuration XML document (see serverConfig.xsd)." env:"CONFIG_XML" type:"existingfile"`
	// HealthzProbe makes the process a health probe instead of a server: it
	// GETs the running server's /api/healthz on the loopback address and
	// exits 0 on success, non-zero otherwise. The container image's
	// HEALTHCHECK uses it, because the scratch-based image has no shell or
	// curl to probe with.
	HealthzProbe bool `name:"healthz-probe" help:"Probe the server's /api/healthz endpoint and exit."`
}

func (cli *CLI) Run() error {
	if cli.HealthzProbe {
		return runHealthzProbe(cli.Addr)
	}

	mux := http.NewServeMux()

	// API routes. The site is a purely client-rendered static export today;
	// these endpoints are the seed of the dynamic features served by this Go
	// backend (profile, dynamic blog data, …).
	mux.Handle("GET /api/healthz", pkgapidyn.NewHealthzHandler())
	// The profile endpoint reports the caller's identity. There is no login
	// or account system yet, so the session manager supplies the same
	// hard-coded visitor session for every request; once sign-in exists,
	// swap it for the JWT-backed session middleware chain.
	mux.Handle("GET /api/profile", pkgapiprofile.NewProfileHandler(pkgsession.NewStaticVisitorSessionManager()))

	// The dynamic blog data endpoints serve the <dynBlogData/> section of the
	// server configuration document (projects, author contacts). The provider
	// re-reads the document on every request, so edits apply without a
	// restart. Registered unconditionally — with no --config-xml the handler
	// serves empty lists — so the frontend can always rely on it.
	var dynProvider pkgmodelsdyn.DynBlogDataProvider
	if cli.ConfigXML != "" {
		dynProvider = pkgmodelsdyn.NewFSBasedDynBlogData(cli.ConfigXML)
	}
	mux.Handle("/api/dyn/", pkgapidyn.NewDynamicBlogDataHandler(dynProvider))

	// The short link endpoints redirect /links/{id} to the destinations of
	// the <shortlink/> entries of the server configuration document. Like the
	// dynamic blog data provider, the provider re-reads the document on every
	// request, so edits apply without a restart. Mounted without the /api
	// prefix — the paths are meant to be shared — and unconditionally: with
	// no --config-xml every id answers 404.
	var shortLinkProvider pkgmodelsshortlink.ShortLinkDataProvider
	if cli.ConfigXML != "" {
		shortLinkProvider = pkgmodelsshortlink.NewFsShortLinkDataProvider(cli.ConfigXML)
	}
	mux.Handle("/links/", pkgapilinks.NewShortLinkHandler(shortLinkProvider))

	// Everything else is the embedded Next.js static export.
	mux.Handle("/", personalsite.Handler())

	handler := pkglog.WithLogTraceId(pkglog.WithOverallLog(logger, mux))

	logger.Info("listening", "addr", cli.Addr)
	return http.ListenAndServe(cli.Addr, handler)
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

func main() {
	var cli CLI
	kong.Parse(&cli)
	if err := cli.Run(); err != nil {
		logger.Error("server exited", "error", err)
		os.Exit(1)
	}
}
