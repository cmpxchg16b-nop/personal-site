package musicbot

// The music bot tests wire a bot-equipped client and a probe client to a
// real SimpleOnMemorySSProvider over plain channel pairs — the harness
// mirrors pkg/rtc/echobot's suite (test-only scaffolding duplicated so the
// suites evolve independently). The probe speaks the wire protocols
// directly — raw JSON frames on dcmsg, raw binary frames on dcbin — so
// the bot is tested against the documented formats, not against its own
// codec. Media assertions ride the probe's own track handler (the
// client's HandleTrack) and real RTP off the wire.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/mewkiz/flac"
	"github.com/mewkiz/flac/frame"
	"github.com/mewkiz/flac/meta"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"personal-site/pkg/models/audiosource"
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

// startBot builds a client with the music bot attached (serving the
// given songbook) and Runs it on the net.
func startBot(t *testing.T, net *clientTestNet, name string, id ss.SubscriberId, sources []*audiosource.AudioSourceData) *rtc.HeadlessRTCClient {
	t.Helper()
	c := startClient(t, net, name, func(c *rtc.RTCClientConfiguration) {
		c.SubscriberId = id
	})
	New(c, Configuration{Logger: testLogger(t), AudioSources: sources})
	return c
}

// requiresOpus skips the test when the opus encoder is unavailable (a
// build without cgo — the linear pcm path needs libopus).
func requiresOpus(t *testing.T) {
	t.Helper()
	if _, err := newMusicEncoder(opusSampleRate, opusChannels); err != nil {
		t.Skipf("the opus encoder is unavailable: %v", err)
	}
}

// testSongbook is the wire suite's standard songbook: the μ-law
// "chiptune" stand-in (the default song incoming calls answer with), a
// linear PCM song, and a flac song — one of every playable family.
func testSongbook(t *testing.T) []*audiosource.AudioSourceData {
	t.Helper()
	return []*audiosource.AudioSourceData{
		muLawSong("chiptune", 10),
		pcmSong("stereo", 2400),
		flacSong(t, "flac", 2400),
	}
}

// muLawSong is a μ-law source (8000 Hz, mono) whose bytes are a
// deterministic non-silent pattern — the RTP assertions need music,
// not silence. frames is the song's length in 20 ms frames.
func muLawSong(name string, frames int) *audiosource.AudioSourceData {
	data := make([]byte, frames*samplesPerFrame)
	for i := range data {
		data[i] = byte(i * 7) // every residue mod 256 appears
	}
	return &audiosource.AudioSourceData{
		Id:               "test-" + name,
		Name:             name,
		Description:      "a test mu-law song",
		Author:           "the test suite",
		SampleFormatType: audiosource.SampleMuLaw,
		BitDepth:         8,
		NumericType:      audiosource.NumUnsignedInt,
		NumChannels:      1,
		Interleaved:      true,
		InlineData:       data,
		Compression:      audiosource.CompressionNone,
		SampleRate:       8000,
		NumTotalSamples:  len(data),
	}
}

// pcmSong is a linear PCM source (48000 Hz, stereo, signed 16-bit)
// carrying distinct tones per channel. samples is the inter-channel
// sample count.
func pcmSong(name string, samples int) *audiosource.AudioSourceData {
	data := make([]byte, samples*4)
	for i := 0; i < samples; i++ {
		l := int16(20000 * math.Sin(2*math.Pi*float64(i)/48)) // 1 kHz
		r := int16(14000 * math.Sin(2*math.Pi*float64(i)/16)) // 3 kHz
		binary.LittleEndian.PutUint16(data[4*i:], uint16(l))
		binary.LittleEndian.PutUint16(data[4*i+2:], uint16(r))
	}
	return &audiosource.AudioSourceData{
		Id:               "test-" + name,
		Name:             name,
		Description:      "a test linear pcm song",
		Author:           "the test suite",
		SampleFormatType: audiosource.SampleLinearPCM,
		BitDepth:         16,
		NumericType:      audiosource.NumSignedInt,
		NumChannels:      2,
		Interleaved:      true,
		InlineData:       data,
		Compression:      audiosource.CompressionNone,
		SampleRate:       48000,
		NumTotalSamples:  samples,
	}
}

