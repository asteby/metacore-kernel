package wasm

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/dynamic"
)

// SettingsResolver supplies the settings a guest reads through env_get,
// resolved for the invocation's tenant and active branch (the branch carried
// by the context, dynamic.BranchIDFromContext).
//
// base is the legacy map the caller passed to Invoke*; the resolver returns
// the FINAL map the guest will see and decides how to layer over base (see
// settings.WasmResolver for the kernel's implementation). It must not mutate
// base. This is the minimal seam: the wasm package does not import the
// settings store.
type SettingsResolver interface {
	ResolveSettings(ctx context.Context, orgID uuid.UUID, addonKey, branchID string, base map[string]string) (map[string]string, error)
}

// WithSettingsResolver enables scoped settings resolution for every
// invocation that carries an org (InvokeFor / InvokeForInTx). Without it
// (the default) the settings map passed to Invoke* is served as is, exactly as
// before. The map parameter of Invoke* is Deprecated as the sole source of
// settings: it remains the fallback and the base the resolver layers over.
// Passing nil disables the resolver.
func (h *Host) WithSettingsResolver(r SettingsResolver) *Host {
	h.settingsResolver = r
	return h
}

// resolveSettings applies the configured resolver. It is a no-op without a
// resolver or without a tenant (uuid.Nil: legacy callers) and fails open to the
// caller-supplied map: a resolver outage must not take guests down, and the
// legacy map still holds the values until the store is authoritative.
func (h *Host) resolveSettings(ctx context.Context, orgID uuid.UUID, addonKey string, settings map[string]string) map[string]string {
	if h.settingsResolver == nil || orgID == uuid.Nil {
		return settings
	}
	got, err := h.settingsResolver.ResolveSettings(ctx, orgID, addonKey, dynamic.BranchIDFromContext(ctx), settings)
	if err != nil {
		slog.Warn("wasm.settings_resolver_failed", "addon_key", addonKey, "org_id", orgID.String(), "err", err.Error())
		return settings
	}
	return got
}
