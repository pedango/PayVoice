package media

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pedango/PayVoice/internal/audio"
	"github.com/pedango/PayVoice/internal/config"
)

// fakeTransport records everything written so tests can assert on what a peer
// would actually have heard.
type fakeTransport struct {
	mu     sync.Mutex
	frames []audio.Frame
	dtmf   []rune
	closed bool
}

func (f *fakeTransport) WriteFrame(fr audio.Frame) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := audio.NewFrame()
	copy(cp, fr)
	f.frames = append(f.frames, cp)
	return nil
}

func (f *fakeTransport) SendDTMF(d rune, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dtmf = append(f.dtmf, d)
	return nil
}

func (f *fakeTransport) Kind() string       { return "fake" }
func (f *fakeTransport) RemoteAddr() string { return "test" }

func (f *fakeTransport) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeTransport) last() audio.Frame {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.frames) == 0 {
		return nil
	}
	return f.frames[len(f.frames)-1]
}

func (f *fakeTransport) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.frames)
}

type recorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *recorder) sink(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recorder) types() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	for i, e := range r.events {
		out[i] = e.Type
	}
	return out
}

func (r *recorder) has(t string) bool {
	for _, got := range r.types() {
		if got == t {
			return true
		}
	}
	return false
}

func newTestLeg(id string, tr Transport, emit EventSink) *Leg {
	return NewLeg(tr, LegOptions{
		ID:           id,
		Direction:    Inbound,
		From:         "0244000000",
		To:           "0245224253",
		JitterTarget: 20 * time.Millisecond,
		JitterMax:    160 * time.Millisecond,
		VADThreshold: 300,
		EndOfSpeech:  200 * time.Millisecond,
		Emit:         emit,
	})
}

func steady(v int16) []int16 {
	f := make([]int16, audio.FrameSamples)
	for i := range f {
		f[i] = v
	}
	return f
}

// TestRoomBridgesTwoLegs is the core behaviour of the whole service: audio
// from a phone leg must reach a browser leg and vice versa, with neither
// hearing itself.
func TestRoomBridgesTwoLegs(t *testing.T) {
	rec := &recorder{}
	ta, tb := &fakeTransport{}, &fakeTransport{}
	a := newTestLeg("leg_a", ta, rec.sink)
	b := newTestLeg("leg_b", tb, rec.sink)

	room := NewRoom("room_1", 8, rec.sink)
	if err := room.Add(a); err != nil {
		t.Fatalf("add leg A: %v", err)
	}
	if err := room.Add(b); err != nil {
		t.Fatalf("add leg B: %v", err)
	}

	// Feed each leg a distinct constant tone, enough frames to clear priming.
	for i := 0; i < 8; i++ {
		ts := uint32(i * audio.FrameSamples)
		a.PushAudio(ts, steady(1000))
		b.PushAudio(ts, steady(-2000))
	}
	for i := 0; i < 8; i++ {
		room.tick()
	}

	gotA, gotB := ta.last(), tb.last()
	if gotA == nil || gotB == nil {
		t.Fatal("no audio was transmitted")
	}
	if gotA[0] != -2000 {
		t.Errorf("leg A heard %d, want leg B's -2000 (self-echo or mixing bug)", gotA[0])
	}
	if gotB[0] != 1000 {
		t.Errorf("leg B heard %d, want leg A's 1000", gotB[0])
	}
}

// TestRoomRemovesLegOnClose makes sure tearing down a leg also detaches it, so
// the mixer cannot keep writing to a closed transport.
func TestRoomRemovesLegOnClose(t *testing.T) {
	rec := &recorder{}
	tr := &fakeTransport{}
	l := newTestLeg("leg_x", tr, rec.sink)

	room := NewRoom("room_x", 8, rec.sink)
	_ = room.Add(l)
	if room.Size() != 1 {
		t.Fatalf("room size = %d, want 1", room.Size())
	}

	l.Close("hangup")

	if room.Size() != 0 {
		t.Errorf("room size = %d after close, want 0", room.Size())
	}
	if !tr.closed {
		t.Error("transport was not closed")
	}
	if !rec.has("leg.ended") {
		t.Errorf("no leg.ended event, got %v", rec.types())
	}
}

