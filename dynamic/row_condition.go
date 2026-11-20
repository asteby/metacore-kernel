package dynamic

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
	"github.com/asteby/metacore-kernel/modelbase"
)

// Per-row conditions as data (manifest v3 models[].row_conditions and
// actions[].condition).
//
// Both gates are evaluated by the SERVER against the persisted row, so hiding a
// button in the UI is never the only protection:
//
//   - Service.ExecAction refuses an action whose target row does not satisfy the
//     action's `condition` (ErrActionConditionNotMet, HTTP 409, code
//     "action_condition_not_met");
//   - Service.Update and Service.Delete refuse a row that does not satisfy the
//     model's row_conditions.update / .delete (ErrRowConditionFailed, HTTP 409,
//     code "row_condition_failed", carrying the declared error_key).
//
// The predicate vocabulary is v3.RowCondition (eq neq in not_in gt gte lt lte
// truthy falsy, all/any groups), the same the SDK renders for ActionCondition.

// ErrRowConditionFailed is returned by Update/Delete when the persisted row does
// not satisfy the model's row_conditions gate for the operation.
var ErrRowConditionFailed = errors.New("row_condition_failed")

// ErrActionConditionNotMet is returned by ExecAction when the target row does
// not satisfy the action's condition.
var ErrActionConditionNotMet = errors.New("action_condition_not_met")

// RowConditionError is the typed form of ErrRowConditionFailed.
type RowConditionError struct {
	Model string
	Op    string // "update" | "delete"
	// ErrorKey is the i18n key declared by the gate ("" = generic).
	ErrorKey string
}

func (e *RowConditionError) Error() string {
	return fmt.Sprintf("row_condition_failed: %s of %s is not allowed on this record", e.Op, e.Model)
}

// Unwrap ties the typed error to the sentinel.
func (e *RowConditionError) Unwrap() error { return ErrRowConditionFailed }

// ActionConditionError is the typed form of ErrActionConditionNotMet.
type ActionConditionError struct {
	Model  string
	Action string
}

func (e *ActionConditionError) Error() string {
	return fmt.Sprintf("action_condition_not_met: action %q does not apply to this %s record", e.Action, e.Model)
}

// Unwrap ties the typed error to the sentinel.
func (e *ActionConditionError) Unwrap() error { return ErrActionConditionNotMet }

// RowConditionResolver returns the row_conditions declared for a model. Hosts
// wire it from their addon registry (the kernel owns no global model index),
// like AppendOnlyResolver. nil, or (nil, false), leaves the model ungated.
type RowConditionResolver func(ctx context.Context, model string) (*v3.RowConditions, bool)

// refuseIfRowCondition loads the persisted row and refuses op ("update" |
// "delete") when the model's gate does not hold for it. A gate that cannot be
// checked (row not loadable) fails closed with the load error; a row that does
// not exist is left to the operation itself (it answers not-found).
func (s *Service) refuseIfRowCondition(ctx context.Context, model, op string, user modelbase.AuthUser, id uuid.UUID) error {
	if s.rowConditions == nil {
		return nil
	}
	rc, ok := s.rowConditions(ctx, model)
	if !ok {
		return nil
	}
	gate := rc.Gate(op)
	if gate.IsEmpty() {
		return nil
	}
	if _, system := user.(systemPrincipal); system {
		return nil
	}
	row, err := s.getRow(ctx, model, user, id)
	if err != nil {
		if errors.Is(err, ErrRecordNotFound) {
			return nil
		}
		return err
	}
	if gate.RowCondition.Eval(row) {
		return nil
	}
	return &RowConditionError{Model: model, Op: op, ErrorKey: gate.ErrorKey}
}
