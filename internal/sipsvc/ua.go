// Package sipsvc implements the SIP signalling plane: inbound call handling,
// outbound origination toward a carrier trunk, and optional registration.
package sipsvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/google/uuid"

	"github.com/pedango/PayVoice/internal/audio"
	"github.com/pedango/PayVoice/internal/config"
	"github.com/pedango/PayVoice/internal/media"
	"github.com/pedango/PayVoice/internal/rtpx"
)

// SIP response codes used by the service.
const (
	statusTrying            = 100
	statusRinging           = 180
	statusOK                = 200
	statusBadRequest        = 400
	statusBusy              = 486
	statusNotAcceptableHere = 488
	statusServerError       = 500
)

// Errors returned by the service.
var (
	// ErrLegNotFound means the leg id has no SIP dialog.
	ErrLegNotFound = errors.New("sipsvc: no SIP dialog for leg")
	// ErrNotRinging means answer was called on a leg that is not ringing.
	ErrNotRinging = errors.New("sipsvc: leg is not ringing")
	// ErrNoTrunk means origination was attempted without a configured trunk.
	ErrNoTrunk = errors.New("sipsvc: no SIP trunk configured")
)

// InboundHandler is called when a new call arrives, after PayVoice has sent
// 180 Ringing but before it answers. The application decides what happens
// next by calling Answer or Hangup.
type InboundHandler func(leg *media.Leg)

// Service is the SIP user agent.
type Service struct {
	cfg      config.SIPConfig
	rtpCfg   config.RTPConfig
	mediaCfg config.MediaConfig
	codec    audio.Codec

	ua  *sipgo.UserAgent
	srv *sipgo.Server
	cli *sipgo.Client
	ds  *sipgo.DialogServerCache
	dc  *sipgo.DialogClientCache

	ports  *rtpx.PortPool
	engine *media.Engine
	emit   media.EventSink
	log    *slog.Logger

	mu       sync.RWMutex
	inbound  map[string]*sipgo.DialogServerSession
	outbound map[string]*sipgo.DialogClientSession
	offers   map[string]*Offer

	onInbound InboundHandler
	contact   sip.ContactHeader

	registered  bool
	registerErr string
}

// Options configures the SIP service.
type Options struct {
	Config    config.SIPConfig
	RTP       config.RTPConfig
	Media     config.MediaConfig
	Engine    *media.Engine
	Ports     *rtpx.PortPool
	Emit      media.EventSink
	Log       *slog.Logger
	OnInbound InboundHandler
}

// New builds the SIP service without starting it.
func New(o Options) (*Service, error) {
	hostname := o.Config.PublicHost
	if hostname == "" {
		hostname = "127.0.0.1"
	}

	ua, err := sipgo.NewUA(
		sipgo.WithUserAgent("PayVoice"),
		sipgo.WithUserAgentHostname(hostname),
	)
	if err != nil {
		return nil, fmt.Errorf("sipsvc: user agent: %w", err)
	}

	srv, err := sipgo.NewServer(ua, sipgo.WithServerLogger(o.Log))
	if err != nil {
		return nil, fmt.Errorf("sipsvc: server: %w", err)
	}
	cli, err := sipgo.NewClient(ua, sipgo.WithClientLogger(o.Log), sipgo.WithClientHostname(hostname))
	if err != nil {
		return nil, fmt.Errorf("sipsvc: client: %w", err)
	}

	_, portStr, err := net.SplitHostPort(o.Config.Listen)
	if err != nil {
		return nil, fmt.Errorf("sipsvc: bad listen address %q: %w", o.Config.Listen, err)
	}
	port, _ := strconv.Atoi(portStr)

	contact := sip.ContactHeader{
		Address: sip.Uri{
			Scheme: "sip",
			User:   "payvoice",
			Host:   hostname,
			Port:   port,
		},
	}

	s := &Service{
		cfg:       o.Config,
		rtpCfg:    o.RTP,
		mediaCfg:  o.Media,
		codec:     audio.ParseCodec(o.Media.Codec),
		ua:        ua,
		srv:       srv,
		cli:       cli,
		ds:        sipgo.NewDialogServerCache(cli, contact),
		dc:        sipgo.NewDialogClientCache(cli, contact),
		ports:     o.Ports,
		engine:    o.Engine,
		emit:      o.Emit,
		log:       o.Log,
		inbound:   make(map[string]*sipgo.DialogServerSession),
		outbound:  make(map[string]*sipgo.DialogClientSession),
		offers:    make(map[string]*Offer),
		onInbound: o.OnInbound,
		contact:   contact,
	}

	srv.OnInvite(s.onInvite)
	srv.OnAck(s.onAck)
	srv.OnBye(s.onBye)
	srv.OnCancel(s.onCancel)
	srv.OnOptions(s.onOptions)

	return s, nil
}

