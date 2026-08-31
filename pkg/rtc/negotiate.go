// Package rtc implements WebRTC session negotiation over the signalling
// server (SS), mirroring the browser client's negotiator in
// web/site/src/api/ss/negotiate.ts.
//
// Negotiator is the contract; PerfectNegotiator implements the MDN
// "perfect negotiation" pattern
// (https://developer.mozilla.org/en-US/docs/Web/API/WebRTC_API/Perfect_negotiation)
// on top of the SS's client-to-client relay (`c2CEv`), using
// github.com/pion/webrtc/v4. The SS is a dumb relay for this traffic: it
// never looks at session descriptions or ICE candidates, it just passes
// c2c events between subscribers.
//
// Where the browser negotiator reads a ReadableStream and writes a
// WritableStream, Negotiate receives inbound events from a Go channel
// and sends outbound ones to a Go channel; a transport (e.g. the
// WebSocket handler) is expected to provide both, one negotiator per
// peer.
//
// Two adaptations of the browser pattern are pion-specific and worth
// spelling out:
//
//   - pion fires its callbacks on its own goroutines, so the callbacks
//     only funnel work into the single Negotiate loop goroutine, which
//     owns all perfect-negotiation state — the Go rendition of the
//     browser's single-threaded event loop (CSP, like
//     SimpleOnMemorySSProvider).
//   - pion cannot roll a pending local offer back: unlike the browser,
//     where setRemoteDescription(offer) implicitly rolls the polite
//     peer's own offer back on a glare, pion rejects every route back
//     to the stable state (SetLocalDescription(SDPTypeRollback) is
//     unreachable — the empty-SDP pre-check rejects the type, and the
//     signaling state machine has no have-local-offer → rollback →
//     stable transition), and a peer stuck in have-local-offer can
//     apply nothing but the answer to its own offer. Yielding to a
//     colliding offer therefore means REBUILDING the peer connection
//     via PerfectNegotiatorOptions.NewPeerConnection and answering on
//     the fresh one; without a factory the polite peer cannot yield
//     and the pair deadlocks on glare.
//
// HeadlessRTCClient (client.go) composes the pieces into a complete
// headless peer — a bot: given a pair of signalling channels (the
// transport's business, e.g. WebSocketSignallingTransport in
// transport.go), it registers as a channel subscriber, keeps the
// registration alive, discovers the channel's members, and runs one peer
// session — a pion peer connection plus a Negotiator — per member,
// dispatching the sessions' data channels by label to the handlers
// registered with HandleDataChannel. It is the Go counterpart of the
// browser's useSignalling + usePeerSessions hooks.
package rtc

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/pion/webrtc/v4"

	"personal-site/pkg/models/ss"
)

// Negotiator drives the WebRTC offer/answer + ICE exchange between the
// local peer and one remote peer, using the signalling server as a
// relay: it receives inbound signalling events from ssEvIn and sends
// outbound ones to ssEvOut. It is the channel-based counterpart of the
// frontend RTCNegotiator.
type Negotiator interface {
	// Negotiate runs the negotiation loop. It returns nil when ssEvIn
	// is closed and ctx.Err() when ctx is done; errors in handling a
	// single event are logged and swallowed, mirroring the frontend's
	// console.error catch-alls, so the loop only ends via ctx or the
	// input channel. It also returns ErrAlreadyNegotiating when called
	// while a previous run is still in flight.
	//
	// The channels stay owned by the caller: Negotiate only receives
	// from ssEvIn and sends to ssEvOut, and never closes either.
	// Outbound sends honor ctx cancellation, so a stopped consumer
	// cannot wedge the loop.
	Negotiate(ctx context.Context, ssEvIn <-chan *ss.SignallingEvent, ssEvOut chan<- *ss.SignallingEvent) error
}

