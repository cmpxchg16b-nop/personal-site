// Package websocket_ss terminates the WebSocket connections of the
// site's signalling clients and bridges them to a pkg/models/ss
// SignallingServiceProvider: ordinary parsed text frames are forwarded to the
// provider, and every event the provider emits is written to the connection
// its To EPAddr resolves to. Client SIP events are intercepted for the
// configured SIPHandler instead of reaching that ordinary provider.
//
// Client identity comes from the caller's session (see
// pkgsession.SessionManager): the session's subject id is the user id and
// the session id is the user session id. The endpoint is not on the
// server's JWT whitelist, so every connection belongs to an authenticated
// session; the From EPAddr of every inbound message is overridden with it
// — a client-supplied From is untrusted input and is discarded. The same
// discipline applies to the register event's username: it is overridden
// with the session's username (the JWT's username claim), so a client
// never picks its own display name — it may send the field empty.
//
// Like a learning switch, the handler builds the association between a
// connection and its authenticated (user id, user session id) pair as soon
// as the connection is registered, and retains source-address learning from
// inbound messages. It never learns — and never needs — the association
// between a subscriber id and a connection: the SS provider translates
// (channel, subscriber) addressing into an EPAddr, so every outbound event
// is routed by its To EPAddr alone. One EPAddr can map to several
// connections — two tabs sharing one session, or a reconnect whose old
// connection has not been noticed closed yet — so the event is delivered to
// every connection the address was learned on. Events with a complete but
// unknown destination are dropped unless UnknownMessageFlood is enabled;
// incomplete destinations are always dropped.
package websocket_ss

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"personal-site/pkg/models/ss"
	pkgsession "personal-site/pkg/session"
)

const (
	inMsgBufferSize     = 64
	outMsgBufferSize    = 64
	ingressBufferSize   = 64
	connQueueBufferSize = 32
)

// DefaultWriteTimeout is the default bound on a single websocket write;
// see WebSocketSSHandler.WriteTimeout.
const DefaultWriteTimeout = 10 * time.Second

var (
	errSIPPayload = errors.New("websocket_ss: exactly one of SIP call and SIP message must be non-nil")
	errHubStopped = errors.New("websocket_ss: hub is stopped")
)

// SIPHandler receives client SIP events and connection-close notifications.
// The hub invokes these methods synchronously: implementations must only
// enqueue work for their own service goroutines and return promptly. In
// particular, they must not perform SIP/network I/O or wait for a hub request.
type SIPHandler interface {
	HandleSIP(userID ss.UserId, deliveryID string, call *ss.SIPCallEvent, message *ss.SIPMessageEvent)
	SIPDisconnected(deliveryID string)
}

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

	// WriteTimeout bounds a single websocket write, so a stuck client
	// cannot block its writer goroutine forever. Zero (or negative)
	// selects DefaultWriteTimeout. Set it before serving the first
	// request.
	WriteTimeout time.Duration

	// UnknownMessageFlood sends events with a complete but unlearned
	// (user id, user session id) destination to every current connection.
	// The default is false. Enable it only for diagnostics or trusted
	// environments: flooding can disclose signalling data across sessions
	// and multiplies outbound traffic. Incomplete destinations are always
	// dropped, and known destinations are never flooded.
	UnknownMessageFlood bool

	sm         pkgsession.SessionManager
	sipHandler SIPHandler
	ctx        context.Context
	inMsg      chan *ss.SignallingEvent // events from all connections, to the provider
	outMsg     chan *ss.SignallingEvent // events emitted by the provider
	ingress    chan hubNote             // notes from connection goroutines to the hub
	hubDone    chan struct{}            // closed when the hub returns
}

