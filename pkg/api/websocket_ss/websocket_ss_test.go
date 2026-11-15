package websocket_ss

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"personal-site/pkg/models/ss"
	pkgsession "personal-site/pkg/session"
	pkgutils "personal-site/pkg/utils"
)

// Test-only identity headers. The handler resolves identity from the
// session middleware, which is awkward to drive in-process, so the test
// server below maps these headers onto the context values the session
// middleware would produce from a JWT, keeping the dial() call sites
// unchanged.
const (
	testUserIdHeader        = "X-Test-UserId"
	testUserSessionIdHeader = "X-Test-UserSessionId"
	testUsernameHeader      = "X-Test-Username"
)

func startTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	return startTestServerWithProvider(t, ss.NewSimpleOnMemorySSProvider(), false)
}

func startTestServerWithProvider(t *testing.T, provider ss.SignallingServiceProvider, unknownMessageFlood bool) *httptest.Server {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	sm := pkgsession.NewOnMemorySessionManager()
	h := NewWebSocketSSHandler(ctx, provider, sm)
	h.UnknownMessageFlood = unknownMessageFlood
	// The real session middleware builds the request-scoped session object
	// from context values, exactly as in production.
	sessionized := pkgsession.WithSessionId(h, sm)
	// Adapter: seed the request context from the test-only identity
	// headers before the session middleware runs.
	adapter := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if v := r.Header.Get(testUserIdHeader); v != "" {
			ctx = context.WithValue(ctx, pkgutils.CtxKeySubjectId, v)
		}
		if v := r.Header.Get(testUserSessionIdHeader); v != "" {
			ctx = context.WithValue(ctx, pkgutils.CtxKeySessionId, v)
		}
		if v := r.Header.Get(testUsernameHeader); v != "" {
			ctx = context.WithValue(ctx, pkgutils.CtxKeyUsername, v)
		}
		sessionized.ServeHTTP(w, r.WithContext(ctx))
	})
	srv := httptest.NewServer(adapter)
	t.Cleanup(func() {
		srv.Close()
		cancel()
	})
	return srv
}

// injectedProvider lets a test emit provider events without requiring any
// client registration or inbound traffic. Client events are drained.
type injectedProvider struct {
	emit chan *ss.SignallingEvent
}

func (p *injectedProvider) Run(ctx context.Context, inMsg <-chan *ss.SignallingEvent, outMsg chan<- *ss.SignallingEvent) {
	defer close(outMsg)
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-inMsg:
			if !ok {
				return
			}
		case ev := <-p.emit:
			select {
			case outMsg <- ev:
			case <-ctx.Done():
				return
			}
		}
	}
}

