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

// DNSAuthRequest is what a DNSAuthorizeFunc decides on.
type DNSAuthRequest struct {
	OrgID    uuid.UUID
	AddonKey string
	// Name is normalized (lower-case, no trailing dot) and already shaped as a
	// valid DNS name.
	Name string
	Type domains.RecordType
	// CanManage reports whether the addon declared `domain:manage <purpose>`
	// for the given domain purpose. The authorizer MUST require it for at least
	// one purpose of the org domain the name falls under, so a mail addon
	// cannot resolve names of a domain reserved for another purpose.
	CanManage func(purpose string) error
}

// DNSAuthorizeFunc decides whether the invocation (org + addon) may resolve
// req.Name. It is REQUIRED: a host that wires a resolver without it answers
// every call `forbidden`, because an unscoped dns_resolve would be an open DNS
// oracle (a data-exfiltration channel through query names) for any installed
// addon. NewOrgDomainDNSAuthorizer is the reference implementation. Its error
// is audit-only: the guest only ever sees a generic denial.
type DNSAuthorizeFunc func(ctx context.Context, req DNSAuthRequest) error

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
		return fail("forbidden", "addon lacks the domain:manage capability")
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
	// Budgets are charged BEFORE authorization so denied attempts count too:
	// otherwise an addon could probe names (or exfiltrate data through them)
	// with an unlimited number of refused calls.
	inv.dnsCalls++
	if inv.dnsCalls > maxDNSResolvePerInvocation {
		inv.logf("audit dns_resolve addon=%s org=%s decision=rate_limited scope=invocation", addonKey, orgID)
		return fail("rate_limited", "too many dns_resolve calls in one invocation")
	}
	if !inv.dnsLimits.allow(orgID, time.Now()) {
		inv.logf("audit dns_resolve addon=%s org=%s decision=rate_limited scope=organization", addonKey, orgID)
		return fail("rate_limited", "too many dns_resolve calls for this organization")
	}
	if inv.dnsAuthorize == nil {
		inv.logf("audit dns_resolve addon=%s name=%s org=%s decision=denied: no authorizer", addonKey, name, orgID)
		return fail("forbidden", "dns_resolve is not authorized")
	}
	if err := inv.dnsAuthorize(ctx, DNSAuthRequest{
		OrgID: orgID, AddonKey: addonKey, Name: name, Type: typ, CanManage: inv.caps.CanManageDomain,
	}); err != nil {
		// The detail goes to the audit log only: the guest must not learn which
		// domains the org owns, their status or the policy internals.
		inv.logf("audit dns_resolve addon=%s name=%s type=%s org=%s decision=denied: %v", addonKey, name, typ, orgID, err)
		return fail("forbidden", "dns name is not authorized for this addon")
	}
	inv.logf("audit dns_resolve addon=%s name=%s type=%s org=%s decision=allowed", addonKey, name, typ, orgID)

	lctx, cancel := context.WithTimeout(ctx, dnsResolveTimeout)
	defer cancel()
	vals, err := inv.dnsResolver.Lookup(lctx, name, typ)
	noRecords := errors.Is(err, domains.ErrNoRecords)
	if err != nil && !noRecords {
		inv.logf("audit dns_resolve addon=%s name=%s type=%s org=%s lookup_error: %v", addonKey, name, typ, orgID, err)
		return fail("dns_lookup_failed", "lookup failed")
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
//
// RESOLVER CHOICE: pass a PUBLIC recursive resolver
// (domains.NewPublicResolver, e.g. 1.1.1.1:53), NOT the system resolver. The
// system resolver usually serves split-horizon / internal names and search
// domains, which would turn dns_resolve into a probe of the host's private
// network. Names are also screened by the authorizer, but a public resolver
// is the defence in depth.
//
// It installs the default per-organization budget (DefaultDNSOrgLimit lookups
// per DefaultDNSOrgWindow); tune it with WithDNSOrgLimit.
func (h *Host) WithDNS(r domains.DNSResolver, authorize DNSAuthorizeFunc) *Host {
	h.dnsResolver = r
	h.dnsAuthorize = authorize
	if h.dnsLimits == nil {
		h.dnsLimits = newDNSOrgLimiter(DefaultDNSOrgLimit, DefaultDNSOrgWindow)
	}
	return h
}

// WithDNSOrgLimit replaces the per-organization dns_resolve budget: at most
// max lookups (allowed or denied) per window across all invocations and
// addons of one organization. max <= 0 restores the default.
func (h *Host) WithDNSOrgLimit(max int, window time.Duration) *Host {
	h.dnsLimits = newDNSOrgLimiter(max, window)
	return h
}
