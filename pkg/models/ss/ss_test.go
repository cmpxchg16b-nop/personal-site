package ss

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// harness wires a provider to buffered in/out channels for a test.
type harness struct {
	p   *SimpleOnMemorySSProvider
	in  chan *SignallingEvent
	out chan *SignallingEvent
}

func startProvider(t *testing.T) *harness {
	t.Helper()
	return startProviderWithAging(t, DefaultSubscriberAging)
}

func startProviderWithAging(t *testing.T, aging time.Duration) *harness {
	t.Helper()
	h := &harness{
		p:   NewSimpleOnMemorySSProviderWithAging(aging),
		in:  make(chan *SignallingEvent, 8),
		out: make(chan *SignallingEvent, 8),
	}
	go h.p.Run(context.Background(), h.in, h.out)
	t.Cleanup(h.p.Shutdown)
	return h
}

// recvOut returns the next emitted event, failing the test on timeout.
func (h *harness) recvOut(t *testing.T) *SignallingEvent {
	t.Helper()
	select {
	case ev, ok := <-h.out:
		if !ok {
			t.Fatal("outMsg closed unexpectedly")
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for an event on outMsg")
		return nil
	}
}

// expectNoOut asserts that nothing is emitted within a short window.
func (h *harness) expectNoOut(t *testing.T) {
	t.Helper()
	select {
	case ev := <-h.out:
		t.Fatalf("unexpected event on outMsg: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

// mustRegister registers sub with username on the default address
// derived from sub, consuming the ack.
func (h *harness) mustRegister(t *testing.T, sub SubscriberId, username string) {
	t.Helper()
	h.mustRegisterFrom(t, EPAddr{
		UserId:        UserId("u-" + string(sub)),
		UserSessionId: UserSessionId("s-" + string(sub)),
	}, sub, username)
}

// mustRegisterFrom registers sub with username from the given address,
// consuming the ack.
func (h *harness) mustRegisterFrom(t *testing.T, from EPAddr, sub SubscriberId, username string) {
	t.Helper()
	h.in <- registerEventFrom(from, sub, WellKnownChIdMain, username)
	if ack := h.recvOut(t); ack.S2CEv == nil || ack.S2CEv.Err != nil {
		t.Fatalf("Register(%q, %q) ack: %+v", sub, username, ack)
	}
}

func registerEvent(sub SubscriberId, ch ChannelId, username string) *SignallingEvent {
	return registerEventFrom(
		EPAddr{UserId: UserId("u-" + string(sub)), UserSessionId: UserSessionId("s-" + string(sub))},
		sub, ch, username)
}

func registerEventFrom(from EPAddr, sub SubscriberId, ch ChannelId, username string) *SignallingEvent {
	return &SignallingEvent{
		From:  from,
		MsgId: MsgId("m-reg-" + string(sub) + "-" + string(from.UserId)),
		C2SEv: &ClientToSSEv{Register: &ClientToSSRegEv{
			SubscriberId: sub,
			ChannelId:    ch,
			Username:     username,
		}},
	}
}

func TestRegisterAck(t *testing.T) {
	h := startProvider(t)
	ev := registerEvent("alice", WellKnownChIdMain, "alice")
	h.in <- ev

	ack := h.recvOut(t)
	if ack.S2CEv == nil {
		t.Fatalf("ack carries no s2CEv: %+v", ack)
	}
	if ack.S2CEv.Err != nil {
		t.Fatalf("ack carries an error: %+v", ack.S2CEv.Err)
	}
	if ack.S2CEv.RegisterResult == nil {
		t.Fatalf("ack carries no registerResult: %+v", ack.S2CEv)
	}
	if ack.S2CEv.RegisterResult.ChannelId != WellKnownChIdMain ||
		ack.S2CEv.RegisterResult.SubscriberId != "alice" {
		t.Fatalf("ack registerResult = %+v, want alice in the main channel", ack.S2CEv.RegisterResult)
	}
	if ack.From.ServiceId != WellKnownSvcIdSS {
		t.Fatalf("ack.From.ServiceId = %q, want %q", ack.From.ServiceId, WellKnownSvcIdSS)
	}
	if ack.To != ev.From {
		t.Fatalf("ack.To = %+v, want %+v", ack.To, ev.From)
	}
	if ack.InReplyTo == nil || *ack.InReplyTo != ev.MsgId {
		t.Fatalf("ack.InReplyTo = %v, want %q", ack.InReplyTo, ev.MsgId)
	}
	if ack.MsgId == "" || ack.MsgId == ev.MsgId {
		t.Fatalf("ack.MsgId = %q, want a fresh, distinct id", ack.MsgId)
	}
}

// TestReregisterSameTuple checks that a subscriber id — bound to zero
// or one (user id, user session id, channel id) tuple — can be
// re-registered by the same tuple (a refresh that may update the
// username), while another tuple is rejected.
func TestReregisterSameTuple(t *testing.T) {
	h := startProvider(t)
	aliceAddr := EPAddr{UserId: "u-alice", UserSessionId: "s-alice"}

	h.mustRegisterFrom(t, aliceAddr, "alice", "alice")

	// Same tuple, same username: a plain refresh.
	h.mustRegisterFrom(t, aliceAddr, "alice", "alice")

	// Same tuple, new username: the profile is updated.
	h.mustRegisterFrom(t, aliceAddr, "alice", "alice2")
	h.in <- &SignallingEvent{
		From:  EPAddr{UserId: "u-q", UserSessionId: "s-q"},
		MsgId: "m-q-rename",
		C2SEv: &ClientToSSEv{UserProfileQuery: &ClientToSSUserProfileQuery{
			SubscriberId: "alice",
			ChannelId:    WellKnownChIdMain,
		}},
	}
	reply := h.recvOut(t)
	if reply.S2CEv == nil || reply.S2CEv.Profile == nil || reply.S2CEv.Profile.Username != "alice2" {
		t.Fatalf("profile after rename = %+v, want username alice2", reply)
	}

	// The old username is free for someone else.
	h.mustRegisterFrom(t, EPAddr{UserId: "u-bob", UserSessionId: "s-bob"}, "bob", "alice")

	// But the subscriber id stays bound to alice's tuple.
	h.in <- registerEventFrom(EPAddr{UserId: "u-eve", UserSessionId: "s-eve"},
		"alice", WellKnownChIdMain, "eve")
	reply = h.recvOut(t)
	if reply.S2CEv == nil || reply.S2CEv.Err == nil ||
		reply.S2CEv.Err.ErrorCode != ErrorCodeSubscriberIdIsRegistered {
		t.Fatalf("other-tuple re-registration reply = %+v, want error code %d",
			reply, ErrorCodeSubscriberIdIsRegistered)
	}
}

// TestRegisterAutoAssignedSubscriberId checks that a registration with
// an empty subscriber id is assigned the next free id from the automatic
// assignment range, sequentially, skipping ids already taken.
func TestRegisterAutoAssignedSubscriberId(t *testing.T) {
	h := startProvider(t)

	registerAuto := func(user, username string) *RegisterResult {
		t.Helper()
		h.in <- registerEventFrom(
			EPAddr{UserId: UserId("u-" + user), UserSessionId: UserSessionId("s-" + user)},
			"", WellKnownChIdMain, username)
		ack := h.recvOut(t)
		if ack.S2CEv == nil || ack.S2CEv.Err != nil || ack.S2CEv.RegisterResult == nil {
			t.Fatalf("auto register(%q) ack: %+v, want a registerResult and no error", username, ack)
		}
		return ack.S2CEv.RegisterResult
	}

	// Ids are assigned sequentially from the start of the range.
	if got := registerAuto("a", "auto-a"); got.SubscriberId != "1000" || got.ChannelId != WellKnownChIdMain {
		t.Fatalf("first auto assignment = %+v, want subscriber 1000 in the main channel", got)
	}
	if got := registerAuto("b", "auto-b"); got.SubscriberId != "1001" {
		t.Fatalf("second auto assignment = %+v, want subscriber 1001", got)
	}

	// Ids already taken are skipped.
	h.mustRegister(t, "1002", "manual")
	if got := registerAuto("c", "auto-c"); got.SubscriberId != "1003" {
		t.Fatalf("auto assignment past a taken id = %+v, want subscriber 1003", got)
	}

	// The assigned id is a working registration.
	h.in <- &SignallingEvent{
		From:  EPAddr{UserId: "u-q", UserSessionId: "s-q"},
		MsgId: "m-q-auto",
		C2SEv: &ClientToSSEv{UserProfileQuery: &ClientToSSUserProfileQuery{
			SubscriberId: "1000",
			ChannelId:    WellKnownChIdMain,
		}},
	}
	reply := h.recvOut(t)
	if reply.S2CEv == nil || reply.S2CEv.Profile == nil || reply.S2CEv.Profile.Username != "auto-a" {
		t.Fatalf("profile of an auto-assigned id = %+v, want username auto-a", reply)
	}
}

// TestRegisterAutoAssignedExhaustion checks that when every id of the
// automatic assignment range is taken, an empty-id registration is
// rejected with ErrorCodeNoSubscriberIdAvailable.
func TestRegisterAutoAssignedExhaustion(t *testing.T) {
	h := startProvider(t)
	for n := autoSubscriberIdRangeStart; n < autoSubscriberIdRangeEnd; n++ {
		id := SubscriberId(strconv.Itoa(n))
		h.mustRegister(t, id, string(id))
	}
	h.in <- registerEventFrom(EPAddr{UserId: "u-late", UserSessionId: "s-late"},
		"", WellKnownChIdMain, "late")
	reply := h.recvOut(t)
	if reply.S2CEv == nil || reply.S2CEv.Err == nil ||
		reply.S2CEv.Err.ErrorCode != ErrorCodeNoSubscriberIdAvailable {
		t.Fatalf("exhausted range reply = %+v, want error code %d", reply, ErrorCodeNoSubscriberIdAvailable)
	}
}

func TestRegisterErrors(t *testing.T) {
	h := startProvider(t)
	h.mustRegister(t, "alice", "alice")

	for _, tc := range []struct {
		name string
		ev   *SignallingEvent
		want ErrorCode
	}{
		// A subscriber id is bound to one (user id, user session id,
		// channel id) tuple: another identity cannot take it.
		{"duplicate subscriber id", registerEventFrom(
			EPAddr{UserId: "u-eve", UserSessionId: "s-eve"},
			"alice", WellKnownChIdMain, "eve"), ErrorCodeSubscriberIdIsRegistered},
		{"username taken", registerEvent("bob", WellKnownChIdMain, "alice"), ErrorCodeUsernameTaken},
		{"unknown channel", registerEvent("bob", "no-such-channel", "bob"), ErrorCodeChannelNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h.in <- tc.ev
			reply := h.recvOut(t)
			if reply.S2CEv == nil || reply.S2CEv.Err == nil {
				t.Fatalf("reply = %+v, want an s2CEv error", reply)
			}
			if reply.S2CEv.Err.ErrorCode != tc.want {
				t.Fatalf("error code = %d, want %d", reply.S2CEv.Err.ErrorCode, tc.want)
			}
			if reply.InReplyTo == nil || *reply.InReplyTo != tc.ev.MsgId {
				t.Fatalf("reply.InReplyTo = %v, want %q", reply.InReplyTo, tc.ev.MsgId)
			}
		})
	}
}

func TestQueryProfile(t *testing.T) {
	h := startProvider(t)
	h.mustRegister(t, "alice", "alice")

	query := func(sub SubscriberId, ch ChannelId) *SignallingEvent {
		return &SignallingEvent{
			From:  EPAddr{UserId: "u-bob"},
			MsgId: MsgId("m-query"),
			C2SEv: &ClientToSSEv{UserProfileQuery: &ClientToSSUserProfileQuery{
				SubscriberId: sub,
				ChannelId:    ch,
			}},
		}
	}

	h.in <- query("alice", WellKnownChIdMain)
	reply := h.recvOut(t)
	if reply.S2CEv == nil || reply.S2CEv.Err != nil || reply.S2CEv.Profile == nil {
		t.Fatalf("reply = %+v, want a profile and no error", reply)
	}
	profile := reply.S2CEv.Profile
	if profile.SubscriberId != "alice" || profile.ChannelId != WellKnownChIdMain || profile.Username != "alice" {
		t.Fatalf("profile = %+v, want alice in the main channel", profile)
	}

	for _, tc := range []struct {
		name string
		ev   *SignallingEvent
		want ErrorCode
	}{
		{"unknown subscriber", query("nobody", WellKnownChIdMain), ErrorCodeSubscriberNotFound},
		{"unknown channel", query("alice", "no-such-channel"), ErrorCodeChannelNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h.in <- tc.ev
			reply := h.recvOut(t)
			if reply.S2CEv == nil || reply.S2CEv.Err == nil {
				t.Fatalf("reply = %+v, want an s2CEv error", reply)
			}
			if reply.S2CEv.Err.ErrorCode != tc.want {
				t.Fatalf("error code = %d, want %d", reply.S2CEv.Err.ErrorCode, tc.want)
			}
		})
	}
}

func TestQueryChannelProfile(t *testing.T) {
	h := startProvider(t)

	query := func(ch ChannelId) *SignallingEvent {
		return &SignallingEvent{
			From:  EPAddr{UserId: "u-bob"},
			MsgId: MsgId("m-ch-query"),
			C2SEv: &ClientToSSEv{ChannelProfileQuery: &ClientToSSChannelProfileQuery{ChannelId: ch}},
		}
	}

	h.in <- query(WellKnownChIdMain)
	reply := h.recvOut(t)
	if reply.S2CEv == nil || reply.S2CEv.Err != nil || reply.S2CEv.ChannelProfile == nil {
		t.Fatalf("reply = %+v, want a channel profile and no error", reply)
	}
	profile := reply.S2CEv.ChannelProfile
	if profile.ChannelId != WellKnownChIdMain || profile.ChannelName != mainChannelName {
		t.Fatalf("channel profile = %+v, want the main channel", profile)
	}

	h.in <- query("no-such-channel")
	reply = h.recvOut(t)
	if reply.S2CEv == nil || reply.S2CEv.Err == nil || reply.S2CEv.Err.ErrorCode != ErrorCodeChannelNotFound {
		t.Fatalf("unknown channel reply = %+v, want error code %d", reply, ErrorCodeChannelNotFound)
	}
}

func TestRelayPassesC2CThrough(t *testing.T) {
	h := startProvider(t)
	h.mustRegister(t, "alice", "alice")
	h.mustRegister(t, "bob", "bob")

	c2c := func() *ClientToClientEv {
		return &ClientToClientEv{FromSubscriber: "alice", ToSubscriber: "bob", ChannelId: WellKnownChIdMain}
	}
	for _, tc := range []struct {
		name string
		ev   *SignallingEvent
	}{
		{"ping", &SignallingEvent{MsgId: "m-ping", C2CEv: func() *ClientToClientEv {
			e := c2c()
			e.Ping = &PingPongMsg{PingId: "p1", SequenceNumber: 1}
			return e
		}()}},
		{"pong", &SignallingEvent{MsgId: "m-pong", C2CEv: func() *ClientToClientEv {
			e := c2c()
			e.Pong = &PingPongMsg{PingId: "p1", SequenceNumber: 1, AckSequenceNumber: 2}
			return e
		}()}},
		{"session description", &SignallingEvent{MsgId: "m-sdp", C2CEv: func() *ClientToClientEv {
			e := c2c()
			e.SessionDesc = &webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: "v=0\r\n"}
			return e
		}()}},
		{"ice candidate", &SignallingEvent{MsgId: "m-ice", C2CEv: func() *ClientToClientEv {
			e := c2c()
			sdpMid := "0"
			e.RTCICECandidate = &webrtc.ICECandidateInit{Candidate: "candidate:1 1 udp 2130706431 192.0.2.1 9 typ host", SDPMid: &sdpMid}
			return e
		}()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h.in <- tc.ev
			got := h.recvOut(t)
			if got != tc.ev {
				t.Fatalf("relayed event = %+v, want the same event passed through", got)
			}
			wantTo := EPAddr{UserId: "u-bob", UserSessionId: "s-bob"}
			if got.To != wantTo {
				t.Fatalf("relayed event To = %+v, want %+v (resolved subscriber address)", got.To, wantTo)
			}
		})
	}
}

func TestRelayUnknownSubscriber(t *testing.T) {
	h := startProvider(t)
	h.mustRegister(t, "alice", "alice")

	for _, tc := range []struct {
		name string
		c2c  *ClientToClientEv
		want ErrorCode
	}{
		{"from not registered", &ClientToClientEv{FromSubscriber: "ghost", ToSubscriber: "alice", ChannelId: WellKnownChIdMain}, ErrorCodeSubscriberNotFound},
		{"to not registered", &ClientToClientEv{FromSubscriber: "alice", ToSubscriber: "ghost", ChannelId: WellKnownChIdMain}, ErrorCodeSubscriberNotFound},
		{"channel not found", &ClientToClientEv{FromSubscriber: "alice", ToSubscriber: "alice", ChannelId: "no-such-channel"}, ErrorCodeChannelNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.c2c.Ping = &PingPongMsg{PingId: "p1", SequenceNumber: 1}
			h.in <- &SignallingEvent{From: EPAddr{UserId: "u-alice"}, MsgId: "m-relay", C2CEv: tc.c2c}
			reply := h.recvOut(t)
			if reply.S2CEv == nil || reply.S2CEv.Err == nil {
				t.Fatalf("reply = %+v, want an s2CEv error", reply)
			}
			if reply.S2CEv.Err.ErrorCode != tc.want {
				t.Fatalf("error code = %d, want %d", reply.S2CEv.Err.ErrorCode, tc.want)
			}
		})
	}
}

