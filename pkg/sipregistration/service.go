package sipregistration

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"personal-site/pkg/models/ss"

	"github.com/emiago/diago"
	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/google/uuid"
	"github.com/pion/webrtc/v4"
)

const (
	defaultServiceQueueSize  = 256
	defaultServiceSlots      = 64
	defaultInboundRingTime   = 60 * time.Second
	defaultSIPHubTimeout     = 2 * time.Second
	defaultSIPDialogTimeout  = 5 * time.Second
	defaultOutboundMsgTime   = 10 * time.Second
	statusSIPDecline         = 603
	maxSIPIdentifierBytes    = 128
	maxSIPDeliveryIDBytes    = 256
	maxSIPSessionDescription = 256 * 1024
	maxSIPCandidateBytes     = 8 * 1024
	maxSIPTerminalReason     = 160
)

// Bridge is the WebSocket delivery surface used by Service. Implementations
// must queue independently copied events and must not call back synchronously
// into Service while a delivery request is in flight.
type Bridge interface {
	FanoutSIP(context.Context, ss.UserId, *ss.SIPCallEvent, *ss.SIPMessageEvent) ([]string, error)
	SendSIP(context.Context, string, *ss.SIPCallEvent, *ss.SIPMessageEvent) (bool, error)
}

// ServiceOption configures a user SIP Service before it starts serving work.
type ServiceOption func(*Service)

// WithICEServers supplies the aggregate STUN/TURN URLs used by server-side
// answerer PeerConnections. Each call receives an independent copy.
func WithICEServers(urls []string) ServiceOption {
	return func(service *Service) {
		service.iceServers = append([]string(nil), urls...)
	}
}

// WithRingTimeout changes the inbound ringing deadline. It is primarily useful
// for focused tests; production uses the 60-second default.
func WithRingTimeout(timeout time.Duration) ServiceOption {
	return func(service *Service) {
		if timeout > 0 {
			service.ringTimeout = timeout
		}
	}
}

type browserMediaHandle interface {
	AddCandidate(webrtc.ICECandidateInit) error
	Close() error
	AnswerDescription() webrtc.SessionDescription
}

type serviceBrowserMedia struct {
	*browserMedia
}

func (b *serviceBrowserMedia) AnswerDescription() webrtc.SessionDescription {
	return b.Answer
}

type mediaRelayHandle interface {
	AddTrack(*webrtc.TrackRemote)
	Close() error
}

type outboundDialog interface {
	sipDialog
	Context() context.Context
	Ack(context.Context) error
	Hangup(context.Context) error
	Close() error
	AnswerSDP() []byte
}

type diagoOutboundDialog struct {
	*diago.DialogClientSession
}

func (d *diagoOutboundDialog) Ack(ctx context.Context) error {
	if d == nil || d.DialogClientSession == nil {
		return errors.New("outbound dialog is not initialized")
	}
	if d.acknowledged() {
		return nil
	}
	return d.DialogClientSession.Ack(ctx)
}

func (d *diagoOutboundDialog) acknowledged() bool {
	return d != nil && d.DialogClientSession != nil && d.LoadState() >= sip.DialogStateConfirmed
}

func (d *diagoOutboundDialog) final2xx() bool {
	return d != nil && d.DialogClientSession != nil && d.InviteResponse != nil && d.InviteResponse.IsSuccess()
}

func (d *diagoOutboundDialog) AnswerSDP() []byte {
	if d == nil || d.DialogClientSession == nil || d.InviteResponse == nil {
		return nil
	}
	return append([]byte(nil), d.InviteResponse.Body()...)
}

type inboundDialog interface {
	sipDialog
	Context() context.Context
	Ringing() error
	Answer() error
	Answered() bool
	Respond(int, string, ...sip.Header) error
	Hangup(context.Context) error
	Close() error
	Request() *sip.Request
}

type diagoInboundDialog struct {
	*diago.DialogServerSession
}

func (d *diagoInboundDialog) Answered() bool {
	if d == nil || d.DialogServerSession == nil {
		return false
	}
	state := d.LoadState()
	return state == sip.DialogStateEstablished || state == sip.DialogStateConfirmed
}

func (d *diagoInboundDialog) Hangup(ctx context.Context) error {
	if d == nil || d.DialogServerSession == nil {
		return errors.New("inbound dialog is not initialized")
	}
	if d.LoadState() == sip.DialogStateEstablished {
		return d.Bye(ctx)
	}
	return d.DialogServerSession.Hangup(ctx)
}

func (d *diagoInboundDialog) Respond(status int, phrase string, headers ...sip.Header) error {
	return d.DialogServerSession.Respond(status, phrase, nil, headers...)
}

func (d *diagoInboundDialog) Request() *sip.Request {
	return d.InviteRequest
}

type inboundRelayDialog struct {
	inboundDialog
	rtp *media.RTPSession
}

func (d *inboundRelayDialog) RTPSession() *media.RTPSession {
	return d.rtp
}

type outboundInviteFunc func(context.Context, operationRuntime, Credential, sip.Uri, *media.MediaSession, func(int)) (outboundDialog, error)
type outboundSIPMediaFunc func(net.IP, sdpModel, bool) (*media.MediaSession, error)
type browserMediaFunc func(webrtc.SessionDescription, negotiatedSelection, []string, func(webrtc.ICECandidateInit), func(*webrtc.TrackRemote)) (browserMediaHandle, error)
type inboundSIPMediaFunc func(inboundDialog, net.IP, negotiatedSelection) (*media.MediaSession, *media.RTPSession, error)
type mediaRelayFunc func(sipDialog, browserMediaHandle, negotiatedSelection) (mediaRelayHandle, error)

type serviceWorkKind uint8

const (
	serviceWorkEvent serviceWorkKind = iota
	serviceWorkDisconnect
)

type serviceWork struct {
	kind       serviceWorkKind
	userID     ss.UserId
	deliveryID string
	call       *ss.SIPCallEvent
	message    *ss.SIPMessageEvent
}

type callTermination struct {
	state     string
	reason    string
	sipStatus int
}

type inboundLeg struct {
	deliveryID string
	terminal   bool
	answering  bool
	pending    []webrtc.ICECandidateInit
}

