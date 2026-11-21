package nativekit

import (
	"errors"
	"fmt"
	"net/http"
)

// Code is a stable, machine-readable error identifier. Codes are API: they
// reach clients and i18n catalogs, so they never change once published.
// Convention: "<area>.<reason>", lower snake case ("stock.negative").
type Code string

// Generic codes shared by every module.
const (
	CodeInvalid             Code = "invalid"
	CodeNotFound            Code = "not_found"
	CodeConflict            Code = "conflict"
	CodeForbidden           Code = "forbidden"
	CodeInsufficient        Code = "insufficient"
	CodeIdempotencyConflict Code = "idempotency.key_reused"
	CodeIdempotencyInFlight Code = "idempotency.in_flight"
	CodeInternal            Code = "internal"
)

// HTTPStatus maps a code to the HTTP status a handler should answer with.
// Unknown codes are 422 (a business rule said no), never 5xx: only
// CodeInternal is a server fault.
func (c Code) HTTPStatus() int {
	switch c {
	case CodeInvalid:
		return http.StatusBadRequest
	case CodeNotFound:
		return http.StatusNotFound
	case CodeForbidden:
		return http.StatusForbidden
	case CodeConflict, CodeIdempotencyConflict, CodeIdempotencyInFlight:
		return http.StatusConflict
	case CodeInternal:
		return http.StatusInternalServerError
	}
	return http.StatusUnprocessableEntity
}

// Error is a typed business error. Meta carries the numbers behind the
// failure (never personal data) so clients can render a precise message.
type Error struct {
	Code Code
	// Message is an English fallback for logs; user-facing text is resolved
	// from Code through i18n.
	Message string
	Meta    map[string]any
	// Err is an optional wrapped cause.
	Err error
}

// E builds an Error.
func E(code Code, message string, meta ...map[string]any) *Error {
	e := &Error{Code: code, Message: message}
	if len(meta) > 0 {
		e.Meta = meta[0]
	}
	return e
}

// Wrap attaches a cause to a new Error.
func Wrap(code Code, message string, cause error) *Error {
	return &Error{Code: code, Message: message, Err: cause}
}

func (e *Error) Error() string {
	switch {
	case e.Message != "" && e.Err != nil:
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Err)
	case e.Message != "":
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	case e.Err != nil:
		return fmt.Sprintf("%s: %v", e.Code, e.Err)
	}
	return string(e.Code)
}

func (e *Error) Unwrap() error { return e.Err }

// Is makes errors.Is(err, nativekit.E(code, "")) match by code, so callers can
// compare against a sentinel of the code without sharing the instance.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

// CodeOf returns the code of the first *Error in err's chain, or "" when none.
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// HasCode reports whether err carries code.
func HasCode(err error, code Code) bool { return CodeOf(err) == code }

// MetaOf returns the Meta of the first *Error in the chain (nil when none).
func MetaOf(err error) map[string]any {
	var e *Error
	if errors.As(err, &e) {
		return e.Meta
	}
	return nil
}
