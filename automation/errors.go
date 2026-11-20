package automation

import (
	"errors"
	"fmt"
)

// Domain errors. Handlers map them to HTTP status codes; agent tools turn them
// into short Spanish messages for the person.
var (
	ErrInvalid      = errors.New("invalid automation")
	ErrNotFound     = errors.New("automation not found")
	ErrForbidden    = errors.New("you do not have permission for this automation")
	ErrLimit        = errors.New("automation limit reached")
	ErrConflict     = errors.New("the automation changed; reload and try again")
	ErrBadState     = errors.New("the automation does not allow that action in its current state")
	ErrBudget       = errors.New("automation budget exceeded")
	ErrDisabled     = errors.New("automations are disabled")
	errPermanentRun = errors.New("fallo permanente de la corrida")
)

// invalid wraps ErrInvalid with a detail.
func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}
