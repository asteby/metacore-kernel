package query

import (
	"fmt"
	"strconv"
	"strings"
)

// Params is the normalized result of parsing a request's query string.
// All fields have safe defaults — zero values are valid and produce the
// documented defaults when fed into Builder methods.
type Params struct {
	// Page is 1-indexed. Defaults to DefaultPage.
	Page int

	// PerPage is clamped to [1, MaxPerPage]. Zero → DefaultPerPage.
	PerPage int

	// SortBy is the column name to order by. Must appear in the bound
	// TableMetadata.Columns — otherwise Builder drops it silently.
	SortBy string

	// Order is "asc" or "desc" (lowercase). Anything else falls back to
	// "desc" inside Builder.
	Order string

	// Search is the free-text search string. Applied against every
	// TableMetadata.SearchColumns entry via ILIKE OR ILIKE OR … .
	Search string

	// Filters is keyed by column name. Keys are whitelisted against
	// TableMetadata.Columns at apply time; unknown keys are dropped.
	Filters map[string]Filter

	// RelationFilters carry "filter the parent by a child column" semantics
	// parsed from `f_<relation>.<field>=<op>:<value>`. Each entry is
	// validated against the builder's declared relations at Apply time —
	// unknown relations are dropped silently (mirrors Filters behaviour).
	//
	// The wire syntax is intentionally a strict superset of Filters: a
	// dot-separated key on the left of "=" becomes a RelationFilter, a
	// flat key stays a Filter. This keeps backward compatibility with
	// every existing host: clients that never use a dot never see the
	// new feature.
	RelationFilters []RelationFilter

	// GroupBy lists column names the SELECT should aggregate over.
	// Whitelisted against TableMetadata.Columns at Apply time. Empty
	// means "no GROUP BY" — the default list query is unchanged.
	GroupBy []string

	// Aggregations are SELECT-list aggregation expressions parsed from
	// `?aggregate=sum(total),count(*),avg(rating)`. Validation against
	// the supported function set happens in ParseFromMap; an unknown
	// function returns ErrUnknownAggregate so the client is told early
	// rather than getting a silently-dropped column.
	Aggregations []Aggregation

	// Preloads lists relation names the builder should pass to
	// db.Preload(...). Each name is validated against the declared
	// relations at Apply time; unknown names are dropped. Parsed from
	// `?with=customer,vehicle`.
	Preloads []string
}

// RelationFilter is one parsed `f_<relation>.<field>=<op>:<value>` directive.
// Relation is the GORM association name (matched against the builder's
// declared RelationDef.Name set); Field is the column on the target table
// to filter against; Op + Value mirror Filter's shape so the same operator
// dispatch is reused.
type RelationFilter struct {
	Relation string
	Field    string
	Op       FilterOp
	Value    interface{}
}

// Aggregation is one parsed entry from `?aggregate=...`. Func is one of
// the supported aggregate constants (AggSum, AggCount, AggAvg, AggMin,
// AggMax); Field is the column name being aggregated, or "*" for
// COUNT(*). Alias is the SELECT alias used in the emitted SQL — e.g.
// `SUM(total) AS sum_total`. ParseFromMap auto-generates an alias when
// the client did not supply one.
type Aggregation struct {
	Func  AggregateFunc
	Field string
	Alias string
}

// AggregateFunc enumerates the SELECT-list aggregate functions the
// builder understands. The string value is the wire token the client
// sends in the `aggregate=` parameter.
type AggregateFunc string

// Supported aggregate function constants. Adding to this set is a
// MAJOR bump because the wire protocol is stable and the SQL emitter
// dispatches on the value.
const (
	AggSum   AggregateFunc = "sum"
	AggCount AggregateFunc = "count"
	AggAvg   AggregateFunc = "avg"
	AggMin   AggregateFunc = "min"
	AggMax   AggregateFunc = "max"
)

// supportedAggregates is the lookup set used by parseAggregate. Kept
// here (not inlined into a switch) so the test suite can assert on its
// membership without reflection.
var supportedAggregates = map[AggregateFunc]struct{}{
	AggSum:   {},
	AggCount: {},
	AggAvg:   {},
	AggMin:   {},
	AggMax:   {},
}

