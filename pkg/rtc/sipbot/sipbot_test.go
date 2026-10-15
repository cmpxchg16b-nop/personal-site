package sipbot

// The sip bot tests wire a bot-equipped client and a probe client to a
// real SimpleOnMemorySSProvider over plain channel pairs — the harness
// mirrors pkg/rtc/musicbot's suite (test-only scaffolding duplicated so
// the suites evolve independently). The probe speaks the wire protocols
// directly — raw JSON frames on dcmsg — so the bot is tested against
// the documented formats, not against its own codec. The SIP side is a
// fake PBX: a sipgo UAS on loopback that challenges REGISTER and INVITE
// with digest auth, answers calls with a hand-written SDP, and streams
// and records real RTP — so the B2BUA is proven end to end in-process:
// the webrtc-leg dialog verbs and media on one side, the SIP
// transactions and relayed RTP on the other.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/google/uuid"
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	pionmedia "github.com/pion/webrtc/v4/pkg/media"

	"personal-site/pkg/models/ss"
	"personal-site/pkg/rtc"
	"personal-site/pkg/rtc/msg_handler"
)

// testTimeout bounds every wait in the suite; under the race detector a
// pion handshake can take several seconds on a loaded machine, so hitting
// it means a real failure, not slowness.
const testTimeout = 30 * time.Second

func testLogger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
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
func waitRegistered(t *testing.T, c *rtc.HeadlessRTCClient) ss.SubscriberId {
	t.Helper()
	waitFor(t, "client registration", func() bool { return c.SubscriberId() != "" })
	return c.SubscriberId()
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
		prov:    ss.NewSimpleOnMemorySSProviderWithAging(aging, nil),
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

// startClient builds a HeadlessRTCClient, connects it to the net, and
// Runs it; configure may adjust the configuration (e.g. a fixed
// subscriber id). The returned cancel stops the client mid-test (a
// subscriber dropout); cleanup cancels anyway.
func startClient(t *testing.T, net *clientTestNet, name string, configure func(*rtc.RTCClientConfiguration)) (*rtc.HeadlessRTCClient, context.CancelFunc) {
	t.Helper()
	toSS, fromSS := net.connect(name)
	config := rtc.RTCClientConfiguration{
		KeepAliveInterval:  50 * time.Millisecond,
		MemberListInterval: 50 * time.Millisecond,
		ReplyTimeout:       2 * time.Second,
		Logger:             testLogger(t),
	}
	if configure != nil {
		configure(&config)
	}
	c, err := rtc.NewHeadlessRTCClient(rtc.PerfectNegotiatorFactory, config)
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

// startBot builds a client with the sip bot attached (on the loopback
// SIP transport, with the given /test-call contact — empty disables it —
// and the given credential pool — nil means none) and Runs it on the
// net.
func startBot(t *testing.T, net *clientTestNet, name string, id ss.SubscriberId, testContact string, pool *SIPCredentialPool) *rtc.HeadlessRTCClient {
	t.Helper()
	return startBotOn(t, net, name, id, testContact, pool, "127.0.0.1")
}

// startBotAuto is startBot without the IPv4-loopback pin: the accounts'
// sockets take their registrars' own address families (the production
// default), which the IPv6 test needs.
func startBotAuto(t *testing.T, net *clientTestNet, name string, id ss.SubscriberId, testContact string, pool *SIPCredentialPool) *rtc.HeadlessRTCClient {
	t.Helper()
	return startBotOn(t, net, name, id, testContact, pool, "")
}

// startBotOn builds a client with the sip bot attached, with the given
// SIP bind host ("": the per-account family selection).
func startBotOn(t *testing.T, net *clientTestNet, name string, id ss.SubscriberId, testContact string, pool *SIPCredentialPool, bindHost string) *rtc.HeadlessRTCClient {
	t.Helper()
	c, _ := startClient(t, net, name, func(c *rtc.RTCClientConfiguration) {
		c.SubscriberId = id
	})
	New(c, NewOnMemoryUserSessionStorage(), pool, Configuration{
		Logger:         testLogger(t),
		BindHost:       bindHost,
		TestSIPContact: testContact,
	})
	return c
}

// startBotWithPage builds a client with the sip bot attached, carrying
// the given yellow page.
func startBotWithPage(t *testing.T, net *clientTestNet, name string, id ss.SubscriberId, yellowPage []YellowPageSection) *rtc.HeadlessRTCClient {
	t.Helper()
	c, _ := startClient(t, net, name, func(c *rtc.RTCClientConfiguration) {
		c.SubscriberId = id
	})
	New(c, NewOnMemoryUserSessionStorage(), nil, Configuration{
		Logger:     testLogger(t),
		BindHost:   "127.0.0.1",
		YellowPage: yellowPage,
	})
	return c
}

// pairUp waits for both clients to be registered and to hold a session
// with each other, returning their subscriber ids.
func pairUp(t *testing.T, a, b *rtc.HeadlessRTCClient) (aId, bId ss.SubscriberId) {
	t.Helper()
	aId = waitRegistered(t, a)
	bId = waitRegistered(t, b)
	waitFor(t, "both clients to hold a session", func() bool {
		return slices.Contains(a.Peers(), bId) && slices.Contains(b.Peers(), aId)
	})
	return aId, bId
}

// wireProbe is a pair of rtc.DataChannelHandlers recording every opened
// channel and every raw frame, per peer.
type wireProbe struct {
	mu     sync.Mutex
	opened map[ss.SubscriberId]map[string]*webrtc.DataChannel // peer → label → channel
	texts  map[ss.SubscriberId][]string                       // dcmsg frames
}

func newWireProbe() *wireProbe {
	return &wireProbe{
		opened: map[ss.SubscriberId]map[string]*webrtc.DataChannel{},
		texts:  map[ss.SubscriberId][]string{},
	}
}

func (p *wireProbe) handler(label string) rtc.DataChannelHandler {
	return rtc.DataChannelHandlerFunc(func(ctx context.Context, channelId ss.ChannelId, peer ss.SubscriberId, dc *webrtc.DataChannel) {
		recordOpen := func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.opened[peer] == nil {
				p.opened[peer] = map[string]*webrtc.DataChannel{}
			}
			p.opened[peer][label] = dc
		}
		dc.OnOpen(recordOpen)
		if dc.ReadyState() == webrtc.DataChannelStateOpen {
			recordOpen()
		}
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			if !msg.IsString {
				return
			}
			p.mu.Lock()
			p.texts[peer] = append(p.texts[peer], string(msg.Data))
			p.mu.Unlock()
		})
	})
}

// register subscribes the probe to the messaging label of a client.
func (p *wireProbe) register(t *testing.T, c *rtc.HeadlessRTCClient) {
	t.Helper()
	c.HandleDataChannel(msg_handler.DataChannelLabelMessages, p.handler(msg_handler.DataChannelLabelMessages))
}

func (p *wireProbe) waitDC(t *testing.T, peer ss.SubscriberId, label string) *webrtc.DataChannel {
	t.Helper()
	waitFor(t, fmt.Sprintf("the %s channel with %s to open", label, peer), func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.opened[peer][label] != nil
	})
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.opened[peer][label]
}

func (p *wireProbe) textsFrom(peer ss.SubscriberId) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.texts[peer])
}

// trackProbe records every remote track arriving at a client (the
// client's HandleTrack).
type trackProbe struct {
	mu     sync.Mutex
	tracks []*webrtc.TrackRemote
}

func newTrackProbe() *trackProbe { return &trackProbe{} }

func (p *trackProbe) register(t *testing.T, c *rtc.HeadlessRTCClient) {
	t.Helper()
	c.HandleTrackFunc(func(_ context.Context, _ ss.ChannelId, _ ss.SubscriberId, track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		p.mu.Lock()
		p.tracks = append(p.tracks, track)
		p.mu.Unlock()
	})
}

// waitTrack waits for the probe's n-th remote track and returns it.
func (p *trackProbe) waitTrack(t *testing.T, n int) *webrtc.TrackRemote {
	t.Helper()
	waitFor(t, fmt.Sprintf("remote track #%d", n), func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.tracks) >= n
	})
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tracks[n-1]
}

// trackCount reports how many remote tracks the probe has seen so far —
// the accept-gate test's negative read (pion's OnTrack is RTP-triggered:
// a single leaked packet would have produced a track).
func (p *trackProbe) trackCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.tracks)
}

// startUserProbe Runs a plain client with a wireProbe on the messaging
// label and a trackProbe on its media. The returned cancel stops the
// client mid-test (a subscriber dropout); the tests that need it take
// it, the rest ignore it (cleanup cancels anyway).
func startUserProbe(t *testing.T, net *clientTestNet, name string, id ss.SubscriberId) (*rtc.HeadlessRTCClient, *wireProbe, *trackProbe, context.CancelFunc) {
	t.Helper()
	return startUserProbeOn(t, net, name, id, nil)
}

// startUserProbeOn is startUserProbe with a configuration hook (the
// pin tests tap the probe's peer connections).
func startUserProbeOn(t *testing.T, net *clientTestNet, name string, id ss.SubscriberId, configure func(*rtc.RTCClientConfiguration)) (*rtc.HeadlessRTCClient, *wireProbe, *trackProbe, context.CancelFunc) {
	t.Helper()
	user, stop := startClient(t, net, name, func(c *rtc.RTCClientConfiguration) {
		c.SubscriberId = id
		if configure != nil {
			configure(c)
		}
	})
	probe := newWireProbe()
	probe.register(t, user)
	tprobe := newTrackProbe()
	tprobe.register(t, user)
	return user, probe, tprobe, stop
}

// tapPCFactory builds a peer-connection factory mirroring the client's
// default (the default codecs plus telephone-event, the politeness
// DTLS role) that stores each created connection in box — the pin
// tests' view of the negotiated SDP.
func tapPCFactory(box *atomic.Pointer[webrtc.PeerConnection]) func(polite bool) (*webrtc.PeerConnection, error) {
	return func(polite bool) (*webrtc.PeerConnection, error) {
		m := &webrtc.MediaEngine{}
		if err := m.RegisterDefaultCodecs(); err != nil {
			return nil, err
		}
		if err := m.RegisterCodec(rtc.TelephoneEventCodec, webrtc.RTPCodecTypeAudio); err != nil {
			return nil, err
		}
		settingEngine := webrtc.SettingEngine{}
		role := webrtc.DTLSRoleClient
		if polite {
			role = webrtc.DTLSRoleServer
		}
		if err := settingEngine.SetAnsweringDTLSRole(role); err != nil {
			return nil, err
		}
		pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithSettingEngine(settingEngine)).NewPeerConnection(webrtc.Configuration{})
		if err != nil {
			return nil, err
		}
		box.Store(pc)
		return pc, nil
	}
}

// waitOfferSettled waits until the tapped probe connection holds a
// stable remote description carrying an audio m-line — the bot's
// ring-time media offer has landed and been answered — and returns the
// offer.
func waitOfferSettled(t *testing.T, box *atomic.Pointer[webrtc.PeerConnection]) *webrtc.SessionDescription {
	t.Helper()
	waitFor(t, "the bot's media offer to settle", func() bool {
		pc := box.Load()
		return pc != nil && pc.SignalingState() == webrtc.SignalingStateStable &&
			pc.RemoteDescription() != nil && strings.Contains(pc.RemoteDescription().SDP, "m=audio")
	})
	remote := box.Load().RemoteDescription()
	if remote == nil {
		t.Fatal("the probe holds no remote description")
	}
	return remote
}

// waitVideoOfferSettled is waitOfferSettled for a video call: the bot's
// two attaches (the audio track's, then the video track's) drive two
// offers toward the probe; the wait is over when the settled remote
// description carries the video m-line — the second attach's offer.
func waitVideoOfferSettled(t *testing.T, box *atomic.Pointer[webrtc.PeerConnection]) *webrtc.SessionDescription {
	t.Helper()
	waitFor(t, "the bot's media offer to settle with video", func() bool {
		pc := box.Load()
		return pc != nil && pc.SignalingState() == webrtc.SignalingStateStable &&
			pc.RemoteDescription() != nil && strings.Contains(pc.RemoteDescription().SDP, "m=video")
	})
	remote := box.Load().RemoteDescription()
	if remote == nil {
		t.Fatal("the probe holds no remote description")
	}
	return remote
}

// sdpAudioFormats extracts the payload-type list of the SDP's audio
// m-line — the codec pin's wire shape.
func sdpAudioFormats(t *testing.T, body string) []string {
	t.Helper()
	for line := range strings.Lines(body) {
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "m=audio ") {
			fields := strings.Fields(line)
			if len(fields) < 4 {
				t.Fatalf("the audio m-line is malformed: %q", line)
			}
			return fields[3:]
		}
	}
	t.Fatalf("no audio m-line in the SDP:\n%s", body)
	return nil
}

// sdpVideoFormats is sdpAudioFormats for the video m-line; an SDP
// without one answers nil.
func sdpVideoFormats(t *testing.T, body string) []string {
	t.Helper()
	for line := range strings.Lines(body) {
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "m=video ") {
			fields := strings.Fields(line)
			if len(fields) < 4 {
				t.Fatalf("the video m-line is malformed: %q", line)
			}
			return fields[3:]
		}
	}
	return nil
}

// sdpVideoPort is the video m-line's port, -1 when the SDP carries none.
func sdpVideoPort(t *testing.T, body string) int {
	t.Helper()
	for line := range strings.Lines(body) {
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "m=video ") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				t.Fatalf("the video m-line is malformed: %q", line)
			}
			port, err := strconv.Atoi(fields[1])
			if err != nil {
				t.Fatalf("the video m-line's port: %v", err)
			}
			return port
		}
	}
	return -1
}

// readOneRTP reads one packet off a remote track, failing the test when
// none arrives in time.
func readOneRTP(t *testing.T, track *webrtc.TrackRemote) *rtp.Packet {
	t.Helper()
	type result struct {
		pkt *rtp.Packet
		err error
	}
	ch := make(chan result, 1)
	go func() {
		pkt, _, err := track.ReadRTP()
		ch <- result{pkt, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("ReadRTP: %v", r.err)
		}
		return r.pkt
	case <-time.After(testTimeout):
		t.Fatal("timed out reading an RTP packet")
		return nil
	}
}

// baseMsg returns a well-formed DCMsg as a raw field map, ready to mutate.
func baseMsg(channelId ss.ChannelId, from, to ss.SubscriberId) map[string]any {
	return map[string]any{
		"mimeVersion":       "1.0",
		"channelId":         string(channelId),
		"fromSubscriberId":  string(from),
		"toSubscriberId":    string(to),
		"creationTimestamp": 1756000000.5,
		"msgId":             uuid.NewString(),
		"mimeType":          "text/plain",
		"plaintext":         "",
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return string(data)
}

// The wire mime types, copied from the protocol documentation — the
// production constants live in msg_handler and are deliberately not used
// here.
const (
	wireMimePlaintext   = "text/plain"
	wireMimeChatControl = "application/x-chat-control"
	wireMimeSip         = "application/x-sip"
)

// rawSipResponse is the suite's wire view of a SIP status line.
type rawSipResponse struct {
	Code   int    `json:"code"`
	Phrase string `json:"phrase"`
}

// rawSip is the suite's wire view of a sip body, in a SIP message or in a
// chat-control amend of an INVITE.
type rawSip struct {
	CallId      string          `json:"callId"`
	Method      string          `json:"method,omitempty"`
	Response    *rawSipResponse `json:"response,omitempty"`
	XMedia      string          `json:"X-Media,omitempty"`
	XCallStatus string          `json:"X-Call-Status,omitempty"`
}

// rawMsg is the suite's wire view of one decoded dcmsg frame — decoded
// straight from the documented format, independent of the production
// codec (which lives in msg_handler).
type rawMsg struct {
	echo      bool
	msgId     ss.MsgId
	from      ss.SubscriberId
	to        ss.SubscriberId
	inReplyTo ss.MsgId
	mimeType  string
	plaintext string
	sip       *rawSip

	// The chatControl body's interesting fields, when present.
	ccSubtype string
	ccTarget  ss.MsgId
	ccSip     *rawSip
}

// decodeMsgRaw decodes one dcmsg frame from the wire, returning nil when
// it does not decode.
func decodeMsgRaw(frame string) *rawMsg {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(frame), &fields); err != nil || fields == nil {
		return nil
	}
	str := func(key string) string {
		var s string
		if raw := fields[key]; raw != nil {
			_ = json.Unmarshal(raw, &s)
		}
		return s
	}
	msg := &rawMsg{
		msgId:     ss.MsgId(str("msgId")),
		from:      ss.SubscriberId(str("fromSubscriberId")),
		to:        ss.SubscriberId(str("toSubscriberId")),
		inReplyTo: ss.MsgId(str("inReplyTo")),
		mimeType:  str("mimeType"),
		plaintext: str("plaintext"),
	}
	if raw := fields["echo"]; raw != nil {
		_ = json.Unmarshal(raw, &msg.echo)
	}
	if raw := fields["sip"]; raw != nil {
		var body rawSip
		if err := json.Unmarshal(raw, &body); err == nil {
			msg.sip = &body
		}
	}
	if raw := fields["chatControl"]; raw != nil {
		var cc struct {
			Subtype         string   `json:"subtype"`
			TargetMessageId ss.MsgId `json:"targetMessageId"`
			Sip             *rawSip  `json:"sip"`
		}
		if err := json.Unmarshal(raw, &cc); err == nil {
			msg.ccSubtype = cc.Subtype
			msg.ccTarget = cc.TargetMessageId
			msg.ccSip = cc.Sip
		}
	}
	return msg
}

// botMessages decodes the frames and returns the bot's own messages
// (everything but the bounces, which carry the echo flag).
func botMessages(frames []string) []*rawMsg {
	var out []*rawMsg
	for _, f := range frames {
		if m := decodeMsgRaw(f); m != nil && !m.echo {
			out = append(out, m)
		}
	}
	return out
}

