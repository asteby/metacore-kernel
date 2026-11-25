package v3

import (
	"strings"
	"testing"
)

// navOwner is a "customers"-like owner of the sales.order anchor (RFC SDK-0001 §4.3).
func navOwner(mut func(m map[string]interface{})) map[string]interface{} {
	m := baseValid()
	m["metadata"].(map[string]interface{})["key"] = "customers"
	m["capabilities"] = []interface{}{mm("kind", "db:write", "target", "addon_customers.*")}
	m["models"] = []interface{}{mm("key", "SalesOrder", "table", "sales_orders", "columns", []interface{}{
		mm("name", "id", "type", "uuid"), mm("name", "organization_id", "type", "uuid", "not_null", true),
	})}
	m["extension_points"] = mm("nav_anchors", []interface{}{
		mm("key", "sales.order", "section", "sales", "title", "nav.sales.orders", "model", "SalesOrder", "order", 10,
			"accepts", []interface{}{"tab", "view", "action", "panel", "filter", "mode"}, "provisional", true),
	})
	m["contributions"] = mm("navigation", []interface{}{
		mm("key", "customers.sales", "section", "sales", "order", 10, "title", "nav.sales.orders", "items", []interface{}{
			mm("key", "pending_credit", "title", "nav.sales.pending_credit", "attach_to", "sales.order", "as", "filter",
				"filter", mm("state", "credit_review"), "locked_scope", true),
		}),
	})
	if mut != nil {
		mut(m)
	}
	return m
}

func navGroup0(m map[string]interface{}) map[string]interface{} {
	return m["contributions"].(map[string]interface{})["navigation"].([]interface{})[0].(map[string]interface{})
}

func navItem0(m map[string]interface{}) map[string]interface{} {
	return navGroup0(m)["items"].([]interface{})[0].(map[string]interface{})
}

func TestNavAnchors_ValidOwnerAndPlugs(t *testing.T) {
	warns, err := ValidateWithOptions(mustJSON(t, navOwner(nil)), Options{Strict: true})
	if err != nil {
		t.Fatalf("valid owner rejected: %v", err)
	}
	for _, w := range warns {
		if strings.Contains(w, RuleNavTargetUnknown) {
			t.Fatalf("unexpected warning %q", w)
		}
	}
	// A plug-in of another addon attaching to an anchor it does not see.
	m := baseValid()
	m["metadata"].(map[string]interface{})["key"] = "fiscal_x"
	m["capabilities"] = []interface{}{mm("kind", "db:write", "target", "addon_fiscal_x.*")}
	m["contributions"] = mm("navigation", []interface{}{
		mm("key", "fiscal_x.sales", "section", "sales", "title", "nav.fiscal", "items", []interface{}{
			mm("key", "invoices_tab", "title", "t", "attach_to", "sales.order", "as", "tab", "model", "Invoice", "fallback", "hide", "order", 30),
			mm("key", "unknown_anchor", "title", "t", "attach_to", "finance.nowhere", "as", "view", "model", "Invoice"),
		}),
	})
	if err := Validate(mustJSON(t, m)); err != nil {
		t.Fatalf("attach_to an anchor that is not installed must validate (fallback applies): %v", err)
	}
}

func TestNavAnchors_Errors(t *testing.T) {
	cases := map[string]struct {
		mut  func(m map[string]interface{})
		want string
	}{
		"section outside registry": {func(m map[string]interface{}) { navGroup0(m)["section"] = "tickets" }, "section"},
		"anchor section mismatch": {func(m map[string]interface{}) {
			m["extension_points"].(map[string]interface{})["nav_anchors"].([]interface{})[0].(map[string]interface{})["section"] = "finance"
		}, "must start with its section"},
		"anchor without resource": {func(m map[string]interface{}) {
			delete(m["extension_points"].(map[string]interface{})["nav_anchors"].([]interface{})[0].(map[string]interface{}), "model")
		}, "exactly one of model, page or url"},
		"anchor foreign model": {func(m map[string]interface{}) {
			m["extension_points"].(map[string]interface{})["nav_anchors"].([]interface{})[0].(map[string]interface{})["model"] = "Invoice"
		}, "is not a model of this addon"},
		"duplicate anchor": {func(m map[string]interface{}) {
			ep := m["extension_points"].(map[string]interface{})
			a := ep["nav_anchors"].([]interface{})
			ep["nav_anchors"] = append(a, a[0])
		}, "duplicate anchor key"},
		"as not accepted by own anchor": {func(m map[string]interface{}) {
			navItem0(m)["as"] = "setting"
			delete(navItem0(m), "filter")
		}, "does not accept as \"setting\""},
		"as without attach_to": {func(m map[string]interface{}) { delete(navItem0(m), "attach_to") }, "as is only valid with attach_to"},
		"attach_to without as": {func(m map[string]interface{}) { delete(navItem0(m), "as") }, "as is required with attach_to"},
		"fallback section on action": {func(m map[string]interface{}) {
			it := navItem0(m)
			it["as"], it["fallback"], it["actions"] = "action", "section", []interface{}{"approve"}
		}, "fallback \"section\" is not allowed"},
		"action without actions": {func(m map[string]interface{}) { navItem0(m)["as"] = "action" }, "needs actions[]"},
		"mode without page":      {func(m map[string]interface{}) { navItem0(m)["as"] = "mode" }, "needs page"},
		"nested attach_to": {func(m map[string]interface{}) {
			navGroup0(m)["items"] = []interface{}{mm("title", "parent", "items", []interface{}{navItem0(m)})}
		}, "only valid on a group's top-level items"},
		"group key of another addon":    {func(m map[string]interface{}) { navGroup0(m)["key"] = "products.sales" }, "must be \"customers.<name>\""},
		"target disagrees with section": {func(m map[string]interface{}) { navGroup0(m)["target"] = "sidebar.finance" }, "disagree"},
		"duplicate item key": {func(m map[string]interface{}) {
			g := navGroup0(m)
			g["items"] = append(g["items"].([]interface{}), mm("key", "pending_credit", "title", "x", "model", "SalesOrder"))
		}, "used by another item"},
		"contract and key": {func(m map[string]interface{}) {
			c := m["compatibility"].(map[string]interface{})
			c["requires"] = append(c["requires"].([]interface{}), mm("key", "x", "contract", "payments.charge", "version", "^1"))
		}, "schema validation failed"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			err := Validate(mustJSON(t, navOwner(c.mut)))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want error containing %q, got %v", c.want, err)
			}
		})
	}
}

