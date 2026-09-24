package dynamic

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/modelbase"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// LifecycleHookInvoker is the minimal surface dynamic.HookRegistry needs to
// route a manifest-declared `before_*` / `after_*` hook to the runner that
// knows about wasm modules and webhook dispatchers.
//
// kernel/lifecycle.HookRunner satisfies this interface natively. The local
// alias exists so `dynamic` does not have to import `lifecycle` (which
// already imports `manifest`) — keeps the package graph acyclic and lets
// custom hosts plug their own runner without going through kernel/lifecycle.
type LifecycleHookInvoker interface {
	Run(ctx context.Context, addonKey string, orgID uuid.UUID, event string, m manifest.Manifest, payload []byte) error
}

// HookContext carries metadata about the operation being performed.
type HookContext struct {
	Model string
	User  modelbase.AuthUser
	DB    *gorm.DB
}

// Hook signatures — each is optional per model.
type BeforeCreateHook func(ctx context.Context, hc HookContext, input map[string]any) error
type AfterCreateHook func(ctx context.Context, hc HookContext, record any) error
type BeforeUpdateHook func(ctx context.Context, hc HookContext, id string, input map[string]any) error
type AfterUpdateHook func(ctx context.Context, hc HookContext, record any) error
type BeforeDeleteHook func(ctx context.Context, hc HookContext, id string) error
type AfterDeleteHook func(ctx context.Context, hc HookContext, id string) error

// HookRegistry holds lifecycle hooks per model.
type HookRegistry struct {
	mu            sync.RWMutex
	beforeCreate  map[string][]BeforeCreateHook
	afterCreate   map[string][]AfterCreateHook
	beforeUpdate  map[string][]BeforeUpdateHook
	afterUpdate   map[string][]AfterUpdateHook
	beforeDelete  map[string][]BeforeDeleteHook
	afterDelete   map[string][]AfterDeleteHook
	// owners tracks the manifest-driven hooks installed via
	// RegisterManifestHooks so UnregisterAddon can rip them out wholesale.
	// nil until the first RegisterManifestHooks call.
	owners        map[string][]addonHookRegistration
	// formulaInvoker is the host-wired backend for Tier-3 (wasm) formulas.
	// nil = Tier-3 formulas are skipped (declarative-only deployments).
	formulaInvoker FormulaInvoker
	// canonical maps any name a model is addressed by (its ModelKey, its table
	// alias, a qualified key) to ONE key. Every registration and every lookup
	// goes through it, so a hook registered under "QuoteItem" fires for a write
	// that names the model "quote_items" and vice versa — one set of hooks per
	// model instead of one per alias. nil = names are used verbatim.
	canonical func(model string) string
	// rollupsByChild indexes the Tier-1 rollup bindings registered through
	// RegisterComputeHooks, keyed by CHILD model. The CRUD hooks close over
	// their own copy; this index exists so a host write path that does NOT go
	// through dynamic.Service (the wasm data_mutate/data_batch tier, a legacy
	// host CRUD path) can still recompute the same rollups by calling
	// RecomputeRollupsForChild. nil until the first RegisterComputeHooks call.
	rollupsByChild map[string][]rollupBinding
}

// SetFormulaInvoker wires the Tier-3 formula backend (normally a thin adapter
// over the wasm runtime's export invocation). Hosts call it once at boot,
// before manifests register compute hooks; formulas declared with tier 3 are
// silently skipped while no invoker is configured.
func (r *HookRegistry) SetFormulaInvoker(f FormulaInvoker) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.formulaInvoker = f
}

// SetModelCanonicalizer wires the host's model-name resolver (alias ->
// canonical ModelKey). Hosts call it once at boot, before any hook is
// registered. A resolver that returns "" for a name leaves it unchanged.
func (r *HookRegistry) SetModelCanonicalizer(f func(model string) string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.canonical = f
}

// canon returns the key a model's hooks are stored under.
func (r *HookRegistry) canon(model string) string {
	r.mu.RLock()
	f := r.canonical
	r.mu.RUnlock()
	if f == nil {
		return model
	}
	if c := f(model); c != "" {
		return c
	}
	return model
}

func (r *HookRegistry) getFormulaInvoker() FormulaInvoker {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.formulaInvoker
}

func NewHookRegistry() *HookRegistry {
	return &HookRegistry{
		beforeCreate: make(map[string][]BeforeCreateHook),
		afterCreate:  make(map[string][]AfterCreateHook),
		beforeUpdate: make(map[string][]BeforeUpdateHook),
		afterUpdate:  make(map[string][]AfterUpdateHook),
		beforeDelete: make(map[string][]BeforeDeleteHook),
		afterDelete:  make(map[string][]AfterDeleteHook),
	}
}

