package dynamic

import (
	"context"

	"github.com/asteby/metacore-kernel/manifest"
)

// ActionDef returns the manifest.ActionDef the host's ActionResolver declares
// for (model, key), the same lookup ExecAction does. It reports false when no
// resolver was wired or the action does not exist. Callers that need to know
// what an action requires BEFORE running it (inbound registration, the target
// catalogue) read it here instead of keeping a second index.
func (s *Service) ActionDef(ctx context.Context, model, key string) (*manifest.ActionDef, bool) {
	if s.actionResolver == nil {
		return nil, false
	}
	def, ok := s.actionResolver(ctx, model, key)
	if !ok || def == nil {
		return nil, false
	}
	return def, true
}

// ActiveStageMachine returns the stage machine of a model when it actually
// restricts transitions (a stage field and at least one stage), the same one
// Update and ExecAction use. It reports false for a model without machine, an
// inert one, or a Service without StageMachineResolver.
func (s *Service) ActiveStageMachine(ctx context.Context, model string) (*StageMachine, bool) {
	sm := s.resolveStageMachine(ctx, model)
	return sm, sm != nil
}
