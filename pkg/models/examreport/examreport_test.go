package examreport

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"personal-site/pkg/models/msgnotify"
	pkgmodelsquestion "personal-site/pkg/models/question"
	pkgmodelssigner "personal-site/pkg/models/signer"

	"github.com/beevik/etree"
	dsig "github.com/russellhaering/goxmldsig"
)

// mustReport builds an ExamReport with distinguishable fields.
func mustReport(t *testing.T, id string) ExamReport {
	t.Helper()
	return ExamReport{
		Id:            id,
		ExamId:        "exam-" + id,
		ExamShortName: "DCACI",
		ExamCode:      "300-620",
		Title:         "Implementing Cisco ACI",
		Description:   "desc",
		ExamCategory:  pkgmodelsquestion.ExamCategoryCertification,
		ExamSessionId: "sess-" + id,
		FinishedAt:    1700000000000,
		Assessment: pkgmodelsquestion.Assessment{
			OverallResult: nil,
			ScoreResult: &pkgmodelsquestion.ScoreResult{
				EarnedScore: 7,
				TotalScore:  10,
			},
		},
	}
}

func TestExamReport_XMLRoundTrip(t *testing.T) {
	report := ExamReport{
		Id:            "rep-1",
		ExamId:        "exam-doc-1",
		ExamShortName: "DCACI",
		ExamCode:      "300-620",
		Title:         "Implementing Cisco ACI",
		Description:   "A description",
		ExamCategory:  pkgmodelsquestion.ExamCategoryCertification,
		ExamSessionId: "session-1",
		FinishedAt:    1700000000123,
		ExamTaker: ExamTaker{
			Persons:   []Person{{Name: "Alice", Fistname: "Alice", Lastname: "Smith", Email: "alice@example.com"}},
			Anonymous: []Anonymous{{SessionId: "anon-1"}},
		},
		Assessment: pkgmodelsquestion.Assessment{
			OverallResult: nil,
			ScoreResult: &pkgmodelsquestion.ScoreResult{
				EarnedScore: 800,
				TotalScore:  1000,
			},
			QuestionScores: []pkgmodelsquestion.QuestionScore{
				{QuestionId: "q1", ScoreEarned: 1},
				{QuestionId: "q2", ScoreEarned: 0},
			},
		},
	}

	out, err := xml.MarshalIndent(&report, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent: unexpected error: %v", err)
	}
	str := string(out)
	t.Logf("marshaled:\n%s", str)

	// Spot-check key elements/attributes that ExamReport is responsible for.
	for _, want := range []string{
		`<examreport id="rep-1">`,
		`<examid>exam-doc-1</examid>`,
		`<examshortname>DCACI</examshortname>`,
		`<examcode>300-620</examcode>`,
		`<title>Implementing Cisco ACI</title>`,
		`<description>A description</description>`,
		`<examcategory>certification-exam</examcategory>`,
		`<examsessionid>session-1</examsessionid>`,
		`<finishedat>1700000000123</finishedat>`,
		`<person name="Alice" fistname="Alice" lastname="Smith" email="alice@example.com"></person>`,
		`<anonymous sessionid="anon-1"></anonymous>`,
	} {
		if !strings.Contains(str, want) {
			t.Errorf("marshaled XML missing %q", want)
		}
	}

	// Round-trip back.
	var got ExamReport
	if err := xml.Unmarshal(out, &got); err != nil {
		t.Fatalf("Unmarshal: unexpected error: %v", err)
	}
	if got.Id != report.Id {
		t.Errorf("Id = %q, want %q", got.Id, report.Id)
	}
	if got.ExamId != report.ExamId {
		t.Errorf("ExamId = %q, want %q", got.ExamId, report.ExamId)
	}
	if got.ExamSessionId != report.ExamSessionId {
		t.Errorf("ExamSessionId = %q, want %q", got.ExamSessionId, report.ExamSessionId)
	}
	if got.FinishedAt != report.FinishedAt {
		t.Errorf("FinishedAt = %d, want %d", got.FinishedAt, report.FinishedAt)
	}
	if got.ExamCategory != report.ExamCategory {
		t.Errorf("ExamCategory = %q, want %q", got.ExamCategory, report.ExamCategory)
	}
	if got.Assessment.ScoreResult == nil ||
		got.Assessment.ScoreResult.EarnedScore != report.Assessment.ScoreResult.EarnedScore {
		t.Errorf("Assessment.ScoreResult not preserved: %+v", got.Assessment.ScoreResult)
	}
	if len(got.ExamTaker.Persons) != 1 || got.ExamTaker.Persons[0].Name != "Alice" || got.ExamTaker.Persons[0].Email != "alice@example.com" {
		t.Errorf("ExamTaker.Persons not preserved: %+v", got.ExamTaker.Persons)
	}
	if len(got.ExamTaker.Anonymous) != 1 || got.ExamTaker.Anonymous[0].SessionId != "anon-1" {
		t.Errorf("ExamTaker.Anonymous not preserved: %+v", got.ExamTaker.Anonymous)
	}
}

