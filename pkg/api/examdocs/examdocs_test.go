package examdocs_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	examdocs "personal-site/pkg/api/examdocs"
	"personal-site/pkg/models/question"
	"personal-site/pkg/session"
	pkgutils "personal-site/pkg/utils"
)

// fakeLoader is an ExamLoader that serves canned exams by URL and can be wired
// to fail for specific URLs, so tests can exercise both the Data and Err lines
// of the NDJSON stream.
type fakeLoader struct {
	byURL  map[string]*question.Exam
	errURL map[string]bool
}

func (l *fakeLoader) LoadFrom(ctx context.Context, url string) (*question.Exam, error) {
	if l.errURL[url] {
		return nil, errors.New("disk read failed")
	}
	if e, ok := l.byURL[url]; ok {
		return e, nil
	}
	return nil, errors.New("exam not found")
}

// fakeSource is an ExamSource with separate system-wide and per-user entry
// sets, so tests can exercise the handler's merged, per-user-first stream.
type fakeSource struct {
	system  []question.ExamSourceEntry
	perUser map[string][]question.ExamSourceEntry
	userErr error
}

func (s *fakeSource) Get() []question.ExamSourceEntry { return s.system }

func (s *fakeSource) GetByUserId(_ context.Context, userId string) ([]question.ExamSourceEntry, error) {
	if s.userErr != nil {
		return nil, s.userErr
	}
	return s.perUser[userId], nil
}

// serveRequest issues a request against the handler wrapped in the session
// middleware (mirroring the production chain), with the subject id seeded in
// the context as the JWT middleware would. When withSession is false the
// request bypasses the session middleware entirely, so the handler's
// GetSessionFromContext misses (producing the 500 guarded in ServeHTTP).
func serveRequest(t *testing.T, h http.Handler, sm *session.OnMemorySessionManager, method, target string, withSession bool) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, nil)
	handler := h
	if withSession {
		ctx := context.WithValue(r.Context(), pkgutils.CtxKeySubjectId, "subject-test")
		r = r.WithContext(ctx)
		handler = session.WithSessionId(h, sm)
	}
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, r)
	return rr
}

// ndLine mirrors the handler's on-the-wire ndjsonLine so tests can decode each
// streamed line. Data decodes straight back into question.ExamExcerpt because
// ExamExcerpt's exported fields carry no JSON tags.
type ndLine struct {
	Err  string                `json:"Err,omitempty"`
	Data *question.ExamDocumentExcerpt `json:"Data,omitempty"`
}

// examWith builds a minimal exam whose first (and only) question collection
// holds one single-choice question per score; ExamExcerptFrom therefore reports
// NumQuestions = len(scores) and TotalScores = sum(scores).
func examWith(id, code string, scores ...float32) *question.Exam {
	qs := make([]question.Question, len(scores))
	for i, s := range scores {
		qs[i] = question.Question{
			Id:    fmt.Sprintf("%s-q%d", id, i+1),
			Type:  question.QuestionTypeSingleChoice,
			Score: s,
		}
	}
	return &question.Exam{
		Id:        id,
		ShortName: id,
		Code:      code,
		Title:     question.PlainText("Title " + id),
		QuestionSet: question.QuestionSet{
			QuestionCollections: []question.QuestionCollection{{Questions: qs}},
		},
	}
}

// parseLines splits an NDJSON body into decoded lines, skipping blanks.
func parseLines(t *testing.T, body string) []ndLine {
	t.Helper()
	var lines []ndLine
	for _, raw := range strings.Split(body, "\n") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		var l ndLine
		if err := json.Unmarshal([]byte(raw), &l); err != nil {
			t.Fatalf("unmarshal ndjson line %q: %v", raw, err)
		}
		lines = append(lines, l)
	}
	return lines
}

