package installer

import (
	"errors"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

type fakeCatalog struct{ addons map[string]manifest.Manifest }

func (c fakeCatalog) ContractOwner(k string) (manifest.Manifest, bool) {
	for _, m := range c.addons {
		for _, ct := range m.Contracts {
			if ct.Key == k {
				return m, true
			}
		}
	}
	return manifest.Manifest{}, false
}

func (c fakeCatalog) Addon(k string) (manifest.Manifest, bool) { m, ok := c.addons[k]; return m, ok }

func paymentsCore() manifest.Manifest {
	return manifest.Manifest{Key: "payments_core", Version: "1.0.0", Role: "contract",
		Contracts:     []v3.Contract{{Key: "payments.charge", Version: "1.2.0", Selection: &v3.ContractSelection{Cardinality: "one"}}},
		AddonRequires: []manifest.AddonRequirement{{Key: "ledger_base", Range: "^1"}}}
}

func catalog() fakeCatalog {
	return fakeCatalog{addons: map[string]manifest.Manifest{
		"payments_core": paymentsCore(),
		"ledger_base":   {Key: "ledger_base", Version: "1.0.0"},
		"mercadopago":   {Key: "mercadopago", Version: "3.0.0", Implements: []v3.Implements{{Contract: "payments.charge", Version: "^1.2"}}},
	}}
}

func pos() manifest.Manifest {
	return manifest.Manifest{Key: "pos", Version: "2.0.0",
		ContractRequires: []manifest.ContractRequirement{{Contract: "payments.charge", Range: "^1"}}}
}

// A6: a consumer of a contract installs the OWNER (and what it needs), never a
// provider; the org is told to choose one.
func TestPlanContractInstall_ConsumerInstallsOwnerNotProvider(t *testing.T) {
	plan, err := PlanContractInstall(nil, pos(), catalog())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(plan.Order, ","); got != "ledger_base,payments_core,pos" {
		t.Fatalf("order = %s", got)
	}
	if plan.Owners["payments.charge"] != "payments_core" {
		t.Fatalf("owners = %v", plan.Owners)
	}
	if strings.Join(plan.ChooseProvider, ",") != "payments.charge" {
		t.Fatalf("choose provider = %v", plan.ChooseProvider)
	}
	if _, ok := plan.Manifests["mercadopago"]; ok {
		t.Fatal("a provider was installed on the org's behalf")
	}
	// Once a provider is installed nothing is left to choose; an installed
	// owner is not reinstalled.
	installed := []InstalledAddon{InstalledFromManifest(paymentsCore()), InstalledFromManifest(catalog().addons["mercadopago"])}
	plan, err = PlanContractInstall(installed, pos(), catalog())
	if err != nil || len(plan.Install) != 0 || len(plan.ChooseProvider) != 0 {
		t.Fatalf("plan = %+v err=%v", plan, err)
	}
}

func TestPlanContractInstall_ProviderBringsOwner(t *testing.T) {
	plan, err := PlanContractInstall(nil, catalog().addons["mercadopago"], catalog())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(plan.Order, ","); got != "ledger_base,payments_core,mercadopago" {
		t.Fatalf("order = %s", got)
	}
	if len(plan.ChooseProvider) != 0 {
		t.Fatalf("a provider needs no provider: %v", plan.ChooseProvider)
	}
}

func TestPlanContractInstall_Errors(t *testing.T) {
	c := pos()
	c.ContractRequires = append(c.ContractRequires, manifest.ContractRequirement{Contract: "nobody.owns"})
	if _, err := PlanContractInstall(nil, c, catalog()); !errors.Is(err, ErrUnknownContract) {
		t.Fatalf("err = %v", err)
	}
	// Optional contract requires are never installed.
	o := manifest.Manifest{Key: "x", ContractRequires: []manifest.ContractRequirement{{Contract: "nobody.owns", Optional: true}}}
	if plan, err := PlanContractInstall(nil, o, catalog()); err != nil || len(plan.Install) != 0 {
		t.Fatalf("optional: %+v %v", plan, err)
	}
	// a.one -> owner b, b needs b2.one -> c, c needs c2.one -> d, d needs d2.one -> e: depth 4.
	deep := fakeCatalog{addons: map[string]manifest.Manifest{}}
	chain := []string{"b", "c", "d", "e"}
	for i, k := range chain {
		m := manifest.Manifest{Key: k, Contracts: []v3.Contract{{Key: k + ".one", Version: "1.0.0"}}}
		if i+1 < len(chain) {
			m.ContractRequires = []manifest.ContractRequirement{{Contract: chain[i+1] + ".one"}}
		}
		deep.addons[k] = m
	}
	a := manifest.Manifest{Key: "a", ContractRequires: []manifest.ContractRequirement{{Contract: "b.one"}}}
	if _, err := PlanContractInstall(nil, a, deep); !errors.Is(err, ErrContractPlanTooDeep) {
		t.Fatalf("err = %v", err)
	}
	// Cycle: owner b requires a contract owned by c, c requires b's.
	cyc := fakeCatalog{addons: map[string]manifest.Manifest{
		"b": {Key: "b", Contracts: []v3.Contract{{Key: "b.one", Version: "1.0.0"}}, ContractRequires: []manifest.ContractRequirement{{Contract: "c.one"}}},
		"c": {Key: "c", Contracts: []v3.Contract{{Key: "c.one", Version: "1.0.0"}}, ContractRequires: []manifest.ContractRequirement{{Contract: "b.one"}}},
	}}
	var ce ErrDependencyCycle
	if _, err := PlanContractInstall(nil, a, cyc); !errors.As(err, &ce) {
		t.Fatalf("err = %v", err)
	}
}

func TestCheckInstallable_ContractRequires(t *testing.T) {
	var me ErrMissingContract
	if err := CheckInstallable(nil, pos()); !errors.As(err, &me) || me.Contract != "payments.charge" {
		t.Fatalf("err = %v", err)
	}
	if err := CheckInstallable([]InstalledAddon{InstalledFromManifest(paymentsCore())}, pos()); err != nil {
		t.Fatalf("owner installed: %v", err)
	}
	old := paymentsCore()
	old.Contracts[0].Version = "0.9.0"
	var ie ErrIncompatibleContract
	if err := CheckInstallable([]InstalledAddon{InstalledFromManifest(old)}, pos()); !errors.As(err, &ie) || ie.Have != "0.9.0" {
		t.Fatalf("err = %v", err)
	}
	// Uninstalling the owner is blocked by the consumer (§6.4) ...
	installed := []InstalledAddon{InstalledFromManifest(paymentsCore()), InstalledFromManifest(pos())}
	if deps := Dependents(installed, "payments_core"); strings.Join(deps, ",") != "pos" {
		t.Fatalf("dependents = %v", deps)
	}
	if order := UninstallOrder(installed, "payments_core"); strings.Join(order, ",") != "pos,payments_core" {
		t.Fatalf("uninstall order = %v", order)
	}
	// ... and an upgrade that breaks the consumer's range is refused.
	up := paymentsCore()
	up.Version, up.Contracts[0].Version = "2.0.0", "2.0.0"
	if err := CheckUpgradable(installed, up); !errors.As(err, &ie) || ie.Addon != "pos" {
		t.Fatalf("upgrade err = %v", err)
	}
	drop := paymentsCore()
	drop.Version, drop.Contracts = "3.0.0", nil
	if err := CheckUpgradable(installed, drop); !errors.As(err, &me) || me.Addon != "pos" {
		t.Fatalf("drop err = %v", err)
	}
	// ResolveOrder puts the owner first.
	order, err := ResolveOrder([]manifest.Manifest{pos(), paymentsCore()})
	if err != nil || strings.Join(order, ",") != "payments_core,pos" {
		t.Fatalf("order = %v %v", order, err)
	}
}

func TestNavAnchorProblems_OnlyNewOnes(t *testing.T) {
	owner := func(key string) manifest.Manifest {
		return manifest.Manifest{Key: key, NavAnchors: []v3.NavAnchor{{Key: "catalog.product", Section: "catalog", Title: "t", Model: "Product", Accepts: []string{"tab"}}}}
	}
	if ps := NavAnchorProblems([]manifest.Manifest{owner("products")}, owner("catalog_core")); len(ps) != 1 {
		t.Fatalf("problems = %v", ps)
	}
	// Upgrading the same owner is not a conflict with itself.
	if ps := NavAnchorProblems([]manifest.Manifest{owner("products")}, owner("products")); len(ps) != 0 {
		t.Fatalf("problems = %v", ps)
	}
	plug := manifest.Manifest{Key: "acme", Navigation: []manifest.NavGroup{{Title: "x", Items: []manifest.NavItem{{Title: "x", AttachTo: "catalog.product", As: "action", Actions: []string{"a"}}}}}}
	if ps := NavAnchorProblems([]manifest.Manifest{owner("products")}, plug); len(ps) != 1 || !strings.Contains(ps[0], "does not accept") {
		t.Fatalf("problems = %v", ps)
	}
	if usesNavAnchors(manifest.Manifest{Navigation: []manifest.NavGroup{{Title: "x", Items: []manifest.NavItem{{Title: "y"}}}}}) {
		t.Fatal("a legacy manifest must skip the anchor gate")
	}
}
