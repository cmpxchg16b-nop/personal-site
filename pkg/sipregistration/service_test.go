package sipregistration

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"personal-site/pkg/models/ss"

	"github.com/emiago/diago"
	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/google/uuid"
	"github.com/pion/webrtc/v4"
)

const serviceTestBrowserOffer = "v=0\r\n" +
	"o=- 1 1 IN IP4 0.0.0.0\r\n" +
	"s=-\r\n" +
	"t=0 0\r\n" +
	"m=audio 9 UDP/TLS/RTP/SAVPF 111 0\r\n" +
	"c=IN IP4 0.0.0.0\r\n" +
	"a=rtpmap:111 opus/48000/2\r\n" +
	"a=fmtp:111 minptime=10;useinbandfec=1\r\n" +
	"a=rtpmap:0 PCMU/8000\r\n"

const serviceTestSIPAudio = "v=0\r\n" +
	"o=- 2 2 IN IP4 198.51.100.10\r\n" +
	"s=-\r\n" +
	"c=IN IP4 198.51.100.10\r\n" +
	"t=0 0\r\n" +
	"m=audio 4000 RTP/AVP 0\r\n" +
	"c=IN IP4 198.51.100.10\r\n" +
	"a=rtpmap:0 PCMU/8000\r\n"

type serviceRecordedEvent struct {
	fanout     bool
	userID     ss.UserId
	deliveryID string
	call       *ss.SIPCallEvent
	message    *ss.SIPMessageEvent
}

type serviceFakeBridge struct {
	mu         sync.Mutex
	events     []serviceRecordedEvent
	deliveries []string
	fanoutErr  error
	sendResult bool
}

func newServiceFakeBridge(deliveries ...string) *serviceFakeBridge {
	return &serviceFakeBridge{deliveries: append([]string(nil), deliveries...), sendResult: true}
}

func (b *serviceFakeBridge) FanoutSIP(_ context.Context, userID ss.UserId, call *ss.SIPCallEvent, message *ss.SIPMessageEvent) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	event := serviceRecordedEvent{fanout: true, userID: userID}
	if call != nil {
		event.call = cloneSIPCallEvent(call)
	}
	if message != nil {
		copy := *message
		event.message = &copy
	}
	b.events = append(b.events, event)
	return append([]string(nil), b.deliveries...), b.fanoutErr
}

func (b *serviceFakeBridge) SendSIP(_ context.Context, deliveryID string, call *ss.SIPCallEvent, message *ss.SIPMessageEvent) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	event := serviceRecordedEvent{deliveryID: deliveryID}
	if call != nil {
		event.call = cloneSIPCallEvent(call)
	}
	if message != nil {
		copy := *message
		event.message = &copy
	}
	b.events = append(b.events, event)
	return b.sendResult, nil
}

func (b *serviceFakeBridge) setDeliveries(deliveries ...string) {
	b.mu.Lock()
	b.deliveries = append([]string(nil), deliveries...)
	b.mu.Unlock()
}

func (b *serviceFakeBridge) matching(predicate func(serviceRecordedEvent) bool) []serviceRecordedEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	var result []serviceRecordedEvent
	for _, event := range b.events {
		if predicate(event) {
			result = append(result, event)
		}
	}
	return result
}

func (b *serviceFakeBridge) waitEvent(t *testing.T, description string, predicate func(serviceRecordedEvent) bool) serviceRecordedEvent {
	t.Helper()
	serviceWait(t, description, func() bool { return len(b.matching(predicate)) != 0 })
	return b.matching(predicate)[0]
}

func serviceWait(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

type serviceFakeRuntime struct {
	ctx    context.Context
	cancel context.CancelFunc

	mu         sync.Mutex
	sendErr    error
	sendCalls  int
	sendTarget string
	sendText   string
}

func newServiceFakeRuntime() *serviceFakeRuntime {
	ctx, cancel := context.WithCancel(context.Background())
	return &serviceFakeRuntime{ctx: ctx, cancel: cancel}
}

func (r *serviceFakeRuntime) Register(context.Context) error    { return nil }
func (r *serviceFakeRuntime) Refresh(context.Context) error     { return nil }
func (r *serviceFakeRuntime) Unregister(context.Context) error  { return nil }
func (r *serviceFakeRuntime) Expiry() time.Duration             { return time.Minute }
func (r *serviceFakeRuntime) Close() error                      { r.cancel(); return nil }
func (r *serviceFakeRuntime) OperationContext() context.Context { return r.ctx }
func (r *serviceFakeRuntime) MediaBindIP() net.IP               { return net.ParseIP("127.0.0.1") }
func (r *serviceFakeRuntime) StopOperations()                   { r.cancel() }
func (r *serviceFakeRuntime) Invite(context.Context, Credential, sip.Uri, *media.MediaSession, func(int)) (*diago.DialogClientSession, error) {
	return nil, errors.New("unexpected direct runtime INVITE")
}
func (r *serviceFakeRuntime) SendMessage(_ context.Context, _ Credential, target sip.Uri, text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sendCalls++
	r.sendTarget = target.String()
	r.sendText = text
	return r.sendErr
}

func newServiceLease(t *testing.T) (*Manager, *entry, *serviceFakeRuntime) {
	t.Helper()
	manager := NewManager(slog.New(slog.DiscardHandler))
	runtime := newServiceFakeRuntime()
	credential, err := normalizeInput(Input{Server: "pbx.example:5070", Username: "1001", Password: "secret", Transport: TransportUDP, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	credential.OwnerID = "user-alice"
	credential.ID = "credential-one"
	expiresAt := time.Now().Add(time.Hour)
	credential.Registration = Registration{State: RegistrationRegistered, ExpiresAt: &expiresAt}
	key := credentialKey{ownerID: credential.OwnerID, id: credential.ID}
	current := &entry{key: key, credential: credential, revision: 7, runtime: runtime}
	manager.entries[key] = current
	manager.aors[credential.SIPURI] = key
	t.Cleanup(func() { _ = manager.Close() })
	return manager, current, runtime
}

type serviceFakeBrowser struct {
	mu         sync.Mutex
	answer     webrtc.SessionDescription
	candidates []webrtc.ICECandidateInit
	closeCalls int
}

func newServiceFakeBrowser() *serviceFakeBrowser {
	return &serviceFakeBrowser{answer: webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: "browser-answer"}}
}

func (b *serviceFakeBrowser) AddCandidate(candidate webrtc.ICECandidateInit) error {
	b.mu.Lock()
	b.candidates = append(b.candidates, cloneICECandidateInit(candidate))
	b.mu.Unlock()
	return nil
}
func (b *serviceFakeBrowser) Close() error {
	b.mu.Lock()
	b.closeCalls++
	b.mu.Unlock()
	return nil
}
func (b *serviceFakeBrowser) AnswerDescription() webrtc.SessionDescription { return b.answer }
func (b *serviceFakeBrowser) candidateCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.candidates)
}

type serviceFakeRelay struct {
	mu         sync.Mutex
	closeCalls int
}

func (r *serviceFakeRelay) AddTrack(*webrtc.TrackRemote) {}
func (r *serviceFakeRelay) Close() error {
	r.mu.Lock()
	r.closeCalls++
	r.mu.Unlock()
	return nil
}

type serviceFakeClientDialog struct {
	ctx    context.Context
	cancel context.CancelFunc
	answer []byte

	mu          sync.Mutex
	operations  []string
	state       sip.DialogState
	ackCalls    int
	hangupCalls int
	closeCalls  int
}

func newServiceFakeClientDialog(answer string) *serviceFakeClientDialog {
	ctx, cancel := context.WithCancel(context.Background())
	return &serviceFakeClientDialog{ctx: ctx, cancel: cancel, answer: []byte(answer)}
}

