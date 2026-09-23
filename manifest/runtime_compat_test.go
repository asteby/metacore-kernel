package manifest_test

import (
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

func runtimeManifest(rng string) *v3.Manifest {
	return &v3.Manifest{Compatibility: v3.Compatibility{Requires: []v3.Requirement{
		{Key: "kernel", Version: ">=3.0.0 <4.0.0"},
		{Key: "metacore-kernel", Version: rng},
		{Key: "products", Version: ">=0.1.0"},
	}}}
}

// QA 0922: customers declared a Tier-3 formula only a kernel >= 0.154.1 host
// can run. "kernel" is the manifest CONTRACT (3.0.0), so it could not stop an
// older host; the "metacore-kernel" key carries the host release range.
func TestFromV3_MetacoreKernelRequirementProjectsToRuntime(t *testing.T) {
	out := manifest.FromV3(runtimeManifest(">=0.154.1"))
	if out.Runtime != ">=0.154.1" {
		t.Fatalf("Runtime = %q, want >=0.154.1", out.Runtime)
	}
	if out.Kernel != ">=3.0.0 <4.0.0" {
		t.Fatalf("Kernel contract must stay untouched, got %q", out.Kernel)
	}
}

func TestEvaluateCompatibility_RuntimeRange(t *testing.T) {
	m := manifest.FromV3(runtimeManifest(">=0.154.1"))
	base := manifest.HostCapabilityProfile{KernelVersion: "3.0.0"}

	ok := base
	ok.RuntimeVersion = "0.154.1"
	if r := m.EvaluateCompatibility(ok); !r.Compatible {
		t.Fatalf("0.154.1 must satisfy >=0.154.1: %v", r.Reasons())
	}

	old := base
	old.RuntimeVersion = "0.152.0"
	r := m.EvaluateCompatibility(old)
	if r.Compatible || r.Issues[0].Kind != manifest.IssueRuntimeRange || !strings.Contains(r.Issues[0].Detail, "upgrade the host") {
		t.Fatalf("0.152.0 must be rejected with a runtime_range issue, got %+v", r)
	}

	// A host that cannot report its release fails closed.
	if r := m.EvaluateCompatibility(base); r.Compatible || r.Issues[0].Kind != manifest.IssueRuntimeRange {
		t.Fatalf("unknown host release must fail closed, got %+v", r)
	}

	// No Runtime range = no constraint, whatever the host reports.
	empty := manifest.FromV3(&v3.Manifest{})
	if r := empty.EvaluateCompatibility(base); !r.Compatible {
		t.Fatalf("no runtime range must pass: %v", r.Reasons())
	}
}

func TestIsReservedRequirementKey(t *testing.T) {
	for _, k := range []string{"kernel", "sdk", "metacore-sdk", "@asteby/metacore-sdk", "metacore-kernel", "github.com/asteby/metacore-kernel"} {
		if !manifest.IsReservedRequirementKey(k) {
			t.Errorf("%q must be reserved (not a peer addon)", k)
		}
	}
	if manifest.IsReservedRequirementKey("products") {
		t.Error("products is a peer addon")
	}
}