// dialRaw opens a websocket without consuming its initial hello.
// Empty userId/sessionId omit the respective identity header.
func dialRaw(t *testing.T, srv *httptest.Server, userId, sessionId, username string) *websocket.Conn {
	t.Helper()
	header := http.Header{}
	if userId != "" {
		header.Set(testUserIdHeader, userId)
	}
	if sessionId != "" {
		header.Set(testUserSessionIdHeader, sessionId)
	}
	if username != "" {
		header.Set(testUsernameHeader, username)
	}
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	c, _, err := websocket.DefaultDialer.Dial(url, header)
	if err != nil {
		t.Fatalf("Dial(%s, %s): %v", userId, sessionId, err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// dial opens a websocket and consumes its hello, keeping every business-event
// test below independent of the connection-local control frame.
func dial(t *testing.T, srv *httptest.Server, userId, sessionId, username string) *websocket.Conn {
	t.Helper()
	c := dialRaw(t, srv, userId, sessionId, username)
	readHello(t, c, userId, sessionId)
	return c
}

func readHello(t *testing.T, c *websocket.Conn, userId, sessionId string) *ss.SignallingEvent {
	t.Helper()
	hello := readEvent(t, c)
	wantEndpoint := ss.EPAddr{UserId: ss.UserId(userId), UserSessionId: ss.UserSessionId(sessionId)}
	if hello.From != (ss.EPAddr{ServiceId: ss.WellKnownSvcIdSS}) {
		t.Fatalf("hello.From = %+v, want the SS service", hello.From)
	}
	if hello.To != wantEndpoint {
		t.Fatalf("hello.To = %+v, want %+v", hello.To, wantEndpoint)
	}
	if hello.MsgId == "" {
		t.Fatal("hello has an empty msgId")
	}
	if hello.InReplyTo != nil || hello.C2SEv != nil || hello.C2CEv != nil {
		t.Fatalf("hello carries unexpected request/relay fields: %+v", hello)
	}
	if hello.S2CEv == nil || hello.S2CEv.Endpoint == nil || *hello.S2CEv.Endpoint != wantEndpoint {
		t.Fatalf("hello endpoint = %+v, want %+v", hello.S2CEv, wantEndpoint)
	}
	return hello
}

func writeEvent(t *testing.T, c *websocket.Conn, ev *ss.SignallingEvent) {
	t.Helper()
	data, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := c.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
}

func readEvent(t *testing.T, c *websocket.Conn) *ss.SignallingEvent {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	var ev ss.SignallingEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatalf("Unmarshal(%q): %v", data, err)
	}
	return &ev
}

// expectNoEvent asserts nothing arrives within a short window. Note that
// a read timeout corrupts a gorilla connection, so this must be the last
// operation on c.
func expectNoEvent(t *testing.T, c *websocket.Conn) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	if _, _, err := c.ReadMessage(); err == nil {
		t.Fatal("unexpected event")
	}
}

func registerEvent(msgId string, sub ss.SubscriberId, username string) *ss.SignallingEvent {
	return &ss.SignallingEvent{
		MsgId: ss.MsgId(msgId),
		C2SEv: &ss.ClientToSSEv{Register: &ss.ClientToSSRegEv{
			SubscriberId: sub,
			ChannelId:    ss.WellKnownChIdMain,
			Username:     username,
		}},
	}
}

// mustRegister registers sub on c and consumes the successful ack.
func mustRegister(t *testing.T, c *websocket.Conn, sub ss.SubscriberId, username string) *ss.SignallingEvent {
	t.Helper()
	writeEvent(t, c, registerEvent("m-reg-"+string(sub), sub, username))
	ack := readEvent(t, c)
	if ack.S2CEv == nil || ack.S2CEv.Err != nil {
		t.Fatalf("Register(%q) ack: %+v", sub, ack)
	}
	return ack
}

func TestCheckOriginAllowing(t *testing.T) {
	check := CheckOriginAllowing([]string{"http://localhost:3000"})
	for _, tc := range []struct {
		name   string
		origin string
		host   string
		want   bool
	}{
		{"no Origin header", "", "localhost:8080", true},
		{"same origin", "http://localhost:8080", "localhost:8080", true},
		{"allowed cross-origin", "http://localhost:3000", "localhost:8080", true},
		{"unlisted cross-origin", "http://evil.example", "localhost:8080", false},
		{"malformed origin", "://nope", "localhost:8080", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/ss/ws", nil)
			r.Host = tc.host
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if got := check(r); got != tc.want {
				t.Errorf("CheckOrigin(origin %q, host %q) = %v, want %v", tc.origin, tc.host, got, tc.want)
			}
		})
	}
}

func TestHelloIsFirstFrame(t *testing.T) {
	srv := startTestServer(t)
	first := dialRaw(t, srv, "u-alice", "s-alice", "alice")
	hello := readHello(t, first, "u-alice", "s-alice")

	second := dialRaw(t, srv, "u-bob", "s-bob", "bob")
	otherHello := readHello(t, second, "u-bob", "s-bob")
	if otherHello.MsgId == hello.MsgId {
		t.Fatalf("two hello events share msgId %q", hello.MsgId)
	}
}

