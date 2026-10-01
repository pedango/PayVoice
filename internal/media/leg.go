package media

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/pedango/PayVoice/internal/audio"
)

// Transport is the network side of a leg. Both the SIP/RTP and the WebRTC
// implementations satisfy it, which is what lets the mixer treat a phone call
// and a browser call as the same thing.
type Transport interface {
	// WriteFrame sends one 20 ms frame of 8 kHz linear PCM to the peer. It
	// must not block the media clock; implementations drop on overflow.
	WriteFrame(audio.Frame) error
	// SendDTMF emits an RFC 4733 telephone-event for the digit.
	SendDTMF(digit rune, duration time.Duration) error
	// Kind reports "sip" or "webrtc".
	Kind() string
	// RemoteAddr is a human-readable peer description for diagnostics.
	RemoteAddr() string
	// Close releases sockets and signalling state.
	Close() error
}

// LegState is the lifecycle of a call leg.
type LegState int32

const (
	// LegStateNew is a leg that exists but has no media yet.
	LegStateNew LegState = iota
	// LegStateRinging is an inbound call awaiting answer, or an outbound call
	// awaiting pickup.
	LegStateRinging
	// LegStateAnswered means media is flowing.
	LegStateAnswered
	// LegStateEnded is terminal.
	LegStateEnded
)

// String renders the state for API responses.
func (s LegState) String() string {
	switch s {
	case LegStateRinging:
		return "ringing"
	case LegStateAnswered:
		return "answered"
	case LegStateEnded:
		return "ended"
	default:
		return "new"
	}
}

// Direction records who placed the call.
type Direction string

// Call directions.
const (
	// Inbound is a call arriving at PayVoice.
	Inbound Direction = "inbound"
	// Outbound is a call PayVoice placed.
	Outbound Direction = "outbound"
)

// Leg is one call: a phone line over SIP/RTP, or a browser over WebRTC.
type Leg struct {
	ID        string
	Kind      string
	Direction Direction
	From      string
	To        string
	CreatedAt time.Time

	// AppRef is an opaque identifier the application attaches when it creates
	// the leg, echoed on every event so the application can correlate without
	// keeping its own map.
	AppRef string

	state    atomic.Int32
	answered atomic.Int64 // unix nanos, 0 until answered

	mu        sync.RWMutex
	transport Transport
	room      *Room

	// jb holds received audio; rx is the frame pulled from it each tick and is
	// also the frame subtracted from the mix so the caller never hears
	// themselves.
	jb *audio.JitterBuffer
	rx audio.Frame
	tx audio.Frame

	vad *audio.VAD
	// player carries prompts addressed to this leg alone.
	player *Player
	// bargeIn stops playback as soon as the caller starts speaking.
	bargeIn atomic.Bool
	// muted drops this leg's contribution to the mix without tearing it down.
	muted atomic.Bool
	// duck attenuates conference audio while a prompt plays, so the agent's
	// voice stays intelligible over background noise.
	duck float64

	// tap forwards received audio to a consumer such as speech recognition.
	// It is held atomically because it is installed from an API handler while
	// the media clock is reading it.
	tap atomic.Pointer[func(audio.Frame)]

	emit EventSink

	closeOnce sync.Once
	done      chan struct{}
}

// LegOptions configures a new leg.
type LegOptions struct {
	ID           string
	Direction    Direction
	From         string
	To           string
	AppRef       string
	JitterTarget time.Duration
	JitterMax    time.Duration
	VADThreshold float64
	EndOfSpeech  time.Duration
	BargeIn      bool
	Emit         EventSink
}

// NewLeg builds a leg around a transport.
func NewLeg(t Transport, o LegOptions) *Leg {
	l := &Leg{
		ID:        o.ID,
		Kind:      t.Kind(),
		Direction: o.Direction,
		From:      o.From,
		To:        o.To,
		AppRef:    o.AppRef,
		CreatedAt: time.Now(),
		transport: t,
		jb: audio.NewJitterBuffer(
			durationToSamples(o.JitterTarget),
			durationToSamples(o.JitterMax),
		),
		rx:     audio.NewFrame(),
		tx:     audio.NewFrame(),
		vad:    audio.NewVAD(o.VADThreshold, o.EndOfSpeech),
		player: NewPlayer(),
		duck:   0.45,
		emit:   o.Emit,
		done:   make(chan struct{}),
	}
	l.bargeIn.Store(o.BargeIn)
	l.state.Store(int32(LegStateNew))
	return l
}

