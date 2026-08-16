package fsbasedassociation

import (
	"context"
	"testing"

	pkgmodelsquestion "personal-site/pkg/models/question"

	"github.com/spf13/afero"
)

// compile-time assertions that post-processors satisfy the interface.
var _ pkgmodelsquestion.ExamDocumentPostProcessor = (*VFSExamAssetsURLPostProcessor)(nil)
var _ pkgmodelsquestion.ExamDocumentPostProcessor = (*VFSExamIDPostProcessor)(nil)

// newExam builds an Exam exercising every asset URL-bearing field so the
// rewriter has something to chew on.
func newExam() *pkgmodelsquestion.Exam {
	score := float32(80)
	return &pkgmodelsquestion.Exam{
		Id:           "exam-1",
		PassingScore: &score,
		QuestionSet: pkgmodelsquestion.QuestionSet{
			QuestionCollections: []pkgmodelsquestion.QuestionCollection{
				{
					Questions: []pkgmodelsquestion.Question{
						{
							Id: "q1",
							Exhibits: pkgmodelsquestion.Exhibits{
								{Image: pkgmodelsquestion.Image{Src: "/assets/exhibit1.png"}},
								{Image: pkgmodelsquestion.Image{Src: "https://example.com/external.png"}},
							},
							ImgDragAndDrop: &pkgmodelsquestion.ImgDragAndDrop{
								ImgCandidates: []pkgmodelsquestion.ImgCandidate{
									{ImgDataSrc: "/assets/cand1.png"},
									{ImgDataSrc: "/assets/cand2.png"},
									{ImgDataSrc: "/other/cand3.png"},
								},
								ImgDropsArea: pkgmodelsquestion.ImgDropsArea{
									ImgBackgroundUrl: "/assets/bg.png",
								},
							},
						},
						// A question with no asset URLs must pass through untouched.
						{
							Id: "q2",
							Exhibits: pkgmodelsquestion.Exhibits{
								{Image: pkgmodelsquestion.Image{Src: "relative/path.png"}},
							},
						},
					},
				},
			},
		},
	}
}

func TestVFSExamAssetsURLPostProcessor_RewritesAssetURLs(t *testing.T) {
	p := NewVFSExamAssetsURLPostProcessor("/api/dyn-assets/uploads/abc", "/assets")
	got, err := p.PostProcess(newExam())
	if err != nil {
		t.Fatalf("PostProcess: unexpected error: %v", err)
	}

	q0 := got.QuestionSet.QuestionCollections[0].Questions[0]

	wantExhibits := []string{
		"/api/dyn-assets/uploads/abc/assets/exhibit1.png",
		"https://example.com/external.png", // non-/assets URL unchanged
	}
	for i, want := range wantExhibits {
		if got := q0.Exhibits[i].Image.Src; got != want {
			t.Errorf("Exhibits[%d].Image.Src = %q, want %q", i, got, want)
		}
	}

	wantCands := []string{
		"/api/dyn-assets/uploads/abc/assets/cand1.png",
		"/api/dyn-assets/uploads/abc/assets/cand2.png",
		"/other/cand3.png", // non-/assets URL unchanged
	}
	for i, want := range wantCands {
		if got := q0.ImgDragAndDrop.ImgCandidates[i].ImgDataSrc; got != want {
			t.Errorf("ImgCandidates[%d].ImgDataSrc = %q, want %q", i, got, want)
		}
	}

	if want := "/api/dyn-assets/uploads/abc/assets/bg.png"; q0.ImgDragAndDrop.ImgDropsArea.ImgBackgroundUrl != want {
		t.Errorf("ImgDropsArea.ImgBackgroundUrl = %q, want %q",
			q0.ImgDragAndDrop.ImgDropsArea.ImgBackgroundUrl, want)
	}

	// The asset-less question is untouched.
	q1 := got.QuestionSet.QuestionCollections[0].Questions[1]
	if got, want := q1.Exhibits[0].Image.Src, "relative/path.png"; got != want {
		t.Errorf("q2 Exhibit Src = %q, want %q", got, want)
	}
}

