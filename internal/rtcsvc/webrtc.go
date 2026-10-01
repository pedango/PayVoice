// Package rtcsvc implements the browser-facing half of the bridge: WebRTC peer
// connections that carry the same G.711 payload as the SIP side.
package rtcsvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"
	pionmedia "github.com/pion/webrtc/v4/pkg/media"

	"github.com/pedango/PayVoice/internal/audio"
	"github.com/pedango/PayVoice/internal/config"
	"github.com/pedango/PayVoice/internal/media"
	"github.com/pedango/PayVoice/internal/rtpx"
)

// dtmfPayloadType is the telephone-event number PayVoice offers to browsers.
// It is fixed because browsers do not negotiate it dynamically in practice.
const dtmfPayloadType = 101

// ErrNoCommonCodec means the browser refused to speak G.711.
var ErrNoCommonCodec = errors.New("rtcsvc: browser offered no PCMU/PCMA audio")

// Service creates and tracks WebRTC legs.
type Service struct {
	api      *webrtc.API
	cfg      config.WebRTCConfig
	mediaCfg config.MediaConfig
	codec    audio.Codec
	engine   *media.Engine
	emit     media.EventSink
	log      *slog.Logger

	mu    sync.RWMutex
	peers map[string]*transport
}

// Options configures the WebRTC service.
type Options struct {
	Config config.WebRTCConfig
	Media  config.MediaConfig
	Engine *media.Engine
	Emit   media.EventSink
	Log    *slog.Logger
}

// New builds the service and its media engine.
//
// Only G.711 and telephone-event are registered. Restricting the media engine
// this way is what makes the bridge cheap: a browser leg and a phone leg end
// up carrying byte-identical payloads, so forwarding between them is a memory
// copy instead of an Opus decode and a G.711 encode. It also removes the
// libopus cgo dependency entirely. The cost is telephone-grade audio, which is
// the correct quality target for something bridged to the PSTN anyway.
func New(o Options) (*Service, error) {
	codec := audio.ParseCodec(o.Media.Codec)

	me := &webrtc.MediaEngine{}
	if err := me.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  mimeFor(codec),
			ClockRate: audio.ClockRate,
			Channels:  1,
		},
		PayloadType: webrtc.PayloadType(codec.PayloadType()),
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, fmt.Errorf("rtcsvc: register %s: %w", codec.Name(), err)
	}

	// Registering telephone-event lets a browser send DTMF through
	// RTCDTMFSender, which is the reliable way to collect a PIN: keypresses
	// cannot be misheard the way spoken digits can.
	if err := me.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  "audio/telephone-event",
			ClockRate: audio.ClockRate,
			Channels:  1,
		},
		PayloadType: dtmfPayloadType,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, fmt.Errorf("rtcsvc: register telephone-event: %w", err)
	}

	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(me, ir); err != nil {
		return nil, fmt.Errorf("rtcsvc: interceptors: %w", err)
	}

	se := webrtc.SettingEngine{}
	if o.Config.UDPPortMin > 0 && o.Config.UDPPortMax > o.Config.UDPPortMin {
		if err := se.SetEphemeralUDPPortRange(
			uint16(o.Config.UDPPortMin),
			uint16(o.Config.UDPPortMax),
		); err != nil {
			return nil, fmt.Errorf("rtcsvc: udp port range: %w", err)
		}
	}
	if o.Config.NAT1To1IP != "" {
		// On a cloud VM the host candidate is a private address the browser
		// can never reach, so it is rewritten to the public one.
		se.SetNAT1To1IPs([]string{o.Config.NAT1To1IP}, webrtc.ICECandidateTypeHost)
	}

	return &Service{
		api: webrtc.NewAPI(
			webrtc.WithMediaEngine(me),
			webrtc.WithInterceptorRegistry(ir),
			webrtc.WithSettingEngine(se),
		),
		cfg:      o.Config,
		mediaCfg: o.Media,
		codec:    codec,
		engine:   o.Engine,
		emit:     o.Emit,
		log:      o.Log,
		peers:    make(map[string]*transport),
	}, nil
}

