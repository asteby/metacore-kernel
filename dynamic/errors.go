package dynamic

import (
	"errors"
	"fmt"
)

var (
	ErrModelNotFound        = errors.New("model not found in registry")
	ErrRecordNotFound       = errors.New("record not found")
	ErrForbidden            = errors.New("permission denied")
	ErrInvalidInput         = errors.New("invalid input")
	ErrInvalidID            = errors.New("invalid id")
	ErrNoOptionsConfig      = errors.New("options config not available")
	ErrNoSearchConfig       = errors.New("search config not available")
	ErrOptionsFieldNotFound = errors.New("field not configured for options")
	ErrSourceModelNotFound  = errors.New("dynamic options source model not found")
	ErrFieldRequired        = errors.New("field is required")

	// ErrActionNotFound is returned when the requested action key is not
	// declared on the model's manifest.
	ErrActionNotFound = errors.New("action not found")
	// ErrNoActionResolver signals that the host did not wire an
	// ActionResolver, so action dispatch is disabled.
	ErrNoActionResolver = errors.New("action resolver not configured")
	// ErrUnsupportedTriggerType is returned when an action declares a
	// Trigger.Type the kernel has no dispatcher for.
	ErrUnsupportedTriggerType = errors.New("unsupported trigger type")

	// ErrInvalidTransition is returned when an Update moves a stage-machine
	// model's stage_field to a (from, to) pair that is not one of the model's
	// declared transitions (or when a required on_transition hook declines). The
	// handler maps it to HTTP 422 Unprocessable Entity.
	ErrInvalidTransition = errors.New("invalid stage transition")

	// ErrInvalidState is returned when an action declares RequiresState and the
	// target record's `status` column is not one of the allowed values. The
	// action is gated on the record's lifecycle state, so dispatching it from a
	// disallowed state is rejected before the trigger runs. The handler maps it
	// to HTTP 409 Conflict.
	ErrInvalidState = errors.New("action not allowed in record's current state")

	// ErrConstraintViolation is returned when a declarative column Constraint
	// (guard predicate, e.g. "quantity >= 0") evaluates false during a
	// create/update. The handler maps it to HTTP 422 Unprocessable Entity. The
	// concrete error is a *ConstraintError carrying the offending ErrorKey.
	ErrConstraintViolation = errors.New("constraint violation")
)

// ConstraintError is the typed error a failed declarative guard produces. It
// wraps ErrConstraintViolation (so errors.Is routes it to 422) and carries the
// manifest ErrorKey + the Expr that failed, so the client gets a stable,
// localizable code instead of a raw message.
type ConstraintError struct {
	ErrorKey string
	Expr     string
}

func (e *ConstraintError) Error() string {
	if e == nil {
		return ErrConstraintViolation.Error()
	}
	return fmt.Sprintf("constraint violation (%s): %s", e.ErrorKey, e.Expr)
}

// Unwrap ties the typed error to the ErrConstraintViolation sentinel for
// errors.Is-based HTTP mapping.
func (e *ConstraintError) Unwrap() error { return ErrConstraintViolation }