var (
	// ErrNilPeerConnection is returned by NewPerfectNegotiator when the
	// peer connection is nil.
	ErrNilPeerConnection = errors.New("rtc: nil PeerConnection")

	// ErrSelfNegotiation is returned by NewPerfectNegotiator when the
	// self and peer subscriber ids are equal.
	ErrSelfNegotiation = errors.New("rtc: cannot negotiate with oneself")

	// ErrAlreadyNegotiating is returned by Negotiate when a previous
	// Negotiate call on the same instance is still running.
	ErrAlreadyNegotiating = errors.New("rtc: Negotiate is already running")
)

// PerfectNegotiatorOptions configures a PerfectNegotiator.
type PerfectNegotiatorOptions struct {
	// ChannelId is the channel both subscribers are registered in. Only
	// c2c events in this channel are negotiation traffic, and outbound
	// events are scoped to it.
	ChannelId ss.ChannelId

	// SelfSubscriber is own subscriber id on the signalling channel.
	SelfSubscriber ss.SubscriberId

	// PeerSubscriber is the subscriber id of the remote peer to
	// negotiate with.
	PeerSubscriber ss.SubscriberId

	// NewPeerConnection, when set, lets the polite peer yield to a
	// colliding offer by rebuilding the local peer connection: the
	// browser rolls its pending offer back implicitly, pion cannot (see
	// the package comment), so the factory must return a fresh peer
	// connection with every track, data channel, and handler the
	// current one has, re-registered. The negotiator closes the old
	// connection, adopts the fresh one, and answers the colliding offer
	// on it. The cost versus the browser is a connection reset whenever
	// this peer yields during an established connection (glare while
	// renegotiating); a glare during session establishment is
	// unnoticeable, as the connection was not up yet anyway.
	//
	// When nil, the polite peer cannot yield: it logs and ignores the
	// colliding offer exactly like the impolite peer would, and the
	// negotiation deadlocks until one side restarts.
	NewPeerConnection func() (*webrtc.PeerConnection, error)

	// Logger receives the negotiation's error diagnostics, mirroring
	// the frontend's console.error calls; nil selects slog's default
	// logger.
	Logger *slog.Logger
}

// PerfectNegotiator implements the perfect negotiation pattern: the same
// code runs on both ends of a connection, caller or callee, and the only
// asymmetry is the polite/impolite role, which resolves offer collisions
// ("glare") without deadlocking. The role is derived deterministically
// from the two subscriber ids — the lexicographically smaller id is
// polite — so both ends agree on the roles without any extra signalling.
// Roles are per pair of peers: one client may be polite towards one peer
// and impolite towards another.
//
// Only c2c events in options.ChannelId from options.PeerSubscriber
// addressed to options.SelfSubscriber are negotiation traffic; every
// other event on the input channel is ignored.
//
// Don't re-use the same negotiator instance when at least one subscriber
// id has changed.
type PerfectNegotiator struct {
	pc      *webrtc.PeerConnection
	options PerfectNegotiatorOptions
	logger  *slog.Logger

	// Perfect-negotiation state. It is owned by the Negotiate loop
	// goroutine alone (see localEvent), so it needs no mutex.
	// makingOffer is tracked in a boolean rather than derived from the
	// signaling state because the signaling state changes
	// asynchronously and would race with an incoming offer in the
	// browser version of the pattern.
	makingOffer                  bool
	ignoreOffer                  bool
	isSettingRemoteAnswerPending bool

	// Polite reports whether this peer is the polite one — the
	// lexicographically smaller of the two subscriber ids. Both ends
	// compute this locally from the same pair of ids, so they
	// deterministically land on opposite roles.
	Polite bool

	// running makes Negotiate single-flight, mirroring the frontend's
	// "already running" guard.
	running atomic.Bool

	// done is closed when the running Negotiate returns; the pion
	// callbacks select on it so a blocked hand-off cannot outlive the
	// loop. It is created in the constructor (so callbacks attached
	// before the first run never abort) and re-created per run.
	done chan struct{}

	// local carries the pion callbacks' work to the Negotiate loop; see
	// localEvent.
	local chan localEvent
}

// localEventBuffer bounds how many callback deliveries may pile up
// while the loop is busy; far more than a gathering round can produce.
const localEventBuffer = 64

