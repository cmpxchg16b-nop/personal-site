package rtc

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"personal-site/pkg/models/ss"
)

// The client tests wire HeadlessRTCClients to a real
// SimpleOnMemorySSProvider over plain channel pairs — the transport seam
// — stamping From like the server-side WebSocket handler does. All state
// exchange between the clients flows through the provider's c2c relay.

// TestDefaultPeerConnectionFactoryAnsweringRole pins the DTLS role
// contract of the peer-connection factory: the polite peer — the
// session's initial offerer — is the DTLS server, so its answers must
// carry setup:passive; the impolite peer's answers carry setup:active.
// pion's defaults answer an actpass offer with active either way,
// flipping the polite peer's role on every renegotiation — which a
// browser refuses ("failed to set SSL role for the transport"), leaving
// it stuck in have-local-offer and breaking every later renegotiation.
func TestDefaultPeerConnectionFactoryAnsweringRole(t *testing.T) {
	for _, tc := range []struct {
		name   string
		polite bool
		want   string
	}{
		{"the polite peer answers passive", true, "a=setup:passive"},
		{"the impolite peer answers active", false, "a=setup:active"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			factory := defaultPeerConnectionFactory(nil)
			pc, err := factory(tc.polite)
			if err != nil {
				t.Fatalf("factory: %v", err)
			}
			defer pc.Close()
			probe, err := webrtc.NewPeerConnection(webrtc.Configuration{})
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			defer probe.Close()
			if _, err := probe.CreateDataChannel("probe", nil); err != nil {
				t.Fatalf("probe CreateDataChannel: %v", err)
			}
			offer, err := probe.CreateOffer(nil)
			if err != nil {
				t.Fatalf("probe CreateOffer: %v", err)
			}
			if err := pc.SetRemoteDescription(offer); err != nil {
				t.Fatalf("SetRemoteDescription: %v", err)
			}
			answer, err := pc.CreateAnswer(nil)
			if err != nil {
				t.Fatalf("CreateAnswer: %v", err)
			}
			if err := pc.SetLocalDescription(answer); err != nil {
				t.Fatalf("SetLocalDescription: %v", err)
			}
			if got := pc.LocalDescription(); got == nil || !strings.Contains(got.SDP, tc.want) {
				t.Fatalf("the answer carries no %q: %v", tc.want, got)
			}
		})
	}
}

// clientTestNet routes between any number of clients and one
// SimpleOnMemorySSProvider, the way the real transport does: each
// client's outbound events are stamped with its address and forwarded to
// the provider, and the provider's outbound events are routed to the
// client whose registered address they are addressed to.
type clientTestNet struct {
	ctx     context.Context
	prov    *ss.SimpleOnMemorySSProvider
	provIn  chan *ss.SignallingEvent
	provOut chan *ss.SignallingEvent

	mu     sync.Mutex
	routes map[ss.UserId]chan *ss.SignallingEvent
}

// newClientTestNet starts the provider (with the given subscriber aging)
// and the router. Everything is torn down via t.Cleanup.
func newClientTestNet(t *testing.T, aging time.Duration) *clientTestNet {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	net := &clientTestNet{
		ctx:     ctx,
		prov:    ss.NewSimpleOnMemorySSProviderWithAging(aging),
		provIn:  make(chan *ss.SignallingEvent, 64),
		provOut: make(chan *ss.SignallingEvent, 64),
		routes:  map[ss.UserId]chan *ss.SignallingEvent{},
	}
	go net.prov.Run(ctx, net.provIn, net.provOut)
	t.Cleanup(net.prov.Shutdown)
	t.Cleanup(cancel)
	go func() {
		for ev := range net.provOut {
			net.mu.Lock()
			route := net.routes[ev.To.UserId]
			net.mu.Unlock()
			if route == nil {
				continue
			}
			select {
			case route <- ev:
			case <-ctx.Done():
				return
			}
		}
		// The provider ended: close every route so the clients' runs end.
		net.mu.Lock()
		for _, route := range net.routes {
			close(route)
		}
		net.mu.Unlock()
	}()
	return net
}

