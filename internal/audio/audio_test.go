package audio

import (
	"math"
	"testing"
	"time"
)

// TestG711RoundTripError checks companding accuracy rather than exact
// equality: G.711 is lossy by design, but the error must stay inside the
// quantisation step of the segment a sample falls in.
func TestG711RoundTripError(t *testing.T) {
	for _, c := range []Codec{CodecPCMU, CodecPCMA} {
		t.Run(c.Name(), func(t *testing.T) {
			var worst float64
			for v := -32768; v <= 32767; v += 7 {
				in := int16(v)
				var out int16
				if c == CodecPCMU {
					out = DecodeMulawSample(EncodeMulawSample(in))
				} else {
					out = DecodeAlawSample(EncodeAlawSample(in))
				}
				// Relative error is the meaningful measure for a logarithmic
				// companding law.
				rel := math.Abs(float64(out-in)) / (math.Abs(float64(in)) + 256)
				if rel > worst {
					worst = rel
				}
			}
			if worst > 0.12 {
				t.Errorf("worst relative companding error %.4f exceeds 12%%", worst)
			}
		})
	}
}

// TestG711SilenceIsQuiet guards the classic gateway bug of filling gaps with
// zero bytes, which in mu-law decodes to near full-scale and is heard as a
// loud tone.
func TestG711SilenceIsQuiet(t *testing.T) {
	for _, c := range []Codec{CodecPCMU, CodecPCMA} {
		pcm := c.Decode(nil, []byte{c.Silence()})
		if abs16(pcm[0]) > 64 {
			t.Errorf("%s silence octet decodes to %d, expected near zero", c.Name(), pcm[0])
		}
		zero := c.Decode(nil, []byte{0x00})
		if c == CodecPCMU && abs16(zero[0]) < 30000 {
			t.Errorf("expected a zero byte to be loud in mu-law, got %d", zero[0])
		}
	}
}

func TestCodecEncodeDecodeFrame(t *testing.T) {
	c := CodecPCMU
	in := make([]int16, FrameSamples)
	for i := range in {
		in[i] = int16(8000 * math.Sin(2*math.Pi*440*float64(i)/SampleRate))
	}
	payload := c.Encode(nil, in)
	if len(payload) != FrameSamples {
		t.Fatalf("payload is %d bytes, want %d", len(payload), FrameSamples)
	}
	out := c.Decode(nil, payload)

	var err float64
	for i := range in {
		d := float64(out[i] - in[i])
		err += d * d
	}
	rmse := math.Sqrt(err / float64(len(in)))
	if rmse > 120 {
		t.Errorf("frame round-trip RMSE %.1f is worse than expected for G.711", rmse)
	}
}

// TestMixerMixMinusSelf is the property that keeps callers from hearing their
// own voice: with two legs, each must receive exactly the other's audio.
func TestMixerMixMinusSelf(t *testing.T) {
	a, b := NewFrame(), NewFrame()
	for i := range a {
		a[i] = int16(100 + i)
		b[i] = int16(-50 - i)
	}

	m := NewMixer()
	m.Reset()
	m.Add(a)
	m.Add(b)
	if m.Contributors() != 2 {
		t.Fatalf("contributors = %d, want 2", m.Contributors())
	}

	dst := NewFrame()
	m.MixMinus(a, dst)
	for i := range dst {
		if dst[i] != b[i] {
			t.Fatalf("leg A heard %d at sample %d, want B's %d", dst[i], i, b[i])
		}
	}
	m.MixMinus(b, dst)
	for i := range dst {
		if dst[i] != a[i] {
			t.Fatalf("leg B heard %d at sample %d, want A's %d", dst[i], i, a[i])
		}
	}
}

// TestMixerSaturates verifies the sum clamps instead of wrapping. A wrap turns
// a loud conference into white noise.
func TestMixerSaturates(t *testing.T) {
	loud := NewFrame()
	for i := range loud {
		loud[i] = 30000
	}
	m := NewMixer()
	m.Reset()
	m.Add(loud)
	m.Add(loud)

	dst := NewFrame()
	m.MixMinus(nil, dst)
	for i := range dst {
		if dst[i] != math.MaxInt16 {
			t.Fatalf("sample %d = %d, want saturation at %d", i, dst[i], math.MaxInt16)
		}
	}
}

