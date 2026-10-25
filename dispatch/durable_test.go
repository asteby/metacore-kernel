package dispatch_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/dispatch"
	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/events"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

var update = flag.Bool("update", false, "rewrite golden files")

// proc is one "process" of the dispatcher over a shared ledger DB: its own bus,
// its own handler, its own Wire. Killing it = cancel(); starting another over
// the same DB = a restart.
type proc struct {
	bus     *events.Bus
	cancel  func()
	barrier chan dispatch.DeliveryResult
}

func startProc(t *testing.T, db *gorm.DB, event string, h dispatch.CompiledHandler, opts ...dispatch.Option) *proc {
	t.Helper()
	p := &proc{bus: events.NewBus(nil), barrier: make(chan dispatch.DeliveryResult, 16)}
	base := []dispatch.Option{
		dispatch.WithCompiledRegistry(dispatch.MapCompiledRegistry{"on_event": h}),
		dispatch.WithLogger(silentLogger()),
		dispatch.WithOnDelivery(func(r dispatch.DeliveryResult) { p.barrier <- r }),
	}
	cancel, err := dispatch.Wire(p.bus, nil, db,
		staticProvider("customers", uuid.New(), event, "compiled", "on_event"),
		append(base, opts...)...)
	if err != nil {
		t.Fatalf("wire: %v", err)
	}
	p.cancel = cancel
	t.Cleanup(cancel)
	return p
}

func (p *proc) await(t *testing.T) dispatch.DeliveryResult {
	t.Helper()
	select {
	case r := <-p.barrier:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for a terminal delivery")
	}
	return dispatch.DeliveryResult{}
}

type countingHandler struct {
	calls atomic.Int32
	fn    func(n int32) error
}

func (c *countingHandler) Handle(context.Context, uuid.UUID, []byte) error {
	n := c.calls.Add(1)
	if c.fn != nil {
		return c.fn(n)
	}
	return nil
}

func ledgerRow(t *testing.T, db *gorm.DB) dispatch.Delivery {
	t.Helper()
	return singleDelivery(t, db)
}

// X1.2: kill the process in the middle of a retry; the restarted process
// resumes the pending row and delivers it exactly once.
func TestDurable_RestartMidRetryDeliversExactlyOnce(t *testing.T) {
	db := ledgerDB(t)
	const event = "pos.order_created"

	a := &countingHandler{fn: func(int32) error { return errors.New("transient") }}
	pa := startProc(t, db, event, a,
		dispatch.WithMaxAttempts(3),
		dispatch.WithRetryBackoff(400*time.Millisecond, time.Second),
		dispatch.WithPollInterval(20*time.Millisecond))
	if err := pa.bus.Publish(context.Background(), "kernel", event, uuid.New(), map[string]any{"order_id": "o-1"}); err != nil {
		t.Fatal(err)
	}
	// Wait for attempt 1 to fail and the retry to be parked in the ledger.
	deadline := time.Now().Add(5 * time.Second)
	for {
		r := ledgerRow(t, db)
		if r.Attempts == 1 && r.Status == dispatch.StatusPending && r.NextAttemptAt != nil && r.LeaseToken == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("retry never parked: %+v", r)
		}
		time.Sleep(5 * time.Millisecond)
	}
	pa.cancel() // kill -9 equivalent: nothing of process A runs again

	b := &countingHandler{}
	pb := startProc(t, db, event, b,
		dispatch.WithMaxAttempts(3),
		dispatch.WithRetryBackoff(400*time.Millisecond, time.Second),
		dispatch.WithPollInterval(20*time.Millisecond))
	res := pb.await(t)
	if res.Status != dispatch.StatusDelivered || res.Attempts != 2 {
		t.Fatalf("restarted process result = %+v, want delivered on attempt 2", res)
	}
	time.Sleep(250 * time.Millisecond) // a double delivery would land here
	if a.calls.Load() != 1 || b.calls.Load() != 1 {
		t.Fatalf("handler calls: before kill=%d after restart=%d, want 1 and 1", a.calls.Load(), b.calls.Load())
	}
	if r := ledgerRow(t, db); r.Status != dispatch.StatusDelivered || r.DeliveredAt == nil {
		t.Fatalf("ledger = %+v", r)
	}
	select {
	case extra := <-pb.barrier:
		t.Fatalf("second terminal notification: %+v", extra)
	default:
	}
}

