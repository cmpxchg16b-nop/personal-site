package rtc

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"personal-site/pkg/models/ss"
)

var transportTestEndpoint = ss.EPAddr{
	UserId:        "alice",
	UserSessionId: "alice-session",
}

func transportTestHello(endpoint ss.EPAddr) *ss.SignallingEvent {
	return &ss.SignallingEvent{
		From:  ss.EPAddr{ServiceId: ss.WellKnownSvcIdSS},
		To:    endpoint,
		MsgId: "endpoint-hello",
		S2CEv: &ss.SSToClientEv{Endpoint: &endpoint},
	}
}

func transportTestC2C(msgID ss.MsgId, to ss.EPAddr) *ss.SignallingEvent {
	return &ss.SignallingEvent{
		To:    to,
		MsgId: msgID,
		C2CEv: &ss.ClientToClientEv{
			// Deliberately look plausible to HeadlessRTCClient's second-layer
			// subscriber filtering. The transport must reject by EPAddr first.
			ToSubscriber: "self-subscriber",
		},
	}
}

func transportTestS2C(msgID ss.MsgId, to ss.EPAddr) *ss.SignallingEvent {
	return &ss.SignallingEvent{
		To:    to,
		MsgId: msgID,
		S2CEv: &ss.SSToClientEv{
			Pong: &ss.PingPongMsg{PingId: string(msgID)},
		},
	}
}

func writeTransportTestEvent(conn *websocket.Conn, ev *ss.SignallingEvent) error {
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, data)
}

func writeTransportTestClose(conn *websocket.Conn) error {
	return conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "test complete"),
		time.Now().Add(time.Second),
	)
}

func startTransportTestServer(t *testing.T, suppressPongs bool) (string, <-chan struct{}) {
	t.Helper()
	return startTransportTestServerWithHandler(t, suppressPongs, nil)
}

func startTransportTestServerWithHandler(
	t *testing.T,
	suppressPongs bool,
	serve func(*websocket.Conn) error,
) (string, <-chan struct{}) {
	t.Helper()
	upgraded := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if suppressPongs {
			conn.SetPingHandler(func(string) error { return nil })
		}
		if serve == nil {
			serve = func(conn *websocket.Conn) error {
				return writeTransportTestEvent(conn, transportTestHello(transportTestEndpoint))
			}
		}
		if err := serve(conn); err != nil {
			return
		}
		upgraded <- struct{}{}
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http"), upgraded
}

func waitTransportTestUpgrade(t *testing.T, upgraded <-chan struct{}) {
	t.Helper()
	select {
	case <-upgraded:
	case <-time.After(time.Second):
		t.Fatal("test WebSocket server did not finish its initial write")
	}
}

func runTransportTestExchange(
	t *testing.T,
	events ...*ss.SignallingEvent,
) []*ss.SignallingEvent {
	t.Helper()
	url, _ := startTransportTestServerWithHandler(t, false, func(conn *websocket.Conn) error {
		if err := writeTransportTestEvent(conn, transportTestHello(transportTestEndpoint)); err != nil {
			return err
		}
		for _, ev := range events {
			if err := writeTransportTestEvent(conn, ev); err != nil {
				return err
			}
		}
		return writeTransportTestClose(conn)
	})
	transport := &WebSocketSignallingTransport{
		URL:          url,
		PingInterval: time.Hour,
		PongTimeout:  time.Second,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	toSS := make(chan *ss.SignallingEvent)
	fromSS := make(chan *ss.SignallingEvent, 32)
	err := transport.Run(ctx, toSS, fromSS)
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Fatalf("Run did not finish the server exchange: %v", err)
	}
	var got []*ss.SignallingEvent
	for ev := range fromSS {
		got = append(got, ev)
	}
	return got
}

func assertTransportTestMessageOrder(t *testing.T, got []*ss.SignallingEvent, want ...ss.MsgId) {
	t.Helper()
	if len(got) != len(want) {
		ids := make([]ss.MsgId, 0, len(got))
		for _, ev := range got {
			ids = append(ids, ev.MsgId)
		}
		t.Fatalf("forwarded message ids = %v, want %v", ids, want)
	}
	for i, msgID := range want {
		if got[i].MsgId != msgID {
			t.Fatalf("forwarded message %d = %q, want %q", i, got[i].MsgId, msgID)
		}
	}
}

