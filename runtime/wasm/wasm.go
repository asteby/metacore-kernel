// Package wasm is the metacore addon WASM runtime. It compiles modules with
// wazero (pure Go, no cgo) and invokes exported functions under a
// per-invocation sandbox: memory cap, wall-clock timeout, and a host module
// that gates every privileged syscall through security.Capabilities.
//
// Design:
//
//   - Compilation is a once-per-addon cost; CompiledModule caches live on Host.
//   - Instantiation is per (addon, installation) through a bounded pool
//     (default size 1); every instance has its own mutable linear memory, so
//     settings/state bleed across installations is impossible and guest state
//     is never shared between pool instances (docs/wasm-abi.md § 22).
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
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/asteby/metacore-kernel/capability"
	"github.com/asteby/metacore-kernel/connectors"
	"github.com/asteby/metacore-kernel/database/staleplan"
	"github.com/asteby/metacore-kernel/domains"
	"github.com/asteby/metacore-kernel/diag"
	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/events"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/security"
	"github.com/google/uuid"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
	"gorm.io/gorm"
)

// defaults used when BackendSpec leaves a knob at zero.
const (
	defaultMemoryMB = 64
	defaultTimeout  = 10 * time.Second
	// maxGuestHTTPWait caps how much outbound-HTTP time an invocation may add
	// on top of its budget (a few slow third-party round trips).
	maxGuestHTTPWait = 2 * time.Minute
	wasmPageSize     = 64 * 1024 // 64 KiB — wazero unit for memory caps
)

// Host owns the shared wazero runtime and compiled/instantiated module
// caches. Safe for concurrent use: caches are sync.Maps and wazero's
// runtime API is concurrency-safe.
type Host struct {
	// rt is the runtime for the global memory ceiling (256 MiB); runtimes
	// with a tighter per-addon limit live in runtimes (see runtimeFor).
	rt            wazero.Runtime
	cache         wazero.CompilationCache
	runtimes      sync.Map // uint32 memory-limit pages -> *runtimeSlot
	poolSize      int
	poolSizeFn    func(addonKey string) int
	poolMemMB     int
	metrics       *hostMetrics
	caps          *security.Capabilities
	addonCaps     func(addonKey string) *security.Capabilities
	bus           *events.Bus
	// settingsResolver, when set, supplies the per-invocation settings (see
	// SettingsResolver). nil keeps the caller-passed map as the only source.
	settingsResolver SettingsResolver
	// diag, when set, captures a sanitized diagnostic of every failed
	// invocation (see WithDiagnostics). nil = no capture, as before.
	diag *diag.Recorder
	db            *gorm.DB
	enforcer      *security.Enforcer
	tableResolver func(table string) string
	// modelOwner resolves the addon that OWNS a ModelKey for canonical-event
	// namespacing on data_mutate / data_batch (mirrors dynamic.Service's
	// AddonKeyForModel). When unset, events stay under the CALLER addon.
	modelOwner      func(model string) string
	execSchema      func(addonKey string) string
	sequenceNext    func(ctx context.Context, orgID uuid.UUID, model, key string) (string, error)
	routingTable    RoutingTableFn
	sequenceStamp   SequenceStampFn
	createCheck     CreateCheckFn
	createStamp     CreateStampFn
	updateStamp     UpdateStampFn
	ctxProvider     ContextProviderFn
	mutationGuard   func(ctx context.Context, logicalTable string, row map[string]any) error
	mutationCompute MutationComputeFn
	// appendOnly reports a LOGICAL table as an append-only ledger
	// (Host.WithAppendOnly): update/delete on it is refused.
	appendOnly func(logicalTable string) bool
	// stageMachine resolves the declarative lifecycle for a logical table /
	// ModelKey (Host.WithStageMachine). data_mutate and data_batch refuse a
	// write that is not a declared transition. nil = no machine on this tier.
	stageMachine StageMachineFn
	// extensions resolves 1:1 extension tables (Host.WithExtensions) so
	// data_mutate / data_query can read and write "<Ext>.<column>".
	extensions ExtensionFn
	// approvalRequester is the embedder-injected dynamic.Service.RequestApproval
	// the `approval_request` import calls (Host.WithApprovals). nil = import
	// answers `approvals_unavailable`.
	approvalRequester func(ctx context.Context, in dynamic.ApprovalInput) (*dynamic.ApprovalRequest, error)
	// approvalConsumer backs the `approval_consume` import (Host.WithApprovalConsumer).
	approvalConsumer func(ctx context.Context, in dynamic.ConsumeInput) error
	connectors       *connectors.Resolver
	capQuerier       capability.Querier
	dnsResolver      domains.DNSResolver
	dnsAuthorize     DNSAuthorizeFunc
	dnsLimits        *dnsOrgLimiter
	edgeCommander    EdgeCommander
	edgeLimits       *edgeLimiter
	emitPolicy       EmitPolicyFunc
	logger           *log.Logger
	compiled         sync.Map // addonKey -> *compiledEntry
	// modules holds one *instPool per instanceKey(addonKey, installation).
	// The pool's admission semaphore also single-flights instantiation: every
	// instance gets a unique wazero module name, so concurrent cold starts no
	// longer collide on "has already been instantiated".
	modules sync.Map
}

