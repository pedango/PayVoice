// Command payvoice runs the SIP/WebRTC media bridge.
//
// PayVoice is a media plane and nothing more. It carries audio between phone
// calls and browsers, reports what it hears as events, and speaks what it is
// told to speak. Every decision about money, identity or conversation flow
// belongs to the application that consumes those events.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pedango/PayVoice/internal/api"
	"github.com/pedango/PayVoice/internal/config"
	"github.com/pedango/PayVoice/internal/media"
	"github.com/pedango/PayVoice/internal/rtcsvc"
	"github.com/pedango/PayVoice/internal/rtpx"
	"github.com/pedango/PayVoice/internal/sipsvc"
	"github.com/pedango/PayVoice/internal/stt"
	"github.com/pedango/PayVoice/internal/tts"
	"github.com/pedango/PayVoice/internal/webhook"
)

// version is stamped at build time with -ldflags.
var version = "dev"

func main() {
	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "payvoice: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := config.Load()
	log := newLogger(cfg)

	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("configuration: %w", err)
	}

	log.Info("starting payvoice",
		"version", version,
		"env", cfg.Env,
		"codec", cfg.Media.Codec,
		"sip", cfg.SIP.Enabled,
	)

	// Signals must be trapped before anything starts listening, so a Ctrl-C
	// during startup still unwinds cleanly.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	hooks := webhook.New(cfg.Webhook, log)
	hooks.Start()
	if cfg.Webhook.URL == "" {
		log.Warn("no webhook URL configured: events will not be delivered anywhere")
	}

	engine := media.NewEngine(cfg.Media, hooks.Sink())
	ttsEngine := tts.NewEngine(cfg.TTS, log)
	sttEngine := stt.NewEngine(cfg.STT, log)

	rtc, err := rtcsvc.New(rtcsvc.Options{
		Config: cfg.WebRTC,
		Media:  cfg.Media,
		Engine: engine,
		Emit:   hooks.Sink(),
		Log:    log,
	})
	if err != nil {
		return fmt.Errorf("webrtc: %w", err)
	}

	var sip *sipsvc.Service
	if cfg.SIP.Enabled {
		ports := rtpx.NewPortPool(cfg.RTP.Host, cfg.RTP.PortMin, cfg.RTP.PortMax)
		sip, err = sipsvc.New(sipsvc.Options{
			Config: cfg.SIP,
			RTP:    cfg.RTP,
			Media:  cfg.Media,
			Engine: engine,
			Ports:  ports,
			Emit:   hooks.Sink(),
			Log:    log,
			// PayVoice does not decide whether to take a call. It reports the
			// ringing leg and waits for the application to answer or reject.
			OnInbound: func(leg *media.Leg) {
				log.Info("inbound call",
					"leg", leg.ID, "from", leg.From, "to", leg.To)
			},
		})
		if err != nil {
			return fmt.Errorf("sip: %w", err)
		}
	} else {
		log.Info("SIP is disabled; only browser legs will be accepted")
	}

	server := api.New(api.Options{
		Config: cfg,
		Engine: engine,
		SIP:    sip,
		WebRTC: rtc,
		TTS:    ttsEngine,
		STT:    sttEngine,
		Hooks:  hooks,
		Log:    log,
	})

	// Each subsystem reports the first fatal error; the first one to fail
	// brings the process down so an orchestrator can restart it cleanly.
	errs := make(chan error, 3)

	go func() { errs <- engine.Run(ctx) }()
	go func() { errs <- server.Run(ctx) }()
	if sip != nil {
		go func() { errs <- sip.Run(ctx) }()
	}

	var fatal error
	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case fatal = <-errs:
		if fatal != nil && !errors.Is(fatal, context.Canceled) {
			log.Error("subsystem failed", "error", fatal)
		}
		stop()
	}

	// Give in-flight calls a moment to tear down and queued events a chance to
	// drain before the process exits.
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if sip != nil {
		if err := sip.Close(); err != nil {
			log.Warn("sip shutdown", "error", err)
		}
	}
	if err := hooks.Close(shutdown); err != nil {
		log.Warn("webhook shutdown", "error", err)
	}

	log.Info("stopped")
	if fatal != nil && !errors.Is(fatal, context.Canceled) {
		return fatal
	}
	return nil
}

func newLogger(cfg *config.Config) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	opts := &slog.HandlerOptions{Level: level}
	if cfg.LogJSON {
		return slog.New(slog.NewJSONHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, opts))
}
