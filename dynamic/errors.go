package dynamic

import "errors"

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
)
