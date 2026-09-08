// Package dyn serves the site's dynamic blog data — the post metadata,
// project and author-contact lists, and the entertain page's media shelves —
// to the frontend as JSON under /api/dyn/. The data comes from a
// pkg/models/dyn DynBlogDataProvider, typically re-read from the
// <dynBlogData/> section of the server configuration document on every
// request.
package dyn

import (
	"encoding/json"
	"net/http"

	pkgmodelsdyn "personal-site/pkg/models/dyn"
	pkgutils "personal-site/pkg/utils"
)

// DynamicBlogDataHandler is an http.Handler that serves the site's dynamic
// blog data, routing the /api/dyn/ subtree internally:
//
//	GET /api/dyn/posts           the blog post metadata list
//	GET /api/dyn/posts/{id}      a single post's metadata (404 when the id is
//	                             unknown) — lets post pages query one post
//	                             without downloading the whole list
//	GET /api/dyn/projects        the project list
//	GET /api/dyn/authorcontacts  the author-contact list
//	GET /api/dyn/entertain       the entertain page's media shelves (live,
//	                             videos, music)
//	GET /api/dyn/menu            the top bar's navigation drawer entries
//
// The handler asks its provider for the data on every request, so a provider
// that re-reads its source (e.g. FSBasedDynBlogData) serves edits without a
// server restart. It is stateless and safe for concurrent use.
type DynamicBlogDataHandler struct {
	provider pkgmodelsdyn.DynBlogDataProvider
	mux      *http.ServeMux
}

// NewDynamicBlogDataHandler constructs a DynamicBlogDataHandler serving the
// data from provider. A nil provider serves empty JSON arrays (an
// all-shelves-empty object for /api/dyn/entertain), and 404 for every post
// id.
func NewDynamicBlogDataHandler(provider pkgmodelsdyn.DynBlogDataProvider) *DynamicBlogDataHandler {
	h := &DynamicBlogDataHandler{provider: provider}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/dyn/posts", h.handlePosts)
	mux.HandleFunc("GET /api/dyn/posts/{id}", h.handlePostById)
	mux.HandleFunc("GET /api/dyn/projects", h.handleProjects)
	mux.HandleFunc("GET /api/dyn/authorcontacts", h.handleAuthorContacts)
	mux.HandleFunc("GET /api/dyn/entertain", h.handleEntertain)
	mux.HandleFunc("GET /api/dyn/menu", h.handleMenu)
	h.mux = mux
	return h
}

func (h *DynamicBlogDataHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// getDynBlogData returns the provider's current data, or an empty
// DynBlogData when no provider is configured.
func (h *DynamicBlogDataHandler) getDynBlogData() (*pkgmodelsdyn.DynBlogData, error) {
	if h.provider == nil {
		return &pkgmodelsdyn.DynBlogData{}, nil
	}
	return h.provider.GetDynBlogData()
}

func (h *DynamicBlogDataHandler) handlePosts(w http.ResponseWriter, r *http.Request) {
	data, err := h.getDynBlogData()
	if err != nil {
		writeError(w, err)
		return
	}
	// Never serialize a nil slice: the response stays a JSON array.
	posts := data.Posts
	if posts == nil {
		posts = []pkgmodelsdyn.PostMetadata{}
	}
	writeJSON(w, posts)
}

// handlePostById serves the single post whose id matches the {id} path
// segment, or 404 when no such post exists.
func (h *DynamicBlogDataHandler) handlePostById(w http.ResponseWriter, r *http.Request) {
	data, err := h.getDynBlogData()
	if err != nil {
		writeError(w, err)
		return
	}
	id := r.PathValue("id")
	for _, p := range data.Posts {
		if p.Id == id {
			writeJSON(w, p)
			return
		}
	}
	http.NotFound(w, r)
}

func (h *DynamicBlogDataHandler) handleProjects(w http.ResponseWriter, r *http.Request) {
	data, err := h.getDynBlogData()
	if err != nil {
		writeError(w, err)
		return
	}
	// Never serialize a nil slice: the response stays a JSON array.
	projects := data.Projects
	if projects == nil {
		projects = []pkgmodelsdyn.Project{}
	}
	writeJSON(w, projects)
}

func (h *DynamicBlogDataHandler) handleAuthorContacts(w http.ResponseWriter, r *http.Request) {
	data, err := h.getDynBlogData()
	if err != nil {
		writeError(w, err)
		return
	}
	contacts := data.AuthorContacts
	if contacts == nil {
		contacts = []pkgmodelsdyn.AuthorContact{}
	}
	writeJSON(w, contacts)
}

func (h *DynamicBlogDataHandler) handleEntertain(w http.ResponseWriter, r *http.Request) {
	data, err := h.getDynBlogData()
	if err != nil {
		writeError(w, err)
		return
	}
	entertain := data.Entertain
	// Never serialize a nil slice: every shelf stays a JSON array.
	if entertain.Live == nil {
		entertain.Live = []pkgmodelsdyn.Media{}
	}
	if entertain.Videos == nil {
		entertain.Videos = []pkgmodelsdyn.Media{}
	}
	if entertain.Music == nil {
		entertain.Music = []pkgmodelsdyn.Media{}
	}
	writeJSON(w, entertain)
}

func (h *DynamicBlogDataHandler) handleMenu(w http.ResponseWriter, r *http.Request) {
	data, err := h.getDynBlogData()
	if err != nil {
		writeError(w, err)
		return
	}
	menu := data.Menu
	if menu == nil {
		menu = []pkgmodelsdyn.MenuEntry{}
	}
	writeJSON(w, menu)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	_ = json.NewEncoder(w).Encode(pkgutils.ErrorResponse{Error: err.Error()})
}
