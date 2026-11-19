package v3

import "fmt"

// validateInlineRelation checks the create_inline / min primitive of one
// model relation (docs/inline-relations.md):
//
//   - create_inline requires kind one_to_many, embed true and not readonly;
//   - min is >= 0 and only makes sense with create_inline;
//   - the child model (through) must be a model of this manifest and
//     foreign_key one of its columns, so the kernel can inject the parent id.
//
// A relation without create_inline and min is not checked at all (the existing
// relations keep validating exactly as before).
func validateInlineRelation(where string, rel ModelRelation, colsByModel map[string]map[string]struct{}) []string {
	var errs []string
	if rel.Min < 0 {
		errs = append(errs, fmt.Sprintf("%s.min %d must be >= 0", where, rel.Min))
	}
	if rel.Min != 0 && !rel.CreateInline {
		errs = append(errs, fmt.Sprintf("%s.min requires create_inline: true", where))
	}
	if !rel.CreateInline {
		return errs
	}
	if rel.Kind != "one_to_many" {
		errs = append(errs, fmt.Sprintf("%s.create_inline requires kind one_to_many (got %q)", where, rel.Kind))
	}
	if !rel.Embed {
		errs = append(errs, fmt.Sprintf("%s.create_inline requires embed: true", where))
	}
	if rel.Readonly {
		errs = append(errs, fmt.Sprintf("%s.create_inline is incompatible with readonly: true", where))
	}
	if rel.Through == "" || rel.ForeignKey == "" {
		return errs // already reported as empty
	}
	childCols, ok := colsByModel[rel.Through]
	if !ok {
		errs = append(errs, fmt.Sprintf("%s.create_inline: through %q is not a model of this manifest", where, rel.Through))
		return errs
	}
	if _, ok := childCols[rel.ForeignKey]; !ok {
		errs = append(errs, fmt.Sprintf("%s.create_inline: foreign_key %q is not a column of model %q", where, rel.ForeignKey, rel.Through))
	}
	return errs
}
