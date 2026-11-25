package contract

import (
	"errors"
	"net/http"
)

// Stable error codes of the {"error":{"code","message",...}} envelope. The
// string values are part of the wire contract and never change.
const (
	ErrCodeUnauthorized            = "unauthorized"
	ErrCodeReplay                  = "replay"
	ErrCodeNotPaired               = "not_paired"
	ErrCodeNotLinked               = "not_linked"
	ErrCodeNotPending              = "not_pending"
	ErrCodeCallbackNotConfigured   = "callback_not_configured"
	ErrCodeCallbackMismatch        = "callback_mismatch"
	ErrCodeCallbackInvalid         = "callback_invalid"
	ErrCodeInProgress              = "in_progress"
	ErrCodeContractUpgradeRequired = "contract_upgrade_required"
	ErrCodeInvalidRequest          = "invalid_request"
	ErrCodeNotFound                = "not_found"
	ErrCodeAlreadyResolved         = "already_resolved"
	ErrCodeStateChanged            = "state_changed"
	ErrCodeMessageTooLarge         = "message_too_large"
	ErrCodeRateLimited             = "rate_limited"
	ErrCodeInsufficientSparks      = "insufficient_sparks"
	ErrCodeGitHub                  = "github_error"
	ErrCodeUnavailable             = "unavailable"
	ErrCodeInternal                = "internal"

	// Channel errors.
	ErrCodeOriginNotAllowed    = "origin_not_allowed"
	ErrCodeChannelInactive     = "channel_inactive"
	ErrCodeSessionExpired      = "session_expired"
	ErrCodeIdentityInvalid     = "identity_invalid"
	ErrCodeCaptchaRequired     = "captcha_required"
	ErrCodeChannelQuotaReached = "channel_quota_exceeded"
	ErrCodeChannelAICapReached = "channel_ai_cap_reached"
	ErrCodeDeskNotVerified     = "desk_not_verified"
)

// Reasons that qualify a not_linked error. The code itself never changes so
// old clients keep working.
const (
	ReasonAutolinkOff  = "autolink_off"
	ReasonNoLicense    = "no_license"
	ReasonLinkDisabled = "link_disabled"
)

// Reasons that qualify a channel_inactive error (the channel status reason).
const (
	ReasonChannelOwner            = "owner"
	ReasonChannelLicenseRevoked   = "license_revoked"
	ReasonChannelAbuse            = "abuse"
	ReasonChannelAddonUninstalled = "addon_uninstalled"
	ReasonChannelStaff            = "staff"
)

// ErrorDetail is the body of the error envelope. InstanceID is only filled
// for not_linked; Reason qualifies not_linked and channel_inactive.
type ErrorDetail struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	InstanceID string `json:"instance_id,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// ErrorResponse is the envelope of every error response.
type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

var codeStatus = map[string]int{
	ErrCodeUnauthorized:            http.StatusUnauthorized,
	ErrCodeReplay:                  http.StatusRequestTimeout,
	ErrCodeNotPaired:               http.StatusForbidden,
	ErrCodeNotLinked:               http.StatusForbidden,
	ErrCodeNotPending:              http.StatusConflict,
	ErrCodeCallbackNotConfigured:   http.StatusConflict,
	ErrCodeInProgress:              http.StatusConflict,
	ErrCodeContractUpgradeRequired: http.StatusConflict,
	ErrCodeAlreadyResolved:         http.StatusConflict,
	ErrCodeStateChanged:            http.StatusConflict,
	ErrCodeCallbackMismatch:        http.StatusBadRequest,
	ErrCodeCallbackInvalid:         http.StatusBadRequest,
	ErrCodeInvalidRequest:          http.StatusBadRequest,
	ErrCodeNotFound:                http.StatusNotFound,
	ErrCodeMessageTooLarge:         http.StatusRequestEntityTooLarge,
	ErrCodeRateLimited:             http.StatusTooManyRequests,
	ErrCodeInsufficientSparks:      http.StatusPaymentRequired,
	ErrCodeGitHub:                  http.StatusBadGateway,
	ErrCodeUnavailable:             http.StatusServiceUnavailable,
	ErrCodeInternal:                http.StatusInternalServerError,

	ErrCodeOriginNotAllowed:    http.StatusForbidden,
	ErrCodeChannelInactive:     http.StatusForbidden,
	ErrCodeSessionExpired:      http.StatusUnauthorized,
	ErrCodeIdentityInvalid:     http.StatusUnauthorized,
	ErrCodeCaptchaRequired:     http.StatusPreconditionRequired,
	ErrCodeChannelQuotaReached: http.StatusPaymentRequired,
	ErrCodeChannelAICapReached: http.StatusPaymentRequired,
	ErrCodeDeskNotVerified:     http.StatusConflict,
}

// StatusForCode returns the canonical HTTP status of an error code, or 0 when
// the code is not part of the contract.
func StatusForCode(code string) int { return codeStatus[code] }

// IsKnownErrorCode reports whether code is part of this contract.
func IsKnownErrorCode(code string) bool { _, ok := codeStatus[code]; return ok }

// APIError is an ErrorDetail plus the HTTP status, for clients that turn an
// error response into a Go error.
type APIError struct {
	Status int
	ErrorDetail
}

func (e *APIError) Error() string { return e.Code + ": " + e.Message }

// Is makes errors.Is(err, NewAPIError(0, code, "")) match by Code only.
func (e *APIError) Is(target error) bool {
	var t *APIError
	return errors.As(target, &t) && t.Code != "" && t.Code == e.Code
}

// NewAPIError builds an *APIError; a zero status is filled from the code.
func NewAPIError(status int, code, message string) *APIError {
	if status == 0 {
		status = StatusForCode(code)
	}
	return &APIError{Status: status, ErrorDetail: ErrorDetail{Code: code, Message: message}}
}
