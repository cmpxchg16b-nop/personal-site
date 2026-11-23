package websocket_ss

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"personal-site/pkg/models/ss"
)

type recordedSIP struct {
	userID     ss.UserId
	deliveryID string
	call       *ss.SIPCallEvent
	message    *ss.SIPMessageEvent
}

type recordingSIPHandler struct {
	events       chan recordedSIP
	disconnected chan string
}

func newRecordingSIPHandler() *recordingSIPHandler {
	return &recordingSIPHandler{
		events:       make(chan recordedSIP, 32),
		disconnected: make(chan string, 32),
	}
}

func (h *recordingSIPHandler) HandleSIP(userID ss.UserId, deliveryID string, call *ss.SIPCallEvent, message *ss.SIPMessageEvent) {
	h.events <- recordedSIP{userID: userID, deliveryID: deliveryID, call: call, message: message}
}

func (h *recordingSIPHandler) SIPDisconnected(deliveryID string) {
	h.disconnected <- deliveryID
}

func (h *recordingSIPHandler) nextEvent(t *testing.T) recordedSIP {
	t.Helper()
	select {
	case event := <-h.events:
		return event
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a SIP callback")
		return recordedSIP{}
	}
}

func (h *recordingSIPHandler) nextDisconnect(t *testing.T) string {
	t.Helper()
	select {
	case deliveryID := <-h.disconnected:
		return deliveryID
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a SIP disconnect")
		return ""
	}
}

func startSIPTestHub(t *testing.T, handler SIPHandler) *WebSocketSSHandler {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h := &WebSocketSSHandler{
		ctx:     ctx,
		inMsg:   make(chan *ss.SignallingEvent, 8),
		outMsg:  make(chan *ss.SignallingEvent, 8),
		ingress: make(chan hubNote, ingressBufferSize),
		hubDone: make(chan struct{}),
	}
	h.SetSIPHandler(handler)
	go h.hub()
	t.Cleanup(func() {
		cancel()
		<-h.hubDone
	})
	return h
}

func registerSIPTestConn(t *testing.T, h *WebSocketSSHandler, userID, sessionID, deliveryID string) *wsConn {
	t.Helper()
	c := newForwardTestConn(camKey{userId: ss.UserId(userID), sessionId: ss.UserSessionId(sessionID)})
	c.deliveryID = deliveryID
	if !h.register(c) {
		t.Fatal("register test connection")
	}
	return c
}

func requireDeliveryIDs(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("delivery IDs = %v, want %v", got, want)
	}
	remaining := make(map[string]struct{}, len(want))
	for _, deliveryID := range want {
		remaining[deliveryID] = struct{}{}
	}
	for _, deliveryID := range got {
		if _, ok := remaining[deliveryID]; !ok {
			t.Fatalf("delivery IDs = %v, want %v (duplicate or unexpected %q)", got, want, deliveryID)
		}
		delete(remaining, deliveryID)
	}
}

func requireSIPEnvelope(t *testing.T, ev *ss.SignallingEvent, key camKey) {
	t.Helper()
	if ev.From != (ss.EPAddr{ServiceId: ss.WellKnownSvcIdSIP}) {
		t.Fatalf("SIP From = %+v, want only the SIP service", ev.From)
	}
	wantTo := ss.EPAddr{UserId: key.userId, UserSessionId: key.sessionId}
	if ev.To != wantTo {
		t.Fatalf("SIP To = %+v, want %+v", ev.To, wantTo)
	}
	if ev.MsgId == "" {
		t.Fatal("SIP event has an empty msgId")
	}
	if ev.InReplyTo != nil || ev.C2SEv != nil || ev.C2CEv != nil || ev.S2CEv == nil {
		t.Fatalf("SIP event has an invalid envelope: %+v", ev)
	}
}

func requireQueuedSIPEvent(t *testing.T, c *wsConn) *ss.SignallingEvent {
	t.Helper()
	select {
	case ev, ok := <-c.queue:
		if !ok {
			t.Fatal("connection queue closed before the SIP event")
		}
		requireSIPEnvelope(t, ev, c.camKey)
		return ev
	default:
		t.Fatal("expected a queued SIP event")
		return nil
	}
}

