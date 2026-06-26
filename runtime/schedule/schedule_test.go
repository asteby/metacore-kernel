package schedule

import (
	"context"
	"io"
	"log"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeTicker is a manually driven Ticker.
type fakeTicker struct {
	ch      chan time.Time
	stopped bool
}

func (t *fakeTicker) Chan() <-chan time.Time { return t.ch }
func (t *fakeTicker) Stop()                   { t.stopped = true }

// fakeClock hands out fakeTickers the test drives with tickAll.
type fakeClock struct {
	mu      sync.Mutex
	tickers []*fakeTicker
}

func (c *fakeClock) NewTicker(time.Duration) Ticker {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTicker{ch: make(chan time.Time, 1)}
	c.tickers = append(c.tickers, t)
	return t
}

// tickAll delivers one tick to every live (non-stopped) ticker.
func (c *fakeClock) tickAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, t := range c.tickers {
		if !t.stopped {
			t.ch <- time.Now()
		}
	}
}

func (c *fakeClock) liveTickers() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, t := range c.tickers {
		if !t.stopped {
			n++
		}
	}
	return n
}

// recordingDispatcher counts fires and signals each one on a channel.
type recordingDispatcher struct {
	mu      sync.Mutex
	targets []string
	fired   chan struct{}
}

func newRecordingDispatcher() *recordingDispatcher {
	return &recordingDispatcher{fired: make(chan struct{}, 16)}
}

func (d *recordingDispatcher) Dispatch(_ context.Context, _ uuid.UUID, target string, _ map[string]any) error {
	d.mu.Lock()
	d.targets = append(d.targets, target)
	d.mu.Unlock()
	d.fired <- struct{}{}
	return nil
}

func (d *recordingDispatcher) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.targets)
}

// waitFire blocks until n fires arrive or the timeout elapses.
func waitFire(t *testing.T, d *recordingDispatcher, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-d.fired:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for fire %d/%d", i+1, n)
		}
	}
}

func quietLogger() *log.Logger { return log.New(io.Discard, "", 0) }

func TestSchedulerFiresDoOnTick(t *testing.T) {
	clock := &fakeClock{}
	disp := newRecordingDispatcher()
	s := New(clock, map[string]Dispatcher{"wasm": disp}, quietLogger())
	org := uuid.New()

	if err := s.Register(org, Schedule{Key: "sync", Every: "5m", Do: "wasm:sync_pull"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	s.Start()
	defer s.Stop()

	clock.tickAll()
	waitFire(t, disp, 1)
	if disp.count() != 1 {
		t.Fatalf("want 1 fire, got %d", disp.count())
	}
	if disp.targets[0] != "sync_pull" {
		t.Fatalf("want target sync_pull, got %q", disp.targets[0])
	}

	clock.tickAll()
	waitFire(t, disp, 1)
	if disp.count() != 2 {
		t.Fatalf("want 2 fires after second tick, got %d", disp.count())
	}
}

func TestSchedulerReregisterIsIdempotent(t *testing.T) {
	clock := &fakeClock{}
	disp := newRecordingDispatcher()
	s := New(clock, map[string]Dispatcher{"wasm": disp}, quietLogger())
	org := uuid.New()

	sched := Schedule{Key: "sync", Every: "5m", Do: "wasm:sync_pull"}
	if err := s.Register(org, sched); err != nil {
		t.Fatalf("Register 1: %v", err)
	}
	s.Start()
	defer s.Stop()
	// Re-register the same (org, key) — simulates a boot/install loop re-run.
	if err := s.Register(org, sched); err != nil {
		t.Fatalf("Register 2: %v", err)
	}

	if got := clock.liveTickers(); got != 1 {
		t.Fatalf("re-register must not duplicate tickers: want 1 live, got %d", got)
	}
	clock.tickAll()
	waitFire(t, disp, 1)
	// Give any erroneous second goroutine a moment to (wrongly) fire.
	time.Sleep(50 * time.Millisecond)
	if disp.count() != 1 {
		t.Fatalf("re-register duplicated fires: want 1, got %d", disp.count())
	}
}

func TestSchedulerInvalidEvery(t *testing.T) {
	s := New(nil, map[string]Dispatcher{"wasm": newRecordingDispatcher()}, quietLogger())
	cases := []string{"", "banana", "0s", "-5m"}
	for _, every := range cases {
		if err := s.Register(uuid.New(), Schedule{Key: "k", Every: every, Do: "wasm:x"}); err == nil {
			t.Fatalf("Register with every=%q must error", every)
		}
	}
}

func TestSchedulerUnknownDispatcherPrefixRejected(t *testing.T) {
	s := New(nil, map[string]Dispatcher{"wasm": newRecordingDispatcher()}, quietLogger())
	if err := s.Register(uuid.New(), Schedule{Key: "k", Every: "1m", Do: "compiled:x"}); err == nil {
		t.Fatal("Register with do whose prefix has no dispatcher must error")
	}
	if err := s.Register(uuid.New(), Schedule{Key: "k", Every: "1m", Do: "no_prefix"}); err == nil {
		t.Fatal("Register with prefix-less do must error")
	}
}

func TestSchedulerUnregisterStopsFires(t *testing.T) {
	clock := &fakeClock{}
	disp := newRecordingDispatcher()
	s := New(clock, map[string]Dispatcher{"wasm": disp}, quietLogger())
	org := uuid.New()
	if err := s.Register(org, Schedule{Key: "sync", Every: "5m", Do: "wasm:sync_pull"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	s.Start()
	defer s.Stop()
	s.Unregister(org, "sync")
	if got := clock.liveTickers(); got != 0 {
		t.Fatalf("Unregister must stop the ticker: want 0 live, got %d", got)
	}
}

func TestSchedulerStartRegisterAfterStart(t *testing.T) {
	clock := &fakeClock{}
	disp := newRecordingDispatcher()
	s := New(clock, map[string]Dispatcher{"wasm": disp}, quietLogger())
	s.Start()
	defer s.Stop()
	// Registering after Start spins the job immediately.
	if err := s.Register(uuid.New(), Schedule{Key: "late", Every: "5m", Do: "wasm:x"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	clock.tickAll()
	waitFire(t, disp, 1)
}