type serviceCall struct {
	id            string
	ownerID       ss.UserId
	deliveryID    string
	credential    Credential
	runtime       operationRuntime
	revision      uint64
	direction     string
	kind          string
	localURI      string
	remoteURI     string
	createdAt     time.Time
	ctx           context.Context
	cancel        context.CancelFunc
	stopRuntime   func() bool
	cleanupOnce   sync.Once
	mu            sync.Mutex
	requested     *callTermination
	browser       browserMediaHandle
	relay         mediaRelayHandle
	pending       []webrtc.ICECandidateInit
	tracks        []*webrtc.TrackRemote
	ringingSent   bool
	accepted      bool
	answered      bool
	outDialog     outboundDialog
	inDialog      inboundDialog
	sipOffer      []byte
	legs          map[string]*inboundLeg
	winner        string
	answerStarted bool
	answerDone    chan struct{}
	answerErr     error
	acceptDone    chan struct{}
}

type outboundMessage struct {
	id         string
	ownerID    ss.UserId
	deliveryID string
	credential Credential
	target     string
	text       string
	createdAt  time.Time
}

// Service translates browser events to the user's owned SIP runtime and SIP
// callbacks back to browser events. The hub-facing methods only enqueue work;
// SIP and WebRTC operations run on separate, bounded goroutines.
type Service struct {
	manager *Manager
	logger  *slog.Logger

	ctx        context.Context
	cancel     context.CancelFunc
	queue      chan serviceWork
	slots      chan struct{}
	workerDone chan struct{}
	closeOnce  sync.Once

	mu          sync.Mutex
	closed      bool
	bridge      Bridge
	calls       map[string]*serviceCall
	messages    map[string]*outboundMessage
	iceServers  []string
	ringTimeout time.Duration
	now         func() time.Time
	newUUID     func() string

	inviteOutbound   outboundInviteFunc
	newOutboundMedia outboundSIPMediaFunc
	newBrowser       browserMediaFunc
	newInboundMedia  inboundSIPMediaFunc
	newRelay         mediaRelayFunc
}

// NewService constructs and runs a Service and installs it on manager. The
// manager closes the installed Service before tearing down any SIP runtime.
func NewService(manager *Manager, logger *slog.Logger, options ...ServiceOption) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	service := &Service{
		manager:     manager,
		logger:      logger,
		ctx:         ctx,
		cancel:      cancel,
		queue:       make(chan serviceWork, defaultServiceQueueSize),
		slots:       make(chan struct{}, defaultServiceSlots),
		workerDone:  make(chan struct{}),
		calls:       make(map[string]*serviceCall),
		messages:    make(map[string]*outboundMessage),
		ringTimeout: defaultInboundRingTime,
		now:         time.Now,
		newUUID:     uuid.NewString,
	}
	service.inviteOutbound = func(ctx context.Context, runtime operationRuntime, credential Credential, target sip.Uri, session *media.MediaSession, progress func(int)) (outboundDialog, error) {
		dialog, err := runtime.Invite(ctx, credential, target, session, progress)
		if dialog == nil {
			return nil, err
		}
		return &diagoOutboundDialog{DialogClientSession: dialog}, err
	}
	service.newOutboundMedia = newOutboundSIPMedia
	service.newBrowser = func(offer webrtc.SessionDescription, selection negotiatedSelection, iceServers []string, onCandidate func(webrtc.ICECandidateInit), onTrack func(*webrtc.TrackRemote)) (browserMediaHandle, error) {
		browser, err := newBrowserMedia(offer, selection, iceServers, onCandidate, onTrack)
		if err != nil {
			return nil, err
		}
		return &serviceBrowserMedia{browserMedia: browser}, nil
	}
	service.newInboundMedia = func(dialog inboundDialog, bindIP net.IP, selection negotiatedSelection) (*media.MediaSession, *media.RTPSession, error) {
		actual, ok := dialog.(*diagoInboundDialog)
		if !ok || actual.DialogServerSession == nil {
			return nil, nil, errors.New("inbound dialog is not a Diago server session")
		}
		return newInboundSIPMedia(actual.DialogServerSession, bindIP, selection)
	}
	service.newRelay = func(dialog sipDialog, browser browserMediaHandle, selection negotiatedSelection) (mediaRelayHandle, error) {
		actual, ok := browser.(*serviceBrowserMedia)
		if !ok || actual.browserMedia == nil {
			return nil, errors.New("browser media is not a Pion answerer")
		}
		return newMediaRelay(dialog, actual.browserMedia, selection)
	}
	for _, option := range options {
		option(service)
	}
	if manager != nil {
		manager.mu.Lock()
		previous := manager.service
		manager.service = service
		closed := manager.closed
		manager.mu.Unlock()
		if previous != nil && previous != service {
			previous.Close()
		}
		if closed {
			go service.run()
			service.Close()
			return service
		}
	}
	go service.run()
	return service
}

// SetBridge installs WebSocket delivery. It must be called before serving.
func (s *Service) SetBridge(bridge Bridge) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.bridge = bridge
	}
}

// HandleSIP implements websocket_ss.SIPHandler without waiting on the service
// queue or performing any network operation on the hub goroutine.
func (s *Service) HandleSIP(userID ss.UserId, deliveryID string, call *ss.SIPCallEvent, message *ss.SIPMessageEvent) {
	if (call == nil) == (message == nil) {
		return
	}
	work := serviceWork{kind: serviceWorkEvent, userID: userID, deliveryID: deliveryID}
	if call != nil {
		work.call = cloneSIPCallEvent(call)
	} else {
		copy := *message
		work.message = &copy
	}
	s.enqueue(work)
}

// SIPDisconnected implements websocket_ss.SIPHandler. Delivery ownership is
// resolved asynchronously against the current call generation.
func (s *Service) SIPDisconnected(deliveryID string) {
	s.enqueue(serviceWork{kind: serviceWorkDisconnect, deliveryID: deliveryID})
}

func cloneSIPCallEvent(event *ss.SIPCallEvent) *ss.SIPCallEvent {
	copy := *event
	if event.SessionDesc != nil {
		description := *event.SessionDesc
		copy.SessionDesc = &description
	}
	if event.RTCICECandidate != nil {
		candidate := cloneICECandidateInit(*event.RTCICECandidate)
		copy.RTCICECandidate = &candidate
	}
	return &copy
}

func (s *Service) enqueue(work serviceWork) {
	select {
	case <-s.ctx.Done():
		return
	default:
	}
	select {
	case s.queue <- work:
	case <-s.ctx.Done():
	default:
		s.logger.Warn("drop SIP browser work on full service queue", "delivery_id", work.deliveryID)
	}
}

func (s *Service) run() {
	defer close(s.workerDone)
	for {
		select {
		case <-s.ctx.Done():
			return
		case work := <-s.queue:
			s.process(work)
		}
	}
}

