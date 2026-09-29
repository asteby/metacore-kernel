package dynamic

import (
	"errors"
	"fmt"

	"github.com/asteby/metacore-kernel/manifest"
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

	// ErrTenantScopeUnavailable is returned when Config.RequireTenantScope is
	// set and a read could not be constrained to one organization — no user on
	// the request, or a model that declares no organization_id. It wraps
	// ErrForbidden so it answers 403 rather than leaking a cross-tenant result
	// set.
	ErrTenantScopeUnavailable = fmt.Errorf("%w: tenant scope could not be applied to this query", ErrForbidden)

	// ErrPermissionServiceMissing is returned when the host set
	// Config.RequirePermissions but wired no permission service. It wraps
	// ErrForbidden so existing error handling maps it to 403 unchanged: from
	// the caller's side the request is simply not allowed, and the operator
	// sees the cause in the warning New() logs at construction.
	ErrPermissionServiceMissing = fmt.Errorf("%w: no permission service wired and RequirePermissions is set", ErrForbidden)

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

	// ErrValidation is the sentinel a failed pre-write field-validation pass
	// wraps. The handler maps it to HTTP 422 Unprocessable Entity and serializes
	// the concrete *ValidationError's per-field code map. See validate.go.
	ErrValidation = errors.New("validation failed")

	// ErrSingletonExists is the sentinel of SingletonExistsError: a second
	// create on a singleton model. The handler maps it to 409 Conflict.
	ErrSingletonExists = errors.New("singleton_exists")

	// ErrNotSingleton is returned by the /current endpoints for a model that
	// is not a singleton. The handler maps it to 404.
	ErrNotSingleton = errors.New("model is not a singleton")
)

// FieldError is a single locale-agnostic validation failure on one column. The
// SDK localizes Code into a human message; the kernel never emits prose. Params
// carries the code's structured detail (e.g. the allowed enum values for
// invalid_option, the ref target for not_found).
type FieldError struct {
	Code   string         `json:"code"`
	Params map[string]any `json:"params,omitempty"`
	// Message is an optional human message, set by app-authored validation
	// (ValidationError.AddMessage, modelbase.Validatable, modelbase.FieldErrors).
	// Declarative kernel checks leave it empty and the SDK localizes Code.
	Message string `json:"message,omitempty"`
}

// CodeInvalid is the code of an app-authored field error that carries its own
// Message (ValidationError.AddMessage).
const CodeInvalid = "invalid"

// ValidationError is the typed error a failed pre-write validation pass
// produces. It wraps ErrValidation (so errors.Is / errors.As route it to 422)
// and carries a column → failures map. The JSON tag matches the wire contract
// consumed by the SDK: `{ "errors": { "<col>": [ { "code", "params" } ] } }`.
type ValidationError struct {
	Fields map[string][]FieldError `json:"errors"`
}

func (e *ValidationError) Error() string {
	if e == nil {
		return ErrValidation.Error()
	}
	return fmt.Sprintf("validation failed: %d field(s)", len(e.Fields))
}

// Unwrap ties the typed error to the ErrValidation sentinel for errors.Is /
// errors.As-based HTTP mapping.
func (e *ValidationError) Unwrap() error { return ErrValidation }

// Empty reports whether the accumulator holds no field errors — used to decide
// whether the write may proceed.
func (e *ValidationError) Empty() bool { return e == nil || len(e.Fields) == 0 }

// NewValidationError returns an empty accumulator for app-authored
// validation (hooks, custom endpoints, Go models). Returned from a BeforeCreate
// / BeforeUpdate hook it produces the same 422 `{errors: {...}}` body as the
// kernel's declarative validation:
//
//	ve := dynamic.NewValidationError()
//	if price <= 0 {
//	    ve.AddMessage("price", "must be positive")
//	}
//	return ve.Err()
func NewValidationError() *ValidationError { return &ValidationError{} }

// Add accumulates one failure with a locale-agnostic code (see package
// validate for the kernel's codes) under field. Chainable.
func (e *ValidationError) Add(field, code string, params map[string]any) *ValidationError {
	if e.Fields == nil {
		e.Fields = make(map[string][]FieldError)
	}
	e.Fields[field] = append(e.Fields[field], FieldError{Code: code, Params: params})
	return e
}

// AddMessage accumulates a failure carrying a ready-to-show message, under
// the code CodeInvalid. Chainable.
func (e *ValidationError) AddMessage(field, message string) *ValidationError {
	if e.Fields == nil {
		e.Fields = make(map[string][]FieldError)
	}
	e.Fields[field] = append(e.Fields[field], FieldError{Code: CodeInvalid, Message: message})
	return e
}

// Err returns e as an error, or nil when it holds no failure — never a
// non-nil error interface wrapping an empty accumulator.
func (e *ValidationError) Err() error {
	if e.Empty() {
		return nil
	}
	return e
}

// add accumulates one failure under a column, allocating the map lazily so a
// clean pass never touches the heap.
func (e *ValidationError) add(field, code string, params map[string]any) {
	e.Add(field, code, params)
}

// AccessDeniedError is the typed error a model AccessPolicy produces when the
// principal does not satisfy the rule of the requested action. It wraps
// ErrForbidden (403) and the handler serializes Model and Action so the
// client can tell a policy denial from a missing capability.
type AccessDeniedError struct {
	Model  string
	Action string
}

func (e *AccessDeniedError) Error() string {
	return fmt.Sprintf("access denied: %s on %s is not allowed for this user", e.Action, e.Model)
}

// Unwrap ties the typed error to ErrForbidden.
func (e *AccessDeniedError) Unwrap() error { return ErrForbidden }

// SingletonExistsError is returned when a create targets a singleton model
// (modelbase.Singleton) whose row already exists for the organization. It
// wraps ErrSingletonExists (409) and carries the existing row id so the client
// can switch to an update.
type SingletonExistsError struct {
	Model string
	ID    string
}

func (e *SingletonExistsError) Error() string {
	return fmt.Sprintf("%s: %s already has a row for this organization", ErrSingletonExists.Error(), e.Model)
}

// Unwrap ties the typed error to ErrSingletonExists.
func (e *SingletonExistsError) Unwrap() error { return ErrSingletonExists }

// ConstraintError is the typed error a failed declarative guard produces. It
// wraps ErrConstraintViolation (so errors.Is routes it to 422) and carries the
// manifest ErrorKey + the Expr that failed, so the client gets a stable,
// localizable code instead of a raw message.
type ConstraintError struct {
	ErrorKey string
	Expr     string
	// Def is the full guard declaration that failed — Create/Update read its
	// OnViolation / Approval policy to decide between rejecting the write and
	// parking it as an ApprovalRequest (see approvals.go).
	Def manifest.ConstraintDef
	// Values are the identifiers referenced by Expr with the values they had
	// when the predicate failed ({unit_price: 80, min_price: 100}) — stored on
	// the ApprovalRequest so the approver sees WHY without re-evaluating.
	Values map[string]any
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

// RequestsApproval reports whether the failed guard asks for a supervisor
// (on_violation: request_approval) instead of rejecting the write.
func (e *ConstraintError) RequestsApproval() bool {
	return e != nil && e.Def.RequestsApproval()
}

// ViolationMap is the {expr, error_key, values} block persisted on an
// ApprovalRequest raised by this guard.
func (e *ConstraintError) ViolationMap() map[string]any {
	if e == nil {
		return nil
	}
	m := map[string]any{"expr": e.Expr, "error_key": e.ErrorKey}
	if len(e.Values) > 0 {
		m["values"] = e.Values
	}
	return m
}
