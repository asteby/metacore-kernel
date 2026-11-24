package v3

import (
	"fmt"

	"github.com/asteby/metacore-kernel/capability"
)

// AIGrants lists the manifest's `ai:complete` capabilities as feature grants
// (target = feature, reason = purpose).
func (m *Manifest) AIGrants() []capability.AIGrant {
	var out []capability.AIGrant
	for _, c := range m.Capabilities {
		if c.Kind == capability.AIPermissionKind {
			out = append(out, capability.AIGrant{Feature: c.Target, Purpose: c.Reason})
		}
	}
	return out
}

// validateAI checks the ai:complete grants with the `ai` block (shared rules:
// capability.ValidateAIDeclaration) and refuses provides_capabilities of a
// host-implemented contract such as ai.complete.
func validateAI(m *Manifest) []string {
	errs := capability.ValidateAIDeclaration(m.AIGrants(), m.AI)
	for i, pc := range m.ProvidesCapabilities {
		if c, ok := capability.Lookup(pc.Key); ok && c.HostImplemented {
			errs = append(errs, fmt.Sprintf("provides_capabilities[%d].key %q is implemented by the host; an addon cannot provide it", i, pc.Key))
		}
	}
	return errs
}
