package wasm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/asteby/metacore-kernel/connectors"
	"github.com/asteby/metacore-kernel/events"
	"github.com/asteby/metacore-kernel/security"
	"github.com/google/uuid"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"gorm.io/gorm"
)

// eventEmit limits guard the bus fan-out from a runaway guest. They mirror
// the values documented in docs/wasm-abi.md § 12.5 and are intentionally
// host-side (not negotiable per addon).
const (
	eventNameMaxBytes    = 256
	eventPayloadMaxBytes = 256 * 1024
)

// invocation is the per-call bag the host module imports read. Living on the
// request context (not a field on Host) means concurrent invocations on the
// same addon can carry different settings without locking.
type invocation struct {
	addonKey     string
	installation uuid.UUID
	settings     map[string]string
	caps         *security.Capabilities
	bus          *events.Bus
	orgID        uuid.UUID
	logger       *log.Logger
	// db_query / db_exec plumbing. db is the standalone connection both
	// imports fall back to; enforcer is the policy gate. tx is non-nil only
	// when the host entered through Host.InvokeInTx — db_exec then runs on
	// the action handler's open transaction so the guest's writes share the
	// action's commit/rollback fate. When db is nil the db_query import
	// returns a `db_unavailable` envelope; same for db_exec when both tx
	// and db are nil.
	db       *gorm.DB
	tx       *gorm.DB
	enforcer *security.Enforcer
	// resolveTable is the embedder-injected logical→physical table mapping
	// the data_mutate import uses (Host.WithTableResolver). nil = identity.
	resolveTable func(table string) string
	// execSchema overrides the schema db_exec scopes bare names to via
	// search_path (Host.WithExecSchema). nil = AddonSchema(addonKey). The
	// capability gate still authorises against AddonSchema regardless.
	execSchema func(addonKey string) string
	// connectors resolves connector credentials for the connector_get import
	// (Host.WithConnectors), scoped to orgID and gated by connector:read. nil =
	// the connectors runtime is off (connector_get returns connector_unavailable).
	connectors *connectors.Resolver
}

type invKey struct{}

func withInvocation(ctx context.Context, inv *invocation) context.Context {
	return context.WithValue(ctx, invKey{}, inv)
}

func invocationFrom(ctx context.Context) *invocation {
	if v, ok := ctx.Value(invKey{}).(*invocation); ok {
		return v
	}
	return nil
}

// orgIDKey is the context tag callers stash a tenant id under via WithOrgID.
// Kept private so consumers cannot collide on the type identity; reads go
// through orgIDFrom.
type orgIDKey struct{}

// WithOrgID returns ctx tagged with the caller's organization id so the WASM
// host imports that need tenant scoping (today: event_emit, see § 12.6 of
// docs/wasm-abi.md) can resolve it without a new function parameter.
//
// Callers SHOULD wrap their context before reaching Host.Invoke /
// Host.InvokeInTx when the invocation originates from a tenant-scoped
// request (HTTP handler, action bridge, dynamic CRUD hook). The Host.InvokeFor
// and Host.InvokeInTxFor sibling helpers do this automatically.
//
// Passing uuid.Nil is a no-op — it does not overwrite an existing tag — so
// nested withers cannot accidentally erase an upstream tenant binding.
func WithOrgID(ctx context.Context, orgID uuid.UUID) context.Context {
	if orgID == uuid.Nil {
		return ctx
	}
	return context.WithValue(ctx, orgIDKey{}, orgID)
}

// orgIDFrom reads the tenant id stashed by WithOrgID, or uuid.Nil if absent.
// uuid.Nil is the documented "no active org" sentinel — see § 12.6 of
// docs/wasm-abi.md for which imports treat that as an error vs. a no-op.
func orgIDFrom(ctx context.Context) uuid.UUID {
	if v, ok := ctx.Value(orgIDKey{}).(uuid.UUID); ok {
		return v
	}
	return uuid.Nil
}

