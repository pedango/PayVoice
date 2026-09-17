package audio

import "math"

// Resample converts linear PCM from inRate to 8 kHz.
//
// Speech synthesis providers return 16, 22.05, 24 or 44.1 kHz audio, all of
// which must reach the telephone network at 8 kHz. Decimating by plain
// interpolation would alias every component above 4 kHz back down into the
// voice band as a metallic whistle, so downsampling runs a low-pass first.
// The filter is a cascaded biquad at 3.4 kHz, the same corner the PSTN itself
// uses, applied forward and backward to cancel phase distortion.
func Resample(in []int16, inRate int) []int16 {
	if inRate == SampleRate || len(in) == 0 {
		out := make([]int16, len(in))
		copy(out, in)
		return out
	}

	src := make([]float64, len(in))
	for i, s := range in {
		src[i] = float64(s)
	}

	if inRate > SampleRate {
		lp := newLowpass(3400, float64(inRate))
		lp.processForwardBackward(src)
	}

	ratio := float64(inRate) / float64(SampleRate)
	outLen := int(float64(len(in)) / ratio)
	out := make([]int16, outLen)

	for i := 0; i < outLen; i++ {
		pos := float64(i) * ratio
		idx := int(pos)
		frac := pos - float64(idx)

		s0 := src[idx]
		s1 := s0
		if idx+1 < len(src) {
			s1 = src[idx+1]
		}
		out[i] = clamp16(int32(math.Round(s0 + frac*(s1-s0))))
	}
	return out
}

// biquad is a transposed direct form II second-order section.
type biquad struct {
	b0, b1, b2, a1, a2 float64
	z1, z2             float64
}

// newLowpass returns a Butterworth (Q = 1/sqrt2) low-pass section.
func newLowpass(cutoff, sampleRate float64) *biquad {
	// Guard against a cutoff at or above Nyquist, which makes the bilinear
	// transform blow up.
	if cutoff >= sampleRate/2 {
		cutoff = sampleRate/2 - 1
	}
	w0 := 2 * math.Pi * cutoff / sampleRate
	cosw := math.Cos(w0)
	alpha := math.Sin(w0) / math.Sqrt2

	a0 := 1 + alpha
	return &biquad{
		b0: (1 - cosw) / 2 / a0,
		b1: (1 - cosw) / a0,
		b2: (1 - cosw) / 2 / a0,
		a1: -2 * cosw / a0,
		a2: (1 - alpha) / a0,
	}
}

func (f *biquad) reset() { f.z1, f.z2 = 0, 0 }

func (f *biquad) process(x float64) float64 {
	y := f.b0*x + f.z1
	f.z1 = f.b1*x - f.a1*y + f.z2
	f.z2 = f.b2*x - f.a2*y
	return y
}

// processForwardBackward filters in place in both directions. Running the
// filter twice squares its magnitude response and cancels its phase response,
// which keeps speech transients from smearing.
func (f *biquad) processForwardBackward(buf []float64) {
	for i := range buf {
		buf[i] = f.process(buf[i])
	}
	f.reset()
	for i := len(buf) - 1; i >= 0; i-- {
		buf[i] = f.process(buf[i])
	}
	f.reset()
}

// Upsample converts 8 kHz linear PCM to outRate using linear interpolation
// followed by a low-pass at the source Nyquist to suppress imaging.
func Upsample(in []int16, outRate int) []int16 {
	if outRate == SampleRate || len(in) == 0 {
		out := make([]int16, len(in))
		copy(out, in)
		return out
	}
	ratio := float64(SampleRate) / float64(outRate)
	outLen := int(float64(len(in)) / ratio)
	buf := make([]float64, outLen)

	for i := 0; i < outLen; i++ {
		pos := float64(i) * ratio
		idx := int(pos)
		frac := pos - float64(idx)
		s0 := float64(in[idx])
		s1 := s0
		if idx+1 < len(in) {
			s1 = float64(in[idx+1])
		}
		buf[i] = s0 + frac*(s1-s0)
	}

	lp := newLowpass(3400, float64(outRate))
	lp.processForwardBackward(buf)

	out := make([]int16, outLen)
	for i, v := range buf {
		out[i] = clamp16(int32(math.Round(v)))
	}
	return out
}
