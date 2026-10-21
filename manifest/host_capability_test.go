package manifest

import (
	"runtime/debug"
	"testing"
)

func TestEvaluateCompatibility_Compatible(t *testing.T) {
	m := &Manifest{
		Kernel:           ">=3.0.0 <4.0.0",
		SDK:              ">=1.2.0",
		HostCapabilities: []string{"wasm-runtime", "webhooks"},
	}
	profile := HostCapabilityProfile{
		KernelVersion: "3.1.0",
		SDKVersion:    "1.5.0",
		Capabilities:  []string{"wasm-runtime", "webhooks", "native-service"},
	}
	res := m.EvaluateCompatibility(profile)
	if !res.Compatible {
		t.Fatalf("expected compatible, got issues: %+v", res.Issues)
	}
	if len(res.Issues) != 0 {
		t.Fatalf("expected no issues, got %+v", res.Issues)
	}
}

func TestEvaluateCompatibility_NoConstraints(t *testing.T) {
	m := &Manifest{}
	res := m.EvaluateCompatibility(HostCapabilityProfile{KernelVersion: "1.0.0"})
	if !res.Compatible {
		t.Fatalf("expected compatible for an empty manifest, got %+v", res.Issues)
	}
}

func TestEvaluateCompatibility_KernelRangeMismatch(t *testing.T) {
	m := &Manifest{Kernel: ">=4.0.0"}
	res := m.EvaluateCompatibility(HostCapabilityProfile{KernelVersion: "3.1.0"})
	if res.Compatible {
		t.Fatalf("expected incompatible")
	}
	if len(res.Issues) != 1 || res.Issues[0].Kind != IssueKernelRange || res.Issues[0].Field != "kernel" {
		t.Fatalf("unexpected issues: %+v", res.Issues)
	}
}

func TestEvaluateCompatibility_SDKRangeFailsClosedWithoutSDKVersion(t *testing.T) {
	m := &Manifest{SDK: ">=2.0.0"}
	// Kernel and SDK versions are independent. A host that omits SDKVersion
	// cannot prove this constraint even when its kernel version happens to fit.
	res := m.EvaluateCompatibility(HostCapabilityProfile{KernelVersion: "1.0.0"})
	if res.Compatible {
		t.Fatalf("expected incompatible")
	}
	if len(res.Issues) != 1 || res.Issues[0].Kind != IssueInvalidRange || res.Issues[0].Field != "sdk" {
		t.Fatalf("unexpected issues: %+v", res.Issues)
	}

	res2 := m.EvaluateCompatibility(HostCapabilityProfile{KernelVersion: "1.0.0", SDKVersion: "2.1.0"})
	if !res2.Compatible {
		t.Fatalf("expected compatible when SDKVersion satisfies the range, got %+v", res2.Issues)
	}
}

func TestEvaluateCompatibility_MissingCapabilities(t *testing.T) {
	m := &Manifest{HostCapabilities: []string{"zeta", "alpha", "beta"}}
	res := m.EvaluateCompatibility(HostCapabilityProfile{
		KernelVersion: "1.0.0",
		Capabilities:  []string{"alpha"},
	})
	if res.Compatible {
		t.Fatalf("expected incompatible")
	}
	if len(res.Issues) != 2 {
		t.Fatalf("expected 2 missing-capability issues, got %+v", res.Issues)
	}
	// Deterministic: missing capabilities are sorted, not manifest-declaration order.
	if res.Issues[0].Capability != "beta" || res.Issues[1].Capability != "zeta" {
		t.Fatalf("expected sorted missing capabilities [beta zeta], got %+v", res.Issues)
	}
	for _, iss := range res.Issues {
		if iss.Kind != IssueMissingCapability || iss.Field != "host_capabilities" {
			t.Fatalf("unexpected issue: %+v", iss)
		}
	}
}

func TestEvaluateCompatibility_InvalidManifestRange(t *testing.T) {
	m := &Manifest{Kernel: "not-a-range"}
	res := m.EvaluateCompatibility(HostCapabilityProfile{KernelVersion: "1.0.0"})
	if res.Compatible {
		t.Fatalf("expected incompatible")
	}
	if len(res.Issues) != 1 || res.Issues[0].Kind != IssueInvalidRange {
		t.Fatalf("unexpected issues: %+v", res.Issues)
	}
}