// startOperation runs blocking SIP work independently of the ordered control
// loop. Reserving a slot is nonblocking and channels are never closed.
func (s *Service) startOperation(operation func()) bool {
	if !s.reserveOperation() {
		return false
	}
	s.launchOperation(operation)
	return true
}

func (s *Service) reserveOperation() bool {
	select {
	case <-s.ctx.Done():
		return false
	default:
	}
	select {
	case s.slots <- struct{}{}:
		return true
	case <-s.ctx.Done():
		return false
	default:
		return false
	}
}

func (s *Service) launchOperation(operation func()) {
	go func() {
		defer func() { <-s.slots }()
		operation()
	}()
}

func (s *Service) releaseOperation() {
	<-s.slots
}

func (s *Service) process(work serviceWork) {
	if work.kind == serviceWorkDisconnect {
		s.processDisconnect(work.deliveryID)
		return
	}
	if work.call != nil {
		s.processCall(work.userID, work.deliveryID, work.call)
		return
	}
	if work.message != nil {
		s.processMessage(work.userID, work.deliveryID, work.message)
	}
}

// Close rejects new work and cancels every call. Runtime teardown by Manager
// unblocks any SIP stack operation still in flight; Close deliberately does not
// wait for a UAS ACK, which can otherwise take the full SIP retransmission timer.
func (s *Service) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		calls := make([]*serviceCall, 0, len(s.calls))
		for _, call := range s.calls {
			calls = append(calls, call)
		}
		s.mu.Unlock()
		s.cancel()
		for _, call := range calls {
			s.requestTermination(call, "ended", "service closed", statusTemporarilyUnavailable)
		}
		<-s.workerDone
		if s.manager != nil {
			s.manager.mu.Lock()
			if s.manager.service == s {
				s.manager.service = nil
			}
			s.manager.mu.Unlock()
		}
	})
	return nil
}

func (s *Service) bridgeSnapshot() Bridge {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	return s.bridge
}

func hubContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, defaultSIPHubTimeout)
}

func (s *Service) sendCall(deliveryID string, event *ss.SIPCallEvent) {
	bridge := s.bridgeSnapshot()
	if bridge == nil || deliveryID == "" {
		return
	}
	ctx, cancel := hubContext(context.Background())
	defer cancel()
	if _, err := bridge.SendSIP(ctx, deliveryID, event, nil); err != nil {
		s.logger.Warn("deliver targeted SIP call event failed", "call_id", event.CallId, "state", event.State)
	}
}

func (s *Service) sendMessage(deliveryID string, event *ss.SIPMessageEvent) {
	bridge := s.bridgeSnapshot()
	if bridge == nil || deliveryID == "" {
		return
	}
	ctx, cancel := hubContext(context.Background())
	defer cancel()
	if _, err := bridge.SendSIP(ctx, deliveryID, nil, event); err != nil {
		s.logger.Warn("deliver targeted SIP message event failed", "message_id", event.MessageId, "state", event.State)
	}
}

func (s *Service) eventFor(call *serviceCall, state, reason string) *ss.SIPCallEvent {
	return &ss.SIPCallEvent{
		CallId:       call.id,
		CredentialId: call.credential.ID,
		Target:       call.remoteURI,
		Kind:         call.kind,
		Direction:    call.direction,
		State:        state,
		LocalUri:     call.localURI,
		RemoteUri:    call.remoteURI,
		Reason:       conciseSIPReason(reason),
	}
}

func conciseSIPReason(reason string) string {
	reason = strings.Join(strings.Fields(reason), " ")
	if len(reason) > maxSIPTerminalReason {
		reason = reason[:maxSIPTerminalReason]
	}
	return reason
}

func validServiceID(value string) bool {
	return value != "" && len(value) <= maxSIPIdentifierBytes && utf8.ValidString(value) && !strings.ContainsAny(value, "\r\n\x00")
}

func validBrowserUUID(value string) bool {
	if !validServiceID(value) || len(value) > 64 {
		return false
	}
	_, err := uuid.Parse(value)
	return err == nil
}

func validOffer(description *webrtc.SessionDescription) bool {
	return description != nil && description.Type == webrtc.SDPTypeOffer && description.SDP != "" && len(description.SDP) <= maxSIPSessionDescription && utf8.ValidString(description.SDP)
}

func (s *Service) newCall(ownerID ss.UserId, deliveryID string, credential Credential, runtime operationRuntime, revision uint64, id, direction, kind, remoteURI string) *serviceCall {
	ctx, cancel := context.WithCancel(s.ctx)
	call := &serviceCall{
		id:         id,
		ownerID:    ownerID,
		deliveryID: deliveryID,
		credential: cloneCredential(credential),
		runtime:    runtime,
		revision:   revision,
		direction:  direction,
		kind:       kind,
		localURI:   credential.SIPURI,
		remoteURI:  remoteURI,
		createdAt:  s.now(),
		ctx:        ctx,
		cancel:     cancel,
	}
	if runtime.OperationContext() != nil {
		call.stopRuntime = context.AfterFunc(runtime.OperationContext(), func() {
			s.requestTermination(call, "ended", "credential unavailable", statusTemporarilyUnavailable)
		})
	}
	return call
}

func (s *Service) insertCall(call *serviceCall) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.calls[call.id] != nil {
		return false
	}
	s.calls[call.id] = call
	return true
}

func (s *Service) lookupCall(id string) *serviceCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[id]
}

func (s *Service) removeCall(call *serviceCall) {
	s.mu.Lock()
	if s.calls[call.id] == call {
		delete(s.calls, call.id)
	}
	s.mu.Unlock()
}

func (s *Service) requestTermination(call *serviceCall, state, reason string, status int) {
	call.mu.Lock()
	if call.requested == nil {
		call.requested = &callTermination{state: state, reason: conciseSIPReason(reason), sipStatus: status}
	}
	call.mu.Unlock()
	call.cancel()
}

func terminationFor(call *serviceCall, state, reason string, status int) callTermination {
	call.mu.Lock()
	defer call.mu.Unlock()
	if call.requested != nil {
		return *call.requested
	}
	return callTermination{state: state, reason: conciseSIPReason(reason), sipStatus: status}
}

func (s *Service) cancelCallsForRuntime(runtime operationRuntime) {
	if runtime == nil {
		return
	}
	s.mu.Lock()
	calls := make([]*serviceCall, 0, len(s.calls))
	for _, call := range s.calls {
		if call.runtime == runtime {
			calls = append(calls, call)
		}
	}
	s.mu.Unlock()
	for _, call := range calls {
		s.requestTermination(call, "ended", "credential replaced", statusTemporarilyUnavailable)
	}
}

