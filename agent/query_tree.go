package agent

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// QueryTree is the strict JSON read request of the semantic catalog:
//
//	{model, fields[], where:[[field, op, value]], order, limit, offset,
//	 group_by[], aggregates[{fn, field, as}], search}
//
// One shape covers lookups, filtered lists, counts and grouped totals, so the
// model needs a single tool call for any read.
type QueryTree struct {
	Model      string           `json:"model"`
	Fields     []string         `json:"fields,omitempty"`
	Where      []QueryCondition `json:"where,omitempty"`
	Order      string           `json:"order,omitempty"`
	Limit      int              `json:"limit,omitempty"`
	Offset     int              `json:"offset,omitempty"`
	GroupBy    []string         `json:"group_by,omitempty"`
	Aggregates []QueryAggregate `json:"aggregates,omitempty"`
	// Search is free text matched (case-insensitive substring) over the
	// model's search columns.
	Search string `json:"search,omitempty"`
}

// QueryCondition is one [field, op, value] predicate.
type QueryCondition struct {
	Field string
	Op    string
	Value any
}

// MarshalJSON renders the condition as its [field, op, value] tuple.
func (c QueryCondition) MarshalJSON() ([]byte, error) {
	if c.Op == "is null" || c.Op == "is not null" {
		return json.Marshal([]any{c.Field, c.Op})
	}
	return json.Marshal([]any{c.Field, c.Op, c.Value})
}

