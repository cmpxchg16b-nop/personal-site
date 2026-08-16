package useruploads_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strconv"
	"strings"
	"testing"

	"personal-site/pkg/api/useruploads"
	"personal-site/pkg/models/userupload"
	"personal-site/pkg/session"
	pkgutils "personal-site/pkg/utils"
)

// --- fakes / helpers -------------------------------------------------------

// fakeManager is a minimal UserUploadManager that records calls and delegates
// to a real OnMemoryUserUploadManager for actual storage behavior, while
// letting tests inject errors.
type fakeManager struct {
	inner *userupload.OnMemoryUserUploadManager

	createErr error
	listErr   error
	getErr    error
	deleteErr error
}

func newFakeManager() *fakeManager {
	return &fakeManager{inner: userupload.NewOnMemoryUserUploadManager()}
}

func (m *fakeManager) CreateNewUserUpload(ctx context.Context, r io.Reader, userId string, metadata userupload.FileMetadata) (userupload.UserUploadSummery, error) {
	if m.createErr != nil {
		return userupload.UserUploadSummery{}, m.createErr
	}
	return m.inner.CreateNewUserUpload(ctx, r, userId, metadata)
}

func (m *fakeManager) ListUserUploads(ctx context.Context, userId string) ([]userupload.UserUploadSummery, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	return m.inner.ListUserUploads(ctx, userId)
}

func (m *fakeManager) GetUserUploadByUploadId(ctx context.Context, userId, uploadId string) (userupload.UserUploadSummery, io.ReadCloser, error) {
	if m.getErr != nil {
		return userupload.UserUploadSummery{}, nil, m.getErr
	}
	return m.inner.GetUserUploadByUploadId(ctx, userId, uploadId)
}

func (m *fakeManager) DeleteUserUpload(ctx context.Context, userId, uploadId string) error {
	if m.deleteErr != nil {
		return m.deleteErr
	}
	return m.inner.DeleteUserUpload(ctx, userId, uploadId)
}

// summaryDTO mirrors the handler's on-the-wire summary shape.
type summaryDTO struct {
	UploadId       string `json:"upload_id"`
	Filename       string `json:"filename"`
	MIMEType       string `json:"mime_type"`
	SizeBytes      int64  `json:"size_bytes"`
	LastModifiedAt int64  `json:"last_modified_at"`
	Sha256         string `json:"sha256"`
	UserId         string `json:"user_id"`
}

type listResponse struct {
	Uploads []summaryDTO `json:"uploads"`
}

func decodeJSON(t *testing.T, body string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), v); err != nil {
		t.Fatalf("decode body %q: %v", body, err)
	}
}

// newMultipartBody builds a multipart/form-data body carrying a single "file"
// field with the given filename, content type, and content. Returns the body
// and the matching Content-Type header value (including the boundary).
func newMultipartBody(t *testing.T, filename, contentType string, content []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	if contentType != "" {
		// Create the part manually so we control the per-part Content-Type
		// header (FormFile's helper does not let you set it).
		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition", `form-data; name="file"; filename="`+filename+`"`)
		h.Set("Content-Type", contentType)
		part, err := mw.CreatePart(h)
		if err != nil {
			t.Fatalf("CreatePart: %v", err)
		}
		if _, err := part.Write(content); err != nil {
			t.Fatalf("part.Write: %v", err)
		}
	} else {
		field, err := mw.CreateFormFile("file", filename)
		if err != nil {
			t.Fatalf("CreateFormFile: %v", err)
		}
		if _, err := field.Write(content); err != nil {
			t.Fatalf("field.Write: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("mw.Close: %v", err)
	}
	return &buf, mw.FormDataContentType()
}

// testEnv wires a handler behind a ServeMux that mirrors the documented mount.
type testEnv struct {
	sm        *session.OnMemorySessionManager
	mgr       *fakeManager
	subjectID string
	mux       *http.ServeMux
}

func newTestEnv(t *testing.T, mgr *fakeManager) *testEnv {
	t.Helper()
	sm := session.NewOnMemorySessionManager()
	h := useruploads.NewUserUploadsHandler(sm, mgr)
	mux := http.NewServeMux()
	mux.Handle("/api/useruploads", h)
	mux.Handle("/api/useruploads/", h)
	return &testEnv{sm: sm, mgr: mgr, subjectID: "subject-test", mux: mux}
}

// serve issues a request through the env's mux with a session attached.
func (e *testEnv) serve(t *testing.T, method, target string, body io.Reader, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, body)
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	ctx := context.WithValue(r.Context(), pkgutils.CtxKeySubjectId, e.subjectID)
	r = r.WithContext(ctx)
	h := session.WithSessionId(e.mux, e.sm)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	return rr
}

// serveNoSession issues a request without attaching a session context value.
func (e *testEnv) serveNoSession(t *testing.T, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, nil)
	rr := httptest.NewRecorder()
	e.mux.ServeHTTP(rr, r)
	return rr
}