// registerHostModule exposes the "metacore_host" imports every guest relies
// on. We keep the surface deliberately narrow: one function per privileged
// capability, each enforced by security.Capabilities.
func registerHostModule(ctx context.Context, h *Host) error {
	b := h.rt.NewHostModuleBuilder("metacore_host")

	// log(msgPtr, msgLen) — noop-safe; logger is always non-nil.
	b.NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module, ptr, n uint32) {
			inv := invocationFrom(ctx)
			if inv == nil {
				return
			}
			msg, ok := mod.Memory().Read(ptr, n)
			if !ok {
				return
			}
			inv.logger.Printf("metacore.wasm addon=%s installation=%s msg=%s",
				inv.addonKey, inv.installation, string(msg))
		}).
		Export("log")

	// env_get(keyPtr, keyLen) -> ptr|len
	// Returns the setting value from the per-invocation settings map. Missing
	// keys return 0 — guests must handle that.
	b.NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module, keyPtr, keyLen uint32) uint64 {
			inv := invocationFrom(ctx)
			if inv == nil {
				return 0
			}
			key, ok := mod.Memory().Read(keyPtr, keyLen)
			if !ok {
				return 0
			}
			val, ok := inv.settings[string(key)]
			if !ok || val == "" {
				return 0
			}
			return writeToGuest(ctx, mod, []byte(val))
		}).
		Export("env_get")

	// http_fetch(urlPtr, urlLen, methodPtr, methodLen, bodyPtr, bodyLen) -> ptr|len
	// The original header-less outbound HTTP import. Kept verbatim for ABI
	// back-compat (existing guests — inventory, tire-warranty — call it): it
	// simply delegates to the shared doHTTP with no custom headers. New guests
	// that need request headers (e.g. Authorization) use http_request below.
	// Enforces Capabilities.CanFetch *before* any syscall, so the SSRF guard
	// applies uniformly to webhook and wasm backends.
	b.NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module,
			urlPtr, urlLen, methodPtr, methodLen, bodyPtr, bodyLen uint32) uint64 {
			inv := invocationFrom(ctx)
			if inv == nil {
				return 0
			}
			url := readString(mod, urlPtr, urlLen)
			method := readString(mod, methodPtr, methodLen)
			body := readBytes(mod, bodyPtr, bodyLen)
			return doHTTP(ctx, mod, inv, url, method, nil, body)
		}).
		Export("http_fetch")

	// http_request(urlPtr,urlLen, methodPtr,methodLen, headersPtr,headersLen, bodyPtr,bodyLen) -> ptr|len
	// Like http_fetch but accepts request headers as a JSON object
	// ({"Authorization":"token ...","Accept":"application/vnd.github+json"}),
	// so a guest can authenticate to a third-party API. Same capability gate
	// (http:fetch), same SSRF guard, same 30s timeout and 8 MiB response cap.
	// A malformed headers JSON returns a bad_request envelope. Empty/zero-length
	// headers behaves exactly like http_fetch.
	b.NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module,
			urlPtr, urlLen, methodPtr, methodLen, headersPtr, headersLen, bodyPtr, bodyLen uint32) uint64 {
			inv := invocationFrom(ctx)
			if inv == nil {
				return 0
			}
			url := readString(mod, urlPtr, urlLen)
			method := readString(mod, methodPtr, methodLen)
			body := readBytes(mod, bodyPtr, bodyLen)
			var headers map[string]string
			if headersLen > 0 {
				raw := readBytes(mod, headersPtr, headersLen)
				if err := json.Unmarshal(raw, &headers); err != nil {
					return writeToGuest(ctx, mod, jsonError("bad_request", "headers must be a JSON object: "+err.Error()))
				}
			}
			return doHTTP(ctx, mod, inv, url, method, headers, body)
		}).
		Export("http_request")

	// connector_get(keyPtr, keyLen) -> ptr|len
	// Resolves the org's credentials for a connector (the v3 `connectors` block)
	// and returns them as a JSON object envelope. Gated by the addon's
	// `connector:read <key>` capability and scoped to the invocation's orgID, so
	// a guest can only read connectors it declared and only for its own org.
	// Returns a JSON error envelope ({"error":...}) on a missing capability,
	// an unconfigured resolver, or an unauthorised/absent connector — guests
	// must handle the error shape.
	b.NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module, keyPtr, keyLen uint32) uint64 {
			inv := invocationFrom(ctx)
			if inv == nil {
				return 0
			}
			key := readString(mod, keyPtr, keyLen)
			if key == "" {
				return writeToGuest(ctx, mod, jsonError("bad_request", "connector key is empty"))
			}
			if err := inv.caps.CanReadConnector(key); err != nil {
				return writeToGuest(ctx, mod, jsonError("forbidden", err.Error()))
			}
			if inv.connectors == nil {
				return writeToGuest(ctx, mod, jsonError("connector_unavailable", "host has no connectors resolver configured"))
			}
			if inv.orgID == uuid.Nil {
				return writeToGuest(ctx, mod, jsonError("no_active_org", "invocation has no bound orgID"))
			}
			creds, err := inv.connectors.Get(ctx, inv.orgID, key)
			if err != nil {
				return writeToGuest(ctx, mod, jsonError("not_found", err.Error()))
			}
			buf, _ := json.Marshal(creds)
			return writeToGuest(ctx, mod, buf)
		}).
		Export("connector_get")

	// event_emit(eventPtr, eventLen, payloadPtr, payloadLen) -> i64
	// Publishes to the in-process events.Bus on behalf of the guest. The
	// return value is always a packed (ptr<<32)|len of a JSON envelope
	// (`{success, data, meta}` on success — `{success:false, error, meta}`
	// on failure) written into guest memory via the guest's `alloc` export.
	// Guests written against the v0.10 ABI that ignored the return value keep
	// working — the envelope is allocated in the guest's own bump arena and
	// the publish side-effect already ran by the time the function returns.
	// See docs/wasm-abi.md § 12.4 for the wire shape and the resolution of
	// the audit ABI v1.0 § 12 inconsistency #4.
	b.NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module,
			eventPtr, eventLen, payloadPtr, payloadLen uint32) uint64 {
			start := time.Now()
			inv := invocationFrom(ctx)
			if inv == nil {
				// No invocation bag — fall back to a minimal error envelope.
				// This path is unreachable in production (every entry through
				// Host.Invoke* sets the bag) but defends against future
				// re-wiring that might forget it.
				return writeToGuest(ctx, mod, eventEmitErr("",
					"bus_unavailable", "invocation context missing",
					uuid.Nil, start))
			}
			if inv.bus == nil {
				return writeToGuest(ctx, mod, eventEmitErr(inv.addonKey,
					"bus_unavailable", "host has no events.Bus configured",
					inv.orgID, start))
			}
			if eventLen == 0 {
				return writeToGuest(ctx, mod, eventEmitErr(inv.addonKey,
					"invalid_event", "event name is empty",
					inv.orgID, start))
			}
			if eventLen > eventNameMaxBytes {
				return writeToGuest(ctx, mod, eventEmitErr(inv.addonKey,
					"invalid_event",
					fmt.Sprintf("event name exceeds %d bytes", eventNameMaxBytes),
					inv.orgID, start))
			}
			nameBytes, ok := mod.Memory().Read(eventPtr, eventLen)
			if !ok {
				return writeToGuest(ctx, mod, eventEmitErr(inv.addonKey,
					"invalid_event", "event name out of guest memory",
					inv.orgID, start))
			}
			if !utf8.Valid(nameBytes) {
				return writeToGuest(ctx, mod, eventEmitErr(inv.addonKey,
					"invalid_event", "event name is not valid UTF-8",
					inv.orgID, start))
			}
			eventName := string(nameBytes)
			if payloadLen > eventPayloadMaxBytes {
				return writeToGuest(ctx, mod, eventEmitErr(inv.addonKey,
					"payload_too_large",
					fmt.Sprintf("payload exceeds %d bytes", eventPayloadMaxBytes),
					inv.orgID, start))
			}
			var payload any
			if payloadLen > 0 {
				body := readBytes(mod, payloadPtr, payloadLen)
				if body == nil {
					return writeToGuest(ctx, mod, eventEmitErr(inv.addonKey,
						"invalid_payload", "payload out of guest memory",
						inv.orgID, start))
				}
				payload = json.RawMessage(body)
			}
			if inv.orgID == uuid.Nil {
				return writeToGuest(ctx, mod, eventEmitErr(inv.addonKey,
					"no_active_org", "invocation has no bound orgID",
					uuid.Nil, start))
			}
			subscribers, err := inv.bus.PublishWithCount(ctx, inv.addonKey, eventName, inv.orgID, payload)
			if err != nil {
				return writeToGuest(ctx, mod, eventEmitErr(inv.addonKey,
					"forbidden", err.Error(), inv.orgID, start))
			}
			return writeToGuest(ctx, mod, eventEmitOK(
				inv.addonKey, eventName, subscribers, inv.orgID, start))
		}).
		Export("event_emit")

	// db_query(sqlPtr, sqlLen, argsPtr, argsLen) -> i64 (ptr|len envelope)
	// See docs/wasm-abi.md § 9. The envelope is always populated — guests
	// distinguish success/failure by the JSON `success` flag.
	b.NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module,
			sqlPtr, sqlLen, argsPtr, argsLen uint32) uint64 {
			inv := invocationFrom(ctx)
			if inv == nil {
				return 0
			}
			sqlText := readString(mod, sqlPtr, sqlLen)
			argsJSON := readBytes(mod, argsPtr, argsLen)
			env := executeDBQuery(ctx, inv.db, inv.addonKey, inv.enforcer, sqlText, argsJSON)
			return writeToGuest(ctx, mod, env)
		}).
		Export("db_query")

	// db_exec(sqlPtr, sqlLen, argsPtr, argsLen) -> i64 (ptr|len envelope)
	// Mirrors db_query but for mutating SQL (INSERT/UPDATE/DELETE/MERGE),
	// gated by `db:write addon_<key>.*`. When the host entered through
	// InvokeInTx the import runs on `inv.tx` so the guest's writes commit
	// or rollback with the surrounding action transaction; otherwise it
	// opens its own short-lived transaction on `inv.db`.
	b.NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module,
			sqlPtr, sqlLen, argsPtr, argsLen uint32) uint64 {
			inv := invocationFrom(ctx)
			if inv == nil {
				return 0
			}
			sqlText := readString(mod, sqlPtr, sqlLen)
			argsJSON := readBytes(mod, argsPtr, argsLen)
			// search_path schema: the addon's isolated schema by default, or
			// the embedder's override (Host.WithExecSchema) — e.g. ops routes
			// bare names to `public` where its live declarative rows are. The
			// capability gate inside executeDBExec still uses AddonSchema.
			searchSchema := AddonSchema(inv.addonKey)
			if inv.execSchema != nil {
				if s := inv.execSchema(inv.addonKey); s != "" {
					searchSchema = s
				}
			}
			env := executeDBExec(ctx, inv.tx, inv.db, inv.addonKey, searchSchema, inv.enforcer, sqlText, argsJSON)
			return writeToGuest(ctx, mod, env)
		}).
		Export("db_exec")

	// data_mutate(reqPtr, reqLen) -> i64 (ptr|len envelope)
	// One org-scoped row mutation (create/update/delete) against a LOGICAL
	// table resolved through the embedder's TableResolver, followed by a
	// post-commit *dynamic.CanonicalEvent on the host bus. Tenant scope
	// (orgID) is taken from the invocation context — never from the guest —
	// exactly like event_emit. Gated by `db:write <logical table>`.
	// See docs/wasm-abi.md § 14.
	b.NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module,
			reqPtr, reqLen uint32) uint64 {
			inv := invocationFrom(ctx)
			if inv == nil {
				return 0
			}
			req := readBytes(mod, reqPtr, reqLen)
			env := executeDataMutate(ctx, inv, req)
			return writeToGuest(ctx, mod, env)
		}).
		Export("data_mutate")

	// data_query(reqPtr, reqLen) -> i64 (ptr|len envelope)
	// Read-only sibling of data_mutate: one org-scoped, equality-filtered
	// SELECT against a LOGICAL table resolved through the embedder's
	// TableResolver (NOT the addon-schema search_path db_query scopes to —
	// shadow schemas hold no live rows in embedding hosts). Tenant scope
	// comes from the invocation context; soft-deleted rows are filtered out
	// automatically. Gated by `db:read <logical table>`. No events.
	// See docs/wasm-abi.md § 15.
	b.NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module,
			reqPtr, reqLen uint32) uint64 {
			inv := invocationFrom(ctx)
			if inv == nil {
				return 0
			}
			req := readBytes(mod, reqPtr, reqLen)
			env := executeDataQueryRecords(ctx, inv, req)
			return writeToGuest(ctx, mod, env)
		}).
		Export("data_query")

	if _, err := b.Instantiate(ctx); err != nil {
		return fmt.Errorf("instantiate metacore_host: %w", err)
	}
	return nil
}

