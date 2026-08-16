package personalsite

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"path/filepath"

	pkgapiexamdocs "personal-site/pkg/api/examdocs"
	pkgapiexamsessions "personal-site/pkg/api/examsessions"
	pkgapiexamtrackings "personal-site/pkg/api/examtrackings"
	pkgauth "personal-site/pkg/auth"
	pkgmodelsexamreport "personal-site/pkg/models/examreport"
	pkgmodelsexamserver "personal-site/pkg/models/examserver"
	pkgmodelsquestion "personal-site/pkg/models/question"
	pkgsession "personal-site/pkg/session"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// TestE2E_PracticeExam walks a full exam session through the real HTTP stack:
//
//  1. Fetch all exam docs
//  2. Start a new exam session
//  3. Mint the first cursor (GetNextQuestion advances the index from -1 to 0)
//  4. Fetch and answer every question one-by-one
//  5. End the session (persisting a tracking report)
//  6. List the exam trackings
//
// The handler chain mirrors main.go exactly, so the request traverses the real
// JWT auth middleware and the stateless session middleware.
func TestE2E_PracticeExam(t *testing.T) {
	// --- Wire up the full server (same as main.go) --------------------------

	repo := pkgmodelsquestion.NewExamRepository([]pkgmodelsquestion.ExamSource{
		pkgmodelsquestion.NewStaticFileExamSource([]pkgmodelsquestion.ExamSourceEntry{
			{Loader: pkgmodelsquestion.NewFileExamLoader(), URLs: []string{filepath.Join("..", "exam1.xml")}},
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
	mux.Handle("/api/examtrackings", pkgapiexamtrackings.NewExamTrackingsHandler(sm, trackingServer))

	jwtSecret := []byte("e2e-test-secret")
	keyProvider := pkgauth.NewStaticSecretProvider(jwtSecret)
	jwtValidator := pkgauth.NewStaticKeyJWTValidator(keyProvider, pkgauth.NewNullBlackListProvider(), false)

	var h http.Handler = mux
	h = pkgsession.WithSessionId(h, sm)
	h = pkgauth.WithWhiteListJWTAuth(h, jwtValidator, []string{"/api/login", "/api/login/", "/api/logout"}, nil)

	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	// --- Mint a JWT (same shape the login handlers produce) ------------------

	jwtIssuer := pkgauth.NewStaticKeyJWTIssuer(keyProvider, "e2e-issuer")
	subjectID := "subject-" + uuid.NewString()
	token := mintToken(t, jwtIssuer, subjectID, "e2e-tester")

	client := &http.Client{Timeout: 10 * time.Second}
	authGet := func(path string) []byte {
		return doReq(t, client, ts.URL, http.MethodGet, path, "", token)
	}
	authPost := func(path, body string) []byte {
		return doReq(t, client, ts.URL, http.MethodPost, path, body, token)
	}
	authDelete := func(path string) {
		doReq(t, client, ts.URL, http.MethodDelete, path, "", token)
	}

	// --- 1. Fetch all exam docs ----------------------------------------------

	t.Log("step 1: fetching exam docs")
	docsBody := authGet("/api/examdocs")
	t.Logf("exam docs (NDJSON):\n%s", docsBody)
	type examDoc struct {
		Data *struct {
			Id          string `json:"id"`
			ShortName   string `json:"shortName"`
			Code        string `json:"code"`
			Title       string `json:"title"`
			NumQuestion int    `json:"numQuestions"`
		} `json:"Data,omitempty"`
		Err string `json:"Err,omitempty"`
	}
	var examID string
	for _, line := range bytes.Split(bytes.TrimSpace(docsBody), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var doc examDoc
		if err := json.Unmarshal(line, &doc); err != nil {
			t.Fatalf("decode exam doc line %q: %v", line, err)
		}
		if doc.Err != "" {
			t.Fatalf("exam doc load error: %s", doc.Err)
		}
		t.Logf("  exam: id=%s code=%s title=%q questions=%d", doc.Data.Id, doc.Data.Code, doc.Data.Title, doc.Data.NumQuestion)
		if examID == "" {
			examID = doc.Data.Id
		}
	}
	if examID == "" {
		t.Fatal("no exam documents returned")
	}

	// --- 2. Start a new exam session -----------------------------------------

	t.Logf("step 2: starting exam session for exam %q", examID)
	createBody := authPost("/api/examsessions", fmt.Sprintf(`{"exam_id":%q}`, examID))
	t.Logf("create response: %s", createBody)
	var createResp struct {
		ExamSessionID string `json:"exam_session_id"`
	}
	json.Unmarshal(createBody, &createResp)
	sessionID := createResp.ExamSessionID
	if sessionID == "" {
		t.Fatalf("empty exam_session_id in create response: %s", createBody)
	}
	t.Logf("  exam session id: %s", sessionID)

	// --- 3 & 4. Fetch and answer all questions one-by-one --------------------

	var allAnswers []map[string]any
	questionNo := 0
	var cursorID *string

	for {
		questionNo++
		// Build the questions URL, appending cursor_id when we have one.
		questionsURL := fmt.Sprintf("/api/examsessions/%s/questions", sessionID)
		if cursorID != nil {
			questionsURL += "?cursor_id=" + *cursorID
		}

		t.Logf("step 3.%d: get next question (%s)", questionNo, questionsURL)
		qBody := authGet(questionsURL)
		t.Logf("  question response: %s", truncate(qBody, 200))

		var qResp struct {
			CursorID *string         `json:"cursor_id"`
			Question *json.RawMessage `json:"question"`
		}
		if err := json.Unmarshal(qBody, &qResp); err != nil {
			t.Fatalf("decode question response: %v", err)
		}

		if qResp.Question == nil {
			t.Logf("  no more questions (question is null)")
			break
		}

		// Inspect the question to build an answer.
		var q struct {
			Id   string `json:"id"`
			Type string `json:"type"`
		}
		json.Unmarshal(*qResp.Question, &q)
		t.Logf("  question id=%s type=%s", q.Id, q.Type)

		// Answer with a plausible (not necessarily correct) answer so the
		// grader produces an assessment.
		answer := buildAnswer(q.Id, q.Type)
		allAnswers = append(allAnswers, answer)

		// Submit the answer for this question (persist, not check-only).
		submitBody, _ := json.Marshal(map[string]any{"answers": []map[string]any{answer}})
		t.Logf("step 4.%d: submit answer for question %s", questionNo, q.Id)
		submitResp := authPost(fmt.Sprintf("/api/examsessions/%s/answer", sessionID), string(submitBody))
		t.Logf("  submit response: %s", truncate(submitResp, 300))

		cursorID = qResp.CursorID
		if cursorID == nil {
			t.Logf("  no next cursor — this was the last question")
			break
		}
	}

	if questionNo == 1 {
		t.Fatal("expected at least one question but got none")
	}
	t.Logf("answered %d questions total", len(allAnswers))

	// --- 5. End the session (persist a tracking report) ----------------------

	t.Logf("step 5: ending exam session %s", sessionID)
	authDelete(fmt.Sprintf("/api/examsessions/%s", sessionID))
	t.Log("  session ended")

	// --- 6. List the exam trackings ------------------------------------------

	t.Log("step 6: listing exam trackings")
	trackingsBody := authGet("/api/examtrackings")
	t.Logf("trackings response:\n%s", trackingsBody)

	var trackings struct {
		ExamReports []struct {
			Id            string `json:"id"`
			ExamId        string `json:"examId"`
			ExamShortName string `json:"examShortName"`
			ExamCode      string `json:"examCode"`
			Title         string `json:"title"`
			ExamSessionId string `json:"examSessionId"`
		} `json:"exam_reports"`
	}
	if err := json.Unmarshal(trackingsBody, &trackings); err != nil {
		t.Fatalf("decode trackings: %v", err)
	}
	if len(trackings.ExamReports) == 0 {
		t.Fatalf("expected at least 1 exam report after ending the session, got 0 (body %s)", trackingsBody)
	}
	for _, r := range trackings.ExamReports {
		t.Logf("  report: id=%s examId=%s code=%s title=%q sessionId=%s",
			r.Id, r.ExamId, r.ExamCode, r.Title, r.ExamSessionId)
	}

	// The finished session's exam id must match the one we started.
	if trackings.ExamReports[0].ExamId != examID {
		t.Errorf("tracking examId = %q, want %q", trackings.ExamReports[0].ExamId, examID)
	}
	if trackings.ExamReports[0].ExamSessionId != sessionID {
		t.Errorf("tracking examSessionId = %q, want %q", trackings.ExamReports[0].ExamSessionId, sessionID)
	}
	t.Log("e2e practice exam completed successfully")
}

// mintToken issues a signed JWT carrying the subject id, mirroring the claim
// shape produced by the login handlers (ID = session id, Subject = subject id,
// Audience = ["session"], Username = the display name).
func mintToken(t *testing.T, issuer *pkgauth.StaticKeyJWTIssuer, subjectID, username string) string {
	t.Helper()
	claims := &pkgauth.CustomClaimType{
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        uuid.NewString(),
			Subject:   subjectID,
			Audience:  []string{pkgauth.AudSession},
			NotBefore: jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		Username: username,
	}
	mapClaims, err := pkgauth.NewMapClaims(claims)
	if err != nil {
		t.Fatalf("NewMapClaims: %v", err)
	}
	token, err := issuer.IssueToken(context.Background(), mapClaims)
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	return token
}

// buildAnswer constructs a best-effort answer for a question. For single-choice
// and multiple-choice it selects the first option (option id "1"); for
// drag-and-drop it connects the first candidate to the first drop.
func buildAnswer(questionID, questionType string) map[string]any {
	switch questionType {
	case "single-choice", "multiple-choice":
		return map[string]any{
			"questionId":   questionID,
			"questionType": questionType,
			"options":      []map[string]any{{"id": "1"}},
		}
	case "drag-and-drop":
		return map[string]any{
			"questionId":   questionID,
			"questionType": questionType,
			"connections":  []map[string]any{{"src": "node-a", "dst": "drop-a"}},
		}
	default:
		return map[string]any{
			"questionId":   questionID,
			"questionType": questionType,
			"options":      []map[string]any{{"id": "1"}},
		}
	}
}

// doReq performs an authenticated HTTP request and returns the response body.
// It fails the test if the status is not 2xx (unless the path is a DELETE,
// where 204 is expected and no body is returned).
func doReq(t *testing.T, client *http.Client, baseURL, method, path, body, token string) []byte {
	t.Helper()
	var bodyReader io.Reader
	if body != "" {
		bodyReader = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequest(method, baseURL+path, bodyReader)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if method == http.MethodDelete {
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("%s %s: status = %d, want %d (body %s)", method, path, resp.StatusCode, http.StatusNoContent, respBody)
		}
		return respBody
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Fatalf("%s %s: status = %d (body %s)", method, path, resp.StatusCode, respBody)
	}
	return respBody
}

// truncate returns the first n bytes of b as a string, with an ellipsis if
// truncated — keeps logs readable for long question JSON.
func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "...(truncated)"
}
