// Package domains is the platform primitive for organization-owned DNS
// domains: who owns a host, what it is used for, which DNS records the use
// requires, whether the world sees them, and (optionally) writing them for the
// tenant through their DNS provider.
//
// It generalizes the storefront custom-domain flow (ops
// handlers/storefront_domains.go) without changing its behaviour: the TXT
// ownership proof, the host normalization/validation rules and the verify
// token format are the same, now reusable by any addon (mail, storefront,
// portals, SSO) through one shape.
//
// # Pieces
//
//	OrgDomain      host + purposes[] + verify_token + status + dns_provider
//	CheckOwnership _asteby-verify.<host> TXT proof (via a DNSResolver)
//	RecordSet      the DNS records a purpose REQUIRES (MX, SPF, DKIM, DMARC...)
//	Check/Evaluate required vs observed: missing | wrong_value | propagating | ok
//	               (+ unknown when the lookup itself failed), with diagnostics
//	DNSApplier     writes a RecordSet through a provider: Plan (diff, no writes)
//	               then Apply (idempotent upsert, never deletes)
//	Cloudflare     the first ZoneProvider behind that interface
//
// Everything is host-neutral: no database, no HTTP framework. The host persists
// OrgDomain rows and gates access (capability domain:manage, RBAC); this
// package supplies the vocabulary and the DNS logic.
//
// # Safety rules baked in
//
//   - A failed lookup is never reported as "missing": it is "unknown". Only an
//     authoritative NXDOMAIN / empty answer is missing.
//   - Applying never deletes a record. A record that would replace something
//     the tenant already has (an SPF, a DMARC, a DKIM key) is an "update" diff
//     entry carrying a warning, and the plan has a Fingerprint the UI confirms:
//     ApplyConfirmed refuses to run if the plan changed after the user saw it.
//   - Secrets (provider tokens) are held in memory only, redacted from String()
//     and never included in errors.
package domains
