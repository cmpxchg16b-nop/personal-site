package rtc

// This file implements HeadlessRTCClient, the composition of the pieces a
// working headless RTC peer — a bot — needs: a channel registration kept
// alive for the connection's lifetime, member discovery, and one peer
// session (a pion peer connection plus a Negotiator) per discovered
// member. It is the Go counterpart of the browser's useSignalling +
// usePeerSessions hooks (web/site/src/api/ss/react.tsx, peersessions.tsx).
//
// The client talks to the signalling server over a pair of channels —
// one for inbound events, one for outbound — exactly like the frontend's
// SSProxy vends a read and a write stream, and exactly like Negotiator
// and SignallingServiceProvider do: the client never sees a WebSocket, a
// handshake, or an authentication detail; wiring the channels to a
// transport (WebSocketSignallingTransport in transport.go, or anything
// else) is the caller's business.
//
// The client deliberately knows nothing about what the data channels
// carry: the browser application runs a JSON messaging protocol on
// "dcmsg" and a binary file transfer on "dcbin", but here those labels —
// and every other — belong to the caller, registered with
// HandleDataChannel the way an http.ServeMux registers patterns. Media is
// the same shape: local tracks attach per session with AddTrack (the
// session's negotiator renegotiates on its own), remote tracks arrive at
// the handler registered with HandleTrack.
//
// Concurrency model: a single hub goroutine — started by the constructor
// and living for the client's lifetime, like SimpleOnMemorySSProvider's
// Run — owns every piece of mutable state (the handler table, the peer
// sessions, the current registration). Everything else — the caller's
// goroutines (Run, HandleDataChannel, the accessors), pion's callback
// goroutines, the negotiators — coordinates with the hub exclusively
// through serviceChan, so the client needs no mutexes (CSP, like the
// other stateful components of the signalling stack).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/pion/webrtc/v4"

	"personal-site/pkg/models/ss"
)

// NegotiatorFactory builds the Negotiator of one peer session: pc is the
// session's freshly created peer connection (the client's data-channel
// wiring already attached), and options carries the c2c addressing the
// client resolved — the channel, the client's own (already registered)
// subscriber id, and the peer's — plus the peer-connection rebuild factory
// the polite peer needs to yield to a colliding offer (see
// PerfectNegotiatorOptions.NewPeerConnection). A factory may adjust the
// options (e.g. a different Logger) before constructing the negotiator.
//
// The client needs a factory rather than a Negotiator instance because
// there is one negotiator per peer, and because the client's own
// subscriber id — part of every negotiator's addressing — may only be
// assigned by the SS at registration, after the client is constructed.
type NegotiatorFactory func(pc *webrtc.PeerConnection, options PerfectNegotiatorOptions) (Negotiator, error)

// PerfectNegotiatorFactory adapts NewPerfectNegotiator to
// NegotiatorFactory — the standard choice for NewHeadlessRTCClient,
// running the MDN perfect negotiation pattern with every peer.
func PerfectNegotiatorFactory(pc *webrtc.PeerConnection, options PerfectNegotiatorOptions) (Negotiator, error) {
	return NewPerfectNegotiator(pc, options)
}

// DataChannelHandler handles one peer's data channel of the label it was
// registered for — the counterpart of http.Handler for data channels.
//
// ServeDataChannel is invoked once per (session, label) incarnation: when
// a session's channel of the label appears — created locally by the polite
// peer, or announced by the remote peer (pion's OnDataChannel, whose event
// carries the data channel this label matches on) — and again with the
// fresh channel when a glare rebuild replaces the session's peer
// connection. The channel may still be connecting; attach OnOpen/OnMessage
// callbacks instead of assuming it is open.
//
// ctx is the peer session's context: it is canceled when the session ends
// (the peer dropped out of the channel, or the client's Run ended), so
// handler-spawned work should derive from it. channelId is the channel
// both subscribers are registered in (the client's configured channel).
// peer is the remote subscriber id.
//
// Every invocation runs on its own goroutine; invocations for different
// sessions may run concurrently. A handler is expected to wire the channel
// up and return; a panicking handler is logged and swallowed.
type DataChannelHandler interface {
	ServeDataChannel(ctx context.Context, channelId ss.ChannelId, peer ss.SubscriberId, dc *webrtc.DataChannel)
}

// DataChannelHandlerFunc adapts a function to a DataChannelHandler — the
// counterpart of http.HandlerFunc.
type DataChannelHandlerFunc func(ctx context.Context, channelId ss.ChannelId, peer ss.SubscriberId, dc *webrtc.DataChannel)

// ServeDataChannel implements DataChannelHandler.
func (f DataChannelHandlerFunc) ServeDataChannel(ctx context.Context, channelId ss.ChannelId, peer ss.SubscriberId, dc *webrtc.DataChannel) {
	f(ctx, channelId, peer, dc)
}

var _ DataChannelHandler = DataChannelHandlerFunc(nil)

// TrackHandler handles one remote media track arriving on a peer session —
// the counterpart of DataChannelHandler for the peer connection's OnTrack:
// the peer's microphone (or any track the peer adds mid-session, once the
// channels are long up) arrives here.
//
// ServeTrack is invoked once per (session, track): when a remote track
// arrives on the session's peer connection — and again for the re-arrived
// track after a glare rebuild replaced the session's peer connection (the
// renegotiation re-announces it). ctx is the peer session's context (see
// DataChannelHandler). Every invocation runs on its own goroutine; a
// panicking handler is logged and swallowed.
type TrackHandler interface {
	ServeTrack(ctx context.Context, channelId ss.ChannelId, peer ss.SubscriberId, track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver)
}

// TrackHandlerFunc adapts a function to a TrackHandler — the counterpart
// of DataChannelHandlerFunc.
type TrackHandlerFunc func(ctx context.Context, channelId ss.ChannelId, peer ss.SubscriberId, track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver)

// ServeTrack implements TrackHandler.
func (f TrackHandlerFunc) ServeTrack(ctx context.Context, channelId ss.ChannelId, peer ss.SubscriberId, track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
	f(ctx, channelId, peer, track, receiver)
}