// A process that dies INSIDE the attempt (lease held, never released): once the
// lease expires another process takes the delivery over; the dead holder, if it
// ever wakes up, cannot overwrite the result (fenced by the lease token).
func TestDurable_CrashInsideAttemptIsRecoveredAfterLeaseExpiry(t *testing.T) {
	db := ledgerDB(t)
	const event = "pos.order_created"

	release := make(chan struct{})
	started := make(chan struct{}, 1)
	a := &countingHandler{fn: func(int32) error {
		started <- struct{}{}
		<-release // the "dead" process never finishes the attempt
		return nil
	}}
	pa := startProc(t, db, event, a, dispatch.WithLease(150*time.Millisecond), dispatch.WithPollInterval(20*time.Millisecond))
	if err := pa.bus.Publish(context.Background(), "kernel", event, uuid.New(), map[string]any{"order_id": "o-2"}); err != nil {
		t.Fatal(err)
	}
	<-started
	pa.cancel()

	b := &countingHandler{}
	pb := startProc(t, db, event, b, dispatch.WithLease(150*time.Millisecond), dispatch.WithPollInterval(20*time.Millisecond))
	res := pb.await(t)
	if res.Status != dispatch.StatusDelivered || res.Attempts != 2 {
		t.Fatalf("takeover result = %+v, want delivered on attempt 2", res)
	}
	close(release) // the zombie wakes up and tries to report success
	time.Sleep(100 * time.Millisecond)
	select {
	case extra := <-pa.barrier:
		t.Fatalf("zombie holder reported a result: %+v", extra)
	default:
	}
	if r := ledgerRow(t, db); r.Status != dispatch.StatusDelivered || r.Attempts != 2 {
		t.Fatalf("ledger overwritten by the zombie: %+v", r)
	}
	if b.calls.Load() != 1 {
		t.Fatalf("takeover ran the handler %d times", b.calls.Load())
	}
}

// A poison event that keeps killing the process must not loop forever: its
// expired lease with every attempt spent dead-letters it.
func TestDurable_ExhaustedExpiredLeaseIsDeadLettered(t *testing.T) {
	db := ledgerDB(t)
	const event = "pos.order_created"
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	a := &countingHandler{fn: func(int32) error { <-block; return nil }}
	pa := startProc(t, db, event, a, dispatch.WithMaxAttempts(1), dispatch.WithLease(100*time.Millisecond), dispatch.WithPollInterval(20*time.Millisecond))
	_ = pa.bus.Publish(context.Background(), "kernel", event, uuid.New(), map[string]any{"order_id": "o-3"})
	for a.calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	pa.cancel()
	pb := startProc(t, db, event, &countingHandler{}, dispatch.WithMaxAttempts(1), dispatch.WithLease(100*time.Millisecond), dispatch.WithPollInterval(20*time.Millisecond))
	res := pb.await(t)
	if res.Status != dispatch.StatusDead || !strings.Contains(res.Err, "lease expired") {
		t.Fatalf("result = %+v, want dead (lease expired)", res)
	}
}

// Rows written by an older kernel (no payload) stay exactly as they were.
func TestDurable_LegacyPendingRowWithoutPayloadIsLeftAlone(t *testing.T) {
	db := ledgerDB(t)
	if err := dispatch.Migrate(db, ""); err != nil {
		t.Fatal(err)
	}
	legacy := dispatch.Delivery{ID: uuid.New(), DeliveryID: "legacy", OrganizationID: uuid.New(), Event: "x.y", AddonKey: "customers",
		HandlerType: "compiled", Export: "on_event", Status: dispatch.StatusPending, Attempts: 1}
	if err := db.Table(dispatch.DefaultTableName).Create(&legacy).Error; err != nil {
		t.Fatal(err)
	}
	h := &countingHandler{}
	startProc(t, db, "x.y", h, dispatch.WithPollInterval(10*time.Millisecond))
	time.Sleep(150 * time.Millisecond)
	if h.calls.Load() != 0 {
		t.Fatal("a payload-less legacy row must not be invoked")
	}
	if r := ledgerRow(t, db); r.Status != dispatch.StatusPending {
		t.Fatalf("legacy row changed: %+v", r)
	}
}

