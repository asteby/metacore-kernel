package staleplan

import (
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
)

// Stats are the process-wide counters of the stale-plan machinery.
type Stats struct {
	// Detected: stale-plan errors seen (retried or not).
	Detected uint64 `json:"detected"`
	// Retried: statements re-run after a reset.
	Retried uint64 `json:"retried"`
	// Recovered: retries that succeeded.
	Recovered uint64 `json:"recovered"`
	// Failed: retries that failed again (any error).
	Failed uint64 `json:"failed"`
	// Skipped: errors not retried (mid-transaction, context done, prepared handle in use).
	Skipped uint64 `json:"skipped"`
	// Resets: times the statement handles were dropped.
	Resets uint64 `json:"resets"`
	// ConnsDeallocated: pooled connections that ran DEALLOCATE ALL on reuse.
	ConnsDeallocated uint64 `json:"conns_deallocated"`
	// TxReplays: whole-transaction replays through RetryTx.
	TxReplays uint64 `json:"tx_replays"`
}

var counters struct {
	detected, retried, recovered, failed, skipped, resets, conns, txReplays atomic.Uint64
}

// GetStats returns a snapshot of the counters.
func GetStats() Stats {
	return Stats{
		Detected:         counters.detected.Load(),
		Retried:          counters.retried.Load(),
		Recovered:        counters.recovered.Load(),
		Failed:           counters.failed.Load(),
		Skipped:          counters.skipped.Load(),
		Resets:           counters.resets.Load(),
		ConnsDeallocated: counters.conns.Load(),
		TxReplays:        counters.txReplays.Load(),
	}
}

// Occurrence is handed to the observer for every detected stale-plan error.
type Occurrence struct {
	Kind Kind
	// Outcome: "recovered", "failed", "skipped".
	Outcome string
	// Op names the call site ("exec", "query", "query_row", "tx_first_stmt", "tx").
	Op  string
	Err error
}

var (
	obsMu    sync.RWMutex
	observer func(Occurrence)
)

// SetObserver installs a callback for every occurrence (nil removes it). Use it
// to forward to a host metrics/alerting system; keep it fast.
func SetObserver(fn func(Occurrence)) {
	obsMu.Lock()
	observer = fn
	obsMu.Unlock()
}

func record(o Occurrence) {
	counters.detected.Add(1)
	switch o.Outcome {
	case "recovered":
		counters.retried.Add(1)
		counters.recovered.Add(1)
		promEvents.WithLabelValues(string(o.Kind), "recovered").Inc()
		slog.Warn("staleplan.recovered", "kind", string(o.Kind), "op", o.Op,
			"hint", "a stale prepared-statement plan was dropped and the statement retried once")
	case "failed":
		counters.retried.Add(1)
		counters.failed.Add(1)
		promEvents.WithLabelValues(string(o.Kind), "failed").Inc()
		slog.Error("staleplan.retry_failed", "kind", string(o.Kind), "op", o.Op, "err", errString(o.Err))
	default:
		counters.skipped.Add(1)
		promEvents.WithLabelValues(string(o.Kind), "skipped").Inc()
		slog.Warn("staleplan.not_retried", "kind", string(o.Kind), "op", o.Op, "err", errString(o.Err))
	}
	obsMu.RLock()
	fn := observer
	obsMu.RUnlock()
	if fn != nil {
		func() {
			defer func() { _ = recover() }()
			fn(o)
		}()
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

var promEvents = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "metacore_staleplan_events_total",
	Help: "Stale prepared-plan errors (SQLSTATE 0A000/26000) by kind and outcome.",
}, []string{"kind", "outcome"})

var promResets = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "metacore_staleplan_resets_total",
	Help: "Times the prepared-statement handles of a pool were dropped.",
})

func init() {
	for _, c := range []prometheus.Collector{promEvents, promResets} {
		if err := prometheus.DefaultRegisterer.Register(c); err != nil {
			var are prometheus.AlreadyRegisteredError
			if !errors.As(err, &are) {
				slog.Warn("staleplan.metrics_register_failed", "err", err.Error())
			}
		}
	}
}
