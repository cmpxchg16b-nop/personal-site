// Package sipcredentials exposes per-user SIP credential management over the
// site's authenticated HTTP API. It is a control protocol for the server-side
// SIP subsystem, not SIP transported to the browser.
package sipcredentials

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"personal-site/pkg/session"
	"personal-site/pkg/sipregistration"
	"personal-site/pkg/utils"
)

const maxBodyBytes = 16 << 10

// Service is the credential authority used by the HTTP adapter. Manager is the
// production implementation; the narrow interface keeps authorization and wire
// behavior independently testable.
type Service interface {
	List(ownerID string) []sipregistration.Credential
	Get(ownerID, id string) (sipregistration.Credential, error)
	Create(ownerID string, input sipregistration.Input) (sipregistration.Credential, error)
	Update(ownerID, id string, input sipregistration.Input) (sipregistration.Credential, error)
	Delete(ownerID, id string) error
}

type Handler struct {
	service        Service
	sm             session.SessionManager
	mux            *http.ServeMux
	allowedOrigins []string
}

func NewHandler(service Service, sm session.SessionManager, allowedOrigins ...string) *Handler {
	h := &Handler{service: service, sm: sm, allowedOrigins: allowedOrigins}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/sip/credentials", h.handleList)
	mux.HandleFunc("POST /api/sip/credentials", h.handleCreate)
	mux.HandleFunc("GET /api/sip/credentials/{id}", h.handleGet)
	mux.HandleFunc("PUT /api/sip/credentials/{id}", h.handleUpdate)
	mux.HandleFunc("DELETE /api/sip/credentials/{id}", h.handleDelete)
	h.mux = mux
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Responses contain account metadata and runtime errors; shared and browser
	// caches must not retain them. Passwords are never part of a response.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if !h.mutationAllowed(w, r) {
		return
	}
	h.mux.ServeHTTP(w, r)
}

// mutationAllowed prevents browser cross-origin writes and simple-form CSRF.
// Bearer-authenticated API clients are not ambient-credential browser flows.
func (h *Handler) mutationAllowed(w http.ResponseWriter, r *http.Request) bool {
	switch r.Method {
	case http.MethodPost, http.MethodPut:
		contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || contentType != "application/json" {
			writeError(w, http.StatusUnsupportedMediaType, errors.New("Content-Type must be application/json"))
			return false
		}
	case http.MethodDelete:
	default:
		return true
	}
	if strings.HasPrefix(strings.ToLower(r.Header.Get("Authorization")), "bearer ") {
		return true
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		writeError(w, http.StatusForbidden, errors.New("Origin is required for cookie-authenticated writes"))
		return false
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		writeError(w, http.StatusForbidden, errors.New("invalid Origin"))
		return false
	}
	requestScheme := "http"
	if r.TLS != nil {
		requestScheme = "https"
	}
	allowed := strings.EqualFold(parsed.Scheme, requestScheme) && strings.EqualFold(parsed.Host, r.Host)
	origin = parsed.Scheme + "://" + parsed.Host
	for _, candidate := range h.allowedOrigins {
		if strings.EqualFold(origin, candidate) {
			allowed = true
			break
		}
	}
	if !allowed {
		writeError(w, http.StatusForbidden, errors.New("Origin is not allowed"))
		return false
	}
	return true
}

type credentialRequest struct {
	Server    string `json:"server"`
	Username  string `json:"username"`
	Password  string `json:"password"`
	Transport string `json:"transport"`
	Enabled   *bool  `json:"enabled"`
}

type credentialResponse struct {
	ID           string                       `json:"id"`
	SIPURI       string                       `json:"sip_uri"`
	Server       string                       `json:"server"`
	Username     string                       `json:"username"`
	Transport    sipregistration.Transport    `json:"transport"`
	Enabled      bool                         `json:"enabled"`
	Registration sipregistration.Registration `json:"registration"`
}

