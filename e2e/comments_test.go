// Package e2e holds end-to-end tests: they build the real server binary,
// run it, and exercise it over HTTP — as opposed to the unit tests living
// next to each package, which call handlers in-process.
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// serverBin is the server binary built once for the whole suite by
// TestMain.
var serverBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "personal-site-e2e")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create temp dir for the server binary: %v\n", err)
		os.Exit(1)
	}
	serverBin = filepath.Join(dir, "server")
	// The test's working directory is this package's directory, inside the
	// module, so the module-qualified package path resolves.
	if out, err := exec.Command("go", "build", "-o", serverBin, "personal-site/cmd/server").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build the server binary: %v\n%s\n", err, out)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// startServer launches the server binary with default arguments; see
// startServerWithArgs.
func startServer(t *testing.T) string {
	t.Helper()
	return startServerWithArgs(t)
}

// startServerWithArgs launches the server binary on a free loopback port
// with extra command-line arguments, waits for its health endpoint to
// answer, and returns the base URL. The process is killed when the test
// ends; on failure its output is dumped to the test log.
func startServerWithArgs(t *testing.T, args ...string) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("release the reserved port: %v", err)
	}
	return startServerOnAddr(t, addr, args...)
}

// startServerOnAddr launches the server binary on addr; see
// startServerWithArgs. Callers that need the address before the server
// starts — e.g. to reference it in the configuration document — reserve
// the port themselves.
func startServerOnAddr(t *testing.T, addr string, args ...string) string {
	t.Helper()

	var output bytes.Buffer
	cmd := exec.Command(serverBin, append([]string{"serve", "--addr", addr}, args...)...)
	cmd.Stdout = &output
	cmd.Stderr = &output
	// The server requires a JWT secret at startup (it signs and validates the
	// login session tokens); supply a throwaway one for the test process.
	cmd.Env = append(os.Environ(), "JWT_SECRET=personal-site-e2e-secret")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the server: %v", err)
	}
	done := make(chan struct{})
	go func() {
		cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-done
		if t.Failed() {
			t.Logf("server output:\n%s", output.String())
		}
	})

	baseURL := "http://" + addr
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case <-done:
			t.Fatalf("server exited before becoming ready:\n%s", output.String())
		default:
		}
		resp, err := http.Get(baseURL + "/api/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return baseURL
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not become ready at %s (last probe error: %v)", baseURL, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// commentView is the test's own view of a comment as served by the API —
// deliberately not shared with the server-side wire type, so that
// accidental changes to the wire format fail here.
type commentView struct {
	ID            string `json:"id"`
	ChannelID     string `json:"channel_id"`
	UserID        string `json:"user_id"`
	SerialNumber  uint64 `json:"serial_number"`
	LastCommentID string `json:"last_comment_id"`
	Content       string `json:"content"`
	MIMEType      string `json:"mime_type"`
	CreationTime  uint64 `json:"creation_time"`
	LastModified  uint64 `json:"last_modified"`
}

// getComments GETs the comments of channelId, returning the status code and
// the decoded comments. jwtCookie is the session cookie to carry ("" for an
// anonymous read — GETs are open).
func getComments(t *testing.T, baseURL, channelId, jwtCookie string) (int, []commentView) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, baseURL+"/api/comments/channel/"+channelId, nil)
	if err != nil {
		t.Fatalf("GET channel %q: %v", channelId, err)
	}
	if jwtCookie != "" {
		req.AddCookie(&http.Cookie{Name: "jwt", Value: jwtCookie})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET channel %q: %v", channelId, err)
	}
	defer resp.Body.Close()
	var body struct {
		Comments []commentView `json:"comments"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("GET channel %q: decode response: %v", channelId, err)
	}
	return resp.StatusCode, body.Comments
}

// putComment PUTs payload (any JSON-marshalable value) to channelId,
// returning the status code and the raw response body. jwtCookie is the
// session cookie to carry ("" exercises the unauthenticated path).
func putComment(t *testing.T, baseURL, channelId, jwtCookie string, payload any) (int, []byte) {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	req, err := http.NewRequest(http.MethodPut,
		baseURL+"/api/comments/channel/"+channelId, bytes.NewReader(b))
	if err != nil {
		t.Fatalf("build PUT request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if jwtCookie != "" {
		req.AddCookie(&http.Cookie{Name: "jwt", Value: jwtCookie})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT channel %q: %v", channelId, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("PUT channel %q: read response: %v", channelId, err)
	}
	return resp.StatusCode, data
}

// mustPutComment PUTs a comment expecting success and returns it decoded.
func mustPutComment(t *testing.T, baseURL, channelId, jwtCookie, content, lastCommentId string) commentView {
	t.Helper()
	status, body := putComment(t, baseURL, channelId, jwtCookie, map[string]string{
		"content":         content,
		"last_comment_id": lastCommentId,
	})
	if status != http.StatusCreated {
		t.Fatalf("PUT channel %q: status: got %d, want %d (body: %s)", channelId, status, http.StatusCreated, body)
	}
	var c commentView
	if err := json.Unmarshal(body, &c); err != nil {
		t.Fatalf("PUT channel %q: decode response: %v", channelId, err)
	}
	return c
}

// profileUsername GETs /api/profile with the session cookie and returns the
// session's username — the identity the comments API is expected to stamp on
// appended comments.
func profileUsername(t *testing.T, baseURL, jwtCookie string) string {
	t.Helper()
	_, _, username := profileIdentity(t, baseURL, jwtCookie)
	if username == "" {
		t.Fatal("GET /api/profile: empty username")
	}
	return username
}

// TestCommentsAPI drives the comments API of a freshly started server
// through a full commenting scenario: reads are anonymous, appends carry a
// visitor session, and the server stamps each comment with the session's
// identity.
func TestCommentsAPI(t *testing.T) {
	baseURL := startServer(t)
	channel := "e2e-post"

	// A fresh channel is an empty list — readable without any session.
	status, comments := getComments(t, baseURL, channel, "")
	if status != http.StatusOK {
		t.Fatalf("GET fresh channel: status: got %d, want %d", status, http.StatusOK)
	}
	if len(comments) != 0 {
		t.Fatalf("GET fresh channel: got %d comments, want 0", len(comments))
	}

	// Appending without a session is rejected: the author comes from the
	// caller's session, so an anonymous PUT has no identity to stamp.
	status, _ = putComment(t, baseURL, channel, "", map[string]string{
		"content":         "anonymous",
		"last_comment_id": "",
	})
	if status != http.StatusUnauthorized {
		t.Errorf("PUT without a session: status: got %d, want %d", status, http.StatusUnauthorized)
	}

	// Log in as a visitor; every append below carries this session's cookie.
	jwtCookie := loginAsVisitor(t, baseURL+"/api/login/visitor")
	if jwtCookie == "" {
		t.Fatal("visitor login did not set a jwt cookie")
	}
	// The identity the server is expected to stamp on the session's comments.
	author := profileUsername(t, baseURL, jwtCookie)

	// The first comment: server-assigned id, serial 0, no parent, and the
	// session's identity as its author.
	first := mustPutComment(t, baseURL, channel, jwtCookie, "first!", "")
	if first.ID == "" {
		t.Error("first comment: id is empty")
	}
	if first.ChannelID != channel {
		t.Errorf("first comment: channel_id = %q, want %q", first.ChannelID, channel)
	}
	if first.UserID != author {
		t.Errorf("first comment: user_id = %q, want the session's identity %q", first.UserID, author)
	}
	if first.SerialNumber != 0 {
		t.Errorf("first comment: serial_number = %d, want 0", first.SerialNumber)
	}
	if first.LastCommentID != "" {
		t.Errorf("first comment: last_comment_id = %q, want empty", first.LastCommentID)
	}
	if first.MIMEType != "text/plain" {
		t.Errorf("first comment: mime_type = %q, want text/plain", first.MIMEType)
	}
	if first.CreationTime == 0 {
		t.Error("first comment: creation_time is 0")
	}
	if first.LastModified != first.CreationTime {
		t.Errorf("first comment: last_modified = %d, want creation_time %d", first.LastModified, first.CreationTime)
	}

	// A second comment chained on the first.
	second := mustPutComment(t, baseURL, channel, jwtCookie, "second!", first.ID)
	if second.SerialNumber != 1 {
		t.Errorf("second comment: serial_number = %d, want 1", second.SerialNumber)
	}
	if second.LastCommentID != first.ID {
		t.Errorf("second comment: last_comment_id = %q, want %q", second.LastCommentID, first.ID)
	}

	// The channel reads back oldest-first — still anonymously.
	status, comments = getComments(t, baseURL, channel, "")
	if status != http.StatusOK {
		t.Fatalf("GET channel: status: got %d, want %d", status, http.StatusOK)
	}
	if len(comments) != 2 {
		t.Fatalf("GET channel: got %d comments, want 2", len(comments))
	}
	if comments[0].ID != first.ID || comments[1].ID != second.ID {
		t.Errorf("GET channel: got ids [%s %s], want [%s %s]",
			comments[0].ID, comments[1].ID, first.ID, second.ID)
	}
	if comments[1].LastCommentID != comments[0].ID {
		t.Errorf("GET channel: chain broken: comment[1].last_comment_id = %q, want %q",
			comments[1].LastCommentID, comments[0].ID)
	}

	// Appending onto a last comment that is no longer the channel's last
	// conflicts; the client is expected to re-read and retry.
	status, _ = putComment(t, baseURL, channel, jwtCookie, map[string]string{
		"content":         "late reply",
		"last_comment_id": first.ID,
	})
	if status != http.StatusConflict {
		t.Errorf("PUT with stale last comment: status: got %d, want %d", status, http.StatusConflict)
	}

	// A last comment id that does not exist at all is a bad request.
	status, _ = putComment(t, baseURL, channel, jwtCookie, map[string]string{
		"content":         "orphan",
		"last_comment_id": "no-such-comment",
	})
	if status != http.StatusBadRequest {
		t.Errorf("PUT with unknown last comment: status: got %d, want %d", status, http.StatusBadRequest)
	}

	// Only text/plain is supported.
	status, _ = putComment(t, baseURL, channel, jwtCookie, map[string]string{
		"content":         "<b>bold</b>",
		"mime_type":       "text/html",
		"last_comment_id": second.ID,
	})
	if status != http.StatusUnsupportedMediaType {
		t.Errorf("PUT text/html: status: got %d, want %d", status, http.StatusUnsupportedMediaType)
	}

	// content is required; the body must be a JSON object.
	status, _ = putComment(t, baseURL, channel, jwtCookie, map[string]string{"last_comment_id": second.ID})
	if status != http.StatusBadRequest {
		t.Errorf("PUT without content: status: got %d, want %d", status, http.StatusBadRequest)
	}
	status, _ = putComment(t, baseURL, channel, jwtCookie, "not a json object")
	if status != http.StatusBadRequest {
		t.Errorf("PUT with a non-object body: status: got %d, want %d", status, http.StatusBadRequest)
	}

	// Other methods on the channel path are not allowed.
	req, err := http.NewRequest(http.MethodPost,
		baseURL+"/api/comments/channel/"+channel, strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST channel: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "jwt", Value: jwtCookie})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST channel: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST channel: status: got %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
	}

	// Channels are isolated: a comment elsewhere starts its own chain and
	// does not leak into the first channel. A second visitor session gets its
	// own identity — the server stamps each comment with the session it came
	// from, not with whatever the client claims.
	jwtCookieB := loginAsVisitor(t, baseURL+"/api/login/visitor")
	if jwtCookieB == "" {
		t.Fatal("second visitor login did not set a jwt cookie")
	}
	authorB := profileUsername(t, baseURL, jwtCookieB)
	other := mustPutComment(t, baseURL, "e2e-other", jwtCookieB, "elsewhere", "")
	if other.SerialNumber != 0 {
		t.Errorf("other channel's first comment: serial_number = %d, want 0", other.SerialNumber)
	}
	if other.UserID != authorB {
		t.Errorf("other channel comment: user_id = %q, want its own session's identity %q", other.UserID, authorB)
	}
	_, comments = getComments(t, baseURL, channel, "")
	if len(comments) != 2 {
		t.Errorf("GET channel after posting elsewhere: got %d comments, want 2", len(comments))
	}
	_, comments = getComments(t, baseURL, "e2e-other", "")
	if len(comments) != 1 {
		t.Errorf("GET other channel: got %d comments, want 1", len(comments))
	}
}