// localEvent is one piece of work a pion callback hands to the
// Negotiate loop: either a pending negotiation-needed signal or one
// locally gathered ICE candidate. Funneling both through a single
// channel keeps every access of the perfect-negotiation state and the
// PeerConnection on the loop goroutine, so the pattern's state machine
// stays as single-threaded as its browser original.
type localEvent struct {
	negotiationNeeded bool
	candidate         *webrtc.ICECandidate
}

// NewPerfectNegotiator constructs a PerfectNegotiator negotiating on pc
// between options.SelfSubscriber and options.PeerSubscriber.
//
// The caller keeps ownership of pc — tracks, OnTrack, data channels,
// closing, etc. PerfectNegotiator only takes over OnNegotiationNeeded
// and OnICECandidate, and it does so HERE, in the constructor, not in
// Negotiate: pion drops a negotiation-needed event fired while no
// handler is attached (the browser re-fires it on the next task), so
// attaching in Negotiate would lose every pc change made between
// construction and the loop's start. Construct the negotiator before
// making any change to pc that triggers negotiation, and start
// Negotiate promptly — before the first run, callback deliveries
// buffer up (up to localEventBuffer of them).
//
// The one ownership exception: when options.NewPeerConnection is set,
// ownership is shared — on a glare the negotiator may replace pc via
// the factory and close the replaced connection (see
// PerfectNegotiatorOptions.NewPeerConnection).
func NewPerfectNegotiator(pc *webrtc.PeerConnection, options PerfectNegotiatorOptions) (*PerfectNegotiator, error) {
	if pc == nil {
		return nil, ErrNilPeerConnection
	}
	// Subscriber ids are unique within a channel (the SS rejects
	// duplicate registrations), so the comparison never ties; equal ids
	// here mean a programming error at the call site.
	if options.SelfSubscriber == options.PeerSubscriber {
		return nil, ErrSelfNegotiation
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	n := &PerfectNegotiator{
		pc:      pc,
		options: options,
		logger:  logger,
		Polite:  options.SelfSubscriber < options.PeerSubscriber,
		local:   make(chan localEvent, localEventBuffer),
		done:    make(chan struct{}),
	}
	n.attach(pc)
	return n, nil
}

// Negotiate implements Negotiator.
func (n *PerfectNegotiator) Negotiate(ctx context.Context, ssEvIn <-chan *ss.SignallingEvent, ssEvOut chan<- *ss.SignallingEvent) error {
	if !n.running.CompareAndSwap(false, true) {
		return ErrAlreadyNegotiating
	}
	defer n.running.Store(false)

	// Fresh done for this run, and re-attach the pion callbacks: a
	// previous run detached them when it returned, and they funnel
	// through this run's done now.
	n.done = make(chan struct{})
	n.attach(n.pc)
	defer func() {
		n.pc.OnNegotiationNeeded(nil)
		n.pc.OnICECandidate(nil)
		close(n.done)
	}()

	// Incoming half: consume signalling events until the input channel
	// closes or the caller cancels.
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-ssEvIn:
			if !ok {
				return nil
			}
			n.handle(ctx, ev, ssEvOut)
		case le := <-n.local:
			n.handleLocal(ctx, le, ssEvOut)
		}
	}
}

// attach wires the negotiator's pion callbacks on pc: both only
// funnel work into the Negotiate loop, aborting once done closes. The
// done channel is captured by value — pion fires the callbacks on its
// own goroutines, which must not read the (reassigned per run) field.
func (n *PerfectNegotiator) attach(pc *webrtc.PeerConnection) {
	done := n.done
	pc.OnNegotiationNeeded(func() {
		select {
		case n.local <- localEvent{negotiationNeeded: true}:
		case <-done:
		}
	})
	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		select {
		case n.local <- localEvent{candidate: candidate}:
		case <-done:
		}
	})
}