// doHTTP is the shared outbound-HTTP implementation behind the http_fetch and
// http_request imports. It applies the capability gate + SSRF guard (CanFetch)
// before any syscall, sets the custom request headers (when provided), defaults
// a JSON Content-Type for a body unless the caller set one, and returns the
// {status, body} JSON envelope (capped at 8 MiB). headers may be nil.
func doHTTP(ctx context.Context, mod api.Module, inv *invocation, url, method string, headers map[string]string, body []byte) uint64 {
	if method == "" {
		method = http.MethodGet
	}
	if err := inv.caps.CanFetch(url); err != nil {
		return writeToGuest(ctx, mod, jsonError("forbidden", err.Error()))
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return writeToGuest(ctx, mod, jsonError("bad_request", err.Error()))
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if len(body) > 0 && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return writeToGuest(ctx, mod, jsonError("transport", err.Error()))
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20)) // 8 MiB safety cap
	env := map[string]any{
		"status": resp.StatusCode,
		"body":   string(respBody),
	}
	buf, _ := json.Marshal(env)
	return writeToGuest(ctx, mod, buf)
}

func readString(mod api.Module, ptr, n uint32) string {
	b, ok := mod.Memory().Read(ptr, n)
	if !ok {
		return ""
	}
	return string(b)
}

func readBytes(mod api.Module, ptr, n uint32) []byte {
	if n == 0 {
		return nil
	}
	b, ok := mod.Memory().Read(ptr, n)
	if !ok {
		return nil
	}
	cp := make([]byte, len(b))
	copy(cp, b)
	return cp
}

// writeToGuest allocates and writes into the guest, returning the packed
// (ptr<<32)|len. On failure it returns 0 — callers must document that to
// their guest-side bindings.
func writeToGuest(ctx context.Context, mod api.Module, data []byte) uint64 {
	if len(data) == 0 {
		return 0
	}
	ptr, err := writeMem(ctx, mod, data)
	if err != nil {
		return 0
	}
	return packPtrLen(ptr, uint32(len(data)))
}

func jsonError(code, msg string) []byte {
	b, _ := json.Marshal(map[string]any{"error": code, "message": msg})
	return b
}

// compile-time assurance that we only ever bind the runtime kind wazero
// provides (defends against accidental interface drift).
var _ wazero.Runtime = (wazero.Runtime)(nil)