func (s *Service) processCall(userID ss.UserId, deliveryID string, event *ss.SIPCallEvent) {
	if event.Action == "dial" {
		s.processDial(userID, deliveryID, event)
		return
	}
	call := s.lookupCall(event.CallId)
	if call == nil || call.ownerID != userID {
		return
	}
	switch event.Action {
	case "ice":
		s.processICE(call, deliveryID, event.RTCICECandidate)
	case "accept":
		s.processAccept(call, deliveryID, event)
	case "decline", "cancel", "hangup":
		s.processCallControl(call, deliveryID, event.Action)
	}
}

func (s *Service) processDial(userID ss.UserId, deliveryID string, event *ss.SIPCallEvent) {
	if !validBrowserUUID(event.CallId) || !validServiceID(deliveryID) || len(deliveryID) > maxSIPDeliveryIDBytes {
		return
	}
	failure := &ss.SIPCallEvent{
		CallId: event.CallId, CredentialId: event.CredentialId, Target: event.Target, Kind: event.Kind,
		Direction: "outgoing", State: "failed", Reason: "invalid dial request",
	}
	if !validServiceID(event.CredentialId) || (event.Kind != "voice" && event.Kind != "video") || !validOffer(event.SessionDesc) {
		s.sendCall(deliveryID, failure)
		return
	}
	credential, runtime, revision, err := s.manager.acquireOperation(string(userID), event.CredentialId)
	if err != nil {
		failure.Reason = operationReason(err)
		s.sendCall(deliveryID, failure)
		return
	}
	failure.LocalUri = credential.SIPURI
	target, display, err := parseSIPTarget(credential, event.Target)
	if err != nil {
		failure.Reason = "invalid SIP target"
		s.sendCall(deliveryID, failure)
		return
	}
	failure.RemoteUri = display
	browserOffer := *event.SessionDesc
	browserModel, err := parseBrowserSDP([]byte(browserOffer.SDP))
	if err != nil {
		failure.Reason = "invalid browser SDP offer"
		s.sendCall(deliveryID, failure)
		return
	}
	call := s.newCall(userID, deliveryID, credential, runtime, revision, event.CallId, "outgoing", event.Kind, display)
	if !s.insertCall(call) {
		call.cancel()
		if call.stopRuntime != nil {
			call.stopRuntime()
		}
		return
	}
	s.sendCall(deliveryID, s.eventFor(call, "dialing", ""))
	if !s.startOperation(func() {
		s.runOutbound(call, target, browserOffer, browserModel)
	}) {
		s.finishOutbound(call, nil, false, callTermination{state: "failed", reason: "SIP service busy"})
	}
}

func operationReason(err error) string {
	switch {
	case errors.Is(err, ErrNotFound):
		return "credential not found"
	case errors.Is(err, ErrOffline):
		return "credential offline"
	case errors.Is(err, context.DeadlineExceeded):
		return "operation timed out"
	case errors.Is(err, context.Canceled):
		return "operation cancelled"
	default:
		return "SIP operation failed"
	}
}

func (s *Service) runOutbound(call *serviceCall, target sip.Uri, offer webrtc.SessionDescription, browserModel sdpModel) {
	var dialog outboundDialog
	var acked bool
	state, reason := "failed", "SIP INVITE failed"
	defer func() {
		termination := terminationFor(call, state, reason, 0)
		s.finishOutbound(call, dialog, acked, termination)
	}()

	session, err := s.newOutboundMedia(call.runtime.MediaBindIP(), browserModel, call.kind == "video")
	if err != nil {
		s.logger.Warn("create outbound SIP media failed", "call_id", call.id, "err", err)
		reason = "no usable SIP audio media"
		return
	}
	progress := func(code int) {
		if code != sip.StatusRinging && code != sip.StatusSessionInProgress {
			return
		}
		call.mu.Lock()
		if call.ringingSent || call.requested != nil || call.ctx.Err() != nil {
			call.mu.Unlock()
			return
		}
		call.ringingSent = true
		call.mu.Unlock()
		s.sendCall(call.deliveryID, s.eventFor(call, "ringing", ""))
	}
	dialog, err = s.inviteOutbound(call.ctx, call.runtime, call.credential, target, session, progress)
	if err != nil {
		s.logger.Warn("outbound SIP INVITE failed", "call_id", call.id, "err", err)
		var response sipgo.ErrDialogResponse
		if errors.As(err, &response) && response.Res != nil {
			state = "rejected"
			reason = fmt.Sprintf("SIP call rejected (%d)", response.Res.StatusCode)
		} else {
			reason = operationReason(err)
		}
		return
	}
	call.mu.Lock()
	call.outDialog = dialog
	call.mu.Unlock()

	selection, negotiateErr := negotiateOutboundSelection([]byte(offer.SDP), dialog.AnswerSDP())
	if negotiateErr == nil {
		negotiateErr = s.validateSelectionEgress(call.credential, selection)
	}
	var browser browserMediaHandle
	if negotiateErr == nil && call.ctx.Err() == nil {
		browser, negotiateErr = s.newBrowser(offer, selection, append([]string(nil), s.iceServers...), func(candidate webrtc.ICECandidateInit) {
			event := s.eventFor(call, "", "")
			event.RTCICECandidate = &candidate
			s.sendCall(call.deliveryID, event)
		}, func(track *webrtc.TrackRemote) {
			s.addBrowserTrack(call, track)
		})
	}
	if negotiateErr == nil && browser != nil {
		call.mu.Lock()
		call.browser = browser
		pending := append([]webrtc.ICECandidateInit(nil), call.pending...)
		call.pending = nil
		cancelled := call.requested != nil || call.ctx.Err() != nil
		call.mu.Unlock()
		for _, candidate := range pending {
			if err := browser.AddCandidate(candidate); err != nil && negotiateErr == nil {
				negotiateErr = err
			}
		}
		if !cancelled && negotiateErr == nil {
			event := s.eventFor(call, "answering", "")
			answer := browser.AnswerDescription()
			event.SessionDesc = &answer
			s.sendCall(call.deliveryID, event)
		}
	}

	// A final 2xx must be ACKed even when cancellation or local negotiation
	// failed. BYE is sent by finishOutbound after this ACK when appropriate.
	ackContext, cancelAck := context.WithTimeout(context.Background(), defaultSIPDialogTimeout)
	ackErr := dialog.Ack(ackContext)
	cancelAck()
	if ackErr != nil {
		s.logger.Warn("outbound SIP ACK failed", "call_id", call.id, "err", ackErr)
		state, reason = "failed", "SIP ACK failed"
		return
	}
	acked = true
	if negotiateErr != nil {
		s.logger.Warn("outbound SIP/browser negotiation failed", "call_id", call.id, "err", negotiateErr)
		reason = "SIP/browser media negotiation failed"
		return
	}
	if call.ctx.Err() != nil {
		state, reason = "cancelled", "call cancelled"
		return
	}
	if browser == nil {
		reason = "browser media was not created"
		return
	}
	call.mu.Lock()
	cancelled := call.requested != nil || call.ctx.Err() != nil
	if !cancelled {
		call.accepted = true
	}
	call.mu.Unlock()
	if cancelled {
		state, reason = "cancelled", "call cancelled"
		return
	}
	s.sendCall(call.deliveryID, s.eventFor(call, "accepted", ""))
	relay, err := s.newRelay(dialog, browser, selection)
	if err != nil {
		s.logger.Warn("create outbound SIP media relay failed", "call_id", call.id, "err", err)
		reason = "SIP media relay failed"
		return
	}
	call.mu.Lock()
	call.relay = relay
	tracks := append([]*webrtc.TrackRemote(nil), call.tracks...)
	call.tracks = nil
	cancelled = call.requested != nil || call.ctx.Err() != nil
	call.mu.Unlock()
	for _, track := range tracks {
		relay.AddTrack(track)
	}
	if cancelled {
		state, reason = "ended", "call ended"
		return
	}

	select {
	case <-call.ctx.Done():
		state, reason = "ended", "call ended"
	case <-dialog.Context().Done():
		state, reason = "ended", "SIP dialog ended"
	}
}

