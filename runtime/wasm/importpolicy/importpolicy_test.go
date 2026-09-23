package importpolicy

import (
	"strings"
	"testing"
)

// module builds a minimal wasm binary with just an import section.
func module(imports ...Import) []byte {
	var sec []byte
	sec = append(sec, byte(len(imports)))
	for _, im := range imports {
		sec = append(sec, byte(len(im.Module)))
		sec = append(sec, im.Module...)
		sec = append(sec, byte(len(im.Name)))
		sec = append(sec, im.Name...)
		sec = append(sec, 0, 0) // func, type 0
	}
	out := []byte{0, 'a', 's', 'm', 1, 0, 0, 0}
	// type section with one () -> () func type, so type 0 exists
	out = append(out, 1, 4, 1, 0x60, 0, 0)
	out = append(out, 2, byte(len(sec)))
	return append(out, sec...)
}

func TestCheck(t *testing.T) {
	ok := module(Import{WASIModule, "fd_write"}, Import{HostModule, "data_query"})
	if bad, err := Check(ok); err != nil || len(bad) != 0 {
		t.Fatalf("allowed imports flagged: %v %v", bad, err)
	}
	// fiscal_mexico@0.32.0: time.LoadLocation linked path_open.
	bad, err := Check(module(Import{WASIModule, "path_open"}, Import{HostModule, "nope"}, Import{"env", "x"}))
	if err != nil || len(bad) != 3 {
		t.Fatalf("want 3 violations, got %v %v", bad, err)
	}
	if !strings.Contains(strings.Join(bad, "\n"), `"wasi_snapshot_preview1"."path_open"`) {
		t.Fatalf("path_open not named: %v", bad)
	}
	if _, err := Check([]byte("nope")); err == nil {
		t.Fatal("non-wasm must error")
	}
}
