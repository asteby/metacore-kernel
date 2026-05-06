package metadata

import (
	"context"
	"strings"

	"github.com/asteby/metacore-kernel/modelbase"
)

// AutoDeriveRefs is a TableTransformer that stamps ColumnDef.Ref on every
// foreign-key column whose name matches a belongs_to RelationDef declared on
// the model.
//
// Service.New does NOT register this transformer — auto-derivation runs
// inline inside computeTable so it sees the def the same factory invocation
// produced and avoids a second modelbase.Get round-trip per request. This
// function is exported for hosts that wire metadata production through a
// different surface and want the same behaviour.
//
// Idempotent and additive: an author-provided Ref always wins. Models
// without HasRelations are a no-op.
func AutoDeriveRefs(_ context.Context, modelKey string, meta *modelbase.TableMetadata) error {
	def, ok := modelbase.Get(modelKey)
	if !ok {
		return nil
	}
	deriveRefsFromDef(def, meta)
	return nil
}

// deriveRefsFromDef does the actual work given the already-resolved def.
// Internal helper used by computeTable so it can amortise the factory call.
func deriveRefsFromDef(def modelbase.ModelDefiner, meta *modelbase.TableMetadata) {
	if def == nil || meta == nil {
		return
	}
	rels, ok := def.(modelbase.HasRelations)
	if !ok {
		return
	}
	relations := rels.DefineRelations()
	if len(relations) == 0 {
		return
	}
	byCol := make(map[string]string, len(relations))
	for _, r := range relations {
		if !strings.EqualFold(r.Kind, "belongs_to") || r.ForeignKey == "" || r.Through == "" {
			continue
		}
		if _, exists := byCol[r.ForeignKey]; !exists {
			byCol[r.ForeignKey] = r.Through
		}
	}
	if len(byCol) == 0 {
		return
	}
	for i := range meta.Columns {
		col := &meta.Columns[i]
		if col.Ref != "" {
			continue
		}
		if target, ok := byCol[col.Key]; ok {
			col.Ref = target
		}
	}
}