func (s *Service) finishOutbound(call *serviceCall, dialog outboundDialog, acked bool, termination callTermination) {
	call.cleanupOnce.Do(func() {
		s.removeCall(call)
		call.cancel()
		if call.stopRuntime != nil {
			call.stopRuntime()
		}
		call.mu.Lock()
		relay := call.relay
		browser := call.browser
		call.relay = nil
		call.browser = nil
		call.mu.Unlock()
		if relay != nil {
			_ = relay.Close()
		} else if browser != nil {
			_ = browser.Close()
		}
		if dialog != nil {
			final2xx := true
			if stateful, ok := dialog.(interface{ final2xx() bool }); ok {
				final2xx = stateful.final2xx()
			}
			if final2xx {
				if stateful, ok := dialog.(interface{ acknowledged() bool }); ok && stateful.acknowledged() {
					acked = true
				}
				if !acked {
					ctx, cancel := context.WithTimeout(context.Background(), defaultSIPDialogTimeout)
					ackErr := dialog.Ack(ctx)
					cancel()
					acked = ackErr == nil
					if stateful, ok := dialog.(interface{ acknowledged() bool }); ok && stateful.acknowledged() {
						acked = true
					}
				}
				if acked && dialog.Context().Err() == nil {
					ctx, cancel := context.WithTimeout(context.Background(), defaultSIPDialogTimeout)
					_ = dialog.Hangup(ctx)
					cancel()
				}
			}
			_ = dialog.Close()
		}
		s.sendCall(call.deliveryID, s.eventFor(call, termination.state, termination.reason))
	})
}

func (s *Service) addBrowserTrack(call *serviceCall, track *webrtc.TrackRemote) {
	if track == nil {
		return
	}
	call.mu.Lock()
	relay := call.relay
	if relay == nil {
		call.tracks = append(call.tracks, track)
		call.mu.Unlock()
		return
	}
	call.mu.Unlock()
	relay.AddTrack(track)
}

func (s *Service) processICE(call *serviceCall, deliveryID string, candidate *webrtc.ICECandidateInit) {
	if candidate == nil || len(candidate.Candidate) > maxSIPCandidateBytes || !utf8.ValidString(candidate.Candidate) ||
		(candidate.SDPMid != nil && len(*candidate.SDPMid) > maxSIPIdentifierBytes) ||
		(candidate.UsernameFragment != nil && len(*candidate.UsernameFragment) > maxSIPIdentifierBytes) {
		return
	}
	copy := cloneICECandidateInit(*candidate)
	call.mu.Lock()
	if call.direction == "outgoing" {
		if deliveryID != call.deliveryID || call.requested != nil {
			call.mu.Unlock()
			return
		}
		if call.browser == nil {
			call.pending = append(call.pending, copy)
			call.mu.Unlock()
			return
		}
		browser := call.browser
		call.mu.Unlock()
		_ = browser.AddCandidate(copy)
		return
	}
	leg := call.legs[deliveryID]
	if leg == nil || leg.terminal || call.winner != deliveryID {
		call.mu.Unlock()
		return
	}
	if call.browser == nil {
		leg.pending = append(leg.pending, copy)
		call.mu.Unlock()
		return
	}
	browser := call.browser
	call.mu.Unlock()
	_ = browser.AddCandidate(copy)
}

func (s *Service) processCallControl(call *serviceCall, deliveryID, action string) {
	if call.direction == "outgoing" {
		if deliveryID != call.deliveryID {
			return
		}
		if action == "cancel" {
			s.requestTermination(call, "cancelled", "cancelled by browser", 0)
		} else {
			s.requestTermination(call, "ended", "ended by browser", 0)
		}
		return
	}
	call.mu.Lock()
	leg := call.legs[deliveryID]
	if leg == nil || leg.terminal || call.requested != nil {
		call.mu.Unlock()
		return
	}
	if call.winner == "" {
		active := 0
		for _, candidate := range call.legs {
			if !candidate.terminal {
				active++
			}
		}
		allDeclined := active == 1
		if !allDeclined {
			leg.terminal = true
		}
		call.mu.Unlock()
		if allDeclined {
			s.requestTermination(call, "rejected", "all deliveries declined", statusSIPDecline)
		} else {
			s.sendCall(deliveryID, s.eventFor(call, "cancelled", "declined by this delivery"))
		}
		return
	}
	winner := call.winner
	call.mu.Unlock()
	if deliveryID == winner {
		s.requestTermination(call, "ended", "ended by browser", statusTemporarilyUnavailable)
	}
}