// waitBotMessage waits until the bot sends peer a message matching pred —
// correlated by the protocol's own keys: a reply's inReplyTo, a dialog
// message's callId, an amend's targetMessageId — never by arrival order.
// Returns the matched message. A timeout dumps everything the probe
// recorded: a mismatch (the bot answered with a failure the predicate
// does not expect) is then visible without a rerun.
func waitBotMessage(t *testing.T, probe *wireProbe, peer ss.SubscriberId, what string, pred func(*rawMsg) bool) *rawMsg {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for {
		for _, m := range botMessages(probe.textsFrom(peer)) {
			if pred(m) {
				return m
			}
		}
		if time.Now().After(deadline) {
			var dump strings.Builder
			for i, frame := range probe.textsFrom(peer) {
				fmt.Fprintf(&dump, "\n  [%d] %s", i, frame)
			}
			t.Fatalf("timed out waiting for %s; the probe recorded:%s", what, dump.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// countBotMessages counts the bot's messages to peer matching pred.
func countBotMessages(probe *wireProbe, peer ss.SubscriberId, pred func(*rawMsg) bool) int {
	n := 0
	for _, m := range botMessages(probe.textsFrom(peer)) {
		if pred(m) {
			n++
		}
	}
	return n
}

// isSipInvite matches an outbound INVITE (any call id).
func isSipInvite(m *rawMsg) bool {
	return m.mimeType == wireMimeSip && m.sip != nil && m.sip.Method == "INVITE"
}

// isSipRequest matches the dialog's request of the given call.
func isSipRequest(callId, method string) func(*rawMsg) bool {
	return func(m *rawMsg) bool {
		return m.mimeType == wireMimeSip && m.sip != nil && m.sip.CallId == callId && m.sip.Method == method
	}
}

// isSipResponse matches the dialog's response of the given call.
func isSipResponse(callId string, code int) func(*rawMsg) bool {
	return func(m *rawMsg) bool {
		return m.mimeType == wireMimeSip && m.sip != nil && m.sip.CallId == callId &&
			m.sip.Response != nil && m.sip.Response.Code == code
	}
}

// isChatReply matches a plain-text reply threaded on the given message
// and carrying the given substring.
func isChatReply(inReplyTo ss.MsgId, contains string) func(*rawMsg) bool {
	return func(m *rawMsg) bool {
		return m.mimeType == wireMimePlaintext && m.inReplyTo == inReplyTo &&
			strings.Contains(m.plaintext, contains)
	}
}

// isChatAnnouncement matches an unthreaded plain-text bot message
// carrying the given substring — the unsolicited WriterFor path (an
// event that is not one of the peer's messages, like the session-start
// loan's announcement).
func isChatAnnouncement(contains string) func(*rawMsg) bool {
	return func(m *rawMsg) bool {
		return m.mimeType == wireMimePlaintext && m.inReplyTo == "" &&
			strings.Contains(m.plaintext, contains)
	}
}

// isCallStatusAmend matches the chat-control amend of the given INVITE
// reporting the given call status.
func isCallStatusAmend(inviteMsgId ss.MsgId, callId, status string) func(*rawMsg) bool {
	return func(m *rawMsg) bool {
		return m.mimeType == wireMimeChatControl && m.ccSubtype == "amend" &&
			m.ccTarget == inviteMsgId && m.ccSip != nil && m.ccSip.CallId == callId &&
			m.ccSip.Method == "INVITE" && m.ccSip.XCallStatus == status
	}
}

// ---------------------------------------------------------------------------
// The fake PBX: a sipgo UAS on loopback.
// ---------------------------------------------------------------------------

// fakePBX is the suite's SIP network: a registrar that digest-challenges
// REGISTER and INVITE, a callee that answers with a hand-written SDP, a
// caller (the inbound-call tests' UAC side) that INVITEs a registrant's
// recorded contact, and one UDP socket streaming and recording real RTP.
// Everything it receives is recorded for the test's assertions.
type fakePBX struct {
	addr    string // its SIP address, host:port (an IPv6 one is bracketed)
	ip      string // its bare IP (the SDP connection address)
	sipHost string // its IP in SIP-URI form (an IPv6 literal bracketed)
	port    int    // its SIP port
	v6      bool   // the IPv6 loopback variant

	answerCode int  // the final response to an INVITE (200, 486, …)
	opus       bool // the SDP answer's codec: opus (96) when set, PCMU (0) otherwise
	video      int  // the SDP's video section: pbxVideo* (none unless enabled)

	// The demuxed-video socket: a video section answered on a port of its
	// own (pbxVideoDemuxed) sends and records there — the "the bot
	// respects the answer's port" proof. The muxed variant shares rtp.
	rtpVideo         *net.UDPConn
	rtpVideoReceived int
	rtpVideoPayloads [][]byte
	rtpVideoPTs      []uint8

	// The UAC side: the client and its dialog cache (a BYE of one of the
	// fake's own outbound calls routes to it — see OnBye).
	client        *sipgo.Client
	clientDialogs *sipgo.DialogClientCache

	rtp     *net.UDPConn // its media socket
	rtpPeer *net.UDPAddr // the bot's media address, parsed from the INVITE's SDP

	mu           sync.Mutex
	registers    int    // authenticated REGISTERs
	unregistered bool   // a de-REGISTER (Expires 0) arrived
	authedUser   string // the Authorization header's username on the last authenticated request
	// refuseRegisterUser, when non-empty, makes the registrar answer 403
	// to an authenticated REGISTER carrying this digest username — a
	// registrar that refuses one account (a bad pooled credential).
	refuseRegisterUser string
	// registrations records each authenticated REGISTER's identity: the
	// From and Contact URI users (both must be the registering user's,
	// never the bot's) and the source address (each account's own
	// socket).
	registrations []registration
	invites       int
	inviteFrom    string
	inviteTo      string
	inviteRURI    string // the last INVITE's Request-URI, verbatim
	inviteSDP     string // the last INVITE's SDP offer, verbatim
	byes          int
	messages      int
	messageFrom   string
	messageTo     string
	messageRURI   string // the last MESSAGE's Request-URI, verbatim
	messageCT     string // the last MESSAGE's Content-Type, verbatim
	messageBody   string // the last MESSAGE's body, verbatim
	rtpReceived   int
	rtpPayloads   [][]byte
	rtpPTs        []uint8 // the payload type each recorded payload arrived with

	dialogs    chan *sipgo.DialogServerSession // established dialogs (after ACK)
	rtpStart   chan struct{}                   // closed when the first dialog's media is up
	rtpStartDo sync.Once

	// tornDown is set when the test's teardown begins. The fake's handlers
	// run on sipgo's goroutines, where a t.Errorf after the test completed
	// panics; a message that arrives mid-teardown (an account's de-REGISTER
	// outliving its test, the socket's close lagging the cleanup) failing
	// is expected, not an error.
	tornDown atomic.Bool
}

// The fake's video-section modes (fakePBX.video): the answer's and the
// own offers' video m-line — none, muxed on the audio socket's port, or
// demuxed on a second socket's port.
const (
	pbxVideoNone = iota
	pbxVideoMuxed
	pbxVideoDemuxed
)

// pbxVideoPT is the fake's video payload type (VP8), the one the bot's
// presumed video list's first entry carries.
const pbxVideoPT = 102

// errf reports a fake-PBX handler failure on the test — unless teardown
// has begun, where a late message's failure is shutdown noise and a
// t.Errorf could fire after the test completed (which panics).
func (f *fakePBX) errf(t *testing.T, format string, args ...any) {
	if f.tornDown.Load() {
		return
	}
	t.Errorf(format, args...)
}

// registration is one authenticated REGISTER's identity as the fake saw
// it on the wire.
type registration struct {
	from    string // the From URI's user
	contact string // the Contact URI's user
	source  string // the datagram's source address (the account's socket)
	expires string // the Expires header's value ("": the REGISTER carried none)
}

// newFakePBX starts a fake PBX answering INVITEs with answerCode, on the
// IPv4 loopback. The SIP socket is a reserved loopback port (freed before
// binding, the small race every test accepts); the media socket stays
// bound.
func newFakePBX(t *testing.T, answerCode int, opus bool) *fakePBX {
	t.Helper()
	return newFakePBXOn(t, "127.0.0.1", answerCode, opus)
}

// newFakePBX6 starts the fake PBX on the IPv6 loopback; the test skips
// when the host has no IPv6 loopback.
func newFakePBX6(t *testing.T, answerCode int, opus bool) *fakePBX {
	t.Helper()
	return newFakePBXOn(t, "::1", answerCode, opus)
}

// newFakePBXOn starts a fake PBX on the given loopback address —
// "127.0.0.1" or "::1".
func newFakePBXOn(t *testing.T, ip string, answerCode int, opus bool) *fakePBX {
	t.Helper()
	listenIP := net.ParseIP(ip)
	rsv, err := net.ListenUDP("udp", &net.UDPAddr{IP: listenIP, Port: 0})
	if err != nil {
		t.Skipf("no %s loopback on this host: %v", ip, err)
	}
	port := rsv.LocalAddr().(*net.UDPAddr).Port
	if err := rsv.Close(); err != nil {
		t.Fatalf("release the reserved port: %v", err)
	}
	return newFakePBXOnPort(t, ip, port, answerCode, opus)
}

// newFakePBXOnPort starts a fake PBX on the given loopback address and
// SIP port. Two PBXs sharing one port — one on 127.0.0.1, one on ::1 —
// let one DNS hostname map to one registrar per address family (the
// ipPreference test).
func newFakePBXOnPort(t *testing.T, ip string, port int, answerCode int, opus bool) *fakePBX {
	t.Helper()
	listenIP := net.ParseIP(ip)
	if c, err := net.ListenPacket("udp", net.JoinHostPort(ip, "0")); err != nil {
		t.Skipf("no %s loopback on this host: %v", ip, err)
	} else {
		_ = c.Close()
	}
	rtpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: listenIP, Port: 0})
	if err != nil {
		t.Fatalf("bind the media socket: %v", err)
	}
	t.Cleanup(func() { rtpConn.Close() })

	v6 := listenIP.To4() == nil
	sipHost := ip
	if v6 {
		sipHost = "[" + ip + "]" // a SIP URI brackets an IPv6 literal
	}
	f := &fakePBX{
		addr:       net.JoinHostPort(ip, strconv.Itoa(port)),
		ip:         ip,
		sipHost:    sipHost,
		port:       port,
		v6:         v6,
		answerCode: answerCode,
		opus:       opus,
		rtp:        rtpConn,
		dialogs:    make(chan *sipgo.DialogServerSession, 4),
		rtpStart:   make(chan struct{}),
	}

	ua, err := sipgo.NewUA()
	if err != nil {
		t.Fatalf("sipgo.NewUA: %v", err)
	}
	srv, err := sipgo.NewServer(ua)
	if err != nil {
		t.Fatalf("sipgo.NewServer: %v", err)
	}
	client, err := sipgo.NewClient(ua)
	if err != nil {
		t.Fatalf("sipgo.NewClient: %v", err)
	}
	dialogs := sipgo.NewDialogServerCache(client, sip.ContactHeader{
		Address: sip.Uri{User: "pbx", Host: f.sipHost, Port: port},
	})
	f.client = client
	f.clientDialogs = sipgo.NewDialogClientCache(client, sip.ContactHeader{
		Address: sip.Uri{User: "caller", Host: f.sipHost, Port: port},
	})

	srv.OnRegister(func(req *sip.Request, tx sip.ServerTransaction) {
		if !f.authenticated(req) {
			f.challenge(t, req, tx)
			return
		}
		f.mu.Lock()
		refused := f.refuseRegisterUser != "" && f.refuseRegisterUser == f.authedUser
		f.mu.Unlock()
		if refused {
			if err := tx.Respond(sip.NewResponseFromRequest(req, 403, "Forbidden", nil)); err != nil {
				f.errf(t, "REGISTER 403: %v", err)
			}
			return
		}
		if exp := req.GetHeader("Expires"); exp != nil && strings.TrimSpace(exp.Value()) == "0" {
			f.mu.Lock()
			f.unregistered = true
			f.mu.Unlock()
		} else {
			reg := registration{from: req.From().Address.User, source: req.Source()}
			if c := req.Contact(); c != nil {
				reg.contact = c.Address.User
			}
			if exp := req.GetHeader("Expires"); exp != nil {
				reg.expires = strings.TrimSpace(exp.Value())
			}
			f.mu.Lock()
			f.registers++
			f.registrations = append(f.registrations, reg)
			f.mu.Unlock()
		}
		if err := tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil)); err != nil {
			f.errf(t, "REGISTER 200: %v", err)
		}
	})
	srv.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
		if !f.authenticated(req) {
			f.challenge(t, req, tx)
			return
		}
		f.mu.Lock()
		f.invites++
		f.inviteFrom = req.From().Address.User
		f.inviteTo = req.To().Address.User
		f.inviteRURI = req.Recipient.String()
		f.mu.Unlock()
		peer := parseSDPMediaAddr(t, req.Body())
		f.mu.Lock()
		f.rtpPeer = peer
		f.inviteSDP = string(req.Body())
		f.mu.Unlock()
		dialog, err := dialogs.ReadInvite(req, tx)
		if err != nil {
			f.errf(t, "ReadInvite: %v", err)
			_ = tx.Respond(sip.NewResponseFromRequest(req, 500, "Server Error", nil))
			return
		}
		if err := dialog.Respond(180, "Ringing", nil); err != nil {
			f.errf(t, "180: %v", err)
		}
		if f.answerCode != 200 {
			if err := dialog.Respond(f.answerCode, "Busy Here", nil); err != nil {
				f.errf(t, "final response: %v", err)
			}
			return
		}
		if err := dialog.RespondSDP(f.sdpBody()); err != nil {
			f.errf(t, "RespondSDP: %v", err)
			return
		}
		f.dialogs <- dialog
	})
	srv.OnAck(func(req *sip.Request, tx sip.ServerTransaction) {
		if err := dialogs.ReadAck(req, tx); err != nil {
			f.errf(t, "ReadAck: %v", err)
		}
		f.rtpStartDo.Do(func() { close(f.rtpStart) })
	})
	srv.OnBye(func(req *sip.Request, tx sip.ServerTransaction) {
		f.mu.Lock()
		f.byes++
		f.mu.Unlock()
		// A BYE of one of the fake's own outbound calls (the inbound-call
		// tests) belongs to a client dialog; a callee's BYE to a server one.
		if cd, err := f.clientDialogs.MatchRequestDialog(req); err == nil {
			if err := cd.ReadBye(req, tx); err != nil {
				f.errf(t, "the caller dialog's ReadBye: %v", err)
			}
			return
		}
		if err := dialogs.ReadBye(req, tx); err != nil {
			_ = tx.Respond(sip.NewResponseFromRequest(req, 481, "Call/Transaction Does Not Exist", nil))
		}
	})
	srv.OnCancel(func(req *sip.Request, tx sip.ServerTransaction) {
		// The transaction layer terminates the INVITE transaction; a
		// 200 to the CANCEL is all the handler owes.
		_ = tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
	})
	srv.OnMessage(func(req *sip.Request, tx sip.ServerTransaction) {
		// The registrar digest-challenges MESSAGEs like it does REGISTERs
		// and INVITEs; only the authenticated retry is recorded.
		if !f.authenticated(req) {
			f.challenge(t, req, tx)
			return
		}
		f.mu.Lock()
		f.messages++
		f.messageFrom = req.From().Address.User
		f.messageTo = req.To().Address.User
		f.messageRURI = req.Recipient.String()
		if ct := req.ContentType(); ct != nil {
			f.messageCT = ct.Value()
		}
		f.messageBody = string(req.Body())
		f.mu.Unlock()
		if err := tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil)); err != nil {
			f.errf(t, "MESSAGE 200: %v", err)
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		if err := srv.ListenAndServe(ctx, "udp", f.addr); err != nil && !errors.Is(err, context.Canceled) {
			f.errf(t, "the fake PBX stopped: %v", err)
		}
	}()
	t.Cleanup(func() { _ = srv.Close() })
	// Registered last, so it runs FIRST of the fake's cleanups: handler
	// failures from then on are teardown noise (see errf).
	t.Cleanup(func() { f.tornDown.Store(true) })

	// The media receive loop: record every RTP payload the bot relays.
	go func() {
		buf := make([]byte, 2000)
		for {
			n, _, err := rtpConn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			pkt := &rtp.Packet{}
			if err := pkt.Unmarshal(buf[:n]); err != nil {
				continue
			}
			f.mu.Lock()
			f.rtpReceived++
			f.rtpPayloads = append(f.rtpPayloads, slices.Clone(pkt.Payload))
			f.rtpPTs = append(f.rtpPTs, pkt.PayloadType)
			f.mu.Unlock()
		}
	}()
	return f
}

// enableVideo makes the fake's SDP (its answers to the bot's INVITEs,
// and the offers its own calls carry) announce a video section: VP8 at
// the fake's video PT — muxed on the media socket's port, or, when
// demuxed, on a second socket's port whose arrivals are recorded apart
// (the "the bot respects the answer's own video port" proof).
func (f *fakePBX) enableVideo(t *testing.T, demuxed bool) {
	t.Helper()
	f.mu.Lock()
	f.video = pbxVideoMuxed
	f.mu.Unlock()
	if !demuxed {
		return
	}
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(f.ip), Port: 0})
	if err != nil {
		t.Fatalf("bind the demuxed video socket: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	f.mu.Lock()
	f.video = pbxVideoDemuxed
	f.rtpVideo = conn
	f.mu.Unlock()
	go func() {
		buf := make([]byte, 2000)
		for {
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			pkt := &rtp.Packet{}
			if err := pkt.Unmarshal(buf[:n]); err != nil {
				continue
			}
			f.mu.Lock()
			f.rtpVideoReceived++
			f.rtpVideoPayloads = append(f.rtpVideoPayloads, slices.Clone(pkt.Payload))
			f.rtpVideoPTs = append(f.rtpVideoPTs, pkt.PayloadType)
			f.mu.Unlock()
		}
	}()
}

// refuseRegister makes the registrar answer 403 to an authenticated
// REGISTER carrying user's digest username — a registrar that refuses
// one account (a bad pooled credential).
func (f *fakePBX) refuseRegister(user string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refuseRegisterUser = user
}

// authenticated reports whether the request carries a digest answer and
// records its username. The fake does not verify the response hash —
// that the bot answered the challenge with the stored credential is the
// assertion; digest correctness is diago's own tested behavior.
func (f *fakePBX) authenticated(req *sip.Request) bool {
	h := req.GetHeader("Authorization")
	if h == nil {
		return false
	}
	f.mu.Lock()
	f.authedUser = between(h.Value(), `username="`, `"`)
	f.mu.Unlock()
	return true
}

// challenge answers 401 with the digest challenge.
func (f *fakePBX) challenge(t *testing.T, req *sip.Request, tx sip.ServerTransaction) {
	res := sip.NewResponseFromRequest(req, 401, "Unauthorized", nil)
	res.AppendHeader(sip.NewHeader("WWW-Authenticate", `Digest realm="pbx", nonce="0123456789abcdef", algorithm=MD5`))
	if err := tx.Respond(res); err != nil {
		f.errf(t, "REGISTER 401: %v", err)
	}
}

// between extracts the value between two markers, "" when absent.
func between(s, left, right string) string {
	i := strings.Index(s, left)
	if i < 0 {
		return ""
	}
	s = s[i+len(left):]
	j := strings.Index(s, right)
	if j < 0 {
		return ""
	}
	return s[:j]
}

// sdpBody is the fake's SDP — the answer to the bot's INVITEs, and the
// offer its own calls carry: audio (the configured codec plus
// telephone-event) on the media socket's port, joined by a video section
// (VP8 at the fake's video PT) when the fake's video mode says so —
// muxed on the same port, or demuxed on the second socket's.
func (f *fakePBX) sdpBody() []byte {
	port := f.rtp.LocalAddr().(*net.UDPAddr).Port
	family := "IP4"
	if f.v6 {
		family = "IP6"
	}
	sdp := fmt.Sprintf("v=0\r\no=- 0 0 IN %s %s\r\ns=fakepbx\r\nc=IN %s %s\r\nt=0 0\r\n", family, f.ip, family, f.ip)
	if f.opus {
		sdp += fmt.Sprintf("m=audio %d RTP/AVP 96 101\r\na=rtpmap:96 opus/48000/2\r\na=rtpmap:101 telephone-event/8000\r\n", port)
	} else {
		sdp += fmt.Sprintf("m=audio %d RTP/AVP 0 101\r\na=rtpmap:0 PCMU/8000\r\na=rtpmap:101 telephone-event/8000\r\n", port)
	}
	if f.video != pbxVideoNone {
		videoPort := port
		if f.video == pbxVideoDemuxed {
			videoPort = f.rtpVideo.LocalAddr().(*net.UDPAddr).Port
		}
		sdp += fmt.Sprintf("m=video %d RTP/AVP %d\r\na=rtpmap:%d VP8/90000\r\n", videoPort, pbxVideoPT, pbxVideoPT)
	}
	return []byte(sdp)
}

// rtpPort is the fake's media socket's port — the custom-SDP callers'
// m-line port.
func (f *fakePBX) rtpPort() int {
	return f.rtp.LocalAddr().(*net.UDPAddr).Port
}

// parseSDPMediaAddr extracts the media address (the c= line's IP, the
// m= line's port) from an SDP body.
func parseSDPMediaAddr(t *testing.T, body []byte) *net.UDPAddr {
	t.Helper()
	var ip string
	var port int
	for line := range strings.Lines(string(body)) {
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "c=IN IP4 ") {
			ip = strings.TrimPrefix(line, "c=IN IP4 ")
		}
		if strings.HasPrefix(line, "c=IN IP6 ") {
			ip = strings.TrimPrefix(line, "c=IN IP6 ")
		}
		if strings.HasPrefix(line, "m=audio ") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				if _, err := fmt.Sscanf(fields[1], "%d", &port); err != nil {
					t.Fatalf("the offer's m= line: %v", err)
				}
			}
		}
	}
	if ip == "" || port == 0 {
		t.Fatalf("no media address in the SDP offer:\n%s", body)
	}
	return &net.UDPAddr{IP: net.ParseIP(ip), Port: port}
}

// waitDialog waits for the next established dialog.
func (f *fakePBX) waitDialog(t *testing.T) *sipgo.DialogServerSession {
	t.Helper()
	select {
	case d := <-f.dialogs:
		return d
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for the callee's dialog")
		return nil
	}
}

// requireStereoOpusSDP asserts the SDP body announces the bot's stereo
// opus: the dual-channel rtpmap and the RFC 7587 fmtp line carrying
// in-band FEC and the stereo receive preference — the exact wire string
// the sip leg must speak.
func requireStereoOpusSDP(t *testing.T, what, body string) {
	t.Helper()
	for _, want := range []string{"a=rtpmap:96 opus/48000/2", "a=fmtp:96 useinbandfec=1;stereo=1"} {
		if !strings.Contains(body, want) {
			t.Fatalf("%s lacks %q:\n%s", what, want, body)
		}
	}
}