func TestVFSExamAssetsURLPostProcessor_DeepCopyDoesNotMutateOriginal(t *testing.T) {
	original := newExam()
	p := NewVFSExamAssetsURLPostProcessor("/prefix", "/assets")

	cp, err := p.PostProcess(original)
	if err != nil {
		t.Fatalf("PostProcess: unexpected error: %v", err)
	}
	if cp == original {
		t.Fatal("PostProcess returned the same pointer; expected a deep copy")
	}

	// The original's URLs must remain unchanged after post-processing.
	origQ0 := original.QuestionSet.QuestionCollections[0].Questions[0]
	if got, want := origQ0.Exhibits[0].Image.Src, "/assets/exhibit1.png"; got != want {
		t.Errorf("original Exhibits[0].Image.Src mutated to %q, want %q", got, want)
	}
	if got, want := origQ0.ImgDragAndDrop.ImgCandidates[0].ImgDataSrc, "/assets/cand1.png"; got != want {
		t.Errorf("original ImgCandidates[0].ImgDataSrc mutated to %q, want %q", got, want)
	}
	if got, want := origQ0.ImgDragAndDrop.ImgDropsArea.ImgBackgroundUrl, "/assets/bg.png"; got != want {
		t.Errorf("original ImgDropsArea.ImgBackgroundUrl mutated to %q, want %q", got, want)
	}

	// The PassingScore pointer must be duplicated, not shared.
	cp.PassingScore = nil
	if original.PassingScore == nil {
		t.Fatal("original PassingScore pointer was shared with the copy")
	}
	if *original.PassingScore != 80 {
		t.Errorf("original PassingScore = %v, want 80", *original.PassingScore)
	}

	// Slices must be independent: appending to a copy's slice must not grow the
	// original's.
	cp.QuestionSet.QuestionCollections[0].Questions[0].Exhibits = append(
		cp.QuestionSet.QuestionCollections[0].Questions[0].Exhibits, pkgmodelsquestion.Exhibit{})
	if got := len(original.QuestionSet.QuestionCollections[0].Questions[0].Exhibits); got != 2 {
		t.Errorf("original Exhibits grew to %d after mutating copy; want 2 (slices not deep-copied)", got)
	}
}

func TestVFSExamAssetsURLPostProcessor_CustomAssetsURLPrefix(t *testing.T) {
	// A non-default asset prefix should only rewrite URLs matching it.
	exam := &pkgmodelsquestion.Exam{
		QuestionSet: pkgmodelsquestion.QuestionSet{
			QuestionCollections: []pkgmodelsquestion.QuestionCollection{{
				Questions: []pkgmodelsquestion.Question{{
					Exhibits: pkgmodelsquestion.Exhibits{
						{Image: pkgmodelsquestion.Image{Src: "/static/a.png"}},
						{Image: pkgmodelsquestion.Image{Src: "/assets/b.png"}},
					},
				}},
			}},
		},
	}

	p := NewVFSExamAssetsURLPostProcessor("/mnt", "/static")
	got, err := p.PostProcess(exam)
	if err != nil {
		t.Fatalf("PostProcess: unexpected error: %v", err)
	}
	if got, want := got.QuestionSet.QuestionCollections[0].Questions[0].Exhibits[0].Image.Src, "/mnt/static/a.png"; got != want {
		t.Errorf("Exhibits[0].Image.Src = %q, want %q", got, want)
	}
	// /assets/b.png must NOT be rewritten because the asset prefix is /static.
	if got, want := got.QuestionSet.QuestionCollections[0].Questions[0].Exhibits[1].Image.Src, "/assets/b.png"; got != want {
		t.Errorf("Exhibits[1].Image.Src = %q, want %q", got, want)
	}
}

