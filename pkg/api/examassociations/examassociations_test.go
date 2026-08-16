package examassociations_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"personal-site/pkg/api/examassociations"
	pkgmodelsquestion "personal-site/pkg/models/question"
	pkgmodelsuserexamdocs "personal-site/pkg/models/userexamdocs"
	pkgmodelsuserupload "personal-site/pkg/models/userupload"
	"personal-site/pkg/session"
	pkgutils "personal-site/pkg/utils"
)

// fakeAssociationManager is a UserExamDocsAssociationManager that records the
// operations it was asked to perform and returns canned results. The handler
// exercises GetAssociationsByUserId, AddAssociation, and DeleteAssociation;
// DereferenceAssociation is stubbed only to satisfy the interface.
type fakeAssociationManager struct {
	getUserid string
	getAssocs []pkgmodelsuserexamdocs.ExamDocAssociation
	getErr    error

	addUserid    string
	addUploadIds []string
	addErr       error

	deleteUserid string
	deleteIds    []string
	deleteErr    error
}

func (f *fakeAssociationManager) GetAssociationsByUserId(_ context.Context, userId string) ([]pkgmodelsuserexamdocs.ExamDocAssociation, error) {
	f.getUserid = userId
	return f.getAssocs, f.getErr
}

func (f *fakeAssociationManager) AddAssociation(_ context.Context, userId string, uploadId string) error {
	f.addUserid = userId
	f.addUploadIds = append(f.addUploadIds, uploadId)
	return f.addErr
}

func (f *fakeAssociationManager) DeleteAssociation(_ context.Context, userId string, associationId string) error {
	f.deleteUserid = userId
	f.deleteIds = append(f.deleteIds, associationId)
	return f.deleteErr
}

func (f *fakeAssociationManager) DereferenceAssociation(_ context.Context, _, _ string) (*pkgmodelsquestion.Exam, error) {
	return nil, errors.New("not implemented by the fake")
}

// sampleAssociation builds a distinguishable ExamDocAssociation for assertion
// targets.
func sampleAssociation(id string) pkgmodelsuserexamdocs.ExamDocAssociation {
	return pkgmodelsuserexamdocs.ExamDocAssociation{
		Id:       id,
		UserId:   "owner-" + id,
		UploadId: "upload-" + id,
	}
}

// associationDTO mirrors the handler's on-the-wire DTO so tests can decode the
// body and pin the wire field names (id, user_id, upload_id).
type associationDTO struct {
	Id       string `json:"id"`
	UserId   string `json:"user_id"`
	UploadId string `json:"upload_id"`
}

// listResponse mirrors the handler's on-the-wire listResponse.
type listResponse struct {
	Associations []associationDTO `json:"associations"`
}

// decodeJSON unmarshals body into v, failing the test on error.
func decodeJSON(t *testing.T, body string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), v); err != nil {
		t.Fatalf("decode body %q: %v", body, err)
	}
}

// testEnv wires a handler behind a ServeMux mounted exactly as main.go does:
// only the collection root and a single-segment association id. The session
// subsystem is stateless: instead of allocating a session object, serve sets
// the context value the JWT middleware would set (the subject id) and runs the
// request through WithSessionId so a Session is built and attached.
type testEnv struct {
	sm        *session.OnMemorySessionManager
	mgr       *fakeAssociationManager
	subjectID string
	mux       *http.ServeMux
}

func newTestEnv(t *testing.T, mgr *fakeAssociationManager) *testEnv {
	t.Helper()
	sm := session.NewOnMemorySessionManager()
	h := examassociations.NewExamAssociationsHandler(sm, mgr)
	mux := http.NewServeMux()
	mux.Handle("/api/examassociations", h)
	mux.Handle("/api/examassociations/{association_id}", h)
	return &testEnv{sm: sm, mgr: mgr, subjectID: "subject-test", mux: mux}
}

// serve issues a request through the env's mux. When withSession is true the
// request is first run through WithSessionId (mirroring the production
// middleware chain) after seeding the subject id in the context, so the handler
// receives a resolved Session. When false, no session is attached and the
// handler's GetSessionFromContext misses (producing the 500 guarded below).
func (e *testEnv) serve(t *testing.T, method, target string, body io.Reader, withSession bool) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, body)
	h := http.Handler(e.mux)
	if withSession {
		ctx := context.WithValue(r.Context(), pkgutils.CtxKeySubjectId, e.subjectID)
		r = r.WithContext(ctx)
		h = session.WithSessionId(e.mux, e.sm)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	return rr
}