// flacSong is a linear PCM source whose inline data is a flac stream
// encoding a stereo tone — encoded with mewkiz/flac's encoder, an
// encoder independent of everything the bot plays it through.
func flacSong(t *testing.T, name string, samples int) *audiosource.AudioSourceData {
	t.Helper()
	src := pcmSong(name, samples)
	src.Compression = audiosource.CompressionFLAC

	channels := make([][]int32, 2)
	for ch := range channels {
		channels[ch] = make([]int32, samples)
	}
	for i := 0; i < samples; i++ {
		for ch := 0; ch < 2; ch++ {
			o := 4*i + 2*ch
			channels[ch][i] = int32(uint16(src.InlineData[o]) | uint16(src.InlineData[o+1])<<8)
		}
	}
	var buf bytes.Buffer
	enc, err := flac.NewEncoder(&buf, &meta.StreamInfo{
		BlockSizeMin:  uint16(samples),
		BlockSizeMax:  uint16(samples),
		SampleRate:    48000,
		NChannels:     2,
		BitsPerSample: 16,
		NSamples:      uint64(samples),
	})
	if err != nil {
		t.Fatalf("flac.NewEncoder: %v", err)
	}
	f := &frame.Frame{
		Header: frame.Header{
			HasFixedBlockSize: true,
			BlockSize:         uint16(samples),
			SampleRate:        48000,
			Channels:          frame.ChannelsLR,
			BitsPerSample:     16,
			Num:               0,
		},
	}
	for _, ch := range channels {
		f.Subframes = append(f.Subframes, &frame.Subframe{
			SubHeader: frame.SubHeader{Pred: frame.PredVerbatim},
			Samples:   ch,
			NSamples:  samples,
		})
	}
	if err := enc.WriteFrame(f); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("Close the flac encoder: %v", err)
	}
	src.InlineData = buf.Bytes()
	return src
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
	c.HandleDataChannel(msg_handler.DataChannelLabelMessages, p.handler(msg_handler.DataChannelLabelMessages))
	c.HandleDataChannel(msg_handler.DataChannelLabelBinary, p.handler(msg_handler.DataChannelLabelBinary))
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

// startUserProbe Runs a plain client with a wireProbe on both labels and
// a trackProbe on its media.
func startUserProbe(t *testing.T, net *clientTestNet, name string, id ss.SubscriberId) (*rtc.HeadlessRTCClient, *wireProbe, *trackProbe) {
	t.Helper()
	user := startClient(t, net, name, func(c *rtc.RTCClientConfiguration) {
		c.SubscriberId = id
	})
	probe := newWireProbe()
	probe.register(t, user)
	tprobe := newTrackProbe()
	tprobe.register(t, user)
	return user, probe, tprobe
}

// trackProbe records every remote track arriving at a client (the
// client's HandleTrack).
type trackProbe struct {
	mu     sync.Mutex
	tracks []*webrtc.TrackRemote
}

func newTrackProbe() *trackProbe {
	return &trackProbe{}
}

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

// The wire mime types, copied from the protocol documentation — the
// production constants live in msg_handler and are deliberately not used
// here, like the binary frame helper above.
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
	raw       map[string]json.RawMessage
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
// message's callId, an amend's targetMessageId — never by arrival order
// (the hub's amends and the handler's replies race freely). Returns the
// matched message.
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
// The tests.
// ---------------------------------------------------------------------------

