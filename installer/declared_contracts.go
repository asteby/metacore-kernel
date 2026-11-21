package installer

import (
	"log/slog"
	"sort"

	"github.com/asteby/metacore-kernel/lifecycle"
	"github.com/asteby/metacore-kernel/manifest"
)

// Capability-contract registry wiring (X-R2). The registry of contracts
// declared by addons (capability.RegisterDeclared) is process memory, so it is
// fed from two places: Install / Upgrade (below) and a host's boot, through
// RehydrateDeclaredContracts, so a restart does not turn every declared key
// back into an unknown one.

// registerDeclaredContracts records the contracts of an installed addon with
// role "contract". It never fails an install or an upgrade: a conflict (another
// addon already owns the contract key) is logged. Uninstall does not
// unregister: other organizations may still run the addon, and a stale entry
// only keeps a key known (warnDanglingContracts makes the removal visible).
func registerDeclaredContracts(m *manifest.Manifest) {
	if err := m.RegisterDeclaredContracts(); err != nil {
		slog.Warn("installer.contracts_register_failed", "addon", m.Key, "version", m.Version, "error", err)
	}
}

// RehydrateReport is the outcome of RehydrateDeclaredContracts.
type RehydrateReport struct {
	// Registered lists the addon keys whose contracts were recorded.
	Registered []string
	// Failed maps an addon key to why its contracts were refused (logged too).
	Failed map[string]error
}

// RehydrateDeclaredContracts re-registers the contracts of every addon in the
// installer's lifecycle registry. A host calls it at boot, after it rehydrates
// the registry from the addons installed in its database (the same pass that
// loads their manifests). Safe to call repeatedly: the same owner replaces its
// own declaration. A failing addon is logged and reported, never fatal.
func (i *Installer) RehydrateDeclaredContracts() RehydrateReport {
	rep := RehydrateReport{Failed: map[string]error{}}
	if i == nil || i.Lifecycles == nil {
		return rep
	}
	all := i.Lifecycles.All()
	keys := make([]string, 0, len(all))
	for k := range all {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		m := all[k].Manifest()
		if len(m.Contracts) == 0 {
			continue
		}
		if err := m.RegisterDeclaredContracts(); err != nil {
			rep.Failed[k] = err
			slog.Warn("installer.contracts_register_failed", "addon", m.Key, "version", m.Version, "error", err, "phase", "rehydrate")
			continue
		}
		rep.Registered = append(rep.Registered, k)
	}
	return rep
}

// warnDanglingContracts logs, before an addon that owns contracts is removed,
// which still-loaded addons implement or extend one of them: their capability
// keys would otherwise dangle without a trace. Only a warning: removal goes on.
func warnDanglingContracts(reg *lifecycle.Registry, m manifest.Manifest) {
	if reg == nil || len(m.Contracts) == 0 {
		return
	}
	owned := map[string]bool{}
	for _, c := range m.Contracts {
		owned[c.Key] = true
	}
	var implementers []string
	for k, a := range reg.All() {
		if k == m.Key {
			continue
		}
		for _, im := range a.Manifest().Implements {
			if owned[im.Contract] {
				implementers = append(implementers, k)
				break
			}
		}
	}
	if len(implementers) == 0 {
		return
	}
	sort.Strings(implementers)
	slog.Warn("installer.contract_removed_with_implementers",
		"addon", m.Key, "implementers", implementers,
		"detail", "these addons implement a contract owned by the addon being uninstalled; their capability keys will dangle")
}