func TestExamHandler(t *testing.T) {
	tests := []struct {
		name            string
		method          string
		sources         []question.ExamSourceEntry
		wantStatus      int
		wantContentType string
		check           func(t *testing.T, rr *httptest.ResponseRecorder)
	}{
		{
			name:   "GET streams exams as NDJSON excerpts",
			method: http.MethodGet,
			sources: []question.ExamSourceEntry{
				{
					Loader: &fakeLoader{byURL: map[string]*question.Exam{
						"u1": examWith("A", "cA", 1),
						"u2": examWith("B", "cB", 1, 2),
					}},
					URLs: []string{"u1", "u2"},
				},
			},
			wantStatus:      http.StatusOK,
			wantContentType: "application/x-ndjson",
			check: func(t *testing.T, rr *httptest.ResponseRecorder) {
				if got := rr.Header().Get("X-Accel-Buffering"); got != "no" {
					t.Errorf("X-Accel-Buffering = %q, want no", got)
				}
				lines := parseLines(t, rr.Body.String())
				if len(lines) != 2 {
					t.Fatalf("got %d lines, want 2 (body %q)", len(lines), rr.Body.String())
				}
				cases := []struct {
					id    string
					num   int
					total float32
				}{
					{"A", 1, 1},
					{"B", 2, 3},
				}
				for i, c := range cases {
					if lines[i].Err != "" {
						t.Errorf("line %d: unexpected Err %q", i, lines[i].Err)
					}
					if lines[i].Data == nil {
						t.Fatalf("line %d: nil Data", i)
					}
					if lines[i].Data.Id != c.id || lines[i].Data.NumQuestions != c.num || lines[i].Data.TotalScores != c.total {
						t.Errorf("line %d: Data = %+v, want {Id:%s NumQuestions:%d TotalScores:%g}",
							i, lines[i].Data, c.id, c.num, c.total)
					}
				}
			},
		},
		{
			name:   "GET reports load failures as in-band Err lines",
			method: http.MethodGet,
			sources: []question.ExamSourceEntry{
				{
					Loader: &fakeLoader{
						byURL:  map[string]*question.Exam{"ok": examWith("A", "cA", 1)},
						errURL: map[string]bool{"bad": true},
					},
					URLs: []string{"ok", "bad"},
				},
			},
			wantStatus:      http.StatusOK,
			wantContentType: "application/x-ndjson",
			check: func(t *testing.T, rr *httptest.ResponseRecorder) {
				lines := parseLines(t, rr.Body.String())
				if len(lines) != 2 {
					t.Fatalf("got %d lines, want 2 (body %q)", len(lines), rr.Body.String())
				}
				if lines[0].Data == nil || lines[0].Data.Id != "A" {
					t.Errorf("line 0 Data = %+v, want exam A", lines[0].Data)
				}
				if lines[0].Err != "" {
					t.Errorf("line 0: unexpected Err %q", lines[0].Err)
				}
				if lines[1].Data != nil {
					t.Errorf("line 1 Data = %+v, want nil", lines[1].Data)
				}
				// The repository wraps the load error with the offending URL.
				if !strings.Contains(lines[1].Err, "bad") {
					t.Errorf("line 1 Err = %q, want it to mention the failing URL %q", lines[1].Err, "bad")
				}
			},
		},
		{
			name:            "GET with no sources streams an empty body",
			method:          http.MethodGet,
			sources:         nil,
			wantStatus:      http.StatusOK,
			wantContentType: "application/x-ndjson",
			check: func(t *testing.T, rr *httptest.ResponseRecorder) {
				if strings.TrimSpace(rr.Body.String()) != "" {
					t.Errorf("got body %q, want empty", rr.Body.String())
				}
			},
		},
		{
			name:            "non-GET responds 405",
			method:          http.MethodPost,
			sources:         nil,
			wantStatus:      http.StatusMethodNotAllowed,
			wantContentType: "text/plain",
			check: func(t *testing.T, rr *httptest.ResponseRecorder) {
				if !strings.Contains(rr.Body.String(), "method not allowed") {
					t.Errorf("got body %q, want it to mention method not allowed", rr.Body.String())
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sm := session.NewOnMemorySessionManager()
			h := examdocs.NewExamHandler(sm, question.NewExamRepository([]question.ExamSource{question.NewStaticFileExamSource(tc.sources)}))
			rr := serveRequest(t, h, sm, tc.method, "/api/examdocs", true)

			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", rr.Code, tc.wantStatus, rr.Body.String())
			}
			if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, tc.wantContentType) {
				t.Errorf("Content-Type = %q, want it to contain %q", ct, tc.wantContentType)
			}
			if tc.check != nil {
				tc.check(t, rr)
			}
		})
	}
}

