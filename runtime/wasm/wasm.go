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
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/asteby/metacore-kernel/connectors"
	"github.com/asteby/metacore-kernel/events"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/security"
	"github.com/google/uuid"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
	"gorm.io/gorm"
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
	rt            wazero.Runtime
	caps          *security.Capabilities
	addonCaps     func(addonKey string) *security.Capabilities
	bus           *events.Bus
	db            *gorm.DB
	enforcer      *security.Enforcer
	tableResolver func(table string) string
	// modelOwner resolves the addon that OWNS a ModelKey for canonical-event
	// namespacing on data_mutate / data_batch (mirrors dynamic.Service's
	// AddonKeyForModel). When unset, events stay under the CALLER addon.
	modelOwner    func(model string) string
	execSchema    func(addonKey string) string
	sequenceNext  func(ctx context.Context, orgID uuid.UUID, model, key string) (string, error)
	routingTable  RoutingTableFn
	mutationGuard func(ctx context.Context, logicalTable string, row map[string]any) error
	connectors    *connectors.Resolver
	logger        *log.Logger
	compiled      sync.Map // addonKey -> *compiledEntry
	modules       sync.Map // instanceKey(addonKey, installation) -> *Module
	// instMu single-flights getOrInstantiate per instance key. Two concurrent
	// deliveries that both miss the module cache used to race
	// InstantiateModule with the SAME wazero module name — wazero rejects the
	// loser with "has already been instantiated", and since a Go guest's init
	// takes seconds, all the fast retries collided with the still-in-flight
	// winner too: the delivery died and only the healer recovered it.
	instMu sync.Map // instanceKey -> *sync.Mutex
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
	// Guests compiled with GOOS=wasip1 (standard Go toolchain) import
	// wasi_snapshot_preview1.* for their runtime initialisation (memory
	// management, clock, random). Without this the instantiator returns
	// "module[wasi_snapshot_preview1] not instantiated" and the addon
	// never loads. MustInstantiate panics only if the runtime is already
	// closed — safe to call unconditionally at boot.
	wasi_snapshot_preview1.MustInstantiate(ctx, rt)
	h := &Host{rt: rt, caps: caps, logger: logger}
	if err := registerHostModule(ctx, h); err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("wasm: register host module: %w", err)
	}
	return h, nil
}

// WithBus binds an events.Bus to the host so guests can publish through the
// `metacore_host.event_emit` import. When unset, the import returns a
// `bus_unavailable` JSON error to the guest.
func (h *Host) WithBus(b *events.Bus) *Host {
	h.bus = b
	return h
}

// WithDB binds the *gorm.DB the `metacore_host.db_query` import reads
// through. When unset, the import returns a `db_unavailable` JSON error.
// The host opens a fresh transaction per call so it can SET LOCAL
// search_path without leaking state between invocations.
func (h *Host) WithDB(db *gorm.DB) *Host {
	h.db = db
	return h
}

// WithEnforcer wires the policy gate the `metacore_host.db_query` import
// consults before opening the transaction. When unset, db_query falls back
// to search_path scoping alone — acceptable in tests and embedded harness
// setups, but production hosts should always provide an Enforcer.
func (h *Host) WithEnforcer(e *security.Enforcer) *Host {
	h.enforcer = e
	return h
}

// WithTableResolver injects the embedder's logical→physical table resolution
// for the `metacore_host.data_mutate` import. The resolver MUST mirror the
// host application's dynamic CRUD table resolution (in ops: unqualified →
// `public.*`) so the rows data_mutate touches are the same rows the UI
// serves — data_mutate deliberately does NOT use the addon-schema
// search_path that scopes db_exec (docs/wasm-abi.md § 14.3). When unset,
// resolution is the identity function.
func (h *Host) WithTableResolver(r func(table string) string) *Host {
	h.tableResolver = r
	return h
}

