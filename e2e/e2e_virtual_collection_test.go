package personalsite

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	pkgapiexamdocs "personal-site/pkg/api/examdocs"
	pkgapiexamsessions "personal-site/pkg/api/examsessions"
	pkgauth "personal-site/pkg/auth"
	pkgmodelsexamreport "personal-site/pkg/models/examreport"
	pkgmodelsexamserver "personal-site/pkg/models/examserver"
	pkgmodelsquestion "personal-site/pkg/models/question"
	pkgsession "personal-site/pkg/session"

	"github.com/google/uuid"
)

// TestE2E_VirtualCollectionSampleSize models a large question bank — the
// CARC-style scenario of 1200 questions on one exam topic — and verifies that
// a certification-exam session driven by a virtual collection serves exactly
// S questions, where S is the <samplesize> declared in the exam document.
// The session is drained via GetNextQuestion until the questions are
// exhausted; the served count N must equal S, every served question must be
// distinct, and none may come from the unreferenced collection.
func TestE2E_VirtualCollectionSampleSize(t *testing.T) {
	const sampleSize = 80

	// 600 + 600 referenced questions, plus an unreferenced collection of 50
	// that must never contribute to the sample.
	dir := t.TempDir()
	writeExamDoc(t, dir, "carc.xml", examDocXML("carc-class-c", "certification-exam",
		fmt.Sprintf(`<virtualcollection><samplesize>%d</samplesize><collectionidx>0</collectionidx><collectionidx>1</collectionidx></virtualcollection>`, sampleSize),
		[]string{
			genCollection("a", 600),
			genCollection("b", 600),
			genCollection("c", 50),
		}))

	// --- Wire up the full server (same as main.go) --------------------------

	repo := pkgmodelsquestion.NewExamRepository([]pkgmodelsquestion.ExamSource{
		pkgmodelsquestion.NewStaticFileExamSource([]pkgmodelsquestion.ExamSourceEntry{
			{Loader: pkgmodelsquestion.NewFileExamLoader(), URLs: []string{filepath.Join(dir, "carc.xml")}},
		}),
	})

	trackingServer := pkgmodelsexamreport.NewOnMemoryExamTrackingServer(nil, nil)
	examServer := pkgmodelsexamserver.NewOnMemoryExamServer(trackingServer, nil)
	go examServer.Run(context.Background())
	t.Cleanup(examServer.Shutdown)

	sm := pkgsession.NewOnMemorySessionManager()
	mux := http.NewServeMux()
	mux.Handle("/api/examdocs", pkgapiexamdocs.NewExamHandler(sm, repo))
	mux.Handle("/api/examsessions", pkgapiexamsessions.NewExamSessionHandler(sm, examServer, repo))
	mux.Handle("/api/examsessions/", pkgapiexamsessions.NewExamSessionHandler(sm, examServer, repo))

	jwtSecret := []byte("e2e-test-secret")
	keyProvider := pkgauth.NewStaticSecretProvider(jwtSecret)
	jwtValidator := pkgauth.NewStaticKeyJWTValidator(keyProvider, pkgauth.NewNullBlackListProvider(), false)

	var h http.Handler = mux
	h = pkgsession.WithSessionId(h, sm)
	h = pkgauth.WithWhiteListJWTAuth(h, jwtValidator, []string{"/api/login", "/api/login/", "/api/logout"}, nil)

	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	jwtIssuer := pkgauth.NewStaticKeyJWTIssuer(keyProvider, "e2e-issuer")
	token := mintToken(t, jwtIssuer, "subject-"+uuid.NewString(), "e2e-tester")
	client := &http.Client{Timeout: 10 * time.Second}

	// --- Start the session and drain its questions ---------------------------

	sessionID := startExamSessionWithOptions(t, client, ts.URL, token, "carc-class-c", 0)

	if got := sessionNumQuestions(t, client, ts.URL, token, sessionID); got != sampleSize {
		t.Fatalf("NumQuestions = %d, want sample size %d", got, sampleSize)
	}

	served := servedQuestionIDs(t, client, ts.URL, token, sessionID)

	if len(served) != sampleSize {
		t.Fatalf("N = %d questions served, want S = %d", len(served), sampleSize)
	}
	seen := make(map[string]bool, len(served))
	for _, id := range served {
		if seen[id] {
			t.Fatalf("question %q served twice", id)
		}
		seen[id] = true
		if id[0] == 'c' {
			t.Fatalf("question %q comes from the unreferenced collection", id)
		}
	}
	t.Logf("served N=%d distinct questions, matching the declared sample size S=%d", len(served), sampleSize)
}

// servedQuestionIDs drains a session via GetNextQuestion and returns the ids
// of every served question, in serve order.
func servedQuestionIDs(t *testing.T, client *http.Client, baseURL, token, sessionID string) []string {
	t.Helper()
	var ids []string
	var cursorID *string
	for {
		url := fmt.Sprintf("/api/examsessions/%s/questions", sessionID)
		if cursorID != nil {
			url += "?cursor_id=" + *cursorID
		}
		body := doReq(t, client, baseURL, http.MethodGet, url, "", token)
		var qResp struct {
			CursorID *string `json:"cursor_id"`
			Question *struct {
				Id string `json:"id"`
			} `json:"question"`
		}
		if err := json.Unmarshal(body, &qResp); err != nil {
			t.Fatalf("decode question response: %v", err)
		}
		if qResp.Question == nil {
			return ids
		}
		ids = append(ids, qResp.Question.Id)
		cursorID = qResp.CursorID
		if cursorID == nil {
			return ids
		}
	}
}
