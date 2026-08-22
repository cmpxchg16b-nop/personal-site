package e2e

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// The test's own views of the signalling wire format — deliberately not
// shared with the server-side types (pkg/models/ss), so that accidental
// changes to the wire format fail here. They mirror the prototype in
// web/site/src/api/ss/types.ts.

const (
	// ssServiceId is the well-known service id of the signalling server.
	ssServiceId = "253dc952-a55b-4d01-a885-c5e240e95fdb"
	// ssMainChannelId is the well-known channel id of the main channel.
	ssMainChannelId = "f887f5b0-7b78-4ceb-a051-f42879f9d98e"
)

// The well-known signalling error codes.
const (
	ssErrSubscriberIdIsRegistered  = 1
	ssErrSubscriberNotFound        = 2
	ssErrChannelNotFound           = 3
	ssErrUsernameTaken             = 4
	ssErrNoSubscriberIdAvailable   = 5
)

type epAddrView struct {
	UserId        string `json:"userId,omitempty"`
	UserSessionId string `json:"userSessionId,omitempty"`
	ServiceId     string `json:"serviceId,omitempty"`
}

type registerEvView struct {
	SubscriberId string `json:"subscriberId"`
	ChannelId    string `json:"channelId"`
	Username     string `json:"username"`
}

type userProfileQueryView struct {
	SubscriberId string `json:"subscriberId"`
	ChannelId    string `json:"channelId"`
}

type listChannelMembersView struct {
	ChannelId string `json:"channelId"`
}

type listChannelsView struct{}

type channelProfileQueryView struct {
	ChannelId string `json:"channelId"`
}

type clientToSSEvView struct {
	Register            *registerEvView          `json:"register,omitempty"`
	UserProfileQuery    *userProfileQueryView    `json:"userProfileQuery,omitempty"`
	ChannelProfileQuery *channelProfileQueryView `json:"channelProfileQuery,omitempty"`
	Ping                *pingPongView            `json:"ping,omitempty"`
	ListChannelMembers  *listChannelMembersView  `json:"listChannelMembers,omitempty"`
	ListChannels        *listChannelsView        `json:"listChannels,omitempty"`
}

type ssErrView struct {
	ErrorCode int    `json:"errorCode"`
	ErrorMsg  string `json:"errorMsg"`
}

type userProfileView struct {
	SubscriberId string `json:"subscriberId"`
	ChannelId    string `json:"channelId"`
	Username     string `json:"username"`
}

type registerResultView struct {
	ChannelId    string `json:"channelId"`
	SubscriberId string `json:"subscriberId"`
}

type channelMbsListResultView struct {
	ChannelId string   `json:"channelId"`
	Members   []string `json:"members"`
	HasMore   bool     `json:"hasMore"`
}

type channelProfileView struct {
	ChannelId   string `json:"channelId"`
	ChannelName string `json:"channelName"`
}

type channelListResultView struct {
	Channels []string `json:"channels"`
	HasMore  bool     `json:"hasMore"`
}

type ssToClientEvView struct {
	Err                  *ssErrView                `json:"err,omitempty"`
	RegisterResult       *registerResultView       `json:"registerResult,omitempty"`
	Profile              *userProfileView          `json:"profile,omitempty"`
	ChannelProfile       *channelProfileView       `json:"channelProfile,omitempty"`
	Pong                 *pingPongView             `json:"pong,omitempty"`
	ChannelMbsListResult *channelMbsListResultView `json:"channelMbsListResult,omitempty"`
	ChannelListResult    *channelListResultView    `json:"channelListResult,omitempty"`
}

type pingPongView struct {
	PingId            string `json:"pingId"`
	SequenceNumber    uint64 `json:"sequenceNumber"`
	AckSequenceNumber uint64 `json:"ackSequenceNumber"`
}

type sessionDescView struct {
	Type string `json:"type"`
	SDP  string `json:"sdp"`
}

type iceCandidateView struct {
	Candidate        string  `json:"candidate"`
	SDPMid           *string `json:"sdpMid,omitempty"`
	SDPMLineIndex    *uint16 `json:"sdpMLineIndex,omitempty"`
	UsernameFragment string  `json:"usernameFragment,omitempty"`
}

type clientToClientEvView struct {
	FromSubscriber  string            `json:"fromSubscriber"`
	ToSubscriber    string            `json:"toSubscriber"`
	ChannelId       string            `json:"channelId"`
	SessionDesc     *sessionDescView  `json:"sessionDesc,omitempty"`
	RTCICECandidate *iceCandidateView `json:"rtcICECandidate,omitempty"`
	Ping            *pingPongView     `json:"ping,omitempty"`
	Pong            *pingPongView     `json:"pong,omitempty"`
}

type signallingEventView struct {
	From      epAddrView            `json:"from"`
	To        epAddrView            `json:"to"`
	MsgId     string                `json:"msgId"`
	InReplyTo string                `json:"inReplyTo,omitempty"`
	C2SEv     *clientToSSEvView     `json:"c2SEv,omitempty"`
	S2CEv     *ssToClientEvView     `json:"s2CEv,omitempty"`
	C2CEv     *clientToClientEvView `json:"c2CEv,omitempty"`
}