func TestHelloMeansEagerCAMLearningCompleted(t *testing.T) {
	provider := &injectedProvider{emit: make(chan *ss.SignallingEvent)}
	srv := startTestServerWithProvider(t, provider, false)
	// dial returns only after the hello arrives. This socket sends no message
	// and never registers, so delivery proves registration-time CAM learning.
	silent := dial(t, srv, "u-silent", "s-silent", "silent")

	provider.emit <- &ss.SignallingEvent{
		From:  ss.EPAddr{ServiceId: ss.WellKnownSvcIdSS},
		To:    ss.EPAddr{UserId: "u-silent", UserSessionId: "s-silent"},
		MsgId: "provider-to-silent",
		S2CEv: &ss.SSToClientEv{Pong: &ss.PingPongMsg{PingId: "eager-cam"}},
	}
	got := readEvent(t, silent)
	if got.MsgId != "provider-to-silent" || got.S2CEv == nil || got.S2CEv.Pong == nil || got.S2CEv.Pong.PingId != "eager-cam" {
		t.Fatalf("silent connection got %+v, want the injected provider event", got)
	}
}

func TestMissingIdentityHeadersRejected(t *testing.T) {
	srv := startTestServer(t)
	url := "ws" + strings.TrimPrefix(srv.URL, "http")

	_, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if !errors.Is(err, websocket.ErrBadHandshake) {
		t.Fatalf("Dial: got %v, want ErrBadHandshake", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestRegisterAckPopulatedFromHeaders(t *testing.T) {
	srv := startTestServer(t)
	alice := dial(t, srv, "u-alice", "s-alice", "alice")

	// The client leaves From empty; the handler populates it.
	ack := mustRegister(t, alice, "alice", "alice")

	wantTo := ss.EPAddr{UserId: "u-alice", UserSessionId: "s-alice"}
	if ack.To != wantTo {
		t.Fatalf("ack.To = %+v, want %+v", ack.To, wantTo)
	}
	if ack.From.ServiceId != ss.WellKnownSvcIdSS {
		t.Fatalf("ack.From.ServiceId = %q, want %q", ack.From.ServiceId, ss.WellKnownSvcIdSS)
	}
	if ack.InReplyTo == nil || *ack.InReplyTo != "m-reg-alice" {
		t.Fatalf("ack.InReplyTo = %v, want %q", ack.InReplyTo, "m-reg-alice")
	}
}

func TestRelayUnicastNoFlood(t *testing.T) {
	srv := startTestServer(t)
	alice := dial(t, srv, "u-alice", "s-alice", "alice")
	bob := dial(t, srv, "u-bob", "s-bob", "bob")
	carol := dial(t, srv, "u-carol", "s-carol", "carol")
	mustRegister(t, alice, "alice", "alice")
	mustRegister(t, bob, "bob", "bob")
	mustRegister(t, carol, "carol", "carol")

	ping := func(to ss.SubscriberId, msgId string) *ss.SignallingEvent {
		return &ss.SignallingEvent{
			MsgId: ss.MsgId(msgId),
			// Addressed to the SS, as the prototype prescribes; the
			// provider rewrites To to the resolved subscriber address.
			To: ss.EPAddr{ServiceId: ss.WellKnownSvcIdSS},
			C2CEv: &ss.ClientToClientEv{
				FromSubscriber: "alice",
				ToSubscriber:   to,
				ChannelId:      ss.WellKnownChIdMain,
				Ping:           &ss.PingPongMsg{PingId: "p1", SequenceNumber: 1},
			},
		}
	}

	// Relay to an unregistered subscriber: the error reply is unicasted
	// back to alice only.
	writeEvent(t, alice, ping("ghost", "m-ghost"))
	reply := readEvent(t, alice)
	if reply.S2CEv == nil || reply.S2CEv.Err == nil {
		t.Fatalf("reply = %+v, want an s2CEv error", reply)
	}
	if reply.S2CEv.Err.ErrorCode != ss.ErrorCodeSubscriberNotFound {
		t.Fatalf("error code = %d, want %d", reply.S2CEv.Err.ErrorCode, ss.ErrorCodeSubscriberNotFound)
	}
	if reply.To.UserId != "u-alice" || reply.To.UserSessionId != "s-alice" {
		t.Fatalf("reply.To = %+v, want alice's address", reply.To)
	}

	// Relay to bob: delivered to bob only, with To rewritten to bob's
	// (user id, user session id) by the provider.
	writeEvent(t, alice, ping("bob", "m-ping"))
	got := readEvent(t, bob)
	if got.C2CEv == nil || got.C2CEv.Ping == nil || got.C2CEv.Ping.PingId != "p1" {
		t.Fatalf("bob got %+v, want the relayed ping", got)
	}
	if got.To.UserId != "u-bob" || got.To.UserSessionId != "s-bob" {
		t.Fatalf("relayed To = %+v, want bob's address", got.To)
	}

	// Nobody else saw either event. (Negative checks last: a read
	// timeout corrupts a gorilla connection.)
	expectNoEvent(t, alice)
	expectNoEvent(t, bob)
	expectNoEvent(t, carol)
}

// TestForgedFromOverridden checks that a client-supplied From is never
// trusted: the handler overrides it with the connection's session
// identity before the event reaches anyone.
func TestForgedFromOverridden(t *testing.T) {
	srv := startTestServer(t)
	alice := dial(t, srv, "u-alice", "s-alice", "alice")
	bob := dial(t, srv, "u-bob", "s-bob", "bob")
	mustRegister(t, alice, "alice", "alice")
	mustRegister(t, bob, "bob", "bob")

	// alice sends a ping to bob carrying a forged From.
	writeEvent(t, alice, &ss.SignallingEvent{
		From:  ss.EPAddr{UserId: "u-mallory", UserSessionId: "s-mallory"},
		To:    ss.EPAddr{ServiceId: ss.WellKnownSvcIdSS},
		MsgId: "m-forged",
		C2CEv: &ss.ClientToClientEv{
			FromSubscriber: "alice",
			ToSubscriber:   "bob",
			ChannelId:      ss.WellKnownChIdMain,
			Ping:           &ss.PingPongMsg{PingId: "p-forged", SequenceNumber: 1},
		},
	})

	got := readEvent(t, bob)
	if got.From.UserId != "u-alice" || got.From.UserSessionId != "s-alice" {
		t.Fatalf("relayed From = %+v, want alice's session identity, not the forged one", got.From)
	}
}

// TestRegisterUsernameOverriddenFromSession checks that a client-supplied
// registration username is never trusted: the handler overrides it with
// the session's username, exactly like the From fields. A forged username
// neither lands on the registration nor triggers UsernameTaken against
// the impersonated name.
func TestRegisterUsernameOverriddenFromSession(t *testing.T) {
	srv := startTestServer(t)
	alice := dial(t, srv, "u-alice", "s-alice", "alice-real")
	bob := dial(t, srv, "u-bob", "s-bob", "bob-real")

	// alice registers asking for a forged display name; bob asks for
	// alice's real one. Both registrations succeed — the wire value is
	// discarded — and the profiles carry the session usernames.
	mustRegister(t, alice, "alice", "mallory")
	mustRegister(t, bob, "bob", "alice-real")

	query := func(c *websocket.Conn, msgId string, sub ss.SubscriberId) *ss.SignallingEvent {
		t.Helper()
		writeEvent(t, c, &ss.SignallingEvent{
			MsgId: ss.MsgId(msgId),
			C2SEv: &ss.ClientToSSEv{UserProfileQuery: &ss.ClientToSSUserProfileQuery{
				SubscriberId: sub,
				ChannelId:    ss.WellKnownChIdMain,
			}},
		})
		reply := readEvent(t, c)
		if reply.S2CEv == nil || reply.S2CEv.Err != nil || reply.S2CEv.Profile == nil {
			t.Fatalf("profile reply = %+v, want a profile and no error", reply)
		}
		return reply
	}
	if p := query(bob, "m-q-alice", "alice").S2CEv.Profile; p.Username != "alice-real" {
		t.Errorf("alice's profile username = %q, want her session username %q", p.Username, "alice-real")
	}
	if p := query(alice, "m-q-bob", "bob").S2CEv.Profile; p.Username != "bob-real" {
		t.Errorf("bob's profile username = %q, want his session username %q", p.Username, "bob-real")
	}
}

func newForwardTestConn(key camKey) *wsConn {
	return &wsConn{queue: make(chan *ss.SignallingEvent, 1), camKey: key}
}

func requireForwardedEvent(t *testing.T, c *wsConn, want *ss.SignallingEvent) {
	t.Helper()
	select {
	case got, ok := <-c.queue:
		if !ok || got != want {
			t.Fatalf("queued event = %+v (open %v), want %p", got, ok, want)
		}
	default:
		t.Fatal("expected a queued event")
	}
}

func requireNoForwardedEvent(t *testing.T, c *wsConn) {
	t.Helper()
	select {
	case got, ok := <-c.queue:
		t.Fatalf("unexpected queued event %+v (open %v)", got, ok)
	default:
	}
}

func TestForwardKnownDeliversToAllMappedConnectionsOnly(t *testing.T) {
	key := camKey{userId: "u-alice", sessionId: "s-alice"}
	alice1 := newForwardTestConn(key)
	alice2 := newForwardTestConn(key)
	bob := newForwardTestConn(camKey{userId: "u-bob", sessionId: "s-bob"})
	table := newCamTable()
	table.learn(key, alice1)
	table.learn(key, alice2)
	table.learn(bob.camKey, bob)
	conns := map[*wsConn]struct{}{alice1: {}, alice2: {}, bob: {}}
	h := &WebSocketSSHandler{UnknownMessageFlood: true}
	ev := &ss.SignallingEvent{To: ss.EPAddr{UserId: key.userId, UserSessionId: key.sessionId}}

	h.forward(table, conns, ev)
	requireForwardedEvent(t, alice1, ev)
	requireForwardedEvent(t, alice2, ev)
	requireNoForwardedEvent(t, bob)
}

func TestForwardUnknownMessageFlood(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flood bool
	}{
		{name: "default drop", flood: false},
		{name: "enabled floods all current connections", flood: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			alice := newForwardTestConn(camKey{userId: "u-alice", sessionId: "s-alice"})
			bob := newForwardTestConn(camKey{userId: "u-bob", sessionId: "s-bob"})
			table := newCamTable()
			table.learn(alice.camKey, alice)
			conns := map[*wsConn]struct{}{alice: {}, bob: {}}
			h := &WebSocketSSHandler{UnknownMessageFlood: tc.flood}
			ev := &ss.SignallingEvent{To: ss.EPAddr{UserId: "u-unknown", UserSessionId: "s-unknown"}}

			h.forward(table, conns, ev)
			for _, c := range []*wsConn{alice, bob} {
				if tc.flood {
					requireForwardedEvent(t, c, ev)
				} else {
					requireNoForwardedEvent(t, c)
				}
			}
		})
	}
}

func TestForwardAlwaysDropsIncompleteDestinations(t *testing.T) {
	for _, tc := range []struct {
		name string
		to   ss.EPAddr
	}{
		{name: "empty"},
		{name: "user only", to: ss.EPAddr{UserId: "u"}},
		{name: "session only", to: ss.EPAddr{UserSessionId: "s"}},
		{name: "service only", to: ss.EPAddr{ServiceId: ss.WellKnownSvcIdSS}},
		{name: "user and service", to: ss.EPAddr{UserId: "u", ServiceId: ss.WellKnownSvcIdSS}},
		{name: "session and service", to: ss.EPAddr{UserSessionId: "s", ServiceId: ss.WellKnownSvcIdSS}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newForwardTestConn(camKey{userId: "u-alice", sessionId: "s-alice"})
			table := newCamTable()
			table.learn(c.camKey, c)
			conns := map[*wsConn]struct{}{c: {}}
			h := &WebSocketSSHandler{UnknownMessageFlood: true}
			h.forward(table, conns, &ss.SignallingEvent{To: tc.to})
			requireNoForwardedEvent(t, c)
		})
	}
}