// Run starts the SIP listener and, if configured, the registration loop.
func (s *Service) Run(ctx context.Context) error {
	if s.cfg.Register {
		go s.registerLoop(ctx)
	}
	s.log.Info("sip listening",
		"addr", s.cfg.Listen,
		"transport", s.cfg.Transport,
		"advertised_host", s.cfg.PublicHost,
		"codec", s.codec.Name(),
	)
	return s.srv.ListenAndServe(ctx, s.cfg.Transport, s.cfg.Listen)
}

// Close releases signalling resources.
func (s *Service) Close() error {
	_ = s.srv.Close()
	_ = s.cli.Close()
	return s.ua.Close()
}

// ---------------------------------------------------------------- inbound

func (s *Service) onInvite(req *sip.Request, tx sip.ServerTransaction) {
	offer, err := ParseOffer(req.Body())
	if err != nil {
		s.log.Warn("rejecting invite", "error", err, "call_id", callID(req))
		code := statusNotAcceptableHere
		if errors.Is(err, ErrNoAudio) {
			code = statusBadRequest
		}
		s.respond(req, tx, code, "Not Acceptable Here")
		return
	}

	sess, err := rtpx.New(s.ports, rtpx.Options{
		Codec:     offer.Codec,
		DTMFPT:    s.dtmfPT(offer),
		Symmetric: s.rtpCfg.SymmetricLatch,
	})
	if err != nil {
		s.log.Error("no rtp port available", "error", err)
		s.respond(req, tx, statusServerError, "Out Of Media Resources")
		return
	}

	remote, err := offer.RemoteAddr()
	if err == nil {
		sess.SetRemote(remote)
	}

	dlg, err := s.ds.ReadInvite(req, tx)
	if err != nil {
		_ = sess.Close()
		s.log.Error("dialog setup failed", "error", err)
		s.respond(req, tx, statusServerError, "Server Internal Error")
		return
	}

	legID := "leg_" + uuid.NewString()
	leg := media.NewLeg(&transport{sess: sess}, media.LegOptions{
		ID:           legID,
		Direction:    media.Inbound,
		From:         uriUser(req.From().Address),
		To:           uriUser(req.To().Address),
		JitterTarget: s.mediaCfg.JitterTarget,
		JitterMax:    s.mediaCfg.JitterMax,
		VADThreshold: s.mediaCfg.VADThreshold,
		EndOfSpeech:  s.mediaCfg.EndOfSpeech,
		BargeIn:      true,
		Emit:         s.emit,
	})
	sess.SetCallbacks(leg.PushAudio, leg.PushDTMF)
	sess.Start()

	s.mu.Lock()
	s.inbound[legID] = dlg
	s.offers[legID] = offer
	s.mu.Unlock()

	s.engine.AddLeg(leg)
	leg.SetState(media.LegStateRinging)

	// Provisional responses keep the carrier's timers happy while the
	// application decides whether to take the call.
	if err := dlg.Respond(statusTrying, "Trying", nil); err != nil {
		s.log.Debug("100 trying failed", "error", err)
	}
	if err := dlg.Respond(statusRinging, "Ringing", nil); err != nil {
		s.log.Debug("180 ringing failed", "error", err)
	}

	// Tear the leg down when the dialog ends for any reason, including the
	// carrier simply vanishing.
	go s.watchServerDialog(dlg, leg)

	if s.onInbound != nil {
		s.onInbound(leg)
	}
}

