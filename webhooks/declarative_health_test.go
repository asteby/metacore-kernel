package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/migrations"
	"github.com/asteby/metacore-kernel/webhooks/signing"
)

func reload(t *testing.T, e *testEnv, w *Webhook) *Webhook {
	t.Helper()
	got, err := e.svc.Get(context.Background(), w.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestAutoDisableAfterConsecutiveFailuresAndManualResume(t *testing.T) {
	e := newEnv(t, nil, 500, 500, 500, 500, 500, 500)
	var alerts []Webhook
	e.svc.onAutoOff = func(_ context.Context, w Webhook) { alerts = append(alerts, w) }
	sub := e.sub(&Declarative{Enabled: true, AutoDisableThreshold: 3})
	sub.RetryMax = 20
	w := e.create(t, sub)
	e.publish(t, "customers.customer.created", customerPayload("evt-1", "cus-1", nil))

	for i := 0; i < 3; i++ {
		e.drain(1)
		e.clk.Advance(2 * time.Hour)
	}
	got := reload(t, e, w)
	if got.ConsecutiveFailures != 3 || got.Declarative.Enabled || got.AutoDisabledAt == nil || got.FailureCount != 3 ||
		got.LastHTTPStatus != 500 || got.LastFailedAt == nil {
		t.Fatalf("health after 3 failures: %+v", got)
	}
	if len(alerts) != 1 || alerts[0].ID != w.ID {
		t.Fatalf("OnAutoDisable calls: %d", len(alerts))
	}
	// switched off: queued delivery is retained, new events are retained, nothing is sent
	e.drain(3)
	e.publish(t, "customers.customer.updated", customerPayload("evt-2", "cus-1", nil))
	if n := len(e.tg.Hits()); n != 3 {
		t.Fatalf("auto-disabled subscription kept sending: %d hits", n)
	}
	held := 0
	for _, d := range e.deliveries(t, w) {
		if d.Held {
			held++
		}
	}
	if held != 2 {
		t.Fatalf("held = %d, want both deliveries retained", held)
	}
	// manual resume: clears the trip and releases what was retained
	if n, err := e.svc.SetEnabled(context.Background(), w.ID, true); err != nil || n != 2 {
		t.Fatalf("resume released %d err %v", n, err)
	}
	got = reload(t, e, w)
	if !got.Declarative.Enabled || got.ConsecutiveFailures != 0 || got.AutoDisabledAt != nil {
		t.Fatalf("resume did not clear health: %+v", got)
	}
}

func TestSuccessResetsConsecutiveFailures(t *testing.T) {
	e := newEnv(t, nil, 500, 500, 201)
	sub := e.sub(&Declarative{Enabled: true, AutoDisableThreshold: 3})
	sub.RetryMax = 20
	w := e.create(t, sub)
	e.publish(t, "customers.customer.created", customerPayload("evt-1", "cus-1", nil))
	for i := 0; i < 3; i++ {
		e.drain(1)
		e.clk.Advance(2 * time.Hour)
	}
	got := reload(t, e, w)
	if got.ConsecutiveFailures != 0 || got.SuccessCount != 1 || got.FailureCount != 2 || !got.Declarative.Enabled || got.LastHTTPStatus != 201 {
		t.Fatalf("%+v", got)
	}
}

func TestNegativeThresholdNeverAutoDisables(t *testing.T) {
	script := make([]int, 15)
	for i := range script {
		script[i] = 500
	}
	e := newEnv(t, nil, script...)
	sub := e.sub(&Declarative{Enabled: true, AutoDisableThreshold: -1})
	sub.RetryMax = 50
	w := e.create(t, sub)
	e.publish(t, "customers.customer.created", customerPayload("evt-1", "cus-1", nil))
	for i := 0; i < 15; i++ {
		e.drain(1)
		e.clk.Advance(2 * time.Hour)
	}
	if got := reload(t, e, w); !got.Declarative.Enabled || got.ConsecutiveFailures != 15 {
		t.Fatalf("%+v", got)
	}
}

func TestDefaultThresholdIsTen(t *testing.T) {
	script := make([]int, 12)
	for i := range script {
		script[i] = 503
	}
	e := newEnv(t, nil, script...)
	sub := e.sub(&Declarative{Enabled: true})
	sub.RetryMax = 50
	w := e.create(t, sub)
	e.publish(t, "customers.customer.created", customerPayload("evt-1", "cus-1", nil))
	for i := 0; i < 12; i++ {
		e.drain(1)
		e.clk.Advance(2 * time.Hour)
	}
	if got := reload(t, e, w); got.Declarative.Enabled || got.ConsecutiveFailures != 10 {
		t.Fatalf("default threshold: %+v", got)
	}
}

func TestClassicWebhooksNeverAutoDisable(t *testing.T) {
	script := make([]int, 6)
	for i := range script {
		script[i] = 500
	}
	e := newEnv(t, nil, script...)
	sub := e.sub(nil, "order.created")
	sub.RetryMax = 50
	w := e.create(t, sub)
	_ = e.svc.Trigger(context.Background(), "order.created", "organization", e.org, JSONMap{"id": "1"})
	for i := 0; i < 6; i++ {
		e.drain(1)
		e.clk.Advance(48 * time.Hour)
	}
	got := reload(t, e, w)
	if !got.Active || got.FailureCount != 6 || got.ConsecutiveFailures != 0 {
		t.Fatalf("classic behaviour changed: %+v", got)
	}
}

func TestRequeueFromDeadLetter(t *testing.T) {
	e := newEnv(t, nil, 422)
	w := e.create(t, e.sub(&Declarative{Enabled: true}))
	e.publish(t, "customers.customer.created", customerPayload("evt-dl", "cus-1", nil))
	e.drain(1)
	row := e.deliveries(t, w)[0]
	if row.NextAttemptAt != nil || row.Succeeded {
		t.Fatalf("should be dead-lettered: %+v", row)
	}
	if err := e.svc.Requeue(context.Background(), row.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Requeue(context.Background(), row.ID); !errors.Is(err, ErrNotDead) {
		t.Fatalf("already queued: %v", err)
	}
	e.drain(1)
	row = e.deliveries(t, w)[0]
	if !row.Succeeded || row.AttemptCount != 1 {
		t.Fatalf("requeued delivery: %+v", row)
	}
	hits := e.tg.Hits()
	if len(hits) != 2 || hits[1].Header.Get("X-Idempotency-Key") != "evt-dl" {
		t.Fatalf("same key on requeue: %+v", hits)
	}
	if err := e.svc.Requeue(context.Background(), row.ID); !errors.Is(err, ErrNotDead) {
		t.Fatalf("delivered: %v", err)
	}
	if err := e.svc.Requeue(context.Background(), uuid.New()); !errors.Is(err, ErrDeliveryNotFound) {
		t.Fatalf("unknown: %v", err)
	}
}

func TestBackoffHasJitterWithinBounds(t *testing.T) {
	for _, tc := range []struct {
		r        float64
		min, max time.Duration
	}{
		{0, 3200 * time.Millisecond, 3200 * time.Millisecond},
		{0.5, 4 * time.Second, 4 * time.Second},
		{0.999999, 4700 * time.Millisecond, 4800 * time.Millisecond},
	} {
		e := newEnv(t, nil, 500)
		r := tc.r
		e.svc.rnd = func() float64 { return r }
		w := e.create(t, e.sub(&Declarative{Enabled: true}))
		e.publish(t, "customers.customer.created", customerPayload("evt-j", "cus-1", nil))
		e.drain(1)
		d := e.deliveries(t, w)[0].NextAttemptAt.Sub(e.clk.Now())
		if d < tc.min || d > tc.max {
			t.Errorf("rnd=%v backoff %v not in [%v,%v]", tc.r, d, tc.min, tc.max)
		}
	}
}

func TestFingerprintHeaderStableAcrossRetries(t *testing.T) {
	e := newEnv(t, nil, 503, 201)
	e.create(t, e.sub(&Declarative{Enabled: true}))
	e.publish(t, "customers.customer.created", customerPayload("evt-fp", "cus-1", nil))
	e.drain(1)
	e.clk.Advance(time.Minute)
	e.drain(1)
	h := e.tg.Hits()
	if len(h) != 2 {
		t.Fatalf("hits %d", len(h))
	}
	for _, x := range h {
		if x.Header.Get("X-Metacore-Body-SHA256") != signing.Fingerprint(x.Body) {
			t.Fatal("fingerprint header does not match the body")
		}
	}
	if h[0].Header.Get("X-Metacore-Body-SHA256") != h[1].Header.Get("X-Metacore-Body-SHA256") {
		t.Fatal("fingerprint must be stable across retries")
	}
}

func TestResponseBodiesAreRedactedBeforePersisting(t *testing.T) {
	got := safeResponseBody(`{"ok":true,"token":"abc123","nested":{"api_key":"k","x":1}}`)
	if strings.Contains(got, "abc123") || strings.Contains(got, `"k"`) || !strings.Contains(got, `"ok":true`) {
		t.Fatalf("redaction: %s", got)
	}
	long := "<html>" + strings.Repeat("x", 1000)
	if n := len(safeResponseBody(long)); n != 256 {
		t.Fatalf("non-JSON excerpt len %d", n)
	}
	if safeResponseBody("") != "" {
		t.Fatal("empty")
	}
}

// El esquema real (migracion 0012) sobre una base creada por las migraciones
// anteriores: lo viejo sigue cargando igual y lo declarativo funciona.
func TestMigration0012BackwardCompatible(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	ctx := context.Background()
	r := migrations.Runner{}
	if err := r.UpTo(ctx, db, 11); err != nil {
		t.Fatalf("UpTo 11: %v", err)
	}
	org, id, did := uuid.New().String(), uuid.New().String(), uuid.New().String()
	// datos escritos ANTES de la migracion
	if err := db.Exec(`INSERT INTO webhooks (id, organization_id, name, url, events, secret, owner_type, owner_id)
		VALUES (?, ?, 'old', 'https://old.example.com/h', '["order.*"]', 's3', 'organization', ?)`, id, org, org).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO webhook_deliveries (id, organization_id, webhook_id, event, payload) VALUES (?, ?, ?, 'order.created', '{"a":1}')`, did, org, id).Error; err != nil {
		t.Fatal(err)
	}
	if err := r.Up(ctx, db); err != nil {
		t.Fatalf("Up: %v", err)
	}
	var w Webhook
	if err := db.First(&w, "id = ?", id).Error; err != nil || w.Declarative != nil || w.Secret != "s3" || w.ConsecutiveFailures != 0 {
		t.Fatalf("old webhook after migration: %+v err %v", w, err)
	}
	var d WebhookDelivery
	if err := db.First(&d, "id = ?", did).Error; err != nil || d.Held || d.InFlight || d.EventID != "" || d.Payload["a"] != float64(1) {
		t.Fatalf("old delivery after migration: %+v err %v", d, err)
	}
	// declarativo: round-trip de la configuracion completa por el esquema real
	svc := New(Config{DB: db, UnsafeAllowPrivateTargets: true})
	decl := &Declarative{Key: "k", Enabled: true, AutoDisableThreshold: 4, Ordering: OrderingPerAggregate,
		Filter:    &Filter{All: []Condition{{Path: "event.branch_id", In: []any{"a"}}}},
		Transform: &Transform{Body: map[string]any{"x": "{{event.id}}"}}}
	nw := &Webhook{URL: "https://x.example.com/h", Events: StringSlice{"a.b"}, Active: true, OwnerType: "organization", OwnerID: uuid.New(), Declarative: decl}
	if err := svc.Create(ctx, nw); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Get(ctx, nw.ID)
	if err != nil {
		t.Fatal(err)
	}
	b1, _ := json.Marshal(decl)
	b2, _ := json.Marshal(got.Declarative)
	if string(b1) != string(b2) {
		t.Fatalf("declarative round trip:\n%s\n%s", b1, b2)
	}
	// y la entrega nueva usa todas las columnas
	if n, err := svc.TriggerEvent(ctx, "organization", nw.OwnerID, Event{Name: "a.b", ID: "e1", BranchID: "a", Payload: map[string]any{}}); err != nil || n != 1 {
		t.Fatalf("trigger: %d %v", n, err)
	}
	// bajar la migracion deja el esquema anterior y no pierde filas
	if err := r.Down(ctx, db, 1); err != nil {
		t.Fatalf("Down: %v", err)
	}
	var n int64
	db.Raw("SELECT count(*) FROM webhooks WHERE id = ?", id).Scan(&n)
	if n != 1 {
		t.Fatal("legacy row lost on Down")
	}
}
