package rtc

// The media tests: the client's track support (AddTrack/RemoveTrack/
// HandleTrack), including the glare rebuild's track re-add — the piece
// that keeps a bot's music alive across a collision.

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"personal-site/pkg/models/ss"
)

// pcmuTestTrack builds a PCMU sample track — the shape a bot's music
// source takes.
func pcmuTestTrack(t *testing.T, id string) *webrtc.TrackLocalStaticSample {
	t.Helper()
	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: 8000, Channels: 1},
		id, "rtc-test")
	if err != nil {
		t.Fatalf("NewTrackLocalStaticSample: %v", err)
	}
	return track
}

// pumpTrack writes one 20 ms frame per tick until ctx ends — the shape of
// a bot's music pump. Writes before the track is bound (and across a
// rebind) drop harmlessly.
func pumpTrack(ctx context.Context, track *webrtc.TrackLocalStaticSample) {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	frame := make([]byte, 160)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := track.WriteSample(media.Sample{Data: frame, Duration: 20 * time.Millisecond}); err != nil {
			return
		}
	}
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

// TestHeadlessRTCClientCarriesTracks is the media happy path: a track
// added to one client's session renegotiates the connection and arrives
// at the other client's track handler, carrying its frames — and the
// error and no-op edges of AddTrack/RemoveTrack behave.
func TestHeadlessRTCClientCarriesTracks(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	a, _ := startTestClient(t, net, "a", func(c *RTCClientConfiguration) { c.SubscriberId = "1-a" })
	b, _ := startTestClient(t, net, "b", func(c *RTCClientConfiguration) { c.SubscriberId = "2-b" })

	tracks := make(chan *webrtc.TrackRemote, 4)
	b.HandleTrackFunc(func(_ context.Context, _ ss.ChannelId, _ ss.SubscriberId, track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		tracks <- track
	})

	aId, bId := waitRegistered(t, a), waitRegistered(t, b)
	waitFor(t, "both clients to hold a session", func() bool {
		return slices.Contains(a.Peers(), bId) && slices.Contains(b.Peers(), aId)
	})

	// No session: AddTrack says so; RemoveTrack is a no-op.
	stray := pcmuTestTrack(t, "stray")
	if _, err := a.AddTrack("nobody", stray); !errors.Is(err, ErrNoPeerSession) {
		t.Fatalf("AddTrack without a session: got %v, want %v", err, ErrNoPeerSession)
	}
	if err := a.RemoveTrack("nobody", stray); err != nil {
		t.Fatalf("RemoveTrack without a session: got %v, want a no-op", err)
	}

	track := pcmuTestTrack(t, "music")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pumpTrack(ctx, track)

	sender, err := a.AddTrack(bId, track)
	if err != nil {
		t.Fatalf("AddTrack: %v", err)
	}
	if sender == nil {
		t.Fatal("AddTrack returned a nil sender")
	}
	// Re-adding the same track is a no-op returning its current sender.
	again, err := a.AddTrack(bId, track)
	if err != nil || again != sender {
		t.Fatalf("re-AddTrack: got (%v, %v), want the same sender and no error", again, err)
	}

	// The track arrives at b's handler, negotiated as PCMU audio.
	var remote *webrtc.TrackRemote
	select {
	case remote = <-tracks:
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for the remote track at b")
	}
	if remote.Kind() != webrtc.RTPCodecTypeAudio {
		t.Fatalf("the remote track's kind is %s, want audio", remote.Kind())
	}
	if codec := remote.Codec(); !strings.EqualFold(codec.MimeType, webrtc.MimeTypePCMU) {
		t.Fatalf("the remote track's codec is %s, want %s", codec.MimeType, webrtc.MimeTypePCMU)
	}

	// ...carrying the pump's 20 ms frames.
	if pkt := readOneRTP(t, remote); len(pkt.Payload) != 160 {
		t.Fatalf("the RTP payload is %d bytes, want 160 (20 ms of PCMU)", len(pkt.Payload))
	}

	// Removing works, and removing again is a no-op.
	if err := a.RemoveTrack(bId, track); err != nil {
		t.Fatalf("RemoveTrack: %v", err)
	}
	if err := a.RemoveTrack(bId, track); err != nil {
		t.Fatalf("RemoveTrack of a detached track: got %v, want a no-op", err)
	}
	if err := a.RemoveTrack(bId, stray); err != nil {
		t.Fatalf("RemoveTrack of a never-attached track: got %v, want a no-op", err)
	}

	// The track handler is a single registration, like a data channel
	// handler's label.
	assertPanics(t, "second HandleTrack", func() {
		b.HandleTrackFunc(func(context.Context, ss.ChannelId, ss.SubscriberId, *webrtc.TrackRemote, *webrtc.RTPReceiver) {})
	})
}