type compiledEntry struct {
	mod wazero.CompiledModule
	// rt is the runtime mod was compiled on; instances MUST be created there.
	rt    wazero.Runtime
	pages uint32 // memory limit of rt
	spec  atomic.Pointer[manifest.BackendSpec]
	// hash of the wasm bytes last compiled for this addonKey. Load skips
	// CompileModule when the payload is unchanged (healer / re-hydrate paths
	// used to recompile identical modules on every boot).
	hash [32]byte
}

const (
	pageBytes      = 64 * 1024
	globalMemoryMB = 256
	globalMemPages = globalMemoryMB * 1024 * 1024 / pageBytes
)

// memoryLimitPages resolves a manifest memory_limit_mb to a wasm page cap:
// 0/negative = the global ceiling; anything above it is clamped to it.
//
// Per-addon enforcement design (hard, inside the call): wazero's
// WithMemoryLimitPages lives on RuntimeConfig, so the Host keeps one runtime
// PER distinct limit (runtimeFor), created lazily and all sharing ONE
// compilation cache — a module compiled for a 16-page runtime reuses the same
// native code on a 1024-page one, so extra runtimes cost no recompile. A guest
// whose memory.grow would pass its limit gets -1 from the engine mid-call
// (Go guests then die with out-of-memory, which traps and evicts the
// instance). invokeOnce still checks the size after the call as a backstop,
// and the wall-clock timeout (guestDeadline + WithCloseOnContextDone) is the
// second barrier against a guest that thrashes instead of growing.
func memoryLimitPages(mb int) uint32 {
	if mb <= 0 || mb >= globalMemoryMB {
		return globalMemPages
	}
	return uint32(mb) * 1024 * 1024 / pageBytes
}

// runtimeSlot lazily builds one runtime (single-flight via once).
type runtimeSlot struct {
	once sync.Once
	rt   wazero.Runtime
	err  error
}

// runtimeFor returns the runtime whose hard memory ceiling is `pages`,
// creating it (WASI + host module registered) on first use. Distinct limits
// are few (manifest MB integers <= 256), so the map stays tiny.
func (h *Host) runtimeFor(pages uint32) (wazero.Runtime, error) {
	v, _ := h.runtimes.LoadOrStore(pages, &runtimeSlot{})
	slot := v.(*runtimeSlot)
	slot.once.Do(func() {
		ctx := context.Background()
		cfg := wazero.NewRuntimeConfig().
			WithCloseOnContextDone(true).
			WithMemoryLimitPages(pages)
		if h.cache != nil {
			cfg = cfg.WithCompilationCache(h.cache)
		}
		rt := wazero.NewRuntimeWithConfig(ctx, cfg)
		// Guests compiled with GOOS=wasip1 (standard Go toolchain) import
		// wasi_snapshot_preview1.* for their runtime initialisation (memory
		// management, clock, random).
		wasi_snapshot_preview1.MustInstantiate(ctx, rt)
		if err := registerHostModule(ctx, h, rt); err != nil {
			_ = rt.Close(ctx)
			slot.err = fmt.Errorf("wasm: register host module: %w", err)
			return
		}
		slot.rt = rt
	})
	return slot.rt, slot.err
}