// TestMusicBotHelpAndUnknownCommand covers the CLI's bookkeeping: /help
// answers with the command list; anything unrecognized answers with the
// help hint. Every reply threads on its command (inReplyTo); every chat
// line is also bounced verbatim — the Server's echo rule.
func TestMusicBotHelpAndUnknownCommand(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot := startBot(t, net, "bot", "2-bot", testSongbook(t))
	user, probe, _ := startUserProbe(t, net, "user", "1-user") // polite: creates the channels
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

	help := waitBotMessage(t, probe, botId, "the /help reply", isChatReply(helpMsg, "/help"))
	for _, cmd := range []string{"/help", "/list-songs", "/play <song>"} {
		if !strings.Contains(help.plaintext, cmd) {
			t.Fatalf("the help text lacks %q: %q", cmd, help.plaintext)
		}
	}
	waitBotMessage(t, probe, botId, "the unknown-command hint", isChatReply(unknownMsg, "/help"))
}

// TestMusicBotListsSongs covers /list-songs: the songbook's one generated
// song is listed, the reply threaded on the command.
func TestMusicBotListsSongs(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot := startBot(t, net, "bot", "2-bot", testSongbook(t))
	user, probe, _ := startUserProbe(t, net, "user", "1-user")
	botId, userId := pairUp(t, bot, user)

	dc := probe.waitDC(t, botId, msg_handler.DataChannelLabelMessages)
	m := baseMsg(ss.WellKnownChIdMain, userId, botId)
	m["plaintext"] = "/list-songs"
	if err := dc.SendText(mustJSON(t, m)); err != nil {
		t.Fatalf("SendText: %v", err)
	}

	list := waitBotMessage(t, probe, botId, "the song list",
		isChatReply(ss.MsgId(m["msgId"].(string)), "chiptune"))
	if !strings.Contains(list.plaintext, "Available songs") {
		t.Fatalf("the song list = %q", list.plaintext)
	}
	for _, name := range []string{"stereo", "flac"} {
		if !strings.Contains(list.plaintext, name) {
			t.Fatalf("the song list = %q, want the injected song %q", list.plaintext, name)
		}
	}
}

// TestMusicBotRefusesAttachments covers the attachment policy: the
// transfer mechanics run (the announcement bounces, the frame is
// acknowledged), and the completed attachment is answered with the
// refusal, threaded on the announcement.
func TestMusicBotRefusesAttachments(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot := startBot(t, net, "bot", "2-bot", testSongbook(t))
	user, probe, _ := startUserProbe(t, net, "user", "1-user")
	botId, userId := pairUp(t, bot, user)

	dc := probe.waitDC(t, botId, msg_handler.DataChannelLabelMessages)
	bin := probe.waitDC(t, botId, msg_handler.DataChannelLabelBinary)

	fileId := uuid.New()
	announcement := baseMsg(ss.WellKnownChIdMain, userId, botId)
	announcement["mimeType"] = wireMimeFileTransferStatus
	announcement["fileTransfer"] = map[string]any{
		"fileId":              fileId.String(),
		"kind":                "file",
		"filename":            "notes.txt",
		"fileMIMEType":        "text/plain",
		"fileSizeTotalBytes":  4,
		"fileSizeTransferred": 0,
		"fileTransferStatus":  "pending",
	}
	announcementId := ss.MsgId(announcement["msgId"].(string))
	if err := dc.SendText(mustJSON(t, announcement)); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	// The announcement rides dcmsg, the chunk dcbin — cross-channel
	// ordering is not guaranteed, so flush the messaging channel first
	// (a chat line answers only after the bot's hub recorded the
	// announcement), making the refusal's threading deterministic.
	flush := baseMsg(ss.WellKnownChIdMain, userId, botId)
	flush["plaintext"] = "/list-songs"
	if err := dc.SendText(mustJSON(t, flush)); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	probe.waitTexts(t, botId, 3) // both bounces + the song list
	if err := bin.Send(fileFrameBytes(fileId, 0, 0, 4, []byte("test"))); err != nil {
		t.Fatalf("bin Send: %v", err)
	}

	// The refusal: a plain-text answer threaded on the announcement.
	refusal := waitBotMessage(t, probe, botId, "the attachment refusal", func(m *rawMsg) bool {
		return m.mimeType == wireMimePlaintext && m.inReplyTo == announcementId
	})
	if !strings.Contains(refusal.plaintext, "not supported") {
		t.Fatalf("the refusal = %q", refusal.plaintext)
	}
	// The transfer was acknowledged — the mechanics never noticed the
	// policy's refusal.
	waitFor(t, "the FACK", func() bool { return len(probe.binsFrom(botId)) == 1 })
}