// The subscription vanished while the delivery waited: cancelled, not retried.
func TestDurable_UninstalledSubscriptionCancelsPending(t *testing.T) {
	db := ledgerDB(t)
	const event = "pos.order_created"
	a := &countingHandler{fn: func(int32) error { return errors.New("down") }}
	pa := startProc(t, db, event, a, dispatch.WithRetryBackoff(300*time.Millisecond, time.Second), dispatch.WithPollInterval(20*time.Millisecond))
	_ = pa.bus.Publish(context.Background(), "kernel", event, uuid.New(), map[string]any{"order_id": "o-4"})
	for a.calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	pa.cancel()

	// "Restart" with the addon uninstalled: the provider yields nothing.
	bus := events.NewBus(nil)
	barrier := make(chan dispatch.DeliveryResult, 4)
	cancel, err := dispatch.Wire(bus, nil, db,
		dispatch.SubscriptionProviderFunc(func(context.Context, uuid.UUID) ([]dispatch.Subscription, error) { return nil, nil }),
		dispatch.WithLogger(silentLogger()), dispatch.WithPollInterval(20*time.Millisecond),
		dispatch.WithOnDelivery(func(r dispatch.DeliveryResult) { barrier <- r }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cancel)
	select {
	case r := <-barrier:
		if r.Status != dispatch.StatusCancelled {
			t.Fatalf("status = %s", r.Status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no terminal status")
	}
	if a.calls.Load() != 1 {
		t.Fatalf("handler ran %d times", a.calls.Load())
	}
}

// X1.3: guest-declared retryability.
func TestDurable_GuestRetryable(t *testing.T) {
	cases := []struct {
		name        string
		answer      string
		wantStatus  string
		wantCalls   int32
		errContains string
	}{
		{"not retryable goes dead at once", `{"success":false,"retryable":false,"error":{"code":"invalid","message":"bad sku"}}`, dispatch.StatusDead, 1, "not retryable"},
		{"retryable is retried until the budget ends", `{"success":false,"retryable":true,"retry_after_ms":1,"error":{"code":"busy","message":"later"}}`, dispatch.StatusDead, 3, "busy"},
		{"unspecified keeps the legacy retry", `{"success":false,"error":{"code":"x","message":"y"}}`, dispatch.StatusDead, 3, "x: y"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := ledgerDB(t)
			var calls atomic.Int32
			inv := dispatch.NativeInvokerFunc(func(context.Context, uuid.UUID, uuid.UUID, string, string, []byte) ([]byte, error) {
				calls.Add(1)
				return []byte(c.answer), nil
			})
			bus := events.NewBus(nil)
			barrier := make(chan dispatch.DeliveryResult, 4)
			cancel, err := dispatch.Wire(bus, nil, db, staticProvider("customers", uuid.New(), "pos.order_created", "native", "op"),
				dispatch.WithNativeInvoker(inv), dispatch.WithLogger(silentLogger()), dispatch.WithMaxAttempts(3),
				dispatch.WithRetryBackoff(0, 0), dispatch.WithPollInterval(10*time.Millisecond),
				dispatch.WithOnDelivery(func(r dispatch.DeliveryResult) { barrier <- r }))
			if err != nil {
				t.Fatal(err)
			}
			defer cancel()
			_ = bus.Publish(context.Background(), "kernel", "pos.order_created", uuid.New(), map[string]any{"a": 1})
			select {
			case r := <-barrier:
				if r.Status != c.wantStatus || !strings.Contains(r.Err, c.errContains) {
					t.Fatalf("result = %+v", r)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("no terminal status")
			}
			if calls.Load() != c.wantCalls {
				t.Fatalf("invocations = %d, want %d", calls.Load(), c.wantCalls)
			}
		})
	}
}

func TestDurable_RetryAfterIsHonoured(t *testing.T) {
	db := ledgerDB(t)
	inv := dispatch.NativeInvokerFunc(func(context.Context, uuid.UUID, uuid.UUID, string, string, []byte) ([]byte, error) {
		return []byte(`{"success":false,"retryable":true,"retry_after_ms":60000}`), nil
	})
	bus := events.NewBus(nil)
	cancel, err := dispatch.Wire(bus, nil, db, staticProvider("customers", uuid.New(), "pos.order_created", "native", "op"),
		dispatch.WithNativeInvoker(inv), dispatch.WithLogger(silentLogger()), dispatch.WithRetryBackoff(time.Millisecond, time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	before := time.Now()
	_ = bus.Publish(context.Background(), "kernel", "pos.order_created", uuid.New(), map[string]any{"a": 1})
	var r dispatch.Delivery
	for i := 0; i < 500; i++ {
		r = ledgerRow(t, db)
		if r.LeaseToken == "" && r.Attempts == 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if r.Status != dispatch.StatusPending || r.NextAttemptAt == nil || r.NextAttemptAt.Sub(before) < 55*time.Second {
		t.Fatalf("retry_after_ms ignored: %+v", r)
	}
}

// X1.3: Replay re-queues terminal deliveries as the next generation; it never
// collides with the ledger and is idempotent per generation.
func TestDurable_ReplayNextGeneration(t *testing.T) {
	db := ledgerDB(t)
	const event = "pos.order_created"
	rec := &recorder{}
	p := startProc(t, db, event, rec, dispatch.WithPollInterval(10*time.Millisecond))
	org := uuid.New()
	_ = p.bus.Publish(context.Background(), "kernel", event, org, map[string]any{"order_id": "o-9"})
	p.await(t)

	res, err := dispatch.Replay(context.Background(), db, "", dispatch.ReplayFilter{OrgID: org, AddonKey: "customers", Event: event})
	if err != nil || res.Requeued != 1 {
		t.Fatalf("replay = %+v err=%v", res, err)
	}
	r := p.await(t)
	if r.Status != dispatch.StatusDelivered {
		t.Fatalf("replayed delivery = %+v", r)
	}
	if rec.count() != 2 {
		t.Fatalf("handler calls = %d, want original + replay", rec.count())
	}
	for i, wantGen := range []float64{0, 1} {
		var got map[string]any
		_ = json.Unmarshal(rec.payload(i), &got)
		env, _ := got["envelope"].(map[string]any)
		gen, _ := env["generation"].(float64)
		if gen != wantGen {
			t.Fatalf("payload %d envelope.generation = %v, want %v", i, gen, wantGen)
		}
		if env["id"] == "" || env["id"] != envOf(t, rec.payload(0))["id"] {
			t.Fatalf("replay must keep the event id: %v", env)
		}
	}
	// Same filter again: that generation already exists, only a NEW one appears
	// if the replayed row is itself replayed — here the gen-1 row is matched
	// and becomes gen 2, while gen 0 -> gen 1 is a no-op.
	again, err := dispatch.Replay(context.Background(), db, "", dispatch.ReplayFilter{OrgID: org, Event: event})
	if err != nil || again.Existing != 1 || again.Requeued != 1 {
		t.Fatalf("second replay = %+v err=%v", again, err)
	}
	if n := deliveryCount(t, db); n != 3 {
		t.Fatalf("ledger rows = %d, want 3 (gen 0,1,2)", n)
	}
}

func envOf(t *testing.T, payload []byte) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatal(err)
	}
	env, _ := got["envelope"].(map[string]any)
	return env
}

// Re-publishing a durable event (the outbox relay after a crash) carries the
// same Meta.ID and collapses into the delivery already recorded.
func TestDurable_RepublishWithSameEventIDIsDeduped(t *testing.T) {
	db := ledgerDB(t)
	const event = "pos.order_created"
	rec := &recorder{}
	p := startProc(t, db, event, rec)
	org := uuid.New()
	ctx := dynamic.WithMeta(context.Background(), dynamic.Meta{ID: "01900000-0000-7000-8000-000000000001"})
	for i := 0; i < 3; i++ {
		_ = p.bus.Publish(ctx, "kernel", event, org, json.RawMessage(`{"order_id":"o-1"}`))
	}
	p.await(t)
	time.Sleep(100 * time.Millisecond)
	if rec.count() != 1 || deliveryCount(t, db) != 1 {
		t.Fatalf("delivered %d times (%d rows), want once", rec.count(), deliveryCount(t, db))
	}
	// A different publication id is a different event.
	ctx2 := dynamic.WithMeta(context.Background(), dynamic.Meta{ID: "01900000-0000-7000-8000-000000000002"})
	_ = p.bus.Publish(ctx2, "kernel", event, org, json.RawMessage(`{"order_id":"o-1"}`))
	p.await(t)
	if rec.count() != 2 {
		t.Fatalf("a new publication id must be delivered, calls=%d", rec.count())
	}
}

// Golden of what a subscriber receives: the unchanged legacy payload plus the
// metacore.event/1 envelope. Run with -update to rewrite after a deliberate
// change of the contract.
func TestEnvelopeGolden(t *testing.T) {
	cases := []struct {
		golden  string
		publish func(bus *events.Bus, org uuid.UUID)
	}{
		{"envelope_v1_domain.golden.json", func(bus *events.Bus, org uuid.UUID) {
			ctx := dynamic.WithActorID(context.Background(), "11111111-1111-1111-1111-111111111111")
			ctx = dynamic.WithBranchID(ctx, "22222222-2222-2222-2222-222222222222")
			ctx = dynamic.WithCorrelationID(ctx, "req-42")
			ctx = dynamic.WithMeta(ctx, dynamic.Meta{
				ID: "01900000-0000-7000-8000-0000000000aa", Version: 1, OccurredAt: "2026-10-25T12:00:00Z",
				AggregateType: "SalesOrder", AggregateID: "so-1", Sequence: 7, CausationID: "01900000-0000-7000-8000-0000000000bb",
			})
			_ = bus.Publish(ctx, "kernel", "pos.order_created", org, json.RawMessage(`{"order_id":"o-1","total":10}`))
		}},
		{"envelope_v1_canonical.golden.json", func(bus *events.Bus, org uuid.UUID) {
			ctx := dynamic.WithMeta(context.Background(), dynamic.Meta{ID: "occ-1", OccurredAt: "2026-10-25T12:00:00Z", Sequence: 3})
			_ = bus.Publish(ctx, "kernel", "pos.order_created", org, map[string]any{
				"id": "row-1", "occurrence_id": "occ-1", "model": "SalesOrder", "action": "updated", "addon_key": "pos",
				"actor_id": "11111111-1111-1111-1111-111111111111", "correlation_id": "req-7",
				"after": map[string]any{"id": "row-1", "state": "paid"},
			})
		}},
	}
	for _, c := range cases {
		t.Run(c.golden, func(t *testing.T) {
			db := ledgerDB(t)
			rec := &recorder{}
			p := startProc(t, db, "pos.order_created", rec)
			org := uuid.MustParse("33333333-3333-3333-3333-333333333333")
			c.publish(p.bus, org)
			p.await(t)
			var buf bytes.Buffer
			if err := json.Indent(&buf, rec.payload(0), "", "  "); err != nil {
				t.Fatal(err)
			}
			buf.WriteByte('\n')
			path := "testdata/" + c.golden
			if *update {
				if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("missing golden (run with -update): %v", err)
			}
			if !bytes.Equal(buf.Bytes(), want) {
				t.Fatalf("delivered payload differs from %s:\n--- got\n%s\n--- want\n%s", path, buf.String(), want)
			}
		})
	}
}

