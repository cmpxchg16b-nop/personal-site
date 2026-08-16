// Package fsbasedassociation provides a filesystem-backed implementation of
// userexamdocs.UserExamDocsAssociationManager.
package fsbasedassociation

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/spf13/afero"

	pkgmodelsquestion "personal-site/pkg/models/question"
	pkgmodelsuserexamdocs "personal-site/pkg/models/userexamdocs"
	pkgmodelsuserupload "personal-site/pkg/models/userupload"
	pkgsession "personal-site/pkg/session"
)

// FsAssociation is one association persisted as an in-process handle to an
// afero.Fs. It pairs the identity fields (Id, UserId) with the virtual
// filesystem that holds the associated exam document content.
type FsAssociation struct {
	// Id is the globally unique identifier of this association.
	Id string

	// UserId is the id of the user who created this association.
	UserId string

	// UploadId identifies the underlying user upload that this association
	// points at.
	UploadId string

	// Fs is the afero filesystem that holds the exam document content for this
	// association.
	Fs afero.Fs
}

// errShuttingDown is reported by dispatch once the actor loop has stopped.
var errShuttingDown = errors.New("fsbasedassociation: shutting down")

// errAssociationNotFound is reported when an association id does not exist for
// the given user.
var errAssociationNotFound = errors.New("fsbasedassociation: association not found")

// FsBasedAssociationManager is a filesystem-backed implementation of
// userexamdocs.UserExamDocsAssociationManager. It runs an actor goroutine (Run)
// that owns its mutable state; all methods dispatch closures onto the actor's
// service channel. All association methods are currently stubs.
type FsBasedAssociationManager struct {
	// Here are the certifications earned by this diligent man:
	http.Handler
	pkgmodelsquestion.ExamLoader
	pkgmodelsquestion.ExamSource

	userUploadManager pkgmodelsuserupload.UserUploadManager
	sm                pkgsession.SessionManager

	// associations is the nested map user_id -> upload_id -> FsAssociation.
	// It is only ever read or written inside closures run by the actor
	// goroutine, so it needs no locking of its own.
	associations map[string]map[string]FsAssociation

	// serviceChan carries closures for the actor goroutine to run.
	serviceChan chan func()

	// done is closed by Shutdown to release the actor loop and any callers
	// blocked dispatching a command.
	done chan struct{}

	closeDoer sync.Once
}

// NewFsBasedAssociationManager constructs an FsBasedAssociationManager backed
// by the given UserUploadManager. Run must be called (in its own goroutine)
// before any association method is invoked.
func NewFsBasedAssociationManager(userUploadManager pkgmodelsuserupload.UserUploadManager, sm pkgsession.SessionManager) *FsBasedAssociationManager {
	return &FsBasedAssociationManager{
		userUploadManager: userUploadManager,
		sm:                sm,
		associations:      make(map[string]map[string]FsAssociation),
		serviceChan:       make(chan func()),
		done:              make(chan struct{}),
	}
}

// Run is the actor loop. Run it in its own goroutine; it returns when ctx is
// canceled or Shutdown is called, after which method calls report errShuttingDown.
func (m *FsBasedAssociationManager) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.done:
			return
		case cmd := <-m.serviceChan:
			cmd()
		}
	}
}

// Shutdown stops the actor. Idempotent: repeated calls are no-ops via closeDoer.
func (m *FsBasedAssociationManager) Shutdown() {
	m.closeDoer.Do(func() {
		close(m.done)
	})
}

// dispatch delivers cmd to the actor goroutine. Because serviceChan is
// unbuffered, a nil return guarantees the actor received cmd and will run it to
// completion, so the caller may safely wait on its response channel.
func (m *FsBasedAssociationManager) dispatch(ctx context.Context, cmd func()) error {
	select {
	case m.serviceChan <- cmd:
		return nil
	case <-m.done:
		return errShuttingDown
	case <-ctx.Done():
		return ctx.Err()
	}
}

