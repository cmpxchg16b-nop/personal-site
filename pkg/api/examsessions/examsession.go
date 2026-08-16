// Package examsessions implements the /api/examsessions HTTP endpoint, exposing
// CRUD over exam sessions scoped to the caller (by subject/user id).
//
//	POST   /api/examsessions           create a session for the exam whose id is
//	        given in the request body as {"exam_id": "...", "options": <n>,
//	        "accept_question_types": ["single-choice", ...]}; "options" is an
//	        examserver.ExamOptions bitmask applied to the new session (0 when
//	        absent); "accept_question_types" restricts which question types the
//	        session serves (absent or empty accepts every type); returns the new
//	        session id as {"exam_session_id": "..."}.
//	GET    /api/examsessions           list the caller's sessions as
//	        {"exam_sessions": [{...}, ...]}.
//	GET    /api/examsessions/{exam_session_id} fetch a single session as
//	        {"exam_session": {...}}, including its current question index.
//	DELETE /api/examsessions/{exam_session_id} terminate the named session.
//	GET    /api/examsessions/{exam_session_id}/my_answer fetch the caller's
//	        last saved submitting answer via GetMyAnswer; responds
//	        {"exam_answer": {...}} or {"exam_answer": null} when no answer
//	        has been submitted.
//	POST   /api/examsessions/{exam_session_id}/answer[?check_only=true]
//	        grade the body (an ExamAnswer) via SubmitAnswer; responds
//	        {"assessment": {...}}. With check_only=true the answer is graded
//	        but not persisted.
//	GET    /api/examsessions/{exam_session_id}/questions?cursor_id=<cursor>
//	        fetch the next question via GetNextQuestion; responds
//	        {"cursor_id": <next or null>, "question": {...} or null}.
//	PUT    /api/examsessions/{exam_session_id}/cursors?cursor_id=<cursor>&index=<n>
//	        reposition the cursor via SeekCursorTo; responds {"cursor_id": <new>}.
//
// Mount it as a subtree so the handler receives every path beneath
// /api/examsessions and resolves the routes internally, e.g.:
//
//	mux.Handle("/api/examsessions", h)
//	mux.Handle("/api/examsessions/", h)
//
// The caller's session is resolved from the request context via the
// SessionManager (see package session) and its subject id (user id) is used to
// scope all operations; the session must already be attached by the session
// middleware.
package examsessions

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"personal-site/pkg/models/examreport"
	"personal-site/pkg/models/examserver"
	"personal-site/pkg/models/question"
	"personal-site/pkg/session"
)

// maxBodyBytes bounds the size of a POST body. A session-creation request is a
// small JSON object, so this comfortably rejects oversized payloads.
const maxBodyBytes = 1 << 20 // 1 MiB

// ExamSessionHandler serves the /api/examsessions API. It resolves the exam
// document to run from an ExamRepository and drives session lifecycle through an
// ExamServer, both scoped to the caller (by subject/user id).
type ExamSessionHandler struct {
	sm     session.SessionManager
	server examserver.ExamServer
	repo   *question.ExamRepository
}

// NewExamSessionHandler constructs an ExamSessionHandler. sm resolves the
// caller's session from the request context and uses its subject id (user id) to
// scope operations; server manages exam session lifecycle; repo looks up the exam
// document to run when a session is created.
func NewExamSessionHandler(sm session.SessionManager, server examserver.ExamServer, repo *question.ExamRepository) *ExamSessionHandler {
	return &ExamSessionHandler{sm: sm, server: server, repo: repo}
}

// createRequest is the JSON body of a POST /api/examsessions request. Options
// is the examserver.ExamOptions bitmask applied to the new session; it is 0
// when the field is absent, meaning questions and options are presented in
// document order and the session is not seekable. AcceptQuestionTypes
// restricts which question types the session serves; it accepts every type
// when absent or empty.
type createRequest struct {
	ExamID              string                  `json:"exam_id"`
	Options             examserver.ExamOptions  `json:"options"`
	AcceptQuestionTypes []question.QuestionType `json:"accept_question_types"`
}

