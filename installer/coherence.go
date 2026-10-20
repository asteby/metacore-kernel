package installer

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/asteby/metacore-kernel/bundle"
	"github.com/asteby/metacore-kernel/cachecoherence"
	"github.com/google/uuid"
)

// coherenceBump invalidates every derived cache (statement pool, schema
// introspection, RBAC, metadata, ...) in dependency order and publishes the
// new epoch for (org, addon). It never fails the caller: a cache that could
// not be flushed is reported through cachecoherence's status and logs.
func (i *Installer) coherenceBump(reason string, orgID uuid.UUID, addonKey string) cachecoherence.Event {
	reg := i.Coherence
	if reg == nil {
		reg = cachecoherence.Default
	}
	opts := []cachecoherence.Option{cachecoherence.WithAddon(addonKey)}
	if orgID != uuid.Nil {
		opts = append(opts, cachecoherence.WithOrg(orgID.String()))
	}
	ev, err := reg.BumpContext(context.Background(), reason, opts...)
	if err != nil {
		slog.Warn("installer.coherence_bump_incomplete", "reason", reason,
			"addon_key", addonKey, "org_id", orgID.String(), "err", err.Error())
	}
	return ev
}

// announce flushes every derived cache and only THEN tells clients the
// manifest changed, stamping the event with the new cache epoch. The order is
// the point: a browser reacting to the broadcast refetches immediately, and
// must not be answered from a cache the install was about to invalidate.
func (i *Installer) announce(ctx context.Context, reason string, evt ManifestChangeEvent) {
	ev := i.coherenceBump(reason, evt.OrgID, evt.AddonKey)
	evt.CacheEpoch = ev.Epoch.Token()
	maybeBroadcastManifestChange(ctx, i.Broadcaster, evt)
}

// verifyBackendExports is the post-load check: the module that is now loaded
// must export every function the manifest declares in backend.exports. When it
// does not, the module is recompiled once from the bundle and checked again;
// a persisting mismatch is reported (and fails the install only with
// StrictBackendExports). A loader without the optional interfaces skips it.
func (i *Installer) verifyBackendExports(ctx context.Context, loader BackendRuntimeLoader, b *bundle.Bundle) error {
	v, ok := loader.(BackendRuntimeVerifier)
	if !ok {
		return nil
	}
	missing, err := v.VerifyLoaded(ctx, b)
	if err != nil {
		slog.Warn("installer.backend_verify_failed", "addon_key", b.Manifest.Key, "err", err.Error())
		return nil
	}
	if len(missing) == 0 {
		return nil
	}
	notify := func(outcome string) {
		if n, ok := loader.(BackendDriftNotifier); ok {
			n.NoteDrift(b.Manifest.Key, outcome)
		}
	}
	slog.Warn("installer.backend_export_drift", "addon_key", b.Manifest.Key,
		"version", b.Manifest.Version, "missing_exports", missing, "action", "reloading module")
	if r, ok := loader.(BackendRuntimeReloader); ok {
		if rerr := r.ReloadFromBundle(ctx, b); rerr != nil {
			slog.Warn("installer.backend_reload_failed", "addon_key", b.Manifest.Key, "err", rerr.Error())
		} else if missing, err = v.VerifyLoaded(ctx, b); err == nil && len(missing) == 0 {
			slog.Info("installer.backend_export_drift_cured", "addon_key", b.Manifest.Key)
			notify("reloaded")
			return nil
		}
	}
	notify("persisting")
	slog.Error("installer.backend_export_drift_persists", "addon_key", b.Manifest.Key,
		"missing_exports", missing,
		"hint", "the bundle's backend module does not export what its manifest declares; routes bound to these exports will answer 501")
	if i.StrictBackendExports {
		return fmt.Errorf("backend module of %q lacks declared exports %v", b.Manifest.Key, missing)
	}
	return nil
}
