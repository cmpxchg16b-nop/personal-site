package sipcredentials_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"personal-site/pkg/api/sipcredentials"
	"personal-site/pkg/session"
	"personal-site/pkg/sipregistration"
	"personal-site/pkg/utils"
)

type testAPI struct {
	manager *sipregistration.Manager
	handler http.Handler
	sm      *session.OnMemorySessionManager
}

func newTestAPI(t *testing.T) *testAPI {
	t.Helper()
	manager := sipregistration.NewManager(slog.New(slog.DiscardHandler))
	t.Cleanup(func() { _ = manager.Close() })
	sm := session.NewOnMemorySessionManager()
	handler := sipcredentials.NewHandler(manager, sm)
	return &testAPI{manager: manager, handler: session.WithSessionId(handler, sm), sm: sm}
}

func (a *testAPI) request(method, path, subject, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer test-token")
	r.Header.Set("Content-Type", "application/json")
	ctx := context.WithValue(r.Context(), utils.CtxKeySubjectId, subject)
	ctx = context.WithValue(ctx, utils.CtxKeySessionId, "session-for-"+subject)
	r = r.WithContext(ctx)
	response := httptest.NewRecorder()
	a.handler.ServeHTTP(response, r)
	return response
}

func decodeCredential(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var credential map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &credential); err != nil {
		t.Fatalf("decode %q: %v", response.Body.String(), err)
	}
	return credential
}

