package compiled

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/lifecycle"
	"github.com/asteby/metacore-kernel/runtime/apiroute"
)

// AddonOf resolves the addon that owns a handler reference for an org. Hosts
// with one addon per registered dispatcher pass Fixed; a host serving every
// addon looks the owner up from its registry by (org, target).
type AddonOf func(ctx context.Context, orgID uuid.UUID, target string) (string, error)

// Fixed is an AddonOf that always answers addonKey.
func Fixed(addonKey string) AddonOf {
	return func(context.Context, uuid.UUID, string) (string, error) { return addonKey, nil }
}

// ModelOwner resolves the addon that owns a model (dynamic.Config.AddonKeyForModel
// has this exact shape). "" means unknown.
type ModelOwner func(ctx context.Context, model string) string

// OrgFromContext reads the tenant of a context for tiers whose interface
// carries none (Tier-3 formulas).
type OrgFromContext func(ctx context.Context) uuid.UUID

func (a AddonOf) resolve(ctx context.Context, orgID uuid.UUID, target string) (string, error) {
	if a == nil {
		return "", ErrNoOwner
	}
	key, err := a(ctx, orgID, target)
	if err != nil {
		return "", err
	}
	if key == "" {
		return "", fmt.Errorf("%w: %q", ErrNoOwner, target)
	}
	return key, nil
}

// ---- actions and on_transition hooks ---------------------------------------

// ActionEnvelope is the JSON an action (or stage hook) handler receives: the
// acted-on row, the caller's payload, the row id at the root and the
// host-owned actor/org. user_id/org_id come from the authenticated caller,
// never from record/payload (which are client-shaped).
func ActionEnvelope(req dynamic.ActionRequest) map[string]any {
	env := map[string]any{
		"record":  req.Row,
		"payload": req.Payload,
	}
	if id, ok := req.Row["id"].(string); ok && id != "" {
		env["id"] = id
	}
	if req.User != nil {
		if uid := req.User.GetID(); uid != uuid.Nil {
			env["user_id"] = uid.String()
		}
		if oid := req.User.GetOrganizationID(); oid != uuid.Nil {
			env["org_id"] = oid.String()
		}
		if b, ok := req.User.(interface{ GetBranchID() uuid.UUID }); ok {
			if bid := b.GetBranchID(); bid != uuid.Nil {
				env["branch_id"] = bid.String()
			}
		}
	}
	return env
}

type actionDispatcher struct {
	d     *Dispatcher
	owner ModelOwner
}

// Actions is the dynamic.ActionDispatcher of `compiled` (and `wasm`) triggers.
// Register it for the trigger types the host wants this executor to serve:
//
//	cfg.ActionDispatchers["wasm"] = d.Actions(owner)     // handler.type wasm|compiled
//	cfg.ActionDispatchers["compiled"] = d.Actions(owner) // on_transition "compiled:<fn>"
//
// The symbol is Trigger.Export, falling back to ActionKey (how stage hooks
// carry the part of `do` after the prefix). Because it is an ordinary
// ActionDispatcher, Service.ExecAction's idempotency, approval, supervisor,
// reason and state gates apply to compiled handlers exactly as to any other.
func (d *Dispatcher) Actions(owner ModelOwner) dynamic.ActionDispatcher {
	return actionDispatcher{d: d, owner: owner}
}

func (a actionDispatcher) Dispatch(ctx context.Context, req dynamic.ActionRequest) (dynamic.ActionResponse, error) {
	if !a.d.Ready() {
		return dynamic.ActionResponse{}, ErrNoExecutor
	}
	fn := req.ActionKey
	if req.Trigger != nil && req.Trigger.Export != "" {
		fn = req.Trigger.Export
	}
	if a.owner == nil {
		return dynamic.ActionResponse{}, ErrNoOwner
	}
	addon := a.owner(ctx, req.Model)
	if addon == "" {
		return dynamic.ActionResponse{}, fmt.Errorf("%w: model %q", ErrNoOwner, req.Model)
	}
	var orgID uuid.UUID
	if req.User != nil {
		orgID = req.User.GetOrganizationID()
	}
	payload, err := json.Marshal(ActionEnvelope(req))
	if err != nil {
		return dynamic.ActionResponse{}, fmt.Errorf("compiled: marshal action payload: %w", err)
	}
	res, err := a.d.Call(ctx, req.DB, orgID, addon, fn, payload)
	if err != nil {
		return dynamic.ActionResponse{}, err
	}
	resp := dynamic.ActionResponse{Success: res.Success, Meta: res.Meta}
	if len(res.Data) > 0 {
		var data any
		if json.Unmarshal(res.Data, &data) == nil {
			resp.Data = data
		}
	}
	if !res.Success {
		resp.Error = &dynamic.ActionError{Code: res.Error.Code, Message: res.Error.Message}
		if resp.Error.Code == "" && resp.Error.Message == "" {
			resp.Error.Message = "action declined"
		}
	}
	return resp, nil
}

// ---- schedules and inbound webhooks ----------------------------------------

type targetDispatcher struct {
	d  *Dispatcher
	of AddonOf
}

