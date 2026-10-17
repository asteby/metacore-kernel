package wasm

import (
	"errors"
	"time"

	kmetrics "github.com/asteby/metacore-kernel/metrics"
	"github.com/prometheus/client_golang/prometheus"
)

// Eviction reasons (label `reason` of wasm_instance_evictions_total).
const (
	evictTrap      = "trap"         // guest trapped / timed out mid-call
	evictMemory    = "memory_limit" // instance grew past memory_limit_mb
	evictClosed    = "closed"       // wazero closed it (timeout, exit)
	evictUnload    = "unload"       // Host.Unload
	evictReplaced  = "replaced"     // Load of a new binary / stale pool
	evictHostClose = "host_close"   // Host.Close
)

type hostMetrics struct {
	invocations *prometheus.CounterVec
	duration    *prometheus.HistogramVec
	poolWait    *prometheus.HistogramVec
	evictions   *prometheus.CounterVec
	compiles    *prometheus.CounterVec
	compileSecs *prometheus.HistogramVec
	instantiate *prometheus.CounterVec
	instances   *prometheus.GaugeVec
	emitViolate *prometheus.CounterVec
}

func registerOrGet[T prometheus.Collector](reg *prometheus.Registry, c T) T {
	if err := reg.Register(c); err != nil {
		var are prometheus.AlreadyRegisteredError
		if errors.As(err, &are) {
			if ex, ok := are.ExistingCollector.(T); ok {
				return ex
			}
		}
		panic(err)
	}
	return c
}

// newHostMetrics registers (idempotently) the wasm collectors on the kernel
// metrics registry. Two Hosts sharing one registry share the collectors.
func newHostMetrics(r *kmetrics.Registry) *hostMetrics {
	if r == nil || r.Prometheus == nil {
		return nil
	}
	p := r.Prometheus
	return &hostMetrics{
		invocations: registerOrGet(p, prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wasm_invocations_total",
			Help: "Wasm guest invocations by addon and result (ok|error).",
		}, []string{"addon", "result"})),
		duration: registerOrGet(p, prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "wasm_invocation_duration_seconds",
			Help:    "End-to-end invocation latency (pool wait + guest call), by addon.",
			Buckets: prometheus.DefBuckets,
		}, []string{"addon"})),
		poolWait: registerOrGet(p, prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "wasm_pool_wait_seconds",
			Help:    "Time an invocation waited for a free instance (includes cold instantiation), by addon.",
			Buckets: []float64{.0005, .001, .005, .01, .05, .1, .5, 1, 2.5, 5, 10},
		}, []string{"addon"})),
		evictions: registerOrGet(p, prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wasm_instance_evictions_total",
			Help: "Instances closed and discarded, by addon and reason.",
		}, []string{"addon", "reason"})),
		compiles: registerOrGet(p, prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wasm_compilations_total",
			Help: "CompileModule calls (served by the persistent cache when warm), by addon.",
		}, []string{"addon"})),
		compileSecs: registerOrGet(p, prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "wasm_compile_duration_seconds",
			Help:    "CompileModule latency; a cache hit is orders of magnitude faster than a cold compile.",
			Buckets: []float64{.001, .005, .01, .05, .1, .5, 1, 5, 15, 60},
		}, []string{"addon"})),
		instantiate: registerOrGet(p, prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wasm_instantiations_total",
			Help: "Instances created (cold starts), by addon.",
		}, []string{"addon"})),
		instances: registerOrGet(p, prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "wasm_instances",
			Help: "Live instances, by addon.",
		}, []string{"addon"})),
		emitViolate: registerOrGet(p, prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wasm_event_emit_violations_total",
			Help: "event_emit calls that broke the emit policy, by addon, rule and action (warned|denied).",
		}, []string{"addon", "rule", "action"})),
	}
}

func (m *hostMetrics) emitViolation(addon, rule string, denied bool) {
	if m == nil {
		return
	}
	action := "warned"
	if denied {
		action = "denied"
	}
	m.emitViolate.WithLabelValues(addon, rule, action).Inc()
}

func (m *hostMetrics) invoked(addon string, err error, d time.Duration) {
	if m == nil {
		return
	}
	res := "ok"
	if err != nil {
		res = "error"
	}
	m.invocations.WithLabelValues(addon, res).Inc()
	m.duration.WithLabelValues(addon).Observe(d.Seconds())
}

func (m *hostMetrics) waited(addon string, d time.Duration) {
	if m != nil {
		m.poolWait.WithLabelValues(addon).Observe(d.Seconds())
	}
}

func (m *hostMetrics) evicted(addon, reason string) {
	if m != nil {
		m.evictions.WithLabelValues(addon, reason).Inc()
		m.instances.WithLabelValues(addon).Dec()
	}
}

func (m *hostMetrics) compiled(addon string, d time.Duration) {
	if m != nil {
		m.compiles.WithLabelValues(addon).Inc()
		m.compileSecs.WithLabelValues(addon).Observe(d.Seconds())
	}
}

func (m *hostMetrics) instantiated(addon string) {
	if m != nil {
		m.instantiate.WithLabelValues(addon).Inc()
		m.instances.WithLabelValues(addon).Inc()
	}
}
