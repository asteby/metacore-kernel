package wasm

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/events"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/security"
	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// encodeSLEB64 is the signed LEB128 of an i64.const operand.
func encodeSLEB64(v int64) []byte {
	var out []byte
	for {
		b := byte(v & 0x7F)
		v >>= 7
		sign := b & 0x40
		if (v == 0 && sign == 0) || (v == -1 && sign != 0) {
			return append(out, b)
		}
		out = append(out, b|0x80)
	}
}

// emitThenWasm is eventEmitWasm's sibling: `emit_test(ptr,len)` calls
// event_emit("ev.fired", payload), DROPS the host's answer, and then either
// returns a fixed answer of its own (tail) or traps (tail == nil). That is the
// shape of a real guest that emits mid-way and then succeeds or fails.
func emitThenWasm(tail []byte) []byte {
	var buf []byte
	buf = append(buf, 0x00, 0x61, 0x73, 0x6D, 0x01, 0x00, 0x00, 0x00)
	buf = append(buf, section(0x01, []byte{
		0x03,
		0x60, 0x01, 0x7F, 0x01, 0x7F,
		0x60, 0x02, 0x7F, 0x7F, 0x01, 0x7E,
		0x60, 0x04, 0x7F, 0x7F, 0x7F, 0x7F, 0x01, 0x7E,
	})...)
	var imports []byte
	imports = append(imports, 0x01)
	imports = append(imports, encodeName("metacore_host")...)
	imports = append(imports, encodeName("event_emit")...)
	imports = append(imports, 0x00, 0x02)
	buf = append(buf, section(0x02, imports)...)
	buf = append(buf, section(0x03, []byte{0x02, 0x00, 0x01})...)
	buf = append(buf, section(0x05, []byte{0x01, 0x00, 0x01})...)
	globals := []byte{0x01, 0x7F, 0x01, 0x41}
	globals = append(globals, encodeSLEB128(1024)...)
	globals = append(globals, 0x0B)
	buf = append(buf, section(0x06, globals)...)
	var exports []byte
	exports = append(exports, 0x03)
	exports = append(exports, encodeName("memory")...)
	exports = append(exports, 0x02, 0x00)
	exports = append(exports, encodeName("alloc")...)
	exports = append(exports, 0x00, 0x01)
	exports = append(exports, encodeName("emit_test")...)
	exports = append(exports, 0x00, 0x02)
	buf = append(buf, section(0x07, exports)...)

	allocBody := withSize([]byte{0x01, 0x01, 0x7F, 0x23, 0x00, 0x22, 0x01, 0x20, 0x00, 0x6A, 0x24, 0x00, 0x20, 0x01, 0x0B})
	emit := []byte{0x00, 0x41, 0x10, 0x41, 0x08, 0x20, 0x00, 0x20, 0x01, 0x10, 0x00, 0x1A} // call + drop
	if tail == nil {
		emit = append(emit, 0x00) // unreachable
	} else {
		emit = append(emit, 0x42)
		emit = append(emit, encodeSLEB64(int64(64)<<32|int64(len(tail)))...)
	}
	emit = append(emit, 0x0B)
	code := append([]byte{0x02}, allocBody...)
	code = append(code, withSize(emit)...)
	buf = append(buf, section(0x0A, code)...)

	data := []byte{0x02}
	data = append(data, 0x00, 0x41)
	data = append(data, encodeSLEB128(16)...)
	data = append(data, 0x0B)
	data = append(data, encodeULEB128(8)...)
	data = append(data, []byte("ev.fired")...)
	data = append(data, 0x00, 0x41)
	data = append(data, encodeSLEB128(64)...)
	data = append(data, 0x0B)
	data = append(data, encodeULEB128(uint32(len(tail)))...)
	data = append(data, tail...)
	buf = append(buf, section(0x0B, data)...)
	return buf
}

type commitHarness struct {
	h     *Host
	db    *gorm.DB
	fired atomic.Int32
	mu    sync.Mutex
	metas []dynamic.Meta
}

func newCommitHarness(t *testing.T, tail []byte, pub func(db *gorm.DB, bus *events.Bus) *dynamic.OutboxPublisher) *commitHarness {
	t.Helper()
	ctx := context.Background()
	x := &commitHarness{}
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:emitcommit_%s?mode=memory&cache=shared&_busy_timeout=5000", uuid.NewString())),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := dynamic.MigrateEventOutbox(db); err != nil {
		t.Fatal(err)
	}
	x.db = db
	bus := events.NewBus(nil)
	_ = bus.SubscribeEvent("kernel", "ev.fired", func(ctx context.Context, _ uuid.UUID, _ string, _ any) error {
		x.fired.Add(1)
		x.mu.Lock()
		x.metas = append(x.metas, dynamic.MetaFromContext(ctx))
		x.mu.Unlock()
		return nil
	})
	h, err := NewHost(ctx, security.Compile("testaddon", nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close(ctx) })
	h.WithBus(bus)
	if pub != nil {
		h.WithEventPublisher(pub(db, bus))
	} else {
		h.WithEventPublisher(dynamic.NewOutboxPublisher(db, bus))
	}
	spec := &manifest.BackendSpec{Runtime: "wasm", Entry: "backend.wasm", Exports: []string{"emit_test"}, MemoryLimitMB: 4, TimeoutMs: 2000}
	if err := h.Load(ctx, "testaddon", emitThenWasm(tail), spec); err != nil {
		t.Fatalf("Load: %v", err)
	}
	x.h = h
	return x
}

