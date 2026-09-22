package sipbot

// This file is one call: the B2BUA's meeting point. A peerCall joins
//
//   - the webrtc-leg — the in-band dialog toward the browser (the
//     callId) and the bot's opus track, attached to the pair's peer
//     connection when the browser answers;
//   - the sip-leg — the diago INVITE dialog toward the callee, and the
//     negotiated codec its answer settles;
//
// and relays media between them with two pump goroutines — one per
// direction — each a read → (maybe transcode) → write loop (the codec
// matrix is transcode.go's). The pumps are self-paced by the arriving
// RTP: no tickers, unlike the music bot there is no free-running
// source.
//
// Concurrency: the call's fields are either set before the state is
// published (target, track, w — and callId before any watcher is armed)
// or gathered under mu — the SIP dial goroutine, the callee's hangup
// watcher, and the peer's serialized dcmsg goroutine genuinely meet on
// them, so a small mutex is the honest tool (the music bot needs none
// only because every touch of its state is on the channel goroutine).
// The bridges and scratch buffers below are owned by their pump
// goroutine alone.

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/emiago/diago"
	"github.com/emiago/diago/media"
	"github.com/pion/webrtc/v4"
	pionmedia "github.com/pion/webrtc/v4/pkg/media"

	"personal-site/pkg/rtc/msg_handler"
)

// callPhase is where the call's webrtc-leg stands (the sip-leg's phase
// is the dialog's own).
type callPhase int

const (
	// phaseRinging: the bot INVITEd the browser and waits for its answer.
	phaseRinging callPhase = iota
	// phaseActive: the browser answered; the opus track is attached.
	phaseActive
)

// peerCall is one chat user's call state.
type peerCall struct {
	// Set before the state is published, read-only after.
	target string
	track  *webrtc.TrackLocalStaticSample // the bot's opus track toward the browser
	w      msg_handler.ResponseWriter     // the /call invocation's writer, captured for the SIP leg's async answers (threaded on the command)
	callId string                         // the webrtc-leg dialog's id

	// dialCancel aborts an unanswered diago INVITE (the user hung up
	// first, /unregister replaced the account, the session ended).
	dialCancel context.CancelFunc

	mu     sync.Mutex
	phase  callPhase
	dialog *diago.DialogClientSession // nil until the callee answers
	relay  *relay                     // nil until the callee answers
	mic    *webrtc.TrackRemote        // nil until the browser's mic arrives
	micOn  bool                       // the browser→SIP pump is running
	ended  bool                       // an end path ran; later ones no-op
}

// relay is the call's media bridge: the two transcoders and the pumps'
// lifetime. stop ends the transcoding state and bars new pump starts;
// pumps already blocked on a read die with their leg (the dialog's
// close, the track's end) — the music bot's drainTrack has the same
// shape.
type relay struct {
	toBrowser *toBrowserBridge
	toSIP     *toSIPBridge

	dialog   *diago.DialogClientSession
	track    *webrtc.TrackLocalStaticSample
	stopCh   chan struct{}
	stopOnce sync.Once
}

// errCallEnded is setDialog's answer when the call already ended while
// the dial was in flight — the caller hangs the answered dialog up and
// says nothing.
var errCallEnded = errors.New("sipbot: the call ended while the callee was being rung")

// setDialog arms the answered sip-leg: the relay is built from the
// negotiated codec and the SIP→browser pump starts. errCallEnded when
// the call already ended (the caller hangs the dialog up instead); any
// other error is a relay-construction failure — the negotiated codec is
// not relayable, or the transcoder could not be built (a pure-Go build
// facing G.711) — and the caller ends the call saying why.
func (c *peerCall) setDialog(dialog *diago.DialogClientSession) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ended {
		return errCallEnded
	}
	r, err := newRelay(dialog, c.track)
	if err != nil {
		return err
	}
	c.dialog = dialog
	c.relay = r
	go r.pumpSIPToBrowser()
	if c.mic != nil {
		c.startMicPumpLocked()
	}
	return nil
}

// setMic notes the browser's mic track; the browser→SIP pump starts
// once both it and the answered dialog exist.
func (c *peerCall) setMic(track *webrtc.TrackRemote) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mic = track
	if c.relay != nil && !c.micOn {
		c.startMicPumpLocked()
	}
}