func (d *serviceFakeClientDialog) Context() context.Context { return d.ctx }
func (d *serviceFakeClientDialog) AnswerSDP() []byte        { return append([]byte(nil), d.answer...) }
func (d *serviceFakeClientDialog) final2xx() bool           { return true }
func (d *serviceFakeClientDialog) acknowledged() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state >= sip.DialogStateConfirmed
}
func (d *serviceFakeClientDialog) Ack(context.Context) error {
	d.mu.Lock()
	d.ackCalls++
	d.state = sip.DialogStateConfirmed
	d.operations = append(d.operations, "ack")
	d.mu.Unlock()
	return nil
}
func (d *serviceFakeClientDialog) Hangup(context.Context) error {
	d.mu.Lock()
	d.hangupCalls++
	d.state = sip.DialogStateEnded
	d.operations = append(d.operations, "bye")
	d.mu.Unlock()
	d.cancel()
	return nil
}
func (d *serviceFakeClientDialog) Close() error {
	d.mu.Lock()
	d.closeCalls++
	d.operations = append(d.operations, "close")
	d.mu.Unlock()
	return nil
}
func (d *serviceFakeClientDialog) AudioReader(...diago.AudioReaderOption) (io.Reader, error) {
	return nil, errors.New("fake dialog has no audio")
}
func (d *serviceFakeClientDialog) MediaSession() *media.MediaSession { return nil }
func (d *serviceFakeClientDialog) RTPSession() *media.RTPSession     { return nil }
func (d *serviceFakeClientDialog) counts() (ack, hangup, close int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ackCalls, d.hangupCalls, d.closeCalls
}
func (d *serviceFakeClientDialog) operationOrder() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.operations...)
}

type serviceFakeServerDialog struct {
	ctx     context.Context
	cancel  context.CancelFunc
	request *sip.Request

	mu            sync.Mutex
	responses     []int
	ringingCalls  int
	answerCalls   int
	hangupCalls   int
	closeCalls    int
	answerSent    bool
	answerErr     error
	answerStarted chan struct{}
	answerRelease chan struct{}
	startOnce     sync.Once
}

func newServiceFakeServerDialog() *serviceFakeServerDialog {
	ctx, cancel := context.WithCancel(context.Background())
	request := sip.NewRequest(sip.INVITE, sip.Uri{Scheme: "sip", User: "1001", Host: "pbx.example"})
	from := &sip.FromHeader{Address: sip.Uri{Scheme: "sip", User: "caller", Host: "remote.example"}, Params: sip.NewParams()}
	from.Params.Add("tag", "remote-tag")
	request.AppendHeader(from)
	request.AppendHeader(&sip.ToHeader{Address: sip.Uri{Scheme: "sip", User: "1001", Host: "pbx.example"}})
	contentType := sip.ContentTypeHeader("application/sdp")
	request.AppendHeader(&contentType)
	request.SetBody([]byte(serviceTestSIPAudio))
	return &serviceFakeServerDialog{ctx: ctx, cancel: cancel, request: request, answerStarted: make(chan struct{})}
}

func (d *serviceFakeServerDialog) Context() context.Context { return d.ctx }
func (d *serviceFakeServerDialog) Request() *sip.Request    { return d.request }
func (d *serviceFakeServerDialog) Ringing() error {
	d.mu.Lock()
	d.ringingCalls++
	d.mu.Unlock()
	return nil
}
func (d *serviceFakeServerDialog) Answer() error {
	d.mu.Lock()
	d.answerCalls++
	d.answerSent = true
	answerErr := d.answerErr
	d.mu.Unlock()
	d.startOnce.Do(func() { close(d.answerStarted) })
	if d.answerRelease != nil {
		select {
		case <-d.answerRelease:
			return answerErr
		case <-d.ctx.Done():
			return d.ctx.Err()
		}
	}
	return answerErr
}
func (d *serviceFakeServerDialog) Answered() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.answerSent
}
func (d *serviceFakeServerDialog) Respond(status int, _ string, _ ...sip.Header) error {
	d.mu.Lock()
	d.responses = append(d.responses, status)
	d.mu.Unlock()
	d.cancel()
	return nil
}
func (d *serviceFakeServerDialog) Hangup(context.Context) error {
	d.mu.Lock()
	d.hangupCalls++
	d.mu.Unlock()
	d.cancel()
	return nil
}
func (d *serviceFakeServerDialog) Close() error {
	d.mu.Lock()
	d.closeCalls++
	d.mu.Unlock()
	return nil
}
func (d *serviceFakeServerDialog) AudioReader(...diago.AudioReaderOption) (io.Reader, error) {
	return nil, errors.New("fake dialog has no audio")
}
func (d *serviceFakeServerDialog) MediaSession() *media.MediaSession { return nil }
func (d *serviceFakeServerDialog) RTPSession() *media.RTPSession     { return nil }
func (d *serviceFakeServerDialog) counts() (ringing, answer, hangup, close int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ringingCalls, d.answerCalls, d.hangupCalls, d.closeCalls
}
func (d *serviceFakeServerDialog) responseCodes() []int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]int(nil), d.responses...)
}

func newServiceForTest(t *testing.T, bridge *serviceFakeBridge, options ...ServiceOption) (*Service, *Manager, *entry, *serviceFakeRuntime, *serviceFakeBrowser, *serviceFakeRelay) {
	t.Helper()
	manager, current, runtime := newServiceLease(t)
	service := NewService(manager, slog.New(slog.DiscardHandler), options...)
	service.SetBridge(bridge)
	browser := newServiceFakeBrowser()
	relay := &serviceFakeRelay{}
	service.newOutboundMedia = func(net.IP, sdpModel, bool) (*media.MediaSession, error) {
		return &media.MediaSession{}, nil
	}
	service.newBrowser = func(webrtc.SessionDescription, negotiatedSelection, []string, func(webrtc.ICECandidateInit), func(*webrtc.TrackRemote)) (browserMediaHandle, error) {
		return browser, nil
	}
	service.newInboundMedia = func(inboundDialog, net.IP, negotiatedSelection) (*media.MediaSession, *media.RTPSession, error) {
		return nil, nil, nil
	}
	service.newRelay = func(sipDialog, browserMediaHandle, negotiatedSelection) (mediaRelayHandle, error) {
		return relay, nil
	}
	t.Cleanup(func() { _ = service.Close() })
	return service, manager, current, runtime, browser, relay
}

func serviceDialEvent(callID string) *ss.SIPCallEvent {
	return &ss.SIPCallEvent{
		CallId: callID, CredentialId: "credential-one", Target: "2002", Kind: "voice", Action: "dial",
		SessionDesc: &webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: serviceTestBrowserOffer},
	}
}

func serviceAcceptEvent(callID string) *ss.SIPCallEvent {
	return &ss.SIPCallEvent{
		CallId: callID, Action: "accept", Kind: "voice",
		SessionDesc: &webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: serviceTestBrowserOffer},
	}
}

func callStateEvent(callID, deliveryID, state string) func(serviceRecordedEvent) bool {
	return func(event serviceRecordedEvent) bool {
		return !event.fanout && event.deliveryID == deliveryID && event.call != nil && event.call.CallId == callID && event.call.State == state
	}
}