// WithModelOwner injects the embedder's ModelKey→owning-addon resolver used
// when data_mutate / data_batch publish post-commit canonical events.
//
// Why: guests often write cross-addon rows they have db:write on (warehouse
// updates customers.SalesReturn on WRR approve). The lifecycle subscribers
// (inventory.on_return_received, fiscal_mexico, …) register on
// `<owner>.<Model>.<action>` — the same namespace dynamic.Service.publishCanonical
// uses for HTTP CRUD. Publishing under the CALLER addon (warehouse.SalesReturn.updated)
// silently starves those handlers. With ModelOwner set, the host attributes
// the event to the model owner; the write is still capability-gated on the
// caller. When unset, behaviour is unchanged (caller namespace).
func (h *Host) WithModelOwner(r func(model string) string) *Host {
	h.modelOwner = r
	return h
}

// WithSequenceNext injects the embedder's folio-sequence backend for the
// `metacore_host.sequence_next` import. The function issues the next formatted
// value for (model, key) in the given org (the embedder wires it to
// dynamic.Service.NextSequence). When unset the import returns a
// `sequence_unavailable` envelope. See docs/wasm-abi.md § 17.
func (h *Host) WithSequenceNext(f func(ctx context.Context, orgID uuid.UUID, model, key string) (string, error)) *Host {
	h.sequenceNext = f
	return h
}

// WithRoutingTable injects the embedder's org routing-table builder for the
// `metacore_host.routing_resolve` import. The function returns the Table built
// from contributions.routes[] of every INSTALLED addon for that org (typically
// via routing.Build + installer.InstalledSet). When unset the import returns a
// `routing_unavailable` envelope. See docs/wasm-abi.md § 18.
func (h *Host) WithRoutingTable(f RoutingTableFn) *Host {
	h.routingTable = f
	return h
}

// WithMutationGuard injects the embedder's declarative-constraints check for
// the `metacore_host.data_mutate` / `data_batch` imports. After each create /
// update is applied (still inside the open transaction), the guard receives
// the LOGICAL table name and the post-mutation row; a non-nil error rolls the
// whole mutation (or batch) back and surfaces to the guest as a
// `constraint_violation` envelope. Deletes are not guarded — guards predicate
// over the row's resulting state, and a deleted row has none. Embedders wire
// this to dynamic.EvalRowConstraints over the model's declared Column
// constraints, so `stock.quantity >= 0` holds on the wasm write path exactly
// as it does on Service.Create/Update. When unset, no guard runs (the
// pre-guards behaviour).
func (h *Host) WithMutationGuard(g func(ctx context.Context, logicalTable string, row map[string]any) error) *Host {
	h.mutationGuard = g
	return h
}

// WithExecSchema injects the schema the `metacore_host.db_exec` import scopes
// bare table names to via `SET LOCAL search_path`. By default db_exec scopes to
// the addon's isolated schema (`addon_<key>`), assuming an embedding host that
// materialises addon data in per-addon schemas. Hosts that keep all addon data
// in a single shared schema (ops: everything in `public`) MUST override this so
// the addon's raw-SQL handlers hit the SAME rows the declarative CRUD /
// data_mutate touch — otherwise db_exec writes land in an empty shadow schema.
//
// This changes ONLY the runtime search_path, NOT the capability gate: the gate
// still authorises writes against `addon_<key>.*` (the addon's declared
// capability), so an explicit cross-schema write (`UPDATE public.users …`) is
// still denied. Bare names stay gated as addon writes but resolve to the
// host-configured schema. When unset, resolution is `AddonSchema(addonKey)`.
func (h *Host) WithExecSchema(fn func(addonKey string) string) *Host {
	h.execSchema = fn
	return h
}

// WithConnectors binds the connectors.Resolver the `metacore_host.connector_get`
// import resolves credentials through. The import is gated by the addon's
// `connector:read <key>` capability and scoped to the invocation's org. When
// unset, connector_get returns a `connector_unavailable` JSON error to the
// guest (the feature-off state).
func (h *Host) WithConnectors(r *connectors.Resolver) *Host {
	h.connectors = r
	return h
}

