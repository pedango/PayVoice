package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pion/webrtc/v4"

	"github.com/pedango/PayVoice/internal/audio"
	"github.com/pedango/PayVoice/internal/media"
	"github.com/pedango/PayVoice/internal/rtcsvc"
	"github.com/pedango/PayVoice/internal/sipsvc"
	"github.com/pedango/PayVoice/internal/stt"
	"github.com/pedango/PayVoice/internal/tts"
)

// ---------------------------------------------------------------- health

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"service": "payvoice",
		"uptime":  time.Since(startedAt).String(),
	})
}

// handleReady reports whether the media clock is actually running. A process
// that is listening but whose clock has stopped is worse than one that is
// down, because a load balancer will keep sending it calls.
func (s *Server) handleReady(w http.ResponseWriter, _ *http.Request) {
	st := s.engine.Stats()
	if !st.Running {
		fail(w, http.StatusServiceUnavailable, "not_ready", "Media clock is not running")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
}

var startedAt = time.Now()

func (s *Server) handleStats(w http.ResponseWriter, _ *http.Request) {
	out := map[string]any{
		"engine":   s.engine.Stats(),
		"webhooks": s.hooks.Stats(),
		"tts":      s.tts.Stats(),
		"stt":      map[string]any{"provider": s.stt.Name(), "enabled": s.stt.Enabled()},
		"uptime":   time.Since(startedAt).String(),
	}
	if s.sip != nil {
		out["sip"] = s.sip.Status()
	} else {
		out["sip"] = map[string]any{"enabled": false}
	}
	if s.rtc != nil {
		out["webrtc"] = map[string]any{"peer_connections": s.rtc.Count()}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleConfig gives the application everything it needs to set up a browser
// client, so ICE servers are configured in one place rather than duplicated.
func (s *Server) handleConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"codec":         s.cfg.Media.Codec,
		"sample_rate":   audio.SampleRate,
		"frame_ms":      audio.FrameDurationMs,
		"ice_servers":   s.rtc.ICEServers(),
		"tts_provider":  s.tts.Default(),
		"tts_available": s.tts.Available(),
		"stt_provider":  s.stt.Name(),
		"sip_enabled":   s.sip != nil,
	})
}

// ---------------------------------------------------------------- rooms

type createRoomRequest struct {
	// SampleRate is accepted for compatibility with callers that specify one.
	// PayVoice always mixes at 8 kHz, and the response says so.
	SampleRate int    `json:"sample_rate"`
	ID         string `json:"id"`
}

func (s *Server) handleCreateRoom(w http.ResponseWriter, r *http.Request) {
	var req createRoomRequest
	if r.ContentLength > 0 && !decode(w, r, &req) {
		return
	}

	id := req.ID
	if id == "" {
		id = "room_" + uuid.NewString()
	}
	if s.engine.Room(id) != nil {
		fail(w, http.StatusConflict, "room_exists", "A room with that id already exists")
		return
	}

	room := s.engine.CreateRoom(id)
	if req.SampleRate != 0 && req.SampleRate != audio.SampleRate {
		s.log.Debug("ignoring requested sample rate, payvoice mixes at 8kHz",
			"requested", req.SampleRate)
	}

	snap := room.Snapshot()
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":          snap.ID,
		"created_at":  snap.CreatedAt,
		"legs":        snap.Legs,
		"max_legs":    snap.MaxLegs,
		"sample_rate": audio.SampleRate,
	})
}

func (s *Server) handleListRooms(w http.ResponseWriter, _ *http.Request) {
	rooms := s.engine.Rooms()
	out := make([]media.RoomSnapshot, 0, len(rooms))
	for _, room := range rooms {
		out = append(out, room.Snapshot())
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (s *Server) handleGetRoom(w http.ResponseWriter, r *http.Request) {
	room := s.engine.Room(r.PathValue("id"))
	if room == nil {
		fail(w, http.StatusNotFound, "room_not_found", "No such room")
		return
	}
	writeJSON(w, http.StatusOK, room.Snapshot())
}

func (s *Server) handleDeleteRoom(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.engine.Room(id) == nil {
		fail(w, http.StatusNotFound, "room_not_found", "No such room")
		return
	}
	s.engine.DestroyRoom(id)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "deleted": true})
}