func (s *Service) watchServerDialog(dlg *sipgo.DialogServerSession, leg *media.Leg) {
	<-dlg.Context().Done()
	s.forget(leg.ID)
	leg.Close("dialog_terminated")
}

// Answer sends 200 OK with an SDP answer, putting the call into media.
func (s *Service) Answer(legID string) error {
	s.mu.RLock()
	dlg, ok := s.inbound[legID]
	offer := s.offers[legID]
	s.mu.RUnlock()
	if !ok {
		return ErrLegNotFound
	}

	leg := s.engine.Leg(legID)
	if leg == nil {
		return ErrLegNotFound
	}
	if leg.State() != media.LegStateRinging {
		return ErrNotRinging
	}

	tr, ok := leg.Transport().(*transport)
	if !ok {
		return errors.New("sipsvc: leg is not a SIP leg")
	}

	answer := BuildSDP(AnswerParams{
		Host:            s.cfg.PublicHost,
		Port:            tr.sess.LocalPort(),
		Codec:           offer.Codec,
		DTMFPayloadType: s.dtmfPT(offer),
	})
	if err := dlg.RespondSDP(answer); err != nil {
		return fmt.Errorf("sipsvc: 200 OK failed: %w", err)
	}

	leg.SetState(media.LegStateAnswered)
	return nil
}

// Hangup terminates a leg's dialog. Ringing inbound calls are rejected with a
// status code; established calls get a BYE.
func (s *Service) Hangup(ctx context.Context, legID string, code int, reason string) error {
	s.mu.RLock()
	in, isIn := s.inbound[legID]
	out, isOut := s.outbound[legID]
	s.mu.RUnlock()

	leg := s.engine.Leg(legID)

	switch {
	case isIn && leg != nil && leg.State() == media.LegStateRinging:
		if code == 0 {
			code = statusBusy
		}
		if reason == "" {
			reason = "Busy Here"
		}
		if err := in.Respond(code, reason, nil); err != nil {
			return err
		}
	case isIn:
		if err := in.Bye(ctx); err != nil {
			return err
		}
	case isOut:
		if err := out.Bye(ctx); err != nil {
			return err
		}
	default:
		return ErrLegNotFound
	}

	s.forget(legID)
	if leg != nil {
		leg.Close("local_hangup")
	}
	return nil
}

func (s *Service) onAck(req *sip.Request, tx sip.ServerTransaction) {
	if err := s.ds.ReadAck(req, tx); err != nil {
		s.log.Debug("ack not matched to a dialog", "error", err, "call_id", callID(req))
	}
}

func (s *Service) onBye(req *sip.Request, tx sip.ServerTransaction) {
	if dlg, err := s.ds.MatchDialogRequest(req); err == nil {
		if leg := s.legForDialog(dlg); leg != nil {
			s.forget(leg.ID)
			leg.Close("remote_bye")
		}
		if err := s.ds.ReadBye(req, tx); err != nil {
			s.log.Debug("bye handling failed", "error", err)
		}
		return
	}

	// The BYE may belong to a call PayVoice placed.
	if dlg, err := s.dc.MatchRequestDialog(req); err == nil {
		if leg := s.legForClientDialog(dlg); leg != nil {
			s.forget(leg.ID)
			leg.Close("remote_bye")
		}
	}
	if err := s.dc.ReadBye(req, tx); err != nil {
		s.log.Debug("client bye handling failed", "error", err)
		s.respond(req, tx, statusOK, "OK")
	}
}

