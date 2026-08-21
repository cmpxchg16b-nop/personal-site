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
)

func startTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	provider := ss.NewSimpleOnMemorySSProvider()
	h := NewWebSocketSSHandler(context.Background(), provider)
	srv := httptest.NewServer(h)
	t.Cleanup(func() {
		srv.Close()
		provider.Shutdown()
	})
	return srv
}

// dial opens a websocket connection with the experimental identity
// headers; empty userId/sessionId omit the respective header.
func dial(t *testing.T, srv *httptest.Server, userId, sessionId string) *websocket.Conn {
	t.Helper()
	header := http.Header{}
	if userId != "" {
		header.Set(HeaderExpUserId, userId)
	}
	if sessionId != "" {
		header.Set(HeaderExpUserSessionId, sessionId)
	}
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	c, _, err := websocket.DefaultDialer.Dial(url, header)
	if err != nil {
		t.Fatalf("Dial(%s, %s): %v", userId, sessionId, err)
	}
	t.Cleanup(func() { c.Close() })
	return c
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
	alice := dial(t, srv, "u-alice", "s-alice")

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
	alice := dial(t, srv, "u-alice", "s-alice")
	bob := dial(t, srv, "u-bob", "s-bob")
	carol := dial(t, srv, "u-carol", "s-carol")
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

func TestReconnectRelearnsAddress(t *testing.T) {
	srv := startTestServer(t)
	first := dial(t, srv, "u-alice", "s-alice")
	mustRegister(t, first, "alice", "alice")
	first.Close()

	// Same (user id, user session id) on a new connection: the handler
	// must re-learn the address onto the new connection. The provider
	// still knows "alice", bound to this same (user, session, channel)
	// tuple, so the re-registration is a refresh — its ack must arrive
	// here (it would otherwise be routed to the dead first connection).
	second := dial(t, srv, "u-alice", "s-alice")
	writeEvent(t, second, registerEvent("m-reg-again", "alice", "alice"))
	reply := readEvent(t, second)
	if reply.S2CEv == nil || reply.S2CEv.Err != nil {
		t.Fatalf("reply = %+v, want a success ack", reply)
	}
	if reply.InReplyTo == nil || *reply.InReplyTo != "m-reg-again" {
		t.Fatalf("reply.InReplyTo = %v, want %q", reply.InReplyTo, "m-reg-again")
	}
}