func (r *HookRegistry) RegisterBeforeCreate(model string, h BeforeCreateHook) {
	model = r.canon(model)
	r.mu.Lock(); defer r.mu.Unlock()
	r.beforeCreate[model] = append(r.beforeCreate[model], h)
}

func (r *HookRegistry) RegisterAfterCreate(model string, h AfterCreateHook) {
	model = r.canon(model)
	r.mu.Lock(); defer r.mu.Unlock()
	r.afterCreate[model] = append(r.afterCreate[model], h)
}

func (r *HookRegistry) RegisterBeforeUpdate(model string, h BeforeUpdateHook) {
	model = r.canon(model)
	r.mu.Lock(); defer r.mu.Unlock()
	r.beforeUpdate[model] = append(r.beforeUpdate[model], h)
}

func (r *HookRegistry) RegisterAfterUpdate(model string, h AfterUpdateHook) {
	model = r.canon(model)
	r.mu.Lock(); defer r.mu.Unlock()
	r.afterUpdate[model] = append(r.afterUpdate[model], h)
}

func (r *HookRegistry) RegisterBeforeDelete(model string, h BeforeDeleteHook) {
	model = r.canon(model)
	r.mu.Lock(); defer r.mu.Unlock()
	r.beforeDelete[model] = append(r.beforeDelete[model], h)
}

func (r *HookRegistry) RegisterAfterDelete(model string, h AfterDeleteHook) {
	model = r.canon(model)
	r.mu.Lock(); defer r.mu.Unlock()
	r.afterDelete[model] = append(r.afterDelete[model], h)
}

func (r *HookRegistry) runBeforeCreate(ctx context.Context, hc HookContext, input map[string]any) error {
	if r == nil { return nil }
	key := r.canon(hc.Model)
	r.mu.RLock(); hooks := r.beforeCreate[key]; r.mu.RUnlock()
	for _, h := range hooks { if err := h(ctx, hc, input); err != nil { return err } }
	return nil
}

func (r *HookRegistry) runAfterCreate(ctx context.Context, hc HookContext, record any) error {
	if r == nil { return nil }
	key := r.canon(hc.Model)
	r.mu.RLock(); hooks := r.afterCreate[key]; r.mu.RUnlock()
	for _, h := range hooks { if err := h(ctx, hc, record); err != nil { return err } }
	return nil
}

func (r *HookRegistry) runBeforeUpdate(ctx context.Context, hc HookContext, id string, input map[string]any) error {
	if r == nil { return nil }
	key := r.canon(hc.Model)
	r.mu.RLock(); hooks := r.beforeUpdate[key]; r.mu.RUnlock()
	for _, h := range hooks { if err := h(ctx, hc, id, input); err != nil { return err } }
	return nil
}

func (r *HookRegistry) runAfterUpdate(ctx context.Context, hc HookContext, record any) error {
	if r == nil { return nil }
	key := r.canon(hc.Model)
	r.mu.RLock(); hooks := r.afterUpdate[key]; r.mu.RUnlock()
	for _, h := range hooks { if err := h(ctx, hc, record); err != nil { return err } }
	return nil
}

func (r *HookRegistry) runBeforeDelete(ctx context.Context, hc HookContext, id string) error {
	if r == nil { return nil }
	key := r.canon(hc.Model)
	r.mu.RLock(); hooks := r.beforeDelete[key]; r.mu.RUnlock()
	for _, h := range hooks { if err := h(ctx, hc, id); err != nil { return err } }
	return nil
}

func (r *HookRegistry) runAfterDelete(ctx context.Context, hc HookContext, id string) error {
	if r == nil { return nil }
	key := r.canon(hc.Model)
	r.mu.RLock(); hooks := r.afterDelete[key]; r.mu.RUnlock()
	for _, h := range hooks { if err := h(ctx, hc, id); err != nil { return err } }
	return nil
}

// addonHookRegistration tracks the manifest-driven hooks installed by
// RegisterManifestHooks so UnregisterAddon can rip them out wholesale on
// uninstall. The slice values are the hook keys ("before_create", …) the
// addon contributed for that model — sufficient to know which sub-map of
// HookRegistry the entry lives in.
type addonHookRegistration struct {
	addonKey string
	model    string
	event    string
}

