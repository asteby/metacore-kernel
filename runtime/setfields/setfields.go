// Package setfields is the host-neutral executor of the declarative
// `set_fields` action handler (RFC-02, docs/rfcs/2026-10-22-hub-primitivos):
//
//	handler: { type: "set_fields", set: { status: "retired", retired_at: "$now",
//	           retired_by_id: "$actor.id", retired_reason: "$payload.reason" } }
//
// It owns the parts that do not depend on a database or an HTTP framework:
// the requires_state gate, resolving the tokens of Handler.Set into concrete
// values and the before/after delta that goes into the action event. The host
// (Ops) wires the result into its own transactional write path; this package
// never writes anything.
//
// Contract (stable): Apply, Resolve, CheckState, Columns, Spec, Input, Result
// and the sentinel errors. Tokens are parsed by v3.ParseSetValue so the
// validator and the runtime can never disagree on what a token is.
package setfields

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// Sentinel errors, matchable with errors.Is. A host maps ErrInvalidState to
// 422 invalid_state and the rest to 422 validation.
var (
	// ErrInvalidState: the row is not in one of the allowed requires_state values.
	ErrInvalidState = errors.New("setfields: record is not in an allowed state")
	// ErrManagedColumn: set targets a kernel-managed column (id,
	// organization_id, created_*, updated_*, deleted_*).
	ErrManagedColumn = errors.New("setfields: managed column cannot be set")
	// ErrMissingValue: a token refers to a payload field, record column or
	// actor that the input does not carry. Fail closed instead of writing NULL.
	ErrMissingValue = errors.New("setfields: token value is missing")
	// ErrEmptySet: Handler.Set has no keys.
	ErrEmptySet = errors.New("setfields: set is empty")
	// ErrBadToken: a set value is not a literal, null or a known token.
	ErrBadToken = errors.New("setfields: invalid set value")
)

// StateError is the detail behind ErrInvalidState.
type StateError struct {
	Field   string
	Current string
	Allowed []string
}

func (e *StateError) Error() string {
	return fmt.Sprintf("setfields: %s=%q is not one of %v", e.Field, e.Current, e.Allowed)
}

// Is makes errors.Is(err, ErrInvalidState) hold.
func (e *StateError) Is(target error) bool { return target == ErrInvalidState }

// Spec is the declarative part of one set_fields action.
type Spec struct {
	// Set is Handler.Set: column -> literal | null | token.
	Set map[string]any
	// RequiresState lists the states (values of StateField) the row must be in.
	// Empty = no gate.
	RequiresState []string
	// StateField is the column that holds the state (the model's stage_field,
	// or "status"). Required when RequiresState is set.
	StateField string
}

// Input is the runtime context a token resolves against. The host fills Now
// and ActorID from the server side: a client can never set them.
type Input struct {
	// Record is the current row (column -> value), already scoped to the org.
	Record map[string]any
	// Payload is the validated action payload (fields/steps).
	Payload map[string]any
	// ActorID is the authenticated actor; resolves $actor.id.
	ActorID string
	// Now resolves $now. Zero = time.Now().UTC().
	Now time.Time
}

// Result is what a host persists and what travels in the action event.
type Result struct {
	// Updates is column -> resolved value to write (exactly the keys of Set).
	Updates map[string]any
	// Before holds the current value of each updated column (nil if absent).
	Before map[string]any
	// After equals Updates, kept as a separate map so an event can carry both
	// without aliasing.
	After map[string]any
}

// Columns returns the sorted column names of set. A host uses it to build the
// WithProtectedWrite allowlist.
func Columns(set map[string]any) []string {
	cols := make([]string, 0, len(set))
	for c := range set {
		cols = append(cols, c)
	}
	sort.Strings(cols)
	return cols
}

// CheckState enforces requires_state: the value of field in record must be one
// of allowed. An empty allowed list always passes.
func CheckState(field string, allowed []string, record map[string]any) error {
	if len(allowed) == 0 {
		return nil
	}
	cur := ""
	if v, ok := record[field]; ok && v != nil {
		cur = fmt.Sprint(v)
	}
	for _, a := range allowed {
		if cur == a {
			return nil
		}
	}
	return &StateError{Field: field, Current: cur, Allowed: append([]string(nil), allowed...)}
}

// Resolve turns Handler.Set into concrete column values. It does not check
// state; use Apply for the full sequence.
func Resolve(set map[string]any, in Input) (map[string]any, error) {
	if len(set) == 0 {
		return nil, ErrEmptySet
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()

	out := make(map[string]any, len(set))
	for _, col := range Columns(set) {
		if v3.ManagedColumn(col) {
			return nil, fmt.Errorf("%w: %q", ErrManagedColumn, col)
		}
		sv, err := v3.ParseSetValue(set[col])
		if err != nil {
			return nil, fmt.Errorf("%w: set.%s: %v", ErrBadToken, col, err)
		}
		switch sv.Kind {
		case v3.SetLiteral:
			out[col] = sv.Literal
		case v3.SetNull:
			out[col] = nil
		case v3.SetNow:
			out[col] = now
		case v3.SetActorID:
			if strings.TrimSpace(in.ActorID) == "" {
				return nil, fmt.Errorf("%w: set.%s needs $actor.id but the caller has no actor", ErrMissingValue, col)
			}
			out[col] = in.ActorID
		case v3.SetPayload:
			v, ok := in.Payload[sv.Name]
			if !ok {
				return nil, fmt.Errorf("%w: set.%s needs $payload.%s", ErrMissingValue, col, sv.Name)
			}
			out[col] = v
		case v3.SetRecord:
			v, ok := in.Record[sv.Name]
			if !ok {
				return nil, fmt.Errorf("%w: set.%s needs $record.%s", ErrMissingValue, col, sv.Name)
			}
			out[col] = v
		}
	}
	return out, nil
}

// Apply runs the executor's pure part in the order RFC-02 §2.2 fixes: the
// requires_state gate first, then token resolution, then the before/after
// delta. The caller performs the write (inside its transaction) with
// Result.Updates and emits the action event with Before/After.
func Apply(spec Spec, in Input) (Result, error) {
	if len(spec.RequiresState) > 0 {
		field := spec.StateField
		if field == "" {
			field = "status"
		}
		if err := CheckState(field, spec.RequiresState, in.Record); err != nil {
			return Result{}, err
		}
	}
	updates, err := Resolve(spec.Set, in)
	if err != nil {
		return Result{}, err
	}
	before := make(map[string]any, len(updates))
	after := make(map[string]any, len(updates))
	for c, v := range updates {
		before[c] = in.Record[c]
		after[c] = v
	}
	return Result{Updates: updates, Before: before, After: after}, nil
}
