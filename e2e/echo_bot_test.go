package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/pion/webrtc/v4"

	pkgauth "personal-site/pkg/auth"
	"personal-site/pkg/models/ss"
	"personal-site/pkg/rtc"
)

// The echo bot's configured identity in this suite's configuration
// documents. The subscriber id is deliberately lexicographically smaller
// than every id of the automatic assignment range (1000-1999): the bot is
// therefore the polite peer of any auto-assigned visitor and the one who
// creates the pair's data channels.
const (
	echoBotSubscriberId = "0-echobot"
	echoBotUsername     = "echo-bot-e2e"
)

// issueSessionToken signs a session token for sub with the given username
// claim, using the throwaway secret every e2e server runs with (see
// startServerOnAddr). The token is the client's whole identity: sub/jti
// become its signalling address, the username claim its registration's
// display name — the endpoint stamps it server-side.
func issueSessionToken(t *testing.T, sub, username string) string {
	t.Helper()
	issuer := pkgauth.NewStaticKeyJWTIssuer(
		pkgauth.NewStaticSecretProvider([]byte("personal-site-e2e-secret")), "e2e")
	token, err := issuer.IssueToken(context.Background(), jwt.MapClaims{
		"sub":      sub,
		"jti":      uuid.NewString(),
		"username": username,
		"exp":      time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("issue a session token for %s: %v", sub, err)
	}
	return token
}

// issueEchoBotJWT signs the bot's session token.
func issueEchoBotJWT(t *testing.T) string {
	t.Helper()
	return issueSessionToken(t, "bot:echo", echoBotUsername)
}

// startEchoBotServer launches the server with an <echoBot/> element in
// its configuration document, pointed at the server's own signalling
// endpoint with a freshly issued session token. The address is reserved
// first so the document can carry it; the fast intervals keep the test's
// discovery and reconnect latencies small.
func startEchoBotServer(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("release the reserved port: %v", err)
	}

	cfg := fmt.Sprintf(`<serverConfig>
  <echoBot url="ws://%s/api/ss/ws" jwt="%s"
           subscriberId="%s"
           keepAliveInterval="1s" memberListInterval="1s"
           replyTimeout="2s" reconnectInterval="1s" />
</serverConfig>
`, addr, issueEchoBotJWT(t), echoBotSubscriberId)
	path := filepath.Join(t.TempDir(), "serverConfig.xml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write the server configuration: %v", err)
	}
	return startServerOnAddr(t, addr, "--config-xml", path)
}