// NewHost constructs a runtime. One Host is expected per process.
//
// Options (or WASM_COMPILE_CACHE_DIR / WASM_POOL_SIZE) configure the
// persistent compilation cache and the per-installation instance pool.
func NewHost(ctx context.Context, caps *security.Capabilities, logger *log.Logger, opts ...Option) (*Host, error) {
	if logger == nil {
		logger = log.Default()
	}
	cfg := resolveConfig(logger, opts)
	h := &Host{caps: caps, logger: logger, poolSize: cfg.poolSize, poolSizeFn: cfg.poolSizeFn, poolMemMB: cfg.poolMemMB,
		metrics: newHostMetrics(cfg.metrics)}
	h.cache = newCompileCache(cfg.cacheDir, logger)
	// The global-ceiling runtime doubles as the default (h.rt).
	rt, err := h.runtimeFor(globalMemPages)
	if err != nil {
		if h.cache != nil {
			_ = h.cache.Close(ctx)
		}
		return nil, err
	}
	h.rt = rt
	return h, nil
}

// newCompileCache builds the cache shared by every runtime of the Host:
// on-disk when dir is set (shared across processes/replicas on one volume),
// in-memory otherwise (still needed so per-limit runtimes share compiles).
// A directory that cannot be created degrades to memory with a log line —
// the cache is an optimisation, never a boot blocker.
func newCompileCache(dir string, logger *log.Logger) wazero.CompilationCache {
	if dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			logger.Printf("wasm: compile cache dir unusable, falling back to memory: %v", err)
		} else if c, err := wazero.NewCompilationCacheWithDir(dir); err != nil {
			logger.Printf("wasm: compile cache dir unusable, falling back to memory: %v", err)
		} else {
			return c
		}
	}
	return wazero.NewCompilationCache()
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
	// Guests query through db: a prepared plan outdated by an addon migration
	// must cost one retry, not every later call (no-op off Postgres).
	if err := staleplan.Install(db); err != nil && h.logger != nil {
		h.logger.Printf("metacore.wasm WARN staleplan install: %v", err)
	}
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

// WithSequenceStamp injects the embedder's declarative-folio stamper: on every
// data_mutate / data_batch CREATE the host calls it, inside the transaction and
// before the INSERT, so the model's declared sequence-bound columns are filled
// exactly as the dynamic CRUD `POST /data` does. Wire it to
// dynamic.Service.StampSequences. When unset creates are not auto-stamped
// (legacy behaviour). See docs/wasm-abi.md § 14.10.
func (h *Host) WithSequenceStamp(f SequenceStampFn) *Host {
	h.sequenceStamp = f
	return h
}

// WithCreateStamp injects the embedder's declarative stamp phase for
// data_mutate / data_batch creates (docs/wasm-abi.md § 24): snapshot columns
// copied from a related row, supervisor-grant redemption, and the formulas that
// read them. It runs inside the write transaction, after the folio stamp and
// before the create check and the INSERT, and overwrites whatever the guest
// sent for the stamped columns. Wire it to dynamic.Service.CreateStamper. When
// unset, creates are not stamped (previous behaviour).
func (h *Host) WithCreateStamp(f CreateStampFn) *Host {
	h.createStamp = f
	return h
}

