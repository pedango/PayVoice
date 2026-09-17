package stt

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/pedango/pedango/internal/audio"
	"github.com/pedango/pedango/internal/config"
)

// deepgram streams mu-law audio to Deepgram's realtime endpoint.
type deepgram struct {
	cfg config.STTConfig
	log *slog.Logger
}

func newDeepgram(cfg config.STTConfig, log *slog.Logger) *deepgram {
	return &deepgram{cfg: cfg, log: log}
}

func (d *deepgram) Name() string { return "deepgram" }

func (d *deepgram) Open(ctx context.Context, onResult func(Result)) (Stream, error) {
	base := d.cfg.BaseURL
	if base == "" {
		base = "wss://api.deepgram.com/v1/listen"
	}

	// Sending mu-law rather than linear PCM halves the upstream bandwidth per
	// call and is exactly what arrived from the network, so nothing is lost to
	// an extra conversion.
	q := url.Values{}
	q.Set("encoding", "mulaw")
	q.Set("sample_rate", strconv.Itoa(audio.SampleRate))
	q.Set("channels", "1")
	q.Set("model", d.cfg.Model)
	q.Set("language", d.cfg.Language)
	q.Set("interim_results", "true")
	q.Set("punctuate", "true")
	q.Set("smart_format", "true")
	if d.cfg.Endpointing > 0 {
		q.Set("endpointing", strconv.Itoa(d.cfg.Endpointing))
	}

	endpoint := base + "?" + q.Encode()
	header := http.Header{}
	header.Set("Authorization", "Token "+d.cfg.APIKey)

	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, res, err := dialer.DialContext(ctx, endpoint, header)
	if err != nil {
		if res != nil {
			return nil, fmt.Errorf("stt: deepgram handshake failed with %d: %w", res.StatusCode, err)
		}
		return nil, fmt.Errorf("stt: deepgram dial: %w", err)
	}

	s := &deepgramStream{
		conn:     conn,
		log:      d.log,
		onResult: onResult,
		// The queue absorbs a stall on the websocket without ever blocking the
		// media clock. Two seconds of audio is enough to ride out a transient
		// and short enough that recognition never lags the conversation.
		queue: make(chan []byte, 100),
		done:  make(chan struct{}),
	}

	go s.writeLoop()
	go s.readLoop()
	return s, nil
}

type deepgramStream struct {
	conn     *websocket.Conn
	log      *slog.Logger
	onResult func(Result)

	queue chan []byte
	done  chan struct{}

	// writeMu guards the websocket, which gorilla requires for concurrent
	// writers: the keepalive and the audio pump both write.
	writeMu sync.Mutex
	once    sync.Once
	dropped uint64
}

func (s *deepgramStream) Write(pcm []int16) error {
	payload := audio.CodecPCMU.Encode(nil, pcm)

	select {
	case <-s.done:
		return nil
	case s.queue <- payload:
		return nil
	default:
		// Dropping audio degrades recognition; blocking would degrade every
		// call on the process. Dropping is the right trade.
		s.dropped++
		return nil
	}
}

func (s *deepgramStream) writeLoop() {
	// Deepgram closes an idle socket, and a call can legitimately be silent
	// for longer than that while the caller thinks.
	keepalive := time.NewTicker(8 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-s.done:
			return

		case <-keepalive.C:
			s.writeMu.Lock()
			err := s.conn.WriteJSON(map[string]string{"type": "KeepAlive"})
			s.writeMu.Unlock()
			if err != nil {
				return
			}

		case payload, ok := <-s.queue:
			if !ok {
				return
			}
			s.writeMu.Lock()
			err := s.conn.WriteMessage(websocket.BinaryMessage, payload)
			s.writeMu.Unlock()
			if err != nil {
				s.log.Debug("deepgram write failed", "error", err)
				return
			}
		}
	}
}

// deepgramResponse is the subset of the realtime message Pedango consumes.
type deepgramResponse struct {
	Type    string `json:"type"`
	IsFinal bool   `json:"is_final"`
	Channel struct {
		Alternatives []struct {
			Transcript string  `json:"transcript"`
			Confidence float64 `json:"confidence"`
		} `json:"alternatives"`
	} `json:"channel"`
}

func (s *deepgramStream) readLoop() {
	defer s.Close()

	for {
		_, data, err := s.conn.ReadMessage()
		if err != nil {
			return
		}

		var res deepgramResponse
		if err := json.Unmarshal(data, &res); err != nil {
			continue
		}
		if len(res.Channel.Alternatives) == 0 {
			continue
		}

		alt := res.Channel.Alternatives[0]
		if alt.Transcript == "" {
			continue
		}
		if s.onResult != nil {
			s.onResult(Result{
				Text:       alt.Transcript,
				Final:      res.IsFinal,
				Confidence: alt.Confidence,
			})
		}
	}
}

func (s *deepgramStream) Close() error {
	s.once.Do(func() {
		close(s.done)

		// Asking Deepgram to close flushes any partial utterance, so a caller
		// who hangs up mid-sentence still yields a final transcript.
		s.writeMu.Lock()
		_ = s.conn.WriteJSON(map[string]string{"type": "CloseStream"})
		s.writeMu.Unlock()

		_ = s.conn.Close()
	})
	return nil
}
