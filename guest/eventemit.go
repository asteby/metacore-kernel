package guest

import (
	"encoding/json"
	"fmt"
)

// EmitEventResult is the success payload returned by EmitEvent. It mirrors
// the `data` + `meta` blocks of the host envelope documented in
// docs/wasm-abi.md § 12.4.
type EmitEventResult struct {
	// Event is the canonical event name the host accepted. Always equal
	// to the name the guest passed in — exposed verbatim so callers can
	// log/trace without re-storing the original string.
	Event string
	// Subscribers is the number of handlers the in-process events.Bus
	// fanned out to. Zero is a successful publish that matched no
	// pattern; it is NOT an error condition.
	Subscribers int
	// Meta carries the host-side bookkeeping fields. New fields land
	// here additively (see Meta.EnvelopeVersion for forward-compat).
	Meta EmitEventMeta
}

// EmitEventMeta is the deserialised `meta` block of the host envelope.
// Fields are populated best-effort: when a field is absent in the wire
// payload the zero value is preserved (no error). This lets guests built
// against envelope v1 keep working on a host that bumps the version with
// extra meta keys.
type EmitEventMeta struct {
	// Addon is the host's view of the addon key emitting the event.
	// Always equal to the addon's manifest key; informational.
	Addon string
	// OrgID is the tenant id the host bound to the invocation. Empty
	// string when the host omitted it (e.g. system-emitted events).
	OrgID string
	// EmittedAt is the host's RFC3339Nano timestamp captured when the
	// host import was entered. Useful for end-to-end tracing.
	EmittedAt string
	// DurationMs measures the synchronous fan-out (every subscriber
	// handler runs serially inside Publish).
	DurationMs int
	// EnvelopeVersion identifies the wire format. Today's value is 1
	// (see docs/wasm-abi.md § 12.4). Future hosts may bump this; the
	// helper does NOT panic on unknown versions — it decodes
	// best-effort and leaves new fields unread.
	EnvelopeVersion int
}

// EmitEventError is the typed error EmitEvent returns when the host
// publishes returns `{success:false}`. `Code` is one of the documented
// error codes (`invalid_event`, `payload_too_large`, `forbidden`,
// `no_active_org`, `bus_unavailable`, `invalid_payload`); `Message` is
// human-readable detail.
//
// Use `errors.As` to branch on the code:
//
//	var emErr *guest.EmitEventError
//	if errors.As(err, &emErr) {
//	    switch emErr.Code {
//	    case "forbidden":    /* surface to caller */
//	    case "invalid_event":/* developer-side bug, log loudly */
//	    }
//	}
type EmitEventError struct {
	// Code is the machine-readable error code from the envelope.
	Code string
	// Message is the human-readable detail.
	Message string
	// Meta is the meta block from the failure envelope. Useful for
	// telemetry: durationMs + emittedAt are still populated on errors.
	Meta EmitEventMeta
}

// Error implements the error interface.
func (e *EmitEventError) Error() string {
	if e == nil {
		return "guest: <nil emit error>"
	}
	if e.Message == "" {
		return fmt.Sprintf("event_emit: %s", e.Code)
	}
	return fmt.Sprintf("event_emit: %s: %s", e.Code, e.Message)
}

// emitWireEnvelope mirrors the JSON shape documented in
// docs/wasm-abi.md § 12.4. It is intentionally private — callers consume
// EmitEventResult / EmitEventError, not the raw envelope.
type emitWireEnvelope struct {
	Success bool             `json:"success"`
	Data    *emitWireData    `json:"data,omitempty"`
	Error   *emitWireError   `json:"error,omitempty"`
	Meta    *emitWireMeta    `json:"meta,omitempty"`
}

type emitWireData struct {
	Event       string `json:"event"`
	Subscribers int    `json:"subscribers"`
}

type emitWireError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type emitWireMeta struct {
	Addon           string `json:"addon"`
	OrgID           string `json:"orgId"`
	EmittedAt       string `json:"emittedAt"`
	DurationMs      int    `json:"durationMs"`
	EnvelopeVersion int    `json:"envelopeVersion"`
}

func (m *emitWireMeta) toExported() EmitEventMeta {
	if m == nil {
		return EmitEventMeta{}
	}
	return EmitEventMeta{
		Addon:           m.Addon,
		OrgID:           m.OrgID,
		EmittedAt:       m.EmittedAt,
		DurationMs:      m.DurationMs,
		EnvelopeVersion: m.EnvelopeVersion,
	}
}

// decodeEmitEnvelope parses the raw envelope bytes the host wrote into
// guest memory and translates them into EmitEventResult / EmitEventError.
// It tolerates unknown envelope versions: an envelope with
// `envelopeVersion > supportedEmitEnvelopeVersion` is decoded best-effort
// (known fields populated, unknown fields ignored) instead of erroring.
// This keeps guests built against v1 working when a future host ships v2.
//
// Exposed for tests that want to exercise the parser without going
// through the wasm import.
func decodeEmitEnvelope(buf []byte) (EmitEventResult, error) {
	if len(buf) == 0 {
		// Pre-PR#62 hosts returned literal 0 on success. We preserve
		// that contract: an empty buffer means "publish ran, no
		// envelope" — return a zero-value result, no error.
		return EmitEventResult{}, nil
	}
	var env emitWireEnvelope
	if err := json.Unmarshal(buf, &env); err != nil {
		return EmitEventResult{}, fmt.Errorf("guest: decode event_emit envelope: %w", err)
	}
	meta := env.Meta.toExported()
	if !env.Success {
		emErr := &EmitEventError{Meta: meta}
		if env.Error != nil {
			emErr.Code = env.Error.Code
			emErr.Message = env.Error.Message
		}
		if emErr.Code == "" {
			emErr.Code = "unknown"
		}
		return EmitEventResult{Meta: meta}, emErr
	}
	res := EmitEventResult{Meta: meta}
	if env.Data != nil {
		res.Event = env.Data.Event
		res.Subscribers = env.Data.Subscribers
	}
	return res, nil
}

// SupportedEmitEnvelopeVersion is the envelope version this package was
// authored against. Decoding is forward-compatible: unknown versions do
// NOT cause an error. The constant is exposed so consumers can log a
// warning when they observe a meta.EnvelopeVersion > this value.
const SupportedEmitEnvelopeVersion = 1
