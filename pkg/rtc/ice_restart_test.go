package rtc

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"personal-site/pkg/models/ss"
)

func newICERestartUnitNegotiator(t *testing.T) (*PerfectNegotiator, *webrtc.PeerConnection) {
	t.Helper()
	return newICERestartUnitNegotiatorWithRole(t, true)
}

func newICERestartUnitNegotiatorWithRole(t *testing.T, polite bool) (*PerfectNegotiator, *webrtc.PeerConnection) {
	t.Helper()
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("NewPeerConnection: %v", err)
	}
	self, peer := ss.SubscriberId("alice"), ss.SubscriberId("bob")
	if !polite {
		self, peer = peer, self
	}
	n, err := NewPerfectNegotiator(pc, PerfectNegotiatorOptions{
		ChannelId:      ss.WellKnownChIdMain,
		SelfSubscriber: self,
		PeerSubscriber: peer,
		Logger:         testLogger(t),
	})
	if err != nil {
		_ = pc.Close()
		t.Fatalf("NewPerfectNegotiator: %v", err)
	}
	n.iceDisconnectGrace = time.Hour
	n.iceRetryBase = time.Hour
	n.iceRetryMax = time.Hour
	n.iceRandom = func() float64 { return 0.5 }
	n.glareRetryDelay = time.Hour
	t.Cleanup(func() {
		n.stopICERestartTimer()
		n.stopGlareRetryTimer()
		_ = pc.Close()
	})
	return n, pc
}

func TestICERestartRequiresInitialConnection(t *testing.T) {
	n, _ := newICERestartUnitNegotiator(t)
	n.handleICEConnectionState(webrtc.ICEConnectionStateDisconnected)
	n.handleICEConnectionState(webrtc.ICEConnectionStateFailed)
	if n.iceRecovering || n.iceRestartTimer != nil {
		t.Fatal("recovery started before an initial connected state")
	}
}

func TestICERestartCancellationAndClose(t *testing.T) {
	n, _ := newICERestartUnitNegotiator(t)
	n.handleICEConnectionState(webrtc.ICEConnectionStateConnected)
	n.handleICEConnectionState(webrtc.ICEConnectionStateDisconnected)
	if !n.iceRecovering || n.iceRestartTimer == nil {
		t.Fatal("disconnected did not schedule recovery")
	}

	n.handleICEConnectionState(webrtc.ICEConnectionStateConnected)
	if n.iceRecovering || n.iceRestartTimer != nil || n.iceRestartAttempts != 0 {
		t.Fatal("connected did not cancel and reset recovery")
	}

	n.handleICEConnectionState(webrtc.ICEConnectionStateDisconnected)
	n.handleICEConnectionState(webrtc.ICEConnectionStateFailed)
	if !n.iceRecovering || n.iceRestartTimer == nil {
		t.Fatal("failed did not keep a restart scheduled")
	}
	n.handleICEConnectionState(webrtc.ICEConnectionStateClosed)
	if n.iceRecovering || n.iceRestartPending || n.iceRestartTimer != nil {
		t.Fatal("closed did not tear recovery down")
	}
}

func TestICERestartTreatsCompletedAsHealthy(t *testing.T) {
	n, _ := newICERestartUnitNegotiator(t)
	n.handleICEConnectionState(webrtc.ICEConnectionStateCompleted)
	n.handleICEConnectionState(webrtc.ICEConnectionStateDisconnected)
	if !n.iceRecovering || n.iceRestartTimer == nil {
		t.Fatal("an initially completed connection did not arm recovery")
	}
	n.handleICEConnectionState(webrtc.ICEConnectionStateCompleted)
	if n.iceRecovering || n.iceRestartTimer != nil {
		t.Fatal("completed did not cancel recovery")
	}
}

func TestICERestartDoesNotAccelerateAnActiveRetry(t *testing.T) {
	n, _ := newICERestartUnitNegotiator(t)
	n.handleICEConnectionState(webrtc.ICEConnectionStateConnected)
	n.handleICEConnectionState(webrtc.ICEConnectionStateDisconnected)
	n.iceRestartAttempts = 1
	timer := n.iceRestartTimer
	n.handleICEConnectionState(webrtc.ICEConnectionStateFailed)
	if n.iceRestartTimer != timer {
		t.Fatal("a repeated failure reset the active retry's backoff")
	}
}

