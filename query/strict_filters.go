package query

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"time"
)

// WithLenientFilters restores the pre-strict behaviour for ONE release:
// filters that cannot be applied are dropped (as Apply always does) instead
// of failing CheckFilters, and each dropped filter is logged as
// `filter.dropped`. Deprecated on arrival: remove once clients stop sending
// garbage filters.
func WithLenientFilters() Option {
	return func(b *Builder) { b.lenientFilters = true }
}

// CheckFilters reports whether every filter in params would actually be
// applied by Apply / ApplyForCount / ApplyForAggregate. It returns an error
// wrapping ErrUnknownFilter naming the first offending filter (sorted, so the
// message is stable). In lenient mode (WithLenientFilters) it never fails and
// logs each dropped filter as `filter.dropped` instead.
//
// Call it before Apply: Apply keeps its signature and still drops silently.
// Empty values (`f_col=`) are treated as "no filter", not as an error.
func (b *Builder) CheckFilters(params Params) error {
	problems := b.filterProblems(params)
	if len(problems) == 0 {
		return nil
	}
	if b.lenientFilters {
		for _, p := range problems {
			log.Printf("filter.dropped: %s", p)
		}
		return nil
	}
	return fmt.Errorf("%w: %s", ErrUnknownFilter, strings.Join(problems, "; "))
}

func (b *Builder) filterProblems(params Params) []string {
	var out []string
	cols := make([]string, 0, len(params.Filters))
	for c := range params.Filters {
		cols = append(cols, c)
	}
	sort.Strings(cols)
	for _, col := range cols {
		if _, ok := b.allowed[col]; !ok || !isSafeIdent(col) {
			out = append(out, fmt.Sprintf("unknown column %q", col))
			continue
		}
		if msg := filterValueProblem(params.Filters[col]); msg != "" {
			out = append(out, fmt.Sprintf("column %q: %s", col, msg))
		}
	}
	for _, rf := range params.RelationFilters {
		name := rf.Relation + "." + rf.Field
		if ext, ok := b.extensions[rf.Relation]; ok {
			if _, ok := ext.Columns[rf.Field]; !ok || !isSafeIdent(rf.Field) {
				out = append(out, fmt.Sprintf("unknown extension field %q", name))
				continue
			}
		} else if rel, ok := b.relations[rf.Relation]; !ok {
			out = append(out, fmt.Sprintf("unknown relation %q", name))
			continue
		} else if rel.Through == "" || rel.ForeignKey == "" || !isSafeIdent(rf.Field) ||
			strings.EqualFold(rel.Kind, "many_to_many") {
			out = append(out, fmt.Sprintf("relation %q cannot be filtered", name))
			continue
		}
		if msg := filterValueProblem(Filter{Op: rf.Op, Value: rf.Value}); msg != "" {
			out = append(out, fmt.Sprintf("relation %q: %s", name, msg))
		}
	}
	return out
}

// filterValueProblem mirrors applyOneFilterMode: it returns a reason when the
// (op, value) pair would be dropped for being malformed. An empty string value
// is "no filter" and passes.
func filterValueProblem(f Filter) string {
	str := func() string {
		if _, ok := f.Value.(string); !ok {
			return fmt.Sprintf("operator %q needs a string value", f.Op)
		}
		return ""
	}
	list := func() string {
		if _, ok := f.Value.([]string); !ok {
			return fmt.Sprintf("operator %q needs a list value", f.Op)
		}
		return ""
	}
	num := func() string {
		if _, ok := f.Value.(float64); !ok {
			return fmt.Sprintf("operator %q needs a numeric value", f.Op)
		}
		return ""
	}
	switch f.Op {
	case OpEq, OpNeq, OpIlike, OpGte, OpLte, OpLike, OpUnaccentIlike:
		return str()
	case OpIn, OpNotIn:
		return list()
	case OpGt, OpLt, OpNumGte, OpNumLte:
		return num()
	case OpRange:
		if _, ok := f.Value.([2]string); !ok {
			return "range needs two bounds"
		}
	case OpNumRange:
		if _, ok := f.Value.([2]*float64); !ok {
			return "num_range needs two numeric bounds"
		}
	case OpDateRange:
		if _, ok := f.Value.([2]time.Time); !ok {
			return "date range needs two valid dates"
		}
	case OpNull, OpNotNull:
	case OpJSONBEq:
		if jf, ok := f.Value.(JSONBFilter); !ok || jf.Key == "" {
			return "jsonb filter needs a key"
		}
	default:
		return fmt.Sprintf("unsupported operator %q", f.Op)
	}
	return ""
}
