package dynamic

import (
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
)

// visible_when.class reaches the served metadata the SDK reads.
func TestToVisibleWhen_Class(t *testing.T) {
	got := toVisibleWhen(&manifest.VisibleWhenDef{Class: "tire"})
	if got == nil || got.Class != "tire" || got.Field != "" {
		t.Fatalf("got %+v", got)
	}
}
