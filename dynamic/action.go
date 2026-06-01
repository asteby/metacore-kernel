package dynamic

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/modelbase"
)

// ActionResolver returns the manifest.ActionDef declared for a (model, key)
// pair. Hosts wire it from their addon registry — the kernel does not own a
// global Actions index because action declarations live next to the model in
// the addon manifest.
type ActionResolver func(ctx context.Context, model, key string) (*manifest.ActionDef, bool)

// ActionRequest is the input to an ActionDispatcher.
//
// DB carries the open transaction when the trigger declared RunInTx; when nil
// the dispatcher must use whatever handle the caller already had (typically
// Service.db). Trigger is never nil — Service.ExecAction synthesises a
// default ActionTrigger{Type:"webhook"} when the manifest omits one, matching
// the legacy bridge behaviour.
type ActionRequest struct {
	Model     string
	ActionKey string
	Row       map[string]any
	Payload   map[string]any
	User      modelbase.AuthUser
	Trigger   *manifest.ActionTrigger
	DB        *gorm.DB
}

// ActionResponse is the dispatcher's reply, in the kernel envelope shape.
//
// When ExecAction ran inside a tx, Success drives commit/rollback. The kernel
// merges Meta with kernel-managed keys (model, action, trigger_type,
// rolled_back) — kernel keys win on collision, so guests cannot fake them.
type ActionResponse struct {
	Success bool
	Data    any
	Meta    map[string]any
	Error   *ActionError
}

// ActionError is the structured error returned by a dispatcher when the
// trigger declined the action (Success=false). The kernel forwards Code and
// Message verbatim into the response envelope's `error` block.
type ActionError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Error implements the error interface so dispatchers can surface a typed
// error through the standard return path when needed.
func (e *ActionError) Error() string {
	if e == nil {
		return ""
	}
	if e.Code == "" {
		return e.Message
	}
	return e.Code + ": " + e.Message
}

// ActionDispatcher dispatches an action invocation to its backend. One
// implementation per Trigger.Type is registered via
// dynamic.Config.ActionDispatchers. The built-in "noop" dispatcher is wired
// automatically when not overridden.
type ActionDispatcher interface {
	Dispatch(ctx context.Context, req ActionRequest) (ActionResponse, error)
}

// NoopDispatcher is the built-in handler for Trigger.Type == "noop". It runs
// no side effect, returning a success envelope with meta.no_op = true so the
// caller can distinguish UI-only confirmations.
type NoopDispatcher struct{}

// Dispatch satisfies ActionDispatcher.
func (NoopDispatcher) Dispatch(_ context.Context, _ ActionRequest) (ActionResponse, error) {
	return ActionResponse{Success: true, Meta: map[string]any{"no_op": true}}, nil
}

// ActionResult is the kernel-side envelope returned by Service.ExecAction.
// Handlers translate it into the HTTP response shape; the integer HTTPStatus
// is the suggested status code (200 on success, 422 on a guest-declined
// action). Errors that abort dispatch before it runs are returned as a
// regular Go error and mapped by handler.handleError.
type ActionResult struct {
	Success    bool
	Data       any
	Meta       map[string]any
	Error      *ActionError
	HTTPStatus int
}

// errRollbackOnFailure is the sentinel Service.ExecAction passes to the
// gorm.Transaction callback to force a rollback when the dispatcher returned
// Success=false. It is consumed inside ExecAction and never leaks.
var errRollbackOnFailure = errors.New("dynamic: action dispatcher returned success=false")