// GetAssociationsByUserId returns every association owned by userId as a
// slice of ExamDocAssociation, or an empty slice if the user has none.
func (m *FsBasedAssociationManager) GetAssociationsByUserId(ctx context.Context, userId string) ([]pkgmodelsuserexamdocs.ExamDocAssociation, error) {
	type result struct {
		associations []pkgmodelsuserexamdocs.ExamDocAssociation
		err          error
	}
	resp := make(chan result, 1)
	cmd := func() {
		userAssocs, ok := m.associations[userId]
		if !ok {
			resp <- result{associations: []pkgmodelsuserexamdocs.ExamDocAssociation{}}
			return
		}
		out := make([]pkgmodelsuserexamdocs.ExamDocAssociation, 0, len(userAssocs))
		for _, fa := range userAssocs {
			out = append(out, pkgmodelsuserexamdocs.ExamDocAssociation{
				Id:       fa.Id,
				UserId:   fa.UserId,
				UploadId: fa.UploadId,
			})
		}
		resp <- result{associations: out}
	}
	if err := m.dispatch(ctx, cmd); err != nil {
		return nil, err
	}
	select {
	case r := <-resp:
		return r.associations, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// DeleteAssociation removes the association identified by associationId from
// userId's associations. It returns errAssociationNotFound if the association
// does not exist or does not belong to userId.
func (m *FsBasedAssociationManager) DeleteAssociation(ctx context.Context, userId string, associationId string) error {
	type result struct{ err error }
	resp := make(chan result, 1)
	cmd := func() {
		userAssocs, ok := m.associations[userId]
		if !ok {
			resp <- result{err: errAssociationNotFound}
			return
		}
		for uploadId, fa := range userAssocs {
			if fa.Id == associationId {
				delete(userAssocs, uploadId)
				if len(userAssocs) == 0 {
					delete(m.associations, userId)
				}
				resp <- result{}
				return
			}
		}
		resp <- result{err: errAssociationNotFound}
	}
	if err := m.dispatch(ctx, cmd); err != nil {
		return err
	}
	select {
	case r := <-resp:
		return r.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// AddAssociation creates a new association owned by userId that points at the
// given uploadId. It fetches the upload's content from the UserUploadManager,
// materializes it as an afero filesystem (only .tar uploads are currently
// supported, read into memory via tarfs), and records the association. It
// returns an error if the upload does not exist, does not belong to userId, is
// not a .tar file, or is not a valid tar archive.
func (m *FsBasedAssociationManager) AddAssociation(ctx context.Context, userId string, uploadId string) error {
	summary, rc, err := m.userUploadManager.GetUserUploadByUploadId(ctx, userId, uploadId)
	if err != nil {
		return fmt.Errorf("get upload %q: %w", uploadId, err)
	}

	if !strings.HasSuffix(summary.Filename, ".tar") {
		rc.Close()
		return fmt.Errorf("unsupported upload %q: only .tar files are supported", summary.Filename)
	}

	data, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		return fmt.Errorf("read upload %q: %w", uploadId, err)
	}
	// Materialize the tar into a brand new MemMapFs so the resulting
	// filesystem is fully writable and reusable (tarfs reuses the same
	// *bytes.Reader for each file handle, so a second Open of the same
	// path would otherwise read from EOF). Copy every entry from the
	// archive into the new MemMapFs.
	memFs := afero.NewMemMapFs()
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar archive for upload %q: %w", uploadId, err)
		}
		path := hdr.Name
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		path = filepath.Clean(path)
		if hdr.FileInfo().IsDir() {
			if err := memFs.MkdirAll(path, hdr.FileInfo().Mode()); err != nil {
				return fmt.Errorf("mkdir %q: %w", path, err)
			}
			continue
		}
		if err := memFs.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return fmt.Errorf("mkdir %q: %w", filepath.Dir(path), err)
		}
		fileData, err := io.ReadAll(tr)
		if err != nil {
			return fmt.Errorf("read file %q from tar: %w", path, err)
		}
		if err := afero.WriteFile(memFs, path, fileData, hdr.FileInfo().Mode()); err != nil {
			return fmt.Errorf("write file %q: %w", path, err)
		}
	}

	type result struct{ err error }
	resp := make(chan result, 1)
	cmd := func() {
		userAssocs, ok := m.associations[userId]
		if !ok {
			userAssocs = make(map[string]FsAssociation)
			m.associations[userId] = userAssocs
		}
		userAssocs[uploadId] = FsAssociation{
			Id:       uuid.NewString(),
			UserId:   userId,
			UploadId: uploadId,
			Fs:       afero.NewReadOnlyFs(memFs),
		}
		resp <- result{}
	}
	if err := m.dispatch(ctx, cmd); err != nil {
		return err
	}
	select {
	case r := <-resp:
		return r.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// examDocumentPath is the path of the exam document within an association's
// virtual filesystem.
const examDocumentPath = "/exam.xml"

// defaultAssetsURLPrefix is the URL prefix that marks an asset reference inside
// an exam document. Such references are rewritten so they resolve through the
// per-upload dynamic assets endpoint.
const defaultAssetsURLPrefix = "/assets"

// DereferenceAssociation loads the exam document backing the association
// identified by associationId, owned by userId. It reads /exam.xml from the
// association's virtual filesystem and rewrites every asset URL (one beginning
// with "/assets") so that it resolves against the per-upload dynamic assets
// endpoint: /api/dyn-assets/uploads/{upload_id}/{vfs_path...}.
func (m *FsBasedAssociationManager) DereferenceAssociation(ctx context.Context, userId string, associationId string) (*pkgmodelsquestion.Exam, error) {
	fa, err := m.getAssociationById(ctx, userId, associationId)
	if err != nil {
		return nil, err
	}
	// dynAssetsPrefix already carries a trailing slash, so prefixing a "/assets"
	// URL yields "/api/dyn-assets/uploads/{upload_id}/assets/...", which ServeHTTP
	// resolves back to the vfs path "/assets/...".
	prefix := dynAssetsPrefix + fa.UploadId
	loader := NewVFSFileExamLoader(fa.Fs, NewVFSExamIDPostProcessor(fa.UploadId), NewVFSExamAssetsURLPostProcessor(prefix, defaultAssetsURLPrefix))
	return loader.LoadFrom(ctx, examDocumentPath)
}

// dataURIBase64Marker separates the scheme/header from the base64 payload in a
// data URI of the form data:<mediatype>;base64,<payload>.
const dataURIBase64Marker = ";base64,"

// associationRef is the JSON payload carried inside a LoadFrom data URI: the
// identity of one user-owned association. The receiver (typically an
// ExamRepository) does not know the requesting user, so both fields are carried
// in-band rather than derived from a session.
type associationRef struct {
	UserId        string `json:"userId"`
	AssociationId string `json:"associationId"`
}

// LoadFrom decodes url as a base64 data URI whose payload is a JSON document
// carrying a userId and associationId, then loads the exam for that association
// via DereferenceAssociation. It implements pkgmodelsquestion.ExamLoader so an
// FsBasedAssociationManager can serve as the Loader of an ExamSourceEntry.
func (m *FsBasedAssociationManager) LoadFrom(ctx context.Context, url string) (*pkgmodelsquestion.Exam, error) {
	userId, associationId, err := decodeAssociationDataURL(url)
	if err != nil {
		return nil, fmt.Errorf("fsbasedassociation: load from data url: %w", err)
	}
	return m.DereferenceAssociation(ctx, userId, associationId)
}

// decodeAssociationDataURL extracts the base64 payload from a data URI, decodes
// it, and unmarshals the resulting JSON into the userId and associationId it
// carries. It errors when the url is not a base64 data URI, the payload does not
// decode, or either identity field is empty.
func decodeAssociationDataURL(url string) (userId, associationId string, err error) {
	idx := strings.Index(url, dataURIBase64Marker)
	if idx < 0 {
		return "", "", fmt.Errorf("not a base64 data uri: missing %q", dataURIBase64Marker)
	}
	raw, err := base64.StdEncoding.DecodeString(url[idx+len(dataURIBase64Marker):])
	if err != nil {
		return "", "", fmt.Errorf("base64 decode: %w", err)
	}
	var ref associationRef
	if err := json.Unmarshal(raw, &ref); err != nil {
		return "", "", fmt.Errorf("json unmarshal: %w", err)
	}
	if ref.UserId == "" || ref.AssociationId == "" {
		return "", "", fmt.Errorf("payload missing userId or associationId")
	}
	return ref.UserId, ref.AssociationId, nil
}

// getAssociationById resolves the association identified by associationId,
// owned by userId, dispatching the map lookup to the actor goroutine. It
// returns errAssociationNotFound if no such association exists.
func (m *FsBasedAssociationManager) getAssociationById(ctx context.Context, userId, associationId string) (FsAssociation, error) {
	type result struct {
		fa  FsAssociation
		err error
	}
	resp := make(chan result, 1)
	cmd := func() {
		userAssocs, ok := m.associations[userId]
		if !ok {
			resp <- result{err: errAssociationNotFound}
			return
		}
		for _, fa := range userAssocs {
			if fa.Id == associationId {
				resp <- result{fa: fa}
				return
			}
		}
		resp <- result{err: errAssociationNotFound}
	}
	if err := m.dispatch(ctx, cmd); err != nil {
		return FsAssociation{}, err
	}
	select {
	case r := <-resp:
		return r.fa, r.err
	case <-ctx.Done():
		return FsAssociation{}, ctx.Err()
	}
}

// getFsByUploadId resolves the afero filesystem backing the association for
// the given user and upload, dispatching the map read to the actor goroutine.
func (m *FsBasedAssociationManager) getFsByUploadId(ctx context.Context, userId, uploadId string) (afero.Fs, error) {
	type result struct {
		fs  afero.Fs
		err error
	}
	resp := make(chan result, 1)
	cmd := func() {
		userAssocs, ok := m.associations[userId]
		if !ok {
			resp <- result{err: errAssociationNotFound}
			return
		}
		fa, ok := userAssocs[uploadId]
		if !ok {
			resp <- result{err: errAssociationNotFound}
			return
		}
		resp <- result{fs: fa.Fs}
	}
	if err := m.dispatch(ctx, cmd); err != nil {
		return nil, err
	}
	select {
	case r := <-resp:
		return r.fs, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// dynAssetsPrefix is the ServeMux mount point for ServeHTTP.
const dynAssetsPrefix = "/api/dyn-assets/uploads/"

// ServeHTTP serves files from the afero filesystem backing the association
// identified by the {upload_id} path parameter, scoped to the caller's session.
// The remaining path after the upload id is resolved against the virtual
// filesystem root.
func (m *FsBasedAssociationManager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	sess, ok := m.sm.GetSessionFromContext(r.Context())
	if !ok {
		http.Error(w, "session not found", http.StatusInternalServerError)
		return
	}
	userId := sess.SubjectId()
	uploadId := r.PathValue("upload_id")

	fs, err := m.getFsByUploadId(r.Context(), userId, uploadId)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	// Strip the mount prefix up to and including the upload id so the file
	// server resolves paths relative to the virtual filesystem root.
	prefix := dynAssetsPrefix + uploadId
	http.StripPrefix(prefix, http.FileServer(afero.NewHttpFs(fs))).ServeHTTP(w, r)
}


// Get always returns nil: a FsBasedAssociationManager is user-aware,
// so it exposes no system-wide entries.
func (m *FsBasedAssociationManager) Get() []pkgmodelsquestion.ExamSourceEntry {
	return nil
}

// GetByUserId returns one ExamSourceEntry per association owned by userId.
// Each entry's Loader is the manager itself (which implements ExamLoader via
// LoadFrom) and its single URL is a base64 data URI that encodes the userId and
// the association's id; LoadFrom decodes it back and dereferences the
// association. An error listing the user's associations is returned directly;
// a failure to encode a single entry's URL aborts the whole listing.
func (m *FsBasedAssociationManager) GetByUserId(ctx context.Context, userId string) ([]pkgmodelsquestion.ExamSourceEntry, error) {
	assocs, err := m.GetAssociationsByUserId(ctx, userId)
	if err != nil {
		return nil, fmt.Errorf("list associations for user %q: %w", userId, err)
	}
	entries := make([]pkgmodelsquestion.ExamSourceEntry, 0, len(assocs))
	for _, a := range assocs {
		url, err := encodeAssociationDataURL(userId, a.Id)
		if err != nil {
			return nil, fmt.Errorf("encode association %q: %w", a.Id, err)
		}
		entries = append(entries, pkgmodelsquestion.ExamSourceEntry{Loader: m, URLs: []string{url}})
	}
	return entries, nil
}

// encodeAssociationDataURL is the inverse of decodeAssociationDataURL: it
// marshals a userId and associationId as JSON, base64-encodes the result, and
// wraps it in a base64 data URI suitable for LoadFrom.
func encodeAssociationDataURL(userId, associationId string) (string, error) {
	raw, err := json.Marshal(associationRef{UserId: userId, AssociationId: associationId})
	if err != nil {
		return "", fmt.Errorf("json marshal: %w", err)
	}
	return "data:application/json" + dataURIBase64Marker + base64.StdEncoding.EncodeToString(raw), nil
}