func TestServiceOutboundLifecycleQueuesICEAndRejectsWrongDelivery(t *testing.T) {
	bridge := newServiceFakeBridge("origin")
	service, _, _, _, browser, relay := newServiceForTest(t, bridge)
	dialog := newServiceFakeClientDialog(serviceTestSIPAudio)
	inviteStarted := make(chan struct{})
	inviteRelease := make(chan struct{})
	var mu sync.Mutex
	inviteCalls := 0
	service.inviteOutbound = func(_ context.Context, _ operationRuntime, _ Credential, _ sip.Uri, _ *media.MediaSession, progress func(int)) (outboundDialog, error) {
		mu.Lock()
		inviteCalls++
		mu.Unlock()
		close(inviteStarted)
		progress(sip.StatusRinging)
		<-inviteRelease
		return dialog, nil
	}
	callID := uuid.NewString()
	service.HandleSIP("user-alice", "origin", serviceDialEvent(callID), nil)
	<-inviteStarted
	service.HandleSIP("user-alice", "origin", serviceDialEvent(callID), nil)
	service.HandleSIP("user-alice", "other", &ss.SIPCallEvent{CallId: callID, Action: "hangup"}, nil)
	service.HandleSIP("user-alice", "other", &ss.SIPCallEvent{CallId: callID, Action: "ice", RTCICECandidate: &webrtc.ICECandidateInit{Candidate: "wrong"}}, nil)
	service.HandleSIP("user-alice", "origin", &ss.SIPCallEvent{CallId: callID, Action: "ice", RTCICECandidate: &webrtc.ICECandidateInit{Candidate: "queued-before-pc"}}, nil)
	close(inviteRelease)

	accepted := bridge.waitEvent(t, "outbound accepted", callStateEvent(callID, "origin", "accepted"))
	if accepted.call.CredentialId != "credential-one" || accepted.call.LocalUri != "sip:1001@pbx.example" || accepted.call.RemoteUri != "sip:2002@pbx.example" || accepted.call.Kind != "voice" || accepted.call.Direction != "outgoing" {
		t.Fatalf("accepted metadata = %+v", accepted.call)
	}
	bridge.waitEvent(t, "outbound ringing", callStateEvent(callID, "origin", "ringing"))
	answering := bridge.waitEvent(t, "outbound answering", callStateEvent(callID, "origin", "answering"))
	if answering.call.SessionDesc == nil || answering.call.SessionDesc.Type != webrtc.SDPTypeAnswer {
		t.Fatalf("answering SDP = %+v", answering.call.SessionDesc)
	}
	serviceWait(t, "queued ICE candidate", func() bool { return browser.candidateCount() == 1 })
	if got := len(bridge.matching(callStateEvent(callID, "other", "ended"))); got != 0 {
		t.Fatal("wrong-delivery hangup affected the call")
	}
	mu.Lock()
	if inviteCalls != 1 {
		t.Fatalf("duplicate dial caused %d INVITEs", inviteCalls)
	}
	mu.Unlock()

	service.HandleSIP("user-alice", "origin", &ss.SIPCallEvent{CallId: callID, Action: "hangup"}, nil)
	bridge.waitEvent(t, "outbound ended", callStateEvent(callID, "origin", "ended"))
	serviceWait(t, "outbound cleanup", func() bool {
		ack, hangup, closed := dialog.counts()
		relay.mu.Lock()
		relayClosed := relay.closeCalls
		relay.mu.Unlock()
		return ack == 1 && hangup == 1 && closed == 1 && relayClosed == 1
	})
	if got := len(bridge.matching(callStateEvent(callID, "origin", "ended"))); got != 1 {
		t.Fatalf("terminal ended events = %d, want exactly one", got)
	}
}