func TestFanoutSIPAcrossSessionsAndDuplicateConnections(t *testing.T) {
	h := startSIPTestHub(t, newRecordingSIPHandler())
	call := &ss.SIPCallEvent{CallId: "call-1", Direction: "incoming", State: "ringing", Kind: "audio"}

	empty, err := h.FanoutSIP(context.Background(), "u-alice", call, nil)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("FanoutSIP without connections = %v, %v; want non-nil empty success", empty, err)
	}

	alice1 := registerSIPTestConn(t, h, "u-alice", "s-one", "delivery-alice-1")
	alice2 := registerSIPTestConn(t, h, "u-alice", "s-one", "delivery-alice-2")
	alice3 := registerSIPTestConn(t, h, "u-alice", "s-two", "delivery-alice-3")
	bob := registerSIPTestConn(t, h, "u-bob", "s-bob", "delivery-bob")

	deliveries, err := h.FanoutSIP(context.Background(), "u-alice", call, nil)
	if err != nil {
		t.Fatalf("FanoutSIP: %v", err)
	}
	requireDeliveryIDs(t, deliveries, "delivery-alice-1", "delivery-alice-2", "delivery-alice-3")

	connections := []*wsConn{alice1, alice2, alice3}
	events := make([]*ss.SignallingEvent, 0, len(connections))
	msgIDs := make(map[ss.MsgId]struct{})
	for _, c := range connections {
		ev := requireQueuedSIPEvent(t, c)
		if ev.S2CEv.SIPMessage != nil || ev.S2CEv.SIPCall == nil {
			t.Fatalf("SIP payload = %+v, want only a call", ev.S2CEv)
		}
		if ev.S2CEv.SIPCall == call || !reflect.DeepEqual(*ev.S2CEv.SIPCall, *call) {
			t.Fatalf("SIP call = %+v, want an independent copy of %+v", ev.S2CEv.SIPCall, call)
		}
		if _, duplicate := msgIDs[ev.MsgId]; duplicate {
			t.Fatalf("two fan-out deliveries share msgId %q", ev.MsgId)
		}
		msgIDs[ev.MsgId] = struct{}{}
		events = append(events, ev)
	}
	for i := range events {
		for j := i + 1; j < len(events); j++ {
			if events[i] == events[j] || events[i].S2CEv == events[j].S2CEv || events[i].S2CEv.SIPCall == events[j].S2CEv.SIPCall {
				t.Fatalf("deliveries %d and %d share an event, body, or call pointer", i, j)
			}
		}
	}
	requireNoForwardedEvent(t, bob)
}

func TestSendSIPTargetsExactCurrentDelivery(t *testing.T) {
	recorder := newRecordingSIPHandler()
	h := startSIPTestHub(t, recorder)
	first := registerSIPTestConn(t, h, "u-alice", "s-one", "delivery-first")
	second := registerSIPTestConn(t, h, "u-alice", "s-one", "delivery-second")
	message := &ss.SIPMessageEvent{MessageId: "message-1", State: "received", Text: "hello"}

	delivered, err := h.SendSIP(context.Background(), "delivery-second", nil, message)
	if err != nil || !delivered {
		t.Fatalf("SendSIP(second) = %v, %v; want true, nil", delivered, err)
	}
	ev := requireQueuedSIPEvent(t, second)
	if ev.S2CEv.SIPCall != nil || ev.S2CEv.SIPMessage == nil || *ev.S2CEv.SIPMessage != *message {
		t.Fatalf("targeted payload = %+v, want %+v", ev.S2CEv, message)
	}
	if ev.S2CEv.SIPMessage == message {
		t.Fatal("targeted delivery shares the caller's message pointer")
	}
	requireNoForwardedEvent(t, first)

	delivered, err = h.SendSIP(context.Background(), "delivery-missing", nil, message)
	if err != nil || delivered {
		t.Fatalf("SendSIP(missing) = %v, %v; want false, nil", delivered, err)
	}
	if !h.note(hubNote{kind: noteClosed, conn: second}) {
		t.Fatal("post close note")
	}
	if got := recorder.nextDisconnect(t); got != "delivery-second" {
		t.Fatalf("disconnect = %q, want delivery-second", got)
	}

	replacement := registerSIPTestConn(t, h, "u-alice", "s-one", "delivery-replacement")
	delivered, err = h.SendSIP(context.Background(), "delivery-second", nil, message)
	if err != nil || delivered {
		t.Fatalf("SendSIP(stale after replacement) = %v, %v; want false, nil", delivered, err)
	}
	delivered, err = h.SendSIP(context.Background(), "delivery-replacement", nil, message)
	if err != nil || !delivered {
		t.Fatalf("SendSIP(replacement) = %v, %v; want true, nil", delivered, err)
	}
	requireQueuedSIPEvent(t, replacement)
	requireNoForwardedEvent(t, first)
}