// ssWSClient is a websocket client of the signalling endpoint, speaking
// the view types above. Its connection identity (userId, sessionId) is the
// visitor session's (subject id, session id), assigned by the server — the
// jwtCookie is what the connection authenticates with.
type ssWSClient struct {
	t         *testing.T
	conn      *websocket.Conn
	baseURL   string
	jwtCookie string
	userId    string
	sessionId string
}

// dialSS logs in as a fresh visitor and connects to the signalling
// endpoint of the server at baseURL with the session cookie. Each call
// produces a distinct identity (a fresh visitor session).
func dialSS(t *testing.T, baseURL string) *ssWSClient {
	t.Helper()
	jwtCookie := loginAsVisitor(t, baseURL+"/api/login/visitor")
	if jwtCookie == "" {
		t.Fatal("visitor login did not set a jwt cookie")
	}
	c := &ssWSClient{t: t, baseURL: baseURL, jwtCookie: jwtCookie}
	c.userId, c.sessionId, _ = profileIdentity(t, baseURL, jwtCookie)
	c.redial()
	t.Cleanup(func() { c.conn.Close() })
	return c
}

// redial (re)connects the client with its session cookie: the new
// connection carries the same identity as the old one.
func (c *ssWSClient) redial() {
	c.t.Helper()
	header := http.Header{}
	header.Set("Cookie", "jwt="+c.jwtCookie)
	url := "ws" + strings.TrimPrefix(c.baseURL, "http") + "/api/ss/ws"
	conn, _, err := websocket.DefaultDialer.Dial(url, header)
	if err != nil {
		c.t.Fatalf("dial %s as (%s, %s): %v", url, c.userId, c.sessionId, err)
	}
	if c.conn != nil {
		c.conn.Close()
	}
	c.conn = conn
}

func (c *ssWSClient) send(ev *signallingEventView) {
	c.t.Helper()
	data, err := json.Marshal(ev)
	if err != nil {
		c.t.Fatalf("marshal event: %v", err)
	}
	if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		c.t.Fatalf("write event: %v", err)
	}
}

func (c *ssWSClient) recv() *signallingEventView {
	c.t.Helper()
	c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, data, err := c.conn.ReadMessage()
	if err != nil {
		c.t.Fatalf("read event: %v", err)
	}
	var ev signallingEventView
	if err := json.Unmarshal(data, &ev); err != nil {
		c.t.Fatalf("unmarshal event %q: %v", data, err)
	}
	return &ev
}

// expectSilent asserts that nothing arrives within a short window. A read
// timeout corrupts a gorilla connection, so this must be the last
// operation on the client.
func (c *ssWSClient) expectSilent() {
	c.t.Helper()
	c.conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, _, err := c.conn.ReadMessage(); err == nil {
		c.t.Fatal("unexpected event")
	}
}

// register registers sub with username and consumes the ack, which must
// be error-free, correlated with the request, and carry the matching
// registerResult. An empty sub asks the server to assign a subscriber id
// from the automatic assignment range; the assigned id is returned.
func (c *ssWSClient) register(msgId, sub, username string) *signallingEventView {
	c.t.Helper()
	c.send(&signallingEventView{
		MsgId: msgId,
		C2SEv: &clientToSSEvView{Register: &registerEvView{
			SubscriberId: sub,
			ChannelId:    ssMainChannelId,
			Username:     username,
		}},
	})
	ack := c.recv()
	if ack.S2CEv == nil {
		c.t.Fatalf("register ack carries no s2CEv: %+v", ack)
	}
	if ack.S2CEv.Err != nil {
		c.t.Fatalf("register ack carries an error: %+v", ack.S2CEv.Err)
	}
	if ack.InReplyTo != msgId {
		c.t.Fatalf("register ack inReplyTo = %q, want %q", ack.InReplyTo, msgId)
	}
	if ack.S2CEv.RegisterResult == nil {
		c.t.Fatalf("register ack carries no registerResult: %+v", ack.S2CEv)
	}
	if ack.S2CEv.RegisterResult.ChannelId != ssMainChannelId {
		c.t.Fatalf("registerResult channelId = %q, want %q", ack.S2CEv.RegisterResult.ChannelId, ssMainChannelId)
	}
	if sub != "" && ack.S2CEv.RegisterResult.SubscriberId != sub {
		c.t.Fatalf("registerResult subscriberId = %q, want %q", ack.S2CEv.RegisterResult.SubscriberId, sub)
	}
	return ack
}

// registerExpectingError registers and consumes the reply, which must be
// an error reply with the given well-known code.
func (c *ssWSClient) registerExpectingError(msgId, sub, channelId, username string, wantCode int) {
	c.t.Helper()
	c.send(&signallingEventView{
		MsgId: msgId,
		C2SEv: &clientToSSEvView{Register: &registerEvView{
			SubscriberId: sub,
			ChannelId:    channelId,
			Username:     username,
		}},
	})
	reply := c.recv()
	if reply.S2CEv == nil || reply.S2CEv.Err == nil {
		c.t.Fatalf("register reply = %+v, want an s2CEv error", reply)
	}
	if reply.S2CEv.Err.ErrorCode != wantCode {
		c.t.Fatalf("register error code = %d, want %d (%s)", reply.S2CEv.Err.ErrorCode, wantCode, reply.S2CEv.Err.ErrorMsg)
	}
	if reply.InReplyTo != msgId {
		c.t.Fatalf("register error reply inReplyTo = %q, want %q", reply.InReplyTo, msgId)
	}
}