var _ TrackHandler = TrackHandlerFunc(nil)

var (
	// ErrNilNegotiatorFactory is returned by NewHeadlessRTCClient when the
	// negotiator factory is nil.
	ErrNilNegotiatorFactory = errors.New("rtc: nil NegotiatorFactory")

	// ErrNilSignallingChannels is returned by Run when either signalling
	// channel is nil.
	ErrNilSignallingChannels = errors.New("rtc: nil signalling channels")

	// ErrAlreadyRunning is returned by Run when a previous Run call on the
	// same client is still in flight.
	ErrAlreadyRunning = errors.New("rtc: Run is already running")

	// ErrNoPeerSession is returned by AddTrack when the client holds no
	// peer session with the given subscriber (no Run is in flight, or the
	// peer is not a channel member).
	ErrNoPeerSession = errors.New("rtc: no peer session with the given subscriber")
)

// errPeerSessionGone answers a rebuild request that arrived after its
// session was torn down.
var errPeerSessionGone = errors.New("rtc: peer session is gone")

// Defaults of the client's periodic activities, mirroring the browser
// client: the keepalive and the listing run well below the SS's default
// subscriber aging (10s), so a live client never ages out and membership
// changes are picked up quickly.
const (
	defaultKeepAliveInterval  = 5 * time.Second
	defaultMemberListInterval = 5 * time.Second
	defaultReplyTimeout       = 5 * time.Second

	// serviceChanBuffer bounds the notes piling up for the hub;
	// sessionChannelBuffer bounds one session's hub-facing channel (a
	// session's inbound pump drains it eagerly, so the hub never wedges
	// behind a busy negotiator).
	serviceChanBuffer    = 64
	sessionChannelBuffer = 16
)

// RTCClientConfiguration configures a HeadlessRTCClient.
type RTCClientConfiguration struct {
	// ChannelId is the channel the client registers in; only its members
	// become peer sessions. Empty selects the well-known main channel.
	ChannelId ss.ChannelId

	// SubscriberId is the subscriber id to register; empty asks the SS to
	// assign one from the automatic assignment range. When the configured
	// id is rejected as already registered to another tuple, the client
	// retries once with an empty id, mirroring the browser.
	SubscriberId ss.SubscriberId

	// ICEServers holds the STUN/TURN URLs of the peer connections (e.g.
	// "stun:stun.l.google.com:19302") — what the browser loads from
	// GET /api/iceServers. Empty means no ICE servers: peers on the local
	// network still connect.
	ICEServers []string

	// NewPeerConnection creates one peer connection per peer session (and
	// per glare rebuild). Nil selects the default: a pion peer connection
	// configured with ICEServers. Set it to customize pion itself (a
	// SettingEngine, media codecs, …); the client attaches its own
	// data-channel wiring on every returned connection.
	NewPeerConnection func() (*webrtc.PeerConnection, error)

	// KeepAliveInterval is how often the channel membership is renewed
	// once registered. Non-positive selects the default; keep it well
	// below the SS's subscriber aging.
	KeepAliveInterval time.Duration

	// MemberListInterval is how often the channel's member list is
	// re-read: sessions appear as the listing discovers members and are
	// torn down when members drop out. Non-positive selects the default.
	MemberListInterval time.Duration

	// ReplyTimeout bounds the wait for the SS's registration reply.
	// Non-positive selects the default.
	ReplyTimeout time.Duration

	// Logger receives the client's diagnostics; nil selects slog's
	// default logger.
	Logger *slog.Logger
}

// HeadlessRTCClient is a headless — UI-less — RTC peer: more of a bot
// than a full client. Run it with a pair of signalling channels and it
// registers as a subscriber of the configured channel, keeps the
// registration alive, discovers the channel's members, and maintains one
// peer session (a peer connection driven by a Negotiator over the c2c
// relay) per fellow member — without any opinion about what the resulting
// data channels carry. Register what to do with a channel by label with
// HandleDataChannel.
//
// The zero value is not usable; construct with NewHeadlessRTCClient. The
// client's hub goroutine lives for the client's lifetime — a client is
// meant to be long-lived, like the frontend's SSProxy singleton.
type HeadlessRTCClient struct {
	newNegotiator NegotiatorFactory

	// The configuration, normalized at construction.
	channelId          ss.ChannelId
	subscriberId       ss.SubscriberId
	newPC              func() (*webrtc.PeerConnection, error)
	keepAliveInterval  time.Duration
	memberListInterval time.Duration
	replyTimeout       time.Duration
	logger             *slog.Logger

	// serviceChan carries every request to the hub goroutine — the only
	// goroutine touching the client's mutable state.
	serviceChan chan clientNote

	// handlers is the data channel handler table by label. Owned by the
	// hub goroutine; registered via handleNote, so it persists across
	// runs like an http.ServeMux's patterns.
	handlers map[string]DataChannelHandler

	// trackHandler is the remote-media-track handler (pc.OnTrack),
	// registered via trackHandlerNote; nil until one is registered. Owned
	// by the hub goroutine.
	trackHandler TrackHandler
}

// NewHeadlessRTCClient constructs a HeadlessRTCClient whose peer sessions
// negotiate through newNegotiator (PerfectNegotiatorFactory for the
// standard pattern), configured by config. The client's hub starts here
// and nothing is connected yet: HandleDataChannel the labels of interest,
// then Run.
func NewHeadlessRTCClient(newNegotiator NegotiatorFactory, config RTCClientConfiguration) (*HeadlessRTCClient, error) {
	if newNegotiator == nil {
		return nil, ErrNilNegotiatorFactory
	}
	channelId := config.ChannelId
	if channelId == "" {
		channelId = ss.WellKnownChIdMain
	}
	newPC := config.NewPeerConnection
	if newPC == nil {
		newPC = defaultPeerConnectionFactory(config.ICEServers)
	}
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}
	c := &HeadlessRTCClient{
		newNegotiator:      newNegotiator,
		channelId:          channelId,
		subscriberId:       config.SubscriberId,
		newPC:              newPC,
		keepAliveInterval:  orDuration(config.KeepAliveInterval, defaultKeepAliveInterval),
		memberListInterval: orDuration(config.MemberListInterval, defaultMemberListInterval),
		replyTimeout:       orDuration(config.ReplyTimeout, defaultReplyTimeout),
		logger:             logger,
		serviceChan:        make(chan clientNote, serviceChanBuffer),
		handlers:           make(map[string]DataChannelHandler),
	}
	go c.hub()
	return c, nil
}

