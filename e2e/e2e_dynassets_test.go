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
	"testing"
	"time"

	"github.com/spf13/afero"

	pkgapiexamassociations "personal-site/pkg/api/examassociations"
	pkgapiloginvisitor "personal-site/pkg/api/login/visitor"
	pkgapiuseruploads "personal-site/pkg/api/useruploads"
	pkgauth "personal-site/pkg/auth"
	pkgcookie "personal-site/pkg/cookie"
	pkgmodelsuserexamdocsfsbasedassociation "personal-site/pkg/models/userexamdocs/fsbasedassociation"
	pkgmodelsuserupload "personal-site/pkg/models/userupload"
	pkgsession "personal-site/pkg/session"
)

// TestE2E_DynAssetsAndAssociations exercises the full upload → associate →
// serve lifecycle for user-provided tarballs, plus de-association and
// re-association, through the real HTTP stack as a logged-in visitor.
func TestE2E_DynAssetsAndAssociations(t *testing.T) {
	// --- Wire up the full server (mirrors main.go) -------------------------

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sm := pkgsession.NewOnMemorySessionManager()
	userUploadManager := pkgmodelsuserupload.NewOnMemoryUserUploadManager()
	associationManager := pkgmodelsuserexamdocsfsbasedassociation.NewFsBasedAssociationManager(userUploadManager, sm)
	go associationManager.Run(ctx)
	t.Cleanup(associationManager.Shutdown)

	mux := http.NewServeMux()

	userUploadsHandler := pkgapiuseruploads.NewUserUploadsHandler(sm, userUploadManager)
	mux.Handle("/api/useruploads", userUploadsHandler)
	mux.Handle("/api/useruploads/", userUploadsHandler)

	examAssociationsHandler := pkgapiexamassociations.NewExamAssociationsHandler(sm, associationManager)
	mux.Handle("/api/examassociations", examAssociationsHandler)
	mux.Handle("/api/examassociations/{association_id}", examAssociationsHandler)

	mux.Handle("/api/dyn-assets/uploads/{upload_id}/{vfs_path...}", associationManager)

	// --- Visitor login -----------------------------------------------------

	jwtSecret := []byte("e2e-test-secret")
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

	jwtCookieValue := loginAsVisitor(t, ts.URL+"/api/login/visitor")
	if jwtCookieValue == "" {
		t.Fatal("visitor login did not set a jwt cookie")
	}
	t.Logf("visitor session cookie obtained (jwt=%q)", truncate([]byte(jwtCookieValue), 30))

	client := &http.Client{Timeout: 10 * time.Second}

	// --- Build the first tarball from an in-memory afero filesystem --------

	treeA := map[string]string{
		"top.txt":                 "this is a top-level file\n",
		"dir1/file_a.txt":         "alpha content\n",
		"dir1/subdir1/file_b.txt": "beta content, nested deeper\n",
		"dir2/file_c.txt":         "gamma content\n",
	}
	tarA, hashesA, err := buildTarFromTree(treeA)
	if err != nil {
		t.Fatalf("build tar A: %v", err)
	}

	// --- Upload + associate + verify every file of tarball A ---------------

	t.Run("tarball-A", func(t *testing.T) {
		uploadIdA := uploadTarball(t, client, ts.URL, jwtCookieValue, tarA, "bundle-a.tar")
		t.Logf("uploaded bundle-a.tar as upload_id=%s", uploadIdA)

		createAssociation(t, client, ts.URL, jwtCookieValue, uploadIdA)
		assocIdA := soleAssociationId(t, client, ts.URL, jwtCookieValue)
		t.Logf("association created: id=%s (upload_id=%s)", assocIdA, uploadIdA)

		verifyDynAssets(t, client, ts.URL, jwtCookieValue, uploadIdA, treeA, hashesA)

		// --- De-association: delete, then dyn-assets must 404 ---------------
		t.Run("de-associate", func(t *testing.T) {
			deleteAssociation(t, client, ts.URL, jwtCookieValue, assocIdA)

			// Listing should now be empty.
			assocs := listAssociations(t, client, ts.URL, jwtCookieValue)
			if len(assocs) != 0 {
				t.Fatalf("after delete, got %d associations, want 0", len(assocs))
			}

			// The virtual fs should no longer be reachable.
			for path := range treeA {
				url := fmt.Sprintf("/api/dyn-assets/uploads/%s/%s", uploadIdA, path)
				if status := headStatus(t, client, ts.URL, jwtCookieValue, url); status != http.StatusNotFound {
					t.Fatalf("after delete, HEAD %s: status = %d, want %d", url, status, http.StatusNotFound)
				}
			}
		})

		// --- Re-association: same upload, new association, fs reachable again
		t.Run("re-associate", func(t *testing.T) {
			createAssociation(t, client, ts.URL, jwtCookieValue, uploadIdA)
			verifyDynAssets(t, client, ts.URL, jwtCookieValue, uploadIdA, treeA, hashesA)
		})
	})

	// --- A second, distinct tarball exercises an independent association ---

	t.Run("tarball-B", func(t *testing.T) {
		treeB := map[string]string{
			"readme.md":     "# second bundle\n",
			"deep/a/b/c.md": "deeply nested\n",
		}
		tarB, hashesB, err := buildTarFromTree(treeB)
		if err != nil {
			t.Fatalf("build tar B: %v", err)
		}
		uploadIdB := uploadTarball(t, client, ts.URL, jwtCookieValue, tarB, "bundle-b.tar")
		t.Logf("uploaded bundle-b.tar as upload_id=%s", uploadIdB)

		createAssociation(t, client, ts.URL, jwtCookieValue, uploadIdB)
		verifyDynAssets(t, client, ts.URL, jwtCookieValue, uploadIdB, treeB, hashesB)
	})
}

