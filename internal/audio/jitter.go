package audio

import "sync"

// JitterBuffer absorbs network delay variation on a receive path and hands the
// mixer exactly one frame per 20 ms tick.
//
// Packets are filed into a ring indexed by RTP timestamp, and a playout cursor
// trails the newest arrival by the target delay. A packet that misses its
// playout slot is counted as late and discarded rather than played out of
// order, because reordered speech is worse than concealed speech. Gaps are
// filled by decaying repetition of the last good frame, the standard cheap
// packet-loss concealment: it avoids the click of digital silence without the
// robotic buzz of indefinite repetition.
//
// Startup is handled by priming: the cursor sits on the first packet's
// timestamp and does not advance until the buffer actually holds the target
// delay. Backdating the cursor instead would make the buffer spend its first
// frames reading timestamps that never existed and report them as loss.
//
// A JitterBuffer is safe for one producer and one consumer running
// concurrently, which is how it is used: an RTP reader pushes, the room mixer
// pulls.
type JitterBuffer struct {
	mu sync.Mutex

	ring     []slot
	size     int
	targetTS uint32 // target delay in samples
	maxTS    uint32 // maximum delay in samples before a forced resync

	started   bool
	playoutTS uint32
	newestTS  uint32

	// priming holds the cursor still until the buffer has banked targetTS of
	// audio. primePulls bounds that stall so a stream that stops after one
	// packet cannot wedge the leg forever.
	priming       bool
	primePulls    int
	maxPrimePulls int

	// last holds the most recent successfully played frame, used for
	// concealment, and concealed counts consecutive concealed frames.
	last      Frame
	concealed int

	stats JitterStats
}

type slot struct {
	ts    uint32
	pcm   Frame
	valid bool
}

// JitterStats reports buffer health. Rising Late or Resync values mean the
// target delay is too low for the path.
type JitterStats struct {
	Received  uint64
	Played    uint64
	Lost      uint64
	Late      uint64
	Duplicate uint64
	Resync    uint64
	Depth     int
	Priming   bool
}

// NewJitterBuffer builds a buffer holding targetSamples of nominal delay and
// refusing to hold more than maxSamples.
func NewJitterBuffer(targetSamples, maxSamples int) *JitterBuffer {
	if targetSamples < FrameSamples {
		targetSamples = FrameSamples
	}
	if maxSamples < targetSamples*2 {
		maxSamples = targetSamples * 2
	}
	// One extra frame of headroom so a packet arriving exactly at the maximum
	// depth does not alias onto the slot the cursor is about to read.
	size := maxSamples/FrameSamples + 2

	jb := &JitterBuffer{
		ring:          make([]slot, size),
		size:          size,
		targetTS:      uint32(targetSamples),
		maxTS:         uint32(maxSamples),
		maxPrimePulls: targetSamples/FrameSamples + 2,
		last:          NewFrame(),
	}
	for i := range jb.ring {
		jb.ring[i].pcm = NewFrame()
	}
	return jb
}

// Push files a decoded payload at the given RTP timestamp. Payloads longer
// than one frame are split, so senders using 10 ms or 30 ms packetization work
// without special handling.
func (jb *JitterBuffer) Push(ts uint32, pcm []int16) {
	jb.mu.Lock()
	defer jb.mu.Unlock()

	jb.stats.Received++

	if !jb.started {
		jb.started = true
		jb.priming = true
		jb.primePulls = 0
		jb.playoutTS = ts
		jb.newestTS = ts
	}

	for off := 0; off < len(pcm); off += FrameSamples {
		end := off + FrameSamples
		if end > len(pcm) {
			end = len(pcm)
		}
		jb.fileLocked(ts+uint32(off), pcm[off:end])
	}
}

func (jb *JitterBuffer) fileLocked(ts uint32, pcm []int16) {
	// Signed arithmetic on the wrapped difference tolerates RTP timestamp
	// rollover, which a plain unsigned comparison would misread as a packet
	// from the distant past.
	delta := int32(ts - jb.playoutTS)

	switch {
	case delta < 0:
		jb.stats.Late++
		return
	case delta > int32(jb.maxTS):
		// Either a silence-suppression gap or a stream restart. Rebase rather
		// than stalling for the whole discontinuity.
		jb.resyncLocked(ts)
	}

	if int32(ts-jb.newestTS) > 0 {
		jb.newestTS = ts
	}

	idx := int(ts/FrameSamples) % jb.size
	s := &jb.ring[idx]
	if s.valid && s.ts == ts {
		jb.stats.Duplicate++
		return
	}
	s.ts = ts
	s.valid = true
	s.pcm.CopyFrom(pcm)
}

func (jb *JitterBuffer) resyncLocked(ts uint32) {
	jb.stats.Resync++
	jb.playoutTS = ts
	jb.newestTS = ts
	jb.priming = true
	jb.primePulls = 0
	jb.concealed = 0
	for i := range jb.ring {
		jb.ring[i].valid = false
	}
}

// Pull writes the next frame of playout audio into dst and reports whether it
// came from a real packet. It always fills dst.
func (jb *JitterBuffer) Pull(dst Frame) bool {
	jb.mu.Lock()
	defer jb.mu.Unlock()

	if !jb.started {
		dst.Clear()
		return false
	}

	if jb.priming {
		banked := int32(jb.newestTS-jb.playoutTS) >= int32(jb.targetTS)
		if banked || jb.primePulls >= jb.maxPrimePulls {
			jb.priming = false
		} else {
			// Hold the cursor: emit silence without advancing or counting
			// loss, so startup does not look like a lossy network.
			jb.primePulls++
			dst.Clear()
			return false
		}
	}

	idx := int(jb.playoutTS/FrameSamples) % jb.size
	s := &jb.ring[idx]

	if s.valid && s.ts == jb.playoutTS {
		s.valid = false
		copy(dst, s.pcm)
		copy(jb.last, s.pcm)
		jb.concealed = 0
		jb.playoutTS += FrameSamples
		jb.stats.Played++
		return true
	}

	jb.conceal(dst)
	jb.playoutTS += FrameSamples
	jb.stats.Lost++
	return false
}

// conceal fills dst with a fading repeat of the last good frame. Past three
// frames (60 ms) the repetition becomes audible as a buzz, so it decays to
// silence instead.
func (jb *JitterBuffer) conceal(dst Frame) {
	jb.concealed++
	if jb.concealed > 3 {
		dst.Clear()
		return
	}
	gain := 1.0 - 0.3*float64(jb.concealed)
	for i, s := range jb.last {
		dst[i] = clamp16(int32(float64(s) * gain))
	}
}

// Stats returns a snapshot of buffer health.
func (jb *JitterBuffer) Stats() JitterStats {
	jb.mu.Lock()
	defer jb.mu.Unlock()

	st := jb.stats
	st.Priming = jb.priming
	st.Depth = 0
	for i := range jb.ring {
		if jb.ring[i].valid {
			st.Depth++
		}
	}
	return st
}

// Reset returns the buffer to its pre-first-packet state, used when a leg is
// re-INVITEd onto a new media path.
func (jb *JitterBuffer) Reset() {
	jb.mu.Lock()
	defer jb.mu.Unlock()

	jb.started = false
	jb.priming = false
	jb.primePulls = 0
	jb.concealed = 0
	jb.last.Clear()
	for i := range jb.ring {
		jb.ring[i].valid = false
	}
}
