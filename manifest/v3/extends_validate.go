package v3

import (
	"fmt"
	"regexp"
	"strings"
)

// extendsRe is the "<addon_key>.<ModelKey>" shape of Model.Extends (the JSON
// schema enforces it too; repeated here so Validate's message names the model).
var extendsRe = regexp.MustCompile(`^[a-z][a-z0-9_]*\.[A-Z][A-Za-z0-9]+$`)

// searchKeyPlaceholderRe finds the {column} placeholders of a SearchKey format.
var searchKeyPlaceholderRe = regexp.MustCompile(`\{([^{}]*)\}`)

// isExtends reports whether the model is a 1:1 extension table.
func isExtends(mod Model) bool { return strings.TrimSpace(mod.Extends) != "" }

// validateExtends checks the Model.Extends primitive (CONTRACT-item-master.md
// §3.1). An extension table's `id` IS the target's id and its tenancy column is
// injected, so it declares neither; it has no life of its own (no seed, no
// sequences, no stage machine, no relations or nested extensions) because a row
// only exists as the other half of a target row.
func validateExtends(m *Manifest, rls string) []string {
	var errs []string
	byKey := make(map[string]Model, len(m.Models))
	for _, mod := range m.Models {
		byKey[mod.Key] = mod
	}
	for mi, mod := range m.Models {
		if !isExtends(mod) {
			continue
		}
		where := fmt.Sprintf("models[%d] (%s).extends", mi, mod.Key)
		target := strings.TrimSpace(mod.Extends)
		if !extendsRe.MatchString(target) {
			errs = append(errs, fmt.Sprintf("%s %q must be \"<addon_key>.<ModelKey>\"", where, target))
			continue
		}
		addon, targetKey, _ := strings.Cut(target, ".")
		if addon == m.Metadata.Key {
			if targetKey == mod.Key {
				errs = append(errs, fmt.Sprintf("%s: a model cannot extend itself", where))
			} else if t, ok := byKey[targetKey]; !ok {
				errs = append(errs, fmt.Sprintf("%s: %q is not a model of this addon", where, target))
			} else if isExtends(t) {
				errs = append(errs, fmt.Sprintf("%s: %q is itself an extension; extend its target instead", where, target))
			} else if !acceptsExtensions(m, targetKey) {
				errs = append(errs, fmt.Sprintf("%s: %q is not listed in extension_points.model_extensions_accepted", where, targetKey))
			}
		}
		for ci, c := range mod.Columns {
			cw := fmt.Sprintf("models[%d] (%s).columns[%d]", mi, mod.Key, ci)
			switch {
			case c.Name == "id":
				errs = append(errs, fmt.Sprintf("%s: an extension table must not declare \"id\" (the installer makes it the primary key and the foreign key to %s)", cw, target))
			case c.Name == rls:
				errs = append(errs, fmt.Sprintf("%s: an extension table must not declare %q (the installer injects the tenancy column)", cw, rls))
			case c.PrimaryKey:
				errs = append(errs, fmt.Sprintf("%s: an extension table has no primary key of its own", cw))
			}
		}
		for _, f := range []struct {
			set  bool
			name string
		}{
			{len(mod.Extensions) > 0, "extensions"},
			{len(mod.Relations) > 0, "relations"},
			{mod.Seed != nil, "seed"},
			{len(mod.Sequences) > 0, "sequences"},
			{mod.StageField != "" || len(mod.Stages) > 0 || len(mod.Transitions) > 0 || len(mod.OnTransition) > 0, "stage machine (stage_field/stages/transitions/on_transition)"},
			{mod.Import != nil, "import"},
			{mod.Locking != "", "locking"},
		} {
			if f.set {
				errs = append(errs, fmt.Sprintf("%s: an extension table cannot declare %s — it has no rows of its own outside %s", where, f.name, target))
			}
		}
	}
	return errs
}

// acceptsExtensions reports whether the manifest opens one of its own models to
// extension. A target in ANOTHER addon is checked at install time, when both
// manifests are known.
func acceptsExtensions(m *Manifest, modelKey string) bool {
	if m.ExtensionPoints == nil {
		return false
	}
	for _, k := range m.ExtensionPoints.ModelExtensionsAccepted {
		if k == modelKey {
			return true
		}
	}
	return false
}

// validateSearchable checks Column.Searchable: a btree index and a filter over
// a scalar value. json/jsonb/vector columns are not searchable this way.
func validateSearchable(m *Manifest) []string {
	var errs []string
	for mi, mod := range m.Models {
		for ci, c := range mod.Columns {
			if !c.Searchable {
				continue
			}
			t := strings.ToLower(strings.TrimSpace(c.Type))
			if t == "json" || t == "jsonb" || t == "vector" || strings.HasPrefix(t, "vector(") {
				errs = append(errs, fmt.Sprintf("models[%d] (%s).columns[%d] %q: searchable is for scalar columns, not %s", mi, mod.Key, ci, c.Name, c.Type))
			}
		}
	}
	return errs
}