func durationToSamples(d time.Duration) int {
	return int(d.Milliseconds()) * audio.SamplesPerMs
}

// State returns the current lifecycle state.
func (l *Leg) State() LegState { return LegState(l.state.Load()) }

// SetState moves the leg forward and emits the matching event. Transitions are
// monotonic: a late RINGING after an ANSWER is ignored rather than rewinding
// the state, which SIP forking can otherwise cause.
func (l *Leg) SetState(s LegState) {
	for {
		cur := l.state.Load()
		if LegState(cur) >= s {
			return
		}
		if l.state.CompareAndSwap(cur, int32(s)) {
			break
		}
	}
	if s == LegStateAnswered {
		l.answered.Store(time.Now().UnixNano())
	}
	l.Emit("leg."+s.String(), map[string]any{
		"from": l.From,
		"to":   l.To,
	})
}

// Emit publishes an event carrying this leg's identity.
func (l *Leg) Emit(kind string, data map[string]any) {
	if l.emit == nil {
		return
	}
	if data == nil {
		data = map[string]any{}
	}
	data["leg_id"] = l.ID
	if l.AppRef != "" {
		data["app_ref"] = l.AppRef
	}
	if r := l.Room(); r != nil {
		data["room_id"] = r.ID
	}
	l.emit(Event{Type: kind, At: time.Now(), Data: data})
}

// PushAudio files a received payload into the jitter buffer. Transports call
// this from their read loop.
func (l *Leg) PushAudio(rtpTimestamp uint32, pcm []int16) {
	l.jb.Push(rtpTimestamp, pcm)
}

// PushDTMF reports a digit decoded from RFC 4733. DTMF is the authentication
// path that matters most: a spoken PIN can be misheard or overheard, a keypad
// press cannot.
func (l *Leg) PushDTMF(digit rune) {
	// A keypress is an explicit interruption, so it always stops the prompt.
	if ids := l.player.Stop(); len(ids) > 0 {
		for _, id := range ids {
			l.Emit("playback.interrupted", map[string]any{"playback_id": id, "cause": "dtmf"})
		}
	}
	l.Emit("dtmf.received", map[string]any{"digit": string(digit)})
}

// Room returns the room this leg is mixed into, or nil.
func (l *Leg) Room() *Room {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.room
}

func (l *Leg) setRoom(r *Room) {
	l.mu.Lock()
	l.room = r
	l.mu.Unlock()
}

// Player exposes the prompt queue for this leg.
func (l *Leg) Player() *Player { return l.player }

// Transport exposes the network side, used for out-of-band operations such as
// sending DTMF toward a carrier.
func (l *Leg) Transport() Transport {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.transport
}

// SetMuted drops or restores this leg's contribution to the mix.
func (l *Leg) SetMuted(m bool) { l.muted.Store(m) }

// Muted reports the mute state.
func (l *Leg) Muted() bool { return l.muted.Load() }

// SetBargeIn controls whether caller speech interrupts prompts.
func (l *Leg) SetBargeIn(b bool) { l.bargeIn.Store(b) }

// SetTap installs a consumer for this leg's received audio, used to feed
// speech recognition. Passing nil removes it. The consumer runs on the media
// clock and must not block.
func (l *Leg) SetTap(fn func(audio.Frame)) {
	if fn == nil {
		l.tap.Store(nil)
		return
	}
	l.tap.Store(&fn)
}

// receive pulls one frame from the jitter buffer, runs voice activity
// detection on it and returns the frame this leg contributes to the mix.
func (l *Leg) receive() audio.Frame {
	l.jb.Pull(l.rx)

	if fn := l.tap.Load(); fn != nil {
		(*fn)(l.rx)
	}

	switch l.vad.Push(l.rx) {
	case audio.EventSpeechStart:
		l.Emit("speech.started", nil)
		if l.bargeIn.Load() {
			if ids := l.player.Stop(); len(ids) > 0 {
				for _, id := range ids {
					l.Emit("playback.interrupted", map[string]any{"playback_id": id, "cause": "barge_in"})
				}
			}
		}
	case audio.EventSpeechEnd:
		l.Emit("speech.ended", nil)
	}

	if l.muted.Load() {
		l.rx.Clear()
	}
	return l.rx
}