// connect attaches one client identity to the net and returns the channel
// pair the client Runs on: (client → server, server → client).
func (net *clientTestNet) connect(name string) (toSS chan<- *ss.SignallingEvent, fromSS <-chan *ss.SignallingEvent) {
	addr := ss.EPAddr{UserId: ss.UserId("u-" + name), UserSessionId: ss.UserSessionId("s-" + name)}
	to := make(chan *ss.SignallingEvent, 64)
	from := make(chan *ss.SignallingEvent, 64)
	net.mu.Lock()
	net.routes[addr.UserId] = from
	net.mu.Unlock()
	go func() {
		for {
			select {
			case ev, ok := <-to:
				if !ok {
					return
				}
				ev.From = addr // populated server-side on the real transport
				if ev.C2SEv != nil && ev.C2SEv.Register != nil {
					// ...and so is the registration's username: the
					// session's, here the client's name.
					ev.C2SEv.Register.Username = name
				}
				select {
				case net.provIn <- ev:
				case <-net.ctx.Done():
					return
				}
			case <-net.ctx.Done():
				return
			}
		}
	}()
	return to, from
}

// startTestClient builds a HeadlessRTCClient, connects it to the net,
// and Runs it; the returned cancel ends the run. configure may adjust
// the configuration (e.g. a fixed subscriber id).
func startTestClient(t *testing.T, net *clientTestNet, name string, configure func(*RTCClientConfiguration)) (*HeadlessRTCClient, context.CancelFunc) {
	t.Helper()
	toSS, fromSS := net.connect(name)
	config := RTCClientConfiguration{
		KeepAliveInterval:  50 * time.Millisecond,
		MemberListInterval: 50 * time.Millisecond,
		ReplyTimeout:       2 * time.Second,
		Logger:             testLogger(t),
	}
	if configure != nil {
		configure(&config)
	}
	c, err := NewHeadlessRTCClient(PerfectNegotiatorFactory, config)
	if err != nil {
		t.Fatalf("NewHeadlessRTCClient(%s): %v", name, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		if err := c.Run(ctx, fromSS, toSS); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("client %s: Run: %v", name, err)
		}
	}()
	return c, cancel
}

// waitFor fails the test unless cond becomes true within testTimeout.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitRegistered waits for the client's registration and returns its
// subscriber id.
func waitRegistered(t *testing.T, c *HeadlessRTCClient) ss.SubscriberId {
	t.Helper()
	waitFor(t, "client registration", func() bool { return c.SubscriberId() != "" })
	return c.SubscriberId()
}

// dcProbe is a DataChannelHandler that records every invocation, the
// channel once it opens, and every received text message, and echoes
// every text message back with an "echo:" prefix — so the other end
// observes the round trip.
type dcProbe struct {
	mu     sync.Mutex
	calls  map[ss.SubscriberId]int
	opened map[ss.SubscriberId]*webrtc.DataChannel
	got    map[ss.SubscriberId][]string
}

func newDCProbe() *dcProbe {
	return &dcProbe{
		calls:  map[ss.SubscriberId]int{},
		opened: map[ss.SubscriberId]*webrtc.DataChannel{},
		got:    map[ss.SubscriberId][]string{},
	}
}

func (p *dcProbe) handler() DataChannelHandler {
	return DataChannelHandlerFunc(func(ctx context.Context, channelId ss.ChannelId, peer ss.SubscriberId, dc *webrtc.DataChannel) {
		p.mu.Lock()
		p.calls[peer]++
		p.mu.Unlock()
		recordOpen := func() {
			p.mu.Lock()
			p.opened[peer] = dc
			p.mu.Unlock()
		}
		dc.OnOpen(recordOpen)
		if dc.ReadyState() == webrtc.DataChannelStateOpen {
			recordOpen()
		}
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			if !msg.IsString {
				return
			}
			text := string(msg.Data)
			p.mu.Lock()
			p.got[peer] = append(p.got[peer], text)
			p.mu.Unlock()
			if !strings.HasPrefix(text, "echo:") {
				_ = dc.SendText("echo:" + text)
			}
		})
	})
}

