package manifest

import (
	"testing"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

func TestFromV3_ProjectsStageAuthority(t *testing.T) {
	m := FromV3(&v3.Manifest{
		Contributions: &v3.Contributions{Actions: []v3.Action{
			{Key: "cancel_sale", TargetModel: "Sale", Handler: v3.Handler{Type: "wasm", Function: "x"},
				StageAuthority: &v3.StageAuthority{Authority: "action_permission", Transitions: []string{"paid->cancelled"}}},
			{Key: "plain", TargetModel: "Sale", Handler: v3.Handler{Type: "wasm", Function: "y"}},
		}},
	})
	var withA, plain *ActionDef
	for _, defs := range m.Actions {
		for i := range defs {
			switch defs[i].Key {
			case "cancel_sale":
				withA = &defs[i]
			case "plain":
				plain = &defs[i]
			}
		}
	}
	if withA == nil || !withA.StageAuthority.Covers("paid", "cancelled") || withA.StageAuthority.Covers("paid", "draft") {
		t.Fatalf("stage_authority lost in projection: %+v", withA)
	}
	if plain == nil || plain.StageAuthority != nil {
		t.Fatalf("plain action must carry none: %+v", plain)
	}
}
