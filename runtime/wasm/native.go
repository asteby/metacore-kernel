package wasm

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/asteby/metacore-kernel/database/staleplan"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Native exports: the in-process sibling of a wasm guest.
//
// A first-party module compiled INTO the host binary declares its contributions
// with the same v3 manifest as any addon (handler: {type:"wasm"|"compiled",
// function:"X"}) and implements "X" as a Go function registered here. It then
// enters through the SAME seam as a wasm export — Host.Invoke / InvokeFor /
// InvokeInTx / InvokeForInTx — so every caller that already dispatches to a
// wasm guest (the action tier, the event dispatcher, schedules, inbound
// webhooks, api/public routes, lifecycle hooks, connector exports) reaches a
// native handler with ZERO extra wiring and the same envelope (JSON in, JSON
// out).
//
// What is shared, by construction, and not re-implemented: the per-call
// invocation (newInvocation: tenant scope, capability policy, enforcer, table
// resolver, create/update stamps, mutation guard + compute, append-only, stage
// machine, extensions, approvals, sequences, routing, connectors, edge), the
// emit-on-commit buffer, the deadline, diagnostics and metrics. NativeCall
// exposes the host imports through the SAME execute* functions the wasm
// closures call, so a native handler cannot write around the guards a guest
// is subject to.

// NativeFunc is a Go export. payload is the JSON envelope the host would hand a
// wasm export; the returned bytes are the JSON result envelope.
type NativeFunc func(ctx context.Context, call *NativeCall, payload []byte) ([]byte, error)

type nativeEntry struct {
	fns map[string]NativeFunc
}

// LoadNative registers (replacing any previous registration) the Go exports of
// addonKey. It is process-lifetime on purpose: the code is part of the binary,
// so installer enable/disable/uninstall (which Unload wasm modules) neither
// need nor get to drop it. Whether an installation may be invoked is decided
// by the host's access gate, exactly as for a wasm addon.
//
// An export name present in fns shadows a wasm export of the same name, so a
// module can be migrated function by function.
func (h *Host) LoadNative(addonKey string, fns map[string]NativeFunc) error {
	if strings.TrimSpace(addonKey) == "" {
		return errors.New("wasm: native: empty addon key")
	}
	cp := make(map[string]NativeFunc, len(fns))
	for name, fn := range fns {
		if strings.TrimSpace(name) == "" || fn == nil {
			return fmt.Errorf("wasm: native: %s: invalid export %q", addonKey, name)
		}
		cp[name] = fn
	}
	h.natives.Store(addonKey, &nativeEntry{fns: cp})
	return nil
}

// UnloadNative drops the Go exports of addonKey (tests, hot-swap).
func (h *Host) UnloadNative(addonKey string) { h.natives.Delete(addonKey) }