func (x *commitHarness) rows(t *testing.T) []dynamic.EventOutbox {
	t.Helper()
	var rows []dynamic.EventOutbox
	if err := x.db.Order("created_at, sequence").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	return rows
}

const aggPayload = `{"aggregate":{"type":"SalesOrder","id":"so-1"},"n":1}`

func TestEmitOnCommit_SuccessPersistsThenPublishes(t *testing.T) {
	x := newCommitHarness(t, []byte(`{"success":true}`), nil)
	org := uuid.New()
	for i := 1; i <= 2; i++ {
		if _, err := x.h.InvokeFor(context.Background(), org, uuid.New(), "testaddon", "emit_test", []byte(aggPayload), nil); err != nil {
			t.Fatal(err)
		}
		if got := int(x.fired.Load()); got != i {
			t.Fatalf("after invocation %d: fired=%d", i, got)
		}
	}
	rows := x.rows(t)
	if len(rows) != 2 {
		t.Fatalf("outbox rows = %d, want 2", len(rows))
	}
	for i, r := range rows {
		if r.PublishedAt == nil {
			t.Fatalf("row %d not stamped published", i)
		}
		if r.AggregateType != "SalesOrder" || r.AggregateID != "so-1" || r.Sequence != int64(i+1) {
			t.Fatalf("row %d aggregate/sequence = %s/%s/%d", i, r.AggregateType, r.AggregateID, r.Sequence)
		}
		if r.EventID == "" || r.AddonKey != "testaddon" || r.EventName != "ev.fired" {
			t.Fatalf("row %d identity: %+v", i, r)
		}
	}
	// The subscriber's ctx carries the SAME identity that was persisted.
	if x.metas[1].ID != rows[1].EventID || x.metas[1].Sequence != 2 {
		t.Fatalf("published meta %+v does not match persisted row %+v", x.metas[1], rows[1])
	}
}

func TestEmitOnCommit_GuestFailureEmitsNothing(t *testing.T) {
	x := newCommitHarness(t, []byte(`{"success":false,"error":{"code":"x","message":"boom"}}`), nil)
	if _, err := x.h.InvokeFor(context.Background(), uuid.New(), uuid.New(), "testaddon", "emit_test", []byte(aggPayload), nil); err != nil {
		t.Fatal(err)
	}
	if n := x.fired.Load(); n != 0 {
		t.Fatalf("a failed invocation published %d event(s)", n)
	}
	if rows := x.rows(t); len(rows) != 0 {
		t.Fatalf("a failed invocation left %d outbox row(s): %+v", len(rows), rows)
	}
}

func TestEmitOnCommit_TrapEmitsNothing(t *testing.T) {
	x := newCommitHarness(t, nil, nil)
	if _, err := x.h.InvokeFor(context.Background(), uuid.New(), uuid.New(), "testaddon", "emit_test", []byte(aggPayload), nil); err == nil {
		t.Fatal("expected the trap to surface as an error")
	}
	if n := x.fired.Load(); n != 0 {
		t.Fatalf("a trapped invocation published %d event(s)", n)
	}
	if rows := x.rows(t); len(rows) != 0 {
		t.Fatalf("a trapped invocation left %d outbox row(s)", len(rows))
	}
}

// A crash between the outbox write and the publication leaves the event
// durable and UNPUBLISHED; the relay (see dynamic TestOutbox_GuestEventCrash...)
// publishes it. Here: the staged event is on disk with its full identity.
func TestEmitOnCommit_CrashAfterPersistLeavesRecoverableRow(t *testing.T) {
	x := newCommitHarness(t, []byte(`{"success":true}`), func(db *gorm.DB, bus *events.Bus) *dynamic.OutboxPublisher {
		p := dynamic.NewOutboxPublisher(db, bus)
		p.OnPersisted(func() error { return fmt.Errorf("simulated crash") })
		return p
	})
	if _, err := x.h.InvokeFor(context.Background(), uuid.New(), uuid.New(), "testaddon", "emit_test", []byte(aggPayload), nil); err != nil {
		t.Fatal(err)
	}
	if n := x.fired.Load(); n != 0 {
		t.Fatalf("published %d event(s) despite the crash", n)
	}
	rows := x.rows(t)
	if len(rows) != 1 || rows[0].PublishedAt != nil {
		t.Fatalf("want exactly one UNPUBLISHED outbox row, got %+v", rows)
	}
	if rows[0].Payload != aggPayload || rows[0].Sequence != 1 || rows[0].EventID == "" {
		t.Fatalf("row lost its identity: %+v", rows[0])
	}
}
