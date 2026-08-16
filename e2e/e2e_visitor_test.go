package personalsite

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	pkgapiexamdocs "personal-site/pkg/api/examdocs"
	pkgapiexamsessions "personal-site/pkg/api/examsessions"
	pkgapiexamtrackings "personal-site/pkg/api/examtrackings"
	pkgapiloginvisitor "personal-site/pkg/api/login/visitor"
	pkgauth "personal-site/pkg/auth"
	pkgcookie "personal-site/pkg/cookie"
	pkgmodelsexamreport "personal-site/pkg/models/examreport"
	pkgmodelsexamserver "personal-site/pkg/models/examserver"
	pkgmodelsquestion "personal-site/pkg/models/question"
	pkgsession "personal-site/pkg/session"
)

// TestE2E_VisitorPracticeAndCertification exercises the full HTTP stack as a
// real visitor: it logs in through /api/login/visitor (which builds the session
// cookie via the injected CookieBuilder), then walks a complete practice exam
// and a complete certification exam end-to-end, finishing by listing the exam
// tracking reports.
func TestE2E_VisitorPracticeAndCertification(t *testing.T) {
	// --- Wire up the full server (mirrors main.go) -------------------------

	repo := pkgmodelsquestion.NewExamRepository([]pkgmodelsquestion.ExamSource{
		pkgmodelsquestion.NewStaticFileExamSource([]pkgmodelsquestion.ExamSourceEntry{
			{Loader: pkgmodelsquestion.NewFileExamLoader(), URLs: []string{
				filepath.Join("..", "exam1.xml"), // certification-exam (DCNA / DC-01)
				filepath.Join("..", "exam2.xml"), // practice-exam (DCPE / DC-02)
			}},
		}),
	})

	trackingServer := pkgmodelsexamreport.NewOnMemoryExamTrackingServer(nil, nil)
	examServer := pkgmodelsexamserver.NewOnMemoryExamServer(trackingServer, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go examServer.Run(ctx)
	t.Cleanup(examServer.Shutdown)

	sm := pkgsession.NewOnMemorySessionManager()
	mux := http.NewServeMux()
	mux.Handle("/api/examdocs", pkgapiexamdocs.NewExamHandler(sm, repo))
	mux.Handle("/api/examsessions", pkgapiexamsessions.NewExamSessionHandler(sm, examServer, repo))
	mux.Handle("/api/examsessions/", pkgapiexamsessions.NewExamSessionHandler(sm, examServer, repo))
	mux.Handle("/api/examtrackings", pkgapiexamtrackings.NewExamTrackingsHandler(sm, trackingServer))

	// --- Visitor login handler with dependency-injected CookieBuilder -------

	jwtSecret := []byte("e2e-test-secret")
	keyProvider := pkgauth.NewStaticSecretProvider(jwtSecret)
	tokenIssuer := pkgauth.NewStaticKeyJWTIssuer(keyProvider, "e2e-issuer")
	tickIssuer := pkgauth.NewSharedTickingTicketGenerator(5 * time.Millisecond)
	tickIssuer.Run(ctx)

	// The SimpleCookieBuilder is created once and reused, exactly as main.go
	// does. This is the code path the refactoring introduced.
	cookieBuilder := &pkgcookie.SimpleCookieBuilder{}
	visitorLoginHandler := pkgapiloginvisitor.NewVisitorLoginHandler(
		tokenIssuer,
		time.Hour,
		tickIssuer,
		cookieBuilder,
	)
	mux.Handle("/api/login/visitor", visitorLoginHandler)

	jwtValidator := pkgauth.NewStaticKeyJWTValidator(keyProvider, pkgauth.NewNullBlackListProvider(), false)

	var h http.Handler = mux
	h = pkgsession.WithSessionId(h, sm)
	h = pkgauth.WithWhiteListJWTAuth(h, jwtValidator, []string{"/api/login", "/api/login/", "/api/logout"}, nil)

	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	// --- Log in as a visitor (the cookie is built by CookieBuilder) --------

	t.Log("logging in as a visitor via /api/login/visitor")
	jwtCookieValue := loginAsVisitor(t, ts.URL+"/api/login/visitor")
	if jwtCookieValue == "" {
		t.Fatal("visitor login did not set a jwt cookie")
	}
	t.Logf("visitor session cookie obtained (jwt=%q)", truncate([]byte(jwtCookieValue), 30))

	// A client that sends the visitor's cookie on every request. The JWT auth
	// middleware reads the token from the "jwt" cookie.
	client := &http.Client{Timeout: 10 * time.Second}
	authGet := func(path string) []byte {
		return cookieReq(t, client, ts.URL, http.MethodGet, path, "", jwtCookieValue)
	}
	authPost := func(path, body string) []byte {
		return cookieReq(t, client, ts.URL, http.MethodPost, path, body, jwtCookieValue)
	}
	authDelete := func(path string) {
		cookieReq(t, client, ts.URL, http.MethodDelete, path, "", jwtCookieValue)
	}

	// --- Discover the available exams --------------------------------------

	type examDoc struct {
		Data *struct {
			Id           string `json:"id"`
			ShortName    string `json:"shortName"`
			Code         string `json:"code"`
			Title        string `json:"title"`
			NumQuestion  int    `json:"numQuestions"`
			ExamCategory string `json:"examCategory"`
		} `json:"Data,omitempty"`
		Err string `json:"Err,omitempty"`
	}
	docsBody := authGet("/api/examdocs")
	t.Logf("exam docs (NDJSON):\n%s", docsBody)

	var practiceExamID, certExamID string
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
		t.Logf("  exam: id=%s code=%s title=%q category=%s questions=%d",
			doc.Data.Id, doc.Data.Code, doc.Data.Title, doc.Data.ExamCategory, doc.Data.NumQuestion)
		switch doc.Data.ExamCategory {
		case string(pkgmodelsquestion.ExamCategoryPractice):
			practiceExamID = doc.Data.Id
		case string(pkgmodelsquestion.ExamCategoryCertification):
			certExamID = doc.Data.Id
		}
	}
	if practiceExamID == "" {
		t.Fatal("no practice-exam found in exam docs")
	}
	if certExamID == "" {
		t.Fatal("no certification-exam found in exam docs")
	}

	// --- Walk the practice exam --------------------------------------------

	t.Run("practice-exam", func(t *testing.T) {
		sessionID := startExamSession(t, authPost, practiceExamID)
		walkExam(t, authGet, authPost, authDelete, sessionID)
	})

	// --- List reports after the practice exam ------------------------------

	t.Log("listing exam trackings after practice exam")
	reports := listTrackings(t, authGet)
	if len(reports) < 1 {
		t.Fatalf("expected >=1 report after practice exam, got %d", len(reports))
	}
	for _, r := range reports {
		t.Logf("  report: code=%s title=%q sessionId=%s", r.ExamCode, r.Title, r.ExamSessionId)
	}

	// --- Walk the certification exam ---------------------------------------

	t.Run("certification-exam", func(t *testing.T) {
		sessionID := startExamSession(t, authPost, certExamID)
		walkExam(t, authGet, authPost, authDelete, sessionID)
	})

	// --- List reports again — both exams should be present -----------------

	t.Log("listing exam trackings after certification exam")
	reports = listTrackings(t, authGet)
	if len(reports) < 2 {
		t.Fatalf("expected >=2 reports after both exams, got %d", len(reports))
	}
	for _, r := range reports {
		t.Logf("  report: code=%s title=%q sessionId=%s", r.ExamCode, r.Title, r.ExamSessionId)
	}

	t.Log("e2e visitor practice + certification exams completed successfully")
}

