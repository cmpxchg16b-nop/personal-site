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
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/google/uuid"
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

// startUserProbe Runs a plain client with a wireProbe on the messaging
// label and a trackProbe on its media. The returned cancel stops the
// client mid-test (a subscriber dropout); the tests that need it take
// it, the rest ignore it (cleanup cancels anyway).
func startUserProbe(t *testing.T, net *clientTestNet, name string, id ss.SubscriberId) (*rtc.HeadlessRTCClient, *wireProbe, *trackProbe, context.CancelFunc) {
	t.Helper()
	user, stop := startClient(t, net, name, func(c *rtc.RTCClientConfiguration) {
		c.SubscriberId = id
	})
	probe := newWireProbe()
	probe.register(t, user)
	tprobe := newTrackProbe()
	tprobe.register(t, user)
	return user, probe, tprobe, stop
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
// Returns the matched message.
func waitBotMessage(t *testing.T, probe *wireProbe, peer ss.SubscriberId, what string, pred func(*rawMsg) bool) *rawMsg {
	t.Helper()
	var found *rawMsg
	waitFor(t, what, func() bool {
		for _, m := range botMessages(probe.textsFrom(peer)) {
			if pred(m) {
				found = m
				return true
			}
		}
		return false
	})
	return found
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
// REGISTER and INVITE, a callee that answers with a hand-written SDP,
// and one UDP socket streaming and recording real RTP. Everything it
// receives is recorded for the test's assertions.
type fakePBX struct {
	addr    string // its SIP address, host:port (an IPv6 one is bracketed)
	ip      string // its bare IP (the SDP connection address)
	sipHost string // its IP in SIP-URI form (an IPv6 literal bracketed)
	v6      bool   // the IPv6 loopback variant

	answerCode int  // the final response to an INVITE (200, 486, …)
	opus       bool // the SDP answer's codec: opus (96) when set, PCMU (0) otherwise

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
	byes          int
	rtpReceived   int
	rtpPayloads   [][]byte

	dialogs    chan *sipgo.DialogServerSession // established dialogs (after ACK)
	rtpStart   chan struct{}                   // closed when the first dialog's media is up
	rtpStartDo sync.Once
}

// registration is one authenticated REGISTER's identity as the fake saw
// it on the wire.
type registration struct {
	from    string // the From URI's user
	contact string // the Contact URI's user
	source  string // the datagram's source address (the account's socket)
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

	srv.OnRegister(func(req *sip.Request, tx sip.ServerTransaction) {
		if !f.authenticated(req) {
			f.challenge(req, tx)
			return
		}
		f.mu.Lock()
		refused := f.refuseRegisterUser != "" && f.refuseRegisterUser == f.authedUser
		f.mu.Unlock()
		if refused {
			if err := tx.Respond(sip.NewResponseFromRequest(req, 403, "Forbidden", nil)); err != nil {
				t.Errorf("REGISTER 403: %v", err)
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
			f.mu.Lock()
			f.registers++
			f.registrations = append(f.registrations, reg)
			f.mu.Unlock()
		}
		if err := tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil)); err != nil {
			t.Errorf("REGISTER 200: %v", err)
		}
	})
	srv.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
		if !f.authenticated(req) {
			f.challenge(req, tx)
			return
		}
		f.mu.Lock()
		f.invites++
		f.inviteFrom = req.From().Address.User
		f.inviteTo = req.To().Address.User
		f.mu.Unlock()
		peer := parseSDPMediaAddr(t, req.Body())
		f.mu.Lock()
		f.rtpPeer = peer
		f.mu.Unlock()
		dialog, err := dialogs.ReadInvite(req, tx)
		if err != nil {
			t.Errorf("ReadInvite: %v", err)
			_ = tx.Respond(sip.NewResponseFromRequest(req, 500, "Server Error", nil))
			return
		}
		if err := dialog.Respond(180, "Ringing", nil); err != nil {
			t.Errorf("180: %v", err)
		}
		if f.answerCode != 200 {
			if err := dialog.Respond(f.answerCode, "Busy Here", nil); err != nil {
				t.Errorf("final response: %v", err)
			}
			return
		}
		if err := dialog.RespondSDP(f.sdpAnswer()); err != nil {
			t.Errorf("RespondSDP: %v", err)
			return
		}
		f.dialogs <- dialog
	})
	srv.OnAck(func(req *sip.Request, tx sip.ServerTransaction) {
		if err := dialogs.ReadAck(req, tx); err != nil {
			t.Logf("ReadAck: %v", err)
		}
		f.rtpStartDo.Do(func() { close(f.rtpStart) })
	})
	srv.OnBye(func(req *sip.Request, tx sip.ServerTransaction) {
		f.mu.Lock()
		f.byes++
		f.mu.Unlock()
		if err := dialogs.ReadBye(req, tx); err != nil {
			_ = tx.Respond(sip.NewResponseFromRequest(req, 481, "Call/Transaction Does Not Exist", nil))
		}
	})
	srv.OnCancel(func(req *sip.Request, tx sip.ServerTransaction) {
		// The transaction layer terminates the INVITE transaction; a
		// 200 to the CANCEL is all the handler owes.
		_ = tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		if err := srv.ListenAndServe(ctx, "udp", f.addr); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("the fake PBX stopped: %v", err)
		}
	}()
	t.Cleanup(func() { _ = srv.Close() })

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
			f.mu.Unlock()
		}
	}()
	return f
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
func (f *fakePBX) challenge(req *sip.Request, tx sip.ServerTransaction) {
	res := sip.NewResponseFromRequest(req, 401, "Unauthorized", nil)
	res.AppendHeader(sip.NewHeader("WWW-Authenticate", `Digest realm="pbx", nonce="0123456789abcdef", algorithm=MD5`))
	if err := tx.Respond(res); err != nil {
		panic(err)
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

// sdpAnswer is the callee's media answer: the fake's media socket, the
// negotiated codec alone (plus telephone-event, as PBXs do).
func (f *fakePBX) sdpAnswer() []byte {
	port := f.rtp.LocalAddr().(*net.UDPAddr).Port
	family := "IP4"
	if f.v6 {
		family = "IP6"
	}
	sdp := fmt.Sprintf("v=0\r\no=- 0 0 IN %s %s\r\ns=fakepbx\r\nc=IN %s %s\r\nt=0 0\r\n", family, f.ip, family, f.ip)
	if f.opus {
		return []byte(sdp + fmt.Sprintf("m=audio %d RTP/AVP 96 101\r\na=rtpmap:96 opus/48000/2\r\na=rtpmap:101 telephone-event/8000\r\n", port))
	}
	return []byte(sdp + fmt.Sprintf("m=audio %d RTP/AVP 0 101\r\na=rtpmap:0 PCMU/8000\r\na=rtpmap:101 telephone-event/8000\r\n", port))
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

// streamOpus sends 20 ms opus RTP packets to the bot's media address —
// the callee's voice — cycling the given payloads until ctx ends (the
// relay drops them while the browser leg is still being set up, so the
// stream simply runs on). It starts once the first dialog's media is up.
func (f *fakePBX) streamOpus(ctx context.Context, payloads [][]byte) {
	<-f.rtpStart
	f.mu.Lock()
	peer := f.rtpPeer
	f.mu.Unlock()
	if peer == nil {
		return
	}
	for i := 0; ; i++ {
		pkt := &rtp.Packet{
			Header:  rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: uint16(i), Timestamp: uint32(i * 960), SSRC: 0xC0FFEE},
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
	testCallMsg := send("/test-call")
	hangupMsg := send("/hangup")

	help := waitBotMessage(t, probe, botId, "the /help reply", isChatReply(helpMsg, "/help"))
	for _, cmd := range []string{"/help", "/register", "/unregister", "/call", "/test-call", "/hangup"} {
		if !strings.Contains(help.plaintext, cmd) {
			t.Fatalf("the help text lacks %q: %q", cmd, help.plaintext)
		}
	}
	waitBotMessage(t, probe, botId, "the unknown-command hint", isChatReply(unknownMsg, "/help"))
	waitBotMessage(t, probe, botId, "the register usage", isChatReply(registerMsg, "Usage: /register"))
	waitBotMessage(t, probe, botId, "the not-registered answer", isChatReply(callMsg, "Not registered"))
	waitBotMessage(t, probe, botId, "the test-call unavailable answer", isChatReply(testCallMsg, "unavailable"))
	waitBotMessage(t, probe, botId, "the no-call answer", isChatReply(hangupMsg, "No call in progress"))

	// No call came of the guarded commands: not one INVITE the whole test.
	time.Sleep(300 * time.Millisecond)
	if n := countBotMessages(probe, botId, isSipInvite); n != 0 {
		t.Fatalf("the guarded commands produced %d INVITEs", n)
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
	// is the AOR and the Contact's user part is the username.
	pbx.waitForPBX(t, "the REGISTER's From and Contact", func() bool {
		return len(pbx.registrations) == 1 &&
			pbx.registrations[0].from == "2001" && pbx.registrations[0].contact == "2001"
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
// the bot phones the browser on the webrtc-leg and the callee on the
// sip-leg — both legs answer, opus media crosses untouched in both
// directions (the passthrough), and the callee's BYE ends the call on
// the browser's side too (BYE, the chat line, the ended amend).
func TestSipBotCallEndToEnd(t *testing.T) {
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

	// The call: the bot phones the user…
	callMsg := chat("/call 1001@" + pbx.addr)
	invite := waitBotMessage(t, probe, botId, "the bot's INVITE", func(m *rawMsg) bool {
		return isSipInvite(m) && m.sip.XMedia == "voice" && m.sip.XCallStatus == "inviting"
	})
	callId, inviteMsgId := invite.sip.CallId, invite.msgId
	waitBotMessage(t, probe, botId, "the calling line", isChatReply(callMsg, "Calling 1001@"))

	// …and the callee, with the user's own identity, digest and all.
	pbx.waitForPBX(t, "the callee's INVITE", func() bool { return pbx.invites == 1 })
	pbx.waitForPBX(t, "the INVITE's From/To", func() bool { return pbx.inviteFrom == "2001" && pbx.inviteTo == "1001" })
	pbx.waitForPBX(t, "the INVITE's digest username", func() bool { return pbx.authedUser == "2001" })
	dialog := pbx.waitDialog(t)

	// The user picks up. The accept's shape mirrors the browser's: the
	// mic goes on the connection FIRST — a fresh m-line on the quiet
	// connection, so nothing collides with it — and the 200 OK then
	// flies. (The reverse order recycles the bot's sendrecv transceiver,
	// and pion re-binds the recycled m-line's inbound RTP without firing
	// OnTrack — the bot would never learn the mic exists.)
	micTrack, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "mic", "probe")
	if err != nil {
		t.Fatalf("the mic track: %v", err)
	}
	if _, err := user.AddTrack(botId, micTrack); err != nil {
		t.Fatalf("attach the mic: %v", err)
	}
	// The mic speaks from here on, one packet per 20 ms (samples written
	// before it binds drop — the "empty room").
	spoken := make([][]byte, 5)
	for i := range spoken {
		p := make([]byte, 30)
		p[0] = 0xF8
		for j := 1; j < len(p); j++ {
			p[j] = byte(i*53 + j*17)
		}
		spoken[i] = p
	}
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
	// Let the mic's offer/answer settle (in-process, a couple of ms)
	// before the 200 OK triggers the bot's own attach offer, so the two
	// renegotiations never meet in flight.
	time.Sleep(300 * time.Millisecond)
	sip(map[string]any{"callId": callId, "response": map[string]any{"code": 200, "phrase": "OK"}})

	// The bot's opus track attaches. The callee's voice starts streaming
	// — the probe's OnTrack fires only once media actually arrives, so
	// the stream runs ahead of the track's wait.
	streamCtx, stopStream := context.WithCancel(context.Background())
	t.Cleanup(stopStream)
	voice := opusVoice(5)
	go pbx.streamOpus(streamCtx, voice)
	track := tprobe.waitTrack(t, 1)
	waitBotMessage(t, probe, botId, "the callee-answered line", isChatReply(callMsg, "Callee answered."))
	waitBotMessage(t, probe, botId, "the accepted amend", isCallStatusAmend(inviteMsgId, callId, "accepted"))

	// The callee speaks: the opus packets cross the SBC byte for byte.
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

	// The user speaks: the mic's opus packets cross byte for byte too.
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

	// The callee hangs up: the browser leg is ended by the bot (BYE, the
	// chat line, the ended amend) — signalling termination both ways.
	if err := dialog.Bye(context.Background()); err != nil {
		t.Fatalf("the callee's BYE: %v", err)
	}
	waitBotMessage(t, probe, botId, "the bot's BYE", isSipRequest(callId, "BYE"))
	waitBotMessage(t, probe, botId, "the callee-hung-up line", isChatReply(callMsg, "Callee hung up."))
	waitBotMessage(t, probe, botId, "the ended amend", isCallStatusAmend(inviteMsgId, callId, "ended"))
}

// TestSipBotCallBusy covers the callee's refusal while the browser leg
// still rings: the dial's failure terminates the webrtc-leg with a
// CANCEL, the chat line says why, the log settles cancelled.
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
	invite := waitBotMessage(t, probe, botId, "the bot's INVITE", isSipInvite)
	callId, inviteMsgId := invite.sip.CallId, invite.msgId

	// The callee is busy; the still-ringing browser leg is cancelled.
	waitBotMessage(t, probe, botId, "the CANCEL", isSipRequest(callId, "CANCEL"))
	waitBotMessage(t, probe, botId, "the call-failed line", isChatReply(callMsg, "Call failed: "))
	waitBotMessage(t, probe, botId, "the cancelled amend", isCallStatusAmend(inviteMsgId, callId, "cancelled"))
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
	invite := waitBotMessage(t, probe, botId, "the bot's INVITE", isSipInvite)
	callId := invite.sip.CallId
	pbx.waitDialog(t)
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
	invite := waitBotMessage(t, probe, botId, "the bot's INVITE", isSipInvite)
	callId := invite.sip.CallId
	waitBotMessage(t, probe, botId, "the calling line", isChatReply(testCallMsg, "Calling 9664@"))
	pbx.waitForPBX(t, "the test callee's INVITE", func() bool { return pbx.invites == 1 && pbx.inviteTo == "9664" })
	pbx.waitDialog(t)

	// The user declines; the answered callee gets the BYE.
	sip(map[string]any{"callId": callId, "response": map[string]any{"code": 603, "phrase": "Decline"}})
	waitBotMessage(t, probe, botId, "the declined line", isChatReply(testCallMsg, "Call declined."))
	pbx.waitForPBX(t, "the callee's BYE", func() bool { return pbx.byes == 1 })
}

// TestSipBotPoolCall covers the pool's happy path: a user with no
// /register /calls — the bot loans an account from the pool, registers
// it (the REGISTER carries the pooled identity, digest and all), says
// so, and dials the callee with it; /unregister returns the loan (the
// de-REGISTER goes out), and the pool hands the same account out again
// to the next /call.
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

	// The /call loans the pool's account, registers it, says so, dials.
	callMsg := chat("/call 1001@" + pbx.addr)
	waitBotMessage(t, probe, botId, "the pooled registration line", isChatReply(callMsg, "Registered as sip:1101@"+pbx.addr+" (an account from the bot's pool)"))
	waitBotMessage(t, probe, botId, "the calling line", isChatReply(callMsg, "Calling 1001@"))
	pbx.waitForPBX(t, "the pooled account's REGISTER, its own socket and identity", func() bool {
		return len(pbx.registrations) == 1 &&
			pbx.registrations[0].from == "1101" && pbx.registrations[0].contact == "1101"
	})
	pbx.waitForPBX(t, "the callee's INVITE with the pooled identity", func() bool {
		return pbx.invites == 1 && pbx.inviteFrom == "1101" && pbx.inviteTo == "1001"
	})
	pbx.waitForPBX(t, "the pooled digest username", func() bool { return pbx.authedUser == "1101" })

	// /unregister returns the loan (the de-REGISTER goes out)…
	unregisterMsg := chat("/unregister")
	waitBotMessage(t, probe, botId, "the unregistered reply", isChatReply(unregisterMsg, "Unregistered."))
	pbx.waitForPBX(t, "the pooled account's de-REGISTER", func() bool { return pbx.unregistered })

	// …and the pool hands the same account out again to the next /call.
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
	bob, bobProbe, _, _ := startUserProbe(t, net, "bob", "1-bob")
	botId, aliceId := pairUp(t, bot, alice)
	_, bobId := pairUp(t, bot, bob)

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

	// Alice loans the pool's only account (a range's single member).
	aliceCall := chat(aliceProbe, aliceId, "/call 1001@"+pbx.addr)
	waitBotMessage(t, aliceProbe, botId, "alice's pooled registration line", isChatReply(aliceCall, "Registered as sip:1101@"))

	// Bob's /call finds the pool empty and says so, pointing at /register;
	// his own /register is unaffected.
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

// TestSipBotPoolSessionEndReleases covers the lifecycle hook: the user's
// chat session ends (the subscriber drops out and ages out), and the
// bot's session-end teardown runs — the pooled loan's de-REGISTER goes
// out and the account returns to the pool, to serve the next user's
// /call. A shorter subscriber aging makes the dropout observable.
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

	aliceCall := chat(aliceProbe, aliceId, "/call 1001@"+pbx.addr)
	waitBotMessage(t, aliceProbe, botId, "alice's pooled registration line", isChatReply(aliceCall, "Registered as sip:1101@"))
	pbx.waitForPBX(t, "the pooled account's REGISTER", func() bool { return pbx.registers == 1 })

	// Alice drops out: her registration ages out, the bot's session with
	// her ends, and the hook returns the loan — the de-REGISTER goes out.
	aliceStop()
	pbx.waitForPBX(t, "the pooled account's de-REGISTER on the session's end", func() bool { return pbx.unregistered })

	// Bob's /call loans the returned account — a pool that kept the loan
	// would answer "empty".
	bob, bobProbe, _, _ := startUserProbe(t, net, "bob", "1-bob")
	_, bobId := pairUp(t, bot, bob)
	bobCall := chat(bobProbe, bobId, "/call 1002@"+pbx.addr)
	waitBotMessage(t, bobProbe, botId, "bob's pooled registration line", isChatReply(bobCall, "Registered as sip:1101@"))
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

	// The pooled account registers against the IPv6 registrar and dials.
	callMsg := chat("/call 1001@" + pbx.addr)
	waitBotMessage(t, probe, botId, "the pooled registration line", isChatReply(callMsg, "Registered as sip:1101@"+pbx.addr))
	pbx.waitForPBX(t, "the pooled account's REGISTER over IPv6", func() bool {
		return len(pbx.registrations) == 1 && pbx.registrations[0].from == "1101"
	})
	pbx.waitForPBX(t, "the INVITE over IPv6", func() bool {
		return pbx.invites == 1 && pbx.inviteFrom == "1101" && pbx.inviteTo == "1001"
	})

	// The manual path takes the same family-aware socket.
	registerMsg := chat("/register 2001@" + pbx.addr + " s3cret")
	waitBotMessage(t, probe, botId, "the manual registered reply over IPv6", isChatReply(registerMsg, "Registered as sip:2001@"+pbx.addr))
	pbx.waitForPBX(t, "the manual REGISTER over IPv6", func() bool { return pbx.registers == 2 })
}
