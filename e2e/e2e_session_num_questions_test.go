package personalsite

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

// TestE2E_ExamSessionNumQuestions verifies that GET
// /api/examsessions/{exam_session_id} reports the actual size of the
// per-session question collection — after collection selection — rather than
// some static document-level count. Three collection shapes are covered:
//
//   - default: every real collection flattened into one (sum of all sizes);
//   - ExamOptionRandomQuestionColl: exactly one real collection's size,
//     whichever was picked;
//   - a virtual collection: exactly its sample size, even though the
//     referenced collections are larger.
//
// Each reported count is cross-checked against the number of questions the
// session actually serves via GetNextQuestion.
func TestE2E_ExamSessionNumQuestions(t *testing.T) {
	// --- Exam documents ------------------------------------------------------

	dir := t.TempDir()

	// Exam "real-coll": three real collections of distinct sizes (2, 3, 4) so
	// a random collection pick is distinguishable from the flattened total.
	writeExamDoc(t, dir, "real.xml", examDocXML("real-coll", "certification-exam", "",
		[]string{
			genCollection("a", 2),
			genCollection("b", 3),
			genCollection("c", 4),
		}))

	// Exam "virtual-coll": a virtual collection sampling 5 questions from the
	// 8 questions of collections 0 and 1; collection 2 is unreferenced.
	writeExamDoc(t, dir, "virtual.xml", examDocXML("virtual-coll", "certification-exam",
		`<virtualcollection><samplesize>5</samplesize><collectionidx>0</collectionidx><collectionidx>1</collectionidx></virtualcollection>`,
		[]string{
			genCollection("a", 4),
			genCollection("b", 4),
			genCollection("c", 4),
		}))

	// --- Wire up the full server (same as main.go) --------------------------

	repo := pkgmodelsquestion.NewExamRepository([]pkgmodelsquestion.ExamSource{
		pkgmodelsquestion.NewStaticFileExamSource([]pkgmodelsquestion.ExamSourceEntry{
			{Loader: pkgmodelsquestion.NewFileExamLoader(), URLs: []string{
				filepath.Join(dir, "real.xml"),
				filepath.Join(dir, "virtual.xml"),
			}},
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

	// --- Cases ---------------------------------------------------------------

	t.Run("flattened real collections", func(t *testing.T) {
		sessionID := startExamSessionWithOptions(t, client, ts.URL, token, "real-coll", 0)
		got := sessionNumQuestions(t, client, ts.URL, token, sessionID)
		if got != 9 {
			t.Fatalf("NumQuestions = %d, want flattened total 9", got)
		}
		assertServedCount(t, client, ts.URL, token, sessionID, got)
	})

	t.Run("randomly picked real collection", func(t *testing.T) {
		sessionID := startExamSessionWithOptions(t, client, ts.URL, token, "real-coll",
			uint32(pkgmodelsexamserver.ExamOptionRandomQuestionColl))
		got := sessionNumQuestions(t, client, ts.URL, token, sessionID)
		// Exactly one of the real collections (sizes 2, 3, 4) is picked —
		// never the flattened 9, and never the first collection's 2 by fiat.
		if got != 2 && got != 3 && got != 4 {
			t.Fatalf("NumQuestions = %d, want one of {2, 3, 4} (a single collection)", got)
		}
		assertServedCount(t, client, ts.URL, token, sessionID, got)
	})

	t.Run("virtual collection", func(t *testing.T) {
		sessionID := startExamSessionWithOptions(t, client, ts.URL, token, "virtual-coll", 0)
		got := sessionNumQuestions(t, client, ts.URL, token, sessionID)
		if got != 5 {
			t.Fatalf("NumQuestions = %d, want the virtual collection's sample size 5", got)
		}
		assertServedCount(t, client, ts.URL, token, sessionID, got)
	})
}

// writeExamDoc writes an exam document to dir/name.
func writeExamDoc(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// examDocXML assembles an exam document with the given id, category, optional
// <virtualcollection> block, and question collections.
func examDocXML(id, category, vcBlock string, collections []string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<root>
<exam id="` + id + `" shortname="X" code="1">
  <title>t</title><description>d</description>
  <examcategory>` + category + `</examcategory>
  ` + vcBlock + `
  <questionset>` + strings.Join(collections, "") + `</questionset>
</exam>
</root>`
}

// genCollection emits a <questioncollection> of n single-choice questions
// whose ids share the given prefix.
func genCollection(prefix string, n int) string {
	var sb strings.Builder
	sb.WriteString("<questioncollection>")
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&sb, `<question id="%s%d" type="single-choice" score="1">`+
			`<description>q %s%d</description>`+
			`<options><option id="1">x</option><option id="2">y</option></options>`+
			`<correctanswer><options><option id="1">x</option></options></correctanswer>`+
			`</question>`, prefix, i, prefix, i)
	}
	sb.WriteString("</questioncollection>")
	return sb.String()
}

// startExamSessionWithOptions creates a session for examID with the given
// ExamOptions bitmask and returns the new exam session id.
func startExamSessionWithOptions(t *testing.T, client *http.Client, baseURL, token, examID string, options uint32) string {
	t.Helper()
	body := doReq(t, client, baseURL, http.MethodPost, "/api/examsessions",
		fmt.Sprintf(`{"exam_id":%q,"options":%d}`, examID, options), token)
	var resp struct {
		ExamSessionID string `json:"exam_session_id"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || resp.ExamSessionID == "" {
		t.Fatalf("create session: id=%q err=%v (body %s)", resp.ExamSessionID, err, body)
	}
	return resp.ExamSessionID
}

// sessionNumQuestions fetches GET /api/examsessions/{id} and returns the
// NumQuestions of the session's exam excerpt.
func sessionNumQuestions(t *testing.T, client *http.Client, baseURL, token, sessionID string) int {
	t.Helper()
	body := doReq(t, client, baseURL, http.MethodGet, "/api/examsessions/"+sessionID, "", token)
	var resp struct {
		ExamSession struct {
			ExamExcerpt struct {
				NumQuestions int `json:"NumQuestions"`
			} `json:"exam_excerpt"`
		} `json:"exam_session"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode session response: %v (body %s)", err, body)
	}
	return resp.ExamSession.ExamExcerpt.NumQuestions
}

// assertServedCount walks the session's questions via GetNextQuestion and
// verifies exactly want questions are served.
func assertServedCount(t *testing.T, client *http.Client, baseURL, token, sessionID string, want int) {
	t.Helper()
	n := 0
	var cursorID *string
	for {
		url := fmt.Sprintf("/api/examsessions/%s/questions", sessionID)
		if cursorID != nil {
			url += "?cursor_id=" + *cursorID
		}
		body := doReq(t, client, baseURL, http.MethodGet, url, "", token)
		var qResp struct {
			CursorID *string          `json:"cursor_id"`
			Question *json.RawMessage `json:"question"`
		}
		if err := json.Unmarshal(body, &qResp); err != nil {
			t.Fatalf("decode question response: %v", err)
		}
		if qResp.Question == nil {
			break
		}
		n++
		cursorID = qResp.CursorID
		if cursorID == nil {
			break
		}
	}
	if n != want {
		t.Fatalf("session serves %d questions, but NumQuestions reported %d", n, want)
	}
}
