// Package examreport defines the data model for an exam report: the full
// report produced after an exam taker finishes an exam session.
//
// It mirrors the <examreport> and <examtaker> elements defined in exam.xsd.
// Assessment-related types (overall result, scores, assessment) are reused from
// the question package.
package examreport

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	htmltemplate "html/template"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	texttemplate "text/template"
	"time"

	"personal-site/pkg/models/msgnotify"
	pkgmodelsquestion "personal-site/pkg/models/question"
	pkgmodelssigner "personal-site/pkg/models/signer"

	"github.com/beevik/etree"
	"github.com/google/uuid"
)

// ErrExamTrackingNotFound is returned by DeleteExamTracking when the user has
// no exam report with the given id.
var ErrExamTrackingNotFound = errors.New("examreport: exam report not found")

// Person is one named <person> within an <examtaker>: a real exam candidate
// identified by full name. Fistname is spelled as in the XSD attribute. Email
// is the candidate's email address; it lets the exam report server deliver
// the report to the candidate when they consented to mailing.
type Person struct {
	Name     string `xml:"name,attr" json:"name"`
	Fistname string `xml:"fistname,attr" json:"fistname,omitempty"`
	Lastname string `xml:"lastname,attr" json:"lastname,omitempty"`
	Email    string `xml:"email,attr" json:"email,omitempty"`
}

// Anonymous is one <anonymous> entry within an <examtaker>: an unidentified
// exam taker tracked only by session id.
type Anonymous struct {
	SessionId string `xml:"sessionid,attr" json:"sessionId"`
}

// ExamTaker is the <examtaker> element: the list of persons and/or anonymous
// sessions who took the exam. Either may be empty.
type ExamTaker struct {
	XMLName   xml.Name    `xml:"examtaker" json:"-"`
	Persons   []Person    `xml:"person" json:"persons,omitempty"`
	Anonymous []Anonymous `xml:"anonymous" json:"anonymous,omitempty"`
}

// ExamReport is the <examreport> element: a full report sent to the exam
// assessment tracking server after an exam taker has finished the exam session.
type ExamReport struct {
	XMLName xml.Name `xml:"examreport" json:"-"`

	// Id is the id of the exam report; it has to be globally unique, not the id
	// of the exam document, nor the id of the exam session.
	Id string `xml:"id,attr" json:"id"`

	// ExamTaker is the person or anonymous session that took the exam.
	ExamTaker ExamTaker `xml:"examtaker" json:"examTaker"`

	// ExamId is the exam document id, not the exam session id.
	ExamId string `xml:"examid" json:"examId"`

	// ExamShortName is the short name copied from the origin exam document.
	ExamShortName string `xml:"examshortname" json:"examShortName,omitempty"`

	// ExamCode is the code copied from the origin exam document.
	ExamCode string `xml:"examcode" json:"examCode,omitempty"`

	// Title is the title of the exam.
	Title string `xml:"title" json:"title"`

	// Description is the description of the exam. Optional.
	Description string `xml:"description,omitempty" json:"description,omitempty"`

	// PassingScore is the mandated passing score of the exam, copied directly
	// from the exam element.
	PassingScore *float32 `xml:"passingscore" json:"passingScore,omitempty"`

	// ExamCategory is copied directly from the origin exam document too.
	ExamCategory pkgmodelsquestion.ExamCategory `xml:"examcategory" json:"examCategory"`

	// ExamSessionId is the id of the exam session which the exam taker was in.
	ExamSessionId string `xml:"examsessionid" json:"examSessionId"`

	// FinishedAt is the millisecond-resolution unix timestamp when the exam
	// session was finished by the exam taker.
	FinishedAt int64 `xml:"finishedat" json:"finishedAt"`

	// Assessment contains the grade and the score that was achieved by the
	// exam taker.
	Assessment pkgmodelsquestion.Assessment `xml:"assessment" json:"assessment"`
}

// ExamTrackingServer is the server that persists and retrieves exam reports.
// Reports are keyed by the exam taker (userid), so the history of a user's
// finished exam sessions can be looked up via GetExamReportsByUserId.
type ExamTrackingServer interface {
	// Put stores an exam report for the given userid. mailingConsent is the
	// exam taker's consent to the exam report being emailed to the exam
	// taker's email address; it is carried as a label on the notification
	// emitted for the stored report, so downstream messaging can act on it.
	Put(ctx context.Context, userid string, examReport ExamReport, mailingConsent bool) error

	// GetExamReportsByUserId returns all exam reports recorded for userid.
	GetExamReportsByUserId(ctx context.Context, userid string) ([]ExamReport, error)

	// DeleteExamTracking removes the exam report identified by examReportId
	// from userid's reports. It returns ErrExamTrackingNotFound when the user
	// has no report with that id.
	DeleteExamTracking(ctx context.Context, userid string, examReportId string) error
}