// TestMusicBotDeclinesVideoCalls covers the call policy's first rule: a
// video INVITE is declined on the spot (and never bounced — the call
// protocol's discipline).
func TestMusicBotDeclinesVideoCalls(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot := startBot(t, net, "bot", "2-bot", testSongbook(t))
	user, probe, tprobe := startUserProbe(t, net, "user", "1-user")
	botId, userId := pairUp(t, bot, user)

	dc := probe.waitDC(t, botId, msg_handler.DataChannelLabelMessages)
	callId := uuid.NewString()
	m := baseMsg(ss.WellKnownChIdMain, userId, botId)
	m["mimeType"] = wireMimeSip
	m["sip"] = map[string]any{
		"callId": callId, "method": "INVITE", "X-Media": "video", "X-Call-Status": "inviting",
	}
	if err := dc.SendText(mustJSON(t, m)); err != nil {
		t.Fatalf("SendText: %v", err)
	}

	// The 603 Decline names the dialog's call id; the INVITE is never
	// bounced.
	decline := waitBotMessage(t, probe, botId, "the 603 Decline", isSipResponse(callId, 603))
	if decline.from != botId || decline.to != userId || decline.sip.Response.Phrase != "Decline" {
		t.Fatalf("decline = %+v, want a 603 Decline of %s from the bot", decline, callId)
	}
	time.Sleep(300 * time.Millisecond)
	for _, frame := range probe.textsFrom(botId) {
		if decoded := decodeMsgRaw(frame); decoded != nil && decoded.echo {
			t.Fatalf("the INVITE was bounced: %s", frame)
		}
	}

	// No media came of it.
	tprobe.mu.Lock()
	defer tprobe.mu.Unlock()
	if len(tprobe.tracks) != 0 {
		t.Fatalf("a declined call produced %d tracks", len(tprobe.tracks))
	}
}

// TestMusicBotAcceptsVoiceCall covers the inbound call flow, end to end
// through real media: a voice INVITE gets a 200 OK, the bot's music track
// arrives at the probe and plays 20 ms PCMU frames, a chat line reports
// what plays, and a BYE ends it (a second INVITE is taken anew).
func TestMusicBotAcceptsVoiceCall(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot := startBot(t, net, "bot", "2-bot", testSongbook(t))
	user, probe, tprobe := startUserProbe(t, net, "user", "1-user")
	botId, userId := pairUp(t, bot, user)

	dc := probe.waitDC(t, botId, msg_handler.DataChannelLabelMessages)
	sipMsg := func(body map[string]any) ss.MsgId {
		t.Helper()
		m := baseMsg(ss.WellKnownChIdMain, userId, botId)
		m["mimeType"] = wireMimeSip
		m["sip"] = body
		if err := dc.SendText(mustJSON(t, m)); err != nil {
			t.Fatalf("SendText: %v", err)
		}
		return ss.MsgId(m["msgId"].(string))
	}
	invite := func(callId string) ss.MsgId {
		return sipMsg(map[string]any{
			"callId": callId, "method": "INVITE", "X-Media": "voice", "X-Call-Status": "inviting",
		})
	}

	callId := uuid.NewString()
	inviteMsgId := invite(callId)

	// The 200 OK (the dialog's own correlation: the call id), and the
	// "now playing" chat line threaded on the INVITE.
	ok := waitBotMessage(t, probe, botId, "the 200 OK", isSipResponse(callId, 200))
	if ok.from != botId || ok.to != userId || ok.sip.Response.Phrase != "OK" {
		t.Fatalf("the answer = %+v, want a 200 OK of %s from the bot", ok, callId)
	}
	waitBotMessage(t, probe, botId, "the now-playing line", isChatReply(inviteMsgId, "chiptune"))

	// The music is on the wire: an audio track arrives and its frames are
	// 160 bytes of actual music (not the μ-law silence byte).
	track := tprobe.waitTrack(t, 1)
	if track.Kind() != webrtc.RTPCodecTypeAudio {
		t.Fatalf("the track's kind is %s, want audio", track.Kind())
	}
	pkt := readOneRTP(t, track)
	if len(pkt.Payload) != 160 {
		t.Fatalf("the RTP payload is %d bytes, want 160 (20 ms of PCMU)", len(pkt.Payload))
	}
	distinct := map[byte]bool{}
	for _, b := range pkt.Payload {
		distinct[b] = true
	}
	if len(distinct) < 8 {
		t.Fatalf("the payload looks like silence (%d distinct bytes), want music", len(distinct))
	}

	// Hang up, then ring again: the state machine resets — the second
	// INVITE gets a fresh 200 OK of its own.
	sipMsg(map[string]any{"callId": callId, "method": "BYE"})
	secondCall := uuid.NewString()
	invite(secondCall)
	waitBotMessage(t, probe, botId, "the second call's 200 OK", isSipResponse(secondCall, 200))

	// The bot's own calls' bookkeeping never fires for peer-owned calls:
	// not one chat-control message the whole test.
	time.Sleep(300 * time.Millisecond)
	if n := countBotMessages(probe, botId, func(m *rawMsg) bool { return m.mimeType == wireMimeChatControl }); n != 0 {
		t.Fatalf("the bot amended a call it does not own: %d chat-control messages", n)
	}
}