// TestSSRegisterAndProfile covers registration (success and the
// well-known error codes) and the user profile query, end to end through
// the running server.
func TestSSRegisterAndProfile(t *testing.T) {
	baseURL := startServer(t)

	alice := dialSS(t, baseURL)

	// A successful registration is acknowledged by the SS service, To the
	// address the handler populated from the caller's session.
	ack := alice.register("e2e-reg-1", "alice", "alice")
	if ack.From.ServiceId != ssServiceId {
		t.Errorf("ack from.serviceId = %q, want %q", ack.From.ServiceId, ssServiceId)
	}
	if ack.To.UserId != alice.userId || ack.To.UserSessionId != alice.sessionId {
		t.Errorf("ack to = %+v, want alice's session identity %s/%s", ack.To, alice.userId, alice.sessionId)
	}
	if ack.MsgId == "" || ack.MsgId == "e2e-reg-1" {
		t.Errorf("ack msgId = %q, want a fresh, distinct id", ack.MsgId)
	}

	// The profile of a registered subscriber reads back.
	alice.send(&signallingEventView{
		MsgId: "e2e-query-1",
		C2SEv: &clientToSSEvView{UserProfileQuery: &userProfileQueryView{
			SubscriberId: "alice",
			ChannelId:    ssMainChannelId,
		}},
	})
	reply := alice.recv()
	if reply.S2CEv == nil || reply.S2CEv.Err != nil || reply.S2CEv.Profile == nil {
		t.Fatalf("profile reply = %+v, want a profile and no error", reply)
	}
	profile := reply.S2CEv.Profile
	if profile.SubscriberId != "alice" || profile.ChannelId != ssMainChannelId || profile.Username != "alice" {
		t.Errorf("profile = %+v, want alice in the main channel", profile)
	}
	if reply.InReplyTo != "e2e-query-1" {
		t.Errorf("profile reply inReplyTo = %q, want %q", reply.InReplyTo, "e2e-query-1")
	}

	// Querying an unknown subscriber answers SubscriberNotFound.
	alice.send(&signallingEventView{
		MsgId: "e2e-query-2",
		C2SEv: &clientToSSEvView{UserProfileQuery: &userProfileQueryView{
			SubscriberId: "nobody",
			ChannelId:    ssMainChannelId,
		}},
	})
	reply = alice.recv()
	if reply.S2CEv == nil || reply.S2CEv.Err == nil || reply.S2CEv.Err.ErrorCode != ssErrSubscriberNotFound {
		t.Errorf("unknown subscriber reply = %+v, want error code %d", reply, ssErrSubscriberNotFound)
	}

	// Re-registering a live subscriber id from the same (user, session,
	// channel) tuple is a refresh, not an error...
	alice.register("e2e-reg-2", "alice", "alice")

	// ...while another identity cannot take the id over, a taken username
	// is rejected, and an unknown channel is rejected — each with its
	// well-known code.
	bob := dialSS(t, baseURL)
	bob.registerExpectingError("e2e-reg-3", "alice", ssMainChannelId, "bob", ssErrSubscriberIdIsRegistered)
	bob.registerExpectingError("e2e-reg-4", "bob", ssMainChannelId, "alice", ssErrUsernameTaken)
	bob.registerExpectingError("e2e-reg-5", "bob", "no-such-channel", "bob", ssErrChannelNotFound)

	alice.expectSilent()
	bob.expectSilent()
}

// TestSSRegisterAutoAssign covers automatic subscriber id assignment: a
// registration with an empty subscriber id is assigned the next free id
// from the 1000-1999 range, echoed in the registerResult, and the
// assigned id is a working registration.
func TestSSRegisterAutoAssign(t *testing.T) {
	baseURL := startServer(t)

	alice := dialSS(t, baseURL)
	bob := dialSS(t, baseURL)

	ack := alice.register("e2e-auto-1", "", "auto-alice")
	if got := ack.S2CEv.RegisterResult.SubscriberId; got != "1000" {
		t.Fatalf("first auto-assigned subscriber id = %q, want %q", got, "1000")
	}
	ack = bob.register("e2e-auto-2", "", "auto-bob")
	if got := ack.S2CEv.RegisterResult.SubscriberId; got != "1001" {
		t.Fatalf("second auto-assigned subscriber id = %q, want %q", got, "1001")
	}

	// The assigned id is a working, queryable registration.
	alice.send(&signallingEventView{
		MsgId: "e2e-auto-query",
		C2SEv: &clientToSSEvView{UserProfileQuery: &userProfileQueryView{
			SubscriberId: "1000",
			ChannelId:    ssMainChannelId,
		}},
	})
	reply := alice.recv()
	if reply.S2CEv == nil || reply.S2CEv.Profile == nil || reply.S2CEv.Profile.Username != "auto-alice" {
		t.Fatalf("profile of auto-assigned id = %+v, want username auto-alice", reply)
	}
}