// WithUpdateStamp injects the embedder's declarative stamp phase for
// data_mutate / data_batch UPDATES (docs/wasm-abi.md § 24): snapshots are
// re-taken when their source column changes, and a change to anything a
// supervisor grant covered drops the authorization (a new grant id in the patch
// is redeemed on the write's transaction). Wire it to
// dynamic.Service.UpdateStamper. When unset, updates are not stamped.
func (h *Host) WithUpdateStamp(f UpdateStampFn) *Host {
	h.updateStamp = f
	return h
}

// WithCreateCheck injects the embedder's pre-INSERT check for data_mutate /
// data_batch creates. It runs inside the write transaction after the folio
// stamp and before the INSERT; a non-nil error rolls the mutation (or the
// whole batch) back and surfaces to the guest as `invalid_reference` when it
// wraps dynamic.ErrDeletedRef, else `validation_error`. Embedders wire it to
// dynamic.CheckNoDeletedRefs over the model's declared ref columns so a guest
// cannot create a sale line for a soft-deleted product (QA VEN-N08). When
// unset, creates are not checked (previous behaviour).
func (h *Host) WithCreateCheck(f CreateCheckFn) *Host {
	h.createCheck = f
	return h
}

// WithContextProvider injects the embedder's execution-context port for the
// `metacore_host.ctx_get` import (acting user, roles, org config). When unset
// the import answers `context_unavailable`. See docs/wasm-abi.md § 20.
func (h *Host) WithContextProvider(f ContextProviderFn) *Host {
	h.ctxProvider = f
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

// MutationComputeFn is the embedder's declarative-compute pass for the wasm
// write tier. It receives the OPEN TRANSACTION, the LOGICAL table and the row
// the mutation settled on, and is expected to maintain whatever the manifest
// declares over that table — in practice dynamic.HookRegistry's
// RecomputeRollupsForChild. Returning an error rolls the mutation back.
type MutationComputeFn func(ctx context.Context, tx *gorm.DB, orgID uuid.UUID, logicalTable, action string, row map[string]any) error

// WithMutationCompute injects the declarative COMPUTE pass for the
// `metacore_host.data_mutate` / `data_batch` imports — the wasm-tier twin of
// the compute hooks dynamic.Service fires on its own CRUD.
//
// Without it a guest write is a blind spot for the compute engine: the child
// row commits and every Tier-1 rollup declared over it keeps a stale value
// (asteby-hq/ops#1403 — a customer's balance_due never dropped when a payment
// lowered the invoice's amount_due, so paying never freed credit). It runs
// after the mutation is applied and after the guard, still INSIDE the
// transaction, so the recompute reads the new child state and commits with it.
//
// Unlike the guard it also runs for DELETES, with the pre-delete row: an
// aggregate must fall when a child disappears, and the row is passed only so
// the parent FK can be resolved (the row is already gone from `tx`, so the
// aggregate excludes it naturally). When unset, no compute runs.
func (h *Host) WithMutationCompute(f MutationComputeFn) *Host {
	h.mutationCompute = f
	return h
}

// WithApprovals injects the embedder's dynamic.Service.RequestApproval for the
// `metacore_host.approval_request` import (docs/wasm-abi.md § 19). A wasm
// handler that decides, on its own logic, that a mutation needs a human
// approver before it runs calls this import instead of mutating directly; the
// kernel parks a pending ApprovalRequest of kind `explicit` and answers the
// guest with its id + status. The import never performs a mutation itself —
// the guest's own `payload.op` is what an ApprovalApplier (registered via
// Service.RegisterApprovalApplier) replays once approved. When unset, the
// import answers `approvals_unavailable`.
func (h *Host) WithApprovals(f func(ctx context.Context, in dynamic.ApprovalInput) (*dynamic.ApprovalRequest, error)) *Host {
	h.approvalRequester = f
	return h
}

// WithApprovalConsumer injects the embedder's dynamic.Service.ConsumeApproval
// for the `metacore_host.approval_consume` import (docs/wasm-abi.md § 23). A
// handler that writes a document line under a supervisor authorization (price
// under the floor, discount over the cap) redeems the PIN grant through it:
// the host checks org, policy, requester, freshness, single use and the
// recorded context in one atomic step. When unset, the import answers
// `approvals_unavailable`.
func (h *Host) WithApprovalConsumer(f func(ctx context.Context, in dynamic.ConsumeInput) error) *Host {
	h.approvalConsumer = f
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
// Load again for the same addonKey with identical bytes is a no-op (keeps
// live installation instances). A different payload replaces the prior
// compile and drops installation instances — use this on addon upgrade.
func (h *Host) Load(ctx context.Context, addonKey string, wasmBytes []byte, spec *manifest.BackendSpec) error {
	if spec == nil || spec.Runtime != "wasm" {
		return fmt.Errorf("wasm: backend spec must have runtime=wasm")
	}
	sum := sha256.Sum256(wasmBytes)
	limit := memoryLimitPages(spec.MemoryLimitMB)
	if prev, ok := h.compiled.Load(addonKey); ok {
		if p, _ := prev.(*compiledEntry); p != nil && p.hash == sum && p.pages == limit {
			// Same artifact and limit — refresh the spec pointer only
			// (exports/timeouts may have been edited without a binary change).
			old := p.spec.Swap(spec)
			if old != nil && old.Concurrency != spec.Concurrency {
				// Pools are sized at creation: rebuild them (lazily, on the next
				// call) so a new concurrency applies without an addon upgrade.
				h.closePools(addonKey, uuid.Nil, evictReplaced)
			}
			// The spec moved but the binary did not: the exact way a manifest
			// gets ahead of its module.
			if m := missingExports(p); len(m) > 0 {
				h.warnDrift(addonKey, m)
			}
			return nil
		}
	}
	rt, err := h.runtimeFor(limit)
	if err != nil {
		return err
	}
	start := time.Now()
	cm, err := rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		return fmt.Errorf("wasm: compile %s: %w", addonKey, h.diagnoseCompile(ctx, addonKey, wasmBytes, limit, err))
	}
	h.metrics.compiled(addonKey, time.Since(start))
	for _, m := range cm.ExportedMemories() {
		if min := m.Min(); min > limit {
			_ = cm.Close(ctx)
			return fmt.Errorf("wasm: %s declares %d initial memory pages, over its memory_limit_mb (%d pages)", addonKey, min, limit)
		}
	}
	entry := &compiledEntry{mod: cm, rt: rt, pages: limit, hash: sum}
	entry.spec.Store(spec)
	if prev, ok := h.compiled.Swap(addonKey, entry); ok {
		// Drop stale installation instances — they point to the old compile.
		h.closePools(addonKey, uuid.Nil, evictReplaced)
		if p, _ := prev.(*compiledEntry); p != nil {
			_ = p.mod.Close(ctx)
		}
	}
	if m := missingExports(entry); len(m) > 0 {
		h.warnDrift(addonKey, m)
	}
	return nil
}

// diagnoseCompile turns the engine's rejection of a module whose declared
// memory exceeds a tight per-addon runtime limit into the memory_limit_mb
// error operators know. Failure path only; the probe compiles on the global
// runtime (cache-shared, so cheap when the bytes were valid).
func (h *Host) diagnoseCompile(ctx context.Context, addonKey string, wasmBytes []byte, limit uint32, orig error) error {
	if limit == globalMemPages {
		return orig
	}
	probe, err := h.rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		return orig
	}
	defer probe.Close(ctx)
	for _, m := range probe.ExportedMemories() {
		if min := m.Min(); min > limit {
			return fmt.Errorf("%s declares %d initial memory pages, over its memory_limit_mb (%d pages)", addonKey, min, limit)
		}
	}
	return orig
}

