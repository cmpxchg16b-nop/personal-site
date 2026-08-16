package examserver

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"personal-site/pkg/auth"
	"personal-site/pkg/models/msgnotify"
	pkgmodelquestions "personal-site/pkg/models/question"

	"personal-site/pkg/models/examreport"
)

// recordingNotifier is a MsgNotifySvc that records the messages it is asked
// to send. It claims any address and accepts only the service address family,
// like a service message hub would.
type recordingNotifier struct {
	sent []recordedNotification
}

type recordedNotification struct {
	replyTo msgnotify.AddrId
	to      msgnotify.AddrId
	msg     msgnotify.Msg
}

func (r *recordingNotifier) AreYou(addrId msgnotify.AddrId) bool { return true }

func (r *recordingNotifier) GetAcceptedSenderAddressFamilies() []msgnotify.MsgNotifyAddrFamily {
	return []msgnotify.MsgNotifyAddrFamily{msgnotify.MsgNotifyAddrFamilyService}
}

func (r *recordingNotifier) GetAcceptedRecipientAddressFamilies() []msgnotify.MsgNotifyAddrFamily {
	return []msgnotify.MsgNotifyAddrFamily{msgnotify.MsgNotifyAddrFamilyService}
}

func (r *recordingNotifier) Send(ctx context.Context, replyTo msgnotify.AddrId, to msgnotify.AddrId, msg msgnotify.Msg) error {
	r.sent = append(r.sent, recordedNotification{replyTo: replyTo, to: to, msg: msg})
	return nil
}

func TestStartNewExamSession_SendsNotification(t *testing.T) {
	exam, err := pkgmodelquestions.NewFileExamLoader().LoadFile(filepath.Join("..", "..", "..", "exam1.xml"))
	if err != nil {
		t.Fatalf("load exam: %v", err)
	}

	notifier := &recordingNotifier{}
	srv := NewOnMemoryExamServer(examreport.NewOnMemoryExamTrackingServer(nil, nil), []msgnotify.MsgNotifySvc{notifier})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Run(ctx)
	defer srv.Shutdown()

	// A failed session start must not notify.
	if _, err := srv.StartNewExamSession(ctx, nil, "user-1", examreport.Person{}, 0, nil); err != errEmptyExam {
		t.Fatalf("expected errEmptyExam for nil exam, got %v", err)
	}
	if len(notifier.sent) != 0 {
		t.Fatalf("sent %d messages for a failed session start, want 0", len(notifier.sent))
	}

	sessionId, err := srv.StartNewExamSession(ctx, exam, "user-1", examreport.Person{}, 0, nil)
	if err != nil {
		t.Fatalf("StartNewExamSession: %v", err)
	}

	if len(notifier.sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(notifier.sent))
	}
	got := notifier.sent[0]
	if got.replyTo.AddressFamily != msgnotify.MsgNotifyAddrFamilyService ||
		got.replyTo.Address != msgnotify.WellKnownAddrServiceOnMemoryExamSessionServer {
		t.Errorf("sender = %+v, want the on-memory exam session server service address", got.replyTo)
	}
	if v := got.replyTo.Tags.GetByLabelKey(msgnotify.WellKnownLabelKeyMsgSource); !reflect.DeepEqual(v, []string{msgnotify.WellKnownLabelValueExamSessionServer}) {
		t.Errorf("tag %s = %v, want [%s]", msgnotify.WellKnownLabelKeyMsgSource, v, msgnotify.WellKnownLabelValueExamSessionServer)
	}
	if v := got.replyTo.Tags.GetByLabelKey(msgnotify.WellKnownLabelKeyExamTakerSubjectId); !reflect.DeepEqual(v, []string{"user-1"}) {
		t.Errorf("tag %s = %v, want [user-1]", msgnotify.WellKnownLabelKeyExamTakerSubjectId, v)
	}
	if got.to.AddressFamily != msgnotify.MsgNotifyAddrFamilyService || got.to.Address != "" {
		t.Errorf("recipient = %+v, want the opaque service address (service, \"\")", got.to)
	}
	for _, want := range []string{"user-1", string(sessionId), exam.Id} {
		if !strings.Contains(got.msg.Text, want) {
			t.Errorf("text = %q, want it to mention %q", got.msg.Text, want)
		}
	}
}

