package dynamic

// approvals.go — the generic APPROVALS primitive.
//
// A declarative guard (manifest v3 Column.constraints[]) may declare
// `on_violation: "request_approval"` + `approval: {roles, …}`, and an action
// may declare `approval: {when?, roles, …}`. When the guard fails (or the
// supervised action is invoked), the kernel does NOT write: it parks the whole
// pending mutation as an ApprovalRequest (status pending) in its own
// `approval_requests` table, emits the canonical `approval.requested` event and
// answers the caller with `success:false`, `error.code = "approval_required"`
// and `meta.approval_request_id`. A user holding one of the approver roles
// later calls ApproveRequest, which re-applies the SAME mutation on behalf of
// the original requester skipping ONLY the constraint that raised it (every
// other guard still runs, row locking still applies), or RejectRequest.
//
// Mutation kinds are pluggable: the stored payload carries an `op`
// discriminator and a matching ApprovalApplier replays it. The kernel ships
// the `create` / `update` / `action` appliers (Service.Create / Update /
// ExecAction); the wasm runtime registers `data_mutate` / `data_batch` /
// `wasm_callback` (docs/wasm-abi.md § 19); hosts may override any of them.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/manifest/computeexpr"
	"github.com/asteby/metacore-kernel/modelbase"
)

// Approval request lifecycle. pending → approved → applied | failed;
// pending → rejected; pending → expired. Terminal: applied, failed, rejected,
// expired. `approved` is transient (the apply runs right after the decision)
// but is persisted so a crash mid-apply is visible.
const (
	ApprovalStatusPending  = "pending"
	ApprovalStatusApproved = "approved"
	ApprovalStatusRejected = "rejected"
	ApprovalStatusExpired  = "expired"
	ApprovalStatusApplied  = "applied"
	ApprovalStatusFailed   = "failed"
)

// Approval request kinds — what raised the request.
const (
	ApprovalKindConstraint = "constraint" // a column guard with on_violation: request_approval
	ApprovalKindAction     = "action"     // a supervised action (Action.approval)
	ApprovalKindExplicit   = "explicit"   // a wasm handler calling approval_request
)

// Payload `op` discriminators the kernel ships appliers for. Hosts and the
// wasm runtime register more (see RegisterApprovalApplier).
const (
	ApprovalOpCreate = "create"
	ApprovalOpUpdate = "update"
	ApprovalOpAction = "action"
)

// ApprovalRequiredCode is the stable, documented `error.code` a caller gets
// when its mutation was parked for approval instead of being applied.
const ApprovalRequiredCode = "approval_required"

// ApprovalModelKey is the canonical model segment of the approval events
// (`kernel.ApprovalRequest.<action>` in CanonicalEvent terms) and the name the
// core model registers under for hosts that project it into their dynamic UI.
const ApprovalModelKey = "ApprovalRequest"

// ApprovalEventPrefix namespaces the bus events this primitive emits:
// approval.requested | approval.approved | approval.rejected |
// approval.expired | approval.applied | approval.failed.
const ApprovalEventPrefix = "approval."