func (s *Service) onCancel(req *sip.Request, tx sip.ServerTransaction) {
	if dlg, err := s.ds.MatchDialogRequest(req); err == nil {
		if leg := s.legForDialog(dlg); leg != nil {
			s.forget(leg.ID)
			leg.Close("caller_cancelled")
		}
	}
	s.respond(req, tx, statusOK, "OK")
}

// onOptions answers the keepalive probes carriers use to decide whether an
// endpoint is alive. Not answering these gets a trunk marked down.
func (s *Service) onOptions(req *sip.Request, tx sip.ServerTransaction) {
	res := sip.NewResponseFromRequest(req, statusOK, "OK", nil)
	res.AppendHeader(sip.NewHeader("Allow", "INVITE, ACK, BYE, CANCEL, OPTIONS, INFO"))
	if err := tx.Respond(res); err != nil {
		s.log.Debug("options response failed", "error", err)
	}
}

// ---------------------------------------------------------------- outbound

// Originate places a call toward the configured trunk and returns immediately
// with a ringing leg. The leg transitions to answered when the far end picks
// up, which the application observes through leg.answered events.
func (s *Service) Originate(ctx context.Context, to, from, appRef string) (*media.Leg, error) {
	if s.cfg.Trunk == "" {
		return nil, ErrNoTrunk
	}
	if from == "" {
		from = s.cfg.Identity
	}

	recipient, err := parseTrunkURI(s.cfg.Trunk, to, s.cfg.Domain)
	if err != nil {
		return nil, err
	}

	// The media port must exist before the offer can be written.
	sess, err := rtpx.New(s.ports, rtpx.Options{
		Codec:     s.codec,
		DTMFPT:    s.rtpCfg.DTMFPayloadType,
		Symmetric: s.rtpCfg.SymmetricLatch,
	})
	if err != nil {
		return nil, err
	}

	legID := "leg_" + uuid.NewString()
	leg := media.NewLeg(&transport{sess: sess}, media.LegOptions{
		ID:           legID,
		Direction:    media.Outbound,
		From:         from,
		To:           to,
		AppRef:       appRef,
		JitterTarget: s.mediaCfg.JitterTarget,
		JitterMax:    s.mediaCfg.JitterMax,
		VADThreshold: s.mediaCfg.VADThreshold,
		EndOfSpeech:  s.mediaCfg.EndOfSpeech,
		BargeIn:      true,
		Emit:         s.emit,
	})
	sess.SetCallbacks(leg.PushAudio, leg.PushDTMF)
	sess.Start()

	body := BuildSDP(AnswerParams{
		Host:            s.cfg.PublicHost,
		Port:            sess.LocalPort(),
		Codec:           s.codec,
		DTMFPayloadType: s.rtpCfg.DTMFPayloadType,
	})

	fromHDR := &sip.FromHeader{
		DisplayName: "PayVoice",
		Address:     sip.Uri{Scheme: "sip", User: from, Host: s.cfg.Domain},
		Params:      sip.NewParams(),
	}

	dlg, err := s.dc.Invite(ctx, recipient, body,
		fromHDR,
		sip.NewHeader("Content-Type", "application/sdp"),
	)
	if err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("sipsvc: invite failed: %w", err)
	}

	s.mu.Lock()
	s.outbound[legID] = dlg
	s.mu.Unlock()

	s.engine.AddLeg(leg)
	leg.SetState(media.LegStateRinging)

	go s.awaitAnswer(dlg, leg, sess)
	return leg, nil
}

