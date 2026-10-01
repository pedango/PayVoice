package media

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pedango/PayVoice/internal/audio"
	"github.com/pedango/PayVoice/internal/config"
)

// Engine owns every room and leg, and drives them from a single media clock.
//
// A per-leg timer would be the obvious design and the wrong one: hundreds of
// independent 20 ms tickers drift against each other, so a leg that joins a
// room has to be handed from its own clock to the room's, and that handover
// races. One clock for the whole process removes the race, keeps every room
// sample-aligned, and costs a single timer. The work per tick is a few hundred
// microseconds of integer arithmetic, far short of the 20 ms budget.
type Engine struct {
	cfg  config.MediaConfig
	emit EventSink

	mu    sync.RWMutex
	rooms map[string]*Room
	legs  map[string]*Leg

	running atomic.Bool
	metrics EngineMetrics
}

// EngineMetrics counts media clock health. LateTicks rising means the process
// is starved and audio is stuttering.
type EngineMetrics struct {
	Ticks      atomic.Uint64
	LateTicks  atomic.Uint64
	MaxLagUs   atomic.Int64
	LegsOpened atomic.Uint64
	LegsClosed atomic.Uint64
}

// NewEngine builds an engine. emit receives every media event.
func NewEngine(cfg config.MediaConfig, emit EventSink) *Engine {
	return &Engine{
		cfg:   cfg,
		emit:  emit,
		rooms: make(map[string]*Room),
		legs:  make(map[string]*Leg),
	}
}

// Run drives the media clock until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) error {
	e.running.Store(true)
	defer e.running.Store(false)

	const period = audio.FrameDurationMs * time.Millisecond
	ticker := time.NewTicker(period)
	defer ticker.Stop()

	reap := time.NewTicker(5 * time.Second)
	defer reap.Stop()

	for {
		select {
		case <-ctx.Done():
			e.shutdown()
			return ctx.Err()

		case <-reap.C:
			e.reap()

		case t := <-ticker.C:
			start := time.Now()
			e.tick()

			e.metrics.Ticks.Add(1)
			// Lag is measured from the tick's scheduled time, not from the
			// start of work, so scheduler delay is counted too.
			if lag := start.Sub(t) + time.Since(start); lag > period {
				e.metrics.LateTicks.Add(1)
				if us := lag.Microseconds(); us > e.metrics.MaxLagUs.Load() {
					e.metrics.MaxLagUs.Store(us)
				}
			}
		}
	}
}

func (e *Engine) tick() {
	e.mu.RLock()
	rooms := make([]*Room, 0, len(e.rooms))
	for _, r := range e.rooms {
		rooms = append(rooms, r)
	}
	solo := make([]*Leg, 0, len(e.legs))
	for _, l := range e.legs {
		if l.Room() == nil && l.State() == LegStateAnswered {
			solo = append(solo, l)
		}
	}
	e.mu.RUnlock()

	for _, r := range rooms {
		r.tick()
	}
	// A leg outside a room still needs a clock, otherwise prompts played to a
	// caller before they are bridged would never leave the queue.
	for _, l := range solo {
		l.tickSolo()
	}
}

// reap closes rooms that have been empty past the idle timeout and legs that
// have exceeded the maximum call duration. Without this, a carrier that never
// sends BYE would leak a leg and its RTP port for the life of the process.
func (e *Engine) reap() {
	e.mu.RLock()
	rooms := make([]*Room, 0, len(e.rooms))
	for _, r := range e.rooms {
		rooms = append(rooms, r)
	}
	legs := make([]*Leg, 0, len(e.legs))
	for _, l := range e.legs {
		legs = append(legs, l)
	}
	e.mu.RUnlock()

	for _, r := range rooms {
		if e.cfg.RoomIdleTimeout > 0 && r.idleSince() > e.cfg.RoomIdleTimeout {
			e.DestroyRoom(r.ID)
		}
	}
	for _, l := range legs {
		if l.State() == LegStateEnded {
			e.RemoveLeg(l.ID)
			continue
		}
		if e.cfg.LegMaxDuration > 0 && time.Since(l.CreatedAt) > e.cfg.LegMaxDuration {
			l.Close("max_duration")
			e.RemoveLeg(l.ID)
		}
	}
}

