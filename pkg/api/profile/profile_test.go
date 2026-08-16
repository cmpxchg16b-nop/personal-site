package profile_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"personal-site/pkg/api/profile"
	"personal-site/pkg/session"
	pkgutils "personal-site/pkg/utils"
)

// profileResponse mirrors the handler's on-the-wire ProfileResponse so tests can
// decode the body for assertion targets.
type profileResponse struct {
	SessionID string `json:"session_id"`
	SubjectID string `json:"subject_id"`
	Username  string `json:"username"`
	Email     string `json:"email"`
}

// decodeJSON unmarshals body into v, failing the test on error.
func decodeJSON(t *testing.T, body string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), v); err != nil {
		t.Fatalf("decode body %q: %v", body, err)
	}
}

// newRequest seeds the request context with session/subject ids, username and
// email the way the JWT middleware does in production. An empty value leaves
// that context key unset.
func newRequest(method, target, sessionID, subjectID, username, email string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	ctx := r.Context()
	if sessionID != "" {
		ctx = context.WithValue(ctx, pkgutils.CtxKeySessionId, sessionID)
	}
	if subjectID != "" {
		ctx = context.WithValue(ctx, pkgutils.CtxKeySubjectId, subjectID)
	}
	if username != "" {
		ctx = context.WithValue(ctx, pkgutils.CtxKeyUsername, username)
	}
	if email != "" {
		ctx = context.WithValue(ctx, pkgutils.CtxKeyEmail, email)
	}
	return r.WithContext(ctx)
}

func TestProfileHandler(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		sessionID  string
		subjectID  string
		username   string
		email      string
		noSession  bool
		wantStatus int
		wantAllow  string
		wantCT     string
		want       profileResponse
		check      func(t *testing.T, rr *httptest.ResponseRecorder)
	}{
		{
			name:       "GET returns session id, subject id, username and email",
			method:     http.MethodGet,
			sessionID:  "sess-123",
			subjectID:  "subj-456",
			username:   "alice",
			email:      "alice@example.com",
			wantStatus: http.StatusOK,
			wantCT:     "application/json",
			want:       profileResponse{SessionID: "sess-123", SubjectID: "subj-456", Username: "alice", Email: "alice@example.com"},
		},
		{
			name:       "GET with only a session id omits the other fields",
			method:     http.MethodGet,
			sessionID:  "sess-only",
			wantStatus: http.StatusOK,
			wantCT:     "application/json",
			want:       profileResponse{SessionID: "sess-only"},
		},
		{
			name:       "GET with only a subject id omits the session id",
			method:     http.MethodGet,
			subjectID:  "subj-only",
			wantStatus: http.StatusOK,
			wantCT:     "application/json",
			want:       profileResponse{SubjectID: "subj-only"},
		},
		{
			name:       "GET with no context values returns empty fields, not null",
			method:     http.MethodGet,
			wantStatus: http.StatusOK,
			wantCT:     "application/json",
			want:       profileResponse{},
			check: func(t *testing.T, rr *httptest.ResponseRecorder) {
				// Empty strings must be emitted as "" rather than null.
				for _, field := range []string{"session_id", "subject_id", "username", "email"} {
					if !strings.Contains(rr.Body.String(), "\""+field+"\":\"\"") {
						t.Errorf("body = %q, want empty %s string", rr.Body.String(), field)
					}
				}
			},
		},
		{
			name:       "GET without a session object responds 500",
			method:     http.MethodGet,
			noSession:  true,
			wantStatus: http.StatusInternalServerError,
			check: func(t *testing.T, rr *httptest.ResponseRecorder) {
				if !strings.Contains(rr.Body.String(), "session not found") {
					t.Errorf("body = %q, want it to mention session not found", rr.Body.String())
				}
			},
		},
		{
			name:       "POST responds 405 with Allow: GET",
			method:     http.MethodPost,
			wantStatus: http.StatusMethodNotAllowed,
			wantAllow:  "GET",
			check: func(t *testing.T, rr *httptest.ResponseRecorder) {
				if !strings.Contains(rr.Body.String(), "method not allowed") {
					t.Errorf("body = %q, want it to mention method not allowed", rr.Body.String())
				}
			},
		},
		{
			name:       "DELETE responds 405 with Allow: GET",
			method:     http.MethodDelete,
			wantStatus: http.StatusMethodNotAllowed,
			wantAllow:  "GET",
		},
		{
			name:       "PUT responds 405 with Allow: GET",
			method:     http.MethodPut,
			wantStatus: http.StatusMethodNotAllowed,
			wantAllow:  "GET",
		},
	}

	sm := session.NewOnMemorySessionManager()
	h := profile.NewProfileHandler(sm)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Route through the session middleware so the request-scoped
			// session object is built from the seeded context values, as in
			// production. When noSession is set the request bypasses that
			// middleware entirely, so the handler's GetSessionFromContext
			// misses (producing the guarded 500).
			var handler http.Handler = h
			if !tc.noSession {
				handler = session.WithSessionId(h, sm)
			}

			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, newRequest(tc.method, "/api/profile", tc.sessionID, tc.subjectID, tc.username, tc.email))

			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", rr.Code, tc.wantStatus, rr.Body.String())
			}
			if tc.wantAllow != "" {
				if got := rr.Header().Get("Allow"); got != tc.wantAllow {
					t.Errorf("Allow = %q, want %q", got, tc.wantAllow)
				}
			}
			if tc.wantCT != "" {
				if got := rr.Header().Get("Content-Type"); !strings.Contains(got, tc.wantCT) {
					t.Errorf("Content-Type = %q, want it to contain %q", got, tc.wantCT)
				}
			}
			// Only the success path produces a JSON body worth decoding.
			if tc.wantStatus == http.StatusOK {
				var got profileResponse
				decodeJSON(t, rr.Body.String(), &got)
				if got != tc.want {
					t.Errorf("response = %+v, want %+v", got, tc.want)
				}
			}
			if tc.check != nil {
				tc.check(t, rr)
			}
		})
	}
}

// TestProfileHandler_RouteMounted runs the handler behind a ServeMux at the
// documented mount point to confirm the wiring in main.go (mux.Handle) reaches
// it correctly.
func TestProfileHandler_RouteMounted(t *testing.T) {
	sm := session.NewOnMemorySessionManager()
	mux := http.NewServeMux()
	mux.Handle("/api/profile", profile.NewProfileHandler(sm))

	var h http.Handler = mux
	h = session.WithSessionId(h, sm)

	r := newRequest(http.MethodGet, "/api/profile", "sess-route", "subj-route", "routey", "route@example.com")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rr.Code, rr.Body.String())
	}
	var got profileResponse
	decodeJSON(t, rr.Body.String(), &got)
	if got.SessionID != "sess-route" || got.SubjectID != "subj-route" || got.Username != "routey" || got.Email != "route@example.com" {
		t.Errorf("response = %+v, want sess-route/subj-route/routey/route@example.com", got)
	}
}