// transmit builds this leg's outbound frame from the conference mix plus its
// own prompts, and hands it to the transport.
func (l *Leg) transmit(mix *audio.Mixer, own audio.Frame) {
	prompt := audio.NewFrame()
	playing, ev := l.player.Next(prompt)
	l.reportPlayout(ev)

	if mix == nil {
		// Not in a room: the leg hears only its own prompts.
		if playing {
			copy(l.tx, prompt)
		} else {
			l.tx.Clear()
		}
	} else {
		gain := 1.0
		if playing {
			gain = l.duck
		}
		mix.MixMinusGain(own, l.tx, gain)
		if playing {
			for i := range l.tx {
				l.tx[i] = saturate(int32(l.tx[i]) + int32(prompt[i]))
			}
		}
	}

	t := l.Transport()
	if t == nil {
		return
	}
	// A failed write is a transient network condition; the media clock must
	// keep running for every other leg regardless.
	_ = t.WriteFrame(l.tx)
}

// tickSolo runs one media cycle for a leg that is not in a room, so prompts
// can be played to a caller before they are bridged to anyone.
func (l *Leg) tickSolo() {
	own := l.receive()
	l.transmit(nil, own)
}

func (l *Leg) reportPlayout(ev PlayoutEvent) {
	if ev.Started != "" {
		l.Emit("playback.started", map[string]any{"playback_id": ev.Started})
	}
	if ev.Finished != "" {
		l.Emit("playback.finished", map[string]any{"playback_id": ev.Finished})
	}
}

func saturate(v int32) int16 {
	if v > 32767 {
		return 32767
	}
	if v < -32768 {
		return -32768
	}
	return int16(v)
}

// Close tears the leg down exactly once and emits leg.ended.
func (l *Leg) Close(cause string) {
	l.closeOnce.Do(func() {
		close(l.done)

		if r := l.Room(); r != nil {
			r.Remove(l.ID)
		}

		l.mu.Lock()
		t := l.transport
		l.transport = nil
		l.mu.Unlock()

		if t != nil {
			_ = t.Close()
		}

		l.state.Store(int32(LegStateEnded))
		l.Emit("leg.ended", map[string]any{
			"cause":       cause,
			"duration_ms": l.DurationMs(),
			"jitter":      l.jb.Stats(),
		})
	})
}

// Done is closed when the leg is torn down.
func (l *Leg) Done() <-chan struct{} { return l.done }

// DurationMs reports billable duration: time since answer, or zero if the call
// was never answered.
func (l *Leg) DurationMs() int64 {
	ns := l.answered.Load()
	if ns == 0 {
		return 0
	}
	return time.Since(time.Unix(0, ns)).Milliseconds()
}

// JitterStats exposes receive-path health.
func (l *Leg) JitterStats() audio.JitterStats { return l.jb.Stats() }

// Snapshot is the API representation of a leg.
type Snapshot struct {
	ID         string            `json:"id"`
	Type       string            `json:"type"`
	State      string            `json:"state"`
	Direction  string            `json:"direction"`
	From       string            `json:"from,omitempty"`
	To         string            `json:"to,omitempty"`
	RoomID     string            `json:"room_id,omitempty"`
	AppRef     string            `json:"app_ref,omitempty"`
	RemoteAddr string            `json:"remote_addr,omitempty"`
	Muted      bool              `json:"muted"`
	Playing    bool              `json:"playing"`
	CreatedAt  time.Time         `json:"created_at"`
	DurationMs int64             `json:"duration_ms"`
	Jitter     audio.JitterStats `json:"jitter"`
}

// Snapshot renders the leg for the control API.
func (l *Leg) Snapshot() Snapshot {
	s := Snapshot{
		ID:         l.ID,
		Type:       l.Kind,
		State:      l.State().String(),
		Direction:  string(l.Direction),
		From:       l.From,
		To:         l.To,
		AppRef:     l.AppRef,
		Muted:      l.Muted(),
		Playing:    l.player.Playing(),
		CreatedAt:  l.CreatedAt,
		DurationMs: l.DurationMs(),
		Jitter:     l.jb.Stats(),
	}
	if r := l.Room(); r != nil {
		s.RoomID = r.ID
	}
	if t := l.Transport(); t != nil {
		s.RemoteAddr = t.RemoteAddr()
	}
	return s
}
