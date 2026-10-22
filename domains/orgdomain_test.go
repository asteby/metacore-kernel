package domains

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestNormalizeHost(t *testing.T) {
	cases := map[string]string{
		"https://Tienda.Acme.MX/inicio": "tienda.acme.mx",
		"  SHOP.acme.mx  ":              "shop.acme.mx",
		"shop.acme.mx:8443":             "shop.acme.mx",
		"http://shop.acme.mx:80/a/b":    "shop.acme.mx",
		"shop.acme.mx.":                 "shop.acme.mx",
		"":                              "",
		"   ":                           "",
	}
	for in, want := range cases {
		if got := NormalizeHost(in); got != want {
			t.Errorf("NormalizeHost(%q) = %q, want %q", in, got, want)
		}
	}
}

// The messages are byte-identical to the storefront flow they generalize.
func TestValidateHostMatchesStorefrontFlow(t *testing.T) {
	cases := []struct {
		host     string
		reserved []string
		code     string
		msg      string
	}{
		{"", nil, "required", "host is required"},
		{"localhost", nil, "not_fqdn", "host must be a fully qualified domain name"},
		{strings.Repeat("a", 250) + ".com", nil, "not_fqdn", "host must be a fully qualified domain name"},
		{"192.168.0.1", nil, "ip_address", "an IP address is not a valid domain"},
		{"bad_label.acme.mx", nil, "invalid", "host is not a valid domain name"},
		{"-x.acme.mx", nil, "invalid", "host is not a valid domain name"},
		{"a..b.mx", nil, "invalid", "host is not a valid domain name"},
		{"tienda.tiendas.test", []string{"tiendas.test"}, "reserved", "this domain belongs to the platform and cannot be registered"},
		{"tiendas.test", []string{" Tiendas.Test. "}, "reserved", "this domain belongs to the platform and cannot be registered"},
	}
	for _, c := range cases {
		err := ValidateHost(c.host, c.reserved...)
		var he *HostError
		if !errors.As(err, &he) || he.Code != c.code || he.Message != c.msg {
			t.Errorf("ValidateHost(%q) = %v, want code %s msg %q", c.host, err, c.code, c.msg)
		}
	}
	for _, ok := range []string{"tienda.acme.mx", "a.b.c.example.co.uk", "xn--mxico-bva.mx", "shop-1.acme.mx"} {
		if err := ValidateHost(ok, "tiendas.test"); err != nil {
			t.Errorf("ValidateHost(%q) rejected: %v", ok, err)
		}
	}
}

func TestVerifyToken(t *testing.T) {
	a, b := NewVerifyToken(), NewVerifyToken()
	if !strings.HasPrefix(a, "asteby-verify-") || len(a) != len("asteby-verify-")+32 || a == b {
		t.Fatalf("tokens: %q %q", a, b)
	}
	if VerifyRecordName("acme.mx") != "_asteby-verify.acme.mx" {
		t.Fatal("verify record name")
	}
}

func TestNewOrgDomain(t *testing.T) {
	org := uuid.New()
	d, err := NewOrgDomain(org, "https://Mail.Acme.MX/", []string{"mail", "storefront"}, "", "tiendas.test")
	if err != nil {
		t.Fatal(err)
	}
	if d.Host != "mail.acme.mx" || d.Status != StatusPending || d.DNSProvider != ProviderManual || d.VerifyToken == "" || d.OrganizationID != org {
		t.Fatalf("%+v", d)
	}
	if !d.HasPurpose("mail") || d.HasPurpose("portal") || d.Active() {
		t.Fatal("purposes / active")
	}
	rec, ok := d.VerifyRecord()
	if !ok || rec.Name != "_asteby-verify.mail.acme.mx" || rec.Value != d.VerifyToken || rec.Type != TypeTXT {
		t.Fatalf("verify record: %+v", rec)
	}
	d.Status = StatusVerified
	if _, ok := d.VerifyRecord(); ok || !d.Active() {
		t.Fatal("verified domain publishes no ownership record")
	}
	bad := []struct {
		host     string
		purposes []string
		provider string
	}{
		{"nope", []string{"mail"}, ""},
		{"x.tiendas.test", []string{"mail"}, ""},
		{"ok.acme.mx", nil, ""},
		{"ok.acme.mx", []string{"Mail"}, ""},
		{"ok.acme.mx", []string{"mail", "mail"}, ""},
		{"ok.acme.mx", []string{"mail"}, "Bad Provider"},
		{"ok.acme.mx", make([]string, 9), ""},
	}
	for i, c := range bad {
		if _, err := NewOrgDomain(org, c.host, c.purposes, c.provider, "tiendas.test"); err == nil {
			t.Errorf("case %d: expected an error", i)
		}
	}
	d2 := OrgDomain{Host: "A.acme.mx", Purposes: []string{"mail"}, Status: StatusPending}
	if d2.Validate() == nil {
		t.Error("non-normalized host must fail Validate")
	}
	d3 := OrgDomain{Host: "a.acme.mx", Purposes: []string{"mail"}, Status: "weird"}
	if d3.Validate() == nil {
		t.Error("bad status must fail Validate")
	}
	ps, _ := NormalizePurposes([]string{"mail", "app"})
	if strings.Join(ps, ",") != "app,mail" {
		t.Errorf("purposes sorted: %v", ps)
	}
}

func TestParentZones(t *testing.T) {
	got := strings.Join(ParentZones("a.b.example.com"), ",")
	if got != "a.b.example.com,b.example.com,example.com" {
		t.Fatalf("ParentZones = %s", got)
	}
	if strings.Join(ParentZones("example.com"), ",") != "example.com" {
		t.Fatal("apex")
	}
}