// TestMusicBotPlayPhonesTheUser covers the outbound call flow: /play
// invites the user, the answer attaches the music (the probe reads real
// RTP), the Server keeps the INVITE's logged status current on its own
// (the accepted/ended amends), a second /play switches the song without a
// new call, and the BYE resets the state machine (the next /play invites
// anew).
func TestMusicBotPlayPhonesTheUser(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot := startBot(t, net, "bot", "2-bot", testSongbook(t))
	user, probe, tprobe := startUserProbe(t, net, "user", "1-user")
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

	playMsg := chat("/play chiptune")

	// The bot phones the user: a voice INVITE (inviting), and the calling
	// line threaded on the /play command.
	invite := waitBotMessage(t, probe, botId, "the bot's INVITE", func(m *rawMsg) bool {
		return isSipInvite(m) && m.sip.XMedia == "voice" && m.sip.XCallStatus == "inviting"
	})
	callId := invite.sip.CallId
	inviteMsgId := invite.msgId
	if callId == "" {
		t.Fatalf("the INVITE = %+v carries no call id", invite)
	}
	waitBotMessage(t, probe, botId, "the calling line", isChatReply(playMsg, "chiptune"))

	// The user picks up: the music track attaches and flows, and the
	// Server amends the bot's own INVITE to "accepted" on its own.
	sip(map[string]any{"callId": callId, "response": map[string]any{"code": 200, "phrase": "OK"}})
	track := tprobe.waitTrack(t, 1)
	if pkt := readOneRTP(t, track); len(pkt.Payload) != 160 {
		t.Fatalf("the RTP payload is %d bytes, want 160 (20 ms of PCMU)", len(pkt.Payload))
	}
	amend := waitBotMessage(t, probe, botId, "the accepted amend", isCallStatusAmend(inviteMsgId, callId, "accepted"))
	if amend.ccSip.XMedia != "voice" {
		t.Fatalf("the amend lost the call's media: %+v", amend.ccSip)
	}

	// Mid-call /play switches the song — answered, but no new INVITE.
	switchMsg := chat("/play chiptune")
	waitBotMessage(t, probe, botId, "the now-playing line", isChatReply(switchMsg, "Now playing"))
	time.Sleep(300 * time.Millisecond)
	if n := countBotMessages(probe, botId, isSipInvite); n != 1 {
		t.Fatalf("the mid-call /play produced %d INVITEs, want still 1", n)
	}

	// The user hangs up: the Server amends the INVITE to "ended"...
	sip(map[string]any{"callId": callId, "method": "BYE"})
	waitBotMessage(t, probe, botId, "the ended amend", isCallStatusAmend(inviteMsgId, callId, "ended"))

	// ...and the state machine reset: the next /play invites anew, with a
	// fresh call id.
	chat("/play chiptune")
	waitBotMessage(t, probe, botId, "the fresh INVITE", func(m *rawMsg) bool {
		return isSipInvite(m) && m.sip.CallId != "" && m.sip.CallId != callId
	})
}

