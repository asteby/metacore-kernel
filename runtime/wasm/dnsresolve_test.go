package wasm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

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
	return func(_ context.Context, _ uuid.UUID, _ string, name string) error {
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
