package wasm

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// moduleSeq makes every wazero module name unique process-wide: a pool rebuilt
// after Unload/Load must not reuse the name of a still-draining instance.
var moduleSeq atomic.Uint64

// errStaleEntry: the compiled module under an in-flight call was replaced or
// unloaded; invokeImpl re-resolves it and retries.
var errStaleEntry = errors.New("wasm: compiled module superseded")

var errPoolClosed = errors.New("wasm: instance pool closed (addon unloaded or replaced)")

// Module is an instantiated wasm module (one pool slot) bound to one
// installation. A Module is used by ONE invocation at a time: exclusivity
// comes from the pool, not from a per-module lock.
type Module struct {
	inst   api.Module
	closed atomic.Bool
}

// instPool is the bounded set of instances for one (addon, installation).
//
// tokens (capacity = size) is the admission semaphore: holding a token entitles
// the holder to one instance, either popped from idle or created on demand
// (so at most `size` instances ever exist). Guest state (linear memory, Go
// heap, globals) is per instance and is NOT shared across the pool.
type instPool struct {
	host         *Host
	addonKey     string
	installation uuid.UUID
	entry        *compiledEntry
	size         int
	tokens       chan struct{}

	mu     sync.Mutex
	idle   []*Module
	closed bool
}

func newInstPool(h *Host, addonKey string, installation uuid.UUID, entry *compiledEntry, size int) *instPool {
	return &instPool{
		host: h, addonKey: addonKey, installation: installation, entry: entry,
		size: size, tokens: make(chan struct{}, size),
	}
}

// acquire blocks until an instance is available, ctx ends, or the pool closes.
func (p *instPool) acquire(ctx context.Context) (*Module, error) {
	select {
	case p.tokens <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		<-p.tokens
		return nil, errPoolClosed
	}
	for len(p.idle) > 0 {
		m := p.idle[len(p.idle)-1]
		p.idle = p.idle[:len(p.idle)-1]
		if !m.inst.IsClosed() {
			p.mu.Unlock()
			return m, nil
		}
		p.host.closeModule(p.addonKey, m, evictClosed)
	}
	p.mu.Unlock()

	m, err := p.instantiate(ctx)
	if err != nil {
		<-p.tokens
		return nil, err
	}
	return m, nil
}

// release returns a healthy instance to the pool (or discards it if the pool
// closed or wazero closed the instance meanwhile).
func (p *instPool) release(m *Module) {
	p.mu.Lock()
	if !p.closed && !m.inst.IsClosed() {
		p.idle = append(p.idle, m)
		p.mu.Unlock()
		<-p.tokens
		return
	}
	closed := p.closed
	p.mu.Unlock()
	reason := evictClosed
	if closed {
		reason = evictUnload
	}
	p.host.closeModule(p.addonKey, m, reason)
	<-p.tokens
}

// evict discards a poisoned instance (trap, over memory) and frees its slot.
func (p *instPool) evict(m *Module, reason string) {
	p.host.closeModule(p.addonKey, m, reason)
	<-p.tokens
}

// close shuts the pool: idle instances close now, busy ones when released.
func (p *instPool) close(reason string) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	idle := p.idle
	p.idle = nil
	p.mu.Unlock()
	for _, m := range idle {
		p.host.closeModule(p.addonKey, m, reason)
	}
}

// stats reports (idle, busy) instance counts.
func (p *instPool) stats() (idle, busy int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.idle), len(p.tokens)
}

func (p *instPool) instantiate(ctx context.Context) (*Module, error) {
	// Per-addon memory_limit_mb is hard-enforced by the runtime this module
	// was compiled on (see runtimeFor); invokeOnce keeps a post-call check as
	// a backstop.
	cfg := wazero.NewModuleConfig().
		WithName(fmt.Sprintf("%s-%s-%d", p.addonKey, p.installation, moduleSeq.Add(1))).
		// RandSource is required by some toolchains' runtime init (e.g. Go's
		// runtime.fastrand). SysNanotime/Walltime give guests a monotonic +
		// wall clock without stdin/stdout; we still omit stdio on purpose.
		WithRandSource(rand.Reader).
		WithSysNanotime().
		WithSysWalltime().
		// Addon guests are WASI REACTORS (GOOS=wasip1 -buildmode=c-shared):
		// they export `_initialize`, not `_start`. List both: wazero invokes
		// whichever the module actually exports.
		WithStartFunctions("_initialize", "_start")

	inst, err := p.entry.rt.InstantiateModule(ctx, p.entry.mod, cfg)
	if err != nil {
		// The compile this pool is bound to was unloaded/replaced while we
		// were instantiating: not a guest fault, the caller re-resolves.
		if cur, ok := p.host.compiled.Load(p.addonKey); !ok || cur.(*compiledEntry) != p.entry {
			return nil, errStaleEntry
		}
		return nil, fmt.Errorf("wasm: instantiate %s/%s: %w", p.addonKey, p.installation, err)
	}
	p.host.metrics.instantiated(p.addonKey)
	return &Module{inst: inst}, nil
}

// closeModule closes m exactly once and records the eviction.
func (h *Host) closeModule(addonKey string, m *Module, reason string) {
	if m == nil || !m.closed.CompareAndSwap(false, true) {
		return
	}
	_ = m.inst.Close(context.Background())
	h.metrics.evicted(addonKey, reason)
}
