package examserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"strings"
	"sync"
	"time"

	"personal-site/pkg/auth"
	"personal-site/pkg/models/msgnotify"
	pkgmodelquestions "personal-site/pkg/models/question"

	"personal-site/pkg/models/examreport"

	"github.com/google/uuid"
)

type ExamSessionId string
type QuestionCursor string

type ExamOptions uint32

const (
	ExamOptionRandomQuestions    ExamOptions = 1 << iota // randomized questions ordering within a collection
	ExamOptionRandomOptions                              // randomized options ordering
	ExamOptionSeekable                                   // the 'Seekable' allows the client to seek to question with given number at will
	ExamOptionRandomQuestionColl                         // Randomized question collection picking
	// SendExamReportEmail is the exam taker's consent to the exam server
	// emailing the exam report to the exam taker's email address once the
	// exam session is completed.
	ExamOptionSendExamReportEmail
)

var (
	errExamNotFound       = errors.New("exam not found")
	errNotOwner           = errors.New("exam session does not belong to the caller")
	errNotSeekable        = errors.New("exam is not seekable")
	errSeekableNotAllowed = errors.New("seekable exam sessions are not allowed for certification exams")
	errInvalidCursor      = errors.New("invalid question cursor")
	errOutOfRange         = errors.New("question index out of range")
	errShuttingDown       = errors.New("exam server is shutting down")
	errEmptyExam          = errors.New("exam has no questions")
)

type ExamSessionExcerpt struct {
	// This Id is the id of exam session, by which the exam server use to keep track of exam sessions
	Id ExamSessionId

	ExamExcerpt pkgmodelquestions.ExamDocumentExcerpt
	// millisecond-resolution unix timestamp
	StartedAt uint64

	// Options is the ExamOptions bitmask the session was created with.
	Options ExamOptions

	// CurrentQuestionIndex is the virtual index of the question most recently
	// served by GetNextQuestion. It is -1 before the first question has been
	// fetched, since the client must call GetNextQuestion even to obtain the
	// first question.
	CurrentQuestionIndex int

	// CurrentQuestion is the question most recently served by GetNextQuestion.
	// It is nil until the first question has been fetched. It is updated in
	// lock-step with CurrentQuestionIndex.
	CurrentQuestion *pkgmodelquestions.Question
}

type ExamServer interface {
	// Start a new exam session. acceptQuestionTypes restricts which question
	// types the session serves; an empty slice accepts every type. If no
	// question remains after filtering, the call fails as if the exam were
	// empty. taker is the exam taker's profile: it is tied to the session and
	// injected into the exam report as the <examtaker> <person> when the
	// session ends; a zero Person keeps the exam taker anonymous.
	StartNewExamSession(ctx context.Context, exam *pkgmodelquestions.Exam, userId string, taker examreport.Person, examOptions ExamOptions, acceptQuestionTypes []pkgmodelquestions.QuestionType) (ExamSessionId, error)

	// List started exam sessions of a given user
	ListExamSessions(ctx context.Context, userId string) []ExamSessionExcerpt

	// Get a single exam session by its id, scoped to the caller. Returns an
	// error if the session does not exist or does not belong to the caller.
	GetExamSessionById(ctx context.Context, examId ExamSessionId, userId string) (ExamSessionExcerpt, error)

	// Terminate the specified exam session
	EndExamSession(ctx context.Context, examId ExamSessionId, userId string) error

	// the initial cursor should be nil
	// if has more, `nextCursor` won't be nil
	GetNextQuestion(ctx context.Context, examId ExamSessionId, userId string, cursor *QuestionCursor) (question *pkgmodelquestions.Question, nextCursor *QuestionCursor, err error)

	// if the cursor is nil, a brand-new cursor will be created, you should always use the `repositionedCursor` to get the next question, whether it was succeeded or not.
	// if the exam is un-seekable, the operation will fail.
	// the content of cursor is opaque, the client should never assume anything about it.
	SeekCursorTo(ctx context.Context, examId ExamSessionId, userId string, cursor *QuestionCursor, newVirtualIndex int) (repositionedCursor *QuestionCursor, err error)

	SubmitAnswer(ctx context.Context, examId ExamSessionId, userId string, answer *pkgmodelquestions.ExamAnswer, checkOnly bool) (*pkgmodelquestions.Assessment, error)

	// GetMyAnswer returns the last saved submitting answer for the session, or
	// (nil, nil) if no answer has been submitted yet.
	GetMyAnswer(ctx context.Context, examId ExamSessionId, userId string) (*pkgmodelquestions.ExamAnswer, error)
}