func TestExamReport_OptionalFieldsOmitempty(t *testing.T) {
	// Description and PassingScore are optional; a zero report should omit them.
	report := ExamReport{
		Id:            "rep-2",
		ExamId:        "exam-doc-2",
		Title:         "T",
		ExamCategory:  pkgmodelsquestion.ExamCategoryPractice,
		ExamSessionId: "session-2",
		FinishedAt:    1,
	}
	out, err := xml.Marshal(&report)
	if err != nil {
		t.Fatalf("Marshal: unexpected error: %v", err)
	}
	str := string(out)
	if strings.Contains(str, "<description>") {
		t.Errorf("expected <description> to be omitted, got:\n%s", str)
	}
	if strings.Contains(str, "<passingscore>") {
		t.Errorf("expected <passingscore> to be omitted, got:\n%s", str)
	}
}

func TestReportKey(t *testing.T) {
	cases := []struct {
		userid string
		idx    int64
		want   string
	}{
		{"alice", 0, "alice:0"},
		{"alice", 42, "alice:42"},
		{"u:with:colons", 3, "u:with:colons:3"},
	}
	for _, c := range cases {
		if got := reportKey(c.userid, c.idx); got != c.want {
			t.Errorf("reportKey(%q,%d) = %q, want %q", c.userid, c.idx, got, c.want)
		}
	}
}