// defaultPeerConnectionFactory builds the default peer-connection factory:
// a pion peer connection with the configured ICE servers — mirroring the
// browser's { iceServers: urls.length ? [{ urls }] : [] }.
func defaultPeerConnectionFactory(iceServers []string) func() (*webrtc.PeerConnection, error) {
	return func() (*webrtc.PeerConnection, error) {
		config := webrtc.Configuration{}
		if len(iceServers) > 0 {
			config.ICEServers = []webrtc.ICEServer{{URLs: iceServers}}
		}
		return webrtc.NewPeerConnection(config)
	}
}

func orDuration(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// HandleDataChannel registers handler for the data channels of the given
// label — like http.ServeMux.Handle registers a pattern: every peer
// session's channel whose label matches is handed to handler as it
// appears, whether it is created locally (the polite side of a session
// creates a channel per handled label) or announced by the peer
// (OnDataChannel). Channels of labels with no registered handler are kept
// and delivered when a handler is registered later, mirroring the
// frontend's subscribeChannel.
//
// It panics when handler is nil or a handler for label is already
// registered — mirroring http.ServeMux. It is safe to call before or
// while Run is in flight; when it returns, the handler is live and every
// existing session's channel of the label has been created or delivered.
func (c *HeadlessRTCClient) HandleDataChannel(label string, handler DataChannelHandler) {
	if handler == nil {
		panic("rtc: nil data channel handler")
	}
	ack := make(chan error, 1)
	c.serviceChan <- handleNote{label: label, handler: handler, ack: ack}
	if err := <-ack; err != nil {
		panic(err)
	}
}

// HandleDataChannelFunc registers a function as the handler of the data
// channels of the given label — the counterpart of http.HandleFunc.
func (c *HeadlessRTCClient) HandleDataChannelFunc(label string, handler func(ctx context.Context, channelId ss.ChannelId, peer ss.SubscriberId, dc *webrtc.DataChannel)) {
	c.HandleDataChannel(label, DataChannelHandlerFunc(handler))
}

// HandleTrack registers handler for the remote media tracks of every peer
// session — the counterpart of HandleDataChannel for the peer
// connection's OnTrack, and the mirror of the browser's
// PeerSessions.subscribeTracks. It panics when handler is nil or a track
// handler is already registered. It is safe to call before or while Run is
// in flight.
func (c *HeadlessRTCClient) HandleTrack(handler TrackHandler) {
	if handler == nil {
		panic("rtc: nil track handler")
	}
	ack := make(chan error, 1)
	c.serviceChan <- trackHandlerNote{handler: handler, ack: ack}
	if err := <-ack; err != nil {
		panic(err)
	}
}

// HandleTrackFunc registers a function as the remote-media-track handler —
// the counterpart of HandleDataChannelFunc.
func (c *HeadlessRTCClient) HandleTrackFunc(handler func(ctx context.Context, channelId ss.ChannelId, peer ss.SubscriberId, track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver)) {
	c.HandleTrack(TrackHandlerFunc(handler))
}

// AddTrack adds track to the peer session's peer connection, to be sent to
// the peer — the mirror of the browser's PeerSessions.addTrack: the
// session's negotiator renegotiates automatically (pion fires
// OnNegotiationNeeded), so no signalling work is needed here. It returns
// the sender, mirroring pion's own AddTrack. ErrNoPeerSession is returned
// when there is no session with the peer; re-adding an already attached
// track is a no-op returning its current sender.
//
// The track survives a glare rebuild: the client re-adds the session's
// tracks to the rebuilt connection. The returned sender does NOT — it
// belongs to the replaced connection — so RemoveTrack keys by track, never
// by sender. The track must be a comparable implementation (pion's track
// types are pointers).
func (c *HeadlessRTCClient) AddTrack(peer ss.SubscriberId, track webrtc.TrackLocal) (*webrtc.RTPSender, error) {
	if track == nil {
		panic("rtc: nil track")
	}
	reply := make(chan addTrackReply, 1)
	c.serviceChan <- addTrackNote{peer: peer, track: track, reply: reply}
	r := <-reply
	return r.sender, r.err
}

// RemoveTrack removes track (previously added with AddTrack) from the peer
// session — the mirror of the browser's PeerSessions.removeTrack, keyed by
// track rather than sender so a glare rebuild (which swaps the sender)
// cannot stale the caller's handle. Removing renegotiates like adding
// does. It is a no-op when the session or the track is already gone.
func (c *HeadlessRTCClient) RemoveTrack(peer ss.SubscriberId, track webrtc.TrackLocal) error {
	if track == nil {
		panic("rtc: nil track")
	}
	reply := make(chan error, 1)
	c.serviceChan <- removeTrackNote{peer: peer, track: track, reply: reply}
	return <-reply
}

// SubscriberId reports the subscriber id the client is registered as;
// empty while no Run is registered (before the first one, between runs,
// after the last one ended).
func (c *HeadlessRTCClient) SubscriberId() ss.SubscriberId {
	reply := make(chan ss.SubscriberId, 1)
	c.serviceChan <- selfNote{reply: reply}
	return <-reply
}

// Peers reports the subscriber ids the client currently holds a peer
// session with, sorted. A session's presence says nothing about its
// connection state — a freshly discovered peer is still negotiating.
func (c *HeadlessRTCClient) Peers() []ss.SubscriberId {
	reply := make(chan []ss.SubscriberId, 1)
	c.serviceChan <- peersNote{reply: reply}
	return <-reply
}

// Run serves one signalling connection: it registers the client as a
// subscriber of the configured channel and drives it — renewing the
// membership, renewing the member listing, and routing c2c events between
// the channels and the per-peer negotiators — until ctx is done or ssEvIn
// closes. Every peer session is torn down before Run returns; the data
// channel handlers persist for a later Run.
//
// The channel directions mirror Negotiator.Negotiate: the client receives
// inbound events from ssEvIn and sends outbound ones to ssEvOut. The
// channels stay owned by the caller: Run never closes either, and outbound
// sends honor ctx cancellation, so a stopped consumer wedges the run until
// canceled (exactly like a Negotiator). Run returns nil when ssEvIn is
// closed, ctx.Err() when ctx is done, the registration's error when the SS
// rejects (or never answers) the registration, and ErrAlreadyRunning when
// a previous Run is still in flight.
func (c *HeadlessRTCClient) Run(ctx context.Context, ssEvIn <-chan *ss.SignallingEvent, ssEvOut chan<- *ss.SignallingEvent) error {
	if ssEvIn == nil || ssEvOut == nil {
		return ErrNilSignallingChannels
	}
	ack := make(chan error, 1)
	select {
	case c.serviceChan <- runNote{ctx: ctx, in: ssEvIn, out: ssEvOut, ack: ack}:
		return <-ack
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ---------------------------------------------------------------------------
// The hub: the single goroutine owning all client state, fed by
// serviceChan and the active run's channels.
// ---------------------------------------------------------------------------

// clientNote is one request to the hub goroutine. Sealed: all
// implementations live here.
type clientNote interface{ isClientNote() }

// runNote asks the hub to serve one signalling connection (Run).
type runNote struct {
	ctx context.Context
	in  <-chan *ss.SignallingEvent
	out chan<- *ss.SignallingEvent
	ack chan<- error // buffered: the run's terminal error, answered once
}

// handleNote registers a data channel handler (HandleDataChannel).
type handleNote struct {
	label   string
	handler DataChannelHandler
	ack     chan<- error // buffered: non-nil on a duplicate registration
}

// announcedNote reports a data channel announced by the peer (OnDataChannel).
type announcedNote struct {
	sess *peerSession
	dc   *webrtc.DataChannel
}

// trackHandlerNote registers the remote-media-track handler (HandleTrack).
type trackHandlerNote struct {
	handler TrackHandler
	ack     chan<- error // buffered: non-nil on a duplicate registration
}

// addTrackNote attaches a local media track to a peer session's peer
// connection (AddTrack).
type addTrackNote struct {
	peer  ss.SubscriberId
	track webrtc.TrackLocal
	reply chan<- addTrackReply // buffered: answered once
}

type addTrackReply struct {
	sender *webrtc.RTPSender
	err    error
}

// removeTrackNote detaches a local media track from a peer session's peer
// connection (RemoveTrack).
type removeTrackNote struct {
	peer  ss.SubscriberId
	track webrtc.TrackLocal
	reply chan<- error // buffered: answered once
}

// trackNote reports a remote media track arriving on a session (OnTrack).
type trackNote struct {
	sess     *peerSession
	track    *webrtc.TrackRemote
	receiver *webrtc.RTPReceiver
}

// rebuildNote is a session's glare yield: the negotiator needs a fresh
// peer connection, re-wired and re-stocked with the session's channels.
type rebuildNote struct {
	sess  *peerSession
	reply chan<- rebuildReply
}

type rebuildReply struct {
	pc  *webrtc.PeerConnection
	err error
}

// selfNote / peersNote answer the accessors.
type selfNote struct{ reply chan<- ss.SubscriberId }
type peersNote struct{ reply chan<- []ss.SubscriberId }

func (runNote) isClientNote()          {}
func (handleNote) isClientNote()       {}
func (announcedNote) isClientNote()    {}
func (trackHandlerNote) isClientNote() {}
func (addTrackNote) isClientNote()     {}
func (removeTrackNote) isClientNote()  {}
func (trackNote) isClientNote()        {}
func (rebuildNote) isClientNote()      {}
func (selfNote) isClientNote()         {}
func (peersNote) isClientNote()        {}

// clientRun is the hub's state for one served connection: the Run
// request's channels and context, the registration, the tickers, the
// in-flight listing round, and the peer sessions.
type clientRun struct {
	ctx    context.Context
	cancel context.CancelFunc
	in     <-chan *ss.SignallingEvent
	out    chan<- *ss.SignallingEvent
	ack    chan<- error

	self      ss.SubscriberId
	keepAlive *time.Ticker
	listing   *time.Ticker
	round     *listingRound
	sessions  map[ss.SubscriberId]*peerSession
}

// hub is the client's single stateful goroutine. While no Run is in
// flight the run-scoped select cases stay nil (and block forever), so
// only the service channel is served — the nil-channel idiom keeps one
// select for both phases.
func (c *HeadlessRTCClient) hub() {
	var run *clientRun
	for {
		var in <-chan *ss.SignallingEvent
		var runDone <-chan struct{}
		var keepAliveC, listingC <-chan time.Time
		if run != nil {
			in = run.in
			runDone = run.ctx.Done()
			keepAliveC = run.keepAlive.C
			listingC = run.listing.C
		}
		select {
		case note := <-c.serviceChan:
			switch n := note.(type) {
			case runNote:
				run = c.startRun(run, n)
			case handleNote:
				c.handleDataChannelNote(run, n)
			case announcedNote:
				c.handleAnnouncedNote(run, n)
			case trackHandlerNote:
				c.handleTrackHandlerNote(n)
			case addTrackNote:
				c.handleAddTrackNote(run, n)
			case removeTrackNote:
				c.handleRemoveTrackNote(run, n)
			case trackNote:
				c.handleTrackNote(run, n)
			case rebuildNote:
				c.handleRebuildNote(run, n)
			case selfNote:
				if run == nil {
					n.reply <- ""
				} else {
					n.reply <- run.self
				}
			case peersNote:
				peers := make([]ss.SubscriberId, 0)
				if run != nil {
					for peer := range run.sessions {
						peers = append(peers, peer)
					}
					slices.Sort(peers)
				}
				n.reply <- peers
			}
		case <-runDone:
			c.endRun(run, run.ctx.Err())
			run = nil
		case ev, ok := <-in:
			if !ok {
				c.endRun(run, nil) // the caller closed the input: a clean end
				run = nil
				continue
			}
			c.handleInbound(run, ev)
		case <-keepAliveC:
			c.sendKeepAlive(run)
		case <-listingC:
			if run.round != nil {
				c.logger.Warn("rtc: member listing round superseded before completion", "msgId", run.round.msgId)
			}
			run.round = c.startListingRound(run)
		}
	}
}

// startRun installs the run's state and performs the initial
// registration, answering the Run caller's ack. The registration's
// request/reply wait happens inline — nothing can reach the client before
// it is registered (the SS cannot resolve its address), so the only
// events to expect are the replies themselves; the wait is bounded by
// ReplyTimeout. A second Run while one is in flight is rejected.
func (c *HeadlessRTCClient) startRun(existing *clientRun, n runNote) *clientRun {
	if existing != nil {
		n.ack <- ErrAlreadyRunning
		return existing
	}
	ctx, cancel := context.WithCancel(n.ctx)
	run := &clientRun{
		ctx:      ctx,
		cancel:   cancel,
		in:       n.in,
		out:      n.out,
		ack:      n.ack,
		sessions: make(map[ss.SubscriberId]*peerSession),
	}
	self, err := c.register(run)
	if err != nil {
		cancel()
		n.ack <- err
		return nil
	}
	run.self = self
	run.keepAlive = time.NewTicker(c.keepAliveInterval)
	run.listing = time.NewTicker(c.memberListInterval)
	c.logger.Info("rtc: registered", "channelId", c.channelId, "subscriberId", self)
	c.sendKeepAlive(run)
	run.round = c.startListingRound(run)
	return run
}

// endRun tears the run down: tickers stopped, every session closed, the
// Run caller's ack answered with the run's terminal error.
func (c *HeadlessRTCClient) endRun(run *clientRun, err error) {
	run.keepAlive.Stop()
	run.listing.Stop()
	run.cancel() // ends every session context
	for _, sess := range run.sessions {
		if err := sess.pc.Close(); err != nil {
			c.logger.Warn("rtc: close peer connection", "peer", sess.peer, "err", err)
		}
	}
	run.ack <- err
}

// register registers the client as a subscriber of the configured channel
// and returns the registered subscriber id. A configured id the SS rejects
// as registered to another tuple is forgotten and retried once with an
// empty (SS-assigned) id, mirroring the browser client.
func (c *HeadlessRTCClient) register(run *clientRun) (ss.SubscriberId, error) {
	id := c.subscriberId
	for attempt := 0; attempt < 2; attempt++ {
		assigned, retry, err := c.registerOnce(run, id)
		if err != nil {
			return "", err
		}
		if !retry {
			return assigned, nil
		}
		id = ""
	}
	// Unreachable: the second attempt registers with an empty id, which
	// cannot hit ErrorCodeSubscriberIdIsRegistered.
	return "", errors.New("rtc: registration failed")
}

// registerOnce performs one registration attempt. retry reports whether
// the caller should retry with an empty subscriber id.
func (c *HeadlessRTCClient) registerOnce(run *clientRun, id ss.SubscriberId) (assigned ss.SubscriberId, retry bool, err error) {
	// The display name is not the client's to choose: the WS endpoint
	// stamps the session's username (the JWT's claim) onto the register
	// event, so the wire field goes empty.
	msgId, ok := c.sendToSS(run, &ss.ClientToSSEv{Register: &ss.ClientToSSRegEv{
		SubscriberId: id,
		ChannelId:    c.channelId,
	}})
	if !ok {
		return "", false, run.ctx.Err()
	}
	timer := time.NewTimer(c.replyTimeout)
	defer timer.Stop()
	for {
		select {
		case <-run.ctx.Done():
			return "", false, run.ctx.Err()
		case <-timer.C:
			return "", false, fmt.Errorf("rtc: registration reply timed out after %s", c.replyTimeout)
		case ev, ok := <-run.in:
			if !ok {
				return "", false, errors.New("rtc: signalling input ended before the registration reply")
			}
			if ev.S2CEv == nil || ev.InReplyTo == nil || *ev.InReplyTo != msgId {
				continue // not the registration reply; nothing else is expected this early
			}
			if result := ev.S2CEv.RegisterResult; result != nil {
				return result.SubscriberId, false, nil
			}
			if e := ev.S2CEv.Err; e != nil {
				if e.ErrorCode == ss.ErrorCodeSubscriberIdIsRegistered && id != "" {
					return "", true, nil
				}
				return "", false, fmt.Errorf("rtc: registration rejected: %s (error code %d)", e.ErrorMsg, e.ErrorCode)
			}
		}
	}
}

// handleInbound routes one inbound event: a c2c event goes to its peer
// session (a session-desc offer from a peer the listing has not
// discovered yet adopts the peer — the browser reconciles the same race
// within one listing interval, a headless client can simply not lose the
// offer); a s2c reply feeds the in-flight listing round.
func (c *HeadlessRTCClient) handleInbound(run *clientRun, ev *ss.SignallingEvent) {
	if c2c := ev.C2CEv; c2c != nil {
		if c2c.ChannelId != c.channelId || c2c.ToSubscriber != run.self || c2c.FromSubscriber == run.self {
			return
		}
		sess := run.sessions[c2c.FromSubscriber]
		if sess == nil {
			if c2c.SessionDesc == nil || c2c.SessionDesc.Type != webrtc.SDPTypeOffer {
				return // a stray answer or candidate cannot start a session
			}
			sess = c.startSession(run, c2c.FromSubscriber)
			if sess == nil {
				return
			}
		}
		// The session's inbound pump drains the channel eagerly, so this
		// send cannot wedge the hub behind a busy negotiator.
		select {
		case sess.in <- ev:
		case <-run.ctx.Done():
		}
		return
	}
	if ev.S2CEv != nil {
		if run.round != nil && ev.InReplyTo != nil && *ev.InReplyTo == run.round.msgId {
			c.handleListingPage(run, ev)
			return
		}
		if e := ev.S2CEv.Err; e != nil {
			// An error reply to something uncorrelated — e.g. a keepalive
			// whose membership expired.
			c.logger.Warn("rtc: signalling error", "code", e.ErrorCode, "err", e.ErrorMsg)
		}
	}
}

// listingRound tracks one in-flight listChannelMembers request: the
// request's message id and the members accumulated over the reply's pages.
type listingRound struct {
	msgId   ss.MsgId
	members []ss.SubscriberId
}

// startListingRound sends a listChannelMembers request and returns the
// round accumulating its replies (nil when the send failed).
func (c *HeadlessRTCClient) startListingRound(run *clientRun) *listingRound {
	msgId, ok := c.sendToSS(run, &ss.ClientToSSEv{ListChannelMembers: &ss.ClientToSSListChannelMembers{
		ChannelId: c.channelId,
	}})
	if !ok {
		return nil
	}
	return &listingRound{msgId: msgId}
}

// handleListingPage folds one reply page into the round; the final page
// reconciles the sessions with the listed members.
func (c *HeadlessRTCClient) handleListingPage(run *clientRun, ev *ss.SignallingEvent) {
	if e := ev.S2CEv.Err; e != nil {
		c.logger.Error("rtc: member listing failed", "code", e.ErrorCode, "err", e.ErrorMsg)
		run.round = nil
		return
	}
	page := ev.S2CEv.ChannelMbsListResult
	if page == nil {
		return
	}
	run.round.members = append(run.round.members, page.Members...)
	if page.HasMore {
		return
	}
	members := run.round.members
	run.round = nil
	c.reconcile(run, members)
}

// reconcile aligns the peer sessions with the channel's member listing:
// a session is started for every newly seen member (except the client
// itself), and the sessions of members that dropped out are closed —
// mirroring the frontend's reconcile effect.
func (c *HeadlessRTCClient) reconcile(run *clientRun, members []ss.SubscriberId) {
	want := make(map[ss.SubscriberId]struct{}, len(members))
	for _, member := range members {
		if member != run.self {
			want[member] = struct{}{}
		}
	}
	for peer := range want {
		if _, ok := run.sessions[peer]; !ok {
			c.startSession(run, peer)
		}
	}
	for peer, sess := range run.sessions {
		if _, ok := want[peer]; !ok {
			c.closeSession(run, sess)
		}
	}
}

// startSession brings up one peer session: a peer connection, its
// negotiator, and — on the polite side — a data channel per handled
// label. It reports nil when the session could not be brought up. The
// negotiator is constructed before any channel is created: pion drops a
// negotiation-needed event fired while no handler is attached (see
// NewPerfectNegotiator).
func (c *HeadlessRTCClient) startSession(run *clientRun, peer ss.SubscriberId) *peerSession {
	pc, err := c.newPC()
	if err != nil {
		c.logger.Error("rtc: create peer connection", "peer", peer, "err", err)
		return nil
	}
	sess := &peerSession{
		peer:     peer,
		polite:   run.self < peer,
		in:       make(chan *ss.SignallingEvent, sessionChannelBuffer),
		out:      make(chan *ss.SignallingEvent, sessionChannelBuffer),
		pc:       pc,
		channels: make(map[string]*webrtc.DataChannel),
	}
	sess.ctx, sess.cancel = context.WithCancel(run.ctx)
	c.attachPeerConnection(sess, pc)
	nego, err := c.newNegotiator(pc, PerfectNegotiatorOptions{
		ChannelId:      c.channelId,
		SelfSubscriber: run.self,
		PeerSubscriber: peer,
		// On a glare the negotiator rebuilds the peer connection through
		// this factory: the swap is hub state, so it is posted as a note;
		// the negotiator's goroutine waits for the reply.
		NewPeerConnection: func() (*webrtc.PeerConnection, error) {
			reply := make(chan rebuildReply, 1)
			select {
			case c.serviceChan <- rebuildNote{sess: sess, reply: reply}:
			case <-sess.ctx.Done():
				return nil, sess.ctx.Err()
			}
			r := <-reply
			return r.pc, r.err
		},
		Logger: c.logger,
	})
	if err != nil {
		_ = pc.Close()
		c.logger.Error("rtc: create negotiator", "peer", peer, "err", err)
		return nil
	}
	run.sessions[peer] = sess
	go sess.run(nego, run.out, c.logger)

	// The polite side creates a channel per handled label (the first
	// creation starts the negotiation); the impolite side's channels
	// arrive via OnDataChannel.
	if sess.polite {
		labels := make([]string, 0, len(c.handlers))
		for label := range c.handlers {
			labels = append(labels, label)
		}
		slices.Sort(labels) // deterministic channel creation order
		for _, label := range labels {
			dc, err := pc.CreateDataChannel(label, nil)
			if err != nil {
				c.logger.Error("rtc: create data channel", "peer", peer, "label", label, "err", err)
				continue
			}
			sess.channels[label] = dc
			c.dispatchDataChannel(sess, dc, c.handlers[label])
		}
	}
	return sess
}

// closeSession tears down one session of the run: the negotiator's
// context is canceled and the peer connection closed.
func (c *HeadlessRTCClient) closeSession(run *clientRun, sess *peerSession) {
	delete(run.sessions, sess.peer)
	sess.cancel()
	if err := sess.pc.Close(); err != nil {
		c.logger.Warn("rtc: close peer connection", "peer", sess.peer, "err", err)
	}
}

// handleDataChannelNote registers a data channel handler: stored in the
// (run-independent) handler table, then applied to every existing session
// — an already-present channel of the label is delivered, a missing one
// is created on the polite side — and the ack answered.
func (c *HeadlessRTCClient) handleDataChannelNote(run *clientRun, n handleNote) {
	if _, dup := c.handlers[n.label]; dup {
		n.ack <- fmt.Errorf("rtc: multiple registrations for data channel label %s", n.label)
		return
	}
	c.handlers[n.label] = n.handler
	if run != nil {
		for _, sess := range run.sessions {
			if dc, ok := sess.channels[n.label]; ok {
				c.dispatchDataChannel(sess, dc, n.handler)
				continue
			}
			if !sess.polite {
				continue // the impolite side's channel arrives via OnDataChannel
			}
			dc, err := sess.pc.CreateDataChannel(n.label, nil)
			if err != nil {
				c.logger.Error("rtc: create data channel", "peer", sess.peer, "label", n.label, "err", err)
				continue
			}
			sess.channels[n.label] = dc
			c.dispatchDataChannel(sess, dc, n.handler)
		}
	}
	n.ack <- nil
}

// handleAnnouncedNote records a data channel announced by the peer and
// dispatches it to its label's handler, if one is registered; otherwise
// the channel is kept and delivered when a handler is registered later.
func (c *HeadlessRTCClient) handleAnnouncedNote(run *clientRun, n announcedNote) {
	if run == nil || run.sessions[n.sess.peer] != n.sess {
		return // a late event of a torn-down session
	}
	label := n.dc.Label()
	n.sess.channels[label] = n.dc
	handler := c.handlers[label]
	if handler == nil {
		c.logger.Debug("rtc: no handler for announced data channel", "peer", n.sess.peer, "label", label)
		return
	}
	c.dispatchDataChannel(n.sess, n.dc, handler)
}

// handleTrackHandlerNote registers the remote-media-track handler,
// rejecting a duplicate registration.
func (c *HeadlessRTCClient) handleTrackHandlerNote(n trackHandlerNote) {
	if c.trackHandler != nil {
		n.ack <- errors.New("rtc: multiple track handler registrations")
		return
	}
	c.trackHandler = n.handler
	n.ack <- nil
}

// handleAddTrackNote attaches a local media track to the session's peer
// connection and records it, so a glare rebuild can re-add it to the
// rebuilt connection. Adding fires the peer connection's
// OnNegotiationNeeded — the session's negotiator renegotiates on its own.
func (c *HeadlessRTCClient) handleAddTrackNote(run *clientRun, n addTrackNote) {
	if run == nil {
		n.reply <- addTrackReply{err: ErrNoPeerSession}
		return
	}
	sess := run.sessions[n.peer]
	if sess == nil {
		n.reply <- addTrackReply{err: ErrNoPeerSession}
		return
	}
	for _, st := range sess.tracks {
		if st.track == n.track {
			// Already attached: a no-op returning the current sender.
			n.reply <- addTrackReply{sender: st.sender}
			return
		}
	}
	sender, err := sess.pc.AddTrack(n.track)
	if err != nil {
		n.reply <- addTrackReply{err: err}
		return
	}
	sess.tracks = append(sess.tracks, sessionTrack{track: n.track, sender: sender})
	n.reply <- addTrackReply{sender: sender}
}

// handleRemoveTrackNote detaches a local media track from the session's
// peer connection; a no-op when the session or the track is already gone.
func (c *HeadlessRTCClient) handleRemoveTrackNote(run *clientRun, n removeTrackNote) {
	if run == nil {
		n.reply <- nil
		return
	}
	sess := run.sessions[n.peer]
	if sess == nil {
		n.reply <- nil
		return
	}
	for i, st := range sess.tracks {
		if st.track == n.track {
			var err error
			if sess.pc.SignalingState() != webrtc.SignalingStateClosed {
				err = sess.pc.RemoveTrack(st.sender)
			}
			if err != nil {
				n.reply <- err
				return
			}
			sess.tracks = slices.Delete(sess.tracks, i, i+1)
			n.reply <- nil
			return
		}
	}
	n.reply <- nil // never attached: the desired end state
}

// handleTrackNote dispatches a remote media track arriving on a session to
// the registered track handler, if any.
func (c *HeadlessRTCClient) handleTrackNote(run *clientRun, n trackNote) {
	if run == nil || run.sessions[n.sess.peer] != n.sess {
		return // a late event of a torn-down session
	}
	handler := c.trackHandler
	if handler == nil {
		c.logger.Debug("rtc: remote track with no track handler", "peer", n.sess.peer, "kind", n.track.Kind())
		return
	}
	c.dispatchTrack(n.sess, n.track, n.receiver, handler)
}

// handleRebuildNote is the hub side of a session's glare yield: a fresh
// peer connection, re-wired and re-stocked with the session's channels —
// every known label is re-created locally (the peer's own channels died
// with the replaced connection either way), the label handlers are
// re-invoked with the fresh channels, and the session's local media tracks
// are re-added (pion cannot roll back; the browser's implicit rollback
// preserves its transceivers, so the rebuild must). The session's state is
// swapped only once the fresh connection is fully stocked, so a failure
// leaves the session untouched. See PerfectNegotiatorOptions.NewPeerConnection.
func (c *HeadlessRTCClient) handleRebuildNote(run *clientRun, n rebuildNote) {
	if run == nil || run.sessions[n.sess.peer] != n.sess {
		n.reply <- rebuildReply{err: errPeerSessionGone}
		return
	}
	fresh, err := c.newPC()
	if err != nil {
		n.reply <- rebuildReply{err: err}
		return
	}
	c.attachPeerConnection(n.sess, fresh)
	type pending struct {
		dc      *webrtc.DataChannel
		handler DataChannelHandler
	}
	var toDispatch []pending
	channels := make(map[string]*webrtc.DataChannel, len(n.sess.channels))
	for label := range n.sess.channels {
		dc, err := fresh.CreateDataChannel(label, nil)
		if err != nil {
			_ = fresh.Close()
			n.reply <- rebuildReply{err: fmt.Errorf("rtc: re-create data channel %q: %w", label, err)}
			return
		}
		channels[label] = dc
		if handler := c.handlers[label]; handler != nil {
			toDispatch = append(toDispatch, pending{dc, handler})
		}
	}
	// The tracks, re-added before the swap completes: the negotiator
	// answers the colliding offer on the fresh connection, and the answer
	// must carry them.
	tracks := make([]sessionTrack, 0, len(n.sess.tracks))
	for _, st := range n.sess.tracks {
		sender, err := fresh.AddTrack(st.track)
		if err != nil {
			_ = fresh.Close()
			n.reply <- rebuildReply{err: fmt.Errorf("rtc: re-add track %q: %w", st.track.ID(), err)}
			return
		}
		tracks = append(tracks, sessionTrack{track: st.track, sender: sender})
	}
	n.sess.pc = fresh
	n.sess.channels = channels
	n.sess.tracks = tracks
	n.reply <- rebuildReply{pc: fresh}
	for _, p := range toDispatch {
		c.dispatchDataChannel(n.sess, p.dc, p.handler)
	}
}

// attachPeerConnection wires the client's own pion handlers on pc —
// distinct from the negotiator's (OnNegotiationNeeded, OnICECandidate):
// every data channel the peer announces is reported to the hub, and so is
// every remote media track arriving mid-session.
func (c *HeadlessRTCClient) attachPeerConnection(sess *peerSession, pc *webrtc.PeerConnection) {
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		select {
		case c.serviceChan <- announcedNote{sess: sess, dc: dc}:
		case <-sess.ctx.Done():
		}
	})
	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		select {
		case c.serviceChan <- trackNote{sess: sess, track: track, receiver: receiver}:
		case <-sess.ctx.Done():
		}
	})
}

