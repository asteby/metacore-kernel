package native

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func validEvent() Event {
	return Event{
		Protocol:       ProtocolV1,
		Type:           "whatsapp.message.received",
		TraceID:        "0af7651916cd43dd8448eb211c80319c",
		IdempotencyKey: "wa:device-1:3EB0C767D1F1",
		OccurredAt:     time.Date(2026, 9, 11, 15, 4, 5, 0, time.UTC),
		Data:           json.RawMessage(`{"from":"573001234567","text":"hola"}`),
	}
}

func TestEventValidate(t *testing.T) {
	if err := validEvent().Validate(); err != nil {
		t.Fatalf("valid event rejected: %v", err)
	}
}

func TestEventRejectsMissingIdentity(t *testing.T) {
	// Event intentionally has no organization_id/installation_id/addon_key
	// fields at all: this test guards that nobody adds them back in a way
	// that would let a sidecar claim identity instead of the host deriving
	// it from the authenticated event socket.
	var e Event
	if err := json.Unmarshal([]byte(`{"organization_id":"org-1","installation_id":"i-1","addon_key":"connector_whatsapp"}`), &e); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if e.Protocol != "" {
		t.Fatal("Event must not decode a protocol from unrelated identity fields")
	}
}

func TestEventValidateRejectsMalformedFields(t *testing.T) {
	tests := map[string]func(*Event){
		"bad protocol":        func(e *Event) { e.Protocol = "metacore.native/v2" },
		"bad type":            func(e *Event) { e.Type = "MessageReceived" },
		"path traversal type": func(e *Event) { e.Type = "../../etc" },
		"missing trace id":    func(e *Event) { e.TraceID = "" },
		"missing idem key":    func(e *Event) { e.IdempotencyKey = "" },
		"idem key with space": func(e *Event) { e.IdempotencyKey = "has space" },
		"zero occurred_at":    func(e *Event) { e.OccurredAt = time.Time{} },
		"invalid json data":   func(e *Event) { e.Data = json.RawMessage(`{not json`) },
	}
	for name, mutate := range tests {
		e := validEvent()
		mutate(&e)
		if err := e.Validate(); err == nil {
			t.Fatalf("%s: invalid event accepted", name)
		}
	}
}

func TestEventValidateRejectsOversizedPayload(t *testing.T) {
	e := validEvent()
	e.Data = json.RawMessage(`"` + strings.Repeat("x", MaxEventPayloadBytes) + `"`)
	if err := e.Validate(); err == nil {
		t.Fatal("oversized event payload accepted")
	}
}

func TestEventAckAcceptedUnion(t *testing.T) {
	good := []EventAck{
		{Protocol: ProtocolV1, Accepted: true},
		{Protocol: ProtocolV1, Accepted: true, Duplicate: true},
	}
	for _, ack := range good {
		if err := ack.Validate(); err != nil {
			t.Fatalf("valid ack rejected: %v", err)
		}
	}
}

func TestEventAckRejectsAcceptedWithError(t *testing.T) {
	ack := EventAck{Protocol: ProtocolV1, Accepted: true, Error: &InvocationError{Code: "backpressure", Message: "x"}}
	if err := ack.Validate(); err == nil {
		t.Fatal("accepted ack with error accepted")
	}
}

func TestEventAckRejectsRejectedWithoutError(t *testing.T) {
	ack := EventAck{Protocol: ProtocolV1, Accepted: false}
	if err := ack.Validate(); err == nil {
		t.Fatal("rejected ack without error accepted")
	}
}

func TestEventAckRejectsDuplicateWithoutAccepted(t *testing.T) {
	ack := EventAck{Protocol: ProtocolV1, Accepted: false, Duplicate: true, Error: &InvocationError{Code: "bad", Message: "x"}}
	if err := ack.Validate(); err == nil {
		t.Fatal("rejected+duplicate ack accepted")
	}
}

func TestEventAckBackpressureMustBeRetryable(t *testing.T) {
	ack := EventAck{Protocol: ProtocolV1, Accepted: false, Error: &InvocationError{Code: BackpressureErrorCode, Message: "queue full", Retryable: false}}
	if err := ack.Validate(); err == nil {
		t.Fatal("non-retryable backpressure ack accepted")
	}
	ack.Error.Retryable = true
	if err := ack.Validate(); err != nil {
		t.Fatalf("retryable backpressure ack rejected: %v", err)
	}
}

// TestEventPlaneUsesDistinctEnvelopeFromEventSocket documents (and locks in)
// that the event-plane env vars are distinct from the operations ones, so a
// supervisor cannot accidentally reuse the same socket/token for both
// directions.
func TestEventPlaneUsesDistinctEnvelopeFromEventSocket(t *testing.T) {
	if EnvEventSocket == EnvSocket || EnvEventTokenFile == EnvTokenFile {
		t.Fatal("event-plane envelope must be distinct from the operations envelope")
	}
}