// TestMusicBotPlayUnknownSong covers the /play guard: an unknown song is
// answered (threaded on the command) without a call.
func TestMusicBotPlayUnknownSong(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot := startBot(t, net, "bot", "2-bot", testSongbook(t))
	user, probe, _ := startUserProbe(t, net, "user", "1-user")
	botId, userId := pairUp(t, bot, user)

	dc := probe.waitDC(t, botId, msg_handler.DataChannelLabelMessages)
	m := baseMsg(ss.WellKnownChIdMain, userId, botId)
	m["plaintext"] = "/play silence"
	if err := dc.SendText(mustJSON(t, m)); err != nil {
		t.Fatalf("SendText: %v", err)
	}

	waitBotMessage(t, probe, botId, "the unknown-song answer",
		isChatReply(ss.MsgId(m["msgId"].(string)), "Unknown song"))
	time.Sleep(300 * time.Millisecond)
	if n := countBotMessages(probe, botId, isSipInvite); n != 0 {
		t.Fatalf("an unknown song produced %d INVITEs", n)
	}
}

// TestMusicBotCallDeclined covers the declined outbound call: the bot
// says so (threaded on the decline itself), the Server amends the INVITE
// to "rejected", and the state machine resets (the next /play invites
// anew).
func TestMusicBotCallDeclined(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot := startBot(t, net, "bot", "2-bot", testSongbook(t))
	user, probe, _ := startUserProbe(t, net, "user", "1-user")
	botId, userId := pairUp(t, bot, user)

	dc := probe.waitDC(t, botId, msg_handler.DataChannelLabelMessages)
	chat := func(text string) {
		t.Helper()
		m := baseMsg(ss.WellKnownChIdMain, userId, botId)
		m["plaintext"] = text
		if err := dc.SendText(mustJSON(t, m)); err != nil {
			t.Fatalf("SendText: %v", err)
		}
	}

	chat("/play chiptune")
	invite := waitBotMessage(t, probe, botId, "the bot's INVITE", isSipInvite)
	callId := invite.sip.CallId
	inviteMsgId := invite.msgId

	// The user declines.
	decline := baseMsg(ss.WellKnownChIdMain, userId, botId)
	decline["mimeType"] = wireMimeSip
	decline["sip"] = map[string]any{"callId": callId, "response": map[string]any{"code": 603, "phrase": "Decline"}}
	if err := dc.SendText(mustJSON(t, decline)); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	declineMsgId := ss.MsgId(decline["msgId"].(string))

	// The bot's line and the Server's amend — either order (two
	// goroutines), so both are correlated waits.
	waitBotMessage(t, probe, botId, "the declined line", isChatReply(declineMsgId, "declined"))
	waitBotMessage(t, probe, botId, "the rejected amend", isCallStatusAmend(inviteMsgId, callId, "rejected"))

	// The state machine reset: the next /play invites anew.
	chat("/play chiptune")
	waitBotMessage(t, probe, botId, "the fresh INVITE", func(m *rawMsg) bool {
		return isSipInvite(m) && m.sip.CallId != "" && m.sip.CallId != callId
	})
}

