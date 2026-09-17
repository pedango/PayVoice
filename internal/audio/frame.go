package audio

import "math"

// Pedango runs one uniform media plane: 8 kHz, mono, 16-bit linear PCM,
// carried as G.711 on every wire. A SIP leg and a browser leg therefore hold
// byte-identical payloads and bridging costs nothing but a memory copy.
const (
	// SampleRate is the narrowband telephony rate used everywhere internally.
	SampleRate = 8000
	// FrameDurationMs is the packetization interval, matching the 20 ms default
	// of every PSTN gateway and of browser PCMU senders.
	FrameDurationMs = 20
	// FrameSamples is the sample count in one frame (160 at 8 kHz / 20 ms).
	FrameSamples = SampleRate * FrameDurationMs / 1000
	// ClockRate is the RTP clock rate for G.711.
	ClockRate = 8000
	// SamplesPerMs is used to convert RTP timestamp deltas to durations.
	SamplesPerMs = SampleRate / 1000
)

// Frame is exactly FrameSamples of linear PCM. Passing frames as slices keeps
// the mixer allocation-free: buffers are owned by their producer and reused.
type Frame []int16

// NewFrame allocates a silent frame.
func NewFrame() Frame { return make(Frame, FrameSamples) }

// Clear zeroes the frame in place.
func (f Frame) Clear() {
	for i := range f {
		f[i] = 0
	}
}

// CopyFrom copies src into f, zero-filling any shortfall and ignoring excess.
func (f Frame) CopyFrom(src []int16) {
	n := copy(f, src)
	for i := n; i < len(f); i++ {
		f[i] = 0
	}
}

// RMS returns the root-mean-square amplitude of the frame, the level used for
// voice activity detection.
func (f Frame) RMS() float64 {
	if len(f) == 0 {
		return 0
	}
	var sum float64
	for _, s := range f {
		v := float64(s)
		sum += v * v
	}
	return math.Sqrt(sum / float64(len(f)))
}

// Scale multiplies every sample by gain, saturating at the 16-bit bounds.
func (f Frame) Scale(gain float64) {
	if gain == 1 {
		return
	}
	for i, s := range f {
		f[i] = clamp16(int32(float64(s) * gain))
	}
}

// clamp16 saturates a 32-bit accumulator to the 16-bit sample range. Wrapping
// instead of clamping is what produces the harsh crackle in naive mixers.
func clamp16(v int32) int16 {
	if v > math.MaxInt16 {
		return math.MaxInt16
	}
	if v < math.MinInt16 {
		return math.MinInt16
	}
	return int16(v)
}
