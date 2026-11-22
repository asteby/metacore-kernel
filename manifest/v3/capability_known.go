package v3

import (
	"fmt"
	"sort"

	"github.com/asteby/metacore-kernel/capability"
)

// Closed registry of capability keys (X-R2), phase 1: an unknown key is a
// WARNING naming the key and where it is used. A later release promotes it to
// an error (remove the rule from warnOnlyWindow, then from legacyTolerated).
const (
	// RuleCapabilityKeyUnknown: a provides_capabilities[].key or a
	// handler.capability is neither a kernel contract, nor declared by a
	// contracts[] of this manifest, nor known to the host's registry
	// (capability.RegisterDeclared) or Options.CapabilityCatalog.
	RuleCapabilityKeyUnknown = "capability_key_unknown"
)

// CapabilityCatalog is the host's view of the keys declared in the scope being
// validated (an organization's capability.Registry implements it; the hub can
// serve its catalog). Optional: nil leaves only the kernel's own contracts and
// the manifest's own contracts[] as known. There is no process-wide registry of
// declared contracts: the answer depends on what THIS scope has installed.
type CapabilityCatalog interface {
	KnownCapability(key string) bool
}

// ContractVersioned is an OPTIONAL extension of RoleCatalog (checked by type
// assertion so the published RoleCatalog interface stays unchanged): it serves
// the version of a contract so implements[].version can be checked. A
// capability.Registry implements it.
type ContractVersioned interface {
	ContractVersion(contractKey string) (version string, ok bool)
}

// DeclaredContracts projects contracts[] onto the capability registry's view.
func (m *Manifest) DeclaredContracts() []capability.Declared {
	out := make([]capability.Declared, 0, len(m.Contracts))
	for _, c := range m.Contracts {
		d := capability.Declared{Key: c.Key, Version: c.Version}
		for _, op := range c.Operations {
			k := capability.KindAction
			if op.Kind == "query" {
				k = capability.KindQuery
			}
			d.Operations = append(d.Operations, capability.DeclaredOperation{Key: op.Key, Kind: k})
		}
		out = append(out, d)
	}
	return out
}

// DeclaredImplements projects implements[] onto the registry's view.
func (m *Manifest) DeclaredImplements() []capability.Implemented {
	out := make([]capability.Implemented, 0, len(m.Implements))
	for _, im := range m.Implements {
		out = append(out, capability.Implemented{Contract: im.Contract, Range: im.Version})
	}
	return out
}

// CapabilityUse is one capability key a manifest mentions and where.
type CapabilityUse struct {
	Key   string
	Where string
}

// CapabilityUses lists the capability keys of provides_capabilities[] and of
// every action / tool / subscription handler of type capability.
func (m *Manifest) CapabilityUses() []CapabilityUse {
	var out []CapabilityUse
	for i, pc := range m.ProvidesCapabilities {
		out = append(out, CapabilityUse{pc.Key, fmt.Sprintf("provides_capabilities[%d]", i)})
	}
	if m.Contributions != nil {
		for i, a := range m.Contributions.Actions {
			if a.Handler.Type == "capability" {
				out = append(out, CapabilityUse{a.Handler.Capability, fmt.Sprintf("contributions.actions[%d].handler", i)})
			}
		}
		for i, t := range m.Contributions.Tools {
			if t.Handler.Type == "capability" {
				out = append(out, CapabilityUse{t.Handler.Capability, fmt.Sprintf("contributions.tools[%d].handler", i)})
			}
		}
		for i, s := range m.Contributions.Subscriptions {
			if s.Handler.Type == "capability" {
				out = append(out, CapabilityUse{s.Handler.Capability, fmt.Sprintf("contributions.subscriptions[%d].handler", i)})
			}
		}
	}
	return out
}

// UnknownCapabilityWarnings returns one warning per use whose key is not
// known: not a kernel contract, not declared by this manifest (declaredHere)
// and not known to the host's catalog cat (optional; nil-safe, typed nil
// included).
func UnknownCapabilityWarnings(uses []CapabilityUse, declaredHere []capability.Declared, cat CapabilityCatalog) []string {
	here := map[string]bool{}
	for _, d := range declaredHere {
		here[d.Key] = true
		for _, op := range d.Operations {
			here[d.Key+"."+op.Key] = true
		}
	}
	if isNilValue(cat) {
		cat = nil
	}
	var out []string
	for _, u := range uses {
		if u.Key == "" || here[u.Key] {
			continue
		}
		if _, ok := capability.KnownBuiltin(u.Key); ok {
			continue
		}
		if cat != nil && cat.KnownCapability(u.Key) {
			continue
		}
		out = append(out, fmt.Sprintf("[%s] %s: capability %q is not in the registry (not a kernel contract, not declared by an installed contracts[]); the registry is closing — declare it in a contract addon", RuleCapabilityKeyUnknown, u.Where, u.Key))
	}
	sort.Strings(out)
	return out
}