// streamRTP sends 20 ms RTP packets of the given payload type and
// timestamp step to the bot's media address — the callee's voice —
// cycling the given payloads until ctx ends (the relay drops them while
// the browser leg is still being set up, so the stream simply runs on).
// It starts once the first dialog's media is up.
func (f *fakePBX) streamRTP(ctx context.Context, payloads [][]byte, pt uint8, tsStep uint32) {
	<-f.rtpStart
	f.mu.Lock()
	peer := f.rtpPeer
	f.mu.Unlock()
	if peer == nil {
		return
	}
	for i := 0; ; i++ {
		pkt := &rtp.Packet{
			Header:  rtp.Header{Version: 2, PayloadType: pt, SequenceNumber: uint16(i), Timestamp: uint32(i) * tsStep, SSRC: 0xC0FFEE},
			Payload: payloads[i%len(payloads)],
		}
		data, err := pkt.Marshal()
		if err != nil {
			return
		}
		if _, err := f.rtp.WriteToUDP(data, peer); err != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// streamOpus streams the payloads as 20 ms opus RTP (PT 96, 960 ticks
// per packet).
func (f *fakePBX) streamOpus(ctx context.Context, payloads [][]byte) {
	f.streamRTP(ctx, payloads, 96, 960)
}

// streamPCMU streams the payloads as 20 ms PCMU RTP (PT 0, 160 ticks
// per packet).
func (f *fakePBX) streamPCMU(ctx context.Context, payloads [][]byte) {
	f.streamRTP(ctx, payloads, 0, 160)
}

// streamVideo streams the payloads as ≈30 fps VP8 RTP (the fake's video
// PT, 3000 90 kHz ticks per packet, a video SSRC of its own) to the bot's
// media address — the callee's camera. The demuxed fake sends from its
// video socket (a real demuxed endpoint's source), the muxed one from
// the shared socket. It starts once the first dialog's media is up.
func (f *fakePBX) streamVideo(ctx context.Context, payloads [][]byte) {
	<-f.rtpStart
	f.mu.Lock()
	peer := f.rtpPeer
	conn := f.rtp
	if f.video == pbxVideoDemuxed {
		conn = f.rtpVideo
	}
	f.mu.Unlock()
	if peer == nil {
		return
	}
	for i := 0; ; i++ {
		pkt := &rtp.Packet{
			Header:  rtp.Header{Version: 2, PayloadType: pbxVideoPT, SequenceNumber: uint16(i), Timestamp: uint32(i) * 3000, SSRC: 0xBEEF01},
			Payload: payloads[i%len(payloads)],
		}
		data, err := pkt.Marshal()
		if err != nil {
			return
		}
		if _, err := conn.WriteToUDP(data, peer); err != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(33 * time.Millisecond):
		}
	}
}

// ---- The fake as a caller: the inbound-call tests' UAC side ----

// fakeCall is one of the fake PBX's own outbound calls: an INVITE to a
// registrant's recorded contact, driven per test (wait the ring, take the
// answer, CANCEL the ring, BYE the answered call).
type fakeCall struct {
	t *testing.T
	f *fakePBX

	dlg *sipgo.DialogClientSession

	// answerCtx scopes the WaitAnswer; cancelling it is the caller's
	// CANCEL (sipgo's WaitAnswer sends one — once a provisional arrived,
	// hence cancel() waits the ring).
	answerCtx    context.Context
	answerCancel context.CancelFunc

	mu        sync.Mutex
	ringing   bool          // a 180 arrived
	res       *sip.Response // the final response (nil until WaitAnswer returned)
	peer      *net.UDPAddr  // the bot's media address, off the 200 OK's SDP
	answerSDP string        // the 200 OK's SDP answer, verbatim
	answered  bool          // the 200 OK arrived and its ACK went out
}

// call rings the registrant user: an INVITE to the contact the
// registration came from, carrying the fake's SDP (the same body its
// answers carry). The INVITE is on the wire when call returns; the
// call's waits drive it from there.
func (f *fakePBX) call(t *testing.T, user string) *fakeCall {
	t.Helper()
	return f.callSDP(t, user, f.sdpBody())
}

// callSDP is call carrying the given SDP body — the codec-negotiation
// tests drive the offer's contents (a list sharing nothing with the
// presumed one earns a 488).
func (f *fakePBX) callSDP(t *testing.T, user string, body []byte) *fakeCall {
	t.Helper()
	f.mu.Lock()
	source := ""
	for _, r := range f.registrations {
		if r.from == user {
			source = r.source
		}
	}
	f.mu.Unlock()
	if source == "" {
		t.Fatalf("the fake PBX holds no registration for %s", user)
	}
	host, portText, err := net.SplitHostPort(source)
	if err != nil {
		t.Fatalf("the registrant's source %q: %v", source, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("the registrant's source %q: %v", source, err)
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]" // an IPv6 literal is bracketed in a SIP URI
	}
	from := &sip.FromHeader{
		Address: sip.Uri{User: "caller", Host: f.sipHost, Port: f.port},
		Params:  sip.NewParams(),
	}
	from.Params.Add("tag", sip.GenerateTagN(16))

	fc := &fakeCall{t: t, f: f}
	fc.answerCtx, fc.answerCancel = context.WithCancel(context.Background())
	t.Cleanup(fc.answerCancel)
	dlg, err := f.clientDialogs.Invite(fc.answerCtx, sip.Uri{User: user, Host: host, Port: port}, body,
		sip.NewHeader("Content-Type", "application/sdp"), from)
	if err != nil {
		t.Fatalf("the fake's INVITE: %v", err)
	}
	fc.dlg = dlg
	go fc.waitAnswer()
	return fc
}

// sendMessage texts the registrant user: a pager-mode SIP MESSAGE (RFC
// 3428) to the contact the registration came from, carrying body with
// the given Content-Type — the inbound-messaging tests' UAC side,
// callSDP's page-mode twin. The answer is the returned response; the
// fake challenges nothing here (the bot's inbound side authenticates
// nothing).
func (f *fakePBX) sendMessage(t *testing.T, user, contentType, body string) *sip.Response {
	t.Helper()
	f.mu.Lock()
	source := ""
	for _, r := range f.registrations {
		if r.from == user {
			source = r.source
		}
	}
	f.mu.Unlock()
	if source == "" {
		t.Fatalf("the fake PBX holds no registration for %s", user)
	}
	host, portText, err := net.SplitHostPort(source)
	if err != nil {
		t.Fatalf("the registrant's source %q: %v", source, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("the registrant's source %q: %v", source, err)
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]" // an IPv6 literal is bracketed in a SIP URI
	}
	uri := sip.Uri{User: user, Host: host, Port: port}
	req := sip.NewRequest(sip.MESSAGE, uri)
	from := &sip.FromHeader{
		Address: sip.Uri{User: "caller", Host: f.sipHost, Port: f.port},
		Params:  sip.NewParams(),
	}
	from.Params.Add("tag", sip.GenerateTagN(16))
	req.AppendHeader(from)
	req.AppendHeader(&sip.ToHeader{Address: *uri.Clone()})
	req.AppendHeader(sip.NewHeader("Content-Type", contentType))
	req.SetBody([]byte(body))
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	res, err := f.client.Do(ctx, req)
	if err != nil {
		t.Fatalf("the fake's MESSAGE: %v", err)
	}
	return res
}

// waitAnswer collects the call's responses: the provisionals (the ring),
// then the final, recorded for the test's waits.
func (fc *fakeCall) waitAnswer() {
	err := fc.dlg.WaitAnswer(fc.answerCtx, sipgo.AnswerOptions{
		OnResponse: func(res *sip.Response) error {
			if res.StatusCode == 180 {
				fc.mu.Lock()
				fc.ringing = true
				fc.mu.Unlock()
			}
			return nil
		},
	})
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if err != nil {
		var resErr *sipgo.ErrDialogResponse
		if errors.As(err, &resErr) {
			fc.res = resErr.Res
		}
		// Otherwise the CANCEL's context.Canceled — res stays nil.
		return
	}
	fc.res = fc.dlg.InviteResponse
}

// waitRinging waits for the caller's 180 Ringing.
func (fc *fakeCall) waitRinging() {
	fc.t.Helper()
	waitFor(fc.t, "the caller's 180 Ringing", func() bool {
		fc.mu.Lock()
		defer fc.mu.Unlock()
		return fc.ringing
	})
}

// waitAnswered waits for the 200 OK, takes the bot's media address off
// its SDP, and sends the ACK — the bot's Answer returns on it.
func (fc *fakeCall) waitAnswered() {
	fc.t.Helper()
	waitFor(fc.t, "the caller's 200 OK", func() bool {
		fc.mu.Lock()
		defer fc.mu.Unlock()
		return fc.res != nil && fc.res.StatusCode == 200
	})
	fc.mu.Lock()
	res := fc.res
	fc.mu.Unlock()
	peer := parseSDPMediaAddr(fc.t, res.Body())
	if err := fc.dlg.Ack(context.Background()); err != nil {
		fc.t.Fatalf("the caller's ACK: %v", err)
	}
	fc.mu.Lock()
	fc.peer = peer
	fc.answerSDP = string(res.Body())
	fc.answered = true
	fc.mu.Unlock()
}

// waitFailed waits for the call's final response and asserts its code.
func (fc *fakeCall) waitFailed(code int) {
	fc.t.Helper()
	waitFor(fc.t, fmt.Sprintf("the caller's %d", code), func() bool {
		fc.mu.Lock()
		defer fc.mu.Unlock()
		return fc.res != nil
	})
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if fc.res.StatusCode != code {
		fc.t.Fatalf("the inbound call's final response is %d %s, want %d", fc.res.StatusCode, fc.res.Reason, code)
	}
}

// cancel hangs the ring up — the caller's CANCEL (which the wire shape
// needs a provisional for, hence the ring wait first).
func (fc *fakeCall) cancel() {
	fc.t.Helper()
	fc.waitRinging()
	fc.answerCancel()
}

// bye hangs the answered call up — the caller's BYE.
func (fc *fakeCall) bye() {
	fc.t.Helper()
	if err := fc.dlg.Bye(context.Background()); err != nil {
		fc.t.Fatalf("the caller's BYE: %v", err)
	}
}

// stream plays the caller's voice: the payloads as opus RTP to the bot's
// media address, one packet per 20 ms, until ctx ends. It runs only once
// the answer was taken (the media address comes from it).
func (fc *fakeCall) stream(ctx context.Context, payloads [][]byte) {
	fc.streamRTP(ctx, payloads, 96, 960)
}

// streamRTP is stream with the payload type and timestamp step of the
// call's negotiated codec (PT 96 and 960 ticks for opus; PT 0 and 160
// for PCMU).
func (fc *fakeCall) streamRTP(ctx context.Context, payloads [][]byte, pt uint8, tsStep uint32) {
	fc.streamRTPOn(fc.f.rtp, 0xCA11E1, ctx, payloads, pt, tsStep, 20*time.Millisecond)
}

// streamVideo plays the caller's camera: the payloads as ≈30 fps VP8 RTP
// (the negotiated video PT, 3000 ticks per packet, a video SSRC of its
// own) to the bot's media address. The fake's own offers mux the video
// on the media socket, so the shared socket sends.
func (fc *fakeCall) streamVideo(ctx context.Context, payloads [][]byte) {
	fc.streamRTPOn(fc.f.rtp, 0xBEEF02, ctx, payloads, pbxVideoPT, 3000, 33*time.Millisecond)
}

// streamRTPOn is the two streamers' shared loop: paced RTP of one SSRC
// from one socket to the bot's media address, until ctx ends.
func (fc *fakeCall) streamRTPOn(conn *net.UDPConn, ssrc uint32, ctx context.Context, payloads [][]byte, pt uint8, tsStep uint32, pace time.Duration) {
	fc.mu.Lock()
	peer := fc.peer
	fc.mu.Unlock()
	if peer == nil {
		return
	}
	for i := 0; ; i++ {
		pkt := &rtp.Packet{
			Header:  rtp.Header{Version: 2, PayloadType: pt, SequenceNumber: uint16(i), Timestamp: uint32(i) * tsStep, SSRC: ssrc},
			Payload: payloads[i%len(payloads)],
		}
		data, err := pkt.Marshal()
		if err != nil {
			return
		}
		if _, err := conn.WriteToUDP(data, peer); err != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pace):
		}
	}
}

// opusVoice returns n distinct synthetic 20 ms opus packets.
func opusVoice(n int) [][]byte {
	payloads := make([][]byte, n)
	for i := range payloads {
		p := make([]byte, 40)
		p[0] = 0xF8 // a fullband 20 ms frame, one per packet
		for j := 1; j < len(p); j++ {
			p[j] = byte(i*67 + j*31) // distinct, non-silent
		}
		payloads[i] = p
	}
	return payloads
}

// pcmuVoice returns n distinct synthetic 20 ms PCMU frames (160 bytes
// at 8 kHz).
func pcmuVoice(n int) [][]byte {
	payloads := make([][]byte, n)
	for i := range payloads {
		p := make([]byte, 160)
		for j := range p {
			p[j] = byte(i*73 + j*19) // distinct, never the 0xFF silence
		}
		payloads[i] = p
	}
	return payloads
}

// videoFrames returns n distinct synthetic VP8 frame payloads (the relay
// never parses them — distinct bytes are what the byte-for-byte
// assertions need).
func videoFrames(n int) [][]byte {
	payloads := make([][]byte, n)
	for i := range payloads {
		p := make([]byte, 300)
		p[0] = 0x10 // a VP8 payload descriptor's bare shape
		for j := 1; j < len(p); j++ {
			p[j] = byte(i*41 + j*17)
		}
		payloads[i] = p
	}
	return payloads
}

// waitRTP waits until at least n RTP packets arrived at the fake's
// media socket and returns their payloads.
func (f *fakePBX) waitRTP(t *testing.T, n int) [][]byte {
	t.Helper()
	waitFor(t, fmt.Sprintf("%d RTP packets at the callee", n), func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.rtpReceived >= n
	})
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.rtpPayloads)
}

// rtpPayloadsByPT is the fake's recorded media-socket payloads of one
// payload type — the PT-demuxed view of the muxed arrivals (the camera's
// packets at the negotiated video PT among the mic's audio).
func (f *fakePBX) rtpPayloadsByPT(pt uint8) [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]byte
	for i, p := range f.rtpPayloads {
		if f.rtpPTs[i] == pt {
			out = append(out, slices.Clone(p))
		}
	}
	return out
}

// waitVideoRTP waits until at least n RTP packets arrived at the fake's
// DEMUXED video socket and returns their payloads.
func (f *fakePBX) waitVideoRTP(t *testing.T, n int) [][]byte {
	t.Helper()
	waitFor(t, fmt.Sprintf("%d RTP packets at the callee's video port", n), func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.rtpVideoReceived >= n
	})
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.rtpVideoPayloads)
}

// waitDTMF waits until the fake received one full RFC 4733 series for
// the event and NOTHING beyond it: diago's writer emits a fixed 7-packet
// encoding (4 start updates, then the END three times) per WriteDTMF, so
// the exact counts prove the press crossed exactly once — the browser
// side's own END retransmissions having been deduped to one emission
// (three series would mean 9 ENDs).
func (f *fakePBX) waitDTMF(t *testing.T, code uint8) {
	t.Helper()
	f.waitForPBX(t, fmt.Sprintf("the DTMF series for event %d", code), func() bool {
		starts, ends := 0, 0
		for i, pt := range f.rtpPTs {
			if pt != media.CodecTelephoneEvent8000.PayloadType {
				continue
			}
			ev, ok := parseDTMFEvent(f.rtpPayloads[i])
			if !ok || ev.code != code {
				continue
			}
			if ev.end {
				ends++
			} else {
				starts++
			}
		}
		return starts == 4 && ends == 3
	})
}

// waitForPBX waits for a recorded-fact predicate on the PBX.
func (f *fakePBX) waitForPBX(t *testing.T, what string, pred func() bool) {
	t.Helper()
	waitFor(t, what, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return pred()
	})
}

// ---------------------------------------------------------------------------
// The tests.
// ---------------------------------------------------------------------------

// TestSipBotCLIAndGuards covers the CLI's bookkeeping: /help answers with
// the command list; anything unrecognized answers with the help hint;
// the commands' guards speak (/register's usage, /call unregistered,
// /test-call without a configured contact, /hangup with no call).
func TestSipBotCLIAndGuards(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot := startBot(t, net, "bot", "2-bot", "", nil)
	user, probe, _, _ := startUserProbe(t, net, "user", "1-user") // polite: creates the channels
	botId, userId := pairUp(t, bot, user)

	dc := probe.waitDC(t, botId, msg_handler.DataChannelLabelMessages)
	send := func(text string) ss.MsgId {
		t.Helper()
		m := baseMsg(ss.WellKnownChIdMain, userId, botId)
		m["plaintext"] = text
		if err := dc.SendText(mustJSON(t, m)); err != nil {
			t.Fatalf("SendText: %v", err)
		}
		return ss.MsgId(m["msgId"].(string))
	}

	helpMsg := send("/help")
	unknownMsg := send("hello there")
	registerMsg := send("/register 2001@sip.example.com")
	callMsg := send("/call 1001@sip.example.com")
	videoCallMsg := send("/video-call 1001@sip.example.com")
	videoCallUsageMsg := send("/video-call")
	testCallMsg := send("/test-call")
	hangupMsg := send("/hangup")

	help := waitBotMessage(t, probe, botId, "the /help reply", isChatReply(helpMsg, "/help"))
	for _, cmd := range []string{"/help", "/register", "/unregister", "/call", "/video-call", "/yellow-page", "/test-call", "/hangup"} {
		if !strings.Contains(help.plaintext, cmd) {
			t.Fatalf("the help text lacks %q: %q", cmd, help.plaintext)
		}
	}
	waitBotMessage(t, probe, botId, "the unknown-command hint", isChatReply(unknownMsg, "/help"))
	waitBotMessage(t, probe, botId, "the register usage", isChatReply(registerMsg, "Usage: /register"))
	waitBotMessage(t, probe, botId, "the not-registered answer", isChatReply(callMsg, "Not registered"))
	waitBotMessage(t, probe, botId, "the video-call not-registered answer", isChatReply(videoCallMsg, "Not registered"))
	waitBotMessage(t, probe, botId, "the video-call usage", isChatReply(videoCallUsageMsg, "Usage: /video-call"))
	waitBotMessage(t, probe, botId, "the test-call unavailable answer", isChatReply(testCallMsg, "unavailable"))
	waitBotMessage(t, probe, botId, "the no-call answer", isChatReply(hangupMsg, "No call in progress"))

	// No call came of the guarded commands: not one INVITE the whole test.
	time.Sleep(300 * time.Millisecond)
	if n := countBotMessages(probe, botId, isSipInvite); n != 0 {
		t.Fatalf("the guarded commands produced %d INVITEs", n)
	}
}

// TestSipBotYellowPage covers /yellow-page: the bot answers with its
// configured phone book — the section captions, the contacts' names,
// dial targets, and descriptions — threaded on the command.
func TestSipBotYellowPage(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot := startBotWithPage(t, net, "bot", "2-bot", []YellowPageSection{
		{ID: "sec-local", Name: "local", Contacts: []YellowPageContact{
			{ID: "echo", Name: "echo test", AOR: "9196"},
			{ID: "echo-delayed", Name: "delayed echo test", AOR: "9195", Description: "echoes back after 250ms"},
			{ID: "moh", Name: "music on hold", AOR: "9664"},
		}},
	})
	user, probe, _, _ := startUserProbe(t, net, "user", "1-user")
	botId, userId := pairUp(t, bot, user)

	dc := probe.waitDC(t, botId, msg_handler.DataChannelLabelMessages)
	m := baseMsg(ss.WellKnownChIdMain, userId, botId)
	m["plaintext"] = "/yellow-page"
	if err := dc.SendText(mustJSON(t, m)); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	reply := waitBotMessage(t, probe, botId, "the /yellow-page reply", isChatReply(ss.MsgId(m["msgId"].(string)), "Yellow page"))
	for _, want := range []string{"local", "echo test", "9196", "delayed echo test", "9195", "echoes back after 250ms", "music on hold", "9664", "/call"} {
		if !strings.Contains(reply.plaintext, want) {
			t.Fatalf("the yellow page lacks %q: %q", want, reply.plaintext)
		}
	}
	// The opaque ids stay in the configuration, out of the chat.
	for _, id := range []string{"sec-local", "echo-delayed"} {
		if strings.Contains(reply.plaintext, id) {
			t.Fatalf("the yellow page leaks the opaque id %q: %q", id, reply.plaintext)
		}
	}
}

// TestSipBotDeclinesIncomingCalls covers the inbound call policy: an
// INVITE from the browser — voice and video alike — is declined with
// 603 (the bot is an outbound SBC).
func TestSipBotDeclinesIncomingCalls(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot := startBot(t, net, "bot", "2-bot", "", nil)
	user, probe, _, _ := startUserProbe(t, net, "user", "1-user")
	botId, userId := pairUp(t, bot, user)

	dc := probe.waitDC(t, botId, msg_handler.DataChannelLabelMessages)
	for _, mediaKind := range []string{"voice", "video"} {
		callId := uuid.NewString()
		m := baseMsg(ss.WellKnownChIdMain, userId, botId)
		m["mimeType"] = wireMimeSip
		m["sip"] = map[string]any{
			"callId": callId, "method": "INVITE", "X-Media": mediaKind, "X-Call-Status": "inviting",
		}
		if err := dc.SendText(mustJSON(t, m)); err != nil {
			t.Fatalf("SendText: %v", err)
		}
		waitBotMessage(t, probe, botId, "the 603 Decline of the "+mediaKind+" call", isSipResponse(callId, 603))
	}
}

// TestSipBotRegisterUnregister covers the registration lifecycle: the
// first REGISTER is digest-challenged and answered with the stored
// credential, the reply carries the AOR, and /unregister takes the
// registration down with a de-REGISTER (Expires 0).
func TestSipBotRegisterUnregister(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot := startBot(t, net, "bot", "2-bot", "", nil)
	user, probe, _, _ := startUserProbe(t, net, "user", "1-user")
	botId, userId := pairUp(t, bot, user)
	pbx := newFakePBX(t, 200, true)

	dc := probe.waitDC(t, botId, msg_handler.DataChannelLabelMessages)
	chat := func(text string) ss.MsgId {
		t.Helper()
		m := baseMsg(ss.WellKnownChIdMain, userId, botId)
		m["plaintext"] = text
		if err := dc.SendText(mustJSON(t, m)); err != nil {
			t.Fatalf("SendText: %v", err)
		}
		return ss.MsgId(m["msgId"].(string))
	}

	registerMsg := chat("/register 2001@" + pbx.addr + " passW_0rd")
	waitBotMessage(t, probe, botId, "the registered reply", isChatReply(registerMsg, "Registered as sip:2001@"+pbx.addr))
	pbx.waitForPBX(t, "the authenticated REGISTER", func() bool { return pbx.registers == 1 })
	pbx.waitForPBX(t, "the REGISTER's digest username", func() bool { return pbx.authedUser == "2001" })
	// The identity on the wire is the user's, never the bot's: the From
	// is the AOR and the Contact's user part is the username. And the
	// REGISTER carries NO Expires header: the registration stands until
	// the account's own lifecycle ends it (the de-REGISTER below), not
	// some lease the registrar could expire under an ongoing call.
	pbx.waitForPBX(t, "the REGISTER's From and Contact, and no Expires", func() bool {
		return len(pbx.registrations) == 1 &&
			pbx.registrations[0].from == "2001" && pbx.registrations[0].contact == "2001" &&
			pbx.registrations[0].expires == ""
	})

	unregisterMsg := chat("/unregister")
	waitBotMessage(t, probe, botId, "the unregistered reply", isChatReply(unregisterMsg, "Unregistered."))
	pbx.waitForPBX(t, "the de-REGISTER", func() bool { return pbx.unregistered })

	// The credential is gone: the next /call says so.
	callMsg := chat("/call 1001@" + pbx.addr)
	waitBotMessage(t, probe, botId, "the not-registered answer", isChatReply(callMsg, "Not registered"))
}

// TestSipBotPerUserStacks proves every registered user gets a SIP
// client of their own: two users register against the same registrar,
// and the wire shows two REGISTERs — each from a distinct socket, each
// stamped with its own user's identity (the From and the Contact's user
// part), never the bot's and never each other's.
func TestSipBotPerUserStacks(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot := startBot(t, net, "bot", "2-bot", "", nil)
	alice, aliceProbe, _, _ := startUserProbe(t, net, "alice", "1-alice")
	bob, bobProbe, _, _ := startUserProbe(t, net, "bob", "1-bob")
	botId, aliceId := pairUp(t, bot, alice)
	_, bobId := pairUp(t, bot, bob)
	pbx := newFakePBX(t, 200, true)

	register := func(probe *wireProbe, userId ss.SubscriberId, aor, password string) ss.MsgId {
		t.Helper()
		dc := probe.waitDC(t, botId, msg_handler.DataChannelLabelMessages)
		m := baseMsg(ss.WellKnownChIdMain, userId, botId)
		m["plaintext"] = fmt.Sprintf("/register %s@%s %s", aor, pbx.addr, password)
		if err := dc.SendText(mustJSON(t, m)); err != nil {
			t.Fatalf("SendText: %v", err)
		}
		return ss.MsgId(m["msgId"].(string))
	}
	aliceMsg := register(aliceProbe, aliceId, "2001", "passW_0rd")
	bobMsg := register(bobProbe, bobId, "2002", "s3cret")
	waitBotMessage(t, aliceProbe, botId, "alice's registered reply", isChatReply(aliceMsg, "Registered as sip:2001@"+pbx.addr))
	waitBotMessage(t, bobProbe, botId, "bob's registered reply", isChatReply(bobMsg, "Registered as sip:2002@"+pbx.addr))

	pbx.waitForPBX(t, "both users' REGISTERs, each on its own socket with its own identity", func() bool {
		if len(pbx.registrations) != 2 {
			return false
		}
		byUser := map[string]registration{}
		for _, r := range pbx.registrations {
			byUser[r.from] = r
		}
		alice, ok1 := byUser["2001"]
		bob, ok2 := byUser["2002"]
		return ok1 && ok2 &&
			alice.contact == "2001" && bob.contact == "2002" &&
			alice.source != bob.source
	})
}