// startExamSession POSTs to /api/examsessions to create a session and returns
// the new exam_session_id.
func startExamSession(t *testing.T, authPost func(path, body string) []byte, examID string) string {
	t.Helper()
	body := authPost("/api/examsessions", fmt.Sprintf(`{"exam_id":%q}`, examID))
	var resp struct {
		ExamSessionID string `json:"exam_session_id"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode create-session response %q: %v", body, err)
	}
	if resp.ExamSessionID == "" {
		t.Fatalf("empty exam_session_id in create response: %s", body)
	}
	t.Logf("started exam session %s for exam %q", resp.ExamSessionID, examID)
	return resp.ExamSessionID
}

// walkExam fetches and answers every question until none remain, then ends the
// session so a tracking report is persisted.
func walkExam(t *testing.T, authGet func(string) []byte, authPost func(string, string) []byte, authDelete func(string), sessionID string) {
	t.Helper()
	var cursorID *string
	answered := 0

	for {
		questionsURL := fmt.Sprintf("/api/examsessions/%s/questions", sessionID)
		if cursorID != nil {
			questionsURL += "?cursor_id=" + *cursorID
		}

		qBody := authGet(questionsURL)
		t.Logf("get next question (%s): %s", questionsURL, truncate(qBody, 200))

		var qResp struct {
			CursorID *string          `json:"cursor_id"`
			Question *json.RawMessage `json:"question"`
		}
		if err := json.Unmarshal(qBody, &qResp); err != nil {
			t.Fatalf("decode question response: %v", err)
		}
		if qResp.Question == nil {
			t.Logf("  no more questions")
			break
		}

		var q struct {
			Id   string `json:"id"`
			Type string `json:"type"`
		}
		json.Unmarshal(*qResp.Question, &q)
		t.Logf("  question id=%s type=%s", q.Id, q.Type)

		answer := buildAnswer(q.Id, q.Type)
		submitBody, _ := json.Marshal(map[string]any{"answers": []map[string]any{answer}})
		submitResp := authPost(fmt.Sprintf("/api/examsessions/%s/answer", sessionID), string(submitBody))
		t.Logf("  submit response: %s", truncate(submitResp, 200))
		answered++

		cursorID = qResp.CursorID
		if cursorID == nil {
			t.Logf("  last question answered")
			break
		}
	}
	if answered == 0 {
		t.Fatalf("exam session %s had no questions", sessionID)
	}
	t.Logf("answered %d questions in session %s, ending session", answered, sessionID)
	authDelete(fmt.Sprintf("/api/examsessions/%s", sessionID))
}

// listTrackings fetches /api/examtrackings and returns the parsed report list.
func listTrackings(t *testing.T, authGet func(string) []byte) []struct {
	ExamCode      string `json:"examCode"`
	Title         string `json:"title"`
	ExamSessionId string `json:"examSessionId"`
} {
	t.Helper()
	body := authGet("/api/examtrackings")
	t.Logf("trackings response:\n%s", body)
	var resp struct {
		ExamReports []struct {
			ExamCode      string `json:"examCode"`
			Title         string `json:"title"`
			ExamSessionId string `json:"examSessionId"`
		} `json:"exam_reports"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode trackings: %v", err)
	}
	return resp.ExamReports
}

