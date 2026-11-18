package inbound

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// ShadowHeader is set to "true" on every response of a dry-run contract, error
// bodies included, so a caller that reads only the status code can tell that
// nothing was written.
const ShadowHeader = "X-Inbound-Shadow"

// Statuses of a dry run. The receipt of a dry-run event keeps StatusApplied /
// StatusFailed (the lifecycle of the reservation) with Receipt.Shadow = true.
const (
	StatusWouldApply = "would_apply" // the event would have been applied
	StatusWouldFail  = "would_fail"  // the event would have been rejected (ErrorCode)
	StatusIgnored    = "ignored"     // origin loop: the real run ignores it too
)

// writesRecords reports whether the contract needs the Executor to touch rows
// (a model route or a record.* action); a command-only or side-effect-only
// contract does not.
func (c *compiled) writesRecords() bool {
	if len(c.chain) == 0 {
		return c.Route.Command == ""
	}
	for _, a := range c.chain {
		if strings.HasPrefix(a.Do, "record.") {
			return true
		}
	}
	return false
}

// checkDryRun fails registration of a dry-run contract whose Executor cannot
// promise it never writes (fail closed).
func (h *Handler) checkDryRun(c *compiled) error {
	if c.writesRecords() && h.cfg.Executor != nil && !executorDryRuns(h.cfg.Executor) {
		return fmt.Errorf("contract %s v%d: dry_run: %w", c.Key, c.Version, ErrDryRunUnsupported)
	}
	return nil
}

func wouldEffect(do string) string {
	switch do {
	case ActionEventEmit:
		return "would_emit"
	case ActionNotify:
		return "would_notify"
	}
	return "would_enqueue"
}

func effectName(a *cAction) string {
	if a.Do == ActionNotify {
		return a.Template
	}
	return a.Name
}

// applyOutcome fills a finished step from what the executor reported; in a dry
// run it also carries the would_* report.
func (s *ActionStep) applyOutcome(out Outcome) {
	s.Status, s.RecordID, s.Created = StepDone, out.RecordID, out.Created
	s.Affected, s.NotFound = out.Affected, out.NotFound
	s.Noop, s.Reason = out.Noop, out.Reason
	if w := out.Would; w != nil {
		s.Would, s.Target, s.Rows, s.State = w.Would, w.Target, w.Rows, w.State
		s.ParamKeys, s.Changes = w.ParamKeys, w.Changes
		s.OnSimulated = w.OnSimulated
		if w.Noop {
			s.Noop = true
		}
	}
}

// routeDo is the action name of a single-route contract, for its one step.
func routeDo(r Route) string {
	if r.Command != "" {
		return ActionCommandRun
	}
	return "record." + string(r.Op)
}

// routeStep is the single step of a dry-run route contract.
func routeStep(c *compiled, out Outcome) ActionStep {
	step := ActionStep{Index: 0, Do: routeDo(c.Route)}
	if out.Stale {
		step.Status, step.ErrorCode = StepSkipped, CodeStaleEvent
		return step
	}
	step.applyOutcome(out)
	return step
}

// serveShadowRoute closes the receipt of a dry-run route contract: the step
// goes to the receipt (so a replay answers the same result) and the answer says
// nothing was written.
func (h *Handler) serveShadowRoute(w http.ResponseWriter, ctx context.Context, c *compiled, cur Receipt, out Outcome) {
	steps := []ActionStep{routeStep(c, out).withoutFrom()} // never "from": see ColumnChange
	if _, e := h.cfg.Receipts.SaveProgress(context.WithoutCancel(ctx), cur.ID, cur.Attempts, steps); e != nil {
		h.cfg.Logger.Error("inbound: save shadow result", "receipt", cur.ID, "err_type", fmt.Sprintf("%T", e))
	}
	h.finish(ctx, cur, StatusApplied, out.RecordID, "")
	writeJSON(w, http.StatusOK, SuccessBody{Status: StatusWouldApply, ReceiptID: cur.ID.String(), RecordID: out.RecordID,
		Actions: steps, Created: out.Created, Shadow: true, Fidelity: FidelityPartial})
}

// withoutFrom returns the step with the current row value (ColumnChange.From) of
// every column removed. Anything that leaves the Handler for a signed event
// (HTTP answer, receipt, replay) goes through it: the current data of the
// database never travels to the producer nor is persisted in Receipt.Progress.
func (s ActionStep) withoutFrom() ActionStep {
	if len(s.Changes) == 0 {
		return s
	}
	ch := make(map[string]ColumnChange, len(s.Changes))
	for k, c := range s.Changes {
		c.From = nil
		ch[k] = c
	}
	s.Changes = ch
	return s
}

