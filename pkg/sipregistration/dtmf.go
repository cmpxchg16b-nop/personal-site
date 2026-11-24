package sipregistration

import "encoding/binary"

// dtmfClockRate is the RFC 4733 rate used by sip-bot and common SIP IVRs.
// Keeping the SIP leg on this rate also lets diago's DTMF writer re-originate
// events instead of mixing browser audio and event timestamp timelines.
const dtmfClockRate = 8000

type dtmfEvent struct {
	code     uint8
	end      bool
	duration uint16
}

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
	default:
		return 0, false
	}
}

// dtmfDeduper collapses the three END retransmissions of one browser key
// press. The packets share the event's code and start timestamp.
type dtmfDeduper struct {
	seen  bool
	code  uint8
	endTS uint32
}

func (d *dtmfDeduper) freshEnd(code uint8, timestamp uint32) bool {
	if d.seen && d.code == code && d.endTS == timestamp {
		return false
	}
	d.seen, d.code, d.endTS = true, code, timestamp
	return true
}
