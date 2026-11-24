package manifest

import (
	"fmt"

	"github.com/asteby/metacore-kernel/capability"
)

// AIGrants lists the manifest's `ai:complete` capabilities as feature grants
// (target = feature, reason = purpose).
func (m *Manifest) AIGrants() []capability.AIGrant {
	return aiGrants(m.Capabilities)
}

func aiGrants(caps []Capability) []capability.AIGrant {
	var out []capability.AIGrant
	for _, c := range caps {
		if c.Kind == capability.AIPermissionKind {
			out = append(out, capability.AIGrant{Feature: c.Target, Purpose: c.Reason})
		}
	}
	return out
}

// AIPolicy is what this addon may do with ai.complete, as its manifest
// declares it. Hosts lower it with the org's configuration (caps) and add a
// sponsor from their configuration; see capability.AIGate.
func (m *Manifest) AIPolicy() capability.AIPolicy {
	return capability.NewAIPolicy(m.Key, m.AIGrants(), m.AI)
}

// validateAI checks the ai:complete grants and the `ai` block together, and
// refuses an addon that tries to provide a host-implemented contract.
func (m *Manifest) validateAI() error {
	if errs := capability.ValidateAIDeclaration(m.AIGrants(), m.AI); len(errs) > 0 {
		return fmt.Errorf("manifest: %s", errs[0])
	}
	for i, pc := range m.ProvidesCapabilities {
		if c, ok := capability.Lookup(pc.Key); ok && c.HostImplemented {
			return fmt.Errorf("manifest.provides_capabilities[%d]: %q is implemented by the host; an addon cannot provide it", i, pc.Key)
		}
	}
	return nil
}
