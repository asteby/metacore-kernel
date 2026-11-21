// Package compiled is the ONE executor of `compiled` (and `wasm`) handler
// references, shared by every tier that dispatches a manifest handler: actions,
// on_transition stage hooks, schedules, inbound webhooks, api routes, lifecycle
// and CRUD hooks, Tier-3 formulas and backfills.
//
// A compiled handler is a Go export registered in the host with
// wasm.Host.LoadNative (runtime/wasm/native.go); a wasm handler is an export of
// the addon's module. Both are reached through the same call,
// Host.InvokeForInTx, so the Dispatcher here is a thin, fail-closed adapter
// from each tier's own dispatch interface onto that single seam. It owns no
// business rule, no guard and no policy: tenant scope, capabilities, the write
// guards and emit-on-commit all live behind the Invoker.
//
// Fail-closed contract (no false OK):
//
//   - no Invoker wired            -> ErrNoExecutor
//   - addon not installed/enabled -> ErrNotInstalled (the resolver says so)
//   - empty/malformed symbol      -> ErrInvalidHandler
//   - handler not loaded/exported -> the Invoker's error, verbatim
//   - non-JSON output             -> ErrBadResult
//
// A handler answering {success:false} is a DECLINE, not a fault: tiers that can
// carry it (actions, hooks) report it as such; tiers that cannot (schedules,
// webhooks) turn it into an error.
package compiled

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/asteby/metacore-kernel/nativekit/observe"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Invoker is the runtime host seam. *wasm.Host satisfies it: a native Go export
// of the addon wins over a wasm export of the same name.
type Invoker interface {
	InvokeForInTx(ctx context.Context, tx *gorm.DB, orgID, installation uuid.UUID, addonKey, function string, payload []byte, settings map[string]string) ([]byte, error)
}

// Installation is what the host knows about an addon installed for an org.
type Installation struct {
	// ID is the installation id handed to the runtime (module instance key).
	ID uuid.UUID
	// Settings are the installation settings the handler reads via env_get /
	// NativeCall.Setting. nil lets the runtime resolve them itself.
	Settings map[string]string
}

// Resolver finds the installation of addonKey for orgID. It MUST return an
// error wrapping ErrNotInstalled when the addon is not installed and enabled
// for the org, so a disabled addon's handler is never run.
type Resolver func(ctx context.Context, orgID uuid.UUID, addonKey string) (Installation, error)

// Typed errors.
var (
	// ErrNoExecutor: no Invoker is wired, so the handler cannot run. Never
	// degraded to a success.
	ErrNoExecutor = errors.New("compiled: no executor wired for the handler")
	// ErrNotInstalled: the addon is not installed/enabled for the org.
	ErrNotInstalled = errors.New("compiled: addon not installed for the organization")
	// ErrInvalidHandler: the handler reference/symbol is empty or malformed.
	ErrInvalidHandler = errors.New("compiled: invalid handler reference")
	// ErrBadResult: the handler's output is not a JSON document.
	ErrBadResult = errors.New("compiled: handler returned a malformed result")
	// ErrNoOwner: the addon that owns the model/route could not be resolved.
	ErrNoOwner = errors.New("compiled: cannot resolve the addon that owns the handler")
	// ErrDeclined: the handler answered {success:false} where the tier cannot
	// carry a decline (schedules, inbound webhooks, lifecycle).
	ErrDeclined = errors.New("compiled: handler declined")
)

var symbolRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// Dispatcher is the single executor. Construct it with New; the zero value and
// a nil *Dispatcher are valid and fail closed (ErrNoExecutor).
type Dispatcher struct {
	inv Invoker
	res Resolver
	obs *observe.Observer
}

// New builds a Dispatcher over inv. res resolves installations; nil means
// "no installation id/settings" (uuid.Nil, runtime-resolved settings), which is
// only appropriate for hosts that gate installation elsewhere.
func New(inv Invoker, res Resolver) *Dispatcher { return &Dispatcher{inv: inv, res: res} }

// Ready reports whether an Invoker is wired. Hosts use it to refuse boot (or
// skip registering a schedule) instead of discovering the gap at fire time.
func (d *Dispatcher) Ready() bool { return d != nil && d.inv != nil }

// Target returns the symbol of a handler reference: "compiled:Fn" and
// "wasm:Fn" both yield "Fn"; a bare symbol is returned as is. The result is
// validated.
func Target(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if prefix, rest, found := strings.Cut(ref, ":"); found {
		if prefix != "compiled" && prefix != "wasm" {
			return "", fmt.Errorf("%w: %q has prefix %q (want compiled|wasm)", ErrInvalidHandler, ref, prefix)
		}
		ref = rest
	}
	if !symbolRe.MatchString(ref) {
		return "", fmt.Errorf("%w: symbol %q", ErrInvalidHandler, ref)
	}
	return ref, nil
}

// Observe installs the observer that receives one Sample per invocation (kind
// "compiled", module = addon key, action = handler symbol). nil disables it.
func (d *Dispatcher) Observe(o *observe.Observer) {
	if d != nil {
		d.obs = o
	}
}

// observed classifies err for the observer without changing what callers see.
func observed(err error) error {
	if errors.Is(err, ErrNotInstalled) {
		return fmt.Errorf("%w: %w", observe.ErrNotInstalled, err)
	}
	return err
}

