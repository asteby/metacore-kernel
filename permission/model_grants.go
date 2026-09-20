package permission

import (
	"strings"
	"sync"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// ModelGrants indexes the manifest's explicit capability -> (model, actions)
// mapping (rbac.permissions[].models) so a host can answer, on the data gate,
// "does any capability this caller holds authorize <action> on <model>?" for
// capabilities that name no (or another) owning addon — pos.sale.read over the
// customers-owned SalesOrder, caja.queue.read, refund_claims.settle.
//
// It is the ONLY way such a capability reaches a model: a capability never
// authorizes a model it does not list. Safe for concurrent use.
type ModelGrants struct {
	mu sync.RWMutex
	// byAddon[addon][capability] -> entries; keyed by addon so a reinstall or
	// upgrade replaces exactly that addon's mapping.
	byAddon map[string]map[string][]modelGrant
}

type modelGrant struct {
	model   string // normalized (see normModel)
	actions map[string]struct{}
}

// NewModelGrants returns an empty index.
func NewModelGrants() *ModelGrants {
	return &ModelGrants{byAddon: map[string]map[string][]modelGrant{}}
}

// normModel reduces a model reference to a comparable form: the part after an
// "addon." qualifier, lowercased, without "_" — so "customers.SalesOrder",
// "SalesOrder" and the table "sales_orders" (via a caller-supplied alias, see
// Allows) compare equal on the key form.
func normModel(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "."); i >= 0 {
		s = s[i+1:]
	}
	return strings.ReplaceAll(strings.ToLower(s), "_", "")
}

func normAction(a string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(a)), "action.")
}

// Register replaces addonKey's mapping with the models declared on perms.
// Permissions without models are ignored (they keep the host's name-based
// resolution, if any).
func (g *ModelGrants) Register(addonKey string, perms []v3.PermissionDef) {
	idx := map[string][]modelGrant{}
	for _, p := range perms {
		capKey := strings.ToLower(strings.TrimSpace(p.Key))
		for _, pm := range p.Models {
			if normModel(pm.Model) == "" || len(pm.Actions) == 0 {
				continue
			}
			acts := make(map[string]struct{}, len(pm.Actions))
			for _, a := range pm.Actions {
				acts[normAction(a)] = struct{}{}
			}
			idx[capKey] = append(idx[capKey], modelGrant{model: normModel(pm.Model), actions: acts})
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(idx) == 0 {
		delete(g.byAddon, addonKey)
		return
	}
	g.byAddon[addonKey] = idx
}

// Unregister drops addonKey's mapping (addon uninstall).
func (g *ModelGrants) Unregister(addonKey string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.byAddon, addonKey)
}

// Allows reports the first capability in `grants` that a manifest maps to one
// of `modelNames` (the model key, its table name, any alias) for one of
// `actions`. Nothing outside the declared mapping is ever authorized.
func (g *ModelGrants) Allows(grants []string, actions []string, modelNames ...string) (string, bool) {
	if g == nil || len(grants) == 0 {
		return "", false
	}
	names := make(map[string]struct{}, len(modelNames))
	for _, n := range modelNames {
		if k := normModel(n); k != "" {
			names[k] = struct{}{}
		}
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	for _, grant := range grants {
		key := strings.ToLower(strings.TrimSpace(grant))
		for _, idx := range g.byAddon {
			for _, mg := range idx[key] {
				if _, ok := names[mg.model]; !ok {
					continue
				}
				for _, a := range actions {
					if _, ok := mg.actions[normAction(a)]; ok {
						return grant, true
					}
				}
			}
		}
	}
	return "", false
}
