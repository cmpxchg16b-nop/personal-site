package fsbasedassociation

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	pkgmodelsuserupload "personal-site/pkg/models/userupload"
	pkgsession "personal-site/pkg/session"
	pkgutils "personal-site/pkg/utils"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func newTestManager(t *testing.T) (*FsBasedAssociationManager, *pkgmodelsuserupload.OnMemoryUserUploadManager, *pkgsession.OnMemorySessionManager, context.Context, context.CancelFunc) {
	t.Helper()
	sm := pkgsession.NewOnMemorySessionManager()
	um := pkgmodelsuserupload.NewOnMemoryUserUploadManager()
	mgr := NewFsBasedAssociationManager(um, sm)
	ctx, cancel := context.WithCancel(context.Background())
	go mgr.Run(ctx)
	t.Cleanup(func() {
		cancel()
		mgr.Shutdown()
	})
	return mgr, um, sm, ctx, cancel
}

func buildTar(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, data := range files {
		hdr := &tar.Header{
			Name: strings.TrimPrefix(name, "/"),
			Mode: 0644,
			Size: int64(len(data)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("WriteHeader %q: %v", name, err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatalf("Write %q: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("Close tar: %v", err)
	}
	return buf.Bytes()
}

func minimalExamXML(id, assetSrc string) []byte {
	// assetSrc is placed in an exhibit image src; empty means no exhibit.
	exhibit := ""
	if assetSrc != "" {
		exhibit = fmt.Sprintf(`<exhibits><exhibit><image src="%s"/></exhibit></exhibits>`, assetSrc)
	}
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<root>
<exam id="%s" shortname="X" code="C">
  <title>t</title><description>d</description>
  <examcategory>certification-exam</examcategory>
  <questionset>
    <questioncollection>
      <question id="q1" type="single-choice">
        <description>q</description>
        %s
        <options><option id="1">a</option></options>
        <correctanswer><options><option id="1">a</option></options></correctanswer>
      </question>
    </questioncollection>
  </questionset>
</exam>
</root>`, id, exhibit))
}

func uploadTarForUser(t *testing.T, um *pkgmodelsuserupload.OnMemoryUserUploadManager, userId, filename string, tarBytes []byte) string {
	t.Helper()
	summary, err := um.CreateNewUserUpload(context.Background(), bytes.NewReader(tarBytes), userId, pkgmodelsuserupload.FileMetadata{Filename: filename})
	if err != nil {
		t.Fatalf("CreateNewUserUpload: %v", err)
	}
	return summary.UploadId
}

func mustAddAssociation(t *testing.T, mgr *FsBasedAssociationManager, userId, uploadId string) {
	t.Helper()
	if err := mgr.AddAssociation(context.Background(), userId, uploadId); err != nil {
		t.Fatalf("AddAssociation: %v", err)
	}
}

func soleAssociation(t *testing.T, mgr *FsBasedAssociationManager, userId string) string {
	t.Helper()
	assocs, err := mgr.GetAssociationsByUserId(context.Background(), userId)
	if err != nil {
		t.Fatalf("GetAssociationsByUserId: %v", err)
	}
	if len(assocs) != 1 {
		t.Fatalf("expected 1 association, got %d", len(assocs))
	}
	return assocs[0].Id
}

// ---------------------------------------------------------------------------
// GetAssociationsByUserId / AddAssociation / DeleteAssociation
// ---------------------------------------------------------------------------

func TestFsBasedAssociationManager_GetAssociationsByUserId_Empty(t *testing.T) {
	mgr, _, _, _, _ := newTestManager(t)
	assocs, err := mgr.GetAssociationsByUserId(context.Background(), "no-such-user")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(assocs) != 0 {
		t.Fatalf("expected empty, got %d", len(assocs))
	}
}

func TestFsBasedAssociationManager_AddAndList(t *testing.T) {
	mgr, um, _, _, _ := newTestManager(t)
	tarBytes := buildTar(t, map[string][]byte{
		"exam.xml": minimalExamXML("exam-1", ""),
	})
	uploadId := uploadTarForUser(t, um, "alice", "bundle.tar", tarBytes)
	mustAddAssociation(t, mgr, "alice", uploadId)

	assocs, err := mgr.GetAssociationsByUserId(context.Background(), "alice")
	if err != nil {
		t.Fatalf("GetAssociationsByUserId: %v", err)
	}
	if len(assocs) != 1 {
		t.Fatalf("expected 1, got %d", len(assocs))
	}
	if assocs[0].UserId != "alice" || assocs[0].UploadId != uploadId {
		t.Errorf("association mismatch: %+v", assocs[0])
	}
	if assocs[0].Id == "" {
		t.Error("association Id should not be empty")
	}
	// other user sees nothing
	other, _ := mgr.GetAssociationsByUserId(context.Background(), "bob")
	if len(other) != 0 {
		t.Fatalf("bob should see 0, got %d", len(other))
	}
}

func TestFsBasedAssociationManager_AddAssociation_OnlyTar(t *testing.T) {
	mgr, um, _, _, _ := newTestManager(t)
	// upload a non-tar file
	tarBytes := []byte("not a tar")
	summary, err := um.CreateNewUserUpload(context.Background(), bytes.NewReader(tarBytes), "alice", pkgmodelsuserupload.FileMetadata{Filename: "file.zip"})
	if err != nil {
		t.Fatalf("CreateNewUserUpload: %v", err)
	}
	err = mgr.AddAssociation(context.Background(), "alice", summary.UploadId)
	if err == nil || !strings.Contains(err.Error(), "only .tar") {
		t.Fatalf("expected unsupported error, got %v", err)
	}
}

func TestFsBasedAssociationManager_AddAssociation_UploadNotFound(t *testing.T) {
	mgr, _, _, _, _ := newTestManager(t)
	err := mgr.AddAssociation(context.Background(), "alice", "no-such-upload")
	if err == nil {
		t.Fatal("expected error for missing upload")
	}
}

func TestFsBasedAssociationManager_AddAssociation_WrongUser(t *testing.T) {
	mgr, um, _, _, _ := newTestManager(t)
	tarBytes := buildTar(t, map[string][]byte{"exam.xml": minimalExamXML("e1", "")})
	uploadId := uploadTarForUser(t, um, "alice", "a.tar", tarBytes)
	// bob tries to associate alice's upload
	err := mgr.AddAssociation(context.Background(), "bob", uploadId)
	if err == nil {
		t.Fatal("expected error when associating another user's upload")
	}
}

func TestFsBasedAssociationManager_DeleteAssociation(t *testing.T) {
	mgr, um, _, _, _ := newTestManager(t)
	tarBytes := buildTar(t, map[string][]byte{"exam.xml": minimalExamXML("e1", "")})
	uploadId := uploadTarForUser(t, um, "alice", "a.tar", tarBytes)
	mustAddAssociation(t, mgr, "alice", uploadId)
	assocId := soleAssociation(t, mgr, "alice")

	if err := mgr.DeleteAssociation(context.Background(), "alice", assocId); err != nil {
		t.Fatalf("DeleteAssociation: %v", err)
	}
	assocs, _ := mgr.GetAssociationsByUserId(context.Background(), "alice")
	if len(assocs) != 0 {
		t.Fatalf("expected 0 after delete, got %d", len(assocs))
	}
	// deleting again should fail
	if err := mgr.DeleteAssociation(context.Background(), "alice", assocId); err == nil {
		t.Fatal("expected error on second delete")
	}
}

func TestFsBasedAssociationManager_DeleteAssociation_WrongUser(t *testing.T) {
	mgr, um, _, _, _ := newTestManager(t)
	tarBytes := buildTar(t, map[string][]byte{"exam.xml": minimalExamXML("e1", "")})
	uploadId := uploadTarForUser(t, um, "alice", "a.tar", tarBytes)
	mustAddAssociation(t, mgr, "alice", uploadId)
	assocId := soleAssociation(t, mgr, "alice")

	if err := mgr.DeleteAssociation(context.Background(), "bob", assocId); err == nil {
		t.Fatal("expected error when deleting another user's association")
	}
}

// ---------------------------------------------------------------------------
// DereferenceAssociation — ID prepending + asset rewriting
// ---------------------------------------------------------------------------

func TestFsBasedAssociationManager_DereferenceAssociation_IDAndAssets(t *testing.T) {
	mgr, um, _, _, _ := newTestManager(t)
	examXML := minimalExamXML("exam-1", "/assets/img.png")
	tarBytes := buildTar(t, map[string][]byte{
		"exam.xml":      examXML,
		"assets/img.png": []byte("fake png"),
	})
	uploadId := uploadTarForUser(t, um, "alice", "bundle.tar", tarBytes)
	mustAddAssociation(t, mgr, "alice", uploadId)
	assocId := soleAssociation(t, mgr, "alice")

	exam, err := mgr.DereferenceAssociation(context.Background(), "alice", assocId)
	if err != nil {
		t.Fatalf("DereferenceAssociation: %v", err)
	}
	// ID must be prepended with /uploads/{upload_id}/
	wantId := "/uploads/" + uploadId + "/exam-1"
	if exam.Id != wantId {
		t.Errorf("Id = %q, want %q", exam.Id, wantId)
	}
	// Asset URL must be rewritten to /api/dyn-assets/uploads/{upload_id}/assets/...
	wantSrc := "/api/dyn-assets/uploads/" + uploadId + "/assets/img.png"
	gotSrc := exam.QuestionSet.QuestionCollections[0].Questions[0].Exhibits[0].Image.Src
	if gotSrc != wantSrc {
		t.Errorf("Exhibit Src = %q, want %q", gotSrc, wantSrc)
	}
}

func TestFsBasedAssociationManager_DereferenceAssociation_NotFound(t *testing.T) {
	mgr, _, _, _, _ := newTestManager(t)
	_, err := mgr.DereferenceAssociation(context.Background(), "alice", "no-such-id")
	if err == nil {
		t.Fatal("expected error for missing association")
	}
}

func TestFsBasedAssociationManager_DereferenceAssociation_WrongUser(t *testing.T) {
	mgr, um, _, _, _ := newTestManager(t)
	tarBytes := buildTar(t, map[string][]byte{"exam.xml": minimalExamXML("e1", "")})
	uploadId := uploadTarForUser(t, um, "alice", "a.tar", tarBytes)
	mustAddAssociation(t, mgr, "alice", uploadId)
	assocId := soleAssociation(t, mgr, "alice")

	_, err := mgr.DereferenceAssociation(context.Background(), "bob", assocId)
	if err == nil {
		t.Fatal("expected error when dereferencing another user's association")
	}
}

func TestFsBasedAssociationManager_DereferenceAssociation_DeepCopy(t *testing.T) {
	mgr, um, _, _, _ := newTestManager(t)
	tarBytes := buildTar(t, map[string][]byte{"exam.xml": minimalExamXML("orig-id", "")})
	uploadId := uploadTarForUser(t, um, "alice", "a.tar", tarBytes)
	mustAddAssociation(t, mgr, "alice", uploadId)
	assocId := soleAssociation(t, mgr, "alice")

	exam1, err := mgr.DereferenceAssociation(context.Background(), "alice", assocId)
	if err != nil {
		t.Fatalf("first DereferenceAssociation: %v", err)
	}
	exam1.Id = "mutated"
	exam2, err := mgr.DereferenceAssociation(context.Background(), "alice", assocId)
	if err != nil {
		t.Fatalf("second DereferenceAssociation: %v", err)
	}
	if exam2.Id == "mutated" {
		t.Error("DereferenceAssociation should return a deep copy each time")
	}
	if exam2.Id != "/uploads/"+uploadId+"/orig-id" {
		t.Errorf("second exam Id = %q, want %q", exam2.Id, "/uploads/"+uploadId+"/orig-id")
	}
}

// ---------------------------------------------------------------------------
// LoadFrom (data URI) + GetByUserId / Get
// ---------------------------------------------------------------------------

func TestFsBasedAssociationManager_LoadFrom_Success(t *testing.T) {
	mgr, um, _, _, _ := newTestManager(t)
	tarBytes := buildTar(t, map[string][]byte{"exam.xml": minimalExamXML("exam-42", "")})
	uploadId := uploadTarForUser(t, um, "alice", "a.tar", tarBytes)
	mustAddAssociation(t, mgr, "alice", uploadId)
	assocId := soleAssociation(t, mgr, "alice")

	// GetByUserId should return a data URI that LoadFrom can decode
	entries, err := mgr.GetByUserId(context.Background(), "alice")
	if err != nil {
		t.Fatalf("GetByUserId: %v", err)
	}
	if len(entries) != 1 || len(entries[0].URLs) != 1 {
		t.Fatalf("expected 1 entry with 1 URL, got %+v", entries)
	}
	url := entries[0].URLs[0]
	if !strings.HasPrefix(url, "data:") {
		t.Fatalf("URL should be data URI, got %q", url)
	}
	exam, err := mgr.LoadFrom(context.Background(), url)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	wantId := "/uploads/" + uploadId + "/exam-42"
	if exam.Id != wantId {
		t.Errorf("Id = %q, want %q", exam.Id, wantId)
	}
	// also test direct LoadFrom via manually encoded URL
	raw, _ := json.Marshal(map[string]string{"userId": "alice", "associationId": assocId})
	manualURL := "data:application/json;base64," + base64.StdEncoding.EncodeToString(raw)
	exam2, err := mgr.LoadFrom(context.Background(), manualURL)
	if err != nil {
		t.Fatalf("LoadFrom manual: %v", err)
	}
	if exam2.Id != wantId {
		t.Errorf("manual LoadFrom Id = %q, want %q", exam2.Id, wantId)
	}
}

func TestFsBasedAssociationManager_LoadFrom_InvalidDataURI(t *testing.T) {
	mgr, _, _, _, _ := newTestManager(t)
	cases := []string{
		"not-a-data-uri",
		"data:application/json;base64,!!!not-base64!!!",
		"data:application/json;base64," + base64.StdEncoding.EncodeToString([]byte(`{"userId":""`)),
		"data:application/json;base64," + base64.StdEncoding.EncodeToString([]byte(`{"userId":"a","associationId":""}`)),
		"data:application/json;base64," + base64.StdEncoding.EncodeToString([]byte(`not json`)),
	}
	for _, url := range cases {
		if _, err := mgr.LoadFrom(context.Background(), url); err == nil {
			t.Errorf("expected error for url %q", url)
		}
	}
}

func TestFsBasedAssociationManager_GetByUserId_Empty(t *testing.T) {
	mgr, _, _, _, _ := newTestManager(t)
	entries, err := mgr.GetByUserId(context.Background(), "nobody")
	if err != nil {
		t.Fatalf("GetByUserId: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected 0, got %d", len(entries))
	}
}

func TestFsBasedAssociationManager_Get_ReturnsNil(t *testing.T) {
	mgr, _, _, _, _ := newTestManager(t)
	if got := mgr.Get(); got != nil {
		t.Errorf("Get() = %v, want nil", got)
	}
}

func TestFsBasedAssociationManager_GetByUserId_Multiple(t *testing.T) {
	mgr, um, _, _, _ := newTestManager(t)
	for i := 0; i < 2; i++ {
		tarBytes := buildTar(t, map[string][]byte{"exam.xml": minimalExamXML(fmt.Sprintf("e%d", i), "")})
		uploadId := uploadTarForUser(t, um, "alice", fmt.Sprintf("a%d.tar", i), tarBytes)
		mustAddAssociation(t, mgr, "alice", uploadId)
	}
	entries, err := mgr.GetByUserId(context.Background(), "alice")
	if err != nil {
		t.Fatalf("GetByUserId: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2, got %d", len(entries))
	}
	for _, e := range entries {
		if e.Loader != mgr {
			t.Error("Loader should be the manager itself")
		}
		if len(e.URLs) != 1 {
			t.Errorf("expected 1 URL per entry, got %d", len(e.URLs))
		}
	}
}

// ---------------------------------------------------------------------------
// ServeHTTP
// ---------------------------------------------------------------------------

func TestFsBasedAssociationManager_ServeHTTP_MethodNotAllowed(t *testing.T) {
	mgr, _, sm, _, _ := newTestManager(t)
	req := httptest.NewRequest(http.MethodPost, "/api/dyn-assets/uploads/0/file.txt", nil)
	// need a session even for method check? handler checks method first
	w := httptest.NewRecorder()
	mgr.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
	_ = sm // avoid unused
}

func TestFsBasedAssociationManager_ServeHTTP_SessionNotFound(t *testing.T) {
	mgr, _, _, _, _ := newTestManager(t)
	req := httptest.NewRequest(http.MethodGet, "/api/dyn-assets/uploads/0/file.txt", nil)
	w := httptest.NewRecorder()
	mgr.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", w.Code, http.StatusInternalServerError)
	}
}

func TestFsBasedAssociationManager_ServeHTTP_NotFound_WrongUpload(t *testing.T) {
	mgr, um, _, _, _ := newTestManager(t)
	tarBytes := buildTar(t, map[string][]byte{"exam.xml": minimalExamXML("e1", "")})
	uploadId := uploadTarForUser(t, um, "alice", "a.tar", tarBytes)
	mustAddAssociation(t, mgr, "alice", uploadId)

	// request with different uploadId
	req := httptest.NewRequest(http.MethodGet, "/api/dyn-assets/uploads/999/file.txt", nil)
	req.SetPathValue("upload_id", "999")
	req = req.WithContext(context.WithValue(context.Background(), pkgutils.CtxKeySessionObject, sessWithSubject("alice")))
	w := httptest.NewRecorder()
	mgr.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestFsBasedAssociationManager_ServeHTTP_Success(t *testing.T) {
	mgr, um, _, _, _ := newTestManager(t)
	tarBytes := buildTar(t, map[string][]byte{
		"exam.xml":   minimalExamXML("e1", ""),
		"hello.txt":  []byte("hello world"),
		"dir/a.txt":  []byte("nested"),
	})
	uploadId := uploadTarForUser(t, um, "alice", "a.tar", tarBytes)
	mustAddAssociation(t, mgr, "alice", uploadId)

	cases := []struct {
		vfsPath string
		want    string
	}{
		{"/hello.txt", "hello world"},
		{"/dir/a.txt", "nested"},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, "/api/dyn-assets/uploads/"+uploadId+tc.vfsPath, nil)
		req.SetPathValue("upload_id", uploadId)
		req = req.WithContext(context.WithValue(context.Background(), pkgutils.CtxKeySessionObject, sessWithSubject("alice")))
		w := httptest.NewRecorder()
		mgr.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("GET %s: status = %d, want %d", tc.vfsPath, w.Code, http.StatusOK)
		}
		body, _ := io.ReadAll(w.Result().Body)
		if string(body) != tc.want {
			t.Errorf("GET %s: body = %q, want %q", tc.vfsPath, string(body), tc.want)
		}
	}
	// HEAD should also succeed
	req := httptest.NewRequest(http.MethodHead, "/api/dyn-assets/uploads/"+uploadId+"/hello.txt", nil)
	req.SetPathValue("upload_id", uploadId)
	req = req.WithContext(context.WithValue(context.Background(), pkgutils.CtxKeySessionObject, sessWithSubject("alice")))
	w := httptest.NewRecorder()
	mgr.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("HEAD status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestFsBasedAssociationManager_ServeHTTP_Isolation(t *testing.T) {
	mgr, um, _, _, _ := newTestManager(t)
	tarBytes := buildTar(t, map[string][]byte{"hello.txt": []byte("secret")})
	uploadId := uploadTarForUser(t, um, "alice", "a.tar", tarBytes)
	mustAddAssociation(t, mgr, "alice", uploadId)

	req := httptest.NewRequest(http.MethodGet, "/api/dyn-assets/uploads/"+uploadId+"/hello.txt", nil)
	req.SetPathValue("upload_id", uploadId)
	req = req.WithContext(context.WithValue(context.Background(), pkgutils.CtxKeySessionObject, sessWithSubject("bob")))
	w := httptest.NewRecorder()
	mgr.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("bob should not access alice's upload: status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

// ---------------------------------------------------------------------------
// Lifecycle / dispatch edge cases
// ---------------------------------------------------------------------------

func TestFsBasedAssociationManager_Shutdown(t *testing.T) {
	sm := pkgsession.NewOnMemorySessionManager()
	um := pkgmodelsuserupload.NewOnMemoryUserUploadManager()
	mgr := NewFsBasedAssociationManager(um, sm)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Run(ctx)
	// give Run a moment to start
	time.Sleep(10 * time.Millisecond)
	mgr.Shutdown()
	// second shutdown should be no-op (no panic)
	mgr.Shutdown()
	_, err := mgr.GetAssociationsByUserId(context.Background(), "alice")
	if err == nil || !strings.Contains(err.Error(), "shutting down") {
		t.Fatalf("expected shutting down error, got %v", err)
	}
}

func TestFsBasedAssociationManager_ContextCancellation(t *testing.T) {
	sm := pkgsession.NewOnMemorySessionManager()
	um := pkgmodelsuserupload.NewOnMemoryUserUploadManager()
	mgr := NewFsBasedAssociationManager(um, sm)
	// Do NOT start Run; dispatch should block until context cancels
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := mgr.GetAssociationsByUserId(ctx, "alice")
	if err == nil {
		t.Fatal("expected error on cancelled context")
	}
}

func TestFsBasedAssociationManager_RunContextCancel(t *testing.T) {
	sm := pkgsession.NewOnMemorySessionManager()
	um := pkgmodelsuserupload.NewOnMemoryUserUploadManager()
	mgr := NewFsBasedAssociationManager(um, sm)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		mgr.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}

// ---------------------------------------------------------------------------
// helpers for session
// ---------------------------------------------------------------------------

func sessWithSubject(subject string) *pkgsession.Session {
	// Session fields are unexported; we set them via the context keys that
	// WithSessionId middleware would use, but for tests we directly inject a
	// Session object. Since we cannot set unexported fields, we create a
	// Session and set its subject via the same mechanism the manager's
	// ServeHTTP uses: it calls sm.GetSessionFromContext which reads
	// pkgutils.CtxKeySessionObject. So we need a Session with SubjectId() == subject.
	// We achieve this by creating a Session via reflection.
	// Simpler: use a custom SessionManager that returns a fixed session.
	// Instead, we construct a Session by using the fact that Session is a struct
	// with unexported fields but we can set them via a helper that uses the
	// same package's ability to set via WithSession? No, WithSession just stores
	// the pointer. So we need to set the unexported field via unsafe or via
	// the test's ability to use the session package's internal constructor.
	// The easiest is to create a Session via the middleware path: set
	// CtxKeySubjectId and then call WithSessionId.
	// For unit tests we bypass by using a fake SessionManager.
	return newSessionWithSubject(subject)
}

// newSessionWithSubject creates a Session with the given subject using the
// same logic as the real middleware: it stores subject in context and then
// extracts via WithSessionId. We simulate by directly creating a Session
// via a helper that uses the session package's unexported fields through
// a test-only constructor. Since we cannot access unexported fields, we
// instead return a Session that we will make GetSessionFromContext return
// via a custom context value. To avoid reflection, we use a trick: the
// Session struct is in the same module, but we are in a different package
// (fsbasedassociation), so we cannot set unexported fields. Instead we
// create a SessionManager that returns the desired session.
// For ServeHTTP tests we inject the session directly via context value
// pkgutils.CtxKeySessionObject, so we need a real Session object with
// SubjectId() returning the subject. We can achieve this by using
// pkgsession.NewOnMemorySessionManager().WithSession and then
// GetSessionFromContext will return it. The Session object itself must have
// subject set; we can set it by using the session package's test helper
// that we define here via a small hack: we create a Session and set its
// fields via the exported method? There is no exported setter, so we use
// reflection via unsafe.
func newSessionWithSubject(subject string) *pkgsession.Session {
	// Use reflection to set unexported field subjectId.
	// This is test-only and avoids needing to change the session package.
	s := &pkgsession.Session{}
	// Use a helper that sets via the context middleware path: create a
	// context with CtxKeySubjectId and then run WithSessionId to produce a
	// Session with the correct subject.
	sm := pkgsession.NewOnMemorySessionManager()
	ctx := context.WithValue(context.Background(), pkgutils.CtxKeySubjectId, subject)
	ctx = context.WithValue(ctx, pkgutils.CtxKeySessionId, "test-session-id")
	// WithSessionId wraps a handler that captures the session from context.
	var captured *pkgsession.Session
	h := pkgsession.WithSessionId(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, _ := sm.GetSessionFromContext(r.Context())
		captured = sess
	}), sm)
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if captured == nil {
		panic("failed to create session with subject")
	}
	// Also need to ensure the original s is not used; return captured
	_ = s
	return captured
}