// TestStartNewExamSession_WalksAllQuestions loads the real exam1.xml and confirms that
// a fresh session presents every question in its question collection, in order.
func TestStartNewExamSession_WalksAllQuestions(t *testing.T) {
	exam, err := pkgmodelquestions.NewFileExamLoader().LoadFile(filepath.Join("..", "..", "..", "exam1.xml"))
	if err != nil {
		t.Fatalf("load exam: %v", err)
	}

	srv := NewOnMemoryExamServer(examreport.NewOnMemoryExamTrackingServer(nil, nil), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Run(ctx)
	defer srv.Shutdown()

	// exam1.xml is a certification exam, so it must not be created seekable;
	// GetNextQuestion walks questions sequentially regardless of seekability.
	examId, err := srv.StartNewExamSession(ctx, exam, "user-1", examreport.Person{}, 0, nil)
	if err != nil {
		t.Fatalf("StartNewExamSession: %v", err)
	}

	var seen []string
	var cursor *QuestionCursor
	for {
		q, next, err := srv.GetNextQuestion(ctx, examId, "user-1", cursor)
		if err != nil {
			t.Fatalf("GetNextQuestion: %v", err)
		}
		if q == nil {
			break // no more questions
		}
		seen = append(seen, q.Id)
		cursor = next
		if cursor == nil {
			break
		}
	}

	// exam1.xml has one collection with 4 questions (ids 1, 4, 6, 7).
	if len(seen) != 4 {
		t.Fatalf("expected 4 questions, got %d (%v)", len(seen), seen)
	}
	for i, want := range []string{"1", "4", "6", "7"} {
		if seen[i] != want {
			t.Errorf("question %d: got id %q, want %q", i, seen[i], want)
		}
	}
}

// TestStartNewExamSession_EmptyExam verifies that an exam with no questions is rejected.
func TestStartNewExamSession_EmptyExam(t *testing.T) {
	srv := NewOnMemoryExamServer(examreport.NewOnMemoryExamTrackingServer(nil, nil), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Run(ctx)
	defer srv.Shutdown()

	if _, err := srv.StartNewExamSession(ctx, nil, "user-1", examreport.Person{}, 0, nil); err != errEmptyExam {
		t.Fatalf("expected errEmptyExam for nil exam, got %v", err)
	}

	empty := &pkgmodelquestions.Exam{Id: "x"}
	if _, err := srv.StartNewExamSession(ctx, empty, "user-1", examreport.Person{}, 0, nil); err != errEmptyExam {
		t.Fatalf("expected errEmptyExam for empty exam, got %v", err)
	}
}

// TestStartNewExamSession_CertificationRejectsSeekable confirms that a
// certification exam cannot be started with ExamOptionSeekable: the candidate
// must answer questions in the fixed order they are served.
func TestStartNewExamSession_CertificationRejectsSeekable(t *testing.T) {
	mkQ := func(id string) pkgmodelquestions.Question {
		return pkgmodelquestions.Question{Id: id, Type: pkgmodelquestions.QuestionTypeSingleChoice}
	}
	certExam := &pkgmodelquestions.Exam{
		Id:           "cert",
		ExamCategory: pkgmodelquestions.ExamCategoryCertification,
		QuestionSet: pkgmodelquestions.QuestionSet{
			QuestionCollections: []pkgmodelquestions.QuestionCollection{
				{Questions: []pkgmodelquestions.Question{mkQ("q1")}},
			},
		},
	}

	srv := NewOnMemoryExamServer(examreport.NewOnMemoryExamTrackingServer(nil, nil), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Run(ctx)
	defer srv.Shutdown()

	if _, err := srv.StartNewExamSession(ctx, certExam, "user-1", examreport.Person{}, ExamOptionSeekable, nil); err != errSeekableNotAllowed {
		t.Fatalf("certification exam with ExamOptionSeekable = %v, want errSeekableNotAllowed", err)
	}

	// The same certification exam without the seekable bit starts fine.
	if _, err := srv.StartNewExamSession(ctx, certExam, "user-1", examreport.Person{}, 0, nil); err != nil {
		t.Fatalf("certification exam without seekable: %v", err)
	}
}

// TestStartNewExamSession_PracticeAllowsSeekable confirms that a practice exam
// may be started seekable and that seeking then works end to end.
func TestStartNewExamSession_PracticeAllowsSeekable(t *testing.T) {
	mkQ := func(id string) pkgmodelquestions.Question {
		return pkgmodelquestions.Question{Id: id, Type: pkgmodelquestions.QuestionTypeSingleChoice}
	}
	practiceExam := &pkgmodelquestions.Exam{
		Id:           "practice",
		ExamCategory: pkgmodelquestions.ExamCategoryPractice,
		QuestionSet: pkgmodelquestions.QuestionSet{
			QuestionCollections: []pkgmodelquestions.QuestionCollection{
				{Questions: []pkgmodelquestions.Question{mkQ("1"), mkQ("2"), mkQ("3")}},
			},
		},
	}

	srv := NewOnMemoryExamServer(examreport.NewOnMemoryExamTrackingServer(nil, nil), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Run(ctx)
	defer srv.Shutdown()

	examId, err := srv.StartNewExamSession(ctx, practiceExam, "user-1", examreport.Person{}, ExamOptionSeekable, nil)
	if err != nil {
		t.Fatalf("practice exam with ExamOptionSeekable: %v", err)
	}

	// Walk to the first question to obtain a cursor, then seek back to it.
	q, cursor, err := srv.GetNextQuestion(ctx, examId, "user-1", nil)
	if err != nil || q == nil {
		t.Fatalf("GetNextQuestion: q=%v err=%v", q, err)
	}
	if _, err := srv.SeekCursorTo(ctx, examId, "user-1", cursor, 0); err != nil {
		t.Errorf("SeekCursorTo on practice exam = %v, want nil", err)
	}
}

// TestStartNewExamSession_RandomCollPicksOneCollection builds a synthetic exam with
// several collections and, with ExamOptionRandomQuestionColl set, confirms the
// session is backed by exactly one collection's questions (a subset), not the
// flattened set.
func TestStartNewExamSession_RandomCollPicksOneCollection(t *testing.T) {
	mkQ := func(id string) pkgmodelquestions.Question {
		return pkgmodelquestions.Question{Id: id, Type: pkgmodelquestions.QuestionTypeSingleChoice}
	}
	exam := &pkgmodelquestions.Exam{
		Id: "synthetic",
		QuestionSet: pkgmodelquestions.QuestionSet{
			QuestionCollections: []pkgmodelquestions.QuestionCollection{
				{Questions: []pkgmodelquestions.Question{mkQ("a1"), mkQ("a2")}},
				{Questions: []pkgmodelquestions.Question{mkQ("b1"), mkQ("b2")}},
				{Questions: []pkgmodelquestions.Question{mkQ("c1"), mkQ("c2")}},
			},
		},
	}

	srv := NewOnMemoryExamServer(examreport.NewOnMemoryExamTrackingServer(nil, nil), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Run(ctx)
	defer srv.Shutdown()

	examId, err := srv.StartNewExamSession(ctx, exam, "user-1", examreport.Person{}, ExamOptionRandomQuestionColl|ExamOptionSeekable, nil)
	if err != nil {
		t.Fatalf("StartNewExamSession: %v", err)
	}

	// All questions in the chosen collection share their leading letter; the
	// flattened path would mix letters.
	var ids []string
	var cursor *QuestionCursor
	for {
		q, next, err := srv.GetNextQuestion(ctx, examId, "user-1", cursor)
		if err != nil {
			t.Fatalf("GetNextQuestion: %v", err)
		}
		if q == nil {
			break
		}
		ids = append(ids, q.Id)
		cursor = next
		if cursor == nil {
			break
		}
	}

	if len(ids) != 2 {
		t.Fatalf("random collection should yield exactly 2 questions, got %d (%v)", len(ids), ids)
	}
	prefix := ids[0][:1]
	for _, id := range ids {
		if id[:1] != prefix {
			t.Fatalf("questions come from different collections (mixed prefixes): %v", ids)
		}
	}
}

// TestStartNewExamSession_FlattensCollectionsByDefault confirms that without
// ExamOptionRandomQuestionColl, every question across all collections is shown.
func TestStartNewExamSession_FlattensCollectionsByDefault(t *testing.T) {
	mkQ := func(id string) pkgmodelquestions.Question {
		return pkgmodelquestions.Question{Id: id, Type: pkgmodelquestions.QuestionTypeSingleChoice}
	}
	exam := &pkgmodelquestions.Exam{
		Id: "synthetic",
		QuestionSet: pkgmodelquestions.QuestionSet{
			QuestionCollections: []pkgmodelquestions.QuestionCollection{
				{Questions: []pkgmodelquestions.Question{mkQ("a1"), mkQ("a2")}},
				{Questions: []pkgmodelquestions.Question{mkQ("b1"), mkQ("b2")}},
			},
		},
	}

	srv := NewOnMemoryExamServer(examreport.NewOnMemoryExamTrackingServer(nil, nil), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Run(ctx)
	defer srv.Shutdown()

	examId, err := srv.StartNewExamSession(ctx, exam, "user-1", examreport.Person{}, ExamOptionSeekable, nil)
	if err != nil {
		t.Fatalf("StartNewExamSession: %v", err)
	}

	var n int
	var cursor *QuestionCursor
	for {
		q, next, err := srv.GetNextQuestion(ctx, examId, "user-1", cursor)
		if err != nil {
			t.Fatalf("GetNextQuestion: %v", err)
		}
		if q == nil {
			break
		}
		n++
		cursor = next
		if cursor == nil {
			break
		}
	}
	if n != 4 {
		t.Fatalf("expected flattened total of 4 questions, got %d", n)
	}
}

// TestGetExamSessionById_CurrentQuestionIndex confirms that a freshly started
// session reports CurrentQuestionIndex == -1 (no question fetched yet), and
// that the index advances to 0, 1, ... as the owner calls GetNextQuestion. It
// also checks that a non-owner cannot retrieve the session and that a missing
// session is reported as an error.
func TestGetExamSessionById_CurrentQuestionIndex(t *testing.T) {
	mkQ := func(id string) pkgmodelquestions.Question {
		return pkgmodelquestions.Question{Id: id, Type: pkgmodelquestions.QuestionTypeSingleChoice}
	}
	exam := &pkgmodelquestions.Exam{
		Id: "index-tracking",
		QuestionSet: pkgmodelquestions.QuestionSet{
			QuestionCollections: []pkgmodelquestions.QuestionCollection{
				{Questions: []pkgmodelquestions.Question{mkQ("1"), mkQ("2"), mkQ("3")}},
			},
		},
	}

	srv := NewOnMemoryExamServer(examreport.NewOnMemoryExamTrackingServer(nil, nil), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Run(ctx)
	defer srv.Shutdown()

	examId, err := srv.StartNewExamSession(ctx, exam, "user-1", examreport.Person{}, ExamOptionSeekable, nil)
	if err != nil {
		t.Fatalf("StartNewExamSession: %v", err)
	}

	// Before any GetNextQuestion, the current index is -1.
	ex, err := srv.GetExamSessionById(ctx, examId, "user-1")
	if err != nil {
		t.Fatalf("GetExamSessionById: %v", err)
	}
	if ex.CurrentQuestionIndex != -1 {
		t.Fatalf("initial CurrentQuestionIndex = %d, want -1", ex.CurrentQuestionIndex)
	}
	if ex.CurrentQuestion != nil {
		t.Fatalf("initial CurrentQuestion = %v, want nil", ex.CurrentQuestion)
	}
	if ex.Id != examId {
		t.Errorf("excerpt.Id = %q, want %q", ex.Id, examId)
	}

	// Each GetNextQuestion should advance the reported index by one.
	var cursor *QuestionCursor
	for want := 0; want < 3; want++ {
		q, next, err := srv.GetNextQuestion(ctx, examId, "user-1", cursor)
		if err != nil || q == nil {
			t.Fatalf("GetNextQuestion(%d): q=%v err=%v", want, q, err)
		}
		cursor = next

		ex, err := srv.GetExamSessionById(ctx, examId, "user-1")
		if err != nil {
			t.Fatalf("GetExamSessionById after fetch %d: %v", want, err)
		}
		if ex.CurrentQuestionIndex != want {
			t.Errorf("after fetch %d, CurrentQuestionIndex = %d, want %d", want, ex.CurrentQuestionIndex, want)
		}
		if ex.CurrentQuestion == nil {
			t.Errorf("after fetch %d, CurrentQuestion = nil, want non-nil", want)
		} else if ex.CurrentQuestion.Id != q.Id {
			t.Errorf("after fetch %d, CurrentQuestion.Id = %q, want %q", want, ex.CurrentQuestion.Id, q.Id)
		}
	}

	// A non-owner must be rejected.
	if _, err := srv.GetExamSessionById(ctx, examId, "user-2"); err != errNotOwner {
		t.Errorf("GetExamSessionById by non-owner = %v, want errNotOwner", err)
	}

	// A missing session must be rejected.
	if _, err := srv.GetExamSessionById(ctx, "does-not-exist", "user-1"); err != errExamNotFound {
		t.Errorf("GetExamSessionById for missing session = %v, want errExamNotFound", err)
	}
}

// hasCorrectAnswer reports whether q carries any correct-answer content.
func hasCorrectAnswer(q *pkgmodelquestions.Question) bool {
	ca := q.CorrectAnswer
	return len(ca.Options) > 0 || len(ca.Combinations) > 0 || len(ca.ConnectionSolutions) > 0
}

// TestGetNextQuestion_StripsCorrectAnswer verifies that served questions never
// carry the correct answer — neither from GetNextQuestion nor from the session
// excerpt's CurrentQuestion — while the grader's internal answer key is
// unaffected and the practice-exam assessment remains the one place the
// correct answer is revealed.
func TestGetNextQuestion_StripsCorrectAnswer(t *testing.T) {
	exam := &pkgmodelquestions.Exam{
		Id:           "strip",
		ExamCategory: pkgmodelquestions.ExamCategoryPractice,
		QuestionSet: pkgmodelquestions.QuestionSet{
			QuestionCollections: []pkgmodelquestions.QuestionCollection{
				{Questions: []pkgmodelquestions.Question{
					singleChoice("sc", 1, "1"),
					dndQuestion("dnd", 1, pkgmodelquestions.ConnectionSolution{
						RequiredUniqueConnections: 1,
						Connects:                  []pkgmodelquestions.Connect{{Src: "a", Dst: "b"}},
					}),
				}},
			},
		},
	}

	srv := NewOnMemoryExamServer(examreport.NewOnMemoryExamTrackingServer(nil, nil), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Run(ctx)
	defer srv.Shutdown()

	examId, err := srv.StartNewExamSession(ctx, exam, "user-1", examreport.Person{}, 0, nil)
	if err != nil {
		t.Fatalf("StartNewExamSession: %v", err)
	}

	// Every served question must be stripped, including the one echoed back as
	// the session excerpt's CurrentQuestion.
	var cursor *QuestionCursor
	for i := 0; i < 2; i++ {
		q, next, err := srv.GetNextQuestion(ctx, examId, "user-1", cursor)
		if err != nil || q == nil {
			t.Fatalf("GetNextQuestion(%d): q=%v err=%v", i, q, err)
		}
		if hasCorrectAnswer(q) {
			t.Errorf("GetNextQuestion(%d) leaked correct answer for %q", i, q.Id)
		}
		cursor = next

		ex, err := srv.GetExamSessionById(ctx, examId, "user-1")
		if err != nil {
			t.Fatalf("GetExamSessionById after fetch %d: %v", i, err)
		}
		if ex.CurrentQuestion == nil || ex.CurrentQuestion.Id != q.Id {
			t.Fatalf("after fetch %d, CurrentQuestion = %v, want Id %q", i, ex.CurrentQuestion, q.Id)
		}
		if hasCorrectAnswer(ex.CurrentQuestion) {
			t.Errorf("CurrentQuestion leaked correct answer for %q", q.Id)
		}
	}

	// Grading still sees the answer key: both answers correct -> full score,
	// and the practice-exam assessment reveals the correct answers.
	assessment, err := srv.SubmitAnswer(ctx, examId, "user-1", examAnswer(
		answer("sc", "1"),
		pkgmodelquestions.Answer{QuestionId: "dnd", Connections: connects([2]string{"a", "b"})},
	), false)
	if err != nil {
		t.Fatalf("SubmitAnswer: %v", err)
	}
	if assessment.ScoreResult.EarnedScore != 2 {
		t.Errorf("earned = %g, want 2", assessment.ScoreResult.EarnedScore)
	}
	if len(assessment.Questions) != 2 {
		t.Fatalf("assessment embedded %d questions, want 2", len(assessment.Questions))
	}
	for _, q := range assessment.Questions {
		if !hasCorrectAnswer(&q) {
			t.Errorf("assessment question %q lost its correct answer", q.Id)
		}
	}
}

// TestOwnershipEnforcement confirms that a user cannot operate on an exam
// session that belongs to another user: EndExamSession, GetNextQuestion,
// SeekCursorTo, and SubmitAnswer all reject a non-owner caller, while the
// owner is unaffected.
func TestOwnershipEnforcement(t *testing.T) {
	mkQ := func(id string) pkgmodelquestions.Question {
		return pkgmodelquestions.Question{Id: id, Type: pkgmodelquestions.QuestionTypeSingleChoice}
	}
	exam := &pkgmodelquestions.Exam{
		Id: "ownership",
		QuestionSet: pkgmodelquestions.QuestionSet{
			QuestionCollections: []pkgmodelquestions.QuestionCollection{
				{Questions: []pkgmodelquestions.Question{mkQ("q1")}},
			},
		},
	}

	srv := NewOnMemoryExamServer(examreport.NewOnMemoryExamTrackingServer(nil, nil), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Run(ctx)
	defer srv.Shutdown()

	examId, err := srv.StartNewExamSession(ctx, exam, "user-1", examreport.Person{}, ExamOptionSeekable, nil)
	if err != nil {
		t.Fatalf("StartNewExamSession: %v", err)
	}

	// A different user must be blocked from every operation on the session.
	if err := srv.EndExamSession(ctx, examId, "user-2"); err != errNotOwner {
		t.Errorf("EndExamSession by non-owner = %v, want errNotOwner", err)
	}
	if _, _, err := srv.GetNextQuestion(ctx, examId, "user-2", nil); err != errNotOwner {
		t.Errorf("GetNextQuestion by non-owner = %v, want errNotOwner", err)
	}
	if _, err := srv.SeekCursorTo(ctx, examId, "user-2", nil, 0); err != errNotOwner {
		t.Errorf("SeekCursorTo by non-owner = %v, want errNotOwner", err)
	}
	if _, err := srv.SubmitAnswer(ctx, examId, "user-2", &pkgmodelquestions.ExamAnswer{}, false); err != errNotOwner {
		t.Errorf("SubmitAnswer by non-owner = %v, want errNotOwner", err)
	}
	if _, err := srv.GetExamSessionById(ctx, examId, "user-2"); err != errNotOwner {
		t.Errorf("GetExamSessionById by non-owner = %v, want errNotOwner", err)
	}

	// The owner can still use their own session.
	if q, _, err := srv.GetNextQuestion(ctx, examId, "user-1", nil); err != nil || q == nil {
		t.Fatalf("GetNextQuestion by owner: q=%v err=%v", q, err)
	}
}

// TestEndExamSession_InjectsExamTakerProfile confirms that the exam taker's
// profile handed to StartNewExamSession is tied to the session and injected
// into the persisted exam report as the <examtaker> person — alongside the
// anonymous session-id entry — so the report server learns the exam taker's
// email address; a session started with a zero Person stays anonymous.
func TestEndExamSession_InjectsExamTakerProfile(t *testing.T) {
	exam := &pkgmodelquestions.Exam{
		Id: "taker-profile",
		QuestionSet: pkgmodelquestions.QuestionSet{
			QuestionCollections: []pkgmodelquestions.QuestionCollection{
				{Questions: []pkgmodelquestions.Question{
					{Id: "q1", Type: pkgmodelquestions.QuestionTypeSingleChoice},
				}},
			},
		},
	}

	tracking := examreport.NewOnMemoryExamTrackingServer(nil, nil)
	srv := NewOnMemoryExamServer(tracking, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Run(ctx)
	defer srv.Shutdown()

	// A session started with a profile reports it as the <examtaker> person.
	taker := examreport.Person{Name: "alice", Email: "alice@example.com"}
	examId, err := srv.StartNewExamSession(ctx, exam, "user-1", taker, 0, nil)
	if err != nil {
		t.Fatalf("StartNewExamSession: %v", err)
	}
	if _, err := srv.SubmitAnswer(ctx, examId, "user-1", examAnswer(answer("q1")), false); err != nil {
		t.Fatalf("SubmitAnswer: %v", err)
	}
	if err := srv.EndExamSession(ctx, examId, "user-1"); err != nil {
		t.Fatalf("EndExamSession: %v", err)
	}

	reports, err := tracking.GetExamReportsByUserId(ctx, "user-1")
	if err != nil || len(reports) != 1 {
		t.Fatalf("reports = %+v, err = %v; want exactly one persisted report", reports, err)
	}
	got := reports[0].ExamTaker
	if len(got.Persons) != 1 || got.Persons[0] != taker {
		t.Errorf("ExamTaker.Persons = %+v, want [%+v]", got.Persons, taker)
	}
	if len(got.Anonymous) != 1 || got.Anonymous[0].SessionId != "user-1" {
		t.Errorf("ExamTaker.Anonymous = %+v, want the user-id session entry kept", got.Anonymous)
	}

	// A session started with a zero Person reports no person at all.
	anonId, err := srv.StartNewExamSession(ctx, exam, "user-2", examreport.Person{}, 0, nil)
	if err != nil {
		t.Fatalf("StartNewExamSession (anonymous): %v", err)
	}
	if _, err := srv.SubmitAnswer(ctx, anonId, "user-2", examAnswer(answer("q1")), false); err != nil {
		t.Fatalf("SubmitAnswer (anonymous): %v", err)
	}
	if err := srv.EndExamSession(ctx, anonId, "user-2"); err != nil {
		t.Fatalf("EndExamSession (anonymous): %v", err)
	}
	anonReports, err := tracking.GetExamReportsByUserId(ctx, "user-2")
	if err != nil || len(anonReports) != 1 {
		t.Fatalf("anonymous reports = %+v, err = %v; want exactly one persisted report", anonReports, err)
	}
	if got := anonReports[0].ExamTaker; len(got.Persons) != 0 {
		t.Errorf("anonymous ExamTaker.Persons = %+v, want none", got.Persons)
	}
}

// TestStartNewExamSession_VisitorMailingConsentIgnored confirms that a
// visitor session (subject id with the visitor prefix) cannot opt into the
// exam report email: the server masks the mailing consent bit off, since a
// visitor has no email address to deliver the report to. A signed-in user
// keeps the bit.
func TestStartNewExamSession_VisitorMailingConsentIgnored(t *testing.T) {
	exam := &pkgmodelquestions.Exam{
		Id: "visitor-consent",
		QuestionSet: pkgmodelquestions.QuestionSet{
			QuestionCollections: []pkgmodelquestions.QuestionCollection{
				{Questions: []pkgmodelquestions.Question{
					{Id: "q1", Type: pkgmodelquestions.QuestionTypeSingleChoice},
				}},
			},
		},
	}

	srv := NewOnMemoryExamServer(examreport.NewOnMemoryExamTrackingServer(nil, nil), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Run(ctx)
	defer srv.Shutdown()

	visitorId := auth.VisitorSubjectPrefix + "anon-1"
	examId, err := srv.StartNewExamSession(ctx, exam, visitorId, examreport.Person{}, ExamOptionSendExamReportEmail, nil)
	if err != nil {
		t.Fatalf("StartNewExamSession (visitor): %v", err)
	}
	excerpt, err := srv.GetExamSessionById(ctx, examId, visitorId)
	if err != nil {
		t.Fatalf("GetExamSessionById (visitor): %v", err)
	}
	if excerpt.Options&ExamOptionSendExamReportEmail != 0 {
		t.Errorf("visitor session options = %b, want the mailing consent bit masked off", excerpt.Options)
	}

	userId := "user-1"
	examId, err = srv.StartNewExamSession(ctx, exam, userId, examreport.Person{}, ExamOptionSendExamReportEmail, nil)
	if err != nil {
		t.Fatalf("StartNewExamSession (user): %v", err)
	}
	excerpt, err = srv.GetExamSessionById(ctx, examId, userId)
	if err != nil {
		t.Fatalf("GetExamSessionById (user): %v", err)
	}
	if excerpt.Options&ExamOptionSendExamReportEmail == 0 {
		t.Errorf("user session options = %b, want the mailing consent bit kept", excerpt.Options)
	}
}

// TestStartNewExamSession_AcceptQuestionTypes loads the real exam1.xml and
// confirms that acceptQuestionTypes restricts the served questions to the
// listed types, that the session excerpt reports the filtered question count,
// and that a filter matching nothing fails like an empty exam.
func TestStartNewExamSession_AcceptQuestionTypes(t *testing.T) {
	exam, err := pkgmodelquestions.NewFileExamLoader().LoadFile(filepath.Join("..", "..", "..", "exam1.xml"))
	if err != nil {
		t.Fatalf("load exam: %v", err)
	}

	srv := NewOnMemoryExamServer(examreport.NewOnMemoryExamTrackingServer(nil, nil), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Run(ctx)
	defer srv.Shutdown()

	// exam1.xml: questions 1, 4, 6 are single-choice and 7 is drag-and-drop.
	// Accepting only the choice types must skip the drag-and-drop question.
	accept := []pkgmodelquestions.QuestionType{
		pkgmodelquestions.QuestionTypeSingleChoice,
		pkgmodelquestions.QuestionTypeMultipleChoice,
	}
	examId, err := srv.StartNewExamSession(ctx, exam, "user-1", examreport.Person{}, 0, accept)
	if err != nil {
		t.Fatalf("StartNewExamSession: %v", err)
	}

	var seen []string
	var cursor *QuestionCursor
	for {
		q, next, err := srv.GetNextQuestion(ctx, examId, "user-1", cursor)
		if err != nil {
			t.Fatalf("GetNextQuestion: %v", err)
		}
		if q == nil {
			break // no more questions
		}
		seen = append(seen, q.Id)
		cursor = next
		if cursor == nil {
			break
		}
	}

	want := []string{"1", "4", "6"}
	if len(seen) != len(want) {
		t.Fatalf("expected %d questions, got %d (%v)", len(want), len(seen), seen)
	}
	for i, id := range want {
		if seen[i] != id {
			t.Errorf("question %d: got id %q, want %q", i, seen[i], id)
		}
	}

	// The session excerpt must describe the filtered question set, not the
	// exam document's full collection, so clients can bound navigation.
	ex, err := srv.GetExamSessionById(ctx, examId, "user-1")
	if err != nil {
		t.Fatalf("GetExamSessionById: %v", err)
	}
	if ex.ExamExcerpt.NumQuestions != len(want) {
		t.Errorf("excerpt NumQuestions = %d, want %d", ex.ExamExcerpt.NumQuestions, len(want))
	}

	// A filter matching no question at all is rejected like an empty exam.
	if _, err := srv.StartNewExamSession(ctx, exam, "user-1", examreport.Person{}, 0, []pkgmodelquestions.QuestionType{"essay"}); err != errEmptyExam {
		t.Errorf("StartNewExamSession with unmatched filter = %v, want errEmptyExam", err)
	}
}

// walkServedQuestions drains GetNextQuestion and returns every served
// question, in serve order.
func walkServedQuestions(t *testing.T, srv *OnMemoryExamServer, ctx context.Context, examId ExamSessionId) []*pkgmodelquestions.Question {
	t.Helper()
	var qs []*pkgmodelquestions.Question
	var cursor *QuestionCursor
	for {
		q, next, err := srv.GetNextQuestion(ctx, examId, "user-1", cursor)
		if err != nil {
			t.Fatalf("GetNextQuestion: %v", err)
		}
		if q == nil {
			return qs
		}
		qs = append(qs, q)
		cursor = next
		if cursor == nil {
			return qs
		}
	}
}

// TestStartNewExamSession_VirtualCollectionSamples builds a certification exam
// whose virtual collection samples 5 questions from collections 0 and 2 (4
// questions each), skipping collection 1. The session must serve exactly the
// 5 sampled questions, all distinct, none from the unreferenced collection —
// and, since 5 questions cannot come from a single 4-question collection, at
// least one from each referenced collection. ExamOptionRandomQuestionColl is
// passed to prove it is moot when a virtual collection is in effect.
func TestStartNewExamSession_VirtualCollectionSamples(t *testing.T) {
	mkQ := func(id string) pkgmodelquestions.Question {
		return pkgmodelquestions.Question{Id: id, Type: pkgmodelquestions.QuestionTypeSingleChoice}
	}
	exam := &pkgmodelquestions.Exam{
		Id:           "cert-vc",
		ExamCategory: pkgmodelquestions.ExamCategoryCertification,
		VirtualCollection: &pkgmodelquestions.VirtualCollection{
			SampleSize:    5,
			CollectionIdx: []int{0, 2},
		},
		QuestionSet: pkgmodelquestions.QuestionSet{
			QuestionCollections: []pkgmodelquestions.QuestionCollection{
				{Questions: []pkgmodelquestions.Question{mkQ("a1"), mkQ("a2"), mkQ("a3"), mkQ("a4")}},
				{Questions: []pkgmodelquestions.Question{mkQ("b1"), mkQ("b2"), mkQ("b3"), mkQ("b4")}},
				{Questions: []pkgmodelquestions.Question{mkQ("c1"), mkQ("c2"), mkQ("c3"), mkQ("c4")}},
			},
		},
	}

	srv := NewOnMemoryExamServer(examreport.NewOnMemoryExamTrackingServer(nil, nil), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Run(ctx)
	defer srv.Shutdown()

	examId, err := srv.StartNewExamSession(ctx, exam, "user-1", examreport.Person{}, ExamOptionRandomQuestionColl, nil)
	if err != nil {
		t.Fatalf("StartNewExamSession: %v", err)
	}

	served := walkServedQuestions(t, srv, ctx, examId)
	if len(served) != 5 {
		t.Fatalf("expected 5 sampled questions, got %d", len(served))
	}
	seen := make(map[string]bool, len(served))
	prefixes := make(map[byte]bool)
	for _, q := range served {
		if seen[q.Id] {
			t.Fatalf("question %q served twice", q.Id)
		}
		seen[q.Id] = true
		prefixes[q.Id[0]] = true
	}
	if prefixes['b'] {
		t.Fatalf("questions from the unreferenced collection were served: %v", seen)
	}
	if !prefixes['a'] || !prefixes['c'] {
		t.Fatalf("pigeonhole: 5 samples from two 4-question collections must cover both, got prefixes %v", prefixes)
	}
}

// TestStartNewExamSession_VirtualCollectionForcesRandomOptionOrder confirms
// that a virtual-collection session shuffles option order even when the caller
// did not request ExamOptionRandomOptions. With 8 questions of 4 options each,
// the probability of every question accidentally keeping its document order is
// (1/4!)^8 — negligible.
func TestStartNewExamSession_VirtualCollectionForcesRandomOptionOrder(t *testing.T) {
	mkQ := func(id string) pkgmodelquestions.Question {
		return pkgmodelquestions.Question{
			Id:   id,
			Type: pkgmodelquestions.QuestionTypeSingleChoice,
			Options: pkgmodelquestions.Options{
				{Id: "o1"}, {Id: "o2"}, {Id: "o3"}, {Id: "o4"},
			},
		}
	}
	var questions []pkgmodelquestions.Question
	for _, id := range []string{"1", "2", "3", "4", "5", "6", "7", "8"} {
		questions = append(questions, mkQ(id))
	}
	exam := &pkgmodelquestions.Exam{
		Id:           "cert-vc-options",
		ExamCategory: pkgmodelquestions.ExamCategoryCertification,
		VirtualCollection: &pkgmodelquestions.VirtualCollection{
			SampleSize:    8, // sample the whole population: every question is served
			CollectionIdx: []int{0},
		},
		QuestionSet: pkgmodelquestions.QuestionSet{
			QuestionCollections: []pkgmodelquestions.QuestionCollection{
				{Questions: questions},
			},
		},
	}

	srv := NewOnMemoryExamServer(examreport.NewOnMemoryExamTrackingServer(nil, nil), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Run(ctx)
	defer srv.Shutdown()

	// No randomization bits requested; the virtual collection must force them.
	examId, err := srv.StartNewExamSession(ctx, exam, "user-1", examreport.Person{}, 0, nil)
	if err != nil {
		t.Fatalf("StartNewExamSession: %v", err)
	}

	served := walkServedQuestions(t, srv, ctx, examId)
	if len(served) != 8 {
		t.Fatalf("expected all 8 questions, got %d", len(served))
	}
	shuffled := 0
	for _, q := range served {
		if len(q.Options) != 4 {
			t.Fatalf("question %q served with %d options, want 4", q.Id, len(q.Options))
		}
		if q.Options[0].Id != "o1" || q.Options[1].Id != "o2" ||
			q.Options[2].Id != "o3" || q.Options[3].Id != "o4" {
			shuffled++
		}
	}
	if shuffled == 0 {
		t.Fatal("no question had its option order shuffled; ExamOptionRandomOptions was not forced")
	}
}

// TestStartNewExamSession_PracticeIgnoresVirtualCollection confirms that a
// practice exam never applies a virtual collection (the loader rejects such
// documents; the server ignores it for programmatically built exams): the
// usual flatten-everything behavior applies instead.
func TestStartNewExamSession_PracticeIgnoresVirtualCollection(t *testing.T) {
	mkQ := func(id string) pkgmodelquestions.Question {
		return pkgmodelquestions.Question{Id: id, Type: pkgmodelquestions.QuestionTypeSingleChoice}
	}
	exam := &pkgmodelquestions.Exam{
		Id:           "practice-vc",
		ExamCategory: pkgmodelquestions.ExamCategoryPractice,
		VirtualCollection: &pkgmodelquestions.VirtualCollection{
			SampleSize:    2,
			CollectionIdx: []int{0},
		},
		QuestionSet: pkgmodelquestions.QuestionSet{
			QuestionCollections: []pkgmodelquestions.QuestionCollection{
				{Questions: []pkgmodelquestions.Question{mkQ("a1"), mkQ("a2")}},
				{Questions: []pkgmodelquestions.Question{mkQ("b1"), mkQ("b2")}},
			},
		},
	}

	srv := NewOnMemoryExamServer(examreport.NewOnMemoryExamTrackingServer(nil, nil), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Run(ctx)
	defer srv.Shutdown()

	examId, err := srv.StartNewExamSession(ctx, exam, "user-1", examreport.Person{}, 0, nil)
	if err != nil {
		t.Fatalf("StartNewExamSession: %v", err)
	}

	if served := walkServedQuestions(t, srv, ctx, examId); len(served) != 4 {
		t.Fatalf("practice exam must serve all 4 flattened questions (virtual collection ignored), got %d", len(served))
	}
}
