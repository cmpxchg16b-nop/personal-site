package personalsite

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/spf13/afero"

	pkgapiexamassociations "personal-site/pkg/api/examassociations"
	pkgapiexamdocs "personal-site/pkg/api/examdocs"
	pkgapiexamsessions "personal-site/pkg/api/examsessions"
	pkgapiexamtrackings "personal-site/pkg/api/examtrackings"
	pkgapiloginvisitor "personal-site/pkg/api/login/visitor"
	pkgapiuseruploads "personal-site/pkg/api/useruploads"
	pkgauth "personal-site/pkg/auth"
	pkgcookie "personal-site/pkg/cookie"
	pkgmodelsexamreport "personal-site/pkg/models/examreport"
	pkgmodelsexamserver "personal-site/pkg/models/examserver"
	pkgmodelsquestion "personal-site/pkg/models/question"
	pkgmodelsuserexamdocsfsbasedassociation "personal-site/pkg/models/userexamdocs/fsbasedassociation"
	pkgmodelsuserupload "personal-site/pkg/models/userupload"
	pkgsession "personal-site/pkg/session"
)

// vfsPracticeExamXML is a valid exam document designed for VFS testing.
// It contains 4 questions covering single-choice, multiple-choice, and
// drag-and-drop, with asset references that will be rewritten to the
// per-upload dyn-assets endpoint.
const vfsPracticeExamXML = `<?xml version="1.0" encoding="UTF-8"?>
<root>
<exam id="vfs-practice-1" shortname="VFSP" code="VFS-01">
  <title>VFS Practice Exam</title>
  <description>A practice exam loaded from user-uploaded VFS tarball.</description>
  <passingscore>2.0</passingscore>
  <examcategory>practice-exam</examcategory>
  <questionset>
    <questioncollection>
      <question id="q1" type="single-choice" score="1">
        <description>What is the capital of France? (Choose one.)</description>
        <exhibits>
          <exhibit>
            <image src="/assets/exhibit1.png" />
          </exhibit>
        </exhibits>
        <options>
          <option id="1">Berlin</option>
          <option id="2">Paris</option>
          <option id="3">Madrid</option>
          <option id="4">Rome</option>
        </options>
        <correctanswer>
          <options><option id="2">Paris</option></options>
        </correctanswer>
      </question>
      <question id="q2" type="multiple-choice" score="1">
        <description>Which of the following are programming languages? (Choose two.)</description>
        <options>
          <option id="1">Go</option>
          <option id="2">HTML</option>
          <option id="3">Python</option>
          <option id="4">CSS</option>
        </options>
        <correctanswer>
          <options>
            <option id="1">Go</option>
            <option id="3">Python</option>
          </options>
        </correctanswer>
      </question>
      <question id="q3" type="single-choice" score="1">
        <description>Which protocol dynamically assigns IP addresses?</description>
        <options>
          <option id="1">DNS</option>
          <option id="2">DHCP</option>
          <option id="3">NAT</option>
          <option id="4">ARP</option>
        </options>
        <correctanswer>
          <options><option id="2">DHCP</option></options>
        </correctanswer>
      </question>
      <question id="q4" type="drag-and-drop" score="1">
        <description>Drag and place the snippets to the correct locations.</description>
        <imgDragAndDrop>
          <imgCandidate nodeId="node-a" nodeLabel="2" width="79" height="57" imgDataSrc="/assets/drag-p2-1.png" />
          <imgCandidate nodeId="node-b" nodeLabel="4" width="79" height="57" imgDataSrc="/assets/drag-p2-2.png" />
          <imgCandidate nodeId="node-c" nodeLabel="8" width="79" height="57" imgDataSrc="/assets/drag-p2-3.png" />
          <imgDropsArea imgBackgroundUrl="/assets/drag-p1.png" width="480" height="540">
            <imgDrop nodeId="drop-a" nodeLabel="drop 1" positionX="265" positionY="100" width="79" height="57" />
            <imgDrop nodeId="drop-b" nodeLabel="drop 2" positionX="265" positionY="200" width="79" height="57" />
            <imgDrop nodeId="drop-c" nodeLabel="drop 3" positionX="265" positionY="300" width="79" height="57" />
          </imgDropsArea>
        </imgDragAndDrop>
        <correctanswer>
          <connectionsolutions>
            <connectionsolution requiredUniqueConnections="3">
              <connect dst="drop-a" src="node-a" />
              <connect dst="drop-b" src="node-b" />
              <connect dst="drop-c" src="node-c" />
            </connectionsolution>
          </connectionsolutions>
        </correctanswer>
      </question>
    </questioncollection>
  </questionset>
</exam>
</root>`

