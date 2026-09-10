package native

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const (
	OperationsPath  = "/v1/operations"
	MaxPayloadBytes = 4 << 20
)

var operationNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,127}$`)

// Invocation is authored by the host. Context is resolved from trusted
// installation and actor state, never copied from addon or browser input.
type Invocation struct {
	Protocol  string            `json:"protocol"`
	Operation string            `json:"operation"`
	Context   InvocationContext `json:"context"`
	Input     json.RawMessage   `json:"input,omitempty"`
}

type InvocationContext struct {
	OrganizationID string `json:"organization_id"`
	InstallationID string `json:"installation_id"`
	AddonKey       string `json:"addon_key"`
	ActorID        string `json:"actor_id,omitempty"`
	TraceID        string `json:"trace_id"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// InvocationResult is a closed success/error union returned by a sidecar.
type InvocationResult struct {
	Protocol string           `json:"protocol"`
	OK       bool             `json:"ok"`
	Data     json.RawMessage  `json:"data,omitempty"`
	Error    *InvocationError `json:"error,omitempty"`
}

type InvocationError struct {
	Code      string          `json:"code"`
	Message   string          `json:"message"`
	Retryable bool            `json:"retryable,omitempty"`
	Details   json.RawMessage `json:"details,omitempty"`
}

func (i Invocation) Validate() error {
	if i.Protocol != ProtocolV1 {
		return fmt.Errorf("native invocation: protocol must be %q", ProtocolV1)
	}
	if !operationNamePattern.MatchString(i.Operation) {
		return errors.New("native invocation: operation must match ^[a-z][a-z0-9_]{0,127}$")
	}
	if strings.TrimSpace(i.Context.OrganizationID) == "" || strings.TrimSpace(i.Context.InstallationID) == "" || strings.TrimSpace(i.Context.AddonKey) == "" {
		return errors.New("native invocation: organization_id, installation_id and addon_key are required")
	}
	if strings.TrimSpace(i.Context.TraceID) == "" {
		return errors.New("native invocation: trace_id is required")
	}
	if len(i.Input) > MaxPayloadBytes {
		return fmt.Errorf("native invocation: input exceeds %d bytes", MaxPayloadBytes)
	}
	if len(i.Input) != 0 && !json.Valid(i.Input) {
		return errors.New("native invocation: input must be valid JSON")
	}
	return nil
}

func (r InvocationResult) Validate() error {
	if r.Protocol != ProtocolV1 {
		return fmt.Errorf("native invocation result: protocol must be %q", ProtocolV1)
	}
	if len(r.Data) > MaxPayloadBytes || (r.Error != nil && len(r.Error.Details) > MaxPayloadBytes) {
		return fmt.Errorf("native invocation result: payload exceeds %d bytes", MaxPayloadBytes)
	}
	if r.OK {
		if r.Error != nil {
			return errors.New("native invocation result: successful result cannot contain error")
		}
	} else if r.Error == nil || !operationNamePattern.MatchString(r.Error.Code) || strings.TrimSpace(r.Error.Message) == "" {
		return errors.New("native invocation result: failed result requires a valid error code and message")
	}
	if len(r.Data) != 0 && !json.Valid(r.Data) {
		return errors.New("native invocation result: data must be valid JSON")
	}
	if r.Error != nil && len(r.Error.Details) != 0 && !json.Valid(r.Error.Details) {
		return errors.New("native invocation result: error details must be valid JSON")
	}
	return nil
}