// TestSSPingPongRelay drives an out-of-band ping session between two
// clients through the SS relay, following the prototype's sequence rules:
// a pong keeps the ping id and answers ack = seq + 1, and the next ping's
// seq is the ack of the last reply.
func TestSSPingPongRelay(t *testing.T) {
	baseURL := startServer(t)

	alice := dialSS(t, baseURL)
	bob := dialSS(t, baseURL)
	carol := dialSS(t, baseURL)
	alice.register("e2e-reg-alice", "alice", "alice")
	bob.register("e2e-reg-bob", "bob", "bob")
	carol.register("e2e-reg-carol", "carol", "carol")

	pingId := "e2e-ping-1"
	seq := uint64(1)
	bobSeq := uint64(0) // bob's own sequence space, not asserted
	for round := 1; round <= 3; round++ {
		// alice pings bob, addressing the SS; the SS translates
		// (channel, subscriber) into bob's endpoint address.
		alice.send(&signallingEventView{
			To:    epAddrView{ServiceId: ssServiceId},
			MsgId: "e2e-ping-" + string(rune('0'+round)),
			C2CEv: &clientToClientEvView{
				FromSubscriber: "alice",
				ToSubscriber:   "bob",
				ChannelId:      ssMainChannelId,
				Ping:           &pingPongView{PingId: pingId, SequenceNumber: seq},
			},
		})

		// bob receives the ping unchanged, with From carrying alice's
		// session identity and To translated to bob's address.
		ping := bob.recv()
		if ping.C2CEv == nil || ping.C2CEv.Ping == nil {
			t.Fatalf("round %d: bob got %+v, want a ping", round, ping)
		}
		if got := ping.C2CEv.Ping.SequenceNumber; got != seq {
			t.Fatalf("round %d: ping seq = %d, want %d", round, got, seq)
		}
		if ping.C2CEv.Ping.PingId != pingId {
			t.Errorf("round %d: ping id = %q, want %q", round, ping.C2CEv.Ping.PingId, pingId)
		}
		if ping.From.UserId != alice.userId || ping.From.UserSessionId != alice.sessionId {
			t.Errorf("round %d: ping from = %+v, want alice's session identity", round, ping.From)
		}
		if ping.To.UserId != bob.userId || ping.To.UserSessionId != bob.sessionId {
			t.Errorf("round %d: ping to = %+v, want bob's session identity", round, ping.To)
		}

		// bob pongs: keep the ping id, ack = seq + 1.
		bobSeq++
		bob.send(&signallingEventView{
			To:    epAddrView{ServiceId: ssServiceId},
			MsgId: "e2e-pong-" + string(rune('0'+round)),
			C2CEv: &clientToClientEvView{
				FromSubscriber: "bob",
				ToSubscriber:   "alice",
				ChannelId:      ssMainChannelId,
				Pong: &pingPongView{
					PingId:            pingId,
					SequenceNumber:    bobSeq,
					AckSequenceNumber: seq + 1,
				},
			},
		})

		// alice receives the pong; the ack of the last reply becomes the
		// seq of the next ping.
		pong := alice.recv()
		if pong.C2CEv == nil || pong.C2CEv.Pong == nil {
			t.Fatalf("round %d: alice got %+v, want a pong", round, pong)
		}
		if got := pong.C2CEv.Pong.AckSequenceNumber; got != seq+1 {
			t.Fatalf("round %d: pong ack = %d, want %d", round, got, seq+1)
		}
		if pong.C2CEv.Pong.PingId != pingId {
			t.Errorf("round %d: pong ping id = %q, want %q", round, pong.C2CEv.Pong.PingId, pingId)
		}
		if pong.To.UserId != alice.userId || pong.To.UserSessionId != alice.sessionId {
			t.Errorf("round %d: pong to = %+v, want alice's session identity", round, pong.To)
		}
		seq = pong.C2CEv.Pong.AckSequenceNumber
	}
	if seq != 4 {
		t.Errorf("seq after 3 rounds = %d, want 4", seq)
	}

	// Relays are unicast: carol saw none of it. (Silence checks last: a
	// read timeout corrupts a gorilla connection.)
	carol.expectSilent()
	bob.expectSilent()
	alice.expectSilent()
}

