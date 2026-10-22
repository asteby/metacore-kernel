package v3

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

type jm = map[string]interface{}

func loadSetupExample(t *testing.T) jm {
	t.Helper()
	b, err := os.ReadFile("../../docs/spec/v3/examples/addon-setup-example.json")
	if err != nil {
		t.Fatal(err)
	}
	var m jm
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func setupSteps(m jm) []interface{} {
	setup := m["contributions"].(jm)["setup"].([]interface{})
	return setup[0].(jm)["steps"].([]interface{})
}

func setupStep(m jm, key string) jm {
	for _, s := range setupSteps(m) {
		if s.(jm)["key"] == key {
			return s.(jm)
		}
	}
	panic("no step " + key)
}

func setupContrib(m jm) jm { return m["contributions"].(jm) }
func setupFlow0(m jm) jm   { return setupContrib(m)["setup"].([]interface{})[0].(jm) }

func TestSetupExampleIsValidAndStrict(t *testing.T) {
	m := loadSetupExample(t)
	if err := Validate(mustJSON(t, m)); err != nil {
		t.Fatal(err)
	}
	// Parse (typed, DisallowUnknownFields) must round-trip the new blocks.
	parsed, err := Parse(mustJSON(t, m))
	if err != nil {
		t.Fatal(err)
	}
	c := parsed.Contributions
	if len(c.Setup) != 1 || len(c.Setup[0].Steps) != 7 || c.Health == nil || c.Health.Every != "5m" {
		t.Fatalf("setup/health not decoded: %+v", c)
	}
	if !parsed.Settings[1].WriteOnly || parsed.Settings[1].VisibleWhen == nil || parsed.Settings[1].VisibleWhen.Field != "dns_mode" {
		t.Fatalf("setting extras not decoded: %+v", parsed.Settings[1])
	}
	if l := parsed.Runtime.NativeService.Listeners; len(l) != 3 || l[0].ProxyProtocol != "v2" {
		t.Fatalf("listeners not decoded: %+v", l)
	}
}

func TestSetupValidation_Negative(t *testing.T) {
	cases := []struct {
		name string
		mut  func(m jm)
		want string // substring of the error
	}{
		{"unknown top-level field in flow", func(m jm) { setupFlow0(m)["bogus"] = 1 }, "bogus"},
		{"bad flow key", func(m jm) { setupFlow0(m)["key"] = "Main-Flow" }, "key"},
		{"bad trigger", func(m jm) { setupFlow0(m)["trigger"] = "always" }, "trigger"},
		{"bad scope", func(m jm) { setupFlow0(m)["scope"] = "global" }, "scope"},
		{"empty steps", func(m jm) { setupFlow0(m)["steps"] = []interface{}{} }, "steps"},
		{"missing title", func(m jm) { setupFlow0(m)["title"] = "" }, "title"},
		{"duplicate flow key", func(m jm) {
			c := setupContrib(m)
			second := jm{}
			b, _ := json.Marshal(setupFlow0(m))
			_ = json.Unmarshal(b, &second)
			c["setup"] = append(c["setup"].([]interface{}), second)
		}, "duplicated"},
		{"two on_install flows", func(m jm) {
			c := setupContrib(m)
			second := jm{}
			b, _ := json.Marshal(setupFlow0(m))
			_ = json.Unmarshal(b, &second)
			second["key"] = "other"
			c["setup"] = append(c["setup"].([]interface{}), second)
		}, "at most one flow"},
		{"undeclared own permission", func(m jm) { setupFlow0(m)["permission"] = "mailer.setup.nope" }, "not declared in rbac.permissions"},
		{"malformed permission", func(m jm) { setupFlow0(m)["permission"] = "Bad Perm" }, "permission"},
		{"duplicate step key", func(m jm) {
			steps := setupFlow0(m)["steps"].([]interface{})
			steps[1].(jm)["key"] = "node"
		}, "duplicated in the flow"},
		{"unknown kind", func(m jm) { setupStep(m, "node")["kind"] = "wizard" }, "kind"},
		{"kind without body", func(m jm) { delete(setupStep(m, "node"), "check") }, "check is required when kind=check"},
		{"kind with foreign body", func(m jm) {
			setupStep(m, "node")["form"] = jm{"settings": []interface{}{"dns_mode"}}
		}, "form is not allowed when kind=check"},
		{"depends_on unknown", func(m jm) { setupStep(m, "settings")["depends_on"] = []interface{}{"ghost"} }, "unknown step"},
		{"depends_on self", func(m jm) { setupStep(m, "settings")["depends_on"] = []interface{}{"settings"} }, "depends on itself"},
		{"depends_on duplicate", func(m jm) { setupStep(m, "settings")["depends_on"] = []interface{}{"node", "node"} }, "twice"},
		{"depends_on cycle", func(m jm) { setupStep(m, "node")["depends_on"] = []interface{}{"dns_check"} }, "cycle"},
		{"native handler without operation", func(m jm) {
			setupStep(m, "node")["check"].(jm)["handler"] = jm{"type": "native"}
		}, "operation"},
		{"wasm handler without function", func(m jm) {
			setupStep(m, "dns_check")["check"].(jm)["handler"] = jm{"type": "wasm"}
		}, "function is required"},
		{"wasm handler bad symbol", func(m jm) {
			setupStep(m, "dns_check")["check"].(jm)["handler"] = jm{"type": "wasm", "function": "1bad-name"}
		}, "function"},
		{"unsupported handler type", func(m jm) {
			setupStep(m, "dns_check")["check"].(jm)["handler"] = jm{"type": "webhook"}
		}, "handler"},
		{"native handler without native_service", func(m jm) { delete(m, "runtime") }, "requires runtime.native_service"},
		{"bad check timeout", func(m jm) { setupStep(m, "dns_check")["check"].(jm)["timeout"] = "1h" }, "timeout"},
		{"poll every too small", func(m jm) {
			setupStep(m, "dns_check")["check"].(jm)["poll"].(jm)["every"] = "10ms"
		}, "poll.every"},
		{"poll max below every", func(m jm) {
			setupStep(m, "dns_check")["check"].(jm)["poll"].(jm)["max_every"] = "5s"
		}, "max_every"},
		{"poll factor below 1", func(m jm) {
			setupStep(m, "dns_check")["check"].(jm)["poll"].(jm)["factor"] = 0.5
		}, "factor"},
		{"poll give_up invalid", func(m jm) {
			setupStep(m, "dns_check")["check"].(jm)["poll"].(jm)["give_up_after"] = "forever"
		}, "give_up_after"},
		{"form with both", func(m jm) {
			setupStep(m, "settings")["form"].(jm)["connector"] = "cloudflare"
		}, "form"},
		{"form with neither", func(m jm) { setupStep(m, "settings")["form"] = jm{} }, "form"},
		{"form unknown setting", func(m jm) {
			setupStep(m, "settings")["form"] = jm{"settings": []interface{}{"ghost"}}
		}, "not a setting of this addon"},
		{"form duplicate setting", func(m jm) {
			setupStep(m, "settings")["form"] = jm{"settings": []interface{}{"dns_mode", "dns_mode"}}
		}, "twice"},
		{"form unknown connector", func(m jm) {
			setupStep(m, "cloudflare")["form"] = jm{"connector": "ghost"}
		}, "not a connector of this addon"},
		{"action not declared", func(m jm) {
			setupStep(m, "apply")["action"] = jm{"action": "ghost"}
		}, "not declared in contributions.actions"},
		{"custom slot not contributed", func(m jm) {
			setupStep(m, "dns_help")["custom"] = jm{"slot": "mailer.nowhere"}
		}, "not contributed in contributions.slots"},
		{"skip_when unknown setting", func(m jm) {
			setupStep(m, "cloudflare")["skip_when"] = jm{"setting": "ghost", "equals": "x"}
		}, "skip_when.setting"},
		{"skip_when needs predicate", func(m jm) {
			setupStep(m, "cloudflare")["skip_when"] = jm{"setting": "dns_mode"}
		}, "exactly one of equals or in"},
		{"skip_when both predicates", func(m jm) {
			setupStep(m, "cloudflare")["skip_when"] = jm{"setting": "dns_mode", "equals": "a", "in": []interface{}{"b"}}
		}, "exactly one of equals or in"},
		{"health bad every", func(m jm) { setupContrib(m)["health"].(jm)["every"] = "1s" }, "contributions.health.every"},
		{"health bad handler", func(m jm) { setupContrib(m)["health"].(jm)["handler"] = jm{"type": "wasm"} }, "contributions.health.handler"},
		{"health undeclared permission", func(m jm) { setupContrib(m)["health"].(jm)["permission"] = "mailer.health.nope" }, "not declared in rbac.permissions"},
		{"visible_when unknown sibling", func(m jm) {
			m["settings"].([]interface{})[1].(jm)["visible_when"] = jm{"field": "ghost", "equals": "x"}
		}, "not a sibling setting"},
		{"visible_when self", func(m jm) {
			m["settings"].([]interface{})[1].(jm)["visible_when"] = jm{"field": "relay_password", "equals": "x"}
		}, "reference itself"},
		{"visible_when class", func(m jm) {
			m["settings"].([]interface{})[1].(jm)["visible_when"] = jm{"class": "x"}
		}, "class"},
		{"visible_when cycle", func(m jm) {
			s := m["settings"].([]interface{})
			s[0].(jm)["visible_when"] = jm{"field": "relay_password", "equals": "x"}
		}, "cycle"},
		{"visible_when without predicate", func(m jm) {
			m["settings"].([]interface{})[1].(jm)["visible_when"] = jm{"field": "dns_mode"}
		}, "requires equals or in"},
		{"write_only with default", func(m jm) {
			m["settings"].([]interface{})[1].(jm)["default"] = "hunter2"
		}, "write_only cannot declare a default"},
		{"credential visible_when unknown", func(m jm) {
			cr := m["connectors"].([]interface{})[0].(jm)["credentials"].([]interface{})
			cr[1].(jm)["visible_when"] = jm{"field": "ghost", "equals": "x"}
		}, "not a sibling setting"},
		{"write_only non-boolean", func(m jm) {
			m["settings"].([]interface{})[1].(jm)["write_only"] = "yes"
		}, "write_only"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := loadSetupExample(t)
			c.mut(m)
			err := Validate(mustJSON(t, m))
			if err == nil {
				t.Fatalf("expected an error containing %q, got none", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not contain %q", err, c.want)
			}
		})
	}
}

func TestSetupBackwardCompatible(t *testing.T) {
	// A manifest with no setup/health/visible_when/write_only validates as before.
	m := loadSetupExample(t)
	delete(setupContrib(m), "setup")
	delete(setupContrib(m), "health")
	for _, s := range m["settings"].([]interface{}) {
		delete(s.(jm), "visible_when")
		delete(s.(jm), "write_only")
	}
	if err := Validate(mustJSON(t, m)); err != nil {
		t.Fatal(err)
	}
}

func TestSetupDependencyCycle(t *testing.T) {
	steps := []SetupStep{
		{Key: "a", DependsOn: []string{"b"}},
		{Key: "b", DependsOn: []string{"c"}},
		{Key: "c", DependsOn: []string{"a"}},
		{Key: "d"},
	}
	cyc := SetupDependencyCycle(steps)
	if len(cyc) != 4 || cyc[0] != cyc[len(cyc)-1] {
		t.Fatalf("cycle = %v", cyc)
	}
	if SetupDependencyCycle([]SetupStep{{Key: "a"}, {Key: "b", DependsOn: []string{"a"}}, {Key: "c", DependsOn: []string{"a", "b"}}}) != nil {
		t.Fatal("diamond DAG is not a cycle")
	}
	// Unknown and self references are not cycles (reported separately).
	if SetupDependencyCycle([]SetupStep{{Key: "a", DependsOn: []string{"a", "zzz"}}}) != nil {
		t.Fatal("self/unknown refs must not be reported as cycles")
	}
}

func TestSetupWasmExports(t *testing.T) {
	m := loadSetupExample(t)
	parsed, err := Parse(mustJSON(t, m))
	if err != nil {
		t.Fatal(err)
	}
	got := SetupWasmExports(parsed.Contributions.Setup, parsed.Contributions.Health)
	want := []string{"ListRequiredDnsRecords", "VerifyDns", "HealthSummary"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("exports = %v want %v", got, want)
	}
}

func TestValidateSetup_LegacyRefsSkipUncheckable(t *testing.T) {
	flows := []SetupFlow{{Key: "main", Title: "t", Permission: "mailer.setup.manage", Steps: []SetupStep{
		{Key: "c", Title: "c", Kind: SetupKindCustom, Custom: &SetupCustom{Slot: "mailer.any"}},
	}}}
	// Slots/Permissions nil: not checkable -> accepted.
	if errs := ValidateSetup(flows, nil, SetupRefs{AddonKey: "mailer"}); len(errs) != 0 {
		t.Fatalf("nil Slots/Permissions must be skipped, got %v", errs)
	}
	// Non-nil empty: enforced.
	errs := ValidateSetup(flows, nil, SetupRefs{AddonKey: "mailer", Slots: map[string]struct{}{}, Permissions: map[string]struct{}{}})
	if len(errs) != 2 {
		t.Fatalf("want slot + permission errors, got %v", errs)
	}
	// Explicit backend.exports membership.
	hflows := []SetupFlow{{Key: "m", Title: "t", Steps: []SetupStep{
		{Key: "c", Title: "c", Kind: SetupKindCheck, Check: &SetupCheck{Handler: SetupHandler{Type: "wasm", Function: "Nope"}}},
	}}}
	if errs := ValidateSetup(hflows, nil, SetupRefs{Exports: map[string]struct{}{"Other": {}}}); len(errs) != 1 || !strings.Contains(errs[0], "backend.exports") {
		t.Fatalf("export membership: %v", errs)
	}
}
