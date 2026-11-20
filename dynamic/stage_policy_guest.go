package dynamic

import (
	"context"
	"errors"
	"log"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/modelbase"
)

// K3b: stage-policy parity for the wasm data_mutate / data_batch imports.
//
// Service.Update enforces, per declared transition, roles / requires_fields /
// guard / approval and the locked-stage rule. The wasm imports used to apply
// only the from -> to whitelist (GateWrite), so a guest could cancel a sale
// without the approval the REST path demands. EnforceWasmStageWrite is the
// single entry the wasm runtime calls (Host.WithStagePolicy) to close that gap
// with the SAME functions the REST path uses (CheckStageLock,
// CheckStageLockDelete, CheckTransitionPolicy, transitionApproval) so the two
// surfaces cannot drift.

// WasmStagePolicyMode selects how strictly the wasm writes are held to the
// stage machine's per-transition policy. See Config.WasmStagePolicy.
type WasmStagePolicyMode string

const (
	// WasmStagePolicyOff keeps the legacy from -> to whitelist only.
	WasmStagePolicyOff WasmStagePolicyMode = "off"
	// WasmStagePolicyWarn evaluates the policy and logs
	// `stage_policy.wasm_bypass` for each write it would refuse, without
	// blocking. The default for one release.
	WasmStagePolicyWarn WasmStagePolicyMode = "warn"
	// WasmStagePolicyEnforce refuses / parks like Service.Update.
	WasmStagePolicyEnforce WasmStagePolicyMode = "enforce"
)

// normalize maps "" and any unknown value to the default (warn): a typo must
// never silently turn the gate off.
func (m WasmStagePolicyMode) normalize() WasmStagePolicyMode {
	switch WasmStagePolicyMode(strings.ToLower(strings.TrimSpace(string(m)))) {
	case WasmStagePolicyOff:
		return WasmStagePolicyOff
	case WasmStagePolicyEnforce:
		return WasmStagePolicyEnforce
	case WasmStagePolicyWarn, "":
		return WasmStagePolicyWarn
	}
	log.Printf("dynamic: unknown Config.WasmStagePolicy %q, using %q", string(m), WasmStagePolicyWarn)
	return WasmStagePolicyWarn
}

// WasmStagePolicy reports the effective mode.
func (s *Service) WasmStagePolicy() WasmStagePolicyMode {
	if s == nil || s.wasmStagePolicy == "" {
		return WasmStagePolicyWarn
	}
	return s.wasmStagePolicy
}

// actionStageAuthority is what ExecAction leaves in the context of a wasm
// action that is the declared authority (Action.stage_authority) for some
// transitions. Only the kernel can set it (the key is unexported, the value
// comes from the installed manifest), never the guest.
type actionStageAuthority struct {
	model string
	key   string
	def   *manifest.StageAuthorityDef
}

type actionStageAuthorityKey struct{}

func withActionStageAuthority(ctx context.Context, model, key string, def *manifest.StageAuthorityDef) context.Context {
	if def == nil || len(def.Transitions) == 0 {
		return ctx
	}
	return context.WithValue(ctx, actionStageAuthorityKey{}, actionStageAuthority{model: model, key: key, def: def})
}

// coversTransition reports whether the running action is the authority for
// from -> to on `model`.
func coversTransition(ctx context.Context, model, from, to string) bool {
	a, has := ctx.Value(actionStageAuthorityKey{}).(actionStageAuthority)
	if !has || !a.def.Covers(from, to) {
		return false
	}
	return strings.EqualFold(a.model, model)
}

// WasmStageWrite describes one wasm mutation of a row of a model that has a
// stage machine. Before is the persisted row (read inside the transaction);
// Patch / Inc are the guest's absolute sets and numeric deltas.
type WasmStageWrite struct {
	Model    string // guest ModelKey; falls back to Table
	Table    string // logical table
	AddonKey string // calling addon (logging, approval inbox)
	Op       string // "update" | "delete"
	RecordID string
	OrgID    uuid.UUID
	Machine  *StageMachine
	Before   map[string]any
	Patch    map[string]any
	Inc      map[string]any
}

// WasmStageApprovalError is returned when the move needs a supervisor: the
// pending ApprovalRequest is ALREADY parked. It unwraps to the transition's
// synthetic *ConstraintError (so the wasm envelope can carry error.policy /
// error.context exactly like a guard that requests approval) and to the typed
// *ApprovalRequiredError (so the envelope can carry approval_request_id).
type WasmStageApprovalError struct {
	Constraint *ConstraintError
	Required   *ApprovalRequiredError
}

