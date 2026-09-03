package e2e

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"personal-site/pkg/models/ss"
	"personal-site/pkg/rtc"
)

// startVisitorProbe brings up one headless client over the real WS endpoint
// authenticated by jwtCookie — the test's stand-in for one browser page
// incarnation, registering exactly like the browser does: an empty
// subscriber id, so the SS assigns a fresh one. The returned cancel tears
// the incarnation down abruptly: the WS closes with no chance to say
// goodbye, exactly a closed browser tab (there is no unregister; the
// registration outlives the connection until it ages out).
func startVisitorProbe(
	t *testing.T,
	baseURL, jwtCookie string,
	opened chan<- *webrtc.DataChannel,
) (context.CancelFunc, *rtc.HeadlessRTCClient) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
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
	})
	transport := &rtc.WebSocketSignallingTransport{
		URL:    "ws" + strings.TrimPrefix(baseURL, "http") + "/api/ss/ws",
		Header: http.Header{"Cookie": []string{"jwt=" + jwtCookie}},
	}
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
	return cancel, client
}

// TestSSEchoBotPageReload pins the browser-reload flow, which used to
// deadlock: the browser re-registered the subscriber id stored in
// localStorage, the SS treated that as a same-tuple refresh (the member
// listing never observed a leave), and the bot kept its stale — dead —
// peer session for that id. The bot, the polite peer, had already
// negotiated and never offered again; the fresh impolite incarnation
// waited for an offer that was never coming (its peer connection sat at
// signalingState "stable" and ICE state "new" forever).
//
// The fix is an incarnation model on both ends: the browser registers a
// FRESH subscriber id on every page load (registerSubscriber), and the SS
// evicts the tuple's previous registration the moment its successor
// registers (handleRegister), so the incarnation change is immediately
// visible to member listings and the bot builds a fresh session — with a
// fresh offer — for the new id.
func TestSSEchoBotPageReload(t *testing.T) {
	baseURL := startEchoBotServer(t)
	jwtCookie := loginAsVisitor(t, baseURL+"/api/login/visitor")
	if jwtCookie == "" {
		t.Fatal("visitor login did not set a jwt cookie")
	}

	// Incarnation 1: connect and reach "connected" (the bot, the polite
	// peer, creates the messaging channel and offers).
	opened := make(chan *webrtc.DataChannel, 4)
	cancel1, probe1 := startVisitorProbe(t, baseURL, jwtCookie, opened)
	select {
	case <-opened:
	case <-time.After(20 * time.Second):
		t.Fatal("incarnation 1: the messaging data channel did not open")
	}
	firstId := probe1.SubscriberId()
	if firstId == "" {
		t.Fatal("incarnation 1 is not registered")
	}

	// The browser closes — abruptly, mid-membership.
	cancel1()
	time.Sleep(200 * time.Millisecond)

	// ...and reopens immediately: the same session cookie (the same
	// (userId, userSessionId) tuple), a fresh incarnation registering a
	// fresh subscriber id, exactly like the fixed browser.
	opened2 := make(chan *webrtc.DataChannel, 4)
	cancel2, probe2 := startVisitorProbe(t, baseURL, jwtCookie, opened2)
	defer cancel2()
	waitFor(t, 10*time.Second, "incarnation 2 to register", func() bool {
		return probe2.SubscriberId() != ""
	})
	secondId := probe2.SubscriberId()
	if secondId == firstId {
		t.Fatalf("incarnation 2 registered the dead incarnation's id %s; want a fresh one", secondId)
	}

	// The bot sees the old member go and the new one arrive, and — still
	// the polite peer — offers to the fresh incarnation: the channel
	// opens, the reload is connected again.
	select {
	case <-opened2:
	case <-time.After(20 * time.Second):
		t.Fatalf(
			"incarnation 2 (%s) never connected: no data channel within 20s (incarnation 1 was %s)",
			secondId, firstId,
		)
	}
}