func TestWebSocketSignallingTransportFiltersMixedFlood(t *testing.T) {
	self := transportTestEndpoint
	selfWithService := self
	selfWithService.ServiceId = "service-metadata-does-not-change-the-cam-pair"
	foreign := ss.EPAddr{UserId: "bob", UserSessionId: "bob-session"}
	sameUserOtherSession := ss.EPAddr{UserId: self.UserId, UserSessionId: "other-session"}
	events := []*ss.SignallingEvent{
		transportTestC2C("foreign-c2c", foreign),
		transportTestS2C("foreign-s2c", foreign),
		transportTestC2C("self-c2c-1", self),
		transportTestC2C("self-c2c-with-service-id", selfWithService),
		transportTestC2C("same-user-other-session-c2c", sameUserOtherSession),
		transportTestS2C("same-user-other-session-s2c", sameUserOtherSession),
		transportTestC2C("empty-to-c2c", ss.EPAddr{}),
		transportTestS2C("empty-to-s2c", ss.EPAddr{}),
		transportTestC2C("user-only-to-c2c", ss.EPAddr{UserId: self.UserId}),
		transportTestS2C("user-only-to-s2c", ss.EPAddr{UserId: self.UserId}),
		transportTestC2C("session-only-to-c2c", ss.EPAddr{UserSessionId: self.UserSessionId}),
		transportTestS2C("session-only-to-s2c", ss.EPAddr{UserSessionId: self.UserSessionId}),
		transportTestS2C("self-s2c-1", self),
		transportTestC2C("self-c2c-2", self),
	}

	got := runTransportTestExchange(t, events...)
	assertTransportTestMessageOrder(t, got,
		"self-c2c-1", "self-c2c-with-service-id", "self-s2c-1", "self-c2c-2",
	)
}

func TestWebSocketSignallingTransportRepeatedHelloDoesNotChangeIdentity(t *testing.T) {
	foreign := ss.EPAddr{UserId: "bob", UserSessionId: "bob-session"}
	got := runTransportTestExchange(t,
		transportTestS2C("before-repeated-hello", transportTestEndpoint),
		transportTestHello(foreign),
		transportTestC2C("foreign-after-repeated-hello", foreign),
		transportTestS2C("self-after-repeated-hello", transportTestEndpoint),
	)
	assertTransportTestMessageOrder(t, got, "before-repeated-hello", "self-after-repeated-hello")
}

func TestWebSocketSignallingTransportSkipsBinaryAndMalformedFramesAfterHello(t *testing.T) {
	selfEvent := transportTestC2C("self-after-noise", transportTestEndpoint)
	binaryEvent, err := json.Marshal(transportTestC2C("binary-c2c", transportTestEndpoint))
	if err != nil {
		t.Fatal(err)
	}
	url, _ := startTransportTestServerWithHandler(t, false, func(conn *websocket.Conn) error {
		if err := writeTransportTestEvent(conn, transportTestHello(transportTestEndpoint)); err != nil {
			return err
		}
		if err := conn.WriteMessage(websocket.BinaryMessage, binaryEvent); err != nil {
			return err
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte("{not-json")); err != nil {
			return err
		}
		if err := writeTransportTestEvent(conn, selfEvent); err != nil {
			return err
		}
		return writeTransportTestClose(conn)
	})
	transport := &WebSocketSignallingTransport{
		URL:          url,
		PingInterval: time.Hour,
		PongTimeout:  time.Second,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	fromSS := make(chan *ss.SignallingEvent, 4)
	if err := transport.Run(ctx, make(chan *ss.SignallingEvent), fromSS); errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run did not finish the server exchange: %v", err)
	}
	var got []*ss.SignallingEvent
	for ev := range fromSS {
		got = append(got, ev)
	}
	assertTransportTestMessageOrder(t, got, "self-after-noise")
}

