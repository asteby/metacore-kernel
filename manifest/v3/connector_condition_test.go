package v3

import (
	"strings"
	"testing"
)

// connectorGateManifest wraps one action condition in the minimum valid addon
// manifest that also declares the connector the gate references, so Parse
// exercises the JSON schema ($defs/Condition has additionalProperties:false —
// an unknown key would be rejected outright) and then validateConditions.
func connectorGateManifest(condition string) []byte {
	return []byte(`{
      "apiVersion": "asteby.com/v3",
      "kind": "Addon",
      "metadata": {"key": "fiscal_mexico", "name": "CFDI", "version": "1.0.0"},
      "compatibility": {"requires": [{"key": "kernel", "version": ">=3.0.0 <4.0.0"}]},
      "connectors": [
        {"key": "factura_com", "label": "factura.com", "auth": "token",
         "credentials": [{"key": "api_key", "type": "secret", "required": true}]}
      ],
      "contributions": {
        "actions": [
          {"key": "stamp_fiscal", "label": "Timbrar", "target_model": "Invoice",
           "handler": {"type": "wasm", "export": "stamp"},
           "condition": ` + condition + `}
        ]
      }
    }`)
}

func TestConnectorConnectedConditionParses(t *testing.T) {
	m, err := Parse(connectorGateManifest(`{"connector_connected": "factura_com"}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	c := m.Contributions.Actions[0].Condition
	if c == nil || c.ConnectorConnected != "factura_com" {
		t.Fatalf("connector gate not parsed: %+v", c)
	}
}

// The gate composes with the record-level field predicate: an action can need
// BOTH a connected PAC and an unstamped invoice.
func TestConnectorGateComposesWithFieldCondition(t *testing.T) {
	m, err := Parse(connectorGateManifest(
		`{"connector_connected": "factura_com", "field": "fiscal_uuid", "operator": "falsy"}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	c := m.Contributions.Actions[0].Condition
	if c.ConnectorConnected != "factura_com" || c.Field != "fiscal_uuid" || c.Operator != "falsy" {
		t.Fatalf("composed condition not parsed: %+v", c)
	}
}

// A gate on a connector the addon does not declare can never become connected,
// so it would silently disable the action forever. That is an authoring bug.
func TestConnectorGateRejectsUndeclaredConnector(t *testing.T) {
	err := Validate(connectorGateManifest(`{"connector_connected": "stripe"}`))
	if err == nil {
		t.Fatal("want an error for a connector the addon does not declare")
	}
	if !strings.Contains(err.Error(), "not a connector declared by this addon") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestConditionRejectsUnknownUnmetPolicy(t *testing.T) {
	err := Validate(connectorGateManifest(`{"connector_connected": "factura_com", "unmet": "explode"}`))
	if err == nil {
		t.Fatal("want an error for an unknown unmet policy")
	}
}

// "disable" is a UI affordance driven by an org-level reason; a record-only
// condition has no reason to report, so declaring it is a mistake.
func TestUnmetDisableNeedsOrgLevelPredicate(t *testing.T) {
	err := Validate(connectorGateManifest(`{"field": "fiscal_uuid", "operator": "falsy", "unmet": "disable"}`))
	if err == nil {
		t.Fatal("want an error for unmet:disable without an org-level predicate")
	}
	if !strings.Contains(err.Error(), "org-level predicate") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// The defaults encode the UX decision: a connector the operator can go connect
// leaves a disabled button with a reason; an addon they have not installed is
// not actionable from that screen, so it stays hidden.
func TestUnmetPolicyDefaults(t *testing.T) {
	cases := []struct {
		name string
		c    *Condition
		want string
	}{
		{"connector defaults to disable", &Condition{ConnectorConnected: "factura_com"}, UnmetDisable},
		{"addon defaults to hide", &Condition{AddonInstalled: "workshop"}, UnmetHide},
		{"explicit wins", &Condition{ConnectorConnected: "factura_com", Unmet: UnmetHide}, UnmetHide},
		{"nil has nothing to enforce", nil, ""},
	}
	for _, tc := range cases {
		if got := tc.c.UnmetPolicy(); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestSatisfiedByResolvesBothPredicates(t *testing.T) {
	installed := func(k string) bool { return k == "workshop" }
	connected := func(k string) bool { return k == "factura_com" }

	both := &Condition{AddonInstalled: "workshop", ConnectorConnected: "factura_com"}
	if !both.SatisfiedBy(installed, connected) {
		t.Error("both predicates hold, want satisfied")
	}
	if (&Condition{ConnectorConnected: "stripe"}).SatisfiedBy(installed, connected) {
		t.Error("stripe is not connected, want unsatisfied")
	}
	if (&Condition{AddonInstalled: "taller"}).SatisfiedBy(installed, connected) {
		t.Error("taller is not installed, want unsatisfied")
	}
}

// A host that cannot resolve a predicate must not silently gate the
// contribution — that is how an upgrade would make every button vanish.
func TestUnresolvablePredicateIsTreatedAsMet(t *testing.T) {
	c := &Condition{ConnectorConnected: "factura_com"}
	if !c.SatisfiedBy(nil, nil) {
		t.Error("nil resolvers must be treated as met")
	}
	// Satisfied() is the legacy addon-only entry point: it knows no connector
	// resolver, so a connector gate must not make it report false.
	if !c.Satisfied(func(string) bool { return true }) {
		t.Error("Satisfied must ignore the connector predicate it cannot resolve")
	}
}
