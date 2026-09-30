package wasm

import (
	"fmt"
	"strings"
)

// dataQueryAggregate is the `aggregate` block of a data_query request: grouped
// aggregates over the org-scoped, filtered rows, so a guest can ask "how much
// is overdue per customer" without paging (and clamping to 200) the rows
// itself. It stays a lookup primitive: no joins, no expressions, no HAVING —
// only sum/count/min/max/avg of a column, grouped by up to
// dataQueryMaxGroupBy plain columns.
type dataQueryAggregate struct {
	// GroupBy are the grouping columns (0..3). Empty = one row for the whole
	// filtered set. They appear in each result row under their own name.
	GroupBy []string `json:"group_by"`
	// Select are the aggregates (1..8), each with a unique alias.
	Select []dataQueryAggFn `json:"select"`
}

// dataQueryAggFn is one aggregate: `{fn, col, as}`. `count` may omit `col`
// (count of rows); every other fn requires it.
type dataQueryAggFn struct {
	Fn  string `json:"fn"`
	Col string `json:"col"`
	As  string `json:"as"`
}

const (
	dataQueryMaxGroupBy = 3
	dataQueryMaxAggs    = 8
)

// compiledAggregate is the validated SQL fragments of an aggregate request.
type compiledAggregate struct {
	selectSQL string
	groupSQL  string
	orderSQL  string
}

// compileAggregate validates the aggregate block and renders its SELECT list,
// GROUP BY and ORDER BY. Every identifier is checked against the same
// allowlist regexp as the rest of the import and quoted; the fn name is
// matched against a closed set and never spliced from guest text. sum/avg are
// cast to float8 so the JSON carries a number, not the decimal string the
// driver returns for numeric.
func compileAggregate(a *dataQueryAggregate, orderBy, orderDir string) (*compiledAggregate, error) {
	if len(a.GroupBy) > dataQueryMaxGroupBy {
		return nil, fmt.Errorf("aggregate.group_by accepts at most %d columns", dataQueryMaxGroupBy)
	}
	if len(a.Select) == 0 || len(a.Select) > dataQueryMaxAggs {
		return nil, fmt.Errorf("aggregate.select must hold 1..%d aggregates", dataQueryMaxAggs)
	}
	names := map[string]bool{}
	parts := make([]string, 0, len(a.GroupBy)+len(a.Select))
	groups := make([]string, 0, len(a.GroupBy))
	for _, col := range a.GroupBy {
		if !dataMutateIdentRe.MatchString(col) || dataQueryBlockedWhereCols[col] {
			return nil, fmt.Errorf("invalid aggregate.group_by column %q", col)
		}
		if names[col] {
			return nil, fmt.Errorf("aggregate.group_by repeats %q", col)
		}
		names[col] = true
		parts = append(parts, quoteIdent(col))
		groups = append(groups, quoteIdent(col))
	}
	for i, f := range a.Select {
		fn := strings.ToLower(f.Fn)
		switch fn {
		case "sum", "count", "min", "max", "avg":
		default:
			return nil, fmt.Errorf("aggregate.select[%d].fn must be sum|count|min|max|avg (got %q)", i, f.Fn)
		}
		if !dataMutateIdentRe.MatchString(f.As) {
			return nil, fmt.Errorf("aggregate.select[%d].as must be an identifier (got %q)", i, f.As)
		}
		if names[f.As] {
			return nil, fmt.Errorf("aggregate alias %q collides with another column or alias", f.As)
		}
		names[f.As] = true
		var expr string
		switch {
		case f.Col == "" && fn == "count":
			expr = "COUNT(*)"
		case f.Col == "":
			return nil, fmt.Errorf("aggregate.select[%d]: %s requires col", i, fn)
		case !dataMutateIdentRe.MatchString(f.Col) || dataQueryBlockedWhereCols[f.Col]:
			return nil, fmt.Errorf("aggregate.select[%d]: invalid col %q", i, f.Col)
		case fn == "sum" || fn == "avg":
			expr = fmt.Sprintf("CAST(%s(%s) AS double precision)", strings.ToUpper(fn), quoteIdent(f.Col))
		default:
			expr = fmt.Sprintf("%s(%s)", strings.ToUpper(fn), quoteIdent(f.Col))
		}
		parts = append(parts, expr+" AS "+quoteIdent(f.As))
	}
	out := &compiledAggregate{selectSQL: strings.Join(parts, ", ")}
	if len(groups) > 0 {
		out.groupSQL = " GROUP BY " + strings.Join(groups, ", ")
	}
	if orderBy != "" {
		if !names[orderBy] {
			return nil, fmt.Errorf("order_by %q must be a group_by column or an aggregate alias", orderBy)
		}
		dir := "ASC"
		if strings.EqualFold(orderDir, "desc") {
			dir = "DESC"
		}
		out.orderSQL = " ORDER BY " + quoteIdent(orderBy) + " " + dir
	}
	return out, nil
}