// awaitAnswer blocks on the far end's final response and wires up media on
// success. It runs detached so Originate can return a ringing leg immediately,
// which is what lets the application play ringback or cancel.
func (s *Service) awaitAnswer(dlg *sipgo.DialogClientSession, leg *media.Leg, sess *rtpx.Session) {
	// A call that is never answered must not hold its port forever.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	err := dlg.WaitAnswer(ctx, sipgo.AnswerOptions{
		Username: s.cfg.Username,
		Password: s.cfg.Password,
		OnResponse: func(res *sip.Response) error {
			if res.StatusCode == statusRinging {
				leg.Emit("leg.ringing", map[string]any{"to": leg.To, "from": leg.From})
			}
			return nil
		},
	})
	if err != nil {
		s.forget(leg.ID)
		leg.Emit("leg.failed", map[string]any{"reason": err.Error()})
		leg.Close("no_answer")
		return
	}

	// The 200 OK carries the answer SDP, which tells us where to send media.
	answer, perr := ParseOffer(dlg.InviteResponse.Body())
	if perr != nil {
		s.log.Error("unparseable answer sdp", "error", perr, "leg", leg.ID)
		_ = dlg.Bye(ctx)
		leg.Close("bad_answer_sdp")
		return
	}
	if remote, rerr := answer.RemoteAddr(); rerr == nil {
		sess.SetRemote(remote)
	}

	if err := dlg.Ack(ctx); err != nil {
		s.log.Error("ack failed", "error", err, "leg", leg.ID)
		leg.Close("ack_failed")
		return
	}

	leg.SetState(media.LegStateAnswered)

	go func() {
		<-dlg.Context().Done()
		s.forget(leg.ID)
		leg.Close("dialog_terminated")
	}()
}

// ---------------------------------------------------------------- registration