// TestExamHandler_PerUserFirst verifies the merged listing: the caller's
// per-user exams stream ahead of the system-wide ones, other users' exams are
// not visible, and a per-user source failure degrades to an in-band Err line
// without suppressing the system exams.
func TestExamHandler_PerUserFirst(t *testing.T) {
	newHandler := func(t *testing.T, src question.ExamSource) (http.Handler, *session.OnMemorySessionManager) {
		t.Helper()
		sm := session.NewOnMemorySessionManager()
		return examdocs.NewExamHandler(sm, question.NewExamRepository([]question.ExamSource{src})), sm
	}

	systemEntries := []question.ExamSourceEntry{
		{Loader: &fakeLoader{byURL: map[string]*question.Exam{"sys": examWith("SYS", "cS", 1)}}, URLs: []string{"sys"}},
	}
	userEntries := []question.ExamSourceEntry{
		{Loader: &fakeLoader{byURL: map[string]*question.Exam{"usr": examWith("USR", "cU", 1)}}, URLs: []string{"usr"}},
	}

	t.Run("caller sees its own exams first, then system exams", func(t *testing.T) {
		h, sm := newHandler(t, &fakeSource{system: systemEntries, perUser: map[string][]question.ExamSourceEntry{"subject-test": userEntries}})
		rr := serveRequest(t, h, sm, http.MethodGet, "/api/examdocs", true)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %q)", rr.Code, rr.Body.String())
		}
		lines := parseLines(t, rr.Body.String())
		if len(lines) != 2 {
			t.Fatalf("got %d lines, want 2 (body %q)", len(lines), rr.Body.String())
		}
		if lines[0].Data == nil || lines[0].Data.Id != "USR" {
			t.Errorf("line 0 = %+v, want the per-user exam USR first", lines[0])
		}
		if lines[1].Data == nil || lines[1].Data.Id != "SYS" {
			t.Errorf("line 1 = %+v, want the system exam SYS second", lines[1])
		}
	})

	t.Run("other users' per-user exams are not visible", func(t *testing.T) {
		h, sm := newHandler(t, &fakeSource{system: systemEntries, perUser: map[string][]question.ExamSourceEntry{"someone-else": userEntries}})
		rr := serveRequest(t, h, sm, http.MethodGet, "/api/examdocs", true)
		lines := parseLines(t, rr.Body.String())
		if len(lines) != 1 || lines[0].Data == nil || lines[0].Data.Id != "SYS" {
			t.Fatalf("lines = %+v, want only the system exam SYS", lines)
		}
	})

	t.Run("per-user source failure is an in-band Err line; system exams still stream", func(t *testing.T) {
		h, sm := newHandler(t, &fakeSource{system: systemEntries, userErr: errors.New("vfs unavailable")})
		rr := serveRequest(t, h, sm, http.MethodGet, "/api/examdocs", true)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %q)", rr.Code, rr.Body.String())
		}
		lines := parseLines(t, rr.Body.String())
		if len(lines) != 2 {
			t.Fatalf("got %d lines, want 2 (body %q)", len(lines), rr.Body.String())
		}
		if !strings.Contains(lines[0].Err, "vfs unavailable") {
			t.Errorf("line 0 = %+v, want the per-user source error first", lines[0])
		}
		if lines[1].Data == nil || lines[1].Data.Id != "SYS" {
			t.Errorf("line 1 = %+v, want the system exam SYS second", lines[1])
		}
	})

	t.Run("missing session in context responds 500", func(t *testing.T) {
		h, sm := newHandler(t, &fakeSource{system: systemEntries})
		rr := serveRequest(t, h, sm, http.MethodGet, "/api/examdocs", false)
		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500 (body %q)", rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "session not found") {
			t.Errorf("body = %q, want it to mention session not found", rr.Body.String())
		}
	})
}

