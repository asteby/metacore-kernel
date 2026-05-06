package manifest

import "strings"

// AutoDeriveColumnRefs walks a ModelDefinition's Relations and stamps Ref
// onto every Column whose Name matches a belongs_to ForeignKey. It mutates
// `def` in place and returns the same pointer so callers can chain.
//
// Auto-derivation only fires for "belongs_to" relations because that is the
// shape that puts the FK column on the OWNING model — the side this
// ModelDefinition describes. one_to_many / many_to_many relations carry the
// FK on the OTHER model, so they contribute Ref values to that model's
// columns, not this one's.
//
// An author-provided Ref always wins. The function is idempotent and a
// no-op for definitions without belongs_to relations, so calling it on
// every loaded manifest is safe.
func AutoDeriveColumnRefs(def *ModelDefinition) *ModelDefinition {
	if def == nil || len(def.Relations) == 0 {
		return def
	}
	byCol := make(map[string]string, len(def.Relations))
	for _, r := range def.Relations {
		if !strings.EqualFold(r.Kind, "belongs_to") || r.ForeignKey == "" || r.Through == "" {
			continue
		}
		if _, exists := byCol[r.ForeignKey]; !exists {
			byCol[r.ForeignKey] = r.Through
		}
	}
	if len(byCol) == 0 {
		return def
	}
	for i := range def.Columns {
		if def.Columns[i].Ref != "" {
			continue
		}
		if target, ok := byCol[def.Columns[i].Name]; ok {
			def.Columns[i].Ref = target
		}
	}
	return def
}