// registerLoop keeps the trunk registration fresh. Re-registration happens at
// half the granted expiry, which is the usual safety margin against clock skew
// and packet loss.
func (s *Service) registerLoop(ctx context.Context) {
	backoff := 5 * time.Second

	for {
		expiry, err := s.register(ctx)
		if err != nil {
			s.mu.Lock()
			s.registered = false
			s.registerErr = err.Error()
			s.mu.Unlock()

			s.log.Error("sip registration failed", "error", err, "retry_in", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 2*time.Minute {
				backoff *= 2
			}
			continue
		}

		s.mu.Lock()
		s.registered = true
		s.registerErr = ""
		s.mu.Unlock()

		backoff = 5 * time.Second
		refresh := expiry / 2
		if refresh < 30*time.Second {
			refresh = 30 * time.Second
		}
		s.log.Info("sip registered", "expires_in", expiry, "refresh_in", refresh)

		select {
		case <-ctx.Done():
			return
		case <-time.After(refresh):
		}
	}
}

func (s *Service) register(ctx context.Context) (time.Duration, error) {
	var recipient sip.Uri
	if err := sip.ParseUri(s.cfg.RegistrarURI, &recipient); err != nil {
		return 0, fmt.Errorf("bad registrar uri %q: %w", s.cfg.RegistrarURI, err)
	}

	expiry := int(s.cfg.RegisterExpiry.Seconds())
	req := sip.NewRequest(sip.REGISTER, recipient)
	addr := sip.Uri{Scheme: "sip", User: s.cfg.Username, Host: s.cfg.Domain}
	req.AppendHeader(&sip.FromHeader{Address: addr, Params: sip.NewParams()})
	req.AppendHeader(&sip.ToHeader{Address: addr, Params: sip.NewParams()})
	req.AppendHeader(&s.contact)
	req.AppendHeader(sip.NewHeader("Expires", strconv.Itoa(expiry)))

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	res, err := s.cli.Do(ctx, req)
	if err != nil {
		return 0, err
	}

	// A registrar almost always challenges the first attempt.
	if res.StatusCode == 401 || res.StatusCode == 407 {
		res, err = s.cli.DoDigestAuth(ctx, req, res, sipgo.DigestAuth{
			Username: s.cfg.Username,
			Password: s.cfg.Password,
		})
		if err != nil {
			return 0, err
		}
	}
	if res.StatusCode != statusOK {
		return 0, fmt.Errorf("registrar replied %d %s", res.StatusCode, res.Reason)
	}

	// Honour the expiry the registrar granted, which is often shorter than
	// the one requested.
	if h := res.GetHeader("Expires"); h != nil {
		if v, err := strconv.Atoi(strings.TrimSpace(h.Value())); err == nil && v > 0 {
			expiry = v
		}
	}
	return time.Duration(expiry) * time.Second, nil
}

// Status reports signalling health for the health endpoint.
type Status struct {
	Enabled        bool   `json:"enabled"`
	Listen         string `json:"listen,omitempty"`
	Transport      string `json:"transport,omitempty"`
	AdvertisedHost string `json:"advertised_host,omitempty"`
	Trunk          string `json:"trunk,omitempty"`
	Registered     bool   `json:"registered"`
	RegisterError  string `json:"register_error,omitempty"`
	Dialogs        int    `json:"dialogs"`
	PortsInUse     int    `json:"rtp_ports_in_use"`
	PortCapacity   int    `json:"rtp_port_capacity"`
}

// Status returns current SIP state.
func (s *Service) Status() Status {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return Status{
		Enabled:        true,
		Listen:         s.cfg.Listen,
		Transport:      s.cfg.Transport,
		AdvertisedHost: s.cfg.PublicHost,
		Trunk:          s.cfg.Trunk,
		Registered:     s.registered || !s.cfg.Register,
		RegisterError:  s.registerErr,
		Dialogs:        len(s.inbound) + len(s.outbound),
		PortsInUse:     s.ports.InUse(),
		PortCapacity:   s.ports.Capacity(),
	}
}

// ---------------------------------------------------------------- helpers

func (s *Service) dtmfPT(o *Offer) uint8 {
	// Echoing the peer's telephone-event number avoids asymmetric numbering,
	// which some gateways mishandle.
	if o != nil && o.DTMFPayloadType != 0 {
		return o.DTMFPayloadType
	}
	return s.rtpCfg.DTMFPayloadType
}

func (s *Service) forget(legID string) {
	s.mu.Lock()
	delete(s.inbound, legID)
	delete(s.outbound, legID)
	delete(s.offers, legID)
	s.mu.Unlock()
}

func (s *Service) legForDialog(dlg *sipgo.DialogServerSession) *media.Leg {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for id, cur := range s.inbound {
		if cur == dlg {
			return s.engine.Leg(id)
		}
	}
	return nil
}

func (s *Service) legForClientDialog(dlg *sipgo.DialogClientSession) *media.Leg {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for id, cur := range s.outbound {
		if cur == dlg {
			return s.engine.Leg(id)
		}
	}
	return nil
}

func (s *Service) respond(req *sip.Request, tx sip.ServerTransaction, code int, reason string) {
	if err := tx.Respond(sip.NewResponseFromRequest(req, code, reason, nil)); err != nil {
		s.log.Debug("response failed", "code", code, "error", err)
	}
}

// parseTrunkURI builds the request URI for an outbound call by placing the
// dialled number in the user part of the trunk address.
func parseTrunkURI(trunk, user, domain string) (sip.Uri, error) {
	var uri sip.Uri
	if err := sip.ParseUri(trunk, &uri); err != nil {
		return sip.Uri{}, fmt.Errorf("sipsvc: bad trunk uri %q: %w", trunk, err)
	}
	if user != "" {
		uri.User = user
	}
	if uri.Host == "" {
		uri.Host = domain
	}
	return uri, nil
}

func uriUser(u sip.Uri) string {
	if u.User != "" {
		return u.User
	}
	return u.Host
}

func callID(req *sip.Request) string {
	if h := req.CallID(); h != nil {
		return h.Value()
	}
	return ""
}

// transport adapts an RTP session to the media package's Transport interface.
type transport struct {
	sess *rtpx.Session
}

func (t *transport) WriteFrame(f audio.Frame) error { return t.sess.WriteFrame(f) }

func (t *transport) SendDTMF(d rune, dur time.Duration) error { return t.sess.SendDTMF(d, dur) }

func (t *transport) Kind() string { return "sip" }

func (t *transport) RemoteAddr() string { return t.sess.RemoteAddr() }

func (t *transport) Close() error { return t.sess.Close() }
