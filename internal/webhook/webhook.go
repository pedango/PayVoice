// Package webhook delivers signed media events to the application.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/pedango/pedango/internal/config"
	"github.com/pedango/pedango/internal/media"
)

// Header names carried on every delivery.
const (
	// HeaderSignature is Pedango's own signature header.
	HeaderSignature = "X-Pedango-Signature"
	// HeaderSignatureCompat is an alias many bridges already read, sent so an
	// existing receiver needs no change.
	HeaderSignatureCompat = "X-Webhook-Signature"
	// HeaderTimestamp carries the signing time for replay rejection.
	HeaderTimestamp = "X-Pedango-Timestamp"
	// HeaderDelivery is a unique id per attempt, for idempotent receivers.
	HeaderDelivery = "X-Pedango-Delivery"
	// HeaderEventType duplicates the event type for cheap routing.
	HeaderEventType = "X-Pedango-Event"
)

// Envelope is the JSON body delivered to the application.
type Envelope struct {
	ID        string         `json:"id"`
	Type      string         `json:"type"`
	CreatedAt time.Time      `json:"created_at"`
	Data      map[string]any `json:"data"`
}

// Dispatcher queues events and delivers them with retries.
//
// Events are sharded across workers by leg id rather than spread over a shared
// queue. Ordering within a leg is not cosmetic: the application reconstructs a
// PIN from a sequence of dtmf.received events, and delivering "4" before "1"
// would authorise the wrong thing. Sharding preserves per-leg order while
// still letting a slow response on one call overlap with another.
type Dispatcher struct {
	cfg    config.WebhookConfig
	client *http.Client
	log    *slog.Logger

	queues []chan Envelope
	wg     sync.WaitGroup
	once   sync.Once
	closed chan struct{}

	sent    atomic.Uint64
	failed  atomic.Uint64
	dropped atomic.Uint64
}

const (
	workerCount = 4
	queueDepth  = 256
)

// New builds a dispatcher. A dispatcher with no URL configured accepts and
// discards events, which is the correct behaviour for local development.
func New(cfg config.WebhookConfig, log *slog.Logger) *Dispatcher {
	d := &Dispatcher{
		cfg: cfg,
		log: log,
		client: &http.Client{
			Timeout: cfg.Timeout,
			Transport: &http.Transport{
				MaxIdleConnsPerHost: workerCount,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		queues: make([]chan Envelope, workerCount),
		closed: make(chan struct{}),
	}
	for i := range d.queues {
		d.queues[i] = make(chan Envelope, queueDepth)
	}
	return d
}

// Start launches the delivery workers.
func (d *Dispatcher) Start() {
	if d.cfg.URL == "" {
		return
	}
	for i := range d.queues {
		d.wg.Add(1)
		go d.worker(d.queues[i])
	}
}

// Sink returns an event sink for the media engine.
func (d *Dispatcher) Sink() media.EventSink {
	return func(e media.Event) { d.Publish(e) }
}

// Publish queues an event. It never blocks: the media clock calls this from
// inside a 20 ms budget, so a slow or wedged receiver must degrade into
// dropped events rather than stuttering audio.
func (d *Dispatcher) Publish(e media.Event) {
	if d.cfg.URL == "" {
		return
	}
	select {
	case <-d.closed:
		return
	default:
	}

	at := e.At
	if at.IsZero() {
		at = time.Now()
	}
	env := Envelope{
		ID:        "evt_" + uuid.NewString(),
		Type:      e.Type,
		CreatedAt: at,
		Data:      e.Data,
	}

	q := d.queues[shardFor(e.Data)]
	select {
	case q <- env:
	default:
		d.dropped.Add(1)
		d.log.Warn("webhook queue full, dropping event", "type", e.Type)
	}
}

// shardFor picks a worker by leg id so one leg's events stay ordered.
func shardFor(data map[string]any) int {
	key, _ := data["leg_id"].(string)
	if key == "" {
		key, _ = data["room_id"].(string)
	}
	if key == "" {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return int(h.Sum32() % workerCount)
}

func (d *Dispatcher) worker(q chan Envelope) {
	defer d.wg.Done()
	for env := range q {
		d.deliver(env)
	}
}

func (d *Dispatcher) deliver(env Envelope) {
	body, err := json.Marshal(env)
	if err != nil {
		d.log.Error("cannot marshal event", "type", env.Type, "error", err)
		return
	}

	backoff := 250 * time.Millisecond
	for attempt := 0; attempt <= d.cfg.Retries; attempt++ {
		if attempt > 0 {
			select {
			case <-d.closed:
				return
			case <-time.After(backoff):
			}
			backoff *= 2
		}

		status, err := d.post(body, env.Type)
		switch {
		case err == nil && status >= 200 && status < 300:
			d.sent.Add(1)
			return
		case err == nil && status >= 400 && status < 500 && status != http.StatusTooManyRequests:
			// The receiver rejected the event outright. Retrying a 4xx just
			// burns the queue, so give up and say so loudly.
			d.failed.Add(1)
			d.log.Error("webhook rejected", "type", env.Type, "status", status)
			return
		default:
			d.log.Warn("webhook delivery failed",
				"type", env.Type, "attempt", attempt+1, "status", status, "error", err)
		}
	}
	d.failed.Add(1)
}

func (d *Dispatcher) post(body []byte, eventType string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d.cfg.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}

	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	sig := Sign(d.cfg.Secret, body)

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Pedango/1.0")
	req.Header.Set(HeaderSignature, sig)
	req.Header.Set(HeaderSignatureCompat, sig)
	req.Header.Set(HeaderTimestamp, timestamp)
	req.Header.Set(HeaderDelivery, uuid.NewString())
	req.Header.Set(HeaderEventType, eventType)

	res, err := d.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	// The body must be drained for the connection to be reused.
	_, _ = res.Body.Read(make([]byte, 0))
	return res.StatusCode, nil
}

// Sign returns the signature header value for a raw body.
//
// The signature covers the exact bytes sent, so a receiver must verify against
// its raw request body rather than a re-encoded copy: any JSON round trip can
// reorder keys and invalidate an otherwise valid signature.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// Verify checks a signature in constant time. It is exported so the same
// implementation can be used in tests on both sides of the wire.
func Verify(secret string, body []byte, header string) bool {
	if secret == "" || header == "" {
		return false
	}
	expected := Sign(secret, body)
	return hmac.Equal([]byte(expected), []byte(header))
}

// Stats reports delivery health.
type Stats struct {
	Configured bool   `json:"configured"`
	Sent       uint64 `json:"sent"`
	Failed     uint64 `json:"failed"`
	Dropped    uint64 `json:"dropped"`
	Queued     int    `json:"queued"`
}

// Stats returns a snapshot.
func (d *Dispatcher) Stats() Stats {
	queued := 0
	for _, q := range d.queues {
		queued += len(q)
	}
	return Stats{
		Configured: d.cfg.URL != "",
		Sent:       d.sent.Load(),
		Failed:     d.failed.Load(),
		Dropped:    d.dropped.Load(),
		Queued:     queued,
	}
}

// Close drains the queues and stops the workers.
func (d *Dispatcher) Close(ctx context.Context) error {
	d.once.Do(func() {
		close(d.closed)
		for _, q := range d.queues {
			close(q)
		}
	})

	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("webhook: shutdown timed out with %d events queued", d.Stats().Queued)
	}
}
