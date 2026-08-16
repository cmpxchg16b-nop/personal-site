package examserver

import (
	pkgmodelquestions "personal-site/pkg/models/question"
)

// SimpleGrader grades an exam submission and returns an assessment of the
// results.
type SimpleGrader interface {
	// Grade evaluates the given examAnswer and returns an Assessment describing
	// the per-question and overall scores, or an error if grading could not be
	// completed.
	Grade(examAnswer *pkgmodelquestions.ExamAnswer) (*pkgmodelquestions.Assessment, error)
}

// SimpleOnMemoryGrader is a SimpleGrader that indexes a question collection in
// memory by question id. It is not safe for concurrent use.
//
// Single-choice, multiple-choice, and drag-and-drop questions are graded;
// questions of any other (unrecognized) type are silently skipped. Answers are
// correlated to questions by questionId, and option sets are compared by option
// id, so neither question nor option ordering affects the result.
//
// The exam category controls whether the original question documents are
// embedded in the produced Assessment: for practice-exam the origin question
// (including its correct answer) is attached for each answered question so the
// candidate can review their mistakes; for certification-exam it is omitted.
type SimpleOnMemoryGrader struct {
	questions    map[string]pkgmodelquestions.Question
	totalScore   float32
	passingScore *float32
	examCategory pkgmodelquestions.ExamCategory
}

// NewSimpleOnMemoryGrader indexes qc by question id and precomputes the total
// achievable score. passingScore, when non-nil, drives the assessment's
// OverallResult: the submission passes once the earned score reaches it.
// examCategory governs whether the original questions are embedded in the
// assessment: practice-exam includes them (for answered questions only), other
// categories omit them.
func NewSimpleOnMemoryGrader(qc *pkgmodelquestions.QuestionCollection, passingScore *float32, examCategory pkgmodelquestions.ExamCategory) *SimpleOnMemoryGrader {
	g := &SimpleOnMemoryGrader{
		questions:    make(map[string]pkgmodelquestions.Question),
		passingScore: passingScore,
		examCategory: examCategory,
	}
	if qc != nil {
		for _, q := range qc.Questions {
			g.questions[q.Id] = q
			g.totalScore += q.Score
		}
	}
	return g
}

// Grade evaluates each submitted answer against its official question and
// builds an Assessment. Questions whose type is none of single-choice,
// multiple-choice, or drag-and-drop, and answers with no matching question,
// are skipped without error. Unanswered questions contribute no QuestionScore;
// their potential score still counts toward TotalScore.
//
// For a practice-exam category the original question document (carrying its
// correct answer) is attached for each answered, gradeable question; questions
// the candidate did not answer are not included. Other categories never attach
// the origin question.
func (g *SimpleOnMemoryGrader) Grade(examAnswer *pkgmodelquestions.ExamAnswer) (*pkgmodelquestions.Assessment, error) {
	var earnedScore float32
	var questionScores []pkgmodelquestions.QuestionScore
	includeQuestions := g.examCategory == pkgmodelquestions.ExamCategoryPractice
	var questions []pkgmodelquestions.Question

	if examAnswer != nil {
		for _, ans := range examAnswer.Answers {
			q, ok := g.questions[ans.QuestionId]
			if !ok {
				continue
			}
			switch q.Type {
			case pkgmodelquestions.QuestionTypeSingleChoice,
				pkgmodelquestions.QuestionTypeMultipleChoice,
				pkgmodelquestions.QuestionTypeDragAndDrop:
			default:
				continue // unsupported type: skip silently
			}

			var earned float32
			if isAnswerCorrect(q, ans) {
				earned = q.Score
			}
			earnedScore += earned
			questionScores = append(questionScores, pkgmodelquestions.QuestionScore{
				QuestionId:  q.Id,
				ScoreEarned: earned,
			})
			if includeQuestions {
				questions = append(questions, q)
			}
		}
	}

	overall := pkgmodelquestions.OverallResultImmediate
	if g.passingScore != nil && earnedScore >= *g.passingScore {
		overall = pkgmodelquestions.OverallResultPass
	}

	return &pkgmodelquestions.Assessment{
		OverallResult: &overall,
		ScoreResult: &pkgmodelquestions.ScoreResult{
			EarnedScore: earnedScore,
			TotalScore:  g.totalScore,
		},
		QuestionScores: questionScores,
		Questions:      questions,
	}, nil
}

