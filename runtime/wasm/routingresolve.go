package wasm

import (
	"context"
	"encoding/json"
	"time"

	"github.com/asteby/metacore-kernel/routing"
	"github.com/google/uuid"
)

// RoutingResolveEnvelopeVersion is the wire-format version of the
// routing_resolve JSON envelope (docs/wasm-abi.md § 18).
const RoutingResolveEnvelopeVersion = 1

// routingResolveRequest is the guest-supplied JSON request. `organization_id`
// is deliberately absent — the table is always the one of the invocation's org.
type routingResolveRequest struct {
	Domain string            `json:"domain"`
	Attrs  map[string]string `json:"attrs"`
}

// executeRoutingResolve answers "which handler wins this decision domain for
// this org, given these attributes?" against the table the embedder builds
// from the routes of every INSTALLED addon (Host.WithRoutingTable).
//
// It is the runtime half of contributions.routes[]: a guest that would
// otherwise act unconditionally asks first and stands down when another addon
// won. `ok:false` means no route matched — the guest decides what that means;
// the kernel does not invent a default it does not own.
//
// Read-only: no transaction, no writes, safe from a subscriber as much as from
// an action handler.
func executeRoutingResolve(ctx context.Context, inv *invocation, reqJSON []byte) []byte {
	start := time.Now()
	addonKey := ""
	orgID := uuid.Nil
	if inv != nil {
		addonKey = inv.addonKey
		orgID = inv.orgID
	}
	fail := func(code, msg string) []byte {
		return routingResolveErr(addonKey, code, msg, orgID, start)
	}

	if inv == nil {
		return fail("invalid_request", "invocation context missing")
	}
	if len(reqJSON) == 0 {
		return fail("invalid_request", "empty request")
	}
	var req routingResolveRequest
	if err := json.Unmarshal(reqJSON, &req); err != nil {
		return fail("invalid_request", "malformed request JSON: "+err.Error())
	}
	if req.Domain == "" {
		return fail("invalid_request", "domain is required")
	}
	if inv.routingTable == nil {
		return fail("routing_unavailable", "host has no routing table configured")
	}
	if orgID == uuid.Nil {
		return fail("invalid_request", "invocation has no bound orgID")
	}

	table, err := inv.routingTable(ctx, orgID)
	if err != nil {
		return fail("routing_error", err.Error())
	}

	handler, winner, ok := table.Resolve(req.Domain, req.Attrs)
	data := map[string]any{"resolved": ok}
	if ok {
		data["handler"] = handler
		data["addon_key"] = winner
		// The question a ceding guest actually asks is "is it me?" — answering
		// it here keeps every guest from re-deriving its own key.
		data["is_self"] = winner == addonKey
	} else {
		data["is_self"] = false
	}
	data["handlers"] = table.Handlers(req.Domain)

	env, _ := json.Marshal(map[string]any{
		"success": true,
		"data":    data,
		"meta":    routingResolveMeta(addonKey, orgID, start),
	})
	return env
}

func routingResolveMeta(addonKey string, orgID uuid.UUID, start time.Time) map[string]any {
	meta := map[string]any{
		"addon":           addonKey,
		"durationMs":      time.Since(start).Milliseconds(),
		"envelopeVersion": RoutingResolveEnvelopeVersion,
	}
	if orgID != uuid.Nil {
		meta["orgId"] = orgID.String()
	}
	return meta
}

func routingResolveErr(addonKey, code, message string, orgID uuid.UUID, start time.Time) []byte {
	b, _ := json.Marshal(map[string]any{
		"success": false,
		"error":   map[string]any{"code": code, "message": message},
		"meta":    routingResolveMeta(addonKey, orgID, start),
	})
	return b
}

// RoutingTableFn is the embedder-injected builder behind the routing_resolve
// import: the org's table, built from routing.Build over the contributions of
// every installed addon. Embedders are expected to cache it per org alongside
// their manifest projection and invalidate on install/uninstall/upgrade —
// rebuilding it on every guest call would re-read every manifest.
type RoutingTableFn func(ctx context.Context, orgID uuid.UUID) (*routing.Table, error)