func TestForwardKnownFailureDoesNotFallbackToFlood(t *testing.T) {
	key := camKey{userId: "u-alice", sessionId: "s-alice"}
	alice := newForwardTestConn(key)
	alice.queue <- &ss.SignallingEvent{MsgId: "already-full"}
	bob := newForwardTestConn(camKey{userId: "u-bob", sessionId: "s-bob"})
	table := newCamTable()
	table.learn(key, alice)
	conns := map[*wsConn]struct{}{alice: {}, bob: {}}
	h := &WebSocketSSHandler{UnknownMessageFlood: true}

	h.forward(table, conns, &ss.SignallingEvent{To: ss.EPAddr{UserId: key.userId, UserSessionId: key.sessionId}})
	requireNoForwardedEvent(t, bob)
	if _, ok := conns[alice]; ok {
		t.Fatal("slow known consumer remains in the connection set")
	}
	if _, ok := table.byAddr[key]; ok {
		t.Fatal("slow known consumer remains in the CAM table")
	}
}

func TestDropConnPurgesAllLearnedAddresses(t *testing.T) {
	key := camKey{userId: "u-alice", sessionId: "s-alice"}
	otherKey := camKey{userId: "u-other", sessionId: "s-other"}
	c := newForwardTestConn(key)
	sibling := newForwardTestConn(key)
	table := newCamTable()
	table.learn(key, c)
	table.learn(otherKey, c) // retain noteMessage's additional source learning
	table.learn(key, sibling)
	conns := map[*wsConn]struct{}{c: {}, sibling: {}}

	dropConn(table, conns, c)
	if _, ok := <-c.queue; ok {
		t.Fatal("dropped connection queue remains open")
	}
	if _, ok := conns[c]; ok {
		t.Fatal("dropped connection remains registered")
	}
	if _, ok := table.byConn[c]; ok {
		t.Fatal("dropped connection retains reverse CAM entries")
	}
	if _, ok := table.byAddr[otherKey]; ok {
		t.Fatal("address learned only by the dropped connection was not purged")
	}
	if got := table.byAddr[key]; len(got) != 1 {
		t.Fatalf("shared address has %d connections, want only its sibling", len(got))
	} else if _, ok := got[sibling]; !ok {
		t.Fatal("shared address lost its surviving sibling")
	}
}