// buildTarFromTree seeds an in-memory afero filesystem with the given
// path→content entries (each a regular file), computes the SHA-256 of each
// file's bytes, then builds a tar archive via tar.Writer.AddFS over an
// afero.IOFS bridge. It returns the tar bytes and the map of path→hex sha256.
func buildTarFromTree(tree map[string]string) (tarBytes []byte, hashes map[string]string, err error) {
	mem := afero.NewMemMapFs()
	hashes = make(map[string]string, len(tree))
	for path, content := range tree {
		// Paths are written without a leading slash so they pass fs.ValidPath
		// when the IOFS bridge opens them; tarfs re-adds the slash on lookup.
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

// uploadTarball POSTs a multipart "file" field containing the tar bytes to
// /api/useruploads and returns the assigned upload_id from the response.
func uploadTarball(t *testing.T, client *http.Client, baseURL, jwtCookie string, tarBytes []byte, filename string) string {
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
		t.Fatalf("upload %s: status = %d, want %d (body %s)", filename, resp.StatusCode, http.StatusCreated, respBody)
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

// createAssociation POSTs {"upload_id": ...} to /api/examassociations.
func createAssociation(t *testing.T, client *http.Client, baseURL, jwtCookie, uploadId string) {
	t.Helper()
	respBody := cookieReq(t, client, baseURL, http.MethodPost, "/api/examassociations",
		fmt.Sprintf(`{"upload_id":%q}`, uploadId), jwtCookie)
	// 201 Created with no body on success.
	_ = respBody
}

// listAssociations GETs /api/examassociations and returns the parsed list.
func listAssociations(t *testing.T, client *http.Client, baseURL, jwtCookie string) []associationDTO {
	t.Helper()
	body := cookieReq(t, client, baseURL, http.MethodGet, "/api/examassociations", "", jwtCookie)
	var resp struct {
		Associations []associationDTO `json:"associations"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode associations %q: %v", body, err)
	}
	return resp.Associations
}

type associationDTO struct {
	Id       string `json:"id"`
	UserId   string `json:"user_id"`
	UploadId string `json:"upload_id"`
}

// soleAssociationId asserts that exactly one association exists and returns it.
func soleAssociationId(t *testing.T, client *http.Client, baseURL, jwtCookie string) string {
	t.Helper()
	assocs := listAssociations(t, client, baseURL, jwtCookie)
	if len(assocs) != 1 {
		t.Fatalf("expected exactly 1 association, got %d (%+v)", len(assocs), assocs)
	}
	return assocs[0].Id
}

// deleteAssociation DELETEs /api/examassociations/{id}.
func deleteAssociation(t *testing.T, client *http.Client, baseURL, jwtCookie, associationId string) {
	t.Helper()
	cookieReq(t, client, baseURL, http.MethodDelete, "/api/examassociations/"+associationId, "", jwtCookie)
}

// verifyDynAssets downloads every file in the tree via the dyn-assets endpoint
// and asserts the SHA-256 of the returned bytes matches the expected hash.
func verifyDynAssets(t *testing.T, client *http.Client, baseURL, jwtCookie, uploadId string, tree map[string]string, hashes map[string]string) {
	t.Helper()
	for path := range tree {
		url := fmt.Sprintf("/api/dyn-assets/uploads/%s/%s", uploadId, path)
		respBody := cookieReq(t, client, baseURL, http.MethodGet, url, "", jwtCookie)

		gotSum := sha256.Sum256(respBody)
		gotHex := hex.EncodeToString(gotSum[:])
		if gotHex != hashes[path] {
			t.Fatalf("dyn-assets %s: sha256 mismatch\ngot  %s\nwant %s", url, gotHex, hashes[path])
		}
		t.Logf("  ✓ %s sha256=%s", path, gotHex)
	}
}

// headStatus performs a HEAD request and returns only the status code. Used to
// check reachability without downloading the body.
func headStatus(t *testing.T, client *http.Client, baseURL, jwtCookie, path string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodHead, baseURL+path, nil)
	if err != nil {
		t.Fatalf("HEAD %s: %v", path, err)
	}
	req.AddCookie(&http.Cookie{Name: "jwt", Value: jwtCookie})
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("HEAD %s: %v", path, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}