// NativeExports lists the native export names of addonKey, sorted; nil when the
// addon has none. Hosts use it for the same declared-handler-vs-implemented
// drift check they run against a wasm module's exports.
func (h *Host) NativeExports(addonKey string) []string {
	v, ok := h.natives.Load(addonKey)
	if !ok {
		return nil
	}
	e := v.(*nativeEntry)
	out := make([]string, 0, len(e.fns))
	for name := range e.fns {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// HasNative reports whether addonKey exports funcName natively.
func (h *Host) HasNative(addonKey, funcName string) bool {
	_, ok := h.nativeFn(addonKey, funcName)
	return ok
}

func (h *Host) nativeFn(addonKey, funcName string) (NativeFunc, bool) {
	v, ok := h.natives.Load(addonKey)
	if !ok {
		return nil, false
	}
	fn, ok := v.(*nativeEntry).fns[funcName]
	return fn, ok
}

// invokeNative runs a native export under the same invocation context a wasm
// export gets. A panic is a fault of the handler (reported like a guest trap),
// never of the host. The call runs on the caller's goroutine, so the deadline
// is cooperative (ctx), unlike wasm where the instance is torn down.
func (h *Host) invokeNative(ctx context.Context, fn NativeFunc, tx *gorm.DB, orgID, installation uuid.UUID, addonKey, funcName string, payload []byte, settings map[string]string) (out []byte, err error) {
	emits := &emitBuffer{}
	ctx = withEmitBuffer(ctx, emits)

	callCtx, deadline, cancel := withGuestDeadline(ctx, defaultTimeout, defaultTimeout+maxGuestHTTPWait)
	defer cancel()
	inv := h.newInvocation(ctx, tx, orgID, installation, addonKey, settings, deadline)
	callCtx = withInvocation(callCtx, inv)

	defer func() {
		if r := recover(); r != nil {
			emits.reset()
			out = nil
			err = &faultError{code: "trap", err: fmt.Errorf("native: %s.%s panicked: %v", addonKey, funcName, r)}
		}
	}()
	res, ferr := fn(callCtx, &NativeCall{ctx: callCtx, inv: inv}, payload)
	if ferr != nil {
		emits.reset()
		if callCtx.Err() != nil && ctx.Err() == nil {
			return nil, &faultError{code: "timeout", err: fmt.Errorf("native: %s.%s: %w", addonKey, funcName, ferr)}
		}
		return nil, ferr
	}
	h.flushEmits(ctx, tx, emits, addonKey, funcName, res, nil)
	return res, nil
}

// NativeCall is the Go face of the metacore_host imports for one native
// invocation. Every method returns the same JSON envelope its wasm import
// writes into guest memory ({success,data,error,meta}), after the same
// capability, tenant and guard checks, because it calls the same function.
//
// Not exposed yet (they need a refactor of their import closure into an
// execute* function first): http_fetch/http_request, connector_get, db_exec,
// smtp_send. A native handler reaches connectors and outbound HTTP through the
// declarative handler types (connector/webhook) meanwhile.
type NativeCall struct {
	ctx context.Context
	inv *invocation
}

// AddonKey is the addon the invocation runs as (capability subject).
func (c *NativeCall) AddonKey() string { return c.inv.addonKey }

// OrgID is the tenant bound to the invocation; never taken from the payload.
func (c *NativeCall) OrgID() uuid.UUID { return c.inv.orgID }

// Setting is env_get: the installation's setting value.
func (c *NativeCall) Setting(key string) (string, bool) {
	v, ok := c.inv.settings[key]
	return v, ok && v != ""
}

// Logf is the log import.
func (c *NativeCall) Logf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	c.inv.logger.Printf("metacore.native addon=%s installation=%s msg=%s", c.inv.addonKey, c.inv.installation, msg)
	c.inv.bag.Log(msg)
}

// DataQuery is the data_query import.
func (c *NativeCall) DataQuery(req []byte) []byte {
	return staleplan.RetryEnvelope(c.inv.db, "data_query", func() []byte { return executeDataQueryRecords(c.ctx, c.inv, req) })
}

// DataMutate is the data_mutate import.
func (c *NativeCall) DataMutate(req []byte) []byte {
	return staleplan.RetryEnvelope(c.inv.db, "data_mutate", func() []byte { return executeDataMutate(c.ctx, c.inv, req) })
}

// DataBatch is the data_batch import.
func (c *NativeCall) DataBatch(req []byte) []byte {
	return staleplan.RetryEnvelope(c.inv.db, "data_batch", func() []byte { return executeDataBatch(c.ctx, c.inv, req) })
}

// DBQuery is the db_query import (addon-schema scoped read SQL).
func (c *NativeCall) DBQuery(sqlText string, argsJSON []byte) []byte {
	return staleplan.RetryEnvelope(c.inv.db, "db_query", func() []byte {
		return executeDBQuery(c.ctx, c.inv.db, c.inv.addonKey, c.inv.enforcer, sqlText, argsJSON)
	})
}

// SequenceNext is the sequence_next import.
func (c *NativeCall) SequenceNext(req []byte) []byte { return executeSequenceNext(c.ctx, c.inv, req) }

// CtxGet is the ctx_get import (acting user, roles, org currency/tax/locale).
func (c *NativeCall) CtxGet(req []byte) []byte { return executeCtxGet(c.ctx, c.inv, req) }

// ApprovalRequest is the approval_request import.
func (c *NativeCall) ApprovalRequest(req []byte) []byte {
	return executeApprovalRequest(c.ctx, c.inv, req)
}

// ApprovalConsume is the approval_consume import.
func (c *NativeCall) ApprovalConsume(req []byte) []byte {
	return executeApprovalConsume(c.ctx, c.inv, req)
}

// CapabilityQuery is the capability_query import.
func (c *NativeCall) CapabilityQuery(req []byte) []byte {
	return executeCapabilityQuery(c.ctx, c.inv, req)
}

// CapabilityProviders is the capability_providers import.
func (c *NativeCall) CapabilityProviders(req []byte) []byte {
	return executeCapabilityProviders(c.ctx, c.inv, req)
}

// RoutingResolve is the routing_resolve import.
func (c *NativeCall) RoutingResolve(req []byte) []byte {
	return executeRoutingResolve(c.ctx, c.inv, req)
}

// EdgeCommand is the edge_command import.
func (c *NativeCall) EdgeCommand(req []byte) []byte { return executeEdgeCommand(c.ctx, c.inv, req) }

// DNSResolve is the dns_resolve import.
func (c *NativeCall) DNSResolve(req []byte) []byte { return executeDNSResolve(c.ctx, c.inv, req) }

// EmitEvent is the event_emit import: staged and published only if the
// invocation succeeds, same policy and bus checks as the guest path.
func (c *NativeCall) EmitEvent(event string, payload []byte) []byte {
	return executeEventEmit(c.ctx, time.Now(), c.inv, uint32(len(event)), uint32(len(payload)),
		func() ([]byte, bool) { return []byte(event), true },
		func() []byte { return payload })
}
