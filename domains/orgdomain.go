package domains

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Status is the ownership / availability state of an OrgDomain.
type Status string

const (
	// StatusPending: registered, ownership not proven yet. A pending domain
	// must not receive traffic nor mail.
	StatusPending Status = "pending"
	// StatusVerified: the _asteby-verify TXT proof passed.
	StatusVerified Status = "verified"
	// StatusDisabled: parked without deleting (keeps its verification).
	StatusDisabled Status = "disabled"
)

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	return s == StatusPending || s == StatusVerified || s == StatusDisabled
}

// DNS provider keys. Free-form beyond these two (a provider is an integration
// choice), validated only for shape.
const (
	// ProviderManual: the tenant edits DNS themselves; the platform only
	// tells them what to publish and verifies it.
	ProviderManual     = "manual"
	ProviderCloudflare = "cloudflare"
)

// Limits.
const (
	MaxPurposes    = 8
	MaxHostLen     = 253
	VerifyPrefix   = "_asteby-verify."
	verifyTokenPfx = "asteby-verify-"
)

var (
	hostLabelRe   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	purposeRe     = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	providerKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
)

// OrgDomain is a domain an organization owns (or is proving it owns). The
// struct carries no persistence tags: hosts map it onto their own table (ops
// org_domains) and keep Purposes in a column/array of their choice.
type OrgDomain struct {
	ID             uuid.UUID `json:"id,omitempty"`
	OrganizationID uuid.UUID `json:"organization_id,omitempty"`
	// Host is the lower-cased FQDN without scheme, port or trailing dot.
	Host string `json:"host"`
	// Purposes say what the domain is used for ("storefront", "mail",
	// "portal"). A domain may serve several. Addons manage only the domains
	// whose purposes match their domain:manage capability.
	Purposes []string `json:"purposes"`
	// VerifyToken is the value to publish at VerifyRecordName(Host).
	VerifyToken string `json:"verify_token,omitempty"`
	Status      Status `json:"status"`
	// DNSProvider is "manual" (default), "cloudflare" or another integration
	// key. It selects the DNSApplier the host uses, if any.
	DNSProvider string     `json:"dns_provider,omitempty"`
	VerifiedAt  *time.Time `json:"verified_at,omitempty"`
}

// NewOrgDomain builds a pending domain with a fresh verify token. host is
// normalized and validated (reserved lists suffixes nobody may register).
func NewOrgDomain(org uuid.UUID, host string, purposes []string, provider string, reserved ...string) (OrgDomain, error) {
	h := NormalizeHost(host)
	if err := ValidateHost(h, reserved...); err != nil {
		return OrgDomain{}, err
	}
	ps, err := NormalizePurposes(purposes)
	if err != nil {
		return OrgDomain{}, err
	}
	if provider == "" {
		provider = ProviderManual
	}
	d := OrgDomain{
		ID: uuid.New(), OrganizationID: org, Host: h, Purposes: ps,
		VerifyToken: NewVerifyToken(), Status: StatusPending, DNSProvider: provider,
	}
	return d, d.Validate()
}

// Validate checks the structural invariants of the row.
func (d OrgDomain) Validate() error {
	if err := ValidateHost(d.Host); err != nil {
		return err
	}
	if d.Host != NormalizeHost(d.Host) {
		return fmt.Errorf("domains: host %q is not normalized", d.Host)
	}
	if _, err := NormalizePurposes(d.Purposes); err != nil {
		return err
	}
	if len(d.Purposes) == 0 {
		return errors.New("domains: at least one purpose is required")
	}
	if !d.Status.Valid() {
		return fmt.Errorf("domains: status %q must be pending|verified|disabled", d.Status)
	}
	if d.DNSProvider != "" && !providerKeyRe.MatchString(d.DNSProvider) {
		return fmt.Errorf("domains: dns_provider %q must match %s", d.DNSProvider, providerKeyRe)
	}
	return nil
}

// HasPurpose reports whether the domain is used for p.
func (d OrgDomain) HasPurpose(p string) bool {
	for _, x := range d.Purposes {
		if x == p {
			return true
		}
	}
	return false
}

