package v3

import (
	"fmt"
	"strings"
)

// RowCondition is the ONE declarative per-row predicate of the manifest: data,
// not code. The same shape gates an action (contributions.actions[].condition),
// a printable document and the generic CRUD of a model (models[].row_conditions),
// and the SDK renders it with the same vocabulary (ActionCondition in
// @asteby/metacore-runtime-react), so the button a user does not see is also the
// operation the server refuses.
//
// Leaf: {field, operator, value}. Group: {operator: "all"|"any", conditions}.
//
//	eq | neq            equality (numbers and booleans coerce; "" == missing)
//	in | not_in         membership in the list `value`
//	gt | gte | lt | lte numeric comparison; a non-numeric side never matches
//	truthy | falsy      the field is present (non-blank, not false/0) or not
//	all | any           AND / OR over `conditions` (nestable; an empty list
//	                    passes)
//
// Legacy aliases (equals, ==, notequals, not_equals, !=, notin, present, set,
// blank, empty) are accepted and normalised by NormalizeRowOperator. An empty
// operator on a leaf is eq. `field` may be a dotted path into nested objects.
type RowCondition struct {
	Field      string         `json:"field,omitempty"`
	Operator   string         `json:"operator,omitempty"`
	Value      any            `json:"value,omitempty"`
	Conditions []RowCondition `json:"conditions,omitempty"`
}

// Canonical row-condition operators.
const (
	RowOpEq     = "eq"
	RowOpNeq    = "neq"
	RowOpIn     = "in"
	RowOpNotIn  = "not_in"
	RowOpGt     = "gt"
	RowOpGte    = "gte"
	RowOpLt     = "lt"
	RowOpLte    = "lte"
	RowOpTruthy = "truthy"
	RowOpFalsy  = "falsy"
	RowOpAll    = "all"
	RowOpAny    = "any"
)

// RowOperators is the canonical set, in documentation order (the JSON schema
// enum carries these plus the legacy aliases).
var RowOperators = []string{
	RowOpEq, RowOpNeq, RowOpIn, RowOpNotIn, RowOpGt, RowOpGte, RowOpLt, RowOpLte,
	RowOpTruthy, RowOpFalsy, RowOpAll, RowOpAny,
}

// NormalizeRowOperator maps an operator (canonical or legacy alias, any case)
// to its canonical name; "" is eq. ok is false for an unknown operator.
func NormalizeRowOperator(op string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(op)) {
	case "", "eq", "equals", "==":
		return RowOpEq, true
	case "neq", "notequals", "not_equals", "!=":
		return RowOpNeq, true
	case "in":
		return RowOpIn, true
	case "not_in", "notin":
		return RowOpNotIn, true
	case "gt":
		return RowOpGt, true
	case "gte":
		return RowOpGte, true
	case "lt":
		return RowOpLt, true
	case "lte":
		return RowOpLte, true
	case "truthy", "present", "set":
		return RowOpTruthy, true
	case "falsy", "blank", "empty":
		return RowOpFalsy, true
	case "all":
		return RowOpAll, true
	case "any":
		return RowOpAny, true
	}
	return "", false
}

// IsEmpty reports whether the condition declares no predicate (it then always
// holds).
func (c *RowCondition) IsEmpty() bool {
	return c == nil || (strings.TrimSpace(c.Field) == "" && len(c.Conditions) == 0 &&
		(strings.TrimSpace(c.Operator) == "" || isGroupOp(c.Operator)) && c.Value == nil)
}

func isGroupOp(op string) bool {
	n, _ := NormalizeRowOperator(op)
	return n == RowOpAll || n == RowOpAny
}

