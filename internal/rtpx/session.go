package rtpx

import (
	"errors"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"

	"github.com/pedango/pedango/internal/audio"
)

// Session is one bidirectional RTP media path for a SIP leg.
type Session struct {
	conn     *net.UDPConn
	rtcpConn *net.UDPConn
	pool     *PortPool
	port     int

	codec   audio.Codec
	audioPT uint8
	dtmfPT  uint8

	ssrc uint32
	seq  atomic.Uint32 // held wide, truncated to 16 bits on the wire
	ts   atomic.Uint32

	mu sync.RWMutex
	// remote is where packets are sent. It starts as the address from the
	// peer's SDP and may be replaced by latching.
	remote *net.UDPAddr
	// sdpRemote is the address the peer advertised, kept so a re-latch can be
	// validated against it.
	sdpRemote *net.UDPAddr
	latched   bool
	lastRecv  time.Time

	symmetric bool
	dtmf      *DTMFCollector

	onAudio func(timestamp uint32, pcm []int16)
	onDTMF  func(digit rune)

	// sendingDTMF suppresses audio while a telephone-event burst is in flight,
	// so the digit is not mixed with speech.
	sendingDTMF atomic.Bool

	stats     Stats
	closeOnce sync.Once
	done      chan struct{}

	// decodeBuf and encodeBuf are reused per packet to keep the media path
	// allocation-free.
	decodeBuf []int16
	encodeBuf []byte
}

// Stats counts packet flow for diagnostics.
type Stats struct {
	PacketsSent     atomic.Uint64
	PacketsReceived atomic.Uint64
	BytesSent       atomic.Uint64
	BytesReceived   atomic.Uint64
	Rejected        atomic.Uint64
	DTMFReceived    atomic.Uint64
	Relatches       atomic.Uint64
}

// Options configures a session.
type Options struct {
	Codec  audio.Codec
	DTMFPT uint8
	// Symmetric enables latching to the source address of received packets.
	Symmetric bool
}

// relatchGrace is how long the latched source may go silent before a different
// source is allowed to take over. Latching to any arriving packet immediately
// would let an attacker who guesses the port inject audio into a live call;
// requiring the current source to have gone quiet first closes that window to
// cases where the call is already broken.
const relatchGrace = 2 * time.Second

// New creates a session on a freshly acquired port pair.
func New(pool *PortPool, o Options) (*Session, error) {
	conn, rtcpConn, port, err := pool.Acquire()
	if err != nil {
		return nil, err
	}

	s := &Session{
		conn:      conn,
		rtcpConn:  rtcpConn,
		pool:      pool,
		port:      port,
		codec:     o.Codec,
		audioPT:   o.Codec.PayloadType(),
		dtmfPT:    o.DTMFPT,
		ssrc:      rand.Uint32(),
		symmetric: o.Symmetric,
		dtmf:      NewDTMFCollector(40 * time.Millisecond),
		done:      make(chan struct{}),
		decodeBuf: make([]int16, 0, 480),
		encodeBuf: make([]byte, 0, 480),
	}
	// Random start values are required by RFC 3550 so that a stream cannot be
	// trivially predicted and injected into.
	s.seq.Store(uint32(rand.Intn(1 << 15)))
	s.ts.Store(rand.Uint32())

	return s, nil
}

// SetCallbacks wires the session to its leg. It must be called before Start,
// which is why reading does not begin in New: the leg cannot be constructed
// until its transport exists, so the callbacks are necessarily set afterwards
// and starting the read loop early would race with them.
func (s *Session) SetCallbacks(onAudio func(uint32, []int16), onDTMF func(rune)) {
	s.onAudio = onAudio
	s.onDTMF = onDTMF
}

// Start begins receiving. Calling it twice is a programming error.
func (s *Session) Start() {
	go s.readLoop()
	go s.drainRTCP()
}

// LocalPort is the RTP port to advertise in SDP.
func (s *Session) LocalPort() int { return s.port }