// waitOpened waits until the probe's channel with peer has opened and
// returns it.
func (p *dcProbe) waitOpened(t *testing.T, peer ss.SubscriberId) *webrtc.DataChannel {
	t.Helper()
	waitFor(t, fmt.Sprintf("channel with %s to open", peer), func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.opened[peer] != nil
	})
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.opened[peer]
}

// waitGot waits until the probe received the exact text from peer.
func (p *dcProbe) waitGot(t *testing.T, peer ss.SubscriberId, text string) {
	t.Helper()
	waitFor(t, fmt.Sprintf("message %q from %s", text, peer), func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return slices.Contains(p.got[peer], text)
	})
}

// waitCalls waits until the probe was invoked n times for peer.
func (p *dcProbe) waitCalls(t *testing.T, peer ss.SubscriberId, n int) {
	t.Helper()
	waitFor(t, fmt.Sprintf("%d handler invocations for %s", n, peer), func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.calls[peer] == n
	})
}

// empty reports whether the probe was never invoked.
func (p *dcProbe) empty() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls) == 0
}

// assertPanics fails the test unless f panics.
func assertPanics(t *testing.T, what string, f func()) {
	t.Helper()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("%s did not panic", what)
		}
	}()
	f()
}

// TestHeadlessRTCClientConnectsToPeersThroughSS is the full-stack happy
// path: two clients register, discover each other through the member
// listing, negotiate through the provider's c2c relay, and exchange data
// channel messages both ways.
func TestHeadlessRTCClientConnectsToPeersThroughSS(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot, _ := startTestClient(t, net, "bot", nil)
	user, _ := startTestClient(t, net, "user", nil)
	botProbe := newDCProbe()
	userProbe := newDCProbe()
	bot.HandleDataChannel("dcmsg", botProbe.handler())
	user.HandleDataChannel("dcmsg", userProbe.handler())

	botId := waitRegistered(t, bot)
	userId := waitRegistered(t, user)
	waitFor(t, "bot to hold a session with the user", func() bool {
		return slices.Equal(bot.Peers(), []ss.SubscriberId{userId})
	})
	waitFor(t, "user to hold a session with the bot", func() bool {
		return slices.Equal(user.Peers(), []ss.SubscriberId{botId})
	})

	botDC := botProbe.waitOpened(t, userId)
	userDC := userProbe.waitOpened(t, botId)

	if err := botDC.SendText("hello from bot"); err != nil {
		t.Fatalf("bot SendText: %v", err)
	}
	userProbe.waitGot(t, botId, "hello from bot")
	botProbe.waitGot(t, userId, "echo:hello from bot")

	if err := userDC.SendText("hi from user"); err != nil {
		t.Fatalf("user SendText: %v", err)
	}
	botProbe.waitGot(t, userId, "hi from user")
	userProbe.waitGot(t, botId, "echo:hi from user")
}

// TestHeadlessRTCClientRegistersSubscriberId covers registration: an
// empty configured subscriber id is assigned by the SS from the automatic
// assignment range; a configured id is kept. When the run ends, the
// subscriber id goes with it.
func TestHeadlessRTCClientRegistersSubscriberId(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	auto, stopAuto := startTestClient(t, net, "auto", nil)
	fixed, _ := startTestClient(t, net, "fixed", func(c *RTCClientConfiguration) {
		c.SubscriberId = "bot-fixed"
	})

	if id := waitRegistered(t, auto); id != "" {
		n, err := strconv.Atoi(string(id))
		if err != nil || n < 1000 || n >= 2000 {
			t.Fatalf("auto-assigned subscriber id is %q, want one in 1000-1999", id)
		}
	}
	if id := waitRegistered(t, fixed); id != "bot-fixed" {
		t.Fatalf("fixed subscriber id is %q, want %q", id, "bot-fixed")
	}

	stopAuto()
	waitFor(t, "the auto client's id to go with its run", func() bool {
		return auto.SubscriberId() == ""
	})
}

