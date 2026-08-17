// Package links serves the site's short links: stable short paths under
// /links/ that redirect to longer or changing destinations. The
// destinations come from a pkg/models/shortlink ShortLinkDataProvider,
// typically re-read from the <shortlink/> entries of the server
// configuration document on every request.
//
// Unlike the site's other dynamic features, the subtree is mounted without
// the /api prefix: short links are meant to be shared with humans, so the
// paths stay short.
package links

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	pkgmodelsshortlink "personal-site/pkg/models/shortlink"
	pkgutils "personal-site/pkg/utils"
)

// ShortLinkHandler is an http.Handler that redirects short link requests to
// their destinations, routing the /links/ subtree internally:
//
//	GET /links/{id}  302 redirect to the short link's destination
//
// The handler asks its provider for the destination on every request, so a
// provider that re-reads its source (e.g. FsShortLinkDataProvider) serves
// edits without a server restart. It is stateless and safe for concurrent
// use.
type ShortLinkHandler struct {
	provider pkgmodelsshortlink.ShortLinkDataProvider
	mux      *http.ServeMux
}

// NewShortLinkHandler constructs a ShortLinkHandler serving the
// destinations from provider. A nil provider answers every id with 404.
func NewShortLinkHandler(provider pkgmodelsshortlink.ShortLinkDataProvider) *ShortLinkHandler {
	h := &ShortLinkHandler{provider: provider}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /links/{id}", h.handleRedirect)
	h.mux = mux
	return h
}

func (h *ShortLinkHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *ShortLinkHandler) handleRedirect(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	href, err := h.getShortLinkHref(r, id)
	if err != nil {
		if errors.Is(err, pkgmodelsshortlink.ErrShortLinkNotFound) {
			http.NotFound(w, r)
			return
		}
		writeError(w, err)
		return
	}
	// 302, not 301: the mapping lives in the server configuration document
	// and can be edited at any time, and browsers cache permanent redirects
	// beyond such edits.
	http.Redirect(w, r, href, http.StatusFound)
}

// getShortLinkHref returns the destination of the short link id, or an
// error wrapping pkgmodelsshortlink.ErrShortLinkNotFound when no provider
// is configured or the id is unknown.
func (h *ShortLinkHandler) getShortLinkHref(r *http.Request, id string) (string, error) {
	if h.provider == nil {
		return "", fmt.Errorf("short link %q: %w", id, pkgmodelsshortlink.ErrShortLinkNotFound)
	}
	return h.provider.GetShortLinkById(r.Context(), id)
}

func writeError(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	_ = json.NewEncoder(w).Encode(pkgutils.ErrorResponse{Error: err.Error()})
}
