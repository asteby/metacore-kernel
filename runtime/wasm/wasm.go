// Package wasm is the metacore addon WASM runtime. It compiles modules with
// wazero (pure Go, no cgo) and invokes exported functions under a
// per-invocation sandbox: memory cap, wall-clock timeout, and a host module
// that gates every privileged syscall through security.Capabilities.
//
// Design:
//
//   - Compilation is a once-per-addon cost; CompiledModule caches live on Host.
//   - Instantiation is once per (addon, installation); each installation has
//     its own mutable linear memory, so settings/state bleed is impossible.
//   - Invocation is a plain function call with the ABI documented in abi.go:
//     guest exports `alloc(size i32) i32` + `<fn>(ptr i32, len i32) i64`;
//     the i64 return packs (ptr<<32)|len of a result buffer in guest memory.
//
// The runtime deliberately does NOT expose stdin/stdout/stderr; the only
// observable side-effects are through the imports registered in
// capabilities.go, each of which is policy-checked.
package wasm

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/asteby/metacore-sdk/pkg/manifest"
	"github.com/asteby/metacore-kernel/security"
	"github.com/google/uuid"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// defaults used when BackendSpec leaves a knob at zero.
const (
	defaultMemoryMB = 64
	defaultTimeout  = 10 * time.Second
	wasmPageSize    = 64 * 1024 // 64 KiB — wazero unit for memory caps
)

// Host owns the shared wazero runtime and compiled/instantiated module
// caches. Safe for concurrent use: caches are sync.Maps and wazero's
// runtime API is concurrency-safe.
type Host struct {
	rt       wazero.Runtime
	caps     *security.Capabilities
	logger   *log.Logger
	compiled sync.Map // addonKey -> *compiledEntry
	modules  sync.Map // instanceKey(addonKey, installation) -> *Module
}

type compiledEntry struct {
	mod  wazero.CompiledModule
	spec *manifest.BackendSpec
}

// Module is an instantiated wasm module bound to one installation.
type Module struct {
	inst     api.Module
	spec     *manifest.BackendSpec
	settings map[string]string
	mu       sync.Mutex // serialises Invoke — wazero modules are not reentrant per memory
}

// NewHost constructs a runtime. One Host is expected per process.
func NewHost(ctx context.Context, caps *security.Capabilities, logger *log.Logger) (*Host, error) {
	if logger == nil {
		logger = log.Default()
	}
	// Memory caps in wazero live on the RuntimeConfig — the runtime refuses
	// to grow any module past this global ceiling. Module-level (per-addon)
	// tuning below still applies via the guest's own memory declarations.
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().
		WithCloseOnContextDone(true).
		WithMemoryLimitPages(uint32(256*1024/64))) // 256 MiB hard ceiling
	h := &Host{rt: rt, caps: caps, logger: logger}
	if err := registerHostModule(ctx, h); err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("wasm: register host module: %w", err)
	}
	return h, nil
}

// Load compiles wasmBytes for addonKey and caches the CompiledModule. Calling
// Load again for the same addonKey replaces the prior compile and drops any
// installation instances — use this on addon upgrade.
func (h *Host) Load(ctx context.Context, addonKey string, wasmBytes []byte, spec *manifest.BackendSpec) error {
	if spec == nil || spec.Runtime != "wasm" {
		return fmt.Errorf("wasm: backend spec must have runtime=wasm")
	}
	cm, err := h.rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		return fmt.Errorf("wasm: compile %s: %w", addonKey, err)
	}
	if prev, ok := h.compiled.Swap(addonKey, &compiledEntry{mod: cm, spec: spec}); ok {
		if p, _ := prev.(*compiledEntry); p != nil {
			_ = p.mod.Close(ctx)
		}
		// Drop stale installation instances — they point to the old compile.
		h.modules.Range(func(k, v any) bool {
			if sk, _ := k.(string); len(sk) > len(addonKey) && sk[:len(addonKey)+1] == addonKey+"|" {
				if m, _ := v.(*Module); m != nil {
					_ = m.inst.Close(ctx)
				}
				h.modules.Delete(k)
			}
			return true
		})
	}
	return nil
}