// Schedule satisfies schedule.Dispatcher (Dispatch) and webhookin.Dispatcher /
// webhookin.ResultDispatcher (Dispatch + DispatchResult): same `do` target, same
// payload map, same fail-closed behaviour, one implementation.
func (d *Dispatcher) Schedule(of AddonOf) interface {
	Dispatch(ctx context.Context, orgID uuid.UUID, target string, payload map[string]any) error
	DispatchResult(ctx context.Context, orgID uuid.UUID, target string, payload map[string]any) ([]byte, error)
} {
	return targetDispatcher{d: d, of: of}
}

// Dispatch fires target. success:false is an error (a schedule/webhook has no
// channel to carry a decline).
func (t targetDispatcher) Dispatch(ctx context.Context, orgID uuid.UUID, target string, payload map[string]any) error {
	_, err := t.run(ctx, orgID, target, payload)
	return err
}

// DispatchResult fires target and returns the handler's raw output, so an
// inbound webhook can shape its HTTP answer from it.
func (t targetDispatcher) DispatchResult(ctx context.Context, orgID uuid.UUID, target string, payload map[string]any) ([]byte, error) {
	res, err := t.run(ctx, orgID, target, payload)
	return res.Raw, err
}

func (t targetDispatcher) run(ctx context.Context, orgID uuid.UUID, target string, payload map[string]any) (Result, error) {
	if !t.d.Ready() {
		return Result{}, ErrNoExecutor
	}
	addon, err := t.of.resolve(ctx, orgID, target)
	if err != nil {
		return Result{}, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return Result{}, fmt.Errorf("compiled: marshal payload: %w", err)
	}
	return t.d.callOK(ctx, nil, orgID, addon, target, body)
}

// ---- api routes ------------------------------------------------------------

// APIRoute is the apiroute.Dispatcher: the request envelope goes in, the
// {status,headers,body} envelope comes out (decoded strictly by
// apiroute.DecodeResponse). The owning addon is req.Addon.
func (d *Dispatcher) APIRoute() apiroute.Dispatcher {
	return apiroute.DispatcherFunc(func(ctx context.Context, orgID uuid.UUID, target string, req apiroute.Request) (apiroute.Response, error) {
		if !d.Ready() {
			return apiroute.Response{}, ErrNoExecutor
		}
		body, err := req.Encode()
		if err != nil {
			return apiroute.Response{}, fmt.Errorf("compiled: encode request: %w", err)
		}
		out, err := d.CallRaw(ctx, nil, orgID, req.Addon, target, body)
		if err != nil {
			return apiroute.Response{}, err
		}
		return apiroute.DecodeResponse(out)
	})
}

// ---- lifecycle / CRUD hooks ------------------------------------------------

// Hook is the lifecycle.HookDispatcher for HookTarget.Type "compiled" (and
// "wasm"): install/enable/disable/upgrade/uninstall and the manifest
// before_*/after_* CRUD hooks. A decline is an error, so a before_* hook vetoes
// the operation and a lifecycle transition aborts.
func (d *Dispatcher) Hook() lifecycle.HookDispatcher {
	return lifecycle.HookDispatcherFunc(func(ctx context.Context, req lifecycle.HookRequest) (lifecycle.HookResponse, error) {
		res, err := d.callOK(ctx, nil, req.OrgID, req.AddonKey, req.Target.Function, req.Payload)
		return lifecycle.HookResponse{Body: res.Raw}, err
	})
}

// ---- Tier-3 formulas -------------------------------------------------------

// Formula is the dynamic.FormulaInvoker for `compiled:<fn>` (and `wasm:<fn>`)
// handlers: the merged row goes in, data.value (or data) comes out. A decline is
// an error that aborts the write. orgOf reads the tenant from ctx.
func (d *Dispatcher) Formula(owner ModelOwner, orgOf OrgFromContext) dynamic.FormulaInvoker {
	return func(ctx context.Context, model, handler string, row map[string]any) (any, error) {
		if !d.Ready() {
			return nil, ErrNoExecutor
		}
		if owner == nil {
			return nil, ErrNoOwner
		}
		addon := owner(ctx, model)
		if addon == "" {
			return nil, fmt.Errorf("%w: model %q", ErrNoOwner, model)
		}
		var org uuid.UUID
		if orgOf != nil {
			org = orgOf(ctx)
		}
		body, err := json.Marshal(row)
		if err != nil {
			return nil, fmt.Errorf("compiled: marshal row: %w", err)
		}
		res, err := d.callOK(ctx, nil, org, addon, handler, body)
		if err != nil {
			return nil, err
		}
		return FormulaValue(res.Data)
	}
}

// FormulaValue extracts the computed value from a formula handler's data: the
// `value` key when data is an object carrying it, else data itself. Empty data
// yields nil (the caller leaves the target unchanged).
func FormulaValue(data json.RawMessage) (any, error) {
	if len(data) == 0 || string(data) == "null" {
		return nil, nil
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(data, &obj) == nil {
		if v, ok := obj["value"]; ok {
			data = v
		}
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("%w: formula value: %v", ErrBadResult, err)
	}
	return out, nil
}
