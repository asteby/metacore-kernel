package manifest

import (
	"fmt"
	"runtime/debug"
	"slices"
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"
)

// HostCapabilityProfile is what a host declares about itself before it
// attempts to install a preset or an addon: the kernel version it runs, the
// SDK version it exposes to addon code, and the set of named runtime
// capabilities it has actually wired up (e.g. "wasm-runtime",
// "native-service", "webhooks", "connectors"). It carries no reference to any
// specific host implementation (no hub/ops/link concept) — it is a plain
// value type any host builds and passes to EvaluateCompatibility.
type HostCapabilityProfile struct {
	// KernelVersion is the running kernel's semver (see manifest.APIVersion
	// for the version this package implements).
	KernelVersion string
	// SDKVersion is the semver of the addon SDK surface the host exposes.
	// Empty means the host cannot prove SDK compatibility. A manifest with an
	// SDK constraint therefore fails closed; kernel and SDK releases are
	// independent contracts and must never be substituted for one another.
	SDKVersion string
	// RuntimeVersion is the metacore-kernel Go module release the host is
	// built with (e.g. "0.154.1"; see HostRuntimeVersion). Empty means the
	// host cannot prove it: a manifest with a Runtime range fails closed.
	RuntimeVersion string
	// Capabilities is the set of named host capabilities available. Order
	// and duplicates are insignificant — lookups are by membership.
	Capabilities []string
}

// hasCapability reports whether name is present in the profile's capability
// set (exact match).
func (p HostCapabilityProfile) hasCapability(name string) bool {
	return slices.Contains(p.Capabilities, name)
}

// CompatibilityIssueKind classifies a single CompatibilityIssue so callers
// can branch on the reason without parsing Detail strings.
type CompatibilityIssueKind string

const (
	// IssueKernelRange: the profile's KernelVersion does not satisfy the
	// manifest's Kernel semver range.
	IssueKernelRange CompatibilityIssueKind = "kernel_range"
	// IssueSDKRange: the profile's SDKVersion does not satisfy the manifest's
	// SDK semver range.
	IssueSDKRange CompatibilityIssueKind = "sdk_range"
	// IssueRuntimeRange: the profile's RuntimeVersion (host metacore-kernel
	// release) does not satisfy the manifest's Runtime range, or the host
	// does not report it.
	IssueRuntimeRange CompatibilityIssueKind = "runtime_range"
	// IssueMissingCapability: the manifest declares a HostCapabilities entry
	// the profile does not have.
	IssueMissingCapability CompatibilityIssueKind = "missing_capability"
	// IssueInvalidRange: the manifest's Kernel/SDK field is not a parseable
	// semver constraint, or the profile's corresponding version is not
	// parseable semver. Distinguished from a real mismatch so callers don't
	// conflate "addon manifest is malformed" with "host is too old".
	IssueInvalidRange CompatibilityIssueKind = "invalid_range"
)

// CompatibilityIssue is one structured reason a manifest is not installable
// against a given HostCapabilityProfile.
type CompatibilityIssue struct {
	Kind CompatibilityIssueKind
	// Field names the manifest field the issue is about ("kernel", "sdk", or
	// "host_capabilities").
	Field string
	// Capability is set only for IssueMissingCapability: the missing name.
	Capability string
	// Detail is a human-readable explanation, safe to surface directly.
	Detail string
}

// CompatibilityResult is the deterministic outcome of evaluating a manifest
// against a HostCapabilityProfile. Compatible is true iff Issues is empty.
type CompatibilityResult struct {
	Compatible bool
	Issues     []CompatibilityIssue
}

// Reasons returns each issue's Detail, in evaluation order, for callers that
// just want human-readable strings (e.g. to render as install-blocking
// messages).
func (r CompatibilityResult) Reasons() []string {
	out := make([]string, 0, len(r.Issues))
	for _, iss := range r.Issues {
		out = append(out, iss.Detail)
	}
	return out
}

