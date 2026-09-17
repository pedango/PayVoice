// Package stt streams call audio to a speech recognition provider.
package stt

import (
	"context"
	"log/slog"

	"github.com/pedango/pedango/internal/config"
)

// Result is one recognition update.
type Result struct {
	Text string `json:"text"`
	// Final marks a stable result. Interim results arrive continuously while
	// the caller is still speaking and must not be acted on: acting on an
	// interim "send five" that resolves to "send fifty" would move the wrong
	// amount of money.
	Final      bool    `json:"final"`
	Confidence float64 `json:"confidence"`
}

// Stream is an open recognition session for one leg.
type Stream interface {
	// Write submits 8 kHz linear PCM. It must never block the media clock.
	Write(pcm []int16) error
	Close() error
}

// Provider opens recognition streams.
type Provider interface {
	Name() string
	Open(ctx context.Context, onResult func(Result)) (Stream, error)
}

// Engine selects the configured provider.
type Engine struct {
	cfg      config.STTConfig
	log      *slog.Logger
	provider Provider
}

// NewEngine builds the engine. When no provider is configured the engine is
// disabled and callers fall back to DTMF, which is the safer input channel for
// money movement anyway.
func NewEngine(cfg config.STTConfig, log *slog.Logger) *Engine {
	e := &Engine{cfg: cfg, log: log}

	switch cfg.Provider {
	case "deepgram":
		if cfg.APIKey == "" {
			log.Warn("deepgram selected but PEDANGO_STT_API_KEY is empty, speech recognition disabled")
			break
		}
		e.provider = newDeepgram(cfg, log)
	case "", "none":
	default:
		log.Warn("unknown STT provider, speech recognition disabled", "provider", cfg.Provider)
	}
	return e
}

// Enabled reports whether recognition is available.
func (e *Engine) Enabled() bool { return e.provider != nil }

// Name reports the active provider, or "none".
func (e *Engine) Name() string {
	if e.provider == nil {
		return "none"
	}
	return e.provider.Name()
}

// Open starts a recognition stream.
func (e *Engine) Open(ctx context.Context, onResult func(Result)) (Stream, error) {
	if e.provider == nil {
		return nopStream{}, nil
	}
	return e.provider.Open(ctx, onResult)
}

// nopStream is returned when recognition is disabled, so callers need no
// special case.
type nopStream struct{}

func (nopStream) Write([]int16) error { return nil }
func (nopStream) Close() error        { return nil }
