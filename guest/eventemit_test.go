package guest

import (
	"encoding/json"
	"errors"
	"testing"
)

// The decoder is the only piece of EmitEvent that runs on the host (the
// wasmimport stub does not). Cover its branches end-to-end so we have a
// regression net independent of TinyGo.

func TestDecodeEmitEnvelope_SuccessHappyPath(t *testing.T) {
	raw := mustMarshal(t, map[string]any{
		"success": true,
		"data": map[string]any{
			"event":       "tickets.resolved",
			"subscribers": 3,
		},
		"meta": map[string]any{
			"addon":           "tickets",
			"orgId":           "11111111-1111-1111-1111-111111111111",
			"emittedAt":       "2026-05-14T10:00:00Z",
			"durationMs":      4,
			"envelopeVersion": 1,
		},
	})
	got, err := decodeEmitEnvelope(raw)
	if err != nil {
		t.Fatalf("decodeEmitEnvelope: %v", err)
	}
	if got.Event != "tickets.resolved" {
		t.Errorf("Event = %q, want tickets.resolved", got.Event)
	}
	if got.Subscribers != 3 {
		t.Errorf("Subscribers = %d, want 3", got.Subscribers)
	}
	if got.Meta.Addon != "tickets" {
		t.Errorf("Meta.Addon = %q, want tickets", got.Meta.Addon)
	}
	if got.Meta.OrgID == "" {
		t.Errorf("Meta.OrgID empty, want uuid")
	}
	if got.Meta.EnvelopeVersion != 1 {
		t.Errorf("Meta.EnvelopeVersion = %d, want 1", got.Meta.EnvelopeVersion)
	}
	if got.Meta.DurationMs != 4 {
		t.Errorf("Meta.DurationMs = %d, want 4", got.Meta.DurationMs)
	}
}

func TestDecodeEmitEnvelope_ZeroSubscribersIsSuccess(t *testing.T) {
	raw := mustMarshal(t, map[string]any{
		"success": true,
		"data":    map[string]any{"event": "tickets.archived", "subscribers": 0},
		"meta":    map[string]any{"addon": "tickets", "envelopeVersion": 1},
	})
	got, err := decodeEmitEnvelope(raw)
	if err != nil {
		t.Fatalf("decodeEmitEnvelope: %v", err)
	}
	if got.Subscribers != 0 {
		t.Errorf("Subscribers = %d, want 0", got.Subscribers)
	}
}

func TestDecodeEmitEnvelope_ErrorEnvelopeReturnsTypedError(t *testing.T) {
	raw := mustMarshal(t, map[string]any{
		"success": false,
		"error": map[string]any{
			"code":    "forbidden",
			"message": "addon \"tickets\" lacks event:emit \"orders.refunded\"",
		},
		"meta": map[string]any{
			"addon":           "tickets",
			"emittedAt":       "2026-05-14T10:00:00Z",
			"durationMs":      0,
			"envelopeVersion": 1,
		},
	})
	_, err := decodeEmitEnvelope(raw)
	if err == nil {
		t.Fatal("decodeEmitEnvelope: expected error envelope to surface")
	}
	var emErr *EmitEventError
	if !errors.As(err, &emErr) {
		t.Fatalf("error is not *EmitEventError: %T (%v)", err, err)
	}
	if emErr.Code != "forbidden" {
		t.Errorf("Code = %q, want forbidden", emErr.Code)
	}
	if emErr.Message == "" {
		t.Errorf("Message empty, want detail")
	}
	if emErr.Meta.Addon != "tickets" {
		t.Errorf("Meta.Addon = %q, want tickets", emErr.Meta.Addon)
	}
	if emErr.Error() == "" {
		t.Errorf("Error() returned empty string")
	}
}

func TestDecodeEmitEnvelope_ErrorWithoutCodeUsesUnknown(t *testing.T) {
	// Defensive: hosts older than the envelope wire format MAY send a
	// failure envelope without an `error.code` field. We fill in
	// "unknown" so callers can still branch on `EmitEventError.Code`.
	raw := mustMarshal(t, map[string]any{
		"success": false,
		"meta":    map[string]any{"addon": "x", "envelopeVersion": 1},
	})
	_, err := decodeEmitEnvelope(raw)
	if err == nil {
		t.Fatal("expected error")
	}
	var emErr *EmitEventError
	if !errors.As(err, &emErr) {
		t.Fatalf("not EmitEventError: %T", err)
	}
	if emErr.Code != "unknown" {
		t.Errorf("Code = %q, want unknown", emErr.Code)
	}
}

func TestDecodeEmitEnvelope_EmptyBufferIsZeroSuccess(t *testing.T) {
	// Pre-PR#62 hosts returned literal 0 on success. Our helper must
	// keep that contract working so guests upgrading the SDK before
	// the host upgrades don't regress.
	got, err := decodeEmitEnvelope(nil)
	if err != nil {
		t.Fatalf("decodeEmitEnvelope(nil): %v", err)
	}
	if got.Event != "" || got.Subscribers != 0 || got.Meta.Addon != "" {
		t.Errorf("expected zero-value result, got %+v", got)
	}
}

func TestDecodeEmitEnvelope_ForwardCompatUnknownVersion(t *testing.T) {
	// A future host bumps envelopeVersion to 2 and adds a new meta
	// field. The decoder must NOT panic and MUST surface the known
	// fields plus the bumped version (so the addon can log or branch).
	raw := mustMarshal(t, map[string]any{
		"success": true,
		"data": map[string]any{
			"event":       "tickets.created",
			"subscribers": 1,
		},
		"meta": map[string]any{
			"addon":           "tickets",
			"envelopeVersion": 2,
			"durationMs":      9,
			// future field — must be silently ignored
			"traceId": "abc-123",
		},
	})
	got, err := decodeEmitEnvelope(raw)
	if err != nil {
		t.Fatalf("decodeEmitEnvelope: %v", err)
	}
	if got.Meta.EnvelopeVersion != 2 {
		t.Errorf("EnvelopeVersion = %d, want 2", got.Meta.EnvelopeVersion)
	}
	if got.Subscribers != 1 {
		t.Errorf("Subscribers = %d, want 1", got.Subscribers)
	}
}

func TestDecodeEmitEnvelope_MalformedJSON(t *testing.T) {
	// Garbage in the response buffer surfaces as a decode error rather
	// than a typed EmitEventError. This is intentional: the typed
	// surface is reserved for host-issued failure envelopes, not for
	// transport corruption (which would point to a kernel bug, not a
	// capability/validation failure).
	_, err := decodeEmitEnvelope([]byte(`{not json`))
	if err == nil {
		t.Fatal("expected decode error")
	}
	var emErr *EmitEventError
	if errors.As(err, &emErr) {
		t.Errorf("malformed JSON surfaced as EmitEventError: %v", emErr)
	}
}

func TestEmitEventError_Error(t *testing.T) {
	// Nil-receiver guard (defensive — errors.As can't actually produce
	// nil, but ensure the formatter is safe).
	var nilErr *EmitEventError
	if got := nilErr.Error(); got == "" {
		t.Errorf("nil receiver returned empty string")
	}

	e := &EmitEventError{Code: "forbidden", Message: "no cap"}
	if got := e.Error(); got != "event_emit: forbidden: no cap" {
		t.Errorf("unexpected: %q", got)
	}

	e2 := &EmitEventError{Code: "bus_unavailable"}
	if got := e2.Error(); got != "event_emit: bus_unavailable" {
		t.Errorf("unexpected: %q", got)
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return b
}