func TestRoomRejectsWhenFull(t *testing.T) {
	rec := &recorder{}
	room := NewRoom("room_small", 1, rec.sink)

	if err := room.Add(newTestLeg("leg_1", &fakeTransport{}, rec.sink)); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if err := room.Add(newTestLeg("leg_2", &fakeTransport{}, rec.sink)); err != ErrRoomFull {
		t.Errorf("second add error = %v, want ErrRoomFull", err)
	}
}

// TestPlayerPacesOutput verifies that a long prompt leaves the service as a
// frame stream rather than one burst.
func TestPlayerPacesOutput(t *testing.T) {
	p := NewPlayer()
	// 100 ms of audio is exactly five frames.
	pcm := make([]int16, audio.FrameSamples*5)
	for i := range pcm {
		pcm[i] = 5000
	}
	p.Enqueue("pb_1", pcm, 1)

	dst := audio.NewFrame()
	var started, finished string
	for i := 0; i < 5; i++ {
		playing, ev := p.Next(dst)
		if !playing {
			t.Fatalf("frame %d: player stopped early", i)
		}
		if ev.Started != "" {
			started = ev.Started
		}
		if ev.Finished != "" {
			finished = ev.Finished
		}
		if dst[0] != 5000 {
			t.Fatalf("frame %d sample = %d, want 5000", i, dst[0])
		}
	}
	if started != "pb_1" {
		t.Errorf("started = %q, want pb_1", started)
	}
	if finished != "pb_1" {
		t.Errorf("finished = %q, want pb_1 on the fifth frame", finished)
	}
	if playing, _ := p.Next(dst); playing {
		t.Error("player kept producing audio past the end of the clip")
	}
}

// TestPlayerStopFadesOut checks the barge-in taper. Cutting straight to zero
// mid-word produces an audible click on a phone line.
func TestPlayerStopFadesOut(t *testing.T) {
	p := NewPlayer()
	pcm := make([]int16, audio.FrameSamples*10)
	for i := range pcm {
		pcm[i] = 8000
	}
	p.Enqueue("pb_fade", pcm, 1)

	dst := audio.NewFrame()
	p.Next(dst)

	if ids := p.Stop(); len(ids) != 1 || ids[0] != "pb_fade" {
		t.Fatalf("Stop returned %v, want [pb_fade]", ids)
	}

	playing, _ := p.Next(dst)
	if !playing {
		t.Fatal("expected a fade frame after stop")
	}
	if dst[0] == 0 || dst[0] >= 8000 {
		t.Errorf("fade sample = %d, want a value between 0 and 8000", dst[0])
	}

	// The taper is short: it must reach silence quickly, not linger.
	for i := 0; i < fadeOutFrames; i++ {
		p.Next(dst)
	}
	if playing, _ := p.Next(dst); playing {
		t.Error("fade did not finish")
	}
}

// TestBargeInStopsPrompt is the behaviour that makes a voice agent usable: the
// caller must be able to interrupt.
func TestBargeInStopsPrompt(t *testing.T) {
	rec := &recorder{}
	l := newTestLeg("leg_barge", &fakeTransport{}, rec.sink)
	l.SetBargeIn(true)

	long := make([]int16, audio.FrameSamples*100)
	for i := range long {
		long[i] = 4000
	}
	l.Player().Enqueue("pb_long", long, 1)

	// Loud caller speech across several frames triggers the detector.
	for i := 0; i < 20; i++ {
		l.PushAudio(uint32(i*audio.FrameSamples), steady(12000))
	}
	for i := 0; i < 20 && l.Player().Playing(); i++ {
		l.receive()
	}

	if l.Player().Playing() {
		t.Fatal("prompt kept playing while the caller spoke")
	}
	if !rec.has("playback.interrupted") {
		t.Errorf("no playback.interrupted event, got %v", rec.types())
	}
}

