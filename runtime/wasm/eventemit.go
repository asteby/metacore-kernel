package wasm

import (
	"context"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/events"
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
		"error":   hostErrorBody(code, message),
		"meta":    meta,
	})
	return b
}

// executeEventEmit is the event_emit implementation shared by the wasm import
// (which reads name/payload out of guest memory) and by native handlers (which
// hold them as Go values): same bus checks, same emit policy, same
// stage-until-commit buffer, same envelope. readName/readPayload abstract
// where the bytes come from; eventLen/payloadLen are the declared sizes.
func executeEventEmit(ctx context.Context, start time.Time, inv *invocation, eventLen, payloadLen uint32, readName func() ([]byte, bool), readPayload func() []byte) []byte {
			if inv == nil {
				// No invocation bag — fall back to a minimal error envelope.
				// This path is unreachable in production (every entry through
				// Host.Invoke* sets the bag) but defends against future
				// re-wiring that might forget it.
				return eventEmitErr("",
					"bus_unavailable", "invocation context missing",
					uuid.Nil, start)
			}
			if inv.bus == nil {
				return eventEmitErr(inv.addonKey,
					"bus_unavailable", "host has no events.Bus configured",
					inv.orgID, start)
			}
			if eventLen == 0 {
				return eventEmitErr(inv.addonKey,
					"invalid_event", "event name is empty",
					inv.orgID, start)
			}
			if eventLen > eventNameMaxBytes {
				return eventEmitErr(inv.addonKey,
					"invalid_event",
					fmt.Sprintf("event name exceeds %d bytes", eventNameMaxBytes),
					inv.orgID, start)
			}
			nameBytes, ok := readName()
			if !ok {
				return eventEmitErr(inv.addonKey,
					"invalid_event", "event name out of guest memory",
					inv.orgID, start)
			}
			if !utf8.Valid(nameBytes) {
				return eventEmitErr(inv.addonKey,
					"invalid_event", "event name is not valid UTF-8",
					inv.orgID, start)
			}
			eventName := string(nameBytes)
			if payloadLen > eventPayloadMaxBytes {
				return eventEmitErr(inv.addonKey,
					"payload_too_large",
					fmt.Sprintf("payload exceeds %d bytes", eventPayloadMaxBytes),
					inv.orgID, start)
			}
			var payload any
			if payloadLen > 0 {
				body := readPayload()
				if body == nil {
					return eventEmitErr(inv.addonKey,
						"invalid_payload", "payload out of guest memory",
						inv.orgID, start)
				}
				payload = json.RawMessage(body)
			}
			if inv.orgID == uuid.Nil {
				return eventEmitErr(inv.addonKey,
					"no_active_org", "invocation has no bound orgID",
					uuid.Nil, start)
			}
			if denied := inv.checkEmit(ctx, eventName); denied != nil {
				return eventEmitErr(inv.addonKey,
					"forbidden", denied.Error(), inv.orgID, start)
			}
			if inv.emits == nil {
				// Hand-built invocation (tests): legacy immediate publish.
				subscribers, err := inv.bus.PublishWithCount(dynamic.WithoutStageAuthority(ctx), inv.addonKey, eventName, inv.orgID, payload)
				if err != nil {
					code := "forbidden"
					if errors.Is(err, events.ErrDurableDelivery) {
						// Not a permission problem: the siblings ran but the
						// durable ledger did not record the event.
						code = "durable_delivery_failed"
					}
					return eventEmitErr(inv.addonKey,
						code, err.Error(), inv.orgID, start)
				}
				return eventEmitOK(
					inv.addonKey, eventName, subscribers, inv.orgID, start)
			}
			// Emit on commit: refuse what the bus would refuse NOW (so the
			// guest still gets its `forbidden`), but publish only if the
			// invocation ends successfully. See emitbuffer.go.
			if err := inv.bus.CheckEmit(inv.addonKey, eventName); err != nil {
				return eventEmitErr(inv.addonKey,
					"forbidden", err.Error(), inv.orgID, start)
			}
			var raw []byte
			if payloadLen > 0 {
				raw = readPayload()
			}
			staged, serr := stageEmit(ctx, inv.addonKey, eventName, inv.orgID, raw)
			if serr != nil {
				return eventEmitErr(inv.addonKey,
					"invalid_payload", serr.Error(), inv.orgID, start)
			}
			if err := inv.emits.add(staged); err != nil {
				return eventEmitErr(inv.addonKey,
					"too_many_events", err.Error(), inv.orgID, start)
			}
			return eventEmitOK(
				inv.addonKey, eventName, inv.bus.EstimateSubscribers(ctx, inv.orgID, eventName), inv.orgID, start)
}
