package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pedango/pedango/internal/config"
	"github.com/pedango/pedango/internal/media"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestSignMatchesIndependentHMAC verifies the signature against a hand-rolled
// HMAC rather than against Sign itself, so a bug in Sign cannot make the test
// agree with it.
func TestSignMatchesIndependentHMAC(t *testing.T) {
	secret := "a-secret-at-least-thirty-two-characters"
	body := []byte(`{"type":"dtmf.received","data":{"digit":"7"}}`)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if got := Sign(secret, body); got != want {
		t.Errorf("Sign = %s, want %s", got, want)
	}
}

// TestSignIsCompatibleWithReceiver reproduces the verification a Node receiver
// performs: HMAC-SHA256 over the raw body, hex encoded, with the sha256=
// prefix stripped. This is the contract that keeps PayPlus able to validate
// Pedango's events without any change on its side.
func TestSignIsCompatibleWithReceiver(t *testing.T) {
	secret := "shared-secret-value-used-by-both-sides"
	body := []byte(`{"id":"evt_1","type":"leg.ringing"}`)

	header := Sign(secret, body)
	stripped := strings.TrimPrefix(header, "sha256=")

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))

	if stripped != expected {
		t.Errorf("receiver would compute %s but header carries %s", expected, stripped)
	}
}

func TestVerifyRejectsTamperedBody(t *testing.T) {
	secret := "another-secret-that-is-long-enough-here"
	body := []byte(`{"amount":100}`)
	sig := Sign(secret, body)

	if !Verify(secret, body, sig) {
		t.Fatal("a valid signature was rejected")
	}
	if Verify(secret, []byte(`{"amount":100000}`), sig) {
		t.Error("a tampered body passed verification")
	}
	if Verify("wrong-secret-wrong-secret-wrong-secre", body, sig) {
		t.Error("the wrong secret passed verification")
	}
	if Verify(secret, body, "") {
		t.Error("an empty signature passed verification")
	}
}