// TestDTMFStopsPromptAndEmits covers keypad entry, the path a PIN travels.
func TestDTMFStopsPromptAndEmits(t *testing.T) {
	rec := &recorder{}
	l := newTestLeg("leg_dtmf", &fakeTransport{}, rec.sink)

	pcm := make([]int16, audio.FrameSamples*50)
	l.Player().Enqueue("pb_pin", pcm, 1)

	l.PushDTMF('7')

	if l.Player().Playing() {
		t.Error("a keypress did not interrupt the prompt")
	}
	if !rec.has("dtmf.received") {
		t.Errorf("no dtmf.received event, got %v", rec.types())
	}
}

// TestLegStateIsMonotonic guards against SIP forking delivering a late 180
// after a 200, which must not rewind an answered call to ringing.
func TestLegStateIsMonotonic(t *testing.T) {
	rec := &recorder{}
	l := newTestLeg("leg_state", &fakeTransport{}, rec.sink)

	l.SetState(LegStateRinging)
	l.SetState(LegStateAnswered)
	l.SetState(LegStateRinging) // late provisional response

	if l.State() != LegStateAnswered {
		t.Errorf("state = %v, want answered", l.State())
	}
}

// TestSoloLegHearsOnlyItsOwnPrompt covers a caller who is not yet bridged.
func TestSoloLegHearsOnlyItsOwnPrompt(t *testing.T) {
	rec := &recorder{}
	tr := &fakeTransport{}
	l := newTestLeg("leg_solo", tr, rec.sink)
	l.SetState(LegStateAnswered)

	pcm := make([]int16, audio.FrameSamples*3)
	for i := range pcm {
		pcm[i] = 6000
	}
	l.Player().Enqueue("pb_solo", pcm, 1)

	l.tickSolo()

	got := tr.last()
	if got == nil {
		t.Fatal("solo leg transmitted nothing")
	}
	if got[0] != 6000 {
		t.Errorf("solo leg sent %d, want its own prompt at 6000", got[0])
	}
}

// TestEngineClockDrivesRooms exercises the real ticker end to end.
func TestEngineClockDrivesRooms(t *testing.T) {
	rec := &recorder{}
	e := NewEngine(config.MediaConfig{
		MaxLegsPerRoom:  8,
		RoomIdleTimeout: time.Hour,
		LegMaxDuration:  time.Hour,
	}, rec.sink)

	room := e.CreateRoom("room_clock")
	tr := &fakeTransport{}
	l := newTestLeg("leg_clock", tr, rec.sink)
	l.SetState(LegStateAnswered)
	e.AddLeg(l)
	if err := room.Add(l); err != nil {
		t.Fatalf("add: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	go func() { _ = e.Run(ctx) }()
	<-ctx.Done()

	// 250 ms at 20 ms per tick is about 12 frames; allow generous slack for
	// scheduler jitter on a loaded CI machine.
	if n := tr.count(); n < 5 {
		t.Errorf("transmitted %d frames in 250ms, want at least 5", n)
	}
	if st := e.Stats(); st.Ticks < 5 {
		t.Errorf("engine ticks = %d, want at least 5", st.Ticks)
	}
}

// TestEngineReapsEndedLegs makes sure a dead leg does not leak its registry
// entry, which would also leak its RTP port.
func TestEngineReapsEndedLegs(t *testing.T) {
	rec := &recorder{}
	e := NewEngine(config.MediaConfig{
		MaxLegsPerRoom:  8,
		RoomIdleTimeout: time.Hour,
		LegMaxDuration:  time.Hour,
	}, rec.sink)

	l := newTestLeg("leg_dead", &fakeTransport{}, rec.sink)
	e.AddLeg(l)
	l.Close("hangup")

	e.reap()

	if e.Leg("leg_dead") != nil {
		t.Error("ended leg was not reaped from the registry")
	}
}

// TestEngineReapsIdleRooms covers the leak of a room whose legs all hung up.
func TestEngineReapsIdleRooms(t *testing.T) {
	rec := &recorder{}
	e := NewEngine(config.MediaConfig{
		MaxLegsPerRoom:  8,
		RoomIdleTimeout: time.Nanosecond,
		LegMaxDuration:  time.Hour,
	}, rec.sink)

	e.CreateRoom("room_idle")
	time.Sleep(2 * time.Millisecond)
	e.reap()

	if e.Room("room_idle") != nil {
		t.Error("idle room was not reaped")
	}
}
