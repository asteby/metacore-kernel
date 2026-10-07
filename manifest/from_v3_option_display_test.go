package manifest

import (
	"testing"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// option_display rides the model definition verbatim; option_metrics resolve
// the aggregated model and every through model to their tables.
func TestFromV3ProjectsOptionDisplayAndMetrics(t *testing.T) {
	raw := []byte(`{
      "apiVersion": "asteby.com/v3",
      "kind": "Addon",
      "metadata": {"key": "inventory", "name": "Inventory", "version": "1.0.0"},
      "compatibility": {"requires": [{"key": "kernel", "version": ">=3.0.0 <4.0.0"}]},
      "models": [
        {"key": "Warehouse", "table": "warehouses", "columns": [
          {"name": "organization_id", "type": "uuid"}, {"name": "name", "type": "text"}, {"name": "branch_id", "type": "uuid"}],
         "option_display": {"title": "name", "subtitle": ["branch_id"]}},
        {"key": "Stock", "table": "stock", "columns": [
          {"name": "organization_id", "type": "uuid"}, {"name": "product_id", "type": "uuid"},
          {"name": "warehouse_id", "type": "uuid"}, {"name": "available", "type": "numeric"}]}
      ],
      "option_metrics": [{
        "key": "stock_available", "target": "products.Product", "model": "Stock",
        "foreign_key": "product_id", "aggregate": "sum", "column": "available",
        "scope": [
          {"context": "warehouse_id", "column": "warehouse_id"},
          {"context": "branch_id", "column": "warehouse_id", "through": {"model": "Warehouse", "column": "branch_id"}}
        ]
      }]
    }`)
	v3m, err := v3.Parse(raw)
	if err != nil {
		t.Fatalf("v3.Parse: %v", err)
	}
	m := FromV3(v3m)
	var wh *ModelDefinition
	for i := range m.ModelDefinitions {
		if m.ModelDefinitions[i].ModelKey == "Warehouse" {
			wh = &m.ModelDefinitions[i]
		}
	}
	if wh == nil || wh.OptionDisplay == nil || wh.OptionDisplay.Title != "name" {
		t.Fatalf("option_display not projected: %+v", wh)
	}
	if len(m.OptionMetrics) != 1 {
		t.Fatalf("want 1 metric, got %d", len(m.OptionMetrics))
	}
	om := m.OptionMetrics[0]
	if om.Table != "stock" || om.Key != "stock_available" || om.Target != "products.Product" {
		t.Fatalf("metric not projected: %+v", om)
	}
	if len(om.ThroughTables) != 2 || om.ThroughTables[0] != "" || om.ThroughTables[1] != "warehouses" {
		t.Fatalf("through tables = %v", om.ThroughTables)
	}
}
