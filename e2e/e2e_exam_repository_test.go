package personalsite

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pkgmodelsquestion "personal-site/pkg/models/question"
	pkgmodelsuserexamdocsfsbasedassociation "personal-site/pkg/models/userexamdocs/fsbasedassociation"
	pkgmodelsuserupload "personal-site/pkg/models/userupload"
	pkgsession "personal-site/pkg/session"
)

// ---------------------------------------------------------------------------
// helpers shared by all ExamRepository e2e tests
// ---------------------------------------------------------------------------

func examXML(id, shortName, code, category string) []byte {
	if category == "" {
		category = "certification-exam"
	}
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<root>
<exam id="%s" shortname="%s" code="%s">
  <title>Title %s</title><description>desc %s</description>
  <examcategory>%s</examcategory>
  <questionset><questioncollection>
    <question id="q1" type="single-choice"><description>q</description>
      <options><option id="1">a</option></options>
      <correctanswer><options><option id="1">a</option></options></correctanswer>
    </question>
  </questioncollection></questionset>
</exam>
</root>`, id, shortName, code, id, id, category))
}

func examXMLWithAsset(id, assetSrc string) []byte {
	exhibit := ""
	if assetSrc != "" {
		exhibit = fmt.Sprintf(`<exhibits><exhibit><image src="%s"/></exhibit></exhibits>`, assetSrc)
	}
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<root>
<exam id="%s" shortname="X" code="C">
  <title>t</title><description>d</description>
  <examcategory>certification-exam</examcategory>
  <questionset><questioncollection>
    <question id="q1" type="single-choice"><description>q</description>
      %s
      <options><option id="1">a</option></options>
      <correctanswer><options><option id="1">a</option></options></correctanswer>
    </question>
  </questioncollection></questionset>
</exam>
</root>`, id, exhibit))
}

func writeTempExam(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile %q: %v", path, err)
	}
	return path
}

