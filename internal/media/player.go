package media

import (
	"sync"

	"github.com/pedango/pedango/internal/audio"
)

// Player is the playout queue for synthesized prompts on a leg or in a room.
//
// Speech arrives from a TTS provider as one large buffer but must leave as a
// paced 20 ms frame stream, because a media path is a real-time clock: dumping
// five seconds of audio into a socket at once overruns the far end's jitter
// buffer and is heard as a garbled burst. The player holds the buffer and
// releases exactly one frame per tick.
//
// It also supports barge-in. When the caller starts speaking the agent must
// stop talking immediately, which means discarding queued audio rather than
// waiting for it to drain.
type Player struct {
	mu    sync.Mutex
	queue []*clip

	// fadeOut counts down frames of taper applied after a barge-in stop, so
	// cutting speech mid-word does not produce an audible click.
	fadeOut int
	fadeSrc audio.Frame
}

type clip struct {
	id   string
	pcm  []int16
	pos  int
	gain float64
	// started records whether a playback.started event has been reported.
	started bool
}

const fadeOutFrames = 2

// NewPlayer returns an empty player.
func NewPlayer() *Player {
	return &Player{fadeSrc: audio.NewFrame()}
}

// Enqueue appends a clip of 8 kHz linear PCM and returns the queue depth.
func (p *Player) Enqueue(id string, pcm []int16, gain float64) int {
	if gain <= 0 {
		gain = 1
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	p.queue = append(p.queue, &clip{id: id, pcm: pcm, gain: gain})
	return len(p.queue)
}

// Stop discards everything queued and returns the ids that were dropped, so
// the caller can report them as interrupted. A short fade is armed to avoid
// clicking when speech is cut mid-word.
func (p *Player) Stop() []string {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.queue) == 0 {
		return nil
	}
	ids := make([]string, 0, len(p.queue))
	for _, c := range p.queue {
		ids = append(ids, c.id)
	}
	p.queue = p.queue[:0]
	p.fadeOut = fadeOutFrames
	return ids
}

// Playing reports whether audio is queued.
func (p *Player) Playing() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.queue) > 0
}

// Pending reports the number of queued clips.
func (p *Player) Pending() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.queue)
}

// PlayoutEvent reports a clip boundary crossed during Next.
type PlayoutEvent struct {
	Started  string
	Finished string
}

// Next fills dst with the next frame of playout and reports whether any audio
// was written. It returns at most one start and one finish event per call.
func (p *Player) Next(dst audio.Frame) (bool, PlayoutEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()

	var ev PlayoutEvent

	if len(p.queue) == 0 {
		if p.fadeOut > 0 {
			p.fadeOut--
			gain := float64(p.fadeOut) / float64(fadeOutFrames)
			copy(dst, p.fadeSrc)
			dst.Scale(gain)
			return true, ev
		}
		dst.Clear()
		return false, ev
	}

	c := p.queue[0]
	if !c.started {
		c.started = true
		ev.Started = c.id
	}

	n := copy(dst, c.pcm[c.pos:])
	for i := n; i < len(dst); i++ {
		dst[i] = 0
	}
	c.pos += n
	if c.gain != 1 {
		dst.Scale(c.gain)
	}
	copy(p.fadeSrc, dst)

	if c.pos >= len(c.pcm) {
		ev.Finished = c.id
		p.queue = p.queue[1:]
	}
	return true, ev
}