func mimeFor(c audio.Codec) string {
	if c == audio.CodecPCMA {
		return webrtc.MimeTypePCMA
	}
	return webrtc.MimeTypePCMU
}

// OfferRequest describes an incoming browser offer.
type OfferRequest struct {
	SDP    string
	From   string
	To     string
	AppRef string
}

// Answer holds the SDP to return to the browser.
type Answer struct {
	LegID string
	SDP   string
}

// Offer accepts a browser's SDP offer, creates a leg, and returns the answer.
//
// The answer is returned with candidates already gathered rather than
// trickled, because the application's signalling channel is a single HTTP
// request/response and has nowhere to deliver later candidates. Gathering is
// capped so a slow or unreachable STUN server cannot stall the caller: host
// candidates alone are enough on a LAN or a VM with a public address.
func (s *Service) Offer(ctx context.Context, req OfferRequest) (*media.Leg, *Answer, error) {
	if strings.TrimSpace(req.SDP) == "" {
		return nil, nil, errors.New("rtcsvc: empty SDP offer")
	}

	pc, err := s.api.NewPeerConnection(webrtc.Configuration{
		ICEServers: s.iceServers(),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("rtcsvc: peer connection: %w", err)
	}

	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{
		MimeType:  mimeFor(s.codec),
		ClockRate: audio.ClockRate,
		Channels:  1,
	}, "audio", "payvoice")
	if err != nil {
		_ = pc.Close()
		return nil, nil, fmt.Errorf("rtcsvc: local track: %w", err)
	}
	sender, err := pc.AddTrack(track)
	if err != nil {
		_ = pc.Close()
		return nil, nil, fmt.Errorf("rtcsvc: add track: %w", err)
	}

	legID := "leg_" + uuid.NewString()
	tr := &transport{
		pc:     pc,
		track:  track,
		codec:  s.codec,
		encBuf: make([]byte, 0, audio.FrameSamples),
	}

	leg := media.NewLeg(tr, media.LegOptions{
		ID:           legID,
		Direction:    media.Inbound,
		From:         req.From,
		To:           req.To,
		AppRef:       req.AppRef,
		JitterTarget: s.mediaCfg.JitterTarget,
		JitterMax:    s.mediaCfg.JitterMax,
		VADThreshold: s.mediaCfg.VADThreshold,
		EndOfSpeech:  s.mediaCfg.EndOfSpeech,
		BargeIn:      true,
		Emit:         s.emit,
	})

	// RTCP from the browser must be drained or the sender's congestion state
	// goes stale and pion stops sending.
	go drainRTCP(sender)

	pc.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		s.readTrack(remote, leg)
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		s.log.Debug("webrtc state", "leg", legID, "state", state.String())
		switch state {
		case webrtc.PeerConnectionStateConnected:
			leg.SetState(media.LegStateAnswered)
		case webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateClosed,
			webrtc.PeerConnectionStateDisconnected:
			s.forget(legID)
			leg.Close("webrtc_" + state.String())
		}
	})

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  req.SDP,
	}); err != nil {
		_ = pc.Close()
		return nil, nil, fmt.Errorf("rtcsvc: remote description: %w", err)
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		_ = pc.Close()
		// pion reports a codec mismatch here, which in practice means the
		// browser was configured to offer Opus only.
		return nil, nil, fmt.Errorf("%w: %v", ErrNoCommonCodec, err)
	}

	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		_ = pc.Close()
		return nil, nil, fmt.Errorf("rtcsvc: local description: %w", err)
	}

	select {
	case <-gathered:
	case <-time.After(2 * time.Second):
		s.log.Warn("ice gathering timed out, answering with partial candidates", "leg", legID)
	case <-ctx.Done():
		_ = pc.Close()
		return nil, nil, ctx.Err()
	}

	local := pc.LocalDescription()
	if local == nil {
		_ = pc.Close()
		return nil, nil, errors.New("rtcsvc: no local description after gathering")
	}

	s.mu.Lock()
	s.peers[legID] = tr
	s.mu.Unlock()

	s.engine.AddLeg(leg)
	leg.SetState(media.LegStateRinging)

	return leg, &Answer{LegID: legID, SDP: local.SDP}, nil
}

