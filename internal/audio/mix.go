package audio

// Mixer builds an N-way conference sum and derives each participant's personal
// feed from it.
//
// The naive approach mixes every leg against every other leg, which is O(N^2)
// per tick. Instead the mixer accumulates one shared sum in 32-bit headroom
// and subtracts each leg's own contribution when producing that leg's output,
// which is O(N) and gives every participant "mix-minus-self": they hear
// everyone but themselves, so they never hear their own voice echoed back.
//
// Accumulating in int32 and clamping only at the final subtraction matters:
// clamping the running sum would distort loud frames even for a listener whose
// own loud contribution is about to be removed.
type Mixer struct {
	sum    []int32
	active int
}

// NewMixer allocates a mixer for one frame duration.
func NewMixer() *Mixer {
	return &Mixer{sum: make([]int32, FrameSamples)}
}

// Reset clears the accumulator for a new tick.
func (m *Mixer) Reset() {
	for i := range m.sum {
		m.sum[i] = 0
	}
	m.active = 0
}

// Add folds one contribution into the shared sum.
func (m *Mixer) Add(f Frame) {
	n := len(f)
	if n > len(m.sum) {
		n = len(m.sum)
	}
	for i := 0; i < n; i++ {
		m.sum[i] += int32(f[i])
	}
	m.active++
}

// Contributors reports how many frames were folded in this tick.
func (m *Mixer) Contributors() int { return m.active }

// MixMinus writes the conference sum less own into dst. Passing a nil own
// frame produces the full mix, which is what an announcement or recording tap
// wants.
func (m *Mixer) MixMinus(own Frame, dst Frame) {
	if own == nil {
		for i := range dst {
			dst[i] = clamp16(m.sum[i])
		}
		return
	}
	for i := range dst {
		v := m.sum[i]
		if i < len(own) {
			v -= int32(own[i])
		}
		dst[i] = clamp16(v)
	}
}

// MixMinusGain is MixMinus with a linear gain applied before saturation, used
// to duck conference audio while the agent is speaking.
func (m *Mixer) MixMinusGain(own Frame, dst Frame, gain float64) {
	if gain == 1 {
		m.MixMinus(own, dst)
		return
	}
	for i := range dst {
		v := m.sum[i]
		if own != nil && i < len(own) {
			v -= int32(own[i])
		}
		dst[i] = clamp16(int32(float64(v) * gain))
	}
}