type joinRoomRequest struct {
	LegID string `json:"leg_id"`
}

func (s *Server) handleJoinRoom(w http.ResponseWriter, r *http.Request) {
	var req joinRoomRequest
	if !decode(w, r, &req) {
		return
	}

	room := s.engine.Room(r.PathValue("id"))
	if room == nil {
		fail(w, http.StatusNotFound, "room_not_found", "No such room")
		return
	}
	leg := s.engine.Leg(req.LegID)
	if leg == nil {
		fail(w, http.StatusNotFound, "leg_not_found", "No such leg")
		return
	}

	if err := room.Add(leg); err != nil {
		switch {
		case errors.Is(err, media.ErrRoomFull):
			fail(w, http.StatusConflict, "room_full", "The room is at capacity")
		case errors.Is(err, media.ErrLegInRoom):
			fail(w, http.StatusConflict, "leg_busy", "The leg is already mixed into another room")
		default:
			fail(w, http.StatusBadRequest, "join_failed", err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, room.Snapshot())
}

func (s *Server) handleLeaveRoom(w http.ResponseWriter, r *http.Request) {
	room := s.engine.Room(r.PathValue("id"))
	if room == nil {
		fail(w, http.StatusNotFound, "room_not_found", "No such room")
		return
	}
	room.Remove(r.PathValue("legID"))
	writeJSON(w, http.StatusOK, room.Snapshot())
}

// ---------------------------------------------------------------- legs

type createLegRequest struct {
	Type   string `json:"type"`
	To     string `json:"to"`
	From   string `json:"from"`
	RoomID string `json:"room_id"`
	AppRef string `json:"app_ref"`
}

// handleCreateLeg originates an outbound call. This is the path that turns a
// browser session into a phone call when the caller loses their data
// connection: the application asks for a SIP leg to the caller's number and
// bridges it into the same room.
func (s *Server) handleCreateLeg(w http.ResponseWriter, r *http.Request) {
	var req createLegRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Type != "" && req.Type != "sip" {
		fail(w, http.StatusBadRequest, "unsupported_type",
			"Only SIP legs can be originated; browser legs start from /v1/webrtc/offer")
		return
	}
	if strings.TrimSpace(req.To) == "" {
		fail(w, http.StatusBadRequest, "invalid_request", "A destination number is required")
		return
	}
	if s.sip == nil {
		fail(w, http.StatusServiceUnavailable, "sip_disabled", "SIP is not enabled on this instance")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	leg, err := s.sip.Originate(ctx, req.To, req.From, req.AppRef)
	if err != nil {
		if errors.Is(err, sipsvc.ErrNoTrunk) {
			fail(w, http.StatusServiceUnavailable, "no_trunk", "No SIP trunk is configured")
			return
		}
		fail(w, http.StatusBadGateway, "originate_failed", err.Error())
		return
	}

	// Joining now means the caller hears the room the moment they answer,
	// rather than a gap while the application reacts to leg.answered.
	if req.RoomID != "" {
		if room := s.engine.Room(req.RoomID); room != nil {
			if err := room.Add(leg); err != nil {
				s.log.Warn("could not join originated leg to room",
					"leg", leg.ID, "room", req.RoomID, "error", err)
			}
		}
	}

	writeJSON(w, http.StatusCreated, leg.Snapshot())
}

func (s *Server) handleListLegs(w http.ResponseWriter, _ *http.Request) {
	legs := s.engine.Legs()
	out := make([]media.Snapshot, 0, len(legs))
	for _, leg := range legs {
		out = append(out, leg.Snapshot())
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (s *Server) handleGetLeg(w http.ResponseWriter, r *http.Request) {
	leg := s.engine.Leg(r.PathValue("id"))
	if leg == nil {
		fail(w, http.StatusNotFound, "leg_not_found", "No such leg")
		return
	}
	writeJSON(w, http.StatusOK, leg.Snapshot())
}

func (s *Server) handleAnswer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	leg := s.engine.Leg(id)
	if leg == nil {
		fail(w, http.StatusNotFound, "leg_not_found", "No such leg")
		return
	}
	if s.sip == nil || leg.Kind != "sip" {
		// A browser leg answers itself when ICE connects, so there is nothing
		// to do and reporting success keeps callers uniform.
		writeJSON(w, http.StatusOK, leg.Snapshot())
		return
	}

	if err := s.sip.Answer(id); err != nil {
		switch {
		case errors.Is(err, sipsvc.ErrLegNotFound):
			fail(w, http.StatusNotFound, "leg_not_found", "No SIP dialog for that leg")
		case errors.Is(err, sipsvc.ErrNotRinging):
			fail(w, http.StatusConflict, "not_ringing", "The leg is not ringing")
		default:
			fail(w, http.StatusBadGateway, "answer_failed", err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, leg.Snapshot())
}

type hangupRequest struct {
	Code   int    `json:"code"`
	Reason string `json:"reason"`
}

func (s *Server) handleHangup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	leg := s.engine.Leg(id)
	if leg == nil {
		fail(w, http.StatusNotFound, "leg_not_found", "No such leg")
		return
	}

	var req hangupRequest
	if r.ContentLength > 0 {
		_ = decode(w, r, &req)
	}

	s.closeStream(id)

	switch leg.Kind {
	case "sip":
		if s.sip != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			defer cancel()
			if err := s.sip.Hangup(ctx, id, req.Code, req.Reason); err != nil {
				s.log.Warn("sip hangup failed, closing leg anyway", "leg", id, "error", err)
			}
		}
	case "webrtc":
		if s.rtc != nil {
			if err := s.rtc.Hangup(id); err != nil {
				s.log.Debug("webrtc hangup", "leg", id, "error", err)
			}
		}
	}

	leg.Close("api_hangup")
	s.engine.RemoveLeg(id)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "ended": true})
}

// ---------------------------------------------------------------- playback

type ttsRequest struct {
	Text     string  `json:"text"`
	Provider string  `json:"provider"`
	Voice    string  `json:"voice"`
	Gain     float64 `json:"gain"`
	// Interrupt clears anything already queued, which is how the agent cuts
	// itself off to answer a caller who just spoke.
	Interrupt bool `json:"interrupt"`
}

func (s *Server) handleLegTTS(w http.ResponseWriter, r *http.Request) {
	var req ttsRequest
	if !decode(w, r, &req) {
		return
	}
	leg := s.engine.Leg(r.PathValue("id"))
	if leg == nil {
		fail(w, http.StatusNotFound, "leg_not_found", "No such leg")
		return
	}

	pcm, ok := s.synth(w, r, req)
	if !ok {
		return
	}

	if req.Interrupt {
		leg.Player().Stop()
	}
	playbackID := "pb_" + uuid.NewString()
	depth := leg.Player().Enqueue(playbackID, pcm, req.Gain)

	writeJSON(w, http.StatusAccepted, map[string]any{
		"playback_id": playbackID,
		"leg_id":      leg.ID,
		"duration_ms": len(pcm) / audio.SamplesPerMs,
		"queued":      depth,
	})
}

func (s *Server) handleRoomTTS(w http.ResponseWriter, r *http.Request) {
	var req ttsRequest
	if !decode(w, r, &req) {
		return
	}
	room := s.engine.Room(r.PathValue("id"))
	if room == nil {
		fail(w, http.StatusNotFound, "room_not_found", "No such room")
		return
	}

	pcm, ok := s.synth(w, r, req)
	if !ok {
		return
	}

	if req.Interrupt {
		room.Player().Stop()
	}
	playbackID := "pb_" + uuid.NewString()
	depth := room.Player().Enqueue(playbackID, pcm, req.Gain)

	writeJSON(w, http.StatusAccepted, map[string]any{
		"playback_id": playbackID,
		"room_id":     room.ID,
		"duration_ms": len(pcm) / audio.SamplesPerMs,
		"queued":      depth,
	})
}

// synth renders text and reports failures as HTTP errors.
func (s *Server) synth(w http.ResponseWriter, r *http.Request, req ttsRequest) ([]int16, bool) {
	if strings.TrimSpace(req.Text) == "" {
		fail(w, http.StatusBadRequest, "invalid_request", "text is required")
		return nil, false
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.TTS.Timeout)
	defer cancel()

	pcm, err := s.tts.Synthesize(ctx, tts.Request{
		Text:     req.Text,
		Voice:    req.Voice,
		Provider: req.Provider,
	})
	if err != nil {
		fail(w, http.StatusBadGateway, "tts_failed", err.Error())
		return nil, false
	}
	return pcm, true
}

func (s *Server) handleStopPlayback(w http.ResponseWriter, r *http.Request) {
	leg := s.engine.Leg(r.PathValue("id"))
	if leg == nil {
		fail(w, http.StatusNotFound, "leg_not_found", "No such leg")
		return
	}
	stopped := leg.Player().Stop()
	writeJSON(w, http.StatusOK, map[string]any{"stopped": stopped})
}

type dtmfRequest struct {
	Digits     string `json:"digits"`
	DurationMs int    `json:"duration_ms"`
}

func (s *Server) handleSendDTMF(w http.ResponseWriter, r *http.Request) {
	var req dtmfRequest
	if !decode(w, r, &req) {
		return
	}
	leg := s.engine.Leg(r.PathValue("id"))
	if leg == nil {
		fail(w, http.StatusNotFound, "leg_not_found", "No such leg")
		return
	}
	transport := leg.Transport()
	if transport == nil {
		fail(w, http.StatusConflict, "leg_closed", "The leg has no transport")
		return
	}

	duration := time.Duration(req.DurationMs) * time.Millisecond
	if duration <= 0 {
		duration = 160 * time.Millisecond
	}

	for _, digit := range req.Digits {
		if err := transport.SendDTMF(digit, duration); err != nil {
			fail(w, http.StatusBadGateway, "dtmf_failed", err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sent": req.Digits})
}

type muteRequest struct {
	Muted bool `json:"muted"`
}

func (s *Server) handleMute(w http.ResponseWriter, r *http.Request) {
	var req muteRequest
	if !decode(w, r, &req) {
		return
	}
	leg := s.engine.Leg(r.PathValue("id"))
	if leg == nil {
		fail(w, http.StatusNotFound, "leg_not_found", "No such leg")
		return
	}
	leg.SetMuted(req.Muted)
	writeJSON(w, http.StatusOK, map[string]any{"id": leg.ID, "muted": req.Muted})
}

// ---------------------------------------------------------------- transcription

type transcribeRequest struct {
	Enabled bool `json:"enabled"`
}

// handleTranscribe attaches or detaches speech recognition on a leg. Audio is
// tapped from the receive path, so what the recognizer hears is exactly what
// the mixer heard, after jitter buffering and concealment.
func (s *Server) handleTranscribe(w http.ResponseWriter, r *http.Request) {
	var req transcribeRequest
	if !decode(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	leg := s.engine.Leg(id)
	if leg == nil {
		fail(w, http.StatusNotFound, "leg_not_found", "No such leg")
		return
	}

	if !req.Enabled {
		leg.SetTap(nil)
		s.closeStream(id)
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "transcribing": false})
		return
	}

	if !s.stt.Enabled() {
		fail(w, http.StatusServiceUnavailable, "stt_disabled",
			"No speech recognition provider is configured")
		return
	}

	stream, err := s.stt.Open(context.Background(), func(res stt.Result) {
		kind := "stt.partial"
		if res.Final {
			kind = "stt.final"
		}
		leg.Emit(kind, map[string]any{
			"transcript": res.Text,
			"confidence": res.Confidence,
			"is_final":   res.Final,
		})
	})
	if err != nil {
		fail(w, http.StatusBadGateway, "stt_failed", err.Error())
		return
	}

	s.streamMu.Lock()
	if existing, ok := s.streams[id]; ok {
		_ = existing.Close()
	}
	s.streams[id] = stream
	s.streamMu.Unlock()

	// The tap runs on the media clock, so it only hands the frame to the
	// stream's queue and returns.
	leg.SetTap(func(f audio.Frame) { _ = stream.Write(f) })

	// Recognition must not outlive the call.
	go func() {
		<-leg.Done()
		s.closeStream(id)
	}()

	writeJSON(w, http.StatusOK, map[string]any{
		"id":           id,
		"transcribing": true,
		"provider":     s.stt.Name(),
	})
}

func (s *Server) closeStream(legID string) {
	s.streamMu.Lock()
	stream, ok := s.streams[legID]
	delete(s.streams, legID)
	s.streamMu.Unlock()

	if ok {
		_ = stream.Close()
	}
}

// ---------------------------------------------------------------- webrtc

type offerRequest struct {
	SDP    string `json:"sdp"`
	RoomID string `json:"room_id"`
	From   string `json:"from"`
	To     string `json:"to"`
	AppRef string `json:"app_ref"`
}

func (s *Server) handleWebRTCOffer(w http.ResponseWriter, r *http.Request) {
	var req offerRequest
	if !decode(w, r, &req) {
		return
	}
	if s.rtc == nil {
		fail(w, http.StatusServiceUnavailable, "webrtc_disabled", "WebRTC is not available")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	leg, answer, err := s.rtc.Offer(ctx, rtcsvc.OfferRequest{
		SDP:    req.SDP,
		From:   req.From,
		To:     req.To,
		AppRef: req.AppRef,
	})
	if err != nil {
		if errors.Is(err, rtcsvc.ErrNoCommonCodec) {
			fail(w, http.StatusUnsupportedMediaType, "no_common_codec",
				"The browser must offer PCMU. PayVoice does not negotiate Opus.")
			return
		}
		fail(w, http.StatusBadRequest, "offer_rejected", err.Error())
		return
	}

	if req.RoomID != "" {
		if room := s.engine.Room(req.RoomID); room != nil {
			if err := room.Add(leg); err != nil {
				s.log.Warn("could not join webrtc leg to room",
					"leg", leg.ID, "room", req.RoomID, "error", err)
			}
		}
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"leg_id": answer.LegID,
		"sdp":    answer.SDP,
		"type":   "answer",
	})
}

// handleICECandidate accepts a trickled candidate from the browser. PayVoice's
// own candidates are already in the answer, so this channel is one-way.
func (s *Server) handleICECandidate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Candidate struct {
			Candidate     string  `json:"candidate"`
			SDPMid        *string `json:"sdpMid"`
			SDPMLineIndex *uint16 `json:"sdpMLineIndex"`
		} `json:"candidate"`
	}
	if !decode(w, r, &req) {
		return
	}
	if s.rtc == nil {
		fail(w, http.StatusServiceUnavailable, "webrtc_disabled", "WebRTC is not available")
		return
	}

	err := s.rtc.AddICECandidate(r.PathValue("id"), webrtc.ICECandidateInit{
		Candidate:     req.Candidate.Candidate,
		SDPMid:        req.Candidate.SDPMid,
		SDPMLineIndex: req.Candidate.SDPMLineIndex,
	})
	if err != nil {
		fail(w, http.StatusBadRequest, "ice_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"added": true})
}
