package batch

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// WatchConfig configures a Watcher. Enabled is false when no directory is set.
type WatchConfig struct {
	// Dir is the batch directory; empty means the watcher is off.
	Dir string
	// Poll is the revision period (default 10 minutes).
	Poll time.Duration
	// Timeout bounds one revision (default 10 minutes).
	Timeout time.Duration
}

// Defaults of a WatchConfig.
const (
	DefaultPoll    = 10 * time.Minute
	DefaultTimeout = 10 * time.Minute
)

// Enabled reports whether the watcher should start.
func (c WatchConfig) Enabled() bool { return strings.TrimSpace(c.Dir) != "" }

// ConfigFromEnv reads <PREFIX>_DIR, <PREFIX>_POLL_MINUTES (1-1440) and
// <PREFIX>_TIMEOUT_MINUTES (1-1440), so each vertical picks its own prefix
// ("INTEL_EXPORT"). An invalid value is an error: the host should log it and
// leave the watcher off.
func ConfigFromEnv(prefix string) (WatchConfig, error) {
	cfg := WatchConfig{
		Dir:     strings.TrimSpace(os.Getenv(prefix + "_DIR")),
		Poll:    DefaultPoll,
		Timeout: DefaultTimeout,
	}
	minutes := func(suffix string, dst *time.Duration) error {
		name := prefix + suffix
		v := strings.TrimSpace(os.Getenv(name))
		if v == "" {
			return nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 24*60 {
			return fmt.Errorf("%s: invalid value %q (1-1440)", name, v)
		}
		*dst = time.Duration(n) * time.Minute
		return nil
	}
	if err := minutes("_POLL_MINUTES", &cfg.Poll); err != nil {
		return cfg, err
	}
	if err := minutes("_TIMEOUT_MINUTES", &cfg.Timeout); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// Watcher runs a revision function on a ticker: one revision at a time,
// panics recovered, each bounded by a timeout.
type Watcher struct {
	cfg WatchConfig
	run func(ctx context.Context) (Result, error)
	log *slog.Logger

	mu sync.Mutex // one revision at a time
}

// NewWatcher builds a Watcher around run, usually a closure over Process.
func NewWatcher(cfg WatchConfig, run func(ctx context.Context) (Result, error), log *slog.Logger) *Watcher {
	if cfg.Poll <= 0 {
		cfg.Poll = DefaultPoll
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if log == nil {
		log = slog.Default()
	}
	return &Watcher{cfg: cfg, run: run, log: log.With("component", "batch-watcher")}
}

// RunOnce does one revision now, serialized with the ticker, bounded by the
// timeout. A panic of run comes back as an error.
func (w *Watcher) RunOnce(ctx context.Context) (res Result, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("batch: panic in revision: %v", r)
		}
	}()
	rctx, cancel := context.WithTimeout(ctx, w.cfg.Timeout)
	defer cancel()
	return w.run(rctx)
}

// Start runs a revision right away and then every Poll, until ctx ends or the
// returned stop is called; stop waits for the goroutine (and any revision in
// flight) to finish. A disabled config is a no-op.
func (w *Watcher) Start(parent context.Context) (stop func()) {
	if !w.cfg.Enabled() {
		return func() {}
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.log.Info("batch watcher on", "dir", w.cfg.Dir, "every", w.cfg.Poll.String())
		w.tick(ctx)
		t := time.NewTicker(w.cfg.Poll)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				w.tick(ctx)
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
}

func (w *Watcher) tick(ctx context.Context) {
	res, err := w.RunOnce(ctx)
	switch {
	case err != nil && ctx.Err() == nil:
		w.log.Error("batch revision failed", "error", err.Error(), "manifest", res.ManifestSHA)
	case res.Imported:
		w.log.Info("batch imported", "manifest", res.ManifestSHA, "files", res.Files, "warnings", len(res.Warnings))
	case res.SameContent:
		w.log.Info("new manifest with the files of the last batch: not re-imported", "manifest", res.ManifestSHA)
	}
}