// NewWebSocketSSHandler constructs a WebSocketSSHandler bridging to
// provider, which must be non-nil, resolving each connection's identity
// through sm (the request-scoped session populated by the session
// middleware upstream). The provider's Run loop and the handler's hub
// start immediately and live until ctx is done (or the provider stops);
// connections still open then are closed.
func NewWebSocketSSHandler(ctx context.Context, provider ss.SignallingServiceProvider, sm pkgsession.SessionManager) *WebSocketSSHandler {
	h := &WebSocketSSHandler{
		Upgrader: websocket.Upgrader{ReadBufferSize: 4096, WriteBufferSize: 4096},
		sm:       sm,
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

// SetSIPHandler sets the external user-SIP event handler. It must be called
// before serving the first connection, like the handler's other configuration
// setters. A nil handler drops intercepted SIP events without forwarding them
// to the ordinary provider.
func (h *WebSocketSSHandler) SetSIPHandler(handler SIPHandler) {
	h.sipHandler = handler
}

// FanoutSIP queues an independent copy of call or message on every current
// socket belonging to userID, across sessions and duplicate connections. It
// returns the delivery IDs whose queues accepted the event. No matching
// sockets is a successful empty result. Exactly one payload must be non-nil.
func (h *WebSocketSSHandler) FanoutSIP(ctx context.Context, userID ss.UserId, call *ss.SIPCallEvent, message *ss.SIPMessageEvent) ([]string, error) {
	if (call == nil) == (message == nil) {
		return nil, errSIPPayload
	}
	result, err := h.requestSIP(ctx, hubNote{kind: noteFanoutSIP, userID: userID, call: call, message: message})
	if err != nil {
		return nil, err
	}
	return result.deliveryIDs, nil
}

// SendSIP queues call or message only on the current socket identified by
// deliveryID. It returns false when that delivery is stale or missing, or its
// bounded queue is full. Exactly one payload must be non-nil.
func (h *WebSocketSSHandler) SendSIP(ctx context.Context, deliveryID string, call *ss.SIPCallEvent, message *ss.SIPMessageEvent) (bool, error) {
	if (call == nil) == (message == nil) {
		return false, errSIPPayload
	}
	result, err := h.requestSIP(ctx, hubNote{kind: noteSendSIP, deliveryID: deliveryID, call: call, message: message})
	if err != nil {
		return false, err
	}
	return result.delivered, nil
}

// requestSIP posts a bounded hub request and waits for its acknowledgement.
// The capacity-one ack lets the hub finish even when the caller abandons the
// request through ctx cancellation.
func (h *WebSocketSSHandler) requestSIP(ctx context.Context, n hubNote) (sipResult, error) {
	n.ack = make(chan sipResult, 1)
	select {
	case <-ctx.Done():
		return sipResult{}, ctx.Err()
	default:
	}
	select {
	case h.ingress <- n:
	case <-ctx.Done():
		return sipResult{}, ctx.Err()
	case <-h.ctx.Done():
		return sipResult{}, h.ctx.Err()
	case <-h.hubDone:
		return sipResult{}, errHubStopped
	}
	select {
	case result := <-n.ack:
		return result, nil
	case <-ctx.Done():
		return sipResult{}, ctx.Err()
	case <-h.ctx.Done():
		return sipResult{}, h.ctx.Err()
	case <-h.hubDone:
		return sipResult{}, errHubStopped
	}
}

// ServeHTTP implements http.Handler. Requests whose session carries no
// identity (no subject id or no session id) are answered 400; all other
// requests are upgraded to WebSocket.
func (h *WebSocketSSHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var userId ss.UserId
	var sessionId ss.UserSessionId
	var username string
	if sess, ok := h.sm.GetSessionFromContext(r.Context()); ok {
		userId = ss.UserId(sess.SubjectId())
		sessionId = ss.UserSessionId(sess.Id())
		username = sess.Username()
	}
	if userId == "" || sessionId == "" {
		http.Error(w, "session carries no identity (subject id or session id is empty)", http.StatusBadRequest)
		return
	}
	conn, err := h.Upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // the upgrader has already answered the request
	}
	endpoint := ss.EPAddr{UserId: userId, UserSessionId: sessionId}
	c := &wsConn{
		remote:     conn.RemoteAddr().String(),
		conn:       conn,
		queue:      make(chan *ss.SignallingEvent, connQueueBufferSize),
		camKey:     camKeyOf(endpoint),
		deliveryID: uuid.NewString(),
	}
	// The hello is connection-local and is queued before registration can
	// make any provider event eligible for delivery, so it is always first.
	c.queue <- &ss.SignallingEvent{
		From:  ss.EPAddr{ServiceId: ss.WellKnownSvcIdSS},
		To:    endpoint,
		MsgId: ss.MsgId(uuid.NewString()),
		S2CEv: &ss.SSToClientEv{Endpoint: &endpoint},
	}
	// Do not start either pump until the hub has eagerly learned c.camKey.
	// Thus observing the hello also proves that eager CAM learning finished.
	if !h.register(c) {
		conn.Close()
		return
	}
	go h.writePump(c)
	go h.readPump(c, username)
}

// hub is the single goroutine owning the connection set and the learned
// address table. It forwards ordinary inbound events to the provider, handles
// SIP interception and delivery requests, and routes outbound events to their
// destination connection. It returns when ctx
// is done or the provider closes outMsg, closing every connection's
// queue (which makes the write pumps close the connections).
func (h *WebSocketSSHandler) hub() {
	defer close(h.hubDone)
	table := newCamTable()
	conns := make(map[*wsConn]struct{})
	defer func() {
		for c := range conns {
			h.dropConn(table, conns, c)
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
				table.learn(n.conn.camKey, n.conn)
				close(n.done)
			case noteClosed:
				if _, ok := conns[n.conn]; ok {
					h.dropConn(table, conns, n.conn)
				}
			case noteMessage:
				if _, ok := conns[n.conn]; !ok {
					continue
				}
				if n.ev.C2SEv != nil && (n.ev.C2SEv.SIPCall != nil || n.ev.C2SEv.SIPMessage != nil) {
					// Intercept SIP before the ordinary provider and replace the
					// complete source address, including any spoofed ServiceId.
					n.ev.From = ss.EPAddr{UserId: n.conn.camKey.userId, UserSessionId: n.conn.camKey.sessionId}
					table.learn(camKeyOf(n.ev.From), n.conn)
					if h.sipHandler != nil {
						h.sipHandler.HandleSIP(n.conn.camKey.userId, n.conn.deliveryID, n.ev.C2SEv.SIPCall, n.ev.C2SEv.SIPMessage)
					}
					continue
				}
				// Source address learning, switch-style: remember
				// this connection serves the From address (one address
				// may be served by several connections).
				table.learn(camKeyOf(n.ev.From), n.conn)
				select {
				case h.inMsg <- n.ev:
				case <-h.ctx.Done():
					return
				}
			case noteFanoutSIP:
				n.ack <- sipResult{deliveryIDs: h.fanoutSIP(table, conns, n.userID, n.call, n.message)}
			case noteSendSIP:
				n.ack <- sipResult{delivered: h.sendSIP(table, conns, n.deliveryID, n.call, n.message)}
			}
		}
	}
}