// dispatchDataChannel invokes a data channel handler on its own goroutine,
// recovering panics — user code must not be able to stall or crash the
// client.
func (c *HeadlessRTCClient) dispatchDataChannel(sess *peerSession, dc *webrtc.DataChannel, handler DataChannelHandler) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				c.logger.Error("rtc: data channel handler panicked",
					"peer", sess.peer, "label", dc.Label(), "err", r)
			}
		}()
		handler.ServeDataChannel(sess.ctx, c.channelId, sess.peer, dc)
	}()
}

// dispatchTrack invokes the track handler on its own goroutine, recovering
// panics — like dispatchDataChannel, user code must not be able to stall
// or crash the client.
func (c *HeadlessRTCClient) dispatchTrack(sess *peerSession, track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver, handler TrackHandler) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				c.logger.Error("rtc: track handler panicked", "peer", sess.peer, "err", r)
			}
		}()
		handler.ServeTrack(sess.ctx, c.channelId, sess.peer, track, receiver)
	}()
}

// sendKeepAlive renews the client's channel membership. The SS answers
// nothing on success; an error reply arrives uncorrelated and is logged
// by the hub.
func (c *HeadlessRTCClient) sendKeepAlive(run *clientRun) {
	c.sendToSS(run, &ss.ClientToSSEv{ChannelKeepAlive: &ss.ClientToSSChannelKeepAlive{
		ChannelId:    c.channelId,
		SubscriberId: run.self,
	}})
}

