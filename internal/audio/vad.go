package audio

import "time"

// VAD is an energy-based voice activity detector with an adaptive noise floor
// and hysteresis.
//
// A fixed energy threshold fails on real phone calls because line noise varies
// by orders of magnitude between a clean SIP trunk and a GSM handset in a
// market. So the detector tracks the noise floor during silence and triggers
// on a ratio above it. Hysteresis (a lower threshold to stay in speech than to
// enter it) stops the detector chattering on the short pauses inside a word,
// which would otherwise chop an utterance into fragments.
type VAD struct {
	// floor is the slowly adapting estimate of background noise RMS.
	floor float64
	// onRatio and offRatio give the hysteresis band.
	onRatio  float64
	offRatio float64
	// minRMS is an absolute gate so a silent line cannot adapt its floor to
	// near zero and then trigger on dither.
	minRMS float64

	speaking     bool
	hangover     int
	hangoverMax  int
	speechFrames int
	minSpeech    int
}

// NewVAD builds a detector. minRMS is the absolute noise gate and endOfSpeech
// is how much trailing silence closes an utterance.
func NewVAD(minRMS float64, endOfSpeech time.Duration) *VAD {
	if minRMS <= 0 {
		minRMS = 300
	}
	frames := int(endOfSpeech / (FrameDurationMs * time.Millisecond))
	if frames < 1 {
		frames = 1
	}
	return &VAD{
		floor:       minRMS,
		onRatio:     3.0,
		offRatio:    1.8,
		minRMS:      minRMS,
		hangoverMax: frames,
		// Two frames (40 ms) of energy before declaring speech rejects clicks
		// and DTMF key noise.
		minSpeech: 2,
	}
}

// Event describes a transition reported by Push.
type Event uint8

const (
	// EventNone means no state change this frame.
	EventNone Event = iota
	// EventSpeechStart means the talker began speaking.
	EventSpeechStart
	// EventSpeechEnd means the trailing silence threshold elapsed.
	EventSpeechEnd
)

// Push feeds one frame and reports any transition.
func (v *VAD) Push(f Frame) Event {
	rms := f.RMS()

	// Adapt the floor only while idle, and only upward quickly / downward
	// slowly, so a burst of speech cannot drag the floor up and deafen the
	// detector.
	if !v.speaking {
		if rms > v.floor {
			v.floor += 0.05 * (rms - v.floor)
		} else {
			v.floor += 0.20 * (rms - v.floor)
		}
		if v.floor < v.minRMS {
			v.floor = v.minRMS
		}
	}

	if !v.speaking {
		if rms > v.floor*v.onRatio {
			v.speechFrames++
			if v.speechFrames >= v.minSpeech {
				v.speaking = true
				v.hangover = v.hangoverMax
				v.speechFrames = 0
				return EventSpeechStart
			}
		} else {
			v.speechFrames = 0
		}
		return EventNone
	}

	if rms > v.floor*v.offRatio {
		v.hangover = v.hangoverMax
		return EventNone
	}
	v.hangover--
	if v.hangover <= 0 {
		v.speaking = false
		return EventSpeechEnd
	}
	return EventNone
}

// Speaking reports the current state.
func (v *VAD) Speaking() bool { return v.speaking }

// NoiseFloor exposes the adapted floor for diagnostics.
func (v *VAD) NoiseFloor() float64 { return v.floor }

// Reset returns the detector to idle.
func (v *VAD) Reset() {
	v.speaking = false
	v.hangover = 0
	v.speechFrames = 0
}
