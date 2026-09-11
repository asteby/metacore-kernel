package native

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Event plane: the reverse channel of the metacore.native/v1 control
// protocol. `POST /v1/operations` lets the host call INTO a sidecar; this
// file defines the symmetric contract that lets a sidecar call OUT to the
// host to publish inbound events (for example an inbound WhatsApp message)
// without a bespoke HTTP bridge per addon.
//
// # Transport and identity
//
// The event channel reuses exactly the identity guarantees of the operation
// channel; it does not invent a parallel auth mechanism:
//
//   - The supervisor provisions a SECOND unix_http socket dedicated to
//     inbound events, at the path in EnvEventSocket, together with its own
//     one-time bearer token at EnvEventTokenFile. Manifests cannot choose or
//     see these paths; they are assigned by the host the same way
//     EnvSocket/EnvTokenFile are for operations.
//   - That socket is scoped 1:1 to a single installation. The host resolves
//     organization_id, installation_id and addon_key from WHICH authenticated
//     socket/token the event arrived on — never from the request body. This
//     is why Event carries no tenant/installation/addon fields: a sidecar
//     cannot claim an identity, exactly as a sidecar cannot override the
//     InvocationContext the host authors for `/v1/operations`.
//   - Requests without a valid, current bearer token for that socket MUST be
//     rejected with 401 before the body is parsed.
//
// # Wire contract
//
//	POST /v1/events                      (over the event unix_http socket)
//	Authorization: Bearer <token from EnvEventTokenFile>
//	Content-Type: application/json
//	Content-Length: <= MaxPayloadBytes (4 MiB)
//
//	{
//	  "protocol": "metacore.native/v1",
//	  "type": "whatsapp.message.received",
//	  "trace_id": "0af7651916cd43dd8448eb211c80319c",
//	  "idempotency_key": "wa:device-1:3EB0C767D1F1",
//	  "occurred_at": "2026-09-11T15:04:05Z",
//	  "data": { "...": "addon-defined, opaque to the kernel" }
//	}
//
// Host response is the closed EventAck union:
//
//	202 Accepted   {"protocol":"metacore.native/v1","accepted":true,"duplicate":false}
//	202 Accepted   {"protocol":"metacore.native/v1","accepted":true,"duplicate":true}   (idempotency replay)
//	429 Too Many Requests + Retry-After: <seconds>
//	               {"protocol":"metacore.native/v1","accepted":false,
//	                "error":{"code":"backpressure","message":"...","retryable":true}}
//	400/401/413/422 for validation/auth/size failures, same InvocationError shape.
//
// # Backpressure
//
// The host is never allowed to block or crash a sidecar because it cannot
// keep up. A host implementation MUST bound its inbound queue and, once
// full, respond 429 with Retry-After rather than hanging the connection or
// tearing down the sidecar process. `accepted:false` with a retryable
// BackpressureErrorCode is the only sanctioned way to signal "slow down";
// the sidecar is expected to retry the same idempotency_key later.
//
// # Deduplication
//
// idempotency_key is mandatory (unlike the optional one on operations,
// which are host-initiated and already de-duplicated by the caller). The
// host is the de-duplication authority: replaying the same
// (installation, idempotency_key) pair MUST return `accepted:true,
// duplicate:true` without reprocessing, so an at-least-once sidecar retry
// after a dropped ack never double-publishes to the canonical event bus.
//
// # Traceability
//
// trace_id is mandatory and MUST be propagated end-to-end: sidecar -> host
// receiver -> canonical event bus -> downstream consumer (e.g. link_inbox
// ingestion command), exactly like trace_id already flows through
// Invocation for the operations direction.
const (
	// EventsPath is the sidecar -> host endpoint for publishing events, on
	// the dedicated event socket (see EnvEventSocket).
	EventsPath = "/v1/events"

	// MaxEventPayloadBytes bounds Event.Data. It intentionally matches
	// MaxPayloadBytes so the two directions of the protocol share one limit.
	MaxEventPayloadBytes = MaxPayloadBytes

	// BackpressureErrorCode is the sanctioned EventAck.Error.Code a host
	// uses to reject an event because it cannot keep up right now. It is
	// always retryable.
	BackpressureErrorCode = "backpressure"

	// DuplicateEventErrorCode marks an event rejected as malformed-but-seen;
	// hosts should prefer accepted:true+duplicate:true for a clean replay,
	// this code is only for a duplicate key with a payload mismatch.
	DuplicateEventErrorCode = "idempotency_conflict"
)

var eventTypePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}(\.[a-z][a-z0-9_]{0,63}){1,3}$`)
var idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,200}$`)
var traceIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// Event is authored by the sidecar and carries no tenant/installation/addon
// identity: the host derives that trusted context from the authenticated
// event socket the request arrived on, mirroring how InvocationContext is
// authored by the host rather than trusted from addon input in the
// operations direction.
type Event struct {
	Protocol       string          `json:"protocol"`
	Type           string          `json:"type"`
	TraceID        string          `json:"trace_id"`
	IdempotencyKey string          `json:"idempotency_key"`
	OccurredAt     time.Time       `json:"occurred_at"`
	Data           json.RawMessage `json:"data,omitempty"`
}

// EventAck is the closed success/error union a host returns for a
// published event, symmetric to InvocationResult.
type EventAck struct {
	Protocol  string           `json:"protocol"`
	Accepted  bool             `json:"accepted"`
	Duplicate bool             `json:"duplicate,omitempty"`
	Error     *InvocationError `json:"error,omitempty"`
}

func (e Event) Validate() error {
	if e.Protocol != ProtocolV1 {
		return fmt.Errorf("native event: protocol must be %q", ProtocolV1)
	}
	if !eventTypePattern.MatchString(e.Type) {
		return errors.New("native event: type must be a dot-namespaced lowercase name, e.g. \"whatsapp.message.received\"")
	}
	if !traceIDPattern.MatchString(e.TraceID) {
		return errors.New("native event: trace_id is required and must match ^[A-Za-z0-9._:-]{1,128}$")
	}
	if !idempotencyKeyPattern.MatchString(e.IdempotencyKey) {
		return errors.New("native event: idempotency_key is required and must match ^[A-Za-z0-9._:-]{1,200}$")
	}
	if e.OccurredAt.IsZero() {
		return errors.New("native event: occurred_at is required")
	}
	if len(e.Data) > MaxEventPayloadBytes {
		return fmt.Errorf("native event: data exceeds %d bytes", MaxEventPayloadBytes)
	}
	if len(e.Data) != 0 && !json.Valid(e.Data) {
		return errors.New("native event: data must be valid JSON")
	}
	return nil
}

func (a EventAck) Validate() error {
	if a.Protocol != ProtocolV1 {
		return fmt.Errorf("native event ack: protocol must be %q", ProtocolV1)
	}
	if !a.Accepted {
		if a.Duplicate {
			return errors.New("native event ack: a rejected ack cannot also claim duplicate")
		}
		if a.Error == nil || strings.TrimSpace(a.Error.Message) == "" {
			return errors.New("native event ack: a rejected ack requires an error message")
		}
		if !operationNamePattern.MatchString(a.Error.Code) {
			return errors.New("native event ack: error.code must match ^[a-z][a-z0-9_]{0,127}$")
		}
		if a.Error.Code == BackpressureErrorCode && !a.Error.Retryable {
			return errors.New("native event ack: backpressure must be marked retryable")
		}
		if len(a.Error.Details) != 0 && !json.Valid(a.Error.Details) {
			return errors.New("native event ack: error details must be valid JSON")
		}
		return nil
	}
	if a.Error != nil {
		return errors.New("native event ack: accepted ack cannot contain error")
	}
	return nil
}
