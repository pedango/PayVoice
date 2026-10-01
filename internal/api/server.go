// Package api exposes PayVoice's control plane over HTTP.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/pedango/PayVoice/internal/config"
	"github.com/pedango/PayVoice/internal/media"
	"github.com/pedango/PayVoice/internal/rtcsvc"
	"github.com/pedango/PayVoice/internal/sipsvc"
	"github.com/pedango/PayVoice/internal/stt"
	"github.com/pedango/PayVoice/internal/tts"
	"github.com/pedango/PayVoice/internal/webhook"
)

// Server is the control-plane HTTP server.
type Server struct {
	cfg    *config.Config
	engine *media.Engine
	sip    *sipsvc.Service
	rtc    *rtcsvc.Service
	tts    *tts.Engine
	stt    *stt.Engine
	hooks  *webhook.Dispatcher
	log    *slog.Logger

	http *http.Server

	// streams tracks open recognition sessions so they can be closed with
	// their leg.
	streamMu sync.Mutex
	streams  map[string]stt.Stream
}

// Options configures the server.
type Options struct {
	Config *config.Config
	Engine *media.Engine
	SIP    *sipsvc.Service
	WebRTC *rtcsvc.Service
	TTS    *tts.Engine
	STT    *stt.Engine
	Hooks  *webhook.Dispatcher
	Log    *slog.Logger
}

// New builds the server and its routes.
func New(o Options) *Server {
	s := &Server{
		cfg:     o.Config,
		engine:  o.Engine,
		sip:     o.SIP,
		rtc:     o.WebRTC,
		tts:     o.TTS,
		stt:     o.STT,
		hooks:   o.Hooks,
		log:     o.Log,
		streams: make(map[string]stt.Stream),
	}

	mux := http.NewServeMux()

	// Health and introspection are deliberately unauthenticated so a load
	// balancer can probe them, and deliberately free of call detail.
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReady)

	mux.Handle("GET /v1/stats", s.protect(s.handleStats))
	mux.Handle("GET /v1/config", s.protect(s.handleConfig))

	mux.Handle("POST /v1/rooms", s.protect(s.handleCreateRoom))
	mux.Handle("GET /v1/rooms", s.protect(s.handleListRooms))
	mux.Handle("GET /v1/rooms/{id}", s.protect(s.handleGetRoom))
	mux.Handle("DELETE /v1/rooms/{id}", s.protect(s.handleDeleteRoom))
	mux.Handle("POST /v1/rooms/{id}/legs", s.protect(s.handleJoinRoom))
	mux.Handle("DELETE /v1/rooms/{id}/legs/{legID}", s.protect(s.handleLeaveRoom))
	mux.Handle("POST /v1/rooms/{id}/tts", s.protect(s.handleRoomTTS))

	mux.Handle("POST /v1/legs", s.protect(s.handleCreateLeg))
	mux.Handle("GET /v1/legs", s.protect(s.handleListLegs))
	mux.Handle("GET /v1/legs/{id}", s.protect(s.handleGetLeg))
	mux.Handle("DELETE /v1/legs/{id}", s.protect(s.handleHangup))
	mux.Handle("POST /v1/legs/{id}/answer", s.protect(s.handleAnswer))
	mux.Handle("POST /v1/legs/{id}/tts", s.protect(s.handleLegTTS))
	mux.Handle("POST /v1/legs/{id}/stop", s.protect(s.handleStopPlayback))
	mux.Handle("POST /v1/legs/{id}/dtmf", s.protect(s.handleSendDTMF))
	mux.Handle("POST /v1/legs/{id}/mute", s.protect(s.handleMute))
	mux.Handle("POST /v1/legs/{id}/transcribe", s.protect(s.handleTranscribe))
	mux.Handle("POST /v1/legs/{id}/ice-candidates", s.protect(s.handleICECandidate))

	mux.Handle("POST /v1/webrtc/offer", s.protect(s.handleWebRTCOffer))

	s.http = &http.Server{
		Addr:              o.Config.HTTP.Addr,
		Handler:           s.recoverer(s.logger(mux)),
		ReadHeaderTimeout: o.Config.HTTP.ReadTimeout,
		ReadTimeout:       o.Config.HTTP.ReadTimeout,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	return s
}

// Run serves until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.http.Shutdown(shutdown)
	}()

	s.log.Info("control api listening", "addr", s.cfg.HTTP.Addr, "auth", len(s.cfg.HTTP.Tokens) > 0)
	if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// ---------------------------------------------------------------- middleware

// protect wraps a handler in authentication and peer checks.
func (s *Server) protect(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.allowedPeer(r) {
			fail(w, http.StatusForbidden, "forbidden", "Caller address is not allowed")
			return
		}
		if !s.authenticated(r) {
			// The challenge tells an operator what is missing without
			// revealing whether the token was wrong or simply absent.
			w.Header().Set("WWW-Authenticate", `Bearer realm="payvoice"`)
			fail(w, http.StatusUnauthorized, "unauthorized", "Missing or invalid bearer token")
			return
		}
		h(w, r)
	})
}

// authenticated compares the bearer token against every configured token in
// constant time, so a timing side channel cannot be used to recover one.
func (s *Server) authenticated(r *http.Request) bool {
	if len(s.cfg.HTTP.Tokens) == 0 {
		// Only development reaches here; Validate rejects an empty token list
		// in production.
		return true
	}

	header := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return false
	}
	presented := []byte(header[len(prefix):])

	// Every candidate is checked even after a match so the work is constant.
	ok := false
	for _, token := range s.cfg.HTTP.Tokens {
		if subtle.ConstantTimeCompare(presented, []byte(token)) == 1 {
			ok = true
		}
	}
	return ok
}

func (s *Server) allowedPeer(r *http.Request) bool {
	if len(s.cfg.HTTP.AllowedIPs) == 0 {
		return true
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	// X-Forwarded-For is only believed when the deployment says a proxy is in
	// front; otherwise any caller could spoof their way past the allowlist.
	if s.cfg.HTTP.TrustProxy {
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			host = strings.TrimSpace(strings.Split(fwd, ",")[0])
		}
	}

	ip := net.ParseIP(host)
	for _, allowed := range s.cfg.HTTP.AllowedIPs {
		if allowed == host {
			return true
		}
		if _, cidr, err := net.ParseCIDR(allowed); err == nil && ip != nil && cidr.Contains(ip) {
			return true
		}
	}
	return false
}

func (s *Server) logger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		level := slog.LevelDebug
		if rec.status >= 500 {
			level = slog.LevelError
		} else if rec.status >= 400 {
			level = slog.LevelWarn
		}
		s.log.Log(r.Context(), level, "http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"took", time.Since(start),
		)
	})
}

// recoverer keeps a panic in one handler from killing the process and every
// call on it.
func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic in handler", "path", r.URL.Path, "panic", rec)
				fail(w, http.StatusInternalServerError, "internal_error", "Unexpected server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// ---------------------------------------------------------------- responses

// errorBody is the error envelope, matching the shape the PayPlus API uses so
// both services report failures identically.
type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func fail(w http.ResponseWriter, status int, code, message string) {
	var body errorBody
	body.Error.Code = code
	body.Error.Message = message
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

// decode reads a JSON body with a size limit, so a malformed or hostile
// request cannot exhaust memory.
func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		fail(w, http.StatusBadRequest, "invalid_request", "Malformed JSON body: "+err.Error())
		return false
	}
	return true
}