func TestSIPQueueOverflowDropsOnlySlowSocket(t *testing.T) {
	recorder := newRecordingSIPHandler()
	h := startSIPTestHub(t, recorder)
	slow := registerSIPTestConn(t, h, "u-alice", "s-one", "delivery-slow")
	fast := registerSIPTestConn(t, h, "u-alice", "s-two", "delivery-fast")
	other := registerSIPTestConn(t, h, "u-bob", "s-bob", "delivery-other")
	slow.queue <- &ss.SignallingEvent{MsgId: "already-full"}

	deliveries, err := h.FanoutSIP(context.Background(), "u-alice", &ss.SIPCallEvent{CallId: "call-1"}, nil)
	if err != nil {
		t.Fatalf("FanoutSIP: %v", err)
	}
	requireDeliveryIDs(t, deliveries, "delivery-fast")
	requireQueuedSIPEvent(t, fast)
	requireNoForwardedEvent(t, other)
	if got := recorder.nextDisconnect(t); got != "delivery-slow" {
		t.Fatalf("disconnect = %q, want delivery-slow", got)
	}
	if _, ok := <-slow.queue; !ok {
		t.Fatal("slow queue closed before its pre-existing event")
	}
	if _, ok := <-slow.queue; ok {
		t.Fatal("slow queue remains open after overflow")
	}

	fast.queue <- &ss.SignallingEvent{MsgId: "targeted-full"}
	delivered, err := h.SendSIP(context.Background(), "delivery-fast", &ss.SIPCallEvent{CallId: "call-2"}, nil)
	if err != nil || delivered {
		t.Fatalf("SendSIP(full) = %v, %v; want false, nil", delivered, err)
	}
	if got := recorder.nextDisconnect(t); got != "delivery-fast" {
		t.Fatalf("disconnect = %q, want delivery-fast", got)
	}

	// The other user's socket survives both slow-consumer removals.
	delivered, err = h.SendSIP(context.Background(), "delivery-other", &ss.SIPCallEvent{CallId: "call-3"}, nil)
	if err != nil || !delivered {
		t.Fatalf("SendSIP(other) = %v, %v; want true, nil", delivered, err)
	}
	requireQueuedSIPEvent(t, other)
}

