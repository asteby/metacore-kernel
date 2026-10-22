package wasm

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/asteby/metacore-kernel/domains"
	"github.com/asteby/metacore-kernel/netguard"
	"github.com/google/uuid"
)

// Per-organization dns_resolve budget (on top of maxDNSResolvePerInvocation).
const (
	DefaultDNSOrgLimit  = 600
	DefaultDNSOrgWindow = time.Hour
)

// dnsOrgLimiter is a fixed-window counter per organization shared by every
// invocation of the host. Denied calls are charged too (the budget is taken
// before authorization). Bounded: stale windows are dropped on access.
type dnsOrgLimiter struct {
	max    int
	window time.Duration
	mu     sync.Mutex
	orgs   map[uuid.UUID]*dnsWindow
}

type dnsWindow struct {
	start time.Time
	n     int
}

func newDNSOrgLimiter(max int, window time.Duration) *dnsOrgLimiter {
	if max <= 0 {
		max = DefaultDNSOrgLimit
	}
	if window <= 0 {
		window = DefaultDNSOrgWindow
	}
	return &dnsOrgLimiter{max: max, window: window, orgs: map[uuid.UUID]*dnsWindow{}}
}

// allow charges one lookup to org. A nil limiter allows everything (a host
// built without WithDNS never reaches here anyway).
func (l *dnsOrgLimiter) allow(org uuid.UUID, now time.Time) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.orgs) > 4096 {
		for k, w := range l.orgs {
			if now.Sub(w.start) >= l.window {
				delete(l.orgs, k)
			}
		}
	}
	w := l.orgs[org]
	if w == nil || now.Sub(w.start) >= l.window {
		w = &dnsWindow{start: now}
		l.orgs[org] = w
	}
	if w.n >= l.max {
		return false
	}
	w.n++
	return true
}

// OrgDomainLister returns the domains an organization registered. Hosts back
// it with their org_domains table.
type OrgDomainLister interface {
	ListOrgDomains(ctx context.Context, orgID uuid.UUID) ([]domains.OrgDomain, error)
}

// ForbiddenDNSSuffixes aliases netguard.ForbiddenHostSuffixes: never
// resolvable through dns_resolve, even under a domain the organization owns.
var ForbiddenDNSSuffixes = netguard.ForbiddenHostSuffixes

// NewOrgDomainDNSAuthorizer is the reference DNSAuthorizeFunc. Rules, all
// fail-closed:
//
//  1. IP literals and names under ForbiddenDNSSuffixes are denied.
//  2. The name must equal or sit under an OrgDomain of the invoking
//     organization (longest match wins); disabled domains are denied.
//  3. The addon must hold `domain:manage` for at least one purpose of that
//     OrgDomain (req.CanManage), so a mail addon cannot resolve a domain
//     reserved for storefront.
//  4. While the domain is PENDING only the exact ownership proof is allowed:
//     TXT _asteby-verify.<host>. Arbitrary sub-names (which would let an addon
//     encode data into query names) are allowed only on VERIFIED domains.
func NewOrgDomainDNSAuthorizer(lister OrgDomainLister) DNSAuthorizeFunc {
	return func(ctx context.Context, req DNSAuthRequest) error {
		if lister == nil {
			return errors.New("no domain lister configured")
		}
		name := strings.ToLower(strings.TrimSuffix(req.Name, "."))
		if _, err := netip.ParseAddr(name); err == nil {
			return errors.New("IP literal")
		}
		if netguard.HasForbiddenSuffix(name) {
			return errors.New("forbidden special-use suffix")
		}
		list, err := lister.ListOrgDomains(ctx, req.OrgID)
		if err != nil {
			return fmt.Errorf("listing org domains: %w", err)
		}
		var best *domains.OrgDomain
		for i := range list {
			d := &list[i]
			// A row with no organization is malformed, never "everyone's".
			if d.OrganizationID == uuid.Nil || d.OrganizationID != req.OrgID {
				continue
			}
			if name == d.Host || strings.HasSuffix(name, "."+d.Host) {
				if best == nil || len(d.Host) > len(best.Host) {
					best = d
				}
			}
		}
		if best == nil {
			return fmt.Errorf("%s is not under a domain of this organization", name)
		}
		if best.Status == domains.StatusDisabled {
			return fmt.Errorf("domain %s is disabled", best.Host)
		}
		if req.CanManage == nil {
			return errors.New("no capability check available")
		}
		allowedPurpose := false
		for _, p := range best.Purposes {
			if req.CanManage(p) == nil {
				allowedPurpose = true
				break
			}
		}
		if !allowedPurpose {
			return fmt.Errorf("addon %s lacks domain:manage for the purposes of %s", req.AddonKey, best.Host)
		}
		if best.Status != domains.StatusVerified {
			if req.Type != domains.TypeTXT || name != domains.VerifyRecordName(best.Host) {
				return fmt.Errorf("domain %s is pending: only TXT %s may be resolved", best.Host, domains.VerifyRecordName(best.Host))
			}
		}
		return nil
	}
}
