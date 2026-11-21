package rtc

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
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
// hitting it means a real failure, not slowness. It is generous all the
// same: under the race detector with the sibling suites (echobot,
// musicbot) running in parallel, a pion handshake can take several
// seconds on a loaded machine.
const testTimeout = 30 * time.Second

// testClient is one in-process SS client: a peer connection with a
// negotiator attached, plus the transport channels the test pumps
// between it and the SS. The mutex also protects data-channel bookkeeping
// from pion's callback goroutines.
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
// peer and yields by rolling her offer back; bob is impolite and ignores
// hers. Both data channels must still come up on the original connections.
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

// TestPolitePeerRollsBackOnGlare drives the collision deterministically:
// the polite negotiator abandons its pending offer, answers on the same peer
// connection, and never replaces the session's ICE/DTLS identity.
func TestPolitePeerRollsBackOnGlare(t *testing.T) {
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
	if !nego.Polite {
		t.Fatal(`"alice" must be the polite peer of "bob"`)
	}

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
	if state := pc.SignalingState(); state != webrtc.SignalingStateStable {
		t.Fatalf("peer connection signaling state is %s, want stable", state)
	}
	if remote := pc.RemoteDescription(); remote == nil || remote.Type != webrtc.SDPTypeOffer {
		t.Fatalf("peer connection remote description: %+v", remote)
	}
	if pc.ConnectionState() == webrtc.PeerConnectionStateClosed {
		t.Fatal("glare closed the original peer connection")
	}
}

// fingerprintNegotiatorHarness drives a real initial offer/answer directly,
// then lets each focused test inject its next description without an ICE or SS
// goroutine making the PC-swap assertion timing-dependent.
type fingerprintNegotiatorHarness struct {
	t                    *testing.T
	nego                 *PerfectNegotiator
	original             *webrtc.PeerConnection
	remote               *webrtc.PeerConnection
	out                  chan *ss.SignallingEvent
	replacements         int
	offerHasApplications []bool
}

func newFingerprintNegotiatorHarness(t *testing.T) *fingerprintNegotiatorHarness {
	t.Helper()
	original, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("NewPeerConnection(original): %v", err)
	}
	remote, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		_ = original.Close()
		t.Fatalf("NewPeerConnection(remote): %v", err)
	}
	h := &fingerprintNegotiatorHarness{
		t: t, original: original, remote: remote,
		out: make(chan *ss.SignallingEvent, 8),
	}
	h.nego, err = NewPerfectNegotiator(original, PerfectNegotiatorOptions{
		ChannelId:      ss.WellKnownChIdMain,
		SelfSubscriber: "alice",
		PeerSubscriber: "bob",
		NewPeerConnection: func(offerHasApplication bool) (*webrtc.PeerConnection, error) {
			h.replacements++
			h.offerHasApplications = append(h.offerHasApplications, offerHasApplication)
			return webrtc.NewPeerConnection(webrtc.Configuration{})
		},
		Logger: testLogger(t),
	})
	if err != nil {
		_ = original.Close()
		_ = remote.Close()
		t.Fatalf("NewPerfectNegotiator: %v", err)
	}
	t.Cleanup(func() {
		h.nego.stopICERestartTimer()
		h.nego.stopGlareRetryTimer()
		_ = h.nego.pc.Close()
		_ = remote.Close()
	})

	if _, err := remote.CreateDataChannel("initial", nil); err != nil {
		t.Fatalf("CreateDataChannel(initial): %v", err)
	}
	initialOffer := setTestOffer(t, remote, nil)
	initialAnswer := h.receiveAnswer(initialOffer)
	if err := remote.SetRemoteDescription(*initialAnswer); err != nil {
		t.Fatalf("remote.SetRemoteDescription(initial answer): %v", err)
	}
	// SetLocal/RemoteDescription starts ICE on Pion's operations goroutine.
	// Waiting for both transports to leave new avoids restarting gathering while
	// that asynchronous start is still installing its gatherer.
	waitFor(t, "initial ICE transports to start", func() bool {
		return original.ICEConnectionState() != webrtc.ICEConnectionStateNew &&
			remote.ICEConnectionState() != webrtc.ICEConnectionStateNew
	})
	if h.replacements != 0 || h.nego.pc != original {
		t.Fatal("the first remote offer replaced the peer connection")
	}
	if original.CurrentRemoteDescription() == nil {
		t.Fatal("initial offer/answer did not establish a stable remote description")
	}
	return h
}