// startMicPumpLocked starts the browser→SIP pump. mu held.
func (c *peerCall) startMicPumpLocked() {
	c.micOn = true
	go c.relay.pumpBrowserToSIP(c.mic)
}

// activate marks the webrtc-leg answered; false when the call ended
// meanwhile or the phase was already past ringing.
func (c *peerCall) activate() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ended || c.phase != phaseRinging {
		return false
	}
	c.phase = phaseActive
	return true
}

// end runs the call's end exactly once, answering its snapshot for the
// caller's follow-up (which legs to tell, what to hang up): the dial is
// aborted, the relay stopped, the state marked. The dialog's own BYE is
// the caller's — it knows the context (and must not block on it).
func (c *peerCall) end() (dialog *diago.DialogClientSession, phase callPhase, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ended {
		return nil, 0, false
	}
	c.ended = true
	c.dialCancel()
	if c.relay != nil {
		c.relay.stop()
	}
	return c.dialog, c.phase, true
}

// snapshot reports the call's current phase and dialog — for end paths
// deciding which termination verb each leg speaks.
func (c *peerCall) snapshot() (dialog *diago.DialogClientSession, phase callPhase) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dialog, c.phase
}

// newRelay builds the media bridge for the answered dialog: the
// negotiated codec read off the dialog's media properties, the two
// transcoders.
func newRelay(dialog *diago.DialogClientSession, track *webrtc.TrackLocalStaticSample) (*relay, error) {
	props := diago.MediaProps{}
	if _, err := dialog.AudioReader(diago.WithAudioReaderMediaProps(&props)); err != nil {
		return nil, err
	}
	toBrowser, err := newToBrowserBridge(props.Codec)
	if err != nil {
		return nil, err
	}
	toSIP, err := newToSIPBridge(props.Codec)
	if err != nil {
		return nil, err
	}
	return &relay{
		toBrowser: toBrowser,
		toSIP:     toSIP,
		dialog:    dialog,
		track:     track,
		stopCh:    make(chan struct{}),
	}, nil
}

// stop ends the relay's transcoding state; safe to call repeatedly.
func (r *relay) stop() {
	r.stopOnce.Do(func() { close(r.stopCh) })
}

// stopped reports whether the relay was stopped — a pump start checks
// it to not run on a dead call.
func (r *relay) stopped() bool {
	select {
	case <-r.stopCh:
		return true
	default:
		return false
	}
}

// pumpSIPToBrowser relays the callee's audio to the browser: sip-leg
// payloads read off the dialog, converted, written to the call's opus
// track. The pump is self-paced by the arriving RTP; while the track is
// unbound (the browser still rings) pion drops the writes — the music
// bot's "empty room". It dies on the dialog's close (the callee's BYE,
// the call's hangup) or a conversion error.
func (r *relay) pumpSIPToBrowser() {
	if r.stopped() {
		return
	}
	reader, err := r.dialog.AudioReader()
	if err != nil {
		return
	}
	buf := make([]byte, media.RTPBufSize)
	for {
		n, err := reader.Read(buf)
		if err != nil {
			return // the dialog ended
		}
		if r.stopped() {
			return
		}
		werr := r.toBrowser.put(buf[:n], func(data []byte, dur time.Duration) error {
			return r.track.WriteSample(pionmedia.Sample{Data: data, Duration: dur})
		})
		if werr != nil {
			return
		}
	}
}

// pumpBrowserToSIP relays the browser's mic to the callee: opus packets
// read off the mic's remote track, converted, written to the dialog.
// Paced by the browser's packets; it dies on the track's end (the
// peer's teardown, the session's close) or a conversion error.
func (r *relay) pumpBrowserToSIP(mic *webrtc.TrackRemote) {
	if r.stopped() {
		return
	}
	writer, err := r.dialog.AudioWriter()
	if err != nil {
		return
	}
	for {
		pkt, _, err := mic.ReadRTP()
		if err != nil {
			return // the track or the connection ended
		}
		if r.stopped() {
			return
		}
		werr := r.toSIP.put(pkt.Payload, func(data []byte) error {
			_, err := writer.Write(data)
			return err
		})
		if werr != nil {
			return
		}
	}
}