// loginAsVisitor hits the visitor login endpoint and extracts the JWT cookie
// value from the Set-Cookie header. It uses a client that does NOT follow the
// TemporaryRedirect so the cookie is captured directly from the login response.
func loginAsVisitor(t *testing.T, loginURL string) string {
	t.Helper()
	client := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // don't follow the redirect to "/"
		},
	}
	resp, err := client.Get(loginURL)
	if err != nil && resp == nil {
		t.Fatalf("visitor login request failed: %v", err)
	}
	if resp != nil {
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
	}
	if resp == nil || (resp.StatusCode < 200 || resp.StatusCode >= 400) {
		status := "(nil)"
		if resp != nil {
			status = fmt.Sprintf("%d", resp.StatusCode)
		}
		t.Fatalf("visitor login returned unexpected status %s", status)
	}
	for _, c := range resp.Cookies() {
		if c.Name == "jwt" {
			return c.Value
		}
	}
	return ""
}

// cookieReq performs an HTTP request carrying the visitor's JWT cookie.
func cookieReq(t *testing.T, client *http.Client, baseURL, method, path, body, jwtCookie string) []byte {
	t.Helper()
	var bodyReader io.Reader
	if body != "" {
		bodyReader = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequest(method, baseURL+path, bodyReader)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	req.AddCookie(&http.Cookie{Name: "jwt", Value: jwtCookie})
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
