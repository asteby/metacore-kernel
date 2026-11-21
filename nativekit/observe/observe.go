// Package observe is the single observation point for what native modules and
// compiled actions do: one Sample per invocation, fanned out to any number of
// sinks (Prometheus metrics, a tracer, the host's activity log).
//
// Stable contract (names and labels are API):
//
//		metacore_action_total{kind,module,action,outcome}        counter
//		metacore_action_duration_seconds{kind,module,action}     histogram
//
//	  - kind:    "compiled" (an addon's compiled handler) or "native" (an
//	    operation of a compiled-in module).
//	  - module:  the addon / module key. Bounded by what is installed.
//	  - action:  the handler symbol or operation name. Bounded by what manifests
//	    declare: never put ids, org ids, user ids or free text here.
//	  - outcome: ok | declined | not_installed | invalid | forbidden | error.
//
// Per-organization or per-record data is deliberately NOT a label (cardinality);
// it travels on the Sample for sinks that are not metrics (tracing, audit).
//
// Auditing: this package does not write an activity log. AuditSink hands each
// Sample to a host function (OPS wires it to addonbundle.Audit), so there is one
// audit writer and one place that decides what is recorded.
package observe

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/nativekit"
)

// Kind separates the two families of invocation.
type Kind string

const (
	KindCompiled Kind = "compiled"
	KindNative   Kind = "native"
)

// Outcome is the low-cardinality result of an invocation.
type Outcome string

const (
	OutcomeOK           Outcome = "ok"
	OutcomeDeclined     Outcome = "declined"
	OutcomeNotInstalled Outcome = "not_installed"
	OutcomeInvalid      Outcome = "invalid"
	OutcomeForbidden    Outcome = "forbidden"
	OutcomeError        Outcome = "error"
)

// ErrDeclined marks a business refusal that is not a fault of the system. Wrap
// it (errors.Is) or return typed nativekit errors.
var ErrDeclined = errors.New("observe: declined")

// ErrNotInstalled marks an invocation refused because the module is not
// installed for the organization; hosts wrap their own sentinel with it.
var ErrNotInstalled = errors.New("observe: not installed")

// Classify maps an error to an Outcome. nil is ok; typed nativekit errors map by
// code (a business rule saying no is "declined", not "error"); anything else is
// error.
func Classify(err error) Outcome {
	if err == nil {
		return OutcomeOK
	}
	if errors.Is(err, ErrNotInstalled) {
		return OutcomeNotInstalled
	}
	if errors.Is(err, ErrDeclined) {
		return OutcomeDeclined
	}
	switch nativekit.CodeOf(err) {
	case "", nativekit.CodeInternal:
		return OutcomeError
	case nativekit.CodeForbidden:
		return OutcomeForbidden
	case nativekit.CodeInvalid:
		return OutcomeInvalid
	}
	return OutcomeDeclined
}

// Sample is one finished invocation.
type Sample struct {
	Kind     Kind
	Module   string
	Action   string
	Outcome  Outcome
	Duration time.Duration
	// Org and Actor are for non-metric sinks only.
	Org   uuid.UUID
	Actor string
	Err   error
}

// Sink receives every Sample. It must not block: a slow sink slows every
// invocation. The Observer recovers a sink's panic.
type Sink func(ctx context.Context, s Sample)

// Observer fans Samples out to its sinks. nil is a valid no-op Observer, so
// instrumentation never needs a nil check at call sites.
type Observer struct {
	mu    sync.RWMutex
	sinks []Sink
}

// New returns an Observer with the given sinks.
func New(sinks ...Sink) *Observer { return &Observer{sinks: sinks} }

// Add registers another sink.
func (o *Observer) Add(s Sink) {
	if o == nil || s == nil {
		return
	}
	o.mu.Lock()
	o.sinks = append(o.sinks, s)
	o.mu.Unlock()
}

// Record delivers a finished Sample to every sink.
func (o *Observer) Record(ctx context.Context, s Sample) {
	if o == nil {
		return
	}
	o.mu.RLock()
	sinks := o.sinks
	o.mu.RUnlock()
	for _, sink := range sinks {
		func() {
			defer func() { _ = recover() }()
			sink(ctx, s)
		}()
	}
}

// Span measures one invocation. Start it, then End it.
type Span struct {
	o      *Observer
	s      Sample
	start  time.Time
	closed bool
}

// Start begins measuring. module and action must be low-cardinality.
func (o *Observer) Start(kind Kind, module, action string) *Span {
	return &Span{o: o, s: Sample{Kind: kind, Module: module, Action: action}, start: time.Now()}
}

// With attaches org and actor (never metric labels).
func (sp *Span) With(org uuid.UUID, actor string) *Span {
	sp.s.Org, sp.s.Actor = org, actor
	return sp
}

// End records the Sample once; err is classified with Classify. It returns err
// unchanged so it can wrap a return: return span.End(ctx, err).
func (sp *Span) End(ctx context.Context, err error) error {
	if sp == nil || sp.closed {
		return err
	}
	sp.closed = true
	s := sp.s
	s.Outcome, s.Duration, s.Err = Classify(err), time.Since(sp.start), err
	sp.o.Record(ctx, s)
	return err
}

// Track runs fn as one observed invocation.
func Track[T any](ctx context.Context, o *Observer, kind Kind, module, action string, fn func() (T, error)) (T, error) {
	sp := o.Start(kind, module, action)
	v, err := fn()
	return v, sp.End(ctx, err)
}