// SSRC reports the synchronisation source this session sends with.
func (s *Session) SSRC() uint32 { return s.ssrc }

// SetRemote points the session at the address from the peer's SDP.
func (s *Session) SetRemote(addr *net.UDPAddr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sdpRemote = addr
	if !s.latched {
		s.remote = addr
	}
}

// RemoteAddr reports where media is currently being sent.
func (s *Session) RemoteAddr() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.remote == nil {
		return ""
	}
	return s.remote.String()
}

// WriteFrame packetizes and sends one 20 ms frame of linear PCM.
func (s *Session) WriteFrame(f audio.Frame) error {
	s.mu.RLock()
	remote := s.remote
	s.mu.RUnlock()

	if remote == nil {
		// Media has not been negotiated yet. The timestamp still advances so
		// the stream stays aligned to the wall clock once it does.
		s.ts.Add(uint32(len(f)))
		return nil
	}
	if s.sendingDTMF.Load() {
		s.ts.Add(uint32(len(f)))
		return nil
	}

	s.encodeBuf = s.codec.Encode(s.encodeBuf, f)

	pkt := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    s.audioPT,
			SequenceNumber: uint16(s.seq.Add(1)),
			Timestamp:      s.ts.Add(uint32(len(f))),
			SSRC:           s.ssrc,
		},
		Payload: s.encodeBuf,
	}

	raw, err := pkt.Marshal()
	if err != nil {
		return err
	}
	n, err := s.conn.WriteToUDP(raw, remote)
	if err != nil {
		return err
	}
	s.stats.PacketsSent.Add(1)
	s.stats.BytesSent.Add(uint64(n))
	return nil
}

// SendDTMF transmits a digit as an RFC 4733 telephone-event burst.
//
// The burst is the protocol: one packet per packetization interval for the
// duration of the "keypress", every one carrying the timestamp of the key-down
// instant and a growing duration field, followed by three copies of the end
// packet. The redundancy exists because losing the end packet would leave the
// far end holding the tone.
func (s *Session) SendDTMF(digit rune, duration time.Duration) error {
	code, ok := EventCode(digit)
	if !ok {
		return errors.New("rtpx: not a DTMF digit")
	}
	s.mu.RLock()
	remote := s.remote
	s.mu.RUnlock()
	if remote == nil {
		return errors.New("rtpx: no remote media address")
	}
	if duration <= 0 {
		duration = 160 * time.Millisecond
	}

	go s.dtmfBurst(code, duration, remote)
	return nil
}

func (s *Session) dtmfBurst(code uint8, duration time.Duration, remote *net.UDPAddr) {
	s.sendingDTMF.Store(true)
	defer s.sendingDTMF.Store(false)

	// Every packet in the burst repeats the key-down timestamp.
	eventTS := s.ts.Load()
	steps := int(duration / (audio.FrameDurationMs * time.Millisecond))
	if steps < 1 {
		steps = 1
	}

	ticker := time.NewTicker(audio.FrameDurationMs * time.Millisecond)
	defer ticker.Stop()

	for i := 1; i <= steps; i++ {
		select {
		case <-s.done:
			return
		case <-ticker.C:
		}
		s.writeEvent(TelephoneEvent{
			Event:    code,
			Volume:   10,
			Duration: uint16(i * audio.FrameDurationMs * audio.SamplesPerMs),
		}, eventTS, remote)
	}

	end := TelephoneEvent{
		Event:    code,
		End:      true,
		Volume:   10,
		Duration: uint16(steps * audio.FrameDurationMs * audio.SamplesPerMs),
	}
	for i := 0; i < 3; i++ {
		s.writeEvent(end, eventTS, remote)
	}

	// Audio resumes after the space the digit occupied.
	s.ts.Add(uint32(steps * audio.FrameSamples))
}

