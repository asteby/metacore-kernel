package v3

import (
	"fmt"
	"regexp"
	"strings"
)

var optionFitmentIdent = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)

// validateOptionFitmentShape checks the declaration itself: the three names are
// required identifiers and mode is flag|filter.
func validateOptionFitmentShape(where string, f *OptionFitment) []string {
	if f == nil {
		return nil
	}
	var errs []string
	w := where + ".option_fitment"
	for _, p := range []struct{ name, v string }{
		{"subject_field", f.SubjectField},
		{"subject_model", f.SubjectModel},
		{"target_model", f.TargetModel},
	} {
		if strings.TrimSpace(p.v) == "" {
			errs = append(errs, fmt.Sprintf("%s.%s is required", w, p.name))
		} else if !optionFitmentIdent.MatchString(p.v) {
			errs = append(errs, fmt.Sprintf("%s.%s %q must be an identifier (letters, digits, underscore)", w, p.name, p.v))
		}
	}
	switch f.Mode {
	case "", OptionFitmentFlag, OptionFitmentFilter:
	default:
		errs = append(errs, fmt.Sprintf("%s.mode %q is not one of %s|%s", w, f.Mode, OptionFitmentFlag, OptionFitmentFilter))
	}
	return errs
}

// validateOptionFitment checks Column.option_fitment against the manifest:
//
//   - the column must be a picker (a ref or an options object); static options
//     and options_source lists are not relation pickers whose rows carry the
//     attributes the rules compare;
//   - subject_model / target_model that are models of this manifest resolve;
//     one that is not (a model of another addon) is a WARNING, not an error;
//   - subject_field must be a column of the owning model or of a header model
//     that has the owner as a line (a relation through it). When a header is
//     known and lacks it, or the model is standalone, a typo is an error; when
//     the header may live in another addon it is a warning.
func validateOptionFitment(where string, c Column, mod Model, m *Manifest, ownCols map[string]struct{}, colsByModel map[string]map[string]struct{}, warnings *[]string) []string {
	f := c.OptionFitment
	if f == nil {
		return nil
	}
	errs := validateOptionFitmentShape(where, f)
	w := where + ".option_fitment"
	if c.Ref == "" && c.Options.Dynamic == nil {
		errs = append(errs, fmt.Sprintf("%s needs a picker column: declare ref or an options object on the same column (static options and options_source are not relation pickers)", w))
	}
	if len(errs) > 0 {
		return errs
	}
	warn := func(msg string) {
		if warnings != nil {
			*warnings = append(*warnings, fmt.Sprintf("[option_fitment] %s: %s", w, msg))
		}
	}
	for _, p := range []struct{ name, v string }{{"subject_model", f.SubjectModel}, {"target_model", f.TargetModel}} {
		if _, local := colsByModel[p.v]; !local {
			warn(fmt.Sprintf("%s %q is not a model of this manifest (assumed to belong to another addon)", p.name, p.v))
		}
	}
	if _, ok := ownCols[f.SubjectField]; ok {
		return nil
	}
	headers := 0
	for _, h := range m.Models {
		for _, rel := range h.Relations {
			if rel.Through != mod.Key {
				continue
			}
			headers++
			if cols, ok := colsByModel[h.Key]; ok {
				if _, has := cols[f.SubjectField]; has {
					return nil
				}
			}
		}
	}
	switch {
	case headers > 0:
		return []string{fmt.Sprintf("%s.subject_field %q is not a column of %s nor of the header model(s) it is a line of", w, f.SubjectField, mod.Key)}
	case modelIsLine(mod):
		warn(fmt.Sprintf("subject_field %q is not a column of %s; its header looks to live in another addon", f.SubjectField, mod.Key))
		return nil
	default:
		return []string{fmt.Sprintf("%s.subject_field %q is not a column of %s", w, f.SubjectField, mod.Key)}
	}
}

// modelIsLine reports whether the model is a document line: one of its
// rules addresses a parent row through a ref column.
func modelIsLine(mod Model) bool {
	for _, r := range mod.Rules {
		if strings.TrimSpace(r.Parent) != "" {
			return true
		}
	}
	return false
}