func TestRegisterReturnsWhenHubIsDone(t *testing.T) {
	h := &WebSocketSSHandler{
		ctx:     context.Background(),
		ingress: make(chan hubNote, 1),
		hubDone: make(chan struct{}),
	}
	close(h.hubDone)
	if h.register(newForwardTestConn(camKey{userId: "u", sessionId: "s"})) {
		t.Fatal("register succeeded after the hub stopped")
	}
}

func TestReconnectRelearnsAddress(t *testing.T) {
	srv := startTestServer(t)
	first := dial(t, srv, "u-alice", "s-alice", "alice")
	mustRegister(t, first, "alice", "alice")
	first.Close()

	// Same (user id, user session id) on a new connection: the handler
	// must re-learn the address onto the new connection. The provider
	// still knows "alice", bound to this same (user, session, channel)
	// tuple, so the re-registration is a refresh — its ack must arrive
	// here (it would otherwise be routed to the dead first connection).
	second := dial(t, srv, "u-alice", "s-alice", "alice")
	writeEvent(t, second, registerEvent("m-reg-again", "alice", "alice"))
	reply := readEvent(t, second)
	if reply.S2CEv == nil || reply.S2CEv.Err != nil {
		t.Fatalf("reply = %+v, want a success ack", reply)
	}
	if reply.InReplyTo == nil || *reply.InReplyTo != "m-reg-again" {
		t.Fatalf("reply.InReplyTo = %v, want %q", reply.InReplyTo, "m-reg-again")
	}
}
