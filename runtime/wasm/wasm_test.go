package wasm

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/asteby/metacore-kernel/events"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/security"
	"github.com/google/uuid"
)

// TestHost_InvokeEcho exercises the full path: compile -> instantiate ->
// invoke. The guest is a hand-built module that exports alloc + echo; echo
// simply returns the ptr/len it received, packed per our ABI convention.
func TestHost_InvokeEcho(t *testing.T) {
	ctx := context.Background()

	caps := security.Compile("testaddon", nil)
	h, err := NewHost(ctx, caps, nil)
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer h.Close(ctx)

	spec := &manifest.BackendSpec{
		Runtime:       "wasm",
		Entry:         "backend.wasm",
		Exports:       []string{"echo"},
		MemoryLimitMB: 4,
		TimeoutMs:     2000,
	}
	if err := h.Load(ctx, "testaddon", echoWasm(), spec); err != nil {
		t.Fatalf("Load: %v", err)
	}
	payload := []byte("hello metacore")
	out, err := h.Invoke(ctx, uuid.New(), "testaddon", "echo", payload, nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if string(out) != string(payload) {
		t.Fatalf("echo mismatch: got %q want %q", out, payload)
	}
}

func TestHost_InvokeUnknownExport(t *testing.T) {
	ctx := context.Background()
	h, err := NewHost(ctx, security.Compile("testaddon", nil), nil)
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer h.Close(ctx)

	spec := &manifest.BackendSpec{Runtime: "wasm", Entry: "b.wasm", Exports: []string{"echo"}}
	if err := h.Load(ctx, "testaddon", echoWasm(), spec); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := h.Invoke(ctx, uuid.New(), "testaddon", "not_declared", nil, nil); err == nil {
		t.Fatal("expected error invoking undeclared export")
	}
}

// TestHost_InvokeEventEmit drives the metacore_host.event_emit import end to
// end: build a host with an attached events.Bus, register a subscriber, and
// invoke a guest export that publishes "ev.fired" through the host import.
// The test asserts the subscriber observed the publish *and* that the host
// import returned the v1 `{success, data, meta}` envelope documented in
// docs/wasm-abi.md § 12.4 (resolution of audit ABI v1.0 § 12 inconsistency
// #4 — the import used to return literal 0 on success).
func TestHost_InvokeEventEmit(t *testing.T) {
	ctx := context.Background()

	bus := events.NewBus(nil) // nil enforcer → capability check skipped
	var fired int32
	if err := bus.Subscribe("testaddon", "ev.fired", func(_ context.Context, _ uuid.UUID, _ any) error {
		atomic.AddInt32(&fired, 1)
		return nil
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	h, err := NewHost(ctx, security.Compile("testaddon", nil), nil)
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	h.WithBus(bus)
	defer h.Close(ctx)

	spec := &manifest.BackendSpec{
		Runtime:       "wasm",
		Entry:         "backend.wasm",
		Exports:       []string{"emit_test"},
		MemoryLimitMB: 4,
		TimeoutMs:     2000,
	}
	if err := h.Load(ctx, "testaddon", eventEmitWasm(), spec); err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Guest's emit_test ignores its (ptr,len) input and calls
	// event_emit(eventPtr=16, eventLen=8 → "ev.fired", payloadPtr=0,
	// payloadLen=0). It returns the host's i64 directly via its export
	// signature, so Host.InvokeFor decodes the packed ptr|len back into
	// the envelope bytes — assert the v1 shape.
	orgID := uuid.New()
	out, err := h.InvokeFor(ctx, orgID, uuid.New(), "testaddon", "emit_test", nil, nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got := atomic.LoadInt32(&fired); got != 1 {
		t.Fatalf("subscriber not invoked: got=%d want=1", got)
	}
	env := decodeEnvelope(t, out)
	if got, _ := env["success"].(bool); !got {
		t.Fatalf("expected success=true, got envelope: %s", out)
	}
	data, _ := env["data"].(map[string]any)
	if data == nil {
		t.Fatalf("expected data object, got envelope: %s", out)
	}
	if got, _ := data["event"].(string); got != "ev.fired" {
		t.Fatalf("data.event = %q, want %q", got, "ev.fired")
	}
	if got, _ := data["subscribers"].(float64); got != 1 {
		t.Fatalf("data.subscribers = %v, want 1", got)
	}
	meta, _ := env["meta"].(map[string]any)
	if meta == nil {
		t.Fatalf("expected meta object, got envelope: %s", out)
	}
	if got, _ := meta["addon"].(string); got != "testaddon" {
		t.Fatalf("meta.addon = %q, want testaddon", got)
	}
	if got, _ := meta["orgId"].(string); got != orgID.String() {
		t.Fatalf("meta.orgId = %q, want %q", got, orgID)
	}
	if got, _ := meta["envelopeVersion"].(float64); int(got) != EventEmitEnvelopeVersion {
		t.Fatalf("meta.envelopeVersion = %v, want %d", got, EventEmitEnvelopeVersion)
	}
}

// TestHost_InvokeEventEmitBusUnavailable asserts the host import surfaces a
// bus_unavailable error envelope when no events.Bus is attached.
func TestHost_InvokeEventEmitBusUnavailable(t *testing.T) {
	ctx := context.Background()
	h, err := NewHost(ctx, security.Compile("testaddon", nil), nil)
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer h.Close(ctx)

	spec := &manifest.BackendSpec{
		Runtime:   "wasm",
		Entry:     "backend.wasm",
		Exports:   []string{"emit_test"},
		TimeoutMs: 2000,
	}
	if err := h.Load(ctx, "testaddon", eventEmitWasm(), spec); err != nil {
		t.Fatalf("Load: %v", err)
	}
	out, err := h.InvokeFor(ctx, uuid.New(), uuid.New(), "testaddon", "emit_test", nil, nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("expected bus_unavailable envelope, got empty success")
	}
	if want := `"bus_unavailable"`; !strings.Contains(string(out), want) {
		t.Fatalf("expected error envelope to contain %s, got %q", want, out)
	}
	env := decodeEnvelope(t, out)
	if got, _ := env["success"].(bool); got {
		t.Fatalf("expected success=false, got envelope: %s", out)
	}
	errObj, _ := env["error"].(map[string]any)
	if errObj == nil {
		t.Fatalf("expected error object, got envelope: %s", out)
	}
	if got, _ := errObj["code"].(string); got != "bus_unavailable" {
		t.Fatalf("error.code = %q, want bus_unavailable", got)
	}
}

// TestHost_InvokeEventEmitNoActiveOrg covers the wiring-bug case the doc
// surfaces as `no_active_org` (docs/wasm-abi.md § 12.6). The bus is wired
// but the caller used Invoke (not InvokeFor) → orgID stays uuid.Nil →
// envelope reports the misconfiguration without panicking.
func TestHost_InvokeEventEmitNoActiveOrg(t *testing.T) {
	ctx := context.Background()
	bus := events.NewBus(nil)
	h, err := NewHost(ctx, security.Compile("testaddon", nil), nil)
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	h.WithBus(bus)
	defer h.Close(ctx)
	spec := &manifest.BackendSpec{
		Runtime: "wasm", Entry: "b.wasm",
		Exports: []string{"emit_test"}, TimeoutMs: 2000,
	}
	if err := h.Load(ctx, "testaddon", eventEmitWasm(), spec); err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Plain Invoke leaves orgID at uuid.Nil — the import should refuse to
	// publish under a nil tenant.
	out, err := h.Invoke(ctx, uuid.New(), "testaddon", "emit_test", nil, nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	env := decodeEnvelope(t, out)
	if got, _ := env["success"].(bool); got {
		t.Fatalf("expected success=false, got envelope: %s", out)
	}
	errObj, _ := env["error"].(map[string]any)
	if got, _ := errObj["code"].(string); got != "no_active_org" {
		t.Fatalf("error.code = %q, want no_active_org", got)
	}
	// meta.orgId must be omitted when orgID is uuid.Nil so telemetry
	// doesn't ingest the all-zero UUID as a tenant.
	meta, _ := env["meta"].(map[string]any)
	if _, present := meta["orgId"]; present {
		t.Fatalf("meta.orgId must be omitted on no_active_org, got envelope: %s", out)
	}
}

// TestHost_InvokeEventEmitOrgIsolation asserts a subscriber that filters by
// orgID receives only events from its own tenant. This is the multi-tenant
// scenario the v0.10.2 fix unblocks: before the fix every publish sailed
// out with uuid.Nil, so an org-aware subscriber saw zero matches.
func TestHost_InvokeEventEmitOrgIsolation(t *testing.T) {
	ctx := context.Background()

	bus := events.NewBus(nil)
	orgA := uuid.New()
	orgB := uuid.New()

	var receivedForA int32
	var receivedForB int32
	if err := bus.Subscribe("testaddon", "ev.fired", func(_ context.Context, orgID uuid.UUID, _ any) error {
		switch orgID {
		case orgA:
			atomic.AddInt32(&receivedForA, 1)
		case orgB:
			atomic.AddInt32(&receivedForB, 1)
		}
		return nil
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	h, err := NewHost(ctx, security.Compile("testaddon", nil), nil)
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	h.WithBus(bus)
	defer h.Close(ctx)

	spec := &manifest.BackendSpec{
		Runtime:   "wasm",
		Entry:     "backend.wasm",
		Exports:   []string{"emit_test"},
		TimeoutMs: 2000,
	}
	if err := h.Load(ctx, "testaddon", eventEmitWasm(), spec); err != nil {
		t.Fatalf("Load: %v", err)
	}

	if _, err := h.InvokeFor(ctx, orgA, uuid.New(), "testaddon", "emit_test", nil, nil); err != nil {
		t.Fatalf("InvokeFor(orgA): %v", err)
	}
	if _, err := h.InvokeFor(ctx, orgB, uuid.New(), "testaddon", "emit_test", nil, nil); err != nil {
		t.Fatalf("InvokeFor(orgB): %v", err)
	}
	if _, err := h.InvokeFor(ctx, orgA, uuid.New(), "testaddon", "emit_test", nil, nil); err != nil {
		t.Fatalf("InvokeFor(orgA) 2nd: %v", err)
	}

	if got, want := atomic.LoadInt32(&receivedForA), int32(2); got != want {
		t.Fatalf("orgA: got=%d want=%d", got, want)
	}
	if got, want := atomic.LoadInt32(&receivedForB), int32(1); got != want {
		t.Fatalf("orgB: got=%d want=%d", got, want)
	}
}

// TestHost_InvokeEventEmitLegacyGuestIgnoresReturn asserts the additive
// nature of the envelope: a guest that ignores the i64 return value still
// publishes successfully — the side effect (subscriber fan-out) happens
// before the host writes the envelope into guest memory, so dropping the
// return value on the floor is harmless. This is the wire-compat guarantee
// the audit ABI v1.0 § 12 inconsistency #4 resolution leans on.
func TestHost_InvokeEventEmitLegacyGuestIgnoresReturn(t *testing.T) {
	ctx := context.Background()

	bus := events.NewBus(nil)
	var fired int32
	if err := bus.Subscribe("testaddon", "ev.fired", func(_ context.Context, _ uuid.UUID, _ any) error {
		atomic.AddInt32(&fired, 1)
		return nil
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	h, err := NewHost(ctx, security.Compile("testaddon", nil), nil)
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	h.WithBus(bus)
	defer h.Close(ctx)

	spec := &manifest.BackendSpec{
		Runtime:   "wasm",
		Entry:     "backend.wasm",
		Exports:   []string{"emit_drop"},
		TimeoutMs: 2000,
	}
	// emitDropWasm is the legacy-guest analogue: it calls event_emit,
	// `drop`s the i64 return, and ends with i64.const 0 — matching the
	// pre-envelope contract guests were written against.
	if err := h.Load(ctx, "testaddon", emitDropWasm(), spec); err != nil {
		t.Fatalf("Load: %v", err)
	}
	out, err := h.InvokeFor(ctx, uuid.New(), uuid.New(), "testaddon", "emit_drop", nil, nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("legacy guest should still return empty result, got %q", out)
	}
	if got := atomic.LoadInt32(&fired); got != 1 {
		t.Fatalf("subscriber not invoked under legacy guest: got=%d want=1", got)
	}
}

// decodeEnvelope unmarshals an event_emit envelope, failing the test on
// malformed JSON. Centralised so the assertions stay readable.
func decodeEnvelope(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("envelope is not valid JSON: %v (%q)", err, raw)
	}
	return env
}

func TestHost_LoadRequiresWasmRuntime(t *testing.T) {
	ctx := context.Background()
	h, err := NewHost(ctx, security.Compile("k", nil), nil)
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer h.Close(ctx)
	err = h.Load(ctx, "k", []byte{0}, &manifest.BackendSpec{Runtime: "webhook"})
	if err == nil {
		t.Fatal("expected error for non-wasm runtime")
	}
}

// ---------------------------------------------------------------------------
// Hand-built WASM module. Encodes, in WebAssembly binary format, a module
// with one memory (1 page), a mutable global "bump" allocator, and two
// exports:
//
//	alloc(size i32) -> i32  : returns current bump ptr, then advances it.
//	echo(ptr i32, len i32) -> i64 : returns (ptr<<32)|len.
//
// Keeping the module trivial lets the test exercise the ABI + wazero
// integration without depending on wat2wasm at build time.
// ---------------------------------------------------------------------------
func echoWasm() []byte {
	var buf []byte
	// Magic + version.
	buf = append(buf, 0x00, 0x61, 0x73, 0x6D, 0x01, 0x00, 0x00, 0x00)

	// Type section: two function signatures.
	//   type 0: (i32) -> i32        for alloc
	//   type 1: (i32, i32) -> i64   for echo
	types := []byte{
		0x02, // count
		0x60, 0x01, 0x7F, 0x01, 0x7F, // (i32) -> (i32)
		0x60, 0x02, 0x7F, 0x7F, 0x01, 0x7E, // (i32,i32) -> (i64)
	}
	buf = append(buf, section(0x01, types)...)

	// Function section: two funcs, typeidx 0 and 1.
	funcs := []byte{0x02, 0x00, 0x01}
	buf = append(buf, section(0x03, funcs)...)

	// Memory section: one memory with min=1 page, no max.
	mem := []byte{0x01, 0x00, 0x01}
	buf = append(buf, section(0x05, mem)...)

	// Global section: one mutable i32 initialised to 1024 (keep low addrs
	// free). init expr: i32.const 1024 end.
	globals := []byte{
		0x01,       // count
		0x7F, 0x01, // i32, mut
	}
	globals = append(globals, 0x41)                // i32.const
	globals = append(globals, encodeSLEB128(1024)...) // 1024
	globals = append(globals, 0x0B)                // end
	buf = append(buf, section(0x06, globals)...)

	// Export section: memory, alloc, echo.
	var exports []byte
	exports = append(exports, 0x03) // count
	exports = append(exports, encodeName("memory")...)
	exports = append(exports, 0x02, 0x00) // memory, idx 0
	exports = append(exports, encodeName("alloc")...)
	exports = append(exports, 0x00, 0x00) // func, idx 0
	exports = append(exports, encodeName("echo")...)
	exports = append(exports, 0x00, 0x01) // func, idx 1
	buf = append(buf, section(0x07, exports)...)

	// Code section: two function bodies.
	//
	// alloc(size): save global 0 as return, add size, store back.
	//   local.get 0
	//   global.get 0
	//   global.get 0
	//   local.get 0
	//   i32.add
	//   global.set 0
	//   ... return saved? simpler: return old global, but we need to
	//   compute old then store new. Use a local.
	//
	// We'll use 1 local (i32):
	//   (local $old i32)
	//   global.get 0 ; stack: old
	//   local.tee 1  ; old saved to local 1, still on stack
	//   local.get 0  ; size
	//   i32.add
	//   global.set 0
	//   local.get 1  ; return old
	//   end
	allocBody := []byte{
		0x01, 0x01, 0x7F, // one local group of 1 i32
		0x23, 0x00, // global.get 0
		0x22, 0x01, // local.tee 1
		0x20, 0x00, // local.get 0
		0x6A,       // i32.add
		0x24, 0x00, // global.set 0
		0x20, 0x01, // local.get 1
		0x0B, // end
	}
	allocBody = withSize(allocBody)

	// echo(ptr, len): return (i64(ptr) << 32) | i64(len)
	//   local.get 0            ; ptr
	//   i64.extend_i32_u       ; widen
	//   i64.const 32
	//   i64.shl
	//   local.get 1            ; len
	//   i64.extend_i32_u
	//   i64.or
	//   end
	echoBody := []byte{
		0x00,       // no locals
		0x20, 0x00, // local.get 0
		0xAD,       // i64.extend_i32_u
		0x42, 0x20, // i64.const 32 (SLEB128: 0x20)
		0x86,       // i64.shl
		0x20, 0x01, // local.get 1
		0xAD,       // i64.extend_i32_u
		0x84,       // i64.or
		0x0B,       // end
	}
	echoBody = withSize(echoBody)

	var code []byte
	code = append(code, 0x02) // count
	code = append(code, allocBody...)
	code = append(code, echoBody...)
	buf = append(buf, section(0x0A, code)...)

	return buf
}

// section wraps payload with (id, size_uleb128, payload).
func section(id byte, payload []byte) []byte {
	out := []byte{id}
	out = append(out, encodeULEB128(uint32(len(payload)))...)
	out = append(out, payload...)
	return out
}

// withSize prefixes payload with its ULEB128 length — used by code bodies.
func withSize(payload []byte) []byte {
	var out []byte
	out = append(out, encodeULEB128(uint32(len(payload)))...)
	out = append(out, payload...)
	return out
}

func encodeName(s string) []byte {
	out := encodeULEB128(uint32(len(s)))
	out = append(out, []byte(s)...)
	return out
}

func encodeULEB128(v uint32) []byte {
	var out []byte
	for {
		b := byte(v & 0x7F)
		v >>= 7
		if v == 0 {
			out = append(out, b)
			return out
		}
		out = append(out, b|0x80)
	}
}

func encodeSLEB128(v int32) []byte {
	var out []byte
	for {
		b := byte(v & 0x7F)
		v >>= 7
		signBit := b & 0x40
		if (v == 0 && signBit == 0) || (v == -1 && signBit != 0) {
			out = append(out, b)
			return out
		}
		out = append(out, b|0x80)
	}
}

// eventEmitWasm builds a tiny module that imports
// `metacore_host.event_emit` and exports `emit_test(ptr,len) -> i64`.
// `emit_test` ignores its (ptr,len) input and forwards a hard-coded event
// name "ev.fired" (placed at memory offset 16 by an active data segment) to
// the host import; payload is taken straight from the (ptr,len) Host.Invoke
// passes in. Returning the host's i64 verbatim lets the test inspect both
// the success case (0) and the error envelope (ptr|len of JSON).
func eventEmitWasm() []byte {
	var buf []byte
	buf = append(buf, 0x00, 0x61, 0x73, 0x6D, 0x01, 0x00, 0x00, 0x00)

	// Type section: 3 signatures.
	types := []byte{
		0x03,
		0x60, 0x01, 0x7F, 0x01, 0x7F, // (i32) -> i32
		0x60, 0x02, 0x7F, 0x7F, 0x01, 0x7E, // (i32,i32) -> i64
		0x60, 0x04, 0x7F, 0x7F, 0x7F, 0x7F, 0x01, 0x7E, // (i32,i32,i32,i32) -> i64
	}
	buf = append(buf, section(0x01, types)...)

	// Import section: metacore_host.event_emit at func index 0.
	var imports []byte
	imports = append(imports, 0x01)
	imports = append(imports, encodeName("metacore_host")...)
	imports = append(imports, encodeName("event_emit")...)
	imports = append(imports, 0x00, 0x02) // func, typeidx 2
	buf = append(buf, section(0x02, imports)...)

	// Function section: 2 local funcs (alloc=type 0, emit_test=type 1).
	funcs := []byte{0x02, 0x00, 0x01}
	buf = append(buf, section(0x03, funcs)...)

	// Memory section: 1 page, no max.
	mem := []byte{0x01, 0x00, 0x01}
	buf = append(buf, section(0x05, mem)...)

	// Global section: bump pointer at 1024 (above the data segment).
	globals := []byte{
		0x01,
		0x7F, 0x01,
	}
	globals = append(globals, 0x41)
	globals = append(globals, encodeSLEB128(1024)...)
	globals = append(globals, 0x0B)
	buf = append(buf, section(0x06, globals)...)

	// Export section: memory, alloc (func idx 1), emit_test (func idx 2).
	var exports []byte
	exports = append(exports, 0x03)
	exports = append(exports, encodeName("memory")...)
	exports = append(exports, 0x02, 0x00)
	exports = append(exports, encodeName("alloc")...)
	exports = append(exports, 0x00, 0x01)
	exports = append(exports, encodeName("emit_test")...)
	exports = append(exports, 0x00, 0x02)
	buf = append(buf, section(0x07, exports)...)

	// Code section: alloc + emit_test bodies.
	allocBody := []byte{
		0x01, 0x01, 0x7F,
		0x23, 0x00,
		0x22, 0x01,
		0x20, 0x00,
		0x6A,
		0x24, 0x00,
		0x20, 0x01,
		0x0B,
	}
	allocBody = withSize(allocBody)

	// emit_test(ptr, len): event_emit(16, 8, ptr, len).
	emitBody := []byte{
		0x00,
		0x41, 0x10, // i32.const 16   (eventPtr)
		0x41, 0x08, // i32.const 8    (eventLen — "ev.fired" is 8 bytes)
		0x20, 0x00, // local.get 0    (payloadPtr forwarded)
		0x20, 0x01, // local.get 1    (payloadLen forwarded)
		0x10, 0x00, // call 0         (event_emit @ import idx 0)
		0x0B,
	}
	emitBody = withSize(emitBody)

	var code []byte
	code = append(code, 0x02)
	code = append(code, allocBody...)
	code = append(code, emitBody...)
	buf = append(buf, section(0x0A, code)...)

	// Data section: active segment writes "ev.fired" at memory offset 16.
	var data []byte
	data = append(data, 0x01) // count
	data = append(data, 0x00) // active, memidx 0
	data = append(data, 0x41)
	data = append(data, encodeSLEB128(16)...)
	data = append(data, 0x0B)
	data = append(data, encodeULEB128(8)...)
	data = append(data, []byte("ev.fired")...)
	buf = append(buf, section(0x0B, data)...)

	return buf
}

// emitDropWasm is the "legacy guest" variant of eventEmitWasm. The export
// `emit_drop(ptr,len) -> i64` calls event_emit but `drop`s the i64 return
// value and ends with `i64.const 0`, matching the pre-envelope contract
// guests were written against. Used by
// TestHost_InvokeEventEmitLegacyGuestIgnoresReturn to assert the envelope
// rollout is additive — old guests keep working.
func emitDropWasm() []byte {
	var buf []byte
	buf = append(buf, 0x00, 0x61, 0x73, 0x6D, 0x01, 0x00, 0x00, 0x00)

	// Type section: same three signatures as eventEmitWasm.
	types := []byte{
		0x03,
		0x60, 0x01, 0x7F, 0x01, 0x7F, // (i32) -> i32
		0x60, 0x02, 0x7F, 0x7F, 0x01, 0x7E, // (i32,i32) -> i64
		0x60, 0x04, 0x7F, 0x7F, 0x7F, 0x7F, 0x01, 0x7E, // (i32,i32,i32,i32) -> i64
	}
	buf = append(buf, section(0x01, types)...)

	// Import section: metacore_host.event_emit at func index 0.
	var imports []byte
	imports = append(imports, 0x01)
	imports = append(imports, encodeName("metacore_host")...)
	imports = append(imports, encodeName("event_emit")...)
	imports = append(imports, 0x00, 0x02)
	buf = append(buf, section(0x02, imports)...)

	// Function section: alloc (type 0) + emit_drop (type 1).
	funcs := []byte{0x02, 0x00, 0x01}
	buf = append(buf, section(0x03, funcs)...)

	// Memory: 1 page.
	mem := []byte{0x01, 0x00, 0x01}
	buf = append(buf, section(0x05, mem)...)

	// Bump pointer global at 1024.
	globals := []byte{0x01, 0x7F, 0x01}
	globals = append(globals, 0x41)
	globals = append(globals, encodeSLEB128(1024)...)
	globals = append(globals, 0x0B)
	buf = append(buf, section(0x06, globals)...)

	// Exports: memory, alloc, emit_drop.
	var exports []byte
	exports = append(exports, 0x03)
	exports = append(exports, encodeName("memory")...)
	exports = append(exports, 0x02, 0x00)
	exports = append(exports, encodeName("alloc")...)
	exports = append(exports, 0x00, 0x01)
	exports = append(exports, encodeName("emit_drop")...)
	exports = append(exports, 0x00, 0x02)
	buf = append(buf, section(0x07, exports)...)

	// Code: alloc (identical to eventEmitWasm) + emit_drop.
	allocBody := []byte{
		0x01, 0x01, 0x7F,
		0x23, 0x00,
		0x22, 0x01,
		0x20, 0x00,
		0x6A,
		0x24, 0x00,
		0x20, 0x01,
		0x0B,
	}
	allocBody = withSize(allocBody)

	// emit_drop(ptr, len): event_emit(16, 8, ptr, len), drop, i64.const 0.
	//   The `drop` (0x1A) pops the i64 envelope ptr|len returned by
	//   event_emit — the legacy guest deliberately ignores it. The export
	//   signature still requires an i64 result, so we return `i64.const 0`
	//   which Host.Invoke decodes as an empty []byte (success/empty).
	emitBody := []byte{
		0x00,
		0x41, 0x10, // i32.const 16
		0x41, 0x08, // i32.const 8
		0x20, 0x00, // local.get 0
		0x20, 0x01, // local.get 1
		0x10, 0x00, // call 0 (event_emit)
		0x1A,       // drop
		0x42, 0x00, // i64.const 0
		0x0B,
	}
	emitBody = withSize(emitBody)

	var code []byte
	code = append(code, 0x02)
	code = append(code, allocBody...)
	code = append(code, emitBody...)
	buf = append(buf, section(0x0A, code)...)

	// Data: "ev.fired" at offset 16.
	var data []byte
	data = append(data, 0x01)
	data = append(data, 0x00)
	data = append(data, 0x41)
	data = append(data, encodeSLEB128(16)...)
	data = append(data, 0x0B)
	data = append(data, encodeULEB128(8)...)
	data = append(data, []byte("ev.fired")...)
	buf = append(buf, section(0x0B, data)...)

	return buf
}