// WithAddonCaps injects a per-addon capability resolver. The host's global
// policy (the one handed to NewHost / EnableWASM) is a SINGLE static policy
// shared by every addon, so the two imports that read it — connector_get and
// http_request — can only ever be gated by the union of what all installed
// addons declared. That is org-scoped but not addon-scoped: with a permissive
// global policy any installed addon can read any connector the org configured,
// and can fetch any host any other addon declared.
//
// When a resolver is set, each invocation is stamped with the policy it
// returns for the invoked addonKey instead of the global one, so connector_get
// / http_request are gated by THAT addon's own declarations. Returning nil for
// an addon falls back to the global policy (the pre-resolver behaviour) —
// hosts that want fail-closed semantics for an unknown addon should return an
// empty compiled policy (security.Compile(key, nil)) rather than nil.
//
// db:* is unaffected: it gates through the per-addon *security.Enforcer, which
// was already compiled per manifest.
func (h *Host) WithAddonCaps(fn func(addonKey string) *security.Capabilities) *Host {
	h.addonCaps = fn
	return h
}

// capsFor returns the policy an invocation of addonKey runs under: the
// per-addon policy when a resolver is wired and yields one, else the global.
func (h *Host) capsFor(addonKey string) *security.Capabilities {
	if h.addonCaps != nil {
		if c := h.addonCaps(addonKey); c != nil {
			return c
		}
	}
	return h.caps
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
//
// Tenant scope: orgID stays unset on this entry point — callers that need
// `event_emit` (or any future tenant-scoped host import) must use
// InvokeFor / InvokeForInTx instead, otherwise the guest will see a
// `no_active_org` envelope (docs/wasm-abi.md § 12.6).
func (h *Host) Invoke(ctx context.Context, installation uuid.UUID, addonKey, funcName string, payload []byte, settings map[string]string) ([]byte, error) {
	return h.invokeImpl(ctx, nil, uuid.Nil, installation, addonKey, funcName, payload, settings)
}

// InvokeFor is the tenant-aware sibling of Invoke. The orgID is propagated
// to every host import that needs tenant scope (today: `event_emit` —
// see docs/wasm-abi.md § 12.6). Callers running inside an HTTP handler
// should pass the org id they already resolved via `httpx.ExtractOrgID`;
// background workers / cron paths must thread the org id explicitly.
func (h *Host) InvokeFor(ctx context.Context, orgID, installation uuid.UUID, addonKey, funcName string, payload []byte, settings map[string]string) ([]byte, error) {
	return h.invokeImpl(ctx, nil, orgID, installation, addonKey, funcName, payload, settings)
}

// InvokeInTx is the action-handler-aware sibling of Invoke. The tx is the
// open *gorm.DB transaction the action bridge is running inside; the
// `db_exec` host import piggy-backs on it so the guest's writes commit or
// rollback atomically with the action's own row mutations. Passing a nil tx
// is equivalent to plain Invoke — `db_exec` then opens its own short-lived
// transaction via h.db, used in non-action callers and tests.
func (h *Host) InvokeInTx(ctx context.Context, tx *gorm.DB, installation uuid.UUID, addonKey, funcName string, payload []byte, settings map[string]string) ([]byte, error) {
	return h.invokeImpl(ctx, tx, uuid.Nil, installation, addonKey, funcName, payload, settings)
}

// InvokeForInTx combines tenant scope (orgID) and transaction binding (tx).
// Used by the action bridge when the surrounding handler already resolved
// both the org and the open *gorm.DB transaction.
func (h *Host) InvokeForInTx(ctx context.Context, tx *gorm.DB, orgID, installation uuid.UUID, addonKey, funcName string, payload []byte, settings map[string]string) ([]byte, error) {
	return h.invokeImpl(ctx, tx, orgID, installation, addonKey, funcName, payload, settings)
}

func (h *Host) invokeImpl(ctx context.Context, tx *gorm.DB, orgID uuid.UUID, installation uuid.UUID, addonKey, funcName string, payload []byte, settings map[string]string) ([]byte, error) {
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

	// One retry: a prior timeout/cancel closes the wazero instance (see
	// sys.ExitError) but used to leave the dead Module in h.modules. The next
	// UI action then failed instantly with "module closed with context
	// deadline exceeded" until process restart. Evict + reinstantiate once.
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		out, err := h.invokeOnce(ctx, tx, orgID, installation, addonKey, funcName, payload, settings, entry)
		if err == nil {
			return out, nil
		}
		lastErr = err
		if attempt == 0 && isClosedModuleErr(err) {
			h.dropModule(addonKey, installation)
			continue
		}
		return nil, err
	}
	return nil, lastErr
}

