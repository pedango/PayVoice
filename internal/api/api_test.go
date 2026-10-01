package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	pionmedia "github.com/pion/webrtc/v4/pkg/media"

	"github.com/pedango/PayVoice/internal/audio"
	"github.com/pedango/PayVoice/internal/config"
	"github.com/pedango/PayVoice/internal/media"
	"github.com/pedango/PayVoice/internal/rtcsvc"
	"github.com/pedango/PayVoice/internal/stt"
	"github.com/pedango/PayVoice/internal/tts"
	"github.com/pedango/PayVoice/internal/webhook"
)

const testToken = "test-token-that-is-at-least-32-chars-long"

type harness struct {
	t      *testing.T
	server *httptest.Server
	engine *media.Engine
	cancel context.CancelFunc
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Load()
	cfg.HTTP.Tokens = []string{testToken}
	cfg.Media.MaxLegsPerRoom = 8
	cfg.Media.RoomIdleTimeout = time.Hour
	cfg.Media.LegMaxDuration = time.Hour
	cfg.Media.JitterTarget = 20 * time.Millisecond
	cfg.Media.JitterMax = 200 * time.Millisecond
	cfg.TTS.Provider = "tone"
	// No ICE servers keeps the handshake on loopback host candidates, so the
	// test neither needs the network nor waits on STUN.
	cfg.WebRTC.ICEServers = nil
	cfg.WebRTC.UDPPortMin = 0
	cfg.WebRTC.UDPPortMax = 0

	hooks := webhook.New(cfg.Webhook, log)
	engine := media.NewEngine(cfg.Media, hooks.Sink())

	rtc, err := rtcsvc.New(rtcsvc.Options{
		Config: cfg.WebRTC,
		Media:  cfg.Media,
		Engine: engine,
		Emit:   hooks.Sink(),
		Log:    log,
	})
	if err != nil {
		t.Fatalf("webrtc service: %v", err)
	}

	s := New(Options{
		Config: cfg,
		Engine: engine,
		WebRTC: rtc,
		TTS:    tts.NewEngine(cfg.TTS, log),
		STT:    stt.NewEngine(cfg.STT, log),
		Hooks:  hooks,
		Log:    log,
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = engine.Run(ctx) }()

	h := &harness{
		t:      t,
		server: httptest.NewServer(s.http.Handler),
		engine: engine,
		cancel: cancel,
	}
	t.Cleanup(func() {
		h.server.Close()
		cancel()
	})
	return h
}

func (h *harness) do(method, path string, body any, auth bool) (*http.Response, map[string]any) {
	h.t.Helper()

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("marshal request: %v", err)
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequest(method, h.server.URL+path, reader)
	if err != nil {
		h.t.Fatalf("build request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth {
		req.Header.Set("Authorization", "Bearer "+testToken)
	}

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()

	out := map[string]any{}
	raw, _ := io.ReadAll(res.Body)
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return res, out
}

func TestAuthRequiredOnControlPlane(t *testing.T) {
	h := newHarness(t)

	res, _ := h.do(http.MethodGet, "/v1/stats", nil, false)
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated status = %d, want 401", res.StatusCode)
	}
	if res.Header.Get("WWW-Authenticate") == "" {
		t.Error("401 response is missing a WWW-Authenticate challenge")
	}

	res, _ = h.do(http.MethodGet, "/v1/stats", nil, true)
	if res.StatusCode != http.StatusOK {
		t.Errorf("authenticated status = %d, want 200", res.StatusCode)
	}
}

func TestWrongTokenIsRejected(t *testing.T) {
	h := newHarness(t)

	req, _ := http.NewRequest(http.MethodGet, h.server.URL+"/v1/stats", nil)
	req.Header.Set("Authorization", "Bearer not-the-right-token-but-same-length!!")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", res.StatusCode)
	}
}

// TestHealthIsUnauthenticated matters operationally: a load balancer probe
// cannot carry a bearer token.
func TestHealthIsUnauthenticated(t *testing.T) {
	h := newHarness(t)

	res, body := h.do(http.MethodGet, "/healthz", nil, false)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if body["status"] != "ok" {
		t.Errorf("body = %v, want status ok", body)
	}
}