// createResponse is the JSON body of a successful POST.
type createResponse struct {
	ExamSessionID string `json:"exam_session_id"`
}

// listResponse is the JSON body of a successful GET.
type listResponse struct {
	ExamSessions []examSessionSummary `json:"exam_sessions"`
}

// examSessionSummary is one entry in a list response: the session id, the
// projected exam metadata, when the session was started, the ExamOptions
// bitmask the session was created with, and the virtual index of the question
// most recently served by GetNextQuestion (-1 before the first question has
// been fetched).
type examSessionSummary struct {
	ExamSessionID        string                       `json:"exam_session_id"`
	ExamExcerpt          question.ExamDocumentExcerpt `json:"exam_excerpt"`
	StartedAt            uint64                       `json:"started_at"`
	Options              examserver.ExamOptions       `json:"options"`
	CurrentQuestionIndex int                          `json:"current_question_index"`
	CurrentQuestion      *question.Question           `json:"current_question"`
}

// sessionResponse is the JSON body of a successful GET /{exam_session_id}:
// the single session, wrapped so the resource can grow independently of the
// list shape.
type sessionResponse struct {
	ExamSession examSessionSummary `json:"exam_session"`
}

// toSummary projects an examserver.ExamSessionExcerpt into the wire shape used
// by both the list and single-session responses.
func toSummary(e examserver.ExamSessionExcerpt) examSessionSummary {
	return examSessionSummary{
		ExamSessionID:        string(e.Id),
		ExamExcerpt:          e.ExamExcerpt,
		StartedAt:            e.StartedAt,
		Options:              e.Options,
		CurrentQuestionIndex: e.CurrentQuestionIndex,
		CurrentQuestion:      e.CurrentQuestion,
	}
}

// nextQuestionResponse is the JSON body of a successful GET .../questions. Both
// fields are null when the session has no more questions.
type nextQuestionResponse struct {
	CursorID *string            `json:"cursor_id"`
	Question *question.Question `json:"question"`
}

// seekCursorResponse is the JSON body of a successful PUT .../cursors.
type seekCursorResponse struct {
	CursorID string `json:"cursor_id"`
}

// myAnswerResponse is the JSON body of a successful GET .../my_answer. The
// exam_answer field is null when no answer has been submitted yet.
type myAnswerResponse struct {
	ExamAnswer *question.ExamAnswer `json:"exam_answer"`
}

// submitAnswerResponse is the JSON body of a successful POST .../answer: the
// grading assessment, wrapped so the resource shape can grow independently.
type submitAnswerResponse struct {
	Assessment *question.Assessment `json:"assessment"`
}

// apiPrefix is the path the handler is mounted under. Every route beneath it is
// resolved inside ServeHTTP, so the handler owns its own route tree rather than
// relying on the ServeMux's wildcard captures.
const apiPrefix = "/api/examsessions"