func stepsWithoutFrom(in []ActionStep) []ActionStep {
	if in == nil {
		return nil
	}
	out := make([]ActionStep, len(in))
	for i, s := range in {
		out[i] = s.withoutFrom()
	}
	return out
}

// ------------------------------------------------------- Handler.DryRun ---

// DryRunRequest is one example event for Handler.DryRun (the "probar con un
// evento de ejemplo" of a panel). The caller (the host) has already authorized
// the user for the organization; no signature is checked.
type DryRunRequest struct {
	Org      uuid.UUID
	Contract string
	// Version 0 = the highest registered version.
	Version int
	// Body is the JSON event, exactly as the producer would send it.
	Body []byte
	// Header feeds a {header:X} idempotency template; may be nil.
	Header http.Header
}

// DryRunResult is what the event would do. It is ephemeral: no receipt is kept
// and nothing is reserved, so the same example can be tried again and again.
type DryRunResult struct {
	Shadow bool `json:"shadow"`
	// Fidelity is always FidelityPartial: the report is a forecast, not a full
	// replay of the real run (see FidelityPartial).
	Fidelity string `json:"fidelity"`
	Status   string `json:"status"` // would_apply | would_fail | ignored
	Contract string `json:"contract"`
	Version  int    `json:"version"`
	RecordID string `json:"record_id,omitempty"`
	// Actions is the report per step (a route contract has one).
	Actions []ActionStep `json:"actions,omitempty"`
	// ErrorCode / HTTPStatus / FailedAction describe a would_fail: the code and
	// the status the real run would answer, and the chain action that stopped.
	ErrorCode    string `json:"error_code,omitempty"`
	HTTPStatus   int    `json:"http_status,omitempty"`
	FailedAction *int   `json:"failed_action,omitempty"`
}

// DryRunError is a rejection BEFORE the event could be evaluated (unknown
// contract, malformed body, schema, key or scope); the real run answers the
// same Status and Code.
type DryRunError struct {
	Status  int
	Code    string
	Details []FieldIssue
}

func (e *DryRunError) Error() string { return "inbound: dry run: " + e.Code }

func statusOfKind(k Kind) int {
	switch k {
	case KindInvalid:
		return http.StatusUnprocessableEntity
	case KindConflict:
		return http.StatusConflict
	case KindNotFound:
		return http.StatusNotFound
	case KindForbidden:
		return http.StatusForbidden
	}
	return http.StatusInternalServerError
}

func (r *DryRunResult) fail(err error) {
	var ee *ExecError
	if !errors.As(err, &ee) {
		ee = &ExecError{Kind: KindInternal, Code: "internal", Err: err}
	}
	r.Status, r.ErrorCode, r.HTTPStatus = StatusWouldFail, ee.Code, statusOfKind(ee.Kind)
	if ee.Kind == KindInternal {
		r.ErrorCode = "internal"
	}
}