// ApprovalRequest is the kernel-owned, org-scoped core model that stores one
// pending mutation awaiting a supervisor's decision. jsonb columns are kept as
// JSON strings (cross-dialect: Postgres jsonb, SQLite text); View() parses them
// for API consumers.
type ApprovalRequest struct {
	ID             uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	OrganizationID uuid.UUID `gorm:"type:uuid;not null;index" json:"organization_id"`
	// AddonKey is the addon owning the model / action (or the calling addon
	// for explicit wasm requests); "kernel" for core models.
	AddonKey string `gorm:"size:100;index" json:"addon_key"`
	// ModelKey is the canonical manifest ModelKey the mutation targets.
	ModelKey string `gorm:"size:200;index" json:"model_key"`
	// RecordID is the target row for update/action requests; "" on create.
	RecordID string `gorm:"size:64;index" json:"record_id"`
	// ActionKey is set for kind=action (the supervised action key).
	ActionKey string `gorm:"size:200" json:"action_key"`
	// ConstraintKey is the ErrorKey of the guard that raised a kind=constraint
	// request — the ONE guard the replay skips.
	ConstraintKey string `gorm:"size:200" json:"constraint_key"`
	// Kind is constraint | action | explicit.
	Kind string `gorm:"size:20;index" json:"kind"`
	// Label is the human/i18n title for the inbox card.
	Label string `gorm:"size:255" json:"label"`
	// Status follows the lifecycle documented on the constants above.
	Status string `gorm:"size:20;index;default:'pending'" json:"status"`

	RequestedBy     uuid.UUID  `gorm:"type:uuid;index" json:"requested_by"`
	RequestedByRole string     `gorm:"size:100" json:"requested_by_role"`
	RequestedAt     time.Time  `json:"requested_at"`
	ExpiresAt       *time.Time `gorm:"index" json:"expires_at"`

	// Roles is the JSON array of approver role keys copied from the policy at
	// request time (so a later manifest change never re-opens who may decide).
	Roles          string `gorm:"type:jsonb" json:"roles"`
	ReasonRequired bool   `json:"reason_required"`

	// Payload is the complete pending mutation ({op, model, record_id, input |
	// payload | request …}) the matching ApprovalApplier replays on approve.
	Payload string `gorm:"type:jsonb" json:"payload"`
	// Snapshot is the target row BEFORE the mutation (update/action); "" on create.
	Snapshot string `gorm:"type:jsonb" json:"snapshot"`
	// Violation is {expr, error_key, values} of the guard that raised the
	// request (kind=constraint), so the inbox shows WHY without re-evaluating.
	Violation string `gorm:"type:jsonb" json:"violation"`

	DecidedBy *uuid.UUID `gorm:"type:uuid" json:"decided_by"`
	DecidedAt *time.Time `json:"decided_at"`
	Reason    string     `gorm:"type:text" json:"reason"`

	// AppliedEventID / Result / Error describe the outcome of the replay.
	AppliedEventID string `gorm:"size:64" json:"applied_event_id"`
	Result         string `gorm:"type:jsonb" json:"result"`
	Error          string `gorm:"type:text" json:"error"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName pins the physical table (host schema).
func (ApprovalRequest) TableName() string { return "approval_requests" }

// RoleList parses the stored approver roles.
func (r *ApprovalRequest) RoleList() []string {
	var out []string
	if r.Roles != "" {
		_ = json.Unmarshal([]byte(r.Roles), &out)
	}
	return out
}

// PayloadMap parses the stored pending mutation.
func (r *ApprovalRequest) PayloadMap() map[string]any {
	out := map[string]any{}
	if r.Payload != "" {
		_ = json.Unmarshal([]byte(r.Payload), &out)
	}
	return out
}

// Op returns the payload `op` discriminator that selects the applier.
func (r *ApprovalRequest) Op() string {
	op, _ := r.PayloadMap()["op"].(string)
	return op
}

// IsExpired reports whether a pending request is past its ExpiresAt.
func (r *ApprovalRequest) IsExpired(now time.Time) bool {
	return r.Status == ApprovalStatusPending && r.ExpiresAt != nil && !now.Before(*r.ExpiresAt)
}

// ApprovalRequestView is the API projection of an ApprovalRequest: jsonb
// columns parsed, plus CanDecide (whether the viewing actor may approve /
// reject it right now).
type ApprovalRequestView struct {
	ID              uuid.UUID      `json:"id"`
	OrganizationID  uuid.UUID      `json:"organization_id"`
	AddonKey        string         `json:"addon_key"`
	ModelKey        string         `json:"model_key"`
	RecordID        string         `json:"record_id,omitempty"`
	ActionKey       string         `json:"action_key,omitempty"`
	ConstraintKey   string         `json:"constraint_key,omitempty"`
	Kind            string         `json:"kind"`
	Label           string         `json:"label"`
	Status          string         `json:"status"`
	RequestedBy     uuid.UUID      `json:"requested_by"`
	RequestedByRole string         `json:"requested_by_role,omitempty"`
	RequestedAt     time.Time      `json:"requested_at"`
	ExpiresAt       *time.Time     `json:"expires_at,omitempty"`
	Roles           []string       `json:"roles"`
	ReasonRequired  bool           `json:"reason_required"`
	Payload         map[string]any `json:"payload"`
	Snapshot        map[string]any `json:"snapshot,omitempty"`
	Violation       map[string]any `json:"violation,omitempty"`
	DecidedBy       *uuid.UUID     `json:"decided_by,omitempty"`
	DecidedAt       *time.Time     `json:"decided_at,omitempty"`
	Reason          string         `json:"reason,omitempty"`
	AppliedEventID  string         `json:"applied_event_id,omitempty"`
	Result          any            `json:"result,omitempty"`
	Error           string         `json:"error,omitempty"`
	CanDecide       bool           `json:"can_decide"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
}

