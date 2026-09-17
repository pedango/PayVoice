// Package config loads Pedango's runtime configuration from the environment.
//
// Every value has a development-safe default so that `pedango` runs with no
// configuration at all, but production deployments must set at least
// PEDANGO_API_TOKENS and PEDANGO_WEBHOOK_SECRET; Validate rejects the empty
// values when PEDANGO_ENV is "production".
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the fully resolved runtime configuration.
type Config struct {
	Env      string
	LogLevel string
	LogJSON  bool

	HTTP    HTTPConfig
	SIP     SIPConfig
	RTP     RTPConfig
	WebRTC  WebRTCConfig
	Media   MediaConfig
	Webhook WebhookConfig
	TTS     TTSConfig
	STT     STTConfig
}

// HTTPConfig controls the control-plane REST listener.
type HTTPConfig struct {
	Addr string
	// Tokens accepted in `Authorization: Bearer <token>`. Empty disables token
	// auth, which Validate only tolerates outside production.
	Tokens []string
	// AllowedIPs, when non-empty, restricts the control plane to these peers.
	AllowedIPs  []string
	ReadTimeout time.Duration
	// TrustProxy makes the server honour X-Forwarded-For for peer checks.
	TrustProxy bool
}

// SIPConfig controls the SIP user agent and optional registration to a trunk.
type SIPConfig struct {
	Enabled   bool
	Listen    string
	Transport string
	// PublicHost is the address placed in Contact/Via and SDP when Pedango sits
	// behind NAT. Defaults to the listen host.
	PublicHost string
	// Domain is the SIP domain used in From/To URIs.
	Domain string
	// Trunk is where outbound INVITEs are sent, e.g. "sip:carrier.example:5060".
	Trunk string
	// Identity is the number Pedango presents as caller ID when originating.
	Identity string

	Register       bool
	RegistrarURI   string
	Username       string
	Password       string
	RegisterExpiry time.Duration
}

// RTPConfig controls the media socket pool used by SIP legs.
type RTPConfig struct {
	Host    string
	PortMin int
	PortMax int
	// SymmetricLatch makes Pedango send to the source address of the first
	// received packet rather than the SDP-advertised address. Required for most
	// carriers behind NAT.
	SymmetricLatch bool
	// DTMFPayloadType is the RFC 4733 telephone-event dynamic payload type.
	DTMFPayloadType uint8
}

// WebRTCConfig controls browser-facing peer connections.
type WebRTCConfig struct {
	ICEServers []ICEServer
	UDPPortMin int
	UDPPortMax int
	// NAT1To1IP publishes a fixed host candidate, for cloud VMs with a static
	// public address.
	NAT1To1IP string
}

// ICEServer is a STUN or TURN server offered to browser clients.
type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// MediaConfig controls the mixer and jitter buffers.
type MediaConfig struct {
	// Codec is the G.711 variant used on the wire: "pcmu" or "pcma".
	Codec string
	// JitterTarget is the nominal playout delay held by each receive buffer.
	JitterTarget time.Duration
	JitterMax    time.Duration
	// MaxLegsPerRoom bounds mixer cost.
	MaxLegsPerRoom int
	// RoomIdleTimeout reaps rooms that hold no legs.
	RoomIdleTimeout time.Duration
	// LegMaxDuration hard-caps any single call.
	LegMaxDuration time.Duration
	// VADThreshold is the RMS above which a frame counts as speech.
	VADThreshold float64
	// EndOfSpeech is the trailing silence that closes an utterance.
	EndOfSpeech time.Duration
}

// WebhookConfig controls signed event delivery to the application (PayPlus).
type WebhookConfig struct {
	URL     string
	Secret  string
	Timeout time.Duration
	Retries int
	// Tolerance is how old a signed timestamp may be before the receiver should
	// reject it. Published in docs; enforced by the receiver, not Pedango.
	Tolerance time.Duration
}

// TTSConfig selects and configures the speech synthesis provider.
type TTSConfig struct {
	Provider    string
	APIKey      string
	VoiceID     string
	Model       string
	BaseURL     string
	Timeout     time.Duration
	CacheSize   int
	CacheTTL    time.Duration
	MaxTextRune int
}

// STTConfig selects and configures the speech recognition provider.
type STTConfig struct {
	Provider string
	APIKey   string
	Model    string
	BaseURL  string
	Language string
	// Endpointing is the provider-side silence threshold in milliseconds.
	Endpointing int
}