// TestHeadlessRTCClientRetriesRegistrationWithFreshId covers the
// registration retry: a configured subscriber id that is already
// registered to another tuple is retried once with an empty id, which the
// SS assigns from the automatic range — mirroring the browser client.
func TestHeadlessRTCClientRetriesRegistrationWithFreshId(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	first, _ := startTestClient(t, net, "first", func(c *RTCClientConfiguration) {
		c.SubscriberId = "dup"
	})
	if id := waitRegistered(t, first); id != "dup" {
		t.Fatalf("first client's id is %q, want %q", id, "dup")
	}
	second, _ := startTestClient(t, net, "second", func(c *RTCClientConfiguration) {
		c.SubscriberId = "dup"
	})
	id := waitRegistered(t, second)
	n, err := strconv.Atoi(string(id))
	if err != nil || n < 1000 || n >= 2000 {
		t.Fatalf("second client's id is %q, want an assigned one in 1000-1999", id)
	}
}

// TestHeadlessRTCClientReconcilesMemberDropout runs a client and a peer
// on a provider with short aging, stops the peer, and expects the
// client's session to be torn down once the peer's registration ages out
// and the listing sweeps it.
func TestHeadlessRTCClientReconcilesMemberDropout(t *testing.T) {
	net := newClientTestNet(t, 300*time.Millisecond)
	bot, _ := startTestClient(t, net, "bot", nil)
	peer, stopPeer := startTestClient(t, net, "peer", nil)

	botId := waitRegistered(t, bot)
	peerId := waitRegistered(t, peer)
	waitFor(t, "bot to hold a session with the peer", func() bool {
		return slices.Contains(bot.Peers(), peerId)
	})
	waitFor(t, "the peer to hold a session with the bot", func() bool {
		return slices.Contains(peer.Peers(), botId)
	})

	stopPeer()
	waitFor(t, "bot to drop the departed peer's session", func() bool {
		return !slices.Contains(bot.Peers(), peerId)
	})
}

// TestHeadlessRTCClientLateHandlerRegistrationOnPoliteSide covers
// HandleDataChannel after sessions exist, on the polite side (the smaller
// subscriber id creates the channels): no channel exists while the polite
// side has no handler for the label; registering it creates the channel
// on the existing session and delivers it on both ends.
func TestHeadlessRTCClientLateHandlerRegistrationOnPoliteSide(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot, _ := startTestClient(t, net, "bot", func(c *RTCClientConfiguration) {
		c.SubscriberId = "1-bot" // the smaller id: polite
	})
	user, _ := startTestClient(t, net, "user", func(c *RTCClientConfiguration) {
		c.SubscriberId = "2-user"
	})
	userProbe := newDCProbe()
	user.HandleDataChannel("dcmsg", userProbe.handler())

	botId := waitRegistered(t, bot)
	userId := waitRegistered(t, user)
	waitFor(t, "both clients to hold a session", func() bool {
		return slices.Contains(bot.Peers(), userId) && slices.Contains(user.Peers(), botId)
	})

	// The polite side has no handler for the label: nothing was created,
	// so nothing was negotiated.
	time.Sleep(200 * time.Millisecond)
	if !userProbe.empty() {
		t.Fatal("the user's handler was invoked before the polite side registered one")
	}

	botProbe := newDCProbe()
	bot.HandleDataChannel("dcmsg", botProbe.handler())

	botDC := botProbe.waitOpened(t, userId)
	userProbe.waitOpened(t, botId)
	if err := botDC.SendText("created late"); err != nil {
		t.Fatalf("bot SendText: %v", err)
	}
	userProbe.waitGot(t, botId, "created late")
}