func TestCredentialCRUDIsScopedAndRedactsPassword(t *testing.T) {
	api := newTestAPI(t)
	createBody := `{"server":"PBX.Example","username":"1001","password":"top-secret","transport":"udp","enabled":false}`
	createdResponse := api.request(http.MethodPost, "/api/sip/credentials", "user-alice", createBody)
	if createdResponse.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body %s", createdResponse.Code, createdResponse.Body.String())
	}
	if strings.Contains(createdResponse.Body.String(), "top-secret") || strings.Contains(createdResponse.Body.String(), "password") {
		t.Fatalf("create leaked password: %s", createdResponse.Body.String())
	}
	if got := createdResponse.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	created := decodeCredential(t, createdResponse)
	id, _ := created["id"].(string)
	if id == "" || created["sip_uri"] != "sip:1001@pbx.example" || created["enabled"] != false {
		t.Fatalf("created = %+v", created)
	}
	stored, err := api.manager.Get("user-alice", id)
	if err != nil || stored.Password != "top-secret" {
		t.Fatalf("stored password = %q, err = %v", stored.Password, err)
	}

	duplicate := api.request(http.MethodPost, "/api/sip/credentials", "user-bob", createBody)
	if duplicate.Code != http.StatusConflict {
		t.Fatalf("cross-user duplicate status = %d, body %s", duplicate.Code, duplicate.Body.String())
	}
	if response := api.request(http.MethodGet, "/api/sip/credentials", "user-bob", ""); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"credentials":[]`) {
		t.Fatalf("other user's list = %d %s", response.Code, response.Body.String())
	}
	if response := api.request(http.MethodGet, "/api/sip/credentials/"+id, "user-bob", ""); response.Code != http.StatusNotFound {
		t.Fatalf("cross-user GET status = %d", response.Code)
	}
	if response := api.request(http.MethodPut, "/api/sip/credentials/"+id, "user-bob", createBody); response.Code != http.StatusNotFound {
		t.Fatalf("cross-user PUT status = %d", response.Code)
	}
	if response := api.request(http.MethodDelete, "/api/sip/credentials/"+id, "user-bob", ""); response.Code != http.StatusNotFound {
		t.Fatalf("cross-user DELETE status = %d", response.Code)
	}

	updateBody := `{"server":"new.example:5070","username":"1001","password":"","transport":"tcp","enabled":false}`
	updatedResponse := api.request(http.MethodPut, "/api/sip/credentials/"+id, "user-alice", updateBody)
	if updatedResponse.Code != http.StatusOK || strings.Contains(updatedResponse.Body.String(), "top-secret") {
		t.Fatalf("update = %d %s", updatedResponse.Code, updatedResponse.Body.String())
	}
	stored, err = api.manager.Get("user-alice", id)
	if err != nil || stored.Password != "top-secret" || stored.Transport != sipregistration.TransportTCP || stored.Server != "new.example:5070" {
		t.Fatalf("updated stored credential = %+v, err = %v", stored, err)
	}

	list := api.request(http.MethodGet, "/api/sip/credentials", "user-alice", "")
	if list.Code != http.StatusOK || strings.Contains(list.Body.String(), "top-secret") || !strings.Contains(list.Body.String(), `"state":"disabled"`) {
		t.Fatalf("list = %d %s", list.Code, list.Body.String())
	}
	invalid := api.request(http.MethodPost, "/api/sip/credentials", "user-alice", `{"server":"pbx.example","username":"1002","unknown":true}`)
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("unknown field status = %d, body %s", invalid.Code, invalid.Body.String())
	}
	deleted := api.request(http.MethodDelete, "/api/sip/credentials/"+id, "user-alice", "")
	if deleted.Code != http.StatusNoContent || !bytes.Equal(deleted.Body.Bytes(), []byte{}) {
		t.Fatalf("delete = %d %q", deleted.Code, deleted.Body.String())
	}
	if response := api.request(http.MethodGet, "/api/sip/credentials/"+id, "user-alice", ""); response.Code != http.StatusNotFound {
		t.Fatalf("GET after delete = %d", response.Code)
	}
}

func TestCredentialWritesRejectAmbientCrossOriginRequests(t *testing.T) {
	api := newTestAPI(t)
	body := `{"server":"pbx.example","username":"1001","enabled":false}`
	for _, test := range []struct {
		name        string
		requestURL  string
		origin      string
		contentType string
		wantStatus  int
	}{
		{name: "missing Origin", contentType: "application/json", wantStatus: http.StatusForbidden},
		{name: "cross Origin", origin: "https://evil.example", contentType: "application/json", wantStatus: http.StatusForbidden},
		{name: "HTTPS Origin to HTTP request", origin: "https://example.com", contentType: "application/json", wantStatus: http.StatusForbidden},
		{name: "HTTP Origin to HTTPS request", requestURL: "https://example.com/api/sip/credentials", origin: "http://example.com", contentType: "application/json", wantStatus: http.StatusForbidden},
		{name: "simple form content type", origin: "http://example.com", contentType: "text/plain", wantStatus: http.StatusUnsupportedMediaType},
	} {
		t.Run(test.name, func(t *testing.T) {
			requestURL := "/api/sip/credentials"
			if test.requestURL != "" {
				requestURL = test.requestURL
			}
			r := httptest.NewRequest(http.MethodPost, requestURL, strings.NewReader(body))
			r.Host = "example.com"
			r.Header.Set("Origin", test.origin)
			r.Header.Set("Content-Type", test.contentType)
			ctx := context.WithValue(r.Context(), utils.CtxKeySubjectId, "user-alice")
			response := httptest.NewRecorder()
			api.handler.ServeHTTP(response, r.WithContext(ctx))
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, body %s", response.Code, response.Body.String())
			}
		})
	}
	r := httptest.NewRequest(http.MethodPost, "/api/sip/credentials", strings.NewReader(body))
	r.Host = "example.com"
	r.Header.Set("Origin", "http://example.com")
	r.Header.Set("Content-Type", "application/json")
	ctx := context.WithValue(r.Context(), utils.CtxKeySubjectId, "user-alice")
	response := httptest.NewRecorder()
	api.handler.ServeHTTP(response, r.WithContext(ctx))
	if response.Code != http.StatusCreated {
		t.Fatalf("same-origin write status = %d, body %s", response.Code, response.Body.String())
	}
}

func TestCredentialAPIRequiresSubject(t *testing.T) {
	api := newTestAPI(t)
	response := api.request(http.MethodGet, "/api/sip/credentials", "", "")
	if response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), "subject") {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
}