func (s *Session) writeEvent(e TelephoneEvent, timestamp uint32, remote *net.UDPAddr) {
	pkt := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    s.dtmfPT,
			SequenceNumber: uint16(s.seq.Add(1)),
			Timestamp:      timestamp,
			SSRC:           s.ssrc,
		},
		Payload: EncodeTelephoneEvent(e),
	}
	if raw, err := pkt.Marshal(); err == nil {
		if n, err := s.conn.WriteToUDP(raw, remote); err == nil {
			s.stats.PacketsSent.Add(1)
			s.stats.BytesSent.Add(uint64(n))
		}
	}
}

func (s *Session) readLoop() {
	buf := make([]byte, 1500)
	pkt := &rtp.Packet{}

	for {
		select {
		case <-s.done:
			return
		default:
		}

		n, src, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			// A closed socket ends the loop; anything else is transient.
			select {
			case <-s.done:
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		if n < 12 {
			continue
		}
		if !s.acceptSource(src) {
			s.stats.Rejected.Add(1)
			continue
		}
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			continue
		}

		s.stats.PacketsReceived.Add(1)
		s.stats.BytesReceived.Add(uint64(n))
		s.dispatch(pkt)
	}
}

// acceptSource implements symmetric RTP latching with an anti-injection guard.
func (s *Session) acceptSource(src *net.UDPAddr) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()

	if !s.symmetric {
		s.lastRecv = now
		return true
	}

	if !s.latched {
		s.remote = src
		s.latched = true
		s.lastRecv = now
		return true
	}

	if s.remote != nil && s.remote.IP.Equal(src.IP) && s.remote.Port == src.Port {
		s.lastRecv = now
		return true
	}

	// A different source is only honoured once the latched one has gone quiet,
	// which happens when a carrier moves a call between media servers.
	if now.Sub(s.lastRecv) > relatchGrace {
		s.remote = src
		s.lastRecv = now
		s.stats.Relatches.Add(1)
		return true
	}
	return false
}

func (s *Session) dispatch(pkt *rtp.Packet) {
	switch pkt.PayloadType {
	case s.dtmfPT:
		if digit, ok := s.dtmf.Push(pkt.Timestamp, pkt.Payload); ok {
			s.stats.DTMFReceived.Add(1)
			if s.onDTMF != nil {
				s.onDTMF(digit)
			}
		}
	case s.audioPT:
		if s.onAudio == nil {
			return
		}
		s.decodeBuf = s.codec.Decode(s.decodeBuf, pkt.Payload)
		s.onAudio(pkt.Timestamp, s.decodeBuf)
	default:
		// Comfort noise (PT 13) and anything else unnegotiated is ignored
		// rather than decoded as audio, which would be heard as a burst.
	}
}

// drainRTCP reads and discards RTCP. The socket must be serviced even though
// Pedango does not act on reports: on some platforms an unread UDP socket
// generates ICMP port-unreachable replies that make carriers tear the call
// down.
func (s *Session) drainRTCP() {
	buf := make([]byte, 1500)
	for {
		select {
		case <-s.done:
			return
		default:
		}
		if _, _, err := s.rtcpConn.ReadFromUDP(buf); err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
		}
	}
}

// Stats returns a snapshot of packet counters.
func (s *Session) Snapshot() map[string]uint64 {
	return map[string]uint64{
		"packets_sent":     s.stats.PacketsSent.Load(),
		"packets_received": s.stats.PacketsReceived.Load(),
		"bytes_sent":       s.stats.BytesSent.Load(),
		"bytes_received":   s.stats.BytesReceived.Load(),
		"rejected":         s.stats.Rejected.Load(),
		"dtmf_received":    s.stats.DTMFReceived.Load(),
		"relatches":        s.stats.Relatches.Load(),
	}
}

// Close shuts down the session and returns its ports to the pool.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		close(s.done)
		_ = s.conn.Close()
		_ = s.rtcpConn.Close()
		s.pool.Release(s.port)
	})
	return nil
}
