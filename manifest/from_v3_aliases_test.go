package manifest

import (
	"reflect"
	"testing"

	"github.com/asteby/metacore-kernel/manifest/v3"
)

// Model.aliases reaches the host-side ModelDefinition (RFC 0004).
func TestFromV3ModelAliases(t *testing.T) {
	m := &v3.Manifest{Models: []v3.Model{
		{Key: "Order", Table: "orders", Aliases: []string{"sale", "order-items"}, Columns: []v3.Column{{Name: "id", Type: "uuid"}}},
		{Key: "Plain", Table: "plains", Columns: []v3.Column{{Name: "id", Type: "uuid"}}},
	}}
	defs := FromV3(m).ModelDefinitions
	if !reflect.DeepEqual(defs[0].Aliases, []string{"sale", "order-items"}) {
		t.Fatalf("aliases not carried: %v", defs[0].Aliases)
	}
	if len(defs[1].Aliases) != 0 {
		t.Fatalf("a model without aliases must project none: %v", defs[1].Aliases)
	}
}
