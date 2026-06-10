package dispatch_test

// This file hand-assembles a minimal WebAssembly module used as the smoke
// fixture for the dispatcher E2E. The module imports two host functions —
// `metacore_host.db_exec` and `metacore_host.event_emit` — and exports a single
// `on_event(ptr,len) -> i64` that, when invoked by the dispatcher on a matched
// subscription:
//
//  1. calls db_exec("UPDATE stock SET qty = qty - 1 WHERE id = $1", ["row-1"])
//     — mutating a test table (asserted via sqlmock expectations), and
//  2. calls event_emit("stocker.recomputed", "{}") — emitting a secondary
//     domain event (asserted via a bus subscriber).
//
// It then returns i64 0 (an empty result the dispatcher treats as a clean
// delivery). The module is built in the same hand-rolled WASM-binary style the
// runtime/wasm package's own tests use, so the smoke needs no wat2wasm / build
// toolchain. The (ptr,len) input is ignored — all strings live in active data
// segments at fixed offsets.
//
// Memory layout (active data segments in linear memory page 0):
//
//	offset  16: SQL text       "UPDATE stock SET qty = qty - 1 WHERE id = $1"
//	offset 128: args JSON      "[\"row-1\"]"
//	offset 160: event name     "stocker.recomputed"
//	offset 192: emit payload   "{}"
//
// The bump allocator global starts at 1024, clear of all data segments.
func dbExecEmitWasm() []byte {
	const (
		sqlOff     = 16
		argsOff    = 128
		eventOff   = 160
		payloadOff = 192
	)
	sqlText := []byte("UPDATE stock SET qty = qty - 1 WHERE id = $1")
	argsJSON := []byte(`["row-1"]`)
	eventName := []byte("stocker.recomputed")
	payload := []byte("{}")

	var buf []byte
	buf = append(buf, 0x00, 0x61, 0x73, 0x6D, 0x01, 0x00, 0x00, 0x00) // magic+version

	// Type section: 2 signatures.
	//   type 0: (i32) -> i32                       (alloc)
	//   type 1: (i32,i32,i32,i32) -> i64           (db_exec / event_emit / on_event uses type 2)
	//   type 2: (i32,i32) -> i64                   (on_event)
	types := []byte{
		0x03,
		0x60, 0x01, 0x7F, 0x01, 0x7F, // (i32)->i32
		0x60, 0x04, 0x7F, 0x7F, 0x7F, 0x7F, 0x01, 0x7E, // (i32,i32,i32,i32)->i64
		0x60, 0x02, 0x7F, 0x7F, 0x01, 0x7E, // (i32,i32)->i64
	}
	buf = append(buf, section(0x01, types)...)

	// Import section: db_exec (func idx 0), event_emit (func idx 1), both type 1.
	var imports []byte
	imports = append(imports, 0x02) // count
	imports = append(imports, encodeName("metacore_host")...)
	imports = append(imports, encodeName("db_exec")...)
	imports = append(imports, 0x00, 0x01) // func, typeidx 1
	imports = append(imports, encodeName("metacore_host")...)
	imports = append(imports, encodeName("event_emit")...)
	imports = append(imports, 0x00, 0x01) // func, typeidx 1
	buf = append(buf, section(0x02, imports)...)

	// Function section: 2 local funcs — alloc (type 0) at func idx 2,
	// on_event (type 2) at func idx 3.
	funcs := []byte{0x02, 0x00, 0x02}
	buf = append(buf, section(0x03, funcs)...)

	// Memory section: 1 page, no max.
	mem := []byte{0x01, 0x00, 0x01}
	buf = append(buf, section(0x05, mem)...)

	// Global section: bump pointer at 1024.
	globals := []byte{0x01, 0x7F, 0x01}
	globals = append(globals, 0x41)
	globals = append(globals, encodeSLEB128(1024)...)
	globals = append(globals, 0x0B)
	buf = append(buf, section(0x06, globals)...)

	// Export section: memory, alloc (func idx 2), on_event (func idx 3).
	var exports []byte
	exports = append(exports, 0x03)
	exports = append(exports, encodeName("memory")...)
	exports = append(exports, 0x02, 0x00)
	exports = append(exports, encodeName("alloc")...)
	exports = append(exports, 0x00, 0x02)
	exports = append(exports, encodeName("on_event")...)
	exports = append(exports, 0x00, 0x03)
	buf = append(buf, section(0x07, exports)...)

	// Code section: alloc + on_event.
	allocBody := []byte{
		0x01, 0x01, 0x7F, // one i32 local
		0x23, 0x00, // global.get 0
		0x22, 0x01, // local.tee 1
		0x20, 0x00, // local.get 0
		0x6A,       // i32.add
		0x24, 0x00, // global.set 0
		0x20, 0x01, // local.get 1
		0x0B,
	}
	allocBody = withSize(allocBody)

	// on_event(ptr,len):
	//   db_exec(sqlOff, len(sql), argsOff, len(args)) ; drop i64
	//   event_emit(eventOff, len(event), payloadOff, len(payload)) ; drop i64
	//   i64.const 0
	var ob []byte
	ob = append(ob, 0x00) // no locals
	// db_exec
	ob = append(ob, 0x41)
	ob = append(ob, encodeSLEB128(sqlOff)...)
	ob = append(ob, 0x41)
	ob = append(ob, encodeSLEB128(int32(len(sqlText)))...)
	ob = append(ob, 0x41)
	ob = append(ob, encodeSLEB128(argsOff)...)
	ob = append(ob, 0x41)
	ob = append(ob, encodeSLEB128(int32(len(argsJSON)))...)
	ob = append(ob, 0x10, 0x00) // call 0 (db_exec)
	ob = append(ob, 0x1A)       // drop
	// event_emit
	ob = append(ob, 0x41)
	ob = append(ob, encodeSLEB128(eventOff)...)
	ob = append(ob, 0x41)
	ob = append(ob, encodeSLEB128(int32(len(eventName)))...)
	ob = append(ob, 0x41)
	ob = append(ob, encodeSLEB128(payloadOff)...)
	ob = append(ob, 0x41)
	ob = append(ob, encodeSLEB128(int32(len(payload)))...)
	ob = append(ob, 0x10, 0x01) // call 1 (event_emit)
	ob = append(ob, 0x1A)       // drop
	ob = append(ob, 0x42, 0x00) // i64.const 0
	ob = append(ob, 0x0B)       // end
	onEventBody := withSize(ob)

	var code []byte
	code = append(code, 0x02) // count
	code = append(code, allocBody...)
	code = append(code, onEventBody...)
	buf = append(buf, section(0x0A, code)...)

	// Data section: 4 active segments.
	var data []byte
	data = append(data, 0x04) // count
	data = append(data, activeData(sqlOff, sqlText)...)
	data = append(data, activeData(argsOff, argsJSON)...)
	data = append(data, activeData(eventOff, eventName)...)
	data = append(data, activeData(payloadOff, payload)...)
	buf = append(buf, section(0x0B, data)...)

	return buf
}

// activeData encodes one active data segment writing `b` at memory `offset`.
func activeData(offset int32, b []byte) []byte {
	var out []byte
	out = append(out, 0x00) // active, memidx 0
	out = append(out, 0x41) // i32.const offset
	out = append(out, encodeSLEB128(offset)...)
	out = append(out, 0x0B) // end of offset expr
	out = append(out, encodeULEB128(uint32(len(b)))...)
	out = append(out, b...)
	return out
}

// ---------------------------------------------------------------------------
// WASM binary-encoding helpers (mirrors runtime/wasm/wasm_test.go so the
// dispatch package's smoke fixture is self-contained).
// ---------------------------------------------------------------------------

func section(id byte, payload []byte) []byte {
	out := []byte{id}
	out = append(out, encodeULEB128(uint32(len(payload)))...)
	out = append(out, payload...)
	return out
}

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