// ServeHTTP routes the request by parsing the path beneath apiPrefix. The
// handler is mounted as a subtree and resolves the collection root, a single
// item, and the questions/cursors sub-resources itself:
//
//	""                          -> collection (POST create, GET list)
//	"/{exam_session_id}"            -> item (GET single, DELETE)
//	"/{exam_session_id}/questions"  -> next question (GET)
//	"/{exam_session_id}/cursors"    -> seek cursor (PUT)
//	"/{exam_session_id}/answer"     -> submit/grade answer (POST)
//	"/{exam_session_id}/my_answer"  -> last saved answer (GET)
//
// Anything else beneath the prefix responds 404.
func (h *ExamSessionHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sess, ok := h.sm.GetSessionFromContext(r.Context())
	if !ok {
		http.Error(w, "session not found", http.StatusInternalServerError)
		return
	}

	// segments is the path beneath /api/examsessions split on '/', e.g.
	// [] (collection), [{exam_session_id}], or [{exam_session_id}, "questions"].
	rel := strings.TrimPrefix(r.URL.Path, apiPrefix)
	trimmed := strings.Trim(rel, "/")
	var segments []string
	if trimmed != "" {
		segments = strings.Split(trimmed, "/")
	}

	switch {
	case len(segments) == 0:
		// Collection: POST create, GET list.
		switch r.Method {
		case http.MethodPost:
			h.handleCreate(w, r, sess)
		case http.MethodGet:
			h.handleList(w, r, sess.SubjectId())
		default:
			h.methodNotAllowed(w, "GET, POST")
		}
	case len(segments) == 1:
		// Item: GET single session, DELETE terminate.
		switch r.Method {
		case http.MethodGet:
			h.handleGetSession(w, r, examserver.ExamSessionId(segments[0]), sess.SubjectId())
		case http.MethodDelete:
			h.handleDelete(w, r, examserver.ExamSessionId(segments[0]), sess.SubjectId())
		default:
			h.methodNotAllowed(w, "GET, DELETE")
		}
	case len(segments) == 2 && segments[1] == "questions":
		if r.Method != http.MethodGet {
			h.methodNotAllowed(w, "GET")
			return
		}
		h.handleGetNextQuestion(w, r, examserver.ExamSessionId(segments[0]), sess.SubjectId())
	case len(segments) == 2 && segments[1] == "cursors":
		if r.Method != http.MethodPut {
			h.methodNotAllowed(w, "PUT")
			return
		}
		h.handleSeekCursor(w, r, examserver.ExamSessionId(segments[0]), sess.SubjectId())
	case len(segments) == 2 && segments[1] == "answer":
		if r.Method != http.MethodPost {
			h.methodNotAllowed(w, "POST")
			return
		}
		h.handleSubmitAnswer(w, r, examserver.ExamSessionId(segments[0]), sess.SubjectId())
	case len(segments) == 2 && segments[1] == "my_answer":
		if r.Method != http.MethodGet {
			h.methodNotAllowed(w, "GET")
			return
		}
		h.handleGetMyAnswer(w, r, examserver.ExamSessionId(segments[0]), sess.SubjectId())
	default:
		http.NotFound(w, r)
	}
}