// TestHeadlessRTCClientLateHandlerRegistrationOnImpoliteSide covers the
// mirror image: the impolite side keeps a channel announced for a label
// without a handler, and registering the handler later delivers the kept
// channel — no new channel is created.
func TestHeadlessRTCClientLateHandlerRegistrationOnImpoliteSide(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	user, _ := startTestClient(t, net, "user", func(c *RTCClientConfiguration) {
		c.SubscriberId = "1-user" // the smaller id: polite
	})
	bot, _ := startTestClient(t, net, "bot", func(c *RTCClientConfiguration) {
		c.SubscriberId = "2-bot"
	})
	userProbe := newDCProbe()
	user.HandleDataChannel("dcmsg", userProbe.handler())

	userId := waitRegistered(t, user)
	botId := waitRegistered(t, bot)
	waitFor(t, "both clients to hold a session", func() bool {
		return slices.Contains(bot.Peers(), userId) && slices.Contains(user.Peers(), botId)
	})

	// The polite user's channel comes up; the impolite bot receives the
	// announcement but has no handler — and must not be dispatched one.
	userProbe.waitOpened(t, botId)
	time.Sleep(200 * time.Millisecond)
	botProbe := newDCProbe()
	if !botProbe.empty() {
		t.Fatal("the bot's handler was invoked before it was registered")
	}

	bot.HandleDataChannel("dcmsg", botProbe.handler())
	userDC := userProbe.waitOpened(t, botId)
	botProbe.waitOpened(t, userId)
	if err := userDC.SendText("announced early"); err != nil {
		t.Fatalf("user SendText: %v", err)
	}
	botProbe.waitGot(t, userId, "announced early")
}