// TestSipBotCallEndToEnd covers the whole B2BUA: /register, /call —
// the bot phones the callee on the sip-leg FIRST (a busy or unreachable
// callee would never disturb the browser), and once the callee answered
// — the answer's codec selection in hand — phones the browser on the
// webrtc-leg with the offer pinned to that selection (opus in one
// variant, PCMU in the other: the G.711 path is a passthrough now, so
// it runs without cgo). Media then crosses byte for byte in both
// directions, and the callee's BYE ends the call on the browser's side
// too (BYE, the chat line, the ended amend).
func TestSipBotCallEndToEnd(t *testing.T) {
	for _, tc := range []struct {
		name        string
		opus        bool // the fake PBX's answer codec (and so the call's)
		micCodec    webrtc.RTPCodecCapability
		voice       func(int) [][]byte
		stream      func(*fakePBX, context.Context, [][]byte)
		wantFormats []string // the webrtc-leg pin's m-line payload types
	}{
		{
			name:        "opus",
			opus:        true,
			micCodec:    webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
			voice:       opusVoice,
			stream:      func(f *fakePBX, ctx context.Context, p [][]byte) { f.streamOpus(ctx, p) },
			wantFormats: []string{"111", "126"},
		},
		{
			name:        "pcmu",
			opus:        false,
			micCodec:    webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: 8000, Channels: 1},
			voice:       pcmuVoice,
			stream:      func(f *fakePBX, ctx context.Context, p [][]byte) { f.streamPCMU(ctx, p) },
			wantFormats: []string{"0", "126"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			net := newClientTestNet(t, ss.DefaultSubscriberAging)
			bot := startBot(t, net, "bot", "2-bot", "", nil)
			var pcBox atomic.Pointer[webrtc.PeerConnection]
			user, probe, tprobe, _ := startUserProbeOn(t, net, "user", "1-user", func(c *rtc.RTCClientConfiguration) {
				c.NewPeerConnection = tapPCFactory(&pcBox)
			})
			botId, userId := pairUp(t, bot, user)
			pbx := newFakePBX(t, 200, tc.opus)

			dc := probe.waitDC(t, botId, msg_handler.DataChannelLabelMessages)
			chat := func(text string) ss.MsgId {
				t.Helper()
				m := baseMsg(ss.WellKnownChIdMain, userId, botId)
				m["plaintext"] = text
				if err := dc.SendText(mustJSON(t, m)); err != nil {
					t.Fatalf("SendText: %v", err)
				}
				return ss.MsgId(m["msgId"].(string))
			}
			sip := func(body map[string]any) {
				t.Helper()
				m := baseMsg(ss.WellKnownChIdMain, userId, botId)
				m["mimeType"] = wireMimeSip
				m["sip"] = body
				if err := dc.SendText(mustJSON(t, m)); err != nil {
					t.Fatalf("SendText: %v", err)
				}
			}

			registerMsg := chat("/register 2001@" + pbx.addr + " passW_0rd")
			waitBotMessage(t, probe, botId, "the registered reply", isChatReply(registerMsg, "Registered as"))

			// The call: the SIP leg leads — the bot phones the callee with
			// the user's own identity, digest and all…
			callMsg := chat("/call 1001@" + pbx.addr)
			waitBotMessage(t, probe, botId, "the calling line", isChatReply(callMsg, "Calling 1001@"))
			pbx.waitForPBX(t, "the callee's INVITE", func() bool { return pbx.invites == 1 })
			pbx.waitForPBX(t, "the INVITE's From/To", func() bool { return pbx.inviteFrom == "2001" && pbx.inviteTo == "1001" })
			pbx.waitForPBX(t, "the INVITE's digest username", func() bool { return pbx.authedUser == "2001" })
			// The offer announces stereo opus (RFC 7587) among the presumed
			// WebRTC codec list: the webrtc-leg's stereo negotiation mirrored
			// onto the sip-leg.
			pbx.mu.Lock()
			offer := pbx.inviteSDP
			pbx.mu.Unlock()
			requireStereoOpusSDP(t, "the INVITE's SDP offer", offer)

			// …and ONLY once the callee answered is the browser rung: the
			// established SIP dialog exists by the time the bot's INVITE
			// lands (a busy callee would never have produced one — the
			// browser would have been left alone).
			dialog := pbx.waitDialog(t)
			invite := waitBotMessage(t, probe, botId, "the bot's INVITE", func(m *rawMsg) bool {
				return isSipInvite(m) && m.sip.XMedia == "voice" && m.sip.XCallStatus == "inviting"
			})
			callId, inviteMsgId := invite.sip.CallId, invite.msgId
			waitBotMessage(t, probe, botId, "the callee-answered line", isChatReply(callMsg, "Callee answered."))

			// The webrtc-leg's media offer is pinned to the codec the callee
			// selected: the bot's offer toward the probe carries exactly
			// [the call's codec, telephone-event].
			pinned := waitOfferSettled(t, &pcBox)
			if got := sdpAudioFormats(t, pinned.SDP); !slices.Equal(got, tc.wantFormats) {
				t.Fatalf("the bot's offer is pinned to %v, want %v:\n%s", got, tc.wantFormats, pinned.SDP)
			}

			// The user picks up. The accept's shape mirrors the browser's:
			// the mic joins the pinned m-line (its track speaks the pinned
			// codec), the 200 OK then flies. No settle sleep is needed: the
			// bot's own offer settled above, so the mic's re-offer is the
			// only one in flight.
			micTrack, err := webrtc.NewTrackLocalStaticSample(tc.micCodec, "mic", "probe")
			if err != nil {
				t.Fatalf("the mic track: %v", err)
			}
			if _, err := user.AddTrack(botId, micTrack); err != nil {
				t.Fatalf("attach the mic: %v", err)
			}
			// The mic speaks from here on, one packet per 20 ms (samples
			// written before it binds drop — the "empty room").
			spoken := tc.voice(5)
			micCtx, stopMic := context.WithCancel(context.Background())
			t.Cleanup(stopMic)
			go func() {
				for i := 0; ; i++ {
					if err := micTrack.WriteSample(pionmedia.Sample{Data: spoken[i%len(spoken)], Duration: 20 * time.Millisecond}); err != nil {
						return
					}
					select {
					case <-micCtx.Done():
						return
					case <-time.After(20 * time.Millisecond):
					}
				}
			}()
			sip(map[string]any{"callId": callId, "response": map[string]any{"code": 200, "phrase": "OK"}})

			// The callee's voice starts streaming — the probe's OnTrack
			// fires only once media actually arrives, so the stream runs
			// ahead of the track's wait. The track speaks the pinned codec.
			streamCtx, stopStream := context.WithCancel(context.Background())
			t.Cleanup(stopStream)
			voice := tc.voice(5)
			go tc.stream(pbx, streamCtx, voice)
			track := tprobe.waitTrack(t, 1)
			if codec := track.Codec(); !strings.EqualFold(codec.MimeType, tc.micCodec.MimeType) {
				t.Fatalf("the bot's track toward the browser is %s, want %s", codec.MimeType, tc.micCodec.MimeType)
			}
			waitBotMessage(t, probe, botId, "the accepted amend", isCallStatusAmend(inviteMsgId, callId, "accepted"))

			// The callee speaks: the packets cross the SBC byte for byte.
			heardVoice := map[string]bool{}
			for len(heardVoice) < len(voice) {
				pkt := readOneRTP(t, track)
				heardVoice[string(pkt.Payload)] = true
			}
			for _, want := range voice {
				if !heardVoice[string(want)] {
					t.Fatalf("a streamed packet never reached the browser (the passthrough changed or lost it)")
				}
			}

			// The user speaks: the mic's packets cross byte for byte too.
			heard := map[string]bool{}
			deadline := time.Now().Add(testTimeout)
			for {
				for _, got := range pbx.waitRTP(t, 1) {
					heard[string(got)] = true
				}
				all := true
				for _, p := range spoken {
					if !heard[string(p)] {
						all = false
					}
				}
				if all {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("the callee heard %d of the %d distinct mic packets", len(heard), len(spoken))
				}
			}

			// The callee hangs up: the browser leg is ended by the bot (BYE,
			// the chat line, the ended amend) — signalling termination both
			// ways.
			if err := dialog.Bye(context.Background()); err != nil {
				t.Fatalf("the callee's BYE: %v", err)
			}
			waitBotMessage(t, probe, botId, "the bot's BYE", isSipRequest(callId, "BYE"))
			waitBotMessage(t, probe, botId, "the callee-hung-up line", isChatReply(callMsg, "Callee hung up."))
			waitBotMessage(t, probe, botId, "the ended amend", isCallStatusAmend(inviteMsgId, callId, "ended"))
		})
	}
}

// TestSipBotCallHoldsCalleeAudioUntilAccept covers the relay's accept
// gate: the browser's negotiator answers the bot's ring-time offer at
// once, so the speaker track binds while the call still rings — but no
// media may cross before the user picks up. The callee's voice streamed
// during the ring reaches the probe not at all (pion's OnTrack is
// RTP-triggered — one leaked packet would surface as a track); the
// accept opens the gate, and the voice then crosses byte for byte.
func TestSipBotCallHoldsCalleeAudioUntilAccept(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot := startBot(t, net, "bot", "2-bot", "", nil)
	user, probe, tprobe, _ := startUserProbe(t, net, "user", "1-user")
	botId, userId := pairUp(t, bot, user)
	pbx := newFakePBX(t, 200, true)

	dc := probe.waitDC(t, botId, msg_handler.DataChannelLabelMessages)
	chat := func(text string) ss.MsgId {
		t.Helper()
		m := baseMsg(ss.WellKnownChIdMain, userId, botId)
		m["plaintext"] = text
		if err := dc.SendText(mustJSON(t, m)); err != nil {
			t.Fatalf("SendText: %v", err)
		}
		return ss.MsgId(m["msgId"].(string))
	}

	registerMsg := chat("/register 2001@" + pbx.addr + " passW_0rd")
	waitBotMessage(t, probe, botId, "the registered reply", isChatReply(registerMsg, "Registered as"))

	callMsg := chat("/call 1001@" + pbx.addr)
	waitBotMessage(t, probe, botId, "the calling line", isChatReply(callMsg, "Calling 1001@"))
	dialog := pbx.waitDialog(t)
	invite := waitBotMessage(t, probe, botId, "the bot's INVITE", func(m *rawMsg) bool {
		return isSipInvite(m) && m.sip.XMedia == "voice" && m.sip.XCallStatus == "inviting"
	})
	callId := invite.sip.CallId
	waitBotMessage(t, probe, botId, "the callee-answered line", isChatReply(callMsg, "Callee answered."))

	// The callee is already talking — an IVR greeting, a "hello?" — while
	// the browser still rings: not one packet may cross. 500 ms is ≈25
	// packets' worth of streaming; the track-bound path would deliver the
	// first within milliseconds.
	streamCtx, stopStream := context.WithCancel(context.Background())
	t.Cleanup(stopStream)
	voice := opusVoice(5)
	go pbx.streamOpus(streamCtx, voice)
	time.Sleep(500 * time.Millisecond)
	if leaked := tprobe.trackCount(); leaked != 0 {
		t.Fatalf("the callee's audio reached the still-ringing browser (%d track(s) fired)", leaked)
	}

	// The user picks up: the gate opens and the voice crosses byte for byte.
	m := baseMsg(ss.WellKnownChIdMain, userId, botId)
	m["mimeType"] = wireMimeSip
	m["sip"] = map[string]any{"callId": callId, "response": map[string]any{"code": 200, "phrase": "OK"}}
	if err := dc.SendText(mustJSON(t, m)); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	track := tprobe.waitTrack(t, 1)
	heardVoice := map[string]bool{}
	for len(heardVoice) < len(voice) {
		pkt := readOneRTP(t, track)
		heardVoice[string(pkt.Payload)] = true
	}
	for _, want := range voice {
		if !heardVoice[string(want)] {
			t.Fatalf("a streamed packet never reached the browser (the passthrough changed or lost it)")
		}
	}

	// The callee hangs up: the browser leg is ended by the bot.
	if err := dialog.Bye(context.Background()); err != nil {
		t.Fatalf("the callee's BYE: %v", err)
	}
	waitBotMessage(t, probe, botId, "the bot's BYE", isSipRequest(callId, "BYE"))
	waitBotMessage(t, probe, botId, "the callee-hung-up line", isChatReply(callMsg, "Callee hung up."))
}

// dtmfInjector is the DTMF test's probe-side interceptor, turning
// crafted mic payloads into telephone-event packets on the wire. pion's
// track API rewrites every outgoing packet's PT to the bound codec's, so
// a track cannot emit telephone-event (docs/webrtc-sip-dtmf-
// consideration.md's note on the pion gap) — the rewrite happens below
// the track, on the packet's way out: a payload of
// [0xEE, event, flags, durHi, durLo] leaves with the negotiated
// telephone-event PT and the magic byte stripped, every other payload
// passes untouched. pt is the wire's telephone-event PT, armed once the
// call's media is up.
type dtmfInjector struct {
	interceptor.NoOp
	pt atomic.Int32
}

// BindLocalStream wraps each local stream's writer with the rewrite.
func (i *dtmfInjector) BindLocalStream(_ *interceptor.StreamInfo, writer interceptor.RTPWriter) interceptor.RTPWriter {
	return interceptor.RTPWriterFunc(func(header *rtp.Header, payload []byte, attributes interceptor.Attributes) (int, error) {
		if pt := i.pt.Load(); pt >= 0 && len(payload) == 5 && payload[0] == 0xEE {
			header.PayloadType = uint8(pt)
			return writer.Write(header, payload[1:], attributes)
		}
		return writer.Write(header, payload, attributes)
	})
}

// interceptorFactory adapts a function to interceptor.Factory, the
// registry's currency.
type interceptorFactory func(id string) (interceptor.Interceptor, error)

// NewInterceptor implements interceptor.Factory.
func (f interceptorFactory) NewInterceptor(id string) (interceptor.Interceptor, error) {
	return f(id)
}

// dtmfProbePCFactory builds the DTMF test probe's peer-connection
// factory: the client's default (whose media engine already registers
// telephone-event, mirroring the browser) plus the injector.
func dtmfProbePCFactory(inj *dtmfInjector) func(polite bool) (*webrtc.PeerConnection, error) {
	return func(polite bool) (*webrtc.PeerConnection, error) {
		m := &webrtc.MediaEngine{}
		if err := m.RegisterDefaultCodecs(); err != nil {
			return nil, err
		}
		if err := m.RegisterCodec(rtc.TelephoneEventCodec, webrtc.RTPCodecTypeAudio); err != nil {
			return nil, err
		}
		ir := &interceptor.Registry{}
		if err := webrtc.RegisterDefaultInterceptors(m, ir); err != nil {
			return nil, err
		}
		ir.Add(interceptorFactory(func(string) (interceptor.Interceptor, error) { return inj, nil }))
		// The answering DTLS role follows the session's politeness — the
		// default factory's discipline (pkg/rtc/client.go).
		settingEngine := webrtc.SettingEngine{}
		role := webrtc.DTLSRoleClient
		if polite {
			role = webrtc.DTLSRoleServer
		}
		if err := settingEngine.SetAnsweringDTLSRole(role); err != nil {
			return nil, err
		}
		return webrtc.NewAPI(
			webrtc.WithMediaEngine(m),
			webrtc.WithSettingEngine(settingEngine),
			webrtc.WithInterceptorRegistry(ir),
		).NewPeerConnection(webrtc.Configuration{})
	}
}

// TestSipBotCallDTMF covers the pad's data plane end to end: the
// browser's key presses — RFC 4733 telephone-event packets sharing the
// mic's own RTP stream — cross the SBC and reach the SIP callee as
// diago's event series, while the audio keeps flowing. The probe emits
// the events through the injector (the track API cannot); the fake PBX,
// whose SDP answer offers telephone-event as PBXs do, records the wire.
// The signalling rides along: the bot's INVITE offer must announce
// telephone-event, and the probe learning its PT proves the webrtc-leg
// negotiated it.
func TestSipBotCallDTMF(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot := startBot(t, net, "bot", "2-bot", "", nil)
	inj := &dtmfInjector{}
	inj.pt.Store(-1)
	user, stop := startClient(t, net, "user", func(c *rtc.RTCClientConfiguration) {
		c.SubscriberId = "1-user"
		c.NewPeerConnection = dtmfProbePCFactory(inj)
	})
	defer stop()
	probe := newWireProbe()
	probe.register(t, user)
	botId, userId := pairUp(t, bot, user)
	pbx := newFakePBX(t, 200, true)

	dc := probe.waitDC(t, botId, msg_handler.DataChannelLabelMessages)
	chat := func(text string) ss.MsgId {
		t.Helper()
		m := baseMsg(ss.WellKnownChIdMain, userId, botId)
		m["plaintext"] = text
		if err := dc.SendText(mustJSON(t, m)); err != nil {
			t.Fatalf("SendText: %v", err)
		}
		return ss.MsgId(m["msgId"].(string))
	}
	sip := func(body map[string]any) {
		t.Helper()
		m := baseMsg(ss.WellKnownChIdMain, userId, botId)
		m["mimeType"] = wireMimeSip
		m["sip"] = body
		if err := dc.SendText(mustJSON(t, m)); err != nil {
			t.Fatalf("SendText: %v", err)
		}
	}

	registerMsg := chat("/register 2001@" + pbx.addr + " passW_0rd")
	waitBotMessage(t, probe, botId, "the registered reply", isChatReply(registerMsg, "Registered as"))

	callMsg := chat("/call 1001@" + pbx.addr)
	pbx.waitForPBX(t, "the callee's INVITE", func() bool { return pbx.invites == 1 })
	// The sip-leg's offer always announces telephone-event — the pad's
	// capability declaration toward the SIP network.
	pbx.mu.Lock()
	offer := pbx.inviteSDP
	pbx.mu.Unlock()
	if !strings.Contains(offer, "a=rtpmap:101 telephone-event/8000") {
		t.Fatalf("the INVITE's SDP offer carries no telephone-event:\n%s", offer)
	}
	// The browser rings once the callee answered; the pin carries
	// telephone-event alongside the audio codec.
	dialog := pbx.waitDialog(t)
	invite := waitBotMessage(t, probe, botId, "the bot's INVITE", func(m *rawMsg) bool {
		return isSipInvite(m) && m.sip.XMedia == "voice" && m.sip.XCallStatus == "inviting"
	})
	callId := invite.sip.CallId
	waitBotMessage(t, probe, botId, "the callee-answered line", isChatReply(callMsg, "Callee answered."))

	// The user picks up: the mic joins the pinned m-line (a
	// TrackLocalStaticRTP — WriteRTP's header control is what the press
	// series' shared timestamps need), the 200 OK once the mic's
	// renegotiation settled.
	micTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "mic", "probe")
	if err != nil {
		t.Fatalf("the mic track: %v", err)
	}
	if _, err := user.AddTrack(botId, micTrack); err != nil {
		t.Fatalf("attach the mic: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	sip(map[string]any{"callId": callId, "response": map[string]any{"code": 200, "phrase": "OK"}})

	// Arm the injector: the wire's telephone-event PT is the bot's
	// registered one — the bot offers the webrtc-leg's media, so its
	// MediaEngine's PT is what both directions carry (the answer echoes
	// the offer). The bot accepting the injector's packets at all is the
	// webrtc-leg negotiation's proof.
	inj.pt.Store(int32(rtc.TelephoneEventCodec.PayloadType))

	// The mic's stream, sequenced by hand: audio packets and key presses
	// share one SSRC, one sequence space, one clock.
	seq := uint16(1)
	ts := uint32(1000)
	emitAudio := func(payload []byte) {
		t.Helper()
		pkt := &rtp.Packet{
			Header:  rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: seq, Timestamp: ts, SSRC: 0xD17F},
			Payload: payload,
		}
		seq++
		ts += 960 // 20 ms at 48 kHz
		if err := micTrack.WriteRTP(pkt); err != nil {
			t.Fatalf("the audio packet: %v", err)
		}
	}
	// press emits one key press, the browser's insertDTMF shape: a start
	// packet, then the END retransmitted three times — every packet of
	// the press sharing the event's start timestamp (RFC 4733).
	press := func(event uint8) {
		t.Helper()
		start := ts
		emit := func(end bool, duration uint16) {
			t.Helper()
			flags := uint8(10) // volume 10
			if end {
				flags |= 0x80
			}
			pkt := &rtp.Packet{
				Header:  rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: start, SSRC: 0xD17F},
				Payload: []byte{0xEE, event, flags, byte(duration >> 8), byte(duration)},
			}
			seq++
			if err := micTrack.WriteRTP(pkt); err != nil {
				t.Fatalf("the DTMF packet: %v", err)
			}
		}
		emit(false, 400)
		emit(true, 800)
		emit(true, 800)
		emit(true, 800)
		ts += 4800 // the 100 ms tone went by (800 units at 8 kHz)
	}

	// The audio flows before, through, and after the presses.
	voice := opusVoice(4)
	for _, p := range voice {
		emitAudio(p)
	}
	pbx.waitRTP(t, 1)
	press(5) // "5"
	pbx.waitDTMF(t, 5)
	press(11) // "#"
	pbx.waitDTMF(t, 11)
	for _, p := range voice {
		emitAudio(p)
	}
	// …and the post-press audio reaches the callee too (the pad never
	// wedged the pump).
	heard := map[string]bool{}
	deadline := time.Now().Add(testTimeout)
	for len(heard) < len(voice) {
		for _, got := range pbx.waitRTP(t, 1) {
			heard[string(got)] = true
		}
		if time.Now().After(deadline) {
			t.Fatalf("the callee heard %d of the %d distinct mic packets after the presses", len(heard), len(voice))
		}
	}

	// Settle, then the exact wire totals: each press crossed exactly
	// once — diago's 7-packet series — the browser side's retransmitted
	// ENDs deduped (a duplicate emission would read as 14 telephone-
	// event packets per press, not 7).
	time.Sleep(300 * time.Millisecond)
	pbx.mu.Lock()
	total := 0
	perCode := map[uint8][2]int{} // code → [starts, ends]
	for i, pt := range pbx.rtpPTs {
		if pt != media.CodecTelephoneEvent8000.PayloadType {
			continue
		}
		ev, ok := parseDTMFEvent(pbx.rtpPayloads[i])
		if !ok {
			continue
		}
		total++
		counts := perCode[ev.code]
		if ev.end {
			counts[1]++
		} else {
			counts[0]++
		}
		perCode[ev.code] = counts
	}
	pbx.mu.Unlock()
	if total != 14 || perCode[5] != [2]int{4, 3} || perCode[11] != [2]int{4, 3} {
		t.Fatalf("the DTMF wire totals: %d packets, per-code %v (want 14, 5: [4 3], 11: [4 3])", total, perCode)
	}

	if err := dialog.Bye(context.Background()); err != nil {
		t.Fatalf("the callee's BYE: %v", err)
	}
	waitBotMessage(t, probe, botId, "the callee-hung-up line", isChatReply(callMsg, "Callee hung up."))
}

