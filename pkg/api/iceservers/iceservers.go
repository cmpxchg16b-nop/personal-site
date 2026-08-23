// Package iceservers serves the WebRTC ICE server URLs of the server
// configuration document's <iceServer/> entries (see
// pkg/models/serverconfig and serverConfig.xsd in the project root) at
// GET /api/iceServers. The endpoint needs no authentication — it is on
// the server's JWT whitelist — because a visitor needs the ICE servers
// to establish the WebRTC sessions the signalling server brokers. Every
// request is answered with the URLs of the entries matching the
// request's origin (entries without an allowedOrigin match every
// origin), concatenated and deduplicated.
package iceservers

import (
	"encoding/json"
	"net/http"
	"strings"

	pkgapicommon "personal-site/pkg/api/common"
)

// IceServerEntry is one <iceServer/> entry of the configuration
// document: a set of ICE server URLs, restricted to requests from
// AllowedOrigin when that is non-empty.
type IceServerEntry struct {
	URLs          []string
	AllowedOrigin string
}

// ParseURLs parses the comma-separated urls attribute of an
// <iceServer/> entry into a list of URLs. Surrounding whitespace is
// ignored; empty parts are dropped.
func ParseURLs(s string) []string {
	var urls []string
	for _, part := range strings.Split(s, ",") {
		if url := strings.TrimSpace(part); url != "" {
			urls = append(urls, url)
		}
	}
	return urls
}

// responseBody is the JSON body of GET /api/iceServers.
type responseBody struct {
	URLs []string `json:"urls"`
}

// IceServersHandler is an http.Handler that serves the configured ICE
// server URLs as JSON. The entries are fixed at construction time, so
// the handler is stateless and safe for concurrent use.
type IceServersHandler struct {
	entries []IceServerEntry
}

// NewIceServersHandler constructs an IceServersHandler serving the given
// entries.
func NewIceServersHandler(entries []IceServerEntry) *IceServersHandler {
	return &IceServersHandler{entries: entries}
}

func (h *IceServersHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(responseBody{URLs: h.urlsForRequest(r)})
}

// urlsForRequest concatenates the URLs of every entry matching the
// request's origin — an entry with an empty AllowedOrigin matches every
// origin — deduplicating while preserving first-seen order. The result
// is never nil, so the response stays a JSON array.
func (h *IceServersHandler) urlsForRequest(r *http.Request) []string {
	origin := pkgapicommon.RequestOrigin(r)
	seen := make(map[string]struct{})
	urls := make([]string, 0)
	for _, e := range h.entries {
		if e.AllowedOrigin != "" && e.AllowedOrigin != origin {
			continue
		}
		for _, url := range e.URLs {
			if _, dup := seen[url]; dup {
				continue
			}
			seen[url] = struct{}{}
			urls = append(urls, url)
		}
	}
	return urls
}
