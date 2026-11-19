package dynamic

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/modelbase"
)

// transitionKey is the stable constraint key under which a transition's
// approval is parked and replayed ("transition:draft->cancelled"). It rides the
// existing constraint-approval machinery (ApprovalKindConstraint), so the
// approver inbox, expiry, the approval.requested event and the replay that
// skips exactly this gate all work unchanged.
func transitionKey(from, to string) string { return "transition:" + from + "->" + to }

// Transition returns the declared (from → to) transition, or nil.
func (sm *StageMachine) Transition(from, to string) *manifest.TransitionDef {
	if sm == nil {
		return nil
	}
	for i := range sm.Transitions {
		if sm.Transitions[i].From == from && sm.Transitions[i].To == to {
			return &sm.Transitions[i]
		}
	}
	return nil
}

// CheckTransitionPolicy enforces the optional policy of a declared transition
// (roles, requires_fields, guard) against the merged row — persisted values
// overlaid with the incoming patch. It returns nil when the move declares no
// policy. The approval block is NOT evaluated here (it needs the service); see
// Service.transitionApproval. roles is the acting user's role set; a nil
// `roles` with a roles-gated transition is a denial, except for the system
// principal, which the caller short-circuits by passing bypassRoles=true.
func CheckTransitionPolicy(sm *StageMachine, from, to string, merged map[string]any, roles []string, bypassRoles bool) error {
	t := sm.Transition(from, to)
	if t == nil || from == to {
		return nil
	}
	if len(t.Roles) > 0 && !bypassRoles {
		ok := false
		for _, want := range t.Roles {
			for _, have := range roles {
				if strings.EqualFold(strings.TrimSpace(want), strings.TrimSpace(have)) {
					ok = true
				}
			}
		}
		if !ok {
			return fmt.Errorf("%w: moving %q → %q requires one of the roles %v", ErrForbidden, from, to, t.Roles)
		}
	}
	if len(t.RequiresFields) > 0 {
		ve := &ValidationError{}
		for _, f := range t.RequiresFields {
			if isEmptyValue(merged[f]) {
				ve.add(f, codeRequired, map[string]any{"transition": from + "->" + to})
			}
		}
		if !ve.Empty() {
			return ve
		}
	}
	if g := strings.TrimSpace(t.Guard); g != "" {
		ok, err := evalConstraintExpr(g, mergedNumericEnv(merged, nil))
		if err != nil {
			return fmt.Errorf("%w: transition guard %q: %v", ErrInvalidTransition, g, err)
		}
		if !ok {
			return fmt.Errorf("%w: %q → %q is refused by its guard (%s)", ErrInvalidTransition, from, to, g)
		}
	}
	return nil
}

// transitionApproval returns a synthetic guard violation asking for a
// supervisor when the move declares `approval` and the current context is not
// the replay of that very approval. nil = no approval needed.
func transitionApproval(ctx context.Context, model string, sm *StageMachine, from, to string, merged map[string]any) *ConstraintError {
	t := sm.Transition(from, to)
	if t == nil || t.Approval == nil || from == to {
		return nil
	}
	key := transitionKey(from, to)
	if r, ok := ApprovalReplayFromContext(ctx); ok && r.skipsConstraint(model, key) {
		return nil
	}
	label := t.Approval.Label
	if label == "" {
		label = key
	}
	pol := *t.Approval
	pol.Label = label
	return &ConstraintError{
		ErrorKey: key,
		Expr:     fmt.Sprintf("%s -> %s", from, to),
		Def: manifest.ConstraintDef{
			ErrorKey:    key,
			OnViolation: "request_approval",
			Approval:    &pol,
		},
		Values: map[string]any{"from": from, "to": to},
	}
}

// rolesOf resolves the acting user's role set for transition gating.
func (s *Service) transitionRoles(ctx context.Context, user modelbase.AuthUser) ([]string, bool) {
	if _, ok := user.(systemPrincipal); ok {
		return nil, true
	}
	if user == nil {
		return nil, false
	}
	if r, ok := ApprovalReplayFromContext(ctx); ok && r.Kind != "" {
		// The approver was authorized by role when approving; the replayed
		// mutation runs as the requester and must not be re-gated on roles.
		return nil, true
	}
	return s.principalRoles(ctx, user), false
}

// checkCreateStage keeps a record from being born in a terminal stage: a
// create that names an is_final or locked stage is logged
// (`stage.create_in_final`) and, with Config.StrictStageCreate, refused 422.
func (s *Service) checkCreateStage(sm *StageMachine, model string, input map[string]any) error {
	if !sm.active() {
		return nil
	}
	v, present := input[sm.Field]
	if !present {
		return nil
	}
	stage := StageValue(v)
	for _, st := range sm.Stages {
		if st.Key != stage || !(st.IsFinal || st.Locked) {
			continue
		}
		if s.strictStageCreate {
			return fmt.Errorf("%w: %s cannot be created directly in the final stage %q", ErrInvalidTransition, model, stage)
		}
		log.Printf("dynamic: stage.create_in_final model=%s stage=%s (will be rejected once StrictStageCreate is the default)", model, stage)
		return nil
	}
	return nil
}
