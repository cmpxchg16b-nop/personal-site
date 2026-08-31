package echobot

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pion/webrtc/v4"

	"personal-site/pkg/models/ss"
	"personal-site/pkg/rtc"
)

// The bot tests wire a bot-equipped client and a probe client to a real
// SimpleOnMemorySSProvider over plain channel pairs — the transport seam,
// mirroring pkg/rtc's client tests. The probe speaks the wire protocols
// directly — raw JSON frames on dcmsg, raw binary frames on dcbin — so the
// bot is tested against the documented formats, not against its own codec.

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
// subscriber id).
func startClient(t *testing.T, net *clientTestNet, name string, configure func(*rtc.RTCClientConfiguration)) *rtc.HeadlessRTCClient {
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
	return c
}

// startBot builds a client with a Bot attached and Runs it on the net.
func startBot(t *testing.T, net *clientTestNet, name string, id ss.SubscriberId) *rtc.HeadlessRTCClient {
	t.Helper()
	c := startClient(t, net, name, func(c *rtc.RTCClientConfiguration) {
		c.SubscriberId = id
	})
	New(c, Configuration{Logger: testLogger(t)})
	return c
}

// startUserProbe Runs a plain client with a wireProbe on both labels.
func startUserProbe(t *testing.T, net *clientTestNet, name string, id ss.SubscriberId) (*rtc.HeadlessRTCClient, *wireProbe) {
	t.Helper()
	user := startClient(t, net, name, func(c *rtc.RTCClientConfiguration) {
		c.SubscriberId = id
	})
	probe := newWireProbe()
	probe.register(t, user)
	return user, probe
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
	bins   map[ss.SubscriberId][][]byte                       // dcbin frames
}

func newWireProbe() *wireProbe {
	return &wireProbe{
		opened: map[ss.SubscriberId]map[string]*webrtc.DataChannel{},
		texts:  map[ss.SubscriberId][]string{},
		bins:   map[ss.SubscriberId][][]byte{},
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
			p.mu.Lock()
			defer p.mu.Unlock()
			if msg.IsString {
				p.texts[peer] = append(p.texts[peer], string(msg.Data))
			} else {
				p.bins[peer] = append(p.bins[peer], msg.Data)
			}
		})
	})
}

// register subscribes the probe to both well-known labels of a client.
func (p *wireProbe) register(t *testing.T, c *rtc.HeadlessRTCClient) {
	t.Helper()
	c.HandleDataChannel(DataChannelLabelMessages, p.handler(DataChannelLabelMessages))
	c.HandleDataChannel(DataChannelLabelBinary, p.handler(DataChannelLabelBinary))
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

func (p *wireProbe) binsFrom(peer ss.SubscriberId) [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.bins[peer])
}

// waitTexts waits until at least n text frames arrived from peer and
// returns a snapshot.
func (p *wireProbe) waitTexts(t *testing.T, peer ss.SubscriberId, n int) []string {
	t.Helper()
	waitFor(t, fmt.Sprintf("%d text frames from %s", n, peer), func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.texts[peer]) >= n
	})
	return p.textsFrom(peer)
}

// waitBins waits until at least n binary frames arrived from peer and
// returns a snapshot.
func (p *wireProbe) waitBins(t *testing.T, peer ss.SubscriberId, n int) [][]byte {
	t.Helper()
	waitFor(t, fmt.Sprintf("%d binary frames from %s", n, peer), func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.bins[peer]) >= n
	})
	return p.binsFrom(peer)
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

// fileFrameBytes encodes a FILE frame for the wire, straight from the
// documented layout (binaryframes.ts) — deliberately independent of the
// production codec.
func fileFrameBytes(fileId uuid.UUID, seq uint32, offset, total uint64, payload []byte) []byte {
	out := make([]byte, 48+len(payload))
	copy(out[:4], "FILE")
	copy(out[4:20], fileId[:])
	binary.BigEndian.PutUint32(out[20:24], seq)
	binary.BigEndian.PutUint64(out[24:32], offset)
	binary.BigEndian.PutUint64(out[32:40], total)
	binary.BigEndian.PutUint64(out[40:48], uint64(len(payload)))
	copy(out[48:], payload)
	return out
}

