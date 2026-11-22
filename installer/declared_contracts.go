package installer

import (
	"log/slog"
	"sort"

	"github.com/asteby/metacore-kernel/capability"
	"github.com/asteby/metacore-kernel/lifecycle"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/google/uuid"
)

// Capability-contract registry wiring (X-R2). The declared-contract registry is
// a capability.Registry INSTANCE per scope (the host picks organization or
// installation) and is memory: it is fed by Install / Upgrade (through
// Installer.ContractRegistry) and rebuilt at boot with
// Registry.Rehydrate(Installer.InstalledContracts(org)), so a restart does not
// turn every declared key back into an unknown one.

// registerDeclaredContracts records the contracts of an addon that was just
// installed or upgraded in the org's registry. It never fails an install or an
// upgrade: a conflict (another addon of that scope already owns the contract
// key) is logged. An addon without contracts[]/implements[] still goes through
// the registry so an upgrade that DROPS them clears the old entries. Uninstall
// unregisters it from this org's registry (unregisterDeclaredContracts), and
// warnDanglingContracts makes the removal of a contract owner visible.
func (i *Installer) registerDeclaredContracts(orgID uuid.UUID, m *manifest.Manifest) {
	if i == nil || i.ContractRegistry == nil {
		return
	}
	if err := m.RegisterDeclaredContracts(i.ContractRegistry(orgID)); err != nil {
		slog.Warn("installer.contracts_register_failed", "addon", m.Key, "version", m.Version, "org_id", orgID.String(), "error", err)
	}
}

// unregisterDeclaredContracts forgets, in the org's registry, everything the
// addon owned or implemented. It runs when the addon is uninstalled FROM THAT
// ORG: the registry is per org, so other orgs that still run the addon keep
// their own entries. A nil wiring is a no-op.
func (i *Installer) unregisterDeclaredContracts(orgID uuid.UUID, addonKey string) {
	if i == nil || i.ContractRegistry == nil {
		return
	}
	i.ContractRegistry(orgID).Unregister(addonKey)
}

// InstalledContracts lists, for one organization, the contracts declared by the
// addons it has installed: the persisted installation rows joined with the
// manifests the host loaded into Lifecycles. It is the input of
// capability.Registry.Rehydrate at boot, ordered by installation time so the
// first installed owner of a contract key keeps it.
func (i *Installer) InstalledContracts(orgID uuid.UUID) ([]capability.InstalledContracts, error) {
	var rows []Installation
	if err := i.DB.Where("organization_id = ?", orgID).Order("installed_at ASC, addon_key ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	var out []capability.InstalledContracts
	for _, r := range rows {
		lc, ok := i.Lifecycles.Get(r.AddonKey)
		if !ok {
			continue
		}
		m := lc.Manifest()
		// The lifecycle registry holds ONE manifest per addon key for the whole
		// process, but each organization persists the version IT installed. When
		// they differ the loaded manifest is not what this org runs: feeding its
		// contracts would hand this org another tenant's version, so the addon is
		// skipped (loudly) and the next install/upgrade of it registers the truth.
		if m.Version != r.Version {
			slog.Warn("installer.contracts_rehydrate_version_mismatch",
				"addon", r.AddonKey, "org_id", orgID.String(),
				"installed_version", r.Version, "loaded_version", m.Version,
				"detail", "skipped: the loaded manifest is not the version this organization installed")
			continue
		}
		ds, is := (&m).DeclaredContracts(), (&m).DeclaredImplements()
		if len(ds) == 0 && len(is) == 0 {
			continue
		}
		out = append(out, capability.InstalledContracts{Owner: r.AddonKey, Contracts: ds, Implements: is})
	}
	return out, nil
}

// warnDanglingContracts logs, before an addon that owns contracts is removed,
// which still-loaded addons implement one of them: their capability keys would
// otherwise dangle without a trace. Only a warning: removal goes on.
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
