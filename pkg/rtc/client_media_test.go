package rtc

// The media tests: the client's track support (AddTrack/RemoveTrack/
// HandleTrack), including track continuity when glare rollback resolves a
// collision on the original connection.

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

// audioMFormats extracts the payload-type list of the SDP's audio
// m-line — the codec pin's wire shape.
func audioMFormats(t *testing.T, sdp string) []string {
	t.Helper()
	for line := range strings.Lines(sdp) {
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "m=audio ") {
			fields := strings.Fields(line)
			if len(fields) < 4 {
				t.Fatalf("the audio m-line is malformed: %q", line)
			}
			return fields[3:]
		}
	}
	t.Fatalf("no audio m-line in the SDP:\n%s", sdp)
	return nil
}

// opusPinTestTrack builds an opus sample track — the shape the sip bot's
// pinned speaker track takes.
func opusPinTestTrack(t *testing.T, id string) *webrtc.TrackLocalStaticSample {
	t.Helper()
	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		id, "rtc-test")
	if err != nil {
		t.Fatalf("NewTrackLocalStaticSample: %v", err)
	}
	return track
}

// TestHeadlessRTCClientPinsTrackCodecs is the codec pin's proof
// (AddTrackWithCodecPreferences): the pinned track's m-line negotiates
// exactly the pinned codec list — opus and telephone-event here, the
// engine's opus PT 111 and tel-event's 126 — in the session's offers,
// in its answers to the peer's media re-offers, and across a glare
// rollback on the same connection. The raw peer drives the wire directly so the SDP
// itself is asserted; it registers the same codecs a browser would.
func TestHeadlessRTCClientPinsTrackCodecs(t *testing.T) {
	net := newClientTestNet(t, ss.DefaultSubscriberAging)
	bot, _ := startTestClient(t, net, "bot", func(c *RTCClientConfiguration) {
		c.SubscriberId = "1-bot" // the smaller id: polite — the one that yields
	})
	bot.HandleDataChannel("dcmsg", newDCProbe().handler())
	botId := waitRegistered(t, bot)

	// The raw peer: registers by hand and drives the wire protocol
	// directly, so the test reads every session description verbatim.
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

	// The raw peer's peer connection registers what a browser would: the
	// default codecs plus telephone-event.
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		t.Fatalf("RegisterDefaultCodecs: %v", err)
	}
	if err := m.RegisterCodec(TelephoneEventCodec, webrtc.RTPCodecTypeAudio); err != nil {
		t.Fatalf("register telephone-event: %v", err)
	}
	peerPC, err := webrtc.NewAPI(webrtc.WithMediaEngine(m)).NewPeerConnection(webrtc.Configuration{})
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
	// The pin: opus (a zero PT — the engine's 111) plus telephone-event.
	pin := []webrtc.RTPCodecParameters{
		{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}},
		TelephoneEventCodec,
	}
	wantFormats := []string{"111", "126"}

	// Step 1: the bot discovers the peer and offers (its data channel).
	offer := nextDesc()
	if offer.Type != webrtc.SDPTypeOffer {
		t.Fatalf("the bot's first session description is %s, want an offer", offer.Type)
	}

	// The bot attaches its speaker track with the pin BEFORE the
	// collision lands — the hub note is synchronous, so track and pin
	// are on record when the rebuild happens — and its pump starts.
	music := opusPinTestTrack(t, "music")
	if _, err := bot.AddTrackWithCodecPreferences("2-peer", music, pin); err != nil {
		t.Fatalf("AddTrackWithCodecPreferences: %v", err)
	}
	go pumpTrack(net.ctx, music)

	// The raw peer collides: its own offer — a data channel AND its own
	// audio track (the mic-join), so the bot's answer must carry an
	// audio m-line.
	if _, err := peerPC.CreateDataChannel("raw", nil); err != nil {
		t.Fatalf("CreateDataChannel: %v", err)
	}
	if _, err := peerPC.AddTrack(opusPinTestTrack(t, "peer-music")); err != nil {
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

	// Step 2: the bot rolls back on the original connection,
	// and answers the colliding offer. The answer must carry the
	// audio m-line pinned to exactly [opus, telephone-event]: the pin
	// was re-applied to the fresh connection's re-added track, and the
	// mic's m-line is answered with the pin.
	answer := nextDesc()
	if answer.Type != webrtc.SDPTypeAnswer {
		t.Fatalf("the bot's reply to the collision is %s, want an answer", answer.Type)
	}
	if got := audioMFormats(t, answer.SDP); !slices.Equal(got, wantFormats) {
		t.Fatalf("the answer's audio formats are %v, want the pin %v:\n%s", got, wantFormats, answer.SDP)
	}
	if err := peerPC.SetRemoteDescription(*answer); err != nil {
		t.Fatalf("SetRemoteDescription(answer): %v", err)
	}

	// Step 3: answer the bot's re-offers while the negotiation settles
	// (the re-added track re-fires negotiation-needed); every re-offer's
	// audio m-line must carry the pin as well.
	reofferWindow := time.After(500 * time.Millisecond)
	for done := false; !done; {
		select {
		case desc := <-descs:
			if desc.Type != webrtc.SDPTypeOffer {
				continue
			}
			if got := audioMFormats(t, desc.SDP); !slices.Equal(got, wantFormats) {
				t.Fatalf("a re-offer's audio formats are %v, want the pin %v:\n%s", got, wantFormats, desc.SDP)
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
	// handler fires with opus negotiated.
	var remote *webrtc.TrackRemote
	select {
	case remote = <-peerTracks:
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for the music track after the glare")
	}
	if codec := remote.Codec(); !strings.EqualFold(codec.MimeType, webrtc.MimeTypeOpus) {
		t.Fatalf("the remote track's codec is %s, want %s", codec.MimeType, webrtc.MimeTypeOpus)
	}
	readOneRTP(t, remote)
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

	// Step 2: the bot rolls back on the original connection,
	// and answers the colliding offer. The answer must still carry
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

func TestHeadlessRTCClientFreshPCRebindsSessionMediaAndChannels(t *testing.T) {
	for _, tc := range []struct {
		name                string
		offerHasApplication bool
	}{
		{name: "offerer announces channels", offerHasApplication: true},
		{name: "polite client recreates channels without an application m-line"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			factory := defaultPeerConnectionFactory(nil)
			original, err := factory(true)
			if err != nil {
				t.Fatalf("original factory: %v", err)
			}
			track := opusPinTestTrack(t, "music")
			pin := []webrtc.RTPCodecParameters{
				{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}},
				TelephoneEventCodec,
			}
			originalSender, err := original.AddTrack(track)
			if err != nil {
				_ = original.Close()
				t.Fatalf("original AddTrack: %v", err)
			}
			if err := pinTransceiverCodecs(original, originalSender, pin); err != nil {
				_ = original.Close()
				t.Fatalf("original pinTransceiverCodecs: %v", err)
			}
			originalDC, err := original.CreateDataChannel("dcmsg", nil)
			if err != nil {
				_ = original.Close()
				t.Fatalf("original CreateDataChannel: %v", err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			sess := &peerSession{
				peer: "2-peer", polite: true, ctx: ctx, cancel: cancel,
				in: make(chan *ss.SignallingEvent, 1), out: make(chan *ss.SignallingEvent, 1),
				calls: make(chan callDelivery, 1), pc: original,
				channels: map[string]*webrtc.DataChannel{"dcmsg": originalDC},
				tracks:   []sessionTrack{{track: track, sender: originalSender, codecs: pin}},
			}
			run := &clientRun{sessions: map[ss.SubscriberId]*peerSession{sess.peer: sess}}
			originalIn, originalOut, originalCalls := sess.in, sess.out, sess.calls
			type delivery struct {
				ctx context.Context
				dc  *webrtc.DataChannel
			}
			handled := make(chan delivery, 1)
			c := &HeadlessRTCClient{
				newPC: factory, logger: testLogger(t), serviceChan: make(chan clientNote, 1),
				handlers: map[string]DataChannelHandler{
					"dcmsg": DataChannelHandlerFunc(func(handlerCtx context.Context, _ ss.ChannelId, _ ss.SubscriberId, dc *webrtc.DataChannel) {
						handled <- delivery{ctx: handlerCtx, dc: dc}
					}),
				},
			}
			reply := make(chan rebuildReply, 1)
			c.handleRebuildNote(run, rebuildNote{
				sess: sess, offerHasApplication: tc.offerHasApplication, reply: reply,
			})
			r := <-reply
			if r.err != nil {
				_ = original.Close()
				cancel()
				t.Fatalf("handleRebuildNote: %v", r.err)
			}
			fresh := r.pc
			t.Cleanup(func() {
				cancel()
				_ = original.Close()
				_ = fresh.Close()
			})
			if fresh == nil || fresh == original || sess.pc != fresh {
				t.Fatal("the session did not adopt a distinct fresh peer connection")
			}
			if sess.ctx != ctx || ctx.Err() != nil {
				t.Fatal("rebuilding the PC canceled or replaced the session context")
			}
			if run.sessions[sess.peer] != sess || sess.in != originalIn ||
				sess.out != originalOut || sess.calls != originalCalls {
				t.Fatal("rebuilding the PC replaced the logical session or its queues")
			}
			if len(sess.tracks) != 1 || sess.tracks[0].track != track || sess.tracks[0].sender == originalSender {
				t.Fatal("the fresh PC did not rebind the track and update its sender")
			}
			offer, err := fresh.CreateOffer(nil)
			if err != nil {
				t.Fatalf("fresh CreateOffer: %v", err)
			}
			if got, want := audioMFormats(t, offer.SDP), []string{"111", "126"}; !slices.Equal(got, want) {
				t.Fatalf("fresh PC audio formats=%v, want restored codec pin %v", got, want)
			}

			if tc.offerHasApplication {
				if len(sess.channels) != 0 || len(handled) != 0 || strings.Contains(offer.SDP, "m=application") {
					t.Fatal("fresh PC created a duplicate local channel despite the offer's application m-line")
				}
			} else {
				freshDC := sess.channels["dcmsg"]
				if freshDC == nil || freshDC == originalDC || !strings.Contains(offer.SDP, "m=application") {
					t.Fatal("polite fresh PC did not recreate the known data-channel label")
				}
				select {
				case got := <-handled:
					if got.ctx != ctx || got.dc != freshDC {
						t.Fatal("recreated channel handler received the wrong session context or channel")
					}
				case <-time.After(testTimeout):
					t.Fatal("recreated channel was not delivered exactly once to its handler")
				}
			}

			before := sess.channels["dcmsg"]
			c.handleAnnouncedNote(run, announcedNote{sess: sess, dc: originalDC, source: original})
			if sess.channels["dcmsg"] != before || len(handled) != 0 {
				t.Fatal("a queued channel callback from the replaced PC affected the fresh session")
			}
		})
	}
}
