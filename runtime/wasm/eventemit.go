package wasm

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// EventEmitEnvelopeVersion is the wire-format version of the JSON envelope
// returned by the `metacore_host.event_emit` import. v1 added the canonical
// `{success, data, meta}` shape documented in docs/wasm-abi.md § 12.4,
// aligning the implementation with the ABI v1.0 audit § 12 inconsistency #4
// (the previous implementation returned literal `0` on success — the audit
// flagged the doc-vs-code drift; this version resolves it).
//
// Subscribers and tooling that need to assume the new shape SHOULD gate on
// this constant via reflection or the build-time module version exposed in
// the kernel changelog (`[0.11.0]`).
const EventEmitEnvelopeVersion = 1

// eventEmitOK builds the success envelope per docs/wasm-abi.md § 12.4.
//
//	{
//	  "success": true,
//	  "data":    {"event": "<addon>.<model>.<action>", "subscribers": N},
//	  "meta":    {"addon": "...", "orgId": "...",
//	              "emittedAt": "<rfc3339nano>", "durationMs": N,
//	              "envelopeVersion": 1}
//	}
//
// orgID is included verbatim — when uuid.Nil the field is omitted so we
// don't leak the all-zero UUID into telemetry. Callers should have returned
// the `no_active_org` envelope earlier in that case, but the helper stays
// defensive.
func eventEmitOK(addonKey, event string, subscribers int, orgID uuid.UUID, start time.Time) []byte {
	meta := map[string]any{
		"addon":           addonKey,
		"emittedAt":       start.UTC().Format(time.RFC3339Nano),
		"durationMs":      time.Since(start).Milliseconds(),
		"envelopeVersion": EventEmitEnvelopeVersion,
	}
	if orgID != uuid.Nil {
		meta["orgId"] = orgID.String()
	}
	b, _ := json.Marshal(map[string]any{
		"success": true,
		"data": map[string]any{
			"event":       event,
			"subscribers": subscribers,
		},
		"meta": meta,
	})
	return b
}

// eventEmitErr builds the failure envelope per docs/wasm-abi.md § 12.4.
// `code` is one of the documented error codes (`invalid_event`,
// `payload_too_large`, `forbidden`, `no_active_org`, `bus_unavailable`,
// `invalid_payload`). The shape mirrors the canonical kernel error
// envelope used elsewhere (`{success:false, error:{code,message}, meta}`).
func eventEmitErr(addonKey, code, message string, orgID uuid.UUID, start time.Time) []byte {
	meta := map[string]any{
		"addon":           addonKey,
		"emittedAt":       start.UTC().Format(time.RFC3339Nano),
		"durationMs":      time.Since(start).Milliseconds(),
		"envelopeVersion": EventEmitEnvelopeVersion,
	}
	if orgID != uuid.Nil {
		meta["orgId"] = orgID.String()
	}
	b, _ := json.Marshal(map[string]any{
		"success": false,
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
		"meta": meta,
	})
	return b
}