// vfsAssets holds the binary content for each asset file in the tarball.
// Keys are VFS paths (without leading slash, as stored in tar), values are file bytes.
var vfsAssets = map[string]string{
	"assets/exhibit1.png":  "fake-exhibit-png-bytes-v1",
	"assets/drag-p1.png":   "fake-drag-background-png",
	"assets/drag-p2-1.png": "fake-drag-candidate-1-png",
	"assets/drag-p2-2.png": "fake-drag-candidate-2-png",
	"assets/drag-p2-3.png": "fake-drag-candidate-3-png",
}

// TestE2E_VisitorVFSPracticeExam validates the full visitor lifecycle for a
// VFS-backed exam: tarball upload → association → exam session → question
// traversal with asset fetching → answer submission → session end → tracking.
//
// It mirrors main.go wiring exactly (associationManager as first ExamSource)
// and exercises the real HTTP stack with JWT + session middleware.
func TestE2E_VisitorVFSPracticeExam(t *testing.T) {
	// --- Wire up the full server (mirrors main.go) -------------------------

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sm := pkgsession.NewOnMemorySessionManager()
	userUploadManager := pkgmodelsuserupload.NewOnMemoryUserUploadManager()
	associationManager := pkgmodelsuserexamdocsfsbasedassociation.NewFsBasedAssociationManager(userUploadManager, sm)
	go associationManager.Run(ctx)
	t.Cleanup(associationManager.Shutdown)

	// ExamRepository with VFS as first source (like main.go) plus no static/dir sources.
	repo := pkgmodelsquestion.NewExamRepository([]pkgmodelsquestion.ExamSource{
		associationManager,
	})

	trackingServer := pkgmodelsexamreport.NewOnMemoryExamTrackingServer(nil, nil)
	examServer := pkgmodelsexamserver.NewOnMemoryExamServer(trackingServer, nil)
	go examServer.Run(ctx)
	t.Cleanup(examServer.Shutdown)

	mux := http.NewServeMux()
	mux.Handle("/api/examdocs", pkgapiexamdocs.NewExamHandler(sm, repo))
	mux.Handle("/api/examsessions", pkgapiexamsessions.NewExamSessionHandler(sm, examServer, repo))
	mux.Handle("/api/examsessions/", pkgapiexamsessions.NewExamSessionHandler(sm, examServer, repo))
	mux.Handle("/api/examtrackings", pkgapiexamtrackings.NewExamTrackingsHandler(sm, trackingServer))

	userUploadsHandler := pkgapiuseruploads.NewUserUploadsHandler(sm, userUploadManager)
	mux.Handle("/api/useruploads", userUploadsHandler)
	mux.Handle("/api/useruploads/", userUploadsHandler)

	examAssociationsHandler := pkgapiexamassociations.NewExamAssociationsHandler(sm, associationManager)
	mux.Handle("/api/examassociations", examAssociationsHandler)
	mux.Handle("/api/examassociations/{association_id}", examAssociationsHandler)

	mux.Handle("/api/dyn-assets/uploads/{upload_id}/{vfs_path...}", associationManager)

	// Visitor login handler
	jwtSecret := []byte("e2e-test-secret-vfs")
	keyProvider := pkgauth.NewStaticSecretProvider(jwtSecret)
	tokenIssuer := pkgauth.NewStaticKeyJWTIssuer(keyProvider, "e2e-issuer")
	tickIssuer := pkgauth.NewSharedTickingTicketGenerator(5 * time.Millisecond)
	tickIssuer.Run(ctx)
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

	client := &http.Client{Timeout: 10 * time.Second}

	// --- Login as visitor A ------------------------------------------------

	t.Log("step 1: login as visitor A")
	jwtCookieA := loginAsVisitor(t, ts.URL+"/api/login/visitor")
	if jwtCookieA == "" {
		t.Fatal("visitor A login did not set jwt cookie")
	}
	t.Logf("visitor A jwt cookie obtained (truncated %q)", truncate([]byte(jwtCookieA), 30))

	cookieGetA := func(path string) []byte {
		return cookieReq(t, client, ts.URL, http.MethodGet, path, "", jwtCookieA)
	}
	cookiePostA := func(path, body string) []byte {
		return cookieReq(t, client, ts.URL, http.MethodPost, path, body, jwtCookieA)
	}
	cookieDeleteA := func(path string) {
		cookieReq(t, client, ts.URL, http.MethodDelete, path, "", jwtCookieA)
	}

	// --- Build tarball with exam.xml + assets ------------------------------

	t.Log("step 2: building VFS tarball")
	tarBytes, assetHashes, err := buildVFSTar(vfsPracticeExamXML, vfsAssets)
	if err != nil {
		t.Fatalf("build VFS tar: %v", err)
	}
	t.Logf("  tarball built: %d bytes, %d assets", len(tarBytes), len(vfsAssets))

	// --- Upload tarball via /api/useruploads -------------------------------

	t.Log("step 3: uploading tarball via /api/useruploads")
	uploadID := uploadTarball(t, client, ts.URL, jwtCookieA, tarBytes, "vfs-practice.tar")
	t.Logf("  uploaded as upload_id=%s", uploadID)

	// Verify upload appears in listing
	uploadsBody := cookieGetA("/api/useruploads")
	t.Logf("  uploads listing: %s", uploadsBody)
	var uploadsResp struct {
		Uploads []struct {
			UploadId string `json:"upload_id"`
			Filename string `json:"filename"`
		} `json:"uploads"`
	}
	if err := json.Unmarshal(uploadsBody, &uploadsResp); err != nil {
		t.Fatalf("decode uploads: %v", err)
	}
	if len(uploadsResp.Uploads) != 1 || uploadsResp.Uploads[0].UploadId != uploadID {
		t.Fatalf("expected 1 upload with id %s, got %+v", uploadID, uploadsResp.Uploads)
	}

	// --- Create association via /api/examassociations ----------------------

	t.Log("step 4: creating association via /api/examassociations")
	createAssociation(t, client, ts.URL, jwtCookieA, uploadID)
	assocID := soleAssociationId(t, client, ts.URL, jwtCookieA)
	t.Logf("  association created: id=%s upload_id=%s", assocID, uploadID)

	// The VFS exam id is prefixed: /uploads/{uploadId}/vfs-practice-1
	vfsExamID := "/uploads/" + uploadID + "/vfs-practice-1"
	t.Logf("  expected VFS exam id: %q", vfsExamID)

	// The exam list endpoint must now surface the associated exam to A.
	docsBody := cookieGetA("/api/examdocs")
	if !strings.Contains(string(docsBody), vfsExamID) {
		t.Fatalf("examdocs after association does not contain %q: %s", vfsExamID, docsBody)
	}
	t.Log("  ✓ /api/examdocs lists the associated VFS exam for A")

	// --- Verify exam session creation fails with wrong id ------------------

	t.Log("step 5: verify wrong exam_id is rejected")
	wrongBody := cookieReqWithStatus(t, client, ts.URL, http.MethodPost, "/api/examsessions", `{"exam_id":"does-not-exist"}`, jwtCookieA, http.StatusNotFound)
	t.Logf("  wrong exam_id response (expected 404): %s", truncate(wrongBody, 200))

	// --- Start exam session with VFS exam ----------------------------------

	t.Logf("step 6: starting exam session for VFS exam %q", vfsExamID)
	sessionID := startExamSession(t, cookiePostA, vfsExamID)
	t.Logf("  exam session id: %s", sessionID)

	// --- Walk through all questions, fetching assets and submitting answers --

	t.Log("step 7: walking through questions with asset verification")
	var cursorID *string
	questionNo := 0
	// Track which assets we have verified
	verifiedAssets := make(map[string]bool)

	for {
		questionNo++
		questionsURL := fmt.Sprintf("/api/examsessions/%s/questions", sessionID)
		if cursorID != nil {
			questionsURL += "?cursor_id=" + *cursorID
		}

		t.Logf("  7.%d: GET %s", questionNo, questionsURL)
		qBody := cookieGetA(questionsURL)
		t.Logf("    response: %s", truncate(qBody, 400))

		var qResp struct {
			CursorID *string          `json:"cursor_id"`
			Question *json.RawMessage `json:"question"`
		}
		if err := json.Unmarshal(qBody, &qResp); err != nil {
			t.Fatalf("decode question response: %v", err)
		}
		if qResp.Question == nil {
			t.Logf("    no more questions (question is null)")
			break
		}

		// Parse question to extract metadata and asset URLs
		var q struct {
			Id          string `json:"id"`
			Type        string `json:"type"`
			Description struct {
				Text string `json:"text"`
			} `json:"description"`
			Exhibits []struct {
				Image struct {
					Src string `json:"src"`
				} `json:"image"`
			} `json:"exhibits"`
			ImgDragAndDrop *struct {
				ImgCandidates []struct {
					NodeId     string `json:"nodeId"`
					ImgDataSrc string `json:"imgDataSrc"`
				} `json:"imgCandidates"`
				ImgDropsArea struct {
					ImgBackgroundUrl string `json:"imgBackgroundUrl"`
					Width            int    `json:"width"`
					Height           int    `json:"height"`
				} `json:"imgDropsArea"`
			} `json:"imgDragAndDrop"`
		}
		if err := json.Unmarshal(*qResp.Question, &q); err != nil {
			t.Fatalf("decode question %d: %v", questionNo, err)
		}
		t.Logf("    question id=%s type=%s", q.Id, q.Type)

		// Collect asset URLs from this question
		var assetURLs []string
		for _, ex := range q.Exhibits {
			if ex.Image.Src != "" {
				assetURLs = append(assetURLs, ex.Image.Src)
			}
		}
		if q.ImgDragAndDrop != nil {
			for _, c := range q.ImgDragAndDrop.ImgCandidates {
				if c.ImgDataSrc != "" {
					assetURLs = append(assetURLs, c.ImgDataSrc)
				}
			}
			if q.ImgDragAndDrop.ImgDropsArea.ImgBackgroundUrl != "" {
				assetURLs = append(assetURLs, q.ImgDragAndDrop.ImgDropsArea.ImgBackgroundUrl)
			}
		}

		// Verify each asset URL is rewritten and fetchable via dyn-assets endpoint
		for _, assetURL := range assetURLs {
			t.Logf("    verifying asset URL: %s", assetURL)
			// Must be rewritten to /api/dyn-assets/uploads/{uploadId}/assets/...
			expectedPrefix := "/api/dyn-assets/uploads/" + uploadID + "/assets/"
			if !strings.HasPrefix(assetURL, expectedPrefix) {
				t.Fatalf("asset URL %q should have prefix %q", assetURL, expectedPrefix)
			}
			// Fetch the asset via the dyn-assets endpoint
			assetBody := cookieGetA(assetURL)
			// The VFS path is the suffix after /api/dyn-assets/uploads/{uploadId}/
			vfsPath := strings.TrimPrefix(assetURL, "/api/dyn-assets/uploads/"+uploadID+"/")
			expectedContent, ok := vfsAssets[vfsPath]
			if !ok {
				t.Fatalf("unexpected asset path %q (url %q)", vfsPath, assetURL)
			}
			if string(assetBody) != expectedContent {
				t.Fatalf("asset %q content mismatch: got %q want %q", assetURL, string(assetBody), expectedContent)
			}
			// Also verify SHA256
			sum := sha256.Sum256(assetBody)
			gotHex := hex.EncodeToString(sum[:])
			wantHex := assetHashes[vfsPath]
			if gotHex != wantHex {
				t.Fatalf("asset %q sha256 mismatch: got %s want %s", assetURL, gotHex, wantHex)
			}
			t.Logf("      ✓ asset %s verified (sha256=%s)", vfsPath, gotHex)
			verifiedAssets[vfsPath] = true

			// Also verify HEAD works
			status := headStatus(t, client, ts.URL, jwtCookieA, assetURL)
			if status != http.StatusOK {
				t.Fatalf("HEAD %s: status %d want 200", assetURL, status)
			}
		}

		// Build and submit answer for this question
		answer := buildVFSAnswer(q.Id, q.Type)
		submitBody, _ := json.Marshal(map[string]any{"answers": []map[string]any{answer}})
		t.Logf("    submitting answer for %s: %s", q.Id, string(submitBody))
		submitResp := cookiePostA(fmt.Sprintf("/api/examsessions/%s/answer", sessionID), string(submitBody))
		t.Logf("    submit response: %s", truncate(submitResp, 300))

		// Verify submit response contains assessment
		var submitResult struct {
			Assessment *struct {
				OverallResult string `json:"overallResult"`
				ScoreResult   *struct {
					EarnedScore float64 `json:"earnedScore"`
					TotalScore  float64 `json:"totalScore"`
				} `json:"scoreResult"`
			} `json:"assessment"`
		}
		if err := json.Unmarshal(submitResp, &submitResult); err != nil {
			t.Fatalf("decode submit response: %v", err)
		}
		if submitResult.Assessment == nil {
			t.Fatalf("submit response missing assessment: %s", submitResp)
		}

		cursorID = qResp.CursorID
		if cursorID == nil {
			t.Logf("    last question reached (no next cursor)")
			break
		}
		if questionNo > 10 {
			t.Fatal("too many questions, possible infinite loop")
		}
	}

	if questionNo < 4 {
		t.Fatalf("expected at least 4 questions, got %d", questionNo)
	}
	t.Logf("  answered %d questions", questionNo)

	// Verify all expected assets were seen and verified
	for path := range vfsAssets {
		if !verifiedAssets[path] {
			t.Fatalf("asset %q was never verified via question traversal", path)
		}
	}
	t.Logf("  all %d assets verified via question asset URLs", len(vfsAssets))

	// Also verify direct asset fetching for all files (including those not referenced)
	t.Log("step 8: direct asset fetching for all VFS files")
	for path, expectedContent := range vfsAssets {
		url := fmt.Sprintf("/api/dyn-assets/uploads/%s/%s", uploadID, path)
		body := cookieGetA(url)
		if string(body) != expectedContent {
			t.Fatalf("direct fetch %s: got %q want %q", url, string(body), expectedContent)
		}
		t.Logf("    ✓ direct fetch %s", path)
	}

	// --- End session and verify tracking -----------------------------------

	t.Logf("step 9: ending exam session %s", sessionID)
	cookieDeleteA(fmt.Sprintf("/api/examsessions/%s", sessionID))
	t.Log("  session ended")

	t.Log("step 10: listing exam trackings")
	trackingsBody := cookieGetA("/api/examtrackings")
	t.Logf("  trackings: %s", trackingsBody)
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
		t.Fatalf("expected at least 1 report, got 0")
	}
	found := false
	for _, r := range trackings.ExamReports {
		t.Logf("    report: examId=%s code=%s title=%q sessionId=%s", r.ExamId, r.ExamCode, r.Title, r.ExamSessionId)
		if r.ExamSessionId == sessionID {
			found = true
			if r.ExamId != vfsExamID {
				t.Errorf("tracking examId = %q want %q", r.ExamId, vfsExamID)
			}
			if r.ExamCode != "VFS-01" {
				t.Errorf("tracking examCode = %q want VFS-01", r.ExamCode)
			}
		}
	}
	if !found {
		t.Fatalf("no tracking report for session %s", sessionID)
	}

	// --- Isolation: visitor B cannot access visitor A's VFS exam or assets --

	t.Log("step 11: isolation — login as visitor B")
	jwtCookieB := loginAsVisitor(t, ts.URL+"/api/login/visitor")
	if jwtCookieB == "" {
		t.Fatal("visitor B login failed")
	}
	// B cannot start session with A's VFS exam
	t.Log("  B trying to start session with A's VFS exam (should 404)")
	badBody := cookieReqWithStatus(t, client, ts.URL, http.MethodPost, "/api/examsessions", fmt.Sprintf(`{"exam_id":%q}`, vfsExamID), jwtCookieB, http.StatusNotFound)
	t.Logf("    B start session response (expected 404): %s", truncate(badBody, 200))

	// B cannot fetch A's assets
	t.Log("  B trying to fetch A's assets (should 404)")
	for path := range vfsAssets {
		url := fmt.Sprintf("/api/dyn-assets/uploads/%s/%s", uploadID, path)
		status := headStatus(t, client, ts.URL, jwtCookieB, url)
		if status != http.StatusNotFound {
			t.Fatalf("B HEAD %s: status %d want 404", url, status)
		}
	}
	t.Log("    ✓ B correctly cannot access A's assets")

	// B has no associations
	assocsB := listAssociations(t, client, ts.URL, jwtCookieB)
	if len(assocsB) != 0 {
		t.Fatalf("B should have 0 associations, got %d", len(assocsB))
	}

	// B's exam list must not contain A's VFS exam.
	docsB := cookieReq(t, client, ts.URL, http.MethodGet, "/api/examdocs", "", jwtCookieB)
	if strings.Contains(string(docsB), vfsExamID) {
		t.Fatalf("B's examdocs leaked A's VFS exam %q: %s", vfsExamID, docsB)
	}
	t.Log("    ✓ B's /api/examdocs does not list A's VFS exam")

	// --- De-association: delete and verify exam/assets gone for A -----------

	t.Log("step 12: de-association — delete A's association")
	deleteAssociation(t, client, ts.URL, jwtCookieA, assocID)

	// Listing should be empty
	assocsA := listAssociations(t, client, ts.URL, jwtCookieA)
	if len(assocsA) != 0 {
		t.Fatalf("after delete, A should have 0 associations, got %d", len(assocsA))
	}

	// The exam list must no longer surface the de-associated exam.
	docsAfterDelete := cookieGetA("/api/examdocs")
	if strings.Contains(string(docsAfterDelete), vfsExamID) {
		t.Fatalf("examdocs after de-association still contains %q: %s", vfsExamID, docsAfterDelete)
	}
	t.Log("  ✓ /api/examdocs no longer lists the exam after de-association")

	// Starting new session with same VFS exam should now 404
	t.Log("  A trying to start new session after de-association (should 404)")
	afterDeleteBody := cookieReqWithStatus(t, client, ts.URL, http.MethodPost, "/api/examsessions", fmt.Sprintf(`{"exam_id":%q}`, vfsExamID), jwtCookieA, http.StatusNotFound)
	t.Logf("    response (expected 404): %s", truncate(afterDeleteBody, 200))

	// Assets should 404
	for path := range vfsAssets {
		url := fmt.Sprintf("/api/dyn-assets/uploads/%s/%s", uploadID, path)
		status := headStatus(t, client, ts.URL, jwtCookieA, url)
		if status != http.StatusNotFound {
			t.Fatalf("after delete, HEAD %s: status %d want 404", url, status)
		}
	}
	t.Log("    ✓ assets correctly 404 after de-association")

	// --- Re-association: same upload, new association, exam works again -----

	t.Log("step 13: re-association — same upload, new association")
	createAssociation(t, client, ts.URL, jwtCookieA, uploadID)
	newAssocID := soleAssociationId(t, client, ts.URL, jwtCookieA)
	t.Logf("  new association id=%s", newAssocID)

	// The exam list must surface the exam again.
	docsReassoc := cookieGetA("/api/examdocs")
	if !strings.Contains(string(docsReassoc), vfsExamID) {
		t.Fatalf("examdocs after re-association does not contain %q: %s", vfsExamID, docsReassoc)
	}
	t.Log("  ✓ /api/examdocs lists the exam again after re-association")

	// Assets should be reachable again
	for path, expectedContent := range vfsAssets {
		url := fmt.Sprintf("/api/dyn-assets/uploads/%s/%s", uploadID, path)
		body := cookieGetA(url)
		if string(body) != expectedContent {
			t.Fatalf("after re-associate, fetch %s: got %q want %q", url, string(body), expectedContent)
		}
	}
	t.Log("    ✓ assets reachable after re-association")

	// New exam session should work
	t.Logf("  starting new exam session after re-association")
	newSessionID := startExamSession(t, cookiePostA, vfsExamID)
	t.Logf("    new session id: %s", newSessionID)
	// Fetch first question to confirm
	qBody := cookieGetA(fmt.Sprintf("/api/examsessions/%s/questions", newSessionID))
	var qResp struct {
		Question *json.RawMessage `json:"question"`
	}
	if err := json.Unmarshal(qBody, &qResp); err != nil {
		t.Fatalf("decode question after re-associate: %v", err)
	}
	if qResp.Question == nil {
		t.Fatal("expected question after re-associate, got null")
	}
	t.Log("    ✓ new session serves questions after re-association")
	cookieDeleteA(fmt.Sprintf("/api/examsessions/%s", newSessionID))

	t.Log("e2e visitor VFS practice exam completed successfully")
}