// closePools closes the instance pools of addonKey (installation == uuid.Nil
// means all of its installations).
func (h *Host) closePools(addonKey string, installation uuid.UUID, reason string) int {
	n := 0
	prefix := addonKey + "|"
	h.modules.Range(func(k, v any) bool {
		sk, _ := k.(string)
		if !strings.HasPrefix(sk, prefix) {
			return true
		}
		if installation != uuid.Nil && sk != instanceKey(addonKey, installation) {
			return true
		}
		if h.modules.CompareAndDelete(k, v) {
			v.(*instPool).close(reason)
			n++
		}
		return true
	})
	return n
}

func instanceKey(addonKey string, installation uuid.UUID) string {
	return addonKey + "|" + installation.String()
}

// Unload releases what the host holds for an addon.
//
//   - installation != uuid.Nil: closes that installation's instances (idle ones
//     now, busy ones as their call returns). The compiled module stays, since
//     other installations of the same addon share it. Call this when ONE
//     installation is uninstalled.
//   - installation == uuid.Nil: closes every installation's instances AND
//     removes the addon from the compiled map (its CompiledModule is closed).
//     Later Invokes fail with AddonNotLoadedError until Load runs again. Call
//     this when the addon's last installation goes away (or on a failed fresh
//     install, see installer.BackendRuntimeUnloader).
//
// Idempotent; unknown addons are a no-op. In-flight calls finish on the
// instance they hold and then discard it.
func (h *Host) Unload(ctx context.Context, addonKey string, installation uuid.UUID) error {
	h.closePools(addonKey, installation, evictUnload)
	if installation == uuid.Nil {
		if prev, ok := h.compiled.LoadAndDelete(addonKey); ok {
			if p, _ := prev.(*compiledEntry); p != nil {
				return p.mod.Close(ctx)
			}
		}
	}
	return nil
}