// sendToSS wraps a c2s payload into the SS envelope — addressed to the
// well-known SS service, From left empty for the server-side transport to
// populate from the session — and queues it for sending. It reports false
// when the run's context is done first.
func (c *HeadlessRTCClient) sendToSS(run *clientRun, payload *ss.ClientToSSEv) (ss.MsgId, bool) {
	msgId := newMsgId()
	ev := &ss.SignallingEvent{
		To:    ss.EPAddr{ServiceId: ss.WellKnownSvcIdSS},
		MsgId: msgId,
		C2SEv: payload,
	}
	select {
	case run.out <- ev:
		return msgId, true
	case <-run.ctx.Done():
		return msgId, false
	}
}

// peerSession is one peer connection, its data channels by label, and the
// negotiation driving it — the counterpart of the frontend's PeerSession.
type peerSession struct {
	peer ss.SubscriberId
	// polite mirrors the negotiator's role derivation: the
	// lexicographically smaller subscriber id creates the data channels.
	polite bool

	// ctx ends when the session does (the peer dropped out, or the run
	// ended); data channel handlers receive it.
	ctx    context.Context
	cancel context.CancelFunc

	in  chan *ss.SignallingEvent // hub → session (drained eagerly by pumpIn)
	out chan *ss.SignallingEvent // negotiator → session → transport

	// Owned by the hub goroutine (pc is swapped by the glare rebuild);
	// never touched elsewhere.
	pc       *webrtc.PeerConnection
	channels map[string]*webrtc.DataChannel
	tracks   []sessionTrack
}