func TestEvaluateCompatibility_InvalidHostVersion(t *testing.T) {
	m := &Manifest{Kernel: ">=1.0.0"}
	res := m.EvaluateCompatibility(HostCapabilityProfile{KernelVersion: "not-semver"})
	if res.Compatible {
		t.Fatalf("expected incompatible")
	}
	if len(res.Issues) != 1 || res.Issues[0].Kind != IssueInvalidRange {
		t.Fatalf("unexpected issues: %+v", res.Issues)
	}
}

func TestEvaluateCompatibility_MultipleIssuesOrderedDeterministically(t *testing.T) {
	m := &Manifest{
		Kernel:           ">=4.0.0",
		SDK:              ">=2.0.0",
		HostCapabilities: []string{"missing-one"},
	}
	res := m.EvaluateCompatibility(HostCapabilityProfile{KernelVersion: "1.0.0", SDKVersion: "1.0.0"})
	if res.Compatible {
		t.Fatalf("expected incompatible")
	}
	if len(res.Issues) != 3 {
		t.Fatalf("expected 3 issues, got %+v", res.Issues)
	}
	kinds := []CompatibilityIssueKind{res.Issues[0].Kind, res.Issues[1].Kind, res.Issues[2].Kind}
	want := []CompatibilityIssueKind{IssueKernelRange, IssueSDKRange, IssueMissingCapability}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("expected issue order %v, got %v", want, kinds)
		}
	}
	reasons := res.Reasons()
	if len(reasons) != 3 || reasons[2] == "" {
		t.Fatalf("unexpected Reasons(): %v", reasons)
	}
}

func TestNormalizeModuleVersion_PseudoVersions(t *testing.T) {
	cases := map[string]string{
		"v0.203.1":                               "0.203.1",
		"v1.4.0-rc.1":                            "1.4.0-rc.1",
		"(devel)":                                "",
		"":                                       "",
		"v0.203.2-0.20261016164255-ca5563ec1d19": "0.203.1", // commit after v0.203.1
		"v1.5.0-rc.2.0.20261016164255-ca5563ec1d19": "1.5.0-rc.2",
		"v0.0.0-20261016164255-ca5563ec1d19":        "", // no tagged ancestor
	}
	for in, want := range cases {
		if got := normalizeModuleVersion(in); got != want {
			t.Errorf("normalizeModuleVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

// A host pinned to an untagged kernel commit must satisfy ">=" ranges of the
// release it is built on (it used to be ordered as a prerelease and fail).
func TestCheckRuntime_HostOnPseudoVersion(t *testing.T) {
	m := &Manifest{Runtime: ">=0.182.0"}
	if err := m.CheckRuntime(normalizeModuleVersion("v0.203.2-0.20261016164255-ca5563ec1d19")); err != nil {
		t.Fatalf("CheckRuntime on a pseudo-version host: %v", err)
	}
}

func TestKernelVersionOfLocalReplaceKeepsRequiredRelease(t *testing.T) {
	cases := []struct {
		name string
		dep  debug.Module
		want string
	}{
		{"sin replace", debug.Module{Version: "v0.229.1"}, "0.229.1"},
		{"replace a ruta local (devel)", debug.Module{Version: "v0.229.1", Replace: &debug.Module{Version: "(devel)"}}, "0.229.1"},
		{"replace a ruta local sin versión", debug.Module{Version: "v0.229.1", Replace: &debug.Module{}}, "0.229.1"},
		{"replace a una versión publicada gana", debug.Module{Version: "v0.229.1", Replace: &debug.Module{Version: "v0.230.0"}}, "0.230.0"},
		{"todo devel", debug.Module{Version: "(devel)", Replace: &debug.Module{Version: "(devel)"}}, ""},
	}
	for _, c := range cases {
		if got := kernelVersionOf(&c.dep); got != c.want {
			t.Errorf("%s: kernelVersionOf = %q, want %q", c.name, got, c.want)
		}
	}
}