func TestDroppedConnectionIgnoresQueuedSIPMessage(t *testing.T) {
	recorder := newRecordingSIPHandler()
	h := startSIPTestHub(t, recorder)
	slow := registerSIPTestConn(t, h, "u-alice", "s-one", "delivery-slow")
	fast := registerSIPTestConn(t, h, "u-alice", "s-one", "delivery-fast")
	slow.queue <- &ss.SignallingEvent{MsgId: "already-full"}

	delivered, err := h.SendSIP(context.Background(), slow.deliveryID, &ss.SIPCallEvent{CallId: "overflow"}, nil)
	if err != nil || delivered {
		t.Fatalf("SendSIP(slow) = %v, %v; want false, nil", delivered, err)
	}
	if got := recorder.nextDisconnect(t); got != slow.deliveryID {
		t.Fatalf("disconnect = %q, want %q", got, slow.deliveryID)
	}

	stale := &ss.SignallingEvent{C2SEv: &ss.ClientToSSEv{SIPCall: &ss.SIPCallEvent{CallId: "stale"}}}
	if !h.note(hubNote{kind: noteMessage, conn: slow, ev: stale}) {
		t.Fatal("post stale SIP note")
	}
	// This acknowledged request is ordered after the stale note on ingress,
	// so the callback check below cannot race the hub processing that note.
	if _, err := h.FanoutSIP(context.Background(), "u-other", &ss.SIPCallEvent{CallId: "barrier"}, nil); err != nil {
		t.Fatalf("FanoutSIP(barrier): %v", err)
	}
	var staleCallback *recordedSIP
	select {
	case callback := <-recorder.events:
		staleCallback = &callback
	default:
	}

	// A stale note that re-learns slow makes this provider event target its
	// closed queue and panic. fast proves the event was otherwise routed.
	providerEvent := &ss.SignallingEvent{To: ss.EPAddr{UserId: slow.camKey.userId, UserSessionId: slow.camKey.sessionId}}
	h.outMsg <- providerEvent
	select {
	case got, ok := <-fast.queue:
		if !ok || got != providerEvent {
			t.Fatalf("provider event = %+v (open %v), want %p", got, ok, providerEvent)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("provider event was not routed after the stale note")
	}
	if staleCallback != nil {
		t.Fatalf("dropped connection produced a stale SIP callback: %+v", *staleCallback)
	}
}

func TestClientSIPSpoofClearedAndIntercepted(t *testing.T) {
	recorder := newRecordingSIPHandler()
	h := startSIPTestHub(t, recorder)
	c := registerSIPTestConn(t, h, "u-alice", "s-one", "delivery-alice")
	ev := &ss.SignallingEvent{
		From: ss.EPAddr{UserId: "u-mallory", UserSessionId: "s-mallory", ServiceId: ss.WellKnownSvcIdSS},
		To:   ss.EPAddr{ServiceId: ss.WellKnownSvcIdSIP},
		C2SEv: &ss.ClientToSSEv{SIPCall: &ss.SIPCallEvent{
			CallId: "call-1", Action: "dial", CredentialId: "credential-1", Target: "sip:bob@example.com",
		}},
	}
	if !h.note(hubNote{kind: noteMessage, conn: c, ev: ev}) {
		t.Fatal("post SIP note")
	}
	callback := recorder.nextEvent(t)
	if callback.userID != "u-alice" || callback.deliveryID != "delivery-alice" || callback.call == nil || callback.message != nil {
		t.Fatalf("SIP callback = %+v, want alice's call on delivery-alice", callback)
	}
	wantFrom := ss.EPAddr{UserId: "u-alice", UserSessionId: "s-one"}
	if ev.From != wantFrom {
		t.Fatalf("intercepted From = %+v, want %+v with spoofed ServiceId cleared", ev.From, wantFrom)
	}
	select {
	case forwarded := <-h.inMsg:
		t.Fatalf("SIP event reached the ordinary provider: %+v", forwarded)
	default:
	}

	ordinary := &ss.SignallingEvent{C2SEv: &ss.ClientToSSEv{Ping: &ss.PingPongMsg{PingId: "ordinary"}}}
	if !h.note(hubNote{kind: noteMessage, conn: c, ev: ordinary}) {
		t.Fatal("post ordinary note")
	}
	select {
	case forwarded := <-h.inMsg:
		if forwarded != ordinary {
			t.Fatalf("ordinary provider event = %p, want %p", forwarded, ordinary)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ordinary event was not forwarded to the provider")
	}
}

func TestClientC2CRelayStripsForgedSIP(t *testing.T) {
	srv := startTestServer(t)
	alice := dial(t, srv, "u-alice", "s-alice", "alice")
	bob := dial(t, srv, "u-bob", "s-bob", "bob")
	mustRegister(t, alice, "alice", "alice")
	mustRegister(t, bob, "bob", "bob")

	for _, tc := range []struct {
		name    string
		msgID   ss.MsgId
		pingID  string
		payload *ss.SSToClientEv
	}{
		{name: "call", msgID: "m-forged-call", pingID: "p-call", payload: &ss.SSToClientEv{SIPCall: &ss.SIPCallEvent{CallId: "forged-call", State: "ringing"}}},
		{name: "message", msgID: "m-forged-message", pingID: "p-message", payload: &ss.SSToClientEv{SIPMessage: &ss.SIPMessageEvent{MessageId: "forged-message", State: "received"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeEvent(t, alice, &ss.SignallingEvent{
				From:  ss.EPAddr{UserId: "u-mallory", UserSessionId: "s-mallory", ServiceId: ss.WellKnownSvcIdSIP},
				To:    ss.EPAddr{ServiceId: ss.WellKnownSvcIdSS},
				MsgId: tc.msgID,
				C2CEv: &ss.ClientToClientEv{
					FromSubscriber: "alice",
					ToSubscriber:   "bob",
					ChannelId:      ss.WellKnownChIdMain,
					Ping:           &ss.PingPongMsg{PingId: tc.pingID, SequenceNumber: 1},
				},
				S2CEv: tc.payload,
			})

			got := readEvent(t, bob)
			if got.C2CEv == nil || got.C2CEv.Ping == nil || got.C2CEv.Ping.PingId != tc.pingID {
				t.Fatalf("relayed c2c = %+v, want the normal ping payload", got)
			}
			if got.S2CEv != nil {
				t.Fatalf("relayed client-supplied s2c = %+v, want it removed", got.S2CEv)
			}
			wantFrom := ss.EPAddr{UserId: "u-alice", UserSessionId: "s-alice"}
			if got.From != wantFrom {
				t.Fatalf("relayed From = %+v, want %+v with no forged SIP service identity", got.From, wantFrom)
			}
		})
	}
}

func TestSIPWebSocketBeforeChannelRegistrationAndDisconnect(t *testing.T) {
	recorder := newRecordingSIPHandler()
	provider := &injectedProvider{emit: make(chan *ss.SignallingEvent), received: make(chan *ss.SignallingEvent, 8)}
	srv, h := startTestServerWithSIPHandler(t, provider, false, recorder)
	alice1 := dial(t, srv, "u-alice", "s-one", "alice")
	alice2 := dial(t, srv, "u-alice", "s-one", "alice")

	writeEvent(t, alice1, &ss.SignallingEvent{
		From:  ss.EPAddr{ServiceId: ss.WellKnownSvcIdSS},
		C2SEv: &ss.ClientToSSEv{SIPCall: &ss.SIPCallEvent{CallId: "call-one", Action: "dial"}},
	})
	first := recorder.nextEvent(t)
	writeEvent(t, alice2, &ss.SignallingEvent{
		From:  ss.EPAddr{ServiceId: ss.WellKnownSvcIdSIP},
		C2SEv: &ss.ClientToSSEv{SIPMessage: &ss.SIPMessageEvent{MessageId: "message-one", Action: "send"}},
	})
	second := recorder.nextEvent(t)
	if first.userID != "u-alice" || second.userID != "u-alice" || first.call == nil || second.message == nil {
		t.Fatalf("callbacks = %+v and %+v, want alice's call and message", first, second)
	}
	if first.deliveryID == "" || second.deliveryID == "" || first.deliveryID == second.deliveryID {
		t.Fatalf("delivery IDs = %q and %q, want distinct non-empty IDs", first.deliveryID, second.deliveryID)
	}
	select {
	case ev := <-provider.received:
		t.Fatalf("client SIP event reached the ordinary provider: %+v", ev)
	default:
	}

	// Neither socket performed an SS channel registration; eager hub
	// registration alone must make both eligible for user fan-out.
	deliveries, err := h.FanoutSIP(context.Background(), "u-alice", nil, &ss.SIPMessageEvent{MessageId: "server-message", State: "received"})
	if err != nil {
		t.Fatalf("FanoutSIP: %v", err)
	}
	requireDeliveryIDs(t, deliveries, first.deliveryID, second.deliveryID)
	for _, c := range []*websocket.Conn{alice1, alice2} {
		ev := readEvent(t, c)
		requireSIPEnvelope(t, ev, camKey{userId: "u-alice", sessionId: "s-one"})
		if ev.S2CEv.SIPMessage == nil || ev.S2CEv.SIPMessage.MessageId != "server-message" {
			t.Fatalf("WebSocket SIP event = %+v, want server-message", ev)
		}
	}

	alice1.Close()
	if got := recorder.nextDisconnect(t); got != first.deliveryID {
		t.Fatalf("disconnect = %q, want %q", got, first.deliveryID)
	}
	delivered, err := h.SendSIP(context.Background(), first.deliveryID, nil, &ss.SIPMessageEvent{MessageId: "stale"})
	if err != nil || delivered {
		t.Fatalf("SendSIP(disconnected) = %v, %v; want false, nil", delivered, err)
	}
	delivered, err = h.SendSIP(context.Background(), second.deliveryID, nil, &ss.SIPMessageEvent{MessageId: "targeted"})
	if err != nil || !delivered {
		t.Fatalf("SendSIP(surviving socket) = %v, %v; want true, nil", delivered, err)
	}
	if ev := readEvent(t, alice2); ev.S2CEv == nil || ev.S2CEv.SIPMessage == nil || ev.S2CEv.SIPMessage.MessageId != "targeted" {
		t.Fatalf("targeted WebSocket event = %+v", ev)
	}
}

func TestSIPDeliveryValidationAndCancellation(t *testing.T) {
	h := startSIPTestHub(t, nil)
	call := &ss.SIPCallEvent{CallId: "call-1"}
	message := &ss.SIPMessageEvent{MessageId: "message-1"}
	if _, err := h.FanoutSIP(context.Background(), "u-alice", nil, nil); !errors.Is(err, errSIPPayload) {
		t.Fatalf("FanoutSIP(nil, nil) error = %v, want errSIPPayload", err)
	}
	if _, err := h.FanoutSIP(context.Background(), "u-alice", call, message); !errors.Is(err, errSIPPayload) {
		t.Fatalf("FanoutSIP(call, message) error = %v, want errSIPPayload", err)
	}
	if _, err := h.SendSIP(context.Background(), "delivery", nil, nil); !errors.Is(err, errSIPPayload) {
		t.Fatalf("SendSIP(nil, nil) error = %v, want errSIPPayload", err)
	}
	if _, err := h.SendSIP(context.Background(), "delivery", call, message); !errors.Is(err, errSIPPayload) {
		t.Fatalf("SendSIP(call, message) error = %v, want errSIPPayload", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.FanoutSIP(ctx, "u-alice", call, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("FanoutSIP(canceled) error = %v, want context.Canceled", err)
	}
	if _, err := h.SendSIP(ctx, "delivery", call, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("SendSIP(canceled) error = %v, want context.Canceled", err)
	}
}