// forward routes an outbound event to every connection its complete To
// EPAddr was learned on — one address may map to several connections (e.g.
// two tabs sharing one session). A complete unknown destination is flooded
// only when configured; an incomplete destination is always dropped.
func (h *WebSocketSSHandler) forward(table *camTable, conns map[*wsConn]struct{}, ev *ss.SignallingEvent) {
	key := camKeyOf(ev.To)
	if key.empty() {
		return
	}
	targets, known := table.byAddr[key]
	if !known {
		if !h.UnknownMessageFlood {
			return
		}
		targets = conns
	}
	for c := range targets {
		select {
		case c.queue <- ev:
		default:
			// Slow consumer: drop the connection rather than stall the hub.
			// A failed known delivery never falls back to flooding.
			h.dropConn(table, conns, c)
		}
	}
}

// fanoutSIP delivers to every active socket of userID. The hub owns conns, so
// scanning it cannot race connection registration or removal.
func (h *WebSocketSSHandler) fanoutSIP(table *camTable, conns map[*wsConn]struct{}, userID ss.UserId, call *ss.SIPCallEvent, message *ss.SIPMessageEvent) []string {
	deliveryIDs := make([]string, 0)
	for c := range conns {
		if c.camKey.userId == userID && h.enqueueSIP(table, conns, c, call, message) {
			deliveryIDs = append(deliveryIDs, c.deliveryID)
		}
	}
	return deliveryIDs
}

// sendSIP delivers only to the exact current delivery ID, never to a newer
// socket that happens to have the same authenticated endpoint.
func (h *WebSocketSSHandler) sendSIP(table *camTable, conns map[*wsConn]struct{}, deliveryID string, call *ss.SIPCallEvent, message *ss.SIPMessageEvent) bool {
	if deliveryID == "" {
		return false
	}
	for c := range conns {
		if c.deliveryID == deliveryID {
			return h.enqueueSIP(table, conns, c, call, message)
		}
	}
	return false
}

func (h *WebSocketSSHandler) enqueueSIP(table *camTable, conns map[*wsConn]struct{}, c *wsConn, call *ss.SIPCallEvent, message *ss.SIPMessageEvent) bool {
	select {
	case c.queue <- newSIPEvent(c.camKey, call, message):
		return true
	default:
		h.dropConn(table, conns, c)
		return false
	}
}

func newSIPEvent(key camKey, call *ss.SIPCallEvent, message *ss.SIPMessageEvent) *ss.SignallingEvent {
	body := &ss.SSToClientEv{}
	if call != nil {
		callCopy := *call
		body.SIPCall = &callCopy
	}
	if message != nil {
		messageCopy := *message
		body.SIPMessage = &messageCopy
	}
	return &ss.SignallingEvent{
		From:  ss.EPAddr{ServiceId: ss.WellKnownSvcIdSIP},
		To:    ss.EPAddr{UserId: key.userId, UserSessionId: key.sessionId},
		MsgId: ss.MsgId(uuid.NewString()),
		S2CEv: body,
	}
}

