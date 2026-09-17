// Package rtpx implements the RTP media path for SIP legs: packetization,
// symmetric NAT latching, and RFC 4733 DTMF.
package rtpx

import (
	"encoding/binary"
	"errors"
	"time"
)

// RFC 4733 named telephone events. Indices 0-9 are the digits, then star,
// pound, and the four A-D keys that survive from the original DTMF spec.
var dtmfEvents = []rune{
	'0', '1', '2', '3', '4', '5', '6', '7', '8', '9',
	'*', '#', 'A', 'B', 'C', 'D',
}

// ErrNotDTMF is returned for a payload that is not a telephone-event.
var ErrNotDTMF = errors.New("rtpx: not a telephone-event payload")

// TelephoneEvent is the decoded RFC 4733 payload.
//
//	 0                   1                   2                   3
//	 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//	|     event     |E|R| volume    |          duration             |
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
type TelephoneEvent struct {
	Event    uint8
	End      bool
	Volume   uint8
	Duration uint16
}

// Digit maps the event code to its character.
func (e TelephoneEvent) Digit() (rune, bool) {
	if int(e.Event) >= len(dtmfEvents) {
		return 0, false
	}
	return dtmfEvents[e.Event], true
}

// DecodeTelephoneEvent parses a four-byte telephone-event payload.
func DecodeTelephoneEvent(b []byte) (TelephoneEvent, error) {
	if len(b) < 4 {
		return TelephoneEvent{}, ErrNotDTMF
	}
	return TelephoneEvent{
		Event:    b[0],
		End:      b[1]&0x80 != 0,
		Volume:   b[1] & 0x3F,
		Duration: binary.BigEndian.Uint16(b[2:4]),
	}, nil
}

// EncodeTelephoneEvent builds a four-byte payload. Volume is expressed as dBm0
// below full scale, so larger numbers are quieter; 10 is the usual choice.
func EncodeTelephoneEvent(e TelephoneEvent) []byte {
	b := make([]byte, 4)
	b[0] = e.Event
	b[1] = e.Volume & 0x3F
	if e.End {
		b[1] |= 0x80
	}
	binary.BigEndian.PutUint16(b[2:4], e.Duration)
	return b
}

// EventCode maps a digit character to its RFC 4733 event code.
func EventCode(digit rune) (uint8, bool) {
	for i, d := range dtmfEvents {
		if d == digit {
			return uint8(i), true
		}
	}
	return 0, false
}

// DTMFCollector turns a stream of telephone-event packets into discrete
// keypresses.
//
// This deduplication is the whole point. One keypress is transmitted as a
// burst of packets, one per packetization interval for as long as the key is
// held, plus three redundant end-of-event packets. Every packet in that burst
// shares the RTP timestamp of the key-down instant. A receiver that reports a
// digit per packet turns a single "5" into "5555555555", which on a PIN entry
// path means a locked account.
//
// So the collector keys on the event's RTP timestamp and reports each distinct
// timestamp once. A repeat of the same digit is a genuinely new keypress and
// carries a new timestamp, so double-tapping still works.
type DTMFCollector struct {
	haveLast bool
	lastTS   uint32
	lastCode uint8

	// startedAt supports the minimum-duration filter that rejects the very
	// short bursts some gateways emit as line noise.
	startedAt time.Time
	minDur    time.Duration
}

// NewDTMFCollector builds a collector. minDuration rejects events shorter than
// the given time; zero accepts everything.
func NewDTMFCollector(minDuration time.Duration) *DTMFCollector {
	return &DTMFCollector{minDur: minDuration}
}

// Push feeds one telephone-event packet and returns the digit if this packet
// completes a new keypress.
func (c *DTMFCollector) Push(timestamp uint32, payload []byte) (rune, bool) {
	ev, err := DecodeTelephoneEvent(payload)
	if err != nil {
		return 0, false
	}

	// A new timestamp, or a different digit at the same timestamp (which some
	// gateways produce when keys are pressed in quick succession), starts a new
	// keypress.
	isNew := !c.haveLast || timestamp != c.lastTS || ev.Event != c.lastCode
	if !isNew {
		return 0, false
	}

	digit, ok := ev.Digit()
	if !ok {
		return 0, false
	}

	// Duration is carried in RTP clock units, so at 8 kHz one millisecond is
	// eight ticks.
	if c.minDur > 0 {
		durMs := time.Duration(ev.Duration/8) * time.Millisecond
		// Only the end packet carries the full duration; a key-down packet
		// reports the duration so far. Accept on the end packet, or as soon as
		// an in-progress event has already exceeded the minimum.
		if !ev.End && durMs < c.minDur {
			return 0, false
		}
	}

	c.haveLast = true
	c.lastTS = timestamp
	c.lastCode = ev.Event
	c.startedAt = time.Now()
	return digit, true
}

// Reset clears collector state, used when a leg is re-INVITEd.
func (c *DTMFCollector) Reset() {
	c.haveLast = false
	c.lastTS = 0
	c.lastCode = 0
}