// Load reads configuration from the process environment.
func Load() *Config {
	c := &Config{
		Env:      env("PEDANGO_ENV", "development"),
		LogLevel: env("PEDANGO_LOG_LEVEL", "info"),
		LogJSON:  boolEnv("PEDANGO_LOG_JSON", false),

		HTTP: HTTPConfig{
			Addr:        env("PEDANGO_HTTP_ADDR", "127.0.0.1:8080"),
			Tokens:      listEnv("PEDANGO_API_TOKENS", nil),
			AllowedIPs:  listEnv("PEDANGO_API_ALLOWED_IPS", nil),
			ReadTimeout: durEnv("PEDANGO_HTTP_READ_TIMEOUT", 10*time.Second),
			TrustProxy:  boolEnv("PEDANGO_HTTP_TRUST_PROXY", false),
		},

		SIP: SIPConfig{
			Enabled:        boolEnv("PEDANGO_SIP_ENABLED", false),
			Listen:         env("PEDANGO_SIP_LISTEN", "0.0.0.0:5060"),
			Transport:      strings.ToLower(env("PEDANGO_SIP_TRANSPORT", "udp")),
			PublicHost:     env("PEDANGO_SIP_PUBLIC_HOST", ""),
			Domain:         env("PEDANGO_SIP_DOMAIN", ""),
			Trunk:          env("PEDANGO_SIP_TRUNK", ""),
			Identity:       env("PEDANGO_SIP_IDENTITY", ""),
			Register:       boolEnv("PEDANGO_SIP_REGISTER", false),
			RegistrarURI:   env("PEDANGO_SIP_REGISTRAR", ""),
			Username:       env("PEDANGO_SIP_USERNAME", ""),
			Password:       env("PEDANGO_SIP_PASSWORD", ""),
			RegisterExpiry: durEnv("PEDANGO_SIP_REGISTER_EXPIRY", 300*time.Second),
		},

		RTP: RTPConfig{
			Host:            env("PEDANGO_RTP_HOST", "0.0.0.0"),
			PortMin:         intEnv("PEDANGO_RTP_PORT_MIN", 20000),
			PortMax:         intEnv("PEDANGO_RTP_PORT_MAX", 20400),
			SymmetricLatch:  boolEnv("PEDANGO_RTP_SYMMETRIC", true),
			DTMFPayloadType: uint8(intEnv("PEDANGO_RTP_DTMF_PT", 101)),
		},

		WebRTC: WebRTCConfig{
			ICEServers: parseICEServers(
				env("PEDANGO_ICE_URLS", "stun:stun.l.google.com:19302"),
				env("PEDANGO_TURN_USERNAME", ""),
				env("PEDANGO_TURN_CREDENTIAL", ""),
			),
			UDPPortMin: intEnv("PEDANGO_WEBRTC_PORT_MIN", 40000),
			UDPPortMax: intEnv("PEDANGO_WEBRTC_PORT_MAX", 40400),
			NAT1To1IP:  env("PEDANGO_WEBRTC_PUBLIC_IP", ""),
		},

		Media: MediaConfig{
			Codec:           strings.ToLower(env("PEDANGO_CODEC", "pcmu")),
			JitterTarget:    durEnv("PEDANGO_JITTER_TARGET", 60*time.Millisecond),
			JitterMax:       durEnv("PEDANGO_JITTER_MAX", 240*time.Millisecond),
			MaxLegsPerRoom:  intEnv("PEDANGO_MAX_LEGS_PER_ROOM", 8),
			RoomIdleTimeout: durEnv("PEDANGO_ROOM_IDLE_TIMEOUT", 5*time.Minute),
			LegMaxDuration:  durEnv("PEDANGO_LEG_MAX_DURATION", 30*time.Minute),
			VADThreshold:    floatEnv("PEDANGO_VAD_THRESHOLD", 700),
			EndOfSpeech:     durEnv("PEDANGO_END_OF_SPEECH", 700*time.Millisecond),
		},

		Webhook: WebhookConfig{
			URL:       env("PEDANGO_WEBHOOK_URL", ""),
			Secret:    env("PEDANGO_WEBHOOK_SECRET", ""),
			Timeout:   durEnv("PEDANGO_WEBHOOK_TIMEOUT", 5*time.Second),
			Retries:   intEnv("PEDANGO_WEBHOOK_RETRIES", 3),
			Tolerance: durEnv("PEDANGO_WEBHOOK_TOLERANCE", 5*time.Minute),
		},

		TTS: TTSConfig{
			Provider:    strings.ToLower(env("PEDANGO_TTS_PROVIDER", "tone")),
			APIKey:      env("PEDANGO_TTS_API_KEY", ""),
			VoiceID:     env("PEDANGO_TTS_VOICE_ID", "21m00Tcm4TlvDq8ikWAM"),
			Model:       env("PEDANGO_TTS_MODEL", "eleven_turbo_v2_5"),
			BaseURL:     env("PEDANGO_TTS_BASE_URL", ""),
			Timeout:     durEnv("PEDANGO_TTS_TIMEOUT", 10*time.Second),
			CacheSize:   intEnv("PEDANGO_TTS_CACHE_SIZE", 256),
			CacheTTL:    durEnv("PEDANGO_TTS_CACHE_TTL", time.Hour),
			MaxTextRune: intEnv("PEDANGO_TTS_MAX_CHARS", 1000),
		},

		STT: STTConfig{
			Provider:    strings.ToLower(env("PEDANGO_STT_PROVIDER", "none")),
			APIKey:      env("PEDANGO_STT_API_KEY", ""),
			Model:       env("PEDANGO_STT_MODEL", "nova-2-phonecall"),
			BaseURL:     env("PEDANGO_STT_BASE_URL", ""),
			Language:    env("PEDANGO_STT_LANGUAGE", "en"),
			Endpointing: intEnv("PEDANGO_STT_ENDPOINTING", 300),
		},
	}

	if c.SIP.PublicHost == "" {
		if host, _, err := splitHostPort(c.SIP.Listen); err == nil && host != "0.0.0.0" && host != "::" {
			c.SIP.PublicHost = host
		}
	}
	if c.SIP.Domain == "" {
		c.SIP.Domain = c.SIP.PublicHost
	}
	return c
}

