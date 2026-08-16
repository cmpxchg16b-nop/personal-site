package dyn

import (
	"encoding/json"
	"net/http"
)

// HealthzHandler is an http.Handler that answers GET requests with a small
// JSON liveness payload. It backs /api/healthz, the probe target of the
// container image's HEALTHCHECK.
type HealthzHandler struct{}

// NewHealthzHandler constructs a HealthzHandler.
func NewHealthzHandler() *HealthzHandler {
	return &HealthzHandler{}
}

func (h *HealthzHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
