package examtrackings_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"personal-site/pkg/api/examtrackings"
	"personal-site/pkg/models/examreport"
	"personal-site/pkg/session"
	pkgutils "personal-site/pkg/utils"
)

// fakeTrackingServer is an ExamTrackingServer that records the operations it
// was asked to perform and returns canned results. GetExamReportsByUserId and
// DeleteExamTracking are exercised by the handler; Put is stubbed to satisfy
// the interface so the fake can also be reused in the end-to-end test.
type fakeTrackingServer struct {
	getUserid  string
	getReports []examreport.ExamReport
	getErr     error

	// Put records every report handed to it, in order.
	putUserid  string
	putReports []examreport.ExamReport
	putErr     error

	// DeleteExamTracking records the userids and report ids it was asked to
	// delete, in order.
	deleteUserid string
	deleteIds    []string
	deleteErr    error
}

func (s *fakeTrackingServer) Put(_ context.Context, userid string, report examreport.ExamReport, _ bool) error {
	s.putUserid = userid
	s.putReports = append(s.putReports, report)
	return s.putErr
}

func (s *fakeTrackingServer) GetExamReportsByUserId(_ context.Context, userid string) ([]examreport.ExamReport, error) {
	s.getUserid = userid
	return s.getReports, s.getErr
}

func (s *fakeTrackingServer) DeleteExamTracking(_ context.Context, userid string, examReportId string) error {
	s.deleteUserid = userid
	s.deleteIds = append(s.deleteIds, examReportId)
	return s.deleteErr
}

// sampleReport builds a distinguishable ExamReport for assertion targets.
func sampleReport(id string) examreport.ExamReport {
	return examreport.ExamReport{
		Id:            id,
		ExamId:        "exam-doc-" + id,
		ExamShortName: "DCACI",
		ExamCode:      "300-620",
		Title:         "Implementing Cisco ACI",
		ExamSessionId: "session-" + id,
		FinishedAt:    1700000000000,
	}
}

// listResponse mirrors the handler's on-the-wire listResponse so tests can decode
// the body. The slice is a pointer so a "null" body (absent reports) decodes to
// nil rather than panicking.
type listResponse struct {
	ExamReports []examreport.ExamReport `json:"exam_reports"`
}

// decodeJSON unmarshals body into v, failing the test on error.
func decodeJSON(t *testing.T, body string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), v); err != nil {
		t.Fatalf("decode body %q: %v", body, err)
	}
}

// testEnv wires a handler behind a ServeMux that mirrors the documented mount.
// The session subsystem is stateless: instead of allocating a session object,
// serve sets the context value the JWT middleware would set (the subject id) and
// runs the request through WithSessionId so a Session is built and attached.
type testEnv struct {
	sm        *session.OnMemorySessionManager
	ts        *fakeTrackingServer
	subjectID string
	mux       *http.ServeMux
}

func newTestEnv(t *testing.T, ts *fakeTrackingServer) *testEnv {
	t.Helper()
	sm := session.NewOnMemorySessionManager()
	h := examtrackings.NewExamTrackingsHandler(sm, ts)
	mux := http.NewServeMux()
	mux.Handle("/api/examtrackings", h)
	mux.Handle("/api/examtrackings/", h)
	return &testEnv{sm: sm, ts: ts, subjectID: "subject-test", mux: mux}
}

// serve issues a request through the env's mux. When withSession is true the
// request is first run through WithSessionId (mirroring the production
// middleware chain) after seeding the subject id in the context, so the handler
// receives a resolved Session. When false, no session is attached and the
// handler's GetSessionFromContext misses (producing the 500 guarded below).
func (e *testEnv) serve(t *testing.T, method, target string, withSession bool) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, nil)
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