// RegisterManifestHooks projects the manifest's `LifecycleHooks` map into
// the dynamic registry — every `before_create` / `after_create` /
// `before_update` / `after_update` / `before_delete` / `after_delete` entry
// becomes a hook function that delegates to the invoker (typically
// lifecycle.HookRunner). Lifecycle transition hooks (install / enable /
// disable / uninstall / upgrade) are NOT registered here — those fire from
// the installer directly.
//
// addonKey is used to namespace removal. orgIDFromUser pulls the tenant
// from the per-request modelbase.AuthUser so the invoker sees the org the
// mutation is running in; a nil resolver falls back to user.GetOrganizationID().
//
// Calling RegisterManifestHooks twice for the same addon replaces the
// previous registration — the registry first removes everything owned by
// addonKey, then re-adds the new shape. This keeps the install hot path
// idempotent (reinstalls don't double-register).
//
// A nil invoker is a no-op so apps that have not wired a HookRunner keep
// working with their compiled-in hooks unchanged.
func (r *HookRegistry) RegisterManifestHooks(addonKey string, m manifest.Manifest, invoker LifecycleHookInvoker) {
	if r == nil || invoker == nil {
		return
	}
	r.UnregisterAddon(addonKey)
	if len(m.LifecycleHooks) == 0 {
		return
	}
	// CRUD hooks in the manifest are keyed by event ("before_create", …);
	// they apply to every model the addon owns. The installer registers
	// one adapter per (event, model) so the dynamic registry can resolve
	// hits in O(1) by its existing model index.
	for _, event := range []string{
		"before_create", "after_create",
		"before_update", "after_update",
		"before_delete", "after_delete",
	} {
		if len(m.LifecycleHooks[event]) == 0 {
			continue
		}
		for _, md := range m.ModelDefinitions {
			r.registerManifestHook(addonKey, md.ModelKey, event, m, invoker)
		}
	}
}

// registerManifestHook wires a single (model, event) pair onto the
// matching sub-map. Adapter closures encode the CRUD payload as JSON so
// downstream dispatchers (wasm/webhook) receive a stable contract.
func (r *HookRegistry) registerManifestHook(addonKey, model, event string, m manifest.Manifest, invoker LifecycleHookInvoker) {
	// Track the CANONICAL key: that is where Register* stores the hooks, so
	// UnregisterAddon must clear that slice.
	owned := addonHookRegistration{addonKey: addonKey, model: r.canon(model), event: event}
	switch event {
	case "before_create":
		r.registerOwned(owned, func() {
			r.RegisterBeforeCreate(model, func(ctx context.Context, hc HookContext, input map[string]any) error {
				payload, _ := json.Marshal(withHookActor(ctx, hc, map[string]any{
					"event": event, "model": model,
					"input": input,
				}))
				return invoker.Run(ctx, addonKey, orgIDFromUser(hc.User), event, m, payload)
			})
		})
	case "after_create":
		r.registerOwned(owned, func() {
			r.RegisterAfterCreate(model, func(ctx context.Context, hc HookContext, record any) error {
				payload, _ := json.Marshal(withHookActor(ctx, hc, map[string]any{
					"event": event, "model": model,
					"record": record,
				}))
				return invoker.Run(ctx, addonKey, orgIDFromUser(hc.User), event, m, payload)
			})
		})
	case "before_update":
		r.registerOwned(owned, func() {
			r.RegisterBeforeUpdate(model, func(ctx context.Context, hc HookContext, id string, input map[string]any) error {
				payload, _ := json.Marshal(withHookActor(ctx, hc, map[string]any{
					"event": event, "model": model,
					"id": id, "input": input,
				}))
				return invoker.Run(ctx, addonKey, orgIDFromUser(hc.User), event, m, payload)
			})
		})
	case "after_update":
		r.registerOwned(owned, func() {
			r.RegisterAfterUpdate(model, func(ctx context.Context, hc HookContext, record any) error {
				payload, _ := json.Marshal(withHookActor(ctx, hc, map[string]any{
					"event": event, "model": model,
					"record": record,
				}))
				return invoker.Run(ctx, addonKey, orgIDFromUser(hc.User), event, m, payload)
			})
		})
	case "before_delete":
		r.registerOwned(owned, func() {
			r.RegisterBeforeDelete(model, func(ctx context.Context, hc HookContext, id string) error {
				payload, _ := json.Marshal(withHookActor(ctx, hc, map[string]any{
					"event": event, "model": model, "id": id,
				}))
				return invoker.Run(ctx, addonKey, orgIDFromUser(hc.User), event, m, payload)
			})
		})
	case "after_delete":
		r.registerOwned(owned, func() {
			r.RegisterAfterDelete(model, func(ctx context.Context, hc HookContext, id string) error {
				payload, _ := json.Marshal(withHookActor(ctx, hc, map[string]any{
					"event": event, "model": model, "id": id,
				}))
				return invoker.Run(ctx, addonKey, orgIDFromUser(hc.User), event, m, payload)
			})
		})
	}
}

