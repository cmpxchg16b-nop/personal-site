// Package examtrackings exposes the /api/examtrackings HTTP endpoint, returning
// the exam reports recorded for the caller (by subject/user id) and letting the
// caller delete individual reports.
//
// Until an accounting system is in place, the caller's subject id (user id, see
// package session) is used as the user id against the ExamTrackingServer. The
// ExamTrackingServer handed to the constructor must be the same instance that the
// exam server writes finished reports to, so the reports surfaced here are the
// ones persisted on session end.
package examtrackings

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"personal-site/pkg/models/examreport"
	"personal-site/pkg/session"
)

// apiPrefix is the path the handler is mounted under.
const apiPrefix = "/api/examtrackings"

// ExamTrackingsHandler is an http.Handler that serves the caller's exam reports.
type ExamTrackingsHandler struct {
	sm             session.SessionManager
	trackingServer examreport.ExamTrackingServer
}

// NewExamTrackingsHandler constructs an ExamTrackingsHandler. sm resolves the
// caller's session from the request context; its subject id (user id) is used as
// the user id until an accounting system exists. trackingServer must be the same
// ExamTrackingServer instance handed to the exam server (OnMemoryExamServer), so
// the reports persisted on session end are the ones returned here.
func NewExamTrackingsHandler(sm session.SessionManager, trackingServer examreport.ExamTrackingServer) *ExamTrackingsHandler {
	return &ExamTrackingsHandler{sm: sm, trackingServer: trackingServer}
}

// listResponse is the JSON body of a successful GET /api/examtrackings.
type listResponse struct {
	ExamReports []examreport.ExamReport `json:"exam_reports"`
}

// ServeHTTP implements http.Handler. Routes:
//
//	GET    /api/examtrackings          -> 200 {"exam_reports": [...]}
//	DELETE /api/examtrackings/{id}     -> 204 (404 when no such report exists)
//	anything deeper                    -> 404
//	other methods                      -> 405 with Allow
//
// The caller's session must already be attached to the request context by the
// session middleware (see package session) and its subject id (user id) is used
// to scope every operation.
func (h *ExamTrackingsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sess, ok := h.sm.GetSessionFromContext(r.Context())
	if !ok {
		http.Error(w, "session not found", http.StatusInternalServerError)
		return
	}

	// Resolve the path beneath the prefix so the handler also works when mounted
	// as a subtree (e.g. "/api/examtrackings/"). The collection root and a
	// single report id are served; any deeper path is a 404.
	rel := strings.TrimPrefix(r.URL.Path, apiPrefix)
	trimmed := strings.Trim(rel, "/")

	switch {
	case trimmed == "":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.handleList(w, r, sess.SubjectId())
	case !strings.Contains(trimmed, "/"):
		if r.Method != http.MethodDelete {
			w.Header().Set("Allow", "DELETE")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.handleDelete(w, r, sess.SubjectId(), trimmed)
	default:
		http.NotFound(w, r)
	}
}

// handleList returns the caller's exam reports as {"exam_reports": [...]}.
func (h *ExamTrackingsHandler) handleList(w http.ResponseWriter, r *http.Request, userid string) {
	reports, err := h.trackingServer.GetExamReportsByUserId(r.Context(), userid)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Normalize nil to an empty slice so the body is "[]", not "null" — matching
	// the /api/examsessions list handler's wire shape.
	if reports == nil {
		reports = []examreport.ExamReport{}
	}
	writeJSON(w, listResponse{ExamReports: reports})
}

// handleDelete removes the caller's report identified by reportId. It responds
// 204 on success and 404 when the caller has no report with that id.
func (h *ExamTrackingsHandler) handleDelete(w http.ResponseWriter, r *http.Request, userid, reportId string) {
	if err := h.trackingServer.DeleteExamTracking(r.Context(), userid, reportId); err != nil {
		if errors.Is(err, examreport.ErrExamTrackingNotFound) {
			http.Error(w, "exam report not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeJSON encodes v as the response body.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