// handleLocal runs one piece of work handed over by a pion callback.
func (n *PerfectNegotiator) handleLocal(ctx context.Context, le localEvent, ssEvOut chan<- *ss.SignallingEvent) {
	switch {
	case le.negotiationNeeded:
		n.makeOffer(ctx, ssEvOut)
	case le.candidate != nil:
		// ToJSON yields exactly the browser-compatible
		// ICECandidateInit wire shape.
		candidate := le.candidate.ToJSON()
		n.sendToPeer(ctx, ssEvOut, &ss.ClientToClientEv{RTCICECandidate: &candidate})
	default:
		// A nil candidate means end-of-candidates; on the wire it is an
		// empty-candidate init, which AddICECandidate treats the same.
		n.sendToPeer(ctx, ssEvOut, &ss.ClientToClientEv{
			RTCICECandidate: &webrtc.ICECandidateInit{},
		})
	}
}

// makeOffer is the negotiation-needed path: create an offer, set it
// locally, and push it to the peer.
func (n *PerfectNegotiator) makeOffer(ctx context.Context, ssEvOut chan<- *ss.SignallingEvent) {
	// pion fires negotiation-needed only from the stable state, but the
	// event is processed asynchronously through the loop: an incoming
	// offer may have been applied in the meantime. Offering outside the
	// stable state would corrupt the exchange, and pion re-fires
	// negotiation-needed once the state returns to stable, so skipping
	// here is safe.
	if n.pc.SignalingState() != webrtc.SignalingStateStable {
		return
	}
	n.makingOffer = true
	defer func() { n.makingOffer = false }()
	offer, err := n.pc.CreateOffer(nil)
	if err != nil {
		n.logger.Error("rtc: create offer", "err", err)
		return
	}
	if err := n.pc.SetLocalDescription(offer); err != nil {
		n.logger.Error("rtc: set local description", "err", err)
		return
	}
	local := n.pc.LocalDescription()
	if local == nil {
		return
	}
	n.sendToPeer(ctx, ssEvOut, &ss.ClientToClientEv{SessionDesc: local})
}

// handle handles one inbound signalling event; non-c2c or foreign-peer
// events are ignored.
func (n *PerfectNegotiator) handle(ctx context.Context, ev *ss.SignallingEvent, ssEvOut chan<- *ss.SignallingEvent) {
	c2c := ev.C2CEv
	if c2c == nil ||
		c2c.ChannelId != n.options.ChannelId ||
		c2c.FromSubscriber != n.options.PeerSubscriber ||
		c2c.ToSubscriber != n.options.SelfSubscriber {
		return
	}
	if c2c.SessionDesc != nil {
		n.handleSessionDesc(ctx, c2c.SessionDesc, ssEvOut)
	} else if c2c.RTCICECandidate != nil {
		n.handleICECandidate(c2c.RTCICECandidate)
	}
}

// handleSessionDesc applies an inbound session description, answering
// offers and resolving offer collisions per the perfect negotiation
// pattern.
func (n *PerfectNegotiator) handleSessionDesc(ctx context.Context, desc *webrtc.SessionDescription, ssEvOut chan<- *ss.SignallingEvent) {
	readyForOffer := !n.makingOffer &&
		(n.pc.SignalingState() == webrtc.SignalingStateStable || n.isSettingRemoteAnswerPending)
	offerCollision := desc.Type == webrtc.SDPTypeOffer && !readyForOffer

	n.ignoreOffer = !n.Polite && offerCollision
	if n.ignoreOffer {
		return
	}

	// The polite peer yields to the colliding offer. In the browser this
	// is where setRemoteDescription(offer) implicitly rolls the pending
	// local offer back; pion cannot roll back (see the package comment),
	// so a pending local offer is abandoned by rebuilding the peer
	// connection instead. (When the local offer is still being created
	// — the makingOffer window — nothing has been set locally yet and no
	// rebuild is needed; its later SetLocalDescription simply fails.)
	if offerCollision && n.pc.SignalingState() == webrtc.SignalingStateHaveLocalOffer {
		if err := n.rebuildPeerConnection(); err != nil {
			n.logger.Error("rtc: yield to colliding offer", "err", err)
			n.ignoreOffer = true
			return
		}
	}

	n.isSettingRemoteAnswerPending = desc.Type == webrtc.SDPTypeAnswer
	err := n.pc.SetRemoteDescription(*desc)
	n.isSettingRemoteAnswerPending = false
	if err != nil {
		n.logger.Error("rtc: set remote description", "err", err)
		return
	}

	if desc.Type == webrtc.SDPTypeOffer {
		answer, err := n.pc.CreateAnswer(nil)
		if err != nil {
			n.logger.Error("rtc: create answer", "err", err)
			return
		}
		if err := n.pc.SetLocalDescription(answer); err != nil {
			n.logger.Error("rtc: set local description", "err", err)
			return
		}
		local := n.pc.LocalDescription()
		if local == nil {
			return
		}
		n.sendToPeer(ctx, ssEvOut, &ss.ClientToClientEv{SessionDesc: local})
	}
}