// handleCreate starts a new exam session for the exam document named in the
// request body. The caller's session supplies both the user id (subject id)
// scoping the session and the exam taker's profile (username, email), which
// the exam server ties to the session and injects into the exam report as the
// <examtaker> person when the session ends.
func (h *ExamSessionHandler) handleCreate(w http.ResponseWriter, r *http.Request, sess *session.Session) {
	userId := sess.SubjectId()
	req, ok := decodeCreate(r)
	if !ok {
		http.Error(w, `invalid request body: expected {"exam_id": "..."}`, http.StatusBadRequest)
		return
	}
	if req.ExamID == "" {
		http.Error(w, "exam_id is required", http.StatusBadRequest)
		return
	}
	for _, t := range req.AcceptQuestionTypes {
		if !t.Valid() {
			http.Error(w, "invalid question type: "+string(t), http.StatusBadRequest)
			return
		}
	}

	ctx := r.Context()

	exam, err := h.repo.GetExamDocumentById(ctx, req.ExamID, userId)
	if err != nil {
		// GetExamDocumentById only fails when the exam is unavailable, so map the
		// lot to 404 rather than surfacing the repository internals.
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	// Mirror examserver's emptiness guard so an exam with no questions is
	// reported as a client error (400) rather than a server error.
	if len(exam.QuestionSet.QuestionCollections) == 0 {
		http.Error(w, "exam has no questions", http.StatusBadRequest)
		return
	}
	// Likewise reject up front when no question in the exam matches the
	// caller's accepted types, instead of letting the session creation fail
	// as empty server-side.
	if len(req.AcceptQuestionTypes) > 0 && !examHasAnyType(exam, req.AcceptQuestionTypes) {
		http.Error(w, "exam has no questions of the accepted types", http.StatusBadRequest)
		return
	}

	// The exam taker's profile rides with the caller's session: the exam
	// server injects it into the exam report on session end, which is how the
	// report server learns the exam taker's email address.
	taker := examreport.Person{Name: sess.Username(), Email: sess.Email()}
	sessionID, err := h.server.StartNewExamSession(ctx, exam, userId, taker, req.Options, req.AcceptQuestionTypes)
	if err != nil {
		// With the empty-exam case handled above, the realistic remaining
		// failures are the server shutting down or the request being canceled,
		// both of which are transient: 503 asks the client to retry. examserver
		// keeps its sentinels unexported, so finer-grained mapping is not
		// possible here.
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	w.WriteHeader(http.StatusCreated)
	writeJSON(w, createResponse{ExamSessionID: string(sessionID)})
}

// handleList returns the caller's active exam sessions.
func (h *ExamSessionHandler) handleList(w http.ResponseWriter, r *http.Request, userId string) {
	excerpts := h.server.ListExamSessions(r.Context(), userId)
	out := make([]examSessionSummary, 0, len(excerpts))
	for _, e := range excerpts {
		out = append(out, toSummary(e))
	}
	writeJSON(w, listResponse{ExamSessions: out})
}

// handleGetSession returns a single exam session by id, scoped to the caller.
// The caller's user id is forwarded to GetExamSessionById so ownership is
// enforced by the server.
func (h *ExamSessionHandler) handleGetSession(w http.ResponseWriter, r *http.Request, examID examserver.ExamSessionId, userId string) {
	excerpt, err := h.server.GetExamSessionById(r.Context(), examID, userId)
	if err != nil {
		// GetExamSessionById fails when the session is missing or doesn't
		// belong to the caller; both are reported as 404 (examserver keeps its
		// sentinels unexported, so the two can't be told apart here).
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, sessionResponse{ExamSession: toSummary(excerpt)})
}

// handleDelete terminates a single exam session by id. The caller's user id is
// forwarded to EndExamSession so ownership can be enforced by the server.
func (h *ExamSessionHandler) handleDelete(w http.ResponseWriter, r *http.Request, examID examserver.ExamSessionId, userId string) {
	if err := h.server.EndExamSession(r.Context(), examID, userId); err != nil {
		// The only failure is a missing session (or the server shutting down);
		// treat both as not found so a repeated delete converges to 404.
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleGetNextQuestion returns the next question in the session plus the cursor
// to continue from. An absent cursor_id starts from the beginning; when no more
// questions remain, both cursor_id and question are null.
func (h *ExamSessionHandler) handleGetNextQuestion(w http.ResponseWriter, r *http.Request, examID examserver.ExamSessionId, userId string) {
	q, next, err := h.server.GetNextQuestion(r.Context(), examID, userId, parseCursorID(r))
	if err != nil {
		// examserver's sentinels are unexported, so not-found cannot be told
		// apart from an invalid cursor here; surface a generic server error.
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var cursorID *string
	if next != nil {
		s := string(*next)
		cursorID = &s
	}
	writeJSON(w, nextQuestionResponse{CursorID: cursorID, Question: q})
}

// handleSeekCursor repositions the session cursor to a new virtual index and
// returns the fresh cursor to read from. The index is the required "index"
// query parameter.
func (h *ExamSessionHandler) handleSeekCursor(w http.ResponseWriter, r *http.Request, examID examserver.ExamSessionId, userId string) {
	index, ok := parseIndex(r)
	if !ok {
		http.Error(w, `invalid or missing "index" query parameter`, http.StatusBadRequest)
		return
	}
	repositioned, err := h.server.SeekCursorTo(r.Context(), examID, userId, parseCursorID(r), index)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	cursorID := ""
	if repositioned != nil {
		cursorID = string(*repositioned)
	}
	writeJSON(w, seekCursorResponse{CursorID: cursorID})
}

// handleGetMyAnswer returns the caller's last saved submitting answer for the
// session. The caller's user id is forwarded to GetMyAnswer so ownership is
// enforced by the server. A session that has not yet been answered responds
// with a null exam_answer rather than an error.
func (h *ExamSessionHandler) handleGetMyAnswer(w http.ResponseWriter, r *http.Request, examID examserver.ExamSessionId, userId string) {
	ans, err := h.server.GetMyAnswer(r.Context(), examID, userId)
	if err != nil {
		// GetMyAnswer fails when the session is missing or doesn't belong to the
		// caller; both are reported as 404 (examserver keeps its sentinels
		// unexported, so the two can't be told apart here).
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, myAnswerResponse{ExamAnswer: ans})
}

// handleSubmitAnswer grades the submitted answer against the session. With
// check_only=true the answer is graded but not persisted; otherwise it is saved
// as the latest submission (and surfaces later via my_answer). Either way the
// assessment is returned.
func (h *ExamSessionHandler) handleSubmitAnswer(w http.ResponseWriter, r *http.Request, examID examserver.ExamSessionId, userId string) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "failed reading body", http.StatusBadRequest)
		return
	}
	if len(raw) == 0 {
		http.Error(w, `invalid request body: expected an exam answer`, http.StatusBadRequest)
		return
	}
	var answer question.ExamAnswer
	if err := json.Unmarshal(raw, &answer); err != nil {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	assessment, err := h.server.SubmitAnswer(r.Context(), examID, userId, &answer, parseCheckOnly(r))
	if err != nil {
		// examserver's sentinels are unexported, so not-found/owner/grade errors
		// cannot be told apart here; surface a generic server error, matching the
		// questions and cursors endpoints (which face the same ambiguity).
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, submitAnswerResponse{Assessment: assessment})
}

// methodNotAllowed reports 405 with the given methods in the Allow header.
func (h *ExamSessionHandler) methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

// decodeCreate parses and bounds-checks a POST body. An empty body or invalid
// JSON reports ok == false.
func decodeCreate(r *http.Request) (createRequest, bool) {
	var req createRequest
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		return req, false
	}
	if len(raw) == 0 {
		return req, false
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return req, false
	}
	return req, true
}

// examHasAnyType reports whether any question in any of the exam's question
// collections has a type in accept.
func examHasAnyType(exam *question.Exam, accept []question.QuestionType) bool {
	accepted := make(map[question.QuestionType]struct{}, len(accept))
	for _, t := range accept {
		accepted[t] = struct{}{}
	}
	for _, qc := range exam.QuestionSet.QuestionCollections {
		for _, q := range qc.Questions {
			if _, ok := accepted[q.Type]; ok {
				return true
			}
		}
	}
	return false
}

// parseCursorID reads the optional cursor_id query parameter. An empty or
// absent cursor_id yields nil, which ExamServer treats as "start from the
// beginning".
func parseCursorID(r *http.Request) *examserver.QuestionCursor {
	s := r.URL.Query().Get("cursor_id")
	if s == "" {
		return nil
	}
	c := examserver.QuestionCursor(s)
	return &c
}

// parseIndex reads the required "index" query parameter as a non-negative int.
func parseIndex(r *http.Request) (int, bool) {
	s := r.URL.Query().Get("index")
	if s == "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// parseCheckOnly reads the optional check_only query parameter as a bool. An
// absent or unparseable value yields false, so the answer is persisted.
func parseCheckOnly(r *http.Request) bool {
	v, _ := strconv.ParseBool(r.URL.Query().Get("check_only"))
	return v
}

// writeJSON encodes v as the response body.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
