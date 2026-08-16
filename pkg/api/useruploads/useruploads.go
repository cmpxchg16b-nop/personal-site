// Package useruploads exposes the /api/useruploads HTTP endpoint, letting an
// authenticated caller upload, list, download, and delete their own files.
//
// The caller's subject id (user id, see package session) is used as the user
// id against the backing UserUploadManager, exactly as /api/examtrackings uses
// it against the ExamTrackingServer. Uploads created by one user are never
// visible to another.
//
// Wire shape:
//
//	POST   /api/useruploads          multipart/form-data "file" -> 201 + summary
//	GET    /api/useruploads          -> 200 {"uploads":[...]}
//	GET    /api/useruploads/{id}     -> 200 file bytes (metadata in headers)
//	DELETE /api/useruploads/{id}     -> 204
package useruploads

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"personal-site/pkg/models/userupload"
	"personal-site/pkg/session"
	pkgutils "personal-site/pkg/utils"
)

// apiPrefix is the path the handler is mounted under.
const apiPrefix = "/api/useruploads"

// maxUploadMemory is the most bytes of a multipart upload held in RAM before
// the rest spills to a temp file. The backing OnMemoryUserUploadManager reads
// the entire blob into memory anyway, so this only bounds the peak during
// parsing; everything is ultimately held by the manager.
const maxUploadMemory int64 = 32 << 20 // 32 MiB

// fileFormField is the multipart form field name carrying the uploaded file.
const fileFormField = "file"

// UserUploadsHandler is an http.Handler that exposes the userupload service.
type UserUploadsHandler struct {
	sm  session.SessionManager
	mgr userupload.UserUploadManager
}

// NewUserUploadsHandler constructs a UserUploadsHandler. sm resolves the
// caller's session from the request context; its subject id (user id) is used
// to scope operations. mgr is the backing store (typically an
// OnMemoryUserUploadManager).
func NewUserUploadsHandler(sm session.SessionManager, mgr userupload.UserUploadManager) *UserUploadsHandler {
	return &UserUploadsHandler{sm: sm, mgr: mgr}
}

// summaryDTO is the JSON shape of a UserUploadSummery. The model struct has no
// JSON tags and embeds FileMetadata, so an explicit DTO keeps the wire field
// names stable and flat.
type summaryDTO struct {
	UploadId       string `json:"upload_id"`
	Filename       string `json:"filename"`
	MIMEType       string `json:"mime_type"`
	SizeBytes      int64  `json:"size_bytes"`
	LastModifiedAt int64  `json:"last_modified_at"`
	Sha256         string `json:"sha256"`
	UserId         string `json:"user_id"`
}

func toDTO(s userupload.UserUploadSummery) summaryDTO {
	return summaryDTO{
		UploadId:       s.UploadId,
		Filename:       s.Filename,
		MIMEType:       s.MIMEType,
		SizeBytes:      s.SizeBytes,
		LastModifiedAt: s.LastModifiedAt,
		Sha256:         s.Sha256,
		UserId:         s.UserId,
	}
}

// listResponse is the JSON body of a successful GET /api/useruploads.
type listResponse struct {
	Uploads []summaryDTO `json:"uploads"`
}

// ServeHTTP implements http.Handler.
//
// The caller's session must already be attached to the request context by the
// session middleware (see package session) and its subject id (user id) is used
// to scope all operations. Routes:
//
//	/                     GET=list, POST=create
//	/{uploadId}           GET=download, DELETE=delete
//	anything deeper       404
func (h *UserUploadsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sess, ok := h.sm.GetSessionFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusInternalServerError, "session not found")
		return
	}
	userId := sess.SubjectId()

	rel := strings.TrimPrefix(r.URL.Path, apiPrefix)
	rel = strings.Trim(rel, "/")

	if rel == "" {
		// Collection root.
		switch r.Method {
		case http.MethodGet:
			h.handleList(w, r, userId)
		case http.MethodPost:
			h.handleCreate(w, r, userId)
		default:
			w.Header().Set("Allow", "GET, POST")
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return
	}

	// Item-level path: exactly one segment is the uploadId.
	if segs := strings.Split(rel, "/"); len(segs) == 1 {
		uploadId := segs[0]
		switch r.Method {
		case http.MethodGet:
			h.handleDownload(w, r, userId, uploadId)
		case http.MethodDelete:
			h.handleDelete(w, r, userId, uploadId)
		default:
			w.Header().Set("Allow", "GET, DELETE")
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return
	}

	http.NotFound(w, r)
}

// handleList returns the caller's uploads as {"uploads":[...]}.
func (h *UserUploadsHandler) handleList(w http.ResponseWriter, r *http.Request, userId string) {
	uploads, err := h.mgr.ListUserUploads(r.Context(), userId)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]summaryDTO, 0, len(uploads))
	for _, u := range uploads {
		out = append(out, toDTO(u))
	}
	writeJSON(w, listResponse{Uploads: out})
}

