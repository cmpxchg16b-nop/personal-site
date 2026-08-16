package fsbasedassociation

import (
	"context"
	pkgmodelsquestion "personal-site/pkg/models/question"
	"fmt"
	"strings"

	"github.com/spf13/afero"
)

// VFSFileExamLoader decodes Exam documents from XML files held in an afero
// virtual filesystem. It is the virtual-fs analogue of FileExamLoader: rather
// than reading from the host OS, each LoadFrom opens and reads from the
// constructor-supplied afero.Fs. It is safe for concurrent use once the
// underlying afero.Fs is.
type VFSFileExamLoader struct {
	fs afero.Fs
	postProcessors []pkgmodelsquestion.ExamDocumentPostProcessor
}

// NewVFSFileExamLoader returns an ExamLoader that reads exam XML from the
// given afero virtual filesystem.
func NewVFSFileExamLoader(fs afero.Fs, postProcessors ...pkgmodelsquestion.ExamDocumentPostProcessor) *VFSFileExamLoader {
	return &VFSFileExamLoader{fs: fs, postProcessors: postProcessors}
}

// LoadFrom reads the XML file at path within the virtual filesystem and decodes
// it into an Exam. The decoding logic is shared with FileExamLoader.
func (l *VFSFileExamLoader) LoadFrom(ctx context.Context, path string) (*pkgmodelsquestion.Exam, error) {
	_ = ctx
	f, err := l.fs.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open exam file %q in vfs: %w", path, err)
	}
	defer f.Close()
	data, err := afero.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("read exam file %q in vfs: %w", path, err)
	}

	exam, err := (&pkgmodelsquestion.FileExamLoader{}).Load(data)
	if err != nil {
		return nil, fmt.Errorf("failed to decode exam document (path: %s) data in vfs: %w", path, err)
	}

	for _, pp := range l.postProcessors {
		if pp == nil {
			continue
		}
		exam, err = pp.PostProcess(exam)
		if err != nil {
			return nil, fmt.Errorf("failed to post-process exam document (path: %s): %w", path, err)
		}
	}

	return exam, nil
}

// VFSExamIDPostProcessor implements ExamDocumentPostProcessor by deep-copying an
// Exam and prepending its Id with "/uploads/{upload_id}/". The deep copy
// guarantees the caller's Exam is never mutated.
type VFSExamIDPostProcessor struct {
	uploadId string
}

// NewVFSExamIDPostProcessor returns an ExamDocumentPostProcessor that prepends
// the exam id with "/uploads/{upload_id}/".
func NewVFSExamIDPostProcessor(uploadId string) *VFSExamIDPostProcessor {
	return &VFSExamIDPostProcessor{uploadId: uploadId}
}

// PostProcess returns a deep copy of exam in which Id has been prepended with
// "/uploads/{upload_id}/". A nil exam yields a nil result.
func (p *VFSExamIDPostProcessor) PostProcess(exam *pkgmodelsquestion.Exam) (*pkgmodelsquestion.Exam, error) {
	if exam == nil {
		return nil, nil
	}
	cp := deepCopyExam(exam)
	cp.Id = "/uploads/" + p.uploadId + "/" + cp.Id
	return cp, nil
}

// VFSExamAssetsURLPostProcessor implements ExamDocumentPostProcessor by deep-copying an
// Exam and rewriting every asset URL (one beginning with assetsURLPrefix) so
// that prefix is prepended. The deep copy guarantees the caller's Exam is never
// mutated.
type VFSExamAssetsURLPostProcessor struct {
	prefix           string
	assetsURLPrefix  string
}

// NewVFSExamAssetsURLPostProcessor returns an ExamDocumentPostProcessor that rewrites
// URLs beginning with assetsURLPrefix to prefix + url.
func NewVFSExamAssetsURLPostProcessor(prefix, assetsURLPrefix string) *VFSExamAssetsURLPostProcessor {
	return &VFSExamAssetsURLPostProcessor{prefix: prefix, assetsURLPrefix: assetsURLPrefix}
}

// PostProcess returns a deep copy of exam in which every URL reference that
// begins with assetsURLPrefix has had the processor's prefix prepended. A nil
// exam yields a nil result.
func (p *VFSExamAssetsURLPostProcessor) PostProcess(exam *pkgmodelsquestion.Exam) (*pkgmodelsquestion.Exam, error) {
	if exam == nil {
		return nil, nil
	}
	cp := deepCopyExam(exam)
	p.rewriteAssetURLs(cp)
	return cp, nil
}

// prefixAssetURL prepends the processor's prefix to url when it begins with the
// assets prefix; otherwise url is returned unchanged.
func (p *VFSExamAssetsURLPostProcessor) prefixAssetURL(url string) string {
	if strings.HasPrefix(url, p.assetsURLPrefix) {
		return p.prefix + url
	}
	return url
}

