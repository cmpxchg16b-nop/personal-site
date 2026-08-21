// Package websocket_ss terminates the WebSocket connections of the
// site's signalling clients and bridges them to a pkg/models/ss
// SignallingServiceProvider: every parsed text frame is forwarded to the
// provider, and every event the provider emits is written to the
// connection its To EPAddr resolves to.
//
// Client identity is experimental — taken, for now, from the
// X-Exp-UserId and X-Exp-UserSessionId request headers. As the wire
// prototype allows (see web/site/src/api/ss/types.ts), the handler
// populates empty From EPAddr fields of inbound messages from these
// headers.
//
// Like a learning switch, the handler builds the association between a
// connection's remote ip:port and the (user id, user session id) pair by
// sniffing the From EPAddr field of inbound messages. It never learns —
// and never needs — the association between a subscriber id and a
// connection: the SS provider translates (channel, subscriber)
// addressing into an EPAddr, so every outbound event is unicasted by its
// To EPAddr. Events whose destination was never learned (or whose
// connection is already gone) are dropped.
package websocket_ss

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/gorilla/websocket"

	"personal-site/pkg/models/ss"
)

const (
	// HeaderExpUserId carries the experimental user id of the connecting
	// client.
	HeaderExpUserId = "X-Exp-UserId"

	// HeaderExpUserSessionId carries the experimental user session id of
	// the connecting client.
	HeaderExpUserSessionId = "X-Exp-UserSessionId"
)

const (
	inMsgBufferSize     = 64
	outMsgBufferSize    = 64
	ingressBufferSize   = 64
	connQueueBufferSize = 32

	// writeTimeout bounds a single websocket write so a stuck client
	// cannot block its writer goroutine forever.
	writeTimeout = 10 * time.Second
)

// WebSocketSSHandler is an http.Handler that upgrades signalling client
// connections to WebSocket and bridges them to a
// SignallingServiceProvider.
//
// A single hub goroutine owns the connection set and the learned address
// table; connection goroutines talk to it exclusively through the
// ingress channel, so no mutex is needed anywhere.
type WebSocketSSHandler struct {
	// Upgrader performs the HTTP→WebSocket upgrade. Customize it (e.g.
	// CheckOrigin) before serving the first request.
	Upgrader websocket.Upgrader

	ctx     context.Context
	inMsg   chan *ss.SignallingEvent // events from all connections, to the provider
	outMsg  chan *ss.SignallingEvent // events emitted by the provider
	ingress chan hubNote             // notes from connection goroutines to the hub
	hubDone chan struct{}            // closed when the hub returns
}

// NewWebSocketSSHandler constructs a WebSocketSSHandler bridging to
// provider, which must be non-nil. The provider's Run loop and the
// handler's hub start immediately and live until ctx is done (or the
// provider stops); connections still open then are closed.
func NewWebSocketSSHandler(ctx context.Context, provider ss.SignallingServiceProvider) *WebSocketSSHandler {
	h := &WebSocketSSHandler{
		Upgrader: websocket.Upgrader{ReadBufferSize: 4096, WriteBufferSize: 4096},
		ctx:      ctx,
		inMsg:    make(chan *ss.SignallingEvent, inMsgBufferSize),
		outMsg:   make(chan *ss.SignallingEvent, outMsgBufferSize),
		ingress:  make(chan hubNote, ingressBufferSize),
		hubDone:  make(chan struct{}),
	}
	go provider.Run(ctx, h.inMsg, h.outMsg)
	go h.hub()
	return h
}

// ServeHTTP implements http.Handler. Requests without both experimental
// identity headers are answered 400; all other requests are upgraded to
// WebSocket.
func (h *WebSocketSSHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	userId := ss.UserId(r.Header.Get(HeaderExpUserId))
	sessionId := ss.UserSessionId(r.Header.Get(HeaderExpUserSessionId))
	if userId == "" || sessionId == "" {
		http.Error(w, "missing "+HeaderExpUserId+" or "+HeaderExpUserSessionId+" header", http.StatusBadRequest)
		return
	}
	conn, err := h.Upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // the upgrader has already answered the request
	}
	c := &wsConn{
		remote: conn.RemoteAddr().String(),
		conn:   conn,
		queue:  make(chan *ss.SignallingEvent, connQueueBufferSize),
	}
	if !h.note(hubNote{kind: noteRegister, conn: c}) {
		conn.Close()
		return
	}
	go h.writePump(c)
	go h.readPump(c, userId, sessionId)
}