func TestWebSocketSignallingTransportRejectsInvalidEndpointHello(t *testing.T) {
	self := transportTestEndpoint
	tests := []struct {
		name  string
		write func(*websocket.Conn) error
	}{
		{
			name: "ordinary event before hello",
			write: func(conn *websocket.Conn) error {
				return writeTransportTestEvent(conn, transportTestC2C("business-before-hello", self))
			},
		},
		{
			name: "s2c event without endpoint",
			write: func(conn *websocket.Conn) error {
				return writeTransportTestEvent(conn, transportTestS2C("missing-endpoint", self))
			},
		},
		{
			name: "malformed JSON",
			write: func(conn *websocket.Conn) error {
				return conn.WriteMessage(websocket.TextMessage, []byte("{not-json"))
			},
		},
		{
			name: "binary hello",
			write: func(conn *websocket.Conn) error {
				data, err := json.Marshal(transportTestHello(self))
				if err != nil {
					return err
				}
				return conn.WriteMessage(websocket.BinaryMessage, data)
			},
		},
		{
			name: "sender is not signalling server",
			write: func(conn *websocket.Conn) error {
				ev := transportTestHello(self)
				ev.From.ServiceId = "not-ss"
				return writeTransportTestEvent(conn, ev)
			},
		},
		{
			name: "empty message id",
			write: func(conn *websocket.Conn) error {
				ev := transportTestHello(self)
				ev.MsgId = ""
				return writeTransportTestEvent(conn, ev)
			},
		},
		{
			name: "empty endpoint pair",
			write: func(conn *websocket.Conn) error {
				return writeTransportTestEvent(conn, transportTestHello(ss.EPAddr{}))
			},
		},
		{
			name: "missing user session id",
			write: func(conn *websocket.Conn) error {
				return writeTransportTestEvent(conn, transportTestHello(ss.EPAddr{UserId: self.UserId}))
			},
		},
		{
			name: "missing user id",
			write: func(conn *websocket.Conn) error {
				return writeTransportTestEvent(conn, transportTestHello(ss.EPAddr{UserSessionId: self.UserSessionId}))
			},
		},
		{
			name: "destination differs from payload",
			write: func(conn *websocket.Conn) error {
				ev := transportTestHello(self)
				other := ss.EPAddr{UserId: "bob", UserSessionId: "bob-session"}
				ev.S2CEv.Endpoint = &other
				return writeTransportTestEvent(conn, ev)
			},
		},
		{
			name: "destination is only half populated",
			write: func(conn *websocket.Conn) error {
				ev := transportTestHello(self)
				ev.To.UserSessionId = ""
				return writeTransportTestEvent(conn, ev)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			url, upgraded := startTransportTestServerWithHandler(t, false, tt.write)
			transport := &WebSocketSignallingTransport{
				URL:          url,
				PingInterval: time.Hour,
				PongTimeout:  500 * time.Millisecond,
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			fromSS := make(chan *ss.SignallingEvent, 1)
			done := make(chan error, 1)
			go func() {
				done <- transport.Run(ctx, make(chan *ss.SignallingEvent), fromSS)
			}()
			waitTransportTestUpgrade(t, upgraded)
			var err error
			select {
			case err = <-done:
			case <-time.After(time.Second):
				t.Fatal("Run did not reject the invalid endpoint hello")
			}
			if err == nil {
				t.Fatal("Run succeeded without a valid endpoint hello")
			}
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				t.Fatalf("Run did not reject the invalid hello promptly: %v", err)
			}
			if !strings.Contains(err.Error(), "endpoint hello") {
				t.Fatalf("Run error = %v, want an explicit endpoint hello failure", err)
			}
			if ev, ok := <-fromSS; ok {
				t.Fatalf("event %q was forwarded without a validated identity", ev.MsgId)
			}
		})
	}
}