// Invoke calls a single exported function on behalf of installationID. It
// bounds wall-clock time per BackendSpec.TimeoutMs, serialises access to the
// module's memory, and returns the guest's result bytes.
func (h *Host) Invoke(ctx context.Context, installation uuid.UUID, addonKey, funcName string, payload []byte, settings map[string]string) ([]byte, error) {
	ce, ok := h.compiled.Load(addonKey)
	if !ok {
		return nil, fmt.Errorf("wasm: addon %q not loaded", addonKey)
	}
	entry := ce.(*compiledEntry)

	// Enforce the declared export whitelist. Even if a module ships extra
	// symbols, only the ones in BackendSpec.Exports are dispatchable.
	if !containsString(entry.spec.Exports, funcName) {
		return nil, fmt.Errorf("wasm: function %q not in backend.exports", funcName)
	}

	mod, err := h.getOrInstantiate(ctx, addonKey, installation, entry)
	if err != nil {
		return nil, err
	}

	timeout := defaultTimeout
	if entry.spec.TimeoutMs > 0 {
		timeout = time.Duration(entry.spec.TimeoutMs) * time.Millisecond
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// Stash settings + caller id on the ctx so the host module imports
	// (env_get, http_fetch, log) can read them without global state.
	callCtx = withInvocation(callCtx, &invocation{
		addonKey:     addonKey,
		installation: installation,
		settings:     settings,
		caps:         h.caps,
		logger:       h.logger,
	})

	mod.mu.Lock()
	defer mod.mu.Unlock()

	fn := mod.inst.ExportedFunction(funcName)
	if fn == nil {
		return nil, fmt.Errorf("wasm: export %q missing from module", funcName)
	}
	ptr, err := writeMem(callCtx, mod.inst, payload)
	if err != nil {
		return nil, err
	}
	results, err := fn.Call(callCtx, uint64(ptr), uint64(len(payload)))
	if err != nil {
		return nil, fmt.Errorf("wasm: call %s: %w", funcName, err)
	}
	if len(results) != 1 {
		return nil, fmt.Errorf("wasm: %s returned %d values, want 1 (packed ptr|len)", funcName, len(results))
	}
	out, ok := readMem(mod.inst, results[0])
	if !ok {
		return nil, fmt.Errorf("wasm: %s returned invalid ptr/len", funcName)
	}
	// Copy the slice — the guest can free/overwrite the buffer on the next call.
	cp := make([]byte, len(out))
	copy(cp, out)
	return cp, nil
}

func (h *Host) getOrInstantiate(ctx context.Context, addonKey string, installation uuid.UUID, entry *compiledEntry) (*Module, error) {
	key := addonKey + "|" + installation.String()
	if v, ok := h.modules.Load(key); ok {
		return v.(*Module), nil
	}
	// Module-level memory cap is enforced by the runtime's WithMemoryLimitPages
	// ceiling plus the guest's own memory declaration; wazero's ModuleConfig
	// does not expose a per-instance cap. We still record the MB value on the
	// spec so operators can audit what each addon declared.
	_ = entry.spec.MemoryLimitMB

	cfg := wazero.NewModuleConfig().
		WithName(addonKey + "-" + installation.String()).
		// RandSource is required by some toolchains' runtime init (e.g. Go's
		// runtime.fastrand). SysNanotime/Walltime give guests a monotonic +
		// wall clock without stdin/stdout; we still omit stdio on purpose.
		WithRandSource(rand.Reader).
		WithSysNanotime().
		WithSysWalltime()

	inst, err := h.rt.InstantiateModule(ctx, entry.mod, cfg)
	if err != nil {
		return nil, fmt.Errorf("wasm: instantiate %s/%s: %w", addonKey, installation, err)
	}
	mod := &Module{inst: inst, spec: entry.spec}
	if actual, loaded := h.modules.LoadOrStore(key, mod); loaded {
		// Racing caller beat us to it — dispose ours and use theirs.
		_ = inst.Close(ctx)
		return actual.(*Module), nil
	}
	return mod, nil
}

// Close tears down every instance and the wazero runtime. Call once on
// process shutdown.
func (h *Host) Close(ctx context.Context) error {
	h.modules.Range(func(_, v any) bool {
		if m, _ := v.(*Module); m != nil {
			_ = m.inst.Close(ctx)
		}
		return true
	})
	return h.rt.Close(ctx)
}

func containsString(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