// --- tests ----------------------------------------------------------------

func TestHandler_CreateThenListThenDownloadThenDelete(t *testing.T) {
	env := newTestEnv(t, newFakeManager())
	content := []byte("hello upload")

	body, ct := newMultipartBody(t, "note.txt", "text/plain", content)
	rr := env.serve(t, http.MethodPost, "/api/useruploads", body, ct)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (body %q)", rr.Code, rr.Body.String())
	}
	var created summaryDTO
	decodeJSON(t, rr.Body.String(), &created)
	if created.UploadId == "" {
		t.Fatal("UploadId empty in create response")
	}
	if created.Filename != "note.txt" {
		t.Errorf("Filename = %q, want note.txt", created.Filename)
	}
	if created.MIMEType != "text/plain" {
		t.Errorf("MIMEType = %q, want text/plain", created.MIMEType)
	}
	if created.SizeBytes != int64(len(content)) {
		t.Errorf("SizeBytes = %d, want %d (diligent manager must compute)", created.SizeBytes, len(content))
	}
	if created.Sha256 == "" {
		t.Error("Sha256 empty, want a computed digest")
	}
	if created.UserId != env.subjectID {
		t.Errorf("UserId = %q, want %q", created.UserId, env.subjectID)
	}

	// List.
	rr = env.serve(t, http.MethodGet, "/api/useruploads", nil, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200 (body %q)", rr.Code, rr.Body.String())
	}
	var listed listResponse
	decodeJSON(t, rr.Body.String(), &listed)
	if len(listed.Uploads) != 1 || listed.Uploads[0].UploadId != created.UploadId {
		t.Fatalf("uploads = %+v, want the one we just created", listed.Uploads)
	}
	// Ensure the body is "[]", not "null", when empty elsewhere.
	if !strings.Contains(rr.Body.String(), "\"uploads\":[") {
		t.Errorf("body = %q, want it to contain \"uploads\":[", rr.Body.String())
	}

	// Download.
	rr = env.serve(t, http.MethodGet, "/api/useruploads/"+created.UploadId, nil, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("download status = %d, want 200 (body %q)", rr.Code, rr.Body.String())
	}
	if got := rr.Body.Bytes(); !bytes.Equal(got, content) {
		t.Errorf("downloaded body = %q, want %q", got, content)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "text/plain" {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	if got := rr.Header().Get("X-Upload-Id"); got != created.UploadId {
		t.Errorf("X-Upload-Id = %q, want %q", got, created.UploadId)
	}
	if got := rr.Header().Get("X-Size-Bytes"); got != strconv.FormatInt(int64(len(content)), 10) {
		t.Errorf("X-Size-Bytes = %q, want %d", got, len(content))
	}
	if got := rr.Header().Get("X-Sha256"); got != created.Sha256 {
		t.Errorf("X-Sha256 = %q, want %q", got, created.Sha256)
	}
	if got := rr.Header().Get("Content-Disposition"); !strings.Contains(got, "note.txt") {
		t.Errorf("Content-Disposition = %q, want it to contain note.txt", got)
	}

	// Delete.
	rr = env.serve(t, http.MethodDelete, "/api/useruploads/"+created.UploadId, nil, "")
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204 (body %q)", rr.Code, rr.Body.String())
	}

	// Subsequent download is 404.
	rr = env.serve(t, http.MethodGet, "/api/useruploads/"+created.UploadId, nil, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("download-after-delete status = %d, want 404", rr.Code)
	}
}

func TestHandler_ListEmptyReturnsArray(t *testing.T) {
	env := newTestEnv(t, newFakeManager())
	rr := env.serve(t, http.MethodGet, "/api/useruploads", nil, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := rr.Body.String(); !strings.Contains(got, "\"uploads\":[]") {
		t.Fatalf("body = %q, want it to contain \"uploads\":[]", got)
	}
}

func TestHandler_Create_MissingFileField(t *testing.T) {
	env := newTestEnv(t, newFakeManager())
	// A valid multipart body with no "file" field.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("other", "x")
	_ = mw.Close()
	rr := env.serve(t, http.MethodPost, "/api/useruploads", &buf, mw.FormDataContentType())
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %q)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "file") {
		t.Errorf("body = %q, want it to mention file", rr.Body.String())
	}
}

func TestHandler_Create_BadMultipart(t *testing.T) {
	env := newTestEnv(t, newFakeManager())
	// Send a multipart Content-Type but a body that is not a valid multipart
	// payload.
	rr := env.serve(t, http.MethodPost, "/api/useruploads", strings.NewReader("not multipart"), "multipart/form-data; boundary=bad")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %q)", rr.Code, rr.Body.String())
	}
}