// TestExamHandler_ByLabel exercises the label-filtered listing: the query
// string parses into a LabelFilter (OR within a repeated key, AND across
// keys), non-matching exams are dropped from the stream, and in-band Err
// lines still surface. The plain path ignores query parameters entirely.
func TestExamHandler_ByLabel(t *testing.T) {
	labeled := func(id string, kv ...string) *question.Exam {
		e := examWith(id, "c"+id, 1)
		for i := 0; i+1 < len(kv); i += 2 {
			e.Labels = append(e.Labels, question.Label{Key: kv[i], Value: kv[i+1]})
		}
		return e
	}
	sources := []question.ExamSourceEntry{
		{
			Loader: &fakeLoader{byURL: map[string]*question.Exam{
				"a": labeled("A", "label1", "a", "label2", "c"),
				"b": labeled("B", "label1", "b", "label2", "c"),
				"c": labeled("C", "label1", "a"),
				"d": labeled("D", "label1", "x", "label2", "c"),
				"e": labeled("E"),
			},
				errURL: map[string]bool{"bad": true},
			},
			URLs: []string{"a", "b", "c", "d", "e", "bad"},
		},
	}

	newHandler := func(t *testing.T) (http.Handler, *session.OnMemorySessionManager) {
		t.Helper()
		sm := session.NewOnMemorySessionManager()
		return examdocs.NewExamHandler(sm, question.NewExamRepository([]question.ExamSource{question.NewStaticFileExamSource(sources)})), sm
	}

	// dataIDs returns the ids of the Data lines, in stream order.
	dataIDs := func(t *testing.T, rr *httptest.ResponseRecorder) []string {
		t.Helper()
		var ids []string
		for _, l := range parseLines(t, rr.Body.String()) {
			if l.Data != nil {
				ids = append(ids, l.Data.Id)
			}
		}
		return ids
	}

	t.Run("OR within a key, AND across keys", func(t *testing.T) {
		h, sm := newHandler(t)
		rr := serveRequest(t, h, sm, http.MethodGet, "/api/examdocs/bylabel?label1=a&label1=b&label2=c", true)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %q)", rr.Code, rr.Body.String())
		}
		got := dataIDs(t, rr)
		if len(got) != 2 || got[0] != "A" || got[1] != "B" {
			t.Errorf("ids = %v, want [A B]", got)
		}
	})

	t.Run("no query parameters matches every exam", func(t *testing.T) {
		h, sm := newHandler(t)
		rr := serveRequest(t, h, sm, http.MethodGet, "/api/examdocs/bylabel", true)
		got := dataIDs(t, rr)
		if len(got) != 5 {
			t.Errorf("ids = %v, want all 5 exams", got)
		}
	})

	t.Run("load failures still surface as Err lines", func(t *testing.T) {
		h, sm := newHandler(t)
		rr := serveRequest(t, h, sm, http.MethodGet, "/api/examdocs/bylabel?label2=c", true)
		lines := parseLines(t, rr.Body.String())
		var errs int
		for _, l := range lines {
			if l.Err != "" {
				errs++
			}
		}
		if errs != 1 {
			t.Errorf("got %d Err lines, want 1 (body %q)", errs, rr.Body.String())
		}
	})

	t.Run("plain path ignores query parameters", func(t *testing.T) {
		h, sm := newHandler(t)
		rr := serveRequest(t, h, sm, http.MethodGet, "/api/examdocs?label1=a", true)
		got := dataIDs(t, rr)
		if len(got) != 5 {
			t.Errorf("ids = %v, want all 5 exams (query ignored)", got)
		}
	})

	t.Run("unknown subpath responds 404", func(t *testing.T) {
		h, sm := newHandler(t)
		rr := serveRequest(t, h, sm, http.MethodGet, "/api/examdocs/bogus", true)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (body %q)", rr.Code, rr.Body.String())
		}
	})
}

// failingWriter is an http.ResponseWriter whose Write always errors, simulating
// a client that has disconnected mid-stream. It also implements http.Flusher.
type failingWriter struct {
	header http.Header
}

func (w *failingWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}
func (w *failingWriter) Write([]byte) (int, error) { return 0, errors.New("client gone") }
func (w *failingWriter) WriteHeader(int)           {}
func (w *failingWriter) Flush()                    {}

// TestExamHandler_ClientDisconnect exercises the drain path: when the writer
// fails, ServeHTTP must keep consuming ListExamDocuments' unbuffered channel so
// the producer goroutine is not left blocked, and must return rather than hang.
func TestExamHandler_ClientDisconnect(t *testing.T) {
	repo := question.NewExamRepository([]question.ExamSource{question.NewStaticFileExamSource([]question.ExamSourceEntry{
		{
			Loader: &fakeLoader{byURL: map[string]*question.Exam{
				"u1": examWith("A", "cA", 1),
				"u2": examWith("B", "cB", 1),
			}},
			URLs: []string{"u1", "u2"},
		},
	})})
	sm := session.NewOnMemorySessionManager()
	h := examdocs.NewExamHandler(sm, repo)
	r := httptest.NewRequest(http.MethodGet, "/api/examdocs", nil)
	r = r.WithContext(context.WithValue(r.Context(), pkgutils.CtxKeySubjectId, "subject-test"))
	wrapped := session.WithSessionId(h, sm)

	done := make(chan struct{})
	go func() {
		wrapped.ServeHTTP(&failingWriter{}, r)
		close(done)
	}()
	select {
	case <-done:
		// ServeHTTP returned despite every Write failing: the handler drained the
		// stream instead of blocking on the broken writer or the producer.
	case <-time.After(2 * time.Second):
		t.Fatal("ServeHTTP hung after client disconnect; stream was not drained")
	}
}