func TestWireJSONShape(t *testing.T) {
	data, err := json.Marshal(&SignallingEvent{
		From:  EPAddr{UserId: "u-alice", UserSessionId: "s-1"},
		MsgId: "m1",
		C2CEv: &ClientToClientEv{
			FromSubscriber: "alice",
			ToSubscriber:   "bob",
			ChannelId:      WellKnownChIdMain,
			SessionDesc:    &webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: "v=0\r\n"},
		},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `{"from":{"userId":"u-alice","userSessionId":"s-1"},"to":{},"msgId":"m1","c2CEv":{"fromSubscriber":"alice","toSubscriber":"bob","channelId":"f887f5b0-7b78-4ceb-a051-f42879f9d98e","sessionDesc":{"type":"offer","sdp":"v=0\r\n"}}}`
	if string(data) != want {
		t.Fatalf("wire shape = %s, want %s", data, want)
	}
}

func TestListChannels(t *testing.T) {
	h := startProvider(t)

	// No registration is needed: list the channels right away. Only the
	// main channel exists, so the answer is exactly one final page
	// holding it.
	h.in <- &SignallingEvent{
		From:  EPAddr{UserId: "u-q", UserSessionId: "s-q"},
		MsgId: MsgId("m-list-channels"),
		C2SEv: &ClientToSSEv{ListChannels: &ClientToSSListChannels{}},
	}
	res := h.recvOut(t)
	if res.S2CEv == nil || res.S2CEv.ChannelListResult == nil {
		t.Fatalf("reply = %+v, want a channelListResult", res)
	}
	if res.S2CEv.Err != nil {
		t.Fatalf("reply carries an error: %+v", res.S2CEv.Err)
	}
	if res.InReplyTo == nil || *res.InReplyTo != "m-list-channels" {
		t.Errorf("page InReplyTo = %v, want %q", res.InReplyTo, "m-list-channels")
	}
	page := res.S2CEv.ChannelListResult
	if page.HasMore {
		t.Error("page hasMore = true, want false")
	}
	if want := []ChannelId{WellKnownChIdMain}; !slices.Equal(page.Channels, want) {
		t.Errorf("page channels = %v, want %v", page.Channels, want)
	}
}

// TestPingPongWithServer checks the c2s ping / s2c pong liveness
// exchange between a client and the SS itself: no registration is needed
// first, the pong keeps the ping id and answers ack = seq + 1, and the
// next ping's seq is the ack of the last reply.
func TestPingPongWithServer(t *testing.T) {
	h := startProvider(t)

	// Deliberately no registration: pinging the SS itself must work first.
	from := EPAddr{UserId: "u-alice", UserSessionId: "s-alice"}
	seq := uint64(1)
	for round := 1; round <= 3; round++ {
		ev := &SignallingEvent{
			From:  from,
			To:    EPAddr{ServiceId: WellKnownSvcIdSS},
			MsgId: MsgId("m-c2s-ping-" + string(rune('0'+round))),
			C2SEv: &ClientToSSEv{Ping: &PingPongMsg{PingId: "s-ping", SequenceNumber: seq}},
		}
		h.in <- ev

		pong := h.recvOut(t)
		if pong.S2CEv == nil || pong.S2CEv.Pong == nil {
			t.Fatalf("round %d: reply = %+v, want an s2c pong", round, pong)
		}
		if got := pong.S2CEv.Pong.AckSequenceNumber; got != seq+1 {
			t.Fatalf("round %d: pong ack = %d, want %d", round, got, seq+1)
		}
		if pong.S2CEv.Pong.PingId != "s-ping" {
			t.Errorf("round %d: pong ping id = %q, want %q", round, pong.S2CEv.Pong.PingId, "s-ping")
		}
		if pong.From.ServiceId != WellKnownSvcIdSS {
			t.Errorf("round %d: pong From.ServiceId = %q, want %q", round, pong.From.ServiceId, WellKnownSvcIdSS)
		}
		if pong.To != from {
			t.Errorf("round %d: pong To = %+v, want %+v", round, pong.To, from)
		}
		if pong.InReplyTo == nil || *pong.InReplyTo != ev.MsgId {
			t.Errorf("round %d: pong InReplyTo = %v, want %q", round, pong.InReplyTo, ev.MsgId)
		}
		seq = pong.S2CEv.Pong.AckSequenceNumber
	}
	if seq != 4 {
		t.Errorf("seq after 3 rounds = %d, want 4", seq)
	}
}

func TestListChannelMembers(t *testing.T) {
	h := startProvider(t)

	list := func(msgId string, ch ChannelId) *SignallingEvent {
		return &SignallingEvent{
			From:  EPAddr{UserId: "u-q", UserSessionId: "s-q"},
			MsgId: MsgId(msgId),
			C2SEv: &ClientToSSEv{ListChannelMembers: &ClientToSSListChannelMembers{ChannelId: ch}},
		}
	}
	recvPage := func() (*SignallingEvent, *SSToClientChannelMbsListResult) {
		res := h.recvOut(t)
		if res.S2CEv == nil || res.S2CEv.ChannelMbsListResult == nil {
			t.Fatalf("reply = %+v, want a channelMbsListResult", res)
		}
		if res.S2CEv.Err != nil {
			t.Fatalf("reply carries an error: %+v", res.S2CEv.Err)
		}
		return res, res.S2CEv.ChannelMbsListResult
	}

	// An unknown channel is rejected.
	h.in <- list("m-list-unknown", "no-such-channel")
	reply := h.recvOut(t)
	if reply.S2CEv == nil || reply.S2CEv.Err == nil || reply.S2CEv.Err.ErrorCode != ErrorCodeChannelNotFound {
		t.Fatalf("unknown channel reply = %+v, want error code %d", reply, ErrorCodeChannelNotFound)
	}

	// An empty channel yields exactly one, empty, final page.
	h.in <- list("m-list-empty", WellKnownChIdMain)
	res, page := recvPage()
	if len(page.Members) != 0 || page.HasMore {
		t.Fatalf("empty channel page = %+v, want no members and hasMore=false", page)
	}
	if page.ChannelId != WellKnownChIdMain {
		t.Errorf("page channelId = %q, want %q", page.ChannelId, WellKnownChIdMain)
	}
	if res.InReplyTo == nil || *res.InReplyTo != "m-list-empty" {
		t.Errorf("page InReplyTo = %v, want %q", res.InReplyTo, "m-list-empty")
	}

	// A member list that fits one page comes back as a single sorted page
	// of subscriber ids.
	h.mustRegister(t, "bob", "bob")
	h.mustRegister(t, "alice", "alice")
	h.in <- list("m-list-single", WellKnownChIdMain)
	_, page = recvPage()
	if page.HasMore {
		t.Error("single page: hasMore = true, want false")
	}
	if want := []SubscriberId{"alice", "bob"}; !slices.Equal(page.Members, want) {
		t.Errorf("single page members = %v, want %v", page.Members, want)
	}

	// A larger list is paged; every page is correlated to the request and
	// carries a fresh msg id.
	for i := 0; i < channelMembersPageSize+6-2; i++ { // alice & bob are already registered
		id := SubscriberId(fmt.Sprintf("sub-%03d", i))
		h.mustRegister(t, id, string(id))
	}
	h.in <- list("m-list-paged", WellKnownChIdMain)
	var got []SubscriberId
	msgIds := map[MsgId]bool{}
	pages := 0
	for {
		res, page := recvPage()
		if res.InReplyTo == nil || *res.InReplyTo != "m-list-paged" {
			t.Errorf("page InReplyTo = %v, want %q", res.InReplyTo, "m-list-paged")
		}
		if msgIds[res.MsgId] {
			t.Errorf("page msg id %q reused across pages", res.MsgId)
		}
		msgIds[res.MsgId] = true
		if len(page.Members) > channelMembersPageSize {
			t.Errorf("page carries %d members, page size is %d", len(page.Members), channelMembersPageSize)
		}
		pages++
		got = append(got, page.Members...)
		if !page.HasMore {
			break
		}
	}
	if pages != 2 {
		t.Errorf("pages = %d, want 2", pages)
	}
	if len(got) != channelMembersPageSize+6 {
		t.Errorf("total members = %d, want %d", len(got), channelMembersPageSize+6)
	}
	if !slices.IsSorted(got) {
		t.Error("members across pages are not sorted by subscriber id")
	}
	if got[0] != "alice" || got[1] != "bob" {
		t.Errorf("first members = %q, %q, want alice, bob", got[0], got[1])
	}
}

func keepAliveEventFrom(from EPAddr, sub SubscriberId, ch ChannelId, msgId string) *SignallingEvent {
	return &SignallingEvent{
		From:  from,
		To:    EPAddr{ServiceId: WellKnownSvcIdSS},
		MsgId: MsgId(msgId),
		C2SEv: &ClientToSSEv{ChannelKeepAlive: &ClientToSSChannelKeepAlive{
			ChannelId:    ch,
			SubscriberId: sub,
		}},
	}
}

// TestChannelKeepAlive checks that a channel keepalive is answered with
// nothing on success, and rejected with the well-known error codes when
// the channel is unknown, the subscriber is not registered, or the
// caller's identity is not the registration's tuple.
func TestChannelKeepAlive(t *testing.T) {
	h := startProvider(t)
	aliceAddr := EPAddr{UserId: "u-alice", UserSessionId: "s-alice"}
	h.mustRegisterFrom(t, aliceAddr, "alice", "alice")

	// A successful renewal is silent: nothing is answered.
	h.in <- keepAliveEventFrom(aliceAddr, "alice", WellKnownChIdMain, "m-ka-ok")
	h.expectNoOut(t)

	for _, tc := range []struct {
		name string
		ev   *SignallingEvent
		want ErrorCode
	}{
		{"unknown channel", keepAliveEventFrom(aliceAddr, "alice", "no-such-channel", "m-ka-ch"), ErrorCodeChannelNotFound},
		{"unknown subscriber", keepAliveEventFrom(aliceAddr, "ghost", WellKnownChIdMain, "m-ka-sub"), ErrorCodeSubscriberNotFound},
		// A subscriber id is bound to its registering (user id, user
		// session id) tuple: nobody else may renew the membership.
		{"another identity", keepAliveEventFrom(
			EPAddr{UserId: "u-eve", UserSessionId: "s-eve"},
			"alice", WellKnownChIdMain, "m-ka-eve"), ErrorCodeSubscriberNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h.in <- tc.ev
			reply := h.recvOut(t)
			if reply.S2CEv == nil || reply.S2CEv.Err == nil {
				t.Fatalf("reply = %+v, want an s2CEv error", reply)
			}
			if reply.S2CEv.Err.ErrorCode != tc.want {
				t.Fatalf("error code = %d, want %d", reply.S2CEv.Err.ErrorCode, tc.want)
			}
			if reply.InReplyTo == nil || *reply.InReplyTo != tc.ev.MsgId {
				t.Fatalf("reply.InReplyTo = %v, want %q", reply.InReplyTo, tc.ev.MsgId)
			}
		})
	}
}

