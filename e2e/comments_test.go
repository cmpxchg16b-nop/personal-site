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

	var output bytes.Buffer
	cmd := exec.Command(serverBin, append([]string{"--addr", addr}, args...)...)
	cmd.Stdout = &output
	cmd.Stderr = &output
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
// the decoded comments.
func getComments(t *testing.T, baseURL, channelId string) (int, []commentView) {
	t.Helper()
	resp, err := http.Get(baseURL + "/api/comments/channel/" + channelId)
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
// returning the status code and the raw response body.
func putComment(t *testing.T, baseURL, channelId string, payload any) (int, []byte) {
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
func mustPutComment(t *testing.T, baseURL, channelId, userId, content, lastCommentId string) commentView {
	t.Helper()
	status, body := putComment(t, baseURL, channelId, map[string]string{
		"user_id":         userId,
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

// TestCommentsAPI drives the comments API of a freshly started server
// through a full commenting scenario.
func TestCommentsAPI(t *testing.T) {
	baseURL := startServer(t)
	channel := "e2e-post"

	// A fresh channel is an empty list.
	status, comments := getComments(t, baseURL, channel)
	if status != http.StatusOK {
		t.Fatalf("GET fresh channel: status: got %d, want %d", status, http.StatusOK)
	}
	if len(comments) != 0 {
		t.Fatalf("GET fresh channel: got %d comments, want 0", len(comments))
	}

	// The first comment: server-assigned id, serial 0, no parent. Anyone can
	// post under any user id — there is no authentication.
	first := mustPutComment(t, baseURL, channel, "alice", "first!", "")
	if first.ID == "" {
		t.Error("first comment: id is empty")
	}
	if first.ChannelID != channel {
		t.Errorf("first comment: channel_id = %q, want %q", first.ChannelID, channel)
	}
	if first.UserID != "alice" {
		t.Errorf("first comment: user_id = %q, want %q", first.UserID, "alice")
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

	// A second comment chained on the first, in someone else's name.
	second := mustPutComment(t, baseURL, channel, "bob", "second!", first.ID)
	if second.SerialNumber != 1 {
		t.Errorf("second comment: serial_number = %d, want 1", second.SerialNumber)
	}
	if second.LastCommentID != first.ID {
		t.Errorf("second comment: last_comment_id = %q, want %q", second.LastCommentID, first.ID)
	}

	// The channel reads back oldest-first.
	status, comments = getComments(t, baseURL, channel)
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
	status, _ = putComment(t, baseURL, channel, map[string]string{
		"user_id":         "alice",
		"content":         "late reply",
		"last_comment_id": first.ID,
	})
	if status != http.StatusConflict {
		t.Errorf("PUT with stale last comment: status: got %d, want %d", status, http.StatusConflict)
	}

	// A last comment id that does not exist at all is a bad request.
	status, _ = putComment(t, baseURL, channel, map[string]string{
		"user_id":         "alice",
		"content":         "orphan",
		"last_comment_id": "no-such-comment",
	})
	if status != http.StatusBadRequest {
		t.Errorf("PUT with unknown last comment: status: got %d, want %d", status, http.StatusBadRequest)
	}

	// Only text/plain is supported.
	status, _ = putComment(t, baseURL, channel, map[string]string{
		"user_id":         "alice",
		"content":         "<b>bold</b>",
		"mime_type":       "text/html",
		"last_comment_id": second.ID,
	})
	if status != http.StatusUnsupportedMediaType {
		t.Errorf("PUT text/html: status: got %d, want %d", status, http.StatusUnsupportedMediaType)
	}

	// user_id and content are required; the body must be a JSON object.
	status, _ = putComment(t, baseURL, channel, map[string]string{"content": "anonymous"})
	if status != http.StatusBadRequest {
		t.Errorf("PUT without user_id: status: got %d, want %d", status, http.StatusBadRequest)
	}
	status, _ = putComment(t, baseURL, channel, map[string]string{"user_id": "alice"})
	if status != http.StatusBadRequest {
		t.Errorf("PUT without content: status: got %d, want %d", status, http.StatusBadRequest)
	}
	status, _ = putComment(t, baseURL, channel, "not a json object")
	if status != http.StatusBadRequest {
		t.Errorf("PUT with a non-object body: status: got %d, want %d", status, http.StatusBadRequest)
	}

	// Other methods on the channel path are not allowed.
	resp, err := http.Post(baseURL+"/api/comments/channel/"+channel, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST channel: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST channel: status: got %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
	}

	// Channels are isolated: a comment elsewhere starts its own chain and
	// does not leak into the first channel.
	other := mustPutComment(t, baseURL, "e2e-other", "carol", "elsewhere", "")
	if other.SerialNumber != 0 {
		t.Errorf("other channel's first comment: serial_number = %d, want 0", other.SerialNumber)
	}
	_, comments = getComments(t, baseURL, channel)
	if len(comments) != 2 {
		t.Errorf("GET channel after posting elsewhere: got %d comments, want 2", len(comments))
	}
	_, comments = getComments(t, baseURL, "e2e-other")
	if len(comments) != 1 {
		t.Errorf("GET other channel: got %d comments, want 1", len(comments))
	}
}
