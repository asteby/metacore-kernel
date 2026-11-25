package manifest

import (
	"sort"
	"strings"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// N-1 compatibility of contracts (RFC SDK-0001 §7). Neither projection runs
// inside FromV3: what an installed manifest declares is never rewritten. Hosts
// apply them where the RFC places them: ProvidesCapabilitiesForN1 when SERVING
// a manifest to a host that only understands provides_capabilities[], and
// InferImplements when READING an old provider that only declares
// provides_capabilities[] (reported with the role_inferred warning).

// OperationsFn returns the operation keys of a contract (from the contract
// owner's manifest or a capability.Registry). nil, or ok=false, means unknown.
type OperationsFn func(contractKey string) (ops []string, ok bool)

// ProvidesCapabilitiesForN1 returns m.ProvidesCapabilities plus, for every
// operation of every implements[] entry that has no provides_capabilities key
// of its own, the conventional projection
//
//	{key: "<contract>.<op>", trigger: {type: "wasm", export: "<op>"}}
//
// carrying implements[].countries. The operations come from
// implements[].operations, or from ops when that list is empty (the contract's
// required operations). An addon's own entry always wins. The result is a new
// slice sorted like the input followed by the projected keys in order; m is
// not modified.
func (m *Manifest) ProvidesCapabilitiesForN1(ops OperationsFn) []CapabilityProviderDef {
	out := append([]CapabilityProviderDef(nil), m.ProvidesCapabilities...)
	have := map[string]bool{}
	for _, p := range out {
		have[p.Key] = true
	}
	for _, im := range m.Implements {
		names := im.Operations
		if len(names) == 0 && ops != nil {
			if o, ok := ops(im.Contract); ok {
				names = o
			}
		}
		for _, op := range names {
			k := im.Contract + "." + op
			if op == "" || have[k] {
				continue
			}
			have[k] = true
			out = append(out, CapabilityProviderDef{
				Key:       k,
				Trigger:   &ActionTrigger{Type: "wasm", Export: op},
				Countries: append([]string(nil), im.Countries...),
			})
		}
	}
	return out
}

// InferImplements returns the implements[] entries an old provider implies:
// for each provides_capabilities key that lies under a contract known(contract)
// accepts (the longest dotted prefix wins) and that implements[] does not
// already name, {contract, version: "*"}. Sorted by contract; nil when none.
func InferImplements(provided []string, implemented []v3.Implements, known func(contractKey string) bool) []v3.Implements {
	if known == nil {
		return nil
	}
	have := map[string]bool{}
	for _, im := range implemented {
		have[im.Contract] = true
	}
	found := map[string]bool{}
	for _, k := range provided {
		parts := strings.Split(k, ".")
		for n := len(parts); n >= 2; n-- {
			c := strings.Join(parts[:n], ".")
			if known(c) {
				if !have[c] {
					found[c] = true
				}
				break
			}
		}
	}
	if len(found) == 0 {
		return nil
	}
	keys := make([]string, 0, len(found))
	for c := range found {
		keys = append(keys, c)
	}
	sort.Strings(keys)
	out := make([]v3.Implements, len(keys))
	for i, c := range keys {
		out[i] = v3.Implements{Contract: c, Version: "*"}
	}
	return out
}
