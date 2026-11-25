package capability

import (
	"errors"
	"testing"
)

// A contract declared by an installed owner is selectable by market without a
// hard-coded catalog: binding of the market > org default > single provider,
// ambiguous otherwise (RFC SDK-0001 §3.2, O5).
func TestRegistrySelectableDeclaredContract(t *testing.T) {
	r := NewRegistry()
	if err := r.Register("payments_core", Declared{Key: "acme_pay.charge", Version: "1.2.0", Operations: []DeclaredOperation{{Key: "create", Kind: KindAction}}}); err != nil {
		t.Fatal(err)
	}
	c, ok := r.Selectable("acme_pay.charge")
	if !ok || c.Cardinality != "" || c.Semver() != "1.2.0" {
		t.Fatalf("selectable = %+v %v", c, ok)
	}
	if owner, ok := r.Owner("acme_pay.charge"); !ok || owner != "payments_core" {
		t.Fatalf("owner = %q", owner)
	}
	ps := []Provider{{Addon: "mp", Countries: []string{"MX"}}, {Addon: "stripe", Markets: []string{"us"}}}
	if got, err := Select(c, ps, Market{Code: "us"}); err != nil || got[0].Addon != "stripe" {
		t.Fatalf("market binding: %v %v", got, err)
	}
	if _, err := Select(c, ps, Market{}); !errors.Is(err, ErrAmbiguousProvider) {
		t.Fatalf("org level with two providers: %v", err)
	}
	if got, err := Select(c, ps[:1], Market{}); err != nil || got[0].Addon != "mp" {
		t.Fatalf("single provider: %v %v", got, err)
	}
	if _, ok := r.Selectable("nobody.owns"); ok {
		t.Fatal("unknown contract selectable")
	}
	var nilReg *Registry
	if _, ok := nilReg.Selectable(CommercePaymentsConfirm); !ok {
		t.Fatal("kernel contracts stay selectable through a nil registry")
	}
	_ = r.Register("many_core", Declared{Key: "acme_ship.rate", Version: "1.0.0", Cardinality: All})
	if c, _ := r.Selectable("acme_ship.rate"); !c.Fanout() {
		t.Fatal("cardinality many must fan out")
	}
}