// TestHeadlessRTCClientSurvivesGlare stages the offer collision through
// the whole client: the bot (polite) has a pending offer out when the
// peer (impolite, a scripted raw client — not a HeadlessRTCClient) sends
// its own offer. The bot's negotiator yields by rebuilding the peer
// connection through the hub, the session's channels are re-created and
// re-delivered to the handler, and the rebuilt connection carries data.
func TestHeadlessRTCClientSurvivesGlare(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot, _ := startTestClient(t, net, "bot", func(c *RTCClientConfiguration) {
		c.SubscriberId = "1-bot" // the smaller id: polite — the one that yields
	})
	botProbe := newDCProbe()
	bot.HandleDataChannel("dcmsg", botProbe.handler())
	botId := waitRegistered(t, bot)

	// The raw peer: registers by hand and drives the wire protocol
	// directly, so the test controls the collision exactly.
	toSS, fromSS := net.connect("peer")
	sendC2S := func(payload *ss.ClientToSSEv) {
		t.Helper()
		select {
		case toSS <- &ss.SignallingEvent{
			To:    ss.EPAddr{ServiceId: ss.WellKnownSvcIdSS},
			MsgId: newMsgId(),
			C2SEv: payload,
		}:
		case <-net.ctx.Done():
			t.Fatal("net ended")
		}
	}
	sendC2C := func(payload *ss.ClientToClientEv) {
		t.Helper()
		payload.FromSubscriber = "2-peer"
		payload.ToSubscriber = botId
		payload.ChannelId = ss.WellKnownChIdMain
		select {
		case toSS <- &ss.SignallingEvent{
			To:    ss.EPAddr{ServiceId: ss.WellKnownSvcIdSS},
			MsgId: newMsgId(),
			C2CEv: payload,
		}:
		case <-net.ctx.Done():
			t.Fatal("net ended")
		}
	}
	sendC2S(&ss.ClientToSSEv{Register: &ss.ClientToSSRegEv{
		SubscriberId: "2-peer",
		ChannelId:    ss.WellKnownChIdMain,
		Username:     "peer",
	}})

	peerPC, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("NewPeerConnection: %v", err)
	}
	t.Cleanup(func() { _ = peerPC.Close() })
	announced := make(chan *webrtc.DataChannel, 8)
	peerPC.OnDataChannel(func(dc *webrtc.DataChannel) { announced <- dc })
	peerPC.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		init := webrtc.ICECandidateInit{}
		if candidate != nil {
			init = candidate.ToJSON()
		}
		sendC2C(&ss.ClientToClientEv{RTCICECandidate: &init})
	})

	// One loop drains the raw peer's inbound events: session descriptions
	// are scripted by the steps below, candidates are applied as they come
	// (stale ones — from the bot's replaced connection — fail and are
	// ignored, by design).
	descs := make(chan *webrtc.SessionDescription, 8)
	go func() {
		for {
			select {
			case ev := <-fromSS:
				if ev.C2CEv == nil || ev.C2CEv.FromSubscriber != botId {
					continue
				}
				switch {
				case ev.C2CEv.SessionDesc != nil:
					descs <- ev.C2CEv.SessionDesc
				case ev.C2CEv.RTCICECandidate != nil:
					_ = peerPC.AddICECandidate(*ev.C2CEv.RTCICECandidate)
				}
			case <-net.ctx.Done():
				return
			}
		}
	}()
	nextDesc := func() *webrtc.SessionDescription {
		t.Helper()
		select {
		case desc := <-descs:
			return desc
		case <-time.After(testTimeout):
			t.Fatal("timed out waiting for a session description from the bot")
			return nil
		}
	}

	// Step 1: the bot discovers the peer through the listing, creates its
	// channel, and sends an offer. The raw peer is impolite: it ignores
	// the offer and answers with its own — the collision.
	offer := nextDesc()
	if offer.Type != webrtc.SDPTypeOffer {
		t.Fatalf("the bot's first session description is %s, want an offer", offer.Type)
	}
	if _, err := peerPC.CreateDataChannel("raw", nil); err != nil {
		t.Fatalf("CreateDataChannel: %v", err)
	}
	theirOffer, err := peerPC.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	if err := peerPC.SetLocalDescription(theirOffer); err != nil {
		t.Fatalf("SetLocalDescription: %v", err)
	}
	sendC2C(&ss.ClientToClientEv{SessionDesc: peerPC.LocalDescription()})

	// Step 2: the bot yields — rebuilds its peer connection through the
	// hub — and answers the colliding offer.
	answer := nextDesc()
	if answer.Type != webrtc.SDPTypeAnswer {
		t.Fatalf("the bot's reply to the collision is %s, want an answer", answer.Type)
	}
	if err := peerPC.SetRemoteDescription(*answer); err != nil {
		t.Fatalf("SetRemoteDescription(answer): %v", err)
	}

	// Step 3: the rebuilt connection re-created the session's channel.
	// Whether the bot re-offers it depends on pion's negotiation-needed
	// timing — data channels multiplex over the single SCTP m-line the
	// answer already established, so the channel opens either way.
	// Answer the re-offer when it comes.
	reofferWindow := time.After(500 * time.Millisecond)
	for done := false; !done; {
		select {
		case desc := <-descs:
			if desc.Type != webrtc.SDPTypeOffer {
				continue
			}
			if err := peerPC.SetRemoteDescription(*desc); err != nil {
				t.Fatalf("SetRemoteDescription(reoffer): %v", err)
			}
			reanswer, err := peerPC.CreateAnswer(nil)
			if err != nil {
				t.Fatalf("CreateAnswer: %v", err)
			}
			if err := peerPC.SetLocalDescription(reanswer); err != nil {
				t.Fatalf("SetLocalDescription(reanswer): %v", err)
			}
			sendC2C(&ss.ClientToClientEv{SessionDesc: peerPC.LocalDescription()})
		case <-reofferWindow:
			done = true
		}
	}

	// The handler was invoked twice for the session: once for the
	// original channel, once for the rebuilt one.
	botProbe.waitCalls(t, "2-peer", 2)

	// And the rebuilt connection carries data end to end.
	var peerDC *webrtc.DataChannel
	select {
	case peerDC = <-announced:
		if peerDC.Label() != "dcmsg" {
			t.Fatalf("the announced channel is %q, want %q", peerDC.Label(), "dcmsg")
		}
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for the bot's re-created channel")
	}
	opened := make(chan struct{})
	peerDC.OnOpen(func() { close(opened) })
	gotEcho := make(chan string, 1)
	peerDC.OnMessage(func(msg webrtc.DataChannelMessage) {
		if msg.IsString {
			gotEcho <- string(msg.Data)
		}
	})
	select {
	case <-opened:
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for the announced channel to open")
	}
	if err := peerDC.SendText("survived the glare"); err != nil {
		t.Fatalf("peer SendText: %v", err)
	}
	botProbe.waitGot(t, "2-peer", "survived the glare")
	select {
	case text := <-gotEcho:
		if text != "echo:survived the glare" {
			t.Fatalf("echo is %q, want %q", text, "echo:survived the glare")
		}
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for the bot's echo")
	}
}