// ParseFromMap parses a url.Values-style map into Params. It accepts the
// canonical parameter names used by host frontends:
//
//	page, per_page, sortBy, order, search, f_<col>
//
// It is intentionally lenient: malformed page/per_page values fall back
// to defaults rather than returning errors, which matches the audited
// behaviour of the source services. The returned error is non-nil only
// for structural problems that no reasonable default can paper over.
func ParseFromMap(values map[string][]string) (Params, error) {
	p := Params{
		Page:    DefaultPage,
		PerPage: DefaultPerPage,
		Filters: map[string]Filter{},
	}

	if v, ok := firstNonEmpty(values, "page"); ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			p.Page = n
		}
	}

	if v, ok := firstNonEmpty(values, "per_page"); ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			p.PerPage = n
		}
	}
	if p.PerPage > MaxPerPage {
		p.PerPage = MaxPerPage
	}

	if v, ok := firstNonEmpty(values, "sortBy"); ok {
		p.SortBy = strings.TrimSpace(v)
	}

	if v, ok := firstNonEmpty(values, "order"); ok {
		o := strings.ToLower(strings.TrimSpace(v))
		if o == "asc" || o == "desc" {
			p.Order = o
		}
	}

	if v, ok := firstNonEmpty(values, "search"); ok {
		s := strings.TrimSpace(v)
		if len(s) > MaxSearchTermLength {
			s = s[:MaxSearchTermLength]
		}
		p.Search = s
	}

	for key, vs := range values {
		if !strings.HasPrefix(key, "f_") {
			continue
		}
		col := strings.TrimPrefix(key, "f_")
		if col == "" || len(vs) == 0 {
			continue
		}
		// Take first non-empty occurrence.
		var raw string
		for _, v := range vs {
			if v != "" {
				raw = v
				break
			}
		}
		if raw == "" {
			continue
		}

		// A dot in the column key promotes the directive from a flat
		// Filter to a RelationFilter — `f_customer.name=Juan` becomes
		// {Relation: "customer", Field: "name", Op: eq, Value: "Juan"}.
		// We split on the FIRST dot only so nested-relation expansion
		// (e.g. f_customer.address.city) can be layered on later
		// without a wire-protocol break.
		if dot := strings.Index(col, "."); dot > 0 && dot < len(col)-1 {
			rel := col[:dot]
			field := col[dot+1:]
			if isSafeIdent(rel) && isSafeIdent(field) {
				f := parseFilterValue(raw)
				p.RelationFilters = append(p.RelationFilters, RelationFilter{
					Relation: rel,
					Field:    field,
					Op:       f.Op,
					Value:    f.Value,
				})
			}
			continue
		}

		p.Filters[col] = parseFilterValue(raw)
	}

	if v, ok := firstNonEmpty(values, "group_by"); ok {
		p.GroupBy = parseCSV(v)
	}

	if v, ok := firstNonEmpty(values, "aggregate"); ok {
		aggs, err := parseAggregateList(v)
		if err != nil {
			return p, err
		}
		p.Aggregations = aggs
	}

	if v, ok := firstNonEmpty(values, "with"); ok {
		p.Preloads = parseCSV(v)
	}

	if p.Page < 1 {
		return p, fmt.Errorf("%w: page must be >= 1", ErrInvalidParam)
	}
	return p, nil
}