// TestSipBotCallBusy covers the callee's refusal: the SIP leg leads, so
// a busy callee fails the dial BEFORE the browser was ever rung — the
// chat line saying why is the whole story: no INVITE, no CANCEL, no
// log entry.
func TestSipBotCallBusy(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot := startBot(t, net, "bot", "2-bot", "", nil)
	user, probe, _, _ := startUserProbe(t, net, "user", "1-user")
	botId, userId := pairUp(t, bot, user)
	pbx := newFakePBX(t, 486, true)

	dc := probe.waitDC(t, botId, msg_handler.DataChannelLabelMessages)
	chat := func(text string) ss.MsgId {
		t.Helper()
		m := baseMsg(ss.WellKnownChIdMain, userId, botId)
		m["plaintext"] = text
		if err := dc.SendText(mustJSON(t, m)); err != nil {
			t.Fatalf("SendText: %v", err)
		}
		return ss.MsgId(m["msgId"].(string))
	}

	registerMsg := chat("/register 2001@" + pbx.addr + " passW_0rd")
	waitBotMessage(t, probe, botId, "the registered reply", isChatReply(registerMsg, "Registered as"))

	callMsg := chat("/call 1001@" + pbx.addr)
	waitBotMessage(t, probe, botId, "the calling line", isChatReply(callMsg, "Calling 1001@"))
	pbx.waitForPBX(t, "the callee's INVITE", func() bool { return pbx.invites == 1 })

	// The callee is busy: the chat line says so, and that is all — the
	// browser was never disturbed.
	waitBotMessage(t, probe, botId, "the call-failed line", isChatReply(callMsg, "Call failed: "))
	time.Sleep(300 * time.Millisecond)
	if n := countBotMessages(probe, botId, isSipInvite); n != 0 {
		t.Fatalf("the busy callee still rang the browser: %d INVITEs", n)
	}
	if n := countBotMessages(probe, botId, func(m *rawMsg) bool {
		return m.mimeType == wireMimeChatControl && m.ccSubtype == "amend"
	}); n != 0 {
		t.Fatalf("a call that never rang produced %d log amends", n)
	}
}

// TestSipBotCallRoutesViaRegistrar proves the outbound call leaves
// through the user's own SIP server: the callee lives on a domain the
// bot cannot even resolve, yet the INVITE must arrive at the registrar
// — a registered user's default outbound proxy — with the Request-URI
// untouched (the proxy routes onward to the callee's domain). The
// pre-fix behavior resolved the Request-URI's host and sent the INVITE
// to the callee's domain directly: an unresolvable callee domain failed
// the call before any INVITE left, and a reachable one bypassed the
// proxy (and, the reported bug, was unreachable from the account's
// socket when its family differed from the registrar's).
func TestSipBotCallRoutesViaRegistrar(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot := startBot(t, net, "bot", "2-bot", "", nil)
	user, probe, _, _ := startUserProbe(t, net, "user", "1-user")
	botId, userId := pairUp(t, bot, user)
	pbx := newFakePBX(t, 200, true)

	dc := probe.waitDC(t, botId, msg_handler.DataChannelLabelMessages)
	chat := func(text string) ss.MsgId {
		t.Helper()
		m := baseMsg(ss.WellKnownChIdMain, userId, botId)
		m["plaintext"] = text
		if err := dc.SendText(mustJSON(t, m)); err != nil {
			t.Fatalf("SendText: %v", err)
		}
		return ss.MsgId(m["msgId"].(string))
	}

	registerMsg := chat("/register 2001@" + pbx.addr + " passW_0rd")
	waitBotMessage(t, probe, botId, "the registered reply", isChatReply(registerMsg, "Registered as"))

	// A callee on a domain the bot cannot resolve (RFC 2606's reserved
	// TLD): the call can only work if the INVITE goes to the registrar.
	callMsg := chat("/call 1234@callee.invalid")
	waitBotMessage(t, probe, botId, "the calling line", isChatReply(callMsg, "Calling 1234@"))
	pbx.waitForPBX(t, "the callee's INVITE at the registrar", func() bool { return pbx.invites == 1 })
	pbx.mu.Lock()
	ruri, to := pbx.inviteRURI, pbx.inviteTo
	pbx.mu.Unlock()
	if ruri != "sip:1234@callee.invalid" {
		t.Fatalf("the INVITE's Request-URI is %q, want the callee's address untouched (sip:1234@callee.invalid)", ruri)
	}
	if to != "1234" {
		t.Fatalf("the INVITE's To user is %q, want 1234", to)
	}

	// The call comes up end to end (the registrar/proxy answered): the
	// browser rings, the user picks up, and /hangup takes both legs down.
	pbx.waitDialog(t)
	invite := waitBotMessage(t, probe, botId, "the bot's INVITE", isSipInvite)
	sipMsg := baseMsg(ss.WellKnownChIdMain, userId, botId)
	sipMsg["mimeType"] = wireMimeSip
	sipMsg["sip"] = map[string]any{"callId": invite.sip.CallId, "response": map[string]any{"code": 200, "phrase": "OK"}}
	if err := dc.SendText(mustJSON(t, sipMsg)); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	waitBotMessage(t, probe, botId, "the callee-answered line", isChatReply(callMsg, "Callee answered."))
	hangupMsg := chat("/hangup")
	waitBotMessage(t, probe, botId, "the hung-up reply", isChatReply(hangupMsg, "Hung up."))
	pbx.waitForPBX(t, "the callee's BYE", func() bool { return pbx.byes == 1 })
}

// TestSipBotHangupCommand covers /hangup mid-call: both legs go down —
// the browser leg gets the bot's BYE, the sip-leg's dialog its own.
func TestSipBotHangupCommand(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot := startBot(t, net, "bot", "2-bot", "", nil)
	user, probe, _, _ := startUserProbe(t, net, "user", "1-user")
	botId, userId := pairUp(t, bot, user)
	pbx := newFakePBX(t, 200, true)

	dc := probe.waitDC(t, botId, msg_handler.DataChannelLabelMessages)
	chat := func(text string) ss.MsgId {
		t.Helper()
		m := baseMsg(ss.WellKnownChIdMain, userId, botId)
		m["plaintext"] = text
		if err := dc.SendText(mustJSON(t, m)); err != nil {
			t.Fatalf("SendText: %v", err)
		}
		return ss.MsgId(m["msgId"].(string))
	}
	sip := func(body map[string]any) {
		t.Helper()
		m := baseMsg(ss.WellKnownChIdMain, userId, botId)
		m["mimeType"] = wireMimeSip
		m["sip"] = body
		if err := dc.SendText(mustJSON(t, m)); err != nil {
			t.Fatalf("SendText: %v", err)
		}
	}

	registerMsg := chat("/register 2001@" + pbx.addr + " passW_0rd")
	waitBotMessage(t, probe, botId, "the registered reply", isChatReply(registerMsg, "Registered as"))

	callMsg := chat("/call 1001@" + pbx.addr)
	// The callee answers first; the browser rings on its heels.
	pbx.waitDialog(t)
	invite := waitBotMessage(t, probe, botId, "the bot's INVITE", isSipInvite)
	callId := invite.sip.CallId
	sip(map[string]any{"callId": callId, "response": map[string]any{"code": 200, "phrase": "OK"}})
	// The call is established on both legs; no media needs to flow for
	// the hangup semantics (the probe's OnTrack fires only on arriving
	// RTP, which this test never streams).
	waitBotMessage(t, probe, botId, "the callee-answered line", isChatReply(callMsg, "Callee answered."))

	hangupMsg := chat("/hangup")
	waitBotMessage(t, probe, botId, "the bot's BYE", isSipRequest(callId, "BYE"))
	waitBotMessage(t, probe, botId, "the hung-up reply", isChatReply(hangupMsg, "Hung up."))
	pbx.waitForPBX(t, "the callee's BYE", func() bool { return pbx.byes == 1 })
}

// TestSipBotSendMessage covers /send (§7.3): the chat line becomes one
// pager-mode SIP MESSAGE on the account's identity, leaving through the
// account's registrar — the callee lives on a domain the bot cannot even
// resolve, so the MESSAGE's arrival AT THE REGISTRAR with the
// Request-URI untouched proves the outbound-proxy routing (the
// TestSipBotCallRoutesViaRegistrar trick). The registrar
// digest-challenges the MESSAGE, so the authenticated retry is what is
// recorded — its Authorization's username the proof the retry carried
// the account's credential. The over-limit /send is rejected in chat and
// never reaches the wire.
func TestSipBotSendMessage(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot := startBot(t, net, "bot", "2-bot", "", nil)
	user, probe, _, _ := startUserProbe(t, net, "user", "1-user")
	botId, userId := pairUp(t, bot, user)
	pbx := newFakePBX(t, 200, true)

	dc := probe.waitDC(t, botId, msg_handler.DataChannelLabelMessages)
	chat := func(text string) ss.MsgId {
		t.Helper()
		m := baseMsg(ss.WellKnownChIdMain, userId, botId)
		m["plaintext"] = text
		if err := dc.SendText(mustJSON(t, m)); err != nil {
			t.Fatalf("SendText: %v", err)
		}
		return ss.MsgId(m["msgId"].(string))
	}

	registerMsg := chat("/register 2001@" + pbx.addr + " passW_0rd")
	waitBotMessage(t, probe, botId, "the registered reply", isChatReply(registerMsg, "Registered as"))

	// A recipient on a domain the bot cannot resolve (RFC 2606's reserved
	// TLD): the message can only arrive if it goes to the registrar.
	sendMsg := chat("/send 1234@callee.invalid Hello from the browser")
	waitBotMessage(t, probe, botId, "the sent confirmation", isChatReply(sendMsg, "Message sent to 1234@callee.invalid."))
	pbx.waitForPBX(t, "the MESSAGE at the registrar", func() bool { return pbx.messages == 1 })
	pbx.mu.Lock()
	from, to, ruri, ct, body, authed := pbx.messageFrom, pbx.messageTo, pbx.messageRURI, pbx.messageCT, pbx.messageBody, pbx.authedUser
	pbx.mu.Unlock()
	if ruri != "sip:1234@callee.invalid" {
		t.Fatalf("the MESSAGE's Request-URI is %q, want the recipient's address untouched (sip:1234@callee.invalid)", ruri)
	}
	if from != "2001" || to != "1234" {
		t.Fatalf("the MESSAGE's From/To users are %q/%q, want 2001/1234 — the account's identity, the recipient", from, to)
	}
	if ct != "text/plain; charset=utf-8" {
		t.Fatalf("the MESSAGE's Content-Type is %q, want text/plain; charset=utf-8", ct)
	}
	if body != "Hello from the browser" {
		t.Fatalf("the MESSAGE's body is %q, want %q", body, "Hello from the browser")
	}
	if authed != "2001" {
		t.Fatalf("the authenticated retry's digest username is %q, want 2001", authed)
	}

	// Inner whitespace survives the raw-line parse.
	spacedMsg := chat("/send 1234@callee.invalid spaced   out\ttext")
	waitBotMessage(t, probe, botId, "the second sent confirmation", isChatReply(spacedMsg, "Message sent to"))
	pbx.waitForPBX(t, "the second MESSAGE", func() bool { return pbx.messages == 2 })
	pbx.mu.Lock()
	spaced := pbx.messageBody
	pbx.mu.Unlock()
	if spaced != "spaced   out\ttext" {
		t.Fatalf("the second MESSAGE's body is %q, want the inner whitespace preserved (%q)", spaced, "spaced   out\ttext")
	}

	// Over the size cap (the default 800): rejected in chat, never sent.
	tooBigMsg := chat("/send 1234@callee.invalid " + strings.Repeat("x", 801))
	waitBotMessage(t, probe, botId, "the over-limit rejection", isChatReply(tooBigMsg, "over the 800-byte limit"))
	time.Sleep(100 * time.Millisecond) // let a would-be wire message land
	pbx.mu.Lock()
	messages := pbx.messages
	pbx.mu.Unlock()
	if messages != 2 {
		t.Fatalf("the registrar recorded %d MESSAGEs, want 2 — the over-limit one must never reach the wire", messages)
	}
}

// TestSipBotInboundMessage covers the reverse direction (§7.3): a SIP
// MESSAGE arriving at the registered account's socket is rendered and
// forwarded into the owner's chat — text/plain verbatim (attributed to
// the SIP From), a message/cpim reduced (the inner From/To kept, the
// attachment stripped) — and an unsupported media type is answered 415
// with the browser never bothered.
func TestSipBotInboundMessage(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot := startBot(t, net, "bot", "2-bot", "", nil)
	user, probe, _, _ := startUserProbe(t, net, "user", "1-user")
	botId, userId := pairUp(t, bot, user)
	pbx := newFakePBX(t, 200, true)

	dc := probe.waitDC(t, botId, msg_handler.DataChannelLabelMessages)
	chat := func(text string) ss.MsgId {
		t.Helper()
		m := baseMsg(ss.WellKnownChIdMain, userId, botId)
		m["plaintext"] = text
		if err := dc.SendText(mustJSON(t, m)); err != nil {
			t.Fatalf("SendText: %v", err)
		}
		return ss.MsgId(m["msgId"].(string))
	}

	registerMsg := chat("/register 2001@" + pbx.addr + " passW_0rd")
	waitBotMessage(t, probe, botId, "the registered reply", isChatReply(registerMsg, "Registered as"))

	// text/plain: forwarded verbatim, attributed to the SIP From.
	if res := pbx.sendMessage(t, "2001", "text/plain; charset=utf-8", "hello there"); res.StatusCode != 200 {
		t.Fatalf("the text/plain MESSAGE's answer is %d, want 200", res.StatusCode)
	}
	plain := waitBotMessage(t, probe, botId, "the forwarded text message", isChatAnnouncement("hello there"))
	if want := "Message from sip:caller@" + pbx.ip + ":\n\nhello there"; plain.plaintext != want {
		t.Fatalf("the forwarded text is %q, want %q", plain.plaintext, want)
	}

	// message/cpim: the inner From/To are kept, the attachment stripped.
	cpimBody := "From: Alice <sip:alice@example.com>\r\n" +
		"To: Bob <sip:bob@example.com>\r\n" +
		"\r\n" +
		"Content-Type: multipart/mixed; boundary=bb\r\n" +
		"\r\n" +
		"--bb\r\n" +
		"Content-Type: text/plain\r\n" +
		"\r\n" +
		"Hi Bob\r\n" +
		"--bb\r\n" +
		"Content-Type: application/octet-stream\r\n" +
		"\r\n" +
		"YmluYXJ5\r\n" +
		"--bb--\r\n"
	if res := pbx.sendMessage(t, "2001", "message/cpim", cpimBody); res.StatusCode != 200 {
		t.Fatalf("the cpim MESSAGE's answer is %d, want 200", res.StatusCode)
	}
	cpim := waitBotMessage(t, probe, botId, "the forwarded cpim message", isChatAnnouncement("Hi Bob"))
	wantCPIM := "Message from Alice <sip:alice@example.com> to Bob <sip:bob@example.com>:\n\nHi Bob"
	if cpim.plaintext != wantCPIM {
		t.Fatalf("the forwarded cpim text is %q, want %q", cpim.plaintext, wantCPIM)
	}

	// An unsupported media type: a 415 naming the supported ones, and the
	// browser never hears of it.
	if res := pbx.sendMessage(t, "2001", "application/json", `{"marker":"unsupported-7f3a"}`); res.StatusCode != 415 {
		t.Fatalf("the unsupported MESSAGE's answer is %d, want 415", res.StatusCode)
	} else if accept := res.GetHeader("Accept"); accept == nil || accept.Value() != "text/plain, message/cpim" {
		t.Fatalf("the 415's Accept is %v, want text/plain, message/cpim", accept)
	}
	time.Sleep(200 * time.Millisecond) // let a would-be forwarded message land
	if n := countBotMessages(probe, botId, isChatAnnouncement("unsupported-7f3a")); n != 0 {
		t.Fatalf("the unsupported MESSAGE reached the browser %d time(s), want 0 — it must drop silently", n)
	}
}

// TestSipBotTestCall covers /test-call: the configured test callee is
// dialed; the user's decline of the webrtc-leg then takes the sip-leg
// down with it (the callee sees the BYE).
func TestSipBotTestCall(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	pbx := newFakePBX(t, 200, true)
	bot := startBot(t, net, "bot", "2-bot", "9664@"+pbx.addr, nil)
	user, probe, _, _ := startUserProbe(t, net, "user", "1-user")
	botId, userId := pairUp(t, bot, user)

	dc := probe.waitDC(t, botId, msg_handler.DataChannelLabelMessages)
	chat := func(text string) ss.MsgId {
		t.Helper()
		m := baseMsg(ss.WellKnownChIdMain, userId, botId)
		m["plaintext"] = text
		if err := dc.SendText(mustJSON(t, m)); err != nil {
			t.Fatalf("SendText: %v", err)
		}
		return ss.MsgId(m["msgId"].(string))
	}
	sip := func(body map[string]any) {
		t.Helper()
		m := baseMsg(ss.WellKnownChIdMain, userId, botId)
		m["mimeType"] = wireMimeSip
		m["sip"] = body
		if err := dc.SendText(mustJSON(t, m)); err != nil {
			t.Fatalf("SendText: %v", err)
		}
	}

	registerMsg := chat("/register 2001@" + pbx.addr + " passW_0rd")
	waitBotMessage(t, probe, botId, "the registered reply", isChatReply(registerMsg, "Registered as"))

	testCallMsg := chat("/test-call")
	waitBotMessage(t, probe, botId, "the calling line", isChatReply(testCallMsg, "Calling 9664@"))
	pbx.waitForPBX(t, "the test callee's INVITE", func() bool { return pbx.invites == 1 && pbx.inviteTo == "9664" })
	pbx.waitDialog(t)
	invite := waitBotMessage(t, probe, botId, "the bot's INVITE", isSipInvite)
	callId := invite.sip.CallId

	// The user declines; the answered callee gets the BYE.
	sip(map[string]any{"callId": callId, "response": map[string]any{"code": 603, "phrase": "Decline"}})
	waitBotMessage(t, probe, botId, "the declined line", isChatReply(testCallMsg, "Call declined."))
	pbx.waitForPBX(t, "the callee's BYE", func() bool { return pbx.byes == 1 })
}

// TestSipBotPoolCall covers the pool's happy path: the session's start
// already loans an account from the pool for the credential-less user
// and registers it (the REGISTER carries the pooled identity, digest
// and all), announcing it unthreaded; the /call then dials straight
// away with the pooled identity. /unregister returns the loan (the
// de-REGISTER goes out), and the pool hands the same account out again
// to the next /call — the on-demand fallback, whose registration line
// is threaded on the command.
func TestSipBotPoolCall(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	pbx := newFakePBX(t, 200, true)
	pool, err := NewSIPCredentialPool([]SIPCredential{{URI: "sip:1101@" + pbx.addr, Password: "poolpass"}}, nil)
	if err != nil {
		t.Fatalf("NewSIPCredentialPool: %v", err)
	}
	bot := startBot(t, net, "bot", "2-bot", "", pool)
	user, probe, _, _ := startUserProbe(t, net, "user", "1-user")
	botId, userId := pairUp(t, bot, user)

	dc := probe.waitDC(t, botId, msg_handler.DataChannelLabelMessages)
	chat := func(text string) ss.MsgId {
		t.Helper()
		m := baseMsg(ss.WellKnownChIdMain, userId, botId)
		m["plaintext"] = text
		if err := dc.SendText(mustJSON(t, m)); err != nil {
			t.Fatalf("SendText: %v", err)
		}
		return ss.MsgId(m["msgId"].(string))
	}

	// The session's start already loaned the pool's account and
	// registered it — the announcement is the unthreaded WriterFor path,
	// so the first /call need not bear the REGISTER round trip.
	waitBotMessage(t, probe, botId, "the session-start loan's announcement", isChatAnnouncement("Registered as sip:1101@"+pbx.addr+" (an account from the bot's pool)"))
	pbx.waitForPBX(t, "the pooled account's REGISTER, its own socket and identity", func() bool {
		return len(pbx.registrations) == 1 &&
			pbx.registrations[0].from == "1101" && pbx.registrations[0].contact == "1101"
	})

	// The /call finds the loan standing: no registration line of its own,
	// straight to the calling line.
	callMsg := chat("/call 1001@" + pbx.addr)
	waitBotMessage(t, probe, botId, "the calling line", isChatReply(callMsg, "Calling 1001@"))
	if n := countBotMessages(probe, botId, isChatReply(callMsg, "Registered as")); n != 0 {
		t.Fatalf("the /call announced %d registration lines, want none (the session start announced it)", n)
	}
	pbx.waitForPBX(t, "the callee's INVITE with the pooled identity", func() bool {
		return pbx.invites == 1 && pbx.inviteFrom == "1101" && pbx.inviteTo == "1001"
	})
	pbx.waitForPBX(t, "the pooled digest username", func() bool { return pbx.authedUser == "1101" })

	// /unregister returns the loan (the de-REGISTER goes out)…
	unregisterMsg := chat("/unregister")
	waitBotMessage(t, probe, botId, "the unregistered reply", isChatReply(unregisterMsg, "Unregistered."))
	pbx.waitForPBX(t, "the pooled account's de-REGISTER", func() bool { return pbx.unregistered })

	// …and the pool hands the same account out again to the next /call —
	// the on-demand path, whose registration line is threaded on the
	// command (the session start's loan is long gone).
	callMsg2 := chat("/call 1002@" + pbx.addr)
	waitBotMessage(t, probe, botId, "the second pooled registration line", isChatReply(callMsg2, "Registered as sip:1101@"))
	pbx.waitForPBX(t, "the re-loaned account's REGISTER", func() bool { return pbx.registers == 2 })
}