// Eval reports whether row satisfies the condition. A nil or empty condition
// holds. An unknown operator, or a leaf without a field, does NOT hold: a gate
// that cannot be evaluated refuses rather than admits (validators reject both
// at install time, this is the runtime backstop).
func (c *RowCondition) Eval(row map[string]any) bool {
	if c.IsEmpty() {
		return true
	}
	op, ok := NormalizeRowOperator(c.Operator)
	if !ok {
		return false
	}
	switch op {
	case RowOpAll:
		for i := range c.Conditions {
			if !c.Conditions[i].Eval(row) {
				return false
			}
		}
		return true
	case RowOpAny:
		if len(c.Conditions) == 0 {
			return true
		}
		for i := range c.Conditions {
			if c.Conditions[i].Eval(row) {
				return true
			}
		}
		return false
	}
	field := strings.TrimSpace(c.Field)
	if field == "" {
		return false
	}
	v := lookupExprPath(row, field)
	switch op {
	case RowOpEq:
		return rowEquals(v, c.Value)
	case RowOpNeq:
		return !rowEquals(v, c.Value)
	case RowOpIn, RowOpNotIn:
		found := false
		for _, item := range asList(c.Value) {
			if rowEquals(v, item) {
				found = true
				break
			}
		}
		return found == (op == RowOpIn)
	case RowOpGt, RowOpGte, RowOpLt, RowOpLte:
		a, aok := toFloat(v)
		b, bok := toFloat(c.Value)
		if !aok || !bok {
			return false
		}
		return orderCmp(map[string]string{RowOpGt: ">", RowOpGte: ">=", RowOpLt: "<", RowOpLte: "<="}[op], a, b)
	case RowOpTruthy:
		return rowTruthy(v)
	case RowOpFalsy:
		return !rowTruthy(v)
	}
	return false
}

func asList(v any) []any {
	switch t := v.(type) {
	case []any:
		return t
	case []string:
		out := make([]any, len(t))
		for i, s := range t {
			out[i] = s
		}
		return out
	case nil:
		return nil
	}
	return []any{v}
}

// rowEquals compares a record value with a declared operand: booleans coerce
// ("true"/"1"), numbers compare numerically when both sides parse, everything
// else as trimmed strings; a null/blank operand matches a missing or blank
// value.
func rowEquals(v, operand any) bool {
	switch o := operand.(type) {
	case nil:
		return isBlank(v)
	case bool:
		b, ok := toBool(v)
		return ok && b == o
	}
	if of, ok := toFloat(operand); ok {
		if _, isStr := operand.(string); !isStr {
			if vf, vok := toFloat(v); vok {
				return vf == of
			}
		}
	}
	return valueString(v) == valueString(operand)
}

// rowTruthy: present means non-blank and not an explicit false/zero.
func rowTruthy(v any) bool {
	if isBlank(v) {
		return false
	}
	switch t := v.(type) {
	case bool:
		return t
	case float64:
		return t != 0
	case int:
		return t != 0
	case int64:
		return t != 0
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "false", "0":
			return false
		}
	}
	return true
}

// Validate checks the shape of the condition; where prefixes each message.
// known, when non-nil, is the set of top-level column names a leaf may read
// (the first segment of a dotted path); pass nil to skip the column check
// (action conditions may read derived or related keys the host injects).
func (c *RowCondition) Validate(where string, known map[string]struct{}) []string {
	if c == nil {
		return nil
	}
	return c.validate(where, known, 0)
}

const maxRowConditionDepth = 6

func (c *RowCondition) validate(where string, known map[string]struct{}, depth int) []string {
	if depth > maxRowConditionDepth {
		return []string{fmt.Sprintf("%s: conditions nested deeper than %d", where, maxRowConditionDepth)}
	}
	op, ok := NormalizeRowOperator(c.Operator)
	if !ok {
		return []string{fmt.Sprintf("%s.operator %q is not one of %s", where, c.Operator, strings.Join(RowOperators, "|"))}
	}
	var errs []string
	if op == RowOpAll || op == RowOpAny {
		if strings.TrimSpace(c.Field) != "" || c.Value != nil {
			errs = append(errs, fmt.Sprintf("%s: a %q group takes only `conditions`, not field/value", where, op))
		}
		for i := range c.Conditions {
			errs = append(errs, c.Conditions[i].validate(fmt.Sprintf("%s.conditions[%d]", where, i), known, depth+1)...)
		}
		return errs
	}
	if len(c.Conditions) > 0 {
		errs = append(errs, fmt.Sprintf("%s.conditions is only allowed with operator all|any", where))
	}
	field := strings.TrimSpace(c.Field)
	if field == "" {
		return append(errs, fmt.Sprintf("%s.field is required", where))
	}
	if !validExprPath(field) {
		errs = append(errs, fmt.Sprintf("%s.field %q is not a column path", where, field))
	} else if known != nil {
		root, _, _ := strings.Cut(field, ".")
		if _, ok := known[root]; !ok {
			errs = append(errs, fmt.Sprintf("%s.field %q is not a column of the model", where, field))
		}
	}
	switch op {
	case RowOpIn, RowOpNotIn:
		if _, isList := c.Value.([]any); !isList {
			errs = append(errs, fmt.Sprintf("%s.value must be a list for %s", where, op))
		}
	case RowOpGt, RowOpGte, RowOpLt, RowOpLte:
		if _, ok := toFloat(c.Value); !ok || c.Value == nil {
			errs = append(errs, fmt.Sprintf("%s.value must be a number for %s", where, op))
		}
	case RowOpTruthy, RowOpFalsy:
		if c.Value != nil {
			errs = append(errs, fmt.Sprintf("%s.value is not allowed for %s", where, op))
		}
	}
	return errs
}

