package rtpx

import (
	"fmt"
	"net"
	"sync"
)

// PortPool hands out RTP/RTCP socket pairs from a bounded range.
//
// RFC 3550 requires RTP on an even port and RTCP on the odd port above it, and
// firewalls are configured against a fixed range, so ports cannot simply be
// left to the operating system. The pool binds both sockets before returning
// them, which is the only reliable way to know a port is actually free:
// checking a used-set first still races with other processes on the host.
type PortPool struct {
	mu   sync.Mutex
	host string
	min  int
	max  int
	// next rotates through the range so a released port is not immediately
	// reused. Carriers frequently keep sending to a torn-down port for a few
	// seconds, and that stray audio must not land in a new call.
	next int
	used map[int]struct{}
}

// NewPortPool builds a pool over [min, max]. The range is treated as pairs, so
// min should be even.
func NewPortPool(host string, min, max int) *PortPool {
	if min%2 != 0 {
		min++
	}
	return &PortPool{
		host: host,
		min:  min,
		max:  max,
		next: min,
		used: make(map[int]struct{}),
	}
}

// Acquire binds and returns an RTP socket and its paired RTCP socket.
func (p *PortPool) Acquire() (rtpConn, rtcpConn *net.UDPConn, port int, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	total := (p.max - p.min) / 2
	for attempt := 0; attempt <= total; attempt++ {
		candidate := p.next
		p.next += 2
		if p.next > p.max-1 {
			p.next = p.min
		}
		if _, taken := p.used[candidate]; taken {
			continue
		}

		rtp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(p.host), Port: candidate})
		if err != nil {
			continue
		}
		rtcp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(p.host), Port: candidate + 1})
		if err != nil {
			// The RTP port was free but its RTCP partner was not; this pair is
			// unusable, so release and move on.
			_ = rtp.Close()
			continue
		}

		p.used[candidate] = struct{}{}
		return rtp, rtcp, candidate, nil
	}
	return nil, nil, 0, fmt.Errorf("rtpx: no free RTP port pair in %d-%d", p.min, p.max)
}

// Release returns a port pair to the pool. The caller closes the sockets.
func (p *PortPool) Release(port int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.used, port)
}

// InUse reports how many pairs are allocated, for capacity metrics.
func (p *PortPool) InUse() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.used)
}

// Capacity reports the total number of pairs the range can hold.
func (p *PortPool) Capacity() int { return (p.max - p.min) / 2 }