// handleICECandidate applies one inbound trickle ICE candidate.
func (n *PerfectNegotiator) handleICECandidate(candidate *webrtc.ICECandidateInit) {
	if err := n.pc.AddICECandidate(*candidate); err != nil {
		// Candidates belonging to an offer we chose to ignore are
		// expected to fail; everything else is a real error.
		if !n.ignoreOffer {
			n.logger.Error("rtc: add ice candidate", "err", err)
		}
	}
}

// rebuildPeerConnection swaps the local peer connection for a fresh
// one from options.NewPeerConnection — pion's stand-in for the
// browser's implicit rollback on a glare. The old connection's
// negotiator handlers are detached, the fresh one's are attached, and
// the old connection is closed; the factory must have re-registered
// every track, data channel, and handler on the fresh connection.
// ICE candidates of the colliding offer are not lost: they can only
// arrive after it, by which time the swap has happened.
func (n *PerfectNegotiator) rebuildPeerConnection() error {
	if n.options.NewPeerConnection == nil {
		return errNoPeerConnectionFactory
	}
	fresh, err := n.options.NewPeerConnection()
	if err != nil {
		return err
	}
	old := n.pc
	n.pc = fresh
	n.makingOffer = false
	n.ignoreOffer = false
	n.isSettingRemoteAnswerPending = false
	old.OnNegotiationNeeded(nil)
	old.OnICECandidate(nil)
	n.attach(fresh)
	if err := old.Close(); err != nil {
		n.logger.Warn("rtc: close replaced peer connection", "err", err)
	}
	return nil
}

// errNoPeerConnectionFactory explains a polite peer that cannot yield.
var errNoPeerConnectionFactory = errors.New(
	"rtc: no NewPeerConnection factory configured; the polite peer cannot yield to a colliding offer")

// sendToPeer wraps a c2c payload (a session description or an ICE
// candidate) into the SS envelope and queues it for sending, mirroring
// the frontend exactly.
func (n *PerfectNegotiator) sendToPeer(ctx context.Context, ssEvOut chan<- *ss.SignallingEvent, payload *ss.ClientToClientEv) {
	ev := &ss.SignallingEvent{
		// From is left empty: the server-side transport populates it
		// from the authenticated session (cookie / JWT).
		From: ss.EPAddr{},
		// c2c events are addressed to the SS; it rewrites To to the
		// address ToSubscriber resolves to before relaying.
		To:    ss.EPAddr{ServiceId: ss.WellKnownSvcIdSS},
		MsgId: newMsgId(),
		C2CEv: &ss.ClientToClientEv{
			FromSubscriber:  n.options.SelfSubscriber,
			ToSubscriber:    n.options.PeerSubscriber,
			ChannelId:       n.options.ChannelId,
			SessionDesc:     payload.SessionDesc,
			RTCICECandidate: payload.RTCICECandidate,
		},
	}
	select {
	case ssEvOut <- ev:
	case <-ctx.Done():
	}
}

// newMsgId generates a message id uniquely, statelessly, independently,
// mirroring the frontend's crypto.randomUUID and the SS's server side.
func newMsgId() ss.MsgId {
	return ss.MsgId(uuid.NewString())
}

var _ Negotiator = (*PerfectNegotiator)(nil)
