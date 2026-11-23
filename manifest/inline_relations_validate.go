package manifest

import "fmt"

// validateInlineRelations is the legacy/install-surface twin of
// v3.validateInlineRelation (docs/inline-relations.md): create_inline requires
// kind one_to_many, embed true and not readonly; min is >= 0 and needs
// create_inline; the child model (through) must be a model of the manifest and
// foreign_key one of its columns — unless Addon names the addon that owns the
// child model: then it lives in another manifest and the kernel validates its
// columns at write time. Relations that declare neither create_inline nor min
// are not checked, so existing manifests validate as before.
func validateInlineRelations(rels []RelationDef, colsByModel map[string]map[string]struct{}) error {
	for i, r := range rels {
		if r.Min < 0 {
			return fmt.Errorf("relations[%d]: min %d must be >= 0", i, r.Min)
		}
		if r.Min != 0 && !r.CreateInline {
			return fmt.Errorf("relations[%d]: min requires create_inline", i)
		}
		if !r.CreateInline {
			continue
		}
		if r.Kind != "one_to_many" {
			return fmt.Errorf("relations[%d]: create_inline requires kind one_to_many (got %q)", i, r.Kind)
		}
		if !r.Embed {
			return fmt.Errorf("relations[%d]: create_inline requires embed", i)
		}
		if r.Readonly {
			return fmt.Errorf("relations[%d]: create_inline is incompatible with readonly", i)
		}
		if r.Addon != "" {
			continue // the child model is another addon's: checked at write time
		}
		childCols, ok := colsByModel[r.Through]
		if !ok {
			return fmt.Errorf("relations[%d]: create_inline: through %q is not a model of this manifest", i, r.Through)
		}
		if _, ok := childCols[r.ForeignKey]; !ok {
			return fmt.Errorf("relations[%d]: create_inline: foreign_key %q is not a column of model %q", i, r.ForeignKey, r.Through)
		}
	}
	return nil
}
