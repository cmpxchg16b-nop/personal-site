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
	"fmt"
	"log/slog"
	"math/rand"
	"strings"
	"sync/atomic"
	"time"

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

	// ICE recovery is owned by the Negotiate loop too. The timer channel is nil
	// while no restart is scheduled, disabling that select case.
	iceConnectedOnce   bool
	iceRecovering      bool
	iceRestartPending  bool
	iceRestartAttempts int
	iceRestartTimer    *time.Timer
	iceRestartTimerC   <-chan time.Time

	// These are fields rather than package constants so tests can shorten the
	// waits and make jitter deterministic without changing production defaults.
	iceDisconnectGrace time.Duration
	iceRetryBase       time.Duration
	iceRetryMax        time.Duration
	iceRandom          func() float64
	// iceStateReader follows n.pc across rebuilds. Tests replace it to model a
	// transport failure while driving the real negotiation and offer path.
	iceStateReader func() webrtc.ICEConnectionState
}

const (
	// localEventBuffer bounds how many callback deliveries may pile up while
	// the loop is busy; far more than a gathering round can produce.
	localEventBuffer = 64

	defaultICEDisconnectGrace = 3 * time.Second
	defaultICERetryBase       = 2 * time.Second
	defaultICERetryMax        = 30 * time.Second
)

// localEvent is one piece of work a pion callback hands to the Negotiate
// loop: negotiation-needed, a gathered candidate, or an ICE state change.
// source identifies the originating connection, so any queued callback from a
// connection replaced during glare cannot affect its replacement.
type localEvent struct {
	negotiationNeeded bool
	candidate         *webrtc.ICECandidate
	source            *webrtc.PeerConnection
	iceState          *webrtc.ICEConnectionState
}

// NewPerfectNegotiator constructs a PerfectNegotiator negotiating on pc
// between options.SelfSubscriber and options.PeerSubscriber.
//
// The caller keeps ownership of pc — tracks, OnTrack, data channels,
// closing, etc. PerfectNegotiator takes over OnNegotiationNeeded,
// OnICECandidate, and OnICEConnectionStateChange, and it does so HERE, in the
// constructor, not in Negotiate: pion drops a negotiation-needed event fired while no
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
		pc:                 pc,
		options:            options,
		logger:             logger,
		Polite:             options.SelfSubscriber < options.PeerSubscriber,
		local:              make(chan localEvent, localEventBuffer),
		done:               make(chan struct{}),
		iceDisconnectGrace: defaultICEDisconnectGrace,
		iceRetryBase:       defaultICERetryBase,
		iceRetryMax:        defaultICERetryMax,
		iceRandom:          rand.Float64,
	}
	n.iceStateReader = func() webrtc.ICEConnectionState {
		return n.pc.ICEConnectionState()
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
	n.stopICERestartTimer()
	n.iceConnectedOnce = false
	n.iceRecovering = false
	n.iceRestartPending = false
	n.iceRestartAttempts = 0
	n.handleICEConnectionState(n.currentICEState())
	defer func() {
		n.pc.OnNegotiationNeeded(nil)
		n.pc.OnICECandidate(nil)
		n.pc.OnICEConnectionStateChange(nil)
		n.stopICERestartTimer()
		n.iceRecovering = false
		n.iceRestartPending = false
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
		case <-n.iceRestartTimerC:
			n.handleICERestartTimer(ctx, ssEvOut)
		}
	}
}

// attach wires the negotiator's pion callbacks on pc: all funnel work into
// the Negotiate loop, aborting once done closes. The done channel and pc are
// captured by value — pion fires callbacks on its own goroutines, which must
// not read the (reassigned per run) done field or confuse an old connection's
// ICE state with the current one's.
func (n *PerfectNegotiator) attach(pc *webrtc.PeerConnection) {
	done := n.done
	pc.OnNegotiationNeeded(func() {
		select {
		case n.local <- localEvent{source: pc, negotiationNeeded: true}:
		case <-done:
		}
	})
	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		select {
		case n.local <- localEvent{source: pc, candidate: candidate}:
		case <-done:
		}
	})
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		select {
		case n.local <- localEvent{source: pc, iceState: &state}:
		case <-done:
		}
	})
}