// parseCSV splits a comma-separated list and trims each token. Empty
// tokens are dropped so `?group_by=,foo,,bar,` yields ["foo","bar"].
// Used by group_by and with so both parameters share trimming rules.
func parseCSV(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseAggregateList parses the value of `?aggregate=` into a slice of
// Aggregation entries. The wire syntax is a CSV of `func(field)` tokens
// with an optional `:alias` suffix. Examples:
//
//	sum(total)             → {Func: sum, Field: "total", Alias: "sum_total"}
//	count(*)               → {Func: count, Field: "*", Alias: "count"}
//	avg(rating):mean       → {Func: avg, Field: "rating", Alias: "mean"}
//
// Unknown functions return ErrUnknownAggregate; malformed expressions
// (missing parens, empty field) return ErrInvalidAggregate so clients
// learn early.
func parseAggregateList(raw string) ([]Aggregation, error) {
	tokens := splitTopLevelCommas(raw)
	out := make([]Aggregation, 0, len(tokens))
	for _, tok := range tokens {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		agg, err := parseAggregate(tok)
		if err != nil {
			return nil, err
		}
		out = append(out, agg)
	}
	return out, nil
}

// parseAggregate parses one `func(field)[:alias]` token. Kept as a
// free function so the test suite can hit it directly without going
// through ParseFromMap.
func parseAggregate(tok string) (Aggregation, error) {
	// Optional alias suffix, split off first so it does not confuse
	// the paren parser.
	alias := ""
	if colon := strings.LastIndex(tok, ":"); colon > 0 {
		// Only treat ":" as alias if it is OUTSIDE the parens.
		if strings.LastIndex(tok, ")") < colon {
			alias = strings.TrimSpace(tok[colon+1:])
			tok = strings.TrimSpace(tok[:colon])
		}
	}

	open := strings.Index(tok, "(")
	close := strings.LastIndex(tok, ")")
	if open <= 0 || close != len(tok)-1 || close <= open+1 {
		return Aggregation{}, fmt.Errorf("%w: %q", ErrInvalidAggregate, tok)
	}
	fn := AggregateFunc(strings.ToLower(strings.TrimSpace(tok[:open])))
	field := strings.TrimSpace(tok[open+1 : close])
	if field == "" {
		return Aggregation{}, fmt.Errorf("%w: empty field in %q", ErrInvalidAggregate, tok)
	}
	if !isSafeIdent(string(fn)) {
		return Aggregation{}, fmt.Errorf("%w: bad function token %q", ErrInvalidAggregate, fn)
	}
	if _, ok := supportedAggregates[fn]; !ok {
		return Aggregation{}, fmt.Errorf("%w: %q", ErrUnknownAggregate, fn)
	}
	// Validate field as either "*" (legal only for count) or a safe
	// identifier. Builder re-checks at Apply time but we fail loud here.
	if field != "*" && !isSafeIdent(field) {
		return Aggregation{}, fmt.Errorf("%w: unsafe field %q", ErrInvalidAggregate, field)
	}
	if field == "*" && fn != AggCount {
		return Aggregation{}, fmt.Errorf("%w: %q does not accept *", ErrInvalidAggregate, fn)
	}
	if alias == "" {
		alias = defaultAggAlias(fn, field)
	}
	if !isSafeIdent(alias) {
		return Aggregation{}, fmt.Errorf("%w: unsafe alias %q", ErrInvalidAggregate, alias)
	}
	return Aggregation{Func: fn, Field: field, Alias: alias}, nil
}

// defaultAggAlias derives the SELECT alias when the client did not
// supply one. `sum(total)` → `sum_total`; `count(*)` → `count`.
func defaultAggAlias(fn AggregateFunc, field string) string {
	if field == "*" {
		return string(fn)
	}
	return string(fn) + "_" + field
}

// splitTopLevelCommas splits raw on commas that are NOT nested inside
// parentheses. Required so `sum(total),count(*)` does not get split
// inside a future `concat(a, b)` expression — kept paren-aware now to
// avoid churn when that day comes.
func splitTopLevelCommas(raw string) []string {
	var out []string
	depth := 0
	start := 0
	for i, r := range raw {
		switch r {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				out = append(out, raw[start:i])
				start = i + 1
			}
		}
	}
	out = append(out, raw[start:])
	return out
}

// firstNonEmpty returns the first non-empty value for key in values, or
// the empty string and false if the key is absent or every slot is
// empty. Callers use the second return to distinguish "unset" from
// "set to empty string".
func firstNonEmpty(values map[string][]string, key string) (string, bool) {
	vs, ok := values[key]
	if !ok {
		return "", false
	}
	for _, v := range vs {
		if v != "" {
			return v, true
		}
	}
	return "", false
}