// waitFor polls cond until it holds or the timeout expires.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestSSEchoBotRegisters covers the wiring itself, end to end through the
// running server: the configured bot connects to the signalling endpoint,
// registers as a member of the main channel, and its profile carries the
// username of its token.
func TestSSEchoBotRegisters(t *testing.T) {
	baseURL := startEchoBotServer(t)

	alice := dialSS(t, baseURL)
	alice.register("e2e-bot-reg-alice", "", "alice")

	// The bot's first connection attempt can lose to the listener not
	// being up yet and retry after its reconnect interval, so poll the
	// member list until it shows up.
	found := false
	for round := 0; round < 100 && !found; round++ {
		msgId := fmt.Sprintf("e2e-bot-list-%d", round)
		alice.send(&signallingEventView{
			To:    epAddrView{ServiceId: ssServiceId},
			MsgId: msgId,
			C2SEv: &clientToSSEvView{ListChannelMembers: &listChannelMembersView{ChannelId: ssMainChannelId}},
		})
		// The bot offers a session to every fellow member the moment it
		// discovers one: its negotiation events can be relayed to alice
		// while she polls, so the list pages are awaited by their
		// correlation to the request.
		var members []string
		for {
			reply := alice.recvReply(msgId)
			if reply.S2CEv == nil || reply.S2CEv.ChannelMbsListResult == nil {
				t.Fatalf("reply = %+v, want a channelMbsListResult", reply)
			}
			members = append(members, reply.S2CEv.ChannelMbsListResult.Members...)
			if !reply.S2CEv.ChannelMbsListResult.HasMore {
				break
			}
		}
		found = slices.Contains(members, echoBotSubscriberId)
		if !found {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if !found {
		t.Fatal("the echo bot never appeared in the main channel's member list")
	}

	// Its profile resolves to the configured subscriber id and the
	// username claim of its token.
	alice.send(&signallingEventView{
		To:    epAddrView{ServiceId: ssServiceId},
		MsgId: "e2e-bot-profile",
		C2SEv: &clientToSSEvView{UserProfileQuery: &userProfileQueryView{
			SubscriberId: echoBotSubscriberId,
			ChannelId:    ssMainChannelId,
		}},
	})
	reply := alice.recvReply("e2e-bot-profile")
	if reply.S2CEv == nil || reply.S2CEv.Err != nil || reply.S2CEv.Profile == nil {
		t.Fatalf("profile reply = %+v, want a profile and no error", reply)
	}
	if p := reply.S2CEv.Profile; p.SubscriberId != echoBotSubscriberId || p.Username != echoBotUsername {
		t.Errorf("profile = %+v, want the bot (%s, %s)", p, echoBotSubscriberId, echoBotUsername)
	}
}

// dcMsgView is the test's own view of a DCMsg frame of the messaging data
// channel — deliberately not shared with any implementation, so
// accidental changes to the wire format fail here.
type dcMsgView struct {
	MimeVersion      string `json:"mimeVersion"`
	ChannelId        string `json:"channelId"`
	FromSubscriberId string `json:"fromSubscriberId"`
	ToSubscriberId   string `json:"toSubscriberId"`
	MsgId            string `json:"msgId"`
	InReplyTo        string `json:"inReplyTo"`
	Echo             bool   `json:"echo"`
	MimeType         string `json:"mimeType"`
	Plaintext        string `json:"plaintext"`
}

// TestSSEchoBotEcho drives a full WebRTC session between a probe client
// (a pkg/rtc HeadlessRTCClient over the real WebSocket endpoint, exactly
// like a browser) and the running server's built-in echo bot: the bot
// brokers and establishes the session, opens the pair's messaging
// channel, and does its echo dance with a plain-text chat line — the
// verbatim bounce with echo set, then a fresh reply of identical content
// inReplyTo the original.
func TestSSEchoBotEcho(t *testing.T) {
	baseURL := startEchoBotServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// The probe's messaging-channel state: the first open channel and
	// every frame it receives.
	opened := make(chan *webrtc.DataChannel, 1)
	frames := make(chan []byte, 16)

	client, err := rtc.NewHeadlessRTCClient(rtc.PerfectNegotiatorFactory, rtc.RTCClientConfiguration{
		MemberListInterval: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("construct the probe client: %v", err)
	}
	client.HandleDataChannelFunc("dcmsg", func(_ context.Context, _ ss.ChannelId, _ ss.SubscriberId, dc *webrtc.DataChannel) {
		dc.OnOpen(func() {
			select {
			case opened <- dc:
			default:
			}
		})
		dc.OnMessage(func(m webrtc.DataChannelMessage) {
			if !m.IsString {
				return
			}
			data := append([]byte(nil), m.Data...)
			select {
			case frames <- data:
			case <-ctx.Done():
			}
		})
	})

	jwtCookie := loginAsVisitor(t, baseURL+"/api/login/visitor")
	if jwtCookie == "" {
		t.Fatal("visitor login did not set a jwt cookie")
	}
	transport := &rtc.WebSocketSignallingTransport{
		URL:    "ws" + strings.TrimPrefix(baseURL, "http") + "/api/ss/ws",
		Header: http.Header{"Cookie": []string{"jwt=" + jwtCookie}},
	}
	// The channel pair between the client and the transport; see
	// rtc.SignallingTransport.
	toSS := make(chan *ss.SignallingEvent, 64)
	fromSS := make(chan *ss.SignallingEvent, 64)
	go func() {
		if err := transport.Run(ctx, toSS, fromSS); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("signalling transport ended: %v", err)
		}
	}()
	go func() {
		if err := client.Run(ctx, fromSS, toSS); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("signalling client ended: %v", err)
		}
	}()

	// The probe discovers the bot (its own listing), the bot discovers
	// the probe (its listing), and — the polite peer — brings the
	// messaging channel up.
	waitFor(t, 20*time.Second, "the probe to discover the echo bot", func() bool {
		return slices.Contains(client.Peers(), echoBotSubscriberId)
	})
	var dc *webrtc.DataChannel
	select {
	case dc = <-opened:
	case <-time.After(20 * time.Second):
		t.Fatal("the messaging data channel did not open")
	}
	self := client.SubscriberId()
	if self == "" {
		t.Fatal("the probe is not registered")
	}

	// Send a plain-text chat line to the bot as a hand-built wire frame.
	msgId := "e2e-bot-msg-1"
	frame := fmt.Sprintf(`{"mimeVersion":"1.0","channelId":%q,"fromSubscriberId":%q,"toSubscriberId":%q,"creationTimestamp":%d,"msgId":%q,"mimeType":"text/plain","plaintext":%q}`,
		ssMainChannelId, self, echoBotSubscriberId, time.Now().Unix(), msgId, "hello, bot")
	if err := dc.SendText(frame); err != nil {
		t.Fatalf("send the chat line: %v", err)
	}

	next := func() *dcMsgView {
		t.Helper()
		select {
		case data := <-frames:
			var msg dcMsgView
			if err := json.Unmarshal(data, &msg); err != nil {
				t.Fatalf("decode the frame %q: %v", data, err)
			}
			return &msg
		case <-time.After(15 * time.Second):
			t.Fatal("timed out waiting for a dcmsg frame")
			return nil
		}
	}

	// The protocol's echo: the same frame bounced back with echo set —
	// the channel is ordered, so the bounce precedes the reply.
	bounce := next()
	if !bounce.Echo || bounce.MsgId != msgId || bounce.Plaintext != "hello, bot" {
		t.Errorf("bounce = %+v, want the sent frame with echo set", bounce)
	}
	if bounce.FromSubscriberId != string(self) || bounce.ToSubscriberId != echoBotSubscriberId {
		t.Errorf("bounce addressing = %s -> %s, want %s -> %s",
			bounce.FromSubscriberId, bounce.ToSubscriberId, self, echoBotSubscriberId)
	}

	// ...and the echo purpose: a fresh message with identical content,
	// inReplyTo the original.
	reply := next()
	if reply.Echo {
		t.Errorf("reply = %+v, want a fresh message (echo unset)", reply)
	}
	if reply.MimeType != "text/plain" || reply.Plaintext != "hello, bot" {
		t.Errorf("reply = %+v, want an identical text/plain line", reply)
	}
	if reply.InReplyTo != msgId {
		t.Errorf("reply inReplyTo = %q, want %q", reply.InReplyTo, msgId)
	}
	if reply.MsgId == "" || reply.MsgId == msgId {
		t.Errorf("reply msgId = %q, want a fresh one (the original was %q)", reply.MsgId, msgId)
	}
	if reply.FromSubscriberId != echoBotSubscriberId || reply.ToSubscriberId != string(self) {
		t.Errorf("reply addressing = %s -> %s, want %s -> %s",
			reply.FromSubscriberId, reply.ToSubscriberId, echoBotSubscriberId, self)
	}
}