func TestNavTargetUnknownIsAWarning(t *testing.T) {
	m := baseValid()
	m["contributions"] = mm("navigation", []interface{}{mm("title", "Tickets", "target", "sidebar.tickets", "items", []interface{}{})})
	warns, err := ValidateWithOptions(mustJSON(t, m), Options{Strict: true})
	if err != nil {
		t.Fatalf("legacy target must still validate: %v", err)
	}
	found := false
	for _, w := range warns {
		found = found || strings.Contains(w, "["+RuleNavTargetUnknown+"]")
	}
	if !found {
		t.Fatalf("want %s warning, got %v", RuleNavTargetUnknown, warns)
	}
}

func TestNavSectionForTarget(t *testing.T) {
	for target, want := range map[string]struct {
		s     string
		known bool
	}{
		"": {"operations", true}, "sidebar.sales": {"sales", true}, "sidebar.sales_orders": {"sales", true},
		"sidebar.finance": {"finance", true}, "catalog": {"catalog", true}, "sidebar.Customers": {"customers", true},
		"sidebar.tickets": {"operations", false}, "sidebar.tools": {"operations", false}, "sidebar.settings": {"settings", true},
	} {
		s, known := NavSectionForTarget(target)
		if s != want.s || known != want.known {
			t.Errorf("%q -> %q,%v want %q,%v", target, s, known, want.s, want.known)
		}
	}
	if DefaultNavFallback(NavAsTab) != NavFallbackSection || DefaultNavFallback(NavAsAction) != NavFallbackHide ||
		DefaultNavFallback(NavAsSetting) != NavFallbackHide || DefaultNavFallback(NavAsMode) != NavFallbackSection {
		t.Fatal("A4 fallback defaults changed")
	}
}

func TestRequiresContract(t *testing.T) {
	m := baseValid()
	c := m["compatibility"].(map[string]interface{})
	c["requires"] = append(c["requires"].([]interface{}),
		mm("contract", "payments.charge", "version", "^1", "optional", true, "enables", []interface{}{"nav:pos.charge"}))
	mf, _, err := ParseWithOptions(mustJSON(t, m), Options{Strict: true})
	if err != nil {
		t.Fatalf("contract require rejected: %v", err)
	}
	got := mf.ContractRequirements()
	if len(got) != 1 || got[0].Contract != "payments.charge" || got[0].Version != "^1" || !got[0].Optional || len(got[0].Enables) != 1 {
		t.Fatalf("ContractRequirements = %+v", got)
	}
	if opt := mf.OptionalRequirements(); len(opt) != 0 {
		t.Fatalf("a contract require is not an optional ADDON requirement: %+v", opt)
	}
	if facts := mf.RoleFacts(); len(facts.Requires) != 1 || facts.Requires[0].Key != "kernel" {
		t.Fatalf("role facts must ignore contract requires: %+v", facts.Requires)
	}
	c["requires"] = append(c["requires"].([]interface{}), mm("contract", "Payments", "version", "^1"))
	if err := Validate(mustJSON(t, m)); err == nil {
		t.Fatal("malformed contract key accepted")
	}
}

func TestContractConformanceWarning(t *testing.T) {
	cs := []Contract{{Key: "a.b", Version: "1.0.0"}, {Key: "a.c", Version: "0.9.0"}, {Key: "a.d", Version: "2.0.0", Conformance: "conformance/a.d"}}
	ps := CheckContractConformance(cs)
	if len(ps) != 1 || !strings.Contains(ps[0], "a.b") {
		t.Fatalf("CheckContractConformance = %v", ps)
	}
}
