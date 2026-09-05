// Package homelive serves the WHEP (WebRTC HTTP Egress Protocol) endpoint
// of the home page's live stream — the configuration document's
// <homeLiveWHEPURL/> element (see pkg/models/serverconfig and
// serverConfig.xsd in the project root) — at GET /api/homeLiveWHEPURL. The
// endpoint needs no authentication — it is on the server's JWT whitelist —
// because the home page's Live section renders for every visitor, signed in
// or not: like the ICE servers endpoint (pkg/api/iceservers), it is public
// bootstrap data. An empty URL means the document carries no
// <homeLiveWHEPURL/> element; the frontend then shows no Live section at
// all.
package homelive

import (
	"encoding/json"
	"net/http"
)

// responseBody is the JSON body of GET /api/homeLiveWHEPURL.
type responseBody struct {
	URL string `json:"url"`
}

// HomeLiveHandler is an http.Handler that serves the configured WHEP
// endpoint URL as JSON. The URL is fixed at construction time, so the
// handler is stateless and safe for concurrent use.
type HomeLiveHandler struct {
	url string
}

// NewHomeLiveHandler constructs a HomeLiveHandler serving the given WHEP
// endpoint URL (empty when the document configures none).
func NewHomeLiveHandler(url string) *HomeLiveHandler {
	return &HomeLiveHandler{url: url}
}

func (h *HomeLiveHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(responseBody{URL: h.url})
}