// CallRaw invokes function of addonKey for orgID and returns the handler's raw
// output. tx, when non-nil, is the open transaction the handler's writes join.
func (d *Dispatcher) CallRaw(ctx context.Context, tx *gorm.DB, orgID uuid.UUID, addonKey, function string, payload []byte) ([]byte, error) {
	sp := d.span(addonKey, function, orgID)
	out, err := d.callRaw(ctx, tx, orgID, addonKey, function, payload)
	sp.End(ctx, observed(err))
	return out, err
}

func (d *Dispatcher) span(addonKey, function string, orgID uuid.UUID) *observe.Span {
	var o *observe.Observer
	if d != nil {
		o = d.obs
	}
	return o.Start(observe.KindCompiled, addonKey, function).With(orgID, "")
}

func (d *Dispatcher) callRaw(ctx context.Context, tx *gorm.DB, orgID uuid.UUID, addonKey, function string, payload []byte) ([]byte, error) {
	if !d.Ready() {
		return nil, ErrNoExecutor
	}
	fn, err := Target(function)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(addonKey) == "" {
		return nil, ErrNoOwner
	}
	var inst Installation
	if d.res != nil {
		inst, err = d.res(ctx, orgID, addonKey)
		if err != nil {
			return nil, err
		}
	}
	return d.inv.InvokeForInTx(ctx, tx, orgID, inst.ID, addonKey, fn, payload, inst.Settings)
}

// Fault is the structured error of a {success:false} answer.
type Fault struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	MessageKey string `json:"message_key,omitempty"`
}

// Error implements error.
func (f *Fault) Error() string {
	if f == nil {
		return ""
	}
	switch {
	case f.Code != "" && f.Message != "":
		return f.Code + ": " + f.Message
	case f.Message != "":
		return f.Message
	case f.Code != "":
		return f.Code
	}
	return "success=false"
}

// Result is a handler's decoded {success,data,error,meta,message} envelope.
type Result struct {
	// Raw is the exact output.
	Raw []byte
	// Success is false only when the handler answered success:false. An empty
	// output, or JSON without a `success` key, is a success.
	Success bool
	// Data is the envelope's `data`; when the output carries no `success` key
	// the whole document is the data.
	Data json.RawMessage
	// Meta is the envelope's `meta`.
	Meta map[string]any
	// Message is the envelope's optional human message.
	Message string
	// Error is set when Success is false.
	Error *Fault
}

// Decode parses a handler output. Empty output is a success with no data; a
// non-JSON output is ErrBadResult.
func Decode(raw []byte) (Result, error) {
	res := Result{Raw: raw, Success: true}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return res, nil
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &probe); err != nil {
		// Valid JSON that is not an object (a bare value) is data.
		var any json.RawMessage
		if json.Unmarshal(trimmed, &any) != nil {
			return Result{}, fmt.Errorf("%w: %v", ErrBadResult, err)
		}
		res.Data = any
		return res, nil
	}
	rawSuccess, hasSuccess := probe["success"]
	if !hasSuccess {
		res.Data = json.RawMessage(trimmed)
		return res, nil
	}
	var ok bool
	if err := json.Unmarshal(rawSuccess, &ok); err != nil {
		return Result{}, fmt.Errorf("%w: success is not a boolean", ErrBadResult)
	}
	res.Success = ok
	res.Data = probe["data"]
	if m := probe["meta"]; len(m) > 0 {
		_ = json.Unmarshal(m, &res.Meta)
	}
	if m := probe["message"]; len(m) > 0 {
		_ = json.Unmarshal(m, &res.Message)
	}
	if !ok {
		f := &Fault{}
		if e := probe["error"]; len(e) > 0 {
			if json.Unmarshal(e, f) != nil {
				// a bare string error
				var s string
				if json.Unmarshal(e, &s) == nil {
					f.Message = s
				}
			}
		}
		res.Error = f
	}
	return res, nil
}

// Call is CallRaw plus Decode.
func (d *Dispatcher) Call(ctx context.Context, tx *gorm.DB, orgID uuid.UUID, addonKey, function string, payload []byte) (Result, error) {
	sp := d.span(addonKey, function, orgID)
	out, err := d.callRaw(ctx, tx, orgID, addonKey, function, payload)
	if err != nil {
		sp.End(ctx, observed(err))
		return Result{}, err
	}
	res, err := Decode(out)
	if err == nil && !res.Success {
		sp.End(ctx, fmt.Errorf("%w: %s", observe.ErrDeclined, addonKey))
		return res, nil
	}
	sp.End(ctx, err)
	return res, err
}

// callOK is Call for tiers that cannot carry a decline: success:false becomes
// an error wrapping ErrDeclined.
func (d *Dispatcher) callOK(ctx context.Context, tx *gorm.DB, orgID uuid.UUID, addonKey, function string, payload []byte) (Result, error) {
	res, err := d.Call(ctx, tx, orgID, addonKey, function, payload)
	if err != nil {
		return Result{}, err
	}
	if !res.Success {
		return res, fmt.Errorf("%w: %s.%s: %w", ErrDeclined, addonKey, function, res.Error)
	}
	return res, nil
}