func TestHandler_Download_NotFound(t *testing.T) {
	env := newTestEnv(t, newFakeManager())
	rr := env.serve(t, http.MethodGet, "/api/useruploads/99", nil, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestHandler_Delete_NotFound(t *testing.T) {
	env := newTestEnv(t, newFakeManager())
	rr := env.serve(t, http.MethodDelete, "/api/useruploads/99", nil, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestHandler_MethodNotAllowed(t *testing.T) {
	env := newTestEnv(t, newFakeManager())

	// Collection root: only GET, POST.
	rr := env.serve(t, http.MethodPut, "/api/useruploads", nil, "")
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rr.Code)
	}
	if got := rr.Header().Get("Allow"); got != "GET, POST" {
		t.Errorf("Allow = %q, want \"GET, POST\"", got)
	}

	// Item: only GET, DELETE.
	rr = env.serve(t, http.MethodPost, "/api/useruploads/0", nil, "")
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rr.Code)
	}
	if got := rr.Header().Get("Allow"); got != "GET, DELETE" {
		t.Errorf("Allow = %q, want \"GET, DELETE\"", got)
	}
}

func TestHandler_DeeperPathIs404(t *testing.T) {
	env := newTestEnv(t, newFakeManager())
	rr := env.serve(t, http.MethodGet, "/api/useruploads/0/extra", nil, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestHandler_MissingSession(t *testing.T) {
	env := newTestEnv(t, newFakeManager())
	rr := env.serveNoSession(t, http.MethodGet, "/api/useruploads")
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "session not found") {
		t.Errorf("body = %q, want it to mention session not found", rr.Body.String())
	}
}

func TestHandler_InternalErrorsMapTo500(t *testing.T) {
	env := newTestEnv(t, &fakeManager{
		inner:     userupload.NewOnMemoryUserUploadManager(),
		listErr:   errors.New("storage unavailable"),
		getErr:    errors.New("boom"),
		deleteErr: errors.New("boom"),
	})

	if rr := env.serve(t, http.MethodGet, "/api/useruploads", nil, ""); rr.Code != http.StatusInternalServerError {
		t.Fatalf("list status = %d, want 500", rr.Code)
	}
	if rr := env.serve(t, http.MethodGet, "/api/useruploads/0", nil, ""); rr.Code != http.StatusInternalServerError {
		t.Fatalf("get status = %d, want 500", rr.Code)
	}
	if rr := env.serve(t, http.MethodDelete, "/api/useruploads/0", nil, ""); rr.Code != http.StatusInternalServerError {
		t.Fatalf("delete status = %d, want 500", rr.Code)
	}
}

// TestHandler_Isolation verifies one subject cannot see another's uploads.
func TestHandler_Isolation(t *testing.T) {
	mgr := userupload.NewOnMemoryUserUploadManager()
	sm := session.NewOnMemorySessionManager()
	h := useruploads.NewUserUploadsHandler(sm, mgr)
	mux := http.NewServeMux()
	mux.Handle("/api/useruploads", h)
	mux.Handle("/api/useruploads/", h)

	// Alice uploads one file.
	content := []byte("alice-secret")
	body, ct := newMultipartBody(t, "a.txt", "text/plain", content)
	r := httptest.NewRequest(http.MethodPost, "/api/useruploads", body)
	r.Header.Set("Content-Type", ct)
	r = r.WithContext(context.WithValue(r.Context(), pkgutils.CtxKeySubjectId, "alice"))
	rr := httptest.NewRecorder()
	session.WithSessionId(mux, sm).ServeHTTP(rr, r)
	if rr.Code != http.StatusCreated {
		t.Fatalf("alice create status = %d, want 201 (body %q)", rr.Code, rr.Body.String())
	}
	var aliceUpload summaryDTO
	decodeJSON(t, rr.Body.String(), &aliceUpload)

	// Bob lists and sees nothing.
	r = httptest.NewRequest(http.MethodGet, "/api/useruploads", nil)
	r = r.WithContext(context.WithValue(r.Context(), pkgutils.CtxKeySubjectId, "bob"))
	rr = httptest.NewRecorder()
	session.WithSessionId(mux, sm).ServeHTTP(rr, r)
	var bobList listResponse
	decodeJSON(t, rr.Body.String(), &bobList)
	if len(bobList.Uploads) != 0 {
		t.Fatalf("bob sees %d uploads, want 0 (isolation broken)", len(bobList.Uploads))
	}

	// Bob cannot download Alice's uploadId.
	r = httptest.NewRequest(http.MethodGet, "/api/useruploads/"+aliceUpload.UploadId, nil)
	r = r.WithContext(context.WithValue(r.Context(), pkgutils.CtxKeySubjectId, "bob"))
	rr = httptest.NewRecorder()
	session.WithSessionId(mux, sm).ServeHTTP(rr, r)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("bob download alice's upload status = %d, want 404", rr.Code)
	}
}