// Active reports whether the domain may serve traffic/mail: verified and not
// parked.
func (d OrgDomain) Active() bool { return d.Status == StatusVerified }

// VerifyRecord returns the TXT record the tenant must publish while the domain
// is pending (zero Record once verified or without token).
func (d OrgDomain) VerifyRecord() (Record, bool) {
	if d.VerifyToken == "" || d.Status != StatusPending {
		return Record{}, false
	}
	return Record{Type: TypeTXT, Name: VerifyRecordName(d.Host), Value: d.VerifyToken, Purpose: "ownership"}, true
}

// NormalizePurposes lower-cases nothing (purposes are keys), validates the
// shape, rejects duplicates and returns them sorted.
func NormalizePurposes(in []string) ([]string, error) {
	if len(in) > MaxPurposes {
		return nil, fmt.Errorf("domains: at most %d purposes", MaxPurposes)
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, p := range in {
		if !purposeRe.MatchString(p) {
			return nil, fmt.Errorf("domains: purpose %q must match %s", p, purposeRe)
		}
		if _, dup := seen[p]; dup {
			return nil, fmt.Errorf("domains: purpose %q is duplicated", p)
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// VerifyRecordName is the TXT name that proves ownership of host.
func VerifyRecordName(host string) string { return VerifyPrefix + host }

// NewVerifyToken mints an opaque ownership token ("asteby-verify-" + 128 bits).
func NewVerifyToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// rand.Read failing is catastrophic; never mint an empty token.
		return verifyTokenPfx + uuid.New().String()
	}
	return verifyTokenPfx + hex.EncodeToString(b)
}

// NormalizeHost reduces user input ("https://Tienda.Acme.MX:443/inicio") to the
// bare lower-case host. Identical to the storefront flow it generalizes, plus
// trailing-dot removal.
func NormalizeHost(raw string) string {
	h := strings.ToLower(strings.TrimSpace(raw))
	if h == "" {
		return ""
	}
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}
	if i := strings.IndexByte(h, '/'); i >= 0 {
		h = h[:i]
	}
	if i := strings.LastIndexByte(h, ':'); i >= 0 && !strings.HasPrefix(h, "[") {
		h = h[:i]
	}
	return strings.TrimSuffix(strings.TrimSpace(h), ".")
}

// HostError is a host-validation failure with a stable machine code. Message
// is the same text the storefront flow returns, so adopting this package
// changes no user-facing string.
type HostError struct {
	// Code: required | not_fqdn | ip_address | invalid | reserved.
	Code    string
	Message string
}

func (e *HostError) Error() string { return e.Message }

// ValidateHost accepts a customer-owned FQDN: at least one dot, valid DNS
// labels, not an IP, not under a reserved suffix (the platform base domain...).
func ValidateHost(host string, reserved ...string) error {
	if host == "" {
		return &HostError{"required", "host is required"}
	}
	if len(host) > MaxHostLen || !strings.Contains(host, ".") {
		return &HostError{"not_fqdn", "host must be a fully qualified domain name"}
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return &HostError{"ip_address", "an IP address is not a valid domain"}
	}
	for _, l := range strings.Split(host, ".") {
		if !hostLabelRe.MatchString(l) {
			return &HostError{"invalid", "host is not a valid domain name"}
		}
	}
	for _, r := range reserved {
		r = strings.ToLower(strings.Trim(strings.TrimSpace(r), "."))
		if r != "" && (host == r || strings.HasSuffix(host, "."+r)) {
			return &HostError{"reserved", "this domain belongs to the platform and cannot be registered"}
		}
	}
	return nil
}

// ParentZones lists the candidate zone apexes of host from the most specific to
// the registrable-looking two-label parent: "a.b.example.com" ->
// [a.b.example.com b.example.com example.com]. Providers try them in order.
func ParentZones(host string) []string {
	labels := strings.Split(host, ".")
	var out []string
	for i := 0; i+2 <= len(labels); i++ {
		out = append(out, strings.Join(labels[i:], "."))
	}
	return out
}
