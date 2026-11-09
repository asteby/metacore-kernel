package v3

import (
	"encoding/json"
	"strings"
	"testing"
)

// slotsBase is a minimal valid addon with a frontend and one slot; each test
// mutates it. Visibility defaults to public.
const slotsBase = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "slotdemo", "name": "Slot demo", "version": "1.0.0" },
  "compatibility": { "requires": [{ "key": "kernel", "version": ">=3.1.0 <4.0.0" }] },
  "tenancy": { "isolation": "shared", "rls_column": "organization_id" },
  "frontend": { "entry": "remoteEntry.js", "format": "module", "container": "slotdemo", "expose": "./RecordPanel" },
  "contributions": {
    "slots": [
      { "slot_kind": "record.panel", "entry": "./RecordPanel", "order": 10,
        "props": { "title": "Hola" }, "timeout_ms": 8000, "fallback": "notice" }
    ]
  }
}`

func slotsMutate(t *testing.T, fn func(m map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(slotsBase), &m); err != nil {
		t.Fatal(err)
	}
	if fn != nil {
		fn(m)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func slot0(m map[string]any) map[string]any {
	return m["contributions"].(map[string]any)["slots"].([]any)[0].(map[string]any)
}

func hasRule(warns []string, rule string) bool {
	for _, w := range warns {
		if strings.HasPrefix(w, "["+rule+"]") {
			return true
		}
	}
	return false
}

func TestSlotContribution_NewFieldsRoundTrip(t *testing.T) {
	raw := slotsMutate(t, nil)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	s := m.Contributions.Slots[0]
	if s.TimeoutMS != 8000 || s.Fallback != "notice" || s.Props["title"] != "Hola" {
		t.Fatalf("new fields not decoded: %+v", s)
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	m2, err := Parse(out)
	if err != nil {
		t.Fatalf("re-Parse: %v", err)
	}
	b1, _ := json.Marshal(m.Contributions.Slots)
	b2, _ := json.Marshal(m2.Contributions.Slots)
	if string(b1) != string(b2) {
		t.Fatalf("slots changed on round trip:\n%s\n%s", b1, b2)
	}
}

func TestSlotContribution_FieldsAreOptional(t *testing.T) {
	raw := slotsMutate(t, func(m map[string]any) {
		s := slot0(m)
		delete(s, "props")
		delete(s, "timeout_ms")
		delete(s, "fallback")
	})
	if _, err := Parse(raw); err != nil {
		t.Fatalf("a slot without the new fields must stay valid: %v", err)
	}
}

func TestSlotContribution_SchemaRejectsBadValues(t *testing.T) {
	cases := map[string]func(s map[string]any){
		"timeout below 1000": func(s map[string]any) { s["timeout_ms"] = 999 },
		"timeout above 30000": func(s map[string]any) { s["timeout_ms"] = 30001 },
		"fallback unknown":   func(s map[string]any) { s["fallback"] = "explode" },
		"props not object":   func(s map[string]any) { s["props"] = "x" },
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			raw := slotsMutate(t, func(m map[string]any) { fn(slot0(m)) })
			if _, err := Parse(raw); err == nil {
				t.Fatal("want a schema error")
			}
		})
	}
	for _, ms := range []int{1000, 30000} {
		raw := slotsMutate(t, func(m map[string]any) { slot0(m)["timeout_ms"] = ms })
		if _, err := Parse(raw); err != nil {
			t.Fatalf("timeout_ms=%d must be valid: %v", ms, err)
		}
	}
}

func TestSlotKindUnknown_WarnsInBothModes(t *testing.T) {
	for _, kind := range HostSlotCatalogOpsV1 {
		raw := slotsMutate(t, func(m map[string]any) { slot0(m)["slot_kind"] = kind })
		warns, err := ValidateWithOptions(raw, Options{})
		if err != nil || hasRule(warns, RuleSlotKindUnknown) {
			t.Fatalf("catalog kind %q: err=%v warns=%v", kind, err, warns)
		}
	}
	raw := slotsMutate(t, func(m map[string]any) { slot0(m)["slot_kind"] = "other_addon.panel" })
	for _, strict := range []bool{false, true} {
		warns, err := ValidateWithOptions(raw, Options{Strict: strict})
		if err != nil {
			t.Fatalf("strict=%v: an unknown slot kind must not be an error: %v", strict, err)
		}
		if !hasRule(warns, RuleSlotKindUnknown) {
			t.Fatalf("strict=%v: want slot_kind_unknown warning, got %v", strict, warns)
		}
	}
	// Published by the manifest itself: known, qualified or not.
	for _, kind := range []string{"detail", "slotdemo.detail"} {
		raw := slotsMutate(t, func(m map[string]any) {
			slot0(m)["slot_kind"] = kind
			m["extension_points"] = map[string]any{"slot_kinds": []any{map[string]any{"name": "detail"}}}
		})
		warns, err := ValidateWithOptions(raw, Options{Strict: true})
		if err != nil || hasRule(warns, RuleSlotKindUnknown) {
			t.Fatalf("own slot %q: err=%v warns=%v", kind, err, warns)
		}
	}
}

func TestPrivateFrontendIntegrity_WarnsNotErrors(t *testing.T) {
	private := func(m map[string]any) { m["metadata"].(map[string]any)["visibility"] = "private" }
	raw := slotsMutate(t, private)
	for _, strict := range []bool{false, true} {
		warns, err := ValidateWithOptions(raw, Options{Strict: strict})
		if err != nil {
			t.Fatalf("strict=%v: a private manifest without integrity must NOT be an error: %v", strict, err)
		}
		if !hasRule(warns, RulePrivateFrontendIntegrity) {
			t.Fatalf("strict=%v: want private_frontend_integrity warning, got %v", strict, warns)
		}
	}
	// With integrity, or when public, no warning.
	withIntegrity := slotsMutate(t, func(m map[string]any) {
		private(m)
		m["frontend"].(map[string]any)["integrity"] = "sha384-abc"
	})
	warns, err := ValidateWithOptions(withIntegrity, Options{Strict: true})
	if err != nil || hasRule(warns, RulePrivateFrontendIntegrity) {
		t.Fatalf("with integrity: err=%v warns=%v", err, warns)
	}
	warns, err = ValidateWithOptions(slotsMutate(t, nil), Options{Strict: true})
	if err != nil || hasRule(warns, RulePrivateFrontendIntegrity) {
		t.Fatalf("public: err=%v warns=%v", err, warns)
	}
}

func TestPrivateExplicitCapabilities_WarnsNotErrors(t *testing.T) {
	wasmAction := func(m map[string]any) {
		m["metadata"].(map[string]any)["visibility"] = "private"
		m["frontend"].(map[string]any)["integrity"] = "sha384-abc"
		m["models"] = []any{map[string]any{"key": "Thing", "table": "things", "columns": []any{
			map[string]any{"name": "id", "type": "uuid", "primary_key": true},
			map[string]any{"name": "organization_id", "type": "uuid", "not_null": true},
		}}}
		m["contributions"].(map[string]any)["actions"] = []any{map[string]any{
			"key": "stamp", "target_model": "Thing", "handler": map[string]any{"type": "wasm", "function": "stamp"},
		}}
	}
	raw := slotsMutate(t, wasmAction)
	for _, strict := range []bool{false, true} {
		warns, err := ValidateWithOptions(raw, Options{Strict: strict})
		if err != nil {
			t.Fatalf("strict=%v: unexpected error: %v", strict, err)
		}
		if !hasRule(warns, RulePrivateExplicitCapabilities) {
			t.Fatalf("strict=%v: want private_explicit_capabilities warning, got %v", strict, warns)
		}
	}
	explicit := slotsMutate(t, func(m map[string]any) {
		wasmAction(m)
		m["capabilities"] = []any{map[string]any{"kind": "db:write", "target": "addon_slotdemo.*"}}
	})
	warns, err := ValidateWithOptions(explicit, Options{Strict: true})
	if err != nil || hasRule(warns, RulePrivateExplicitCapabilities) {
		t.Fatalf("explicit capabilities: err=%v warns=%v", err, warns)
	}
}

func TestSlotRules_RegisteredInCompatWindow(t *testing.T) {
	have := map[string]bool{}
	for _, r := range LegacyTolerated() {
		have[r] = true
	}
	for _, r := range []string{RuleSlotKindUnknown, RulePrivateFrontendIntegrity, RulePrivateExplicitCapabilities} {
		if !have[r] {
			t.Fatalf("%s not registered in compat.go", r)
		}
		if _, ok := warnOnlyWindow[r]; !ok {
			t.Fatalf("%s must be warn-only in both modes", r)
		}
	}
}
