package dynamic

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
)

// A jsonb column must accept ANY JSON value — an ARRAY (transfer/adjust/receive
// line-items items[]/lines[]) as well as an OBJECT. Before the fix, jsonb was
// reflected as map[string]any, so json.Unmarshal rejected an array with
// "cannot unmarshal array into Go value of type map[string]interface {}",
// which surfaced from Service.Create's mapToStruct as ErrInvalidInput → the
// 400 "invalid request body" seen on every transfer_stock submit.
func TestJSONBColumnAcceptsArrayAndObject(t *testing.T) {
	field, err := columnToField(manifest.ColumnDef{Name: "items", Type: "jsonb"})
	if err != nil {
		t.Fatalf("columnToField(jsonb): %v", err)
	}
	if got := field.Type; got != reflect.TypeOf(json.RawMessage{}) {
		t.Fatalf("jsonb field type = %v, want json.RawMessage", got)
	}

	st := reflect.StructOf([]reflect.StructField{field})

	cases := map[string]map[string]any{
		"array": {"items": []any{
			map[string]any{"product_id": "p1", "quantity": 2},
			map[string]any{"product_id": "p2", "quantity": 5},
		}},
		"object": {"items": map[string]any{"k": "v"}},
	}

	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			instance := reflect.New(st).Interface()
			if err := mapToStruct(input, instance); err != nil {
				t.Fatalf("mapToStruct(%s) into jsonb: %v", name, err)
			}
			// Round-trips back to the original JSON shape (object OR array).
			b, err := json.Marshal(instance)
			if err != nil {
				t.Fatalf("marshal back: %v", err)
			}
			var out map[string]json.RawMessage
			if err := json.Unmarshal(b, &out); err != nil {
				t.Fatalf("unmarshal back: %v", err)
			}
			want, _ := json.Marshal(input["items"])
			if string(out["items"]) != string(want) {
				t.Fatalf("items round-trip = %s, want %s", out["items"], want)
			}
		})
	}
}