func (e *WasmStageApprovalError) Error() string { return e.Required.Error() }

// Unwrap exposes both typed errors to errors.As / errors.Is.
func (e *WasmStageApprovalError) Unwrap() []error { return []error{e.Constraint, e.Required} }

// EnforceWasmStageWrite applies, to a wasm update/delete, the per-transition
// policy and the locked-stage rule Service.Update enforces. It runs inside the
// guest's transaction, after the from -> to whitelist (GateWrite) passed.
//
// Actor and roles. The guest runs with the actor of the context
// (dynamic.WithActorID: the user who invoked the action or whose event is
// being delivered). Its roles are resolved with the SAME machinery as the REST
// path: Config.ApprovalRequesterResolver loads the principal and
// principalRoles (JWT role, RolesProvider, Config.ActorRolesResolver) lists
// them. No actor, or the SystemActorID, is the SYSTEM principal; a principal
// that cannot be resolved has no roles. Both fail CLOSED on a `roles`-gated
// transition: unlike Service.Update's NewSystemCaller (trusted host code), a
// wasm guest is addon code, so "system" is not a license to skip roles. The
// explicit way for an addon to drive a roles/approval transition is
// Action.stage_authority (see coversTransition); an approved replay of the
// very same transition also passes (ApprovalReplay).
//
// Approval. A move with an `approval` block is PARKED as the same
// ApprovalRequest the REST path creates (constraint key
// `transition:<from>-><to>`) and the call returns *WasmStageApprovalError. The
// guest's whole transaction is rolled back by the caller, so nothing is applied
// twice: on approval the kernel replays the row patch through Service.Update as
// the original requester. The system principal does NOT bypass approval.
//
// Modes: off = no-op; warn = log `stage_policy.wasm_bypass` and return nil;
// enforce = return the refusal.
func (s *Service) EnforceWasmStageWrite(ctx context.Context, w WasmStageWrite) error {
	mode := s.WasmStagePolicy()
	if mode == WasmStagePolicyOff || !w.Machine.active() || w.Before == nil {
		return nil
	}
	model := w.Model
	if model == "" {
		model = w.Table
	}

	pending, err := s.evalWasmStageWrite(ctx, model, w)
	if err == nil && pending == nil {
		return nil
	}
	if mode == WasmStagePolicyWarn {
		reason := "approval_required"
		if err != nil {
			reason = err.Error()
		}
		from, to := wasmMove(w)
		log.Printf("dynamic: stage_policy.wasm_bypass addon=%s model=%s op=%s record=%s move=%s->%s reason=%q (set Config.WasmStagePolicy=enforce to refuse it)",
			w.AddonKey, model, w.Op, w.RecordID, from, to, reason)
		return nil
	}
	if err != nil {
		return err
	}
	return s.parkWasmTransition(ctx, model, w, pending)
}

// wasmMove returns the stage move of the write ("", "" when it is not one).
func wasmMove(w WasmStageWrite) (from, to string) {
	if w.Machine == nil || w.Before == nil {
		return "", ""
	}
	from = stringifyStatus(w.Before[w.Machine.Field])
	to = from
	if v, ok := w.Patch[w.Machine.Field]; ok {
		to = stringifyStatus(v)
	}
	return from, to
}

// foldInc turns the guest's numeric deltas into absolute values over before,
// so the policy checks (lock, requires_fields, guard) and a parked replay see
// the row as it will be.
func foldInc(before, patch, inc map[string]any) map[string]any {
	out := make(map[string]any, len(patch)+len(inc))
	for k, v := range patch {
		out[k] = v
	}
	for k, d := range inc {
		out[k] = numericValue(before[k]) + numericValue(d)
	}
	return out
}

