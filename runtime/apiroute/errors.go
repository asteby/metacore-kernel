package apiroute

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// Error is a gateway error with its HTTP mapping. The package sentinels are
// *Error values (compare with errors.Is); handlers and dispatchers may return
// their own (NewError) to answer with a specific status and code — e.g. 409
// "insufficient_stock" — without building a full Response.
type Error struct {
	// Status is the HTTP status code.
	Status int
	// Code is the machine-readable error code ("not_found").
	Code string
	// Message is the human-readable message sent to the client. Never put
	// secrets or internal details here.
	Message string
	// Field optionally names the offending input field.
	Field string
	// RetryAfter, when > 0, is sent as Retry-After (seconds).
	RetryAfter int
	// LegacyCode, when set, is the code older clients matched before Code was
	// refined. It is sent as "legacy_code" and errors.Is also matches the
	// sentinel carrying it (an insufficient_scope error still matches
	// ErrForbidden).
	LegacyCode string
	// RequiredScopes lists the scopes the caller lacked (insufficient_scope).
	// They come from the route's public contract, never from the credential.
	RequiredScopes []string
}

// Error implements error.
func (e *Error) Error() string {
	if e.Field != "" {
		return "apiroute: " + e.Code + ": " + e.Message + " (" + e.Field + ")"
	}
	return "apiroute: " + e.Code + ": " + e.Message
}

// Is makes a copy of a sentinel (same Status and Code, e.g. RateLimited(30))
// match it under errors.Is.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	if !ok || t == nil || e == nil || t.Status != e.Status {
		return false
	}
	return t.Code == e.Code || (e.LegacyCode != "" && t.Code == e.LegacyCode)
}

// NewError builds an Error with the given HTTP status, code and message.
func NewError(status int, code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

// RateLimited returns ErrRateLimited carrying a Retry-After of retryAfter
// seconds. errors.Is(err, ErrRateLimited) holds.
func RateLimited(retryAfter int) *Error {
	e := *ErrRateLimited
	e.RetryAfter = retryAfter
	return &e
}

// InsufficientScope returns ErrInsufficientScope carrying the scopes the
// caller lacks (sorted copy). errors.Is matches both ErrInsufficientScope and
// ErrForbidden.
func InsufficientScope(missing ...string) *Error {
	e := *ErrInsufficientScope
	e.RequiredScopes = slices.Clone(missing)
	slices.Sort(e.RequiredScopes)
	return &e
}

// Typed errors so the HTTP layer maps to status codes without string matching.
// Wrap them with fmt.Errorf("%w: detail", ...) to add log context; the client
// only ever sees the sentinel's Code and Message.
var (
	// ErrNotFound — no route matches the path (or the owning addon is not
	// enabled for the organization).
	ErrNotFound = &Error{Status: http.StatusNotFound, Code: "not_found", Message: "route not found"}
	// ErrMethodNotAllowed — the path matches, but not with this method.
	ErrMethodNotAllowed = &Error{Status: http.StatusMethodNotAllowed, Code: "method_not_allowed", Message: "method not allowed"}
	// ErrOrgUnresolved — the request's organization could not be resolved:
	// no org on the principal and no (or an unknown) domain.
	ErrOrgUnresolved = &Error{Status: http.StatusNotFound, Code: "organization_not_found", Message: "organization not found"}
	// ErrUnauthorized — the route needs a principal kind the request lacks.
	ErrUnauthorized = &Error{Status: http.StatusUnauthorized, Code: "unauthorized", Message: "authentication required"}
	// ErrForbidden — authenticated, but missing a required scope.
	ErrForbidden = &Error{Status: http.StatusForbidden, Code: "forbidden", Message: "insufficient scope"}
	// ErrInsufficientScope — authenticated, but the credential lacks scopes
	// the route requires. Answers 403 code "insufficient_scope" with
	// "required_scopes" and, for clients that matched the former code,
	// "legacy_code": "forbidden" (kept for at least one minor release).
	ErrInsufficientScope = &Error{Status: http.StatusForbidden, Code: "insufficient_scope", Message: "insufficient scope", LegacyCode: "forbidden"}
	// ErrPayloadTooLarge — the request body exceeds the gateway limit.
	ErrPayloadTooLarge = &Error{Status: http.StatusRequestEntityTooLarge, Code: "payload_too_large", Message: "request body too large"}
	// ErrRateLimited — the principal exceeded its request budget.
	ErrRateLimited = &Error{Status: http.StatusTooManyRequests, Code: "rate_limited", Message: "too many requests"}
	// ErrBadResponse — the handler returned a malformed response envelope.
	ErrBadResponse = &Error{Status: http.StatusBadGateway, Code: "bad_handler_response", Message: "the route handler returned an invalid response"}
	// ErrHandlerFailed — the dispatcher failed (trap, timeout, missing
	// export) without a typed Error.
	ErrHandlerFailed = &Error{Status: http.StatusBadGateway, Code: "handler_failed", Message: "the route handler failed"}
	// ErrNoDispatcher — the gateway has no dispatcher.
	ErrNoDispatcher = &Error{Status: http.StatusInternalServerError, Code: "internal", Message: "internal error"}
)

// methodNotAllowedError carries the methods the path does accept so
// ErrorResponse can emit Allow.
type methodNotAllowedError struct{ allowed []string }

func (e *methodNotAllowedError) Error() string {
	return ErrMethodNotAllowed.Error() + " (allow: " + strings.Join(e.allowed, ", ") + ")"
}
func (e *methodNotAllowedError) Unwrap() error { return ErrMethodNotAllowed }

// AllowedMethods returns the methods carried by a method-not-allowed error
// from Gateway.Handle, or nil.
func AllowedMethods(err error) []string {
	var m *methodNotAllowedError
	if errors.As(err, &m) {
		return m.allowed
	}
	return nil
}

// ErrorResponse maps an error to the JSON error Response
//
//	{"error": {"code": "not_found", "message": "route not found", "field"?: "..."}}
//
// with Cache-Control: no-store (plus Allow for 405 and Retry-After when set).
// An *Error anywhere in the chain gives status/code/message; anything else is
// a 500 "internal" whose detail is never exposed.
func ErrorResponse(err error) Response {
	e := &Error{Status: http.StatusInternalServerError, Code: "internal", Message: "internal error"}
	var typed *Error
	if errors.As(err, &typed) && typed != nil {
		e = typed
	}
	status := e.Status
	if status < 400 || status > 599 {
		status = http.StatusInternalServerError
	}
	payload := map[string]any{"code": e.Code, "message": e.Message}
	if e.Field != "" {
		payload["field"] = e.Field
	}
	if e.LegacyCode != "" {
		payload["legacy_code"] = e.LegacyCode
	}
	if len(e.RequiredScopes) > 0 {
		payload["required_scopes"] = e.RequiredScopes
	}
	body, _ := json.Marshal(map[string]any{"error": payload})
	headers := map[string]string{
		"Content-Type":  "application/json; charset=utf-8",
		"Cache-Control": "no-store",
	}
	if allowed := AllowedMethods(err); len(allowed) > 0 {
		headers["Allow"] = strings.Join(allowed, ", ")
	}
	if e.RetryAfter > 0 {
		headers["Retry-After"] = strconv.Itoa(e.RetryAfter)
	}
	return Response{Status: status, Headers: headers, Body: body}
}