// TestNewHeadlessRTCClientRejectsMisuse covers the constructor guard, the
// Run channel guard, and the http.ServeMux-style panics of
// HandleDataChannel.
func TestNewHeadlessRTCClientRejectsMisuse(t *testing.T) {
	if _, err := NewHeadlessRTCClient(nil, RTCClientConfiguration{}); !errors.Is(err, ErrNilNegotiatorFactory) {
		t.Fatalf("nil factory: got %v, want %v", err, ErrNilNegotiatorFactory)
	}

	c, err := NewHeadlessRTCClient(PerfectNegotiatorFactory, RTCClientConfiguration{})
	if err != nil {
		t.Fatalf("NewHeadlessRTCClient: %v", err)
	}
	if err := c.Run(context.Background(), nil, nil); !errors.Is(err, ErrNilSignallingChannels) {
		t.Fatalf("nil channels: got %v, want %v", err, ErrNilSignallingChannels)
	}

	noop := func(context.Context, ss.ChannelId, ss.SubscriberId, *webrtc.DataChannel) {}
	c.HandleDataChannelFunc("dcmsg", noop)
	assertPanics(t, "a duplicate label registration", func() { c.HandleDataChannelFunc("dcmsg", noop) })
	assertPanics(t, "a nil handler", func() { c.HandleDataChannel("dcbin", nil) })
}

// TestHeadlessRTCClientRunIsSingleFlight covers the double-Run guard.
func TestHeadlessRTCClientRunIsSingleFlight(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	c, _ := startTestClient(t, net, "bot", nil)
	waitRegistered(t, c)

	toSS, fromSS := net.connect("intruder")
	if err := c.Run(context.Background(), fromSS, toSS); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second Run: got %v, want %v", err, ErrAlreadyRunning)
	}
}

// TestHeadlessRTCClientRunEndsWhenInputCloses covers the clean end: the
// provider shuts down, the inbound channel closes, and Run returns nil.
func TestHeadlessRTCClientRunEndsWhenInputCloses(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	toSS, fromSS := net.connect("bot")
	c, err := NewHeadlessRTCClient(PerfectNegotiatorFactory, RTCClientConfiguration{
		KeepAliveInterval:  50 * time.Millisecond,
		MemberListInterval: 50 * time.Millisecond,
		ReplyTimeout:       2 * time.Second,
		Logger:             testLogger(t),
	})
	if err != nil {
		t.Fatalf("NewHeadlessRTCClient: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Run(context.Background(), fromSS, toSS) }()
	waitRegistered(t, c)

	net.prov.Shutdown()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil on a clean shutdown", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Run did not return after the provider shut down")
	}
	waitFor(t, "the subscriber id to go with the run", func() bool {
		return c.SubscriberId() == ""
	})
}