func setTestOffer(t *testing.T, pc *webrtc.PeerConnection, options *webrtc.OfferOptions) *webrtc.SessionDescription {
	t.Helper()
	offer, err := pc.CreateOffer(options)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("SetLocalDescription(offer): %v", err)
	}
	local := pc.LocalDescription()
	if local == nil {
		t.Fatal("SetLocalDescription(offer) left no local description")
	}
	return local
}

func (h *fingerprintNegotiatorHarness) receiveAnswer(offer *webrtc.SessionDescription) *webrtc.SessionDescription {
	h.t.Helper()
	h.nego.handleSessionDesc(context.Background(), offer, h.out)
	select {
	case ev := <-h.out:
		if ev.C2CEv == nil || ev.C2CEv.SessionDesc == nil || ev.C2CEv.SessionDesc.Type != webrtc.SDPTypeAnswer {
			h.t.Fatalf("offer reply = %+v, want an answer", ev)
		}
		return ev.C2CEv.SessionDesc
	default:
		h.t.Fatal("offer produced no answer")
		return nil
	}
}

func TestPerfectNegotiatorKeepsPeerConnectionForSameFingerprintOffers(t *testing.T) {
	t.Run("ICE restart", func(t *testing.T) {
		h := newFingerprintNegotiatorHarness(t)
		before := sdpICECredentials(h.remote.LocalDescription().SDP)
		restart := setTestOffer(t, h.remote, &webrtc.OfferOptions{ICERestart: true})
		if after := sdpICECredentials(restart.SDP); after == before || after == "" {
			t.Fatal("test setup did not produce fresh ICE credentials")
		}
		answer := h.receiveAnswer(restart)
		if h.replacements != 0 || h.nego.pc != h.original {
			t.Fatal("same-fingerprint ICE restart replaced the peer connection")
		}
		if err := h.remote.SetRemoteDescription(*answer); err != nil {
			t.Fatalf("remote.SetRemoteDescription(restart answer): %v", err)
		}
	})

	t.Run("ordinary glare rollback", func(t *testing.T) {
		h := newFingerprintNegotiatorHarness(t)
		setTestOffer(t, h.original, nil)
		collidingOffer := setTestOffer(t, h.remote, nil)
		answer := h.receiveAnswer(collidingOffer)
		if h.replacements != 0 || h.nego.pc != h.original {
			t.Fatal("ordinary same-fingerprint glare replaced the peer connection")
		}
		if state := h.original.SignalingState(); state != webrtc.SignalingStateStable {
			t.Fatalf("rollback left signaling state %s, want stable", state)
		}
		if err := h.remote.SetRemoteDescription(*answer); err != nil {
			t.Fatalf("remote.SetRemoteDescription(glare answer): %v", err)
		}
	})
}

func TestPerfectNegotiatorReplacesPeerConnectionForChangedFingerprint(t *testing.T) {
	for _, tc := range []struct {
		name                string
		offerHasApplication bool
		collision           bool
	}{
		{name: "offerer announces data channels", offerHasApplication: true},
		{name: "offer without application m-line"},
		{name: "changed fingerprint wins glare", offerHasApplication: true, collision: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newFingerprintNegotiatorHarness(t)
			freshRemote, err := webrtc.NewPeerConnection(webrtc.Configuration{})
			if err != nil {
				t.Fatalf("NewPeerConnection(fresh remote): %v", err)
			}
			t.Cleanup(func() { _ = freshRemote.Close() })
			if tc.offerHasApplication {
				if _, err := freshRemote.CreateDataChannel("known-label", nil); err != nil {
					t.Fatalf("CreateDataChannel(fresh remote): %v", err)
				}
			} else {
				if _, err := freshRemote.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio); err != nil {
					t.Fatalf("AddTransceiverFromKind: %v", err)
				}
			}
			if tc.collision {
				setTestOffer(t, h.original, nil)
				h.nego.scheduleGlareRetryTimer()
			}
			h.nego.iceRestartPending = true
			freshOffer := setTestOffer(t, freshRemote, nil)
			answer := h.receiveAnswer(freshOffer)

			if h.replacements != 1 || h.nego.pc == h.original {
				t.Fatalf("changed fingerprint replacements=%d, want exactly one fresh PC", h.replacements)
			}
			if len(h.offerHasApplications) != 1 || h.offerHasApplications[0] != tc.offerHasApplication {
				t.Fatalf("factory application flags=%v, want [%v]", h.offerHasApplications, tc.offerHasApplication)
			}
			if state := h.original.ConnectionState(); state != webrtc.PeerConnectionStateClosed {
				t.Fatalf("replaced PC state=%s, want closed", state)
			}
			if h.nego.iceRestartPending || h.nego.glareRetryTimer != nil {
				t.Fatal("replacement retained a redundant ICE restart or glare retry")
			}
			if state := h.nego.pc.SignalingState(); state != webrtc.SignalingStateStable {
				t.Fatalf("fresh PC signaling state=%s, want stable", state)
			}
			if err := freshRemote.SetRemoteDescription(*answer); err != nil {
				t.Fatalf("freshRemote.SetRemoteDescription(answer): %v", err)
			}
			h.nego.handleLocal(context.Background(), localEvent{source: h.original}, h.out)
			if len(h.out) != 0 {
				t.Fatal("a stale local event from the replaced PC reached the wire")
			}
		})
	}
}