// TestSSRelayRTCData checks that WebRTC payloads — a session description
// and a trickle ICE candidate — pass through the relay unchanged.
func TestSSRelayRTCData(t *testing.T) {
	baseURL := startServer(t)

	alice := dialSS(t, baseURL)
	bob := dialSS(t, baseURL)
	alice.register("e2e-reg-alice", "alice", "alice")
	bob.register("e2e-reg-bob", "bob", "bob")

	sdp := "v=0\r\no=- 42 1 IN IP4 0.0.0.0\r\ns=-\r\n"
	alice.send(&signallingEventView{
		To:    epAddrView{ServiceId: ssServiceId},
		MsgId: "e2e-offer-1",
		C2CEv: &clientToClientEvView{
			FromSubscriber: "alice",
			ToSubscriber:   "bob",
			ChannelId:      ssMainChannelId,
			SessionDesc:    &sessionDescView{Type: "offer", SDP: sdp},
		},
	})
	offer := bob.recv()
	if offer.C2CEv == nil || offer.C2CEv.SessionDesc == nil {
		t.Fatalf("bob got %+v, want a session description", offer)
	}
	if offer.C2CEv.SessionDesc.Type != "offer" || offer.C2CEv.SessionDesc.SDP != sdp {
		t.Errorf("session desc = %+v, want the offer passed through unchanged", offer.C2CEv.SessionDesc)
	}

	sdpMid := "0"
	sdpMLineIndex := uint16(0)
	candidate := "candidate:1 1 udp 2130706431 192.0.2.1 9 typ host"
	bob.send(&signallingEventView{
		To:    epAddrView{ServiceId: ssServiceId},
		MsgId: "e2e-ice-1",
		C2CEv: &clientToClientEvView{
			FromSubscriber: "bob",
			ToSubscriber:   "alice",
			ChannelId:      ssMainChannelId,
			RTCICECandidate: &iceCandidateView{
				Candidate:     candidate,
				SDPMid:        &sdpMid,
				SDPMLineIndex: &sdpMLineIndex,
			},
		},
	})
	ice := alice.recv()
	if ice.C2CEv == nil || ice.C2CEv.RTCICECandidate == nil {
		t.Fatalf("alice got %+v, want an ICE candidate", ice)
	}
	got := ice.C2CEv.RTCICECandidate
	if got.Candidate != candidate || got.SDPMid == nil || *got.SDPMid != sdpMid ||
		got.SDPMLineIndex == nil || *got.SDPMLineIndex != sdpMLineIndex {
		t.Errorf("ICE candidate = %+v, want it passed through unchanged", got)
	}

	bob.expectSilent()
	alice.expectSilent()
}

// TestSSListChannelMembers covers the paginated channel members list:
// the SS answers one request with one or more channelMbsListResult
// messages, all correlated via inReplyTo, ending with hasMore=false.
func TestSSListChannelMembers(t *testing.T) {
	baseURL := startServer(t)

	alice := dialSS(t, baseURL)
	bob := dialSS(t, baseURL)
	carol := dialSS(t, baseURL)
	alice.register("e2e-reg-alice", "alice", "alice")
	bob.register("e2e-reg-bob", "bob", "bob")
	carol.register("e2e-reg-carol", "carol", "carol")

	// The member list of the main channel fits one page: a single final
	// page carrying the subscriber ids.
	alice.send(&signallingEventView{
		To:    epAddrView{ServiceId: ssServiceId},
		MsgId: "e2e-list-1",
		C2SEv: &clientToSSEvView{ListChannelMembers: &listChannelMembersView{ChannelId: ssMainChannelId}},
	})
	var members []string
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("too many channelMbsListResult pages for a 3-member channel")
		}
		reply := alice.recv()
		if reply.S2CEv == nil || reply.S2CEv.ChannelMbsListResult == nil {
			t.Fatalf("reply = %+v, want a channelMbsListResult", reply)
		}
		if reply.S2CEv.Err != nil {
			t.Fatalf("page carries an error: %+v", reply.S2CEv.Err)
		}
		if reply.InReplyTo != "e2e-list-1" {
			t.Errorf("page inReplyTo = %q, want %q", reply.InReplyTo, "e2e-list-1")
		}
		if reply.From.ServiceId != ssServiceId {
			t.Errorf("page from.serviceId = %q, want %q", reply.From.ServiceId, ssServiceId)
		}
		if reply.To.UserId != alice.userId || reply.To.UserSessionId != alice.sessionId {
			t.Errorf("page to = %+v, want alice's session identity", reply.To)
		}
		page := reply.S2CEv.ChannelMbsListResult
		if page.ChannelId != ssMainChannelId {
			t.Errorf("page channelId = %q, want %q", page.ChannelId, ssMainChannelId)
		}
		members = append(members, page.Members...)
		if !page.HasMore {
			break
		}
	}
	sort.Strings(members)
	if want := []string{"alice", "bob", "carol"}; !slices.Equal(members, want) {
		t.Errorf("members = %v, want %v", members, want)
	}

	// An unknown channel is rejected with ChannelNotFound.
	alice.send(&signallingEventView{
		To:    epAddrView{ServiceId: ssServiceId},
		MsgId: "e2e-list-2",
		C2SEv: &clientToSSEvView{ListChannelMembers: &listChannelMembersView{ChannelId: "no-such-channel"}},
	})
	reply := alice.recv()
	if reply.S2CEv == nil || reply.S2CEv.Err == nil || reply.S2CEv.Err.ErrorCode != ssErrChannelNotFound {
		t.Errorf("unknown channel reply = %+v, want error code %d", reply, ssErrChannelNotFound)
	}
	if reply.InReplyTo != "e2e-list-2" {
		t.Errorf("error reply inReplyTo = %q, want %q", reply.InReplyTo, "e2e-list-2")
	}

	alice.expectSilent()
	bob.expectSilent()
	carol.expectSilent()
}