// rewriteAssetURLs rewrites asset URLs in place on the (already deep-copied)
// exam. The fields treated as asset URLs are the image sources: Exhibit.Image.Src,
// ImgCandidate.ImgDataSrc, and ImgDropsArea.ImgBackgroundUrl.
func (p *VFSExamAssetsURLPostProcessor) rewriteAssetURLs(exam *pkgmodelsquestion.Exam) {
	for ci := range exam.QuestionSet.QuestionCollections {
		questions := exam.QuestionSet.QuestionCollections[ci].Questions
		for qi := range questions {
			q := &questions[qi]
			for ei := range q.Exhibits {
				q.Exhibits[ei].Image.Src = p.prefixAssetURL(q.Exhibits[ei].Image.Src)
			}
			if q.ImgDragAndDrop != nil {
				for i := range q.ImgDragAndDrop.ImgCandidates {
					q.ImgDragAndDrop.ImgCandidates[i].ImgDataSrc = p.prefixAssetURL(q.ImgDragAndDrop.ImgCandidates[i].ImgDataSrc)
				}
				q.ImgDragAndDrop.ImgDropsArea.ImgBackgroundUrl = p.prefixAssetURL(q.ImgDragAndDrop.ImgDropsArea.ImgBackgroundUrl)
			}
		}
	}
}

// cloneSlice returns an independent copy of in: a nil input yields nil, otherwise
// a freshly allocated slice whose elements are copies of in's. For slices whose
// element type contains only value-type fields, this is a complete deep copy.
func cloneSlice[T any](in []T) []T {
	if in == nil {
		return nil
	}
	out := make([]T, len(in))
	copy(out, in)
	return out
}

// deepCopyExam returns a fully independent copy of exam: every nested slice is
// reallocated and every pointer is duplicated, so mutating the copy never affects
// the original.
func deepCopyExam(exam *pkgmodelsquestion.Exam) *pkgmodelsquestion.Exam {
	cp := *exam
	if exam.PassingScore != nil {
		score := *exam.PassingScore
		cp.PassingScore = &score
	}
	if collections := exam.QuestionSet.QuestionCollections; collections != nil {
		cp.QuestionSet.QuestionCollections = make([]pkgmodelsquestion.QuestionCollection, len(collections))
		for i, qc := range collections {
			cp.QuestionSet.QuestionCollections[i] = deepCopyQuestionCollection(qc)
		}
	}
	return &cp
}

func deepCopyQuestionCollection(qc pkgmodelsquestion.QuestionCollection) pkgmodelsquestion.QuestionCollection {
	cp := qc
	if qc.Questions != nil {
		cp.Questions = make([]pkgmodelsquestion.Question, len(qc.Questions))
		for i, q := range qc.Questions {
			cp.Questions[i] = deepCopyQuestion(q)
		}
	}
	return cp
}

func deepCopyQuestion(q pkgmodelsquestion.Question) pkgmodelsquestion.Question {
	cp := q
	cp.Exhibits = pkgmodelsquestion.Exhibits(cloneSlice(q.Exhibits))
	cp.Options = pkgmodelsquestion.Options(cloneSlice(q.Options))
	cp.Candidates = pkgmodelsquestion.Candidates(cloneSlice(q.Candidates))
	if q.ImgDragAndDrop != nil {
		cp.ImgDragAndDrop = deepCopyImgDragAndDrop(q.ImgDragAndDrop)
	}
	if q.MultiAreaDrop != nil {
		cp.MultiAreaDrop = deepCopyMultiAreaDrop(q.MultiAreaDrop)
	}
	cp.Drops = pkgmodelsquestion.Drops(cloneSlice(q.Drops))
	cp.CorrectAnswer = deepCopyCorrectAnswer(q.CorrectAnswer)
	return cp
}

func deepCopyImgDragAndDrop(d *pkgmodelsquestion.ImgDragAndDrop) *pkgmodelsquestion.ImgDragAndDrop {
	cp := *d
	cp.ImgCandidates = cloneSlice(d.ImgCandidates)
	area := d.ImgDropsArea
	area.ImgDrops = cloneSlice(area.ImgDrops)
	cp.ImgDropsArea = area
	return &cp
}

func deepCopyMultiAreaDrop(m *pkgmodelsquestion.MultiAreaDrop) *pkgmodelsquestion.MultiAreaDrop {
	cp := *m
	if m.DropAreas != nil {
		cp.DropAreas = make([]pkgmodelsquestion.DropArea, len(m.DropAreas))
		for i, da := range m.DropAreas {
			da.Drops = pkgmodelsquestion.Drops(cloneSlice(da.Drops))
			cp.DropAreas[i] = da
		}
	}
	return &cp
}

func deepCopyCorrectAnswer(ca pkgmodelsquestion.CorrectAnswer) pkgmodelsquestion.CorrectAnswer {
	cp := ca
	cp.Options = pkgmodelsquestion.Options(cloneSlice(ca.Options))
	if ca.Combinations != nil {
		cp.Combinations = make([]pkgmodelsquestion.Combination, len(ca.Combinations))
		for i, cb := range ca.Combinations {
			cb.Options = pkgmodelsquestion.Options(cloneSlice(cb.Options))
			cp.Combinations[i] = cb
		}
	}
	if ca.ConnectionSolutions != nil {
		cp.ConnectionSolutions = make([]pkgmodelsquestion.ConnectionSolution, len(ca.ConnectionSolutions))
		for i, cs := range ca.ConnectionSolutions {
			cs.Connects = cloneSlice(cs.Connects)
			if cs.ConnectCombinations != nil {
				cs.ConnectCombinations = make([]pkgmodelsquestion.ConnectCombination, len(cs.ConnectCombinations))
				for j, cc := range cs.ConnectCombinations {
					cc.ConnectSources = cloneSlice(cc.ConnectSources)
					cc.ConnectDestinations = cloneSlice(cc.ConnectDestinations)
					cs.ConnectCombinations[j] = cc
				}
			}
			cp.ConnectionSolutions[i] = cs
		}
	}
	return cp
}