func TestVFSExamAssetsURLPostProcessor_NilExam(t *testing.T) {
	p := NewVFSExamAssetsURLPostProcessor("/prefix", "/assets")
	got, err := p.PostProcess(nil)
	if err != nil {
		t.Fatalf("PostProcess(nil): unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("PostProcess(nil) = %v, want nil", got)
	}
}

func TestVFSExamAssetsURLPostProcessor_PrefixOnlyMatching(t *testing.T) {
	// Only the exact asset prefix at the start of the URL should be rewritten;
	// URLs that merely contain the prefix substring must be left alone.
	exam := &pkgmodelsquestion.Exam{
		QuestionSet: pkgmodelsquestion.QuestionSet{
			QuestionCollections: []pkgmodelsquestion.QuestionCollection{{
				Questions: []pkgmodelsquestion.Question{{
					Exhibits: pkgmodelsquestion.Exhibits{
						{Image: pkgmodelsquestion.Image{Src: "/assets/sub/x.png"}},  // matches
						{Image: pkgmodelsquestion.Image{Src: "x/assets/y.png"}},    // leading text, no match
						{Image: pkgmodelsquestion.Image{Src: "/my-assets/z.png"}},  // different prefix, no match
					},
				}},
			}},
		},
	}

	p := NewVFSExamAssetsURLPostProcessor("/p", "/assets")
	got, err := p.PostProcess(exam)
	if err != nil {
		t.Fatalf("PostProcess: unexpected error: %v", err)
	}
	srcs := []string{
		got.QuestionSet.QuestionCollections[0].Questions[0].Exhibits[0].Image.Src,
		got.QuestionSet.QuestionCollections[0].Questions[0].Exhibits[1].Image.Src,
		got.QuestionSet.QuestionCollections[0].Questions[0].Exhibits[2].Image.Src,
	}
	want := []string{"/p/assets/sub/x.png", "x/assets/y.png", "/my-assets/z.png"}
	for i := range want {
		if srcs[i] != want[i] {
			t.Errorf("Exhibits[%d].Image.Src = %q, want %q", i, srcs[i], want[i])
		}
	}
}

func TestVFSExamIDPostProcessor_PrependsID(t *testing.T) {
	p := NewVFSExamIDPostProcessor("abc123")
	got, err := p.PostProcess(newExam())
	if err != nil {
		t.Fatalf("PostProcess: unexpected error: %v", err)
	}
	want := "/uploads/abc123/exam-1"
	if got.Id != want {
		t.Errorf("Id = %q, want %q", got.Id, want)
	}
}

func TestVFSExamIDPostProcessor_DeepCopyDoesNotMutateOriginal(t *testing.T) {
	original := newExam()
	p := NewVFSExamIDPostProcessor("upload-xyz")

	cp, err := p.PostProcess(original)
	if err != nil {
		t.Fatalf("PostProcess: unexpected error: %v", err)
	}
	if cp == original {
		t.Fatal("PostProcess returned the same pointer; expected a deep copy")
	}
	if got, want := original.Id, "exam-1"; got != want {
		t.Errorf("original Id mutated to %q, want %q", got, want)
	}
	want := "/uploads/upload-xyz/exam-1"
	if cp.Id != want {
		t.Errorf("copy Id = %q, want %q", cp.Id, want)
	}
	// Mutating the copy must not affect the original's deep-copied fields.
	cp.QuestionSet.QuestionCollections[0].Questions[0].Exhibits = append(
		cp.QuestionSet.QuestionCollections[0].Questions[0].Exhibits, pkgmodelsquestion.Exhibit{})
	if got := len(original.QuestionSet.QuestionCollections[0].Questions[0].Exhibits); got != 2 {
		t.Errorf("original Exhibits grew to %d after mutating copy; want 2", got)
	}
	if original.PassingScore == nil || *original.PassingScore != 80 {
		t.Errorf("original PassingScore mutated: %v", original.PassingScore)
	}
	cp.PassingScore = nil
	if original.PassingScore == nil {
		t.Fatal("original PassingScore pointer was shared with the copy")
	}
}

func TestVFSExamIDPostProcessor_NilExam(t *testing.T) {
	p := NewVFSExamIDPostProcessor("any")
	got, err := p.PostProcess(nil)
	if err != nil {
		t.Fatalf("PostProcess(nil): unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("PostProcess(nil) = %v, want nil", got)
	}
}

func TestVFSExamIDPostProcessor_PreservesOtherFields(t *testing.T) {
	p := NewVFSExamIDPostProcessor("u1")
	orig := newExam()
	got, err := p.PostProcess(orig)
	if err != nil {
		t.Fatalf("PostProcess: unexpected error: %v", err)
	}
	// Only Id should change; other fields must be preserved.
	if got.ShortName != orig.ShortName || got.Code != orig.Code {
		t.Errorf("other top-level fields mutated: got ShortName=%q Code=%q, want %q %q", got.ShortName, got.Code, orig.ShortName, orig.Code)
	}
	if len(got.QuestionSet.QuestionCollections) != len(orig.QuestionSet.QuestionCollections) {
		t.Fatalf("QuestionCollections length changed: got %d, want %d", len(got.QuestionSet.QuestionCollections), len(orig.QuestionSet.QuestionCollections))
	}
	// Asset URLs must remain untouched by the ID processor.
	origSrc := orig.QuestionSet.QuestionCollections[0].Questions[0].Exhibits[0].Image.Src
	gotSrc := got.QuestionSet.QuestionCollections[0].Questions[0].Exhibits[0].Image.Src
	if gotSrc != origSrc {
		t.Errorf("Exhibit Src mutated by ID processor: got %q, want %q", gotSrc, origSrc)
	}
}

func TestVFSFileExamLoader_AppliesMultiplePostProcessors(t *testing.T) {
	// Verify that NewVFSFileExamLoader accepts variadic post-processors and
	// applies them all in order: ID prepending then asset URL rewriting.
	const examXML = `<?xml version="1.0" encoding="UTF-8"?>
<root>
<exam id="exam-1" shortname="X" code="C">
  <title>t</title><description>d</description>
  <examcategory>certification-exam</examcategory>
  <questionset>
    <questioncollection>
      <question id="q1" type="single-choice">
        <description>q</description>
        <exhibits><exhibit><image src="/assets/img.png"/></exhibit></exhibits>
        <options><option id="1">a</option></options>
        <correctanswer><options><option id="1">a</option></options></correctanswer>
      </question>
    </questioncollection>
  </questionset>
</exam>
</root>`
	fs := afero.NewMemMapFs()
	if err := afero.WriteFile(fs, "/exam.xml", []byte(examXML), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	idProc := NewVFSExamIDPostProcessor("my-upload")
	assetsProc := NewVFSExamAssetsURLPostProcessor("/api/dyn-assets/uploads/my-upload", "/assets")
	loader := NewVFSFileExamLoader(fs, idProc, assetsProc)
	got, err := loader.LoadFrom(context.Background(), "/exam.xml")
	if err != nil {
		t.Fatalf("LoadFrom: unexpected error: %v", err)
	}
	if want := "/uploads/my-upload/exam-1"; got.Id != want {
		t.Errorf("Id = %q, want %q (ID post-processor not applied)", got.Id, want)
	}
	wantSrc := "/api/dyn-assets/uploads/my-upload/assets/img.png"
	gotSrc := got.QuestionSet.QuestionCollections[0].Questions[0].Exhibits[0].Image.Src
	if gotSrc != wantSrc {
		t.Errorf("Exhibit Src = %q, want %q (assets post-processor not applied)", gotSrc, wantSrc)
	}
}

func TestVFSFileExamLoader_NoPostProcessor(t *testing.T) {
	const examXML = `<?xml version="1.0" encoding="UTF-8"?>
<root>
<exam id="exam-1" shortname="X" code="C">
  <title>t</title><description>d</description>
  <examcategory>certification-exam</examcategory>
  <questionset>
    <questioncollection>
      <question id="q1" type="single-choice">
        <description>q</description>
        <options><option id="1">a</option></options>
        <correctanswer><options><option id="1">a</option></options></correctanswer>
      </question>
    </questioncollection>
  </questionset>
</exam>
</root>`
	fs := afero.NewMemMapFs()
	if err := afero.WriteFile(fs, "/exam.xml", []byte(examXML), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	loader := NewVFSFileExamLoader(fs)
	got, err := loader.LoadFrom(context.Background(), "/exam.xml")
	if err != nil {
		t.Fatalf("LoadFrom: unexpected error: %v", err)
	}
	if got.Id != "exam-1" {
		t.Errorf("Id = %q, want %q", got.Id, "exam-1")
	}
}