// TestSipBotPoolExhausted covers the pool's limits: one account, two
// users — the second user's /call answers with the pool-empty reply and
// the /register hint; the manual path is unaffected; and once the first
// user /unregisters, the returned loan serves a /call again.
func TestSipBotPoolExhausted(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	pbx := newFakePBX(t, 486, true) // busy: the calls need never complete
	pool, err := NewSIPCredentialPool(nil, []SIPCredentialRange{{UsernameRange: "1101-1101", Password: "poolpass", SIPServer: pbx.addr}})
	if err != nil {
		t.Fatalf("NewSIPCredentialPool: %v", err)
	}
	bot := startBot(t, net, "bot", "2-bot", "", pool)
	alice, aliceProbe, _, _ := startUserProbe(t, net, "alice", "1-alice")
	botId, aliceId := pairUp(t, bot, alice)

	chat := func(probe *wireProbe, userId ss.SubscriberId, text string) ss.MsgId {
		t.Helper()
		dc := probe.waitDC(t, botId, msg_handler.DataChannelLabelMessages)
		m := baseMsg(ss.WellKnownChIdMain, userId, botId)
		m["plaintext"] = text
		if err := dc.SendText(mustJSON(t, m)); err != nil {
			t.Fatalf("SendText: %v", err)
		}
		return ss.MsgId(m["msgId"].(string))
	}

	// Alice's session start loans the pool's only account (a range's
	// single member) — the announcement is unthreaded. Bob must not
	// arrive before it landed: the session starts race (the dcmsg
	// channels come up asynchronously, pair order notwithstanding), so
	// bob starts only once the loan is deterministically alice's.
	waitBotMessage(t, aliceProbe, botId, "alice's session-start announcement", isChatAnnouncement("Registered as sip:1101@"))
	bob, bobProbe, _, _ := startUserProbe(t, net, "bob", "1-bob")
	_, bobId := pairUp(t, bot, bob)

	// Her /call finds the loan standing and dials straight away; Bob's
	// session start found the pool empty (silently), so his /call answers
	// with the pool-empty reply, pointing at /register; his own /register
	// is unaffected.
	aliceCall := chat(aliceProbe, aliceId, "/call 1001@"+pbx.addr)
	waitBotMessage(t, aliceProbe, botId, "alice's calling line", isChatReply(aliceCall, "Calling 1001@"))
	if n := countBotMessages(aliceProbe, botId, isChatReply(aliceCall, "Registered as")); n != 0 {
		t.Fatalf("alice's /call announced %d registration lines, want none (the session start announced it)", n)
	}
	bobCall := chat(bobProbe, bobId, "/call 1001@"+pbx.addr)
	waitBotMessage(t, bobProbe, botId, "the pool-empty reply", isChatReply(bobCall, "pool of SIP accounts is empty"))
	bobRegister := chat(bobProbe, bobId, "/register 2001@"+pbx.addr+" s3cret")
	waitBotMessage(t, bobProbe, botId, "bob's registered reply", isChatReply(bobRegister, "Registered as sip:2001@"))

	// Alice's /unregister returns the loan; once bob drops his own
	// credential, his next /call loans it.
	aliceUnregister := chat(aliceProbe, aliceId, "/unregister")
	waitBotMessage(t, aliceProbe, botId, "alice's unregistered reply", isChatReply(aliceUnregister, "Unregistered."))
	bobUnregister := chat(bobProbe, bobId, "/unregister")
	waitBotMessage(t, bobProbe, botId, "bob's unregistered reply", isChatReply(bobUnregister, "Unregistered."))
	bobCall2 := chat(bobProbe, bobId, "/call 1002@"+pbx.addr)
	waitBotMessage(t, bobProbe, botId, "bob's pooled registration line", isChatReply(bobCall2, "Registered as sip:1101@"))
	pbx.waitForPBX(t, "the account's second loan's REGISTER", func() bool {
		n := 0
		for _, r := range pbx.registrations {
			if r.from == "1101" {
				n++
			}
		}
		return n == 2
	})
}

// TestSipBotPoolRegistrationFails covers the registrar refusing the
// pooled account: the /call answers with the failure and the /register
// hint, the loan returns to the pool (the next /call tries an account
// again — here the same one, the pool's only), and the manual path is
// unaffected.
func TestSipBotPoolRegistrationFails(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	pbx := newFakePBX(t, 200, true)
	pbx.refuseRegister("1101") // the registrar refuses the pooled account
	pool, err := NewSIPCredentialPool([]SIPCredential{{URI: "sip:1101@" + pbx.addr, Password: "badpass"}}, nil)
	if err != nil {
		t.Fatalf("NewSIPCredentialPool: %v", err)
	}
	bot := startBot(t, net, "bot", "2-bot", "", pool)
	user, probe, _, _ := startUserProbe(t, net, "user", "1-user")
	botId, userId := pairUp(t, bot, user)

	dc := probe.waitDC(t, botId, msg_handler.DataChannelLabelMessages)
	chat := func(text string) ss.MsgId {
		t.Helper()
		m := baseMsg(ss.WellKnownChIdMain, userId, botId)
		m["plaintext"] = text
		if err := dc.SendText(mustJSON(t, m)); err != nil {
			t.Fatalf("SendText: %v", err)
		}
		return ss.MsgId(m["msgId"].(string))
	}

	callMsg := chat("/call 1001@" + pbx.addr)
	waitBotMessage(t, probe, botId, "the pooled-registration failure", isChatReply(callMsg, "could not be registered"))
	// The reply names the account and points at /register.
	waitBotMessage(t, probe, botId, "the failure's detail", isChatReply(callMsg, "sip:1101@"+pbx.addr))

	// The loan returned to the pool: the next /call tries it again (and
	// fails the same way — it is not lost, not stuck).
	callMsg2 := chat("/call 1001@" + pbx.addr)
	waitBotMessage(t, probe, botId, "the second try fails the same way", isChatReply(callMsg2, "could not be registered"))

	// The manual path is unaffected.
	registerMsg := chat("/register 2001@" + pbx.addr + " s3cret")
	waitBotMessage(t, probe, botId, "the manual registered reply", isChatReply(registerMsg, "Registered as sip:2001@"))
}

// TestSipBotPoolSessionEndReleases covers the lifecycle hooks: the
// user's chat session starts (the pooled loan and its REGISTER happen
// there, announced unthreaded) and ends (the subscriber drops out and
// ages out), and the bot's session-end teardown runs — the loan's
// de-REGISTER goes out and the account returns to the pool, to serve
// the next user's session. A shorter subscriber aging makes the dropout
// observable.
func TestSipBotPoolSessionEndReleases(t *testing.T) {
	net := newClientTestNet(t, 300*time.Millisecond)
	pbx := newFakePBX(t, 486, true)
	pool, err := NewSIPCredentialPool(nil, []SIPCredentialRange{{UsernameRange: "1101-1101", Password: "poolpass", SIPServer: pbx.addr}})
	if err != nil {
		t.Fatalf("NewSIPCredentialPool: %v", err)
	}
	bot := startBot(t, net, "bot", "2-bot", "", pool)
	alice, aliceProbe, _, aliceStop := startUserProbe(t, net, "alice", "1-alice")
	botId, aliceId := pairUp(t, bot, alice)

	chat := func(probe *wireProbe, userId ss.SubscriberId, text string) ss.MsgId {
		t.Helper()
		dc := probe.waitDC(t, botId, msg_handler.DataChannelLabelMessages)
		m := baseMsg(ss.WellKnownChIdMain, userId, botId)
		m["plaintext"] = text
		if err := dc.SendText(mustJSON(t, m)); err != nil {
			t.Fatalf("SendText: %v", err)
		}
		return ss.MsgId(m["msgId"].(string))
	}

	// The session's start loaned the account and registered it.
	waitBotMessage(t, aliceProbe, botId, "alice's session-start announcement", isChatAnnouncement("Registered as sip:1101@"))
	pbx.waitForPBX(t, "the pooled account's REGISTER", func() bool { return pbx.registers == 1 })
	aliceCall := chat(aliceProbe, aliceId, "/call 1001@"+pbx.addr)
	waitBotMessage(t, aliceProbe, botId, "alice's calling line", isChatReply(aliceCall, "Calling 1001@"))

	// Alice drops out: her registration ages out, the bot's session with
	// her ends, and the hook returns the loan — the de-REGISTER goes out.
	aliceStop()
	pbx.waitForPBX(t, "the pooled account's de-REGISTER on the session's end", func() bool { return pbx.unregistered })

	// Bob's session start loans the returned account — a pool that kept
	// the loan would leave him empty-handed.
	bob, bobProbe, _, _ := startUserProbe(t, net, "bob", "1-bob")
	_, bobId := pairUp(t, bot, bob)
	waitBotMessage(t, bobProbe, botId, "bob's session-start announcement", isChatAnnouncement("Registered as sip:1101@"))
	bobCall := chat(bobProbe, bobId, "/call 1002@"+pbx.addr)
	waitBotMessage(t, bobProbe, botId, "bob's calling line", isChatReply(bobCall, "Calling 1002@"))
}

// TestSipBotIPv6 covers the sip leg over IPv6: an account's socket takes
// its registrar's address family — a pooled account (an IPv6 sipServer in
// the pool) and a user's own /register alike register against an IPv6
// registrar and dial through it, the wire identity the account's own.
// Skipped when the host has no IPv6 loopback.
func TestSipBotIPv6(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	pbx := newFakePBX6(t, 486, true) // busy: the calls need never complete
	pool, err := NewSIPCredentialPool(nil, []SIPCredentialRange{{UsernameRange: "1101-1102", Password: "poolpass", SIPServer: pbx.addr}})
	if err != nil {
		t.Fatalf("NewSIPCredentialPool: %v", err)
	}
	bot := startBotAuto(t, net, "bot", "2-bot", "", pool)
	user, probe, _, _ := startUserProbe(t, net, "user", "1-user")
	botId, userId := pairUp(t, bot, user)

	dc := probe.waitDC(t, botId, msg_handler.DataChannelLabelMessages)
	chat := func(text string) ss.MsgId {
		t.Helper()
		m := baseMsg(ss.WellKnownChIdMain, userId, botId)
		m["plaintext"] = text
		if err := dc.SendText(mustJSON(t, m)); err != nil {
			t.Fatalf("SendText: %v", err)
		}
		return ss.MsgId(m["msgId"].(string))
	}

	// The session's start loaned the pooled account and registered it
	// against the IPv6 registrar; the /call then dials through it.
	waitBotMessage(t, probe, botId, "the session-start loan's announcement", isChatAnnouncement("Registered as sip:1101@"+pbx.addr))
	pbx.waitForPBX(t, "the pooled account's REGISTER over IPv6", func() bool {
		return len(pbx.registrations) == 1 && pbx.registrations[0].from == "1101"
	})
	callMsg := chat("/call 1001@" + pbx.addr)
	waitBotMessage(t, probe, botId, "the calling line", isChatReply(callMsg, "Calling 1001@"))
	pbx.waitForPBX(t, "the INVITE over IPv6", func() bool {
		return pbx.invites == 1 && pbx.inviteFrom == "1101" && pbx.inviteTo == "1001"
	})

	// The manual path takes the same family-aware socket.
	registerMsg := chat("/register 2001@" + pbx.addr + " s3cret")
	waitBotMessage(t, probe, botId, "the manual registered reply over IPv6", isChatReply(registerMsg, "Registered as sip:2001@"+pbx.addr))
	pbx.waitForPBX(t, "the manual REGISTER over IPv6", func() bool { return pbx.registers == 2 })
}

// TestSipBotIPPreference covers the ipPreference attribute end to end:
// the registrar's HOSTNAME (not a literal) resolves through the bot's
// filtering DNS proxy — a fake upstream resolver maps pbx.test to both
// loopbacks, one fake PBX per family sharing one port — and the
// REGISTER and the INVITE must arrive at the preferred family's PBX
// alone, the wire identity staying the hostname.
func TestSipBotIPPreference(t *testing.T) {
	for _, tc := range []struct {
		name string
		pref IPPreference
		v6   bool
	}{
		{"v4Only", IPPreferenceV4Only, false},
		{"v6Only", IPPreferenceV6Only, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// One loopback port for both families' PBXs.
			rsv, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
			if err != nil {
				t.Fatalf("reserve the shared port: %v", err)
			}
			port := rsv.LocalAddr().(*net.UDPAddr).Port
			if err := rsv.Close(); err != nil {
				t.Fatalf("release the reserved port: %v", err)
			}
			pbx4 := newFakePBXOnPort(t, "127.0.0.1", port, 486, true) // busy: the calls need never complete
			pbx6 := newFakePBXOnPort(t, "::1", port, 486, true)
			upstream := newFakeDNS(t, map[string][]string{"pbx.test.": {"127.0.0.1", "::1"}}, nil, nil)

			net := newClientTestNet(t, ss.DefaultSubscriberAging)
			c, _ := startClient(t, net, "bot", func(c *rtc.RTCClientConfiguration) {
				c.SubscriberId = "2-bot"
			})
			New(c, NewOnMemoryUserSessionStorage(), nil, Configuration{
				Logger:              testLogger(t),
				IPPreference:        tc.pref,
				UpstreamDNSResolver: upstream.addr,
			})
			user, probe, _, _ := startUserProbe(t, net, "user", "1-user")
			botId, userId := pairUp(t, c, user)

			dc := probe.waitDC(t, botId, msg_handler.DataChannelLabelMessages)
			chat := func(text string) ss.MsgId {
				t.Helper()
				m := baseMsg(ss.WellKnownChIdMain, userId, botId)
				m["plaintext"] = text
				if err := dc.SendText(mustJSON(t, m)); err != nil {
					t.Fatalf("SendText: %v", err)
				}
				return ss.MsgId(m["msgId"].(string))
			}

			want, other := pbx4, pbx6
			if tc.v6 {
				want, other = pbx6, pbx4
			}
			registrar := fmt.Sprintf("pbx.test:%d", port)

			registerMsg := chat("/register 2001@" + registrar + " passW_0rd")
			waitBotMessage(t, probe, botId, "the registered reply", isChatReply(registerMsg, "Registered as sip:2001@"+registrar))
			want.waitForPBX(t, "the REGISTER at the preferred family's PBX", func() bool {
				return want.registers == 1 && want.registrations[0].from == "2001"
			})

			callMsg := chat("/call 1001@" + registrar)
			waitBotMessage(t, probe, botId, "the call-failed line", isChatReply(callMsg, "Call failed: "))
			want.waitForPBX(t, "the INVITE at the preferred family's PBX", func() bool {
				return want.invites == 1 && want.inviteFrom == "2001" && want.inviteTo == "1001"
			})

			// The suppressed family's PBX stayed silent (the register and
			// the call have both completed at the other one, so a misrouted
			// message would have arrived by now).
			time.Sleep(300 * time.Millisecond)
			other.mu.Lock()
			n := other.registers + other.invites
			other.mu.Unlock()
			if n != 0 {
				t.Errorf("the suppressed family's PBX received %d messages (registers+invites), want 0", n)
			}

			// A clean unregister while everything is alive: the de-REGISTER
			// takes the same family (and the teardown races nothing).
			unregisterMsg := chat("/unregister")
			waitBotMessage(t, probe, botId, "the unregistered reply", isChatReply(unregisterMsg, "Unregistered."))
			want.waitForPBX(t, "the de-REGISTER at the preferred family's PBX", func() bool { return want.unregistered })
		})
	}
}

// TestSipBotMyNumber covers /my-number's three answers: the not-
// registered hint when nothing is registered, the AOR once registered,
// and the not-registered answer again after /unregister.
func TestSipBotMyNumber(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot := startBot(t, net, "bot", "2-bot", "", nil)
	user, probe, _, _ := startUserProbe(t, net, "user", "1-user")
	botId, userId := pairUp(t, bot, user)
	pbx := newFakePBX(t, 200, true)

	dc := probe.waitDC(t, botId, msg_handler.DataChannelLabelMessages)
	chat := func(text string) ss.MsgId {
		t.Helper()
		m := baseMsg(ss.WellKnownChIdMain, userId, botId)
		m["plaintext"] = text
		if err := dc.SendText(mustJSON(t, m)); err != nil {
			t.Fatalf("SendText: %v", err)
		}
		return ss.MsgId(m["msgId"].(string))
	}

	// No registration yet.
	m := chat("/my-number")
	waitBotMessage(t, probe, botId, "the not-registered answer", isChatReply(m, "Not registered to a SIP registrar yet"))

	// Registered: the number is the AOR.
	reg := chat("/register 2001@" + pbx.addr + " passW_0rd")
	waitBotMessage(t, probe, botId, "the registered reply", isChatReply(reg, "Registered as"))
	m = chat("/my-number")
	waitBotMessage(t, probe, botId, "the number", isChatReply(m, "Your number is sip:2001@"+pbx.addr+"."))

	// Unregistered again.
	un := chat("/unregister")
	waitBotMessage(t, probe, botId, "the unregistered reply", isChatReply(un, "Unregistered."))
	m = chat("/my-number")
	waitBotMessage(t, probe, botId, "the not-registered answer again", isChatReply(m, "Not registered to a SIP registrar yet"))
}

// TestSipBotMyNumberPooled covers the pooled variant: the session's
// start loans the account (announced unthreaded), so /my-number shows
// the loaned number — marked as the pool's — right away; a second user,
// whose session start found the pool empty, gets the not-registered
// answer with the pool hint.
func TestSipBotMyNumberPooled(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	pbx := newFakePBX(t, 200, true)
	pool, err := NewSIPCredentialPool([]SIPCredential{{URI: "sip:1101@" + pbx.addr, Password: "poolpass"}}, nil)
	if err != nil {
		t.Fatalf("NewSIPCredentialPool: %v", err)
	}
	bot := startBot(t, net, "bot", "2-bot", "", pool)
	alice, aliceProbe, _, _ := startUserProbe(t, net, "alice", "1-alice")
	botId, aliceId := pairUp(t, bot, alice)

	chat := func(probe *wireProbe, userId ss.SubscriberId, text string) ss.MsgId {
		t.Helper()
		dc := probe.waitDC(t, botId, msg_handler.DataChannelLabelMessages)
		m := baseMsg(ss.WellKnownChIdMain, userId, botId)
		m["plaintext"] = text
		if err := dc.SendText(mustJSON(t, m)); err != nil {
			t.Fatalf("SendText: %v", err)
		}
		return ss.MsgId(m["msgId"].(string))
	}

	// The session's start loaned the pool's account; /my-number shows it,
	// marked as the pool's.
	waitBotMessage(t, aliceProbe, botId, "the session-start loan's announcement", isChatAnnouncement("Registered as sip:1101@"+pbx.addr+" (an account from the bot's pool)"))
	m := chat(aliceProbe, aliceId, "/my-number")
	waitBotMessage(t, aliceProbe, botId, "the pooled number", isChatReply(m, "Your number is sip:1101@"+pbx.addr+" (an account from the bot's pool)."))

	// Bob's session start found the pool empty (alice holds the only
	// account), so his /my-number answers not-registered, with the pool
	// hint.
	bob, bobProbe, _, _ := startUserProbe(t, net, "bob", "1-bob")
	_, bobId := pairUp(t, bot, bob)
	m = chat(bobProbe, bobId, "/my-number")
	waitBotMessage(t, bobProbe, botId, "the not-registered answer with the pool hint", func(got *rawMsg) bool {
		return isChatReply(m, "Not registered to a SIP registrar yet")(got) && strings.Contains(got.plaintext, "pool")
	})
}

