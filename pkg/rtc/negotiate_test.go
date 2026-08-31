package rtc

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"personal-site/pkg/models/ss"
)

// The e2e tests below wire two PerfectNegotiators — each on its own
// pion PeerConnection — through a real SimpleOnMemorySSProvider, the
// same shape the browser client has: one negotiator per peer, SS in the
// middle as a dumb c2c relay.

// testTimeout bounds every wait in the suite; local ICE is fast, so
// hitting it means a real failure, not slowness.
const testTimeout = 10 * time.Second

// testClient is one in-process SS client: a peer connection with a
// negotiator attached, plus the transport channels the test pumps
// between it and the SS. The peer connection is mutable: a glare can
// make the negotiator rebuild it through the factory, so every access
// goes through the mutex, and the data channel bookkeeping follows the
// latest incarnation.
type testClient struct {
	sub    ss.SubscriberId
	nego   *PerfectNegotiator
	addr   ss.EPAddr
	toSS   chan *ss.SignallingEvent // negotiator → SS (pump stamps From)
	fromSS chan *ss.SignallingEvent // SS → negotiator

	mu     sync.Mutex
	pc     *webrtc.PeerConnection
	local  map[string]*webrtc.DataChannel // created locally, by label
	remote map[string]*webrtc.DataChannel // announced by the peer, by label
}

// currentPC returns the latest peer connection.
func (c *testClient) currentPC() *webrtc.PeerConnection {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pc
}

// attach wires the test's own handlers on pc: every peer-announced data
// channel is recorded by label.
func (c *testClient) attach(pc *webrtc.PeerConnection) {
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		c.mu.Lock()
		c.remote[dc.Label()] = dc
		c.mu.Unlock()
	})
}

// createDataChannel creates a local data channel, recorded by label.
func (c *testClient) createDataChannel(t *testing.T, label string) {
	t.Helper()
	dc, err := c.currentPC().CreateDataChannel(label, nil)
	if err != nil {
		t.Fatalf("%s CreateDataChannel(%q): %v", c.sub, label, err)
	}
	c.mu.Lock()
	c.local[label] = dc
	c.mu.Unlock()
}

// localDC waits until the locally-created channel with the label exists
// and is open.
func (c *testClient) localDC(t *testing.T, label string) *webrtc.DataChannel {
	t.Helper()
	return c.waitDC(t, label, false)
}

// remoteDC waits until the peer-announced channel with the label exists
// and is open.
func (c *testClient) remoteDC(t *testing.T, label string) *webrtc.DataChannel {
	t.Helper()
	return c.waitDC(t, label, true)
}

