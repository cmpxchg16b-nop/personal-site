package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"

	personalsite "personal-site"
	pkglog "personal-site/pkg/log"

	"github.com/alecthomas/kong"
)

// logger is the application-wide structured logger used by the HTTP logging
// middleware. It defaults to slog.Default() (text handler on stderr).
var logger = slog.Default()

type CLI struct {
	Addr string `name:"addr" help:"Listening address." env:"ADDR" default:":8080"`
}

func (cli *CLI) Run() error {
	mux := http.NewServeMux()

	// API routes. The site is a purely client-rendered static export today;
	// these endpoints are the seed of the dynamic features served by this Go
	// backend (profile, contact, …).
	mux.HandleFunc("GET /api/healthz", handleHealthz)
	mux.HandleFunc("GET /api/profile", handleProfile)
	mux.HandleFunc("GET /api/contact", handleContact)

	// Everything else is the embedded Next.js static export.
	mux.Handle("/", personalsite.Handler())

	handler := pkglog.WithLogTraceId(pkglog.WithOverallLog(logger, mux))

	logger.Info("listening", "addr", cli.Addr)
	return http.ListenAndServe(cli.Addr, handler)
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// ProfileResponse mirrors the profile shape the frontend's account area
// (ProfileMenu via useProfile) consumes.
type ProfileResponse struct {
	SessionID string `json:"session_id"`
	SubjectID string `json:"subject_id"`
	Username  string `json:"username"`
	Email     string `json:"email"`
}

// handleProfile serves the caller's profile. There is no login or account
// system yet, so everyone gets the same hard-coded visitor identity; once
// sign-in exists, resolve the request's session here instead.
func handleProfile(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ProfileResponse{
		SessionID: "visitor",
		SubjectID: "visitor",
		Username:  "Visitor",
	})
}

// Contact is one way to reach the site owner: an iconizable kind ("email",
// "github", …) plus a human-readable label and the URL to open.
type Contact struct {
	Kind  string `json:"kind"`
	Label string `json:"label"`
	URL   string `json:"url"`
}

// handleContact serves the site owner's contact entries. Placeholder values;
// load them from a config file or database once real data is available.
func handleContact(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode([]Contact{
		{Kind: "email", Label: "you@example.com", URL: "mailto:you@example.com"},
		{Kind: "github", Label: "github.com/your-handle", URL: "https://github.com/your-handle"},
	})
}

func main() {
	var cli CLI
	kong.Parse(&cli)
	if err := cli.Run(); err != nil {
		logger.Error("server exited", "error", err)
		os.Exit(1)
	}
}
