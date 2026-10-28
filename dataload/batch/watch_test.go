package batch

import (
	"context"
	"io"
	"log/slog"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("X_DIR", "")
	cfg, err := ConfigFromEnv("X")
	if err != nil || cfg.Enabled() || cfg.Poll != DefaultPoll || cfg.Timeout != DefaultTimeout {
		t.Fatalf("defaults: %+v %v", cfg, err)
	}
	t.Setenv("X_DIR", " /data/exports ")
	t.Setenv("X_POLL_MINUTES", "3")
	t.Setenv("X_TIMEOUT_MINUTES", "30")
	cfg, err = ConfigFromEnv("X")
	if err != nil || !cfg.Enabled() || cfg.Dir != "/data/exports" || cfg.Poll != 3*time.Minute || cfg.Timeout != 30*time.Minute {
		t.Fatalf("configured: %+v %v", cfg, err)
	}
	for _, bad := range []string{"0", "-1", "1441", "abc", "1.5"} {
		t.Setenv("X_POLL_MINUTES", bad)
		if _, err := ConfigFromEnv("X"); err == nil || !strings.Contains(err.Error(), "X_POLL_MINUTES") {
			t.Errorf("poll %q: err = %v", bad, err)
		}
	}
	t.Setenv("X_POLL_MINUTES", "")
	t.Setenv("X_TIMEOUT_MINUTES", "0")
	if _, err := ConfigFromEnv("X"); err == nil {
		t.Error("timeout 0 accepted")
	}
}

func TestWatcherDisabledIsNoop(t *testing.T) {
	var n atomic.Int32
	w := NewWatcher(WatchConfig{}, func(context.Context) (Result, error) { n.Add(1); return Result{}, nil }, quiet)
	stop := w.Start(context.Background())
	stop()
	if n.Load() != 0 {
		t.Fatal("disabled watcher ran")
	}
}

func TestWatcherStartStopLeavesNoGoroutine(t *testing.T) {
	before := runtime.NumGoroutine()
	var n atomic.Int32
	w := NewWatcher(WatchConfig{Dir: "d", Poll: 5 * time.Millisecond}, func(context.Context) (Result, error) { n.Add(1); return Result{}, nil }, quiet)
	stop := w.Start(context.Background())
	waitFor(t, "3 revisions", func() bool { return n.Load() >= 3 })
	stop()
	stop() // idempotent
	after := n.Load()
	time.Sleep(30 * time.Millisecond)
	if n.Load() != after {
		t.Fatal("revisions kept running after stop")
	}
	waitFor(t, "goroutines to settle", func() bool { return runtime.NumGoroutine() <= before })
}

func TestWatcherStopsWithParentContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var n atomic.Int32
	w := NewWatcher(WatchConfig{Dir: "d", Poll: 5 * time.Millisecond}, func(context.Context) (Result, error) { n.Add(1); return Result{}, nil }, quiet)
	stop := w.Start(ctx)
	waitFor(t, "a revision", func() bool { return n.Load() >= 1 })
	cancel()
	stop() // returns: the goroutine already ended
}

func TestWatcherRecoversPanics(t *testing.T) {
	var n atomic.Int32
	w := NewWatcher(WatchConfig{Dir: "d", Poll: 5 * time.Millisecond}, func(context.Context) (Result, error) {
		if n.Add(1) == 1 {
			panic("boom")
		}
		return Result{}, nil
	}, quiet)
	stop := w.Start(context.Background())
	defer stop()
	waitFor(t, "a revision after the panic", func() bool { return n.Load() >= 3 })

	_, err := NewWatcher(WatchConfig{Dir: "d"}, func(context.Context) (Result, error) { panic("x") }, quiet).RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "panic") {
		t.Fatalf("RunOnce err = %v", err)
	}
}

func TestWatcherRevisionsNeverOverlap(t *testing.T) {
	var inflight, maxInflight, runs atomic.Int32
	w := NewWatcher(WatchConfig{Dir: "d", Poll: time.Millisecond}, func(context.Context) (Result, error) {
		cur := inflight.Add(1)
		for {
			m := maxInflight.Load()
			if cur <= m || maxInflight.CompareAndSwap(m, cur) {
				break
			}
		}
		time.Sleep(8 * time.Millisecond)
		inflight.Add(-1)
		runs.Add(1)
		return Result{}, nil
	}, quiet)
	stop := w.Start(context.Background())
	// manual runs race with the ticker
	for i := 0; i < 5; i++ {
		go func() { _, _ = w.RunOnce(context.Background()) }()
	}
	waitFor(t, "several revisions", func() bool { return runs.Load() >= 6 })
	stop()
	if maxInflight.Load() != 1 {
		t.Fatalf("overlapping revisions: %d", maxInflight.Load())
	}
}

func TestWatcherTimeoutBoundsARevision(t *testing.T) {
	w := NewWatcher(WatchConfig{Dir: "d", Timeout: 20 * time.Millisecond}, func(ctx context.Context) (Result, error) {
		<-ctx.Done()
		return Result{}, ctx.Err()
	}, quiet)
	start := time.Now()
	_, err := w.RunOnce(context.Background())
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("err=%v after %v", err, time.Since(start))
	}
}
