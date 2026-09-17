// Package tts turns text into 8 kHz telephone-ready audio.
package tts

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/pedango/pedango/internal/audio"
	"github.com/pedango/pedango/internal/config"
)

// ErrUnknownProvider is returned when a request names a provider that is not
// configured.
var ErrUnknownProvider = errors.New("tts: unknown provider")

// Request describes one synthesis.
type Request struct {
	Text  string
	Voice string
	// Provider overrides the configured default.
	Provider string
}

// Provider synthesizes speech. Implementations must return 8 kHz mono linear
// PCM so the result can be queued straight onto a media path.
type Provider interface {
	Name() string
	Synthesize(ctx context.Context, r Request) ([]int16, error)
}

// Engine resolves providers and caches their output.
type Engine struct {
	cfg       config.TTSConfig
	log       *slog.Logger
	providers map[string]Provider
	def       string
	cache     *cache
}

// NewEngine builds the engine and registers every provider that has the
// configuration it needs.
func NewEngine(cfg config.TTSConfig, log *slog.Logger) *Engine {
	e := &Engine{
		cfg:       cfg,
		log:       log,
		providers: map[string]Provider{},
		def:       cfg.Provider,
		cache:     newCache(cfg.CacheSize, cfg.CacheTTL),
	}

	// The tone provider always exists so a fresh checkout makes sound without
	// any API key, which matters for verifying the media path in isolation
	// from any vendor.
	e.providers["tone"] = &toneProvider{}
	e.providers["silence"] = &silenceProvider{}

	if cfg.APIKey != "" {
		e.providers["elevenlabs"] = newElevenLabs(cfg)
	}
	if cfg.BaseURL != "" {
		e.providers["http"] = newHTTPProvider(cfg)
	}

	if _, ok := e.providers[e.def]; !ok {
		if e.def != "" && e.def != "tone" {
			log.Warn("configured TTS provider is unavailable, falling back to tone",
				"provider", e.def, "reason", "missing API key or base URL")
		}
		e.def = "tone"
	}
	return e
}

// Available lists the registered provider names.
func (e *Engine) Available() []string {
	out := make([]string, 0, len(e.providers))
	for name := range e.providers {
		out = append(out, name)
	}
	return out
}

// Default reports the provider used when a request does not name one.
func (e *Engine) Default() string { return e.def }

// Synthesize renders text to 8 kHz PCM, serving repeats from cache.
func (e *Engine) Synthesize(ctx context.Context, r Request) ([]int16, error) {
	text := strings.TrimSpace(r.Text)
	if text == "" {
		return nil, errors.New("tts: empty text")
	}
	if e.cfg.MaxTextRune > 0 && len([]rune(text)) > e.cfg.MaxTextRune {
		return nil, fmt.Errorf("tts: text is %d characters, limit is %d",
			len([]rune(text)), e.cfg.MaxTextRune)
	}
	r.Text = text

	name := r.Provider
	if name == "" {
		name = e.def
	}
	p, ok := e.providers[name]
	if !ok {
		// An unknown provider must not silence the call: the prompt still has
		// to be spoken, so fall back rather than fail the request.
		e.log.Warn("unknown TTS provider requested, using default",
			"requested", name, "using", e.def)
		p, ok = e.providers[e.def]
		if !ok {
			return nil, ErrUnknownProvider
		}
		name = e.def
	}

	voice := r.Voice
	if voice == "" {
		voice = e.cfg.VoiceID
	}
	key := cacheKey(name, voice, text)

	if pcm, hit := e.cache.get(key); hit {
		return pcm, nil
	}

	start := time.Now()
	pcm, err := p.Synthesize(ctx, Request{Text: text, Voice: voice})
	if err != nil {
		return nil, err
	}
	e.log.Debug("tts synthesized",
		"provider", name,
		"chars", len(text),
		"samples", len(pcm),
		"took", time.Since(start),
	)

	e.cache.put(key, pcm)
	return pcm, nil
}

func cacheKey(provider, voice, text string) string {
	sum := sha256.Sum256([]byte(provider + "\x00" + voice + "\x00" + text))
	return hex.EncodeToString(sum[:16])
}

// cache is a small LRU with expiry.
//
// IVR prompts are extremely repetitive: "Enter your PIN" is synthesized on
// every single call. Caching removes a network round trip from the critical
// path of every conversation turn and cuts the provider bill by roughly the
// repeat rate.
type cache struct {
	mu    sync.Mutex
	max   int
	ttl   time.Duration
	ll    *list.List
	items map[string]*list.Element

	hits   uint64
	misses uint64
}

type entry struct {
	key       string
	pcm       []int16
	expiresAt time.Time
}

func newCache(max int, ttl time.Duration) *cache {
	if max <= 0 {
		max = 128
	}
	if ttl <= 0 {
		ttl = time.Hour
	}
	return &cache{
		max:   max,
		ttl:   ttl,
		ll:    list.New(),
		items: make(map[string]*list.Element, max),
	}
}

func (c *cache) get(key string) ([]int16, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.items[key]
	if !ok {
		c.misses++
		return nil, false
	}
	ent := el.Value.(*entry)
	if time.Now().After(ent.expiresAt) {
		c.ll.Remove(el)
		delete(c.items, key)
		c.misses++
		return nil, false
	}
	c.ll.MoveToFront(el)
	c.hits++
	return ent.pcm, true
}

func (c *cache) put(key string, pcm []int16) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.items[key]; ok {
		c.ll.MoveToFront(el)
		el.Value.(*entry).pcm = pcm
		el.Value.(*entry).expiresAt = time.Now().Add(c.ttl)
		return
	}
	el := c.ll.PushFront(&entry{key: key, pcm: pcm, expiresAt: time.Now().Add(c.ttl)})
	c.items[key] = el

	for c.ll.Len() > c.max {
		oldest := c.ll.Back()
		if oldest == nil {
			break
		}
		c.ll.Remove(oldest)
		delete(c.items, oldest.Value.(*entry).key)
	}
}

// Stats reports cache effectiveness.
type Stats struct {
	Provider  string   `json:"provider"`
	Available []string `json:"available"`
	CacheSize int      `json:"cache_size"`
	Hits      uint64   `json:"cache_hits"`
	Misses    uint64   `json:"cache_misses"`
}

// Stats returns engine statistics.
func (e *Engine) Stats() Stats {
	e.cache.mu.Lock()
	size, hits, misses := e.cache.ll.Len(), e.cache.hits, e.cache.misses
	e.cache.mu.Unlock()

	return Stats{
		Provider:  e.def,
		Available: e.Available(),
		CacheSize: size,
		Hits:      hits,
		Misses:    misses,
	}
}

// silenceProvider renders the requested duration of nothing, used to hold a
// line open without audio.
type silenceProvider struct{}

func (silenceProvider) Name() string { return "silence" }

func (silenceProvider) Synthesize(_ context.Context, r Request) ([]int16, error) {
	// Roughly the time the text would take to speak, at a typical 15 phonemes
	// per second.
	ms := len(r.Text) * 60
	if ms < 200 {
		ms = 200
	}
	return make([]int16, ms*audio.SamplesPerMs), nil
}