func TestPutAndGet_SingleUser(t *testing.T) {
	srv := NewOnMemoryExamTrackingServer(nil, nil)
	ctx := context.Background()

	// Unknown user returns nil, nil.
	got, err := srv.GetExamReportsByUserId(ctx, "nobody")
	if err != nil {
		t.Fatalf("Get unknown user: unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("Get unknown user = %v, want nil", got)
	}

	r1 := mustReport(t, "r1")
	r2 := mustReport(t, "r2")
	r3 := mustReport(t, "r3")

	if err := srv.Put(ctx, "alice", r1, false); err != nil {
		t.Fatalf("Put r1: %v", err)
	}
	if err := srv.Put(ctx, "alice", r2, false); err != nil {
		t.Fatalf("Put r2: %v", err)
	}
	if err := srv.Put(ctx, "alice", r3, false); err != nil {
		t.Fatalf("Put r3: %v", err)
	}

	reports, err := srv.GetExamReportsByUserId(ctx, "alice")
	if err != nil {
		t.Fatalf("Get: unexpected error: %v", err)
	}
	if len(reports) != 3 {
		t.Fatalf("len(reports) = %d, want 3", len(reports))
	}
	// Insertion order must be preserved.
	wantIds := []string{"r1", "r2", "r3"}
	for i, w := range wantIds {
		if reports[i].Id != w {
			t.Errorf("reports[%d].Id = %q, want %q", i, reports[i].Id, w)
		}
	}
}

func TestPutAndGet_MultipleUsersAreIsolated(t *testing.T) {
	srv := NewOnMemoryExamTrackingServer(nil, nil)
	ctx := context.Background()

	_ = srv.Put(ctx, "alice", mustReport(t, "a1"), false)
	_ = srv.Put(ctx, "bob", mustReport(t, "b1"), false)
	_ = srv.Put(ctx, "alice", mustReport(t, "a2"), false)

	alice, _ := srv.GetExamReportsByUserId(ctx, "alice")
	bob, _ := srv.GetExamReportsByUserId(ctx, "bob")

	if len(alice) != 2 {
		t.Fatalf("alice has %d reports, want 2", len(alice))
	}
	if len(bob) != 1 {
		t.Fatalf("bob has %d reports, want 1", len(bob))
	}
	for _, r := range alice {
		if !strings.HasPrefix(r.Id, "a") {
			t.Errorf("alice should only have 'a*' reports, got %q", r.Id)
		}
	}
}

// TestPut_ConcurrentSameUserNoLoss hammers Put for a single userid from many
// goroutines and then verifies every report was retained with a unique index.
// Under -race this also exercises the CAS loop's memory safety.
func TestPut_ConcurrentSameUserNoLoss(t *testing.T) {
	srv := NewOnMemoryExamTrackingServer(nil, nil)
	ctx := context.Background()

	const goroutines = 64
	const perGoroutine = 25
	const total = goroutines * perGoroutine
	userid := "racer"

	var wg sync.WaitGroup
	wg.Add(goroutines)
	start := make(chan struct{})
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < perGoroutine; i++ {
				r := mustReport(t, "x")
				if err := srv.Put(ctx, userid, r, false); err != nil {
					t.Errorf("Put: unexpected error: %v", err)
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	reports, err := srv.GetExamReportsByUserId(ctx, userid)
	if err != nil {
		t.Fatalf("Get: unexpected error: %v", err)
	}
	if len(reports) != total {
		t.Fatalf("len(reports) = %d, want %d (lost %d reports)",
			len(reports), total, total-len(reports))
	}

	// Confirm the count matches the value stored in the counts map.
	v, ok := srv.counts.Load(userid)
	if !ok {
		t.Fatal("counts map missing entry for user")
	}
	if n := v.(int64); n != int64(total) {
		t.Errorf("counts = %d, want %d", n, total)
	}
}

// TestPutGet_ConcurrentMixed runs Puts and Gets concurrently against the same
// user to stress the read/write interaction under -race. Gets may transiently
// observe fewer than the in-flight count, but must never observe more than the
// committed count and must never panic.
func TestPutGet_ConcurrentMixed(t *testing.T) {
	srv := NewOnMemoryExamTrackingServer(nil, nil)
	ctx := context.Background()

	const puts = 200
	userid := "mixed"

	var wg sync.WaitGroup
	wg.Add(2)

	var getErr atomic.Value // stores error
	go func() {
		defer wg.Done()
		for i := 0; i < puts; i++ {
			rs, err := srv.GetExamReportsByUserId(ctx, userid)
			if err != nil {
				getErr.Store(err)
				return
			}
			// A Get must never report more than the number of completed Puts so
			// far; since reads and writes interleave arbitrarily we just check
			// the upper bound is non-negative and never absurdly large.
			if len(rs) < 0 || len(rs) > puts {
				getErr.Store(fmt.Errorf("Get returned %d reports (want 0..%d)", len(rs), puts))
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < puts; i++ {
			if err := srv.Put(ctx, userid, mustReport(t, "p"), false); err != nil {
				getErr.Store(err)
				return
			}
		}
	}()

	wg.Wait()

	if v := getErr.Load(); v != nil {
		t.Fatalf("concurrent Get/Put error: %v", v)
	}

	final, _ := srv.GetExamReportsByUserId(ctx, userid)
	if len(final) != puts {
		t.Errorf("final report count = %d, want %d", len(final), puts)
	}
}

func TestDeleteExamTracking_Basic(t *testing.T) {
	srv := NewOnMemoryExamTrackingServer(nil, nil)
	ctx := context.Background()

	// Deleting from a user with no reports is a not-found.
	if err := srv.DeleteExamTracking(ctx, "nobody", "r1"); !errors.Is(err, ErrExamTrackingNotFound) {
		t.Fatalf("Delete unknown user = %v, want ErrExamTrackingNotFound", err)
	}

	_ = srv.Put(ctx, "alice", mustReport(t, "r1"), false)
	_ = srv.Put(ctx, "alice", mustReport(t, "r2"), false)
	_ = srv.Put(ctx, "alice", mustReport(t, "r3"), false)
	_ = srv.Put(ctx, "bob", mustReport(t, "r2"), false) // same id, another user

	// Alice cannot delete bob's report even though the id matches her own: her
	// own r2 is removed, bob's r2 must survive.
	if err := srv.DeleteExamTracking(ctx, "alice", "r2"); err != nil {
		t.Fatalf("Delete r2: unexpected error: %v", err)
	}

	alice, _ := srv.GetExamReportsByUserId(ctx, "alice")
	if len(alice) != 2 || alice[0].Id != "r1" || alice[1].Id != "r3" {
		t.Fatalf("alice reports after delete = %+v, want [r1 r3] in order", alice)
	}
	bob, _ := srv.GetExamReportsByUserId(ctx, "bob")
	if len(bob) != 1 || bob[0].Id != "r2" {
		t.Fatalf("bob reports after alice's delete = %+v, want [r2]", bob)
	}

	// Re-deleting the same id and deleting an unknown id are both not-found.
	if err := srv.DeleteExamTracking(ctx, "alice", "r2"); !errors.Is(err, ErrExamTrackingNotFound) {
		t.Errorf("re-delete r2 = %v, want ErrExamTrackingNotFound", err)
	}
	if err := srv.DeleteExamTracking(ctx, "alice", "no-such"); !errors.Is(err, ErrExamTrackingNotFound) {
		t.Errorf("delete unknown id = %v, want ErrExamTrackingNotFound", err)
	}

	// Puts after a deletion keep claiming fresh indexes: no reuse of the hole.
	_ = srv.Put(ctx, "alice", mustReport(t, "r4"), false)
	alice, _ = srv.GetExamReportsByUserId(ctx, "alice")
	wantIds := []string{"r1", "r3", "r4"}
	if len(alice) != len(wantIds) {
		t.Fatalf("alice reports = %+v, want %v", alice, wantIds)
	}
	for i, w := range wantIds {
		if alice[i].Id != w {
			t.Errorf("alice[%d].Id = %q, want %q", i, alice[i].Id, w)
		}
	}
}

// TestDeleteExamTracking_ConcurrentWithPut runs Puts, Deletes, and Gets
// against the same user concurrently to stress the lock-free interaction
// under -race. Deletes may legitimately report not-found (racing another
// delete of the same id, or scanning before the matching Put landed), but any
// nil delete must correspond to a report that disappears, and Gets must never
// panic or observe an absurd count.
func TestDeleteExamTracking_ConcurrentWithPut(t *testing.T) {
	srv := NewOnMemoryExamTrackingServer(nil, nil)
	ctx := context.Background()
	userid := "racer-delete"

	const puts = 200
	// Pre-seed half the reports so deletes have targets from the start.
	for i := 0; i < puts/2; i++ {
		_ = srv.Put(ctx, userid, mustReport(t, fmt.Sprintf("seed-%d", i)), false)
	}

	var wg sync.WaitGroup
	wg.Add(3)
	var opErr atomic.Value // stores error

	go func() {
		defer wg.Done()
		for i := 0; i < puts; i++ {
			if err := srv.Put(ctx, userid, mustReport(t, fmt.Sprintf("live-%d", i)), false); err != nil {
				opErr.Store(fmt.Errorf("Put: %w", err))
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < puts/2; i++ {
			err := srv.DeleteExamTracking(ctx, userid, fmt.Sprintf("seed-%d", i))
			if err != nil && !errors.Is(err, ErrExamTrackingNotFound) {
				opErr.Store(fmt.Errorf("Delete: %w", err))
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < puts; i++ {
			rs, err := srv.GetExamReportsByUserId(ctx, userid)
			if err != nil {
				opErr.Store(fmt.Errorf("Get: %w", err))
				return
			}
			if len(rs) < 0 || len(rs) > 2*puts {
				opErr.Store(fmt.Errorf("Get returned %d reports (want 0..%d)", len(rs), 2*puts))
				return
			}
		}
	}()

	wg.Wait()

	if v := opErr.Load(); v != nil {
		t.Fatalf("concurrent op error: %v", v)
	}

	// All seeds were deleted (each seed id is deleted exactly once and seeds
	// existed before the deleter started), so only live-* reports may remain.
	final, _ := srv.GetExamReportsByUserId(ctx, userid)
	for _, r := range final {
		if !strings.HasPrefix(r.Id, "live-") {
			t.Errorf("unexpected surviving report %q; seeds should all be deleted", r.Id)
		}
	}
}

// Compile-time assertion that the in-memory implementation satisfies the
// interface.
var _ ExamTrackingServer = (*OnMemoryExamTrackingServer)(nil)

// Compile-time assertion that goxmldsig's signing context satisfies the XML
// signer interface consumed by the tracking server.
var _ pkgmodelssigner.XMLETreeSigner = (*dsig.SigningContext)(nil)

// sentMessage records one Send invocation of a fakeNotifier.
type sentMessage struct {
	replyTo msgnotify.AddrId
	to      msgnotify.AddrId
	msg     msgnotify.Msg
}

// fakeNotifier is a MsgNotifySvc that records the messages it is asked to
// send. The accepted sender/recipient address families and the AreYou answer
// are configurable so tests can exercise the negotiation.
type fakeNotifier struct {
	senderFamilies    []msgnotify.MsgNotifyAddrFamily
	recipientFamilies []msgnotify.MsgNotifyAddrFamily
	areYou            bool
	sendErr           error

	mu   sync.Mutex
	sent []sentMessage
}

func (f *fakeNotifier) AreYou(addrId msgnotify.AddrId) bool { return f.areYou }

func (f *fakeNotifier) GetAcceptedSenderAddressFamilies() []msgnotify.MsgNotifyAddrFamily {
	return f.senderFamilies
}

func (f *fakeNotifier) GetAcceptedRecipientAddressFamilies() []msgnotify.MsgNotifyAddrFamily {
	return f.recipientFamilies
}

func (f *fakeNotifier) Send(ctx context.Context, replyTo msgnotify.AddrId, to msgnotify.AddrId, msg msgnotify.Msg) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sentMessage{replyTo: replyTo, to: to, msg: msg})
	return f.sendErr
}

func (f *fakeNotifier) sentMessages() []sentMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentMessage(nil), f.sent...)
}

func TestPut_SendsNotification(t *testing.T) {
	notifier := &fakeNotifier{
		senderFamilies:    []msgnotify.MsgNotifyAddrFamily{msgnotify.MsgNotifyAddrFamilyService},
		recipientFamilies: []msgnotify.MsgNotifyAddrFamily{msgnotify.MsgNotifyAddrFamilyService},
		areYou:            true,
	}
	srv := NewOnMemoryExamTrackingServer([]msgnotify.MsgNotifySvc{notifier}, nil)
	ctx := context.Background()

	report := mustReport(t, "r1")
	pass := pkgmodelsquestion.OverallResultPass
	report.Assessment.OverallResult = &pass
	if err := srv.Put(ctx, "alice", report, true); err != nil {
		t.Fatalf("Put: %v", err)
	}

	sent := notifier.sentMessages()
	if len(sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(sent))
	}
	got := sent[0]
	if got.to.AddressFamily != msgnotify.MsgNotifyAddrFamilyService || got.to.Address != "" {
		t.Errorf("recipient = %+v, want the opaque service address (service, \"\")", got.to)
	}
	if got.replyTo.AddressFamily != msgnotify.MsgNotifyAddrFamilyService {
		t.Errorf("replyTo family = %q, want %q", got.replyTo.AddressFamily, msgnotify.MsgNotifyAddrFamilyService)
	}

	tags := got.replyTo.Tags
	if v := tags.GetByLabelKey(msgnotify.WellKnownLabelKeyMsgSource); !reflect.DeepEqual(v, []string{msgnotify.WellKnownLabelValueExamReportServer}) {
		t.Errorf("tag %s = %v, want [%s]", msgnotify.WellKnownLabelKeyMsgSource, v, msgnotify.WellKnownLabelValueExamReportServer)
	}
	if v := tags.GetByLabelKey(msgnotify.WellKnownLabelKeyExamTakerSubjectId); !reflect.DeepEqual(v, []string{"alice"}) {
		t.Errorf("tag %s = %v, want [alice]", msgnotify.WellKnownLabelKeyExamTakerSubjectId, v)
	}
	if v := tags.GetByLabelKey(msgnotify.WellKnownLabelKeyExamOverallResult); !reflect.DeepEqual(v, []string{"pass"}) {
		t.Errorf("tag %s = %v, want [pass]", msgnotify.WellKnownLabelKeyExamOverallResult, v)
	}
	// A Put notification is labeled as an exam-completion event.
	if v := tags.GetByLabelKey(msgnotify.WellKnownLabelKeyExamEvent); !reflect.DeepEqual(v, []string{msgnotify.WellKnownLabelValueExamCompleted}) {
		t.Errorf("tag %s = %v, want [%s]", msgnotify.WellKnownLabelKeyExamEvent, v, msgnotify.WellKnownLabelValueExamCompleted)
	}
	// The mailing consent handed to Put is carried on the message.
	if v := tags.GetByLabelKey(msgnotify.WellKnownLabelKeyExamReportMailConsent); !reflect.DeepEqual(v, []string{"true"}) {
		t.Errorf("tag %s = %v, want [true]", msgnotify.WellKnownLabelKeyExamReportMailConsent, v)
	}
	// The remaining exam taker labels are present with empty values.
	for _, key := range []string{
		msgnotify.WellKnownLabelKeyExamTakerExmail,
		msgnotify.WellKnownLabelKeyExamTakerUsername,
		msgnotify.WellKnownLabelKeyExamTakerFirstName,
		msgnotify.WellKnownLabelKeyExamTakerLastName,
	} {
		if v := tags.GetByLabelKey(key); !reflect.DeepEqual(v, []string{""}) {
			t.Errorf("tag %s = %v, want [\"\"]", key, v)
		}
	}
	if got.msg.Level != msgnotify.MessageLevelCommon {
		t.Errorf("level = %d, want MessageLevelCommon", got.msg.Level)
	}
	if got.msg.Id == "" {
		t.Error("message id is empty, want a globally unique id")
	}
	if got.msg.Created <= 0 {
		t.Errorf("created = %d, want a positive millisecond timestamp", got.msg.Created)
	}
	for _, want := range []string{"alice", "sess-r1", "r1"} {
		if !strings.Contains(got.msg.Text, want) {
			t.Errorf("text = %q, want it to mention %q", got.msg.Text, want)
		}
	}
}

// TestPut_SignsReportAttachment confirms that when the server is built with
// an XML signer, the exam report XML attached to the completion notification
// carries an enveloped XMLDSIG signature that validates against the signing
// certificate, and that without a signer the attachment stays unsigned.
func TestPut_SignsReportAttachment(t *testing.T) {
	newNotifier := func() *fakeNotifier {
		return &fakeNotifier{
			senderFamilies:    []msgnotify.MsgNotifyAddrFamily{msgnotify.MsgNotifyAddrFamilyService},
			recipientFamilies: []msgnotify.MsgNotifyAddrFamily{msgnotify.MsgNotifyAddrFamilyService},
			areYou:            true,
		}
	}

	t.Run("with signer", func(t *testing.T) {
		notifier := newNotifier()
		keyStore := dsig.RandomKeyStoreForTest()
		xmlSigner := dsig.NewDefaultSigningContext(keyStore)
		srv := NewOnMemoryExamTrackingServer([]msgnotify.MsgNotifySvc{notifier}, xmlSigner)

		if err := srv.Put(context.Background(), "alice", mustReport(t, "r1"), false); err != nil {
			t.Fatalf("Put: %v", err)
		}
		sent := notifier.sentMessages()
		if len(sent) != 1 {
			t.Fatalf("sent %d messages, want 1", len(sent))
		}
		atts := sent[0].msg.Attachments
		if len(atts) != 1 {
			t.Fatalf("attachment count = %d, want 1", len(atts))
		}

		doc := etree.NewDocument()
		if err := doc.ReadFromBytes(atts[0].Content); err != nil {
			t.Fatalf("attachment is not well-formed XML: %v\n%s", err, atts[0].Content)
		}
		root := doc.Root()
		if sig := root.FindElement("./" + dsig.DefaultPrefix + ":" + dsig.SignatureTag); sig == nil {
			t.Fatalf("signed attachment has no enveloped Signature element:\n%s", atts[0].Content)
		}

		// The enveloped signature must validate against the keystore's
		// certificate.
		_, certBytes, err := keyStore.GetKeyPair()
		if err != nil {
			t.Fatalf("GetKeyPair: %v", err)
		}
		if block, _ := pem.Decode(certBytes); block != nil {
			certBytes = block.Bytes
		}
		cert, err := x509.ParseCertificate(certBytes)
		if err != nil {
			t.Fatalf("ParseCertificate: %v", err)
		}
		validationCtx := dsig.NewDefaultValidationContext(&dsig.MemoryX509CertificateStore{Roots: []*x509.Certificate{cert}})
		if _, err := validationCtx.Validate(root); err != nil {
			t.Fatalf("enveloped signature does not validate: %v", err)
		}
	})

	t.Run("without signer", func(t *testing.T) {
		notifier := newNotifier()
		srv := NewOnMemoryExamTrackingServer([]msgnotify.MsgNotifySvc{notifier}, nil)

		if err := srv.Put(context.Background(), "alice", mustReport(t, "r1"), false); err != nil {
			t.Fatalf("Put: %v", err)
		}
		sent := notifier.sentMessages()
		if len(sent) != 1 {
			t.Fatalf("sent %d messages, want 1", len(sent))
		}
		atts := sent[0].msg.Attachments
		if len(atts) != 1 {
			t.Fatalf("attachment count = %d, want 1", len(atts))
		}
		if strings.Contains(string(atts[0].Content), dsig.SignatureTag) {
			t.Errorf("unsigned attachment contains a Signature element:\n%s", atts[0].Content)
		}
	})
}

// TestPut_NotificationLiftsExamTakerProfile confirms that the exam taker
// labels on the notification are lifted from the report's first person, so
// downstream messaging learns the exam taker's email address from the report.
func TestPut_NotificationLiftsExamTakerProfile(t *testing.T) {
	notifier := &fakeNotifier{
		senderFamilies:    []msgnotify.MsgNotifyAddrFamily{msgnotify.MsgNotifyAddrFamilyService},
		recipientFamilies: []msgnotify.MsgNotifyAddrFamily{msgnotify.MsgNotifyAddrFamilyService},
		areYou:            true,
	}
	srv := NewOnMemoryExamTrackingServer([]msgnotify.MsgNotifySvc{notifier}, nil)
	ctx := context.Background()

	report := mustReport(t, "r1")
	report.ExamTaker.Persons = []Person{{Name: "alice", Fistname: "Alice", Lastname: "Smith", Email: "alice@example.com"}}
	if err := srv.Put(ctx, "alice", report, false); err != nil {
		t.Fatalf("Put: %v", err)
	}

	sent := notifier.sentMessages()
	if len(sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(sent))
	}
	tags := sent[0].replyTo.Tags
	for key, want := range map[string]string{
		msgnotify.WellKnownLabelKeyExamTakerExmail:    "alice@example.com",
		msgnotify.WellKnownLabelKeyExamTakerUsername:  "alice",
		msgnotify.WellKnownLabelKeyExamTakerFirstName: "Alice",
		msgnotify.WellKnownLabelKeyExamTakerLastName:  "Smith",
	} {
		if v := tags.GetByLabelKey(key); !reflect.DeepEqual(v, []string{want}) {
			t.Errorf("tag %s = %v, want [%q]", key, v, want)
		}
	}
}

func TestPut_NotificationGreetsExamTakerByName(t *testing.T) {
	newNotifier := func() *fakeNotifier {
		return &fakeNotifier{
			senderFamilies:    []msgnotify.MsgNotifyAddrFamily{msgnotify.MsgNotifyAddrFamilyService},
			recipientFamilies: []msgnotify.MsgNotifyAddrFamily{msgnotify.MsgNotifyAddrFamilyService},
			areYou:            true,
		}
	}

	t.Run("username from the report when available", func(t *testing.T) {
		notifier := newNotifier()
		srv := NewOnMemoryExamTrackingServer([]msgnotify.MsgNotifySvc{notifier}, nil)

		report := mustReport(t, "r1")
		report.ExamTaker.Persons = []Person{{Name: "alice", Fistname: "Alice", Lastname: "Smith", Email: "alice@example.com"}}
		if err := srv.Put(context.Background(), "bob", report, false); err != nil {
			t.Fatalf("Put: %v", err)
		}

		sent := notifier.sentMessages()
		if len(sent) != 1 {
			t.Fatalf("sent %d messages, want 1", len(sent))
		}
		if !strings.Contains(sent[0].msg.Text, "Hello alice,") {
			t.Errorf("text = %q, want it to greet the username from the report", sent[0].msg.Text)
		}
		if strings.Contains(sent[0].msg.Text, "Hello bob,") {
			t.Errorf("text = %q, want it not to greet the raw subject id", sent[0].msg.Text)
		}
		if !strings.Contains(sent[0].msg.HTML, "Hello alice,") {
			t.Errorf("html = %q, want it to greet the username from the report", sent[0].msg.HTML)
		}
	})

	t.Run("falls back to the subject id for an anonymous taker", func(t *testing.T) {
		notifier := newNotifier()
		srv := NewOnMemoryExamTrackingServer([]msgnotify.MsgNotifySvc{notifier}, nil)

		if err := srv.Put(context.Background(), "bob", mustReport(t, "r1"), false); err != nil {
			t.Fatalf("Put: %v", err)
		}

		sent := notifier.sentMessages()
		if len(sent) != 1 {
			t.Fatalf("sent %d messages, want 1", len(sent))
		}
		if !strings.Contains(sent[0].msg.Text, "Hello bob,") {
			t.Errorf("text = %q, want it to greet the subject id as a fallback", sent[0].msg.Text)
		}
	})

	t.Run("report fields are escaped in the HTML body", func(t *testing.T) {
		notifier := newNotifier()
		srv := NewOnMemoryExamTrackingServer([]msgnotify.MsgNotifySvc{notifier}, nil)

		report := mustReport(t, "r1")
		report.ExamTaker.Persons = []Person{{Name: "alice<script>"}}
		if err := srv.Put(context.Background(), "bob", report, false); err != nil {
			t.Fatalf("Put: %v", err)
		}

		sent := notifier.sentMessages()
		if len(sent) != 1 {
			t.Fatalf("sent %d messages, want 1", len(sent))
		}
		if strings.Contains(sent[0].msg.HTML, "alice<script>") {
			t.Errorf("html contains an unescaped report field: %q", sent[0].msg.HTML)
		}
		if !strings.Contains(sent[0].msg.HTML, "alice&lt;script&gt;") {
			t.Errorf("html = %q, want the report fields HTML-escaped", sent[0].msg.HTML)
		}
	})
}

// TestPut_NotificationAttachesSerializedReport confirms that the completion
// notification carries the serialized exam report as an XML attachment.
func TestPut_NotificationAttachesSerializedReport(t *testing.T) {
	notifier := &fakeNotifier{
		senderFamilies:    []msgnotify.MsgNotifyAddrFamily{msgnotify.MsgNotifyAddrFamilyService},
		recipientFamilies: []msgnotify.MsgNotifyAddrFamily{msgnotify.MsgNotifyAddrFamilyService},
		areYou:            true,
	}
	srv := NewOnMemoryExamTrackingServer([]msgnotify.MsgNotifySvc{notifier}, nil)

	if err := srv.Put(context.Background(), "alice", mustReport(t, "r1"), false); err != nil {
		t.Fatalf("Put: %v", err)
	}

	sent := notifier.sentMessages()
	if len(sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(sent))
	}
	attachments := sent[0].msg.Attachments
	if len(attachments) != 1 {
		t.Fatalf("message carries %d attachments, want 1", len(attachments))
	}
	att := attachments[0]
	if att.MIMEType != "application/xml" {
		t.Errorf("attachment MIME type = %q, want application/xml", att.MIMEType)
	}
	if att.Filename != "exam-report-r1.xml" {
		t.Errorf("attachment filename = %q, want exam-report-r1.xml", att.Filename)
	}
	if att.Size != len(att.Content) {
		t.Errorf("attachment size = %d, want %d (the content length)", att.Size, len(att.Content))
	}

	var roundTripped ExamReport
	if err := xml.Unmarshal(att.Content, &roundTripped); err != nil {
		t.Fatalf("attachment content is not a serialized ExamReport: %v", err)
	}
	if roundTripped.Id != "r1" || roundTripped.ExamSessionId != "sess-r1" {
		t.Errorf("round-tripped report = %+v, want id r1 and session sess-r1", roundTripped)
	}
}

func TestDeleteExamTracking_SendsNotification(t *testing.T) {
	notifier := &fakeNotifier{
		senderFamilies:    []msgnotify.MsgNotifyAddrFamily{msgnotify.MsgNotifyAddrFamilyService},
		recipientFamilies: []msgnotify.MsgNotifyAddrFamily{msgnotify.MsgNotifyAddrFamilyService},
		areYou:            true,
	}
	srv := NewOnMemoryExamTrackingServer([]msgnotify.MsgNotifySvc{notifier}, nil)
	ctx := context.Background()

	_ = srv.Put(ctx, "alice", mustReport(t, "r1"), false)
	if err := srv.DeleteExamTracking(ctx, "alice", "r1"); err != nil {
		t.Fatalf("DeleteExamTracking: %v", err)
	}

	sent := notifier.sentMessages()
	if len(sent) != 2 {
		t.Fatalf("sent %d messages, want 2 (put + delete)", len(sent))
	}
	got := sent[1]
	if got.msg.Level != msgnotify.MessageLevelImportant {
		t.Errorf("level = %d, want MessageLevelImportant", got.msg.Level)
	}
	// mustReport has no overall result, so the tag value must be empty.
	if v := got.replyTo.Tags.GetByLabelKey(msgnotify.WellKnownLabelKeyExamOverallResult); !reflect.DeepEqual(v, []string{""}) {
		t.Errorf("tag %s = %v, want [\"\"] for a report without overall result", msgnotify.WellKnownLabelKeyExamOverallResult, v)
	}
	// A delete notification is labeled as a report-deleted event.
	if v := got.replyTo.Tags.GetByLabelKey(msgnotify.WellKnownLabelKeyExamEvent); !reflect.DeepEqual(v, []string{msgnotify.WellKnownLabelValueExamReportDeleted}) {
		t.Errorf("tag %s = %v, want [%s]", msgnotify.WellKnownLabelKeyExamEvent, v, msgnotify.WellKnownLabelValueExamReportDeleted)
	}
	for _, want := range []string{"alice", "r1"} {
		if !strings.Contains(got.msg.Text, want) {
			t.Errorf("text = %q, want it to mention %q", got.msg.Text, want)
		}
	}
}

func TestNotify_SkipsNotifiersWithIncompatibleFamilies(t *testing.T) {
	// This notifier accepts neither the service sender family nor the console
	// recipient family, so it must never be asked to send anything.
	notifier := &fakeNotifier{
		senderFamilies:    []msgnotify.MsgNotifyAddrFamily{msgnotify.MsgNotifyAddrFamilyEmail},
		recipientFamilies: []msgnotify.MsgNotifyAddrFamily{msgnotify.MsgNotifyAddrFamilyEmail},
		areYou:            true,
	}
	srv := NewOnMemoryExamTrackingServer([]msgnotify.MsgNotifySvc{notifier}, nil)
	ctx := context.Background()

	_ = srv.Put(ctx, "alice", mustReport(t, "r1"), false)
	_ = srv.DeleteExamTracking(ctx, "alice", "r1")

	if sent := notifier.sentMessages(); len(sent) != 0 {
		t.Errorf("sent %d messages, want 0 for an incompatible notifier", len(sent))
	}
}

func TestDeleteExamTracking_NotFoundSendsNoNotification(t *testing.T) {
	notifier := &fakeNotifier{
		senderFamilies:    []msgnotify.MsgNotifyAddrFamily{msgnotify.MsgNotifyAddrFamilyService},
		recipientFamilies: []msgnotify.MsgNotifyAddrFamily{msgnotify.MsgNotifyAddrFamilyService},
		areYou:            true,
	}
	srv := NewOnMemoryExamTrackingServer([]msgnotify.MsgNotifySvc{notifier}, nil)
	ctx := context.Background()

	if err := srv.DeleteExamTracking(ctx, "alice", "nope"); !errors.Is(err, ErrExamTrackingNotFound) {
		t.Fatalf("DeleteExamTracking = %v, want ErrExamTrackingNotFound", err)
	}
	if sent := notifier.sentMessages(); len(sent) != 0 {
		t.Errorf("sent %d messages, want 0 for a failed delete", len(sent))
	}
}

func TestNotify_NotifierErrorIsLoggedNotPropagated(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	notifier := &fakeNotifier{
		senderFamilies:    []msgnotify.MsgNotifyAddrFamily{msgnotify.MsgNotifyAddrFamilyService},
		recipientFamilies: []msgnotify.MsgNotifyAddrFamily{msgnotify.MsgNotifyAddrFamilyService},
		areYou:            true,
		sendErr:           errors.New("delivery exploded"),
	}
	srv := NewOnMemoryExamTrackingServer([]msgnotify.MsgNotifySvc{notifier}, nil)

	// Put must succeed even though the notifier failed.
	if err := srv.Put(context.Background(), "alice", mustReport(t, "r1"), false); err != nil {
		t.Fatalf("Put = %v, want nil: notification errors must not fail tracking", err)
	}
	if out := buf.String(); !strings.Contains(out, "notification delivery failed") || !strings.Contains(out, "delivery exploded") {
		t.Errorf("log output = %q, want it to mention the delivery failure and the error", out)
	}
}

func TestNotify_SkipsNotifierThatDoesNotClaimRecipient(t *testing.T) {
	// The families all match, but the notifier answers no to the AreYou probe
	// for the recipient address, so it must be skipped.
	notifier := &fakeNotifier{
		senderFamilies:    []msgnotify.MsgNotifyAddrFamily{msgnotify.MsgNotifyAddrFamilyService},
		recipientFamilies: []msgnotify.MsgNotifyAddrFamily{msgnotify.MsgNotifyAddrFamilyService},
		areYou:            false,
	}
	srv := NewOnMemoryExamTrackingServer([]msgnotify.MsgNotifySvc{notifier}, nil)
	ctx := context.Background()

	_ = srv.Put(ctx, "alice", mustReport(t, "r1"), false)

	if sent := notifier.sentMessages(); len(sent) != 0 {
		t.Errorf("sent %d messages, want 0 for a notifier that does not claim the recipient", len(sent))
	}
}