func TestICERestartBackoff(t *testing.T) {
	n, _ := newICERestartUnitNegotiator(t)
	n.iceRetryBase = 2 * time.Second
	n.iceRetryMax = 30 * time.Second
	for _, tc := range []struct {
		attempts int
		want     time.Duration
	}{
		{0, 2 * time.Second},
		{1, 4 * time.Second},
		{2, 8 * time.Second},
		{3, 16 * time.Second},
		{4, 30 * time.Second},
		{5, 30 * time.Second},
		{100, 30 * time.Second},
	} {
		n.iceRestartAttempts = tc.attempts
		if got := n.nextICERestartDelay(); got != tc.want {
			t.Errorf("attempts=%d: delay=%s, want %s", tc.attempts, got, tc.want)
		}
	}

	n.iceRestartAttempts = 0
	n.iceRandom = func() float64 { return 0 }
	if got := n.nextICERestartDelay(); got != 1600*time.Millisecond {
		t.Errorf("minimum jitter: delay=%s, want 1.6s", got)
	}
	n.iceRandom = func() float64 { return 1 }
	if got := n.nextICERestartDelay(); got != 2400*time.Millisecond {
		t.Errorf("maximum jitter: delay=%s, want 2.4s", got)
	}
}

func TestICERestartRetransmitsOrdinaryOfferOutsideStable(t *testing.T) {
	n, pc := newICERestartUnitNegotiator(t)
	if _, err := pc.CreateDataChannel("chat", nil); err != nil {
		t.Fatalf("CreateDataChannel: %v", err)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("SetLocalDescription: %v", err)
	}
	if pc.SignalingState() != webrtc.SignalingStateHaveLocalOffer {
		t.Fatalf("signaling state=%s, want have-local-offer", pc.SignalingState())
	}
	before := sdpICECredentials(pc.LocalDescription().SDP)

	out := make(chan *ss.SignallingEvent, 1)
	n.iceConnectedOnce = true
	n.iceRecovering = true
	n.handleICERestartTimer(context.Background(), out)
	if !n.iceRestartPending {
		t.Fatal("ordinary pending offer incorrectly satisfied the latched ICE restart")
	}
	if n.iceRestartAttempts != 1 || n.iceRestartTimer == nil {
		t.Fatal("restart watchdog was not retained")
	}
	if len(out) != 1 {
		t.Fatalf("retry emitted %d events, want the ordinary offer retransmitted", len(out))
	}
	ev := <-out
	if ev.C2CEv == nil || ev.C2CEv.SessionDesc == nil ||
		ev.C2CEv.SessionDesc.Type != webrtc.SDPTypeOffer ||
		sdpICECredentials(ev.C2CEv.SessionDesc.SDP) != before {
		t.Fatalf("event=%+v, want the same ordinary offer rather than a new negotiation", ev)
	}
}

func TestICERestartImpoliteRetransmitsPendingOffer(t *testing.T) {
	n, pc := newICERestartUnitNegotiatorWithRole(t, false)
	if _, err := pc.CreateDataChannel("chat", nil); err != nil {
		t.Fatalf("CreateDataChannel: %v", err)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("SetLocalDescription: %v", err)
	}

	out := make(chan *ss.SignallingEvent, 4)
	n.iceConnectedOnce = true
	n.iceRecovering = true
	n.handleICERestartTimer(context.Background(), out)
	var request, retransmitted bool
	for len(out) > 0 {
		ev := <-out
		if ev.C2CEv == nil {
			continue
		}
		request = request || ev.C2CEv.ICERestartRequest
		if desc := ev.C2CEv.SessionDesc; desc != nil && desc.Type == webrtc.SDPTypeOffer &&
			sdpICECredentials(desc.SDP) == sdpICECredentials(offer.SDP) {
			retransmitted = true
		}
	}
	if !request || !retransmitted {
		t.Fatalf("impolite retry request=%v retransmitted=%v, want both", request, retransmitted)
	}
}