func TestExamTrackingsHandler(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		target     string
		noSession  bool
		ts         *fakeTrackingServer
		wantStatus int
		wantAllow  string
		wantCT     string
		check      func(t *testing.T, rr *httptest.ResponseRecorder, env *testEnv)
	}{
		{
			name:       "GET returns the caller's reports as JSON",
			method:     http.MethodGet,
			target:     "/api/examtrackings",
			ts:         &fakeTrackingServer{getReports: []examreport.ExamReport{sampleReport("r1"), sampleReport("r2")}},
			wantStatus: http.StatusOK,
			wantCT:     "application/json",
			check: func(t *testing.T, rr *httptest.ResponseRecorder, env *testEnv) {
				var got listResponse
				decodeJSON(t, rr.Body.String(), &got)
				if len(got.ExamReports) != 2 {
					t.Fatalf("len(ExamReports) = %d, want 2 (body %q)", len(got.ExamReports), rr.Body.String())
				}
				if got.ExamReports[0].Id != "r1" || got.ExamReports[1].Id != "r2" {
					t.Errorf("report ids = %q, %q, want r1, r2", got.ExamReports[0].Id, got.ExamReports[1].Id)
				}
				if env.ts.getUserid != env.subjectID {
					t.Errorf("tracking server queried with userid %q, want subject id %q", env.ts.getUserid, env.subjectID)
				}
			},
		},
		{
			name:       "GET with no reports returns an empty array, not null",
			method:     http.MethodGet,
			target:     "/api/examtrackings",
			ts:         &fakeTrackingServer{getReports: nil},
			wantStatus: http.StatusOK,
			wantCT:     "application/json",
			check: func(t *testing.T, rr *httptest.ResponseRecorder, env *testEnv) {
				// The handler normalizes nil -> [] so the body is "[]", not "null".
				if !strings.Contains(rr.Body.String(), "\"exam_reports\":[]") {
					t.Fatalf("body = %q, want it to contain \"exam_reports\":[]", rr.Body.String())
				}
				var got listResponse
				decodeJSON(t, rr.Body.String(), &got)
				if len(got.ExamReports) != 0 {
					t.Errorf("len(ExamReports) = %d, want 0", len(got.ExamReports))
				}
			},
		},
		{
			name:       "GET on trailing slash still serves the collection root",
			method:     http.MethodGet,
			target:     "/api/examtrackings/",
			ts:         &fakeTrackingServer{getReports: []examreport.ExamReport{sampleReport("r1")}},
			wantStatus: http.StatusOK,
			wantCT:     "application/json",
			check: func(t *testing.T, rr *httptest.ResponseRecorder, env *testEnv) {
				var got listResponse
				decodeJSON(t, rr.Body.String(), &got)
				if len(got.ExamReports) != 1 || got.ExamReports[0].Id != "r1" {
					t.Errorf("ExamReports = %+v, want one report r1", got.ExamReports)
				}
			},
		},
		{
			name:       "subject id flows as the user id: a caller sees only its own reports",
			method:     http.MethodGet,
			target:     "/api/examtrackings",
			ts:         &fakeTrackingServer{getReports: []examreport.ExamReport{sampleReport("only-mine")}},
			wantStatus: http.StatusOK,
			check: func(t *testing.T, rr *httptest.ResponseRecorder, env *testEnv) {
				if env.ts.getUserid == "" {
					t.Fatal("tracking server was never queried")
				}
				// The userid passed must be the subject id, not a fixed constant
				// or empty string.
				if env.ts.getUserid != env.subjectID {
					t.Errorf("userid = %q, want subject id %q", env.ts.getUserid, env.subjectID)
				}
			},
		},
		{
			name:       "GET on a report id responds 405 with Allow: DELETE",
			method:     http.MethodGet,
			target:     "/api/examtrackings/r1",
			ts:         &fakeTrackingServer{},
			wantStatus: http.StatusMethodNotAllowed,
			wantAllow:  "DELETE",
		},
		{
			name:       "POST on a report id responds 405 with Allow: DELETE",
			method:     http.MethodPost,
			target:     "/api/examtrackings/r1",
			ts:         &fakeTrackingServer{},
			wantStatus: http.StatusMethodNotAllowed,
			wantAllow:  "DELETE",
		},
		{
			name:       "deeper path beneath a report id is a 404",
			method:     http.MethodGet,
			target:     "/api/examtrackings/r1/extra",
			ts:         &fakeTrackingServer{},
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "DELETE a report responds 204 and forwards the subject id and report id",
			method:     http.MethodDelete,
			target:     "/api/examtrackings/r1",
			ts:         &fakeTrackingServer{},
			wantStatus: http.StatusNoContent,
			check: func(t *testing.T, rr *httptest.ResponseRecorder, env *testEnv) {
				if env.ts.deleteUserid != env.subjectID {
					t.Errorf("delete userid = %q, want subject id %q", env.ts.deleteUserid, env.subjectID)
				}
				if len(env.ts.deleteIds) != 1 || env.ts.deleteIds[0] != "r1" {
					t.Errorf("delete ids = %v, want [r1]", env.ts.deleteIds)
				}
			},
		},
		{
			name:       "DELETE on a trailing-slash report id still resolves the id",
			method:     http.MethodDelete,
			target:     "/api/examtrackings/r9/",
			ts:         &fakeTrackingServer{},
			wantStatus: http.StatusNoContent,
			check: func(t *testing.T, rr *httptest.ResponseRecorder, env *testEnv) {
				if len(env.ts.deleteIds) != 1 || env.ts.deleteIds[0] != "r9" {
					t.Errorf("delete ids = %v, want [r9]", env.ts.deleteIds)
				}
			},
		},
		{
			name:       "DELETE an unknown report responds 404",
			method:     http.MethodDelete,
			target:     "/api/examtrackings/no-such",
			ts:         &fakeTrackingServer{deleteErr: examreport.ErrExamTrackingNotFound},
			wantStatus: http.StatusNotFound,
			check: func(t *testing.T, rr *httptest.ResponseRecorder, env *testEnv) {
				if !strings.Contains(rr.Body.String(), "exam report not found") {
					t.Errorf("body = %q, want it to mention exam report not found", rr.Body.String())
				}
			},
		},
		{
			name:       "DELETE tracking server error surfaces as 500",
			method:     http.MethodDelete,
			target:     "/api/examtrackings/r1",
			ts:         &fakeTrackingServer{deleteErr: errors.New("storage unavailable")},
			wantStatus: http.StatusInternalServerError,
			check: func(t *testing.T, rr *httptest.ResponseRecorder, env *testEnv) {
				if !strings.Contains(rr.Body.String(), "storage unavailable") {
					t.Errorf("body = %q, want it to surface the tracking server error", rr.Body.String())
				}
			},
		},
		{
			name:       "DELETE with missing session in context responds 500",
			method:     http.MethodDelete,
			target:     "/api/examtrackings/r1",
			noSession:  true,
			ts:         &fakeTrackingServer{},
			wantStatus: http.StatusInternalServerError,
			check: func(t *testing.T, rr *httptest.ResponseRecorder, env *testEnv) {
				if !strings.Contains(rr.Body.String(), "session not found") {
					t.Errorf("body = %q, want it to mention session not found", rr.Body.String())
				}
				if len(env.ts.deleteIds) != 0 {
					t.Errorf("tracking server got deletes %v, want no call when session is absent", env.ts.deleteIds)
				}
			},
		},
		{
			name:       "POST on the collection responds 405 with Allow: GET",
			method:     http.MethodPost,
			target:     "/api/examtrackings",
			ts:         &fakeTrackingServer{},
			wantStatus: http.StatusMethodNotAllowed,
			wantAllow:  "GET",
			check: func(t *testing.T, rr *httptest.ResponseRecorder, env *testEnv) {
				if !strings.Contains(rr.Body.String(), "method not allowed") {
					t.Errorf("body = %q, want it to mention method not allowed", rr.Body.String())
				}
			},
		},
		{
			name:       "DELETE on the collection responds 405 with Allow: GET",
			method:     http.MethodDelete,
			target:     "/api/examtrackings",
			ts:         &fakeTrackingServer{},
			wantStatus: http.StatusMethodNotAllowed,
			wantAllow:  "GET",
		},
		{
			name:       "missing session in context responds 500",
			method:     http.MethodGet,
			target:     "/api/examtrackings",
			noSession:  true,
			ts:         &fakeTrackingServer{},
			wantStatus: http.StatusInternalServerError,
			check: func(t *testing.T, rr *httptest.ResponseRecorder, env *testEnv) {
				if !strings.Contains(rr.Body.String(), "session not found") {
					t.Errorf("body = %q, want it to mention session not found", rr.Body.String())
				}
				if env.ts.getUserid != "" {
					t.Errorf("tracking server was queried as %q, want no call when session is absent", env.ts.getUserid)
				}
			},
		},
		{
			name:       "tracking server error surfaces as 500",
			method:     http.MethodGet,
			target:     "/api/examtrackings",
			ts:         &fakeTrackingServer{getErr: errors.New("storage unavailable")},
			wantStatus: http.StatusInternalServerError,
			check: func(t *testing.T, rr *httptest.ResponseRecorder, env *testEnv) {
				if !strings.Contains(rr.Body.String(), "storage unavailable") {
					t.Errorf("body = %q, want it to surface the tracking server error", rr.Body.String())
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t, tc.ts)
			rr := env.serve(t, tc.method, tc.target, !tc.noSession)

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

// TestExamTrackingsHandler_EndToEnd walks the documented write/read-back flow: a
// report Put under a subject id (as the exam server does on session end) is
// returned to a subsequent GET scoped to that same subject id. It uses the real
// OnMemoryExamTrackingServer rather than a fake, so it exercises the actual
// store the handler is wired to in main.go.
func TestExamTrackingsHandler_EndToEnd(t *testing.T) {
	ts := examreport.NewOnMemoryExamTrackingServer(nil, nil)
	sm := session.NewOnMemorySessionManager()
	subjectID := "subject-endtoend"

	// Simulate the exam server persisting two finished-session reports under the
	// caller's subject id.
	ctx := context.Background()
	if err := ts.Put(ctx, subjectID, sampleReport("e1"), false); err != nil {
		t.Fatalf("Put e1: %v", err)
	}
	if err := ts.Put(ctx, subjectID, sampleReport("e2"), false); err != nil {
		t.Fatalf("Put e2: %v", err)
	}
	// A second, unrelated subject has its own reports that must not leak.
	if err := ts.Put(ctx, "subject-other", sampleReport("not-mine"), false); err != nil {
		t.Fatalf("Put other: %v", err)
	}

	h := examtrackings.NewExamTrackingsHandler(sm, ts)
	mux := http.NewServeMux()
	mux.Handle("/api/examtrackings", h)
	wrapped := session.WithSessionId(mux, sm)

	// Seed the subject id in the context (as the JWT middleware would) and run
	// through WithSessionId so the handler receives a resolved Session.
	r := httptest.NewRequest(http.MethodGet, "/api/examtrackings", nil)
	r = r.WithContext(context.WithValue(r.Context(), pkgutils.CtxKeySubjectId, subjectID))
	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rr.Code, rr.Body.String())
	}
	var got listResponse
	decodeJSON(t, rr.Body.String(), &got)
	if len(got.ExamReports) != 2 {
		t.Fatalf("len(ExamReports) = %d, want 2 (the caller's own reports)", len(got.ExamReports))
	}
	for _, r := range got.ExamReports {
		if strings.Contains(r.Id, "not-mine") {
			t.Errorf("leaked report %q from another session; isolation broken", r.Id)
		}
	}
	if got.ExamReports[0].Id != "e1" || got.ExamReports[1].Id != "e2" {
		t.Errorf("report ids = %q, %q, want e1, e2 in insertion order", got.ExamReports[0].Id, got.ExamReports[1].Id)
	}

	// DELETE one of the caller's own reports through the handler (subtree mount,
	// as in main.go), then confirm the list shrinks to the other one.
	delMux := http.NewServeMux()
	delMux.Handle("/api/examtrackings", h)
	delMux.Handle("/api/examtrackings/", h)
	delWrapped := session.WithSessionId(delMux, sm)

	delReq := httptest.NewRequest(http.MethodDelete, "/api/examtrackings/e1", nil)
	delReq = delReq.WithContext(context.WithValue(delReq.Context(), pkgutils.CtxKeySubjectId, subjectID))
	delRR := httptest.NewRecorder()
	delWrapped.ServeHTTP(delRR, delReq)
	if delRR.Code != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204 (body %q)", delRR.Code, delRR.Body.String())
	}

	// Deleting the same report again is a 404.
	delRR2 := httptest.NewRecorder()
	delWrapped.ServeHTTP(delRR2, delReq)
	if delRR2.Code != http.StatusNotFound {
		t.Fatalf("second DELETE status = %d, want 404", delRR2.Code)
	}

	// The other subject's report is not deletable by this caller, and survives.
	delOther := httptest.NewRequest(http.MethodDelete, "/api/examtrackings/not-mine", nil)
	delOther = delOther.WithContext(context.WithValue(delOther.Context(), pkgutils.CtxKeySubjectId, subjectID))
	delOtherRR := httptest.NewRecorder()
	delWrapped.ServeHTTP(delOtherRR, delOther)
	if delOtherRR.Code != http.StatusNotFound {
		t.Fatalf("DELETE other subject's report status = %d, want 404", delOtherRR.Code)
	}

	r2 := httptest.NewRequest(http.MethodGet, "/api/examtrackings", nil)
	r2 = r2.WithContext(context.WithValue(r2.Context(), pkgutils.CtxKeySubjectId, subjectID))
	rr2 := httptest.NewRecorder()
	wrapped.ServeHTTP(rr2, r2)
	if rr2.Code != http.StatusOK {
		t.Fatalf("GET after delete status = %d, want 200", rr2.Code)
	}
	var got2 listResponse
	decodeJSON(t, rr2.Body.String(), &got2)
	if len(got2.ExamReports) != 1 || got2.ExamReports[0].Id != "e2" {
		t.Fatalf("reports after delete = %+v, want only e2", got2.ExamReports)
	}

	otherReports, err := ts.GetExamReportsByUserId(ctx, "subject-other")
	if err != nil || len(otherReports) != 1 {
		t.Fatalf("other subject's reports = %+v, %v; want untouched", otherReports, err)
	}
}