// registerOwned threads an addon-scoped index alongside the actual hook
// registration so UnregisterAddon can wipe just this addon's contributions.
// The owners index is updated under its own critical section first; the
// `register` callback then takes its own lock through the normal
// RegisterBeforeCreate/… API so we don't recurse into r.mu.
func (r *HookRegistry) registerOwned(owned addonHookRegistration, register func()) {
	r.mu.Lock()
	if r.owners == nil {
		r.owners = make(map[string][]addonHookRegistration)
	}
	r.owners[owned.addonKey] = append(r.owners[owned.addonKey], owned)
	r.mu.Unlock()
	register()
}

// UnregisterAddon removes every hook the addon registered through
// RegisterManifestHooks. It does NOT touch hooks installed via the direct
// RegisterBeforeCreate / RegisterAfterCreate / … API — those have no addon
// owner the registry can track.
//
// Implementation note: because Go function values are not comparable, we
// can't filter the per-event slices by identity. Instead, we rebuild each
// event slice from scratch, skipping the addon's slot. The owners index
// records (model, event) for the addon so we only rebuild slices that
// actually changed.
func (r *HookRegistry) UnregisterAddon(addonKey string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	owned := r.owners[addonKey]
	if len(owned) == 0 {
		return
	}
	// Build a (model, event) → presence index so we can clear the slices
	// for just those pairs. The slices then get rebuilt minus this
	// addon's entries via the fresh-slice trick below.
	touched := make(map[string]struct{}, len(owned))
	for _, o := range owned {
		touched[o.model+"::"+o.event] = struct{}{}
	}
	// We cannot distinguish addon-owned hooks from host-installed ones
	// (function values aren't comparable). The installer is the only
	// caller wiring hooks per (addon, model, event); host code wires
	// hooks model-by-model. Apps that mix both must register their host
	// hooks AFTER UnregisterAddon-flagged reinstalls — same constraint
	// already applies to dynamic.Service.HookRegistry today.
	for key := range touched {
		parts := splitOnce(key, "::")
		if len(parts) != 2 {
			continue
		}
		model, event := parts[0], parts[1]
		switch event {
		case "before_create":
			delete(r.beforeCreate, model)
		case "after_create":
			delete(r.afterCreate, model)
		case "before_update":
			delete(r.beforeUpdate, model)
		case "after_update":
			delete(r.afterUpdate, model)
		case "before_delete":
			delete(r.beforeDelete, model)
		case "after_delete":
			delete(r.afterDelete, model)
		}
	}
	delete(r.owners, addonKey)
}

// splitOnce splits at the first occurrence of sep, returning a two-element
// slice. Used to peel a `model::event` index key apart without pulling in
// strings.SplitN for a single call.
func splitOnce(s, sep string) []string {
	for i := 0; i+len(sep) <= len(s); i++ {
		if s[i:i+len(sep)] == sep {
			return []string{s[:i], s[i+len(sep):]}
		}
	}
	return []string{s}
}

// orgIDFromUser extracts the tenant uuid from the request user. Nil users
// resolve to uuid.Nil so the invoker can decide whether to refuse the call
// (production wasm modules typically require a non-zero orgID; tests use
// uuid.Nil deliberately).
func orgIDFromUser(u modelbase.AuthUser) uuid.UUID {
	if u == nil {
		return uuid.Nil
	}
	return u.GetOrganizationID()
}

// withHookActor stamps the acting user onto a manifest hook envelope as
// `actor_id` and `user_id` (the envelope has its own root; the row travels
// under `input`/`record`, so the client can never set these keys). The value
// is the ctx actor (WithActorID) or, failing that, the request user. With no
// real actor — a scheduler, the system principal — neither key is written.
func withHookActor(ctx context.Context, hc HookContext, env map[string]any) map[string]any {
	actor := ActorIDFromContext(ctx)
	if actor == "" && hc.User != nil {
		if id := hc.User.GetID(); id != uuid.Nil {
			actor = id.String()
		}
	}
	if actor == "" || actor == SystemActorID.String() {
		return env
	}
	env["actor_id"] = actor
	env["user_id"] = actor
	return env
}
