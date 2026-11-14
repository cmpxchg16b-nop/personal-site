package rtc

import (
	"context"
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

func startTransportTestServer(t *testing.T, suppressPongs bool) (string, <-chan struct{}) {
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

func TestWebSocketSignallingTransportDetectsMissingPongs(t *testing.T) {
	url, upgraded := startTransportTestServer(t, true)
	transport := &WebSocketSignallingTransport{
		URL:          url,
		PingInterval: 10 * time.Millisecond,
		PongTimeout:  60 * time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	toSS := make(chan *ss.SignallingEvent)
	fromSS := make(chan *ss.SignallingEvent)
	done := make(chan error, 1)
	go func() { done <- transport.Run(ctx, toSS, fromSS) }()
	<-upgraded

	select {
	case err := <-done:
		if err == nil || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Run error=%v, want a pong/read timeout before the outer context", err)
		}
	case <-time.After(900 * time.Millisecond):
		t.Fatal("Run remained blocked after pongs stopped")
	}
}

func TestWebSocketSignallingTransportHealthyPongsKeepConnectionAlive(t *testing.T) {
	url, upgraded := startTransportTestServer(t, false)
	transport := &WebSocketSignallingTransport{
		URL:          url,
		PingInterval: 10 * time.Millisecond,
		PongTimeout:  60 * time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Millisecond)
	defer cancel()
	toSS := make(chan *ss.SignallingEvent)
	fromSS := make(chan *ss.SignallingEvent)
	done := make(chan error, 1)
	go func() { done <- transport.Run(ctx, toSS, fromSS) }()
	<-upgraded

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Run error=%v, want only the outer context deadline", err)
		}
	case <-time.After(time.Second):
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
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	toSS := make(chan *ss.SignallingEvent, 1)
	fromSS := make(chan *ss.SignallingEvent)
	done := make(chan error, 1)
	go func() { done <- transport.Run(ctx, toSS, fromSS) }()
	<-upgraded
	fail.Store(true)
	toSS <- &ss.SignallingEvent{MsgId: "force-write-failure"}

	select {
	case err := <-done:
		if !errors.Is(err, errForcedTransportWrite) {
			t.Fatalf("Run error=%v, want the write failure to unblock the read loop", err)
		}
	case <-time.After(400 * time.Millisecond):
		t.Fatal("write pump exited but Run remained blocked in ReadMessage")
	}
}