func TestJitterBufferInOrder(t *testing.T) {
	jb := NewJitterBuffer(3*FrameSamples, 12*FrameSamples)
	const base = 160000

	for i := 0; i < 6; i++ {
		jb.Push(uint32(base+i*FrameSamples), ramp(int16(i+1)))
	}

	dst := NewFrame()
	for i := 0; i < 6; i++ {
		if !jb.Pull(dst) {
			t.Fatalf("pull %d concealed, expected a real frame", i)
		}
		if dst[0] != int16(i+1) {
			t.Fatalf("pull %d returned frame %d, want %d", i, dst[0], i+1)
		}
	}
}

// TestJitterBufferReordering is the buffer's whole purpose: a packet that
// arrives out of order but before its playout slot must still be played in
// order.
func TestJitterBufferReordering(t *testing.T) {
	jb := NewJitterBuffer(3*FrameSamples, 12*FrameSamples)
	const base = 160000

	jb.Push(base+0*FrameSamples, ramp(1))
	jb.Push(base+2*FrameSamples, ramp(3)) // arrives early
	jb.Push(base+1*FrameSamples, ramp(2)) // the late one catches up
	jb.Push(base+3*FrameSamples, ramp(4))

	dst := NewFrame()
	for want := int16(1); want <= 4; want++ {
		if !jb.Pull(dst) {
			t.Fatalf("frame %d concealed", want)
		}
		if dst[0] != want {
			t.Fatalf("out of order: got frame %d, want %d", dst[0], want)
		}
	}
}

// TestJitterBufferPrimingIsNotLoss covers the startup contract: the buffer
// stalls until it holds the target delay, and that stall must not be reported
// as packet loss or a healthy line will look broken in the metrics.
func TestJitterBufferPrimingIsNotLoss(t *testing.T) {
	jb := NewJitterBuffer(3*FrameSamples, 12*FrameSamples)
	jb.Push(7000, ramp(1))

	dst := NewFrame()
	if jb.Pull(dst) {
		t.Fatal("buffer played out before it primed")
	}
	if dst.RMS() != 0 {
		t.Error("priming should emit silence")
	}
	if st := jb.Stats(); st.Lost != 0 {
		t.Errorf("Lost = %d during priming, want 0", st.Lost)
	}
	if !jb.Stats().Priming {
		t.Error("expected the buffer to report that it is priming")
	}

	// Once the target delay is banked, playout starts from the first packet.
	jb.Push(7000+1*FrameSamples, ramp(2))
	jb.Push(7000+2*FrameSamples, ramp(3))
	jb.Push(7000+3*FrameSamples, ramp(4))

	if !jb.Pull(dst) || dst[0] != 1 {
		t.Fatalf("after priming got frame %d, want 1", dst[0])
	}
}

// TestJitterBufferPrimingIsBounded makes sure a stream that stops after one
// packet cannot wedge the leg in a permanent stall.
func TestJitterBufferPrimingIsBounded(t *testing.T) {
	jb := NewJitterBuffer(2*FrameSamples, 8*FrameSamples)
	jb.Push(400, ramp(9))

	dst := NewFrame()
	var got bool
	for i := 0; i < 10 && !got; i++ {
		got = jb.Pull(dst)
	}
	if !got {
		t.Fatal("buffer never gave up priming on a stalled stream")
	}
	if dst[0] != 9 {
		t.Errorf("got frame %d, want the single buffered frame 9", dst[0])
	}
}

