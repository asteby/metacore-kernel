package wasm

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/domains"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/security"
	"github.com/google/uuid"
)

type stubDNS struct {
	vals  map[string][]string
	err   error
	calls []string
}

func (s *stubDNS) Name() string { return "stub" }
func (s *stubDNS) Lookup(_ context.Context, name string, t domains.RecordType) ([]string, error) {
	s.calls = append(s.calls, string(t)+" "+name)
	if s.err != nil {
		return nil, s.err
	}
	if v, ok := s.vals[string(t)+"|"+name]; ok {
		return v, nil
	}
	return nil, domains.ErrNoRecords
}

func dnsInv(caps []manifest.Capability, r domains.DNSResolver, auth DNSAuthorizeFunc) *invocation {
	return &invocation{
		addonKey: "mail", orgID: uuid.New(),
		caps:        security.Compile("mail", caps),
		dnsResolver: r, dnsAuthorize: auth,
	}
}

func allowUnder(suffix string) DNSAuthorizeFunc {
	return func(_ context.Context, req DNSAuthRequest) error {
		name := req.Name
		if name == suffix || strings.HasSuffix(name, "."+suffix) {
			return nil
		}
		return fmt.Errorf("%s is not under a domain of this organization", name)
	}
}

var manageMail = []manifest.Capability{{Kind: "domain:manage", Target: "mail"}}

func TestDNSResolve_Success(t *testing.T) {
	r := &stubDNS{vals: map[string][]string{"TXT|_asteby-verify.acme.mx": {"asteby-verify-abc"}, "MX|acme.mx": {"10 mx.acme.mx"}}}
	inv := dnsInv(manageMail, r, allowUnder("acme.mx"))
	ok, d := capEnv(t, executeDNSResolve(context.Background(), inv, []byte(`{"name":"_Asteby-Verify.Acme.MX.","type":"txt"}`)))
	if !ok || d["name"] != "_asteby-verify.acme.mx" || d["type"] != "TXT" || d["no_records"] != false {
		t.Fatalf("%v", d)
	}
	vals := d["values"].([]any)
	if len(vals) != 1 || vals[0] != "asteby-verify-abc" {
		t.Fatalf("values = %v", vals)
	}
	if len(r.calls) != 1 || r.calls[0] != "TXT _asteby-verify.acme.mx" {
		t.Fatalf("lookups: %v", r.calls)
	}
}

func TestDNSResolve_NoRecordsIsSuccessLookupFailureIsError(t *testing.T) {
	inv := dnsInv(manageMail, &stubDNS{}, allowUnder("acme.mx"))
	ok, d := capEnv(t, executeDNSResolve(context.Background(), inv, []byte(`{"name":"acme.mx","type":"MX"}`)))
	if !ok || d["no_records"] != true || len(d["values"].([]any)) != 0 {
		t.Fatalf("authoritative empty answer: %v", d)
	}
	inv = dnsInv(manageMail, &stubDNS{err: errors.New("i/o timeout")}, allowUnder("acme.mx"))
	ok, e := capEnv(t, executeDNSResolve(context.Background(), inv, []byte(`{"name":"acme.mx","type":"MX"}`)))
	if ok || e["code"] != "dns_lookup_failed" {
		t.Fatalf("a failed lookup must never look like 'missing': %v", e)
	}
}