// TestSSListChannelsAndProfile covers dynamic channel discovery: the
// channel list comes back as one final page (only the main channel
// exists) and a channel profile query resolves its display name. No
// registration is needed.
func TestSSListChannelsAndProfile(t *testing.T) {
	baseURL := startServer(t)

	alice := dialSS(t, baseURL)

	// Deliberately before any registration: discovery needs none.
	alice.send(&signallingEventView{
		To:    epAddrView{ServiceId: ssServiceId},
		MsgId: "e2e-list-channels",
		C2SEv: &clientToSSEvView{ListChannels: &listChannelsView{}},
	})
	list := alice.recv()
	if list.S2CEv == nil || list.S2CEv.ChannelListResult == nil {
		t.Fatalf("list reply = %+v, want a channelListResult", list)
	}
	if list.S2CEv.Err != nil {
		t.Fatalf("list reply carries an error: %+v", list.S2CEv.Err)
	}
	if list.InReplyTo != "e2e-list-channels" {
		t.Errorf("list inReplyTo = %q, want %q", list.InReplyTo, "e2e-list-channels")
	}
	page := list.S2CEv.ChannelListResult
	if page.HasMore || len(page.Channels) != 1 || page.Channels[0] != ssMainChannelId {
		t.Fatalf("channel list page = %+v, want a single final page holding the main channel", page)
	}

	alice.send(&signallingEventView{
		To:    epAddrView{ServiceId: ssServiceId},
		MsgId: "e2e-ch-profile",
		C2SEv: &clientToSSEvView{ChannelProfileQuery: &channelProfileQueryView{ChannelId: ssMainChannelId}},
	})
	profile := alice.recv()
	if profile.S2CEv == nil || profile.S2CEv.ChannelProfile == nil {
		t.Fatalf("profile reply = %+v, want a channelProfile", profile)
	}
	if got := profile.S2CEv.ChannelProfile; got.ChannelId != ssMainChannelId || got.ChannelName != "main" {
		t.Errorf("channel profile = %+v, want the main channel named %q", got, "main")
	}

	alice.send(&signallingEventView{
		To:    epAddrView{ServiceId: ssServiceId},
		MsgId: "e2e-ch-profile-unknown",
		C2SEv: &clientToSSEvView{ChannelProfileQuery: &channelProfileQueryView{ChannelId: "no-such-channel"}},
	})
	errReply := alice.recv()
	if errReply.S2CEv == nil || errReply.S2CEv.Err == nil || errReply.S2CEv.Err.ErrorCode != ssErrChannelNotFound {
		t.Fatalf("unknown channel reply = %+v, want error code %d", errReply, ssErrChannelNotFound)
	}

	alice.expectSilent()
}

// TestSSClientServerPing covers the c2s ping / s2c pong liveness
// exchange with the SS itself: it works before any registration, the
// pong keeps the ping id and answers ack = seq + 1, and the next ping's
// seq is the ack of the last reply.
func TestSSClientServerPing(t *testing.T) {
	baseURL := startServer(t)

	alice := dialSS(t, baseURL)

	// Deliberately before any registration.
	seq := uint64(1)
	for round := 1; round <= 3; round++ {
		msgId := "e2e-ss-ping-" + string(rune('0'+round))
		alice.send(&signallingEventView{
			To:    epAddrView{ServiceId: ssServiceId},
			MsgId: msgId,
			C2SEv: &clientToSSEvView{Ping: &pingPongView{PingId: "e2e-liveness", SequenceNumber: seq}},
		})
		pong := alice.recv()
		if pong.S2CEv == nil || pong.S2CEv.Pong == nil {
			t.Fatalf("round %d: reply = %+v, want an s2c pong", round, pong)
		}
		if pong.S2CEv.Err != nil {
			t.Fatalf("round %d: pong carries an error: %+v", round, pong.S2CEv.Err)
		}
		if got := pong.S2CEv.Pong.AckSequenceNumber; got != seq+1 {
			t.Fatalf("round %d: pong ack = %d, want %d", round, got, seq+1)
		}
		if pong.S2CEv.Pong.PingId != "e2e-liveness" {
			t.Errorf("round %d: pong ping id = %q, want %q", round, pong.S2CEv.Pong.PingId, "e2e-liveness")
		}
		if pong.From.ServiceId != ssServiceId {
			t.Errorf("round %d: pong from.serviceId = %q, want %q", round, pong.From.ServiceId, ssServiceId)
		}
		if pong.To.UserId != alice.userId || pong.To.UserSessionId != alice.sessionId {
			t.Errorf("round %d: pong to = %+v, want alice's session identity", round, pong.To)
		}
		if pong.InReplyTo != msgId {
			t.Errorf("round %d: pong inReplyTo = %q, want %q", round, pong.InReplyTo, msgId)
		}
		seq = pong.S2CEv.Pong.AckSequenceNumber
	}
	if seq != 4 {
		t.Errorf("seq after 3 rounds = %d, want 4", seq)
	}

	alice.expectSilent()
}