// View projects the row for API consumers. canDecide is computed by the caller
// (it depends on the viewing actor's roles).
func (r *ApprovalRequest) View(canDecide bool) ApprovalRequestView {
	v := ApprovalRequestView{
		ID: r.ID, OrganizationID: r.OrganizationID, AddonKey: r.AddonKey, ModelKey: r.ModelKey,
		RecordID: r.RecordID, ActionKey: r.ActionKey, ConstraintKey: r.ConstraintKey, Kind: r.Kind,
		Label: r.Label, Status: r.Status, RequestedBy: r.RequestedBy, RequestedByRole: r.RequestedByRole,
		RequestedAt: r.RequestedAt, ExpiresAt: r.ExpiresAt, Roles: r.RoleList(), ReasonRequired: r.ReasonRequired,
		Payload: r.PayloadMap(), DecidedBy: r.DecidedBy, DecidedAt: r.DecidedAt, Reason: r.Reason,
		AppliedEventID: r.AppliedEventID, Error: r.Error, CanDecide: canDecide,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
	if v.Roles == nil {
		v.Roles = []string{}
	}
	if r.Snapshot != "" {
		_ = json.Unmarshal([]byte(r.Snapshot), &v.Snapshot)
	}
	if r.Violation != "" {
		_ = json.Unmarshal([]byte(r.Violation), &v.Violation)
	}
	if r.Result != "" {
		var res any
		if json.Unmarshal([]byte(r.Result), &res) == nil {
			v.Result = res
		}
	}
	return v
}

// ToMap is the View as a generic map (the shape carried in canonical events).
func (r *ApprovalRequest) ToMap(canDecide bool) map[string]any {
	b, _ := json.Marshal(r.View(canDecide))
	out := map[string]any{}
	_ = json.Unmarshal(b, &out)
	return out
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

var (
	// ErrApprovalRequired is the sentinel wrapped by *ApprovalRequiredError: the
	// mutation was NOT applied; it now waits as a pending ApprovalRequest. The
	// handler maps it to HTTP 409 + error.code "approval_required".
	ErrApprovalRequired = errors.New("approval required")
	// ErrApprovalNotPending is returned when deciding a request that is no
	// longer pending (already decided / expired). HTTP 409.
	ErrApprovalNotPending = errors.New("approval request is not pending")
	// ErrApprovalExpired is returned when deciding a request past its expiry
	// (the request is marked expired as a side effect). HTTP 409.
	ErrApprovalExpired = errors.New("approval request has expired")
	// ErrApprovalReasonRequired is returned when the policy demands a reason and
	// the decision carries none. HTTP 422.
	ErrApprovalReasonRequired = errors.New("a reason is required for this decision")
	// ErrApprovalForbidden wraps ErrForbidden: the actor holds none of the
	// approver roles. HTTP 403.
	ErrApprovalForbidden = fmt.Errorf("%w: actor is not an approver for this request", ErrForbidden)
	// ErrApprovalApplierMissing means no applier is registered for the stored
	// payload op (a host wiring gap). The request is marked failed.
	ErrApprovalApplierMissing = errors.New("no approval applier registered for this mutation kind")
)

// ApprovalRequiredError is the typed error Create / Update / ExecAction return
// when the mutation was parked for approval. It wraps ErrApprovalRequired and
// carries the freshly-created request so handlers can answer with its id.
type ApprovalRequiredError struct {
	Request *ApprovalRequest
}

func (e *ApprovalRequiredError) Error() string {
	if e == nil || e.Request == nil {
		return ErrApprovalRequired.Error()
	}
	return fmt.Sprintf("approval required (%s): request %s is pending", e.Request.Label, e.Request.ID)
}

// Unwrap ties the typed error to ErrApprovalRequired for errors.Is routing.
func (e *ApprovalRequiredError) Unwrap() error { return ErrApprovalRequired }

// Meta is the `meta` block a handler merges into the approval_required
// envelope: {approval_required, approval_request_id, approval_roles,
// approval_label, approval_expires_at}.
func (e *ApprovalRequiredError) Meta() map[string]any {
	if e == nil || e.Request == nil {
		return map[string]any{"approval_required": true}
	}
	m := map[string]any{
		"approval_required":   true,
		"approval_request_id": e.Request.ID.String(),
		"approval_roles":      e.Request.RoleList(),
		"approval_label":      e.Request.Label,
		"approval_kind":       e.Request.Kind,
	}
	if e.Request.ExpiresAt != nil {
		m["approval_expires_at"] = e.Request.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return m
}

// ApprovalApplyError is returned by ApproveRequest when the decision was
// recorded but the replay of the mutation failed (status = failed). The
// request is returned alongside so the caller can show the stored error.
type ApprovalApplyError struct {
	Request *ApprovalRequest
	Err     error
}

func (e *ApprovalApplyError) Error() string {
	return fmt.Sprintf("approval applied with error: %v", e.Err)
}

func (e *ApprovalApplyError) Unwrap() error { return e.Err }

// ---------------------------------------------------------------------------
// Replay context
// ---------------------------------------------------------------------------

// ApprovalReplay is the marker ApproveRequest attaches to the context of the
// re-applied mutation. Create / Update / ExecAction read it to (a) skip the
// permission check — the request was authorized when it was raised and the
// approver was authorized by role — and (b) skip EXACTLY the guard / action
// approval that raised the request. Every other constraint still runs.
type ApprovalReplay struct {
	RequestID     uuid.UUID
	Kind          string
	Model         string // the model name as originally invoked (table or ModelKey)
	ModelKey      string // the canonical ModelKey
	ConstraintKey string // kind=constraint: the ErrorKey to skip
	ActionKey     string // kind=action: the action whose approval gate to skip
}

type approvalReplayKey struct{}

// WithApprovalReplay returns a child context carrying the replay marker.
func WithApprovalReplay(ctx context.Context, r ApprovalReplay) context.Context {
	return context.WithValue(ctx, approvalReplayKey{}, r)
}

// ApprovalReplayFromContext reads the replay marker, if any.
func ApprovalReplayFromContext(ctx context.Context) (ApprovalReplay, bool) {
	r, ok := ctx.Value(approvalReplayKey{}).(ApprovalReplay)
	return r, ok
}

// replayMatchesModel reports whether the replay targets `model` (either the
// raw invocation name or the canonical key). An empty model on the replay
// matches everything (explicit wasm requests carry no model).
func (r ApprovalReplay) matchesModel(model string) bool {
	if r.Model == "" && r.ModelKey == "" {
		return true
	}
	return strings.EqualFold(r.Model, model) || strings.EqualFold(r.ModelKey, model)
}

// skipsConstraint reports whether the replay approved this exact guard.
func (r ApprovalReplay) skipsConstraint(model, errorKey string) bool {
	return r.Kind == ApprovalKindConstraint && r.ConstraintKey != "" && r.ConstraintKey == errorKey && r.matchesModel(model)
}

// skipsActionApproval reports whether the replay approved this exact action.
func (r ApprovalReplay) skipsActionApproval(model, actionKey string) bool {
	return r.Kind == ApprovalKindAction && r.ActionKey == actionKey && r.matchesModel(model)
}

// approvalReplayActive reports whether ctx carries a replay marker (used to
// bypass checkPerm on the re-applied mutation).
func approvalReplayActive(ctx context.Context) bool {
	_, ok := ApprovalReplayFromContext(ctx)
	return ok
}

// ---------------------------------------------------------------------------
// Appliers
// ---------------------------------------------------------------------------

// ApprovalApplier replays one stored mutation kind on approve. `actor` is the
// approver; the requester is recovered from req.RequestedBy. It returns the
// replay result (the created/updated row, the action data …) for the request's
// Result column, or an error that marks the request failed.
type ApprovalApplier func(ctx context.Context, svc *Service, req *ApprovalRequest, actor modelbase.AuthUser) (any, error)

// RegisterApprovalApplier binds (or overrides) the applier for a payload op.
// The kernel registers create/update/action at construction; the wasm runtime
// registers data_mutate/data_batch/wasm_callback via Host.WithApprovals; hosts
// may override any of them (e.g. to route `action` through their own dispatch).
func (s *Service) RegisterApprovalApplier(op string, fn ApprovalApplier) {
	s.approvalMu.Lock()
	defer s.approvalMu.Unlock()
	if s.approvalAppliers == nil {
		s.approvalAppliers = map[string]ApprovalApplier{}
	}
	if fn == nil {
		delete(s.approvalAppliers, op)
		return
	}
	s.approvalAppliers[op] = fn
}

func (s *Service) approvalApplier(op string) (ApprovalApplier, bool) {
	s.approvalMu.Lock()
	defer s.approvalMu.Unlock()
	fn, ok := s.approvalAppliers[op]
	return fn, ok && fn != nil
}

// registerBuiltinApprovalAppliers wires the kernel-native replay backends.
func (s *Service) registerBuiltinApprovalAppliers() {
	s.RegisterApprovalApplier(ApprovalOpCreate, applyApprovedCreate)
	s.RegisterApprovalApplier(ApprovalOpUpdate, applyApprovedUpdate)
	s.RegisterApprovalApplier(ApprovalOpAction, applyApprovedAction)
}

func applyApprovedCreate(ctx context.Context, svc *Service, req *ApprovalRequest, _ modelbase.AuthUser) (any, error) {
	p := req.PayloadMap()
	model, _ := p["model"].(string)
	input, _ := p["input"].(map[string]any)
	if model == "" || input == nil {
		return nil, fmt.Errorf("%w: create payload lacks model/input", ErrInvalidInput)
	}
	return svc.Create(ctx, model, svc.approvalRequester(ctx, req), cloneMap(input))
}

func applyApprovedUpdate(ctx context.Context, svc *Service, req *ApprovalRequest, _ modelbase.AuthUser) (any, error) {
	p := req.PayloadMap()
	model, _ := p["model"].(string)
	input, _ := p["input"].(map[string]any)
	idStr, _ := p["record_id"].(string)
	id, err := uuid.Parse(idStr)
	if model == "" || input == nil || err != nil {
		return nil, fmt.Errorf("%w: update payload lacks model/record_id/input", ErrInvalidInput)
	}
	return svc.Update(ctx, model, svc.approvalRequester(ctx, req), id, cloneMap(input))
}

func applyApprovedAction(ctx context.Context, svc *Service, req *ApprovalRequest, _ modelbase.AuthUser) (any, error) {
	p := req.PayloadMap()
	model, _ := p["model"].(string)
	key, _ := p["action_key"].(string)
	payload, _ := p["payload"].(map[string]any)
	idStr, _ := p["record_id"].(string)
	id, err := uuid.Parse(idStr)
	if model == "" || key == "" || err != nil {
		return nil, fmt.Errorf("%w: action payload lacks model/action_key/record_id", ErrInvalidInput)
	}
	if payload == nil {
		payload = map[string]any{}
	}
	res, err := svc.ExecAction(ctx, model, svc.approvalRequester(ctx, req), id, key, cloneMap(payload))
	if err != nil {
		return nil, err
	}
	if !res.Success {
		msg := "action declined"
		if res.Error != nil {
			msg = res.Error.Error()
		}
		return res.Data, fmt.Errorf("%s", msg)
	}
	return res.Data, nil
}

// approvalActor is the synthetic principal the replay runs as when the host
// wires no RequesterResolver: the ORIGINAL requester, so created_by / actor
// attribution stays truthful (the seller made the sale; the manager approved
// it — recorded on the request itself).
type approvalActor struct {
	id   uuid.UUID
	org  uuid.UUID
	role string
}

func (a *approvalActor) GetID() uuid.UUID             { return a.id }
func (a *approvalActor) GetOrganizationID() uuid.UUID { return a.org }
func (a *approvalActor) GetEmail() string             { return "" }
func (a *approvalActor) GetRole() string              { return a.role }
func (a *approvalActor) GetPasswordHash() string      { return "" }
func (a *approvalActor) SetEmail(string)              {}
func (a *approvalActor) SetName(string)               {}
func (a *approvalActor) SetPasswordHash(string)       {}
func (a *approvalActor) SetRole(v string)             { a.role = v }
func (a *approvalActor) SetOrganizationID(v uuid.UUID) { a.org = v }

// approvalRequester resolves the principal the replay runs as.
func (s *Service) approvalRequester(ctx context.Context, req *ApprovalRequest) modelbase.AuthUser {
	if s.approvalRequesterResolver != nil {
		if u := s.approvalRequesterResolver(ctx, req.OrganizationID, req.RequestedBy); u != nil {
			return u
		}
	}
	return &approvalActor{id: req.RequestedBy, org: req.OrganizationID, role: req.RequestedByRole}
}

// ---------------------------------------------------------------------------
// Table + helpers
// ---------------------------------------------------------------------------

var (
	approvalMu       sync.Mutex
	approvalMigrated = map[*gorm.DB]bool{}
)

// MigrateApprovals creates / updates the approval_requests table. Idempotent;
// Service calls it lazily on first use, hosts may call it eagerly at boot.
func MigrateApprovals(db *gorm.DB) error {
	approvalMu.Lock()
	defer approvalMu.Unlock()
	if approvalMigrated[db] {
		return nil
	}
	if err := db.AutoMigrate(&ApprovalRequest{}); err != nil {
		return fmt.Errorf("migrate approval_requests: %w", err)
	}
	approvalMigrated[db] = true
	return nil
}

func (s *Service) ensureApprovalTable() error { return MigrateApprovals(s.db) }

// ApprovalInput is the request-creation contract shared by the constraint /
// action paths and the wasm `approval_request` import.
type ApprovalInput struct {
	Kind            string
	AddonKey        string
	ModelKey        string
	Model           string // as invoked (table or ModelKey); stored in the payload
	RecordID        string
	ActionKey       string
	ConstraintKey   string
	Label           string
	OrgID           uuid.UUID
	RequestedBy     uuid.UUID
	RequestedByRole string
	Roles           []string
	ReasonRequired  bool
	ExpiresHours    int
	// Payload MUST carry "op" — the applier discriminator.
	Payload   map[string]any
	Snapshot  map[string]any
	Violation map[string]any
}

// RequestApproval persists a pending ApprovalRequest from an ApprovalInput and
// emits `approval.requested`. It is the single creation path (constraint /
// action / explicit). Validation: org + op + at least one role.
func (s *Service) RequestApproval(ctx context.Context, in ApprovalInput) (*ApprovalRequest, error) {
	if in.OrgID == uuid.Nil {
		return nil, fmt.Errorf("%w: approval request requires an organization", ErrInvalidInput)
	}
	op, _ := in.Payload["op"].(string)
	if strings.TrimSpace(op) == "" {
		return nil, fmt.Errorf("%w: approval payload requires an op", ErrInvalidInput)
	}
	roles := normalizeRoles(in.Roles)
	if len(roles) == 0 {
		return nil, fmt.Errorf("%w: approval request requires at least one approver role", ErrInvalidInput)
	}
	if err := s.ensureApprovalTable(); err != nil {
		return nil, err
	}
	kind := in.Kind
	if kind == "" {
		kind = ApprovalKindExplicit
	}
	label := strings.TrimSpace(in.Label)
	if label == "" {
		switch {
		case in.ConstraintKey != "":
			label = in.ConstraintKey
		case in.ActionKey != "":
			label = in.ActionKey
		default:
			label = in.ModelKey
		}
	}
	now := time.Now().UTC()
	req := &ApprovalRequest{
		ID:              uuid.New(),
		OrganizationID:  in.OrgID,
		AddonKey:        in.AddonKey,
		ModelKey:        in.ModelKey,
		RecordID:        in.RecordID,
		ActionKey:       in.ActionKey,
		ConstraintKey:   in.ConstraintKey,
		Kind:            kind,
		Label:           label,
		Status:          ApprovalStatusPending,
		RequestedBy:     in.RequestedBy,
		RequestedByRole: in.RequestedByRole,
		RequestedAt:     now,
		Roles:           mustJSON(roles),
		ReasonRequired:  in.ReasonRequired,
		Payload:         mustJSON(in.Payload),
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if in.ExpiresHours > 0 {
		exp := now.Add(time.Duration(in.ExpiresHours) * time.Hour)
		req.ExpiresAt = &exp
	}
	if in.Snapshot != nil {
		req.Snapshot = mustJSON(in.Snapshot)
	}
	if in.Violation != nil {
		req.Violation = mustJSON(in.Violation)
	}
	if err := s.db.WithContext(ctx).Create(req).Error; err != nil {
		return nil, fmt.Errorf("dynamic: approval request: %w", err)
	}
	s.publishApprovalEvent(ctx, "requested", req, nil)
	return req, nil
}

// openConstraintApproval turns a guard violation flagged request_approval into
// a pending request for a create/update mutation. `input` is the ORIGINAL
// caller input (pre hooks / coercion) so the replay runs the exact same call.
func (s *Service) openConstraintApproval(ctx context.Context, model string, user modelbase.AuthUser, op, recordID string, input, before map[string]any, ce *ConstraintError) error {
	pol := ce.Def.Approval
	payload := map[string]any{
		"op":    op,
		"model": model,
		"input": input,
	}
	if recordID != "" {
		payload["record_id"] = recordID
	}
	req, err := s.RequestApproval(ctx, ApprovalInput{
		Kind:            ApprovalKindConstraint,
		AddonKey:        s.approvalAddonKey(ctx, model),
		ModelKey:        s.approvalModelKey(ctx, model),
		Model:           model,
		RecordID:        recordID,
		ConstraintKey:   ce.ErrorKey,
		Label:           pol.Label,
		OrgID:           orgIDFromUser(user),
		RequestedBy:     user.GetID(),
		RequestedByRole: user.GetRole(),
		Roles:           pol.Roles,
		ReasonRequired:  pol.ReasonRequired,
		ExpiresHours:    pol.ExpiresHours,
		Payload:         payload,
		Snapshot:        before,
		Violation:       ce.ViolationMap(),
	})
	if err != nil {
		return err
	}
	return &ApprovalRequiredError{Request: req}
}

// openActionApproval parks a supervised action invocation.
func (s *Service) openActionApproval(ctx context.Context, model string, user modelbase.AuthUser, id uuid.UUID, key string, def *manifest.ActionDef, row, payload map[string]any) (*ApprovalRequest, error) {
	pol := def.Approval
	label := pol.Label
	if label == "" {
		label = def.Label
	}
	return s.RequestApproval(ctx, ApprovalInput{
		Kind:            ApprovalKindAction,
		AddonKey:        s.approvalAddonKey(ctx, model),
		ModelKey:        s.approvalModelKey(ctx, model),
		Model:           model,
		RecordID:        id.String(),
		ActionKey:       key,
		Label:           label,
		OrgID:           orgIDFromUser(user),
		RequestedBy:     user.GetID(),
		RequestedByRole: user.GetRole(),
		Roles:           pol.Roles,
		ReasonRequired:  pol.ReasonRequired,
		ExpiresHours:    pol.ExpiresHours,
		Payload: map[string]any{
			"op":         ApprovalOpAction,
			"model":      model,
			"action_key": key,
			"record_id":  id.String(),
			"payload":    payload,
		},
		Snapshot: row,
		Violation: map[string]any{
			"when":   pol.When,
			"values": exprValues(pol.When, mergedNumericEnv(row, payload)),
		},
	})
}

// actionNeedsApproval decides whether this invocation must be parked: the
// action declares an approval policy, the replay marker does not already cover
// it, and `when` (if any) evaluates TRUE against the merged record ∪ payload
// numeric environment. A malformed `when` fails closed (approval required) and
// is logged — validation rejects it at publish time, so this is defense in depth.
func actionNeedsApproval(ctx context.Context, model, key string, def *manifest.ActionDef, row, payload map[string]any) bool {
	if def == nil || def.Approval == nil {
		return false
	}
	if r, ok := ApprovalReplayFromContext(ctx); ok && r.skipsActionApproval(model, key) {
		return false
	}
	when := strings.TrimSpace(def.Approval.When)
	if when == "" {
		return true
	}
	env := mergedNumericEnv(row, payload)
	ok, err := evalConstraintExpr(when, env)
	if err != nil {
		log.Printf("dynamic: action %s.%s approval.when %q is malformed (%v) — requiring approval", model, key, when, err)
		return true
	}
	return ok
}

func mergedNumericEnv(row, payload map[string]any) map[string]float64 {
	env := make(map[string]float64, len(row)+len(payload))
	for k, v := range row {
		env[k] = computeexpr.ToFloat(v)
	}
	for k, v := range payload {
		env[k] = computeexpr.ToFloat(v)
	}
	return env
}

func (s *Service) approvalAddonKey(ctx context.Context, model string) string {
	if s.addonKeyForModel != nil {
		if k := s.addonKeyForModel(ctx, model); k != "" {
			return k
		}
	}
	return "kernel"
}

func (s *Service) approvalModelKey(ctx context.Context, model string) string {
	if s.modelKeyForModel != nil {
		if mk := s.modelKeyForModel(ctx, model); mk != "" {
			return mk
		}
	}
	return model
}

// ---------------------------------------------------------------------------
// Decisions
// ---------------------------------------------------------------------------

// actorRoles resolves the acting user's org roles through the host resolver
// (falling back to the single AuthUser.GetRole()).
func (s *Service) actorRoles(ctx context.Context, user modelbase.AuthUser) []string {
	if user == nil {
		return nil
	}
	if s.actorRolesResolver != nil {
		if roles := s.actorRolesResolver(ctx, user); len(roles) > 0 {
			return normalizeRoles(roles)
		}
	}
	return normalizeRoles([]string{user.GetRole()})
}

// canDecide reports whether the actor holds one of the request's approver roles.
func (s *Service) canDecide(ctx context.Context, user modelbase.AuthUser, req *ApprovalRequest) bool {
	if req == nil || req.Status != ApprovalStatusPending {
		return false
	}
	return rolesIntersect(s.actorRoles(ctx, user), req.RoleList())
}

// CanDecideApproval is the exported form of canDecide for host handlers.
func (s *Service) CanDecideApproval(ctx context.Context, user modelbase.AuthUser, req *ApprovalRequest) bool {
	return s.canDecide(ctx, user, req)
}

// loadApproval reads one request org-scoped to the actor.
func (s *Service) loadApproval(ctx context.Context, user modelbase.AuthUser, id uuid.UUID) (*ApprovalRequest, error) {
	if err := s.ensureApprovalTable(); err != nil {
		return nil, err
	}
	orgID := orgIDFromUser(user)
	if orgID == uuid.Nil {
		return nil, ErrTenantScopeUnavailable
	}
	var req ApprovalRequest
	err := s.db.WithContext(ctx).Where("id = ? AND organization_id = ?", id, orgID).First(&req).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrRecordNotFound
		}
		return nil, err
	}
	return &req, nil
}

// GetApproval returns one request (org-scoped). A pending request past its
// expiry is expired on read.
func (s *Service) GetApproval(ctx context.Context, user modelbase.AuthUser, id uuid.UUID) (*ApprovalRequest, error) {
	req, err := s.loadApproval(ctx, user, id)
	if err != nil {
		return nil, err
	}
	if req.IsExpired(time.Now().UTC()) {
		s.expireOne(ctx, req)
	}
	return req, nil
}

// ApproveRequest records an approval and re-applies the stored mutation.
// Order of checks: pending → not expired → actor role → reason. The decision
// is persisted with a status-guarded UPDATE so two concurrent approvers cannot
// both win. On replay failure the request is marked failed and an
// *ApprovalApplyError (carrying the request) is returned.
func (s *Service) ApproveRequest(ctx context.Context, actor modelbase.AuthUser, id uuid.UUID, reason string) (*ApprovalRequest, error) {
	req, err := s.decide(ctx, actor, id, reason, ApprovalStatusApproved)
	if err != nil {
		return req, err
	}
	s.publishApprovalEvent(ctx, "approved", req, actor)

	// Replay on behalf of the requester, skipping only what was approved.
	replayCtx := WithApprovalReplay(ctx, ApprovalReplay{
		RequestID:     req.ID,
		Kind:          req.Kind,
		Model:         payloadString(req, "model"),
		ModelKey:      req.ModelKey,
		ConstraintKey: req.ConstraintKey,
		ActionKey:     req.ActionKey,
	})
	if req.RequestedBy != uuid.Nil {
		replayCtx = WithActorID(replayCtx, req.RequestedBy.String())
	}
	applier, ok := s.approvalApplier(req.Op())
	var result any
	var applyErr error
	if !ok {
		applyErr = fmt.Errorf("%w: op %q", ErrApprovalApplierMissing, req.Op())
	} else {
		result, applyErr = applier(replayCtx, s, req, actor)
	}

	now := time.Now().UTC()
	if applyErr != nil {
		req.Status = ApprovalStatusFailed
		req.Error = applyErr.Error()
		req.UpdatedAt = now
		if err := s.db.WithContext(ctx).Model(req).Select("status", "error", "updated_at").Updates(req).Error; err != nil {
			log.Printf("dynamic: approval %s: persist failed status: %v", req.ID, err)
		}
		s.publishApprovalEvent(ctx, "failed", req, actor)
		return req, &ApprovalApplyError{Request: req, Err: applyErr}
	}
	req.Status = ApprovalStatusApplied
	req.Result = mustJSON(result)
	if m, ok := result.(map[string]any); ok {
		if idv, ok := m["id"].(string); ok {
			req.AppliedEventID = idv
			if req.RecordID == "" {
				req.RecordID = idv
			}
		}
	}
	req.UpdatedAt = now
	if err := s.db.WithContext(ctx).Model(req).Select("status", "result", "applied_event_id", "record_id", "updated_at").Updates(req).Error; err != nil {
		log.Printf("dynamic: approval %s: persist applied status: %v", req.ID, err)
	}
	s.publishApprovalEvent(ctx, "applied", req, actor)
	return req, nil
}

// RejectRequest records a rejection. Same gate as ApproveRequest; nothing is
// replayed.
func (s *Service) RejectRequest(ctx context.Context, actor modelbase.AuthUser, id uuid.UUID, reason string) (*ApprovalRequest, error) {
	req, err := s.decide(ctx, actor, id, reason, ApprovalStatusRejected)
	if err != nil {
		return req, err
	}
	s.publishApprovalEvent(ctx, "rejected", req, actor)
	return req, nil
}

func (s *Service) decide(ctx context.Context, actor modelbase.AuthUser, id uuid.UUID, reason, status string) (*ApprovalRequest, error) {
	if actor == nil {
		return nil, ErrForbidden
	}
	req, err := s.loadApproval(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if req.Status != ApprovalStatusPending {
		return req, ErrApprovalNotPending
	}
	if req.IsExpired(now) {
		s.expireOne(ctx, req)
		return req, ErrApprovalExpired
	}
	if !s.canDecide(ctx, actor, req) {
		return req, ErrApprovalForbidden
	}
	reason = strings.TrimSpace(reason)
	if req.ReasonRequired && reason == "" {
		return req, ErrApprovalReasonRequired
	}
	actorID := actor.GetID()
	res := s.db.WithContext(ctx).Model(&ApprovalRequest{}).
		Where("id = ? AND organization_id = ? AND status = ?", req.ID, req.OrganizationID, ApprovalStatusPending).
		Updates(map[string]any{
			"status":     status,
			"decided_by": actorID,
			"decided_at": now,
			"reason":     reason,
			"updated_at": now,
		})
	if res.Error != nil {
		return req, res.Error
	}
	if res.RowsAffected == 0 {
		// Lost the race against another approver / the expirer.
		return req, ErrApprovalNotPending
	}
	req.Status = status
	req.DecidedBy = &actorID
	req.DecidedAt = &now
	req.Reason = reason
	req.UpdatedAt = now
	return req, nil
}

// expireOne flips a due pending request to expired (status-guarded) and emits
// the event. Best-effort: a lost race simply means someone else expired or
// decided it first.
func (s *Service) expireOne(ctx context.Context, req *ApprovalRequest) {
	now := time.Now().UTC()
	res := s.db.WithContext(ctx).Model(&ApprovalRequest{}).
		Where("id = ? AND status = ?", req.ID, ApprovalStatusPending).
		Updates(map[string]any{"status": ApprovalStatusExpired, "updated_at": now})
	if res.Error != nil || res.RowsAffected == 0 {
		return
	}
	req.Status = ApprovalStatusExpired
	req.UpdatedAt = now
	s.publishApprovalEvent(ctx, "expired", req, nil)
}

// ExpireApprovals expires every pending request past its ExpiresAt (all orgs)
// and returns how many flipped. Hosts call it from a schedule or rely on
// StartApprovalExpirer; reads also expire lazily.
func (s *Service) ExpireApprovals(ctx context.Context) (int, error) {
	if err := s.ensureApprovalTable(); err != nil {
		return 0, err
	}
	var due []ApprovalRequest
	now := time.Now().UTC()
	if err := s.db.WithContext(ctx).
		Where("status = ? AND expires_at IS NOT NULL AND expires_at <= ?", ApprovalStatusPending, now).
		Limit(500).Find(&due).Error; err != nil {
		return 0, err
	}
	n := 0
	for i := range due {
		before := due[i].Status
		s.expireOne(ctx, &due[i])
		if due[i].Status != before {
			n++
		}
	}
	return n, nil
}

// StartApprovalExpirer runs ExpireApprovals every `interval` until
// StopApprovalExpirer. Idempotent.
func (s *Service) StartApprovalExpirer(interval time.Duration) {
	if s.approvalExpireStop != nil || interval <= 0 {
		return
	}
	stop := make(chan struct{})
	s.approvalExpireStop = stop
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if _, err := s.ExpireApprovals(context.Background()); err != nil {
					log.Printf("dynamic: approval expirer: %v", err)
				}
			}
		}
	}()
}