func TestPerfectNegotiatorDoesNotReplaceForUnusableDescriptions(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind webrtc.SDPType
		sdp  func(string) string
	}{
		{name: "answer", kind: webrtc.SDPTypeAnswer, sdp: func(raw string) string { return raw }},
		{name: "malformed offer", kind: webrtc.SDPTypeOffer, sdp: func(string) string { return "not SDP" }},
		{name: "offer without fingerprint", kind: webrtc.SDPTypeOffer, sdp: withoutSDPFingerprints},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newFingerprintNegotiatorHarness(t)
			freshRemote, err := webrtc.NewPeerConnection(webrtc.Configuration{})
			if err != nil {
				t.Fatalf("NewPeerConnection(fresh remote): %v", err)
			}
			t.Cleanup(func() { _ = freshRemote.Close() })
			if _, err := freshRemote.CreateDataChannel("fresh", nil); err != nil {
				t.Fatalf("CreateDataChannel: %v", err)
			}
			desc := *setTestOffer(t, freshRemote, nil)
			desc.Type = tc.kind
			desc.SDP = tc.sdp(desc.SDP)
			h.nego.handleSessionDesc(context.Background(), &desc, h.out)
			if h.replacements != 0 || h.nego.pc != h.original {
				t.Fatalf("%s incorrectly replaced the peer connection", tc.name)
			}
		})
	}
}

func withoutSDPFingerprints(raw string) string {
	var b strings.Builder
	for line := range strings.Lines(raw) {
		if !strings.HasPrefix(line, "a=fingerprint:") {
			b.WriteString(line)
		}
	}
	return b.String()
}

func TestSessionDTLSFingerprintSemantics(t *testing.T) {
	const base = "v=0\r\n" +
		"o=- 1 1 IN IP4 127.0.0.1\r\n" +
		"s=-\r\n" +
		"t=0 0\r\n"
	for _, tc := range []struct {
		name string
		raw  string
		want string
		ok   bool
	}{
		{
			name: "session level takes precedence",
			raw: base + "a=fingerprint:SHA-256 AA:BB\r\n" +
				"a=group:BUNDLE 0\r\n" +
				"m=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\n" +
				"a=mid:0\r\n" +
				"a=fingerprint:sha-256 CC:DD\r\n",
			want: "sha-256 aa:bb", ok: true,
		},
		{
			name: "first BUNDLE mid selects the media fingerprint",
			raw: base + "a=group:BUNDLE 1 0\r\n" +
				"m=audio 9 UDP/TLS/RTP/SAVPF 111\r\n" +
				"a=mid:0\r\n" +
				"a=fingerprint:sha-256 AA:AA\r\n" +
				"m=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\n" +
				"a=mid:1\r\n" +
				"a=fingerprint:SHA-256 BB:BB\r\n",
			want: "sha-256 bb:bb", ok: true,
		},
		{
			name: "shared non-BUNDLE media fingerprints match session-level form",
			raw: base + "m=audio 9 UDP/TLS/RTP/SAVPF 111\r\n" +
				"a=mid:0\r\n" +
				"a=fingerprint:sha-256 AA:BB\r\n" +
				"m=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\n" +
				"a=mid:1\r\n" +
				"a=fingerprint:SHA-256 aa:bb\r\n",
			want: "sha-256 aa:bb", ok: true,
		},
		{
			name: "missing fingerprint",
			raw: base + "m=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\n" +
				"a=mid:0\r\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			desc := webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: tc.raw}
			parsed, err := desc.Unmarshal()
			if err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			got, ok := sessionDTLSFingerprint(parsed)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("sessionDTLSFingerprint()=(%q, %v), want (%q, %v)", got, ok, tc.want, tc.ok)
			}
		})
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