// UnloadAddon is Unload for the whole addon (all installations).
func (h *Host) UnloadAddon(ctx context.Context, addonKey string) error {
	return h.Unload(ctx, addonKey, uuid.Nil)
}

// poolFor returns the live pool for the installation, creating it (or
// replacing one bound to a superseded compile) as needed.
func (h *Host) poolFor(addonKey string, installation uuid.UUID, entry *compiledEntry) *instPool {
	key := instanceKey(addonKey, installation)
	for {
		if v, ok := h.modules.Load(key); ok {
			p := v.(*instPool)
			if p.entry == entry {
				return p
			}
			// Stale: bound to a replaced compile.
			if h.modules.CompareAndDelete(key, v) {
				p.close(evictReplaced)
			}
			continue
		}
		np := newInstPool(h, addonKey, installation, entry, h.poolSizeFor(addonKey, entry))
		if actual, loaded := h.modules.LoadOrStore(key, np); loaded {
			if actual.(*instPool).entry == entry {
				return actual.(*instPool)
			}
			continue
		}
		return np
	}
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

func (h *Host) invokeImpl(ctx context.Context, tx *gorm.DB, orgID uuid.UUID, installation uuid.UUID, addonKey, funcName string, payload []byte, settings map[string]string) (out []byte, err error) {
	settings = h.resolveSettings(ctx, orgID, addonKey, settings)
	start := time.Now()
	defer func() { h.metrics.invoked(addonKey, err, time.Since(start)) }()
	ctx, finishDiag := h.beginDiag(ctx, orgID, addonKey, funcName, settings)
	defer func() { finishDiag(&out, &err, recover()) }()

	ce, ok := h.compiled.Load(addonKey)
	if !ok {
		return nil, &AddonNotLoadedError{AddonKey: addonKey}
	}
	entry := ce.(*compiledEntry)

	// Enforce the declared export whitelist. Even if a module ships extra
	// symbols, only the ones in BackendSpec.Exports are dispatchable.
	if !containsString(entry.spec.Load().Exports, funcName) {
		return nil, fmt.Errorf("wasm: function %q not in backend.exports", funcName)
	}

	// One retry: an instance closed under us (unload race, or a prior
	// timeout/cancel closing the wazero instance, see sys.ExitError) is
	// discarded by the pool; run once more on a fresh/other instance.
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		out, err := h.invokeOnce(ctx, tx, orgID, installation, addonKey, funcName, payload, settings, entry)
		if err == nil {
			return out, nil
		}
		lastErr = err
		if errors.Is(err, errStaleEntry) || errors.Is(err, errPoolClosed) {
			// Load/Unload raced us: re-resolve (gone => not loaded).
			ce, ok := h.compiled.Load(addonKey)
			if !ok {
				return nil, &AddonNotLoadedError{AddonKey: addonKey}
			}
			entry = ce.(*compiledEntry)
			continue
		}
		if attempt == 0 && isClosedModuleErr(err) {
			continue
		}
		return nil, err
	}
	return nil, lastErr
}

