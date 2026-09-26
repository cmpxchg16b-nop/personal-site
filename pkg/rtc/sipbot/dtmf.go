package sipbot

// This file is the relay's DTMF vocabulary — RFC 4733 telephone-event.
// A DTMF event rides the same RTP session (same SSRC, shared sequence
// space) as the audio it interrupts, distinguished only by its
// negotiated dynamic payload type, so the bot never sees a separate
// track: the events surface inside the mic's (and the dialog's) read
// stream, and parsing them is the application's own work — pion has no
// high-level DTMF API. The payload is four bytes:
//
//	| event |E|R| volume |          duration            |
//
// event is the key (0-9 the digits, 10 = *, 11 = #, 12-15 = A-D); E
// marks the event's end; duration the elapsed length in clock units
// (8000 Hz here, independent of the audio codec's rate). One key press
// is a packet series — a start (marker bit set), updates with growing
// durations, and an END packet retransmitted three times against loss —
// all sharing the event's start timestamp, which is what the relay's
// dedupe keys on.
//
// Only the WebRTC-leg → SIP-leg direction is transported: the browser
// user's key presses reach the SIP network (IVRs live there). SIP-side
// events are dropped at the relay (call.go's pumpSIPToBrowser) and
// never reach the browser — see docs/sip-bot.md §8.

import (
	"encoding/binary"
	"strings"

	"github.com/pion/webrtc/v4"
)

// telephoneEventPT answers the negotiated telephone-event payload type
// of the peer connection the receiver belongs to, or false when the
// negotiation did not settle on telephone-event (the browser leg then
// sends no DTMF at all). Never hardcode the PT: pion echoes the
// browser's choice when it answers and uses the registered one when it
// offers.
func telephoneEventPT(receiver *webrtc.RTPReceiver) (uint8, bool) {
	for _, codec := range receiver.GetParameters().Codecs {
		if strings.EqualFold(codec.MimeType, "audio/telephone-event") {
			return uint8(codec.PayloadType), true
		}
	}
	return 0, false
}

// dtmfEvent is one parsed RFC 4733 event packet's payload.
type dtmfEvent struct {
	code     uint8
	end      bool
	duration uint16 // clock units at 8000 Hz (duration/8 = ms)
}

// parseDTMFEvent decodes the 4-byte event payload; false for a
// truncated one.
func parseDTMFEvent(payload []byte) (dtmfEvent, bool) {
	if len(payload) < 4 {
		return dtmfEvent{}, false
	}
	return dtmfEvent{
		code:     payload[0],
		end:      payload[1]&0x80 != 0,
		duration: binary.BigEndian.Uint16(payload[2:4]),
	}, true
}

// digit maps the event's code to its key; false for a code outside the
// dial pad's (16 and up — flash, fax/modem tones — are not the pad's
// business).
func (e dtmfEvent) digit() (rune, bool) {
	switch {
	case e.code <= 9:
		return rune('0' + e.code), true
	case e.code == 10:
		return '*', true
	case e.code == 11:
		return '#', true
	case e.code <= 15:
		return rune('A' + e.code - 12), true
	}
	return 0, false
}

// dtmfDeduper drops the END packet's retransmissions: the three copies
// share the event's start timestamp, so a (code, timestamp) repeat is
// never a new press. Key presses do not interleave (a sender finishes
// one tone before starting the next), so the single last-END pair is
// the whole state. The zero value is ready.
type dtmfDeduper struct {
	seen  bool
	code  uint8
	endTS uint32
}

// freshEnd reports whether an END packet with this code and timestamp is
// a new key press (and remembers it). Only ENDs pass through here —
// start/update packets are not the relay's cue (it acts on a complete
// key press, never on a press still in flight).
func (d *dtmfDeduper) freshEnd(code uint8, ts uint32) bool {
	if d.seen && d.code == code && d.endTS == ts {
		return false
	}
	d.seen, d.code, d.endTS = true, code, ts
	return true
}