// dropConn removes c before notifying the SIP service, so a disconnect
// callback can never observe that delivery as still active.
func (h *WebSocketSSHandler) dropConn(table *camTable, conns map[*wsConn]struct{}, c *wsConn) {
	dropConn(table, conns, c)
	if h.sipHandler != nil {
		h.sipHandler.SIPDisconnected(c.deliveryID)
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

// register waits until the hub has added c and learned its immutable CAM
// key. It also returns when shutdown prevents that acknowledgement.
func (h *WebSocketSSHandler) register(c *wsConn) bool {
	done := make(chan struct{})
	if !h.note(hubNote{kind: noteRegister, conn: c, done: done}) {
		return false
	}
	select {
	case <-done:
		return true
	case <-h.ctx.Done():
		return false
	case <-h.hubDone:
		return false
	}
}

// readPump parses the text frames of c as SignallingEvents and hands them
// to the hub, enforcing the client protocol boundary: From is stamped with
// the connection's session identity (clearing any claimed service), every
// client-supplied S2CEv is discarded, and a register event's username is
// stamped with the session's username. Binary and unparseable frames are
// skipped. It returns when the connection fails or the handler shuts down,
// and reports the closure to the hub.
func (h *WebSocketSSHandler) readPump(c *wsConn, username string) {
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
		ev.From = ss.EPAddr{UserId: c.camKey.userId, UserSessionId: c.camKey.sessionId}
		ev.S2CEv = nil
		if ev.C2SEv != nil && ev.C2SEv.Register != nil {
			ev.C2SEv.Register.Username = username
		}
		if !h.note(hubNote{kind: noteMessage, conn: c, ev: &ev}) {
			return
		}
	}
}

// writeTimeout resolves the configured WriteTimeout; a non-positive
// value selects DefaultWriteTimeout.
func (h *WebSocketSSHandler) writeTimeout() time.Duration {
	if h.WriteTimeout <= 0 {
		return DefaultWriteTimeout
	}
	return h.WriteTimeout
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
			c.conn.SetWriteDeadline(time.Now().Add(h.writeTimeout()))
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
	// camKey is fixed at upgrade from the authenticated session identity.
	camKey camKey
	// deliveryID uniquely identifies this socket for external SIP delivery.
	deliveryID string
}

type noteKind int

const (
	noteRegister noteKind = iota + 1
	noteMessage
	noteClosed
	noteFanoutSIP
	noteSendSIP
)

// hubNote is a note from a connection's goroutines to the hub: the
// connection registered, delivered a parsed event, or was closed. Notes
// of one connection arrive in the order they happened.
type hubNote struct {
	kind noteKind
	conn *wsConn
	ev   *ss.SignallingEvent
	// done is closed after a register note has been fully applied.
	done chan struct{}

	userID     ss.UserId
	deliveryID string
	call       *ss.SIPCallEvent
	message    *ss.SIPMessageEvent
	ack        chan sipResult
}

// sipResult is the buffered acknowledgement of a SIP delivery request.
type sipResult struct {
	deliveryIDs []string
	delivered   bool
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

// camTable holds the learned (user id, user session id) → connections
// associations — one address can map to several connections (e.g. two
// tabs sharing one session, or a reconnect whose old connection has not
// been noticed closed yet) — with a reverse index for fast aging on
// link-down. It is owned by the hub goroutine and needs no locking.
type camTable struct {
	byAddr map[camKey]map[*wsConn]struct{}
	byConn map[*wsConn][]camKey
}

func newCamTable() *camTable {
	return &camTable{
		byAddr: make(map[camKey]map[*wsConn]struct{}),
		byConn: make(map[*wsConn][]camKey),
	}
}

// learn records that addr is reachable via c, keeping any connections
// the address was already learned on.
func (t *camTable) learn(addr camKey, c *wsConn) {
	if addr.empty() {
		return
	}
	set, ok := t.byAddr[addr]
	if !ok {
		set = make(map[*wsConn]struct{})
		t.byAddr[addr] = set
	} else if _, dup := set[c]; dup {
		return
	}
	set[c] = struct{}{}
	t.byConn[c] = append(t.byConn[c], addr)
}

// purge forgets every address learned on c.
func (t *camTable) purge(c *wsConn) {
	for _, addr := range t.byConn[c] {
		if set, ok := t.byAddr[addr]; ok {
			delete(set, c)
			if len(set) == 0 {
				delete(t.byAddr, addr)
			}
		}
	}
	delete(t.byConn, c)
}

var _ http.Handler = (*WebSocketSSHandler)(nil)

// CheckOriginAllowing returns a websocket.Upgrader CheckOrigin func that
// applies gorilla's default policy — no Origin header, or an Origin whose
// host matches the request's — and additionally trusts origins in
// allowedOrigins (scheme://host), mirroring the login handlers. The extra
// trust covers development, where the browser's origin is the frontend
// dev server proxying /api/* to this server.
func CheckOriginAllowing(allowedOrigins []string) func(r *http.Request) bool {
	return func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true
		}
		u, err := url.Parse(origin)
		if err != nil {
			return false
		}
		if strings.EqualFold(u.Host, r.Host) {
			return true
		}
		return slices.Contains(allowedOrigins, u.Scheme+"://"+u.Host)
	}
}