func TestImpoliteGlareRetransmitsPendingOffer(t *testing.T) {
	n, pc := newICERestartUnitNegotiatorWithRole(t, false)
	if _, err := pc.CreateDataChannel("chat", nil); err != nil {
		t.Fatalf("CreateDataChannel: %v", err)
	}
	localOffer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer(local): %v", err)
	}
	if err := pc.SetLocalDescription(localOffer); err != nil {
		t.Fatalf("SetLocalDescription(local): %v", err)
	}
	remote, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("NewPeerConnection(remote): %v", err)
	}
	t.Cleanup(func() { _ = remote.Close() })
	if _, err := remote.CreateDataChannel("other", nil); err != nil {
		t.Fatalf("CreateDataChannel(remote): %v", err)
	}
	remoteOffer, err := remote.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer(remote): %v", err)
	}

	out := make(chan *ss.SignallingEvent, 2)
	n.handleSessionDesc(context.Background(), &remoteOffer, out)
	if !n.ignoreOffer {
		t.Fatal("impolite peer did not ignore the colliding remote offer")
	}
	if len(out) != 0 || n.glareRetryTimer == nil {
		t.Fatal("ordinary glare must schedule, not immediately emit, a retry")
	}
	n.handleGlareRetryTimer(context.Background(), out)
	if len(out) != 1 {
		t.Fatalf("unresolved glare emitted %d events, want the local offer retransmitted", len(out))
	}
	if n.glareRetryTimer == nil {
		t.Fatal("unresolved glare did not retain its spaced retry")
	}
	ev := <-out
	if ev.C2CEv == nil || ev.C2CEv.SessionDesc == nil ||
		sdpICECredentials(ev.C2CEv.SessionDesc.SDP) != sdpICECredentials(localOffer.SDP) {
		t.Fatalf("glare response=%+v, want pending local offer", ev)
	}
}