// TestSipBotInboundCallEndToEnd covers the reverse direction: a SIP
// subscriber dials the registered number, the bot selects the call's
// codec from the caller's offer (the first of the presumed WebRTC codec
// list the offer carries — opus in one variant, PCMU in the other) and
// rings the browser with the selection pinned; on the accept the 200
// OK's SDP answer carries exactly [the selection, telephone-event];
// media crosses byte for byte in both directions; and the caller's BYE
// ends the browser leg (BYE, the chat line, the ended amend).
func TestSipBotInboundCallEndToEnd(t *testing.T) {
	for _, tc := range []struct {
		name       string
		opus       bool // the fake PBX's offered codec (and so the call's)
		micCodec   webrtc.RTPCodecCapability
		voice      func(int) [][]byte
		stream     func(*fakeCall, context.Context, [][]byte)
		wantOffer  []string // the webrtc-leg pin's m-line payload types
		wantAnswer []string // the 200 OK's answer m-line payload types
	}{
		{
			name:       "opus",
			opus:       true,
			micCodec:   webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
			voice:      opusVoice,
			stream:     func(fc *fakeCall, ctx context.Context, p [][]byte) { fc.stream(ctx, p) },
			wantOffer:  []string{"111", "126"},
			wantAnswer: []string{"96", "101"},
		},
		{
			name:       "pcmu",
			opus:       false,
			micCodec:   webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: 8000, Channels: 1},
			voice:      pcmuVoice,
			stream:     func(fc *fakeCall, ctx context.Context, p [][]byte) { fc.streamRTP(ctx, p, 0, 160) },
			wantOffer:  []string{"0", "126"},
			wantAnswer: []string{"0", "101"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			net := newClientTestNet(t, ss.DefaultSubscriberAging)
			bot := startBot(t, net, "bot", "2-bot", "", nil)
			var pcBox atomic.Pointer[webrtc.PeerConnection]
			user, probe, tprobe, _ := startUserProbeOn(t, net, "user", "1-user", func(c *rtc.RTCClientConfiguration) {
				c.NewPeerConnection = tapPCFactory(&pcBox)
			})
			botId, userId := pairUp(t, bot, user)
			pbx := newFakePBX(t, 200, tc.opus)

			dc := probe.waitDC(t, botId, msg_handler.DataChannelLabelMessages)
			chat := func(text string) ss.MsgId {
				t.Helper()
				m := baseMsg(ss.WellKnownChIdMain, userId, botId)
				m["plaintext"] = text
				if err := dc.SendText(mustJSON(t, m)); err != nil {
					t.Fatalf("SendText: %v", err)
				}
				return ss.MsgId(m["msgId"].(string))
			}
			sip := func(body map[string]any) {
				t.Helper()
				m := baseMsg(ss.WellKnownChIdMain, userId, botId)
				m["mimeType"] = wireMimeSip
				m["sip"] = body
				if err := dc.SendText(mustJSON(t, m)); err != nil {
					t.Fatalf("SendText: %v", err)
				}
			}

			registerMsg := chat("/register 2001@" + pbx.addr + " passW_0rd")
			waitBotMessage(t, probe, botId, "the registered reply", isChatReply(registerMsg, "Registered as"))

			// Someone on the SIP network dials the registered number.
			call := pbx.call(t, "2001")

			// The browser rings: the bot's INVITE, the chat line; the caller
			// hears the 180.
			invite := waitBotMessage(t, probe, botId, "the bot's INVITE", func(m *rawMsg) bool {
				return isSipInvite(m) && m.sip.XMedia == "voice" && m.sip.XCallStatus == "inviting"
			})
			callId, inviteMsgId := invite.sip.CallId, invite.msgId
			waitBotMessage(t, probe, botId, "the incoming-call line", func(m *rawMsg) bool {
				return m.mimeType == wireMimePlaintext && strings.Contains(m.plaintext, "Incoming call from caller@")
			})
			call.waitRinging()

			// The ring's media offer is pinned to the codec selected from
			// the caller's offer: it carries exactly [the selection,
			// telephone-event] toward the browser.
			pinned := waitOfferSettled(t, &pcBox)
			if got := sdpAudioFormats(t, pinned.SDP); !slices.Equal(got, tc.wantOffer) {
				t.Fatalf("the ring's offer is pinned to %v, want %v:\n%s", got, tc.wantOffer, pinned.SDP)
			}

			// The user picks up: the mic joins the pinned m-line (its track
			// speaks the pinned codec), the 200 OK then flies — the bot's
			// own offer settled above, so the mic's re-offer is the only
			// one in flight.
			micTrack, err := webrtc.NewTrackLocalStaticSample(tc.micCodec, "mic", "probe")
			if err != nil {
				t.Fatalf("the mic track: %v", err)
			}
			if _, err := user.AddTrack(botId, micTrack); err != nil {
				t.Fatalf("attach the mic: %v", err)
			}
			spoken := tc.voice(5)
			micCtx, stopMic := context.WithCancel(context.Background())
			t.Cleanup(stopMic)
			go func() {
				for i := 0; ; i++ {
					if err := micTrack.WriteSample(pionmedia.Sample{Data: spoken[i%len(spoken)], Duration: 20 * time.Millisecond}); err != nil {
						return
					}
					select {
					case <-micCtx.Done():
						return
					case <-time.After(20 * time.Millisecond):
					}
				}
			}()
			sip(map[string]any{"callId": callId, "response": map[string]any{"code": 200, "phrase": "OK"}})

			// The caller gets his 200 OK + SDP; his ACK arms the relay. The
			// answer is pinned to the selection: exactly [the call's codec,
			// telephone-event].
			call.waitAnswered()
			call.mu.Lock()
			answer := call.answerSDP
			call.mu.Unlock()
			if got := sdpAudioFormats(t, answer); !slices.Equal(got, tc.wantAnswer) {
				t.Fatalf("the 200 OK's answer formats are %v, want the pin %v:\n%s", got, tc.wantAnswer, answer)
			}
			if tc.opus {
				// The answer announces stereo opus too: the negotiation keeps
				// the local codecs, so the offer's fmtp is the answer's as well.
				requireStereoOpusSDP(t, "the 200 OK's SDP answer", answer)
			}

			// The caller speaks: the packets cross to the browser byte for
			// byte. The stream runs ahead of the track's wait — the probe's
			// OnTrack fires only once media actually arrives.
			streamCtx, stopStream := context.WithCancel(context.Background())
			t.Cleanup(stopStream)
			voice := tc.voice(5)
			go tc.stream(call, streamCtx, voice)
			track := tprobe.waitTrack(t, 1)
			if codec := track.Codec(); !strings.EqualFold(codec.MimeType, tc.micCodec.MimeType) {
				t.Fatalf("the bot's track toward the browser is %s, want %s", codec.MimeType, tc.micCodec.MimeType)
			}
			waitBotMessage(t, probe, botId, "the accepted amend", isCallStatusAmend(inviteMsgId, callId, "accepted"))
			heardVoice := map[string]bool{}
			for len(heardVoice) < len(voice) {
				pkt := readOneRTP(t, track)
				heardVoice[string(pkt.Payload)] = true
			}
			for _, want := range voice {
				if !heardVoice[string(want)] {
					t.Fatalf("a streamed packet never reached the browser (the passthrough changed or lost it)")
				}
			}

			// The user speaks: the mic's packets cross to the caller byte
			// for byte.
			heard := map[string]bool{}
			deadline := time.Now().Add(testTimeout)
			for {
				for _, got := range pbx.waitRTP(t, 1) {
					heard[string(got)] = true
				}
				all := true
				for _, p := range spoken {
					if !heard[string(p)] {
						all = false
					}
				}
				if all {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("the caller heard %d of the %d distinct mic packets", len(heard), len(spoken))
				}
			}

			// The caller hangs up: the browser leg is ended by the bot (BYE,
			// the chat line, the ended amend).
			call.bye()
			waitBotMessage(t, probe, botId, "the bot's BYE", isSipRequest(callId, "BYE"))
			waitBotMessage(t, probe, botId, "the caller-hung-up line", func(m *rawMsg) bool {
				return m.mimeType == wireMimePlaintext && strings.Contains(m.plaintext, "Caller hung up.")
			})
			waitBotMessage(t, probe, botId, "the ended amend", isCallStatusAmend(inviteMsgId, callId, "ended"))
		})
	}
}

// TestSipBotInboundCallNoCommonCodec covers the offer sharing nothing
// with the presumed WebRTC codec list: the caller gets a 488 at once
// and the browser never rings.
func TestSipBotInboundCallNoCommonCodec(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot := startBot(t, net, "bot", "2-bot", "", nil)
	user, probe, _, _ := startUserProbe(t, net, "user", "1-user")
	botId, userId := pairUp(t, bot, user)
	pbx := newFakePBX(t, 200, true)

	dc := probe.waitDC(t, botId, msg_handler.DataChannelLabelMessages)
	chat := func(text string) ss.MsgId {
		t.Helper()
		m := baseMsg(ss.WellKnownChIdMain, userId, botId)
		m["plaintext"] = text
		if err := dc.SendText(mustJSON(t, m)); err != nil {
			t.Fatalf("SendText: %v", err)
		}
		return ss.MsgId(m["msgId"].(string))
	}

	registerMsg := chat("/register 2001@" + pbx.addr + " passW_0rd")
	waitBotMessage(t, probe, botId, "the registered reply", isChatReply(registerMsg, "Registered as"))

	// A caller whose offer shares nothing with the presumed list (speex
	// alone) gets the 488…
	speex := fmt.Sprintf("v=0\r\no=- 0 0 IN IP4 %s\r\ns=fakepbx\r\nc=IN IP4 %s\r\nt=0 0\r\nm=audio %d RTP/AVP 96\r\na=rtpmap:96 speex/8000\r\n", pbx.ip, pbx.ip, pbx.rtpPort())
	call := pbx.callSDP(t, "2001", []byte(speex))
	call.waitFailed(488)

	// …and the browser is never disturbed.
	time.Sleep(300 * time.Millisecond)
	if n := countBotMessages(probe, botId, isSipInvite); n != 0 {
		t.Fatalf("the unbridgeable call still rang the browser: %d INVITEs", n)
	}
}

// TestSipBotInboundCallDeclined covers the user's decline: the caller's
// INVITE is answered 603 and the call settles as rejected.
func TestSipBotInboundCallDeclined(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot := startBot(t, net, "bot", "2-bot", "", nil)
	user, probe, _, _ := startUserProbe(t, net, "user", "1-user")
	botId, userId := pairUp(t, bot, user)
	pbx := newFakePBX(t, 200, true)

	dc := probe.waitDC(t, botId, msg_handler.DataChannelLabelMessages)
	chat := func(text string) ss.MsgId {
		t.Helper()
		m := baseMsg(ss.WellKnownChIdMain, userId, botId)
		m["plaintext"] = text
		if err := dc.SendText(mustJSON(t, m)); err != nil {
			t.Fatalf("SendText: %v", err)
		}
		return ss.MsgId(m["msgId"].(string))
	}
	sip := func(body map[string]any) {
		t.Helper()
		m := baseMsg(ss.WellKnownChIdMain, userId, botId)
		m["mimeType"] = wireMimeSip
		m["sip"] = body
		if err := dc.SendText(mustJSON(t, m)); err != nil {
			t.Fatalf("SendText: %v", err)
		}
	}

	registerMsg := chat("/register 2001@" + pbx.addr + " passW_0rd")
	waitBotMessage(t, probe, botId, "the registered reply", isChatReply(registerMsg, "Registered as"))

	call := pbx.call(t, "2001")
	invite := waitBotMessage(t, probe, botId, "the bot's INVITE", isSipInvite)
	call.waitRinging()

	// The user declines: the caller gets the 603, the chat says so, the
	// log entry settles rejected.
	sip(map[string]any{"callId": invite.sip.CallId, "response": map[string]any{"code": 603, "phrase": "Decline"}})
	call.waitFailed(603)
	waitBotMessage(t, probe, botId, "the declined line", func(m *rawMsg) bool {
		return m.mimeType == wireMimePlaintext && strings.Contains(m.plaintext, "Call declined.")
	})
	waitBotMessage(t, probe, botId, "the rejected amend", isCallStatusAmend(invite.msgId, invite.sip.CallId, "rejected"))
}

// TestSipBotInboundCallCancelled covers the caller giving up while the
// browser rings: the bot CANCELS the browser leg.
func TestSipBotInboundCallCancelled(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot := startBot(t, net, "bot", "2-bot", "", nil)
	user, probe, _, _ := startUserProbe(t, net, "user", "1-user")
	botId, userId := pairUp(t, bot, user)
	pbx := newFakePBX(t, 200, true)

	dc := probe.waitDC(t, botId, msg_handler.DataChannelLabelMessages)
	chat := func(text string) ss.MsgId {
		t.Helper()
		m := baseMsg(ss.WellKnownChIdMain, userId, botId)
		m["plaintext"] = text
		if err := dc.SendText(mustJSON(t, m)); err != nil {
			t.Fatalf("SendText: %v", err)
		}
		return ss.MsgId(m["msgId"].(string))
	}

	registerMsg := chat("/register 2001@" + pbx.addr + " passW_0rd")
	waitBotMessage(t, probe, botId, "the registered reply", isChatReply(registerMsg, "Registered as"))

	call := pbx.call(t, "2001")
	invite := waitBotMessage(t, probe, botId, "the bot's INVITE", isSipInvite)

	// The caller gives up while the browser rings: the ring stops (the
	// bot's CANCEL), the chat says so, the log entry settles cancelled.
	call.cancel()
	waitBotMessage(t, probe, botId, "the bot's CANCEL", isSipRequest(invite.sip.CallId, "CANCEL"))
	waitBotMessage(t, probe, botId, "the caller-gave-up line", func(m *rawMsg) bool {
		return m.mimeType == wireMimePlaintext && strings.Contains(m.plaintext, "Caller gave up.")
	})
	waitBotMessage(t, probe, botId, "the cancelled amend", isCallStatusAmend(invite.msgId, invite.sip.CallId, "cancelled"))
}

// TestSipBotInboundCallBusy covers the one-call-per-user gate from the
// SIP side: while an outbound call stands, an inbound call gets a 486.
func TestSipBotInboundCallBusy(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot := startBot(t, net, "bot", "2-bot", "", nil)
	user, probe, _, _ := startUserProbe(t, net, "user", "1-user")
	botId, userId := pairUp(t, bot, user)
	pbx := newFakePBX(t, 200, true)

	dc := probe.waitDC(t, botId, msg_handler.DataChannelLabelMessages)
	chat := func(text string) ss.MsgId {
		t.Helper()
		m := baseMsg(ss.WellKnownChIdMain, userId, botId)
		m["plaintext"] = text
		if err := dc.SendText(mustJSON(t, m)); err != nil {
			t.Fatalf("SendText: %v", err)
		}
		return ss.MsgId(m["msgId"].(string))
	}

	registerMsg := chat("/register 2001@" + pbx.addr + " passW_0rd")
	waitBotMessage(t, probe, botId, "the registered reply", isChatReply(registerMsg, "Registered as"))

	// An outbound call stands (the callee answered).
	callMsg := chat("/call 1001@" + pbx.addr)
	waitBotMessage(t, probe, botId, "the calling line", isChatReply(callMsg, "Calling 1001@"))
	pbx.waitForPBX(t, "the callee's INVITE", func() bool { return pbx.invites == 1 })

	// A second caller dials the number: busy.
	inbound := pbx.call(t, "2001")
	inbound.waitFailed(486)

	hangupMsg := chat("/hangup")
	waitBotMessage(t, probe, botId, "the hung-up reply", isChatReply(hangupMsg, "Hung up."))
}

// TestSipBotInboundCallUserHangsUp covers the user hanging up an
// answered inbound call: the caller's dialog gets the BYE.
func TestSipBotInboundCallUserHangsUp(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot := startBot(t, net, "bot", "2-bot", "", nil)
	user, probe, _, _ := startUserProbe(t, net, "user", "1-user")
	botId, userId := pairUp(t, bot, user)
	pbx := newFakePBX(t, 200, true)

	dc := probe.waitDC(t, botId, msg_handler.DataChannelLabelMessages)
	chat := func(text string) ss.MsgId {
		t.Helper()
		m := baseMsg(ss.WellKnownChIdMain, userId, botId)
		m["plaintext"] = text
		if err := dc.SendText(mustJSON(t, m)); err != nil {
			t.Fatalf("SendText: %v", err)
		}
		return ss.MsgId(m["msgId"].(string))
	}
	sip := func(body map[string]any) {
		t.Helper()
		m := baseMsg(ss.WellKnownChIdMain, userId, botId)
		m["mimeType"] = wireMimeSip
		m["sip"] = body
		if err := dc.SendText(mustJSON(t, m)); err != nil {
			t.Fatalf("SendText: %v", err)
		}
	}

	registerMsg := chat("/register 2001@" + pbx.addr + " passW_0rd")
	waitBotMessage(t, probe, botId, "the registered reply", isChatReply(registerMsg, "Registered as"))

	call := pbx.call(t, "2001")
	invite := waitBotMessage(t, probe, botId, "the bot's INVITE", isSipInvite)
	call.waitRinging()
	sip(map[string]any{"callId": invite.sip.CallId, "response": map[string]any{"code": 200, "phrase": "OK"}})
	call.waitAnswered()

	// The user hangs up: the caller's dialog gets the BYE, the log entry
	// settles ended.
	sip(map[string]any{"callId": invite.sip.CallId, "method": "BYE"})
	pbx.waitForPBX(t, "the BYE at the caller", func() bool { return pbx.byes == 1 })
	waitBotMessage(t, probe, botId, "the ended amend", isCallStatusAmend(invite.msgId, invite.sip.CallId, "ended"))
}

// ---------------------------------------------------------------------------
// The video calls (docs/sip-bot.md §7.1/§7.2's video variants).
// ---------------------------------------------------------------------------

// streamSamples writes the payloads onto the track at the given sample
// duration and pace until ctx ends — the probe's mic pump.
func streamSamples(ctx context.Context, track *webrtc.TrackLocalStaticSample, payloads [][]byte, dur, pace time.Duration) {
	for i := 0; ; i++ {
		if err := track.WriteSample(pionmedia.Sample{Data: payloads[i%len(payloads)], Duration: dur}); err != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pace):
		}
	}
}

// streamVideoRTP writes the payloads as ≈30 fps RTP packets — the
// probe's camera pump. A TrackLocalStaticRTP, not a sample track:
// WriteRTP bypasses pion's payloader (which would prepend the VP8
// payload descriptor), so the payload crossing is the frame verbatim —
// the byte-for-byte assertions' premise.
func streamVideoRTP(ctx context.Context, track *webrtc.TrackLocalStaticRTP, payloads [][]byte) {
	for i := 0; ; i++ {
		pkt := &rtp.Packet{
			Header:  rtp.Header{Version: 2, SequenceNumber: uint16(i), Timestamp: uint32(i) * 3000, SSRC: 0xCA4E7A, Marker: true},
			Payload: payloads[i%len(payloads)],
		}
		if err := track.WriteRTP(pkt); err != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(33 * time.Millisecond):
		}
	}
}

// waitTrackKind waits for the probe's first remote track of the given
// kind — the video tests' two tracks (the callee's voice, the callee's
// camera) fire in stream order, so the kind, not the index, identifies.
func (p *trackProbe) waitTrackKind(t *testing.T, kind webrtc.RTPCodecType) *webrtc.TrackRemote {
	t.Helper()
	find := func() *webrtc.TrackRemote {
		p.mu.Lock()
		defer p.mu.Unlock()
		for _, tr := range p.tracks {
			if tr.Kind() == kind {
				return tr
			}
		}
		return nil
	}
	waitFor(t, "a remote "+kind.String()+" track", func() bool { return find() != nil })
	return find()
}

// hearAll reads the track until every payload of want crossed — the
// byte-for-byte passthrough assertion.
func hearAll(t *testing.T, track *webrtc.TrackRemote, want [][]byte) {
	t.Helper()
	heard := map[string]bool{}
	for len(heard) < len(want) {
		pkt := readOneRTP(t, track)
		heard[string(pkt.Payload)] = true
	}
}

// waitRecordedPT waits until the fake's media socket recorded every
// payload of want at the given PT — the camera's arrivals at the
// negotiated video PT among the mic's audio on the muxed socket.
func (f *fakePBX) waitRecordedPT(t *testing.T, pt uint8, want [][]byte) {
	t.Helper()
	waitFor(t, "the fake recording the video payloads at their PT", func() bool {
		heard := map[string]bool{}
		for _, p := range f.rtpPayloadsByPT(pt) {
			heard[string(p)] = true
		}
		for _, w := range want {
			if !heard[string(w)] {
				return false
			}
		}
		return true
	})
}

// acceptVideoCall is the video tests' browser-side accept: the mic and
// the camera join their pinned m-lines (pion's kind-based transceiver
// reuse), both streaming from then on, and the 200 OK flies. It returns
// the payload sets (the mic's, the camera's) the fake-side assertions
// then expect recorded.
func acceptVideoCall(t *testing.T, user *rtc.HeadlessRTCClient, botId ss.SubscriberId, sip func(map[string]any), callId string) (spoken, filmed [][]byte) {
	t.Helper()
	micTrack, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "mic", "probe")
	if err != nil {
		t.Fatalf("the mic track: %v", err)
	}
	cameraTrack, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}, "camera", "probe")
	if err != nil {
		t.Fatalf("the camera track: %v", err)
	}
	if _, err := user.AddTrack(botId, micTrack); err != nil {
		t.Fatalf("attach the mic: %v", err)
	}
	if _, err := user.AddTrack(botId, cameraTrack); err != nil {
		t.Fatalf("attach the camera: %v", err)
	}
	spoken, filmed = opusVoice(5), videoFrames(5)
	micCtx, stopMic := context.WithCancel(context.Background())
	t.Cleanup(stopMic)
	go streamSamples(micCtx, micTrack, spoken, 20*time.Millisecond, 20*time.Millisecond)
	go streamVideoRTP(micCtx, cameraTrack, filmed)
	sip(map[string]any{"callId": callId, "response": map[string]any{"code": 200, "phrase": "OK"}})
	return spoken, filmed
}

// TestSipBotVideoCallEndToEnd covers /video-call's happy path end to
// end: the SIP offer carries audio AND video on ONE muxed port; the
// callee's video answer rings the browser as a video call; the
// webrtc-leg's settled offer is pinned to the negotiated video codec;
// the callee's voice and camera cross to the browser byte for byte; the
// browser's own camera crosses to the callee at the negotiated video PT
// on the muxed socket; and the callee's BYE ends both legs.
func TestSipBotVideoCallEndToEnd(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot := startBot(t, net, "bot", "2-bot", "", nil)
	var pcBox atomic.Pointer[webrtc.PeerConnection]
	user, probe, tprobe, _ := startUserProbeOn(t, net, "user", "1-user", func(c *rtc.RTCClientConfiguration) {
		c.NewPeerConnection = tapPCFactory(&pcBox)
	})
	botId, userId := pairUp(t, bot, user)
	pbx := newFakePBX(t, 200, true)
	pbx.enableVideo(t, false) // the muxed video answer

	dc := probe.waitDC(t, botId, msg_handler.DataChannelLabelMessages)
	chat := func(text string) ss.MsgId {
		t.Helper()
		m := baseMsg(ss.WellKnownChIdMain, userId, botId)
		m["plaintext"] = text
		if err := dc.SendText(mustJSON(t, m)); err != nil {
			t.Fatalf("SendText: %v", err)
		}
		return ss.MsgId(m["msgId"].(string))
	}
	sip := func(body map[string]any) {
		t.Helper()
		m := baseMsg(ss.WellKnownChIdMain, userId, botId)
		m["mimeType"] = wireMimeSip
		m["sip"] = body
		if err := dc.SendText(mustJSON(t, m)); err != nil {
			t.Fatalf("SendText: %v", err)
		}
	}

	registerMsg := chat("/register 2001@" + pbx.addr + " passW_0rd")
	waitBotMessage(t, probe, botId, "the registered reply", isChatReply(registerMsg, "Registered as"))

	callMsg := chat("/video-call 1001@" + pbx.addr)
	waitBotMessage(t, probe, botId, "the calling line", isChatReply(callMsg, "Calling 1001@"))
	pbx.waitForPBX(t, "the callee's INVITE", func() bool { return pbx.invites == 1 })

	// The offer carries audio AND video on ONE port — the muxed media.
	pbx.mu.Lock()
	offer := pbx.inviteSDP
	pbx.mu.Unlock()
	if strings.Count(offer, "m=video") != 1 {
		t.Fatalf("the offer lacks the video m-line:\n%s", offer)
	}
	if audioPort, videoPort := parseSDPMediaAddr(t, []byte(offer)).Port, sdpVideoPort(t, offer); audioPort != videoPort {
		t.Fatalf("the offer's ports are (audio %d, video %d), want the one muxed port:\n%s", audioPort, videoPort, offer)
	}
	if !strings.Contains(offer, "a=rtpmap:102 VP8/90000") {
		t.Fatalf("the offer lacks the VP8 rtpmap line:\n%s", offer)
	}

	dialog := pbx.waitDialog(t)
	invite := waitBotMessage(t, probe, botId, "the bot's video INVITE", func(m *rawMsg) bool {
		return isSipInvite(m) && m.sip.XMedia == "video" && m.sip.XCallStatus == "inviting"
	})
	callId, inviteMsgId := invite.sip.CallId, invite.msgId
	waitBotMessage(t, probe, botId, "the callee-answered line", isChatReply(callMsg, "Callee answered."))
	// The callee took the video: no degrades-to-voice note.
	if n := countBotMessages(probe, botId, isChatReply(callMsg, "Callee has no video")); n != 0 {
		t.Fatalf("the answered video call still said %d degrades-to-voice note(s)", n)
	}

	// The webrtc-leg's settled offer is pinned to the negotiated video
	// codec — the video m-line carries exactly the engine's VP8.
	pinned := waitVideoOfferSettled(t, &pcBox)
	if got := sdpVideoFormats(t, pinned.SDP); !slices.Equal(got, []string{"96"}) {
		t.Fatalf("the offer's video pin is %v, want [96]:\n%s", got, pinned.SDP)
	}
	if got := sdpAudioFormats(t, pinned.SDP); !slices.Equal(got, []string{"111", "126"}) {
		t.Fatalf("the offer's audio pin is %v, want [111 126]:\n%s", got, pinned.SDP)
	}

	// The user picks up with mic and camera; the callee's voice and video
	// start streaming.
	spoken, filmed := acceptVideoCall(t, user, botId, sip, callId)
	streamCtx, stopStream := context.WithCancel(context.Background())
	t.Cleanup(stopStream)
	calleeVoice, calleeVideo := opusVoice(6), videoFrames(6)
	go pbx.streamOpus(streamCtx, calleeVoice)
	go pbx.streamVideo(streamCtx, calleeVideo)

	// The callee's media crosses to the browser byte for byte, audio and
	// video alike (the tracks identified by kind — their firing order is
	// the streams' own).
	audioTrack := tprobe.waitTrackKind(t, webrtc.RTPCodecTypeAudio)
	videoTrack := tprobe.waitTrackKind(t, webrtc.RTPCodecTypeVideo)
	if got := videoTrack.Codec(); !strings.EqualFold(got.MimeType, webrtc.MimeTypeVP8) {
		t.Fatalf("the bot's video track toward the browser is %s, want VP8", got.MimeType)
	}
	waitBotMessage(t, probe, botId, "the accepted amend", isCallStatusAmend(inviteMsgId, callId, "accepted"))
	hearAll(t, audioTrack, calleeVoice)
	hearAll(t, videoTrack, calleeVideo)

	// The user's mic and camera cross to the callee — the camera's
	// packets at the negotiated video PT on the MUXED socket (the answer's
	// video port was the audio's).
	pbx.waitRTP(t, 1)
	pbx.waitRecordedPT(t, pbxVideoPT, filmed)
	heardMic := map[string]bool{}
	deadline := time.Now().Add(testTimeout)
	for len(heardMic) < len(spoken) {
		for _, got := range pbx.waitRTP(t, 1) {
			heardMic[string(got)] = true
		}
		if time.Now().After(deadline) {
			t.Fatalf("the callee heard %d of the %d distinct mic packets", len(heardMic), len(spoken))
		}
	}

	// The callee hangs up: the browser leg is ended by the bot.
	if err := dialog.Bye(context.Background()); err != nil {
		t.Fatalf("the callee's BYE: %v", err)
	}
	waitBotMessage(t, probe, botId, "the bot's BYE", isSipRequest(callId, "BYE"))
	waitBotMessage(t, probe, botId, "the callee-hung-up line", isChatReply(callMsg, "Callee hung up."))
	waitBotMessage(t, probe, botId, "the ended amend", isCallStatusAmend(inviteMsgId, callId, "ended"))
}