func (e *Engine) shutdown() {
	e.mu.Lock()
	legs := make([]*Leg, 0, len(e.legs))
	for _, l := range e.legs {
		legs = append(legs, l)
	}
	rooms := make([]*Room, 0, len(e.rooms))
	for _, r := range e.rooms {
		rooms = append(rooms, r)
	}
	e.legs = map[string]*Leg{}
	e.rooms = map[string]*Room{}
	e.mu.Unlock()

	for _, l := range legs {
		l.Close("shutdown")
	}
	for _, r := range rooms {
		r.Close()
	}
}

// CreateRoom registers a new room under the given id.
func (e *Engine) CreateRoom(id string) *Room {
	r := NewRoom(id, e.cfg.MaxLegsPerRoom, e.emit)

	e.mu.Lock()
	e.rooms[id] = r
	e.mu.Unlock()

	if e.emit != nil {
		e.emit(Event{Type: "room.created", At: time.Now(), Data: map[string]any{"room_id": id}})
	}
	return r
}

// Room looks up a room.
func (e *Engine) Room(id string) *Room {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.rooms[id]
}

// DestroyRoom closes and deregisters a room.
func (e *Engine) DestroyRoom(id string) {
	e.mu.Lock()
	r := e.rooms[id]
	delete(e.rooms, id)
	e.mu.Unlock()

	if r != nil {
		r.Close()
	}
}

// Rooms lists every room.
func (e *Engine) Rooms() []*Room {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]*Room, 0, len(e.rooms))
	for _, r := range e.rooms {
		out = append(out, r)
	}
	return out
}

// AddLeg registers a leg with the engine so the media clock drives it.
func (e *Engine) AddLeg(l *Leg) {
	e.mu.Lock()
	e.legs[l.ID] = l
	e.mu.Unlock()
	e.metrics.LegsOpened.Add(1)
}

// Leg looks up a leg.
func (e *Engine) Leg(id string) *Leg {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.legs[id]
}

// RemoveLeg deregisters a leg without closing it.
func (e *Engine) RemoveLeg(id string) {
	e.mu.Lock()
	_, existed := e.legs[id]
	delete(e.legs, id)
	e.mu.Unlock()
	if existed {
		e.metrics.LegsClosed.Add(1)
	}
}

// Legs lists every leg.
func (e *Engine) Legs() []*Leg {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]*Leg, 0, len(e.legs))
	for _, l := range e.legs {
		out = append(out, l)
	}
	return out
}

// Stats is the health snapshot exposed at /healthz and /metrics.
type Stats struct {
	Running    bool   `json:"running"`
	Rooms      int    `json:"rooms"`
	Legs       int    `json:"legs"`
	Ticks      uint64 `json:"ticks"`
	LateTicks  uint64 `json:"late_ticks"`
	MaxLagUs   int64  `json:"max_lag_us"`
	LegsOpened uint64 `json:"legs_opened"`
	LegsClosed uint64 `json:"legs_closed"`
}

// Stats returns engine health.
func (e *Engine) Stats() Stats {
	e.mu.RLock()
	rooms, legs := len(e.rooms), len(e.legs)
	e.mu.RUnlock()

	return Stats{
		Running:    e.running.Load(),
		Rooms:      rooms,
		Legs:       legs,
		Ticks:      e.metrics.Ticks.Load(),
		LateTicks:  e.metrics.LateTicks.Load(),
		MaxLagUs:   e.metrics.MaxLagUs.Load(),
		LegsOpened: e.metrics.LegsOpened.Load(),
		LegsClosed: e.metrics.LegsClosed.Load(),
	}
}