func (s *Service) processDisconnect(deliveryID string) {
	s.mu.Lock()
	calls := make([]*serviceCall, 0, len(s.calls))
	for _, call := range s.calls {
		calls = append(calls, call)
	}
	s.mu.Unlock()
	for _, call := range calls {
		if call.direction == "outgoing" {
			if call.deliveryID == deliveryID {
				s.requestTermination(call, "ended", "browser disconnected", 0)
			}
			continue
		}
		call.mu.Lock()
		leg := call.legs[deliveryID]
		if leg == nil || leg.terminal || call.requested != nil {
			call.mu.Unlock()
			continue
		}
		leg.terminal = true
		winner := call.winner
		active := 0
		for _, candidate := range call.legs {
			if !candidate.terminal {
				active++
			}
		}
		call.mu.Unlock()
		if winner == deliveryID {
			s.requestTermination(call, "ended", "browser disconnected", statusTemporarilyUnavailable)
		} else if winner == "" && active == 0 {
			s.requestTermination(call, "cancelled", "no active delivery", statusTemporarilyUnavailable)
		}
	}
}

func (s *Service) validateSelectionEgress(credential Credential, selection negotiatedSelection) error {
	sections := []*mediaSection{selection.SIPAudio}
	if len(selection.Video) == 1 {
		sections = append(sections, selection.SIPVideo)
	}
	for _, section := range sections {
		if section == nil || section.Port == 0 {
			continue
		}
		if err := s.validateMediaIP(credential, section.ConnectionIP); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) validateMediaIP(_ Credential, ip net.IP) error {
	if ip == nil {
		return errors.New("SIP media address is missing")
	}
	return nil
}

func (s *Service) processMessage(userID ss.UserId, deliveryID string, event *ss.SIPMessageEvent) {
	if event.Action != "send" || !validBrowserUUID(event.MessageId) || !validServiceID(deliveryID) || len(deliveryID) > maxSIPDeliveryIDBytes {
		return
	}
	result := &ss.SIPMessageEvent{
		MessageId: event.MessageId, CredentialId: event.CredentialId, Target: event.Target, Text: event.Text,
		Direction: "outgoing", State: "failed", CreatedAt: float64(s.now().UnixNano()) / 1e9,
	}
	if !validServiceID(event.CredentialId) {
		result.Reason = "invalid message request"
		s.sendMessage(deliveryID, result)
		return
	}
	s.mu.Lock()
	if s.closed || s.messages[event.MessageId] != nil {
		s.mu.Unlock()
		return
	}
	pending := &outboundMessage{id: event.MessageId, ownerID: userID, deliveryID: deliveryID, createdAt: s.now()}
	s.messages[event.MessageId] = pending
	s.mu.Unlock()
	cleanup := func() {
		s.mu.Lock()
		if s.messages[event.MessageId] == pending {
			delete(s.messages, event.MessageId)
		}
		s.mu.Unlock()
	}
	async := false
	defer func() {
		if !async {
			cleanup()
		}
	}()

	credential, runtime, _, err := s.manager.acquireOperation(string(userID), event.CredentialId)
	if err != nil {
		result.Reason = operationReason(err)
		s.sendMessage(deliveryID, result)
		return
	}
	result.LocalUri = credential.SIPURI
	target, display, err := parseSIPTarget(credential, event.Target)
	if err != nil {
		result.Reason = "invalid SIP target"
		s.sendMessage(deliveryID, result)
		return
	}
	result.RemoteUri = display
	if err := validateOutboundText(event.Text); err != nil {
		result.Reason = "invalid message text"
		s.sendMessage(deliveryID, result)
		return
	}
	text := event.Text
	if !s.startOperation(func() {
		defer cleanup()
		ctx, cancel := context.WithTimeout(s.ctx, defaultOutboundMsgTime)
		defer cancel()
		if runtime.OperationContext() != nil {
			stop := context.AfterFunc(runtime.OperationContext(), cancel)
			defer stop()
		}
		if err := runtime.SendMessage(ctx, credential, target, text); err != nil {
			result.Reason = operationReason(err)
			s.sendMessage(deliveryID, result)
			return
		}
		result.State = "sent"
		result.Reason = ""
		s.sendMessage(deliveryID, result)
	}) {
		result.Reason = "SIP service busy"
		s.sendMessage(deliveryID, result)
		return
	}
	async = true
}

func (s *Service) serveInboundInvite(credential Credential, runtime operationRuntime, revision uint64, dialog inboundDialog) {
	request := dialog.Request()
	if request == nil || !validInboundContentType(request.ContentType(), "application/sdp") || len(request.Body()) == 0 || len(request.Body()) > maxSIPSessionDescription {
		s.respondInbound(dialog, 488, "Not Acceptable Here", nil)
		return
	}
	offer := append([]byte(nil), request.Body()...)
	model, err := parseSDP(offer)
	if err != nil || !activeMedia(model.Audio) || len(primaryRTPCodecs(model.Audio.Codecs)) == 0 {
		s.respondInbound(dialog, 488, "Not Acceptable Here", nil)
		return
	}
	for _, section := range []*mediaSection{model.Audio, model.Video} {
		if section != nil && section.Port != 0 {
			if err := s.validateMediaIP(credential, section.ConnectionIP); err != nil {
				s.respondInbound(dialog, sip.StatusForbidden, "Forbidden", nil)
				return
			}
		}
	}
	kind := "voice"
	if activeMedia(model.Video) {
		kind = "video"
	}
	remoteURI := sipHeaderURI(request.From())
	call := s.newCall(ss.UserId(credential.OwnerID), "", credential, runtime, revision, s.newUUID(), "incoming", kind, remoteURI)
	call.inDialog = dialog
	call.sipOffer = offer
	call.legs = make(map[string]*inboundLeg)
	if !s.insertCall(call) {
		call.cancel()
		if call.stopRuntime != nil {
			call.stopRuntime()
		}
		s.respondInbound(dialog, statusTemporarilyUnavailable, "Temporarily Unavailable", nil)
		return
	}

	call.mu.Lock()
	bridge := s.bridgeSnapshot()
	if bridge == nil {
		call.mu.Unlock()
		s.respondInbound(dialog, statusTemporarilyUnavailable, "Temporarily Unavailable", nil)
		s.finishInbound(call, callTermination{state: "cancelled", reason: "no active delivery"})
		return
	}
	ctx, cancel := hubContext(call.ctx)
	deliveries, fanoutErr := bridge.FanoutSIP(ctx, call.ownerID, s.eventFor(call, "ringing", ""), nil)
	cancel()
	if fanoutErr == nil {
		for _, deliveryID := range deliveries {
			if validServiceID(deliveryID) && len(deliveryID) <= maxSIPDeliveryIDBytes {
				call.legs[deliveryID] = &inboundLeg{deliveryID: deliveryID}
			}
		}
	}
	active := len(call.legs)
	call.mu.Unlock()
	if fanoutErr != nil || active == 0 {
		s.respondInbound(dialog, statusTemporarilyUnavailable, "Temporarily Unavailable", nil)
		s.finishInbound(call, callTermination{state: "cancelled", reason: "no active delivery"})
		return
	}
	if err := dialog.Ringing(); err != nil {
		s.finishInbound(call, callTermination{state: "failed", reason: "SIP ringing failed"})
		return
	}
	s.waitInbound(call)
}

func validInboundContentType(header *sip.ContentTypeHeader, expected string) bool {
	if header == nil {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(header.Value())
	return err == nil && strings.EqualFold(mediaType, expected)
}

func sipHeaderURI(header *sip.FromHeader) string {
	if header == nil || header.Address.User == "" || header.Address.Host == "" {
		return "sip:unknown@invalid"
	}
	return canonicalAOR(header.Address.User, header.Address.Host)
}

func (s *Service) respondInbound(dialog inboundDialog, status int, phrase string, headers []sip.Header) {
	if err := dialog.Respond(status, phrase, headers...); err != nil {
		s.logger.Warn("respond to inbound SIP request failed", "status", status)
	}
}

func inboundStatusPhrase(status int) string {
	switch status {
	case sip.StatusForbidden:
		return "Forbidden"
	case statusTemporarilyUnavailable:
		return "Temporarily Unavailable"
	case 488:
		return "Not Acceptable Here"
	case statusSIPDecline:
		return "Decline"
	default:
		return "Call Failed"
	}
}

func (s *Service) waitInbound(call *serviceCall) {
	timer := time.NewTimer(s.ringTimeout)
	defer timer.Stop()
	var timeout <-chan time.Time = timer.C
	for {
		select {
		case <-call.inDialog.Context().Done():
			s.waitInboundAccept(call)
			call.mu.Lock()
			answered := call.answered
			call.mu.Unlock()
			state, reason := "cancelled", "caller cancelled"
			if answered {
				state, reason = "ended", "SIP dialog ended"
			}
			termination := terminationFor(call, state, reason, 0)
			s.finishInbound(call, termination)
			return
		case <-call.ctx.Done():
			termination := terminationFor(call, "ended", "call ended", statusTemporarilyUnavailable)
			call.mu.Lock()
			answering := call.answerStarted
			call.mu.Unlock()
			answered := false
			if answering {
				select {
				case <-call.answerDone:
				case <-call.inDialog.Context().Done():
				}
				s.waitInboundAccept(call)
				call.mu.Lock()
				answered = call.answered
				call.mu.Unlock()
			}
			if call.inDialog.Context().Err() == nil {
				if answered {
					ctx, cancel := context.WithTimeout(context.Background(), defaultSIPDialogTimeout)
					_ = call.inDialog.Hangup(ctx)
					cancel()
				} else {
					status := termination.sipStatus
					if status == 0 {
						status = statusTemporarilyUnavailable
					}
					s.respondInbound(call.inDialog, status, inboundStatusPhrase(status), nil)
				}
			}
			s.finishInbound(call, termination)
			return
		case <-timeout:
			call.mu.Lock()
			winner := call.winner
			call.mu.Unlock()
			if winner != "" {
				timeout = nil
				continue
			}
			s.requestTermination(call, "cancelled", "ring timeout", statusTemporarilyUnavailable)
		}
	}
}

func (s *Service) waitInboundAccept(call *serviceCall) {
	call.mu.Lock()
	answering := call.answerStarted
	done := call.acceptDone
	call.mu.Unlock()
	if answering && done != nil {
		<-done
	}
}

func (s *Service) finishInbound(call *serviceCall, termination callTermination) {
	call.cleanupOnce.Do(func() {
		s.removeCall(call)
		call.cancel()
		if call.stopRuntime != nil {
			call.stopRuntime()
		}
		call.mu.Lock()
		relay := call.relay
		browser := call.browser
		call.relay = nil
		call.browser = nil
		deliveries := make([]string, 0, len(call.legs))
		for id, leg := range call.legs {
			if !leg.terminal {
				leg.terminal = true
				deliveries = append(deliveries, id)
			}
		}
		call.mu.Unlock()
		if relay != nil {
			_ = relay.Close()
		} else if browser != nil {
			_ = browser.Close()
		}
		if call.inDialog != nil {
			_ = call.inDialog.Close()
		}
		for _, deliveryID := range deliveries {
			s.sendCall(deliveryID, s.eventFor(call, termination.state, termination.reason))
		}
	})
}

func (s *Service) processAccept(call *serviceCall, deliveryID string, event *ss.SIPCallEvent) {
	if call.direction != "incoming" {
		return
	}
	call.mu.Lock()
	leg := call.legs[deliveryID]
	available := leg != nil && !leg.terminal && call.winner == "" && call.requested == nil && call.ctx.Err() == nil
	offer := append([]byte(nil), call.sipOffer...)
	call.mu.Unlock()
	if !available {
		return
	}
	if !validOffer(event.SessionDesc) {
		s.rejectInboundAccept(call, deliveryID, "invalid browser SDP offer", 488)
		return
	}
	selection, err := negotiateInboundSelection([]byte(event.SessionDesc.SDP), offer)
	if err != nil {
		s.rejectInboundAccept(call, deliveryID, "no common browser/SIP media", 488)
		return
	}
	if err := s.validateSelectionEgress(call.credential, selection); err != nil {
		s.rejectInboundAccept(call, deliveryID, "SIP media address is not allowed", sip.StatusForbidden)
		return
	}
	if !s.reserveOperation() {
		s.rejectInboundAccept(call, deliveryID, "SIP service busy", statusTemporarilyUnavailable)
		return
	}

	call.mu.Lock()
	leg = call.legs[deliveryID]
	if leg == nil || leg.terminal || call.winner != "" || call.requested != nil || call.ctx.Err() != nil {
		call.mu.Unlock()
		s.releaseOperation()
		return
	}
	call.winner = deliveryID
	call.answerStarted = true
	call.answered = false
	call.answerErr = nil
	call.answerDone = make(chan struct{})
	call.acceptDone = make(chan struct{})
	leg.answering = true
	losers := make([]string, 0, len(call.legs)-1)
	for id, candidate := range call.legs {
		if id != deliveryID && !candidate.terminal {
			candidate.terminal = true
			losers = append(losers, id)
		}
	}
	call.mu.Unlock()
	for _, loser := range losers {
		s.sendCall(loser, s.eventFor(call, "cancelled", "answered on another delivery"))
	}
	s.launchOperation(func() {
		s.runInboundAccept(call, deliveryID, *event.SessionDesc, selection)
	})
}

func (s *Service) rejectInboundAccept(call *serviceCall, deliveryID, reason string, status int) {
	call.mu.Lock()
	leg := call.legs[deliveryID]
	if leg == nil || leg.terminal || call.winner != "" || call.requested != nil || call.ctx.Err() != nil {
		call.mu.Unlock()
		return
	}
	leg.terminal = true
	active := 0
	for _, candidate := range call.legs {
		if !candidate.terminal {
			active++
		}
	}
	call.mu.Unlock()
	if active == 0 && call.inDialog.Context().Err() == nil {
		s.requestTermination(call, "rejected", reason, status)
	}
	s.sendCall(deliveryID, s.eventFor(call, "rejected", reason))
}

func (s *Service) requestInboundFailure(call *serviceCall, reason string, status int) {
	// A remote CANCEL/BYE owns the terminal outcome. Do not turn the Answer or
	// media error caused by that dialog teardown into a misleading local failure.
	if call.inDialog.Context().Err() == nil {
		s.requestTermination(call, "failed", reason, status)
	}
}

func (s *Service) runInboundAccept(call *serviceCall, deliveryID string, offer webrtc.SessionDescription, selection negotiatedSelection) {
	defer close(call.acceptDone)
	if call.ctx.Err() != nil {
		call.mu.Lock()
		call.answerErr = context.Canceled
		close(call.answerDone)
		call.mu.Unlock()
		return
	}
	_, rtpSession, err := s.newInboundMedia(call.inDialog, call.runtime.MediaBindIP(), selection)
	if err != nil {
		call.mu.Lock()
		call.answerErr = err
		close(call.answerDone)
		call.mu.Unlock()
		s.requestInboundFailure(call, "SIP media negotiation failed", 488)
		return
	}
	browser, err := s.newBrowser(offer, selection, append([]string(nil), s.iceServers...), func(candidate webrtc.ICECandidateInit) {
		event := s.eventFor(call, "", "")
		event.RTCICECandidate = &candidate
		s.sendCall(deliveryID, event)
	}, func(track *webrtc.TrackRemote) {
		s.addBrowserTrack(call, track)
	})
	if err != nil {
		call.mu.Lock()
		call.answerErr = err
		close(call.answerDone)
		call.mu.Unlock()
		s.requestInboundFailure(call, "browser media negotiation failed", 488)
		return
	}
	call.mu.Lock()
	call.browser = browser
	leg := call.legs[deliveryID]
	var pending []webrtc.ICECandidateInit
	if leg != nil {
		pending = append(pending, leg.pending...)
		leg.pending = nil
	}
	call.mu.Unlock()
	for _, candidate := range pending {
		_ = browser.AddCandidate(candidate)
	}
	if call.ctx.Err() == nil {
		event := s.eventFor(call, "answering", "")
		answer := browser.AnswerDescription()
		event.SessionDesc = &answer
		s.sendCall(deliveryID, event)
	}

	answerErr := call.inDialog.Answer()
	answered := answerErr == nil || call.inDialog.Answered()
	call.mu.Lock()
	call.answerErr = answerErr
	call.answered = answered
	close(call.answerDone)
	cancelled := call.requested != nil || call.ctx.Err() != nil
	call.mu.Unlock()
	if answerErr != nil {
		s.requestInboundFailure(call, "SIP answer failed", statusTemporarilyUnavailable)
		return
	}
	if cancelled {
		return
	}
	call.mu.Lock()
	cancelled = call.requested != nil || call.ctx.Err() != nil
	if !cancelled {
		call.accepted = true
	}
	call.mu.Unlock()
	if cancelled {
		return
	}
	s.sendCall(deliveryID, s.eventFor(call, "accepted", ""))
	relayDialog := &inboundRelayDialog{inboundDialog: call.inDialog, rtp: rtpSession}
	relay, err := s.newRelay(relayDialog, browser, selection)
	if err != nil {
		s.requestInboundFailure(call, "SIP media relay failed", statusTemporarilyUnavailable)
		return
	}
	call.mu.Lock()
	call.relay = relay
	tracks := append([]*webrtc.TrackRemote(nil), call.tracks...)
	call.tracks = nil
	cancelled = call.requested != nil || call.ctx.Err() != nil
	call.mu.Unlock()
	for _, track := range tracks {
		relay.AddTrack(track)
	}
}

// handleInboundMessage validates and fans out one pager-mode MESSAGE. The only
// blocking operation is the bounded hub request needed to choose the SIP status.
func (s *Service) handleInboundMessage(credential Credential, request *sip.Request) (int, string, []sip.Header) {
	if len(request.Body()) > maxOutboundTextBytes {
		return sip.StatusRequestEntityTooLarge, "Request Entity Too Large", nil
	}
	if !validInboundContentType(request.ContentType(), "text/plain") {
		return sip.StatusUnsupportedMediaType, "Unsupported Media Type", []sip.Header{sip.NewHeader("Accept", "text/plain")}
	}
	body := request.Body()
	if !utf8.Valid(body) {
		return sip.StatusUnsupportedMediaType, "Unsupported Media Type", []sip.Header{sip.NewHeader("Accept", "text/plain")}
	}
	bridge := s.bridgeSnapshot()
	if bridge == nil {
		return statusTemporarilyUnavailable, "Temporarily Unavailable", nil
	}
	now := s.now()
	event := &ss.SIPMessageEvent{
		MessageId:    s.newUUID(),
		CredentialId: credential.ID,
		Target:       sipHeaderURI(request.From()),
		Text:         string(body),
		Direction:    "incoming",
		State:        "received",
		LocalUri:     credential.SIPURI,
		RemoteUri:    sipHeaderURI(request.From()),
		CreatedAt:    float64(now.UnixNano()) / 1e9,
	}
	ctx, cancel := hubContext(s.ctx)
	defer cancel()
	deliveries, err := bridge.FanoutSIP(ctx, ss.UserId(credential.OwnerID), nil, event)
	if err != nil || len(deliveries) == 0 {
		return statusTemporarilyUnavailable, "Temporarily Unavailable", nil
	}
	return sip.StatusOK, "OK", nil
}
