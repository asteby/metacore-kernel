// sync.go hosts the helpers that hydrate / drain a Registry from a
// manifest.Manifest. They were promoted from a host-side tool registry
// service when the bridge layer moved into the kernel — see kernel/bridge
// for the host-side glue that calls them.
//
// Rationale: the Registry is a process-global runtime mirror of the addon's
// declared tools, used by /api/metacore/tools/execute to dispatch in O(1).
// The sync helpers wrap the (uninstall-then-install) idempotent pattern so
// re-installing a manifest version doesn't leak stale tools.

package tool

import (
	"fmt"
	"sync"

	"github.com/asteby/metacore-kernel/installer"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/security"
)

// globalRegistry is the singleton callers reach via GlobalRegistry(). Built
// lazily so tests that never touch the kernel pay no allocation.
var (
	globalRegistryOnce sync.Once
	globalRegistry     *Registry
)

// GlobalRegistry returns the process-global Registry, allocating on first
// call. Hosts share a single registry so /tools/execute resolves uniformly
// regardless of which package registered a tool.
func GlobalRegistry() *Registry {
	globalRegistryOnce.Do(func() {
		globalRegistry = NewRegistry()
	})
	return globalRegistry
}

// SyncFromManifest wires every manifest.ToolDef declared by an addon into
// registry as an HTTPDispatcher backed by the caller-provided dispatcher.
// Replace semantics make re-installs idempotent without a separate purge.
//
// Preconditions:
//   - inst.ID, inst.OrganizationID and inst.AddonKey must be set.
//   - dispatcher is the shared *security.WebhookDispatcher the host owns.
//   - registry is typically GlobalRegistry() but a scoped one is fine for tests.
//
// Returns the number of tools registered. A zero-Tool manifest is a no-op
// that still purges any prior tools for the addon (re-install with no
// tools should drain stale rows).
func SyncFromManifest(m manifest.Manifest, inst installer.Installation, dispatcher *security.WebhookDispatcher, registry *Registry) (int, error) {
	if registry == nil {
		return 0, fmt.Errorf("tool.SyncFromManifest: nil registry")
	}
	if dispatcher == nil {
		return 0, fmt.Errorf("tool.SyncFromManifest: nil dispatcher")
	}
	if m.Key == "" {
		return 0, fmt.Errorf("tool.SyncFromManifest: manifest missing Key")
	}

	// Re-installs must not carry stale tool rows into the new manifest.
	registry.UnregisterAddon(m.Key)

	// installer.Installation has no BaseURL column — tools must declare
	// absolute endpoints. Leaving BaseURL empty surfaces a clear error from
	// resolveEndpoint when an addon ships a relative endpoint, instead of
	// silently dispatching to /endpoint.
	baseURL := ""

	count := 0
	for _, def := range m.Tools {
		kt := &HTTPDispatcher{
			Inst: Installation{
				ID:       inst.ID,
				OrgID:    inst.OrganizationID,
				AddonKey: m.Key,
				BaseURL:  baseURL,
			},
			Definition: def,
			Dispatcher: dispatcher,
		}
		registry.Replace(kt)
		count++
	}
	return count, nil
}

// RemoveAddon drains every tool the addon contributed. Thin wrapper so
// callers don't need to remember the UnregisterAddon method name.
func RemoveAddon(registry *Registry, addonKey string) int {
	if registry == nil || addonKey == "" {
		return 0
	}
	return registry.UnregisterAddon(addonKey)
}
