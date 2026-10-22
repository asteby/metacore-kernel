package setupflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Status is the state of a setup step as reported by its handler.
type Status string

const (
	// StatusPending: waiting on something external (DNS propagation, a
	// webhook arriving). The executor polls again.
	StatusPending Status = "pending"
	// StatusRunning: the handler started asynchronous work. Polls again.
	StatusRunning Status = "running"
	// StatusOK: the step is satisfied.
	StatusOK Status = "ok"
	// StatusWarn: satisfied with caveats the user should read; counts as done.
	StatusWarn Status = "warn"
	// StatusError: failed; the user must act (Fix says how). Does not poll.
	StatusError Status = "error"
	// StatusBlocked: cannot even be evaluated until something else is fixed
	// (a prerequisite outside the step). Does not poll.
	StatusBlocked Status = "blocked"
)

// Valid reports whether s is one of the six statuses.
func (s Status) Valid() bool {
	switch s {
	case StatusPending, StatusRunning, StatusOK, StatusWarn, StatusError, StatusBlocked:
		return true
	}
	return false
}

// Settled reports whether the status ends polling.
func (s Status) Settled() bool { return s.Valid() && s != StatusPending && s != StatusRunning }

// Done reports whether the step counts as completed (ok or warn).
func (s Status) Done() bool { return s == StatusOK || s == StatusWarn }

// Limits keep handler output small enough to persist and render.
const (
	MaxMessageKeyLen = 200
	MaxDetailBytes   = 4 << 10
	MaxParamsBytes   = 8 << 10
	MaxRetryAfterS   = 24 * 60 * 60
	MaxIssues        = 50
	MaxMetrics       = 50
)

var messageKeyRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]*$`)

// Fix is a remediation the UI offers next to a failing result. Exactly one of
// Action (a contributions.actions[] key of the addon), Step (a step key of the
// same flow to open) or URL (an https documentation link) is set.
type Fix struct {
	// LabelKey is the i18n key of the button/link text.
	LabelKey string         `json:"label_key"`
	Action   string         `json:"action,omitempty"`
	Step     string         `json:"step,omitempty"`
	URL      string         `json:"url,omitempty"`
	Params   map[string]any `json:"params,omitempty"`
}

// Validate checks the Fix shape.
func (f Fix) Validate() error {
	if !messageKeyRe.MatchString(f.LabelKey) || len(f.LabelKey) > MaxMessageKeyLen {
		return fmt.Errorf("fix.label_key %q is not a valid i18n key", f.LabelKey)
	}
	n := 0
	for _, set := range []bool{f.Action != "", f.Step != "", f.URL != ""} {
		if set {
			n++
		}
	}
	if n != 1 {
		return errors.New("fix needs exactly one of action, step or url")
	}
	if f.URL != "" {
		u, err := url.Parse(f.URL)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("fix.url %q must be an https URL", f.URL)
		}
	}
	return checkParams("fix.params", f.Params)
}

// CheckResult is what a check handler returns (inside the envelope's data).
type CheckResult struct {
	Status Status `json:"status"`
	// MessageKey is the i18n key of the one-line summary; Params fill it.
	MessageKey string         `json:"message_key"`
	Params     map[string]any `json:"params,omitempty"`
	// Detail is free technical text for the "details" disclosure (a resolver
	// answer, an upstream error). Never rendered as the primary message.
	Detail string `json:"detail,omitempty"`
	// RetryAfterS is a hint, in seconds, for the next poll of a pending or
	// running result; the executor clamps it into the poll window.
	RetryAfterS int  `json:"retry_after_s,omitempty"`
	Fix         *Fix `json:"fix,omitempty"`
}

// Validate checks a result a handler returned.
func (r CheckResult) Validate() error {
	if !r.Status.Valid() {
		return fmt.Errorf("status %q must be pending|running|ok|warn|error|blocked", r.Status)
	}
	if !messageKeyRe.MatchString(r.MessageKey) || len(r.MessageKey) > MaxMessageKeyLen {
		return fmt.Errorf("message_key %q is not a valid i18n key", r.MessageKey)
	}
	if len(r.Detail) > MaxDetailBytes {
		return fmt.Errorf("detail exceeds %d bytes", MaxDetailBytes)
	}
	if r.RetryAfterS < 0 || r.RetryAfterS > MaxRetryAfterS {
		return fmt.Errorf("retry_after_s must be between 0 and %d", MaxRetryAfterS)
	}
	if err := checkParams("params", r.Params); err != nil {
		return err
	}
	if r.Fix != nil {
		if r.Status.Done() {
			return errors.New("fix is only meaningful for a non-ok result")
		}
		return r.Fix.Validate()
	}
	return nil
}

func checkParams(field string, p map[string]any) error {
	if len(p) == 0 {
		return nil
	}
	b, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("%s is not JSON-encodable: %w", field, err)
	}
	if len(b) > MaxParamsBytes {
		return fmt.Errorf("%s exceeds %d bytes", field, MaxParamsBytes)
	}
	return nil
}

// Envelope is the kernel handler response {success,data,meta}.
type Envelope struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data,omitempty"`
	Meta    json.RawMessage `json:"meta,omitempty"`
	// Error carries the failure message when Success is false (host handlers
	// use either "error" or "message").
	Error   string `json:"error,omitempty"`
	Message string `json:"message,omitempty"`
}

// ErrHandlerFailed wraps a handler that answered {success:false}.
var ErrHandlerFailed = errors.New("setupflow: handler reported failure")

// unwrap extracts data from an envelope. A bare object (no "success" key) is
// accepted as data so simple native operations can answer without wrapping.
func unwrap(raw []byte) (json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, errors.New("setupflow: empty handler response")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("setupflow: handler response is not a JSON object: %w", err)
	}
	if _, wrapped := probe["success"]; !wrapped {
		return raw, nil
	}
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("setupflow: malformed envelope: %w", err)
	}
	if !env.Success {
		msg := strings.TrimSpace(env.Error + " " + env.Message)
		if msg == "" {
			msg = "no message"
		}
		return nil, fmt.Errorf("%w: %s", ErrHandlerFailed, msg)
	}
	if len(env.Data) == 0 || bytes.Equal(bytes.TrimSpace(env.Data), []byte("null")) {
		return nil, errors.New("setupflow: envelope has no data")
	}
	return env.Data, nil
}

func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data after JSON value")
	}
	return nil
}

// ParseCheckResult decodes and validates a check handler response (envelope or
// bare CheckResult). Unknown fields are rejected so a typo in a handler is
// caught instead of silently dropping the hint.
func ParseCheckResult(raw []byte) (CheckResult, error) {
	data, err := unwrap(raw)
	if err != nil {
		return CheckResult{}, err
	}
	var r CheckResult
	if err := decodeStrict(data, &r); err != nil {
		return CheckResult{}, fmt.Errorf("setupflow: invalid check result: %w", err)
	}
	if err := r.Validate(); err != nil {
		return CheckResult{}, fmt.Errorf("setupflow: invalid check result: %w", err)
	}
	return r, nil
}