// TestSipBotVideoCallDemuxedAnswer covers the answer that demuxes the
// video onto a port of its own (Asterisk's shape): the bot's camera
// stream goes THERE (a dedicated socket, the answer respected), never to
// the audio port — while the callee's video still arrives at the bot's
// one muxed port and crosses to the browser.
func TestSipBotVideoCallDemuxedAnswer(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot := startBot(t, net, "bot", "2-bot", "", nil)
	user, probe, tprobe, _ := startUserProbe(t, net, "user", "1-user")
	botId, userId := pairUp(t, bot, user)
	pbx := newFakePBX(t, 200, true)
	pbx.enableVideo(t, true) // the demuxed video answer

	dc := probe.waitDC(t, botId, msg_handler.DataChannelLabelMessages)
	chat := func(text string) ss.MsgId {
		t.Helper()
		m := baseMsg(ss.WellKnownChIdMain, userId, botId)
		m["plaintext"] = text
		if err := dc.SendText(mustJSON(t, m)); err != nil {
			t.Fatalf("SendText: %v", err)
		}
		return ss.MsgId(m["msgId"].(string))
	}
	sip := func(body map[string]any) {
		t.Helper()
		m := baseMsg(ss.WellKnownChIdMain, userId, botId)
		m["mimeType"] = wireMimeSip
		m["sip"] = body
		if err := dc.SendText(mustJSON(t, m)); err != nil {
			t.Fatalf("SendText: %v", err)
		}
	}

	registerMsg := chat("/register 2001@" + pbx.addr + " passW_0rd")
	waitBotMessage(t, probe, botId, "the registered reply", isChatReply(registerMsg, "Registered as"))

	callMsg := chat("/video-call 1001@" + pbx.addr)
	waitBotMessage(t, probe, botId, "the calling line", isChatReply(callMsg, "Calling 1001@"))
	dialog := pbx.waitDialog(t)
	invite := waitBotMessage(t, probe, botId, "the bot's video INVITE", func(m *rawMsg) bool {
		return isSipInvite(m) && m.sip.XMedia == "video" && m.sip.XCallStatus == "inviting"
	})
	callId := invite.sip.CallId
	waitBotMessage(t, probe, botId, "the callee-answered line", isChatReply(callMsg, "Callee answered."))

	_, filmed := acceptVideoCall(t, user, botId, sip, callId)
	streamCtx, stopStream := context.WithCancel(context.Background())
	t.Cleanup(stopStream)
	calleeVideo := videoFrames(6)
	go pbx.streamVideo(streamCtx, calleeVideo)

	// The camera's packets land on the answer's OTHER port — all of them
	// at the video PT…
	pbx.waitVideoRTP(t, len(filmed))
	heard := map[string]bool{}
	pbx.mu.Lock()
	for i, p := range pbx.rtpVideoPayloads {
		if pbx.rtpVideoPTs[i] != pbxVideoPT {
			pbx.mu.Unlock()
			t.Fatalf("a packet at PT %d reached the demuxed video port, want %d", pbx.rtpVideoPTs[i], pbxVideoPT)
		}
		heard[string(p)] = true
	}
	pbx.mu.Unlock()
	for _, w := range filmed {
		if !heard[string(w)] {
			t.Fatalf("a camera packet never reached the demuxed video port")
		}
	}
	// …and none on the audio port (give a would-be muxed copy its chance).
	time.Sleep(300 * time.Millisecond)
	if got := pbx.rtpPayloadsByPT(pbxVideoPT); len(got) != 0 {
		t.Fatalf("%d camera packet(s) landed on the audio port, want none — the answer demuxed the video", len(got))
	}

	// The callee's video still arrives at the bot's one muxed port and
	// crosses to the browser byte for byte.
	videoTrack := tprobe.waitTrackKind(t, webrtc.RTPCodecTypeVideo)
	hearAll(t, videoTrack, calleeVideo)

	if err := dialog.Bye(context.Background()); err != nil {
		t.Fatalf("the callee's BYE: %v", err)
	}
	waitBotMessage(t, probe, botId, "the callee-hung-up line", isChatReply(callMsg, "Callee hung up."))
}

// TestSipBotVideoCallCalleeDeclinesVideo covers the answer without
// video: /video-call's offer carried it, the callee's answer dropped it —
// the browser rings as a VOICE call (the offer toward it carries no video
// m-line), the chat note says why, and the call completes voice-only.
func TestSipBotVideoCallCalleeDeclinesVideo(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot := startBot(t, net, "bot", "2-bot", "", nil)
	var pcBox atomic.Pointer[webrtc.PeerConnection]
	user, probe, tprobe, _ := startUserProbeOn(t, net, "user", "1-user", func(c *rtc.RTCClientConfiguration) {
		c.NewPeerConnection = tapPCFactory(&pcBox)
	})
	botId, userId := pairUp(t, bot, user)
	pbx := newFakePBX(t, 200, true) // no video in the answer

	dc := probe.waitDC(t, botId, msg_handler.DataChannelLabelMessages)
	chat := func(text string) ss.MsgId {
		t.Helper()
		m := baseMsg(ss.WellKnownChIdMain, userId, botId)
		m["plaintext"] = text
		if err := dc.SendText(mustJSON(t, m)); err != nil {
			t.Fatalf("SendText: %v", err)
		}
		return ss.MsgId(m["msgId"].(string))
	}
	sip := func(body map[string]any) {
		t.Helper()
		m := baseMsg(ss.WellKnownChIdMain, userId, botId)
		m["mimeType"] = wireMimeSip
		m["sip"] = body
		if err := dc.SendText(mustJSON(t, m)); err != nil {
			t.Fatalf("SendText: %v", err)
		}
	}

	registerMsg := chat("/register 2001@" + pbx.addr + " passW_0rd")
	waitBotMessage(t, probe, botId, "the registered reply", isChatReply(registerMsg, "Registered as"))

	callMsg := chat("/video-call 1001@" + pbx.addr)
	waitBotMessage(t, probe, botId, "the calling line", isChatReply(callMsg, "Calling 1001@"))
	pbx.waitForPBX(t, "the callee's INVITE", func() bool { return pbx.invites == 1 })
	pbx.mu.Lock()
	offer := pbx.inviteSDP
	pbx.mu.Unlock()
	if !strings.Contains(offer, "m=video") {
		t.Fatalf("the /video-call offer lacks the video m-line:\n%s", offer)
	}

	// The callee's audio-only answer degrades the call: the chat note, the
	// VOICE ring, the offer toward the browser carrying no video m-line.
	dialog := pbx.waitDialog(t)
	waitBotMessage(t, probe, botId, "the degrades-to-voice note", isChatReply(callMsg, "Callee has no video — voice call."))
	invite := waitBotMessage(t, probe, botId, "the bot's (voice) INVITE", func(m *rawMsg) bool {
		return isSipInvite(m) && m.sip.XMedia == "voice" && m.sip.XCallStatus == "inviting"
	})
	callId := invite.sip.CallId
	pinned := waitOfferSettled(t, &pcBox)
	if strings.Contains(pinned.SDP, "m=video") {
		t.Fatalf("the degraded call's offer toward the browser carries video:\n%s", pinned.SDP)
	}

	// The voice call completes: the mic crosses to the callee, the
	// callee's voice to the browser.
	micTrack, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "mic", "probe")
	if err != nil {
		t.Fatalf("the mic track: %v", err)
	}
	if _, err := user.AddTrack(botId, micTrack); err != nil {
		t.Fatalf("attach the mic: %v", err)
	}
	spoken := opusVoice(4)
	micCtx, stopMic := context.WithCancel(context.Background())
	t.Cleanup(stopMic)
	go streamSamples(micCtx, micTrack, spoken, 20*time.Millisecond, 20*time.Millisecond)
	sip(map[string]any{"callId": callId, "response": map[string]any{"code": 200, "phrase": "OK"}})
	streamCtx, stopStream := context.WithCancel(context.Background())
	t.Cleanup(stopStream)
	voice := opusVoice(4)
	go pbx.streamOpus(streamCtx, voice)
	hearAll(t, tprobe.waitTrackKind(t, webrtc.RTPCodecTypeAudio), voice)
	heard := map[string]bool{}
	deadline := time.Now().Add(testTimeout)
	for len(heard) < len(spoken) {
		for _, got := range pbx.waitRTP(t, 1) {
			heard[string(got)] = true
		}
		if time.Now().After(deadline) {
			t.Fatalf("the callee heard %d of the %d distinct mic packets", len(heard), len(spoken))
		}
	}

	if err := dialog.Bye(context.Background()); err != nil {
		t.Fatalf("the callee's BYE: %v", err)
	}
	waitBotMessage(t, probe, botId, "the callee-hung-up line", isChatReply(callMsg, "Callee hung up."))
}

// TestSipBotInboundVideoCallEndToEnd covers the inbound video call: the
// caller's offer carries audio AND video; the browser rings as a video
// call; the 200 OK's answer carries the negotiated audio plus the video
// section at the OFFER's payload type on the ONE muxed port (RFC 3264's
// echo); media crosses both ways, the caller's camera to the browser and
// the browser's to the caller; and the caller's BYE ends both legs.
func TestSipBotInboundVideoCallEndToEnd(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot := startBot(t, net, "bot", "2-bot", "", nil)
	var pcBox atomic.Pointer[webrtc.PeerConnection]
	user, probe, tprobe, _ := startUserProbeOn(t, net, "user", "1-user", func(c *rtc.RTCClientConfiguration) {
		c.NewPeerConnection = tapPCFactory(&pcBox)
	})
	botId, userId := pairUp(t, bot, user)
	pbx := newFakePBX(t, 200, true)
	pbx.enableVideo(t, false) // the fake's own offer carries muxed video

	dc := probe.waitDC(t, botId, msg_handler.DataChannelLabelMessages)
	chat := func(text string) ss.MsgId {
		t.Helper()
		m := baseMsg(ss.WellKnownChIdMain, userId, botId)
		m["plaintext"] = text
		if err := dc.SendText(mustJSON(t, m)); err != nil {
			t.Fatalf("SendText: %v", err)
		}
		return ss.MsgId(m["msgId"].(string))
	}
	sip := func(body map[string]any) {
		t.Helper()
		m := baseMsg(ss.WellKnownChIdMain, userId, botId)
		m["mimeType"] = wireMimeSip
		m["sip"] = body
		if err := dc.SendText(mustJSON(t, m)); err != nil {
			t.Fatalf("SendText: %v", err)
		}
	}

	registerMsg := chat("/register 2001@" + pbx.addr + " passW_0rd")
	waitBotMessage(t, probe, botId, "the registered reply", isChatReply(registerMsg, "Registered as"))

	// Someone on the SIP network video-calls the registered number.
	call := pbx.call(t, "2001")
	invite := waitBotMessage(t, probe, botId, "the bot's video INVITE", func(m *rawMsg) bool {
		return isSipInvite(m) && m.sip.XMedia == "video" && m.sip.XCallStatus == "inviting"
	})
	callId, inviteMsgId := invite.sip.CallId, invite.msgId
	waitBotMessage(t, probe, botId, "the incoming-call line", func(m *rawMsg) bool {
		return m.mimeType == wireMimePlaintext && strings.Contains(m.plaintext, "Incoming call from caller@")
	})
	call.waitRinging()

	// The ring's settled offer is pinned to the offered video codec.
	pinned := waitVideoOfferSettled(t, &pcBox)
	if got := sdpVideoFormats(t, pinned.SDP); !slices.Equal(got, []string{"96"}) {
		t.Fatalf("the ring's video pin is %v, want [96]:\n%s", got, pinned.SDP)
	}

	// The user picks up with mic and camera; the caller's 200 OK carries
	// the answer: the audio pin and the video section at the offer's PT
	// on the ONE muxed port.
	spoken, filmed := acceptVideoCall(t, user, botId, sip, callId)
	call.waitAnswered()
	call.mu.Lock()
	answer := call.answerSDP
	call.mu.Unlock()
	if got := sdpAudioFormats(t, answer); !slices.Equal(got, []string{"96", "101"}) {
		t.Fatalf("the answer's audio formats are %v, want [96 101]:\n%s", got, answer)
	}
	if got := sdpVideoFormats(t, answer); !slices.Equal(got, []string{"102"}) {
		t.Fatalf("the answer's video formats are %v, want the offer's PT echoed [102]:\n%s", got, answer)
	}
	if audioPort, videoPort := parseSDPMediaAddr(t, []byte(answer)).Port, sdpVideoPort(t, answer); audioPort != videoPort {
		t.Fatalf("the answer's ports are (audio %d, video %d), want the one muxed port:\n%s", audioPort, videoPort, answer)
	}
	if !strings.Contains(answer, "a=rtpmap:102 VP8/90000") {
		t.Fatalf("the answer lacks the VP8 rtpmap line:\n%s", answer)
	}

	// Media crosses byte for byte in both directions, audio and video
	// alike.
	streamCtx, stopStream := context.WithCancel(context.Background())
	t.Cleanup(stopStream)
	callerVoice, callerVideo := opusVoice(6), videoFrames(6)
	go call.stream(streamCtx, callerVoice)
	go call.streamVideo(streamCtx, callerVideo)
	audioTrack := tprobe.waitTrackKind(t, webrtc.RTPCodecTypeAudio)
	videoTrack := tprobe.waitTrackKind(t, webrtc.RTPCodecTypeVideo)
	waitBotMessage(t, probe, botId, "the accepted amend", isCallStatusAmend(inviteMsgId, callId, "accepted"))
	hearAll(t, audioTrack, callerVoice)
	hearAll(t, videoTrack, callerVideo)
	pbx.waitRecordedPT(t, pbxVideoPT, filmed)
	heardMic := map[string]bool{}
	deadline := time.Now().Add(testTimeout)
	for len(heardMic) < len(spoken) {
		for _, got := range pbx.waitRTP(t, 1) {
			heardMic[string(got)] = true
		}
		if time.Now().After(deadline) {
			t.Fatalf("the caller heard %d of the %d distinct mic packets", len(heardMic), len(spoken))
		}
	}

	// The caller hangs up: the browser leg is ended by the bot.
	call.bye()
	waitBotMessage(t, probe, botId, "the bot's BYE", isSipRequest(callId, "BYE"))
	waitBotMessage(t, probe, botId, "the caller-hung-up line", func(m *rawMsg) bool {
		return m.mimeType == wireMimePlaintext && strings.Contains(m.plaintext, "Caller hung up.")
	})
	waitBotMessage(t, probe, botId, "the ended amend", isCallStatusAmend(inviteMsgId, callId, "ended"))
}

// TestSipBotInboundVideoNoCommonVideoCodec covers the video offer
// sharing nothing with the presumed video list: the answer still carries
// a video m-line — the RFC 3264 parity — as the port-0 rejection naming
// the offered formats, the browser rings as a VOICE call, and the call
// completes voice-only.
func TestSipBotInboundVideoNoCommonVideoCodec(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot := startBot(t, net, "bot", "2-bot", "", nil)
	user, probe, tprobe, _ := startUserProbe(t, net, "user", "1-user")
	botId, userId := pairUp(t, bot, user)
	pbx := newFakePBX(t, 200, true)

	dc := probe.waitDC(t, botId, msg_handler.DataChannelLabelMessages)
	chat := func(text string) ss.MsgId {
		t.Helper()
		m := baseMsg(ss.WellKnownChIdMain, userId, botId)
		m["plaintext"] = text
		if err := dc.SendText(mustJSON(t, m)); err != nil {
			t.Fatalf("SendText: %v", err)
		}
		return ss.MsgId(m["msgId"].(string))
	}
	sip := func(body map[string]any) {
		t.Helper()
		m := baseMsg(ss.WellKnownChIdMain, userId, botId)
		m["mimeType"] = wireMimeSip
		m["sip"] = body
		if err := dc.SendText(mustJSON(t, m)); err != nil {
			t.Fatalf("SendText: %v", err)
		}
	}

	registerMsg := chat("/register 2001@" + pbx.addr + " passW_0rd")
	waitBotMessage(t, probe, botId, "the registered reply", isChatReply(registerMsg, "Registered as"))

	// The caller offers video in a codec the presumed list does not name
	// (MP4V-ES alone).
	offer := fmt.Sprintf("v=0\r\no=- 0 0 IN IP4 %s\r\ns=fakepbx\r\nc=IN IP4 %s\r\nt=0 0\r\n"+
		"m=audio %d RTP/AVP 96 101\r\na=rtpmap:96 opus/48000/2\r\na=rtpmap:101 telephone-event/8000\r\n"+
		"m=video %d RTP/AVP 31\r\na=rtpmap:31 MP4V-ES/90000\r\n", pbx.ip, pbx.ip, pbx.rtpPort(), pbx.rtpPort())
	call := pbx.callSDP(t, "2001", []byte(offer))

	// The browser rings as a voice call; the caller hears the 180.
	invite := waitBotMessage(t, probe, botId, "the bot's (voice) INVITE", func(m *rawMsg) bool {
		return isSipInvite(m) && m.sip.XMedia == "voice" && m.sip.XCallStatus == "inviting"
	})
	callId := invite.sip.CallId
	call.waitRinging()

	// The accept's answer keeps the m-line parity: the video section
	// rejected (port 0), the offered formats named.
	micTrack, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "mic", "probe")
	if err != nil {
		t.Fatalf("the mic track: %v", err)
	}
	if _, err := user.AddTrack(botId, micTrack); err != nil {
		t.Fatalf("attach the mic: %v", err)
	}
	spoken := opusVoice(4)
	micCtx, stopMic := context.WithCancel(context.Background())
	t.Cleanup(stopMic)
	go streamSamples(micCtx, micTrack, spoken, 20*time.Millisecond, 20*time.Millisecond)
	sip(map[string]any{"callId": callId, "response": map[string]any{"code": 200, "phrase": "OK"}})
	call.waitAnswered()
	call.mu.Lock()
	answer := call.answerSDP
	call.mu.Unlock()
	if got := sdpVideoPort(t, answer); got != 0 {
		t.Fatalf("the answer's video port is %d, want 0 (the rejection):\n%s", got, answer)
	}
	if got := sdpVideoFormats(t, answer); !slices.Equal(got, []string{"31"}) {
		t.Fatalf("the answer's video formats are %v, want the offered [31]:\n%s", got, answer)
	}
	if got := sdpAudioFormats(t, answer); !slices.Equal(got, []string{"96", "101"}) {
		t.Fatalf("the answer's audio formats are %v, want [96 101]:\n%s", got, answer)
	}

	// The voice call completes: the caller's voice crosses to the browser,
	// the mic's to the caller.
	streamCtx, stopStream := context.WithCancel(context.Background())
	t.Cleanup(stopStream)
	voice := opusVoice(4)
	go call.stream(streamCtx, voice)
	hearAll(t, tprobe.waitTrackKind(t, webrtc.RTPCodecTypeAudio), voice)
	heard := map[string]bool{}
	deadline := time.Now().Add(testTimeout)
	for len(heard) < len(spoken) {
		for _, got := range pbx.waitRTP(t, 1) {
			heard[string(got)] = true
		}
		if time.Now().After(deadline) {
			t.Fatalf("the caller heard %d of the %d distinct mic packets", len(heard), len(spoken))
		}
	}

	call.bye()
	waitBotMessage(t, probe, botId, "the caller-hung-up line", func(m *rawMsg) bool {
		return m.mimeType == wireMimePlaintext && strings.Contains(m.plaintext, "Caller hung up.")
	})
}