// TestJitterBufferConcealsGap checks that a missing packet produces a decaying
// repeat rather than a click, and that repetition stops before it buzzes.
func TestJitterBufferConcealsGap(t *testing.T) {
	jb := NewJitterBuffer(1*FrameSamples, 8*FrameSamples)
	const base = 5000

	loud := make([]int16, FrameSamples)
	for i := range loud {
		loud[i] = 10000
	}
	jb.Push(base, loud)
	jb.Push(base+FrameSamples, loud)

	dst := NewFrame()
	for i := 0; i < 2; i++ {
		if !jb.Pull(dst) || dst[0] != 10000 {
			t.Fatalf("pull %d = %d, want the pushed frame", i, dst[0])
		}
	}

	// First concealed frame should be a quieter echo of the last good one.
	if jb.Pull(dst) {
		t.Fatal("expected concealment for the missing packet")
	}
	if dst[0] >= 10000 || dst[0] <= 4000 {
		t.Errorf("first concealed sample %d, want a fading repeat of 10000", dst[0])
	}

	// By the fourth consecutive loss it must have decayed to silence.
	for i := 0; i < 3; i++ {
		jb.Pull(dst)
	}
	if dst[0] != 0 {
		t.Errorf("sustained loss still emitting %d, want silence", dst[0])
	}

	if st := jb.Stats(); st.Lost < 4 {
		t.Errorf("Lost = %d, want at least 4", st.Lost)
	}
}

// TestJitterBufferDropsLate verifies a packet arriving after its slot is
// discarded rather than played out of sequence.
func TestJitterBufferDropsLate(t *testing.T) {
	jb := NewJitterBuffer(1*FrameSamples, 8*FrameSamples)
	const base = 900

	jb.Push(base, ramp(1))
	jb.Push(base+1*FrameSamples, ramp(2))
	jb.Push(base+2*FrameSamples, ramp(3))

	dst := NewFrame()
	jb.Pull(dst)
	jb.Pull(dst) // cursor is now past base+FrameSamples

	jb.Push(base+FrameSamples, ramp(2)) // too late now
	if st := jb.Stats(); st.Late != 1 {
		t.Errorf("Late = %d, want 1", st.Late)
	}
}

// TestJitterBufferResync covers silence suppression: after a long gap the
// buffer must rebase instead of stalling for the whole discontinuity.
func TestJitterBufferResync(t *testing.T) {
	jb := NewJitterBuffer(2*FrameSamples, 8*FrameSamples)

	jb.Push(1000, ramp(1))
	jb.Push(1000+SampleRate*30, ramp(2)) // 30 seconds later

	if st := jb.Stats(); st.Resync != 1 {
		t.Fatalf("Resync = %d, want 1", st.Resync)
	}
	dst := NewFrame()
	var got bool
	for i := 0; i < 10 && !got; i++ {
		got = jb.Pull(dst)
	}
	if !got || dst[0] != 2 {
		t.Errorf("after resync got frame %d, want 2", dst[0])
	}
}

// TestJitterBufferTimestampRollover covers the 32-bit RTP timestamp wrapping,
// which silently breaks buffers that compare timestamps with unsigned <.
func TestJitterBufferTimestampRollover(t *testing.T) {
	jb := NewJitterBuffer(2*FrameSamples, 8*FrameSamples)
	base := uint32(math.MaxUint32 - FrameSamples + 1) // wraps on the next frame

	jb.Push(base, ramp(1))
	jb.Push(base+FrameSamples, ramp(2)) // wrapped past zero
	jb.Push(base+2*FrameSamples, ramp(3))

	dst := NewFrame()
	for want := int16(1); want <= 3; want++ {
		if !jb.Pull(dst) {
			t.Fatalf("frame %d concealed across rollover", want)
		}
		if dst[0] != want {
			t.Fatalf("across rollover got %d, want %d", dst[0], want)
		}
	}
}

func TestVADDetectsSpeechOverNoise(t *testing.T) {
	v := NewVAD(300, 100*time.Millisecond)

	noise := NewFrame()
	for i := range noise {
		noise[i] = int16(200 * math.Sin(float64(i)))
	}
	for i := 0; i < 50; i++ {
		if ev := v.Push(noise); ev == EventSpeechStart {
			t.Fatal("VAD triggered on background noise")
		}
	}

	speech := NewFrame()
	for i := range speech {
		speech[i] = int16(9000 * math.Sin(2*math.Pi*300*float64(i)/SampleRate))
	}
	var started bool
	for i := 0; i < 10 && !started; i++ {
		started = v.Push(speech) == EventSpeechStart
	}
	if !started {
		t.Fatal("VAD never detected speech")
	}

	var ended bool
	for i := 0; i < 40 && !ended; i++ {
		ended = v.Push(noise) == EventSpeechEnd
	}
	if !ended {
		t.Fatal("VAD never detected end of speech")
	}
}

