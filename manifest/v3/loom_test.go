package v3

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// loomBase: the fiscal_mexico case of RFC 0005 (csf reader exposed as a Part
// and grafted on the customer dialog header).
const loomBase = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "fiscal_mexico", "name": "Fiscal", "version": "1.0.0" },
  "compatibility": { "requires": [{ "key": "kernel", "version": ">=3.1.0 <4.0.0" }] },
  "tenancy": { "isolation": "shared", "rls_column": "organization_id" },
  "frontend": { "entry": "remoteEntry.js", "format": "federation", "container": "fiscal_mexico", "expose": "./parts/csf-import" },
  "ui": {
    "exposes": [
      { "id": "fiscal_mexico.csf-import", "version": "1.0.0", "expose": "./parts/csf-import",
        "needs": { "network": "none", "offline": true, "records": { "find": ["customers.Customer"] }, "device": ["camera"] }, "self_review": true }
    ],
    "seams": [
      { "id": "fiscal_mexico.invoice.footer", "version": "1.0.0", "lenses": { "invoice": "money", "customer": "pii.fiscal" } }
    ],
    "contributions": [
      { "id": "fiscal_mexico.csf-in-customer", "part": "fiscal_mexico.csf-import", "seam": "record.dialog.header",
        "requires": "^1", "kind": "view", "order": 10,
        "when": { "model": ["Customer"], "mode": ["create", "edit"], "permission": ["customer.create"],
                  "setting": { "key": "fiscal_mexico.csf_reader", "equals": true },
                  "condition": { "addon_installed": "customers" } },
        "reads": ["values", "mode", "recordId"], "writes": ["name", "tax_id", "fiscal_data.*"],
        "emits": ["record.select"], "network": "none", "offline": true }
    ]
  }
}`

func loomMutate(t *testing.T, fn func(m map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(loomBase), &m); err != nil {
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

func graft0(m map[string]any) map[string]any {
	return m["ui"].(map[string]any)["contributions"].([]any)[0].(map[string]any)
}

func TestLoom_ValidAndRoundTrip(t *testing.T) {
	raw := loomMutate(t, nil)
	warns, err := ValidateWithOptions(raw, Options{Strict: true})
	if err != nil {
		t.Fatalf("valid ui block rejected: %v", err)
	}
	if hasRule(warns, RuleSlotKindUnknown) {
		t.Fatalf("record.dialog.header is in the SDK catalog: %v", warns)
	}
	m, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	g := m.UI.Contributions[0]
	if g.When.Setting.Equals != true || g.When.Condition.AddonInstalled != "customers" || g.Writes[2] != "fiscal_data.*" {
		t.Fatalf("decoded graft: %#v", g)
	}
	b1, _ := json.Marshal(m)
	var m2 Manifest
	if err := json.Unmarshal(b1, &m2); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(m.UI, m2.UI) {
		t.Fatalf("ui changed on round trip")
	}
}

// A manifest without `ui` serializes without the key: existing manifests are
// served byte-for-byte as before.
func TestLoom_AbsentIsOmitted(t *testing.T) {
	m, err := Parse(slotsMutate(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if m.UI != nil {
		t.Fatal("ui must stay nil when absent")
	}
	b, _ := json.Marshal(m)
	if strings.Contains(string(b), `"ui"`) {
		t.Fatalf("absent ui leaked into the wire: %s", b)
	}
}

func TestLoom_Errors(t *testing.T) {
	cases := []struct {
		name string
		mut  func(m map[string]any)
		want string
	}{
		{"unqualified part id", func(m map[string]any) {
			m["ui"].(map[string]any)["exposes"].([]any)[0].(map[string]any)["id"] = "csf"
		}, "schema validation failed"},
		{"foreign prefix", func(m map[string]any) { graft0(m)["id"] = "other.csf" }, "must start with this addon's key"},
		{"own part not exposed", func(m map[string]any) { graft0(m)["part"] = "fiscal_mexico.missing" }, "is not in this manifest's ui.exposes"},
		{"managed column", func(m map[string]any) { graft0(m)["writes"] = []any{"tax_id", "organization_id"} }, "managed column"},
		{"replace outside preset", func(m map[string]any) { graft0(m)["mode"] = "replace" }, "only allowed in a Preset"},
		{"bad requires", func(m map[string]any) { graft0(m)["requires"] = "^^1" }, "not a semver range"},
		{"hosts without network", func(m map[string]any) { graft0(m)["hosts"] = []any{"x.com"} }, "only valid with network hosts"},
		{"network hosts empty", func(m map[string]any) { graft0(m)["network"] = "hosts" }, "requires a non-empty hosts list"},
		{"bad seam version", func(m map[string]any) {
			m["ui"].(map[string]any)["seams"].([]any)[0].(map[string]any)["version"] = "uno"
		}, "must be a semver version"},
		{"bad data class", func(m map[string]any) {
			m["ui"].(map[string]any)["seams"].([]any)[0].(map[string]any)["lenses"] = map[string]any{"x": "secretish"}
		}, "schema validation failed"},
		{"unknown graft field", func(m map[string]any) { graft0(m)["component"] = "x" }, "component"},
		{"bad device", func(m map[string]any) { graft0(m)["device"] = []any{"bluetooth"} }, "schema validation failed"},
		{"duplicated device", func(m map[string]any) { graft0(m)["device"] = []any{"camera", "camera"} }, "twice"},
		{"empty records model", func(m map[string]any) {
			m["ui"].(map[string]any)["exposes"].([]any)[0].(map[string]any)["needs"] = map[string]any{"records": map[string]any{"find": []any{" "}}}
		}, "must name a model"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ValidateWithOptions(loomMutate(t, c.mut), Options{})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want error containing %q, got %v", c.want, err)
			}
		})
	}
}

func TestLoom_UnknownSeamIsWarning(t *testing.T) {
	raw := loomMutate(t, func(m map[string]any) { graft0(m)["seam"] = "pos.cart.summary" })
	warns, err := ValidateWithOptions(raw, Options{Strict: true})
	if err != nil {
		t.Fatalf("an unknown seam must not reject: %v", err)
	}
	if !hasRule(warns, RuleSlotKindUnknown) {
		t.Fatalf("want slot_kind_unknown warning, got %v", warns)
	}
}
