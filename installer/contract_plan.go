package installer

import (
	"errors"
	"fmt"
	"sort"

	"github.com/asteby/metacore-kernel/manifest"
)

// Cascade install by contract (RFC SDK-0001 §6, decision A6).
//
// A consumer that requires a CONTRACT (requires[].contract) and a provider
// that IMPLEMENTS one both need the contract's OWNER installed first. The
// plan installs the owner (and, recursively, what the owner needs), never a
// provider: choosing the provider is the organization's decision, so the plan
// only lists the contracts that still have no provider (ChooseProvider) and
// the operation answers no_provider until one is bound (degrade, not break).
//
// The kernel holds what is INSTALLED; what is PUBLISHED lives in the hub, so
// the catalog is an interface the host implements over its hub client.

// MaxContractPlanDepth bounds the cascade: the candidate is depth 0, the owner
// of a contract it needs depth 1, and so on (RFC §6.2: deeper than 3 fails).
const MaxContractPlanDepth = 3

var (
	// ErrUnknownContract: no published addon owns a required contract.
	ErrUnknownContract = errors.New("installer: no published owner for contract")
	// ErrContractPlanTooDeep: the cascade needs more than MaxContractPlanDepth levels.
	ErrContractPlanTooDeep = errors.New("installer: contract cascade too deep")
)

// ContractCatalog is the host's view of the PUBLISHED addons (the hub).
type ContractCatalog interface {
	// ContractOwner returns the manifest of the published addon that owns the
	// contract (its highest compatible version, as the host decides).
	ContractOwner(contractKey string) (manifest.Manifest, bool)
	// Addon returns a published addon by key (to follow the owner's own
	// addon requirements). ok=false leaves that requirement to CheckInstallable.
	Addon(key string) (manifest.Manifest, bool)
}

// ContractPlan is the outcome of PlanContractInstall.
type ContractPlan struct {
	// Install are the addons to install BEFORE the candidate, in install order
	// (owners first). Never a provider picked on the org's behalf.
	Install []string
	// Order is Install followed by the candidate.
	Order []string
	// Owners maps every contract the plan resolved to its owner addon key.
	Owners map[string]string
	// ChooseProvider lists, sorted, the required contracts with selection
	// cardinality "one" that have no implementer installed or planned: the
	// org must bind a provider (settings.<family>); until then calls answer
	// no_provider.
	ChooseProvider []string
	// Manifests are the planned manifests by key (Install members only).
	Manifests map[string]manifest.Manifest
}

// PlanContractInstall plans the install of candidate against the org's
// installed addons and the published catalog. It follows non-optional
// requires[].contract, implements[] (a provider brings its owner) and the
// non-optional addon requires of every PLANNED owner. Optional contract
// requirements are not installed: their enables[] stay off. Errors:
// ErrUnknownContract, ErrContractPlanTooDeep, ErrDependencyCycle.
func PlanContractInstall(installed []InstalledAddon, candidate manifest.Manifest, cat ContractCatalog) (ContractPlan, error) {
	plan := ContractPlan{Owners: map[string]string{}, Manifests: map[string]manifest.Manifest{}}
	isInstalled := map[string]bool{}
	for _, a := range installed {
		isInstalled[a.Key] = true
	}
	for _, c := range candidate.Contracts {
		plan.Owners[c.Key] = candidate.Key
	}
	implementers := map[string]bool{} // contract -> some implementer installed or planned
	for _, a := range installed {
		for _, c := range a.Implements {
			implementers[c] = true
		}
		for _, c := range a.Contracts {
			if _, ok := plan.Owners[c.Key]; !ok {
				plan.Owners[c.Key] = a.Key
			}
		}
	}
	for _, im := range candidate.Implements {
		implementers[im.Contract] = true
	}
	oneContract := map[string]bool{}
	noteOwned := func(m manifest.Manifest) {
		for _, c := range m.Contracts {
			oneContract[c.Key] = c.Selection == nil || c.Selection.Cardinality != "many"
		}
	}
	noteOwned(candidate)
	for _, a := range installed {
		for _, c := range a.Contracts {
			oneContract[c.Key] = c.One
		}
	}
	required := map[string]bool{}

	type node struct {
		m     manifest.Manifest
		depth int
	}
	queue := []node{{candidate, 0}}
	var errs []error
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		var needs []string
		for _, r := range n.m.ContractRequires {
			if !r.Optional {
				needs = append(needs, r.Contract)
				required[r.Contract] = true
			}
		}
		for _, im := range n.m.Implements {
			needs = append(needs, im.Contract)
		}
		for _, c := range needs {
			if _, ok := plan.Owners[c]; ok {
				continue
			}
			if cat == nil {
				errs = append(errs, fmt.Errorf("%w %q (required by %q; no catalog)", ErrUnknownContract, c, n.m.Key))
				continue
			}
			om, ok := cat.ContractOwner(c)
			if !ok || om.Key == "" {
				errs = append(errs, fmt.Errorf("%w %q (required by %q)", ErrUnknownContract, c, n.m.Key))
				continue
			}
			plan.Owners[c] = om.Key
			if isInstalled[om.Key] || om.Key == candidate.Key {
				continue
			}
			if _, done := plan.Manifests[om.Key]; done {
				continue
			}
			if n.depth+1 > MaxContractPlanDepth {
				return ContractPlan{}, fmt.Errorf("%w: %s -> %s (contract %s) exceeds %d levels", ErrContractPlanTooDeep, n.m.Key, om.Key, c, MaxContractPlanDepth)
			}
			for _, oc := range om.Contracts {
				if _, ok := plan.Owners[oc.Key]; !ok {
					plan.Owners[oc.Key] = om.Key
				}
			}
			noteOwned(om)
			plan.Manifests[om.Key] = om
			queue = append(queue, node{om, n.depth + 1})
		}
		// Addon requires of a planned owner (not of the candidate: those stay
		// with CheckInstallable, as before).
		if n.depth == 0 || cat == nil {
			continue
		}
		for _, r := range n.m.AddonRequires {
			if r.Optional || manifest.IsReservedRequirementKey(r.Key) || isInstalled[r.Key] || r.Key == candidate.Key {
				continue
			}
			if _, done := plan.Manifests[r.Key]; done {
				continue
			}
			am, ok := cat.Addon(r.Key)
			if !ok {
				continue
			}
			if n.depth+1 > MaxContractPlanDepth {
				return ContractPlan{}, fmt.Errorf("%w: %s -> %s exceeds %d levels", ErrContractPlanTooDeep, n.m.Key, am.Key, MaxContractPlanDepth)
			}
			noteOwned(am)
			plan.Manifests[am.Key] = am
			queue = append(queue, node{am, n.depth + 1})
		}
	}
	if err := errors.Join(errs...); err != nil {
		return ContractPlan{}, err
	}
	for _, m := range plan.Manifests {
		for _, im := range m.Implements {
			implementers[im.Contract] = true
		}
	}

	set := []manifest.Manifest{candidate}
	for _, m := range plan.Manifests {
		set = append(set, m)
	}
	order, err := ResolveOrder(set)
	if err != nil {
		return ContractPlan{}, err
	}
	for _, k := range order {
		if k != candidate.Key {
			plan.Install = append(plan.Install, k)
		}
	}
	plan.Order = append(append([]string(nil), plan.Install...), candidate.Key)
	for c := range required {
		if oneContract[c] && !implementers[c] {
			plan.ChooseProvider = append(plan.ChooseProvider, c)
		}
	}
	sort.Strings(plan.ChooseProvider)
	return plan, nil
}
