package marketplace

import (
	"testing"

	"github.com/asteby/metacore-kernel/bundle"
)

func TestExtractRequires_SkipsKernelAndOptionalPeers(t *testing.T) {
	raw := []byte(`{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "pos", "name": "POS", "version": "1.0.0" },
  "compatibility": {
    "requires": [
      { "key": "kernel", "version": ">=3.0.0 <4.0.0" },
      { "key": "customers", "version": ">=0.1.0" },
      { "key": "caja", "version": ">=0.2.0", "optional": true, "reason": "policy only" }
    ]
  }
}`)
	got := extractRequires(&bundle.Bundle{RawManifest: raw})
	if len(got) != 1 || got[0] != "customers" {
		t.Fatalf("extractRequires = %#v, want [customers] (kernel + optional caja skipped)", got)
	}
}

func TestExtractRequires_AllOptional_ReturnsNil(t *testing.T) {
	raw := []byte(`{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "pos", "name": "POS", "version": "1.0.0" },
  "compatibility": {
    "requires": [
      { "key": "kernel", "version": ">=3.0.0 <4.0.0" },
      { "key": "caja", "version": ">=0.2.0", "optional": true }
    ]
  }
}`)
	if got := extractRequires(&bundle.Bundle{RawManifest: raw}); got != nil {
		t.Fatalf("extractRequires = %#v, want nil when only kernel + optional remain", got)
	}
}