// TestHeadlessRTCClientTracksSurviveGlare is the media counterpart of
// TestHeadlessRTCClientSurvivesGlare: the polite client, already carrying
// a track, yields to the colliding offer by rebuilding its peer
// connection — and the rebuilt connection must still carry the track
// (the answer carries the audio m-line, and the music flows).
func TestHeadlessRTCClientTracksSurviveGlare(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot, _ := startTestClient(t, net, "bot", func(c *RTCClientConfiguration) {
		c.SubscriberId = "1-bot" // the smaller id: polite — the one that yields
	})
	bot.HandleDataChannel("dcmsg", newDCProbe().handler())
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
	peerTracks := make(chan *webrtc.TrackRemote, 4)
	peerPC.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) { peerTracks <- track })
	peerPC.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		init := webrtc.ICECandidateInit{}
		if candidate != nil {
			init = candidate.ToJSON()
		}
		sendC2C(&ss.ClientToClientEv{RTCICECandidate: &init})
	})

	// One loop drains the raw peer's inbound events: session descriptions
	// are scripted below, candidates are applied as they come.
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

	// Step 1: the bot discovers the peer and offers (its data channel).
	offer := nextDesc()
	if offer.Type != webrtc.SDPTypeOffer {
		t.Fatalf("the bot's first session description is %s, want an offer", offer.Type)
	}

	// The bot attaches its music track BEFORE the collision lands — the
	// hub note is synchronous, so the track is on record when the rebuild
	// happens — and its pump starts (writes drop until the track binds).
	music := pcmuTestTrack(t, "music")
	if _, err := bot.AddTrack("2-peer", music); err != nil {
		t.Fatalf("AddTrack: %v", err)
	}
	go pumpTrack(net.ctx, music)

	// The raw peer collides: its own offer — a data channel AND its own
	// audio track, so the bot's answer must carry an audio m-line.
	if _, err := peerPC.CreateDataChannel("raw", nil); err != nil {
		t.Fatalf("CreateDataChannel: %v", err)
	}
	if _, err := peerPC.AddTrack(pcmuTestTrack(t, "peer-music")); err != nil {
		t.Fatalf("peer AddTrack: %v", err)
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
	// hub — and answers the colliding offer. The answer must still carry
	// the audio m-line: the track was re-added to the fresh connection.
	answer := nextDesc()
	if answer.Type != webrtc.SDPTypeAnswer {
		t.Fatalf("the bot's reply to the collision is %s, want an answer", answer.Type)
	}
	if !strings.Contains(answer.SDP, "m=audio") {
		t.Fatalf("the answer lost the music track across the glare rebuild:\n%s", answer.SDP)
	}
	if err := peerPC.SetRemoteDescription(*answer); err != nil {
		t.Fatalf("SetRemoteDescription(answer): %v", err)
	}

	// Step 3: answer the bot's re-offers while the negotiation settles
	// (the re-added track re-fires negotiation-needed), as in the
	// data-channel glare test.
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

	// The music flows from the rebuilt connection: the raw peer's track
	// handler fires and a 20 ms frame reads back.
	var remote *webrtc.TrackRemote
	select {
	case remote = <-peerTracks:
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for the music track after the glare")
	}
	if pkt := readOneRTP(t, remote); len(pkt.Payload) != 160 {
		t.Fatalf("the RTP payload is %d bytes, want 160 (20 ms of PCMU)", len(pkt.Payload))
	}
}
