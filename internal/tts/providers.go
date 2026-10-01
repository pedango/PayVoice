package tts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/pedango/PayVoice/internal/audio"
	"github.com/pedango/PayVoice/internal/config"
)

// maxAudioBytes caps a provider response. Without a cap, a misconfigured
// provider streaming an error page or an enormous file would be read into
// memory in full.
const maxAudioBytes = 8 << 20

// elevenLabs synthesizes through the ElevenLabs API.
type elevenLabs struct {
	cfg    config.TTSConfig
	client *http.Client
}

func newElevenLabs(cfg config.TTSConfig) *elevenLabs {
	return &elevenLabs{cfg: cfg, client: &http.Client{Timeout: cfg.Timeout}}
}

func (e *elevenLabs) Name() string { return "elevenlabs" }

func (e *elevenLabs) Synthesize(ctx context.Context, r Request) ([]int16, error) {
	voice := r.Voice
	if voice == "" {
		voice = e.cfg.VoiceID
	}

	// Asking for ulaw_8000 makes the provider do the resampling, so PayVoice
	// receives exactly the format the telephone network wants and skips both a
	// decode and a downsample on the critical path.
	url := fmt.Sprintf(
		"https://api.elevenlabs.io/v1/text-to-speech/%s/stream?output_format=ulaw_8000",
		voice,
	)

	body, err := json.Marshal(map[string]any{
		"text":     r.Text,
		"model_id": e.cfg.Model,
		"voice_settings": map[string]any{
			"stability":        0.5,
			"similarity_boost": 0.75,
		},
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "audio/basic")
	req.Header.Set("xi-api-key", e.cfg.APIKey)

	res, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tts: elevenlabs request: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return nil, fmt.Errorf("tts: elevenlabs replied %d: %s",
			res.StatusCode, strings.TrimSpace(string(detail)))
	}

	raw, err := io.ReadAll(io.LimitReader(res.Body, maxAudioBytes))
	if err != nil {
		return nil, fmt.Errorf("tts: reading elevenlabs audio: %w", err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("tts: elevenlabs returned no audio")
	}

	return audio.CodecPCMU.Decode(nil, raw), nil
}

// httpProvider posts to a generic synthesis endpoint, which is how a
// self-hosted engine such as Piper or Coqui is plugged in.
type httpProvider struct {
	cfg    config.TTSConfig
	client *http.Client
}

func newHTTPProvider(cfg config.TTSConfig) *httpProvider {
	return &httpProvider{cfg: cfg, client: &http.Client{Timeout: cfg.Timeout}}
}

func (h *httpProvider) Name() string { return "http" }

func (h *httpProvider) Synthesize(ctx context.Context, r Request) ([]int16, error) {
	body, err := json.Marshal(map[string]any{
		"text":        r.Text,
		"voice":       r.Voice,
		"sample_rate": audio.SampleRate,
		"format":      "wav",
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.cfg.BaseURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if h.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+h.cfg.APIKey)
	}

	res, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tts: http provider: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return nil, fmt.Errorf("tts: http provider replied %d: %s",
			res.StatusCode, strings.TrimSpace(string(detail)))
	}

	raw, err := io.ReadAll(io.LimitReader(res.Body, maxAudioBytes))
	if err != nil {
		return nil, err
	}

	// Accept a WAV container, and fall back to headerless 16-bit PCM, which is
	// what several self-hosted engines emit.
	pcm, rate, err := audio.DecodeWAV(raw)
	if err != nil {
		pcm, rate = audio.DecodeRawPCM16LE(raw), audio.SampleRate
	}
	if rate != audio.SampleRate {
		pcm = audio.Resample(pcm, rate)
	}
	return pcm, nil
}

// toneProvider renders text as a short pattern of tones.
//
// It exists so that the media path can be exercised end to end with no vendor
// account: if a caller hears these beeps, then synthesis, queueing, mixing,
// packetization and transport are all working, and any silence afterwards is a
// provider problem rather than a PayVoice problem.
type toneProvider struct{}

func (toneProvider) Name() string { return "tone" }

func (toneProvider) Synthesize(_ context.Context, r Request) ([]int16, error) {
	words := strings.Fields(r.Text)
	if len(words) == 0 {
		words = []string{"payvoice"}
	}
	if len(words) > 24 {
		words = words[:24]
	}

	var out []int16
	for i, w := range words {
		// Pitch follows word length so the pattern is recognisably tied to the
		// text rather than a uniform buzz.
		freq := 320.0 + float64(len(w)%7)*55.0
		dur := 90 + len(w)*18
		if dur > 320 {
			dur = 320
		}
		out = append(out, tone(freq, dur)...)

		gap := 60
		if i == len(words)-1 {
			gap = 220
		}
		out = append(out, make([]int16, gap*audio.SamplesPerMs)...)
	}
	return out, nil
}

// tone renders a sine with a short raised-cosine envelope. The envelope is not
// decoration: an abrupt start or stop produces a broadband click that is
// clearly audible over a phone line.
func tone(freq float64, durationMs int) []int16 {
	n := durationMs * audio.SamplesPerMs
	out := make([]int16, n)
	ramp := 4 * audio.SamplesPerMs

	for i := 0; i < n; i++ {
		env := 1.0
		switch {
		case i < ramp:
			env = 0.5 * (1 - math.Cos(math.Pi*float64(i)/float64(ramp)))
		case i > n-ramp:
			env = 0.5 * (1 - math.Cos(math.Pi*float64(n-i)/float64(ramp)))
		}
		v := 8000 * env * math.Sin(2*math.Pi*freq*float64(i)/audio.SampleRate)
		out[i] = int16(v)
	}
	return out
}

// ensure the timeout type stays referenced if the file is trimmed.
var _ = time.Second
