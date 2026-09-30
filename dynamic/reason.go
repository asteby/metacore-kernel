package dynamic

// reason.go — mandatory reason on destructive operations (PER-4).
//
// A model declares `reason_required` in its manifest (delete and/or a list of
// actions). The kernel then refuses the operation unless the request states WHY,
// and stamps the reason on the canonical event (CanonicalEvent.Reason) so the
// universal activity log answers who removed/cancelled what, when and why —
// without every addon re-implementing a "motivo" prompt and audit column.
//
// The reason travels in the request: `?reason=` (or a JSON body `{"reason"}`) on
// DELETE, `reason` in an action payload. A missing/too-short reason is a 422
// field error on `reason` (`required` / `min`), the same shape the SDK already
// paints on a form control.

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/validate"
)

// ReasonPolicyResolver returns the mandatory-reason policy of a model, or
// (nil, false) when it declares none. The host wires it from its addon registry,
// like ConstraintResolver.
type ReasonPolicyResolver func(ctx context.Context, model string) (*manifest.ReasonRequiredDef, bool)

// ReasonField is the field name a reason failure is reported under.
const ReasonField = "reason"

type reasonKey struct{}

// WithReason returns a ctx carrying the operator's stated reason. Service.Delete
// reads it, and publishCanonical stamps it on the event.
func WithReason(ctx context.Context, reason string) context.Context {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return ctx
	}
	return context.WithValue(ctx, reasonKey{}, reason)
}

// ReasonFromContext returns the reason stored by WithReason ("" when none).
func ReasonFromContext(ctx context.Context) string {
	v, _ := ctx.Value(reasonKey{}).(string)
	return v
}

func (s *Service) reasonPolicy(ctx context.Context, model string) *manifest.ReasonRequiredDef {
	if s.reasonPolicies == nil {
		return nil
	}
	p, ok := s.reasonPolicies(ctx, model)
	if !ok {
		return nil
	}
	return p
}

// CheckReason validates reason against the policy's minimum length. Exported so
// hosts that run their own delete/action path (ops' legacy handlers) enforce the
// same rule and answer the same 422 shape as Service.
func CheckReason(p *manifest.ReasonRequiredDef, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return NewValidationError().Add(ReasonField, validate.CodeRequired, nil)
	}
	if min := p.Min(); utf8.RuneCountInString(reason) < min {
		return NewValidationError().Add(ReasonField, validate.CodeMin, map[string]any{"min": min})
	}
	return nil
}

// requireDeleteReason enforces the delete policy and returns ctx carrying the
// reason (so the canonical event stamps it). No policy / delete not covered =
// ctx unchanged, nil.
func (s *Service) requireDeleteReason(ctx context.Context, model string) (context.Context, error) {
	p := s.reasonPolicy(ctx, model)
	if p == nil || !p.Delete {
		return ctx, nil
	}
	if err := CheckReason(p, ReasonFromContext(ctx)); err != nil {
		return ctx, err
	}
	return ctx, nil
}

// ActionReason extracts the reason of an action invocation: payload `reason`,
// else the ctx one (WithReason).
func ActionReason(ctx context.Context, payload map[string]any) string {
	if v, ok := payload[ReasonField].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return ReasonFromContext(ctx)
}

// requireActionReason enforces the policy for one action. Returns the ctx with
// the reason attached and whether the action was covered (the caller then
// publishes the audit event once it succeeds).
func (s *Service) requireActionReason(ctx context.Context, model, key string, payload map[string]any) (context.Context, bool, error) {
	p := s.reasonPolicy(ctx, model)
	if p == nil || !p.RequiresAction(key) {
		return ctx, false, nil
	}
	reason := ActionReason(ctx, payload)
	if err := CheckReason(p, reason); err != nil {
		return ctx, true, err
	}
	return WithReason(ctx, reason), true, nil
}
