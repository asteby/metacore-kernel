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
	// Enforces Capabilities.CanFetch *before* any syscall happens, so the
	// SSRF guard applies uniformly to webhook and wasm backends.
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
			if len(body) > 0 {
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
		}).
		Export("http_fetch")

	// event_emit(eventPtr, eventLen, payloadPtr, payloadLen) -> i64
	// Publishes to the in-process events.Bus on behalf of the guest. Returns 0
	// on a successful publish (subscribers, if any, ran synchronously inside
	// Bus.Publish). On failure returns a packed (ptr<<32)|len of a JSON
	// {"error","message"} envelope written into guest memory — the guest
	// inspects len != 0 to detect failure.
	b.NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module,
			eventPtr, eventLen, payloadPtr, payloadLen uint32) uint64 {
			inv := invocationFrom(ctx)
			if inv == nil {
				return 0
			}
			if inv.bus == nil {
				return writeToGuest(ctx, mod, jsonError("bus_unavailable",
					"host has no events.Bus configured"))
			}
			if eventLen == 0 {
				return writeToGuest(ctx, mod, jsonError("invalid_event",
					"event name is empty"))
			}
			if eventLen > eventNameMaxBytes {
				return writeToGuest(ctx, mod, jsonError("invalid_event",
					fmt.Sprintf("event name exceeds %d bytes", eventNameMaxBytes)))
			}
			nameBytes, ok := mod.Memory().Read(eventPtr, eventLen)
			if !ok {
				return writeToGuest(ctx, mod, jsonError("invalid_event",
					"event name out of guest memory"))
			}
			if !utf8.Valid(nameBytes) {
				return writeToGuest(ctx, mod, jsonError("invalid_event",
					"event name is not valid UTF-8"))
			}
			eventName := string(nameBytes)
			if payloadLen > eventPayloadMaxBytes {
				return writeToGuest(ctx, mod, jsonError("payload_too_large",
					fmt.Sprintf("payload exceeds %d bytes", eventPayloadMaxBytes)))
			}
			var payload any
			if payloadLen > 0 {
				body := readBytes(mod, payloadPtr, payloadLen)
				if body == nil {
					return writeToGuest(ctx, mod, jsonError("invalid_payload",
						"payload out of guest memory"))
				}
				payload = json.RawMessage(body)
			}
			if err := inv.bus.Publish(ctx, inv.addonKey, eventName, inv.orgID, payload); err != nil {
				return writeToGuest(ctx, mod, jsonError("forbidden", err.Error()))
			}
			return 0
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
			env := executeDBExec(ctx, inv.tx, inv.db, inv.addonKey, inv.enforcer, sqlText, argsJSON)
			return writeToGuest(ctx, mod, env)
		}).
		Export("db_exec")

	if _, err := b.Instantiate(ctx); err != nil {
		return fmt.Errorf("instantiate metacore_host: %w", err)
	}
	return nil
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