func (c *testClient) waitDC(t *testing.T, label string, fromRemote bool) *webrtc.DataChannel {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for {
		c.mu.Lock()
		var dc *webrtc.DataChannel
		if fromRemote {
			dc = c.remote[label]
		} else {
			dc = c.local[label]
		}
		state := webrtc.DataChannelStateClosed
		if dc != nil {
			state = dc.ReadyState()
		}
		c.mu.Unlock()
		if dc != nil && state == webrtc.DataChannelStateOpen {
			return dc
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: data channel %q (remote=%v) is %s, want open", c.sub, label, fromRemote, state)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitConnected waits until the client's current peer connection reports
// connected.
func (c *testClient) waitConnected(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for {
		state := c.currentPC().ConnectionState()
		if state == webrtc.PeerConnectionStateConnected {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: peer connection state is %s, want connected", c.sub, state)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// register registers the client as a subscriber of the main channel,
// consuming the registerResult reply off its inbound channel before
// the negotiator starts reading it.
func (c *testClient) register(t *testing.T) {
	t.Helper()
	c.toSS <- &ss.SignallingEvent{
		MsgId: ss.MsgId("m-reg-" + string(c.sub)),
		C2SEv: &ss.ClientToSSEv{Register: &ss.ClientToSSRegEv{
			SubscriberId: c.sub,
			ChannelId:    ss.WellKnownChIdMain,
			Username:     string(c.sub),
		}},
	}
	deadline := time.After(testTimeout)
	for {
		select {
		case ev := <-c.fromSS:
			if ev.S2CEv != nil && ev.S2CEv.Err != nil {
				t.Fatalf("register %s: err %+v", c.sub, ev.S2CEv.Err)
			}
			if ev.S2CEv != nil && ev.S2CEv.RegisterResult != nil {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s's registerResult", c.sub)
		}
	}
}

// ssTestNet wires exactly two clients to one SimpleOnMemorySSProvider
// the way the real transports do: each client's outbound events are
// stamped with its address and forwarded to the provider, and the
// provider's outbound events are routed to the client whose registered
// address they are addressed to.
type ssTestNet struct {
	ctx     context.Context
	a, b    *testClient
	provIn  chan *ss.SignallingEvent
	provOut chan *ss.SignallingEvent
}

// newSSTestNet builds the two clients and starts the provider and the
// transport pumps. Everything is torn down via t.Cleanup.
func newSSTestNet(t *testing.T, subA, subB ss.SubscriberId) *ssTestNet {
	t.Helper()

	newClient := func(self, peer ss.SubscriberId) *testClient {
		t.Helper()
		c := &testClient{
			sub:    self,
			addr:   ss.EPAddr{UserId: ss.UserId("u-" + string(self)), UserSessionId: ss.UserSessionId("s-" + string(self))},
			toSS:   make(chan *ss.SignallingEvent, 16),
			fromSS: make(chan *ss.SignallingEvent, 16),
			local:  map[string]*webrtc.DataChannel{},
			remote: map[string]*webrtc.DataChannel{},
		}
		pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
		if err != nil {
			t.Fatalf("NewPeerConnection: %v", err)
		}
		c.pc = pc
		c.attach(pc)
		t.Cleanup(func() { _ = c.currentPC().Close() })

		nego, err := NewPerfectNegotiator(pc, PerfectNegotiatorOptions{
			ChannelId:      ss.WellKnownChIdMain,
			SelfSubscriber: self,
			PeerSubscriber: peer,
			Logger:         testLogger(t),
			// The glare factory: a fresh peer connection with the same
			// data channels re-created — exactly what a real caller
			// must provide.
			NewPeerConnection: func() (*webrtc.PeerConnection, error) {
				fresh, err := webrtc.NewPeerConnection(webrtc.Configuration{})
				if err != nil {
					return nil, err
				}
				c.mu.Lock()
				defer c.mu.Unlock()
				c.pc = fresh
				c.attach(fresh)
				for label := range c.local {
					dc, err := fresh.CreateDataChannel(label, nil)
					if err != nil {
						return nil, err
					}
					c.local[label] = dc
				}
				return fresh, nil
			},
		})
		if err != nil {
			t.Fatalf("NewPerfectNegotiator(%s): %v", self, err)
		}
		c.nego = nego
		return c
	}

	ctx, cancel := context.WithCancel(context.Background())
	net := &ssTestNet{
		ctx:     ctx,
		a:       newClient(subA, subB),
		b:       newClient(subB, subA),
		provIn:  make(chan *ss.SignallingEvent, 16),
		provOut: make(chan *ss.SignallingEvent, 16),
	}

	provider := ss.NewSimpleOnMemorySSProvider()
	go provider.Run(ctx, net.provIn, net.provOut)
	t.Cleanup(provider.Shutdown)
	t.Cleanup(cancel)

	// Provider outbound → the addressed client. Both clients exist
	// before this goroutine starts, so the map need not be guarded.
	byUser := map[ss.UserId]*testClient{net.a.addr.UserId: net.a, net.b.addr.UserId: net.b}
	go func() {
		for ev := range net.provOut {
			c, ok := byUser[ev.To.UserId]
			if !ok {
				continue
			}
			select {
			case c.fromSS <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()

	// Client outbound → provider inbound, with From stamped by the
	// transport the way the real server-side handler does.
	for _, c := range []*testClient{net.a, net.b} {
		go func(c *testClient) {
			for ev := range c.toSS {
				ev.From = c.addr
				select {
				case net.provIn <- ev:
				case <-ctx.Done():
					return
				}
			}
		}(c)
	}

	return net
}

// startNegotiators runs both negotiators' loops in the background.
func (net *ssTestNet) startNegotiators(t *testing.T) {
	t.Helper()
	for _, c := range []*testClient{net.a, net.b} {
		go func(c *testClient) {
			if err := c.nego.Negotiate(net.ctx, c.fromSS, c.toSS); err != nil &&
				!errors.Is(err, context.Canceled) {
				t.Errorf("negotiator %s: %v", c.sub, err)
			}
		}(c)
	}
}

// assertDCMessage sends text over one data channel and fails the test
// unless the exact text arrives on the other end.
func assertDCMessage(t *testing.T, from, to *webrtc.DataChannel, text string) {
	t.Helper()
	got := make(chan string, 1)
	to.OnMessage(func(msg webrtc.DataChannelMessage) {
		if msg.IsString {
			got <- string(msg.Data)
		}
	})
	if err := from.SendText(text); err != nil {
		t.Fatalf("SendText(%q): %v", text, err)
	}
	select {
	case s := <-got:
		if s != text {
			t.Fatalf("data channel message is %q, want %q", s, text)
		}
	case <-time.After(testTimeout):
		t.Fatalf("timed out waiting for data channel message %q", text)
	}
}

// TestPerfectNegotiatorConnectsTwoPeersThroughSS runs the happy path:
// alice's data channel triggers the offer, the SS relays it to bob, bob
// answers, ICE trickles both ways, and the connection — negotiated
// entirely through SignallingEvents — carries data.
func TestPerfectNegotiatorConnectsTwoPeersThroughSS(t *testing.T) {
	net := newSSTestNet(t, "alice", "bob")
	net.a.register(t)
	net.b.register(t)
	net.startNegotiators(t)

	net.a.createDataChannel(t, "chat")

	net.a.waitConnected(t)
	net.b.waitConnected(t)
	aChat := net.a.localDC(t, "chat")
	bChat := net.b.remoteDC(t, "chat")
	assertDCMessage(t, aChat, bChat, "hello from alice")
	assertDCMessage(t, bChat, aChat, "hi from bob")
}

// TestPerfectNegotiatorSurvivesGlare has both peers demand negotiation
// at the same time — each creates a data channel before either offer
// has been answered — the simultaneous-offer collision the perfect
// negotiation pattern exists for. alice ("alice" < "bob") is the polite
// peer and yields (rebuilding her peer connection, since pion cannot
// roll an offer back); bob is impolite and ignores hers. Both data
// channels must still come up.
func TestPerfectNegotiatorSurvivesGlare(t *testing.T) {
	net := newSSTestNet(t, "alice", "bob")
	if !net.a.nego.Polite || net.b.nego.Polite {
		t.Fatal("polite roles are not derived from the subscriber ids")
	}
	net.a.register(t)
	net.b.register(t)
	net.startNegotiators(t)

	net.a.createDataChannel(t, "a-side")
	net.b.createDataChannel(t, "b-side")

	net.a.waitConnected(t)
	net.b.waitConnected(t)
	aSide := net.a.localDC(t, "a-side")
	bSideA := net.b.remoteDC(t, "a-side")
	bSide := net.b.localDC(t, "b-side")
	aSideB := net.a.remoteDC(t, "b-side")
	assertDCMessage(t, aSide, bSideA, "a wins the collision")
	assertDCMessage(t, bSide, aSideB, "b keeps its own channel")
}

// TestPolitePeerRebuildsOnGlare drives the collision deterministically,
// without ICE: the polite negotiator's peer connection is parked in
// have-local-offer (as a sent offer leaves it), a colliding offer
// arrives, and the negotiator must rebuild the peer connection through
// the factory, answer on the fresh one, and close the replaced one.
func TestPolitePeerRebuildsOnGlare(t *testing.T) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("NewPeerConnection: %v", err)
	}

	rebuilt := make(chan *webrtc.PeerConnection, 1)
	nego, err := NewPerfectNegotiator(pc, PerfectNegotiatorOptions{
		ChannelId:      ss.WellKnownChIdMain,
		SelfSubscriber: "alice",
		PeerSubscriber: "bob",
		Logger:         testLogger(t),
		NewPeerConnection: func() (*webrtc.PeerConnection, error) {
			fresh, err := webrtc.NewPeerConnection(webrtc.Configuration{})
			if err != nil {
				return nil, err
			}
			rebuilt <- fresh
			return fresh, nil
		},
	})
	if err != nil {
		t.Fatalf("NewPerfectNegotiator: %v", err)
	}
	if !nego.Polite {
		t.Fatal(`"alice" must be the polite peer of "bob"`)
	}

	// Park the local side in have-local-offer, as a sent offer leaves
	// it, with a pending local channel the factory must recreate.
	if _, err := pc.CreateDataChannel("mine", nil); err != nil {
		t.Fatalf("CreateDataChannel: %v", err)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("SetLocalDescription: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	in := make(chan *ss.SignallingEvent, 1)
	out := make(chan *ss.SignallingEvent, 8)
	// Drain the outbound channel: trickled ICE candidates for the
	// parked offer keep flowing, and Negotiate — like the frontend's
	// WritableStream writer — backpressures on a full channel.
	outEvents := make(chan *ss.SignallingEvent, 32)
	go func() {
		for {
			select {
			case ev := <-out:
				outEvents <- ev
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		if err := nego.Negotiate(ctx, in, out); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Negotiate: %v", err)
		}
	}()

	// The colliding offer: a genuine offer from the peer.
	peerPC, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("NewPeerConnection: %v", err)
	}
	t.Cleanup(func() { _ = peerPC.Close() })
	if _, err := peerPC.CreateDataChannel("theirs", nil); err != nil {
		t.Fatalf("CreateDataChannel: %v", err)
	}
	theirOffer, err := peerPC.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	in <- &ss.SignallingEvent{
		MsgId: "m-glare",
		C2CEv: &ss.ClientToClientEv{
			ChannelId:      ss.WellKnownChIdMain,
			FromSubscriber: "bob",
			ToSubscriber:   "alice",
			SessionDesc:    &theirOffer,
		},
	}

	// The factory ran and the replaced connection was closed.
	var fresh *webrtc.PeerConnection
	select {
	case fresh = <-rebuilt:
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for the peer connection rebuild")
	}
	deadline := time.Now().Add(testTimeout)
	for pc.ConnectionState() != webrtc.PeerConnectionStateClosed {
		if time.Now().After(deadline) {
			t.Fatalf("replaced peer connection state is %s, want closed", pc.ConnectionState())
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Cleanup(func() { _ = fresh.Close() })

	// The colliding offer was applied and answered on the fresh one.
	answer := func() *ss.SignallingEvent {
		deadline := time.After(testTimeout)
		for {
			select {
			case ev := <-outEvents:
				if ev.C2CEv != nil && ev.C2CEv.SessionDesc != nil {
					return ev
				}
			case <-deadline:
				return nil
			}
		}
	}()
	if answer == nil || answer.C2CEv.SessionDesc.Type != webrtc.SDPTypeAnswer {
		t.Fatalf("glare reply: %+v", answer)
	}
	if state := fresh.SignalingState(); state != webrtc.SignalingStateStable {
		t.Fatalf("fresh peer connection signaling state is %s, want stable", state)
	}
	if remote := fresh.RemoteDescription(); remote == nil || remote.Type != webrtc.SDPTypeOffer {
		t.Fatalf("fresh peer connection remote description: %+v", remote)
	}
}

// TestPerfectNegotiatorIgnoresForeignEvents feeds a negotiator events
// that are not negotiation traffic and asserts it emits nothing: an s2c
// reply, a c2c event in another channel, and c2c events from or to the
// wrong subscribers.
func TestPerfectNegotiatorIgnoresForeignEvents(t *testing.T) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("NewPeerConnection: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	nego, err := NewPerfectNegotiator(pc, PerfectNegotiatorOptions{
		ChannelId:      ss.WellKnownChIdMain,
		SelfSubscriber: "alice",
		PeerSubscriber: "bob",
		Logger:         testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewPerfectNegotiator: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	in := make(chan *ss.SignallingEvent, 8)
	out := make(chan *ss.SignallingEvent, 8)
	go func() {
		if err := nego.Negotiate(ctx, in, out); err != nil &&
			!errors.Is(err, context.Canceled) {
			t.Errorf("Negotiate: %v", err)
		}
	}()

	c2c := func(ch ss.ChannelId, from, to ss.SubscriberId) *ss.SignallingEvent {
		return &ss.SignallingEvent{
			MsgId: "m-foreign",
			C2CEv: &ss.ClientToClientEv{
				ChannelId: ch, FromSubscriber: from, ToSubscriber: to,
				RTCICECandidate: &webrtc.ICECandidateInit{Candidate: "candidate:1 1 UDP 1 127.0.0.1 1 typ host"},
			},
		}
	}
	in <- &ss.SignallingEvent{MsgId: "m-s2c", S2CEv: &ss.SSToClientEv{}}
	in <- c2c("other-channel", "bob", "alice")
	in <- c2c(ss.WellKnownChIdMain, "carol", "alice")
	in <- c2c(ss.WellKnownChIdMain, "bob", "carol")

	select {
	case ev := <-out:
		t.Fatalf("unexpected outbound event: %+v", ev)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestNewPerfectNegotiatorRejectsMisuse covers the constructor and
// single-flight guards.
func TestNewPerfectNegotiatorRejectsMisuse(t *testing.T) {
	if _, err := NewPerfectNegotiator(nil, PerfectNegotiatorOptions{
		SelfSubscriber: "alice", PeerSubscriber: "bob",
	}); !errors.Is(err, ErrNilPeerConnection) {
		t.Fatalf("nil peer connection: got %v, want %v", err, ErrNilPeerConnection)
	}

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("NewPeerConnection: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	if _, err := NewPerfectNegotiator(pc, PerfectNegotiatorOptions{
		ChannelId: ss.WellKnownChIdMain, SelfSubscriber: "alice", PeerSubscriber: "alice",
	}); !errors.Is(err, ErrSelfNegotiation) {
		t.Fatalf("self negotiation: got %v, want %v", err, ErrSelfNegotiation)
	}

	nego, err := NewPerfectNegotiator(pc, PerfectNegotiatorOptions{
		ChannelId: ss.WellKnownChIdMain, SelfSubscriber: "alice", PeerSubscriber: "bob",
		Logger: testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewPerfectNegotiator: %v", err)
	}

	// A first run blocked on a never-closing input keeps the
	// negotiator busy; a second run must refuse, and canceling the
	// context must unwind the first.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	in := make(chan *ss.SignallingEvent)
	out := make(chan *ss.SignallingEvent)
	done := make(chan error, 1)
	go func() { done <- nego.Negotiate(ctx, in, out) }()
	time.Sleep(50 * time.Millisecond)
	if err := nego.Negotiate(ctx, in, out); !errors.Is(err, ErrAlreadyNegotiating) {
		t.Fatalf("second Negotiate: got %v, want %v", err, ErrAlreadyNegotiating)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("first Negotiate: got %v, want %v", err, context.Canceled)
	}

	// With the first run unwound, a fresh run is allowed again.
	in2 := make(chan *ss.SignallingEvent)
	close(in2)
	if err := nego.Negotiate(context.Background(), in2, out); err != nil {
		t.Fatalf("re-run after close: %v", err)
	}
}

// testLogger keeps negotiator diagnostics in the test output.
func testLogger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}