// evalWasmStageWrite computes the refusal (err) or the approval the move needs
// (pending) without side effects.
func (s *Service) evalWasmStageWrite(ctx context.Context, model string, w WasmStageWrite) (pending *ConstraintError, err error) {
	sm := w.Machine
	if w.Op == "delete" {
		return nil, CheckStageLockDelete(sm, w.Before)
	}
	input := foldInc(w.Before, w.Patch, w.Inc)
	if err := CheckStageLock(sm, w.Before, input); err != nil {
		return nil, err
	}
	from, to := wasmMove(w)
	if from == to {
		return nil, nil
	}
	merged := make(map[string]any, len(w.Before)+len(input))
	for k, v := range w.Before {
		merged[k] = v
	}
	for k, v := range input {
		merged[k] = v
	}

	// Authority: an approved replay of THIS move, or the running action being
	// its declared authority, stands in for roles and approval. Nothing else.
	authority := coversTransition(ctx, model, from, to)
	if r, ok := ApprovalReplayFromContext(ctx); ok && r.skipsConstraint(model, transitionKey(from, to)) {
		authority = true
	}

	var roles []string
	if !authority {
		roles = s.wasmActorRoles(ctx, w.OrgID)
	}
	if err := CheckTransitionPolicy(sm, from, to, merged, roles, authority); err != nil {
		return nil, err
	}
	if authority {
		return nil, nil
	}
	return transitionApproval(ctx, model, sm, from, to, merged), nil
}

// wasmActorPrincipal resolves the acting user of a wasm invocation. nil means
// the system principal (no actor, SystemActorID, unparsable id).
func (s *Service) wasmActorPrincipal(ctx context.Context, orgID uuid.UUID) modelbase.AuthUser {
	id, err := uuid.Parse(ActorIDFromContext(ctx))
	if err != nil || id == uuid.Nil || id == SystemActorID {
		return nil
	}
	if s.approvalRequesterResolver != nil {
		if u := s.approvalRequesterResolver(ctx, orgID, id); u != nil {
			return u
		}
	}
	return &approvalActor{id: id, org: orgID}
}

// wasmActorRoles lists the roles of the wasm actor; nil for the system
// principal or an unresolvable one (fail closed on roles-gated moves).
func (s *Service) wasmActorRoles(ctx context.Context, orgID uuid.UUID) []string {
	u := s.wasmActorPrincipal(ctx, orgID)
	if u == nil {
		return nil
	}
	return s.principalRoles(ctx, u)
}

// parkWasmTransition persists the pending ApprovalRequest of a wasm move (same
// kind, key, label, roles and payload shape as the REST path, so the approver
// inbox, expiry, approval.requested event and the ApprovalOpUpdate replay work
// unchanged) and returns *WasmStageApprovalError. A request already pending for
// the same record, transition and requester is reused: a guest that retries the
// same action does not stack requests.
func (s *Service) parkWasmTransition(ctx context.Context, model string, w WasmStageWrite, ce *ConstraintError) error {
	if err := s.ensureApprovalTable(); err != nil {
		return err
	}
	actor := SystemActorID
	role := SystemRole
	if u := s.wasmActorPrincipal(ctx, w.OrgID); u != nil {
		actor, role = u.GetID(), u.GetRole()
	}
	var existing ApprovalRequest
	err := s.db.WithContext(ctx).
		Where("organization_id = ? AND record_id = ? AND constraint_key = ? AND requested_by = ? AND kind = ? AND status = ?",
			w.OrgID, w.RecordID, ce.ErrorKey, actor, ApprovalKindConstraint, ApprovalStatusPending).
		Order("requested_at DESC").First(&existing).Error
	if err == nil {
		return wasmApprovalErr(ce, &existing)
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}

	pol := ce.Def.Approval
	req, err := s.RequestApproval(ctx, ApprovalInput{
		Kind:            ApprovalKindConstraint,
		AddonKey:        s.approvalAddonKey(ctx, model),
		ModelKey:        s.approvalModelKey(ctx, model),
		Model:           model,
		RecordID:        w.RecordID,
		ConstraintKey:   ce.ErrorKey,
		Label:           pol.Label,
		OrgID:           w.OrgID,
		RequestedBy:     actor,
		RequestedByRole: role,
		Roles:           pol.Roles,
		ReasonRequired:  pol.ReasonRequired,
		ExpiresHours:    pol.ExpiresHours,
		Payload: map[string]any{
			"op":        ApprovalOpUpdate,
			"model":     model,
			"record_id": w.RecordID,
			"input":     foldInc(w.Before, w.Patch, w.Inc),
			"source":    "wasm:" + w.AddonKey,
		},
		Snapshot:  w.Before,
		Violation: ce.ViolationMap(),
	})
	if err != nil {
		return err
	}
	return wasmApprovalErr(ce, req)
}

func wasmApprovalErr(ce *ConstraintError, req *ApprovalRequest) error {
	return &WasmStageApprovalError{Constraint: ce, Required: &ApprovalRequiredError{Request: req}}
}