// RowGate is one gated operation of RowConditions: the condition the persisted
// row must satisfy, and the i18n key the refusal carries.
type RowGate struct {
	RowCondition
	// ErrorKey is the i18n key of the refusal message. Empty = a generic one.
	ErrorKey string `json:"error_key,omitempty"`
}

// RowConditions declares, as data, which rows of a model the generic CRUD may
// change or remove (models[].row_conditions): `update: {field: "id",
// operator: "neq", value: 1}` protects a default row, `delete: {field: "debt",
// operator: "lte", value: 0}` forbids deleting a customer that still owes. The
// server evaluates the PERSISTED row before the write and refuses with 409
// (code "row_condition_failed"), whatever the client shows. Create has no
// persisted row and is not gated here. Actions carry their own `condition`.
type RowConditions struct {
	Update *RowGate `json:"update,omitempty"`
	Delete *RowGate `json:"delete,omitempty"`
}

// Gate returns the gate of op ("update" | "delete"), or nil.
func (r *RowConditions) Gate(op string) *RowGate {
	if r == nil {
		return nil
	}
	switch op {
	case "update":
		return r.Update
	case "delete":
		return r.Delete
	}
	return nil
}

// Empty reports whether no operation is gated.
func (r *RowConditions) Empty() bool {
	return r == nil || (r.Update.IsEmpty() && r.Delete.IsEmpty())
}

// IsEmpty reports whether the gate declares no predicate.
func (g *RowGate) IsEmpty() bool { return g == nil || g.RowCondition.IsEmpty() }

// Validate checks both gates against the model's column names.
func (r *RowConditions) Validate(where string, known map[string]struct{}) []string {
	if r == nil {
		return nil
	}
	var errs []string
	for _, op := range []struct {
		name string
		g    *RowGate
	}{{"update", r.Update}, {"delete", r.Delete}} {
		if op.g == nil {
			continue
		}
		if op.g.RowCondition.IsEmpty() {
			errs = append(errs, fmt.Sprintf("%s.%s declares no predicate", where, op.name))
			continue
		}
		errs = append(errs, op.g.RowCondition.Validate(where+"."+op.name, known)...)
	}
	return errs
}

// Row returns the row predicate of a contribution Condition (field/operator/
// value/conditions), or nil when it declares none. The org-level predicates
// (addon_installed, connector_connected) are not part of it.
func (c *Condition) Row() *RowCondition {
	if c == nil {
		return nil
	}
	rc := RowCondition{Field: c.Field, Operator: c.Operator, Value: c.Value, Conditions: c.Conditions}
	if rc.IsEmpty() {
		return nil
	}
	return &rc
}

// RowMatches reports whether row satisfies the Condition's row predicate; a
// Condition without one (or nil) matches.
func (c *Condition) RowMatches(row map[string]any) bool { return c.Row().Eval(row) }

// Row returns the predicate of a RecordFieldCondition, or nil when empty.
func (c *RecordFieldCondition) Row() *RowCondition {
	if c == nil {
		return nil
	}
	rc := RowCondition{Field: c.Field, Operator: c.Operator, Value: c.Value, Conditions: c.Conditions}
	if rc.IsEmpty() {
		return nil
	}
	return &rc
}

// Matches reports whether row satisfies the condition; nil/empty matches.
func (c *RecordFieldCondition) Matches(row map[string]any) bool { return c.Row().Eval(row) }
