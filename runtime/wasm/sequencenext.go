package wasm

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// DataSequenceEnvelopeVersion is the wire-format version of the sequence_next
// JSON envelope (docs/wasm-abi.md § 17).
const DataSequenceEnvelopeVersion = 1

// sequenceNextRequest is the guest-supplied JSON request. `organization_id` is
// deliberately absent — tenant scope ALWAYS comes from the invocation context.
type sequenceNextRequest struct {
	Model string `json:"model"`
	Key   string `json:"key"`
}

// executeSequenceNext issues the next formatted folio value for (model, key) in
// the invocation's org, via the embedder-injected sequence backend
// (Host.WithSequenceNext). All failures surface inside the JSON envelope; the
// function never returns an error (wire shape `{success, data, meta}`).
func executeSequenceNext(ctx context.Context, inv *invocation, reqJSON []byte) []byte {
	start := time.Now()
	addonKey := ""
	orgID := uuid.Nil
	if inv != nil {
		addonKey = inv.addonKey
		orgID = inv.orgID
	}
	fail := func(code, msg string) []byte {
		return sequenceNextErr(addonKey, code, msg, orgID, start)
	}

	if inv == nil {
		return fail("invalid_request", "invocation context missing")
	}
	if len(reqJSON) == 0 {
		return fail("invalid_request", "empty request")
	}
	var req sequenceNextRequest
	if err := json.Unmarshal(reqJSON, &req); err != nil {
		return fail("invalid_request", "malformed request JSON: "+err.Error())
	}
	if req.Model == "" || req.Key == "" {
		return fail("invalid_request", "model and key are required")
	}
	if inv.sequenceNext == nil {
		return fail("sequence_unavailable", "host has no sequence backend configured")
	}
	if orgID == uuid.Nil {
		return fail("invalid_request", "invocation has no bound orgID")
	}

	// Capability gate: minting a folio is a write against the model's counter,
	// so it demands db:write on the LOGICAL model name — same enforcer and
	// deny-on-unregistered semantics as data_mutate. Without it any addon could
	// burn another addon's fiscal folios (gaps) or probe its issuance volume.
	if inv.enforcer != nil {
		if err := inv.enforcer.CheckCapability(addonKey, "db:write", req.Model); err != nil {
			return fail("forbidden", err.Error())
		}
	}

	value, err := inv.sequenceNext(ctx, orgID, req.Model, req.Key)
	if err != nil {
		return fail("sequence_error", err.Error())
	}
	env, _ := json.Marshal(map[string]any{
		"success": true,
		"data":    map[string]any{"value": value},
		"meta":    sequenceNextMeta(addonKey, orgID, start),
	})
	return env
}

func sequenceNextMeta(addonKey string, orgID uuid.UUID, start time.Time) map[string]any {
	meta := map[string]any{
		"addon":           addonKey,
		"durationMs":      time.Since(start).Milliseconds(),
		"envelopeVersion": DataSequenceEnvelopeVersion,
	}
	if orgID != uuid.Nil {
		meta["orgId"] = orgID.String()
	}
	return meta
}

func sequenceNextErr(addonKey, code, message string, orgID uuid.UUID, start time.Time) []byte {
	b, _ := json.Marshal(map[string]any{
		"success": false,
		"error":   map[string]any{"code": code, "message": message},
		"meta":    sequenceNextMeta(addonKey, orgID, start),
	})
	return b
}
