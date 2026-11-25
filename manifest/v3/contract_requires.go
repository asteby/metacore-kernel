package v3

import (
	"fmt"
	"strings"

	"github.com/Masterminds/semver/v3"
)

// ContractRequirement is one compatibility.requires entry that depends on a
// capability contract instead of a concrete addon (RFC SDK-0001 §3.2). The
// installer resolves it to the contract's OWNER; the organization chooses the
// provider (decision A6).
type ContractRequirement struct {
	Contract string
	Version  string
	Optional bool
	Reason   string
	Enables  []string
}

// ContractRequirements lists the requires[].contract entries in declaration
// order (Enables copied). Addon requirements are not included.
func (m *Manifest) ContractRequirements() []ContractRequirement {
	if m == nil {
		return nil
	}
	var out []ContractRequirement
	for _, r := range m.Compatibility.Requires {
		c := strings.TrimSpace(r.Contract)
		if c == "" || strings.TrimSpace(r.Key) != "" {
			continue
		}
		out = append(out, ContractRequirement{
			Contract: c, Version: strings.TrimSpace(r.Version), Optional: r.Optional,
			Reason: r.Reason, Enables: append([]string(nil), r.Enables...),
		})
	}
	return out
}

// RuleContractConformanceMissing: a contract at version >= 1.0.0 declares no
// conformance suite (RFC SDK-0001 §3.2, N1). Warning-only in this release in
// every mode, so no published contract stops validating; the hub may refuse
// to publish on it (CheckContractConformance).
const RuleContractConformanceMissing = "contract_conformance_missing"

// CheckContractConformance returns one problem per contract >= 1.0.0 that has
// no conformance path. Publish-time callers (the hub) treat them as errors.
func CheckContractConformance(cs []Contract) []string {
	var out []string
	for i, c := range cs {
		v, err := semver.NewVersion(c.Version)
		if err != nil || v.Major() < 1 || strings.TrimSpace(c.Conformance) != "" {
			continue
		}
		out = append(out, fmt.Sprintf("contracts[%d] (%s): version %s >= 1.0.0 needs conformance (path of the conformance vectors)", i, c.Key, c.Version))
	}
	return out
}

func contractConformanceWarnings(cs []Contract) []string {
	ps := CheckContractConformance(cs)
	for i, p := range ps {
		ps[i] = fmt.Sprintf("[%s] %s", RuleContractConformanceMissing, p)
	}
	return ps
}
