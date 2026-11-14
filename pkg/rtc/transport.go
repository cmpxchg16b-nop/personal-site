package rtc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"personal-site/pkg/models/ss"
)

// SignallingTransport carries signalling events between a
// HeadlessRTCClient and the signalling server. It is the counterpart of
// the browser's SSProxy: a dumb pipe with JSON framing and no protocol
// logic, vended as a pair of channels — the Go rendition of
// getReadStream/getWriteStream, and the same contract
// SignallingServiceProvider.Run and Negotiator.Negotiate use. The client
// never sees the transport: the caller wires the two together, e.g.
//
//	toSS   := make(chan *ss.SignallingEvent, 64) // client → server
//	fromSS := make(chan *ss.SignallingEvent, 64) // server → client
//	go transport.Run(ctx, toSS, fromSS)
//	err := client.Run(ctx, fromSS, toSS)
//
// Run pumps events until ctx is done or the transport fails: events from
// ssEvIn are delivered to the server, events from the server are emitted
// on ssEvOut. Run closes ssEvOut when it returns — which is how the
// client learns the connection ended — and returns ctx.Err() on
// cancellation, nil on a clean server-side shutdown, and the transport's
// error otherwise. The transport never closes ssEvIn (the client owns
// it); cancel ctx to stop the transport.
type SignallingTransport interface {
	Run(ctx context.Context, ssEvIn <-chan *ss.SignallingEvent, ssEvOut chan<- *ss.SignallingEvent) error
}

const (
	// DefaultWriteTimeout is the default bound on a single websocket write —
	// the client-side counterpart of the handler's DefaultWriteTimeout, so a
	// wedged socket cannot block the write pump forever.
	DefaultWriteTimeout = 10 * time.Second

	// DefaultPingInterval and DefaultPongTimeout mirror the browser proxy's
	// liveness checks. Pings share the write pump; pongs extend the read deadline.
	DefaultPingInterval = time.Second
	DefaultPongTimeout  = 3 * time.Second
)

// WebSocketSignallingTransport is the production SignallingTransport: one
// WebSocket connection to the signalling endpoint
// (WebSocketSSHandler server-side), text frames carrying one JSON
// SignallingEvent each. Binary and unparseable frames are skipped,
// mirroring the server side.
//
// The endpoint requires an authenticated session (it is not on the JWT
// whitelist): put the session cookie (jwt=...) into Header.
type WebSocketSignallingTransport struct {
	// URL is the endpoint's WebSocket URL (ws:// or wss://).
	URL string

	// Header carries extra HTTP headers for the handshake — the endpoint
	// requires an authenticated session, so a deployment typically puts
	// its session cookie (jwt=...) here.
	Header http.Header

	// Dialer dials the connection; nil selects websocket.DefaultDialer.
	Dialer *websocket.Dialer

	// WriteTimeout bounds a single websocket write. Non-positive selects
	// DefaultWriteTimeout.
	WriteTimeout time.Duration

	// PingInterval is how often the write pump sends a WebSocket ping.
	// Non-positive selects DefaultPingInterval.
	PingInterval time.Duration

	// PongTimeout bounds the wait for pong responses. Non-positive selects
	// DefaultPongTimeout.
	PongTimeout time.Duration
}

// Run implements SignallingTransport. A failed dial — including a
// rejected handshake (the endpoint requires an authenticated session) —
// is returned as an error carrying the HTTP status when available.
func (t *WebSocketSignallingTransport) Run(ctx context.Context, ssEvIn <-chan *ss.SignallingEvent, ssEvOut chan<- *ss.SignallingEvent) error {
	if t.URL == "" {
		return errors.New("rtc: WebSocketSignallingTransport.URL is empty")
	}
	dialer := t.Dialer
	if dialer == nil {
		dialer = websocket.DefaultDialer
	}
	conn, resp, err := dialer.DialContext(ctx, t.URL, t.Header)
	if err != nil {
		if resp != nil {
			return fmt.Errorf("rtc: signalling handshake failed: %s: %w", resp.Status, err)
		}
		return fmt.Errorf("rtc: signalling dial: %w", err)
	}

	// Teardown order (defers run LIFO): done stops the write pump's
	// select, closing the connection unblocks a write in flight, wg.Wait
	// joins the pump, and only then is ssEvOut closed.
	done := make(chan struct{})
	var wg sync.WaitGroup
	defer close(ssEvOut)
	defer wg.Wait()
	defer conn.Close()
	defer close(done)

	// Unblock the read loop on cancellation: conn.Close makes the pending
	// ReadMessage fail.
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()

	// A pong proves that the server still reads this connection. Merely
	// receiving application events is not enough to detect a half-open roaming
	// path, so only pongs (and the initial deadline) arm this timer.
	pongTimeout := t.pongTimeout()
	_ = conn.SetReadDeadline(time.Now().Add(pongTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongTimeout))
	})

	// A failed write must close the socket: otherwise this write pump exits
	// while Run remains blocked in ReadMessage and the caller never reconnects.
	writeFailure := make(chan error, 1)
	failWrite := func(err error) {
		writeFailure <- err
		_ = conn.Close()
	}

	// Write pump: the sole application/ping writer for this connection.
	wg.Add(1)
	go func() {
		defer wg.Done()
		ping := time.NewTicker(t.pingInterval())
		defer ping.Stop()
		for {
			select {
			case <-done:
				return
			case <-ping.C:
				deadline := time.Now().Add(t.writeTimeout())
				if err := conn.WriteControl(websocket.PingMessage, nil, deadline); err != nil {
					failWrite(fmt.Errorf("rtc: signalling ping: %w", err))
					return
				}
			case ev, ok := <-ssEvIn:
				if !ok {
					return
				}
				data, err := json.Marshal(ev)
				if err != nil {
					continue
				}
				_ = conn.SetWriteDeadline(time.Now().Add(t.writeTimeout()))
				if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
					failWrite(fmt.Errorf("rtc: signalling write: %w", err))
					return
				}
			}
		}
	}()

	// Read loop, on Run's own goroutine.
	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			select {
			case writeErr := <-writeFailure:
				return writeErr
			default:
			}
			return fmt.Errorf("rtc: signalling read: %w", err)
		}
		if mt != websocket.TextMessage {
			continue
		}
		var ev ss.SignallingEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			continue
		}
		select {
		case ssEvOut <- &ev:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// writeTimeout resolves the configured WriteTimeout; a non-positive value
// selects DefaultWriteTimeout.
func (t *WebSocketSignallingTransport) writeTimeout() time.Duration {
	if t.WriteTimeout <= 0 {
		return DefaultWriteTimeout
	}
	return t.WriteTimeout
}

func (t *WebSocketSignallingTransport) pingInterval() time.Duration {
	if t.PingInterval <= 0 {
		return DefaultPingInterval
	}
	return t.PingInterval
}

func (t *WebSocketSignallingTransport) pongTimeout() time.Duration {
	if t.PongTimeout <= 0 {
		return DefaultPongTimeout
	}
	return t.PongTimeout
}

var _ SignallingTransport = (*WebSocketSignallingTransport)(nil)