func TestDNSResolve_Gates(t *testing.T) {
	good := &stubDNS{}
	cases := []struct {
		name string
		inv  *invocation
		req  string
		code string
	}{
		{"no capability", dnsInv(nil, good, allowUnder("acme.mx")), `{"name":"acme.mx","type":"TXT"}`, "forbidden"},
		{"other capability", dnsInv([]manifest.Capability{{Kind: "http:fetch", Target: "api.x.com"}}, good, allowUnder("acme.mx")), `{"name":"acme.mx","type":"TXT"}`, "forbidden"},
		{"name outside org domains", dnsInv(manageMail, good, allowUnder("acme.mx")), `{"name":"victim.com","type":"TXT"}`, "forbidden"},
		{"no authorizer fails closed", dnsInv(manageMail, good, nil), `{"name":"acme.mx","type":"TXT"}`, "forbidden"},
		{"no resolver", dnsInv(manageMail, nil, allowUnder("acme.mx")), `{"name":"acme.mx","type":"TXT"}`, "dns_unavailable"},
		{"bad type", dnsInv(manageMail, good, allowUnder("acme.mx")), `{"name":"acme.mx","type":"SOA"}`, "invalid_request"},
		{"bad name", dnsInv(manageMail, good, allowUnder("acme.mx")), `{"name":"a b.acme.mx","type":"TXT"}`, "invalid_request"},
		{"empty name", dnsInv(manageMail, good, allowUnder("acme.mx")), `{"name":"","type":"TXT"}`, "invalid_request"},
		{"bad json", dnsInv(manageMail, good, allowUnder("acme.mx")), `{`, "invalid_request"},
	}
	for _, c := range cases {
		ok, e := capEnv(t, executeDNSResolve(context.Background(), c.inv, []byte(c.req)))
		if ok || e["code"] != c.code {
			t.Errorf("%s: ok=%v err=%v want %s", c.name, ok, e, c.code)
		}
	}
	if len(good.calls) != 0 {
		t.Fatalf("a refused request must never reach the resolver: %v", good.calls)
	}
	noOrg := dnsInv(manageMail, good, allowUnder("acme.mx"))
	noOrg.orgID = uuid.Nil
	if ok, e := capEnv(t, executeDNSResolve(context.Background(), noOrg, []byte(`{"name":"acme.mx","type":"TXT"}`))); ok || e["code"] != "no_active_org" {
		t.Errorf("no org: %v", e)
	}
	if ok, e := capEnv(t, executeDNSResolve(context.Background(), nil, []byte(`{}`))); ok || e["code"] != "invalid_request" {
		t.Errorf("nil invocation: %v", e)
	}
}

func TestDNSResolve_PerInvocationBudget(t *testing.T) {
	inv := dnsInv(manageMail, &stubDNS{}, allowUnder("acme.mx"))
	for i := 0; i < maxDNSResolvePerInvocation; i++ {
		if ok, e := capEnv(t, executeDNSResolve(context.Background(), inv, []byte(`{"name":"acme.mx","type":"TXT"}`))); !ok {
			t.Fatalf("call %d refused early: %v", i, e)
		}
	}
	if ok, e := capEnv(t, executeDNSResolve(context.Background(), inv, []byte(`{"name":"acme.mx","type":"TXT"}`))); ok || e["code"] != "rate_limited" {
		t.Fatalf("budget not enforced: %v", e)
	}
}

func TestHostWithDNS(t *testing.T) {
	h := &Host{}
	r := &stubDNS{}
	if h.WithDNS(r, allowUnder("x.com")) != h || h.dnsResolver != r || h.dnsAuthorize == nil {
		t.Fatal("WithDNS must store both and chain")
	}
}

type listerStub []domains.OrgDomain

func (l listerStub) ListOrgDomains(context.Context, uuid.UUID) ([]domains.OrgDomain, error) {
	return l, nil
}

func TestDNSResolve_DenialsAreGenericAndCharged(t *testing.T) {
	inv := dnsInv(manageMail, &stubDNS{}, func(context.Context, DNSAuthRequest) error {
		return errors.New("secret detail: org owns shop.internal-corp.com (verified)")
	})
	logged := ""
	inv.logger = log.New(&logBuf{&logged}, "", 0)
	ok, e := capEnv(t, executeDNSResolve(context.Background(), inv, []byte(`{"name":"acme.mx","type":"TXT"}`)))
	if ok || e["code"] != "forbidden" {
		t.Fatalf("%v", e)
	}
	if strings.Contains(fmt.Sprint(e), "secret detail") {
		t.Fatalf("authorizer error leaked to the guest: %v", e)
	}
	if !strings.Contains(logged, "secret detail") {
		t.Fatalf("detail must reach the audit log: %q", logged)
	}
	// Denials consume the per-invocation budget.
	for i := 1; i < maxDNSResolvePerInvocation; i++ {
		executeDNSResolve(context.Background(), inv, []byte(`{"name":"acme.mx","type":"TXT"}`))
	}
	if inv.dnsCalls != maxDNSResolvePerInvocation {
		t.Fatalf("dnsCalls=%d", inv.dnsCalls)
	}
	if _, e := capEnv(t, executeDNSResolve(context.Background(), inv, []byte(`{"name":"acme.mx","type":"TXT"}`))); e["code"] != "rate_limited" {
		t.Fatalf("denied calls must exhaust the budget: %v", e)
	}
}

