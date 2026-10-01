package media

import (
	"errors"
	"sync"
	"time"

	"github.com/pedango/PayVoice/internal/audio"
)

// Errors returned by room operations.
var (
	// ErrRoomFull means the room already holds its configured maximum legs.
	ErrRoomFull = errors.New("room is full")
	// ErrLegInRoom means the leg is already mixed somewhere.
	ErrLegInRoom = errors.New("leg already belongs to a room")
)

// Event is one thing that happened on the media plane. Events are what the
// application reacts to; PayVoice itself holds no call logic.
type Event struct {
	Type string         `json:"type"`
	At   time.Time      `json:"-"`
	Data map[string]any `json:"data"`
}

// EventSink receives events. Implementations must not block: they are called
// from the media clock.
type EventSink func(Event)

// Room is a mixing bridge. Legs in a room hear each other; a two-leg room is
// an ordinary bridged call, and a one-leg room is a caller talking to the
// agent.
type Room struct {
	ID        string
	CreatedAt time.Time

	mu    sync.RWMutex
	legs  []*Leg
	index map[string]*Leg

	mixer  *audio.Mixer
	player *Player
	// scratch holds each leg's receive frame for the current tick so the
	// transmit pass can subtract it without pulling from the buffer twice.
	scratch []audio.Frame
	ann     audio.Frame

	maxLegs  int
	emptyAt  time.Time
	emit     EventSink
	closed   bool
	closeOne sync.Once
}

// NewRoom creates an empty room.
func NewRoom(id string, maxLegs int, emit EventSink) *Room {
	if maxLegs <= 0 {
		maxLegs = 8
	}
	return &Room{
		ID:        id,
		CreatedAt: time.Now(),
		index:     make(map[string]*Leg, maxLegs),
		mixer:     audio.NewMixer(),
		player:    NewPlayer(),
		ann:       audio.NewFrame(),
		maxLegs:   maxLegs,
		emptyAt:   time.Now(),
		emit:      emit,
	}
}

// Player exposes the room-wide announcement queue. Audio played here is heard
// by every leg, which is how the voice agent addresses a bridged call.
func (r *Room) Player() *Player { return r.player }

// Add mixes a leg into the room.
func (r *Room) Add(l *Leg) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return errors.New("room is closed")
	}
	if _, dup := r.index[l.ID]; dup {
		r.mu.Unlock()
		return nil
	}
	if len(r.legs) >= r.maxLegs {
		r.mu.Unlock()
		return ErrRoomFull
	}
	if existing := l.Room(); existing != nil && existing != r {
		r.mu.Unlock()
		return ErrLegInRoom
	}

	r.legs = append(r.legs, l)
	r.index[l.ID] = l
	r.emptyAt = time.Time{}
	r.mu.Unlock()

	l.setRoom(r)
	r.emitEvent("room.leg_joined", map[string]any{"room_id": r.ID, "leg_id": l.ID})
	return nil
}

// Remove detaches a leg. The leg keeps running; it simply stops being mixed.
func (r *Room) Remove(legID string) {
	r.mu.Lock()
	l, ok := r.index[legID]
	if !ok {
		r.mu.Unlock()
		return
	}
	delete(r.index, legID)
	for i, cur := range r.legs {
		if cur.ID == legID {
			r.legs = append(r.legs[:i], r.legs[i+1:]...)
			break
		}
	}
	if len(r.legs) == 0 {
		r.emptyAt = time.Now()
	}
	r.mu.Unlock()

	l.setRoom(nil)
	r.emitEvent("room.leg_left", map[string]any{"room_id": r.ID, "leg_id": legID})
}

// Legs returns the current membership.
func (r *Room) Legs() []*Leg {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Leg, len(r.legs))
	copy(out, r.legs)
	return out
}

// Size reports the membership count.
func (r *Room) Size() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.legs)
}

// tick runs one 20 ms mixing cycle.
//
// The two passes matter: every leg's receive frame must be collected before
// any leg transmits, otherwise early legs would mix against a partially built
// sum and hear a different conference from late legs.
func (r *Room) tick() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	legs := make([]*Leg, len(r.legs))
	copy(legs, r.legs)
	if cap(r.scratch) < len(legs) {
		r.scratch = make([]audio.Frame, len(legs))
	}
	scratch := r.scratch[:len(legs)]
	r.mu.Unlock()

	if len(legs) == 0 {
		return
	}

	r.mixer.Reset()
	for i, l := range legs {
		scratch[i] = l.receive()
		r.mixer.Add(scratch[i])
	}

	// Room-wide announcements join the sum, so no leg has them subtracted.
	if playing, ev := r.player.Next(r.ann); playing {
		r.mixer.Add(r.ann)
		r.reportPlayout(ev)
	} else {
		r.reportPlayout(ev)
	}

	for i, l := range legs {
		l.transmit(r.mixer, scratch[i])
	}
}

func (r *Room) reportPlayout(ev PlayoutEvent) {
	if ev.Started != "" {
		r.emitEvent("playback.started", map[string]any{"room_id": r.ID, "playback_id": ev.Started})
	}
	if ev.Finished != "" {
		r.emitEvent("playback.finished", map[string]any{"room_id": r.ID, "playback_id": ev.Finished})
	}
}

func (r *Room) emitEvent(kind string, data map[string]any) {
	if r.emit == nil {
		return
	}
	r.emit(Event{Type: kind, At: time.Now(), Data: data})
}

// idleSince reports how long the room has held no legs, or zero if occupied.
func (r *Room) idleSince() time.Duration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.legs) > 0 || r.emptyAt.IsZero() {
		return 0
	}
	return time.Since(r.emptyAt)
}

// Close detaches every leg and marks the room dead.
func (r *Room) Close() {
	r.closeOne.Do(func() {
		r.mu.Lock()
		r.closed = true
		legs := r.legs
		r.legs = nil
		r.index = map[string]*Leg{}
		r.mu.Unlock()

		for _, l := range legs {
			l.setRoom(nil)
		}
		r.emitEvent("room.destroyed", map[string]any{"room_id": r.ID})
	})
}

// RoomSnapshot is the API representation of a room.
type RoomSnapshot struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	Legs      []string  `json:"legs"`
	Playing   bool      `json:"playing"`
	MaxLegs   int       `json:"max_legs"`
}

// Snapshot renders the room for the control API.
func (r *Room) Snapshot() RoomSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ids := make([]string, 0, len(r.legs))
	for _, l := range r.legs {
		ids = append(ids, l.ID)
	}
	return RoomSnapshot{
		ID:        r.ID,
		CreatedAt: r.CreatedAt,
		Legs:      ids,
		Playing:   r.player.Playing(),
		MaxLegs:   r.maxLegs,
	}
}