func TestICERestartRetransmitsUnansweredRestartOffer(t *testing.T) {
	for _, tc := range []struct {
		name              string
		deliverFirstOffer bool
	}{
		{name: "first offer lost", deliverFirstOffer: false},
		{name: "first answer lost", deliverFirstOffer: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, pc := newICERestartUnitNegotiator(t)
			remote, err := webrtc.NewPeerConnection(webrtc.Configuration{})
			if err != nil {
				t.Fatalf("NewPeerConnection(remote): %v", err)
			}
			t.Cleanup(func() { _ = remote.Close() })
			if _, err := pc.CreateDataChannel("chat", nil); err != nil {
				t.Fatalf("CreateDataChannel: %v", err)
			}
			initialOffer, err := pc.CreateOffer(nil)
			if err != nil {
				t.Fatalf("CreateOffer(initial): %v", err)
			}
			if err := pc.SetLocalDescription(initialOffer); err != nil {
				t.Fatalf("SetLocalDescription(initial): %v", err)
			}
			if err := remote.SetRemoteDescription(initialOffer); err != nil {
				t.Fatalf("remote.SetRemoteDescription(initial): %v", err)
			}
			initialAnswer, err := remote.CreateAnswer(nil)
			if err != nil {
				t.Fatalf("CreateAnswer(initial): %v", err)
			}
			if err := remote.SetLocalDescription(initialAnswer); err != nil {
				t.Fatalf("remote.SetLocalDescription(initial): %v", err)
			}
			if err := pc.SetRemoteDescription(initialAnswer); err != nil {
				t.Fatalf("SetRemoteDescription(initial): %v", err)
			}

			answer := func(offer *ss.SignallingEvent) {
				t.Helper()
				if err := remote.SetRemoteDescription(*offer.C2CEv.SessionDesc); err != nil {
					t.Fatalf("remote.SetRemoteDescription(restart): %v", err)
				}
				description, err := remote.CreateAnswer(nil)
				if err != nil {
					t.Fatalf("CreateAnswer(restart): %v", err)
				}
				if err := remote.SetLocalDescription(description); err != nil {
					t.Fatalf("remote.SetLocalDescription(restart): %v", err)
				}
			}

			out := make(chan *ss.SignallingEvent, 4)
			n.iceConnectedOnce = true
			n.iceRecovering = true
			n.handleICERestartTimer(context.Background(), out)
			if len(out) != 1 {
				t.Fatalf("first restart emitted %d events, want one offer", len(out))
			}
			first := <-out
			if first.C2CEv == nil || first.C2CEv.SessionDesc == nil || first.C2CEv.SessionDesc.Type != webrtc.SDPTypeOffer {
				t.Fatalf("first event=%+v, want restart offer", first)
			}
			if tc.deliverFirstOffer {
				answer(first) // the answer itself is not delivered to n
			}

			// Pion cannot create another offer in have-local-offer. The next
			// watchdog must retransmit it, prompting an answer even when the
			// remote already answered the first copy.
			n.handleICERestartTimer(context.Background(), out)
			if len(out) != 1 {
				t.Fatalf("retry emitted %d events, want the pending offer retransmitted", len(out))
			}
			second := <-out
			if second.C2CEv == nil || second.C2CEv.SessionDesc == nil ||
				second.C2CEv.SessionDesc.Type != webrtc.SDPTypeOffer ||
				sdpICECredentials(second.C2CEv.SessionDesc.SDP) != sdpICECredentials(first.C2CEv.SessionDesc.SDP) {
				t.Fatalf("retransmission=%+v, want the same offer/credentials (newly gathered candidates are allowed)", second)
			}

			answer(second)
			n.handleSessionDesc(context.Background(), remote.LocalDescription(), out)
			if len(out) != 0 {
				t.Fatal("answering the retransmitted restart offer triggered a redundant new offer")
			}
			if pc.SignalingState() != webrtc.SignalingStateStable {
				t.Fatalf("local signaling state=%s, want stable", pc.SignalingState())
			}
		})
	}
}

func TestICERestartTimerRechecksHealthyState(t *testing.T) {
	for _, state := range []webrtc.ICEConnectionState{
		webrtc.ICEConnectionStateConnected,
		webrtc.ICEConnectionStateCompleted,
	} {
		t.Run(state.String(), func(t *testing.T) {
			n, _ := newICERestartUnitNegotiator(t)
			n.iceStateReader = func() webrtc.ICEConnectionState { return state }
			n.iceConnectedOnce = true
			n.iceRecovering = true
			n.iceRestartPending = true
			n.iceRestartAttempts = 1
			out := make(chan *ss.SignallingEvent, 1)
			n.handleICERestartTimer(context.Background(), out)
			if n.iceRecovering || n.iceRestartPending || n.iceRestartAttempts != 0 {
				t.Fatal("timer did not recognize the already-recovered transport")
			}
			if len(out) != 0 {
				t.Fatal("timer restarted an already-healthy transport")
			}
		})
	}
}