// StopApprovalExpirer terminates the background expirer.
func (s *Service) StopApprovalExpirer() {
	if s.approvalExpireStop != nil {
		close(s.approvalExpireStop)
		s.approvalExpireStop = nil
	}
}

// ---------------------------------------------------------------------------
// Listing
// ---------------------------------------------------------------------------

// ApprovalListQuery filters ListApprovals. Mine = requested by the actor;
// ForMe = pending requests the actor may decide (role match); Status/Model/
// Kind are exact filters. Page is 1-based; PerPage defaults to 50 (max 200).
type ApprovalListQuery struct {
	Status  string
	Kind    string
	Model   string
	Mine    bool
	ForMe   bool
	Page    int
	PerPage int
}

// ListApprovals returns the actor's org requests newest-first with CanDecide
// resolved per row. Due pending requests are expired first (lazy expiry).
func (s *Service) ListApprovals(ctx context.Context, user modelbase.AuthUser, q ApprovalListQuery) ([]ApprovalRequestView, int64, error) {
	if user == nil {
		return nil, 0, ErrForbidden
	}
	orgID := orgIDFromUser(user)
	if orgID == uuid.Nil {
		return nil, 0, ErrTenantScopeUnavailable
	}
	if err := s.ensureApprovalTable(); err != nil {
		return nil, 0, err
	}
	// Lazy expiry scoped to this org.
	var due []ApprovalRequest
	now := time.Now().UTC()
	_ = s.db.WithContext(ctx).
		Where("organization_id = ? AND status = ? AND expires_at IS NOT NULL AND expires_at <= ?", orgID, ApprovalStatusPending, now).
		Limit(200).Find(&due).Error
	for i := range due {
		s.expireOne(ctx, &due[i])
	}

	db := s.db.WithContext(ctx).Model(&ApprovalRequest{}).Where("organization_id = ?", orgID)
	if q.Status != "" {
		db = db.Where("status = ?", q.Status)
	}
	if q.Kind != "" {
		db = db.Where("kind = ?", q.Kind)
	}
	if q.Model != "" {
		db = db.Where("model_key = ?", q.Model)
	}
	if q.Mine {
		db = db.Where("requested_by = ?", user.GetID())
	}
	if q.ForMe {
		db = db.Where("status = ?", ApprovalStatusPending)
	}
	if q.PerPage <= 0 {
		q.PerPage = 50
	}
	if q.PerPage > 200 {
		q.PerPage = 200
	}
	if q.Page <= 0 {
		q.Page = 1
	}

	roles := s.actorRoles(ctx, user)
	var rows []ApprovalRequest
	var total int64
	if q.ForMe {
		// Role matching happens in Go (jsonb overlap is dialect-specific and the
		// pending set is small): fetch the org's pending rows, filter, paginate.
		if err := db.Order("requested_at DESC").Limit(2000).Find(&rows).Error; err != nil {
			return nil, 0, err
		}
		filtered := rows[:0]
		for i := range rows {
			if rolesIntersect(roles, rows[i].RoleList()) {
				filtered = append(filtered, rows[i])
			}
		}
		total = int64(len(filtered))
		start := (q.Page - 1) * q.PerPage
		if start > len(filtered) {
			start = len(filtered)
		}
		end := start + q.PerPage
		if end > len(filtered) {
			end = len(filtered)
		}
		rows = filtered[start:end]
	} else {
		if err := db.Count(&total).Error; err != nil {
			return nil, 0, err
		}
		if err := db.Order("requested_at DESC").Offset((q.Page - 1) * q.PerPage).Limit(q.PerPage).Find(&rows).Error; err != nil {
			return nil, 0, err
		}
	}
	out := make([]ApprovalRequestView, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		out = append(out, r.View(r.Status == ApprovalStatusPending && rolesIntersect(roles, r.RoleList())))
	}
	return out, total, nil
}

