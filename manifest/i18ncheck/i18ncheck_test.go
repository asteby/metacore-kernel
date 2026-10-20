package i18ncheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const manifestJSON = `{
  "apiVersion": "asteby.com/v3", "kind": "Addon",
  "metadata": { "key": "pay", "name": "Pay", "version": "0.1.0" },
  "i18n": { "default_locale": "es-MX", "bundles": [
    { "locale": "es-MX", "path": "locales/es-MX.json" },
    { "locale": "en-US", "path": "locales/en-US.json" } ] },
  "settings": [
    { "key": "session_expiration_minutes", "type": "integer",
      "label": "pay.settings.session_expiration_minutes",
      "description": "pay.settings.session_expiration_minutes_help" },
    { "key": "literal", "type": "string", "label": "Etiqueta literal" }
  ],
  "connectors": [ { "key": "pay", "label": "pay.connector.label",
    "form_layout": { "mode": "steps", "sections": [ { "key": "c", "title": "pay.connector.section.c" } ] },
    "credentials": [ { "key": "access_token", "type": "secret", "label": "pay.connector.field.access_token",
      "input_rules": { "pattern_hint": "pay.connector.hint.access_token",
        "forbid_patterns": [ { "pattern": "^pk_", "message": "pay.connector.error.public_key" } ] } },
      { "key": "env", "type": "select", "options": [ { "value": "sandbox", "label": "pay.option.sandbox" } ] } ] } ],
  "contributions": { "actions": [ { "key": "refund", "label": "pay.action.refund",
    "handler": { "type": "wasm", "function": "h" },
    "fields": [ { "key": "amount", "type": "number", "label": "pay.action.refund.amount" } ] } ] }
}`

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCheckDir(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "manifest.json", manifestJSON)
	full := `{"pay":{"settings":{"session_expiration_minutes":"Vigencia","session_expiration_minutes_help":"Minutos"},
	  "connector":{"label":"Pay","section":{"c":"Conexión"},"field":{"access_token":"Access Token"},
	    "hint":{"access_token":"APP_USR-…"},"error":{"public_key":"Parece una Public Key"}},
	  "option":{"sandbox":"Pruebas"},"action":{"refund":{"amount":"Monto"}}}}`
	write(t, dir, "locales/es-MX.json", strings.Replace(full, `"action":{"refund":{"amount":"Monto"}}`, `"action":{"refund":{"amount":"Monto"}},"x":"y"`, 1))
	// en-US: flat, unprefixed, missing two keys and one empty.
	write(t, dir, "locales/en-US.json", `{"settings.session_expiration_minutes":"Validity",
	  "connector.label":"Pay","connector.section.c":"Connection","connector.field.access_token":"Access Token",
	  "connector.hint.access_token":"","connector.error.public_key":"Looks like a public key","option.sandbox":"Sandbox"}`)
	rep, err := CheckDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	// es-MX lacks pay.action.refund (the action label; "refund" is an object there).
	got := map[string]bool{}
	for _, m := range rep.Missing {
		got[m.Locale+" "+m.Key] = true
	}
	want := []string{
		"es-MX pay.action.refund",
		"en-US pay.settings.session_expiration_minutes_help",
		"en-US pay.connector.hint.access_token",
		"en-US pay.action.refund",
		"en-US pay.action.refund.amount",
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing %q not reported; got %v", w, got)
		}
	}
	if len(rep.Missing) != len(want) {
		t.Errorf("got %d missing, want %d: %v", len(rep.Missing), len(want), rep.Missing)
	}
	if rep.OK() {
		t.Fatal("report must fail")
	}
	if rep.Referenced != 10 {
		t.Errorf("referenced = %d", rep.Referenced)
	}
}

func TestUnreadableBundleAndNoBundles(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "manifest.json", manifestJSON)
	write(t, dir, "locales/es-MX.json", `{}`)
	rep, err := CheckDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Problems) != 1 || !strings.Contains(rep.Problems[0], "en-US") {
		t.Fatalf("problems = %v", rep.Problems)
	}
	dir2 := t.TempDir()
	write(t, dir2, "manifest.json", strings.Replace(manifestJSON, `"i18n"`, `"_x"`, 1))
	if _, err := CheckDir(dir2); err != nil {
		// _x is not a manifest field; lenient decode must still accept it.
		t.Fatal(err)
	}
}

func TestForeignPrefixKeyIsChecked(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "manifest.json", `{"apiVersion":"asteby.com/v3","kind":"Addon",
	  "metadata":{"key":"tires_inventory","name":"x","version":"0.1.0"},
	  "settings":[{"key":"a","type":"string","label":"tires.settings.a"},{"key":"b","type":"string","label":"Texto libre"}]}`)
	write(t, dir, "locales/es.json", `{"tires_inventory":{"settings":{"a":"A"}}}`)
	rep, err := CheckDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Missing) != 1 || rep.Missing[0].Key != "tires.settings.a" {
		t.Fatalf("missing = %v", rep.Missing)
	}
	if len(rep.Literals) != 1 || rep.Literals[0].Key != "Texto libre" {
		t.Fatalf("literals = %v", rep.Literals)
	}
}