// ExecAction resolves a row, runs an action's trigger, and returns the
// kernel envelope. The contract is the four-step flow declared in the
// dynamic-actions doc:
//
//  1. Load the row through Service.Get (org-scoped).
//  2. Open a DB transaction iff Trigger.Type=="wasm" && Trigger.RunInTx.
//  3. Dispatch on Trigger.Type via the registered ActionDispatcher.
//  4. Commit on Success=true / rollback on Success=false (or on a Go error).
//
// The legacy "manifest declares no Trigger" case maps to a synthetic
// ActionTrigger{Type:"webhook"} so existing addons that rely on
// bridge/actions.go's implicit hook resolution keep working unchanged once a
// "webhook" dispatcher is wired.
func (s *Service) ExecAction(ctx context.Context, model string, user modelbase.AuthUser, id uuid.UUID, key string, payload map[string]any) (ActionResult, error) {
	if s.actionResolver == nil {
		return ActionResult{}, ErrNoActionResolver
	}
	if err := s.checkPerm(ctx, user, model, "update"); err != nil {
		return ActionResult{}, err
	}

	def, ok := s.actionResolver(ctx, model, key)
	if !ok || def == nil {
		return ActionResult{}, ErrActionNotFound
	}

	trig := def.Trigger
	if trig == nil {
		trig = &manifest.ActionTrigger{Type: "webhook"}
	}

	row, err := s.Get(ctx, model, user, id)
	if err != nil {
		return ActionResult{}, err
	}

	// State gate: when the action declares requires_state, the target record's
	// `status` column must be one of the allowed values. Enforced before the
	// trigger runs so a disallowed transition never reaches the dispatcher.
	// Additive — when RequiresState is empty the action is always valid, so
	// existing actions are unaffected.
	if len(def.RequiresState) > 0 {
		if err := checkRequiresState(row, def.RequiresState); err != nil {
			return ActionResult{}, err
		}
	}

	dispatcher, ok := s.actionDispatchers[trig.Type]
	if !ok {
		return ActionResult{}, fmt.Errorf("%w: %q", ErrUnsupportedTriggerType, trig.Type)
	}

	runInTx := trig.Type == "wasm" && trig.RunInTx
	kernelMeta := map[string]any{
		"model":        model,
		"action":       key,
		"trigger_type": trig.Type,
	}

	if !runInTx {
		req := ActionRequest{
			Model:     model,
			ActionKey: key,
			Row:       row,
			Payload:   payload,
			User:      user,
			Trigger:   trig,
		}
		resp, derr := dispatcher.Dispatch(ctx, req)
		if derr != nil {
			return ActionResult{}, derr
		}
		return buildResult(resp, kernelMeta), nil
	}

	var resp ActionResponse
	var dispErr error
	txErr := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		req := ActionRequest{
			Model:     model,
			ActionKey: key,
			Row:       row,
			Payload:   payload,
			User:      user,
			Trigger:   trig,
			DB:        tx,
		}
		resp, dispErr = dispatcher.Dispatch(ctx, req)
		if dispErr != nil {
			return dispErr
		}
		if !resp.Success {
			return errRollbackOnFailure
		}
		return nil
	})

	if dispErr != nil {
		return ActionResult{}, dispErr
	}

	rolledBack := txErr != nil
	kernelMeta["rolled_back"] = rolledBack
	return buildResult(resp, kernelMeta), nil
}

// buildResult merges dispatcher-supplied meta with kernel-managed meta (kernel
// wins) and chooses the suggested HTTP status. 200 on success, 422 on a
// dispatcher-declined action (the doc-prescribed code for "guest decided no",
// distinct from a 4xx the kernel itself produced).
func buildResult(resp ActionResponse, kernelMeta map[string]any) ActionResult {
	meta := mergeMeta(resp.Meta, kernelMeta)
	if resp.Success {
		return ActionResult{
			Success:    true,
			Data:       resp.Data,
			Meta:       meta,
			HTTPStatus: 200,
		}
	}
	return ActionResult{
		Success:    false,
		Error:      resp.Error,
		Meta:       meta,
		HTTPStatus: 422,
	}
}

// checkRequiresState enforces an action's RequiresState gate against the loaded
// record. It reads the record's `status` column and returns ErrInvalidState
// (wrapped with the offending/allowed values) when the status is not one of the
// allowed states. allowed is guaranteed non-empty by the caller.
func checkRequiresState(row map[string]any, allowed []string) error {
	status := stringifyStatus(row["status"])
	for _, s := range allowed {
		if s == status {
			return nil
		}
	}
	return fmt.Errorf("%w: status %q not in %v", ErrInvalidState, status, allowed)
}

// stringifyStatus normalises a record's status column value to a string for
// comparison. The dynamic CRUD layer returns column values as their JSON/Go
// scalar shapes; status is a text column so it is almost always a string, but
// we coerce defensively (e.g. []byte from some drivers) and treat a missing or
// null status as the empty string.
func stringifyStatus(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []byte:
		return string(t)
	case fmt.Stringer:
		return t.String()
	default:
		return fmt.Sprintf("%v", t)
	}
}

func mergeMeta(guest, kernel map[string]any) map[string]any {
	out := make(map[string]any, len(guest)+len(kernel))
	for k, v := range guest {
		out[k] = v
	}
	for k, v := range kernel {
		out[k] = v
	}
	return out
}