// hub is the single goroutine owning the connection set and the learned
// address table. It forwards inbound events to the provider and routes
// outbound events to their destination connection. It returns when ctx
// is done or the provider closes outMsg, closing every connection's
// queue (which makes the write pumps close the connections).
func (h *WebSocketSSHandler) hub() {
	defer close(h.hubDone)
	table := newCamTable()
	conns := make(map[*wsConn]struct{})
	defer func() {
		for c := range conns {
			close(c.queue)
		}
	}()

	for {
		select {
		case <-h.ctx.Done():
			return
		case ev, ok := <-h.outMsg:
			if !ok {
				return // the provider stopped
			}
			h.forward(table, conns, ev)
		case n := <-h.ingress:
			switch n.kind {
			case noteRegister:
				conns[n.conn] = struct{}{}
			case noteClosed:
				if _, ok := conns[n.conn]; ok {
					dropConn(table, conns, n.conn)
				}
			case noteMessage:
				// Source address learning, switch-style: remember
				// which connection this From was seen on.
				table.learn(camKeyOf(n.ev.From), n.conn)
				select {
				case h.inMsg <- n.ev:
				case <-h.ctx.Done():
					return
				}
			}
		}
	}
}

// forward routes an outbound event to the connection its To EPAddr was
// learned on. Events with an empty or never-learned destination are
// dropped.
func (h *WebSocketSSHandler) forward(table *camTable, conns map[*wsConn]struct{}, ev *ss.SignallingEvent) {
	key := camKeyOf(ev.To)
	if key.empty() {
		return
	}
	c, ok := table.byAddr[key]
	if !ok {
		return
	}
	select {
	case c.queue <- ev:
	default:
		// Slow consumer: drop the connection rather than stall the hub.
		dropConn(table, conns, c)
	}
}

// dropConn forgets c and every address learned on it; closing its queue
// makes the write pump close the websocket connection.
func dropConn(table *camTable, conns map[*wsConn]struct{}, c *wsConn) {
	delete(conns, c)
	table.purge(c)
	close(c.queue)
}

// note posts n to the hub, reporting false while the handler is
// shutting down.
func (h *WebSocketSSHandler) note(n hubNote) bool {
	select {
	case h.ingress <- n:
		return true
	case <-h.ctx.Done():
		return false
	case <-h.hubDone:
		return false
	}
}

// readPump parses the text frames of c as SignallingEvents, populates
// empty From fields from the connection's identity headers, and hands
// the events to the hub. Binary and unparseable frames are skipped. It
// returns when the connection fails or the handler shuts down, and
// reports the closure to the hub.
func (h *WebSocketSSHandler) readPump(c *wsConn, userId ss.UserId, sessionId ss.UserSessionId) {
	defer h.note(hubNote{kind: noteClosed, conn: c})
	for {
		mt, data, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		if mt != websocket.TextMessage {
			continue
		}
		var ev ss.SignallingEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			continue
		}
		if ev.From.UserId == "" {
			ev.From.UserId = userId
		}
		if ev.From.UserSessionId == "" {
			ev.From.UserSessionId = sessionId
		}
		if !h.note(hubNote{kind: noteMessage, conn: c, ev: &ev}) {
			return
		}
	}
}

// writePump writes the events queued for c until the queue is closed or
// a write fails; it closes the websocket connection when it returns.
func (h *WebSocketSSHandler) writePump(c *wsConn) {
	defer c.conn.Close()
	for {
		select {
		case <-h.ctx.Done():
			return
		case ev, ok := <-c.queue:
			if !ok {
				return
			}
			data, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
				return
			}
		}
	}
}

// wsConn is one upgraded websocket connection plus its outbound queue.
// remote is its ip:port, kept for logs and debugging.
type wsConn struct {
	remote string
	conn   *websocket.Conn
	queue  chan *ss.SignallingEvent
}

type noteKind int

const (
	noteRegister noteKind = iota + 1
	noteMessage
	noteClosed
)

// hubNote is a note from a connection's goroutines to the hub: the
// connection registered, delivered a parsed event, or was closed. Notes
// of one connection arrive in the order they happened.
type hubNote struct {
	kind noteKind
	conn *wsConn
	ev   *ss.SignallingEvent
}

// camKey is the (user id, user session id) pair a connection is learned
// under, mirroring a switch's CAM table entry.
type camKey struct {
	userId    ss.UserId
	sessionId ss.UserSessionId
}

func camKeyOf(addr ss.EPAddr) camKey {
	return camKey{userId: addr.UserId, sessionId: addr.UserSessionId}
}

func (k camKey) empty() bool { return k.userId == "" || k.sessionId == "" }

// camTable holds the learned (user id, user session id) → connection
// associations, with a reverse index for fast aging on link-down. It is
// owned by the hub goroutine and needs no locking.
type camTable struct {
	byAddr map[camKey]*wsConn
	byConn map[*wsConn][]camKey
}

func newCamTable() *camTable {
	return &camTable{
		byAddr: make(map[camKey]*wsConn),
		byConn: make(map[*wsConn][]camKey),
	}
}

// learn records that addr was last seen on c.
func (t *camTable) learn(addr camKey, c *wsConn) {
	if addr.empty() || t.byAddr[addr] == c {
		return
	}
	t.byAddr[addr] = c
	t.byConn[c] = append(t.byConn[c], addr)
}

// purge forgets every address learned on c.
func (t *camTable) purge(c *wsConn) {
	for _, addr := range t.byConn[c] {
		if t.byAddr[addr] == c {
			delete(t.byAddr, addr)
		}
	}
	delete(t.byConn, c)
}

var _ http.Handler = (*WebSocketSSHandler)(nil)