// OnMemoryExamServer implements ExamServer as a single CSP actor.
//
// All mutable state lives in sessionsStore, and sessionsStore is owned by
// exactly one goroutine: the one running Run. Every public method hands a
// closure to that goroutine through serviceChan; the closure executes with
// exclusive access to the map. No mutexes (other than the closeDoer used to
// make Shutdown idempotent) guard session state.
//
// The server holds no question bank and no RNG: both live per session (see
// OnMemoryExamSession), supplied through StartNewExamSession. Each session's RNG is
// only ever touched inside closures run by the actor goroutine, so it remains
// lock-free.
type OnMemoryExamServer struct {
	// sessionsStore is only ever read or written inside closures run by the
	// actor goroutine.
	sessionsStore map[ExamSessionId]*OnMemoryExamSession

	// userSessions is the reverse index of sessionsStore: it maps a user id
	// to the set of exam ids that user has started, so that ListExamSessions
	// is O(sessions-for-user) instead of scanning every session. Like
	// sessionsStore, it is only ever touched inside closures run by the actor
	// goroutine, so it stays in lock-step with sessionsStore without any
	// locking of its own.
	userSessions map[string]map[ExamSessionId]struct{}

	// serviceChan carries closures for the actor goroutine to run.
	serviceChan chan func()

	// done is closed by Shutdown to release the actor loop and any callers
	// blocked dispatching a command.
	done chan struct{}

	closeDoer sync.Once

	// examTrackingServer persists finished exam reports. It is safe to call
	// from any goroutine.
	examTrackingServer examreport.ExamTrackingServer

	// notifiers are the messaging notification services that are asked to
	// deliver a notification when an exam session starts. It may be empty.
	notifiers []msgnotify.MsgNotifySvc
}

// NewOnMemoryExamServer constructs an in-memory exam server. The exam options,
// question bank, and RNG are all supplied per exam via StartNewExamSession, not held by
// the server. examTrackingServer is the sink to which finished exam reports are
// persisted. notifiers is the list of messaging notification services notified
// when an exam session starts; it may be nil or empty, in which case no
// notifications are sent.
func NewOnMemoryExamServer(examTrackingServer examreport.ExamTrackingServer, notifiers []msgnotify.MsgNotifySvc) *OnMemoryExamServer {
	return &OnMemoryExamServer{
		sessionsStore:      make(map[ExamSessionId]*OnMemoryExamSession),
		userSessions:       make(map[string]map[ExamSessionId]struct{}),
		serviceChan:        make(chan func()),
		done:               make(chan struct{}),
		examTrackingServer: examTrackingServer,
		notifiers:          notifiers,
	}
}

// notificationSender is the reply-to address the OnMemoryExamServer uses for
// the notifications it sends.
var notificationSender = msgnotify.AddrId{
	AddressFamily: msgnotify.MsgNotifyAddrFamilyService,
	Address:       msgnotify.WellKnownAddrServiceOnMemoryExamSessionServer,
}

// notificationRecipient is the address notifications are addressed to. The
// exam session server does not know — and never cares — who the final
// recipient is: its job is only to hand the message to the next hop, the
// notifiers that claim the address (AreYou == true).
var notificationRecipient = msgnotify.AddrId{
	AddressFamily: msgnotify.MsgNotifyAddrFamilyService,
	Address:       "",
}

// notifySessionStarted spams a session-started notification through every
// notifier that accepts both the sender and the recipient address families
// and claims the recipient address when probed with AreYou. Delivery is
// best-effort: Send errors are logged and never fail the session operation.
func (srv *OnMemoryExamServer) notifySessionStarted(ctx context.Context, userId string, sessionId ExamSessionId, exam *pkgmodelquestions.Exam) {
	sender := notificationSender
	sender.Tags = msgnotify.AssociationsList{
		msgnotify.MakeLabelKey(msgnotify.WellKnownLabelKeyMsgSource, msgnotify.WellKnownLabelValueExamSessionServer),
		msgnotify.MakeLabelKey(msgnotify.WellKnownLabelKeyExamTakerSubjectId, userId),
	}

	msg := msgnotify.Msg{
		Id:      uuid.NewString(),
		Created: time.Now().UnixMilli(),
		Title:   "Exam session started",
		Level:   msgnotify.MessageLevelCommon,
		Text:    fmt.Sprintf("User %s started exam session %s for exam %s.", userId, sessionId, exam.Id),
	}
	for _, svc := range srv.notifiers {
		if !acceptsAddrFamily(svc.GetAcceptedSenderAddressFamilies(), sender.AddressFamily) {
			continue
		}
		if !acceptsAddrFamily(svc.GetAcceptedRecipientAddressFamilies(), notificationRecipient.AddressFamily) {
			continue
		}
		// Confirm with the service that it is the recipient before emitting.
		if !svc.AreYou(notificationRecipient) {
			continue
		}
		if err := svc.Send(ctx, sender, notificationRecipient, msg); err != nil {
			slog.WarnContext(ctx, "examserver: notification delivery failed",
				"messageId", msg.Id, "to", notificationRecipient, "error", err)
		}
	}
}