type listResponse struct {
	Credentials []credentialResponse `json:"credentials"`
}

func responseOf(credential sipregistration.Credential) credentialResponse {
	return credentialResponse{
		ID:        credential.ID,
		SIPURI:    credential.SIPURI,
		Server:    credential.Server,
		Username:  credential.Username,
		Transport: credential.Transport,
		Enabled:   credential.Enabled,
		Registration: sipregistration.Registration{
			State:        credential.Registration.State,
			LastError:    credential.Registration.LastError,
			Attempts:     credential.Registration.Attempts,
			RegisteredAt: credential.Registration.RegisteredAt,
			ExpiresAt:    credential.Registration.ExpiresAt,
			RetryAt:      credential.Registration.RetryAt,
		},
	}
}

func (h *Handler) handleList(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.ownerID(w, r)
	if !ok {
		return
	}
	credentials := h.service.List(ownerID)
	response := listResponse{Credentials: make([]credentialResponse, 0, len(credentials))}
	for _, credential := range credentials {
		response.Credentials = append(response.Credentials, responseOf(credential))
	}
	writeJSON(w, http.StatusOK, response)
}

func (h *Handler) handleGet(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.ownerID(w, r)
	if !ok {
		return
	}
	credential, err := h.service.Get(ownerID, r.PathValue("id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, responseOf(credential))
}

func (h *Handler) handleCreate(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.ownerID(w, r)
	if !ok {
		return
	}
	request, ok := decodeRequest(w, r)
	if !ok {
		return
	}
	enabled := true
	if request.Enabled != nil {
		enabled = *request.Enabled
	}
	credential, err := h.service.Create(ownerID, inputOf(request, enabled))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, responseOf(credential))
}

func (h *Handler) handleUpdate(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.ownerID(w, r)
	if !ok {
		return
	}
	request, ok := decodeRequest(w, r)
	if !ok {
		return
	}
	enabled := false
	preserveEnabled := request.Enabled == nil
	if request.Enabled != nil {
		enabled = *request.Enabled
	}
	input := inputOf(request, enabled)
	input.PreserveEnabled = preserveEnabled
	credential, err := h.service.Update(ownerID, r.PathValue("id"), input)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, responseOf(credential))
}

func (h *Handler) handleDelete(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.ownerID(w, r)
	if !ok {
		return
	}
	if err := h.service.Delete(ownerID, r.PathValue("id")); err != nil {
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) ownerID(w http.ResponseWriter, r *http.Request) (string, bool) {
	sess, ok := h.sm.GetSessionFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, errors.New("no session on the request"))
		return "", false
	}
	ownerID := strings.TrimSpace(sess.SubjectId())
	if ownerID == "" {
		writeError(w, http.StatusUnauthorized, errors.New("session carries no subject id"))
		return "", false
	}
	return ownerID, true
}

func decodeRequest(w http.ResponseWriter, r *http.Request) (credentialRequest, bool) {
	var request credentialRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return credentialRequest{}, false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, errors.New("request body must contain one JSON object"))
		return credentialRequest{}, false
	}
	return request, true
}

func inputOf(request credentialRequest, enabled bool) sipregistration.Input {
	return sipregistration.Input{
		Server:    request.Server,
		Username:  request.Username,
		Password:  request.Password,
		Transport: sipregistration.Transport(request.Transport),
		Enabled:   enabled,
	}
}

func writeServiceError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, sipregistration.ErrInvalidInput):
		status = http.StatusBadRequest
	case errors.Is(err, sipregistration.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, sipregistration.ErrConflict):
		status = http.StatusConflict
	case errors.Is(err, sipregistration.ErrQuota):
		status = http.StatusTooManyRequests
	case errors.Is(err, sipregistration.ErrClosed):
		status = http.StatusServiceUnavailable
	}
	writeError(w, status, err)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, utils.ErrorResponse{Error: err.Error()})
}