// playSongAndAnswer drives the outbound call flow up to the answered
// call: /play <song>, the INVITE, the 200 OK — returning the dialog's
// call id.
func playSongAndAnswer(t *testing.T, probe *wireProbe, dc *webrtc.DataChannel, botId, userId ss.SubscriberId, song string) string {
	t.Helper()
	m := baseMsg(ss.WellKnownChIdMain, userId, botId)
	m["plaintext"] = "/play " + song
	if err := dc.SendText(mustJSON(t, m)); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	invite := waitBotMessage(t, probe, botId, "the bot's INVITE", isSipInvite)
	callId := invite.sip.CallId
	if callId == "" {
		t.Fatalf("the INVITE = %+v carries no call id", invite)
	}
	answer := baseMsg(ss.WellKnownChIdMain, userId, botId)
	answer["mimeType"] = wireMimeSip
	answer["sip"] = map[string]any{"callId": callId, "response": map[string]any{"code": 200, "phrase": "OK"}}
	if err := dc.SendText(mustJSON(t, answer)); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	return callId
}

// TestMusicBotPlaysPCMSongAsOpus covers the linear PCM family, end to
// end through real media: /play on a 48 kHz stereo song phones the user,
// and the answered call's track carries opus (48000 Hz, stereo) — the
// song keeps its rate and channels, nothing is downmixed — with
// non-empty packets on the wire.
func TestMusicBotPlaysPCMSongAsOpus(t *testing.T) {
	requiresOpus(t)
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot := startBot(t, net, "bot", "2-bot", testSongbook(t))
	user, probe, tprobe := startUserProbe(t, net, "user", "1-user")
	botId, userId := pairUp(t, bot, user)
	dc := probe.waitDC(t, botId, msg_handler.DataChannelLabelMessages)

	playSongAndAnswer(t, probe, dc, botId, userId, "stereo")

	track := tprobe.waitTrack(t, 1)
	if track.Kind() != webrtc.RTPCodecTypeAudio {
		t.Fatalf("the track's kind is %s, want audio", track.Kind())
	}
	if got := track.Codec().MimeType; got != webrtc.MimeTypeOpus {
		t.Fatalf("the track's codec is %s, want %s", got, webrtc.MimeTypeOpus)
	}
	if got := track.Codec().ClockRate; got != 48000 {
		t.Fatalf("the track's clock rate is %d, want 48000", got)
	}
	// The music flows: several 20 ms opus packets, each non-empty.
	for i := 0; i < 3; i++ {
		if pkt := readOneRTP(t, track); len(pkt.Payload) == 0 {
			t.Fatalf("opus packet %d is empty", i)
		}
	}
}

// TestMusicBotPlaysFLACSong covers the compressed family: a flac
// source's song decodes lazily and rides the same opus path.
func TestMusicBotPlaysFLACSong(t *testing.T) {
	requiresOpus(t)
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot := startBot(t, net, "bot", "2-bot", testSongbook(t))
	user, probe, tprobe := startUserProbe(t, net, "user", "1-user")
	botId, userId := pairUp(t, bot, user)
	dc := probe.waitDC(t, botId, msg_handler.DataChannelLabelMessages)

	playSongAndAnswer(t, probe, dc, botId, userId, "flac")

	track := tprobe.waitTrack(t, 1)
	if got := track.Codec().MimeType; got != webrtc.MimeTypeOpus {
		t.Fatalf("the track's codec is %s, want %s (the flac decodes to linear pcm)", got, webrtc.MimeTypeOpus)
	}
	if pkt := readOneRTP(t, track); len(pkt.Payload) == 0 {
		t.Fatal("the opus packet is empty")
	}
}