// UnmarshalJSON accepts the tuple form and the object form {field, op, value}.
func (c *QueryCondition) UnmarshalJSON(b []byte) error {
	var tuple []any
	if err := json.Unmarshal(b, &tuple); err == nil {
		return c.fromTuple(tuple)
	}
	var obj struct {
		Field string `json:"field"`
		Op    string `json:"op"`
		Value any    `json:"value"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		return fmt.Errorf("where: cada condición es [campo, op, valor]")
	}
	c.Field, c.Op, c.Value = obj.Field, obj.Op, obj.Value
	return nil
}

func (c *QueryCondition) fromTuple(t []any) error {
	if len(t) < 2 || len(t) > 3 {
		return fmt.Errorf("where: cada condición es [campo, op, valor] (recibí %d elementos)", len(t))
	}
	f, ok1 := t[0].(string)
	op, ok2 := t[1].(string)
	if !ok1 || !ok2 {
		return fmt.Errorf("where: campo y op deben ser texto: %v", t)
	}
	c.Field, c.Op = f, op
	if len(t) == 3 {
		c.Value = t[2]
	}
	return nil
}

// QueryAggregate is one aggregate column: fn(field) AS as.
type QueryAggregate struct {
	Fn    string `json:"fn"`
	Field string `json:"field,omitempty"`
	As    string `json:"as,omitempty"`
}

// Query tree limits.
const (
	DefaultQueryTreeLimit = 20
	MaxQueryTreeLimit     = 200
)

// QueryOps are the accepted where operators.
var QueryOps = []string{"=", "!=", ">", ">=", "<", "<=", "in", "not in", "like", "ilike", "between", "is null", "is not null"}

// QueryAggregateFns are the accepted aggregate functions.
var QueryAggregateFns = []string{"count", "count_distinct", "sum", "avg", "min", "max"}

// ParseQueryTree reads a QueryTree from tool params (the decoded JSON of the
// tool call). Unknown keys are rejected so typos surface instead of being
// silently ignored.
func ParseQueryTree(params map[string]any) (QueryTree, error) {
	allowed := map[string]bool{"model": true, "fields": true, "where": true, "order": true, "limit": true, "offset": true, "group_by": true, "aggregates": true, "search": true}
	var unknown []string
	clean := map[string]any{}
	for k, v := range params {
		if !allowed[k] {
			unknown = append(unknown, k)
			continue
		}
		if v != nil {
			clean[k] = v
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return QueryTree{}, fmt.Errorf("parámetros desconocidos: %s (usa model, fields, where, order, limit, offset, group_by, aggregates, search)", strings.Join(unknown, ", "))
	}
	// Models sometimes send a single string where a list is expected.
	for _, k := range []string{"fields", "group_by"} {
		if s, ok := clean[k].(string); ok {
			clean[k] = splitCSV(s)
		}
	}
	if s, ok := clean["where"].(string); ok {
		var w any
		if err := json.Unmarshal([]byte(s), &w); err != nil {
			return QueryTree{}, fmt.Errorf("where debe ser una lista de [campo, op, valor]")
		}
		clean["where"] = w
	}
	// A single condition instead of a list of conditions.
	if w, ok := clean["where"].([]any); ok && len(w) > 0 {
		if _, isStr := w[0].(string); isStr {
			clean["where"] = []any{w}
		}
	}
	b, err := json.Marshal(clean)
	if err != nil {
		return QueryTree{}, err
	}
	var t QueryTree
	if err := json.Unmarshal(b, &t); err != nil {
		return QueryTree{}, fmt.Errorf("consulta inválida: %v", err)
	}
	return t, nil
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// QueryTreeError lists everything wrong with a query, each with a fix.
type QueryTreeError struct {
	Model    string
	Problems []string
}

func (e *QueryTreeError) Error() string {
	return "consulta inválida en " + e.Model + ": " + strings.Join(e.Problems, "; ")
}

// ValidateQueryTree checks t against the model's catalog metadata and returns
// the normalized tree: defaults applied (limit 20, max 200), field names
// matched case-insensitively, a field given by its label or without its _id
// suffix rewritten to the real name when the match is unambiguous, and enum
// labels mapped to their values. Everything else that is wrong comes back in
// one *QueryTreeError with "¿quisiste decir …?" suggestions.
func ValidateQueryTree(m CatalogModel, t QueryTree) (QueryTree, error) {
	out := t
	out.Model = m.Name
	var problems []string
	known := map[string]CatalogField{"id": {Name: "id", Type: "id"}, "created_at": {Name: "created_at", Type: "dt"}, "updated_at": {Name: "updated_at", Type: "dt"}}
	for _, f := range m.Fields {
		known[f.Name] = f
	}
	resolve := func(raw, use string) (CatalogField, bool) {
		f, ok, problem := resolveField(m, known, raw)
		if !ok {
			problems = append(problems, use+": "+problem)
		}
		return f, ok
	}

	out.Fields = nil
	for _, raw := range t.Fields {
		if f, ok := resolve(raw, "fields"); ok {
			out.Fields = appendUnique(out.Fields, f.Name)
		}
	}

	out.Where = nil
	for _, c := range t.Where {
		f, ok := resolve(c.Field, "where")
		if !ok {
			continue
		}
		nc, problem := normalizeCondition(f, c)
		if problem != "" {
			problems = append(problems, "where "+f.Name+": "+problem)
			continue
		}
		out.Where = append(out.Where, nc)
	}

	out.GroupBy = nil
	for _, raw := range t.GroupBy {
		if f, ok := resolve(raw, "group_by"); ok {
			out.GroupBy = appendUnique(out.GroupBy, f.Name)
		}
	}

	aliases := map[string]bool{}
	out.Aggregates = nil
	for _, a := range t.Aggregates {
		fn := strings.ToLower(strings.TrimSpace(a.Fn))
		if !containsString(QueryAggregateFns, fn) {
			problems = append(problems, fmt.Sprintf("aggregates: función %q no soportada (usa %s)", a.Fn, strings.Join(QueryAggregateFns, ", ")))
			continue
		}
		na := QueryAggregate{Fn: fn, As: strings.TrimSpace(a.As)}
		field := strings.TrimSpace(a.Field)
		if field == "*" {
			field = ""
		}
		if field == "" && fn != "count" {
			problems = append(problems, fmt.Sprintf("aggregates: %s necesita field", fn))
			continue
		}
		if field != "" {
			f, ok := resolve(field, "aggregates")
			if !ok {
				continue
			}
			if (fn == "sum" || fn == "avg") && !isNumericField(f) {
				problems = append(problems, fmt.Sprintf("aggregates: %s(%s) necesita un campo numérico (%s es %s)", fn, f.Name, f.Name, cardTypeOf(f)))
				continue
			}
			na.Field = f.Name
		}
		if na.As == "" {
			na.As = fn
			if na.Field != "" {
				na.As = fn + "_" + na.Field
			}
		}
		if !isValidField(na.As) {
			problems = append(problems, fmt.Sprintf("aggregates: alias %q inválido (solo letras, números y _)", na.As))
			continue
		}
		aliases[na.As] = true
		out.Aggregates = append(out.Aggregates, na)
	}
	if len(out.GroupBy) > 0 && len(out.Aggregates) == 0 {
		out.Aggregates = []QueryAggregate{{Fn: "count", As: "count"}}
		aliases["count"] = true
	}
	if len(out.Aggregates) > 0 && len(out.Fields) > 0 {
		problems = append(problems, "con aggregates no uses fields: las columnas de salida son group_by + aggregates")
	}

	if order := strings.TrimSpace(t.Order); order != "" {
		var parts []string
		for _, term := range strings.Split(order, ",") {
			fs := strings.Fields(strings.TrimSpace(term))
			if len(fs) == 0 {
				continue
			}
			name, dir := fs[0], "asc"
			if strings.HasPrefix(name, "-") {
				name, dir = name[1:], "desc"
			}
			if len(fs) > 1 {
				dir = strings.ToLower(fs[1])
			}
			if len(fs) > 2 || (dir != "asc" && dir != "desc") {
				problems = append(problems, fmt.Sprintf("order: usa \"campo asc|desc\" (recibí %q)", term))
				continue
			}
			if aliases[name] {
				parts = append(parts, name+" "+dir)
				continue
			}
			f, ok := resolve(name, "order")
			if !ok {
				continue
			}
			if len(out.Aggregates) > 0 && !containsString(out.GroupBy, f.Name) {
				problems = append(problems, fmt.Sprintf("order: con aggregates ordena por un campo de group_by o un alias (%s)", strings.Join(sortedKeys(aliases), ", ")))
				continue
			}
			parts = append(parts, f.Name+" "+dir)
		}
		out.Order = strings.Join(parts, ", ")
	}

	switch {
	case t.Limit <= 0:
		out.Limit = DefaultQueryTreeLimit
	case t.Limit > MaxQueryTreeLimit:
		out.Limit = MaxQueryTreeLimit
	}
	if out.Offset < 0 {
		out.Offset = 0
	}
	out.Search = strings.TrimSpace(t.Search)
	if len(problems) > 0 {
		return out, &QueryTreeError{Model: m.Name, Problems: problems}
	}
	return out, nil
}

// resolveField finds raw among the model fields: exact name, case-folded
// name, then an unambiguous label or "<raw>_id" match. On failure it returns
// the problem text with the closest suggestions.
func resolveField(m CatalogModel, known map[string]CatalogField, raw string) (CatalogField, bool, string) {
	name := strings.TrimSpace(raw)
	if f, ok := known[name]; ok {
		return f, true, ""
	}
	fold := foldText(name)
	var cands []CatalogField
	for _, f := range sortedFields(known) {
		if foldText(f.Name) == fold {
			return f, true, ""
		}
		if (f.Label != "" && foldText(f.Label) == fold) || foldText(f.Name) == fold+"_id" {
			cands = append(cands, f)
		}
	}
	if len(cands) == 1 {
		return cands[0], true, ""
	}
	sugg := suggestFields(m, name, 3)
	msg := fmt.Sprintf("el campo %q no existe en %s", raw, m.Name)
	if len(sugg) > 0 {
		msg += "; ¿quisiste decir " + strings.Join(sugg, " o ") + "?"
	}
	return CatalogField{}, false, msg
}

func sortedFields(known map[string]CatalogField) []CatalogField {
	out := make([]CatalogField, 0, len(known))
	for _, f := range known {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// suggestFields ranks fields by edit distance of the name and of the label
// (and by token overlap), formatted as "`partner_id` (Cliente)".
func suggestFields(m CatalogModel, raw string, n int) []string {
	q := foldText(strings.TrimSpace(raw))
	qStem := strings.Join(catalogStems(raw), " ")
	type cand struct {
		f    CatalogField
		dist int
	}
	var cs []cand
	for _, f := range m.Fields {
		d := editDistance(q, foldText(f.Name))
		if f.Label != "" {
			d = min(d, editDistance(q, foldText(f.Label)))
			if qStem != "" && strings.Contains(strings.Join(catalogStems(f.Label), " "), qStem) {
				d = min(d, 1)
			}
		}
		if qStem != "" && strings.Contains(strings.Join(catalogStems(f.Name), " "), qStem) {
			d = min(d, 1)
		}
		limit := max(2, len([]rune(q))/3)
		if d <= limit {
			cs = append(cs, cand{f, d})
		}
	}
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].dist != cs[j].dist {
			return cs[i].dist < cs[j].dist
		}
		return cs[i].f.Name < cs[j].f.Name
	})
	var out []string
	for i, c := range cs {
		if i >= n {
			break
		}
		s := "`" + c.f.Name + "`"
		if c.f.Label != "" {
			s += " (" + c.f.Label + ")"
		}
		out = append(out, s)
	}
	return out
}

// normalizeCondition validates op and value against the field and maps enum
// labels to values. It returns the problem text on failure.
func normalizeCondition(f CatalogField, c QueryCondition) (QueryCondition, string) {
	op := strings.ToLower(strings.Join(strings.Fields(c.Op), " "))
	switch op {
	case "==", "eq":
		op = "="
	case "<>", "ne", "neq":
		op = "!="
	case "gt":
		op = ">"
	case "gte":
		op = ">="
	case "lt":
		op = "<"
	case "lte":
		op = "<="
	case "not_in", "nin":
		op = "not in"
	case "is_null", "isnull":
		op = "is null"
	case "is_not_null", "not null", "notnull":
		op = "is not null"
	}
	if !containsString(QueryOps, op) {
		return c, fmt.Sprintf("operador %q no soportado (usa %s)", c.Op, strings.Join(QueryOps, " "))
	}
	nc := QueryCondition{Field: f.Name, Op: op, Value: c.Value}
	switch op {
	case "is null", "is not null":
		nc.Value = nil
		return nc, ""
	case "=", "!=":
		if c.Value == nil {
			if op == "=" {
				nc.Op = "is null"
			} else {
				nc.Op = "is not null"
			}
			return nc, ""
		}
	case "in", "not in":
		list, ok := asList(c.Value)
		if !ok || len(list) == 0 {
			return c, op + " necesita una lista no vacía de valores"
		}
		for i, v := range list {
			nv, problem := enumValue(f, v)
			if problem != "" {
				return c, problem
			}
			list[i] = nv
		}
		nc.Value = list
		return nc, ""
	case "between":
		list, ok := asList(c.Value)
		if !ok || len(list) != 2 {
			return c, "between necesita [desde, hasta]"
		}
		if isNumericField(f) {
			for i := range list {
				list[i] = numberish(list[i])
			}
		}
		nc.Value = list
		return nc, ""
	case "like", "ilike":
		s, ok := c.Value.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return c, op + " necesita texto"
		}
		if f.Rel != "" || len(f.Enum) > 0 || isNumericField(f) || f.Type == "bool" || f.Type == "id" {
			return c, fmt.Sprintf("%s solo aplica a campos de texto (%s es %s)", op, f.Name, cardTypeOf(f))
		}
		if !strings.ContainsAny(s, "%_") {
			s = "%" + s + "%"
		}
		nc.Value = s
		return nc, ""
	}
	if _, isList := asList(c.Value); isList {
		return c, op + " necesita un solo valor (para varios usa in)"
	}
	nv, problem := enumValue(f, c.Value)
	if problem != "" {
		return c, problem
	}
	if isNumericField(f) {
		nv = numberish(nv)
	}
	nc.Value = nv
	return nc, ""
}

// enumValue maps v onto an enum value of f (accepting the label), or returns
// the problem with the valid values.
func enumValue(f CatalogField, v any) (any, string) {
	if len(f.Enum) == 0 {
		return v, ""
	}
	s := strings.TrimSpace(fmt.Sprint(v))
	fs := foldText(s)
	for _, o := range f.Enum {
		if o.Value == s {
			return o.Value, ""
		}
	}
	for _, o := range f.Enum {
		if foldText(o.Value) == fs || (o.Label != "" && foldText(o.Label) == fs) {
			return o.Value, ""
		}
	}
	vals := make([]string, 0, len(f.Enum))
	for _, o := range f.Enum {
		e := o.Value
		if o.Label != "" && o.Label != o.Value {
			e += "=" + o.Label
		}
		vals = append(vals, e)
	}
	return nil, fmt.Sprintf("valor %q no válido; usa uno de: %s", s, strings.Join(vals, ", "))
}

func asList(v any) ([]any, bool) {
	switch t := v.(type) {
	case []any:
		return append([]any(nil), t...), true
	case []string:
		out := make([]any, len(t))
		for i, s := range t {
			out[i] = s
		}
		return out, true
	}
	return nil, false
}

func isNumericField(f CatalogField) bool {
	switch f.Type {
	case "int", "num", "money":
		return f.Rel == "" && len(f.Enum) == 0
	}
	return false
}

func cardTypeOf(f CatalogField) string {
	switch {
	case f.Rel == "m2o":
		return "->" + f.Ref
	case f.Rel != "":
		return "relación"
	case len(f.Enum) > 0:
		return "enum"
	}
	return f.Type
}

func appendUnique(list []string, s string) []string {
	if containsString(list, s) {
		return list
	}
	return append(list, s)
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ToQueryRequest converts a validated tree into the legacy QueryRequest (the
// equality-filter port of CRUDService.Query). It fails when the tree needs
// more than that port can express; CRUDService.QueryTree runs any tree.
func (t QueryTree) ToQueryRequest() (QueryRequest, error) {
	if len(t.Aggregates) > 0 || len(t.GroupBy) > 0 {
		return QueryRequest{}, fmt.Errorf("query tree with aggregates: use ToAggregateRequest or CRUDService.QueryTree")
	}
	if t.Offset > 0 || len(t.Fields) > 0 {
		return QueryRequest{}, fmt.Errorf("query tree with fields/offset needs CRUDService.QueryTree")
	}
	req := QueryRequest{Model: t.Model, Search: t.Search, Limit: t.Limit}
	if strings.Contains(t.Order, ",") {
		return QueryRequest{}, fmt.Errorf("query tree with several order terms needs CRUDService.QueryTree")
	}
	req.OrderBy = t.Order
	filters, err := equalityFilters(t.Where)
	if err != nil {
		return QueryRequest{}, err
	}
	req.Filters = filters
	return req, nil
}

// ToAggregateRequest converts a validated tree with one aggregate and at most
// one group_by into the legacy AggregateRequest.
func (t QueryTree) ToAggregateRequest() (AggregateRequest, error) {
	if len(t.Aggregates) != 1 || len(t.GroupBy) > 1 {
		return AggregateRequest{}, fmt.Errorf("legacy aggregate port takes one aggregate and one group_by; use CRUDService.QueryTree")
	}
	a := t.Aggregates[0]
	if a.Fn == "count_distinct" {
		return AggregateRequest{}, fmt.Errorf("count_distinct needs CRUDService.QueryTree")
	}
	req := AggregateRequest{Model: t.Model, Function: a.Fn, Field: a.Field}
	if len(t.GroupBy) == 1 {
		req.GroupBy = t.GroupBy[0]
	}
	filters, err := equalityFilters(t.Where)
	if err != nil {
		return AggregateRequest{}, err
	}
	req.Filters = filters
	return req, nil
}

func equalityFilters(where []QueryCondition) (map[string]any, error) {
	if len(where) == 0 {
		return nil, nil
	}
	out := map[string]any{}
	for _, c := range where {
		if c.Op != "=" {
			return nil, fmt.Errorf("operator %q needs CRUDService.QueryTree (legacy port only takes =)", c.Op)
		}
		if _, dup := out[c.Field]; dup {
			return nil, fmt.Errorf("two conditions on %s need CRUDService.QueryTree", c.Field)
		}
		out[c.Field] = c.Value
	}
	return out, nil
}

// numberish converts JSON numbers given as strings ("10") for numeric fields.
func numberish(v any) any {
	if s, ok := v.(string); ok {
		if n, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
			return n
		}
	}
	return v
}
