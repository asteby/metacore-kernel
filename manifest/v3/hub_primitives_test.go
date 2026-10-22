package v3

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// hubPrimitivesJSON is a fully valid manifest that exercises every field added
// by the hub-primitives contract (RFC-01/02/04/06/07/08). It is modeled on the
// hub_fleet spike (asteby-hq/addons spike/hub-fleet).
const hubPrimitivesJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "hub_fleet", "name": "Hub fleet", "version": "0.2.0", "visibility": "private" },
  "compatibility": { "requires": [{ "key": "kernel", "version": ">=3.1.0 <4.0.0" }] },
  "tenancy": { "isolation": "shared", "rls_column": "organization_id" },
  "capabilities": [
    { "kind": "db:write", "target": "addon_hub_fleet.*" },
    { "kind": "http:fetch", "target": "connector:trust_core.base_url", "reason": "Pedir al nucleo la revocacion." },
    { "kind": "http:fetch", "target": "core.example.com", "reason": "Destino configurable." }
  ],
  "connectors": [
    {
      "key": "trust_core", "label": "hub_fleet.connector.trust_core", "auth": "token",
      "credentials": [
        { "key": "base_url", "type": "string", "required": true },
        { "key": "signing_secret", "type": "secret", "required": true }
      ]
    },
    {
      "key": "fleet_feed", "label": "hub_fleet.connector.fleet_feed", "auth": "token",
      "credentials": [ { "key": "signing_secret", "type": "secret", "required": true } ]
    }
  ],
  "settings": [ { "key": "core_url", "type": "string", "label": "hub_fleet.setting.core_url" } ],
  "models": [
    {
      "key": "Instance",
      "table": "instances",
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true },
        { "name": "organization_id", "type": "uuid", "not_null": true },
        { "name": "instance_id", "type": "text", "not_null": true },
        { "name": "name", "type": "text" },
        { "name": "environment", "type": "text" },
        { "name": "source_seq", "type": "bigint" },
        { "name": "license_status", "type": "text", "protected": true },
        { "name": "last_seen_at", "type": "timestamptz" },
        { "name": "status", "type": "text", "protected": true },
        { "name": "retired_at", "type": "timestamptz", "protected": true },
        { "name": "retired_by_id", "type": "uuid", "protected": true },
        { "name": "retired_reason", "type": "text", "protected": true },
        { "name": "notes", "type": "text" },
        { "name": "created_at", "type": "timestamptz" },
        { "name": "updated_at", "type": "timestamptz" },
        { "name": "deleted_at", "type": "timestamptz" }
      ],
      "indices": [
        { "name": "hub_fleet_instances_org_instance_uq", "columns": ["organization_id", "instance_id"], "unique": true, "where": "deleted_at IS NULL" }
      ],
      "reason_required": { "actions": ["retire"], "min_length": 3 },
      "ingest": {
        "key": ["instance_id"],
        "cursor": "source_seq",
        "fields": ["name", "environment", "license_status", "last_seen_at", "source_seq"],
        "exclusive": true,
        "on_delete": "soft",
        "max_batch": 500
      }
    }
  ],
  "webhooks": [
    { "key": "fleet_changes", "path": "/fleet/changes", "verify": "metacore-hmac-v1",
      "secret_ref": "fleet_feed.signing_secret", "do": "upsert:Instance" }
  ],
  "contributions": {
    "actions": [
      {
        "key": "retire", "label": "hub_fleet.action.retire", "icon": "Archive",
        "target_model": "Instance", "placement": "row", "tone": "danger",
        "requires_state": ["active"],
        "confirm": true, "confirm_message": "hub_fleet.action.retire_impact",
        "confirm_typed": { "field": "name" },
        "fields": [ { "key": "reason", "type": "string", "widget": "textarea", "required": true } ],
        "handler": {
          "type": "set_fields",
          "set": {
            "status": "retired", "retired_at": "$now", "retired_by_id": "$actor.id",
            "retired_reason": "$payload.reason", "notes": "$$literal"
          }
        }
      },
      {
        "key": "request_revoke", "label": "hub_fleet.action.request_revoke",
        "target_model": "Instance", "placement": "row",
        "fields": [ { "key": "reason", "type": "string", "widget": "textarea", "required": true } ],
        "handler": {
          "type": "webhook",
          "url_ref": "trust_core.base_url",
          "path": "/core/v1/fleet/instances/{record.instance_id}/revoke-request",
          "method": "POST",
          "signing": { "scheme": "metacore-hmac-v1", "secret_ref": "trust_core.signing_secret" },
          "send_record": ["instance_id", "license_status"],
          "timeout_ms": 10000,
          "retries": 0,
          "on_success": { "set": { "license_status": "revoke_requested" } }
        }
      },
      {
        "key": "ping_core", "label": "hub_fleet.action.ping_core",
        "target_model": "Instance", "placement": "row",
        "handler": {
          "type": "webhook",
          "url_setting": "core_url",
          "path": "/ping/{payload.region}",
          "signing": { "scheme": "instance-ed25519-v1" },
          "fields_ignored": null
        },
        "fields": [ { "key": "region", "type": "string" } ]
      }
    ],
    "dashboard": [
      {
        "key": "instances_online", "title": "hub_fleet.kpi.online", "kind": "stat",
        "query": { "model": "Instance", "aggregate": "count",
                   "where": { "environment": "production", "last_seen_at": { "gte": "$now-15m" } } },
        "link": { "model": "Instance", "filter": { "environment": "production" } }
      },
      {
        "key": "instances_active_24h", "title": "hub_fleet.kpi.active_24h", "kind": "stat",
        "query": { "model": "Instance", "aggregate": "count", "date_field": "last_seen_at",
                   "range": "last_24_hours", "where": { "environment": "production" } },
        "compare": { "to": "previous_period" },
        "link": { "url": "/m/instances?f_environment=production" }
      }
    ]
  },
  "rbac": {
    "permissions": [
      { "key": "hub_fleet.instance.read", "models": [{ "model": "Instance", "actions": ["index", "show"] }] }
    ],
    "roles": [
      { "key": "hub_fleet_viewer", "label": "hub_fleet.role.viewer", "permissions": ["hub_fleet.instance.read", "hub_fleet.instance.retire"] }
    ]
  }
}`

// hubMutate decodes hubPrimitivesJSON, applies fn and returns the JSON.
func hubMutate(t *testing.T, fn func(m map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(hubPrimitivesJSON), &m); err != nil {
		t.Fatalf("base: %v", err)
	}
	// The "fields_ignored" key in the base's ping_core is a deliberate typo
	// guard: remove it so the base is valid, tests re-add unknown keys on purpose.
	acts := m["contributions"].(map[string]any)["actions"].([]any)
	delete(acts[2].(map[string]any)["handler"].(map[string]any), "fields_ignored")
	if fn != nil {
		fn(m)
	}
	return mustJSON(t, m)
}

func action(m map[string]any, i int) map[string]any {
	return m["contributions"].(map[string]any)["actions"].([]any)[i].(map[string]any)
}
func handler(m map[string]any, i int) map[string]any {
	return action(m, i)["handler"].(map[string]any)
}
func model0(m map[string]any) map[string]any { return m["models"].([]any)[0].(map[string]any) }
func widget(m map[string]any, i int) map[string]any {
	return m["contributions"].(map[string]any)["dashboard"].([]any)[i].(map[string]any)
}

func TestHubPrimitives_BaseIsValidStrict(t *testing.T) {
	raw := hubMutate(t, nil)
	warns, err := ValidateWithOptions(raw, Options{Strict: true})
	if err != nil {
		t.Fatalf("base must validate strict: %v", err)
	}
	// hub_fleet.instance.retire is not declared but resolves by action key.
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}
	m, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	h := m.Contributions.Actions[1].Handler
	if h.Retries == nil || *h.Retries != 0 {
		t.Fatalf("explicit retries:0 must survive (pointer), got %v", h.Retries)
	}
	if m.Models[0].Ingest == nil || !m.Models[0].Ingest.Exclusive || m.Models[0].Ingest.Cursor != "source_seq" {
		t.Fatalf("ingest not decoded: %+v", m.Models[0].Ingest)
	}
	if m.Contributions.Dashboard[0].Link == nil || m.Contributions.Dashboard[0].Link.Model != "Instance" {
		t.Fatalf("widget link not decoded")
	}
}

// TestHubPrimitives_SpikeFixtureValidatesStrict: the hub_fleet spike manifest
// with the new fields applied (testdata/hub_fleet_v230.json) validates strict.
func TestHubPrimitives_SpikeFixtureValidatesStrict(t *testing.T) {
	raw, err := os.ReadFile("testdata/hub_fleet_v230.json")
	if err != nil {
		t.Fatal(err)
	}
	warns, err := ValidateWithOptions(raw, Options{Strict: true})
	if err != nil {
		t.Fatalf("spike + new fields must validate strict: %v", err)
	}
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}
}

func TestHubPrimitives_Rejections(t *testing.T) {
	cases := []struct {
		name string
		fn   func(m map[string]any)
		want string
	}{
		// ---- RFC-01 webhook ----
		{"two url sources", func(m map[string]any) { handler(m, 1)["url"] = "https://x.example.com" }, "mutually exclusive"},
		{"url_ref undeclared credential", func(m map[string]any) { handler(m, 1)["url_ref"] = "trust_core.nope" }, "undeclared connector credential"},
		{"url_ref to a secret", func(m map[string]any) { handler(m, 1)["url_ref"] = "trust_core.signing_secret" }, `of type "string"`},
		{"url_ref without http:fetch", func(m map[string]any) {
			m["capabilities"] = []any{map[string]any{"kind": "db:write", "target": "addon_hub_fleet.*"}, map[string]any{"kind": "http:fetch", "target": "core.example.com"}}
		}, "requires a capability"},
		{"url_setting unknown", func(m map[string]any) { handler(m, 2)["url_setting"] = "nope" }, "not a key of settings[]"},
		{"url_setting without fixed host", func(m map[string]any) {
			m["capabilities"] = []any{map[string]any{"kind": "db:write", "target": "addon_hub_fleet.*"}, map[string]any{"kind": "http:fetch", "target": "connector:trust_core.base_url"}}
		}, "fixed host"},
		{"path with literal url", func(m map[string]any) {
			h := handler(m, 1)
			delete(h, "url_ref")
			h["url"] = "https://x.example.com"
		}, "literal url is already complete"},
		{"path without base", func(m map[string]any) { delete(handler(m, 1), "url_ref") }, "needs url_ref or url_setting"},
		{"path unknown record column", func(m map[string]any) { handler(m, 1)["path"] = "/x/{record.nope}" }, `"nope" is not a column`},
		{"path undeclared payload field", func(m map[string]any) { handler(m, 2)["path"] = "/x/{payload.zzz}" }, `"zzz" is not a field`},
		{"path bad placeholder", func(m map[string]any) { handler(m, 1)["path"] = "/x/{actor.id}" }, "must be {record.<column>} or {payload.<field>}"},
		{"path with query", func(m map[string]any) { handler(m, 1)["path"] = "/x?a=1" }, "no query"},
		{"bad method", func(m map[string]any) { handler(m, 1)["method"] = "GET" }, "method"},
		{"hmac without secret_ref", func(m map[string]any) {
			handler(m, 1)["signing"] = map[string]any{"scheme": "metacore-hmac-v1"}
		}, "secret_ref is required"},
		{"hmac secret_ref not a secret", func(m map[string]any) {
			handler(m, 1)["signing"] = map[string]any{"scheme": "metacore-hmac-v1", "secret_ref": "trust_core.base_url"}
		}, `of type "secret"`},
		{"ed25519 with secret_ref", func(m map[string]any) {
			handler(m, 2)["signing"] = map[string]any{"scheme": "instance-ed25519-v1", "secret_ref": "trust_core.signing_secret"}
		}, "not allowed with instance-ed25519-v1"},
		{"unknown scheme", func(m map[string]any) { handler(m, 1)["signing"] = map[string]any{"scheme": "plain"} }, "signing"},
		{"send_record unknown column", func(m map[string]any) { handler(m, 1)["send_record"] = []any{"nope"} }, `"nope" is not a column`},
		{"timeout out of range", func(m map[string]any) { handler(m, 1)["timeout_ms"] = 50 }, "timeout_ms"},
		{"retries out of range", func(m map[string]any) { handler(m, 1)["retries"] = 9 }, "retries"},
		{"on_success.set bad token", func(m map[string]any) {
			handler(m, 1)["on_success"] = map[string]any{"set": map[string]any{"license_status": "$bogus"}}
		}, "unknown token"},
		{"webhook fields on a wasm handler", func(m map[string]any) {
			handler(m, 1)["type"] = "wasm"
			handler(m, 1)["function"] = "Revoke"
		}, "only allowed when type=webhook"},
		{"webhook fields on a tool", func(m map[string]any) {
			m["contributions"].(map[string]any)["tools"] = []any{map[string]any{
				"key": "t", "description": "d", "input_schema": map[string]any{"type": "object"},
				"handler": map[string]any{"type": "webhook", "url_ref": "trust_core.base_url"},
			}}
		}, "tools"},

		// ---- RFC-02 set_fields ----
		{"set unknown column", func(m map[string]any) { handler(m, 0)["set"] = map[string]any{"nope": 1} }, `"nope" is not a column`},
		{"set managed column", func(m map[string]any) { handler(m, 0)["set"] = map[string]any{"organization_id": "x"} }, "managed by the kernel"},
		{"set audit column", func(m map[string]any) { handler(m, 0)["set"] = map[string]any{"deleted_at": "$now"} }, "managed by the kernel"},
		{"set unknown token", func(m map[string]any) { handler(m, 0)["set"] = map[string]any{"status": "$yesterday"} }, "unknown token"},
		{"set undeclared payload", func(m map[string]any) { handler(m, 0)["set"] = map[string]any{"status": "$payload.zzz"} }, "$payload.zzz"},
		{"set unknown record col", func(m map[string]any) { handler(m, 0)["set"] = map[string]any{"status": "$record.zzz"} }, "$record.zzz"},
		{"set object value", func(m map[string]any) { handler(m, 0)["set"] = map[string]any{"status": map[string]any{"a": 1}} }, "scalar"},
		{"set empty", func(m map[string]any) { handler(m, 0)["set"] = map[string]any{} }, "set"},
		{"set_fields on create placement", func(m map[string]any) { action(m, 0)["placement"] = "create" }, "only works as a row action"},
		{"set_fields without target", func(m map[string]any) { delete(action(m, 0), "target_model") }, "needs the action's target_model"},
		{"set_fields on append_only", func(m map[string]any) { model0(m)["append_only"] = true }, "append_only"},
		{"set on a wasm handler", func(m map[string]any) {
			handler(m, 0)["type"] = "wasm"
			handler(m, 0)["function"] = "F"
		}, "only allowed when type=set_fields"},
		{"confirm_typed unknown field", func(m map[string]any) { action(m, 0)["confirm_typed"] = map[string]any{"field": "nope"} }, `"nope" is not a column`},
		{"confirm_typed without row target", func(m map[string]any) { action(m, 0)["placement"] = "table" }, "row action"},
		{"bad tone", func(m map[string]any) { action(m, 0)["tone"] = "loud" }, "tone"},

		// ---- RFC-04 ingest ----
		{"ingest key without unique index", func(m map[string]any) { model0(m)["indices"] = []any{} }, "UNIQUE index"},
		{"ingest index missing org", func(m map[string]any) {
			model0(m)["indices"] = []any{map[string]any{"name": "u", "columns": []any{"instance_id"}, "unique": true}}
		}, "UNIQUE index"},
		{"ingest index with other predicate", func(m map[string]any) {
			model0(m)["indices"] = []any{map[string]any{"name": "u", "columns": []any{"organization_id", "instance_id"}, "unique": true, "where": "name IS NOT NULL"}}
		}, "UNIQUE index"},
		{"ingest unknown key col", func(m map[string]any) { model0(m)["ingest"].(map[string]any)["key"] = []any{"nope"} }, `"nope" is not a column`},
		{"ingest too many keys", func(m map[string]any) {
			model0(m)["ingest"].(map[string]any)["key"] = []any{"a", "b", "c", "d", "e"}
		}, "key"},
		{"ingest text cursor", func(m map[string]any) { model0(m)["ingest"].(map[string]any)["cursor"] = "name" }, "bigint or timestamp"},
		{"ingest managed field", func(m map[string]any) {
			model0(m)["ingest"].(map[string]any)["fields"] = []any{"created_at"}
		}, "managed by the kernel"},
		{"ingest field id", func(m map[string]any) { model0(m)["ingest"].(map[string]any)["fields"] = []any{"id"} }, "managed by the kernel"},
		{"ingest bad on_delete", func(m map[string]any) { model0(m)["ingest"].(map[string]any)["on_delete"] = "hard" }, "on_delete"},
		{"ingest max_batch too big", func(m map[string]any) { model0(m)["ingest"].(map[string]any)["max_batch"] = 5000 }, "max_batch"},
		{"ingest on append_only", func(m map[string]any) { model0(m)["append_only"] = true }, "append_only"},
		{"upsert to foreign model", func(m map[string]any) { m["webhooks"].([]any)[0].(map[string]any)["do"] = "upsert:Other" }, "not a model of this addon"},
		{"upsert to model without ingest", func(m map[string]any) { delete(model0(m), "ingest") }, "no ingest block"},
		{"upsert without verify", func(m map[string]any) {
			w := m["webhooks"].([]any)[0].(map[string]any)
			delete(w, "verify")
			delete(w, "secret_ref")
		}, "needs verify"},
		{"upsert with verify addon", func(m map[string]any) { m["webhooks"].([]any)[0].(map[string]any)["verify"] = "addon" }, "needs verify"},
		{"upsert prefix in a schedule", func(m map[string]any) {
			m["schedules"] = []any{map[string]any{"key": "s", "every": "1h", "do": "upsert:Instance"}}
		}, "does not match pattern"},

		// ---- RFC-08 dashboard ----
		{"now on equality", func(m map[string]any) {
			widget(m, 0)["query"].(map[string]any)["where"] = map[string]any{"last_seen_at": "$now-15m"}
		}, "only works with an operator"},
		{"now with eq", func(m map[string]any) {
			widget(m, 0)["query"].(map[string]any)["where"] = map[string]any{"last_seen_at": map[string]any{"eq": "$now"}}
		}, "gt|gte|lt|lte"},
		{"now on text column", func(m map[string]any) {
			widget(m, 0)["query"].(map[string]any)["where"] = map[string]any{"name": map[string]any{"gte": "$now-1h"}}
		}, "date/timestamp"},
		{"now bad duration", func(m map[string]any) {
			widget(m, 0)["query"].(map[string]any)["where"] = map[string]any{"last_seen_at": map[string]any{"gte": "$now-15x"}}
		}, "$now or $now-<n>"},
		{"now too far", func(m map[string]any) {
			widget(m, 0)["query"].(map[string]any)["where"] = map[string]any{"last_seen_at": map[string]any{"gte": "$now-401d"}}
		}, "400d"},
		{"bad range", func(m map[string]any) { widget(m, 1)["query"].(map[string]any)["range"] = "last_5_minutes" }, "range"},
		{"link to foreign model", func(m map[string]any) { widget(m, 0)["link"] = map[string]any{"model": "Other"} }, "not a model of this addon"},
		{"link filter unknown column", func(m map[string]any) {
			widget(m, 0)["link"] = map[string]any{"model": "Instance", "filter": map[string]any{"zzz": "x"}}
		}, "not a column"},
		{"link absolute url", func(m map[string]any) { widget(m, 1)["link"] = map[string]any{"url": "https://evil.example.com/x"} }, "link"},
		{"link protocol-relative url", func(m map[string]any) { widget(m, 1)["link"] = map[string]any{"url": "//evil.example.com/x"} }, "link"},
		{"link backslash url", func(m map[string]any) { widget(m, 1)["link"] = map[string]any{"url": "/\\evil.example.com"} }, "link"},
		{"link model and url", func(m map[string]any) { widget(m, 1)["link"] = map[string]any{"model": "Instance", "url": "/m/x"} }, "mutually exclusive"},
		{"link empty", func(m map[string]any) { widget(m, 1)["link"] = map[string]any{} }, "needs model"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Validate(hubMutate(t, c.fn))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to contain %q", err, c.want)
			}
		})
	}
}

// TestHubPrimitives_LiteralDollarEscape: "$$x" is the literal "$x".
func TestHubPrimitives_ParseSetValue(t *testing.T) {
	ok := []struct {
		in   any
		kind SetValueKind
		name string
	}{
		{nil, SetNull, ""}, {"x", SetLiteral, ""}, {float64(3), SetLiteral, ""}, {true, SetLiteral, ""},
		{"$now", SetNow, ""}, {"$actor.id", SetActorID, ""},
		{"$payload.reason", SetPayload, "reason"}, {"$record.status", SetRecord, "status"},
	}
	for _, c := range ok {
		got, err := ParseSetValue(c.in)
		if err != nil || got.Kind != c.kind || got.Name != c.name {
			t.Errorf("ParseSetValue(%v) = %+v, %v; want kind %s name %q", c.in, got, err, c.kind, c.name)
		}
	}
	if v, err := ParseSetValue("$$5.00"); err != nil || v.Literal != "$5.00" {
		t.Errorf("escape: %+v %v", v, err)
	}
	for _, bad := range []any{"$", "$now-1h", "$actor.email", "$payload.", "$payload.a-b", "$record.", []any{1}, map[string]any{"a": 1}} {
		if _, err := ParseSetValue(bad); err == nil {
			t.Errorf("ParseSetValue(%v) should fail", bad)
		}
	}
}

func TestHubPrimitives_NowTokenOffset(t *testing.T) {
	for in, want := range map[string]int{"$now": 0, "$now-15m": 15, "$now-2h": 120, "$now-1d": 1440, "$now-400d": 400 * 1440} {
		got, ok := NowTokenOffset(in)
		if !ok || got != want {
			t.Errorf("%s = %d,%v want %d", in, got, ok, want)
		}
	}
	for _, bad := range []string{"$now-", "$now-0m", "$now-1w", "$now+5m", "$now-401d", "now-5m", "$now-9999999999999999999m"} {
		if _, ok := NowTokenOffset(bad); ok {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

// ---- tolerated rules ----

// hubWarnings validates in BOTH modes: the tolerated hub rules are warning-only
// during their window, so neither mode may fail, and both must report them.
func hubWarnings(t *testing.T, fn func(m map[string]any)) []string {
	t.Helper()
	var first []string
	for _, strict := range []bool{false, true} {
		warns, err := ValidateWithOptions(hubMutate(t, fn), Options{Strict: strict})
		if err != nil {
			t.Fatalf("strict=%v: a tolerated rule must not be an error during its window: %v", strict, err)
		}
		if len(warns) == 0 {
			t.Fatalf("strict=%v: expected a warning", strict)
		}
		if first == nil {
			first = warns
		}
	}
	return first
}

func TestHubPrimitives_ToleratedRules(t *testing.T) {
	cases := []struct {
		name string
		fn   func(m map[string]any)
		rule string
	}{
		{"webhook without signing", func(m map[string]any) { delete(handler(m, 1), "signing") }, RuleWebhookActionSigning},
		{"webhook without destination", func(m map[string]any) {
			h := handler(m, 1)
			delete(h, "url_ref")
			delete(h, "path")
		}, RuleWebhookActionSigning},
		{"webhook with http literal", func(m map[string]any) {
			h := handler(m, 1)
			delete(h, "url_ref")
			delete(h, "path")
			h["url"] = "http://127.0.0.1:18099/x"
		}, RuleWebhookActionSigning},
		{"role key with a typo", func(m map[string]any) {
			m["rbac"].(map[string]any)["roles"].([]any)[0].(map[string]any)["permissions"] = []any{"hub_fleet.instanse.read"}
		}, RuleRBACPermissionResolves},
		{"role key with a non-CRUD verb", func(m map[string]any) {
			m["rbac"].(map[string]any)["roles"].([]any)[0].(map[string]any)["permissions"] = []any{"hub_fleet.instance.explode"}
		}, RuleRBACPermissionResolves},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			warns := hubWarnings(t, c.fn)
			found := false
			for _, w := range warns {
				if strings.HasPrefix(w, "["+c.rule+"]") {
					found = true
				}
			}
			if !found {
				t.Fatalf("want a [%s] warning, got %v", c.rule, warns)
			}
		})
	}
	// Both rules are inside their window.
	have := map[string]bool{}
	for _, r := range LegacyTolerated() {
		have[r] = true
	}
	if !have[RuleWebhookActionSigning] || !have[RuleRBACPermissionResolves] {
		t.Fatalf("rules not registered in compat.go: %v", LegacyTolerated())
	}
}

func TestHubPrimitives_RBACResolution_OK(t *testing.T) {
	// Keys of another addon, canonical forms and declared keys never warn.
	raw := hubMutate(t, func(m map[string]any) {
		m["rbac"].(map[string]any)["roles"].([]any)[0].(map[string]any)["permissions"] = []any{
			"hub_fleet.instance.read", "hub_fleet.instance.write", "hub_fleet.instance.retire",
			"instances.index", "screen.dashboard.access", "general.approve_refund", "pos.session.read",
		}
	})
	warns, err := ValidateWithOptions(raw, Options{Strict: true})
	if err != nil || len(warns) != 0 {
		t.Fatalf("warns=%v err=%v", warns, err)
	}
}

// ---- branding.login (RFC-07) ----

func TestBrandingLogin_ParseAndValidate(t *testing.T) {
	login := map[string]any{
		"eyebrow": "Plano de control", "title": "La flota de Asteby,", "title_accent": "en un solo lugar.",
		"subtitle":   "Instancias, licencias, catalogo y actualizaciones.",
		"highlights": []any{map[string]any{"icon": "Server", "label": "Flota"}, map[string]any{"icon": "KeyRound", "label": "Licencias"}},
		"stats":      []any{},
		"signup":     false,
		"i18n":       map[string]any{"en": map[string]any{"eyebrow": "Control plane", "title": "Asteby's fleet,", "title_accent": "in one place."}},
	}
	b, errs := ParseBranding(map[string]any{"platform_name": "Asteby Hub", "login": login})
	if len(errs) != 0 {
		t.Fatalf("parse: %v", errs)
	}
	if b.Login == nil || b.Login.Signup == nil || *b.Login.Signup || len(b.Login.Highlights) != 2 || b.Login.I18n["en"].Eyebrow != "Control plane" {
		t.Fatalf("login not decoded: %+v", b.Login)
	}
	if errs := b.Validate(); len(errs) != 0 {
		t.Fatalf("validate: %v", errs)
	}
	if got := b.ToMap()["login"]; got == nil {
		t.Fatal("ToMap drops login")
	}

	// null removes (merge-patch): parses to a nil block.
	if b, errs := ParseBranding(map[string]any{"login": nil}); len(errs) != 0 || b.Login != nil {
		t.Fatalf("null login: %v %+v", errs, b.Login)
	}
	// Unknown key inside login is rejected.
	if _, errs := ParseBranding(map[string]any{"login": map[string]any{"title": "x", "bogus": 1}}); len(errs) == 0 {
		t.Fatal("unknown login key must fail")
	}
}

func TestBrandingLogin_Rejections(t *testing.T) {
	long := strings.Repeat("a", 81)
	hl := func(n int) []LoginHighlight {
		out := make([]LoginHighlight, n)
		for i := range out {
			out[i] = LoginHighlight{Icon: "Server", Label: "x"}
		}
		return out
	}
	cases := []struct {
		name string
		l    LoginCopy
		want string
	}{
		{"html in title", LoginCopy{Title: "<b>hi</b>"}, "plain text"},
		{"long eyebrow", LoginCopy{Eyebrow: long}, "max 80"},
		{"long subtitle", LoginCopy{Subtitle: strings.Repeat("a", 201)}, "max 200"},
		{"9 highlights", LoginCopy{Highlights: hl(9)}, "max 8"},
		{"bad icon", LoginCopy{Highlights: []LoginHighlight{{Icon: "server", Label: "x"}}}, "Lucide PascalCase"},
		{"long label", LoginCopy{Highlights: []LoginHighlight{{Icon: "Server", Label: strings.Repeat("a", 25)}}}, "max 24"},
		{"empty label", LoginCopy{Highlights: []LoginHighlight{{Icon: "Server"}}}, "label is required"},
		{"4 stats", LoginCopy{Stats: []LoginStat{{"a", "1"}, {"b", "2"}, {"c", "3"}, {"d", "4"}}}, "max 3"},
		{"stat without value", LoginCopy{Stats: []LoginStat{{Label: "a"}}}, "label and a value"},
		{"bad locale", LoginCopy{I18n: map[string]LoginCopyLocale{"EN_us": {Title: "x"}}}, "locale key"},
		{"html in i18n", LoginCopy{I18n: map[string]LoginCopyLocale{"en": {Title: "<i>x</i>"}}}, "plain text"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			errs := (Branding{Login: &c.l}).Validate()
			if len(errs) == 0 || !strings.Contains(strings.Join(errs, "\n"), c.want) {
				t.Fatalf("errs = %v, want %q", errs, c.want)
			}
		})
	}
}

// presetWithBranding builds a minimal valid Preset manifest carrying branding.
func presetWithBranding(t *testing.T, branding any) []byte {
	t.Helper()
	m := map[string]any{
		"apiVersion": "asteby.com/v3", "kind": "Preset",
		"metadata":      map[string]any{"key": "hub", "name": "Hub", "version": "1.0.0"},
		"compatibility": map[string]any{"requires": []any{map[string]any{"key": "kernel", "version": ">=3.1.0 <4.0.0"}}},
		"preset": map[string]any{
			"addons":   []any{map[string]any{"key": "hub_fleet", "version": ">=0.1.0"}},
			"defaults": map[string]any{"branding": branding},
		},
	}
	return mustJSON(t, m)
}

func TestPresetBrandingSchema_Login(t *testing.T) {
	good := map[string]any{"platform_name": "Hub", "login": map[string]any{
		"title": "x", "highlights": []any{map[string]any{"icon": "Server", "label": "Flota"}}, "signup": false,
		"i18n": map[string]any{"en": map[string]any{"title": "y"}},
	}}
	if err := Validate(presetWithBranding(t, good)); err != nil {
		t.Fatalf("preset with login: %v", err)
	}
	if err := Validate(presetWithBranding(t, map[string]any{"login": nil})); err != nil {
		t.Fatalf("null login (merge-patch removal): %v", err)
	}
	for name, bad := range map[string]any{
		"html":          map[string]any{"title": "<b>x</b>"},
		"unknown key":   map[string]any{"title": "x", "bogus": 1},
		"9 highlights":  map[string]any{"highlights": []any{1, 2, 3, 4, 5, 6, 7, 8, 9}},
		"bad icon":      map[string]any{"highlights": []any{map[string]any{"icon": "server", "label": "x"}}},
		"4 stats":       map[string]any{"stats": []any{map[string]any{"label": "a", "value": "1"}, map[string]any{"label": "a", "value": "1"}, map[string]any{"label": "a", "value": "1"}, map[string]any{"label": "a", "value": "1"}}},
		"long title":    map[string]any{"title": strings.Repeat("a", 81)},
		"bad i18n key":  map[string]any{"i18n": map[string]any{"EN_us": map[string]any{"title": "x"}}},
		"i18n unknown":  map[string]any{"i18n": map[string]any{"en": map[string]any{"bogus": "x"}}},
		"signup string": map[string]any{"signup": "no"},
	} {
		if err := Validate(presetWithBranding(t, map[string]any{"login": bad})); err == nil {
			t.Errorf("%s: schema accepted an invalid login block", name)
		}
	}
}