func TestDNSResolve_OrgBudgetAcrossInvocations(t *testing.T) {
	lim := newDNSOrgLimiter(3, time.Hour)
	org := uuid.New()
	for i := 0; i < 3; i++ {
		inv := dnsInv(manageMail, &stubDNS{}, allowUnder("acme.mx"))
		inv.orgID, inv.dnsLimits = org, lim
		if ok, e := capEnv(t, executeDNSResolve(context.Background(), inv, []byte(`{"name":"victim.com","type":"TXT"}`))); ok || e["code"] != "forbidden" {
			t.Fatalf("call %d: %v", i, e)
		}
	}
	inv := dnsInv(manageMail, &stubDNS{}, allowUnder("acme.mx"))
	inv.orgID, inv.dnsLimits = org, lim
	if _, e := capEnv(t, executeDNSResolve(context.Background(), inv, []byte(`{"name":"acme.mx","type":"TXT"}`))); e["code"] != "rate_limited" {
		t.Fatalf("org budget not enforced across invocations: %v", e)
	}
	other := dnsInv(manageMail, &stubDNS{}, allowUnder("acme.mx"))
	other.dnsLimits = lim
	if ok, _ := capEnv(t, executeDNSResolve(context.Background(), other, []byte(`{"name":"acme.mx","type":"TXT"}`))); !ok {
		t.Fatal("another org must not be affected")
	}
}

func TestOrgDomainDNSAuthorizer(t *testing.T) {
	org := uuid.New()
	verified := domains.OrgDomain{OrganizationID: org, Host: "acme.mx", Purposes: []string{"mail"}, Status: domains.StatusVerified}
	pending := domains.OrgDomain{OrganizationID: org, Host: "new.mx", Purposes: []string{"mail"}, Status: domains.StatusPending, VerifyToken: "t"}
	store := domains.OrgDomain{OrganizationID: org, Host: "shop.mx", Purposes: []string{"storefront"}, Status: domains.StatusVerified}
	parked := domains.OrgDomain{OrganizationID: org, Host: "off.mx", Purposes: []string{"mail"}, Status: domains.StatusDisabled}
	az := NewOrgDomainDNSAuthorizer(listerStub{verified, pending, store, parked})
	mail := security.Compile("mail", manageMail)
	cases := []struct {
		name, typ string
		want      bool
	}{
		{"acme.mx", "MX", true},
		{"anything.deep.acme.mx", "TXT", true},    // verified: arbitrary sub-names
		{"_asteby-verify.new.mx", "TXT", true},    // pending: exact proof only
		{"_asteby-verify.new.mx", "A", false},     // wrong type
		{"data.new.mx", "TXT", false},             // pending: no arbitrary sub-name
		{"new.mx", "TXT", false},                  // pending: not even the apex
		{"x._asteby-verify.new.mx", "TXT", false}, // not the exact name
		{"shop.mx", "A", false},                   // purpose not managed by this addon
		{"off.mx", "A", false},                    // disabled
		{"victim.com", "A", false},                // not an org domain
		{"acme.mx.evil.com", "A", false},
		{"x.acme.mx.internal", "A", false},
		{"db.cluster.local", "A", false},
		{"1.0.0.127.in-addr.arpa", "A", false},
		{"127.0.0.1", "A", false},
		{"::1", "A", false},
		{"host.local", "A", false},
	}
	for _, c := range cases {
		err := az(context.Background(), DNSAuthRequest{OrgID: org, AddonKey: "mail", Name: c.name, Type: domains.RecordType(c.typ), CanManage: mail.CanManageDomain})
		if (err == nil) != c.want {
			t.Errorf("%s %s: err=%v want allowed=%v", c.typ, c.name, err, c.want)
		}
	}
	// Other organization's domain row is ignored.
	foreign := verified
	foreign.OrganizationID = uuid.New()
	az2 := NewOrgDomainDNSAuthorizer(listerStub{foreign})
	if err := az2(context.Background(), DNSAuthRequest{OrgID: org, Name: "acme.mx", Type: "A", CanManage: mail.CanManageDomain}); err == nil {
		t.Error("foreign org domain must not authorize")
	}
	// No capability for the purpose.
	if err := az(context.Background(), DNSAuthRequest{OrgID: org, Name: "acme.mx", Type: "A", CanManage: security.Compile("x", nil).CanManageDomain}); err == nil {
		t.Error("addon without domain:manage must be denied")
	}
}

type logBuf struct{ s *string }

func (l *logBuf) Write(p []byte) (int, error) { *l.s += string(p); return len(p), nil }