// isAnswerCorrect reports whether ans matches the official correct answer
// for q. The match semantics differ per question type:
//   - single-choice: correct so long as exactly one option is submitted and
//     that option is one of the correct options; option ordering is
//     irrelevant.
//   - multiple-choice: correct only when the submitted option set exactly
//     matches the correct option set; option ordering is irrelevant.
//   - drag-and-drop: correct when the submitted connections satisfy at least
//     one of the question's connection solutions.
func isAnswerCorrect(q pkgmodelquestions.Question, ans pkgmodelquestions.Answer) bool {
	correct, submitted := q.CorrectAnswer.Options, ans.Options
	switch q.Type {
	case pkgmodelquestions.QuestionTypeSingleChoice:
		if len(submitted) != 1 {
			return false
		}
		correctIds := make(map[string]struct{}, len(correct))
		for _, o := range correct {
			correctIds[o.Id] = struct{}{}
		}
		_, ok := correctIds[submitted[0].Id]
		return ok
	case pkgmodelquestions.QuestionTypeMultipleChoice:
		if len(correct) != len(submitted) {
			return false
		}
		seen := make(map[string]struct{}, len(submitted))
		for _, o := range submitted {
			seen[o.Id] = struct{}{}
		}
		for _, o := range correct {
			if _, ok := seen[o.Id]; !ok {
				return false
			}
		}
		return true
	case pkgmodelquestions.QuestionTypeDragAndDrop:
		return isConnectionAnswerCorrect(q.CorrectAnswer.ConnectionSolutions, ans.Connections)
	}
	return false
}

// connectionKey identifies a connection by its (src, dst) pair for set
// membership tests.
type connectionKey struct {
	src string
	dst string
}

// acceptedConnections expands a ConnectionSolution into the set of connections
// it accepts: its explicit connects plus the Cartesian products of its connect
// combinations.
func acceptedConnections(sol pkgmodelquestions.ConnectionSolution) map[connectionKey]struct{} {
	accepted := make(map[connectionKey]struct{})
	for _, c := range sol.Connects {
		accepted[connectionKey{c.Src, c.Dst}] = struct{}{}
	}
	for _, combo := range sol.ConnectCombinations {
		for _, s := range combo.ConnectSources {
			for _, d := range combo.ConnectDestinations {
				accepted[connectionKey{s.Id, d.Id}] = struct{}{}
			}
		}
	}
	return accepted
}

// isConnectionSolutionSatisfied reports whether the submitted connections
// satisfy sol: the number of unique submitted connections sol accepts reaches
// its RequiredUniqueConnections. Duplicate submissions of the same connection
// count once.
func isConnectionSolutionSatisfied(sol pkgmodelquestions.ConnectionSolution, submitted []pkgmodelquestions.Connect) bool {
	accepted := acceptedConnections(sol)
	unique := make(map[connectionKey]struct{}, len(submitted))
	for _, c := range submitted {
		unique[connectionKey{c.Src, c.Dst}] = struct{}{}
	}
	count := 0
	for k := range unique {
		if _, ok := accepted[k]; ok {
			count++
		}
	}
	return count >= sol.RequiredUniqueConnections
}

// isConnectionAnswerCorrect reports whether the submitted connections satisfy
// at least one of the question's connection solutions.
func isConnectionAnswerCorrect(solutions []pkgmodelquestions.ConnectionSolution, submitted []pkgmodelquestions.Connect) bool {
	for _, sol := range solutions {
		if isConnectionSolutionSatisfied(sol, submitted) {
			return true
		}
	}
	return false
}
