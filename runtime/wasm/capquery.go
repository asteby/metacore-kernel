package wasm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/asteby/metacore-kernel/capability"
	"github.com/google/uuid"
)

// CapQueryEnvelopeVersion is the wire version of the capability_query envelope.
const CapQueryEnvelopeVersion = 1

// maxCapQueryDepth bounds provider -> capability_query -> provider chains. A
// provider invoked by a query may itself query a capability (a payment addon
// asking for tax), but never unboundedly.
const maxCapQueryDepth = 3

type capQueryDepthKey struct{}

func capQueryDepth(ctx context.Context) int {
	d, _ := ctx.Value(capQueryDepthKey{}).(int)
	return d
}

// capQueryRequest is the guest's request: it names the capability and the
// market code, never the provider (except to follow up a record it created:
// confirm, status, refund, cancel, track…).
type capQueryRequest struct {
	Capability string         `json:"capability"`
	Market     string         `json:"market,omitempty"`
	Provider   string         `json:"provider,omitempty"`
	Input      map[string]any `json:"input,omitempty"`
}

// executeCapabilityQuery implements `metacore_host.capability_query`: the
// guest asks for a capability and the host resolves, per (org, market), which
// installed addon provides it, fans out when the contract says so, and returns
// the capability.QueryResult envelope as `data`. Gated by the addon's
// `capability:query <key>` capability, hard-enforced like connector_get.
func executeCapabilityQuery(ctx context.Context, inv *invocation, reqJSON []byte) []byte {
	start := time.Now()
	addonKey, orgID := "", uuid.Nil
	if inv != nil {
		addonKey, orgID = inv.addonKey, inv.orgID
	}
	fail := func(code, msg string) []byte {
		b, _ := json.Marshal(map[string]any{
			"success": false,
			"error":   hostErrorBody(code, msg),
			"meta":    capQueryMeta(addonKey, orgID, start),
		})
		return b
	}
	if inv == nil {
		return fail("invalid_request", "invocation context missing")
	}
	if orgID == uuid.Nil {
		return fail("no_active_org", "invocation has no bound orgID")
	}
	var req capQueryRequest
	if err := json.Unmarshal(reqJSON, &req); err != nil {
		return fail("invalid_request", "malformed request JSON: "+err.Error())
	}
	req.Capability = strings.TrimSpace(req.Capability)
	if req.Capability == "" {
		return fail("invalid_request", "capability is required")
	}
	if err := inv.caps.CanQueryCapability(req.Capability); err != nil {
		inv.logf("audit capability_query addon=%s capability=%s org=%s decision=denied: %v", addonKey, req.Capability, orgID, err)
		return fail("forbidden", err.Error())
	}
	inv.logf("audit capability_query addon=%s capability=%s market=%s org=%s decision=allowed", addonKey, req.Capability, req.Market, orgID)
	if inv.capQuerier == nil {
		return fail("capability_unavailable", "host has no capability querier configured")
	}
	depth := capQueryDepth(ctx)
	if depth >= maxCapQueryDepth {
		return fail("capability_depth", "capability_query nested too deeply")
	}
	qctx := context.WithValue(ctx, capQueryDepthKey{}, depth+1)
	res, err := inv.capQuerier.Query(qctx, capability.Query{
		Org:        orgID,
		Market:     capability.Market{Code: strings.TrimSpace(req.Market)},
		Capability: req.Capability,
		Input:      req.Input,
		Provider:   strings.TrimSpace(req.Provider),
		Caller:     addonKey,
	})
	if err != nil {
		return fail(capQueryErrorCode(err), err.Error())
	}
	env, _ := json.Marshal(map[string]any{
		"success": true,
		"data":    res,
		"meta":    capQueryMeta(addonKey, orgID, start),
	})
	return env
}

// executeCapabilityProviders implements `metacore_host.capability_providers`:
// the guest lists the providers of a capability for a market and how the
// resolver treats each one (serves, selected, default, priority, status,
// reached) without invoking any. Same gate as capability_query (the addon needs
// `capability:query <key>`) and the same resolver (capability.Eligible ->
// capability.Select). The import is registered by the host capability table
// (capabilities.go) and the import allowlist; this is its implementation.
func executeCapabilityProviders(ctx context.Context, inv *invocation, reqJSON []byte) []byte {
	start := time.Now()
	addonKey, orgID := "", uuid.Nil
	if inv != nil {
		addonKey, orgID = inv.addonKey, inv.orgID
	}
	fail := func(code, msg string) []byte {
		b, _ := json.Marshal(map[string]any{
			"success": false,
			"error":   hostErrorBody(code, msg),
			"meta":    capQueryMeta(addonKey, orgID, start),
		})
		return b
	}
	if inv == nil {
		return fail("invalid_request", "invocation context missing")
	}
	if orgID == uuid.Nil {
		return fail("no_active_org", "invocation has no bound orgID")
	}
	var req capQueryRequest
	if err := json.Unmarshal(reqJSON, &req); err != nil {
		return fail("invalid_request", "malformed request JSON: "+err.Error())
	}
	req.Capability = strings.TrimSpace(req.Capability)
	if req.Capability == "" {
		return fail("invalid_request", "capability is required")
	}
	if err := inv.caps.CanQueryCapability(req.Capability); err != nil {
		inv.logf("audit capability_providers addon=%s capability=%s org=%s decision=denied: %v", addonKey, req.Capability, orgID, err)
		return fail("forbidden", err.Error())
	}
	lister, ok := inv.capQuerier.(capability.ProviderLister)
	if !ok {
		return fail("capability_unavailable", "host has no capability provider lister configured")
	}
	listing, err := lister.ListProviders(ctx, capability.Query{
		Org:        orgID,
		Market:     capability.Market{Code: strings.TrimSpace(req.Market)},
		Capability: req.Capability,
		Caller:     addonKey,
	})
	if err != nil {
		return fail(capQueryErrorCode(err), err.Error())
	}
	env, _ := json.Marshal(map[string]any{
		"success": true,
		"data":    listing,
		"meta":    capQueryMeta(addonKey, orgID, start),
	})
	return env
}

func capQueryErrorCode(err error) string {
	switch {
	case errors.Is(err, capability.ErrNoProvider):
		return "no_provider"
	case errors.Is(err, capability.ErrAmbiguousProvider):
		return "ambiguous_provider"
	case errors.Is(err, capability.ErrNotConfigured):
		return "not_configured"
	case errors.Is(err, capability.ErrInvalidInput):
		return "validation_error"
	case errors.Is(err, capability.ErrNotQuery):
		return "not_query"
	case errors.Is(err, capability.ErrProviderRequired):
		return "provider_required"
	case errors.Is(err, capability.ErrIdempotencyRequired):
		return "idempotency_required"
	}
	return "capability_error"
}

func capQueryMeta(addonKey string, orgID uuid.UUID, start time.Time) map[string]any {
	meta := map[string]any{
		"addon":           addonKey,
		"durationMs":      time.Since(start).Milliseconds(),
		"envelopeVersion": CapQueryEnvelopeVersion,
	}
	if orgID != uuid.Nil {
		meta["orgId"] = orgID.String()
	}
	return meta
}

// WithCapabilityQuerier injects the embedder's capability.Querier for the
// `metacore_host.capability_query` import. When unset the import answers
// `capability_unavailable`.
func (h *Host) WithCapabilityQuerier(q capability.Querier) *Host {
	h.capQuerier = q
	return h
}