// handleCreate stores a new upload from a multipart/form-data request. The file
// is read from the "file" form field; filename and MIME type are taken from the
// part headers. SizeBytes, LastModifiedAt, and Sha256 are left for the backing
// manager to compute (a diligent manager overrides all three). On success it
// responds 201 with the resulting summary.
func (h *UserUploadsHandler) handleCreate(w http.ResponseWriter, r *http.Request, userId string) {
	if err := r.ParseMultipartForm(maxUploadMemory); err != nil {
		writeError(w, http.StatusBadRequest, "invalid multipart request: "+err.Error())
		return
	}

	file, header, err := r.FormFile(fileFormField)
	if err != nil {
		writeError(w, http.StatusBadRequest, `missing "file" field: `+err.Error())
		return
	}
	defer file.Close()

	metadata := userupload.FileMetadata{
		Filename: header.Filename,
		MIMEType: header.Header.Get("Content-Type"),
	}

	summary, err := h.mgr.CreateNewUserUpload(r.Context(), file, userId, metadata)
	if err != nil {
		if errors.Is(err, userupload.ErrEmptyFilename) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	w.WriteHeader(http.StatusCreated)
	writeJSON(w, toDTO(summary))
}

// handleDownload streams the content of an upload. Because the body is the raw
// file bytes, the summary metadata is exposed via response headers:
//
//	X-Upload-Id, X-Filename, X-Mime-Type, X-Size-Bytes, X-Sha256,
//	X-Last-Modified-At
//
// Content-Type mirrors the stored MIME type when present, otherwise
// application/octet-stream. Content-Disposition is set to attachment with the
// original filename.
func (h *UserUploadsHandler) handleDownload(w http.ResponseWriter, r *http.Request, userId, uploadId string) {
	summary, rc, err := h.mgr.GetUserUploadByUploadId(r.Context(), userId, uploadId)
	if err != nil {
		if errors.Is(err, userupload.ErrUploadNotFound) {
			writeError(w, http.StatusNotFound, "upload not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rc.Close()

	w.Header().Set("X-Upload-Id", summary.UploadId)
	w.Header().Set("X-Filename", summary.Filename)
	w.Header().Set("X-Mime-Type", summary.MIMEType)
	w.Header().Set("X-Size-Bytes", strconv.FormatInt(summary.SizeBytes, 10))
	w.Header().Set("X-Sha256", summary.Sha256)
	w.Header().Set("X-Last-Modified-At", strconv.FormatInt(summary.LastModifiedAt, 10))

	contentType := summary.MIMEType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+summary.Filename+`"`)

	if _, err := io.Copy(w, rc); err != nil {
		// Headers (and possibly a partial body) are already sent; nothing left
		// to do but log via the response writer's status. The client will see
		// a truncated body.
		return
	}
}

// handleDelete removes an upload. The per-user count is never decremented, so
// the uploadId is never reused. 204 on success, 404 if not found.
func (h *UserUploadsHandler) handleDelete(w http.ResponseWriter, r *http.Request, userId, uploadId string) {
	if err := h.mgr.DeleteUserUpload(r.Context(), userId, uploadId); err != nil {
		if errors.Is(err, userupload.ErrUploadNotFound) {
			writeError(w, http.StatusNotFound, "upload not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
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
