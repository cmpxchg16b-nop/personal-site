// Package examassociations exposes the /api/examassociations HTTP endpoint,
// letting an authenticated caller list, create, and delete the associations
// (bindings between their uploads and exam documents) held by a
// UserExamDocsAssociationManager.
//
// The caller's subject id (user id, see package session) is used as the user
// id against the backing manager, exactly as /api/examtrackings and
// /api/useruploads do. Associations created by one user are never visible to
// another.
//
// Wire shape:
//
//	GET    /api/examassociations                  -> 200 {"associations":[...]}
//	POST   /api/examassociations                  201
//	DELETE /api/examassociations/{association_id} -> 204
//
// No binary blob is required for POST: the client is assumed to have already
// uploaded the file via /api/useruploads and supplies only its upload_id.
package examassociations

import (
	"encoding/json"
	"errors"
	"net/http"

	pkgmodelsuserexamdocs "personal-site/pkg/models/userexamdocs"
	pkgmodelsuserupload "personal-site/pkg/models/userupload"
	pkgsession "personal-site/pkg/session"
	pkgutils "personal-site/pkg/utils"
)

// associationDTO is the JSON shape of an ExamDocAssociation. The model struct
// has no JSON tags, so an explicit DTO keeps the wire field names stable.
type associationDTO struct {
	Id       string `json:"id"`
	UserId   string `json:"user_id"`
	UploadId string `json:"upload_id"`
}

func toDTO(a pkgmodelsuserexamdocs.ExamDocAssociation) associationDTO {
	return associationDTO{
		Id:       a.Id,
		UserId:   a.UserId,
		UploadId: a.UploadId,
	}
}

// createRequest is the JSON body of a POST /api/examassociations.
type createRequest struct {
	UploadId string `json:"upload_id"`
}

// listResponse is the JSON body of a successful GET /api/examassociations.
type listResponse struct {
	Associations []associationDTO `json:"associations"`
}

// ExamAssociationsHandler is an http.Handler that exposes the association service.
type ExamAssociationsHandler struct {
	sm  pkgsession.SessionManager
	mgr pkgmodelsuserexamdocs.UserExamDocsAssociationManager
}

// NewExamAssociationsHandler constructs an ExamAssociationsHandler. sm resolves
// the caller's session from the request context; its subject id (user id) is
// used to scope operations. mgr is the backing association manager.
func NewExamAssociationsHandler(sm pkgsession.SessionManager, mgr pkgmodelsuserexamdocs.UserExamDocsAssociationManager) *ExamAssociationsHandler {
	return &ExamAssociationsHandler{sm: sm, mgr: mgr}
}

// ServeHTTP implements http.Handler.
//
// The caller's session must already be attached to the request context by the
// session middleware (see package session) and its subject id (user id) is used
// to scope all operations. Routes:
//
//	/                         GET=list, POST=create
//	/{association_id}         DELETE=delete
//	anything deeper           404
func (h *ExamAssociationsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sess, ok := h.sm.GetSessionFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusInternalServerError, "session not found")
		return
	}
	userId := sess.SubjectId()

	associationId := r.PathValue("association_id")

	switch {
	case associationId == "" && r.Method == http.MethodGet:
		h.handleList(w, r, userId)
	case associationId == "" && r.Method == http.MethodPost:
		h.handleCreate(w, r, userId)
	case associationId != "" && r.Method == http.MethodDelete:
		h.handleDelete(w, r, userId, associationId)
	default:
		if associationId == "" {
			w.Header().Set("Allow", "GET, POST")
		} else {
			w.Header().Set("Allow", "DELETE")
		}
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleList returns the caller's associations as {"associations":[...]}.
func (h *ExamAssociationsHandler) handleList(w http.ResponseWriter, r *http.Request, userId string) {
	assocs, err := h.mgr.GetAssociationsByUserId(r.Context(), userId)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]associationDTO, 0, len(assocs))
	for _, a := range assocs {
		out = append(out, toDTO(a))
	}
	writeJSON(w, listResponse{Associations: out})
}

// handleCreate binds a new association to an existing upload. The client
// supplies only the upload_id in the JSON body. On success it responds 201.
func (h *ExamAssociationsHandler) handleCreate(w http.ResponseWriter, r *http.Request, userId string) {
	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if req.UploadId == "" {
		writeError(w, http.StatusBadRequest, "upload_id is required")
		return
	}

	if err := h.mgr.AddAssociation(r.Context(), userId, req.UploadId); err != nil {
		if errors.Is(err, pkgmodelsuserupload.ErrUploadNotFound) {
			writeError(w, http.StatusNotFound, "upload not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// handleDelete removes an association. 204 on success.
func (h *ExamAssociationsHandler) handleDelete(w http.ResponseWriter, r *http.Request, userId, associationId string) {
	if err := h.mgr.DeleteAssociation(r.Context(), userId, associationId); err != nil {
		writeError(w, http.StatusNotFound, "association not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeJSON encodes v as the response body.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes an error response in the project's standard {"err":...}
// shape (see pkgutils.ErrorResponse).
func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(pkgutils.ErrorResponse{Error: msg})
}