func TestICERestartOfferRunsWhenAnswerRestoresStable(t *testing.T) {
	n, pc := newICERestartUnitNegotiator(t)
	remote, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("NewPeerConnection(remote): %v", err)
	}
	t.Cleanup(func() { _ = remote.Close() })
	if _, err := pc.CreateDataChannel("chat", nil); err != nil {
		t.Fatalf("CreateDataChannel: %v", err)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("SetLocalDescription(offer): %v", err)
	}
	before := sdpICECredentials(pc.LocalDescription().SDP)
	if err := remote.SetRemoteDescription(offer); err != nil {
		t.Fatalf("remote.SetRemoteDescription: %v", err)
	}
	answer, err := remote.CreateAnswer(nil)
	if err != nil {
		t.Fatalf("CreateAnswer: %v", err)
	}
	if err := remote.SetLocalDescription(answer); err != nil {
		t.Fatalf("remote.SetLocalDescription: %v", err)
	}

	out := make(chan *ss.SignallingEvent, 1)
	n.iceConnectedOnce = true
	n.iceRecovering = true
	n.handleICERestartTimer(context.Background(), out)
	if !n.iceRestartPending || len(out) != 1 {
		t.Fatal("ordinary offer was not retransmitted with the restart still latched")
	}
	<-out // the retransmitted ordinary offer, not a new ICE-restart offer
	n.handleSessionDesc(context.Background(), remote.LocalDescription(), out)
	if n.iceRestartPending {
		t.Fatal("restart remained pending after its offer was sent")
	}
	select {
	case ev := <-out:
		if ev.C2CEv == nil || ev.C2CEv.SessionDesc == nil || ev.C2CEv.SessionDesc.Type != webrtc.SDPTypeOffer {
			t.Fatalf("event=%+v, want an ICE-restart offer", ev)
		}
		if got := sdpICECredentials(ev.C2CEv.SessionDesc.SDP); got == before || got == "" {
			t.Fatalf("restart credentials did not change:\nbefore:\n%s\nafter:\n%s", before, got)
		}
	default:
		t.Fatal("no restart offer followed the answer that restored stable signaling")
	}
}

func TestICERestartIgnoresStaleConnectionState(t *testing.T) {
	n, pc := newICERestartUnitNegotiator(t)
	other, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("NewPeerConnection(other): %v", err)
	}
	t.Cleanup(func() { _ = other.Close() })

	connected := webrtc.ICEConnectionStateConnected
	n.handleLocal(context.Background(), localEvent{source: pc, iceState: &connected}, nil)
	failed := webrtc.ICEConnectionStateFailed
	n.handleLocal(context.Background(), localEvent{source: other, iceState: &failed}, nil)
	if n.iceRecovering || n.iceRestartTimer != nil {
		t.Fatal("a replaced connection's failure started recovery")
	}
	n.handleLocal(context.Background(), localEvent{source: pc, iceState: &failed}, nil)
	if !n.iceRecovering {
		t.Fatal("the current connection's failure did not start recovery")
	}
}

func TestICERestartIgnoresStaleNegotiationAndCandidates(t *testing.T) {
	n, pc := newICERestartUnitNegotiator(t)
	other, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("NewPeerConnection(other): %v", err)
	}
	t.Cleanup(func() { _ = other.Close() })
	out := make(chan *ss.SignallingEvent, 2)

	n.handleLocal(context.Background(), localEvent{source: other, negotiationNeeded: true}, out)
	n.handleLocal(context.Background(), localEvent{source: other}, out) // old end-of-candidates
	if len(out) != 0 {
		t.Fatal("a replaced connection emitted negotiation traffic")
	}
	n.handleLocal(context.Background(), localEvent{source: pc}, out)
	if len(out) != 1 {
		t.Fatal("the current connection's candidate was not relayed")
	}
}

func sdpICECredentials(sdp string) string {
	var credentials []string
	for line := range strings.Lines(sdp) {
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "a=ice-ufrag:") || strings.HasPrefix(line, "a=ice-pwd:") {
			credentials = append(credentials, line)
		}
	}
	return strings.Join(credentials, "\n")
}