// readTrack pumps a browser's audio and DTMF into the leg.
func (s *Service) readTrack(remote *webrtc.TrackRemote, leg *media.Leg) {
	codecName := strings.ToLower(remote.Codec().MimeType)
	isDTMF := strings.Contains(codecName, "telephone-event")

	dtmf := rtpx.NewDTMFCollector(40 * time.Millisecond)
	decode := make([]int16, 0, 480)

	for {
		pkt, _, err := remote.ReadRTP()
		if err != nil {
			return
		}
		if len(pkt.Payload) == 0 {
			continue
		}

		if isDTMF || uint8(pkt.PayloadType) == dtmfPayloadType {
			if digit, ok := dtmf.Push(pkt.Timestamp, pkt.Payload); ok {
				leg.PushDTMF(digit)
			}
			continue
		}

		decode = s.codec.Decode(decode, pkt.Payload)
		leg.PushAudio(pkt.Timestamp, decode)
	}
}

// drainRTCP reads and discards receiver reports.
func drainRTCP(sender *webrtc.RTPSender) {
	buf := make([]byte, 1500)
	for {
		if _, _, err := sender.Read(buf); err != nil {
			return
		}
	}
}

// AddICECandidate feeds a trickled candidate from the browser.
func (s *Service) AddICECandidate(legID string, candidate webrtc.ICECandidateInit) error {
	s.mu.RLock()
	tr, ok := s.peers[legID]
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("rtcsvc: no peer connection for leg %s", legID)
	}
	return tr.pc.AddICECandidate(candidate)
}

// ICEServers returns the configuration browsers should use.
func (s *Service) ICEServers() []config.ICEServer { return s.cfg.ICEServers }

func (s *Service) iceServers() []webrtc.ICEServer {
	out := make([]webrtc.ICEServer, 0, len(s.cfg.ICEServers))
	for _, srv := range s.cfg.ICEServers {
		entry := webrtc.ICEServer{URLs: srv.URLs}
		if srv.Username != "" {
			entry.Username = srv.Username
			entry.Credential = srv.Credential
			entry.CredentialType = webrtc.ICECredentialTypePassword
		}
		out = append(out, entry)
	}
	return out
}

func (s *Service) forget(legID string) {
	s.mu.Lock()
	delete(s.peers, legID)
	s.mu.Unlock()
}

// Hangup closes a browser leg.
func (s *Service) Hangup(legID string) error {
	s.mu.Lock()
	tr, ok := s.peers[legID]
	delete(s.peers, legID)
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("rtcsvc: no peer connection for leg %s", legID)
	}
	return tr.Close()
}

// Count reports live peer connections.
func (s *Service) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.peers)
}

// transport adapts a peer connection to the media package's Transport.
type transport struct {
	pc     *webrtc.PeerConnection
	track  *webrtc.TrackLocalStaticSample
	codec  audio.Codec
	encBuf []byte

	mu        sync.Mutex
	closeOnce sync.Once
}

func (t *transport) WriteFrame(f audio.Frame) error {
	t.mu.Lock()
	t.encBuf = t.codec.Encode(t.encBuf, f)
	payload := t.encBuf
	t.mu.Unlock()

	return t.track.WriteSample(pionmedia.Sample{
		Data:     payload,
		Duration: audio.FrameDurationMs * time.Millisecond,
	})
}

// SendDTMF is not supported toward a browser: a browser plays received
// telephone-event packets as nothing at all, so the agent speaks instead.
func (t *transport) SendDTMF(rune, time.Duration) error {
	return errors.New("rtcsvc: DTMF cannot be sent to a browser leg")
}

func (t *transport) Kind() string { return "webrtc" }

func (t *transport) RemoteAddr() string {
	stats := t.pc.GetStats()
	for _, entry := range stats {
		if pair, ok := entry.(webrtc.ICECandidatePairStats); ok && pair.State == "succeeded" {
			return pair.RemoteCandidateID
		}
	}
	return t.pc.ConnectionState().String()
}

func (t *transport) Close() error {
	var err error
	t.closeOnce.Do(func() {
		err = t.pc.Close()
	})
	return err
}