// TestDispatcherDeliversSignedEvent is the end-to-end check against a real
// HTTP receiver.
func TestDispatcherDeliversSignedEvent(t *testing.T) {
	secret := "delivery-test-secret-long-enough-ok-yes"

	var (
		mu       sync.Mutex
		gotBody  []byte
		gotSig   string
		gotEvent string
		received = make(chan struct{}, 1)
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody = body
		gotSig = r.Header.Get(HeaderSignature)
		gotEvent = r.Header.Get(HeaderEventType)
		mu.Unlock()

		select {
		case received <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := New(config.WebhookConfig{
		URL:     srv.URL,
		Secret:  secret,
		Timeout: 2 * time.Second,
		Retries: 1,
	}, testLogger())
	d.Start()

	d.Publish(media.Event{
		Type: "dtmf.received",
		At:   time.Now(),
		Data: map[string]any{"leg_id": "leg_abc", "digit": "4"},
	})

	select {
	case <-received:
	case <-time.After(3 * time.Second):
		t.Fatal("webhook was never delivered")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = d.Close(ctx)

	mu.Lock()
	defer mu.Unlock()

	if !Verify(secret, gotBody, gotSig) {
		t.Errorf("delivered signature did not verify\nbody: %s\nsig: %s", gotBody, gotSig)
	}
	if gotEvent != "dtmf.received" {
		t.Errorf("event header = %q, want dtmf.received", gotEvent)
	}

	var env Envelope
	if err := json.Unmarshal(gotBody, &env); err != nil {
		t.Fatalf("delivered body is not valid JSON: %v", err)
	}
	if env.Type != "dtmf.received" {
		t.Errorf("envelope type = %q, want dtmf.received", env.Type)
	}
	if env.Data["digit"] != "4" {
		t.Errorf("envelope lost the digit: %v", env.Data)
	}
	if !strings.HasPrefix(env.ID, "evt_") {
		t.Errorf("envelope id = %q, want an evt_ prefix", env.ID)
	}
}

// TestDispatcherRetriesServerErrors covers a receiver that is briefly down.
func TestDispatcherRetriesServerErrors(t *testing.T) {
	var attempts sync.WaitGroup
	attempts.Add(2)

	var count int
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		count++
		current := count
		mu.Unlock()

		if current <= 2 {
			attempts.Done()
		}
		if current == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := New(config.WebhookConfig{
		URL:     srv.URL,
		Secret:  "retry-secret-value-long-enough-for-test",
		Timeout: time.Second,
		Retries: 3,
	}, testLogger())
	d.Start()

	d.Publish(media.Event{Type: "leg.answered", Data: map[string]any{"leg_id": "leg_retry"}})

	done := make(chan struct{})
	go func() { attempts.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("dispatcher did not retry after a 500")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = d.Close(ctx)

	if d.Stats().Sent != 1 {
		t.Errorf("Sent = %d, want 1 after a successful retry", d.Stats().Sent)
	}
}

// TestDispatcherGivesUpOnClientError checks that a 400 is not retried. A
// malformed event will never become valid, and retrying it only crowds out
// events that would succeed.
func TestDispatcherGivesUpOnClientError(t *testing.T) {
	var mu sync.Mutex
	var count int
	hit := make(chan struct{}, 4)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		count++
		mu.Unlock()
		select {
		case hit <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	d := New(config.WebhookConfig{
		URL:     srv.URL,
		Secret:  "client-error-secret-long-enough-for-test",
		Timeout: time.Second,
		Retries: 3,
	}, testLogger())
	d.Start()

	d.Publish(media.Event{Type: "leg.ended", Data: map[string]any{"leg_id": "leg_400"}})

	select {
	case <-hit:
	case <-time.After(3 * time.Second):
		t.Fatal("event was never delivered")
	}
	time.Sleep(600 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = d.Close(ctx)

	mu.Lock()
	defer mu.Unlock()
	if count != 1 {
		t.Errorf("receiver was hit %d times, want exactly 1 for a 4xx", count)
	}
}

// TestPublishNeverBlocks is a hard requirement: Publish runs on the media
// clock, so a wedged receiver must cost dropped events rather than stuttering
// audio on every live call.
func TestPublishNeverBlocks(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-block
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	defer close(block)

	d := New(config.WebhookConfig{
		URL:     srv.URL,
		Secret:  "blocking-secret-value-long-enough-test",
		Timeout: 30 * time.Second,
		Retries: 0,
	}, testLogger())
	d.Start()

	done := make(chan struct{})
	go func() {
		// Far more than the queue can hold.
		for i := 0; i < queueDepth*workerCount*3; i++ {
			d.Publish(media.Event{
				Type: "speech.started",
				Data: map[string]any{"leg_id": "leg_flood"},
			})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Publish blocked when the receiver stalled")
	}

	if d.Stats().Dropped == 0 {
		t.Error("expected events to be dropped once the queue filled")
	}
}

// TestShardingKeepsLegEventsOrdered protects PIN reconstruction: the digits of
// a PIN arrive as separate events and must be delivered in order.
func TestShardingKeepsLegEventsOrdered(t *testing.T) {
	first := shardFor(map[string]any{"leg_id": "leg_pin"})
	for i := 0; i < 50; i++ {
		if got := shardFor(map[string]any{"leg_id": "leg_pin", "digit": i}); got != first {
			t.Fatalf("events for one leg were routed to shards %d and %d", first, got)
		}
	}
}

func TestPublishWithoutURLIsNoop(t *testing.T) {
	d := New(config.WebhookConfig{}, testLogger())
	d.Start()
	d.Publish(media.Event{Type: "leg.ringing", Data: map[string]any{"leg_id": "x"}})

	if st := d.Stats(); st.Configured || st.Queued != 0 {
		t.Errorf("stats = %+v, want an unconfigured dispatcher with nothing queued", st)
	}
}
