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
// Internal helper used by computeTable so it can amortise the factory call. It
// does two additive things from a model's HasRelations declaration:
//
//   - stamps ColumnDef.Ref on FK columns whose name matches a belongs_to
//     relation (so the SDK renders a reference-aware select), and
//   - projects the inverse one_to_many / many_to_many relations onto
//     meta.Relations so a detail page can list the owner's child records
//     (vehicles, addresses, polymorphic attachments).
//
// Both are no-ops for models without HasRelations or without the relevant
// relation kinds, so the served payload of a flat model is unchanged.
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
		switch {
		case strings.EqualFold(r.Kind, "belongs_to"):
			// belongs_to feeds column Ref auto-derivation (the FK lives on THIS
			// model). It is not an inverse-view relation, so it is not projected
			// onto meta.Relations.
			if r.ForeignKey == "" || r.Through == "" {
				continue
			}
			if _, exists := byCol[r.ForeignKey]; !exists {
				byCol[r.ForeignKey] = r.Through
			}
		case strings.EqualFold(r.Kind, "one_to_many"), strings.EqualFold(r.Kind, "many_to_many"):
			// Inverse-view relations: surface them to the SDK so a detail page
			// renders a related-records panel. Through + ForeignKey are required;
			// skip malformed entries rather than emit a half-wired relation.
			if r.ForeignKey == "" || r.Through == "" {
				continue
			}
			meta.Relations = append(meta.Relations, modelbase.RelationMeta{
				Name:       r.Name,
				Kind:       r.Kind,
				Through:    r.Through,
				ForeignKey: r.ForeignKey,
				Scope:      r.Scope,
				Label:      r.Label,
				Readonly:   r.Readonly,
				Embed:      r.Embed,
			})
		}
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
