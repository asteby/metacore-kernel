package observe

import (
	"context"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
)

// PrometheusSink registers the stable metrics on reg (registering twice is
// tolerated: the existing collectors are reused) and returns the Sink that
// feeds them.
func PrometheusSink(reg prometheus.Registerer) Sink {
	total := register(reg, prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "metacore_action_total",
		Help: "Invocations of compiled actions and native module operations, by kind, module, action and outcome.",
	}, []string{"kind", "module", "action", "outcome"}))
	dur := register(reg, prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "metacore_action_duration_seconds",
		Help:    "Duration of compiled actions and native module operations.",
		Buckets: prometheus.DefBuckets,
	}, []string{"kind", "module", "action"}))
	return func(_ context.Context, s Sample) {
		total.WithLabelValues(string(s.Kind), s.Module, s.Action, string(s.Outcome)).Inc()
		dur.WithLabelValues(string(s.Kind), s.Module, s.Action).Observe(s.Duration.Seconds())
	}
}

func register[C prometheus.Collector](reg prometheus.Registerer, c C) C {
	if err := reg.Register(c); err != nil {
		if are, ok := err.(prometheus.AlreadyRegisteredError); ok {
			if existing, ok := are.ExistingCollector.(C); ok {
				return existing
			}
		}
		panic(err)
	}
	return c
}

// AuditRecord is what AuditSink hands the host's activity-log writer.
type AuditRecord struct {
	Org     uuid.UUID
	Actor   string
	Kind    Kind
	Module  string
	Action  string
	Outcome Outcome
}

// AuditSink forwards the Samples that pass want to fn. The host decides what is
// worth recording (typically mutations of declared actions, not reads) and owns
// the writer: in OPS fn calls addonbundle.Audit, so the kernel never writes a
// second audit trail. A nil want records everything.
func AuditSink(want func(Sample) bool, fn func(ctx context.Context, r AuditRecord)) Sink {
	return func(ctx context.Context, s Sample) {
		if want != nil && !want(s) {
			return
		}
		fn(ctx, AuditRecord{Org: s.Org, Actor: s.Actor, Kind: s.Kind, Module: s.Module, Action: s.Action, Outcome: s.Outcome})
	}
}
