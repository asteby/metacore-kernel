package v3

import (
	"strings"
	"testing"
)

// withDashboard returns a baseValid manifest carrying the supplied dashboard
// widgets plus a frontend block (needed by kind:custom cases).
func withDashboard(widgets []interface{}, withFrontend bool) map[string]interface{} {
	m := baseValid()
	m["contributions"] = map[string]interface{}{
		"dashboard": widgets,
	}
	if withFrontend {
		m["frontend"] = map[string]interface{}{
			"entry":  "https://cdn.example.com/remoteEntry.js",
			"format": "federation",
		}
	}
	return m
}

func TestDashboard_ValidStatWidget(t *testing.T) {
	m := withDashboard([]interface{}{
		map[string]interface{}{
			"key":   "stock_units",
			"title": "inventory.widget.stock_units",
			"kind":  "stat",
			"query": map[string]interface{}{
				"model":     "Stock",
				"aggregate": "sum",
				"field":     "quantity",
			},
		},
	}, false)
	if err := Validate(mustJSON(t, m)); err != nil {
		t.Fatalf("expected valid, got: %v", err)
	}
}

func TestDashboard_ValidBarLineCustom(t *testing.T) {
	m := withDashboard([]interface{}{
		map[string]interface{}{
			"key": "by_wh", "title": "t.bar", "kind": "bar",
			"query": map[string]interface{}{
				"model": "Stock", "aggregate": "sum", "field": "quantity", "group_by": "warehouse_id",
			},
		},
		map[string]interface{}{
			"key": "trend", "title": "t.line", "kind": "area",
			"query": map[string]interface{}{
				"model": "Movement", "aggregate": "count",
				"date_field": "created_at", "interval": "month", "range": "last_12_months",
			},
		},
		map[string]interface{}{
			"key": "heatmap", "title": "t.custom", "kind": "custom",
			"expose": "./StockHeatmap",
		},
	}, true)
	if err := Validate(mustJSON(t, m)); err != nil {
		t.Fatalf("expected valid, got: %v", err)
	}
}

func TestDashboard_RejectsMissingRequired(t *testing.T) {
	// Missing title (schema-required).
	m := withDashboard([]interface{}{
		map[string]interface{}{"key": "x", "kind": "stat",
			"query": map[string]interface{}{"model": "S", "aggregate": "count"}},
	}, false)
	if err := Validate(mustJSON(t, m)); err == nil {
		t.Fatal("expected error for missing title")
	}
}

func TestDashboard_RejectsBadKind(t *testing.T) {
	m := withDashboard([]interface{}{
		map[string]interface{}{"key": "x", "title": "t", "kind": "gauge",
			"query": map[string]interface{}{"model": "S", "aggregate": "count"}},
	}, false)
	if err := Validate(mustJSON(t, m)); err == nil {
		t.Fatal("expected error for unknown kind")
	}
}

func TestDashboard_DeclarativeRequiresQueryModel(t *testing.T) {
	// stat with no query.
	m := withDashboard([]interface{}{
		map[string]interface{}{"key": "x", "title": "t", "kind": "stat"},
	}, false)
	err := Validate(mustJSON(t, m))
	if err == nil || !strings.Contains(err.Error(), "query is required") {
		t.Fatalf("expected query-required error, got: %v", err)
	}
}

func TestDashboard_NonCountRequiresField(t *testing.T) {
	m := withDashboard([]interface{}{
		map[string]interface{}{"key": "x", "title": "t", "kind": "stat",
			"query": map[string]interface{}{"model": "S", "aggregate": "sum"}},
	}, false)
	err := Validate(mustJSON(t, m))
	if err == nil || !strings.Contains(err.Error(), "field is required") {
		t.Fatalf("expected field-required error, got: %v", err)
	}
}

func TestDashboard_BarRequiresGroupBy(t *testing.T) {
	m := withDashboard([]interface{}{
		map[string]interface{}{"key": "x", "title": "t", "kind": "bar",
			"query": map[string]interface{}{"model": "S", "aggregate": "count"}},
	}, false)
	err := Validate(mustJSON(t, m))
	if err == nil || !strings.Contains(err.Error(), "group_by is required") {
		t.Fatalf("expected group_by-required error, got: %v", err)
	}
}

func TestDashboard_LineRequiresDateFieldInterval(t *testing.T) {
	m := withDashboard([]interface{}{
		map[string]interface{}{"key": "x", "title": "t", "kind": "line",
			"query": map[string]interface{}{"model": "S", "aggregate": "count"}},
	}, false)
	err := Validate(mustJSON(t, m))
	if err == nil || !strings.Contains(err.Error(), "date_field is required") {
		t.Fatalf("expected date_field-required error, got: %v", err)
	}
}

func TestDashboard_CustomRequiresExposeAndFrontend(t *testing.T) {
	// custom with expose but NO frontend block.
	m := withDashboard([]interface{}{
		map[string]interface{}{"key": "x", "title": "t", "kind": "custom", "expose": "./X"},
	}, false)
	err := Validate(mustJSON(t, m))
	if err == nil || !strings.Contains(err.Error(), "frontend") {
		t.Fatalf("expected frontend-required error, got: %v", err)
	}

	// custom with frontend but NO expose.
	m2 := withDashboard([]interface{}{
		map[string]interface{}{"key": "x", "title": "t", "kind": "custom"},
	}, true)
	err2 := Validate(mustJSON(t, m2))
	if err2 == nil || !strings.Contains(err2.Error(), "expose") {
		t.Fatalf("expected expose-required error, got: %v", err2)
	}
}

func TestDashboard_CompareRequiresDateRange(t *testing.T) {
	m := withDashboard([]interface{}{
		map[string]interface{}{"key": "x", "title": "t", "kind": "stat",
			"query":   map[string]interface{}{"model": "S", "aggregate": "count"},
			"compare": map[string]interface{}{"to": "previous_period"}},
	}, false)
	err := Validate(mustJSON(t, m))
	if err == nil || !strings.Contains(err.Error(), "compare requires") {
		t.Fatalf("expected compare error, got: %v", err)
	}
}

func TestDashboard_DuplicateKeyRejected(t *testing.T) {
	m := withDashboard([]interface{}{
		map[string]interface{}{"key": "dup", "title": "t", "kind": "stat",
			"query": map[string]interface{}{"model": "S", "aggregate": "count"}},
		map[string]interface{}{"key": "dup", "title": "t2", "kind": "stat",
			"query": map[string]interface{}{"model": "S", "aggregate": "count"}},
	}, false)
	err := Validate(mustJSON(t, m))
	if err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Fatalf("expected duplicate-key error, got: %v", err)
	}
}

func TestDashboard_RejectsUnknownField(t *testing.T) {
	// additionalProperties:false on the widget should reject a typo'd field.
	m := withDashboard([]interface{}{
		map[string]interface{}{"key": "x", "title": "t", "kind": "stat",
			"colour": "blue", // not in schema
			"query":  map[string]interface{}{"model": "S", "aggregate": "count"}},
	}, false)
	if err := Validate(mustJSON(t, m)); err == nil {
		t.Fatal("expected schema rejection of unknown widget field")
	}
}