// validateSearchKeys checks Model.SearchKeys: unique names that do not shadow a
// column, and formats whose every placeholder is a scalar column of the model.
func validateSearchKeys(m *Manifest) []string {
	var errs []string
	for mi, mod := range m.Models {
		if len(mod.SearchKeys) == 0 {
			continue
		}
		cols := make(map[string]Column, len(mod.Columns))
		for _, c := range mod.Columns {
			cols[c.Name] = c
		}
		seen := map[string]bool{}
		for si, sk := range mod.SearchKeys {
			where := fmt.Sprintf("models[%d] (%s).search_keys[%d]", mi, mod.Key, si)
			if seen[sk.Name] {
				errs = append(errs, fmt.Sprintf("%s: duplicate name %q", where, sk.Name))
			}
			seen[sk.Name] = true
			if _, clash := cols[sk.Name]; clash {
				errs = append(errs, fmt.Sprintf("%s: name %q collides with a declared column", where, sk.Name))
			}
			switch sk.Match {
			case "", "normalized", "normalized_prefix", "exact":
			default:
				errs = append(errs, fmt.Sprintf("%s.match %q must be normalized|normalized_prefix|exact", where, sk.Match))
			}
			ph := searchKeyPlaceholderRe.FindAllStringSubmatch(sk.Format, -1)
			if len(ph) == 0 {
				errs = append(errs, fmt.Sprintf("%s.format %q has no {column} placeholder", where, sk.Format))
			}
			for _, p := range ph {
				name := strings.TrimSpace(p[1])
				c, ok := cols[name]
				if !ok {
					errs = append(errs, fmt.Sprintf("%s.format: {%s} is not a declared column on the model", where, name))
					continue
				}
				t := strings.ToLower(strings.TrimSpace(c.Type))
				if t == "json" || t == "jsonb" || t == "vector" || strings.HasPrefix(t, "vector(") {
					errs = append(errs, fmt.Sprintf("%s.format: {%s} is %s; only scalar columns compose a key", where, name, c.Type))
				}
			}
			if rest := searchKeyPlaceholderRe.ReplaceAllString(sk.Format, ""); strings.ContainsAny(rest, "{}") {
				errs = append(errs, fmt.Sprintf("%s.format %q has an unbalanced brace", where, sk.Format))
			}
		}
	}
	return errs
}

// validateAttributeClasses checks Model.attribute_classes and every
// visible_when.class (CONTRACT-item-master.md §3.2): classes live on extension
// tables, their keys are unique across the manifest, their sections name the
// table's own columns, and a class a column is shown for exists.
func validateAttributeClasses(m *Manifest) []string {
	var errs []string
	classes := map[string]string{} // class key → owning model key
	for mi, mod := range m.Models {
		if len(mod.AttributeClasses) > 0 && !isExtends(mod) {
			errs = append(errs, fmt.Sprintf("models[%d] (%s).attribute_classes: only a model with extends declares attribute classes", mi, mod.Key))
		}
		cols := map[string]struct{}{}
		for _, c := range mod.Columns {
			cols[c.Name] = struct{}{}
		}
		for ci, cl := range mod.AttributeClasses {
			where := fmt.Sprintf("models[%d] (%s).attribute_classes[%d]", mi, mod.Key, ci)
			if owner, dup := classes[cl.Key]; dup {
				errs = append(errs, fmt.Sprintf("%s: class %q is already declared on %s", where, cl.Key, owner))
			}
			classes[cl.Key] = mod.Key
			seenSec := map[string]bool{}
			for si, sec := range cl.Sections {
				if seenSec[sec.Key] {
					errs = append(errs, fmt.Sprintf("%s.sections[%d]: duplicate key %q", where, si, sec.Key))
				}
				seenSec[sec.Key] = true
				for _, f := range sec.Fields {
					if _, ok := cols[f]; !ok {
						errs = append(errs, fmt.Sprintf("%s.sections[%d]: %q is not a column of %s", where, si, f, mod.Key))
					}
				}
			}
		}
	}
	for mi, mod := range m.Models {
		for ci, c := range mod.Columns {
			vw := c.VisibleWhen
			if vw == nil || vw.Class == "" {
				continue
			}
			where := fmt.Sprintf("models[%d] (%s).columns[%d].visible_when", mi, mod.Key, ci)
			if vw.Field != "" || vw.Equals != "" || len(vw.In) > 0 {
				errs = append(errs, fmt.Sprintf("%s: class is used alone, without field/equals/in", where))
			}
			if _, ok := classes[vw.Class]; !ok {
				errs = append(errs, fmt.Sprintf("%s.class %q is not declared in attribute_classes", where, vw.Class))
			}
		}
	}
	return errs
}
