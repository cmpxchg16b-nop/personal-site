// Package comments serves the site's commenting API: reading the comments
// of a channel (a blog post, a page, a chat thread, …) and appending new
// ones. The comments come from a pkg/models/comment CommentServiceProvider.
//
// The handler stays thin: it does not participate in the commenting logic
// itself. Finding the channel's last comment is the client's job (GET the
// channel, take the last comment's id); the client-supplied last_comment_id
// is passed to the provider unchanged.
//
// Reading is open to everyone (the GET routes are on the server's JWT
// whitelist); appending requires a session: the author of every PUT is the
// caller's session identity (its username, falling back to the subject id),
// never the request body, so no client can comment in the name of anyone
// else.
package comments

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	pkgmodelscomment "personal-site/pkg/models/comment"
	pkgsession "personal-site/pkg/session"
	pkgutils "personal-site/pkg/utils"
)

// maxPutCommentBody caps PUT request bodies at 1 MiB, so comment payloads
// cannot become a memory-exhaustion vector.
const maxPutCommentBody = 1 << 20

// CommentsHandler is an http.Handler serving the commenting API, routing
// the /api/comments/ subtree internally:
//
//	GET /api/comments/channel/{channelId}  the channel's comments, oldest first
//	PUT /api/comments/channel/{channelId}  append a comment to the channel
//
// It is stateless and safe for concurrent use.
type CommentsHandler struct {
	provider pkgmodelscomment.CommentServiceProvider
	sm       pkgsession.SessionManager
	mux      *http.ServeMux
}

// NewCommentsHandler constructs a CommentsHandler storing and serving
// comments through provider, which must be non-nil, and resolving the author
// of appended comments through sm (the request-scoped session populated by
// the session middleware upstream).
func NewCommentsHandler(provider pkgmodelscomment.CommentServiceProvider, sm pkgsession.SessionManager) *CommentsHandler {
	h := &CommentsHandler{provider: provider, sm: sm}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/comments/channel/{channelId}", h.handleGetComments)
	mux.HandleFunc("PUT /api/comments/channel/{channelId}", h.handlePutComment)
	h.mux = mux
	return h
}

func (h *CommentsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// commentJSON is the wire representation of a comment. RichContent is left
// out: for text/plain — the only supported type — it always equals Content
// encoded as UTF-8, so sending it would only duplicate Content.
type commentJSON struct {
	ID            string `json:"id"`
	ChannelID     string `json:"channel_id"`
	UserID        string `json:"user_id"`
	SerialNumber  uint64 `json:"serial_number"`
	LastCommentID string `json:"last_comment_id"`
	Content       string `json:"content"`
	MIMEType      string `json:"mime_type"`
	CreationTime  uint64 `json:"creation_time"`
	LastModified  uint64 `json:"last_modified"`
}

func toCommentJSON(c *pkgmodelscomment.Comment) commentJSON {
	return commentJSON{
		ID:            c.Id,
		ChannelID:     c.ChannelId,
		UserID:        c.UserId,
		SerialNumber:  c.SerialNumber,
		LastCommentID: c.LastCommentId,
		Content:       c.Content,
		MIMEType:      c.MIMEType,
		CreationTime:  c.CreationTime,
		LastModified:  c.LastModified,
	}
}

// getCommentsResponse is the response body of
// GET /api/comments/channel/{channelId}.
type getCommentsResponse struct {
	Comments []commentJSON `json:"comments"`
}

// putCommentRequest is the request body of
// PUT /api/comments/channel/{channelId}. The comment's author is NOT part of
// it: the server takes the identity from the caller's session.
type putCommentRequest struct {
	Content string `json:"content"`
	// MIMEType defaults to text/plain when empty.
	MIMEType string `json:"mime_type"`
	// LastCommentID is the id of the channel's last comment as the client
	// knows it, or empty when the client believes the channel is still
	// empty. When another comment landed in between, the PUT fails with 409
	// and the client is expected to re-read the channel and retry.
	LastCommentID string `json:"last_comment_id"`
}

func (h *CommentsHandler) handleGetComments(w http.ResponseWriter, r *http.Request) {
	channelId := r.PathValue("channelId")
	// The empty requestingUser: reads are open to everyone and the provider
	// has no per-user visibility rules.
	resp := getCommentsResponse{Comments: []commentJSON{}}
	for ev := range h.provider.GetCommentsByChannelId(r.Context(), "", channelId) {
		if ev.Err != nil {
			writeError(w, http.StatusInternalServerError, ev.Err)
			return
		}
		resp.Comments = append(resp.Comments, toCommentJSON(ev.Comment))
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *CommentsHandler) handlePutComment(w http.ResponseWriter, r *http.Request) {
	channelId := r.PathValue("channelId")

	var req putCommentRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPutCommentBody)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	if req.Content == "" {
		writeError(w, http.StatusBadRequest, errors.New("content is required"))
		return
	}

	// The comment's author comes from the caller's session: the
	// human-friendly username when the JWT carries one, else the subject id.
	// PUT is not on the JWT whitelist, so an authenticated session is always
	// present; a missing or identity-less one means the upstream middleware
	// chain is misconfigured.
	sess, ok := h.sm.GetSessionFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, errors.New("no session on the request"))
		return
	}
	userId := strings.TrimSpace(sess.Username())
	if userId == "" {
		userId = sess.SubjectId()
	}
	if userId == "" {
		writeError(w, http.StatusUnauthorized, errors.New("session carries no identity"))
		return
	}

	c := &pkgmodelscomment.Comment{Content: req.Content, MIMEType: req.MIMEType}
	if err := h.provider.PutComment(r.Context(), c, req.LastCommentID, userId, channelId); err != nil {
		switch {
		case errors.Is(err, pkgmodelscomment.ErrUnsupportedMIMEType):
			writeError(w, http.StatusUnsupportedMediaType, err)
		case errors.Is(err, pkgmodelscomment.ErrStaleLastComment):
			writeError(w, http.StatusConflict, err)
		case errors.Is(err, pkgmodelscomment.ErrCommentNotFound),
			errors.Is(err, pkgmodelscomment.ErrCommentChannelMismatch):
			writeError(w, http.StatusBadRequest, err)
		default:
			writeError(w, http.StatusInternalServerError, err)
		}
		return
	}
	writeJSON(w, http.StatusCreated, toCommentJSON(c))
}

// writeJSON encodes v as the response body with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError reports err to the client with the given status, in the site's
// common error shape (pkgutils.ErrorResponse).
func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, pkgutils.ErrorResponse{Error: err.Error()})
}