func waitForNewICECredentials(t *testing.T, client *testClient, before string) *webrtc.PeerConnection {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for {
		current := client.currentPC()
		if local := current.LocalDescription(); local != nil {
			if got := sdpICECredentials(local.SDP); got != before {
				if got == "" {
					t.Fatalf("ICE restart produced no credentials")
				}
				return current
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for new ICE credentials; before:\n%s", before)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForICERestart(t *testing.T, client *testClient, original *webrtc.PeerConnection, before string) {
	t.Helper()
	if current := waitForNewICECredentials(t, client, before); current != original {
		t.Fatal("one-sided ICE restart replaced the peer connection")
	}
}

func TestPerfectNegotiatorRestartsICEAndKeepsDataChannel(t *testing.T) {
	for _, tc := range []struct {
		name     string
		state    webrtc.ICEConnectionState
		grace    time.Duration
		impolite bool
	}{
		{name: "polite/disconnected", state: webrtc.ICEConnectionStateDisconnected, grace: 20 * time.Millisecond},
		{name: "polite/failed", state: webrtc.ICEConnectionStateFailed, grace: time.Hour},
		{name: "impolite/disconnected", state: webrtc.ICEConnectionStateDisconnected, grace: 20 * time.Millisecond, impolite: true},
		{name: "impolite/failed", state: webrtc.ICEConnectionStateFailed, grace: time.Hour, impolite: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			net := newSSTestNet(t, "alice", "bob")
			initiator := net.a
			if tc.impolite {
				initiator = net.b
			}
			initiator.nego.iceDisconnectGrace = tc.grace
			initiator.nego.iceStateReader = func() webrtc.ICEConnectionState { return tc.state }
			net.a.register(t)
			net.b.register(t)
			net.startNegotiators(t)
			net.a.createDataChannel(t, "chat")
			net.a.waitConnected(t)
			net.b.waitConnected(t)
			aChat := net.a.localDC(t, "chat")
			bChat := net.b.remoteDC(t, "chat")

			pc := initiator.currentPC()
			local := pc.LocalDescription()
			if local == nil {
				t.Fatal("connected peer has no local description")
			}
			before := sdpICECredentials(local.SDP)
			if before == "" {
				t.Fatal("connected peer has no ICE credentials")
			}

			connected := webrtc.ICEConnectionStateConnected
			failed := tc.state
			initiator.nego.local <- localEvent{source: pc, iceState: &connected}
			initiator.nego.local <- localEvent{source: pc, iceState: &failed}
			waitForICERestart(t, initiator, pc, before)
			// Stop the synthetic failure's watchdog; the real implementation gets
			// this event from pion when the restarted transport reconnects.
			initiator.nego.local <- localEvent{source: pc, iceState: &connected}

			assertDCMessage(t, aChat, bChat, "data survives ICE restart")
			assertDCMessage(t, bChat, aChat, "both directions survive")
		})
	}
}

func TestPerfectNegotiatorRecoversFromSimultaneousICEFailure(t *testing.T) {
	net := newSSTestNet(t, "alice", "bob")
	for _, client := range []*testClient{net.a, net.b} {
		client.nego.iceStateReader = func() webrtc.ICEConnectionState {
			return webrtc.ICEConnectionStateFailed
		}
		client.nego.iceRetryBase = time.Hour
		client.nego.iceRetryMax = time.Hour
	}
	net.a.register(t)
	net.b.register(t)
	net.startNegotiators(t)
	net.a.createDataChannel(t, "chat")
	net.a.waitConnected(t)
	net.b.waitConnected(t)
	net.a.localDC(t, "chat")
	net.b.remoteDC(t, "chat")

	before := make(map[*testClient]string)
	original := make(map[*testClient]*webrtc.PeerConnection)
	for _, client := range []*testClient{net.a, net.b} {
		original[client] = client.currentPC()
		local := original[client].LocalDescription()
		if local == nil {
			t.Fatalf("%s has no local description", client.sub)
		}
		before[client] = sdpICECredentials(local.SDP)
	}
	connected := webrtc.ICEConnectionStateConnected
	failed := webrtc.ICEConnectionStateFailed
	for _, client := range []*testClient{net.a, net.b} {
		pc := client.currentPC()
		client.nego.local <- localEvent{source: pc, iceState: &connected}
		client.nego.local <- localEvent{source: pc, iceState: &failed}
	}
	for _, client := range []*testClient{net.a, net.b} {
		waitForICERestart(t, client, original[client], before[client])
	}
	for _, client := range []*testClient{net.a, net.b} {
		client.nego.local <- localEvent{source: client.currentPC(), iceState: &connected}
	}

	// Only the polite peer offered a restart; the impolite peer requested it.
	// Neither peer needed the established-session glare-rebuild path.
	aChat := net.a.localDC(t, "chat")
	bChat := net.b.remoteDC(t, "chat")
	assertDCMessage(t, aChat, bChat, "simultaneous restart recovered")
	assertDCMessage(t, bChat, aChat, "simultaneous recovery is bidirectional")
}