// Production reports whether the service is running with production guardrails.
func (c *Config) Production() bool { return c.Env == "production" }

// Validate returns an error for any configuration that is unsafe to run.
func (c *Config) Validate() error {
	if c.Media.Codec != "pcmu" && c.Media.Codec != "pcma" {
		return fmt.Errorf("PEDANGO_CODEC must be pcmu or pcma, got %q", c.Media.Codec)
	}
	if c.RTP.PortMin <= 0 || c.RTP.PortMax <= c.RTP.PortMin {
		return fmt.Errorf("invalid RTP port range %d-%d", c.RTP.PortMin, c.RTP.PortMax)
	}
	if (c.RTP.PortMax-c.RTP.PortMin)%2 != 0 {
		return fmt.Errorf("RTP port range must span an even number of ports for RTP/RTCP pairing")
	}
	if c.RTP.DTMFPayloadType < 96 || c.RTP.DTMFPayloadType > 127 {
		return fmt.Errorf("PEDANGO_RTP_DTMF_PT must be a dynamic payload type in 96-127")
	}
	if c.Media.JitterMax < c.Media.JitterTarget {
		return fmt.Errorf("PEDANGO_JITTER_MAX must be >= PEDANGO_JITTER_TARGET")
	}
	if c.SIP.Enabled && c.SIP.Register {
		if c.SIP.RegistrarURI == "" || c.SIP.Username == "" {
			return fmt.Errorf("SIP registration needs PEDANGO_SIP_REGISTRAR and PEDANGO_SIP_USERNAME")
		}
	}
	if c.SIP.Enabled && c.SIP.PublicHost == "" {
		return fmt.Errorf("set PEDANGO_SIP_PUBLIC_HOST: a wildcard listen address cannot be advertised in SDP")
	}

	if !c.Production() {
		return nil
	}
	if len(c.HTTP.Tokens) == 0 {
		return fmt.Errorf("PEDANGO_API_TOKENS is required in production")
	}
	for _, t := range c.HTTP.Tokens {
		if len(t) < 32 {
			return fmt.Errorf("every PEDANGO_API_TOKENS entry must be at least 32 characters")
		}
	}
	if c.Webhook.URL != "" && c.Webhook.Secret == "" {
		return fmt.Errorf("PEDANGO_WEBHOOK_SECRET is required in production when a webhook URL is set")
	}
	if c.Webhook.Secret != "" && len(c.Webhook.Secret) < 32 {
		return fmt.Errorf("PEDANGO_WEBHOOK_SECRET must be at least 32 characters")
	}
	if c.Webhook.URL != "" && !strings.HasPrefix(c.Webhook.URL, "https://") {
		return fmt.Errorf("PEDANGO_WEBHOOK_URL must use https in production")
	}
	return nil
}

func parseICEServers(urls, user, cred string) []ICEServer {
	var out []ICEServer
	for _, raw := range strings.Split(urls, ",") {
		u := strings.TrimSpace(raw)
		if u == "" {
			continue
		}
		s := ICEServer{URLs: []string{u}}
		if strings.HasPrefix(u, "turn:") || strings.HasPrefix(u, "turns:") {
			s.Username = user
			s.Credential = cred
		}
		out = append(out, s)
	}
	return out
}

func splitHostPort(addr string) (string, string, error) {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return "", "", fmt.Errorf("missing port in %q", addr)
	}
	return addr[:i], addr[i+1:], nil
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func intEnv(key string, def int) int {
	if v, err := strconv.Atoi(env(key, "")); err == nil {
		return v
	}
	return def
}

func floatEnv(key string, def float64) float64 {
	if v, err := strconv.ParseFloat(env(key, ""), 64); err == nil {
		return v
	}
	return def
}

func boolEnv(key string, def bool) bool {
	if v, err := strconv.ParseBool(env(key, "")); err == nil {
		return v
	}
	return def
}

func durEnv(key string, def time.Duration) time.Duration {
	raw := env(key, "")
	if raw == "" {
		return def
	}
	if v, err := time.ParseDuration(raw); err == nil {
		return v
	}
	// Bare numbers are read as milliseconds, which is how most telephony
	// tunables are expressed.
	if ms, err := strconv.Atoi(raw); err == nil {
		return time.Duration(ms) * time.Millisecond
	}
	return def
}

func listEnv(key string, def []string) []string {
	raw := env(key, "")
	if raw == "" {
		return def
	}
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}
