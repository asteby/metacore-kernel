package dynamic

import (
	"context"
	"fmt"
	"log"
	"strings"

	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/modelbase"
)

// StageMachine is the resolved stage-machine config for a model, as returned by
// a host's StageMachineResolver. It mirrors the stage-machine block of a
// manifest.ModelDefinition (the host typically builds it straight off the
// installed manifest). An empty Stages slice means "no machine": Service.Update
// applies no transition restriction and fires no hooks (the legacy behaviour).
type StageMachine struct {
	// Field is the column carrying the stage (e.g. "stage").
	Field string
	// Stages enumerates the lifecycle stages (value/label/colour/order).
	Stages []manifest.StageDef
	// Transitions whitelists the allowed (from → to) moves.
	Transitions []manifest.TransitionDef
	// OnTransition declares the hooks fired after a valid move is persisted.
	OnTransition []manifest.TransitionHookDef
}

// StageMachineResolver returns the stage machine declared for a model name.
// Hosts wire it from their addon registry; the kernel owns no global model
// index. Returning (nil, false) — or a machine with empty Stages — disables the
// stage machine for that model, leaving Service.Update unrestricted.
type StageMachineResolver func(ctx context.Context, model string) (*StageMachine, bool)

// active reports whether the machine actually restricts transitions: it needs a
// stage field and at least one declared stage. An empty machine is inert.
func (sm *StageMachine) active() bool {
	return sm != nil && sm.Field != "" && len(sm.Stages) > 0
}

// allows reports whether moving from → to is one of the declared transitions.
// A no-op move (from == to) is always allowed — it is not a transition.
func (sm *StageMachine) allows(from, to string) bool {
	if from == to {
		return true
	}
	for _, t := range sm.Transitions {
		if t.From == from && t.To == to {
			return true
		}
	}
	return false
}

// matchingHooks returns the OnTransition hooks that fire for a from → to move,
// in declaration order. "*" matches any stage on either side.
func (sm *StageMachine) matchingHooks(from, to string) []manifest.TransitionHookDef {
	var out []manifest.TransitionHookDef
	for _, h := range sm.OnTransition {
		if (h.From == "*" || h.From == "" || h.From == from) && (h.To == "*" || h.To == to) {
			out = append(out, h)
		}
	}
	return out
}

// resolveStageMachine looks up the active stage machine for a model, or nil when
// the host wired no resolver / the model has none / the machine is inert.
func (s *Service) resolveStageMachine(ctx context.Context, model string) *StageMachine {
	if s.stageMachines == nil {
		return nil
	}
	sm, ok := s.stageMachines(ctx, model)
	if !ok || !sm.active() {
		return nil
	}
	return sm
}

// checkTransition validates a prospective stage move against the machine's
// declared transitions. It returns the before/after stage values and whether the
// stage actually changed. When the move changes the stage to a (from, to) pair
// that is not whitelisted it returns ErrInvalidTransition (mapped to HTTP 422).
func (s *Service) checkTransition(sm *StageMachine, before map[string]any, input map[string]any) (fromStage, toStage string, changed bool, err error) {
	fromStage = stringifyStatus(before[sm.Field])
	toStage = fromStage
	if v, present := input[sm.Field]; present {
		toStage = stringifyStatus(v)
	}
	if toStage == fromStage {
		return fromStage, toStage, false, nil
	}
	if !sm.allows(fromStage, toStage) {
		return fromStage, toStage, true, fmt.Errorf("%w: %q → %q is not a declared transition", ErrInvalidTransition, fromStage, toStage)
	}
	return fromStage, toStage, true, nil
}

// runTransitionHooks dispatches the OnTransition hooks that match a from → to
// move, in declaration order, AFTER the move is persisted and BEFORE the
// canonical event. Each hook's `do` ("wasm:<export>" | "webhook:<key>" |
// "compiled:<fn>") is split on its prefix and routed to the matching
// ActionDispatcher with the {before, after, actor, org} payload.
//
// A failing hook is logged and skipped UNLESS it is Required, in which case the
// error is returned so the caller can roll back the transition. db is the handle
// the hook runs against (the open transaction when the move runs in one).
func (s *Service) runTransitionHooks(ctx context.Context, model string, user modelbase.AuthUser, db *gorm.DB, sm *StageMachine, from, to string, before, after map[string]any) error {
	hooks := sm.matchingHooks(from, to)
	if len(hooks) == 0 {
		return nil
	}
	payload := map[string]any{
		"before": before,
		"after":  after,
		"actor":  user.GetID(),
		"org":    user.GetOrganizationID(),
	}
	for _, h := range hooks {
		prefix, target, found := strings.Cut(h.Do, ":")
		if !found {
			err := fmt.Errorf("dynamic: stage hook %q on %s has no dispatch prefix", h.Do, model)
			if h.Required {
				return err
			}
			log.Printf("dynamic: skipping malformed stage hook on %s.%s→%s: %v", model, from, to, err)
			continue
		}
		dispatcher, ok := s.actionDispatchers[prefix]
		if !ok {
			err := fmt.Errorf("%w: %q (stage hook %q)", ErrUnsupportedTriggerType, prefix, h.Do)
			if h.Required {
				return err
			}
			log.Printf("dynamic: no dispatcher for stage hook %q on %s.%s→%s, skipping", h.Do, model, from, to)
			continue
		}
		// Build the dispatch request. For wasm the target is the export name; the
		// other dispatchers ignore Export and resolve their target out-of-band
		// (the webhook key / compiled fn name travels as the ActionKey).
		trig := &manifest.ActionTrigger{Type: prefix}
		if prefix == "wasm" {
			trig.Export = target
		}
		req := ActionRequest{
			Model:     model,
			ActionKey: target,
			Row:       after,
			Payload:   payload,
			User:      user,
			Trigger:   trig,
			DB:        db,
		}
		resp, derr := dispatcher.Dispatch(ctx, req)
		if derr != nil {
			if h.Required {
				return fmt.Errorf("dynamic: required stage hook %q failed: %w", h.Do, derr)
			}
			log.Printf("dynamic: stage hook %q on %s.%s→%s failed (non-required): %v", h.Do, model, from, to, derr)
			continue
		}
		if !resp.Success {
			if h.Required {
				return fmt.Errorf("%w: required stage hook %q declined", ErrInvalidTransition, h.Do)
			}
			log.Printf("dynamic: stage hook %q on %s.%s→%s declined (non-required)", h.Do, model, from, to)
		}
	}
	return nil
}