func (h *Host) invokeOnce(ctx context.Context, tx *gorm.DB, orgID uuid.UUID, installation uuid.UUID, addonKey, funcName string, payload []byte, settings map[string]string, entry *compiledEntry) ([]byte, error) {
	spec := entry.spec.Load()
	timeout := defaultTimeout
	if spec.TimeoutMs > 0 {
		timeout = time.Duration(spec.TimeoutMs) * time.Millisecond
	}

	// Wait for a free instance, bounded by the caller's ctx and by one
	// timeout budget (a saturated pool must shed load, not queue forever).
	// The guest budget below starts only AFTER the instance is ours.
	pool := h.poolFor(addonKey, installation, entry)
	waitStart := time.Now()
	waitCtx, waitCancel := context.WithTimeout(ctx, timeout)
	mod, err := pool.acquire(waitCtx)
	waitCancel()
	h.metrics.waited(addonKey, time.Since(waitStart))
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			return nil, &faultError{code: "pool_busy", err: fmt.Errorf("wasm: %s: all %d pooled instances busy for %s", addonKey, pool.size, timeout)}
		}
		return nil, err
	}
	evicted := false
	defer func() {
		if !evicted {
			pool.release(mod)
		}
	}()
	evict := func(reason string) {
		evicted = true
		pool.evict(mod, reason)
	}

	// Outbound HTTP waits do not count against the budget (see
	// guestDeadline); maxGuestHTTPWait bounds the extension.
	callCtx, deadline, cancel := withGuestDeadline(ctx, timeout, timeout+maxGuestHTTPWait)
	defer cancel()
	// Stash settings + caller id on the ctx so the host module imports
	// (env_get, http_fetch, log) can read them without global state.
	callCtx = withInvocation(callCtx, &invocation{
		deadline:          deadline,
		addonKey:          addonKey,
		installation:      installation,
		settings:          settings,
		caps:              h.capsFor(addonKey),
		bus:               h.bus,
		orgID:             orgID,
		db:                h.db,
		tx:                tx,
		enforcer:          h.enforcer,
		resolveTable:      h.tableResolver,
		modelOwner:        h.modelOwner,
		execSchema:        h.execSchema,
		sequenceNext:      h.sequenceNext,
		routingTable:      h.routingTable,
		sequenceStamp:     h.sequenceStamp,
		createCheck:       h.createCheck,
		createStamp:       h.createStamp,
		updateStamp:       h.updateStamp,
		ctxProvider:       h.ctxProvider,
		mutationGuard:     h.mutationGuard,
		mutationCompute:   h.mutationCompute,
		appendOnly:        h.appendOnly,
		stageMachine:      h.stageMachine,
		extensions:        h.extensions,
		approvalRequester: h.approvalRequester,
		approvalConsumer:  h.approvalConsumer,
		connectors:        h.connectors,
		capQuerier:        h.capQuerier,
		dnsResolver:       h.dnsResolver,
		dnsAuthorize:      h.dnsAuthorize,
		dnsLimits:         h.dnsLimits,
		edgeCommander:     h.edgeCommander,
		edgeLimits:        h.edgeLimits,
		emitPolicy:        h.emitPolicy,
		metrics:           h.metrics,
		logger:            h.logger,
		bag:               diagBagFrom(ctx),
	})

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
		// A trap (unreachable, fatalpanic, out-of-bounds, host-import panic)
		// abandons the guest mid-call: its runtime (Go's scheduler/heap) is
		// left half-unwound but wazero keeps the instance open. Serving it
		// again cascades into "unreachable" / "invalid table access" on
		// unrelated exports until restart. Evict so the next Invoke gets a
		// fresh reactor; the failed call itself is NOT retried (not idempotent).
		evict(evictTrap)
		code := "trap"
		switch {
		case ctx.Err() != nil:
			code = "canceled"
		case callCtx.Err() != nil:
			code = "timeout"
		}
		return nil, &faultError{code: code, err: fmt.Errorf("wasm: call %s: %w", funcName, err)}
	}
	if mem := mod.inst.Memory(); mem != nil {
		if limit := entry.pages; mem.Size() > limit*pageBytes {
			evict(evictMemory)
			return nil, &faultError{code: "memory_limit", err: fmt.Errorf("wasm: %s exceeded memory_limit_mb (%d pages > %d)", addonKey, mem.Size()/pageBytes, limit)}
		}
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
// closed (timeout/cancel/exit) and is safe to retry on a fresh module.
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

// Close tears down every instance and the wazero runtime. Call once on
// process shutdown.
// IsCompiled reports whether addonKey's wasm module has been compiled into
// this process's runtime (Load succeeded at least once since boot). This is
// the readiness signal an embedder needs to answer "did the warm-up finish?"
// instead of guessing a fixed sleep: right after a restart, `compiled` is
// empty and every addon action 422s ("module not loaded") until Load catches
// up (server.go's boot sequence loads addons one at a time, ~3-5s each — see
// roadmap item 6, "despliegues sin ventana rota"). It does NOT mean an
// instance exists for any particular org/installation — that's `modules`,
// scoped per (addonKey, installation) and irrelevant to a process-wide
// readiness check.
func (h *Host) IsCompiled(addonKey string) bool {
	_, ok := h.compiled.Load(addonKey)
	return ok
}

// CompiledKeys returns the addon keys currently compiled into this process's
// runtime, in no particular order. Same scope note as IsCompiled: process-wide,
// not per-installation.
func (h *Host) CompiledKeys() []string {
	var keys []string
	h.compiled.Range(func(k, _ any) bool {
		if s, ok := k.(string); ok {
			keys = append(keys, s)
		}
		return true
	})
	return keys
}

func (h *Host) Close(ctx context.Context) error {
	h.modules.Range(func(k, v any) bool {
		if p, _ := v.(*instPool); p != nil {
			p.close(evictHostClose)
		}
		h.modules.Delete(k)
		return true
	})
	var first error
	h.runtimes.Range(func(_, v any) bool {
		if slot, _ := v.(*runtimeSlot); slot != nil && slot.rt != nil {
			if err := slot.rt.Close(ctx); err != nil && first == nil {
				first = err
			}
		}
		return true
	})
	if h.cache != nil {
		if err := h.cache.Close(ctx); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func containsString(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// AddonNotLoadedError reports an invocation of an addon whose module is not in
// the runtime — typically because the boot-time reload after a restart has
// not reached it yet. It implements dispatch.NotReadyError, so event
// deliveries wait for the module instead of dying in the reload window. The
// message is unchanged ("wasm: addon %q not loaded"): hosts match on it.
type AddonNotLoadedError struct{ AddonKey string }

func (e *AddonNotLoadedError) Error() string {
	return fmt.Sprintf("wasm: addon %q not loaded", e.AddonKey)
}

// NotReady marks the error as transient-until-loaded (dispatch.NotReadyError).
func (e *AddonNotLoadedError) NotReady() bool { return true }