// OnMemoryExamTrackingServer is an in-memory, lock-free implementation of
// ExamTrackingServer. It is safe for concurrent use by multiple goroutines.
//
// It uses two sync.Maps:
//
//   - reports maps the synthesized key "{userid}:{index}" to an ExamReport;
//   - counts  maps userid to an int64 holding the number of reports stored for
//     that user.
//
// Put advances a user's count via sync.Map.CompareAndSwap to atomically claim a
// unique index: it reads the current count c, then CAS-advances it to c+1; only
// when the CAS succeeds is index c known to be safe to write. Because the count
// is monotonically increased only by Put (it never decreases), there is no ABA
// problem: each Put observes a strictly increasing index and no report is ever
// overwritten or lost, with no mutex required.
//
// DeleteExamTracking removes a report's map entry but never decrements the
// count, so deletion leaves a hole in the user's index space: Put's invariants
// above are untouched, and Get skips the hole exactly as it skips the
// in-flight window of a concurrent Put.
type OnMemoryExamTrackingServer struct {
	// reports maps "{userid}:{index}" to ExamReport.
	reports sync.Map
	// counts maps userid to int64, the number of reports for that user.
	counts sync.Map

	// notifiers are the messaging notification services that are asked to
	// deliver a notification when an exam report is stored or deleted. It may
	// be empty.
	notifiers []msgnotify.MsgNotifySvc

	// xmlSigner, when non-nil, envelops an XMLDSIG signature into the exam
	// report XML document attached to notifications. When nil, attachments
	// are sent unsigned.
	xmlSigner pkgmodelssigner.XMLETreeSigner
}

// NewOnMemoryExamTrackingServer returns a ready-to-use OnMemoryExamTrackingServer.
// notifiers is the list of messaging notification services notified when an
// exam report is stored (Put) or deleted (DeleteExamTracking); it may be nil
// or empty, in which case no notifications are sent.
//
// xmlSigner is optional: when non-nil, the serialized exam report XML
// document that travels as a notification attachment is re-parsed and
// enveloped-signed (XMLDSIG) with it, so recipients can authenticate the
// report; when nil, attachments are sent unsigned. *dsig.SigningContext from
// github.com/russellhaering/goxmldsig satisfies the interface.
func NewOnMemoryExamTrackingServer(notifiers []msgnotify.MsgNotifySvc, xmlSigner pkgmodelssigner.XMLETreeSigner) *OnMemoryExamTrackingServer {
	return &OnMemoryExamTrackingServer{notifiers: notifiers, xmlSigner: xmlSigner}
}

// notificationSender is the reply-to address the OnMemoryExamTrackingServer
// uses for the notifications it sends.
var notificationSender = msgnotify.AddrId{
	AddressFamily: msgnotify.MsgNotifyAddrFamilyService,
	Address:       msgnotify.WellKnownAddrServiceOnMemoryExamTrackingServer,
}

// notificationRecipient is the address notifications are addressed to. The
// tracking server does not know — and never cares — who the final recipient
// is: its job is only to hand the message to the next hop, the notifiers that
// claim the address (AreYou == true); the lifelong fate of the message is the
// next hop's concern.
var notificationRecipient = msgnotify.AddrId{
	AddressFamily: msgnotify.MsgNotifyAddrFamilyService,
	Address:       "",
}

