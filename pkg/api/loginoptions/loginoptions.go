// Package loginoptions serves the login page's configurable IdP list to the
// frontend as JSON at GET /api/login/loginoptions. The list comes from the
// <loginOptions/> section of the server configuration document (see
// pkg/models/serverconfig and serverConfig.xsd in the project root).
package loginoptions

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	pkgapicommon "personal-site/pkg/api/common"
)

// LoginOption is one entry of the login options list: kind identifies the
// IdP type (the frontend uses it to pick the login icon), name is the
// option's unique key, displayName and label build the button caption, and
// loginURL is where the button navigates. It carries the same fields as
// serverconfig.LoginOptionXML under JSON wire names.
type LoginOption struct {
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Label       string `json:"label,omitempty"`
	LoginURL    string `json:"loginURL"`
	// AllowedOrigins restricts the origins the option is served to: when
	// non-empty, the option appears in the response only for requests whose
	// origin (see pkg/api/common RequestOrigin) appears in the list. An
	// empty list means no restriction. It is internal state and never
	// serialized to the frontend.
	AllowedOrigins []string `json:"-"`
}

// ParseAllowedOrigins parses the comma-separated allowedOrigins attribute of
// a <loginOption/> entry into a list of origins. An empty string yields a
// nil (empty) list, i.e. no restriction; surrounding whitespace is ignored.
func ParseAllowedOrigins(s string) []string {
	var origins []string
	for _, part := range strings.Split(s, ",") {
		if origin := strings.TrimSpace(part); origin != "" {
			origins = append(origins, origin)
		}
	}
	return origins
}

// LoginOptionsHandler is an http.Handler that serves the configured login
// options as a JSON array. The list is fixed at construction time, so the
// handler is stateless and safe for concurrent use. Options carrying an
// AllowedOrigins restriction are filtered per request against the request's
// origin.
type LoginOptionsHandler struct {
	options []LoginOption
}

// NewLoginOptionsHandler constructs a LoginOptionsHandler serving the given
// options. A nil slice is served as an empty JSON array, never null.
func NewLoginOptionsHandler(options []LoginOption) *LoginOptionsHandler {
	if options == nil {
		options = []LoginOption{}
	}
	return &LoginOptionsHandler{options: options}
}

func (h *LoginOptionsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(h.optionsForRequest(r))
}

// optionsForRequest returns the options visible to a request arriving from
// the given origin: an option with a non-empty AllowedOrigins list is
// visible only when the request's origin appears in it. The result is never
// nil, so the response stays a JSON array.
func (h *LoginOptionsHandler) optionsForRequest(r *http.Request) []LoginOption {
	origin := pkgapicommon.RequestOrigin(r)
	visible := make([]LoginOption, 0, len(h.options))
	for _, opt := range h.options {
		if len(opt.AllowedOrigins) > 0 && !slices.Contains(opt.AllowedOrigins, origin) {
			continue
		}
		visible = append(visible, opt)
	}
	return visible
}