func TestServiceOutboundImmediateControlOrdering(t *testing.T) {
	t.Run("immediate ICE is queued while INVITE is blocked", func(t *testing.T) {
		bridge := newServiceFakeBridge("origin")
		service, _, _, _, browser, _ := newServiceForTest(t, bridge)
		dialog := newServiceFakeClientDialog(serviceTestSIPAudio)
		inviteStarted := make(chan struct{})
		inviteRelease := make(chan struct{})
		service.inviteOutbound = func(ctx context.Context, _ operationRuntime, _ Credential, _ sip.Uri, _ *media.MediaSession, _ func(int)) (outboundDialog, error) {
			close(inviteStarted)
			select {
			case <-inviteRelease:
				return dialog, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		callID := uuid.NewString()
		service.HandleSIP("user-alice", "origin", serviceDialEvent(callID), nil)
		service.HandleSIP("user-alice", "origin", &ss.SIPCallEvent{
			CallId: callID, Action: "ice", RTCICECandidate: &webrtc.ICECandidateInit{Candidate: "immediate"},
		}, nil)
		select {
		case <-inviteStarted:
		case <-time.After(4 * time.Second):
			t.Fatal("INVITE did not start")
		}
		call := service.lookupCall(callID)
		serviceWait(t, "immediate ICE queued behind dial", func() bool {
			call.mu.Lock()
			defer call.mu.Unlock()
			return len(call.pending) == 1 && call.pending[0].Candidate == "immediate"
		})
		close(inviteRelease)
		bridge.waitEvent(t, "immediate-ICE call accepted", callStateEvent(callID, "origin", "accepted"))
		serviceWait(t, "immediate ICE applied", func() bool { return browser.candidateCount() == 1 })
		service.HandleSIP("user-alice", "origin", &ss.SIPCallEvent{CallId: callID, Action: "hangup"}, nil)
		bridge.waitEvent(t, "immediate-ICE call cleanup", callStateEvent(callID, "origin", "ended"))
		serviceWait(t, "immediate-ICE dialog cleanup", func() bool {
			ack, hangup, closed := dialog.counts()
			return ack == 1 && hangup == 1 && closed == 1
		})
	})

	t.Run("immediate disconnect cancels the dialed call", func(t *testing.T) {
		bridge := newServiceFakeBridge("origin")
		service, _, _, _, _, _ := newServiceForTest(t, bridge)
		inviteStarted := make(chan struct{})
		service.inviteOutbound = func(ctx context.Context, _ operationRuntime, _ Credential, _ sip.Uri, _ *media.MediaSession, _ func(int)) (outboundDialog, error) {
			close(inviteStarted)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		callID := uuid.NewString()
		service.HandleSIP("user-alice", "origin", serviceDialEvent(callID), nil)
		service.SIPDisconnected("origin")
		ended := bridge.waitEvent(t, "immediate-disconnect terminal event", callStateEvent(callID, "origin", "ended"))
		if ended.call.Reason != "browser disconnected" {
			t.Fatalf("immediate disconnect reason = %q", ended.call.Reason)
		}
		select {
		case <-inviteStarted:
		default:
			t.Fatal("call ended without starting its INVITE")
		}
		if got := len(bridge.matching(callStateEvent(callID, "origin", "accepted"))); got != 0 {
			t.Fatal("immediately disconnected call reached accepted")
		}
	})
}

func TestServiceOutboundDialogErrorStillAcksAndByes(t *testing.T) {
	for _, test := range []struct {
		name       string
		alreadyACK bool
	}{
		{name: "unacknowledged final 2xx dialog"},
		{name: "Diago already acknowledged", alreadyACK: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			bridge := newServiceFakeBridge("origin")
			service, _, _, _, _, _ := newServiceForTest(t, bridge)
			dialog := newServiceFakeClientDialog(serviceTestSIPAudio)
			if test.alreadyACK {
				dialog.mu.Lock()
				dialog.state = sip.DialogStateConfirmed
				dialog.ackCalls = 1
				dialog.operations = append(dialog.operations, "ack")
				dialog.mu.Unlock()
			}
			service.inviteOutbound = func(context.Context, operationRuntime, Credential, sip.Uri, *media.MediaSession, func(int)) (outboundDialog, error) {
				return dialog, errors.New("final 2xx media setup failed")
			}
			callID := uuid.NewString()
			service.HandleSIP("user-alice", "origin", serviceDialEvent(callID), nil)
			bridge.waitEvent(t, "dialog-plus-error failure", callStateEvent(callID, "origin", "failed"))
			serviceWait(t, "dialog-plus-error cleanup", func() bool {
				ack, hangup, closed := dialog.counts()
				return ack == 1 && hangup == 1 && closed == 1
			})
			order := dialog.operationOrder()
			if len(order) != 3 || order[0] != "ack" || order[1] != "bye" || order[2] != "close" {
				t.Fatalf("dialog-plus-error operations = %v, want one ACK then BYE and close", order)
			}
		})
	}
}

func TestServiceOutboundPreAnswerCancelAndDialogEnd(t *testing.T) {
	t.Run("cancel aborts INVITE before a final response", func(t *testing.T) {
		bridge := newServiceFakeBridge("origin")
		service, _, _, _, _, _ := newServiceForTest(t, bridge)
		inviteStarted := make(chan struct{})
		service.inviteOutbound = func(ctx context.Context, _ operationRuntime, _ Credential, _ sip.Uri, _ *media.MediaSession, _ func(int)) (outboundDialog, error) {
			close(inviteStarted)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		callID := uuid.NewString()
		service.HandleSIP("user-alice", "origin", serviceDialEvent(callID), nil)
		<-inviteStarted
		service.HandleSIP("user-alice", "origin", &ss.SIPCallEvent{CallId: callID, Action: "cancel"}, nil)
		bridge.waitEvent(t, "pre-answer cancellation", callStateEvent(callID, "origin", "cancelled"))
	})

	t.Run("remote dialog context produces one terminal event without BYE", func(t *testing.T) {
		bridge := newServiceFakeBridge("origin")
		service, _, _, _, _, _ := newServiceForTest(t, bridge)
		dialog := newServiceFakeClientDialog(serviceTestSIPAudio)
		service.inviteOutbound = func(context.Context, operationRuntime, Credential, sip.Uri, *media.MediaSession, func(int)) (outboundDialog, error) {
			return dialog, nil
		}
		callID := uuid.NewString()
		service.HandleSIP("user-alice", "origin", serviceDialEvent(callID), nil)
		bridge.waitEvent(t, "accepted before remote dialog end", callStateEvent(callID, "origin", "accepted"))
		dialog.cancel()
		bridge.waitEvent(t, "remote dialog terminal event", callStateEvent(callID, "origin", "ended"))
		serviceWait(t, "remote dialog cleanup", func() bool {
			ack, hangup, closed := dialog.counts()
			return ack == 1 && hangup == 0 && closed == 1
		})
		if got := len(bridge.matching(callStateEvent(callID, "origin", "ended"))); got != 1 {
			t.Fatalf("remote terminal events = %d, want one", got)
		}
	})
}

func TestServiceOutboundPost200CancelAcksThenByes(t *testing.T) {
	bridge := newServiceFakeBridge("origin")
	service, _, _, _, browser, _ := newServiceForTest(t, bridge)
	dialog := newServiceFakeClientDialog(serviceTestSIPAudio)
	service.inviteOutbound = func(context.Context, operationRuntime, Credential, sip.Uri, *media.MediaSession, func(int)) (outboundDialog, error) {
		return dialog, nil
	}
	browserStarted := make(chan struct{})
	browserRelease := make(chan struct{})
	service.newBrowser = func(webrtc.SessionDescription, negotiatedSelection, []string, func(webrtc.ICECandidateInit), func(*webrtc.TrackRemote)) (browserMediaHandle, error) {
		close(browserStarted)
		<-browserRelease
		return browser, nil
	}
	callID := uuid.NewString()
	service.HandleSIP("user-alice", "origin", serviceDialEvent(callID), nil)
	<-browserStarted
	service.HandleSIP("user-alice", "origin", &ss.SIPCallEvent{CallId: callID, Action: "cancel"}, nil)
	call := service.lookupCall(callID)
	serviceWait(t, "post-200 cancellation", func() bool {
		call.mu.Lock()
		defer call.mu.Unlock()
		return call.requested != nil
	})
	if ack, hangup, _ := dialog.counts(); ack != 0 || hangup != 0 {
		t.Fatalf("ACK/BYE occurred before controlled negotiation release: %d/%d", ack, hangup)
	}
	close(browserRelease)
	bridge.waitEvent(t, "cancelled outbound call", callStateEvent(callID, "origin", "cancelled"))
	serviceWait(t, "ACK then BYE", func() bool {
		ack, hangup, _ := dialog.counts()
		return ack == 1 && hangup == 1
	})
	order := dialog.operationOrder()
	if len(order) < 2 || order[0] != "ack" || order[1] != "bye" {
		t.Fatalf("dialog operation order = %v, want ACK then BYE", order)
	}
	if got := len(bridge.matching(callStateEvent(callID, "origin", "accepted"))); got != 0 {
		t.Fatal("cancelled pre-ACK call reached accepted")
	}
}

func TestServiceOutboundOwnershipAndCredentialReplacement(t *testing.T) {
	bridge := newServiceFakeBridge("origin")
	service, manager, current, _, _, _ := newServiceForTest(t, bridge)
	dialog := newServiceFakeClientDialog(serviceTestSIPAudio)
	var mu sync.Mutex
	inviteCalls := 0
	service.inviteOutbound = func(context.Context, operationRuntime, Credential, sip.Uri, *media.MediaSession, func(int)) (outboundDialog, error) {
		mu.Lock()
		inviteCalls++
		mu.Unlock()
		return dialog, nil
	}
	missingID := uuid.NewString()
	service.HandleSIP("user-bob", "origin", serviceDialEvent(missingID), nil)
	failed := bridge.waitEvent(t, "wrong-owner failure", callStateEvent(missingID, "origin", "failed"))
	if failed.call.Reason != "credential not found" {
		t.Fatalf("wrong-owner failure = %+v", failed.call)
	}
	mu.Lock()
	if inviteCalls != 0 {
		t.Fatal("wrong-owner dial reached INVITE")
	}
	mu.Unlock()

	callID := uuid.NewString()
	service.HandleSIP("user-alice", "origin", serviceDialEvent(callID), nil)
	bridge.waitEvent(t, "accepted before replacement", callStateEvent(callID, "origin", "accepted"))
	// Delete/Close increment revision before cancelWorker. Runtime-pointer
	// matching must still terminate the older leased call, not a replacement.
	manager.mu.Lock()
	current.revision++
	manager.mu.Unlock()
	manager.cancelWorker(current)
	ended := bridge.waitEvent(t, "replacement terminal event", callStateEvent(callID, "origin", "ended"))
	if ended.call.Reason != "credential replaced" && ended.call.Reason != "credential unavailable" {
		t.Fatalf("replacement reason = %q", ended.call.Reason)
	}
	serviceWait(t, "replacement BYE", func() bool {
		_, hangup, _ := dialog.counts()
		return hangup == 1
	})
}

func startServiceInbound(t *testing.T, service *Service, current *entry, runtime *serviceFakeRuntime, dialog *serviceFakeServerDialog) (string, <-chan struct{}) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		service.serveInboundInvite(current.credential, runtime, current.revision, dialog)
		close(done)
	}()
	serviceWait(t, "inbound 180", func() bool {
		ringing, _, _, _ := dialog.counts()
		return ringing == 1
	})
	call := (*serviceCall)(nil)
	service.mu.Lock()
	for _, candidate := range service.calls {
		if candidate.inDialog == dialog {
			call = candidate
		}
	}
	service.mu.Unlock()
	if call == nil {
		t.Fatal("inbound call was not registered")
	}
	return call.id, done
}

func waitServiceRoutine(t *testing.T, done <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func TestServiceInboundCallerIdentity(t *testing.T) {
	bridge := newServiceFakeBridge("tab-one")
	service, _, current, runtime, _, _ := newServiceForTest(t, bridge)
	dialog := newServiceFakeServerDialog()
	dialog.request.From().DisplayName = "Alice Caller"
	callID, done := startServiceInbound(t, service, current, runtime, dialog)
	ringing := bridge.waitEvent(t, "incoming caller identity", func(event serviceRecordedEvent) bool {
		return event.fanout && event.call != nil && event.call.CallId == callID && event.call.State == "ringing"
	})
	if ringing.call.RemoteName != "Alice Caller" || ringing.call.RemoteUri != "sip:caller@remote.example" {
		t.Fatalf("incoming caller identity = %+v", ringing.call)
	}

	dialog.cancel()
	cancelled := bridge.waitEvent(t, "cancelled caller identity", callStateEvent(callID, "tab-one", "cancelled"))
	if cancelled.call.RemoteName != "Alice Caller" || cancelled.call.RemoteUri != "sip:caller@remote.example" {
		t.Fatalf("cancelled caller identity = %+v", cancelled.call)
	}
	waitServiceRoutine(t, done, "caller-identity serve return")
}

func TestServiceInboundFirstAcceptCancelsLosers(t *testing.T) {
	bridge := newServiceFakeBridge("tab-one", "tab-two")
	service, _, current, runtime, _, _ := newServiceForTest(t, bridge)
	dialog := newServiceFakeServerDialog()
	callID, done := startServiceInbound(t, service, current, runtime, dialog)
	bridge.waitEvent(t, "incoming ringing fanout", func(event serviceRecordedEvent) bool {
		return event.fanout && event.call != nil && event.call.CallId == callID && event.call.State == "ringing" && event.call.Direction == "incoming"
	})

	service.HandleSIP("user-alice", "tab-two", serviceAcceptEvent(callID), nil)
	bridge.waitEvent(t, "winner answering", callStateEvent(callID, "tab-two", "answering"))
	bridge.waitEvent(t, "loser cancellation", callStateEvent(callID, "tab-one", "cancelled"))
	bridge.waitEvent(t, "winner accepted", callStateEvent(callID, "tab-two", "accepted"))
	service.HandleSIP("user-alice", "tab-one", serviceAcceptEvent(callID), nil)
	service.HandleSIP("user-alice", "tab-one", &ss.SIPCallEvent{CallId: callID, Action: "hangup"}, nil)
	if _, answer, _, _ := dialog.counts(); answer != 1 {
		t.Fatalf("SIP Answer calls = %d, want one winner", answer)
	}

	service.HandleSIP("user-alice", "tab-two", &ss.SIPCallEvent{CallId: callID, Action: "hangup"}, nil)
	bridge.waitEvent(t, "winner ended", callStateEvent(callID, "tab-two", "ended"))
	waitServiceRoutine(t, done, "inbound serve return")
	if _, _, hangup, _ := dialog.counts(); hangup != 1 {
		t.Fatalf("winner hangup caused %d BYEs", hangup)
	}
}

func TestServiceInboundInvalidAcceptRejectsOnlyAffectedLeg(t *testing.T) {
	t.Run("sole invalid accept rejects the call with 488", func(t *testing.T) {
		bridge := newServiceFakeBridge("winner")
		service, _, current, runtime, _, _ := newServiceForTest(t, bridge)
		dialog := newServiceFakeServerDialog()
		callID, done := startServiceInbound(t, service, current, runtime, dialog)
		invalid := serviceAcceptEvent(callID)
		invalid.SessionDesc = nil
		service.HandleSIP("user-alice", "winner", invalid, nil)
		rejected := bridge.waitEvent(t, "sole invalid leg rejection", callStateEvent(callID, "winner", "rejected"))
		if rejected.call.Reason != "invalid browser SDP offer" {
			t.Fatalf("invalid accept reason = %q", rejected.call.Reason)
		}
		waitServiceRoutine(t, done, "sole-invalid serve return")
		if codes := dialog.responseCodes(); len(codes) != 1 || codes[0] != 488 {
			t.Fatalf("sole-invalid SIP responses = %v, want [488]", codes)
		}
		if _, answer, hangup, _ := dialog.counts(); answer != 0 || hangup != 0 {
			t.Fatalf("sole-invalid Answer/BYE calls = %d/%d, want zero", answer, hangup)
		}
	})

	t.Run("invalid accept leaves another leg able to win", func(t *testing.T) {
		bridge := newServiceFakeBridge("tab-one", "tab-two")
		service, _, current, runtime, _, _ := newServiceForTest(t, bridge)
		dialog := newServiceFakeServerDialog()
		callID, done := startServiceInbound(t, service, current, runtime, dialog)
		invalid := serviceAcceptEvent(callID)
		invalid.SessionDesc.SDP = "not SDP"
		service.HandleSIP("user-alice", "tab-one", invalid, nil)
		rejected := bridge.waitEvent(t, "invalid leg rejection", callStateEvent(callID, "tab-one", "rejected"))
		if rejected.call.Reason != "no common browser/SIP media" {
			t.Fatalf("no-common-media reason = %q", rejected.call.Reason)
		}
		if codes := dialog.responseCodes(); len(codes) != 0 {
			t.Fatalf("invalid leg prematurely ended SIP: %v", codes)
		}
		call := service.lookupCall(callID)
		call.mu.Lock()
		winner := call.winner
		secondLegActive := !call.legs["tab-two"].terminal
		call.mu.Unlock()
		if winner != "" || !secondLegActive {
			t.Fatalf("invalid accept consumed winner %q or retired valid leg", winner)
		}

		service.HandleSIP("user-alice", "tab-two", serviceAcceptEvent(callID), nil)
		bridge.waitEvent(t, "valid leg answering", callStateEvent(callID, "tab-two", "answering"))
		bridge.waitEvent(t, "valid leg accepted", callStateEvent(callID, "tab-two", "accepted"))
		if _, answer, _, _ := dialog.counts(); answer != 1 {
			t.Fatalf("valid leg Answer calls = %d, want one", answer)
		}
		service.HandleSIP("user-alice", "tab-two", &ss.SIPCallEvent{CallId: callID, Action: "hangup"}, nil)
		bridge.waitEvent(t, "valid leg ended", callStateEvent(callID, "tab-two", "ended"))
		waitServiceRoutine(t, done, "invalid-then-valid serve return")
		if _, _, hangup, _ := dialog.counts(); hangup != 1 {
			t.Fatalf("valid leg hangup caused %d BYEs, want one", hangup)
		}
	})
}

func TestServiceInboundOneDeclineThenAllDecline(t *testing.T) {
	bridge := newServiceFakeBridge("tab-one", "tab-two")
	service, _, current, runtime, _, _ := newServiceForTest(t, bridge)
	dialog := newServiceFakeServerDialog()
	callID, done := startServiceInbound(t, service, current, runtime, dialog)
	service.HandleSIP("user-alice", "tab-one", &ss.SIPCallEvent{CallId: callID, Action: "decline"}, nil)
	bridge.waitEvent(t, "single-leg decline", callStateEvent(callID, "tab-one", "cancelled"))
	if codes := dialog.responseCodes(); len(codes) != 0 {
		t.Fatalf("one decline prematurely ended SIP: %v", codes)
	}
	service.HandleSIP("user-alice", "tab-two", &ss.SIPCallEvent{CallId: callID, Action: "decline"}, nil)
	bridge.waitEvent(t, "all-declined result", callStateEvent(callID, "tab-two", "rejected"))
	waitServiceRoutine(t, done, "all-declined serve return")
	if codes := dialog.responseCodes(); len(codes) != 1 || codes[0] != statusSIPDecline {
		t.Fatalf("all-decline SIP responses = %v, want [603]", codes)
	}
}

func TestServiceInboundCancelTimeoutAndDisconnect(t *testing.T) {
	t.Run("caller CANCEL cancels every leg", func(t *testing.T) {
		bridge := newServiceFakeBridge("tab-one", "tab-two")
		service, _, current, runtime, _, _ := newServiceForTest(t, bridge)
		dialog := newServiceFakeServerDialog()
		callID, done := startServiceInbound(t, service, current, runtime, dialog)
		dialog.cancel()
		bridge.waitEvent(t, "CANCEL first leg", callStateEvent(callID, "tab-one", "cancelled"))
		bridge.waitEvent(t, "CANCEL second leg", callStateEvent(callID, "tab-two", "cancelled"))
		waitServiceRoutine(t, done, "CANCEL serve return")
	})

	t.Run("ring timeout uses configured short deadline", func(t *testing.T) {
		bridge := newServiceFakeBridge("tab-one")
		service, _, current, runtime, _, _ := newServiceForTest(t, bridge, WithRingTimeout(15*time.Millisecond))
		dialog := newServiceFakeServerDialog()
		callID, done := startServiceInbound(t, service, current, runtime, dialog)
		bridge.waitEvent(t, "ring-timeout terminal event", callStateEvent(callID, "tab-one", "cancelled"))
		waitServiceRoutine(t, done, "ring-timeout serve return")
		if codes := dialog.responseCodes(); len(codes) != 1 || codes[0] != statusTemporarilyUnavailable {
			t.Fatalf("timeout responses = %v, want [480]", codes)
		}
	})

	t.Run("no queued delivery and disconnected legs return 480", func(t *testing.T) {
		bridge := newServiceFakeBridge()
		service, _, current, runtime, _, _ := newServiceForTest(t, bridge)
		noUA := newServiceFakeServerDialog()
		noUADone := make(chan struct{})
		go func() {
			service.serveInboundInvite(current.credential, runtime, current.revision, noUA)
			close(noUADone)
		}()
		waitServiceRoutine(t, noUADone, "no-UA serve return")
		if codes := noUA.responseCodes(); len(codes) != 1 || codes[0] != statusTemporarilyUnavailable {
			t.Fatalf("no-UA responses = %v, want [480]", codes)
		}

		bridge.setDeliveries("tab-one", "tab-two")
		dialog := newServiceFakeServerDialog()
		_, disconnectedDone := startServiceInbound(t, service, current, runtime, dialog)
		service.SIPDisconnected("tab-one")
		call := (*serviceCall)(nil)
		service.mu.Lock()
		for _, candidate := range service.calls {
			if candidate.inDialog == dialog {
				call = candidate
			}
		}
		service.mu.Unlock()
		serviceWait(t, "first disconnected leg", func() bool {
			call.mu.Lock()
			defer call.mu.Unlock()
			return call.legs["tab-one"].terminal && !call.legs["tab-two"].terminal
		})
		if codes := dialog.responseCodes(); len(codes) != 0 {
			t.Fatalf("one disconnect prematurely ended SIP: %v", codes)
		}
		service.SIPDisconnected("tab-two")
		waitServiceRoutine(t, disconnectedDone, "all-disconnected serve return")
		if codes := dialog.responseCodes(); len(codes) != 1 || codes[0] != statusTemporarilyUnavailable {
			t.Fatalf("all-disconnected responses = %v, want [480]", codes)
		}
	})
}

func TestServiceInboundMediaFailureBeforeAnswerUses488(t *testing.T) {
	for _, failure := range []string{"SIP media", "browser media"} {
		t.Run(failure, func(t *testing.T) {
			bridge := newServiceFakeBridge("winner")
			service, _, current, runtime, _, _ := newServiceForTest(t, bridge)
			if failure == "SIP media" {
				service.newInboundMedia = func(inboundDialog, net.IP, negotiatedSelection) (*media.MediaSession, *media.RTPSession, error) {
					return nil, nil, errors.New("SIP media setup failed")
				}
			} else {
				service.newBrowser = func(webrtc.SessionDescription, negotiatedSelection, []string, func(webrtc.ICECandidateInit), func(*webrtc.TrackRemote)) (browserMediaHandle, error) {
					return nil, errors.New("browser media setup failed")
				}
			}
			dialog := newServiceFakeServerDialog()
			callID, done := startServiceInbound(t, service, current, runtime, dialog)
			service.HandleSIP("user-alice", "winner", serviceAcceptEvent(callID), nil)
			bridge.waitEvent(t, "pre-answer media failure", callStateEvent(callID, "winner", "failed"))
			waitServiceRoutine(t, done, "pre-answer media failure serve return")
			if codes := dialog.responseCodes(); len(codes) != 1 || codes[0] != 488 {
				t.Fatalf("pre-answer media failure responses = %v, want [488]", codes)
			}
			if _, answer, hangup, _ := dialog.counts(); answer != 0 || hangup != 0 {
				t.Fatalf("pre-answer media failure Answer/BYE calls = %d/%d, want zero", answer, hangup)
			}
		})
	}
}

func TestServiceInboundAnswerSentErrorUsesBYE(t *testing.T) {
	bridge := newServiceFakeBridge("winner")
	service, _, current, runtime, _, _ := newServiceForTest(t, bridge)
	dialog := newServiceFakeServerDialog()
	dialog.answerErr = errors.New("ACK transaction failed after 200")
	callID, done := startServiceInbound(t, service, current, runtime, dialog)
	service.HandleSIP("user-alice", "winner", serviceAcceptEvent(callID), nil)
	bridge.waitEvent(t, "answer-sent error terminal event", callStateEvent(callID, "winner", "failed"))
	waitServiceRoutine(t, done, "answer-sent error serve return")
	if codes := dialog.responseCodes(); len(codes) != 0 {
		t.Fatalf("post-200 Answer error sent final responses %v, want BYE only", codes)
	}
	if _, answer, hangup, _ := dialog.counts(); answer != 1 || hangup != 1 {
		t.Fatalf("post-200 Answer error Answer/BYE calls = %d/%d, want 1/1", answer, hangup)
	}
}

func TestServiceInboundPostAnswerHangupUsesBYE(t *testing.T) {
	bridge := newServiceFakeBridge("winner")
	service, _, current, runtime, _, _ := newServiceForTest(t, bridge)
	dialog := newServiceFakeServerDialog()
	callID, done := startServiceInbound(t, service, current, runtime, dialog)
	service.HandleSIP("user-alice", "winner", serviceAcceptEvent(callID), nil)
	bridge.waitEvent(t, "post-answer accepted", callStateEvent(callID, "winner", "accepted"))
	service.HandleSIP("user-alice", "winner", &ss.SIPCallEvent{CallId: callID, Action: "hangup"}, nil)
	bridge.waitEvent(t, "post-answer ended", callStateEvent(callID, "winner", "ended"))
	waitServiceRoutine(t, done, "post-answer serve return")
	if codes := dialog.responseCodes(); len(codes) != 0 {
		t.Fatalf("post-answer hangup sent final responses %v, want BYE only", codes)
	}
	if _, answer, hangup, _ := dialog.counts(); answer != 1 || hangup != 1 {
		t.Fatalf("post-answer Answer/BYE calls = %d/%d, want 1/1", answer, hangup)
	}
}

func TestServiceInboundPreACKHangupWaitsForAnswer(t *testing.T) {
	bridge := newServiceFakeBridge("winner")
	service, _, current, runtime, _, _ := newServiceForTest(t, bridge)
	dialog := newServiceFakeServerDialog()
	dialog.answerRelease = make(chan struct{})
	callID, done := startServiceInbound(t, service, current, runtime, dialog)
	service.HandleSIP("user-alice", "winner", serviceAcceptEvent(callID), nil)
	select {
	case <-dialog.answerStarted:
	case <-time.After(4 * time.Second):
		t.Fatal("SIP Answer did not start")
	}
	service.HandleSIP("user-alice", "winner", &ss.SIPCallEvent{CallId: callID, Action: "hangup"}, nil)
	call := service.lookupCall(callID)
	serviceWait(t, "pre-ACK hangup request", func() bool {
		call.mu.Lock()
		defer call.mu.Unlock()
		return call.requested != nil
	})
	if _, _, hangup, _ := dialog.counts(); hangup != 0 {
		t.Fatal("BYE was sent before Answer returned")
	}
	close(dialog.answerRelease)
	bridge.waitEvent(t, "pre-ACK hangup terminal event", callStateEvent(callID, "winner", "ended"))
	waitServiceRoutine(t, done, "pre-ACK hangup serve return")
	if _, _, hangup, _ := dialog.counts(); hangup != 1 {
		t.Fatalf("pre-ACK hangup BYEs = %d, want one after Answer", hangup)
	}
	if got := len(bridge.matching(callStateEvent(callID, "winner", "accepted"))); got != 0 {
		t.Fatal("immediately hung-up pre-ACK call emitted accepted")
	}
}

func TestServiceMessageSendResultsAndInboundStatus(t *testing.T) {
	bridge := newServiceFakeBridge("incoming-delivery")
	service, _, _, runtime, _, _ := newServiceForTest(t, bridge)
	messageID := uuid.NewString()
	service.HandleSIP("user-alice", "origin", nil, &ss.SIPMessageEvent{
		MessageId: messageID, CredentialId: "credential-one", Target: "2002", Text: "hello", Action: "send",
	})
	sent := bridge.waitEvent(t, "MESSAGE sent result", func(event serviceRecordedEvent) bool {
		return !event.fanout && event.deliveryID == "origin" && event.message != nil && event.message.MessageId == messageID && event.message.State == "sent"
	})
	if sent.message.LocalUri != "sip:1001@pbx.example" || sent.message.RemoteUri != "sip:2002@pbx.example" || sent.message.Text != "hello" {
		t.Fatalf("sent metadata = %+v", sent.message)
	}

	runtime.mu.Lock()
	runtime.sendErr = errors.New("registrar rejected MESSAGE")
	runtime.mu.Unlock()
	failedID := uuid.NewString()
	service.HandleSIP("user-alice", "origin", nil, &ss.SIPMessageEvent{
		MessageId: failedID, CredentialId: "credential-one", Target: "2002", Text: "not logged", Action: "send",
	})
	failed := bridge.waitEvent(t, "MESSAGE failed result", func(event serviceRecordedEvent) bool {
		return !event.fanout && event.deliveryID == "origin" && event.message != nil && event.message.MessageId == failedID && event.message.State == "failed"
	})
	if failed.message.Reason == "" || strings.Contains(failed.message.Reason, "not logged") {
		t.Fatalf("failed MESSAGE reason = %q", failed.message.Reason)
	}

	credential := currentCredentialForTest(service)
	request := newServiceFakeServerDialog().request
	request.SetBody([]byte("incoming hello"))
	contentType := sip.ContentTypeHeader("Text/Plain; Charset=UTF-8")
	request.RemoveHeader("Content-Type")
	request.AppendHeader(&contentType)
	status, _, _ := service.handleInboundMessage(credential, request)
	if status != sip.StatusOK {
		t.Fatalf("text/plain with case/parameters status = %d", status)
	}
	incoming := bridge.waitEvent(t, "incoming MESSAGE", func(event serviceRecordedEvent) bool {
		return event.fanout && event.message != nil && event.message.State == "received" && event.message.Text == "incoming hello"
	})
	if incoming.message.CredentialId != credential.ID || incoming.message.LocalUri != credential.SIPURI || incoming.message.RemoteUri != "sip:caller@remote.example" || incoming.message.Direction != "incoming" || incoming.message.CreatedAt == 0 {
		t.Fatalf("incoming MESSAGE metadata = %+v", incoming.message)
	}

	request.SetBody(make([]byte, maxOutboundTextBytes+1))
	status, _, _ = service.handleInboundMessage(credential, request)
	if status != sip.StatusRequestEntityTooLarge {
		t.Fatalf("oversized MESSAGE status = %d, want 413", status)
	}
	request.SetBody([]byte("{}"))
	unsupported := sip.ContentTypeHeader("application/json")
	request.RemoveHeader("Content-Type")
	request.AppendHeader(&unsupported)
	status, _, headers := service.handleInboundMessage(credential, request)
	if status != sip.StatusUnsupportedMediaType || len(headers) != 1 || !strings.EqualFold(headers[0].Name(), "Accept") || headers[0].Value() != "text/plain" {
		t.Fatalf("unsupported MESSAGE = %d, headers %+v", status, headers)
	}
	contentType = sip.ContentTypeHeader("text/plain")
	request.RemoveHeader("Content-Type")
	request.AppendHeader(&contentType)
	request.SetBody([]byte("no delivery"))
	bridge.setDeliveries()
	status, _, _ = service.handleInboundMessage(credential, request)
	if status != statusTemporarilyUnavailable {
		t.Fatalf("undeliverable MESSAGE status = %d, want 480", status)
	}
}

func currentCredentialForTest(service *Service) Credential {
	service.manager.mu.RLock()
	defer service.manager.mu.RUnlock()
	for _, current := range service.manager.entries {
		return cloneCredential(current.credential)
	}
	return Credential{}
}

type serviceInviteObservation struct {
	kind string
	auth string
	from string
	body string
}

func startServiceInvitePBX(t *testing.T, address string) (chan serviceInviteObservation, func(), func()) {
	t.Helper()
	observations := make(chan serviceInviteObservation, 16)
	finalRelease := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(finalRelease) }) }
	ua, err := sipgo.NewUA()
	if err != nil {
		t.Fatal(err)
	}
	server, err := sipgo.NewServer(ua)
	if err != nil {
		t.Fatal(err)
	}
	server.OnRegister(func(request *sip.Request, transaction sip.ServerTransaction) {
		_ = transaction.Respond(sip.NewResponseFromRequest(request, sip.StatusOK, "OK", nil))
	})
	server.OnInvite(func(request *sip.Request, transaction sip.ServerTransaction) {
		authorization := request.GetHeader("Authorization")
		if authorization == nil || !strings.Contains(authorization.Value(), "Digest") {
			response := sip.NewResponseFromRequest(request, sip.StatusUnauthorized, "Unauthorized", nil)
			response.AppendHeader(sip.NewHeader("WWW-Authenticate", `Digest realm="loopback", nonce="0123456789abcdef", algorithm=MD5`))
			observations <- serviceInviteObservation{kind: "invite", from: request.From().Address.User, body: string(request.Body())}
			_ = transaction.Respond(response)
			return
		}
		observations <- serviceInviteObservation{kind: "invite", auth: authorization.Value(), from: request.From().Address.User, body: string(request.Body())}
		provisional := sip.NewResponseFromRequest(request, sip.StatusRinging, "Ringing", nil)
		if provisional.To().Params == nil {
			provisional.To().Params = sip.NewParams()
		}
		provisional.To().Params.Add("tag", "pbx-dialog-tag")
		if err := transaction.Respond(provisional); err != nil {
			t.Logf("PBX 180 response: %v", err)
		}
		<-finalRelease
		response := sip.NewSDPResponseFromRequest(request, []byte(serviceTestSIPAudio))
		if response.To().Params == nil {
			response.To().Params = sip.NewParams()
		}
		response.To().Params.Add("tag", "pbx-dialog-tag")
		host, portText, err := net.SplitHostPort(address)
		if err != nil {
			t.Errorf("PBX address: %v", err)
			return
		}
		port, err := net.LookupPort("udp", portText)
		if err != nil {
			t.Errorf("PBX port: %v", err)
			return
		}
		response.AppendHeader(&sip.ContactHeader{Address: sip.Uri{Scheme: "sip", User: "2002", Host: host, Port: port}})
		if err := transaction.Respond(response); err != nil {
			t.Logf("PBX 200 response: %v", err)
		}
	})
	server.OnAck(func(_ *sip.Request, _ sip.ServerTransaction) {
		observations <- serviceInviteObservation{kind: "ack"}
	})
	server.OnBye(func(request *sip.Request, transaction sip.ServerTransaction) {
		observations <- serviceInviteObservation{kind: "bye"}
		_ = transaction.Respond(sip.NewResponseFromRequest(request, sip.StatusOK, "OK", nil))
	})
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	listenCtx := context.WithValue(ctx, sipgo.ListenReadyCtxKey, sipgo.ListenReadyFuncCtxValue(func(string, string) { close(ready) }))
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe(listenCtx, "udp", address) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("INVITE PBX listener: %v", err)
	case <-time.After(time.Second):
		t.Fatal("INVITE PBX listener did not become ready")
	}
	cleanup := func() {
		release()
		cancel()
		_ = ua.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("INVITE PBX did not stop")
		}
	}
	return observations, release, cleanup
}

