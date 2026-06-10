package dispatch

import (
	"log/slog"

	"github.com/asteby/metacore-kernel/security"
)

// DefaultTableName is the delivery ledger's destination table. Overridable via
// WithTableName for hosts that keep a historical name.
const DefaultTableName = "event_deliveries"

// defaultMaxAttempts is the number of invocation attempts (including the first)
// before a delivery is marked dead. Bounded so a permanently broken subscriber
// cannot retry forever.
const defaultMaxAttempts = 3

// Options configures a Dispatcher. Construct via With* functional options
// passed to Wire.
type Options struct {
	tableName   string
	maxAttempts int
	capability  CapabilityChecker
	compiled    CompiledRegistry
	logger      *slog.Logger
	onDelivery  func(DeliveryResult)
}

// DeliveryResult is the terminal outcome of one delivery, surfaced to an
// optional WithOnDelivery observer. It is fired exactly once per delivery,
// after the ledger row reaches a terminal status (delivered | dead). Useful
// for host-side metrics/tracing and for tests to await the async queue.
type DeliveryResult struct {
	DeliveryID string
	AddonKey   string
	Event      string
	Function   string
	Status     string // StatusDelivered | StatusDead
	Attempts   int
	Err        string
}

// Option mutates Options. Applied left-to-right in Wire.
type Option func(*Options)

func defaultOptions() *Options {
	return &Options{
		tableName:   DefaultTableName,
		maxAttempts: defaultMaxAttempts,
		capability:  nil, // permit-all until a host wires a checker
		compiled:    nil, // compiled tier inert until a registry is wired
		logger:      slog.Default(),
	}
}

// WithTableName overrides the delivery ledger table (default "event_deliveries").
func WithTableName(name string) Option {
	return func(o *Options) {
		if name != "" {
			o.tableName = name
		}
	}
}

// WithMaxAttempts sets how many invocation attempts a delivery gets before it
// is marked dead. Values < 1 are clamped to 1 (a single attempt, no retry).
func WithMaxAttempts(n int) Option {
	return func(o *Options) {
		if n < 1 {
			n = 1
		}
		o.maxAttempts = n
	}
}

// WithCapabilityChecker gates every delivery on the addon's own
// `event:subscribe` capability. Strongly recommended in production — without
// it, the dispatcher permits all deliveries (see CapabilityChecker doc).
func WithCapabilityChecker(c CapabilityChecker) Option {
	return func(o *Options) { o.capability = c }
}

// WithCompiledRegistry wires the in-process registry that resolves
// "compiled" subscription handlers. Without it the compiled tier is inert.
func WithCompiledRegistry(r CompiledRegistry) Option {
	return func(o *Options) { o.compiled = r }
}

// WithLogger replaces the slog.Logger (default slog.Default()).
func WithLogger(l *slog.Logger) Option {
	return func(o *Options) {
		if l != nil {
			o.logger = l
		}
	}
}

// WithOnDelivery registers an observer invoked once per delivery after it
// reaches a terminal status (delivered | dead). Runs on the delivery's own
// goroutine — keep it cheap and non-blocking. Intended for metrics/tracing
// and for tests that need to await async completion.
func WithOnDelivery(fn func(DeliveryResult)) Option {
	return func(o *Options) { o.onDelivery = fn }
}

// NewEnforcerChecker adapts a security.Enforcer into a CapabilityChecker so the
// dispatcher reuses the SAME policy gate the wasm db_exec / event_emit imports
// consult. CanDeliver maps to enforcer.CheckCapability(addonKey,
// "event:subscribe", event). A nil enforcer yields a permit-all checker.
func NewEnforcerChecker(e *security.Enforcer) CapabilityChecker {
	if e == nil {
		return CapabilityCheckerFunc(func(string, string) error { return nil })
	}
	return CapabilityCheckerFunc(func(addonKey, event string) error {
		return e.CheckCapability(addonKey, "event:subscribe", event)
	})
}