func TestRoomLifecycle(t *testing.T) {
	h := newHarness(t)

	res, room := h.do(http.MethodPost, "/v1/rooms", map[string]any{"sample_rate": 16000}, true)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", res.StatusCode)
	}

	id, _ := room["id"].(string)
	if id == "" {
		t.Fatalf("no room id in %v", room)
	}
	// A caller may ask for 16 kHz, but the response must state the truth.
	if room["sample_rate"] != float64(audio.SampleRate) {
		t.Errorf("sample_rate = %v, want %d", room["sample_rate"], audio.SampleRate)
	}

	res, _ = h.do(http.MethodGet, "/v1/rooms/"+id, nil, true)
	if res.StatusCode != http.StatusOK {
		t.Errorf("get status = %d, want 200", res.StatusCode)
	}

	res, _ = h.do(http.MethodDelete, "/v1/rooms/"+id, nil, true)
	if res.StatusCode != http.StatusOK {
		t.Errorf("delete status = %d, want 200", res.StatusCode)
	}

	res, _ = h.do(http.MethodGet, "/v1/rooms/"+id, nil, true)
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("get after delete = %d, want 404", res.StatusCode)
	}
}

func TestRoomTTSQueuesPlayback(t *testing.T) {
	h := newHarness(t)

	_, room := h.do(http.MethodPost, "/v1/rooms", map[string]any{}, true)
	id := room["id"].(string)

	res, body := h.do(http.MethodPost, "/v1/rooms/"+id+"/tts", map[string]any{
		"text":     "Enter your four digit PIN",
		"provider": "tone",
	}, true)

	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("tts status = %d, want 202: %v", res.StatusCode, body)
	}
	if body["playback_id"] == nil {
		t.Error("response carries no playback_id")
	}
	if d, _ := body["duration_ms"].(float64); d <= 0 {
		t.Errorf("duration_ms = %v, want a positive duration", body["duration_ms"])
	}
}

func TestTTSRejectsEmptyText(t *testing.T) {
	h := newHarness(t)

	_, room := h.do(http.MethodPost, "/v1/rooms", map[string]any{}, true)
	id := room["id"].(string)

	res, _ := h.do(http.MethodPost, "/v1/rooms/"+id+"/tts", map[string]any{"text": "  "}, true)
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", res.StatusCode)
	}
}

func TestUnknownResourcesReturn404(t *testing.T) {
	h := newHarness(t)

	for _, path := range []string{"/v1/legs/leg_missing", "/v1/rooms/room_missing"} {
		res, _ := h.do(http.MethodGet, path, nil, true)
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, res.StatusCode)
		}
	}
}

func TestJoinRoomRejectsUnknownLeg(t *testing.T) {
	h := newHarness(t)

	_, room := h.do(http.MethodPost, "/v1/rooms", map[string]any{}, true)
	id := room["id"].(string)

	res, _ := h.do(http.MethodPost, "/v1/rooms/"+id+"/legs",
		map[string]any{"leg_id": "leg_nope"}, true)
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", res.StatusCode)
	}
}

func TestOriginateWithoutSIPIsUnavailable(t *testing.T) {
	h := newHarness(t)

	res, _ := h.do(http.MethodPost, "/v1/legs",
		map[string]any{"type": "sip", "to": "233245224253"}, true)
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 when SIP is disabled", res.StatusCode)
	}
}