// handleLocal runs one piece of work handed over by a pion callback.
func (n *PerfectNegotiator) handleLocal(ctx context.Context, le localEvent, ssEvOut chan<- *ss.SignallingEvent) {
	if le.source != nil && le.source != n.pc {
		return
	}
	switch {
	case le.iceState != nil:
		n.handleICEConnectionState(*le.iceState)
	case le.negotiationNeeded:
		// A media/data-channel renegotiation can satisfy a pending ICE restart;
		// there is no reason to send a second restart offer immediately after it.
		n.makeOffer(ctx, ssEvOut, n.iceRestartPending)
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

// makeOffer creates an offer, sets it locally, and pushes it to the peer.
// iceRestart requests fresh ICE credentials. Its bool reports whether an offer
// reached the wire; a pending ICE restart stays latched until that happens.
func (n *PerfectNegotiator) makeOffer(ctx context.Context, ssEvOut chan<- *ss.SignallingEvent, iceRestart bool) bool {
	// pion fires negotiation-needed only from the stable state, but the event
	// is processed asynchronously: an incoming offer may have been applied in
	// the meantime. Unlike ordinary negotiation-needed (which pion re-fires), a
	// custom ICE-restart request must remain pending when this guard fails.
	if n.pc.SignalingState() != webrtc.SignalingStateStable {
		return false
	}
	n.makingOffer = true
	defer func() { n.makingOffer = false }()
	var options *webrtc.OfferOptions
	if iceRestart {
		options = &webrtc.OfferOptions{ICERestart: true}
	}
	offer, err := n.pc.CreateOffer(options)
	if err != nil {
		n.logger.Error("rtc: create offer", "iceRestart", iceRestart, "err", err)
		return false
	}
	if err := n.pc.SetLocalDescription(offer); err != nil {
		n.logger.Error("rtc: set local description", "iceRestart", iceRestart, "err", err)
		return false
	}
	local := n.pc.LocalDescription()
	if local == nil {
		return false
	}
	n.logger.Info("rtc: send offer", "peer", n.options.PeerSubscriber, "iceRestart", iceRestart, "sdp", sdpSummary(local.SDP))
	n.sendToPeer(ctx, ssEvOut, &ss.ClientToClientEv{SessionDesc: local})
	if iceRestart {
		n.iceRestartPending = false
	}
	return true
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
	} else if c2c.ICERestartRequest && n.Polite {
		// Only the polite peer originates automated restart offers. The
		// requester retains its watchdog, so there is no local retry timer here.
		n.iceRestartPending = true
		n.makeOffer(ctx, ssEvOut, true)
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
		n.logger.Info("rtc: ignore colliding offer", "peer", n.options.PeerSubscriber)
		return
	}
	n.logger.Info("rtc: apply remote description", "peer", n.options.PeerSubscriber,
		"type", desc.Type, "signalingState", n.pc.SignalingState(), "collision", offerCollision,
		"polite", n.Polite, "sdp", sdpSummary(desc.SDP))

	// The polite peer yields to the colliding offer. In the browser this
	// is where setRemoteDescription(offer) implicitly rolls the pending
	// local offer back; pion cannot roll back (see the package comment),
	// so a pending local offer is abandoned by rebuilding the peer
	// connection instead. (When the local offer is still being created
	// — the makingOffer window — nothing has been set locally yet and no
	// rebuild is needed; its later SetLocalDescription simply fails.)
	if offerCollision && n.pc.SignalingState() == webrtc.SignalingStateHaveLocalOffer {
		n.logger.Info("rtc: glare, rebuilding the peer connection", "peer", n.options.PeerSubscriber)
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
		n.logger.Info("rtc: send answer", "peer", n.options.PeerSubscriber, "sdp", sdpSummary(local.SDP))
		n.sendToPeer(ctx, ssEvOut, &ss.ClientToClientEv{SessionDesc: local})
	}

	// An ICE-restart timer can fire while another offer/answer is in flight.
	// Pion has no browser-style restartIce() latch, so retry the pending offer
	// explicitly as soon as the remote description returns us to stable.
	if n.iceRestartPending && n.pc.SignalingState() == webrtc.SignalingStateStable {
		n.makeOffer(ctx, ssEvOut, true)
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

// handleICEConnectionState arms recovery only after an initially usable
// connection. Disconnected gets a grace period; failed accelerates any pending
// attempt. Checking is deliberately not an outcome: the active watchdog keeps
// running until ICE connects or the session shuts down.
func (n *PerfectNegotiator) handleICEConnectionState(state webrtc.ICEConnectionState) {
	switch state {
	case webrtc.ICEConnectionStateConnected, webrtc.ICEConnectionStateCompleted:
		if n.iceRecovering {
			n.logger.Info("rtc: ICE recovered", "peer", n.options.PeerSubscriber, "restarts", n.iceRestartAttempts)
		}
		n.iceConnectedOnce = true
		n.iceRecovering = false
		n.iceRestartPending = false
		n.iceRestartAttempts = 0
		n.stopICERestartTimer()
	case webrtc.ICEConnectionStateDisconnected:
		if !n.iceConnectedOnce || n.iceRecovering {
			return
		}
		n.iceRecovering = true
		n.logger.Info("rtc: ICE disconnected, restart scheduled", "peer", n.options.PeerSubscriber, "delay", n.iceDisconnectGrace)
		n.scheduleICERestart(n.iceDisconnectGrace)
	case webrtc.ICEConnectionStateFailed:
		if !n.iceConnectedOnce || (n.iceRecovering && n.iceRestartAttempts > 0) {
			return
		}
		n.iceRecovering = true
		n.stopICERestartTimer()
		n.scheduleICERestart(0)
	case webrtc.ICEConnectionStateClosed:
		n.iceRecovering = false
		n.iceRestartPending = false
		n.stopICERestartTimer()
	}
}

// handleICERestartTimer requests one restart offer and schedules the next
// watchdog regardless of whether signaling was stable. The pending latch keeps
// the request alive across an in-flight offer/answer; only a connected state or
// teardown cancels the retry sequence.
func (n *PerfectNegotiator) handleICERestartTimer(ctx context.Context, ssEvOut chan<- *ss.SignallingEvent) {
	n.iceRestartTimer = nil
	n.iceRestartTimerC = nil
	if !n.iceRecovering {
		return
	}
	state := n.currentICEState()
	switch state {
	case webrtc.ICEConnectionStateConnected, webrtc.ICEConnectionStateCompleted:
		// The timer and pion's connected/completed callback can become ready
		// together. Do not restart a healthy transport merely because the timer
		// won the select before that callback was processed.
		n.handleICEConnectionState(state)
		return
	case webrtc.ICEConnectionStateClosed:
		n.handleICEConnectionState(webrtc.ICEConnectionStateClosed)
		return
	}

	if n.Polite {
		n.iceRestartPending = true
		n.logger.Info("rtc: restarting ICE", "peer", n.options.PeerSubscriber, "attempt", n.iceRestartAttempts+1)
		n.makeOffer(ctx, ssEvOut, true)
	} else {
		n.logger.Info("rtc: requesting ICE restart", "peer", n.options.PeerSubscriber, "attempt", n.iceRestartAttempts+1)
		n.sendToPeer(ctx, ssEvOut, &ss.ClientToClientEv{ICERestartRequest: true})
	}
	delay := n.nextICERestartDelay()
	n.iceRestartAttempts++
	n.scheduleICERestart(delay)
}

func (n *PerfectNegotiator) currentICEState() webrtc.ICEConnectionState {
	if n.iceStateReader != nil {
		return n.iceStateReader()
	}
	return n.pc.ICEConnectionState()
}

func (n *PerfectNegotiator) scheduleICERestart(delay time.Duration) {
	if !n.iceRecovering || n.iceRestartTimer != nil {
		return
	}
	n.iceRestartTimer = time.NewTimer(delay)
	n.iceRestartTimerC = n.iceRestartTimer.C
}

func (n *PerfectNegotiator) stopICERestartTimer() {
	if n.iceRestartTimer != nil {
		n.iceRestartTimer.Stop()
		n.iceRestartTimer = nil
	}
	n.iceRestartTimerC = nil
}

// nextICERestartDelay returns base*2^attempt, nominally capped, with independent
// ±20% jitter so two peers do not retry in lockstep. The loop avoids exponent
// overflow on a connection that remains down for a long time.
func (n *PerfectNegotiator) nextICERestartDelay() time.Duration {
	delay := n.iceRetryBase
	for i := 0; i < n.iceRestartAttempts && delay < n.iceRetryMax; i++ {
		if delay > n.iceRetryMax/2 {
			delay = n.iceRetryMax
			break
		}
		delay *= 2
	}
	if delay > n.iceRetryMax {
		delay = n.iceRetryMax
	}
	random := n.iceRandom
	if random == nil {
		random = rand.Float64
	}
	return time.Duration(float64(delay) * (0.8 + 0.4*random()))
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
	// The replacement has fresh ICE credentials already; an additional restart
	// offer would be redundant. Keep the recovery watchdog until it connects.
	n.iceRestartPending = false
	old.OnNegotiationNeeded(nil)
	old.OnICECandidate(nil)
	old.OnICEConnectionStateChange(nil)
	n.attach(fresh)
	if err := old.Close(); err != nil {
		n.logger.Warn("rtc: close replaced peer connection", "err", err)
	}
	return nil
}

// errNoPeerConnectionFactory explains a polite peer that cannot yield.
var errNoPeerConnectionFactory = errors.New(
	"rtc: no NewPeerConnection factory configured; the polite peer cannot yield to a colliding offer")

// sdpSummary condenses a session description to what a failed
// SetRemoteDescription is about: per m-section, the mid and the a=setup
// role.
func sdpSummary(raw string) string {
	var b strings.Builder
	mid := "?"
	first := true
	for line := range strings.Lines(raw) {
		line = strings.TrimRight(line, "\r\n")
		if rest, ok := strings.CutPrefix(line, "a=mid:"); ok {
			mid = rest
		} else if rest, ok := strings.CutPrefix(line, "a=setup:"); ok {
			if !first {
				b.WriteString("; ")
			}
			first = false
			fmt.Fprintf(&b, "mid=%s setup=%s", mid, rest)
		}
	}
	return b.String()
}

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
			FromSubscriber:    n.options.SelfSubscriber,
			ToSubscriber:      n.options.PeerSubscriber,
			ChannelId:         n.options.ChannelId,
			SessionDesc:       payload.SessionDesc,
			RTCICECandidate:   payload.RTCICECandidate,
			ICERestartRequest: payload.ICERestartRequest,
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
