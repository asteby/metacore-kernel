package v3

import (
	"fmt"
	"sort"
	"strings"

	"github.com/asteby/metacore-kernel/manifest/computeexpr"
)

// Derived columns and read_permission.
//
// A column computed from a gated column would hand its value to whoever may
// read the derived one: margin = price - cost lets the reader of price and
// margin compute cost. The rule is therefore explicit, never inherited: a
// derived column (formula target, generated column, rollup target, snapshot
// target) must declare the SAME read_permission as every gated column it
// reads. Sources gated by two different permissions cannot feed one column.
// A search key may not compose a gated column at all, because the list search
// matches it for every reader. Tier-3 (wasm) formulas read arbitrary columns
// the manifest does not name: their author gates the target by hand.

// DerivedReadPermissionError reports a derived column that does not cover the
// read_permission of its sources, or "" when it does. sources maps each
// source column to its read_permission ("" = ungated). Shared by the v3 and
// legacy validators.
func DerivedReadPermissionError(target, targetPerm string, sources map[string]string) string {
	names := make([]string, 0, len(sources))
	for n := range sources {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, src := range names {
		p := sources[src]
		if p == "" || p == targetPerm {
			continue
		}
		if targetPerm == "" {
			return fmt.Sprintf("%q derives from %q, which requires read_permission %q: declare the same read_permission on %q", target, src, p, target)
		}
		return fmt.Sprintf("%q (read_permission %q) derives from %q, which requires a different read_permission %q", target, targetPerm, src, p)
	}
	return ""
}

func columnPerms(cols []Column) map[string]string {
	out := make(map[string]string, len(cols))
	for _, c := range cols {
		out[c.Name] = c.ReadPermission
	}
	return out
}

func sourcesOf(names []string, perms map[string]string) map[string]string {
	out := make(map[string]string, len(names))
	for _, n := range names {
		out[n] = perms[n]
	}
	return out
}

// validateDerivedReadPermissions applies the rule above to every model.
func validateDerivedReadPermissions(m *Manifest) []string {
	byKey := make(map[string]map[string]string, len(m.Models))
	for _, mod := range m.Models {
		p := columnPerms(mod.Columns)
		byKey[mod.Key] = p
		if mod.Table != "" {
			byKey[mod.Table] = p
		}
	}
	var errs []string
	add := func(where, msg string) {
		if msg != "" {
			errs = append(errs, where+": "+msg)
		}
	}
	for mi, mod := range m.Models {
		perms := byKey[mod.Key]
		cols := make(map[string]Column, len(mod.Columns))
		for _, c := range mod.Columns {
			cols[c.Name] = c
		}
		for ci, c := range mod.Columns {
			if strings.TrimSpace(c.Generated) == "" {
				continue
			}
			if ids, err := computeexpr.Idents(c.Generated); err == nil {
				add(fmt.Sprintf("models[%d].columns[%d].generated", mi, ci), DerivedReadPermissionError(c.Name, c.ReadPermission, sourcesOf(ids, perms)))
			}
		}
		for fi, f := range mod.Formulas {
			if f.Tier == 3 || strings.TrimSpace(f.Expr) == "" {
				continue
			}
			if ids, err := computeexpr.Idents(f.Expr); err == nil {
				add(fmt.Sprintf("models[%d].formulas[%d]", mi, fi), DerivedReadPermissionError(f.Target, perms[f.Target], sourcesOf(ids, perms)))
			}
		}
		for ri, rel := range mod.Relations {
			child := byKey[rel.Through]
			for ki, rl := range rel.Rollups {
				var ids []string
				if rl.From != "" {
					ids = []string{rl.From}
				} else if rl.Expr != "" {
					ids, _ = computeexpr.Idents(rl.Expr)
				}
				add(fmt.Sprintf("models[%d].relations[%d].rollups[%d]", mi, ri, ki), DerivedReadPermissionError(rl.Target, perms[rl.Target], sourcesOf(ids, child)))
			}
		}
		for si, sn := range mod.Snapshots {
			src := byKey[refModel(cols[sn.From].Ref)]
			if src == nil {
				continue // another addon's model: its gates are not visible here
			}
			for from, to := range sn.Fields {
				add(fmt.Sprintf("models[%d].snapshots[%d]", mi, si), DerivedReadPermissionError(to, perms[to], map[string]string{from: src[from]}))
			}
		}
		for si, sk := range mod.SearchKeys {
			for _, ph := range searchKeyPlaceholderRe.FindAllStringSubmatch(sk.Format, -1) {
				name := strings.TrimSpace(ph[1])
				if p := perms[name]; p != "" {
					errs = append(errs, fmt.Sprintf("models[%d].search_keys[%d]: {%s} requires read_permission %q; a search key cannot compose a gated column (the list search would match it for every reader)", mi, si, name, p))
				}
			}
		}
	}
	return errs
}

// refModel returns the model segment of a column ref ("addon.Model",
// "Model" or "table").
func refModel(ref string) string {
	if i := strings.LastIndex(ref, "."); i >= 0 {
		return ref[i+1:]
	}
	return ref
}