func TestExamAssociationsHandler(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		target     string
		body       string
		noSession  bool
		mgr        *fakeAssociationManager
		wantStatus int
		wantAllow  string
		wantCT     string
		check      func(t *testing.T, rr *httptest.ResponseRecorder, env *testEnv)
	}{
		{
			name:       "GET returns the caller's associations as JSON",
			method:     http.MethodGet,
			target:     "/api/examassociations",
			mgr:        &fakeAssociationManager{getAssocs: []pkgmodelsuserexamdocs.ExamDocAssociation{sampleAssociation("a1"), sampleAssociation("a2")}},
			wantStatus: http.StatusOK,
			wantCT:     "application/json",
			check: func(t *testing.T, rr *httptest.ResponseRecorder, env *testEnv) {
				var got listResponse
				decodeJSON(t, rr.Body.String(), &got)
				if len(got.Associations) != 2 {
					t.Fatalf("len(Associations) = %d, want 2 (body %q)", len(got.Associations), rr.Body.String())
				}
				if got.Associations[0].Id != "a1" || got.Associations[1].Id != "a2" {
					t.Errorf("association ids = %q, %q, want a1, a2", got.Associations[0].Id, got.Associations[1].Id)
				}
				// The DTO carries the model fields through under the wire names.
				if got.Associations[0].UserId != "owner-a1" || got.Associations[0].UploadId != "upload-a1" {
					t.Errorf("association a1 = %+v, want user_id owner-a1 and upload_id upload-a1", got.Associations[0])
				}
				if env.mgr.getUserid != env.subjectID {
					t.Errorf("manager queried with userid %q, want subject id %q", env.mgr.getUserid, env.subjectID)
				}
			},
		},
		{
			name:       "GET with no associations returns an empty array, not null",
			method:     http.MethodGet,
			target:     "/api/examassociations",
			mgr:        &fakeAssociationManager{getAssocs: nil},
			wantStatus: http.StatusOK,
			wantCT:     "application/json",
			check: func(t *testing.T, rr *httptest.ResponseRecorder, env *testEnv) {
				// The handler normalizes nil -> [] so the body is "[]", not "null".
				if !strings.Contains(rr.Body.String(), "\"associations\":[]") {
					t.Fatalf("body = %q, want it to contain \"associations\":[]", rr.Body.String())
				}
				var got listResponse
				decodeJSON(t, rr.Body.String(), &got)
				if len(got.Associations) != 0 {
					t.Errorf("len(Associations) = %d, want 0", len(got.Associations))
				}
			},
		},
		{
			name:       "GET manager error surfaces as 500",
			method:     http.MethodGet,
			target:     "/api/examassociations",
			mgr:        &fakeAssociationManager{getErr: errors.New("storage unavailable")},
			wantStatus: http.StatusInternalServerError,
			check: func(t *testing.T, rr *httptest.ResponseRecorder, env *testEnv) {
				if !strings.Contains(rr.Body.String(), "storage unavailable") {
					t.Errorf("body = %q, want it to surface the manager error", rr.Body.String())
				}
			},
		},
		{
			name:       "GET with missing session in context responds 500",
			method:     http.MethodGet,
			target:     "/api/examassociations",
			noSession:  true,
			mgr:        &fakeAssociationManager{},
			wantStatus: http.StatusInternalServerError,
			check: func(t *testing.T, rr *httptest.ResponseRecorder, env *testEnv) {
				if !strings.Contains(rr.Body.String(), "session not found") {
					t.Errorf("body = %q, want it to mention session not found", rr.Body.String())
				}
				if env.mgr.getUserid != "" {
					t.Errorf("manager was queried as %q, want no call when session is absent", env.mgr.getUserid)
				}
			},
		},
		{
			name:       "POST creates an association and responds 201",
			method:     http.MethodPost,
			target:     "/api/examassociations",
			body:       `{"upload_id":"u1"}`,
			mgr:        &fakeAssociationManager{},
			wantStatus: http.StatusCreated,
			check: func(t *testing.T, rr *httptest.ResponseRecorder, env *testEnv) {
				if env.mgr.addUserid != env.subjectID {
					t.Errorf("add userid = %q, want subject id %q", env.mgr.addUserid, env.subjectID)
				}
				if len(env.mgr.addUploadIds) != 1 || env.mgr.addUploadIds[0] != "u1" {
					t.Errorf("add upload ids = %v, want [u1]", env.mgr.addUploadIds)
				}
				if rr.Body.Len() != 0 {
					t.Errorf("body = %q, want empty on 201", rr.Body.String())
				}
			},
		},
		{
			name:       "POST with malformed JSON responds 400",
			method:     http.MethodPost,
			target:     "/api/examassociations",
			body:       `{"upload_id":`,
			mgr:        &fakeAssociationManager{},
			wantStatus: http.StatusBadRequest,
			check: func(t *testing.T, rr *httptest.ResponseRecorder, env *testEnv) {
				if !strings.Contains(rr.Body.String(), "invalid JSON body") {
					t.Errorf("body = %q, want it to mention invalid JSON body", rr.Body.String())
				}
				if len(env.mgr.addUploadIds) != 0 {
					t.Errorf("manager got adds %v, want no call on malformed body", env.mgr.addUploadIds)
				}
			},
		},
		{
			name:       "POST without upload_id responds 400",
			method:     http.MethodPost,
			target:     "/api/examassociations",
			body:       `{}`,
			mgr:        &fakeAssociationManager{},
			wantStatus: http.StatusBadRequest,
			check: func(t *testing.T, rr *httptest.ResponseRecorder, env *testEnv) {
				if !strings.Contains(rr.Body.String(), "upload_id is required") {
					t.Errorf("body = %q, want it to mention upload_id is required", rr.Body.String())
				}
				if len(env.mgr.addUploadIds) != 0 {
					t.Errorf("manager got adds %v, want no call when upload_id is missing", env.mgr.addUploadIds)
				}
			},
		},
		{
			name:       "POST for a missing upload responds 404",
			method:     http.MethodPost,
			target:     "/api/examassociations",
			body:       `{"upload_id":"no-such"}`,
			mgr:        &fakeAssociationManager{addErr: pkgmodelsuserupload.ErrUploadNotFound},
			wantStatus: http.StatusNotFound,
			check: func(t *testing.T, rr *httptest.ResponseRecorder, env *testEnv) {
				if !strings.Contains(rr.Body.String(), "upload not found") {
					t.Errorf("body = %q, want it to mention upload not found", rr.Body.String())
				}
			},
		},
		{
			name:       "POST for a wrapped ErrUploadNotFound also responds 404",
			method:     http.MethodPost,
			target:     "/api/examassociations",
			body:       `{"upload_id":"no-such"}`,
			mgr:        &fakeAssociationManager{addErr: fmt.Errorf("association store: %w", pkgmodelsuserupload.ErrUploadNotFound)},
			wantStatus: http.StatusNotFound,
			check: func(t *testing.T, rr *httptest.ResponseRecorder, env *testEnv) {
				if !strings.Contains(rr.Body.String(), "upload not found") {
					t.Errorf("body = %q, want it to mention upload not found", rr.Body.String())
				}
			},
		},
		{
			name:       "POST manager error surfaces as 500",
			method:     http.MethodPost,
			target:     "/api/examassociations",
			body:       `{"upload_id":"u1"}`,
			mgr:        &fakeAssociationManager{addErr: errors.New("storage unavailable")},
			wantStatus: http.StatusInternalServerError,
			check: func(t *testing.T, rr *httptest.ResponseRecorder, env *testEnv) {
				if !strings.Contains(rr.Body.String(), "storage unavailable") {
					t.Errorf("body = %q, want it to surface the manager error", rr.Body.String())
				}
			},
		},
		{
			name:       "POST with missing session in context responds 500",
			method:     http.MethodPost,
			target:     "/api/examassociations",
			body:       `{"upload_id":"u1"}`,
			noSession:  true,
			mgr:        &fakeAssociationManager{},
			wantStatus: http.StatusInternalServerError,
			check: func(t *testing.T, rr *httptest.ResponseRecorder, env *testEnv) {
				if !strings.Contains(rr.Body.String(), "session not found") {
					t.Errorf("body = %q, want it to mention session not found", rr.Body.String())
				}
				if len(env.mgr.addUploadIds) != 0 {
					t.Errorf("manager got adds %v, want no call when session is absent", env.mgr.addUploadIds)
				}
			},
		},
		{
			name:       "DELETE an association responds 204 and forwards the subject id and association id",
			method:     http.MethodDelete,
			target:     "/api/examassociations/a1",
			mgr:        &fakeAssociationManager{},
			wantStatus: http.StatusNoContent,
			check: func(t *testing.T, rr *httptest.ResponseRecorder, env *testEnv) {
				if env.mgr.deleteUserid != env.subjectID {
					t.Errorf("delete userid = %q, want subject id %q", env.mgr.deleteUserid, env.subjectID)
				}
				if len(env.mgr.deleteIds) != 1 || env.mgr.deleteIds[0] != "a1" {
					t.Errorf("delete ids = %v, want [a1]", env.mgr.deleteIds)
				}
			},
		},
		{
			name:       "DELETE an unknown association responds 404",
			method:     http.MethodDelete,
			target:     "/api/examassociations/no-such",
			mgr:        &fakeAssociationManager{deleteErr: errors.New("not in store")},
			wantStatus: http.StatusNotFound,
			check: func(t *testing.T, rr *httptest.ResponseRecorder, env *testEnv) {
				if !strings.Contains(rr.Body.String(), "association not found") {
					t.Errorf("body = %q, want it to mention association not found", rr.Body.String())
				}
			},
		},
		{
			name:       "DELETE with missing session in context responds 500",
			method:     http.MethodDelete,
			target:     "/api/examassociations/a1",
			noSession:  true,
			mgr:        &fakeAssociationManager{},
			wantStatus: http.StatusInternalServerError,
			check: func(t *testing.T, rr *httptest.ResponseRecorder, env *testEnv) {
				if !strings.Contains(rr.Body.String(), "session not found") {
					t.Errorf("body = %q, want it to mention session not found", rr.Body.String())
				}
				if len(env.mgr.deleteIds) != 0 {
					t.Errorf("manager got deletes %v, want no call when session is absent", env.mgr.deleteIds)
				}
			},
		},
		{
			name:       "PUT on the collection responds 405 with Allow: GET, POST",
			method:     http.MethodPut,
			target:     "/api/examassociations",
			mgr:        &fakeAssociationManager{},
			wantStatus: http.StatusMethodNotAllowed,
			wantAllow:  "GET, POST",
			check: func(t *testing.T, rr *httptest.ResponseRecorder, env *testEnv) {
				if !strings.Contains(rr.Body.String(), "method not allowed") {
					t.Errorf("body = %q, want it to mention method not allowed", rr.Body.String())
				}
			},
		},
		{
			name:       "DELETE on the collection responds 405 with Allow: GET, POST",
			method:     http.MethodDelete,
			target:     "/api/examassociations",
			mgr:        &fakeAssociationManager{},
			wantStatus: http.StatusMethodNotAllowed,
			wantAllow:  "GET, POST",
		},
		{
			name:       "GET on an association id responds 405 with Allow: DELETE",
			method:     http.MethodGet,
			target:     "/api/examassociations/a1",
			mgr:        &fakeAssociationManager{},
			wantStatus: http.StatusMethodNotAllowed,
			wantAllow:  "DELETE",
		},
		{
			name:       "POST on an association id responds 405 with Allow: DELETE",
			method:     http.MethodPost,
			target:     "/api/examassociations/a1",
			body:       `{"upload_id":"u1"}`,
			mgr:        &fakeAssociationManager{},
			wantStatus: http.StatusMethodNotAllowed,
			wantAllow:  "DELETE",
			check: func(t *testing.T, rr *httptest.ResponseRecorder, env *testEnv) {
				if len(env.mgr.addUploadIds) != 0 {
					t.Errorf("manager got adds %v, want no call on a 405", env.mgr.addUploadIds)
				}
			},
		},
		{
			name:       "deeper path beneath an association id is a 404",
			method:     http.MethodGet,
			target:     "/api/examassociations/a1/extra",
			mgr:        &fakeAssociationManager{},
			wantStatus: http.StatusNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t, tc.mgr)

			var body io.Reader
			if tc.body != "" {
				body = strings.NewReader(tc.body)
			}
			rr := env.serve(t, tc.method, tc.target, body, !tc.noSession)

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
			if tc.check != nil {
				tc.check(t, rr, env)
			}
		})
	}
}
