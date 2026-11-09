package webhookout

import (
	"errors"
	"fmt"
	"net/http"
)

// Codes of an ActionError (RFC-01 §2.3). The host maps them to the action
// response; Status() gives the HTTP status the RFC assigns to each one.
const (
	// CodeRejected: the destination answered a non-2xx that is not retryable
	// (4xx, or a 3xx, which is never followed). The action did NOT succeed.
	CodeRejected = "webhook_rejected"
	// CodeUnavailable: network error, 429 or 5xx after the retries.
	CodeUnavailable = "webhook_unavailable"
	// CodeTimeout: the total budget (handler.timeout_ms) was exhausted.
	CodeTimeout = "webhook_timeout"
	// CodeMisconfigured: the handler, the base URL, the signer or the egress
	// policy do not allow the request. Nothing was sent.
	CodeMisconfigured = "webhook_misconfigured"
	// CodeConfirmMismatch: confirm_typed was not satisfied. Nothing was sent.
	CodeConfirmMismatch = "confirm_mismatch"
)

// Sentinel causes, reachable with errors.Is through ActionError.Unwrap.
var (
	// ErrDestinationBlocked: the destination is loopback/private/link-local or
	// otherwise outside the host's egress policy.
	ErrDestinationBlocked = errors.New("webhookout: destination not allowed")
	// ErrNoSigner: the client has no Signer, so nothing can be sent unsigned.
	ErrNoSigner = errors.New("webhookout: no signer configured")
	// ErrConfirmMismatch: the typed confirmation does not match the record.
	ErrConfirmMismatch = errors.New("webhookout: typed confirmation does not match")
)

// ActionError is the ONLY error an invocation returns: a failed or ambiguous
// call is always an error, never a success. Message is safe to show to the
// user: it never contains response bodies beyond the destination's own short
// message, nor headers, keys or signatures.
type ActionError struct {
	Code    string
	Message string
	// Status is the destination's HTTP status when there was an answer.
	Status int
	// Attempts is how many requests went out (0 when nothing was sent).
	Attempts int
	cause    error
}

func (e *ActionError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("%s: %s (status %d)", e.Code, e.Message, e.Status)
	}
	return e.Code + ": " + e.Message
}

func (e *ActionError) Unwrap() error { return e.cause }

// HTTPStatus is the status the host should answer to its own caller.
func (e *ActionError) HTTPStatus() int {
	switch e.Code {
	case CodeRejected, CodeMisconfigured, CodeConfirmMismatch:
		return http.StatusUnprocessableEntity
	case CodeTimeout:
		return http.StatusGatewayTimeout
	default:
		return http.StatusBadGateway
	}
}

func misconfigured(cause error, format string, a ...any) *ActionError {
	return &ActionError{Code: CodeMisconfigured, Message: fmt.Sprintf(format, a...), cause: cause}
}