// TestChannelKeepAliveRenewsMembership checks that channel keepalives
// extend the registration — the subscriber stays live past the aging
// interval — and that once they stop and the membership expires, a late
// keepalive is rejected and renews nothing.
func TestChannelKeepAliveRenewsMembership(t *testing.T) {
	const aging = 200 * time.Millisecond
	h := startProviderWithAging(t, aging)
	aliceAddr := EPAddr{UserId: "u-alice", UserSessionId: "s-alice"}
	h.mustRegisterFrom(t, aliceAddr, "alice", "alice")

	profileErr := func(msgId string) *SSToClientEv {
		t.Helper()
		// From a third party, so the query itself does not refresh alice.
		h.in <- &SignallingEvent{
			From:  EPAddr{UserId: "u-q", UserSessionId: "s-q"},
			MsgId: MsgId(msgId),
			C2SEv: &ClientToSSEv{UserProfileQuery: &ClientToSSUserProfileQuery{
				SubscriberId: "alice",
				ChannelId:    WellKnownChIdMain,
			}},
		}
		reply := h.recvOut(t)
		if reply.S2CEv == nil {
			t.Fatalf("query reply = %+v, want an s2CEv", reply)
		}
		return reply.S2CEv
	}

	// Keepalives every aging/4 keep the membership valid well past one
	// aging interval.
	for i := 1; i <= 5; i++ {
		time.Sleep(aging / 4)
		h.in <- keepAliveEventFrom(aliceAddr, "alice", WellKnownChIdMain,
			fmt.Sprintf("m-ka-%d", i))
	}
	h.expectNoOut(t) // renewals are silent
	if s2c := profileErr("m-q-alive"); s2c.Err != nil || s2c.Profile == nil {
		t.Fatalf("profile after keepalives: err=%+v profile=%+v, want a live registration", s2c.Err, s2c.Profile)
	}

	// Once they stop, the membership expires: a third-party query finds
	// alice gone (evicting the expired entry — expiration is lazy)...
	time.Sleep(5 * aging / 2)
	if s2c := profileErr("m-q-expired"); s2c.Err == nil || s2c.Err.ErrorCode != ErrorCodeSubscriberNotFound {
		t.Fatalf("profile after expiry = %+v, want error code %d", s2c, ErrorCodeSubscriberNotFound)
	}

	// ...and a late keepalive is rejected — the registration is gone, not
	// merely stale — and renews nothing.
	h.in <- keepAliveEventFrom(aliceAddr, "alice", WellKnownChIdMain, "m-ka-late")
	reply := h.recvOut(t)
	if reply.S2CEv == nil || reply.S2CEv.Err == nil ||
		reply.S2CEv.Err.ErrorCode != ErrorCodeSubscriberNotFound {
		t.Fatalf("late keepalive reply = %+v, want error code %d", reply, ErrorCodeSubscriberNotFound)
	}
	if s2c := profileErr("m-q-still-expired"); s2c.Err == nil || s2c.Err.ErrorCode != ErrorCodeSubscriberNotFound {
		t.Fatalf("profile after late keepalive = %+v, want error code %d", s2c, ErrorCodeSubscriberNotFound)
	}
}

