package dyntest

import (
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// recordConditionManifestJSON declares a row action gated on a record field
// (credit_pending) — the shape customers ships for reject_credit / authorize_*.
// Field/Operator/Value must survive Parse → FromV3 so the host metadata
// payload can drive isActionConditionMet without ops hardcoding each key.
const recordConditionManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "customers", "name": "Customers", "version": "1.0.0" },
  "compatibility": { "requires": [ { "key": "kernel", "version": ">=0.1.0" } ] },
  "models": [
    {
      "key": "SalesOrder",
      "table": "sales_orders",
      "label": "Sales orders",
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true },
        { "name": "organization_id", "type": "uuid", "not_null": true },
        { "name": "credit_pending", "type": "boolean" }
      ]
    }
  ],
  "contributions": {
    "actions": [
      {
        "key": "reject_credit",
        "label": "Reject credit",
        "target_model": "SalesOrder",
        "handler": { "type": "webhook" },
        "condition": {
          "field": "credit_pending",
          "operator": "eq",
          "value": true
        }
      }
    ]
  }
}`

func TestRecordConditionParseAndProject(t *testing.T) {
	m, err := v3.Parse([]byte(recordConditionManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse rejected record-level condition: %v", err)
	}
	cond := m.Contributions.Actions[0].Condition
	if cond == nil || cond.Field != "credit_pending" || cond.Operator != "eq" {
		t.Fatalf("v3 action condition not parsed: %+v", cond)
	}
	if got, ok := cond.Value.(bool); !ok || !got {
		t.Fatalf("v3 action condition value = %#v, want true", cond.Value)
	}

	host := manifest.FromV3(m)
	actions := host.Actions["SalesOrder"]
	if len(actions) != 1 || actions[0].Condition == nil {
		t.Fatalf("host action missing condition: %+v", actions)
	}
	hc := actions[0].Condition
	if hc.Field != "credit_pending" || hc.Operator != "eq" {
		t.Fatalf("host dropped record condition: %+v", hc)
	}
	if got, ok := hc.Value.(bool); !ok || !got {
		t.Fatalf("host condition value = %#v, want true", hc.Value)
	}
	// Org-level SatisfiedBy must ignore Field — a record-only condition is
	// always "served"; the SDK evaluates the row gate client-side.
	if !hc.SatisfiedBy(nil, nil) {
		t.Fatalf("record-only condition must SatisfiedBy as met for org gate")
	}
}