// notify delivers a notification through every notifier that accepts both the
// sender and the recipient address families and claims the recipient address
// when probed with AreYou, so unsupported services are skipped before Send is
// attempted. Delivery is best-effort: Send errors are logged and never fail
// the tracking operation.
//
// The message carries text as its plaintext body and, when html is non-empty,
// html as its rich-text body. The serialized exam report travels as an
// attachment; a report that cannot be serialized is logged and the
// notification goes out without the attachment.
//
// mailingConsent is the exam taker's consent to the exam report being emailed
// to the exam taker's email address; it is carried on the message as the
// WellKnownLabelKeyExamReportMailConsent label so downstream messaging can act
// on it. Operations that have no consent decision (DeleteExamTracking) pass
// false.
//
// event identifies the exam lifecycle event the notification reports (one of
// the WellKnownLabelValueExam* constants); it is carried as the
// WellKnownLabelKeyExamEvent label so downstream messaging can tell, say, a
// completed exam session apart from a report deletion.
func (s *OnMemoryExamTrackingServer) notify(ctx context.Context, userid string, report ExamReport, mailingConsent bool, event string, title string, level msgnotify.MessageLevel, text, html string) {
	// The sender carries the message tags: the message source, the exam
	// taker's subject id, the overall result of the exam assessment, the
	// mailing consent, and the exam taker labels lifted from the report's
	// first person (left empty when the exam taker is anonymous).
	overallResult := ""
	if report.Assessment.OverallResult != nil {
		overallResult = string(*report.Assessment.OverallResult)
	}
	var exmail, username, firstName, lastName string
	if len(report.ExamTaker.Persons) > 0 {
		p := report.ExamTaker.Persons[0]
		exmail, username, firstName, lastName = p.Email, p.Name, p.Fistname, p.Lastname
	}
	sender := notificationSender
	sender.Tags = msgnotify.AssociationsList{
		msgnotify.MakeLabelKey(msgnotify.WellKnownLabelKeyMsgSource, msgnotify.WellKnownLabelValueExamReportServer),
		msgnotify.MakeLabelKey(msgnotify.WellKnownLabelKeyExamEvent, event),
		msgnotify.MakeLabelKey(msgnotify.WellKnownLabelKeyExamTakerSubjectId, userid),
		msgnotify.MakeLabelKey(msgnotify.WellKnownLabelKeyExamOverallResult, overallResult),
		msgnotify.MakeLabelKey(msgnotify.WellKnownLabelKeyExamReportMailConsent, strconv.FormatBool(mailingConsent)),
		msgnotify.MakeLabelKey(msgnotify.WellKnownLabelKeyExamTakerExmail, exmail),
		msgnotify.MakeLabelKey(msgnotify.WellKnownLabelKeyExamTakerUsername, username),
		msgnotify.MakeLabelKey(msgnotify.WellKnownLabelKeyExamTakerFirstName, firstName),
		msgnotify.MakeLabelKey(msgnotify.WellKnownLabelKeyExamTakerLastName, lastName),
	}

	msg := msgnotify.Msg{
		Id:      uuid.NewString(),
		Created: time.Now().UnixMilli(),
		Title:   title,
		Level:   level,
		Text:    text,
		HTML:    html,
	}
	if attachment, err := s.reportAttachment(report); err != nil {
		// Best effort, exactly like delivery itself: a report that cannot be
		// serialized must not fail the notification, let alone the tracking
		// operation.
		slog.WarnContext(ctx, "examreport: failed to serialize report attachment, sending without it",
			"reportId", report.Id, "error", err)
	} else {
		msg.Attachments = []msgnotify.BlobAttachment{attachment}
	}
	for _, svc := range s.notifiers {
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
			slog.WarnContext(ctx, "examreport: notification delivery failed",
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

// Put stores examReport under userid. It is safe for concurrent use: Put claims
// a unique per-user index by compare-and-swapping the count, so concurrent Puts
// for the same userid never collide on the same key.
func (s *OnMemoryExamTrackingServer) Put(ctx context.Context, userid string, examReport ExamReport, mailingConsent bool) error {
	s.counts.LoadOrStore(userid, int64(0))
	for {
		cur, _ := s.counts.Load(userid)
		idx := cur.(int64)
		// Atomically claim idx by advancing the count only if it is still idx.
		if s.counts.CompareAndSwap(userid, idx, idx+1) {
			// idx is now ours: safe to store the report at this index.
			s.reports.Store(reportKey(userid, idx), examReport)
			text, html := examCompletionMessage(userid, examReport)
			s.notify(ctx, userid, examReport, mailingConsent, msgnotify.WellKnownLabelValueExamCompleted,
				"Exam session completed", msgnotify.MessageLevelCommon, text, html)
			return nil
		}
	}
}

// GetExamReportsByUserId returns all exam reports stored for userid in the order
// they were Put, or nil if the user has none. The returned slice is independent
// of the stored state and safe to use without further synchronization.
//
// Note: the count is advanced before a report is stored, so under a concurrent
// Put Get may observe a count that includes an in-flight report; such
// not-yet-stored entries are skipped and will appear on a subsequent call. No
// report is ever lost.
func (s *OnMemoryExamTrackingServer) GetExamReportsByUserId(ctx context.Context, userid string) ([]ExamReport, error) {
	v, ok := s.counts.Load(userid)
	if !ok {
		return nil, nil
	}
	n := v.(int64)
	out := make([]ExamReport, 0, n)
	for i := int64(0); i < n; i++ {
		if rv, ok := s.reports.Load(reportKey(userid, i)); ok {
			out = append(out, rv.(ExamReport))
		}
	}
	return out, nil
}

// DeleteExamTracking removes the report with the given id from userid's
// reports, or returns ErrExamTrackingNotFound when the user has no such
// report. It is safe for concurrent use.
//
// The scan tolerates concurrent Puts: a slot whose count was already claimed
// but whose report is not yet stored simply fails the Load and is skipped,
// exactly as in Get. Deletion never decrements the count, so indexes stay
// monotonic and a deleted index is never reused; because each index is
// written at most once, the Load-then-Delete pair can only ever remove the
// report it observed — no CAS or mutex is needed. Two concurrent deletes of
// the same id may both return nil; the net effect is one deletion, so the
// operation is safely idempotent.
func (s *OnMemoryExamTrackingServer) DeleteExamTracking(ctx context.Context, userid string, examReportId string) error {
	v, ok := s.counts.Load(userid)
	if !ok {
		return ErrExamTrackingNotFound
	}
	n := v.(int64)
	for i := int64(0); i < n; i++ {
		key := reportKey(userid, i)
		rv, ok := s.reports.Load(key)
		if !ok {
			continue
		}
		report := rv.(ExamReport)
		if report.Id != examReportId {
			continue
		}
		s.reports.Delete(key)
		s.notify(ctx, userid, report, false, msgnotify.WellKnownLabelValueExamReportDeleted,
			"Exam report deleted", msgnotify.MessageLevelImportant,
			fmt.Sprintf("The exam report %s of %s has been deleted.", report.Id, displayName(userid, report)), "")
		return nil
	}
	return ErrExamTrackingNotFound
}

// reportKey builds the synthesized reports-map key "{userid}:{index}".
func reportKey(userid string, idx int64) string {
	return userid + ":" + strconv.FormatInt(idx, 10)
}

// displayName returns the exam taker's display name for notifications: the
// username lifted from the report's first person when available, otherwise
// the raw subject id.
func displayName(userid string, report ExamReport) string {
	if len(report.ExamTaker.Persons) > 0 && report.ExamTaker.Persons[0].Name != "" {
		return report.ExamTaker.Persons[0].Name
	}
	return userid
}

// reportAttachment serializes report as an XML <examreport> document (the
// shape defined by exam.xsd) so it can travel as an email attachment. When the
// server was built with an XML signer, the serialized document is re-parsed
// and enveloped-signed (XMLDSIG) before being attached.
func (s *OnMemoryExamTrackingServer) reportAttachment(report ExamReport) (msgnotify.BlobAttachment, error) {
	raw, err := xml.MarshalIndent(report, "", "  ")
	if err != nil {
		return msgnotify.BlobAttachment{}, fmt.Errorf("examreport: serializing report %s: %w", report.Id, err)
	}
	if s.xmlSigner != nil {
		if raw, err = signEnvelopedXML(raw, s.xmlSigner); err != nil {
			return msgnotify.BlobAttachment{}, fmt.Errorf("examreport: signing report %s: %w", report.Id, err)
		}
	}
	content := append([]byte(xml.Header), raw...)
	return msgnotify.BlobAttachment{
		Id:       "exam-report-" + report.Id,
		Content:  content,
		MIMEType: "application/xml",
		Size:     len(content),
		Filename: "exam-report-" + report.Id + ".xml",
	}, nil
}

// signEnvelopedXML re-parses the XML document produced by encoding/xml,
// envelops a signature into its root element with xmlSigner, and returns the
// signed document.
func signEnvelopedXML(raw []byte, xmlSigner pkgmodelssigner.XMLETreeSigner) ([]byte, error) {
	doc := etree.NewDocument()
	if err := doc.ReadFromBytes(raw); err != nil {
		return nil, fmt.Errorf("re-parsing XML: %w", err)
	}
	signed, err := xmlSigner.SignEnveloped(doc.Root())
	if err != nil {
		return nil, fmt.Errorf("enveloping XMLDSIG signature: %w", err)
	}
	// SignEnveloped does not mutate the parsed document: it returns a copy of
	// the root element with the Signature appended, so the signed document is
	// built around that copy.
	signedDoc := etree.NewDocument()
	signedDoc.SetRoot(signed)
	out, err := signedDoc.WriteToBytes()
	if err != nil {
		return nil, fmt.Errorf("serializing signed document: %w", err)
	}
	return out, nil
}

// completionNotice carries the values rendered into the exam-completion
// notification bodies.
type completionNotice struct {
	Name           string
	ExamTitle      string
	ExamCode       string
	ExamSessionId  string
	ReportId       string
	OverallResult  string // empty when the assessment has no overall result
	Score          string // "earned/total", empty when the assessment has no scores
	FinishedAt     string
	AttachmentName string
}

var completionTextTmpl = texttemplate.Must(texttemplate.New("exam-completion").Parse(`Hello {{.Name}},

your exam session for "{{.ExamTitle}}"{{if .ExamCode}} ({{.ExamCode}}){{end}} has completed, and your exam report has been recorded.

  Exam session:   {{.ExamSessionId}}
  Exam report:    {{.ReportId}}
{{- if .OverallResult}}
  Overall result: {{.OverallResult}}
{{- end}}
{{- if .Score}}
  Score:          {{.Score}}
{{- end}}
  Finished at:    {{.FinishedAt}}

The full exam report is attached as {{.AttachmentName}}.
`))

var completionHTMLTmpl = htmltemplate.Must(htmltemplate.New("exam-completion").Parse(`<!DOCTYPE html>
<html lang="en">
<head><meta charset="UTF-8"><title>Exam session completed</title></head>
<body style="margin:0;padding:24px;font-family:Arial,Helvetica,sans-serif;color:#1f2933;background-color:#f8fafc;">
  <div style="max-width:560px;margin:0 auto;padding:24px;background-color:#ffffff;border:1px solid #e2e8f0;border-radius:8px;">
    <h2 style="margin:0 0 16px;color:#0f172a;">Exam session completed</h2>
    <p>Hello {{.Name}},</p>
    <p>your exam session for <strong>{{.ExamTitle}}</strong>{{if .ExamCode}} ({{.ExamCode}}){{end}} has completed, and your exam report has been recorded.</p>
    <table style="border-collapse:collapse;margin:16px 0;">
      <tr><td style="padding:4px 16px 4px 0;color:#64748b;">Exam session</td><td>{{.ExamSessionId}}</td></tr>
      <tr><td style="padding:4px 16px 4px 0;color:#64748b;">Exam report</td><td>{{.ReportId}}</td></tr>
      {{- if .OverallResult}}
      <tr><td style="padding:4px 16px 4px 0;color:#64748b;">Overall result</td><td><strong>{{.OverallResult}}</strong></td></tr>
      {{- end}}
      {{- if .Score}}
      <tr><td style="padding:4px 16px 4px 0;color:#64748b;">Score</td><td>{{.Score}}</td></tr>
      {{- end}}
      <tr><td style="padding:4px 16px 4px 0;color:#64748b;">Finished at</td><td>{{.FinishedAt}}</td></tr>
    </table>
    <p>The full exam report is attached as <code>{{.AttachmentName}}</code>.</p>
  </div>
</body>
</html>`))

// examCompletionMessage builds the plain-text and HTML bodies of the
// notification sent when an exam session completes. The exam taker is greeted
// by the username lifted from the report when available, falling back to the
// raw subject id. The HTML body is escaped by html/template, so report fields
// can never break the markup.
func examCompletionMessage(userid string, report ExamReport) (text, html string) {
	n := completionNotice{
		Name:           displayName(userid, report),
		ExamTitle:      report.Title,
		ExamCode:       report.ExamCode,
		ExamSessionId:  report.ExamSessionId,
		ReportId:       report.Id,
		FinishedAt:     time.UnixMilli(report.FinishedAt).UTC().Format(time.RFC1123),
		AttachmentName: "exam-report-" + report.Id + ".xml",
	}
	if report.Assessment.OverallResult != nil {
		// The enum values ("pass", "immediate") read better shouted in a
		// human-facing message body.
		n.OverallResult = strings.ToUpper(string(*report.Assessment.OverallResult))
	}
	if report.Assessment.ScoreResult != nil {
		n.Score = fmt.Sprintf("%g/%g", report.Assessment.ScoreResult.EarnedScore, report.Assessment.ScoreResult.TotalScore)
	}

	var textBuf, htmlBuf bytes.Buffer
	if err := completionTextTmpl.Execute(&textBuf, n); err != nil {
		// Template execution fails only on I/O errors, which a bytes.Buffer
		// never produces; guard anyway and fall back to a minimal body.
		text = fmt.Sprintf("User %s completed exam session %s; exam report %s recorded.", userid, report.ExamSessionId, report.Id)
	} else {
		text = textBuf.String()
	}
	if err := completionHTMLTmpl.Execute(&htmlBuf, n); err != nil {
		html = ""
	} else {
		html = htmlBuf.String()
	}
	return text, html
}