// TestResampleRejectsAliasing feeds a 6 kHz tone at 16 kHz. Without the
// anti-alias filter it would fold to 2 kHz in the 8 kHz output and be clearly
// audible; with it, the output must be near silent.
func TestResampleRejectsAliasing(t *testing.T) {
	const inRate = 16000
	in := make([]int16, inRate) // one second
	for i := range in {
		in[i] = int16(12000 * math.Sin(2*math.Pi*6000*float64(i)/inRate))
	}

	out := Resample(in, inRate)
	if len(out) != SampleRate {
		t.Fatalf("resampled length %d, want %d", len(out), SampleRate)
	}

	// Ignore filter settling at the edges.
	body := Frame(out[400 : len(out)-400])
	if rms := body.RMS(); rms > 1200 {
		t.Errorf("aliased energy RMS %.0f, expected the 6 kHz tone to be filtered out", rms)
	}
}

// TestResamplePreservesVoiceBand confirms the filter does not also remove the
// speech we care about.
func TestResamplePreservesVoiceBand(t *testing.T) {
	const inRate = 16000
	in := make([]int16, inRate)
	for i := range in {
		in[i] = int16(12000 * math.Sin(2*math.Pi*800*float64(i)/inRate))
	}

	out := Resample(in, inRate)
	body := Frame(out[400 : len(out)-400])
	if rms := body.RMS(); rms < 6000 {
		t.Errorf("800 Hz tone survived at RMS %.0f, expected roughly 8485", rms)
	}
}

func TestWAVRoundTrip(t *testing.T) {
	pcm := make([]int16, 800)
	for i := range pcm {
		pcm[i] = int16(5000 * math.Sin(float64(i)/10))
	}

	decoded, rate, err := DecodeWAV(EncodeWAV(pcm, 16000))
	if err != nil {
		t.Fatalf("DecodeWAV: %v", err)
	}
	if rate != 16000 {
		t.Errorf("sample rate = %d, want 16000", rate)
	}
	if len(decoded) != len(pcm) {
		t.Fatalf("decoded %d samples, want %d", len(decoded), len(pcm))
	}
	for i := range pcm {
		if decoded[i] != pcm[i] {
			t.Fatalf("sample %d = %d, want %d", i, decoded[i], pcm[i])
		}
	}
}

func TestDecodeWAVSkipsMetadataChunks(t *testing.T) {
	pcm := []int16{1, 2, 3, 4}
	base := EncodeWAV(pcm, 8000)

	// Splice a LIST chunk between fmt and data, as real providers do.
	list := []byte{'L', 'I', 'S', 'T', 4, 0, 0, 0, 'I', 'N', 'F', 'O'}
	spliced := make([]byte, 0, len(base)+len(list))
	spliced = append(spliced, base[:36]...)
	spliced = append(spliced, list...)
	spliced = append(spliced, base[36:]...)

	decoded, _, err := DecodeWAV(spliced)
	if err != nil {
		t.Fatalf("DecodeWAV with LIST chunk: %v", err)
	}
	if len(decoded) != len(pcm) {
		t.Fatalf("decoded %d samples, want %d", len(decoded), len(pcm))
	}
}

func TestDecodeWAVRejectsNonWAV(t *testing.T) {
	if _, _, err := DecodeWAV([]byte("ID3\x04not audio at all")); err == nil {
		t.Fatal("expected an error for a non-WAVE payload")
	}
}

// ramp builds a frame whose every sample is v, so tests can identify frames by
// their first sample.
func ramp(v int16) []int16 {
	f := make([]int16, FrameSamples)
	for i := range f {
		f[i] = v
	}
	return f
}

func abs16(v int16) int16 {
	if v < 0 {
		return -v
	}
	return v
}
