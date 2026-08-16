package profile

import (
	"encoding/json"
	"net/http"

	pkgsession "personal-site/pkg/session"
)

// ProfileHandler is an http.Handler that serves the caller's profile (session
// id, subject id, username and email) at GET /api/profile.
type ProfileHandler struct {
	sm pkgsession.SessionManager
}

// NewProfileHandler constructs a ProfileHandler, injecting the SessionManager
// used to resolve the request-scoped session object (populated upstream by the
// session middleware).
func NewProfileHandler(sm pkgsession.SessionManager) *ProfileHandler {
	return &ProfileHandler{sm: sm}
}

type ProfileResponse struct {
	SessionID string `json:"session_id"`
	SubjectID string `json:"subject_id"`
	Username  string `json:"username"`
	Email     string `json:"email"`
}

func (h *ProfileHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	sess, ok := h.sm.GetSessionFromContext(r.Context())
	if !ok {
		http.Error(w, "session not found", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	resp := &ProfileResponse{
		SessionID: sess.Id(),
		SubjectID: sess.SubjectId(),
		Username:  sess.Username(),
		Email:     sess.Email(),
	}
	json.NewEncoder(w).Encode(resp)
}
