package rtpx

import (
	"net"
	"testing"
	"time"
)

// TestDTMFCollectorDeduplicatesBurst is the single most important test in this
// package.
//
// One keypress is not one packet. It is a burst of packets repeated every
// packetization interval for as long as the key is held, plus three redundant
// end packets, and every packet in the burst shares the RTP timestamp of the
// key-down instant. A collector that reports per packet turns one "5" into a
// dozen, and on a PIN entry path that means an instantly locked account.
func TestDTMFCollectorDeduplicatesBurst(t *testing.T) {
	c := NewDTMFCollector(0)
	const ts = 48000

	var digits []rune
	// Eight key-down packets, growing in duration, all at one timestamp.
	for i := 1; i <= 8; i++ {
		payload := EncodeTelephoneEvent(TelephoneEvent{
			Event:    5,
			Volume:   10,
			Duration: uint16(i * 160),
		})
		if d, ok := c.Push(ts, payload); ok {
			digits = append(digits, d)
		}
	}
	// Three redundant end packets.
	for i := 0; i < 3; i++ {
		payload := EncodeTelephoneEvent(TelephoneEvent{
			Event: 5, End: true, Volume: 10, Duration: 1280,
		})
		if d, ok := c.Push(ts, payload); ok {
			digits = append(digits, d)
		}
	}

	if len(digits) != 1 {
		t.Fatalf("one keypress produced %d digits (%q), want exactly 1", len(digits), string(digits))
	}
	if digits[0] != '5' {
		t.Errorf("got %q, want '5'", digits[0])
	}
}

// TestDTMFCollectorAcceptsRepeatedDigit checks the other half: pressing the
// same key twice is two keypresses, distinguished only by timestamp. A PIN of
// "1122" depends on this.
func TestDTMFCollectorAcceptsRepeatedDigit(t *testing.T) {
	c := NewDTMFCollector(0)

	var digits []rune
	for press := 0; press < 2; press++ {
		ts := uint32(8000 + press*2400)
		for i := 0; i < 5; i++ {
			payload := EncodeTelephoneEvent(TelephoneEvent{Event: 1, Duration: uint16(i * 160)})
			if d, ok := c.Push(ts, payload); ok {
				digits = append(digits, d)
			}
		}
	}

	if string(digits) != "11" {
		t.Errorf("two presses of the same key gave %q, want \"11\"", string(digits))
	}
}

func TestDTMFCollectorFullPIN(t *testing.T) {
	c := NewDTMFCollector(0)
	want := "4071"

	var got []rune
	for i, digit := range want {
		code, ok := EventCode(digit)
		if !ok {
			t.Fatalf("no event code for %q", digit)
		}
		ts := uint32(1000 + i*1600)
		// Each press: several key-down packets then an end packet.
		for rep := 0; rep < 6; rep++ {
			payload := EncodeTelephoneEvent(TelephoneEvent{
				Event:    code,
				End:      rep == 5,
				Duration: uint16((rep + 1) * 160),
			})
			if d, ok := c.Push(ts, payload); ok {
				got = append(got, d)
			}
		}
	}

	if string(got) != want {
		t.Errorf("collected %q, want %q", string(got), want)
	}
}

// TestDTMFCollectorRejectsShortEvents covers the spurious sub-40ms bursts some
// gateways emit as line noise.
func TestDTMFCollectorRejectsShortEvents(t *testing.T) {
	c := NewDTMFCollector(40 * time.Millisecond)

	// 160 timestamp units at 8 kHz is 20 ms, under the threshold.
	short := EncodeTelephoneEvent(TelephoneEvent{Event: 3, Duration: 160})
	if _, ok := c.Push(500, short); ok {
		t.Error("accepted a 20ms event despite a 40ms minimum")
	}

	// An end packet reporting a full 100 ms must be accepted.
	long := EncodeTelephoneEvent(TelephoneEvent{Event: 3, End: true, Duration: 800})
	if _, ok := c.Push(500, long); !ok {
		t.Error("rejected a completed 100ms event")
	}
}

func TestTelephoneEventRoundTrip(t *testing.T) {
	cases := []TelephoneEvent{
		{Event: 0, Volume: 10, Duration: 160},
		{Event: 11, End: true, Volume: 63, Duration: 65535},
		{Event: 15, Volume: 0, Duration: 0},
	}

	for _, want := range cases {
		got, err := DecodeTelephoneEvent(EncodeTelephoneEvent(want))
		if err != nil {
			t.Fatalf("decode %+v: %v", want, err)
		}
		if got != want {
			t.Errorf("round trip gave %+v, want %+v", got, want)
		}
	}
}

func TestTelephoneEventDigitMapping(t *testing.T) {
	cases := map[uint8]rune{0: '0', 9: '9', 10: '*', 11: '#', 12: 'A', 15: 'D'}
	for code, want := range cases {
		got, ok := TelephoneEvent{Event: code}.Digit()
		if !ok || got != want {
			t.Errorf("event %d mapped to %q (ok=%v), want %q", code, got, ok, want)
		}
	}
	if _, ok := (TelephoneEvent{Event: 99}).Digit(); ok {
		t.Error("event 99 should not map to a digit")
	}
}

func TestDecodeTelephoneEventRejectsShortPayload(t *testing.T) {
	if _, err := DecodeTelephoneEvent([]byte{1, 2}); err == nil {
		t.Error("expected an error for a truncated payload")
	}
}

func TestPortPoolAllocatesEvenPairs(t *testing.T) {
	pool := NewPortPool("127.0.0.1", 41000, 41020)

	rtp, rtcp, port, err := pool.Acquire()
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer rtp.Close()
	defer rtcp.Close()

	if port%2 != 0 {
		t.Errorf("RTP port %d is odd; RFC 3550 requires an even port", port)
	}
	if got := rtcp.LocalAddr().(*net.UDPAddr).Port; got != port+1 {
		t.Errorf("RTCP port %d, want %d", got, port+1)
	}
	if pool.InUse() != 1 {
		t.Errorf("InUse = %d, want 1", pool.InUse())
	}

	pool.Release(port)
	if pool.InUse() != 0 {
		t.Errorf("InUse = %d after release, want 0", pool.InUse())
	}
}

// TestPortPoolExhaustion checks that running out of ports is reported rather
// than silently blocking a call.
func TestPortPoolExhaustion(t *testing.T) {
	// A two-port range holds exactly one pair.
	pool := NewPortPool("127.0.0.1", 41100, 41102)

	rtp, rtcp, _, err := pool.Acquire()
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	defer rtp.Close()
	defer rtcp.Close()

	if _, _, _, err := pool.Acquire(); err == nil {
		t.Error("expected exhaustion error from a single-pair pool")
	}
}