func waitServiceInviteObservation(t *testing.T, observations <-chan serviceInviteObservation, kind string, after int) serviceInviteObservation {
	t.Helper()
	seen := 0
	deadline := time.After(4 * time.Second)
	for {
		select {
		case observation := <-observations:
			if observation.kind != kind {
				continue
			}
			seen++
			if seen > after {
				return observation
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s observation %d", kind, after+1)
		}
	}
}

func TestServiceOutboundInviteOverRealUDPAuthenticatesAcksAndByes(t *testing.T) {
	reservation, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Skipf("UDP loopback unavailable: %v", err)
	}
	address := reservation.LocalAddr().String()
	_ = reservation.Close()
	observations, releaseFinal, stopPBX := startServiceInvitePBX(t, address)
	t.Cleanup(stopPBX)

	manager := NewManager(slog.New(slog.DiscardHandler))
	manager.registrationExpiry = time.Minute
	manager.attemptTimeout = time.Second
	manager.unregisterTimeout = time.Second
	t.Cleanup(func() { _ = manager.Close() })
	created, err := manager.Create("user-alice", Input{Server: address, Username: "1001", Password: "secret", Transport: TransportUDP, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	serviceWait(t, "INVITE test registration", func() bool {
		credential, _ := manager.Get("user-alice", created.ID)
		return credential.Registration.State == RegistrationRegistered
	})
	bridge := newServiceFakeBridge("origin")
	service := NewService(manager, slog.New(slog.DiscardHandler))
	service.SetBridge(bridge)
	browser := newServiceFakeBrowser()
	relay := &serviceFakeRelay{}
	service.newBrowser = func(webrtc.SessionDescription, negotiatedSelection, []string, func(webrtc.ICECandidateInit), func(*webrtc.TrackRemote)) (browserMediaHandle, error) {
		return browser, nil
	}
	service.newRelay = func(sipDialog, browserMediaHandle, negotiatedSelection) (mediaRelayHandle, error) {
		return relay, nil
	}
	t.Cleanup(func() { _ = service.Close() })

	callID := uuid.NewString()
	dial := serviceDialEvent(callID)
	dial.CredentialId = created.ID
	dial.Target = "2002"
	service.HandleSIP("user-alice", "origin", dial, nil)
	first := waitServiceInviteObservation(t, observations, "invite", 0)
	second := waitServiceInviteObservation(t, observations, "invite", 0)
	if first.auth != "" || !strings.Contains(second.auth, "Digest") || !strings.Contains(second.auth, `username="1001"`) {
		t.Fatalf("INVITE auth sequence = %q then %q", first.auth, second.auth)
	}
	if first.from != "1001" || second.from != "1001" || !strings.Contains(second.body, "m=audio") || !strings.Contains(second.body, "PCMU/8000") {
		t.Fatalf("INVITE identity/capabilities = %+v / %+v", first, second)
	}
	bridge.waitEvent(t, "real UDP INVITE ringing", callStateEvent(callID, "origin", "ringing"))
	releaseFinal()
	bridge.waitEvent(t, "real UDP INVITE answering", callStateEvent(callID, "origin", "answering"))
	bridge.waitEvent(t, "real UDP INVITE accepted", callStateEvent(callID, "origin", "accepted"))
	waitServiceInviteObservation(t, observations, "ack", 0)

	service.HandleSIP("user-alice", "origin", &ss.SIPCallEvent{CallId: callID, Action: "hangup"}, nil)
	waitServiceInviteObservation(t, observations, "bye", 0)
	bridge.waitEvent(t, "real UDP INVITE ended", callStateEvent(callID, "origin", "ended"))
}

func TestServiceRuntimeRejectsTaggedInviteBeforeDiagoUpdate(t *testing.T) {
	registrar := newLoopbackRegistrar(t)
	manager := NewManager(slog.New(slog.DiscardHandler))
	manager.registrationExpiry = time.Minute
	manager.attemptTimeout = time.Second
	manager.unregisterTimeout = time.Second
	t.Cleanup(func() { _ = manager.Close() })
	created, err := manager.Create("user-alice", Input{Server: registrar.address, Username: "1001", Password: "secret", Transport: TransportUDP, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	registration := registrar.waitObservation(t, "register", 0)
	serviceWait(t, "re-INVITE test registration", func() bool {
		credential, _ := manager.Get("user-alice", created.ID)
		return credential.Registration.State == RegistrationRegistered
	})
	contact, err := sipUriForLookup(registration.contact)
	if err != nil {
		t.Fatal(err)
	}
	request := sip.NewRequest(sip.INVITE, sip.Uri{Scheme: "sip", User: "1001", Host: contact.Host, Port: contact.Port})
	request.SetDestination(registration.source)
	from := &sip.FromHeader{Address: sip.Uri{Scheme: "sip", User: "caller", Host: "pbx.example"}, Params: sip.NewParams()}
	from.Params.Add("tag", "caller-tag")
	request.AppendHeader(from)
	to := &sip.ToHeader{Address: sip.Uri{Scheme: "sip", User: "1001", Host: "127.0.0.1"}, Params: sip.NewParams()}
	to.Params.Add("tag", "existing-dialog-tag")
	request.AppendHeader(to)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	response, err := registrar.client.Do(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 488 {
		t.Fatalf("tagged INVITE status = %d, want 488 before Diago media update", response.StatusCode)
	}
}

func TestServiceMessageOverRealUDPUsesOwnedDigestAndInboundFanout(t *testing.T) {
	registrar := newLoopbackRegistrar(t)
	manager := NewManager(slog.New(slog.DiscardHandler))
	manager.registrationExpiry = time.Minute
	manager.attemptTimeout = time.Second
	manager.unregisterTimeout = time.Second
	t.Cleanup(func() { _ = manager.Close() })
	created, err := manager.Create("user-alice", Input{Server: registrar.address, Username: "1001", Password: "secret", Transport: TransportUDP, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	registration := registrar.waitObservation(t, "register", 0)
	serviceWait(t, "real UDP registration", func() bool {
		credential, _ := manager.Get("user-alice", created.ID)
		return credential.Registration.State == RegistrationRegistered
	})
	bridge := newServiceFakeBridge("incoming-delivery")
	service := NewService(manager, slog.New(slog.DiscardHandler))
	service.SetBridge(bridge)
	t.Cleanup(func() { _ = service.Close() })

	messageID := uuid.NewString()
	service.HandleSIP("user-alice", "origin", nil, &ss.SIPMessageEvent{
		MessageId: messageID, CredentialId: created.ID, Target: "2002@target.invalid", Text: "hello world", Action: "send",
	})
	bridge.waitEvent(t, "real UDP sent result", func(event serviceRecordedEvent) bool {
		return !event.fanout && event.message != nil && event.message.MessageId == messageID && event.message.State == "sent"
	})
	first := registrar.waitObservation(t, "message", 0)
	second := registrar.waitObservation(t, "message", 0)
	if first.authorization != "" || !strings.Contains(second.authorization, "Digest") || !strings.Contains(second.authorization, `username="1001"`) {
		t.Fatalf("service MESSAGE auth sequence = %q then %q", first.authorization, second.authorization)
	}
	if first.body != "hello world" || second.body != "hello world" || first.from != "1001" || second.fromHost != "127.0.0.1" {
		t.Fatalf("service MESSAGE wire identity/body = %+v / %+v", first, second)
	}

	if status := registrar.sendMessage(t, registration, "1001"); status != sip.StatusOK {
		t.Fatalf("inbound service MESSAGE status = %d, want 200", status)
	}
	bridge.waitEvent(t, "real UDP inbound fanout", func(event serviceRecordedEvent) bool {
		return event.fanout && event.userID == "user-alice" && event.message != nil && event.message.State == "received" && event.message.Text == "hello" && event.message.CredentialId == created.ID
	})
	bridge.setDeliveries()
	if status := registrar.sendMessage(t, registration, "1001"); status != statusTemporarilyUnavailable {
		t.Fatalf("inbound MESSAGE without browser status = %d, want 480", status)
	}
	if status := registrar.sendMessageToHost(t, registration, "1001", "192.168.10.201"); status != statusTemporarilyUnavailable {
		t.Fatalf("inbound MESSAGE with PBX-rewritten To host status = %d, want 480 rather than 404", status)
	}
}