// buildVFSTar creates a tarball with exam.xml at root and asset files.
// It returns the tar bytes and a map of VFS path → hex SHA256 for verification.
func buildVFSTar(examXML string, assets map[string]string) ([]byte, map[string]string, error) {
	mem := afero.NewMemMapFs()
	hashes := make(map[string]string, len(assets))

	// Write exam.xml
	if err := afero.WriteFile(mem, "exam.xml", []byte(examXML), 0o644); err != nil {
		return nil, nil, fmt.Errorf("write exam.xml: %w", err)
	}

	// Write each asset
	for path, content := range assets {
		if err := afero.WriteFile(mem, path, []byte(content), 0o644); err != nil {
			return nil, nil, fmt.Errorf("write %q: %w", path, err)
		}
		sum := sha256.Sum256([]byte(content))
		hashes[path] = hex.EncodeToString(sum[:])
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.AddFS(afero.NewIOFS(mem)); err != nil {
		return nil, nil, fmt.Errorf("tar AddFS: %w", err)
	}
	if err := tw.Close(); err != nil {
		return nil, nil, fmt.Errorf("tar close: %w", err)
	}
	return buf.Bytes(), hashes, nil
}

// uploadTarballVFS is a helper that POSTs a tarball to /api/useruploads.
// (Renamed to avoid collision with e2e_dynassets_test.go's uploadTarball)
func uploadTarballVFS(t *testing.T, client *http.Client, baseURL, jwtCookie string, tarBytes []byte, filename string) string {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fh := make(textproto.MIMEHeader)
	fh.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, filename))
	fh.Set("Content-Type", "application/x-tar")
	part, err := mw.CreatePart(fh)
	if err != nil {
		t.Fatalf("create multipart part: %v", err)
	}
	if _, err := part.Write(tarBytes); err != nil {
		t.Fatalf("write tar bytes: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+"/api/useruploads", &body)
	if err != nil {
		t.Fatalf("new upload request: %v", err)
	}
	req.AddCookie(&http.Cookie{Name: "jwt", Value: jwtCookie})
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("upload request: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload %s: status = %d want %d (body %s)", filename, resp.StatusCode, http.StatusCreated, respBody)
	}
	var summary struct {
		UploadId string `json:"upload_id"`
	}
	if err := json.Unmarshal(respBody, &summary); err != nil {
		t.Fatalf("decode upload response %q: %v", respBody, err)
	}
	if summary.UploadId == "" {
		t.Fatalf("empty upload_id in response: %s", respBody)
	}
	return summary.UploadId
}

