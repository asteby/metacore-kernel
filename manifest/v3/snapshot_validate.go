package v3

import (
	"fmt"
	"strings"

	"github.com/asteby/metacore-kernel/manifest/computeexpr"
)

// validateSnapshots checks Model.snapshots: the source column exists and is a
// ref, the source/target identifiers are safe, every target exists on the model,
// is declared protected (a client must never write it) and is mapped once.
func validateSnapshots(mi int, mod Model, ownCols map[string]struct{}) []string {
	var errs []string
	cols := make(map[string]Column, len(mod.Columns))
	for _, c := range mod.Columns {
		cols[c.Name] = c
	}
	seenTarget := map[string]struct{}{}
	for si, sn := range mod.Snapshots {
		where := fmt.Sprintf("models[%d].snapshots[%d]", mi, si)
		from, ok := cols[sn.From]
		switch {
		case strings.TrimSpace(sn.From) == "":
			errs = append(errs, where+".from is empty")
		case !ok:
			errs = append(errs, fmt.Sprintf("%s.from %q is not a column of the model", where, sn.From))
		case strings.TrimSpace(from.Ref) == "":
			errs = append(errs, fmt.Sprintf("%s.from %q has no ref: a snapshot needs the related table", where, sn.From))
		}
		if len(sn.Fields) == 0 {
			errs = append(errs, where+".fields is empty")
		}
		for src, dst := range sn.Fields {
			if !computeexpr.IdentRe.MatchString(src) {
				errs = append(errs, fmt.Sprintf("%s.fields: source column %q is not a valid identifier", where, src))
			}
			t, tok := cols[dst]
			switch {
			case !tok:
				errs = append(errs, fmt.Sprintf("%s.fields: target %q is not a column of the model", where, dst))
			case !t.Protected:
				errs = append(errs, fmt.Sprintf("%s.fields: target %q must be declared protected (a snapshot is system-owned)", where, dst))
			case dst == sn.From:
				errs = append(errs, fmt.Sprintf("%s.fields: target %q is the from column", where, dst))
			}
			if _, dup := seenTarget[dst]; dup {
				errs = append(errs, fmt.Sprintf("%s.fields: target %q is filled by more than one snapshot", where, dst))
			}
			seenTarget[dst] = struct{}{}
		}
	}
	return errs
}

// validateGrant checks a Column.grant binding.
func validateGrant(where string, g *GrantBinding, mod Model, ownCols map[string]struct{}) []string {
	var errs []string
	if strings.TrimSpace(g.Policy) == "" {
		errs = append(errs, where+".policy is empty")
	}
	var sets *Column
	for i := range mod.Columns {
		if mod.Columns[i].Name == g.Sets {
			sets = &mod.Columns[i]
		}
	}
	switch {
	case sets == nil:
		errs = append(errs, fmt.Sprintf("%s.sets %q is not a column of the model", where, g.Sets))
	case !sets.Protected:
		errs = append(errs, fmt.Sprintf("%s.sets %q must be declared protected", where, g.Sets))
	}
	for k, col := range g.ContextEq {
		if strings.TrimSpace(k) == "" {
			errs = append(errs, where+".context_eq has an empty key")
		}
		if _, ok := ownCols[col]; !ok {
			errs = append(errs, fmt.Sprintf("%s.context_eq[%q]: %q is not a column of the model", where, k, col))
		}
	}
	if len(g.ContextGte) == 0 {
		errs = append(errs, where+".context_gte is empty: the grant must cover some amount (otherwise it is never needed)")
	}
	for k, expr := range g.ContextGte {
		if err := validateArithExpr(expr, ownCols); err != nil {
			errs = append(errs, fmt.Sprintf("%s.context_gte[%q] %q: %v", where, k, expr, err))
		}
	}
	return errs
}