// TestSSDisconnectAndReconnect covers the handler's source-address
// learning across a disconnect/reconnect: the learned (user id, user
// session id) → connection association is purged on disconnect and
// re-learned on the new connection.
func TestSSDisconnectAndReconnect(t *testing.T) {
	baseURL := startServer(t)

	alice := dialSS(t, baseURL)
	bob := dialSS(t, baseURL)
	alice.register("e2e-reg-alice", "alice", "alice")
	bob.register("e2e-reg-bob", "bob", "bob")

	ping := func(to, msgId string) *signallingEventView {
		return &signallingEventView{
			To:    epAddrView{ServiceId: ssServiceId},
			MsgId: msgId,
			C2CEv: &clientToClientEvView{
				FromSubscriber: "bob",
				ToSubscriber:   to,
				ChannelId:      ssMainChannelId,
				Ping:           &pingPongView{PingId: "e2e-reconnect", SequenceNumber: 1},
			},
		}
	}

	// Baseline: bob's ping to alice reaches her first connection.
	bob.send(ping("alice", "e2e-ping-1"))
	if ev := alice.recv(); ev.C2CEv == nil || ev.C2CEv.Ping == nil {
		t.Fatalf("alice got %+v, want the relayed ping", ev)
	}

	// alice disconnects; the address she was learned under is purged with
	// the connection.
	alice.conn.Close()

	// On reconnect with the same session cookie — the same identity — the
	// address is re-learned onto the new connection: the provider still
	// knows "alice", bound to this same (user, session, channel) tuple, so
	// the re-registration is a refresh — its ack arriving here proves the
	// re-learn. (The prototype has no unregister message, so the
	// registration survived the disconnect.)
	alice.redial()
	alice.register("e2e-reg-alice-again", "alice", "alice")

	// Relays addressed to the re-registered subscriber reach the new
	// connection.
	bob.send(ping("alice", "e2e-ping-2"))
	ev := alice.recv()
	if ev.C2CEv == nil || ev.C2CEv.Ping == nil || ev.C2CEv.Ping.PingId != "e2e-reconnect" {
		t.Fatalf("alice got %+v, want the relayed ping", ev)
	}
	if ev.To.UserId != alice.userId || ev.To.UserSessionId != alice.sessionId {
		t.Errorf("relayed to = %+v, want alice's session identity", ev.To)
	}

	// Silence checks last: a read timeout corrupts a gorilla connection.
	bob.expectSilent()
	alice.expectSilent()
}

// TestSSQuickReconnectRoaming is the quick-disconnect-and-reconnect
// roaming test: bob drops and immediately re-dials with the same
// identity, mid-ping-session. Because ping session state (ping id,
// seq/ack chain) is purely client-side and registrations outlive
// connections, alice keeps pinging bob on the SAME ping session without
// noticing the transient offline — the only requirement is that bob
// speaks first after reconnecting (any message, e.g. a liveness ping,
// re-learns his address onto the new connection, switch-style). Bob's
// profile stays queryable throughout, even while he is offline.
func TestSSQuickReconnectRoaming(t *testing.T) {
	baseURL := startServer(t)

	alice := dialSS(t, baseURL)
	bob := dialSS(t, baseURL)
	alice.register("e2e-reg-alice", "alice", "alice")
	bob.register("e2e-reg-bob", "bob", "bob")

	pingId := "e2e-roam"
	seq := uint64(1)
	bobSeq := uint64(0) // bob's own sequence space, kept across the reconnect

	// round drives one full ping/pong exchange of the shared session
	// between alice and the given bob connection.
	round := func(tag string, bobC *ssWSClient) {
		alice.send(&signallingEventView{
			To:    epAddrView{ServiceId: ssServiceId},
			MsgId: "e2e-roam-ping-" + tag,
			C2CEv: &clientToClientEvView{
				FromSubscriber: "alice",
				ToSubscriber:   "bob",
				ChannelId:      ssMainChannelId,
				Ping:           &pingPongView{PingId: pingId, SequenceNumber: seq},
			},
		})
		ping := bobC.recv()
		if ping.C2CEv == nil || ping.C2CEv.Ping == nil {
			t.Fatalf("round %s: bob got %+v, want a ping", tag, ping)
		}
		if got := ping.C2CEv.Ping.SequenceNumber; got != seq {
			t.Fatalf("round %s: ping seq = %d, want %d", tag, got, seq)
		}
		if ping.C2CEv.Ping.PingId != pingId {
			t.Fatalf("round %s: ping id = %q, want the same session %q", tag, ping.C2CEv.Ping.PingId, pingId)
		}
		if ping.To.UserId != bob.userId || ping.To.UserSessionId != bob.sessionId {
			t.Errorf("round %s: ping to = %+v, want bob's session identity", tag, ping.To)
		}

		bobSeq++
		bobC.send(&signallingEventView{
			To:    epAddrView{ServiceId: ssServiceId},
			MsgId: "e2e-roam-pong-" + tag,
			C2CEv: &clientToClientEvView{
				FromSubscriber: "bob",
				ToSubscriber:   "alice",
				ChannelId:      ssMainChannelId,
				Pong: &pingPongView{
					PingId:            pingId,
					SequenceNumber:    bobSeq,
					AckSequenceNumber: seq + 1,
				},
			},
		})
		pong := alice.recv()
		if pong.C2CEv == nil || pong.C2CEv.Pong == nil {
			t.Fatalf("round %s: alice got %+v, want a pong", tag, pong)
		}
		if got := pong.C2CEv.Pong.AckSequenceNumber; got != seq+1 {
			t.Fatalf("round %s: pong ack = %d, want %d", tag, got, seq+1)
		}
		seq = pong.C2CEv.Pong.AckSequenceNumber
	}

	queryBob := func(msgId string) {
		alice.send(&signallingEventView{
			To:    epAddrView{ServiceId: ssServiceId},
			MsgId: msgId,
			C2SEv: &clientToSSEvView{UserProfileQuery: &userProfileQueryView{
				SubscriberId: "bob",
				ChannelId:    ssMainChannelId,
			}},
		})
		reply := alice.recv()
		if reply.S2CEv == nil || reply.S2CEv.Err != nil || reply.S2CEv.Profile == nil {
			t.Fatalf("%s: reply = %+v, want a profile and no error", msgId, reply)
		}
		if p := reply.S2CEv.Profile; p.SubscriberId != "bob" || p.Username != "bob" {
			t.Errorf("%s: profile = %+v, want bob", msgId, p)
		}
	}

	// Round 1 runs on bob's first connection.
	round("1", bob)

	// bob drops. His registration — and therefore his profile — survive:
	// alice can still query it while he is offline.
	bob.conn.Close()
	queryBob("e2e-query-offline")

	// bob quickly reconnects with the same session cookie — the same
	// identity — and announces himself with a liveness ping to the SS,
	// re-learning his address onto the new connection.
	bob.redial()
	bob.send(&signallingEventView{
		To:    epAddrView{ServiceId: ssServiceId},
		MsgId: "e2e-bob-alive",
		C2SEv: &clientToSSEvView{Ping: &pingPongView{PingId: "e2e-bob-liveness", SequenceNumber: 1}},
	})
	if pong := bob.recv(); pong.S2CEv == nil || pong.S2CEv.Pong == nil {
		t.Fatalf("bob liveness reply = %+v, want a pong", pong)
	}

	// Rounds 2 and 3 run on the new connection, same ping session, seq/ack
	// chain unbroken — alice never noticed the transient offline.
	round("2", bob)
	round("3", bob)
	if seq != 4 {
		t.Errorf("seq after 3 rounds = %d, want 4", seq)
	}

	// And bob's profile is still queryable after the reconnect.
	queryBob("e2e-query-after")

	// Silence checks last: a read timeout corrupts a gorilla connection.
	bob.expectSilent()
	alice.expectSilent()
}

