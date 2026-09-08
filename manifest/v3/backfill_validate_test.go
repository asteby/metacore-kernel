package v3

import "testing"

// backfillManifest wraps a backfills block in the minimum valid addon manifest
// so Parse exercises the JSON schema (additionalProperties:false at the top
// level would reject "backfills" outright if the schema entry were missing)
// and then validatePipelineRuntime.
func backfillManifest(backfills string) []byte {
	return []byte(`{
      "apiVersion": "asteby.com/v3",
      "kind": "Addon",
      "metadata": {"key": "receivables", "name": "Receivables", "version": "1.0.0"},
      "compatibility": {"requires": [{"key": "kernel", "version": ">=3.0.0 <4.0.0"}]},
      "backfills": ` + backfills + `
    }`)
}

func TestBackfillParsesAndProjects(t *testing.T) {
	raw := backfillManifest(`[
      {
        "key": "customer_statements",
        "on": ["install", "upgrade"],
        "source": {"table": "invoices", "distinct": "customer_id"},
        "do": "wasm:backfill_party",
        "arg": "party_id",
        "with": {"party_type": "customer"}
      }
    ]`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(m.Backfills) != 1 {
		t.Fatalf("want 1 backfill, got %d", len(m.Backfills))
	}
	b := m.Backfills[0]
	if b.Source.Table != "invoices" || b.Source.Distinct != "customer_id" {
		t.Fatalf("source not parsed: %+v", b.Source)
	}
	if b.Arg != "party_id" || b.With["party_type"] != "customer" {
		t.Fatalf("arg/with not parsed: %+v", b)
	}
}

// The default is BOTH transitions: an addon whose sweep only ran at install
// would leave every already-installed org stale forever, which is the failure
// the primitive exists to fix.
func TestBackfillOmittedOnMeansInstallAndUpgrade(t *testing.T) {
	raw := backfillManifest(`[
      {"key": "sweep", "source": {"table": "invoices", "distinct": "customer_id"}, "do": "wasm:sweep"}
    ]`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(m.Backfills[0].On) != 0 {
		t.Fatalf("want empty On (the both-transitions default), got %v", m.Backfills[0].On)
	}
}

func TestBackfillRejectsBadDeclarations(t *testing.T) {
	cases := []struct {
		name      string
		backfills string
	}{
		{"duplicate key", `[
          {"key": "s", "source": {"table": "a", "distinct": "c"}, "do": "wasm:x"},
          {"key": "s", "source": {"table": "b", "distinct": "c"}, "do": "wasm:y"}
        ]`},
		{"unknown do prefix", `[
          {"key": "s", "source": {"table": "a", "distinct": "c"}, "do": "lambda:x"}
        ]`},
		{"unknown on value", `[
          {"key": "s", "on": ["reinstall"], "source": {"table": "a", "distinct": "c"}, "do": "wasm:x"}
        ]`},
		// `with` shadowing the value field would silently win or lose
		// depending on merge order, so it is rejected rather than resolved.
		{"with shadows arg", `[
          {"key": "s", "source": {"table": "a", "distinct": "c"}, "do": "wasm:x",
           "arg": "party_id", "with": {"party_id": "nope"}}
        ]`},
		{"with shadows default arg", `[
          {"key": "s", "source": {"table": "a", "distinct": "c"}, "do": "wasm:x",
           "with": {"id": "nope"}}
        ]`},
		{"with shadows reserved backfill key", `[
          {"key": "s", "source": {"table": "a", "distinct": "c"}, "do": "wasm:x",
           "with": {"backfill": "nope"}}
        ]`},
		{"missing source column", `[
          {"key": "s", "source": {"table": "a"}, "do": "wasm:x"}
        ]`},
		{"unknown field", `[
          {"key": "s", "source": {"table": "a", "distinct": "c"}, "do": "wasm:x", "batch": 10}
        ]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse(backfillManifest(tc.backfills)); err == nil {
				t.Fatal("expected rejection, got none")
			}
		})
	}
}