// TestWebRTCOfferEstablishesMedia is the integration test for the browser half
// of the bridge. It runs a real peer connection against the service: offers
// PCMU, completes ICE and DTLS over loopback, sends audio, and checks that the
// samples reach the leg's receive path.
//
// If this passes, then codec negotiation, ICE, DTLS-SRTP, depacketization,
// G.711 decoding and the jitter buffer are all working together.
func TestWebRTCOfferEstablishesMedia(t *testing.T) {
	h := newHarness(t)

	// Build a "browser" that speaks only PCMU, matching what the real client
	// is configured to offer.
	me := &webrtc.MediaEngine{}
	if err := me.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypePCMU,
			ClockRate: audio.ClockRate,
			Channels:  1,
		},
		PayloadType: 0,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		t.Fatalf("register codec: %v", err)
	}

	api := webrtc.NewAPI(webrtc.WithMediaEngine(me))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("browser peer connection: %v", err)
	}
	defer pc.Close()

	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypePCMU,
		ClockRate: audio.ClockRate,
		Channels:  1,
	}, "audio", "browser")
	if err != nil {
		t.Fatalf("browser track: %v", err)
	}
	if _, err := pc.AddTrack(track); err != nil {
		t.Fatalf("add track: %v", err)
	}

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("create offer: %v", err)
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("set local description: %v", err)
	}
	<-gathered

	res, body := h.do(http.MethodPost, "/v1/webrtc/offer",
		map[string]any{"sdp": pc.LocalDescription().SDP, "from": "browser-test"}, true)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("offer status = %d, want 201: %v", res.StatusCode, body)
	}

	legID, _ := body["leg_id"].(string)
	answerSDP, _ := body["sdp"].(string)
	if legID == "" || answerSDP == "" {
		t.Fatalf("incomplete answer: %v", body)
	}

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  answerSDP,
	}); err != nil {
		t.Fatalf("set remote description: %v", err)
	}

	connected := make(chan struct{})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateConnected {
			close(connected)
		}
	})

	select {
	case <-connected:
	case <-time.After(15 * time.Second):
		t.Fatal("peer connection never reached connected state")
	}

	leg := h.engine.Leg(legID)
	if leg == nil {
		t.Fatalf("leg %s is not registered with the engine", legID)
	}

	// Send a second of audible tone as G.711.
	payload := audio.CodecPCMU.Encode(nil, toneFrame())
	deadline := time.After(10 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("no audio reached the leg: %+v", leg.JitterStats())
		default:
		}

		if err := track.WriteSample(pionmedia.Sample{
			Data:     payload,
			Duration: audio.FrameDurationMs * time.Millisecond,
		}); err != nil {
			t.Fatalf("write sample: %v", err)
		}
		time.Sleep(audio.FrameDurationMs * time.Millisecond)

		if leg.JitterStats().Received > 10 {
			break
		}
	}

	if leg.Kind != "webrtc" {
		t.Errorf("leg kind = %q, want webrtc", leg.Kind)
	}

	res, _ = h.do(http.MethodDelete, "/v1/legs/"+legID, nil, true)
	if res.StatusCode != http.StatusOK {
		t.Errorf("hangup status = %d, want 200", res.StatusCode)
	}
}

// TestWebRTCOfferRejectsOpusOnly covers a browser configured without G.711.
func TestWebRTCOfferRejectsOpusOnly(t *testing.T) {
	h := newHarness(t)

	me := &webrtc.MediaEngine{}
	if err := me.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeOpus,
			ClockRate: 48000,
			Channels:  2,
		},
		PayloadType: 111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		t.Fatalf("register opus: %v", err)
	}

	api := webrtc.NewAPI(webrtc.WithMediaEngine(me))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("peer connection: %v", err)
	}
	defer pc.Close()

	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypeOpus,
		ClockRate: 48000,
		Channels:  2,
	}, "audio", "opus-browser")
	if err != nil {
		t.Fatalf("track: %v", err)
	}
	if _, err := pc.AddTrack(track); err != nil {
		t.Fatalf("add track: %v", err)
	}

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("create offer: %v", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("set local description: %v", err)
	}

	res, _ := h.do(http.MethodPost, "/v1/webrtc/offer",
		map[string]any{"sdp": offer.SDP}, true)

	if res.StatusCode < 400 {
		t.Errorf("status = %d, want a client error for an Opus-only offer", res.StatusCode)
	}
}

func toneFrame() []int16 {
	f := make([]int16, audio.FrameSamples)
	for i := range f {
		f[i] = int16(6000 * (float64((i%40))/20 - 1))
	}
	return f
}