// TestSSSubscriberAging checks end to end that a subscriber registration
// expires after the aging interval without activity, and that the expired
// subscriber id is free again.
func TestSSSubscriberAging(t *testing.T) {
	baseURL := startServerWithArgs(t, "--ss-aging", "500ms")

	alice := dialSS(t, baseURL)
	bob := dialSS(t, baseURL)
	alice.register("e2e-reg-alice", "alice", "alice")
	bob.register("e2e-reg-bob", "bob", "bob")

	queryBob := func(msgId string) *signallingEventView {
		alice.send(&signallingEventView{
			To:    epAddrView{ServiceId: ssServiceId},
			MsgId: msgId,
			C2SEv: &clientToSSEvView{UserProfileQuery: &userProfileQueryView{
				SubscriberId: "bob",
				ChannelId:    ssMainChannelId,
			}},
		})
		return alice.recv()
	}

	// Within the aging interval bob's profile is queryable.
	if reply := queryBob("e2e-query-fresh"); reply.S2CEv == nil || reply.S2CEv.Err != nil || reply.S2CEv.Profile == nil {
		t.Fatalf("fresh query reply = %+v, want a profile and no error", reply)
	}

	// After the aging interval without any activity from bob, his
	// registration is invalid. (Alice's query revives her own
	// registration, but never bob's.)
	time.Sleep(1200 * time.Millisecond)
	reply := queryBob("e2e-query-expired")
	if reply.S2CEv == nil || reply.S2CEv.Err == nil || reply.S2CEv.Err.ErrorCode != ssErrSubscriberNotFound {
		t.Fatalf("expired query reply = %+v, want error code %d", reply, ssErrSubscriberNotFound)
	}

	// The expired subscriber id and username are free again: dave takes
	// them over.
	dave := dialSS(t, baseURL)
	dave.register("e2e-reg-dave", "bob", "bob")

	// Silence checks last: a read timeout corrupts a gorilla connection.
	bob.expectSilent()
	dave.expectSilent()
	alice.expectSilent()
}

// TestSSRejectsMissingSession checks that the handshake fails cleanly
// without a session cookie: the endpoint is not on the JWT whitelist, so
// the request never reaches the handler.
func TestSSRejectsMissingSession(t *testing.T) {
	baseURL := startServer(t)
	url := "ws" + strings.TrimPrefix(baseURL, "http") + "/api/ss/ws"

	_, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if !errors.Is(err, websocket.ErrBadHandshake) {
		t.Fatalf("dial without a session cookie: got %v, want ErrBadHandshake", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}