// decodeAckRaw decodes a FACK frame straight from the documented layout.
func decodeAckRaw(t *testing.T, data []byte) (fileId uuid.UUID, ackSeq uint32, ackedBytes uint64) {
	t.Helper()
	if len(data) != 32 {
		t.Fatalf("FACK frame size is %d, want 32", len(data))
	}
	if string(data[:4]) != "FACK" {
		t.Fatalf("frame type is %q, want %q", data[:4], "FACK")
	}
	copy(fileId[:], data[4:20])
	return fileId, binary.BigEndian.Uint32(data[20:24]), binary.BigEndian.Uint64(data[24:32])
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// The wire mime types, copied from the protocol documentation — the
// production constants live in msg_handler and are deliberately not used
// here, like the binary frame helpers above.
const (
	wireMimePlaintext          = "text/plain"
	wireMimeFileTransferStatus = "application/x-file-transfer-status"
	wireMimeChatControl        = "application/x-chat-control"
	wireMimeSip                = "application/x-sip"
)

// rawSipResponse is the suite's wire view of a SIP status line.
type rawSipResponse struct {
	Code   int    `json:"code"`
	Phrase string `json:"phrase"`
}

// rawChatControl is the suite's wire view of a chatControl body.
type rawChatControl struct {
	Subtype         string   `json:"subtype"`
	TargetMessageId ss.MsgId `json:"targetMessageId"`
	Text            string   `json:"text"`
}

// rawMsg is the suite's wire view of one decoded dcmsg frame — decoded
// straight from the documented format, independent of the production
// codec (which lives in msg_handler now).
type rawMsg struct {
	raw         map[string]json.RawMessage
	echo        bool
	msgId       ss.MsgId
	from        ss.SubscriberId
	to          ss.SubscriberId
	inReplyTo   ss.MsgId
	mimeType    string
	plaintext   string
	fileId      string // the fileTransfer body's, when present
	sipCallId   string // the sip body's, when present
	sipResponse *rawSipResponse
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
		raw:       fields,
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
	if raw := fields["fileTransfer"]; raw != nil {
		var body struct {
			FileId string `json:"fileId"`
		}
		if err := json.Unmarshal(raw, &body); err == nil {
			msg.fileId = body.FileId
		}
	}
	if raw := fields["sip"]; raw != nil {
		var body struct {
			CallId   string          `json:"callId"`
			Response *rawSipResponse `json:"response"`
		}
		if err := json.Unmarshal(raw, &body); err == nil {
			msg.sipCallId = body.CallId
			msg.sipResponse = body.Response
		}
	}
	return msg
}

// decodeChatControlRaw decodes the chatControl body of a decoded frame.
func decodeChatControlRaw(t *testing.T, m *rawMsg) *rawChatControl {
	t.Helper()
	raw := m.raw["chatControl"]
	if raw == nil {
		t.Fatal("frame carries no chatControl body")
	}
	var cc rawChatControl
	if err := json.Unmarshal(raw, &cc); err != nil {
		t.Fatalf("chatControl body: %v", err)
	}
	return &cc
}

// TestBotEchoesTextMessageAndReplies covers the chat-line flow: the bot
// bounces the message verbatim with echo set (unknown fields ride along)
// and, being an echo bot, answers it with a fresh message of identical
// content. The bot is impolite here: its channels arrive via OnDataChannel.
func TestBotEchoesTextMessageAndReplies(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot := startBot(t, net, "bot", "2-bot")
	user, probe := startUserProbe(t, net, "user", "1-user") // polite: creates the channels
	botId, userId := pairUp(t, bot, user)

	dc := probe.waitDC(t, botId, DataChannelLabelMessages)
	m := baseMsg(ss.WellKnownChIdMain, userId, botId)
	m["plaintext"] = "hello there"
	m["xCustom"] = 42 // an unknown field must ride the bounce verbatim
	msgId := ss.MsgId(m["msgId"].(string))
	if err := dc.SendText(mustJSON(t, m)); err != nil {
		t.Fatalf("SendText: %v", err)
	}

	got := probe.waitTexts(t, botId, 2)

	bounce := decodeMsgRaw(got[0])
	if bounce == nil {
		t.Fatalf("the bounce did not decode: %s", got[0])
	}
	if !bounce.echo || bounce.msgId != msgId || bounce.from != userId || bounce.to != botId ||
		bounce.mimeType != wireMimePlaintext || bounce.plaintext != "hello there" {
		t.Fatalf("bounce = %+v, want the original message with echo set", bounce)
	}
	if _, ok := bounce.raw["xCustom"]; !ok {
		t.Fatal("the bounce dropped the unknown field")
	}

	reply := decodeMsgRaw(got[1])
	if reply == nil {
		t.Fatalf("the reply did not decode: %s", got[1])
	}
	if reply.echo || reply.from != botId || reply.to != userId ||
		reply.mimeType != wireMimePlaintext || reply.plaintext != "hello there" ||
		reply.msgId == msgId || reply.inReplyTo != msgId {
		t.Fatalf("reply = %+v, want a fresh message with identical content answering %s", reply, msgId)
	}
}

// TestBotNeverEchoesAnEcho covers the loop guard: a frame already carrying
// the echo flag is never bounced.
func TestBotNeverEchoesAnEcho(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot := startBot(t, net, "bot", "1-bot")
	user, probe := startUserProbe(t, net, "user", "2-user")
	botId, userId := pairUp(t, bot, user)

	dc := probe.waitDC(t, botId, DataChannelLabelMessages)
	m := baseMsg(ss.WellKnownChIdMain, userId, botId)
	m["plaintext"] = "hello"
	m["echo"] = true
	if err := dc.SendText(mustJSON(t, m)); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if got := probe.textsFrom(botId); len(got) != 0 {
		t.Fatalf("the bot answered an echo: %v", got)
	}
}

// TestBotEchoesChatControlWithoutReply covers the other echoed kind: a
// chat-control message is bounced (so its sender applies it) but gets no
// echo-purpose reply.
func TestBotEchoesChatControlWithoutReply(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot := startBot(t, net, "bot", "1-bot")
	user, probe := startUserProbe(t, net, "user", "2-user")
	botId, userId := pairUp(t, bot, user)

	dc := probe.waitDC(t, botId, DataChannelLabelMessages)
	m := baseMsg(ss.WellKnownChIdMain, userId, botId)
	m["mimeType"] = "application/x-chat-control"
	m["chatControl"] = map[string]any{"subtype": "delete", "targetMessageId": uuid.NewString()}
	msgId := ss.MsgId(m["msgId"].(string))
	if err := dc.SendText(mustJSON(t, m)); err != nil {
		t.Fatalf("SendText: %v", err)
	}

	got := probe.waitTexts(t, botId, 1)
	bounce := decodeMsgRaw(got[0])
	if bounce == nil {
		t.Fatalf("the bounce did not decode: %s", got[0])
	}
	if !bounce.echo || bounce.msgId != msgId || bounce.mimeType != wireMimeChatControl {
		t.Fatalf("bounce = %+v, want the chat-control message with echo set", bounce)
	}
	time.Sleep(300 * time.Millisecond)
	if got := probe.textsFrom(botId); len(got) != 1 {
		t.Fatalf("chat control got a reply: %v", got[1:])
	}
}

// TestBotDeclinesCalls covers the call protocol: SIP messages are never
// bounced, and an incoming INVITE — voice or video — is answered with a
// 603 Decline naming the dialog's call id. The rest of the dialog (CANCEL,
// BYE) is ignored.
func TestBotDeclinesCalls(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot := startBot(t, net, "bot", "1-bot")
	user, probe := startUserProbe(t, net, "user", "2-user")
	botId, userId := pairUp(t, bot, user)

	dc := probe.waitDC(t, botId, DataChannelLabelMessages)
	sipMsg := func(body map[string]any) map[string]any {
		m := baseMsg(ss.WellKnownChIdMain, userId, botId)
		m["mimeType"] = "application/x-sip"
		m["sip"] = body
		return m
	}
	invite := func(callId, media string) map[string]any {
		return sipMsg(map[string]any{
			"callId": callId, "method": "INVITE", "X-Media": media, "X-Call-Status": "inviting",
		})
	}
	send := func(m map[string]any) {
		t.Helper()
		if err := dc.SendText(mustJSON(t, m)); err != nil {
			t.Fatalf("SendText: %v", err)
		}
	}

	voiceCall := uuid.NewString()
	send(invite(voiceCall, "voice"))
	videoCall := uuid.NewString()
	send(invite(videoCall, "video"))

	got := probe.waitTexts(t, botId, 2)
	for i, callId := range []string{voiceCall, videoCall} {
		decline := decodeMsgRaw(got[i])
		if decline == nil {
			t.Fatalf("the decline of %s did not decode: %s", callId, got[i])
		}
		if decline.echo || decline.from != botId || decline.to != userId || decline.mimeType != wireMimeSip {
			t.Fatalf("decline = %+v, want a fresh SIP message from the bot", decline)
		}
		if decline.sipCallId != callId || decline.sipResponse == nil ||
			*decline.sipResponse != (rawSipResponse{Code: 603, Phrase: "Decline"}) {
			t.Fatalf("decline = %+v, want a 603 Decline of %s", decline, callId)
		}
	}

	// The rest of the dialog is neither bounced nor answered.
	send(sipMsg(map[string]any{"callId": voiceCall, "method": "CANCEL"}))
	send(sipMsg(map[string]any{"callId": voiceCall, "method": "BYE"}))
	time.Sleep(300 * time.Millisecond)
	if got := probe.textsFrom(botId); len(got) != 2 {
		t.Fatalf("the dialog got more than its declines: %v", got[2:])
	}
}

// TestBotDropsMalformedMessages covers the drop rules: frames that do not
// decode, that fail validation, or that name another pair get no answer at
// all.
func TestBotDropsMalformedMessages(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot := startBot(t, net, "bot", "1-bot")
	user, probe := startUserProbe(t, net, "user", "2-user")
	botId, userId := pairUp(t, bot, user)
	dc := probe.waitDC(t, botId, DataChannelLabelMessages)

	good := func() map[string]any {
		m := baseMsg(ss.WellKnownChIdMain, userId, botId)
		m["plaintext"] = "hi"
		return m
	}
	without := func(m map[string]any, key string) map[string]any {
		delete(m, key)
		return m
	}

	frames := []string{
		"not json at all",
		"[1,2,3]",
		mustJSON(t, without(good(), "msgId")),
		mustJSON(t, without(good(), "plaintext")),
		mustJSON(t, func() map[string]any { m := good(); m["echo"] = "yes"; return m }()),
		mustJSON(t, func() map[string]any { m := good(); m["channelId"] = "no-such-channel"; return m }()),
		mustJSON(t, func() map[string]any { m := good(); m["fromSubscriberId"] = "someone-else"; return m }()),
		mustJSON(t, func() map[string]any {
			m := good()
			m["mimeType"] = "application/x-file-transfer-status" // no fileTransfer body
			return m
		}()),
		mustJSON(t, func() map[string]any {
			m := good()
			m["mimeType"] = "application/x-sip"
			m["sip"] = map[string]any{"callId": uuid.NewString(), "method": "INVITE"} // no X-Call-Status
			return m
		}()),
		mustJSON(t, func() map[string]any {
			m := good()
			m["mimeType"] = "application/x-sip"
			m["sip"] = map[string]any{ // method XOR response: both set
				"callId": uuid.NewString(), "method": "INVITE", "X-Call-Status": "inviting",
				"response": map[string]any{"code": 200, "phrase": "OK"},
			}
			return m
		}()),
	}
	for _, frame := range frames {
		if err := dc.SendText(frame); err != nil {
			t.Fatalf("SendText: %v", err)
		}
	}
	time.Sleep(400 * time.Millisecond)
	if got := probe.textsFrom(botId); len(got) != 0 {
		t.Fatalf("malformed frames got answers: %v", got)
	}
}

// TestBotReceivesFileAndReportsHash covers the file-transfer flow: the
// announcement's bounce, the per-frame cumulative FACKs, and the running
// sha256 reported as a chat message — opened by the first chunk, amended
// by the final one (intermediate chunks are throttled away).
func TestBotReceivesFileAndReportsHash(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot := startBot(t, net, "bot", "1-bot")
	user, probe := startUserProbe(t, net, "user", "2-user")
	botId, userId := pairUp(t, bot, user)
	msgDC := probe.waitDC(t, botId, DataChannelLabelMessages)
	binDC := probe.waitDC(t, botId, DataChannelLabelBinary)

	content := []byte(strings.Repeat("0123456789", 10)) // 100 bytes
	fileId := uuid.New()

	// The announcement rides dcmsg and is bounced.
	announce := baseMsg(ss.WellKnownChIdMain, userId, botId)
	announce["mimeType"] = "application/x-file-transfer-status"
	announce["fileTransfer"] = map[string]any{
		"fileId":              fileId.String(),
		"kind":                "file",
		"filename":            "data.bin",
		"fileMIMEType":        "application/octet-stream",
		"fileSizeTotalBytes":  100,
		"fileSizeTransferred": 0,
		"fileTransferStatus":  "pending",
	}
	announceId := ss.MsgId(announce["msgId"].(string))
	if err := msgDC.SendText(mustJSON(t, announce)); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	got := probe.waitTexts(t, botId, 1)
	bounce := decodeMsgRaw(got[0])
	if bounce == nil {
		t.Fatalf("the announcement's bounce did not decode: %s", got[0])
	}
	if !bounce.echo || bounce.msgId != announceId ||
		bounce.mimeType != wireMimeFileTransferStatus || bounce.fileId != fileId.String() {
		t.Fatalf("bounce = %+v, want the announcement with echo set", bounce)
	}

	// The bytes ride dcbin, in three frames.
	for i, frame := range [][]byte{
		fileFrameBytes(fileId, 0, 0, 100, content[:40]),
		fileFrameBytes(fileId, 1, 40, 100, content[40:80]),
		fileFrameBytes(fileId, 2, 80, 100, content[80:]),
	} {
		if err := binDC.Send(frame); err != nil {
			t.Fatalf("Send frame %d: %v", i, err)
		}
	}

	// Every accepted frame is acknowledged, cumulatively.
	acks := probe.waitBins(t, botId, 3)
	wantAcks := [][2]uint64{{1, 40}, {2, 80}, {3, 100}}
	for i, want := range wantAcks {
		id, ackSeq, ackedBytes := decodeAckRaw(t, acks[i])
		if id != fileId || uint64(ackSeq) != want[0] || ackedBytes != want[1] {
			t.Fatalf("ack %d = (%s, %d, %d), want (%s, %d, %d)", i, id, ackSeq, ackedBytes, fileId, want[0], want[1])
		}
	}

	// The running hash: the first chunk opens the report; the middle
	// chunk is throttled away (hashReportChunkInterval), so the single
	// amend is the final chunk's, carrying the complete digest. (The
	// channel is ordered: had the middle chunk amended, got[2] would be
	// its amend, not this one.)
	got = probe.waitTexts(t, botId, 3)
	hashMsg := decodeMsgRaw(got[1])
	if hashMsg == nil {
		t.Fatalf("the hash message did not decode: %s", got[1])
	}
	if hashMsg.echo || hashMsg.from != botId || hashMsg.to != userId ||
		hashMsg.mimeType != wireMimePlaintext || hashMsg.plaintext != sha256Hex(content[:40]) ||
		hashMsg.inReplyTo != announceId {
		t.Fatalf("hash message = %+v, want a sha256 report answering %s", hashMsg, announceId)
	}
	amend := decodeMsgRaw(got[2])
	if amend == nil {
		t.Fatalf("the final amend did not decode: %s", got[2])
	}
	if amend.echo || amend.from != botId || amend.mimeType != wireMimeChatControl {
		t.Fatalf("final amend = %+v, want a chat-control message from the bot", amend)
	}
	cc := decodeChatControlRaw(t, amend)
	if cc.Subtype != "amend" || cc.TargetMessageId != hashMsg.msgId || cc.Text != sha256Hex(content) {
		t.Fatalf("final amend = %+v, want an amend of %s to %q", cc, hashMsg.msgId, sha256Hex(content))
	}
}

// TestBotThrottlesHashReports covers the hash-report throttle: between
// the first chunk's opening message and the final chunk's mandatory amend,
// a report rides only every hashReportChunkInterval chunks.
func TestBotThrottlesHashReports(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot := startBot(t, net, "bot", "1-bot")
	user, probe := startUserProbe(t, net, "user", "2-user")
	botId, _ := pairUp(t, bot, user)
	probe.waitDC(t, botId, DataChannelLabelMessages)
	binDC := probe.waitDC(t, botId, DataChannelLabelBinary)

	// One byte per chunk, one chunk past the report interval.
	content := make([]byte, hashReportChunkInterval+1)
	for i := range content {
		content[i] = byte(i)
	}
	fileId := uuid.New()
	for i, b := range content {
		frame := fileFrameBytes(fileId, uint32(i), uint64(i), uint64(len(content)), []byte{b})
		if err := binDC.Send(frame); err != nil {
			t.Fatalf("Send frame %d: %v", i, err)
		}
	}

	// Every chunk is still acknowledged, cumulatively.
	acks := probe.waitBins(t, botId, len(content))
	id, ackSeq, ackedBytes := decodeAckRaw(t, acks[len(acks)-1])
	if id != fileId || ackSeq != uint32(len(content)) || ackedBytes != uint64(len(content)) {
		t.Fatalf("last ack = (%s, %d, %d), want (%s, %d, %d)",
			id, ackSeq, ackedBytes, fileId, len(content), len(content))
	}

	// Three reports only: the opening message, the interval amend at
	// chunk hashReportChunkInterval, and the final chunk's amend.
	got := probe.waitTexts(t, botId, 3)
	hashMsg := decodeMsgRaw(got[0])
	if hashMsg == nil {
		t.Fatalf("the hash message did not decode: %s", got[0])
	}
	if hashMsg.echo || hashMsg.from != botId || hashMsg.mimeType != wireMimePlaintext ||
		hashMsg.plaintext != sha256Hex(content[:1]) {
		t.Fatalf("hash message = %+v, want the first chunk's sha256 report", hashMsg)
	}
	for i, want := range []string{
		sha256Hex(content[:hashReportChunkInterval]),
		sha256Hex(content),
	} {
		amend := decodeMsgRaw(got[1+i])
		if amend == nil {
			t.Fatalf("amend %d did not decode: %s", i, got[1+i])
		}
		if amend.echo || amend.from != botId || amend.mimeType != wireMimeChatControl {
			t.Fatalf("amend %d = %+v, want a chat-control message from the bot", i, amend)
		}
		cc := decodeChatControlRaw(t, amend)
		if cc.Subtype != "amend" || cc.TargetMessageId != hashMsg.msgId || cc.Text != want {
			t.Fatalf("amend %d = %+v, want an amend of %s to %q", i, cc, hashMsg.msgId, want)
		}
	}
}

// TestBotReceivesEmptyFile covers the degenerate transfer: one FILE frame
// with an empty payload, acknowledged and hashed (the empty-stream hash).
func TestBotReceivesEmptyFile(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot := startBot(t, net, "bot", "1-bot")
	user, probe := startUserProbe(t, net, "user", "2-user")
	botId, _ := pairUp(t, bot, user)
	probe.waitDC(t, botId, DataChannelLabelMessages)
	binDC := probe.waitDC(t, botId, DataChannelLabelBinary)

	fileId := uuid.New()
	if err := binDC.Send(fileFrameBytes(fileId, 0, 0, 0, nil)); err != nil {
		t.Fatalf("Send: %v", err)
	}

	acks := probe.waitBins(t, botId, 1)
	id, ackSeq, ackedBytes := decodeAckRaw(t, acks[0])
	if id != fileId || ackSeq != 1 || ackedBytes != 0 {
		t.Fatalf("ack = (%s, %d, %d), want (%s, 1, 0)", id, ackSeq, ackedBytes, fileId)
	}
	got := probe.waitTexts(t, botId, 1)
	hashMsg := decodeMsgRaw(got[0])
	if hashMsg == nil {
		t.Fatalf("the hash message did not decode: %s", got[0])
	}
	if hashMsg.plaintext != sha256Hex(nil) {
		t.Fatalf("hash = %q, want %q (the empty-stream hash)", hashMsg.plaintext, sha256Hex(nil))
	}
	if _, ok := hashMsg.raw["inReplyTo"]; ok {
		t.Fatal("an unannounced file's hash message carries an inReplyTo")
	}

	time.Sleep(200 * time.Millisecond)
	if got := probe.textsFrom(botId); len(got) != 1 {
		t.Fatalf("the empty file got amends: %v", got[1:])
	}
}

// TestBotDropsCorruptTransfers covers the reassembly rules: a stream must
// start at seq 0 / offset 0 and continue its contiguous prefix exactly — a
// gap, an overlap, or a mismatched total drops the transfer, and
// unaccepted frames are not acknowledged.
func TestBotDropsCorruptTransfers(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot := startBot(t, net, "bot", "1-bot")
	user, probe := startUserProbe(t, net, "user", "2-user")
	botId, _ := pairUp(t, bot, user)
	probe.waitDC(t, botId, DataChannelLabelMessages)
	binDC := probe.waitDC(t, botId, DataChannelLabelBinary)

	payload := []byte(strings.Repeat("x", 40))
	send := func(frame []byte) {
		t.Helper()
		if err := binDC.Send(frame); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}

	// A stream whose first frame does not start it: silently dropped.
	late := uuid.New()
	send(fileFrameBytes(late, 1, 0, 100, payload))

	// Three streams accepted and then corrupted: a seq gap, a mismatched
	// total, an offset overlap.
	gap := uuid.New()
	send(fileFrameBytes(gap, 0, 0, 100, payload))
	send(fileFrameBytes(gap, 2, 40, 100, payload))
	mismatch := uuid.New()
	send(fileFrameBytes(mismatch, 0, 0, 100, payload))
	send(fileFrameBytes(mismatch, 1, 40, 200, payload))
	overlap := uuid.New()
	send(fileFrameBytes(overlap, 0, 0, 100, payload))
	send(fileFrameBytes(overlap, 1, 39, 100, payload))

	// Only the three clean first frames are acknowledged; each opened a
	// hash report on the messaging channel.
	acks := probe.waitBins(t, botId, 3)
	for i, fileId := range []uuid.UUID{gap, mismatch, overlap} {
		id, ackSeq, ackedBytes := decodeAckRaw(t, acks[i])
		if id != fileId || ackSeq != 1 || ackedBytes != 40 {
			t.Fatalf("ack %d = (%s, %d, %d), want (%s, 1, 40)", i, id, ackSeq, ackedBytes, fileId)
		}
	}
	probe.waitTexts(t, botId, 3)
	time.Sleep(300 * time.Millisecond)
	if got := probe.binsFrom(botId); len(got) != 3 {
		t.Fatalf("corrupt frames got acknowledged: %d extra frames", len(got)-3)
	}
	if got := probe.textsFrom(botId); len(got) != 3 {
		t.Fatalf("corrupt frames moved a hash: %v", got[3:])
	}
}
