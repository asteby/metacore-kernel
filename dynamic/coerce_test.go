package dynamic

import (
	"testing"

	"github.com/google/uuid"
)

type coerceTarget struct {
	ID       uuid.UUID `json:"id"`
	ParentID uuid.UUID `json:"parent_id"`
	Code     string    `json:"code"`
	Qty      int       `json:"qty"`
	Price    float64   `json:"price"`
	Active   bool      `json:"active"`
}

func TestCoerceInputToStruct(t *testing.T) {
	valid := uuid.New().String()
	in := map[string]any{
		"id":        valid,   // valid uuid → kept
		"parent_id": "test",  // invalid uuid → dropped
		"code":      "798",   // string field → kept verbatim
		"qty":       "98",    // numeric string → int
		"price":     "9.99",  // numeric string → float
		"active":    "false", // bool string → bool
		"unknown":   "x",     // no matching field → untouched
	}
	coerceInputToStruct(in, &coerceTarget{})

	if in["id"] != valid {
		t.Fatalf("valid uuid should be kept, got %v", in["id"])
	}
	if _, ok := in["parent_id"]; ok {
		t.Fatalf("invalid uuid parent_id should be dropped, got %v", in["parent_id"])
	}
	if in["code"] != "798" {
		t.Fatalf("string code should be verbatim, got %v", in["code"])
	}
	if v, ok := in["qty"].(int64); !ok || v != 98 {
		t.Fatalf("qty should coerce to int64(98), got %#v", in["qty"])
	}
	if v, ok := in["price"].(float64); !ok || v != 9.99 {
		t.Fatalf("price should coerce to float64(9.99), got %#v", in["price"])
	}
	if v, ok := in["active"].(bool); !ok || v != false {
		t.Fatalf("active should coerce to bool(false), got %#v", in["active"])
	}
	if in["unknown"] != "x" {
		t.Fatalf("unknown field should be untouched, got %v", in["unknown"])
	}
}

func TestCoerceInputToStruct_EmptyTypedDropped(t *testing.T) {
	in := map[string]any{
		"parent_id": "", // empty uuid → dropped
		"qty":       "", // empty number → dropped
		"active":    "", // empty bool → dropped
		"code":      "", // empty string → kept (strings accept "")
	}
	coerceInputToStruct(in, &coerceTarget{})
	for _, k := range []string{"parent_id", "qty", "active"} {
		if _, ok := in[k]; ok {
			t.Fatalf("empty typed field %q should be dropped", k)
		}
	}
	if v, ok := in["code"]; !ok || v != "" {
		t.Fatalf("empty string field should be kept, got %#v", in["code"])
	}
}

// Real addon models are reflect.StructOf with no methods; coercion must still
// resolve fields by json tag.
func TestCoerceInputToStruct_NonStringValuesUntouched(t *testing.T) {
	in := map[string]any{
		"active": true, // already bool
		"qty":    int64(5),
		"price":  3.14,
	}
	coerceInputToStruct(in, &coerceTarget{})
	if in["active"] != true || in["qty"] != int64(5) || in["price"] != 3.14 {
		t.Fatalf("non-string values must pass through untouched, got %#v", in)
	}
}