// sessionTrack is one local media track attached to the session's peer
// connection, with its current sender — refreshed when a glare rebuild
// re-adds the track to the rebuilt connection, so the session's record
// never holds a stale sender.
type sessionTrack struct {
	track  webrtc.TrackLocal
	sender *webrtc.RTPSender
}

// run drives the session's negotiation until the session's context ends.
// The session's channels are never closed by the hub — teardown cancels
// the context instead.
func (sess *peerSession) run(nego Negotiator, sink chan<- *ss.SignallingEvent, logger *slog.Logger) {
	negoIn := make(chan *ss.SignallingEvent)
	go sess.pumpIn(negoIn)
	go sess.pumpOut(sink)
	// The negotiator sends on sess.out only from within Negotiate, so
	// closing it after Negotiate returned is race-free.
	err := nego.Negotiate(sess.ctx, negoIn, sess.out)
	close(sess.out)
	if err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("rtc: negotiator ended", "peer", sess.peer, "err", err)
	}
}

// pumpIn moves events from the session's hub-facing channel to the
// negotiator through an unbounded queue (the nil-channel idiom: the
// output case is disabled while the queue is empty). The hub — and
// therefore every other session — is never blocked by a negotiator that
// is busy, e.g. rebuilding the peer connection through the hub on a
// glare: such a cycle would deadlock.
func (sess *peerSession) pumpIn(negoIn chan<- *ss.SignallingEvent) {
	var queue []*ss.SignallingEvent
	for {
		// Terminate promptly on teardown, even with a backed-up queue.
		select {
		case <-sess.ctx.Done():
			return
		default:
		}
		var out chan<- *ss.SignallingEvent
		var first *ss.SignallingEvent
		if len(queue) > 0 {
			out = negoIn
			first = queue[0]
		}
		select {
		case ev := <-sess.in:
			queue = append(queue, ev)
		case out <- first:
			queue = queue[1:]
		case <-sess.ctx.Done():
			return
		}
	}
}

// pumpOut funnels the negotiator's outbound events to the run's outbound
// channel, applying its backpressure to the negotiator alone.
func (sess *peerSession) pumpOut(sink chan<- *ss.SignallingEvent) {
	for ev := range sess.out {
		select {
		case sink <- ev:
		case <-sess.ctx.Done():
			return
		}
	}
}