// buildVFSAnswer constructs an answer for the VFS practice exam questions.
func buildVFSAnswer(questionID, questionType string) map[string]any {
	switch questionID {
	case "q1":
		// Correct: Paris (id 2)
		return map[string]any{
			"questionId":   questionID,
			"questionType": questionType,
			"options":      []map[string]any{{"id": "2"}},
		}
	case "q2":
		// Correct: Go (1) + Python (3)
		return map[string]any{
			"questionId":   questionID,
			"questionType": questionType,
			"options":      []map[string]any{{"id": "1"}, {"id": "3"}},
		}
	case "q3":
		// Correct: DHCP (2)
		return map[string]any{
			"questionId":   questionID,
			"questionType": questionType,
			"options":      []map[string]any{{"id": "2"}},
		}
	case "q4":
		// Correct drag-and-drop
		return map[string]any{
			"questionId":   questionID,
			"questionType": questionType,
			"connections": []map[string]any{
				{"src": "node-a", "dst": "drop-a"},
				{"src": "node-b", "dst": "drop-b"},
				{"src": "node-c", "dst": "drop-c"},
			},
		}
	default:
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
}

// cookieReqWithStatus performs a cookie-authenticated request and asserts the
// response status equals wantStatus, returning the body.
func cookieReqWithStatus(t *testing.T, client *http.Client, baseURL, method, path, body, jwtCookie string, wantStatus int) []byte {
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
	if resp.StatusCode != wantStatus {
		t.Fatalf("%s %s: status = %d want %d (body %s)", method, path, resp.StatusCode, wantStatus, respBody)
	}
	return respBody
}

// Ensure the VFS-specific upload helper is referenced (avoid unused lint).
var _ = uploadTarballVFS