func TestWebSocketSignallingTransportTimesOutMissingHello(t *testing.T) {
	url, upgraded := startTransportTestServerWithHandler(t, false, func(*websocket.Conn) error {
		return nil
	})
	transport := &WebSocketSignallingTransport{
		URL:          url,
		PingInterval: time.Hour,
		PongTimeout:  50 * time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	fromSS := make(chan *ss.SignallingEvent, 1)
	done := make(chan error, 1)
	go func() {
		done <- transport.Run(ctx, make(chan *ss.SignallingEvent), fromSS)
	}()
	waitTransportTestUpgrade(t, upgraded)
	var err error
	select {
	case err = <-done:
	case <-time.After(800 * time.Millisecond):
		t.Fatal("Run did not stop after the endpoint hello read timeout")
	}
	if err == nil || !strings.Contains(err.Error(), "endpoint hello") {
		t.Fatalf("Run error = %v, want an endpoint hello timeout", err)
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("Run error = %v, want the bounded read timeout", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run error = %v, want the hello timeout before the outer context", err)
	}
}

func TestWebSocketSignallingTransportHelloWaitHonorsCancellation(t *testing.T) {
	url, upgraded := startTransportTestServerWithHandler(t, false, func(*websocket.Conn) error {
		return nil
	})
	transport := &WebSocketSignallingTransport{
		URL:          url,
		PingInterval: time.Hour,
		PongTimeout:  time.Hour,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	toSS := make(chan *ss.SignallingEvent)
	fromSS := make(chan *ss.SignallingEvent)
	done := make(chan error, 1)
	go func() { done <- transport.Run(ctx, toSS, fromSS) }()
	waitTransportTestUpgrade(t, upgraded)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run remained blocked in the endpoint hello read after cancellation")
	}
}

func TestWebSocketSignallingTransportDefaultProtocolPingTiming(t *testing.T) {
	if DefaultPingInterval != 500*time.Millisecond {
		t.Fatalf("DefaultPingInterval = %v, want 500ms", DefaultPingInterval)
	}
	if DefaultPongTimeout != 3*time.Second {
		t.Fatalf("DefaultPongTimeout = %v, want 3s", DefaultPongTimeout)
	}
}

func TestWebSocketSignallingTransportDetectsMissingPongs(t *testing.T) {
	url, upgraded := startTransportTestServer(t, true)
	transport := &WebSocketSignallingTransport{
		URL:          url,
		PingInterval: 20 * time.Millisecond,
		PongTimeout:  200 * time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	toSS := make(chan *ss.SignallingEvent)
	fromSS := make(chan *ss.SignallingEvent)
	done := make(chan error, 1)
	go func() { done <- transport.Run(ctx, toSS, fromSS) }()
	waitTransportTestUpgrade(t, upgraded)

	select {
	case err := <-done:
		if err == nil || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Run error=%v, want a pong/read timeout before the outer context", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run remained blocked after pongs stopped")
	}
}

func TestWebSocketSignallingTransportHealthyPongsKeepConnectionAlive(t *testing.T) {
	url, upgraded := startTransportTestServer(t, false)
	transport := &WebSocketSignallingTransport{
		URL:          url,
		PingInterval: 20 * time.Millisecond,
		PongTimeout:  200 * time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 450*time.Millisecond)
	defer cancel()
	toSS := make(chan *ss.SignallingEvent)
	fromSS := make(chan *ss.SignallingEvent)
	done := make(chan error, 1)
	go func() { done <- transport.Run(ctx, toSS, fromSS) }()
	waitTransportTestUpgrade(t, upgraded)

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Run error=%v, want only the outer context deadline", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop at the outer context deadline")
	}
}

var errForcedTransportWrite = errors.New("forced transport write failure")

type failWriteConn struct {
	net.Conn
	fail *atomic.Bool
}

func (c *failWriteConn) Write(p []byte) (int, error) {
	if c.fail.Load() {
		return 0, errForcedTransportWrite
	}
	return c.Conn.Write(p)
}

func TestWebSocketSignallingTransportWriteFailureUnblocksRead(t *testing.T) {
	url, upgraded := startTransportTestServer(t, false)
	var fail atomic.Bool
	dialer := *websocket.DefaultDialer
	dialer.NetDialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &failWriteConn{Conn: conn, fail: &fail}, nil
	}
	transport := &WebSocketSignallingTransport{
		URL:          url,
		Dialer:       &dialer,
		WriteTimeout: 50 * time.Millisecond,
		PingInterval: time.Hour,
		PongTimeout:  time.Hour,
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	toSS := make(chan *ss.SignallingEvent, 1)
	fromSS := make(chan *ss.SignallingEvent)
	done := make(chan error, 1)
	go func() { done <- transport.Run(ctx, toSS, fromSS) }()
	waitTransportTestUpgrade(t, upgraded)
	fail.Store(true)
	toSS <- &ss.SignallingEvent{MsgId: "force-write-failure"}

	select {
	case err := <-done:
		if !errors.Is(err, errForcedTransportWrite) {
			t.Fatalf("Run error=%v, want the write failure to unblock the read loop", err)
		}
	case <-time.After(800 * time.Millisecond):
		t.Fatal("write pump exited but Run remained blocked in ReadMessage")
	}
}