// DryRun evaluates an example event against a registered contract, in dry-run
// mode whatever the contract's own DryRun says: same schema, key, scope, match,
// column, `when` and order-guard checks as a real event, no signature, no
// receipt, no idempotency reservation, and nothing written or emitted. The
// Executor must implement DryRunSupporter (ErrDryRunUnsupported otherwise).
func (h *Handler) DryRun(ctx context.Context, req DryRunRequest) (*DryRunResult, error) {
	c, ok := h.cfg.Contracts.get(req.Contract, req.Version)
	if !ok {
		return nil, &DryRunError{Status: http.StatusNotFound, Code: CodeUnknownContract}
	}
	if c.writesRecords() && !executorDryRuns(h.cfg.Executor) {
		return nil, ErrDryRunUnsupported
	}
	payload, err := decodeJSON(req.Body)
	if err != nil {
		if errors.Is(err, errNumberRange) {
			return nil, &DryRunError{Status: http.StatusUnprocessableEntity, Code: CodeInvalidSchema,
				Details: []FieldIssue{{Path: "/", Rule: "number_range"}}}
		}
		return nil, &DryRunError{Status: http.StatusBadRequest, Code: CodeMalformedJSON}
	}
	origin, hop, oerr := takeOrigin(payload)
	if oerr != nil {
		return nil, &DryRunError{Status: http.StatusUnprocessableEntity, Code: CodeInvalidOrigin}
	}
	res := &DryRunResult{Shadow: true, Fidelity: FidelityPartial, Contract: c.Key, Version: c.Version}
	if (origin != "" && (origin == OriginPrefix+c.Key || OriginExcluded(origin, c.IgnoreOrigins))) || hop >= h.cfg.MaxHops {
		res.Status = StatusIgnored
		return res, nil
	}
	if verr := c.schema.Validate(payload); verr != nil {
		return nil, &DryRunError{Status: http.StatusUnprocessableEntity, Code: CodeInvalidSchema, Details: schemaIssues(verr)}
	}
	evKey, err := eventKey(c.tmpl, payload, req.Header)
	if err != nil {
		return nil, &DryRunError{Status: http.StatusUnprocessableEntity, Code: CodeMissingEventKey}
	}
	op := Operation{Org: req.Org, Contract: c.Key, EventKey: evKey, Route: c.Route, Values: applyMapping(c.Route, payload),
		CreateDefaults: c.Route.Defaults, Origin: OriginPrefix + c.Key, Hop: hop, DryRun: true, DryRunReveal: true}
	if sc := c.Route.Scope; sc != nil {
		op.Branch = scopeValue(payload, sc.Branch)
		op.Entity = scopeValue(payload, sc.Entity)
		op.RequireBranch = sc.Branch != ""
		op.RequireEntity = sc.Entity != ""
		if (op.RequireBranch && op.Branch == "") || (op.RequireEntity && op.Entity == "") {
			return nil, &DryRunError{Status: http.StatusUnprocessableEntity, Code: CodeScopeRequired}
		}
	}
	if len(c.chain) > 0 && !chainScopeOK(c, payload) {
		return nil, &DryRunError{Status: http.StatusUnprocessableEntity, Code: CodeScopeRequired}
	}

	if len(c.chain) > 0 {
		var steps []ActionStep
		// A zero Receipt makes the run ephemeral: no progress is persisted.
		recID, failedAt, rerr := h.runChain(ctx, c, req.Org, evKey, payload, hop, Receipt{}, &steps, true)
		res.Actions, res.RecordID, res.Status = steps, recID, StatusWouldApply
		if rerr != nil {
			res.fail(rerr)
			res.FailedAction = &failedAt
		}
		return res, nil
	}
	out, xerr := h.execute(ctx, c, op)
	if xerr != nil {
		res.fail(xerr)
		res.Actions = []ActionStep{{Index: 0, Do: routeDo(c.Route), Status: StepFailed, ErrorCode: res.ErrorCode}}
		return res, nil
	}
	step := routeStep(c, out)
	res.Status, res.RecordID, res.Actions = StatusWouldApply, out.RecordID, []ActionStep{step}
	return res, nil
}

// replayActions is the progress a `replayed` answer carries: a shadow receipt
// is stripped of any `from` again, whatever wrote it (a receipt of the first
// dry-run release stored it).
func replayActions(cur Receipt) []ActionStep {
	if cur.Shadow {
		return stepsWithoutFrom(cur.Progress)
	}
	return cur.Progress
}

func fidelityOf(shadow bool) string {
	if shadow {
		return FidelityPartial
	}
	return ""
}

// ContractState is the EFFECTIVE mode of a registered contract version, as this
// process serves it (for a status endpoint: after a rollout or a reload, check
// that every replica reports the same).
type ContractState struct {
	Key     string `json:"key"`
	Version int    `json:"version"`
	DryRun  bool   `json:"dry_run"`
}

// ContractStates lists every registered contract version and whether this
// process runs it in dry-run, sorted by key and version.
func (h *Handler) ContractStates() []ContractState {
	all := h.cfg.Contracts.all()
	out := make([]ContractState, 0, len(all))
	for _, c := range all {
		out = append(out, ContractState{Key: c.Key, Version: c.Version, DryRun: c.DryRun})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key != out[j].Key {
			return out[i].Key < out[j].Key
		}
		return out[i].Version < out[j].Version
	})
	return out
}

// SetDryRun switches dry_run of a registered contract version: the contract is
// recompiled and validated again (checkDryRun included) and replaces the
// registered one atomically, so the next event already runs in the new mode.
// Changing the flag in a stored row WITHOUT reloading the contract leaves the
// compiled copy of this process with the old mode: after any change, call
// SetDryRun or RegisterContract and verify it with ContractStates on every
// replica. Updating a replica that predates dry-run support is NOT safe (see
// docs/inbound-events.md, "Orden de despliegue").
func (h *Handler) SetDryRun(ctx context.Context, key string, version int, dry bool) error {
	if version < 1 {
		return invalidf("%s: SetDryRun needs an explicit version", key)
	}
	c, ok := h.cfg.Contracts.get(key, version)
	if !ok {
		return &DryRunError{Status: http.StatusNotFound, Code: CodeUnknownContract}
	}
	next := c.Contract
	next.DryRun = dry
	return h.RegisterContract(ctx, next)
}