// TestSubscriberAging checks that a registration expires when the
// subscriber is idle longer than the aging interval, that any activity —
// here c2s liveness pings — keeps it valid, and that an expired
// subscriber id and username are free again.
func TestSubscriberAging(t *testing.T) {
	const aging = 200 * time.Millisecond
	h := startProviderWithAging(t, aging)

	queryProfile := func(sub SubscriberId, msgId string) *SignallingEvent {
		// From a third party, so the query itself does not refresh sub.
		return &SignallingEvent{
			From:  EPAddr{UserId: "u-q", UserSessionId: "s-q"},
			MsgId: MsgId(msgId),
			C2SEv: &ClientToSSEv{UserProfileQuery: &ClientToSSUserProfileQuery{
				SubscriberId: sub,
				ChannelId:    WellKnownChIdMain,
			}},
		}
	}
	profileErr := func(sub SubscriberId, msgId string) *SSToClientEv {
		t.Helper()
		h.in <- queryProfile(sub, msgId)
		reply := h.recvOut(t)
		if reply.S2CEv == nil {
			t.Fatalf("query %q reply = %+v, want an s2CEv", sub, reply)
		}
		return reply.S2CEv
	}

	h.mustRegister(t, "bob", "bob")

	// Fresh registration: queryable.
	if s2c := profileErr("bob", "m-q-fresh"); s2c.Err != nil || s2c.Profile == nil {
		t.Fatalf("fresh profile reply: err=%+v profile=%+v", s2c.Err, s2c.Profile)
	}

	// Keepalive: liveness pings from bob's address every aging/4 keep the
	// registration valid well past one aging interval.
	for i := 1; i <= 5; i++ {
		time.Sleep(aging / 4)
		h.in <- &SignallingEvent{
			From:  EPAddr{UserId: "u-bob", UserSessionId: "s-bob"},
			To:    EPAddr{ServiceId: WellKnownSvcIdSS},
			MsgId: MsgId(fmt.Sprintf("m-keepalive-%d", i)),
			C2SEv: &ClientToSSEv{Ping: &PingPongMsg{PingId: "keepalive", SequenceNumber: uint64(i)}},
		}
		if pong := h.recvOut(t); pong.S2CEv == nil || pong.S2CEv.Pong == nil {
			t.Fatalf("keepalive %d: reply = %+v, want a pong", i, pong)
		}
	}
	if s2c := profileErr("bob", "m-q-alive"); s2c.Err != nil || s2c.Profile == nil {
		t.Fatalf("profile after keepalive: err=%+v profile=%+v, want a live registration", s2c.Err, s2c.Profile)
	}

	// Idle for longer than the aging interval: the registration is
	// invalid.
	time.Sleep(5 * aging / 2)
	if s2c := profileErr("bob", "m-q-expired"); s2c.Err == nil || s2c.Err.ErrorCode != ErrorCodeSubscriberNotFound {
		t.Fatalf("expired profile reply = %+v, want error code %d", s2c, ErrorCodeSubscriberNotFound)
	}

	// The expired subscriber id and username are free again.
	h.mustRegister(t, "bob", "bob")
}

func TestMalformedEventDropped(t *testing.T) {
	h := startProvider(t)
	h.in <- nil
	h.in <- &SignallingEvent{MsgId: "m-empty"}
	h.expectNoOut(t)
}

func TestShutdownStopsRun(t *testing.T) {
	p := NewSimpleOnMemorySSProvider()
	in := make(chan *SignallingEvent)
	out := make(chan *SignallingEvent, 1)
	done := make(chan struct{})
	go func() {
		p.Run(context.Background(), in, out)
		close(done)
	}()

	p.Shutdown()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after Shutdown")
	}
	if _, ok := <-out; ok {
		t.Fatal("outMsg not closed after Run returned")
	}
}

func TestClosedInMsgStopsRun(t *testing.T) {
	p := NewSimpleOnMemorySSProvider()
	in := make(chan *SignallingEvent)
	out := make(chan *SignallingEvent)
	done := make(chan struct{})
	go func() {
		p.Run(context.Background(), in, out)
		close(done)
	}()

	close(in)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after inMsg was closed")
	}
	if _, ok := <-out; ok {
		t.Fatal("outMsg not closed after Run returned")
	}
	p.Shutdown() // still safe and idempotent after Run returned
}