// TestMusicBotSwitchesSongFamilies covers the mid-call switch across
// codec families: the track's codec follows the song, so the switch
// replaces the track on the wire — the PCMU track of the chiptune is
// succeeded by an opus track, and the music never leaves the wire.
func TestMusicBotSwitchesSongFamilies(t *testing.T) {
	requiresOpus(t)
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot := startBot(t, net, "bot", "2-bot", testSongbook(t))
	user, probe, tprobe := startUserProbe(t, net, "user", "1-user")
	botId, userId := pairUp(t, bot, user)
	dc := probe.waitDC(t, botId, msg_handler.DataChannelLabelMessages)

	playSongAndAnswer(t, probe, dc, botId, userId, "chiptune")
	pcmuTrack := tprobe.waitTrack(t, 1)
	if got := pcmuTrack.Codec().MimeType; got != webrtc.MimeTypePCMU {
		t.Fatalf("the first track's codec is %s, want %s", got, webrtc.MimeTypePCMU)
	}

	m := baseMsg(ss.WellKnownChIdMain, userId, botId)
	m["plaintext"] = "/play stereo"
	switchMsg := ss.MsgId(m["msgId"].(string))
	if err := dc.SendText(mustJSON(t, m)); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	waitBotMessage(t, probe, botId, "the now-playing line", isChatReply(switchMsg, "Now playing"))

	opusTrack := tprobe.waitTrack(t, 2)
	if got := opusTrack.Codec().MimeType; got != webrtc.MimeTypeOpus {
		t.Fatalf("the replaced track's codec is %s, want %s", got, webrtc.MimeTypeOpus)
	}
	if pkt := readOneRTP(t, opusTrack); len(pkt.Payload) == 0 {
		t.Fatal("the opus packet is empty")
	}
}

// TestMusicBotLoopsAShortSong covers the looping: a two-frame song
// (40 ms) keeps playing far beyond its own length — six packets are
// three times around the loop — because the player rewinds its stream
// at the end.
func TestMusicBotLoopsAShortSong(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot := startBot(t, net, "bot", "2-bot", []*audiosource.AudioSourceData{muLawSong("tiny", 2)})
	user, probe, tprobe := startUserProbe(t, net, "user", "1-user")
	botId, userId := pairUp(t, bot, user)
	dc := probe.waitDC(t, botId, msg_handler.DataChannelLabelMessages)

	callId := uuid.NewString()
	m := baseMsg(ss.WellKnownChIdMain, userId, botId)
	m["mimeType"] = wireMimeSip
	m["sip"] = map[string]any{"callId": callId, "method": "INVITE", "X-Media": "voice", "X-Call-Status": "inviting"}
	if err := dc.SendText(mustJSON(t, m)); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	waitBotMessage(t, probe, botId, "the 200 OK", isSipResponse(callId, 200))

	track := tprobe.waitTrack(t, 1)
	for i := 0; i < 6; i++ {
		if pkt := readOneRTP(t, track); len(pkt.Payload) != 160 {
			t.Fatalf("packet %d's payload is %d bytes, want 160 (20 ms of PCMU)", i, len(pkt.Payload))
		}
	}
}

// TestMusicBotEmptySongbook covers the degenerate wiring: with no song
// injected, a voice INVITE is left unanswered (there is no song to
// answer with) and /play answers with the unknown-song hint.
func TestMusicBotEmptySongbook(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot := startBot(t, net, "bot", "2-bot", nil)
	user, probe, tprobe := startUserProbe(t, net, "user", "1-user")
	botId, userId := pairUp(t, bot, user)
	dc := probe.waitDC(t, botId, msg_handler.DataChannelLabelMessages)

	callId := uuid.NewString()
	m := baseMsg(ss.WellKnownChIdMain, userId, botId)
	m["mimeType"] = wireMimeSip
	m["sip"] = map[string]any{"callId": callId, "method": "INVITE", "X-Media": "voice", "X-Call-Status": "inviting"}
	if err := dc.SendText(mustJSON(t, m)); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	for _, frame := range probe.textsFrom(botId) {
		if decoded := decodeMsgRaw(frame); decoded != nil && decoded.sip != nil && decoded.sip.CallId == callId {
			t.Fatalf("the bot answered a call with no song to play: %s", frame)
		}
	}
	tprobe.mu.Lock()
	tracks := len(tprobe.tracks)
	tprobe.mu.Unlock()
	if tracks != 0 {
		t.Fatalf("a songless bot offered %d tracks", tracks)
	}

	cmd := baseMsg(ss.WellKnownChIdMain, userId, botId)
	cmd["plaintext"] = "/play chiptune"
	if err := dc.SendText(mustJSON(t, cmd)); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	waitBotMessage(t, probe, botId, "the unknown-song answer",
		isChatReply(ss.MsgId(cmd["msgId"].(string)), "Unknown song"))
}
