package wasm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/asteby/metacore-kernel/domains"
	"github.com/google/uuid"
)

// DNSResolveEnvelopeVersion is the wire version of the dns_resolve envelope.
const DNSResolveEnvelopeVersion = 1

// maxDNSResolvePerInvocation bounds lookups per invocation: a verification
// pass needs a handful (one per required record), never hundreds.
const maxDNSResolvePerInvocation = 64

const dnsResolveTimeout = 5 * time.Second

// DNSAuthorizeFunc decides whether the invocation (org + addon) may resolve
// name: the embedder checks that name equals or is under a domain the org owns
// (domains.OrgDomain) whose purposes the addon may manage. It is REQUIRED: a
// host that wires a resolver without it answers every call `forbidden`, because
// an unscoped dns_resolve would be an open DNS oracle for any installed addon.
type DNSAuthorizeFunc func(ctx context.Context, orgID uuid.UUID, addonKey, name string) error

type dnsResolveRequest struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// executeDNSResolve implements `metacore_host.dns_resolve`: read-only DNS
// lookup of one name/type, for ownership proofs and record verification from a
// wasm handler. Gated by a `domain:manage` capability AND by the host's
// DNSAuthorizeFunc (the name must belong to one of the org's domains).
//
// Success: data = {name, type, values:[canonical values], no_records:bool}.
// An authoritative empty answer is success with no_records=true; a failed
// lookup (timeout, SERVFAIL) is the error `dns_lookup_failed` and must never
// be read as "the record does not exist".
func executeDNSResolve(ctx context.Context, inv *invocation, reqJSON []byte) []byte {
	start := time.Now()
	addonKey, orgID := "", uuid.Nil
	if inv != nil {
		addonKey, orgID = inv.addonKey, inv.orgID
	}
	fail := func(code, msg string) []byte {
		b, _ := json.Marshal(map[string]any{
			"success": false,
			"error":   hostErrorBody(code, msg),
			"meta":    dnsMeta(addonKey, orgID, start),
		})
		return b
	}
	if inv == nil {
		return fail("invalid_request", "invocation context missing")
	}
	if orgID == uuid.Nil {
		return fail("no_active_org", "invocation has no bound orgID")
	}
	if err := inv.caps.CanResolveDNS(); err != nil {
		inv.logf("audit dns_resolve addon=%s org=%s decision=denied: %v", addonKey, orgID, err)
		return fail("forbidden", err.Error())
	}
	var req dnsResolveRequest
	if err := json.Unmarshal(reqJSON, &req); err != nil {
		return fail("invalid_request", "malformed request JSON: "+err.Error())
	}
	name := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(req.Name), "."))
	typ := domains.RecordType(strings.ToUpper(strings.TrimSpace(req.Type)))
	if !typ.Valid() {
		return fail("invalid_request", "type must be one of A, AAAA, CNAME, MX, TXT")
	}
	if err := (domains.Record{Type: domains.TypeTXT, Name: name, Value: "x"}).Validate(); err != nil {
		return fail("invalid_request", "name is not a valid DNS name")
	}
	if inv.dnsResolver == nil {
		return fail("dns_unavailable", "host has no DNS resolver configured")
	}
	if inv.dnsAuthorize == nil {
		inv.logf("audit dns_resolve addon=%s name=%s org=%s decision=denied: no authorizer", addonKey, name, orgID)
		return fail("forbidden", "host has no DNS authorization policy configured")
	}
	if err := inv.dnsAuthorize(ctx, orgID, addonKey, name); err != nil {
		inv.logf("audit dns_resolve addon=%s name=%s org=%s decision=denied: %v", addonKey, name, orgID, err)
		return fail("forbidden", err.Error())
	}
	inv.dnsCalls++
	if inv.dnsCalls > maxDNSResolvePerInvocation {
		return fail("rate_limited", "too many dns_resolve calls in one invocation")
	}
	inv.logf("audit dns_resolve addon=%s name=%s type=%s org=%s decision=allowed", addonKey, name, typ, orgID)

	lctx, cancel := context.WithTimeout(ctx, dnsResolveTimeout)
	defer cancel()
	vals, err := inv.dnsResolver.Lookup(lctx, name, typ)
	noRecords := errors.Is(err, domains.ErrNoRecords)
	if err != nil && !noRecords {
		return fail("dns_lookup_failed", "lookup failed: "+err.Error())
	}
	if vals == nil {
		vals = []string{}
	}
	env, _ := json.Marshal(map[string]any{
		"success": true,
		"data":    map[string]any{"name": name, "type": string(typ), "values": vals, "no_records": noRecords || len(vals) == 0},
		"meta":    dnsMeta(addonKey, orgID, start),
	})
	return env
}

func dnsMeta(addonKey string, orgID uuid.UUID, start time.Time) map[string]any {
	meta := map[string]any{
		"addon":           addonKey,
		"durationMs":      time.Since(start).Milliseconds(),
		"envelopeVersion": DNSResolveEnvelopeVersion,
	}
	if orgID != uuid.Nil {
		meta["orgId"] = orgID.String()
	}
	return meta
}

// WithDNS injects the resolver and the authorization policy for the
// `metacore_host.dns_resolve` import (ABI 1.17). Without a resolver the import
// answers `dns_unavailable`; without authorize it answers `forbidden`.
func (h *Host) WithDNS(r domains.DNSResolver, authorize DNSAuthorizeFunc) *Host {
	h.dnsResolver = r
	h.dnsAuthorize = authorize
	return h
}