// ApprovalCounts is the inbox badge: pending requests the actor may decide and
// the actor's own pending requests.
type ApprovalCounts struct {
	ForMe int64 `json:"for_me"`
	Mine  int64 `json:"mine"`
}

// CountApprovals computes the badge counts for the actor.
func (s *Service) CountApprovals(ctx context.Context, user modelbase.AuthUser) (ApprovalCounts, error) {
	var c ApprovalCounts
	_, forMe, err := s.ListApprovals(ctx, user, ApprovalListQuery{ForMe: true, PerPage: 1})
	if err != nil {
		return c, err
	}
	_, mine, err := s.ListApprovals(ctx, user, ApprovalListQuery{Mine: true, Status: ApprovalStatusPending, PerPage: 1})
	if err != nil {
		return c, err
	}
	c.ForMe, c.Mine = forMe, mine
	return c, nil
}

// ---------------------------------------------------------------------------
// Events
// ---------------------------------------------------------------------------

// publishApprovalEvent emits `approval.<action>` on the bus (producer "kernel")
// with a *CanonicalEvent payload (Model "ApprovalRequest") so host bridges that
// type-assert CanonicalEvent — activity log, notification rules — consume it
// like any other lifecycle event. After carries the request view.
func (s *Service) publishApprovalEvent(ctx context.Context, action string, req *ApprovalRequest, actor modelbase.AuthUser) {
	if s.bus == nil || req == nil {
		return
	}
	actorID := ""
	switch {
	case actor != nil:
		actorID = actor.GetID().String()
	case req.RequestedBy != uuid.Nil:
		actorID = req.RequestedBy.String()
	}
	payload := &CanonicalEvent{
		ID:            req.ID.String(),
		Model:         ApprovalModelKey,
		Action:        action,
		AddonKey:      "kernel",
		ActorID:       actorID,
		CorrelationID: CorrelationIDFromContext(ctx),
		After:         req.ToMap(false),
	}
	if err := s.bus.Publish(ctx, "kernel", ApprovalEventPrefix+action, req.OrganizationID, payload); err != nil {
		log.Printf("dynamic: publish %s%s: %v", ApprovalEventPrefix, action, err)
	}
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

func normalizeRoles(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, r := range in {
		k := strings.ToLower(strings.TrimSpace(r))
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	return out
}

func rolesIntersect(actor, required []string) bool {
	if len(actor) == 0 || len(required) == 0 {
		return false
	}
	set := make(map[string]bool, len(actor))
	for _, r := range actor {
		set[strings.ToLower(strings.TrimSpace(r))] = true
	}
	for _, r := range required {
		if set[strings.ToLower(strings.TrimSpace(r))] {
			return true
		}
	}
	return false
}

func mustJSON(v any) string {
	if v == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func cloneMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func payloadString(req *ApprovalRequest, key string) string {
	v, _ := req.PayloadMap()[key].(string)
	return v
}

var exprIdentRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

// exprValues picks, from env, the identifiers referenced by expr — the
// "values" block of a stored violation ({unit_price: 80, min_price: 100}).
func exprValues(expr string, env map[string]float64) map[string]any {
	out := map[string]any{}
	for _, id := range exprIdentRe.FindAllString(expr, -1) {
		if v, ok := env[id]; ok {
			out[id] = v
		}
	}
	return out
}