// acceptsAddrFamily reports whether family is present in families.
func acceptsAddrFamily(families []msgnotify.MsgNotifyAddrFamily, family msgnotify.MsgNotifyAddrFamily) bool {
	for _, f := range families {
		if f == family {
			return true
		}
	}
	return false
}

// Run is the actor loop. Run it in its own goroutine; it returns when ctx is
// canceled or Shutdown is called, after which method calls report errShuttingDown.
func (srv *OnMemoryExamServer) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-srv.done:
			return
		case cmd := <-srv.serviceChan:
			cmd()
		}
	}
}

// Shutdown stops the actor. Idempotent: repeated calls are no-ops via closeDoer.
func (srv *OnMemoryExamServer) Shutdown() {
	srv.closeDoer.Do(func() {
		close(srv.done)
	})
}

// dispatch delivers cmd to the actor goroutine. Because serviceChan is
// unbuffered, a nil return guarantees the actor received cmd and will run it to
// completion, so the caller may safely wait on its response channel.
func (srv *OnMemoryExamServer) dispatch(ctx context.Context, cmd func()) error {
	select {
	case srv.serviceChan <- cmd:
		return nil
	case <-srv.done:
		return errShuttingDown
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (srv *OnMemoryExamServer) StartNewExamSession(ctx context.Context, exam *pkgmodelquestions.Exam, userId string, taker examreport.Person, examOptions ExamOptions, acceptQuestionTypes []pkgmodelquestions.QuestionType) (ExamSessionId, error) {
	type result struct {
		examId ExamSessionId
		err    error
	}
	resp := make(chan result, 1)
	cmd := func() {
		if exam == nil || len(exam.QuestionSet.QuestionCollections) == 0 {
			resp <- result{err: errEmptyExam}
			return
		}
		// A certification exam is a proctored, high-stakes assessment: the
		// candidate must answer questions in the fixed order they are served and
		// cannot jump back and forth. Only practice exams may be seekable.
		if examOptions&ExamOptionSeekable != 0 && exam.ExamCategory == pkgmodelquestions.ExamCategoryCertification {
			resp <- result{err: errSeekableNotAllowed}
			return
		}
		opts := examOptions
		// A visitor session has no email address the exam report could be
		// delivered to: mask the mailing consent off outright instead of
		// carrying a consent that can never be honored.
		if strings.HasPrefix(userId, auth.VisitorSubjectPrefix) {
			opts &^= ExamOptionSendExamReportEmail
		}
		examId := ExamSessionId(uuid.NewString())
		sess := newExamSession(examId, userId, taker, exam, opts, acceptQuestionTypes)
		if len(sess.QuestionPermutation) == 0 {
			// Question-type filtering (or collections that were empty to begin
			// with) left nothing to serve.
			resp <- result{err: errEmptyExam}
			return
		}
		srv.sessionsStore[examId] = sess
		sessions, ok := srv.userSessions[userId]
		if !ok {
			sessions = make(map[ExamSessionId]struct{})
			srv.userSessions[userId] = sessions
		}
		sessions[examId] = struct{}{}
		resp <- result{examId: examId}
	}
	if err := srv.dispatch(ctx, cmd); err != nil {
		return "", err
	}
	select {
	case r := <-resp:
		if r.err == nil {
			// Spam the session-started notification outside the actor
			// goroutine, so delivery never blocks session dispatch.
			srv.notifySessionStarted(ctx, userId, r.examId, exam)
		}
		return r.examId, r.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (srv *OnMemoryExamServer) ListExamSessions(ctx context.Context, userId string) []ExamSessionExcerpt {
	type result struct{ excerpts []ExamSessionExcerpt }
	resp := make(chan result, 1)
	cmd := func() {
		sessions := srv.userSessions[userId]
		excerpts := make([]ExamSessionExcerpt, 0, len(sessions))
		for id := range sessions {
			sess := srv.sessionsStore[id]
			excerpts = append(excerpts, sessionExcerpt(sess))
		}
		resp <- result{excerpts: excerpts}
	}
	if err := srv.dispatch(ctx, cmd); err != nil {
		return nil
	}
	select {
	case r := <-resp:
		return r.excerpts
	case <-ctx.Done():
		return nil
	}
}

func (srv *OnMemoryExamServer) GetExamSessionById(ctx context.Context, examId ExamSessionId, userId string) (ExamSessionExcerpt, error) {
	type result struct {
		excerpt ExamSessionExcerpt
		err     error
	}
	resp := make(chan result, 1)
	cmd := func() {
		sess, ok := srv.sessionsStore[examId]
		if !ok {
			resp <- result{err: errExamNotFound}
			return
		}
		if sess.UserId != userId {
			resp <- result{err: errNotOwner}
			return
		}
		resp <- result{excerpt: sessionExcerpt(sess)}
	}
	if err := srv.dispatch(ctx, cmd); err != nil {
		return ExamSessionExcerpt{}, err
	}
	select {
	case r := <-resp:
		return r.excerpt, r.err
	case <-ctx.Done():
		return ExamSessionExcerpt{}, ctx.Err()
	}
}

// EndExamSession ends the exam session identified by examId for userId.
//
// Before removing the session it persists a full exam report to the
// ExamTrackingServer — but only when the session has a graded assessment (a
// submitted answer). If the user never submitted an answer there is nothing to
// report, so the report step is skipped and the session is deleted directly.
// When a report is generated, the session is removed only if the report is
// successfully persisted; on any failure the session is left intact so the
// caller can retry.
func (srv *OnMemoryExamServer) EndExamSession(ctx context.Context, examId ExamSessionId, userId string) error {
	type result struct{ err error }
	resp := make(chan result, 1)
	cmd := func() {
		sess, ok := srv.sessionsStore[examId]
		if !ok {
			resp <- result{err: errExamNotFound}
			return
		}
		if sess.UserId != userId {
			resp <- result{err: errNotOwner}
			return
		}

		// Before removing the session, persist a full exam report for it — but
		// only when the session has a graded assessment, i.e. the user submitted
		// an answer that can be graded. If the user never submitted, there is no
		// assessment to report, so we skip straight to deletion. When a report is
		// generated, the session is deleted only if the report is successfully
		// persisted; on any failure the session is left intact.
		if sess.ExamAnswer != nil {
			assessment, err := sess.Grader.Grade(sess.ExamAnswer)
			if err != nil {
				resp <- result{err: fmt.Errorf("end exam session %q: grade submitted answer: %w", examId, err)}
				return
			}
			taker := examreport.ExamTaker{Anonymous: []examreport.Anonymous{{SessionId: userId}}}
			if sess.Taker != (examreport.Person{}) {
				taker.Persons = []examreport.Person{sess.Taker}
			}
			report := newExamReport(
				taker,
				sess.Exam,
				sess.ExamId,
				assessment,
			)
			if err := srv.examTrackingServer.Put(ctx, userId, report, sess.Options&ExamOptionSendExamReportEmail != 0); err != nil {
				resp <- result{err: fmt.Errorf("end exam session %q: persist exam report: %w", examId, err)}
				return
			}
		}

		delete(srv.sessionsStore, examId)
		if sessions, ok := srv.userSessions[sess.UserId]; ok {
			delete(sessions, examId)
			if len(sessions) == 0 {
				delete(srv.userSessions, sess.UserId)
			}
		}
		resp <- result{}
	}
	if err := srv.dispatch(ctx, cmd); err != nil {
		return err
	}
	select {
	case r := <-resp:
		return r.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (srv *OnMemoryExamServer) GetNextQuestion(ctx context.Context, examId ExamSessionId, userId string, cursor *QuestionCursor) (question *pkgmodelquestions.Question, nextCursor *QuestionCursor, err error) {
	type result struct {
		question   *pkgmodelquestions.Question
		nextCursor *QuestionCursor
		err        error
	}
	resp := make(chan result, 1)
	cmd := func() {
		sess, ok := srv.sessionsStore[examId]
		if !ok {
			resp <- result{err: errExamNotFound}
			return
		}
		if sess.UserId != userId {
			resp <- result{err: errNotOwner}
			return
		}
		idx := 0
		if cursor != nil {
			if idx, ok = sess.Cursors[string(*cursor)]; !ok {
				resp <- result{err: errInvalidCursor}
				return
			}
		}
		perm := sess.QuestionPermutation
		if idx < 0 || idx >= len(perm) {
			// No more questions: nil question, nil cursor, no error.
			resp <- result{}
			return
		}
		question = sess.cachedQuestion(perm[idx])
		sess.CurrentQuestionIndex = idx
		sess.CurrentQuestion = question
		if idx+1 < len(perm) {
			// The cursor's meaning is unchanged ("next question to read"), so
			// advance it in place instead of minting a new token. On the very
			// first call the incoming cursor is nil, so one is created.
			c := cursor
			if c == nil {
				fresh := QuestionCursor(uuid.NewString())
				c = &fresh
			}
			sess.Cursors[string(*c)] = idx + 1
			nextCursor = c
		}
		resp <- result{question: question, nextCursor: nextCursor}
	}
	if err := srv.dispatch(ctx, cmd); err != nil {
		return nil, nil, err
	}
	select {
	case r := <-resp:
		return r.question, r.nextCursor, r.err
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}

func (srv *OnMemoryExamServer) SeekCursorTo(ctx context.Context, examId ExamSessionId, userId string, cursor *QuestionCursor, newIndex int) (*QuestionCursor, error) {
	type result struct {
		cursor *QuestionCursor
		err    error
	}
	resp := make(chan result, 1)
	cmd := func() {
		sess, ok := srv.sessionsStore[examId]
		if !ok {
			resp <- result{err: errExamNotFound}
			return
		}
		if sess.UserId != userId {
			resp <- result{err: errNotOwner}
			return
		}
		if sess.Options&ExamOptionSeekable == 0 {
			resp <- result{err: errNotSeekable}
			return
		}
		if newIndex < 0 || newIndex >= len(sess.QuestionPermutation) {
			resp <- result{err: errOutOfRange}
			return
		}
		// Seeking repositions traversal: mint a fresh cursor and invalidate
		// the old one so it can no longer be used.
		newId := uuid.NewString()
		sess.Cursors[newId] = newIndex
		if cursor != nil {
			delete(sess.Cursors, string(*cursor))
		}
		c := QuestionCursor(newId)
		resp <- result{cursor: &c}
	}
	if err := srv.dispatch(ctx, cmd); err != nil {
		return nil, err
	}
	select {
	case r := <-resp:
		return r.cursor, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// SubmitAnswer grades the submitted answer against the session's question
// collection via the session's Grader. When checkOnly is false the answer is
// also stored on the session as the latest submission; when true it is graded
// but not persisted, enabling a preview of the score.
func (srv *OnMemoryExamServer) SubmitAnswer(ctx context.Context, examId ExamSessionId, userId string, answer *pkgmodelquestions.ExamAnswer, checkOnly bool) (*pkgmodelquestions.Assessment, error) {
	type result struct {
		assessment *pkgmodelquestions.Assessment
		err        error
	}
	resp := make(chan result, 1)
	cmd := func() {
		sess, ok := srv.sessionsStore[examId]
		if !ok {
			resp <- result{err: errExamNotFound}
			return
		}
		if sess.UserId != userId {
			resp <- result{err: errNotOwner}
			return
		}
		assessment, err := sess.Grader.Grade(answer)
		if err != nil {
			resp <- result{err: err}
			return
		}
		if !checkOnly {
			sess.ExamAnswer = answer
		}
		resp <- result{assessment: assessment}
	}
	if err := srv.dispatch(ctx, cmd); err != nil {
		return nil, err
	}
	select {
	case r := <-resp:
		return r.assessment, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// GetMyAnswer returns the last saved submitting answer for the session. If no
// answer has been submitted yet it returns (nil, nil).
func (srv *OnMemoryExamServer) GetMyAnswer(ctx context.Context, examId ExamSessionId, userId string) (*pkgmodelquestions.ExamAnswer, error) {
	type result struct {
		answer *pkgmodelquestions.ExamAnswer
		err    error
	}
	resp := make(chan result, 1)
	cmd := func() {
		sess, ok := srv.sessionsStore[examId]
		if !ok {
			resp <- result{err: errExamNotFound}
			return
		}
		if sess.UserId != userId {
			resp <- result{err: errNotOwner}
			return
		}
		resp <- result{answer: sess.ExamAnswer}
	}
	if err := srv.dispatch(ctx, cmd); err != nil {
		return nil, err
	}
	select {
	case r := <-resp:
		return r.answer, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type OnMemoryQuestion struct {
	Question *pkgmodelquestions.Question

	// a nil Permutation is identical to the identical permutation: id: x -> x
	OptionPermutation []int
}

// the singleton OnMemoryExamServer should be the sole ownership holder of every OnMemoryExamSession
type OnMemoryExamSession struct {
	ExamId ExamSessionId
	UserId string

	// Taker is the exam taker's profile, tied to the session at start and
	// injected into the exam report as the <examtaker> <person> when the
	// session ends; a zero Person keeps the report's exam taker anonymous.
	Taker examreport.Person

	Exam      *pkgmodelquestions.Exam
	Questions *pkgmodelquestions.QuestionCollection

	// Grader grades submissions against this session's question collection;
	// it is instantiated once the collection is resolved at session start.
	Grader SimpleGrader

	// StartedAt is the millisecond-resolution unix timestamp captured when the
	// session was created; it is surfaced unchanged through ListExamSessions.
	StartedAt uint64

	// Options captured for this exam; drives seekable checks and option
	// shuffling when a question is first materialized.
	Options ExamOptions

	// this would be the map of virtual question index to actual question index
	// a nil Permutation is identical to the identical permutation: id: x -> x
	QuestionPermutation []int

	// map[questionId]OnMemoryQuestion, you should always try this first, since it stored the question with shuffuled options order.
	CachedQuestion map[string]OnMemoryQuestion

	// for OnMemoryExamServer/OnMemoryExamSession, cursor id should be uuid
	Cursors map[string]int

	// CurrentQuestionIndex is the virtual index of the question most recently
	// served by GetNextQuestion. It starts at -1 (no question fetched yet) and
	// is advanced each time GetNextQuestion serves a question. It is owned by
	// the actor goroutine like all other session state.
	CurrentQuestionIndex int

	// CurrentQuestion is the question most recently served by GetNextQuestion.
	// It has the same lifetime and ownership semantics as CurrentQuestionIndex:
	// nil until the first question has been fetched, then updated in lock-step
	// with CurrentQuestionIndex each time GetNextQuestion serves a question.
	CurrentQuestion *pkgmodelquestions.Question

	// rng is the per-session random source, used to shuffle the question order
	// and each question's options. It is owned by the actor goroutine (touched
	// only inside dispatch closures) and is therefore lock-free.
	rng *rand.Rand

	// virtualCollection records whether this session's question collection was
	// assembled from the exam's virtual collection. Such sessions forcibly
	// randomize question and option order regardless of the requested options.
	virtualCollection bool

	// ExamAnswer holds the user's latest submitted answer (the parsed <examanswer>
	// element; see exam1.xml for its shape). When SubmitAnswer is called with
	// checkOnly set to true, this field is not updated.
	ExamAnswer *pkgmodelquestions.ExamAnswer
}

// cachedQuestion returns the question at actualIdx, building and caching a
// copy with shuffled options and a stripped correct answer on first access.
// The session's rng is used, which is owned by the actor goroutine and
// therefore single-threaded.
func (sess *OnMemoryExamSession) cachedQuestion(actualIdx int) *pkgmodelquestions.Question {
	orig := &sess.Questions.Questions[actualIdx]
	if cached, ok := sess.CachedQuestion[orig.Id]; ok {
		return cached.Question
	}
	// A virtual-collection session randomizes option order even when the
	// caller did not request ExamOptionRandomOptions.
	opts := sess.Options
	if sess.virtualCollection {
		opts |= ExamOptionRandomOptions
	}
	omq := buildOnMemoryQuestion(orig, opts, sess.rng)
	sess.CachedQuestion[orig.Id] = omq
	return omq.Question
}

// newExamSession allocates a session backed by exam's question set, selecting
// the question collection to present, dropping questions whose type the caller
// does not accept, and computing the question permutation up front (the order
// in which questions are presented). Option permutations are derived lazily,
// per question, as it is first requested.
func newExamSession(examId ExamSessionId, userId string, taker examreport.Person, exam *pkgmodelquestions.Exam, opts ExamOptions, acceptQuestionTypes []pkgmodelquestions.QuestionType) *OnMemoryExamSession {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	qc, virtual := selectQuestionCollection(exam, opts, rng)
	qc.Questions = filterQuestionsByType(qc.Questions, acceptQuestionTypes)
	n := len(qc.Questions)
	var qPerm []int
	// A session backed by a virtual collection serves its sampled questions in
	// random order even when ExamOptionRandomQuestions was not requested.
	if virtual || opts&ExamOptionRandomQuestions != 0 {
		qPerm = rng.Perm(n)
	} else {
		qPerm = identityPermutation(n)
	}
	return &OnMemoryExamSession{
		ExamId:               examId,
		UserId:               userId,
		Taker:                taker,
		Exam:                 exam,
		StartedAt:            uint64(time.Now().UnixMilli()),
		Questions:            &qc,
		Options:              opts,
		QuestionPermutation:  qPerm,
		CachedQuestion:       make(map[string]OnMemoryQuestion),
		Cursors:              make(map[string]int),
		CurrentQuestionIndex: -1,
		rng:                  rng,
		virtualCollection:    virtual,
		Grader:               NewSimpleOnMemoryGrader(&qc, exam.PassingScore, exam.ExamCategory),
	}
}

// selectQuestionCollection resolves which QuestionCollection from the exam's
// question set is presented to a candidate, and reports whether that
// collection was assembled from the exam's virtual collection.
//
// A certification exam's virtual collection always wins when present: the
// collection is assembled on the fly by sampling from the union of the
// referenced real collections, and ExamOptionRandomQuestionColl is moot — no
// real collection is ever chosen. Practice exams ignore any virtual
// collection (the loader already rejects such documents; this is defense in
// depth for programmatically built exams).
//
// Otherwise, with ExamOptionRandomQuestionColl set, one collection is picked
// at random (the point of a multi-collection set: vary the exam by drawing a
// different subset). Otherwise every collection's questions are flattened
// into a single combined collection, so the candidate sees all questions.
func selectQuestionCollection(exam *pkgmodelquestions.Exam, opts ExamOptions, rng *rand.Rand) (pkgmodelquestions.QuestionCollection, bool) {
	cols := exam.QuestionSet.QuestionCollections
	if vc := exam.VirtualCollection; vc != nil && exam.ExamCategory == pkgmodelquestions.ExamCategoryCertification {
		return sampleVirtualCollection(vc, cols, rng), true
	}
	if len(cols) == 0 {
		return pkgmodelquestions.QuestionCollection{}, false
	}
	if opts&ExamOptionRandomQuestionColl != 0 {
		return cols[rng.Intn(len(cols))], false
	}
	// Flatten all collections into one so the candidate sees every question.
	var total int
	for _, c := range cols {
		total += len(c.Questions)
	}
	flat := make([]pkgmodelquestions.Question, 0, total)
	for _, c := range cols {
		flat = append(flat, c.Questions...)
	}
	return pkgmodelquestions.QuestionCollection{Questions: flat}, false
}

// sampleVirtualCollection draws SampleSize distinct questions at random from
// the union of the referenced collections' questions. The draw order is
// already random; the session additionally shuffles the serve order. The
// loader guarantees the population covers SampleSize and every index is in
// range; out-of-range indices are skipped here so a programmatically built
// exam degrades to a smaller sample instead of panicking.
func sampleVirtualCollection(vc *pkgmodelquestions.VirtualCollection, cols []pkgmodelquestions.QuestionCollection, rng *rand.Rand) pkgmodelquestions.QuestionCollection {
	var total int
	for _, idx := range vc.CollectionIdx {
		if idx >= 0 && idx < len(cols) {
			total += len(cols[idx].Questions)
		}
	}
	pool := make([]pkgmodelquestions.Question, 0, total)
	for _, idx := range vc.CollectionIdx {
		if idx >= 0 && idx < len(cols) {
			pool = append(pool, cols[idx].Questions...)
		}
	}
	n := min(vc.SampleSize, len(pool))
	sampled := make([]pkgmodelquestions.Question, 0, n)
	for _, i := range rng.Perm(len(pool))[:n] {
		sampled = append(sampled, pool[i])
	}
	return pkgmodelquestions.QuestionCollection{Questions: sampled}
}

// filterQuestionsByType returns the questions whose type is in accept,
// preserving their order. An empty accept list accepts every type. The input
// slice is never mutated; a fresh slice is returned whenever filtering
// applies.
func filterQuestionsByType(questions []pkgmodelquestions.Question, accept []pkgmodelquestions.QuestionType) []pkgmodelquestions.Question {
	if len(accept) == 0 {
		return questions
	}
	accepted := make(map[pkgmodelquestions.QuestionType]struct{}, len(accept))
	for _, t := range accept {
		accepted[t] = struct{}{}
	}
	filtered := make([]pkgmodelquestions.Question, 0, len(questions))
	for _, q := range questions {
		if _, ok := accepted[q.Type]; ok {
			filtered = append(filtered, q)
		}
	}
	return filtered
}

// sessionExcerpt projects a session into an ExamSessionExcerpt. Unlike
// pkgmodelquestions.ExamExcerptFrom, which derives its counts from the exam
// document's first collection, NumQuestions and TotalScores describe the
// session's actual question set — after collection selection and
// question-type filtering — so clients can rely on NumQuestions to bound
// navigation.
func sessionExcerpt(sess *OnMemoryExamSession) ExamSessionExcerpt {
	excerpt := pkgmodelquestions.ExamExcerptFrom(sess.Exam)
	excerpt.NumQuestions = len(sess.Questions.Questions)
	excerpt.TotalScores = 0
	for _, q := range sess.Questions.Questions {
		excerpt.TotalScores += q.Score
	}
	return ExamSessionExcerpt{
		Id:                   sess.ExamId,
		ExamExcerpt:          excerpt,
		StartedAt:            sess.StartedAt,
		Options:              sess.Options,
		CurrentQuestionIndex: sess.CurrentQuestionIndex,
		CurrentQuestion:      sess.CurrentQuestion,
	}
}

// buildOnMemoryQuestion returns a shallow copy of orig whose Options are
// reordered according to the (random or identity) option permutation and whose
// CorrectAnswer is stripped: a served question must never reveal the answer
// key. The only legitimate reveal is the grader's assessment, which embeds the
// origin question (correct answer included) for practice-exam submissions.
// The original question bank is never mutated; option Ids are preserved so a
// submitted answer's option ids still match the grader's own unstripped copy.
func buildOnMemoryQuestion(orig *pkgmodelquestions.Question, opts ExamOptions, rng *rand.Rand) OnMemoryQuestion {
	qCopy := *orig
	qCopy.CorrectAnswer = pkgmodelquestions.CorrectAnswer{}
	m := len(orig.Options)
	if m == 0 {
		return OnMemoryQuestion{Question: &qCopy, OptionPermutation: identityPermutation(0)}
	}
	var optPerm []int
	if opts&ExamOptionRandomOptions != 0 {
		optPerm = rng.Perm(m)
	} else {
		optPerm = identityPermutation(m)
	}
	reordered := make(pkgmodelquestions.Options, m)
	for i, p := range optPerm {
		reordered[i] = orig.Options[p]
	}
	qCopy.Options = reordered
	return OnMemoryQuestion{Question: &qCopy, OptionPermutation: optPerm}
}

// newExamReport assembles an examreport.ExamReport for a finished exam
// session. The exam metadata (id, short name, code, title, description, passing
// score, category) is copied from exam; ExamSessionId is carried over from the
// session; FinishedAt records the moment the report is generated, as a
// millisecond-resolution unix timestamp; a fresh globally-unique report Id is
// minted. The examTaker and assessment are taken verbatim from the arguments;
// assessment must not be nil.
func newExamReport(examTaker examreport.ExamTaker, exam *pkgmodelquestions.Exam, examSessionId ExamSessionId, assessment *pkgmodelquestions.Assessment) examreport.ExamReport {
	return examreport.ExamReport{
		Id:            uuid.NewString(),
		ExamTaker:     examTaker,
		ExamId:        exam.Id,
		ExamShortName: exam.ShortName,
		ExamCode:      exam.Code,
		Title:         string(exam.Title),
		Description:   string(exam.Description),
		PassingScore:  exam.PassingScore,
		ExamCategory:  exam.ExamCategory,
		ExamSessionId: string(examSessionId),
		FinishedAt:    time.Now().UnixMilli(),
		Assessment:    *assessment,
	}
}

// identityPermutation returns [0, 1, ..., n-1], the identical permutation,
// generated programmatically.
func identityPermutation(n int) []int {
	p := make([]int, n)
	for i := range p {
		p[i] = i
	}
	return p
}