// EvaluateCompatibility runs a deterministic, side-effect-free check of the
// manifest's Kernel range, SDK range and HostCapabilities against profile.
// It never mutates the manifest and performs no I/O — hosts call it before
// preset resolution / InstallAddonFunc dispatch and before installer.Install,
// so an incompatible addon is rejected with structured reasons instead of
// failing mid-install.
//
// Evaluation order is fixed (kernel, then sdk, then host capabilities in
// manifest declaration order) so Issues is stable across runs for the same
// inputs — callers can diff results or assert on them without sorting.
func (m *Manifest) EvaluateCompatibility(profile HostCapabilityProfile) CompatibilityResult {
	var issues []CompatibilityIssue

	if iss := checkSemverField("kernel", m.Kernel, profile.KernelVersion, IssueKernelRange); iss != nil {
		issues = append(issues, *iss)
	}

	if m.SDK != "" {
		if iss := checkSemverField("sdk", m.SDK, profile.SDKVersion, IssueSDKRange); iss != nil {
			issues = append(issues, *iss)
		}
	}

	if m.Runtime != "" {
		if strings.TrimSpace(profile.RuntimeVersion) == "" {
			issues = append(issues, CompatibilityIssue{
				Kind:   IssueRuntimeRange,
				Field:  "runtime",
				Detail: fmt.Sprintf("manifest.runtime: addon requires metacore-kernel %s but the host does not report its kernel release", m.Runtime),
			})
		} else if iss := checkSemverField("runtime", m.Runtime, profile.RuntimeVersion, IssueRuntimeRange); iss != nil {
			if iss.Kind == IssueRuntimeRange {
				iss.Detail = fmt.Sprintf("manifest.runtime: addon requires metacore-kernel %s; this host runs %s — upgrade the host first", m.Runtime, profile.RuntimeVersion)
			}
			issues = append(issues, *iss)
		}
	}

	missing := make([]string, 0, len(m.HostCapabilities))
	for _, name := range m.HostCapabilities {
		if !profile.hasCapability(name) {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	for _, name := range missing {
		issues = append(issues, CompatibilityIssue{
			Kind:       IssueMissingCapability,
			Field:      "host_capabilities",
			Capability: name,
			Detail:     fmt.Sprintf("manifest.host_capabilities: host is missing required capability %q", name),
		})
	}

	return CompatibilityResult{
		Compatible: len(issues) == 0,
		Issues:     issues,
	}
}

// checkSemverField evaluates one semver-range field ("kernel" or "sdk")
// against a version string, returning nil when it's empty (no constraint) or
// satisfied, and a structured issue otherwise.
func checkSemverField(field, rangeExpr, version string, kind CompatibilityIssueKind) *CompatibilityIssue {
	if rangeExpr == "" {
		return nil
	}
	constraint, err := semver.NewConstraint(rangeExpr)
	if err != nil {
		return &CompatibilityIssue{
			Kind:   IssueInvalidRange,
			Field:  field,
			Detail: fmt.Sprintf("manifest.%s: invalid range %q: %v", field, rangeExpr, err),
		}
	}
	v, err := semver.NewVersion(version)
	if err != nil {
		return &CompatibilityIssue{
			Kind:   IssueInvalidRange,
			Field:  field,
			Detail: fmt.Sprintf("host %s version %q is not semver: %v", field, version, err),
		}
	}
	if !constraint.Check(v) {
		return &CompatibilityIssue{
			Kind:   kind,
			Field:  field,
			Detail: fmt.Sprintf("manifest.%s: host %s %s does not satisfy %s", field, field, version, rangeExpr),
		}
	}
	return nil
}

// kernelModulePath is the Go module whose release HostRuntimeVersion reports.
const kernelModulePath = "github.com/asteby/metacore-kernel"

// HostRuntimeVersion returns the metacore-kernel release the running binary
// was built with ("0.154.1", without the "v"), read from the Go build info.
// "" when it cannot be determined (a `replace` to a local path, or tests),
// which EvaluateCompatibility treats as "cannot prove" for Runtime ranges.
func HostRuntimeVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, d := range bi.Deps {
		if d.Path != kernelModulePath {
			continue
		}
		// A `replace` to a released version wins; a replace to a local path
		// (no version) keeps the required release, which is what the code
		// was written against.
		if d.Replace != nil && d.Replace.Version != "" {
			return normalizeModuleVersion(d.Replace.Version)
		}
		return normalizeModuleVersion(d.Version)
	}
	return ""
}

// normalizeModuleVersion strips the "v" and rejects pseudo / devel versions
// that are not a real release ("(devel)", "").
func normalizeModuleVersion(v string) string {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if v == "" || strings.HasPrefix(v, "(") {
		return ""
	}
	return v
}

// CheckRuntime returns an install-blocking error when the manifest declares a
// Runtime range (compatibility.requires "metacore-kernel") the host release
// does not satisfy — or the host cannot report one. nil when there is no
// range or it is satisfied. Installers call it on install AND upgrade.
func (m *Manifest) CheckRuntime(hostRuntimeVersion string) error {
	if m.Runtime == "" {
		return nil
	}
	r := m.EvaluateCompatibility(HostCapabilityProfile{
		KernelVersion:  "",
		RuntimeVersion: hostRuntimeVersion,
	})
	for _, iss := range r.Issues {
		if iss.Field == "runtime" {
			return fmt.Errorf("%s", iss.Detail)
		}
	}
	return nil
}