func buildExamTar(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, data := range files {
		hdr := &tar.Header{
			Name: strings.TrimPrefix(name, "/"),
			Mode: 0o644,
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

func newVFSManager(t *testing.T) (*pkgmodelsuserexamdocsfsbasedassociation.FsBasedAssociationManager, *pkgmodelsuserupload.OnMemoryUserUploadManager, context.Context, context.CancelFunc) {
	t.Helper()
	sm := pkgsession.NewOnMemorySessionManager()
	um := pkgmodelsuserupload.NewOnMemoryUserUploadManager()
	mgr := pkgmodelsuserexamdocsfsbasedassociation.NewFsBasedAssociationManager(um, sm)
	ctx, cancel := context.WithCancel(context.Background())
	go mgr.Run(ctx)
	t.Cleanup(func() {
		cancel()
		mgr.Shutdown()
	})
	return mgr, um, ctx, cancel
}

func uploadAndAssociate(t *testing.T, mgr *pkgmodelsuserexamdocsfsbasedassociation.FsBasedAssociationManager, um *pkgmodelsuserupload.OnMemoryUserUploadManager, userId string, tarBytes []byte) string {
	t.Helper()
	summary, err := um.CreateNewUserUpload(context.Background(), bytes.NewReader(tarBytes), userId, pkgmodelsuserupload.FileMetadata{Filename: "bundle.tar"})
	if err != nil {
		t.Fatalf("CreateNewUserUpload: %v", err)
	}
	if err := mgr.AddAssociation(context.Background(), userId, summary.UploadId); err != nil {
		t.Fatalf("AddAssociation: %v", err)
	}
	return summary.UploadId
}

func collectEvents(ch <-chan pkgmodelsquestion.ExamDataEvent) (exams []*pkgmodelsquestion.Exam, errs []error) {
	for ev := range ch {
		if ev.Err != nil {
			errs = append(errs, ev.Err)
		} else {
			exams = append(exams, ev.Data)
		}
	}
	return
}

func examIDs(exams []*pkgmodelsquestion.Exam) []string {
	ids := make([]string, len(exams))
	for i, e := range exams {
		ids[i] = e.Id
	}
	return ids
}

func containsID(exams []*pkgmodelsquestion.Exam, id string) bool {
	for _, e := range exams {
		if e.Id == id {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 1. Static file source
// ---------------------------------------------------------------------------

func TestE2E_ExamRepository_StaticFileSource(t *testing.T) {
	dir := t.TempDir()

	// Two valid exams on disk.
	p1 := writeTempExam(t, dir, "a.xml", examXML("static-1", "S1", "C1", ""))
	p2 := writeTempExam(t, dir, "b.xml", examXML("static-2", "S2", "C2", ""))

	t.Run("single entry multiple URLs", func(t *testing.T) {
		src := pkgmodelsquestion.NewStaticFileExamSource([]pkgmodelsquestion.ExamSourceEntry{
			{Loader: pkgmodelsquestion.NewFileExamLoader(), URLs: []string{p1, p2}},
		})
		repo := pkgmodelsquestion.NewExamRepository([]pkgmodelsquestion.ExamSource{src})

		exams, errs := collectEvents(repo.ListExamDocuments())
		if len(errs) != 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
		if len(exams) != 2 {
			t.Fatalf("expected 2 exams, got %d (%v)", len(exams), examIDs(exams))
		}
		if !containsID(exams, "static-1") || !containsID(exams, "static-2") {
			t.Fatalf("missing expected ids, got %v", examIDs(exams))
		}

		// GetExamDocumentById should hit the cache populated by ListExamDocuments.
		for _, want := range []string{"static-1", "static-2"} {
			got, err := repo.GetExamDocumentById(context.Background(), want, "any-user")
			if err != nil {
				t.Fatalf("GetExamDocumentById(%q): %v", want, err)
			}
			if got.Id != want {
				t.Fatalf("GetExamDocumentById(%q) id = %q, want %q", want, got.Id, want)
			}
		}

		// Unknown id must return not-found.
		if _, err := repo.GetExamDocumentById(context.Background(), "no-such", "any-user"); err == nil {
			t.Fatal("expected not-found for unknown id")
		}
	})

	t.Run("multiple entries", func(t *testing.T) {
		src := pkgmodelsquestion.NewStaticFileExamSource([]pkgmodelsquestion.ExamSourceEntry{
			{Loader: pkgmodelsquestion.NewFileExamLoader(), URLs: []string{p1}},
			{Loader: pkgmodelsquestion.NewFileExamLoader(), URLs: []string{p2}},
		})
		repo := pkgmodelsquestion.NewExamRepository([]pkgmodelsquestion.ExamSource{src})
		exams, errs := collectEvents(repo.ListExamDocuments())
		if len(errs) != 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
		if len(exams) != 2 {
			t.Fatalf("expected 2, got %d", len(exams))
		}
	})

	t.Run("invalid file emits error event", func(t *testing.T) {
		badPath := writeTempExam(t, dir, "bad.xml", []byte("not xml at all"))
		src := pkgmodelsquestion.NewStaticFileExamSource([]pkgmodelsquestion.ExamSourceEntry{
			{Loader: pkgmodelsquestion.NewFileExamLoader(), URLs: []string{p1, badPath}},
		})
		repo := pkgmodelsquestion.NewExamRepository([]pkgmodelsquestion.ExamSource{src})
		exams, errs := collectEvents(repo.ListExamDocuments())
		if len(exams) != 1 || exams[0].Id != "static-1" {
			t.Fatalf("expected 1 good exam, got %v errs %v", examIDs(exams), errs)
		}
		if len(errs) != 1 {
			t.Fatalf("expected 1 error event, got %d", len(errs))
		}
		if !strings.Contains(errs[0].Error(), "bad.xml") {
			t.Fatalf("error should mention bad file, got %v", errs[0])
		}
	})

	t.Run("GetByUserId returns nothing for static source", func(t *testing.T) {
		src := pkgmodelsquestion.NewStaticFileExamSource([]pkgmodelsquestion.ExamSourceEntry{
			{Loader: pkgmodelsquestion.NewFileExamLoader(), URLs: []string{p1}},
		})
		repo := pkgmodelsquestion.NewExamRepository([]pkgmodelsquestion.ExamSource{src})
		exams, errs := collectEvents(repo.ListExamDocumentsByUserId(context.Background(), "alice"))
		if len(exams) != 0 || len(errs) != 0 {
			t.Fatalf("static source should expose no per-user exams, got exams %v errs %v", examIDs(exams), errs)
		}
	})

	t.Run("reload on cache miss", func(t *testing.T) {
		// Do not call ListExamDocuments first; GetExamDocumentById must trigger reload.
		src := pkgmodelsquestion.NewStaticFileExamSource([]pkgmodelsquestion.ExamSourceEntry{
			{Loader: pkgmodelsquestion.NewFileExamLoader(), URLs: []string{p1}},
		})
		repo := pkgmodelsquestion.NewExamRepository([]pkgmodelsquestion.ExamSource{src})
		got, err := repo.GetExamDocumentById(context.Background(), "static-1", "u")
		if err != nil {
			t.Fatalf("GetExamDocumentById: %v", err)
		}
		if got.Id != "static-1" {
			t.Fatalf("id = %q, want static-1", got.Id)
		}
	})
}

// ---------------------------------------------------------------------------
// 2. Dynamic directory source
// ---------------------------------------------------------------------------

func TestE2E_ExamRepository_DynamicDirSource(t *testing.T) {
	t.Run("discovers xml files and ignores others", func(t *testing.T) {
		dir := t.TempDir()
		writeTempExam(t, dir, "one.xml", examXML("dir-1", "D1", "C1", ""))
		writeTempExam(t, dir, "two.xml", examXML("dir-2", "D2", "C2", ""))
		if err := os.WriteFile(filepath.Join(dir, "README.txt"), []byte("ignore"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
			t.Fatal(err)
		}

		src := pkgmodelsquestion.NewDynamicDirExamSource(dir)
		repo := pkgmodelsquestion.NewExamRepository([]pkgmodelsquestion.ExamSource{src})

		exams, errs := collectEvents(repo.ListExamDocuments())
		if len(errs) != 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
		if len(exams) != 2 {
			t.Fatalf("expected 2, got %d (%v)", len(exams), examIDs(exams))
		}
		if !containsID(exams, "dir-1") || !containsID(exams, "dir-2") {
			t.Fatalf("missing ids, got %v", examIDs(exams))
		}
	})

	t.Run("empty dir yields no exams", func(t *testing.T) {
		dir := t.TempDir()
		src := pkgmodelsquestion.NewDynamicDirExamSource(dir)
		repo := pkgmodelsquestion.NewExamRepository([]pkgmodelsquestion.ExamSource{src})
		exams, errs := collectEvents(repo.ListExamDocuments())
		if len(exams) != 0 || len(errs) != 0 {
			t.Fatalf("expected no exams/errors for empty dir, got %v / %v", examIDs(exams), errs)
		}
		if _, err := repo.GetExamDocumentById(context.Background(), "any", "u"); err == nil {
			t.Fatal("expected not-found for empty dir")
		}
	})

	t.Run("missing dir yields no exams", func(t *testing.T) {
		src := pkgmodelsquestion.NewDynamicDirExamSource(filepath.Join(t.TempDir(), "no-such"))
		repo := pkgmodelsquestion.NewExamRepository([]pkgmodelsquestion.ExamSource{src})
		exams, errs := collectEvents(repo.ListExamDocuments())
		if len(exams) != 0 || len(errs) != 0 {
			t.Fatalf("expected no exams/errors for missing dir, got %v / %v", examIDs(exams), errs)
		}
	})

	t.Run("invalid xml in dir emits error", func(t *testing.T) {
		dir := t.TempDir()
		writeTempExam(t, dir, "good.xml", examXML("good-1", "G", "C", ""))
		writeTempExam(t, dir, "bad.xml", []byte("<root><exam id=\"bad\">broken"))
		src := pkgmodelsquestion.NewDynamicDirExamSource(dir)
		repo := pkgmodelsquestion.NewExamRepository([]pkgmodelsquestion.ExamSource{src})
		exams, errs := collectEvents(repo.ListExamDocuments())
		if len(exams) != 1 || exams[0].Id != "good-1" {
			t.Fatalf("expected 1 good exam, got %v", examIDs(exams))
		}
		if len(errs) != 1 {
			t.Fatalf("expected 1 error, got %d", len(errs))
		}
	})

	t.Run("GetByUserId returns nothing for dir source", func(t *testing.T) {
		dir := t.TempDir()
		writeTempExam(t, dir, "a.xml", examXML("x", "X", "C", ""))
		src := pkgmodelsquestion.NewDynamicDirExamSource(dir)
		repo := pkgmodelsquestion.NewExamRepository([]pkgmodelsquestion.ExamSource{src})
		exams, errs := collectEvents(repo.ListExamDocumentsByUserId(context.Background(), "alice"))
		if len(exams) != 0 || len(errs) != 0 {
			t.Fatalf("dir source should expose no per-user exams, got %v / %v", examIDs(exams), errs)
		}
	})

	t.Run("dynamically added file is picked up", func(t *testing.T) {
		dir := t.TempDir()
		src := pkgmodelsquestion.NewDynamicDirExamSource(dir)
		repo := pkgmodelsquestion.NewExamRepository([]pkgmodelsquestion.ExamSource{src})

		// Initially empty.
		exams, _ := collectEvents(repo.ListExamDocuments())
		if len(exams) != 0 {
			t.Fatalf("expected 0 initially, got %d", len(exams))
		}

		// Add a file after repo construction; next List should see it (dir is re-scanned).
		writeTempExam(t, dir, "late.xml", examXML("late-1", "L", "C", ""))
		exams, errs := collectEvents(repo.ListExamDocuments())
		if len(errs) != 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
		if len(exams) != 1 || exams[0].Id != "late-1" {
			t.Fatalf("expected late-1, got %v", examIDs(exams))
		}

		// GetExamDocumentById should also find it via reload.
		got, err := repo.GetExamDocumentById(context.Background(), "late-1", "u")
		if err != nil {
			t.Fatalf("GetExamDocumentById: %v", err)
		}
		if got.Id != "late-1" {
			t.Fatalf("id = %q, want late-1", got.Id)
		}
	})
}

// ---------------------------------------------------------------------------
// 3. User-associated VFS source (FsBasedAssociationManager)
// ---------------------------------------------------------------------------

func TestE2E_ExamRepository_UserVFSSource(t *testing.T) {
	t.Run("single user single exam", func(t *testing.T) {
		mgr, um, _, _ := newVFSManager(t)
		tarBytes := buildExamTar(t, map[string][]byte{
			"exam.xml": examXML("vfs-1", "V1", "C1", ""),
		})
		uploadId := uploadAndAssociate(t, mgr, um, "alice", tarBytes)

		repo := pkgmodelsquestion.NewExamRepository([]pkgmodelsquestion.ExamSource{mgr})

		// System-wide listing must be empty (VFS is user-scoped).
		exams, errs := collectEvents(repo.ListExamDocuments())
		if len(exams) != 0 || len(errs) != 0 {
			t.Fatalf("system listing should be empty for VFS-only repo, got %v / %v", examIDs(exams), errs)
		}

		// Per-user listing for alice should return the exam with prefixed id.
		exams, errs = collectEvents(repo.ListExamDocumentsByUserId(context.Background(), "alice"))
		if len(errs) != 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
		if len(exams) != 1 {
			t.Fatalf("expected 1 per-user exam, got %d", len(exams))
		}
		wantId := "/uploads/" + uploadId + "/vfs-1"
		if exams[0].Id != wantId {
			t.Fatalf("per-user exam id = %q, want %q", exams[0].Id, wantId)
		}

		// GetExamDocumentById for alice should find it.
		got, err := repo.GetExamDocumentById(context.Background(), wantId, "alice")
		if err != nil {
			t.Fatalf("GetExamDocumentById: %v", err)
		}
		if got.Id != wantId {
			t.Fatalf("id = %q, want %q", got.Id, wantId)
		}

		// Other user must not see it.
		exams, _ = collectEvents(repo.ListExamDocumentsByUserId(context.Background(), "bob"))
		if len(exams) != 0 {
			t.Fatalf("bob should see 0 exams, got %v", examIDs(exams))
		}
		if _, err := repo.GetExamDocumentById(context.Background(), wantId, "bob"); err == nil {
			t.Fatal("bob should not find alice's exam")
		}
	})

	t.Run("multiple exams for same user", func(t *testing.T) {
		mgr, um, _, _ := newVFSManager(t)
		for i := 0; i < 3; i++ {
			tarBytes := buildExamTar(t, map[string][]byte{
				"exam.xml": examXML(fmt.Sprintf("multi-%d", i), "M", "C", ""),
			})
			uploadAndAssociate(t, mgr, um, "alice", tarBytes)
		}
		repo := pkgmodelsquestion.NewExamRepository([]pkgmodelsquestion.ExamSource{mgr})
		exams, errs := collectEvents(repo.ListExamDocumentsByUserId(context.Background(), "alice"))
		if len(errs) != 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
		if len(exams) != 3 {
			t.Fatalf("expected 3, got %d (%v)", len(exams), examIDs(exams))
		}
	})

	t.Run("user isolation", func(t *testing.T) {
		mgr, um, _, _ := newVFSManager(t)
		tarA := buildExamTar(t, map[string][]byte{"exam.xml": examXML("alice-exam", "A", "C", "")})
		tarB := buildExamTar(t, map[string][]byte{"exam.xml": examXML("bob-exam", "B", "C", "")})
		uploadA := uploadAndAssociate(t, mgr, um, "alice", tarA)
		uploadB := uploadAndAssociate(t, mgr, um, "bob", tarB)

		repo := pkgmodelsquestion.NewExamRepository([]pkgmodelsquestion.ExamSource{mgr})

		aliceExams, _ := collectEvents(repo.ListExamDocumentsByUserId(context.Background(), "alice"))
		bobExams, _ := collectEvents(repo.ListExamDocumentsByUserId(context.Background(), "bob"))

		if len(aliceExams) != 1 || aliceExams[0].Id != "/uploads/"+uploadA+"/alice-exam" {
			t.Fatalf("alice exams: %v", examIDs(aliceExams))
		}
		if len(bobExams) != 1 || bobExams[0].Id != "/uploads/"+uploadB+"/bob-exam" {
			t.Fatalf("bob exams: %v", examIDs(bobExams))
		}

		// Cross-user Get must fail.
		if _, err := repo.GetExamDocumentById(context.Background(), aliceExams[0].Id, "bob"); err == nil {
			t.Fatal("bob should not find alice's exam via GetExamDocumentById")
		}
		if _, err := repo.GetExamDocumentById(context.Background(), bobExams[0].Id, "alice"); err == nil {
			t.Fatal("alice should not find bob's exam via GetExamDocumentById")
		}
	})

	t.Run("asset URL rewriting and id prefixing", func(t *testing.T) {
		mgr, um, _, _ := newVFSManager(t)
		tarBytes := buildExamTar(t, map[string][]byte{
			"exam.xml": examXMLWithAsset("asset-exam", "/assets/img.png"),
			"assets/img.png": []byte("fake png"),
		})
		uploadId := uploadAndAssociate(t, mgr, um, "alice", tarBytes)
		repo := pkgmodelsquestion.NewExamRepository([]pkgmodelsquestion.ExamSource{mgr})

		exams, _ := collectEvents(repo.ListExamDocumentsByUserId(context.Background(), "alice"))
		if len(exams) != 1 {
			t.Fatalf("expected 1, got %d", len(exams))
		}
		exam := exams[0]
		if exam.Id != "/uploads/"+uploadId+"/asset-exam" {
			t.Fatalf("id = %q, want prefixed", exam.Id)
		}
		// The exhibit image src should have been rewritten to /api/dyn-assets/uploads/{uploadId}/assets/...
		src := exam.QuestionSet.QuestionCollections[0].Questions[0].Exhibits[0].Image.Src
		wantPrefix := "/api/dyn-assets/uploads/" + uploadId + "/assets"
		if !strings.HasPrefix(src, wantPrefix) {
			t.Fatalf("asset src = %q, want prefix %q", src, wantPrefix)
		}

		// Same via GetExamDocumentById.
		got, err := repo.GetExamDocumentById(context.Background(), exam.Id, "alice")
		if err != nil {
			t.Fatalf("GetExamDocumentById: %v", err)
		}
		gotSrc := got.QuestionSet.QuestionCollections[0].Questions[0].Exhibits[0].Image.Src
		if gotSrc != src {
			t.Fatalf("GetExamDocumentById asset src = %q, want %q", gotSrc, src)
		}
	})

	t.Run("no associations yields empty per-user listing", func(t *testing.T) {
		mgr, _, _, _ := newVFSManager(t)
		repo := pkgmodelsquestion.NewExamRepository([]pkgmodelsquestion.ExamSource{mgr})
		exams, errs := collectEvents(repo.ListExamDocumentsByUserId(context.Background(), "nobody"))
		if len(exams) != 0 || len(errs) != 0 {
			t.Fatalf("expected empty, got %v / %v", examIDs(exams), errs)
		}
	})
}

// ---------------------------------------------------------------------------
// 4. Combined sources (mirrors main.go wiring)
// ---------------------------------------------------------------------------

func TestE2E_ExamRepository_CombinedSources(t *testing.T) {
	// System sources: one static file + one dynamic directory.
	sysDir := t.TempDir()
	staticDir := t.TempDir()
	staticPath := writeTempExam(t, staticDir, "static.xml", examXML("sys-static", "SS", "C1", ""))
	writeTempExam(t, sysDir, "dir.xml", examXML("sys-dir", "SD", "C2", ""))

	staticSrc := pkgmodelsquestion.NewStaticFileExamSource([]pkgmodelsquestion.ExamSourceEntry{
		{Loader: pkgmodelsquestion.NewFileExamLoader(), URLs: []string{staticPath}},
	})
	dirSrc := pkgmodelsquestion.NewDynamicDirExamSource(sysDir)

	// User VFS source.
	mgr, um, _, _ := newVFSManager(t)
	tarBytes := buildExamTar(t, map[string][]byte{
		"exam.xml": examXML("user-exam", "UE", "C3", ""),
	})
	uploadId := uploadAndAssociate(t, mgr, um, "alice", tarBytes)
	userExamId := "/uploads/" + uploadId + "/user-exam"

	// Wire exactly like main.go: associationManager first, then static, then dir.
	repo := pkgmodelsquestion.NewExamRepository([]pkgmodelsquestion.ExamSource{mgr, staticSrc, dirSrc})

	t.Run("ListExamDocuments returns only system exams", func(t *testing.T) {
		exams, errs := collectEvents(repo.ListExamDocuments())
		if len(errs) != 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
		if len(exams) != 2 {
			t.Fatalf("expected 2 system exams, got %d (%v)", len(exams), examIDs(exams))
		}
		if !containsID(exams, "sys-static") || !containsID(exams, "sys-dir") {
			t.Fatalf("missing system ids, got %v", examIDs(exams))
		}
		if containsID(exams, userExamId) {
			t.Fatalf("system listing should not contain user exam %q", userExamId)
		}
	})

	t.Run("ListExamDocumentsByUserId returns only user exams", func(t *testing.T) {
		exams, errs := collectEvents(repo.ListExamDocumentsByUserId(context.Background(), "alice"))
		if len(errs) != 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
		if len(exams) != 1 || exams[0].Id != userExamId {
			t.Fatalf("expected [%q], got %v", userExamId, examIDs(exams))
		}

		// Bob has no user exams.
		exams, _ = collectEvents(repo.ListExamDocumentsByUserId(context.Background(), "bob"))
		if len(exams) != 0 {
			t.Fatalf("bob should see 0, got %v", examIDs(exams))
		}
	})

	t.Run("GetExamDocumentById user precedence over system", func(t *testing.T) {
		// Alice can fetch both system and user exams.
		for _, id := range []string{"sys-static", "sys-dir", userExamId} {
			got, err := repo.GetExamDocumentById(context.Background(), id, "alice")
			if err != nil {
				t.Fatalf("GetExamDocumentById(%q) for alice: %v", id, err)
			}
			if got.Id != id {
				t.Fatalf("id = %q, want %q", got.Id, id)
			}
		}
		// Bob can fetch system exams but not alice's user exam.
		for _, id := range []string{"sys-static", "sys-dir"} {
			if _, err := repo.GetExamDocumentById(context.Background(), id, "bob"); err != nil {
				t.Fatalf("GetExamDocumentById(%q) for bob: %v", id, err)
			}
		}
		if _, err := repo.GetExamDocumentById(context.Background(), userExamId, "bob"); err == nil {
			t.Fatal("bob should not find alice's user exam")
		}
	})

	t.Run("user exam shadows system exam with same raw id", func(t *testing.T) {
		// Create a user exam whose raw id collides with a system exam id.
		// The user exam's stored id is prefixed, so there is no actual collision,
		// but we verify that a lookup for the system id still returns the system exam
		// and a lookup for the prefixed id returns the user exam.
		tarBytes2 := buildExamTar(t, map[string][]byte{
			"exam.xml": examXML("sys-static", "SS2", "C9", ""),
		})
		uploadId2 := uploadAndAssociate(t, mgr, um, "alice", tarBytes2)
		shadowId := "/uploads/" + uploadId2 + "/sys-static"

		// System id lookup returns system exam (shortname SS, not SS2).
		got, err := repo.GetExamDocumentById(context.Background(), "sys-static", "alice")
		if err != nil {
			t.Fatalf("GetExamDocumentById sys-static: %v", err)
		}
		if got.ShortName != "SS" {
			t.Fatalf("expected system exam SS, got %q", got.ShortName)
		}

		// Prefixed id lookup returns user exam.
		got, err = repo.GetExamDocumentById(context.Background(), shadowId, "alice")
		if err != nil {
			t.Fatalf("GetExamDocumentById %q: %v", shadowId, err)
		}
		if got.ShortName != "SS2" {
			t.Fatalf("expected user exam SS2, got %q", got.ShortName)
		}
	})

	t.Run("not found", func(t *testing.T) {
		if _, err := repo.GetExamDocumentById(context.Background(), "does-not-exist", "alice"); err == nil {
			t.Fatal("expected not-found")
		}
		if !strings.Contains(mustErr(repo, "does-not-exist", "alice"), "not found") {
			t.Fatal("error should mention not found")
		}
	})
}

func mustErr(repo *pkgmodelsquestion.ExamRepository, id, userId string) string {
	_, err := repo.GetExamDocumentById(context.Background(), id, userId)
	if err == nil {
		return ""
	}
	return err.Error()
}

// ---------------------------------------------------------------------------
// 5. Cache behavior and error resilience
// ---------------------------------------------------------------------------

func TestE2E_ExamRepository_CacheAndErrorResilience(t *testing.T) {
	t.Run("cache populated by ListExamDocuments serves Get without reload", func(t *testing.T) {
		dir := t.TempDir()
		p := writeTempExam(t, dir, "a.xml", examXML("cached-1", "C1", "C1", ""))
		src := pkgmodelsquestion.NewStaticFileExamSource([]pkgmodelsquestion.ExamSourceEntry{
			{Loader: pkgmodelsquestion.NewFileExamLoader(), URLs: []string{p}},
		})
		repo := pkgmodelsquestion.NewExamRepository([]pkgmodelsquestion.ExamSource{src})

		// Populate cache.
		exams, _ := collectEvents(repo.ListExamDocuments())
		if len(exams) != 1 {
			t.Fatalf("expected 1, got %d", len(exams))
		}

		// Remove the file; Get should still succeed from cache.
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		got, err := repo.GetExamDocumentById(context.Background(), "cached-1", "u")
		if err != nil {
			t.Fatalf("GetExamDocumentById from cache: %v", err)
		}
		if got.Id != "cached-1" {
			t.Fatalf("id = %q, want cached-1", got.Id)
		}
	})

	t.Run("mixed valid and invalid across multiple sources", func(t *testing.T) {
		dir := t.TempDir()
		goodDir := t.TempDir()
		writeTempExam(t, goodDir, "good.xml", examXML("good-dir", "G", "C", ""))
		badPath := writeTempExam(t, dir, "bad.xml", []byte("bad xml"))
		goodPath := writeTempExam(t, dir, "good.xml", examXML("good-static", "GS", "C", ""))

		staticSrc := pkgmodelsquestion.NewStaticFileExamSource([]pkgmodelsquestion.ExamSourceEntry{
			{Loader: pkgmodelsquestion.NewFileExamLoader(), URLs: []string{goodPath, badPath}},
		})
		dirSrc := pkgmodelsquestion.NewDynamicDirExamSource(goodDir)
		repo := pkgmodelsquestion.NewExamRepository([]pkgmodelsquestion.ExamSource{staticSrc, dirSrc})

		exams, errs := collectEvents(repo.ListExamDocuments())
		if len(exams) != 2 {
			t.Fatalf("expected 2 good exams, got %d (%v)", len(exams), examIDs(exams))
		}
		if len(errs) != 1 {
			t.Fatalf("expected 1 error, got %d", len(errs))
		}
		if !containsID(exams, "good-static") || !containsID(exams, "good-dir") {
			t.Fatalf("missing good ids, got %v", examIDs(exams))
		}
	})

	t.Run("empty repository", func(t *testing.T) {
		repo := pkgmodelsquestion.NewExamRepository(nil)
		exams, errs := collectEvents(repo.ListExamDocuments())
		if len(exams) != 0 || len(errs) != 0 {
			t.Fatalf("expected empty, got %v / %v", examIDs(exams), errs)
		}
		if _, err := repo.GetExamDocumentById(context.Background(), "any", "u"); err == nil {
			t.Fatal("expected not-found for empty repo")
		}
		exams, errs = collectEvents(repo.ListExamDocumentsByUserId(context.Background(), "u"))
		if len(exams) != 0 || len(errs) != 0 {
			t.Fatalf("expected empty per-user for empty repo, got %v / %v", examIDs(exams), errs)
		}
	})
}

// ---------------------------------------------------------------------------
// 6. Real repo files (exam1.xml / exam2.xml) as static sources
// ---------------------------------------------------------------------------

func TestE2E_ExamRepository_RealFiles(t *testing.T) {
	// Use the actual exam files at the repo root to ensure the loader handles
	// the full production document structure.
	repo := pkgmodelsquestion.NewExamRepository([]pkgmodelsquestion.ExamSource{
		pkgmodelsquestion.NewStaticFileExamSource([]pkgmodelsquestion.ExamSourceEntry{
			{Loader: pkgmodelsquestion.NewFileExamLoader(), URLs: []string{
				filepath.Join("..", "exam1.xml"),
				filepath.Join("..", "exam2.xml"),
			}},
		}),
	})

	exams, errs := collectEvents(repo.ListExamDocuments())
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(exams) != 2 {
		t.Fatalf("expected 2 real exams, got %d", len(exams))
	}
	for _, e := range exams {
		if e.Id == "" || e.Title == "" {
			t.Fatalf("exam missing id/title: %+v", e)
		}
		if len(e.QuestionSet.QuestionCollections) == 0 {
			t.Fatalf("exam %q has no question collections", e.Id)
		}
		t.Logf("  real exam: id=%q shortName=%q code=%q title=%q collections=%d", e.Id, e.ShortName, e.Code, e.Title, len(e.QuestionSet.QuestionCollections))
	}

	// Each real exam must be fetchable by id.
	for _, e := range exams {
		got, err := repo.GetExamDocumentById(context.Background(), e.Id, "any-user")
		if err != nil {
			t.Fatalf("GetExamDocumentById(%q): %v", e.Id, err)
		}
		if got.Id != e.Id {
			t.Fatalf("id mismatch: got %q want %q", got.Id, e.Id)
		}
	}
}