func (h *Host) invokeOnce(ctx context.Context, tx *gorm.DB, orgID uuid.UUID, installation uuid.UUID, addonKey, funcName string, payload []byte, settings map[string]string, entry *compiledEntry) ([]byte, error) {
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
		addonKey:      addonKey,
		installation:  installation,
		settings:      settings,
		caps:          h.capsFor(addonKey),
		bus:           h.bus,
		orgID:         orgID,
		db:            h.db,
		tx:            tx,
		enforcer:      h.enforcer,
		resolveTable:  h.tableResolver,
		modelOwner:    h.modelOwner,
		execSchema:    h.execSchema,
		sequenceNext:  h.sequenceNext,
		routingTable:  h.routingTable,
		mutationGuard: h.mutationGuard,
		connectors:    h.connectors,
		logger:        h.logger,
	})

	mod.mu.Lock()
	defer mod.mu.Unlock()

	if mod.inst.IsClosed() {
		return nil, fmt.Errorf("wasm: module closed")
	}

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

// isClosedModuleErr reports whether err means the cached wazero instance was
// closed (timeout/cancel/exit) and is safe to drop + retry on a fresh module.
func isClosedModuleErr(err error) bool {
	if err == nil {
		return false
	}
	var exitErr *sys.ExitError
	if errors.As(err, &exitErr) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "module closed")
}

// dropModule removes a poisoned instance from the cache so the next
// getOrInstantiate builds a fresh reactor. Safe if the key is already gone.
func (h *Host) dropModule(addonKey string, installation uuid.UUID) {
	key := addonKey + "|" + installation.String()
	if v, ok := h.modules.LoadAndDelete(key); ok {
		if m, _ := v.(*Module); m != nil && m.inst != nil {
			_ = m.inst.Close(context.Background())
		}
	}
}

func (h *Host) getOrInstantiate(ctx context.Context, addonKey string, installation uuid.UUID, entry *compiledEntry) (*Module, error) {
	key := addonKey + "|" + installation.String()
	// Single-flight per instance key: only one goroutine instantiates; the
	// rest wait and hit the cache. See instMu on Host for the failure mode
	// this prevents (wazero name collision → dead deliveries).
	muAny, _ := h.instMu.LoadOrStore(key, &sync.Mutex{})
	mu := muAny.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	if v, ok := h.modules.Load(key); ok {
		mod := v.(*Module)
		// A prior Invoke whose callCtx timed out / was cancelled closes the
		// wazero Module in place. Serving it again yields instant
		// "module closed with context deadline exceeded" on alloc.
		if mod.inst != nil && !mod.inst.IsClosed() {
			return mod, nil
		}
		h.modules.Delete(key)
		if mod.inst != nil {
			_ = mod.inst.Close(ctx)
		}
	}
	// Module-level memory cap is enforced by the runtime's WithMemoryLimitPages
	// ceiling plus the guest's own memory declaration; wazero's ModuleConfig
	// does not expose a per-instance cap. We still record the MB value on the
	// spec so operators can audit what each addon declared.
	_ = entry.spec.MemoryLimitMB

	cfg := wazero.NewModuleConfig().
		WithName(addonKey+"-"+installation.String()).
		// RandSource is required by some toolchains' runtime init (e.g. Go's
		// runtime.fastrand). SysNanotime/Walltime give guests a monotonic +
		// wall clock without stdin/stdout; we still omit stdio on purpose.
		WithRandSource(rand.Reader).
		WithSysNanotime().
		WithSysWalltime().
		// Addon guests are WASI REACTORS (GOOS=wasip1 -buildmode=c-shared):
		// they export `_initialize`, not `_start`. wazero's default start list
		// is just "_start", so reactors were instantiated WITHOUT running the
		// Go runtime init — the first host call into `alloc` then died with
		// "out of bounds memory access" (no heap). List both: wazero invokes
		// whichever the module actually exports.
		WithStartFunctions("_initialize", "_start")

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
