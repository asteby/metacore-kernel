package wasm

import (
	"context"
	"sort"
	"testing"

	"github.com/asteby/metacore-kernel/runtime/wasm/importpolicy"
	"github.com/asteby/metacore-kernel/security"
)

// TestHostFunctionsMatchRegisteredModule keeps importpolicy.HostFunctions —
// the list the hub scanner and the addons CI gate import — equal to what this
// runtime actually registers. Adding a host function without listing it makes
// every guest using it unpublishable; listing one that is not registered lets
// a guest pass the gates and fail to instantiate at install time.
func TestHostFunctionsMatchRegisteredModule(t *testing.T) {
	ctx := context.Background()
	h, err := NewHost(ctx, security.Compile("", nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer h.rt.Close(ctx)
	mod := h.rt.Module(importpolicy.HostModule)
	if mod == nil {
		t.Fatalf("host module %q not instantiated", importpolicy.HostModule)
	}
	registered := map[string]bool{}
	for name := range mod.ExportedFunctionDefinitions() {
		registered[name] = true
	}
	var missing, phantom []string
	for name := range registered {
		if _, ok := importpolicy.HostFunctions[name]; !ok {
			missing = append(missing, name)
		}
	}
	for name := range importpolicy.HostFunctions {
		if !registered[name] {
			phantom = append(phantom, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(phantom)
	if len(missing) > 0 || len(phantom) > 0 {
		t.Fatalf("importpolicy.HostFunctions drifted: registered but not listed %v; listed but not registered %v", missing, phantom)
	}
}
