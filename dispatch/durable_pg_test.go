package dispatch_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/dispatch"
	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/events"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// pgSchemaDB opens an isolated schema on TEST_POSTGRES_DSN (skipped when unset).
// Each call to the returned opener yields a NEW pool on the same schema, i.e. a
// separate "replica".
func pgSchemaDB(t *testing.T) func() *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set: skipping Postgres dispatch test")
	}
	cfg := &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)}
	admin, err := gorm.Open(postgres.Open(dsn), cfg)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	schema := "dispatch_test_" + hex.EncodeToString(b[:])
	if err := admin.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatal(err)
	}
	var pools []*gorm.DB
	t.Cleanup(func() {
		for _, p := range pools {
			if s, err := p.DB(); err == nil {
				_ = s.Close()
			}
		}
		admin.Exec("DROP SCHEMA " + schema + " CASCADE")
		if s, err := admin.DB(); err == nil {
			_ = s.Close()
		}
	})
	full := dsn + " search_path=" + schema
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, _ := url.Parse(dsn)
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		full = u.String()
	}
	return func() *gorm.DB {
		// A fresh Config per pool: gorm.Open mutates the one it is given.
		db, err := gorm.Open(postgres.Open(full), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
		if err != nil {
			t.Fatal(err)
		}
		pools = append(pools, db)
		return db
	}
}

// Several replicas sweeping the same ledger (FOR UPDATE SKIP LOCKED) run every
// pending delivery exactly once.
func TestDurablePG_ReplicasNeverClaimTheSameDeliveryTwice(t *testing.T) {
	open := pgSchemaDB(t)
	const event = "pos.order_created"
	const n = 60

	seed := open()
	if err := dispatch.Migrate(seed, ""); err != nil {
		t.Fatal(err)
	}
	// Pending rows as a crashed process would have left them.
	org := uuid.New()
	now := time.Now().UTC()
	for i := 0; i < n; i++ {
		r := dispatch.Delivery{
			ID: uuid.New(), DeliveryID: fmt.Sprintf("d-%03d", i), RootDeliveryID: fmt.Sprintf("d-%03d", i),
			OrganizationID: org, Event: event, AddonKey: "customers", HandlerType: "compiled", Export: "on_event",
			SubEvent: event, Status: dispatch.StatusPending, Attempts: 0, NextAttemptAt: &now,
			Payload: fmt.Sprintf(`{"n":%d}`, i),
		}
		if err := seed.Table(dispatch.DefaultTableName).Create(&r).Error; err != nil {
			t.Fatal(err)
		}
	}

	var mu sync.Mutex
	seen := map[string]int{}
	handler := dispatch.CompiledHandlerFunc(func(_ context.Context, _ uuid.UUID, payload []byte) error {
		mu.Lock()
		seen[string(payload[:min(len(payload), 12)])]++ // unique per row via the stamped payload prefix
		mu.Unlock()
		time.Sleep(2 * time.Millisecond)
		return nil
	})
	done := make(chan dispatch.DeliveryResult, n*2)
	for i := 0; i < 3; i++ {
		cancel, err := dispatch.Wire(events.NewBus(nil), nil, open(),
			staticProvider("customers", uuid.New(), event, "compiled", "on_event"),
			dispatch.WithCompiledRegistry(dispatch.MapCompiledRegistry{"on_event": handler}),
			dispatch.WithLogger(silentLogger()), dispatch.WithPollInterval(10*time.Millisecond),
			dispatch.WithOnDelivery(func(r dispatch.DeliveryResult) { done <- r }))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(cancel)
	}
	for i := 0; i < n; i++ {
		select {
		case r := <-done:
			if r.Status != dispatch.StatusDelivered || r.Attempts != 1 {
				t.Fatalf("result = %+v", r)
			}
		case <-time.After(20 * time.Second):
			t.Fatalf("only %d/%d deliveries finished", i, n)
		}
	}
	time.Sleep(200 * time.Millisecond)
	select {
	case extra := <-done:
		t.Fatalf("extra terminal notification: %+v", extra)
	default:
	}
	mu.Lock()
	defer mu.Unlock()
	total := 0
	for k, c := range seen {
		total += c
		if c != 1 {
			t.Fatalf("payload %q ran %d times", k, c)
		}
	}
	if total != n {
		t.Fatalf("handler runs = %d, want %d", total, n)
	}
}

// End to end on Postgres: a durable publication (outbox + sequence + envelope),
// a failing first attempt, the process killed, a new one resumes it once.
func TestDurablePG_PublishKillRestart(t *testing.T) {
	open := pgSchemaDB(t)
	const event = "warehouse.stock_picked"
	db := open()
	if err := dynamic.MigrateEventOutbox(db); err != nil {
		t.Fatal(err)
	}

	a := &countingHandler{fn: func(int32) error { return fmt.Errorf("transient") }}
	pa := startProc(t, db, event, a, dispatch.WithRetryBackoff(300*time.Millisecond, time.Second), dispatch.WithPollInterval(20*time.Millisecond))
	pub := dynamic.NewOutboxPublisher(db, pa.bus)
	org := uuid.New()
	for i := 0; i < 2; i++ {
		pub.Publish(context.Background(), nil, []dynamic.StagedEvent{{AddonKey: "kernel", Name: event, OrgID: org,
			Payload: []byte(`{"sku":"A"}`), AggregateType: "Pick", AggregateID: "p-1"}})
	}
	for a.calls.Load() < 2 {
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	pa.cancel()

	var rows []dynamic.EventOutbox
	if err := db.Order("sequence").Find(&rows).Error; err != nil || len(rows) != 2 || rows[0].Sequence != 1 || rows[1].Sequence != 2 || rows[1].PublishedAt == nil {
		t.Fatalf("outbox rows = %+v err=%v", rows, err)
	}

	b := &recorder{}
	pb := startProc(t, db, event, b, dispatch.WithRetryBackoff(300*time.Millisecond, time.Second), dispatch.WithPollInterval(20*time.Millisecond))
	for i := 0; i < 2; i++ {
		select {
		case <-pb.barrier:
		case <-time.After(10 * time.Second):
			var rows []dispatch.Delivery
			db.Table(dispatch.DefaultTableName).Find(&rows)
			for _, r := range rows {
				t.Logf("ledger: status=%s attempts=%d next=%v lease=%v err=%q", r.Status, r.Attempts, r.NextAttemptAt, r.LeaseUntil, r.LastError)
			}
			t.Fatalf("restarted process delivered %d/2", i)
		}
	}
	time.Sleep(150 * time.Millisecond)
	if b.count() != 2 || a.calls.Load() != 2 {
		t.Fatalf("calls before=%d after=%d, want 2 and 2", a.calls.Load(), b.count())
	}
	seqs := map[float64]bool{}
	for i := 0; i < 2; i++ {
		seqs[envOf(t, b.payload(i))["sequence"].(float64)] = true
	}
	if !seqs[1] || !seqs[2] {
		t.Fatalf("envelope sequences = %v", seqs)
	}
}

// An index left INVALID by an interrupted CREATE INDEX CONCURRENTLY is dropped
// and rebuilt; a valid one is left alone; the table keeps accepting inserts.
func TestDurablePG_DueIndexIsBuiltConcurrentlyAndHealsWhenInvalid(t *testing.T) {
	db := pgSchemaDB(t)()
	if err := dispatch.Migrate(db, ""); err != nil {
		t.Fatal(err)
	}
	const idx = "idx_" + dispatch.DefaultTableName + "_due"
	valid := func() (exists, ok bool) {
		var rows []bool
		if err := db.Raw("SELECT i.indisvalid FROM pg_index i WHERE i.indexrelid = to_regclass(?)", idx).Scan(&rows).Error; err != nil {
			t.Fatal(err)
		}
		return len(rows) > 0, len(rows) > 0 && rows[0]
	}
	if e, ok := valid(); !e || !ok {
		t.Fatalf("index after Migrate: exists=%v valid=%v", e, ok)
	}
	// Simulate an interrupted concurrent build.
	if err := db.Exec("UPDATE pg_index SET indisvalid = false WHERE indexrelid = to_regclass(?)", idx).Error; err != nil {
		t.Skipf("cannot flag the index invalid without superuser: %v", err)
	}
	if e, ok := valid(); !e || ok {
		t.Fatalf("setup: exists=%v valid=%v, want invalid", e, ok)
	}
	if err := dispatch.Migrate(db, ""); err != nil {
		t.Fatal(err)
	}
	if e, ok := valid(); !e || !ok {
		t.Fatalf("index after healing: exists=%v valid=%v", e, ok)
	}
	// Idempotent when already valid, and Wire (async path) converges too.
	if err := dispatch.Migrate(db, ""); err != nil {
		t.Fatal(err)
	}
}

// Another replica is building the index with CREATE INDEX CONCURRENTLY: the
// catalog shows it INVALID, which used to look like an interrupted build and got
// it dropped (waiting on the builder, then "tuple concurrently updated"). While
// a session holds the build lock the index must not be touched; once the lock is
// gone (builder crashed) the invalid leftover is healed.
func TestDurablePG_DueIndexBeingBuiltByAnotherReplicaIsNotDropped(t *testing.T) {
	open := pgSchemaDB(t)
	db := open()
	if err := dispatch.Migrate(db, ""); err != nil {
		t.Fatal(err)
	}
	const idx = "idx_" + dispatch.DefaultTableName + "_due"
	oid := func() (id uint32, valid bool) {
		var r struct {
			Oid   uint32
			Valid bool
		}
		if err := db.Raw("SELECT i.indexrelid::oid::text::bigint AS oid, i.indisvalid AS valid FROM pg_index i WHERE i.indexrelid = to_regclass(?)", idx).Scan(&r).Error; err != nil {
			t.Fatal(err)
		}
		return r.Oid, r.Valid
	}
	before, _ := oid()
	if err := db.Exec("UPDATE pg_index SET indisvalid = false WHERE indexrelid = to_regclass(?)", idx).Error; err != nil {
		t.Skipf("cannot flag the index invalid without superuser: %v", err)
	}

	// The "other replica": a dedicated connection holding the build lock.
	builder := open()
	release := make(chan struct{})
	held := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_ = builder.Connection(func(c *gorm.DB) error {
			var got bool
			c.Raw("SELECT pg_try_advisory_lock(hashtextextended(?, 0))", "metacore.dispatch.due_index:"+dispatch.DefaultTableName).Scan(&got)
			if !got {
				t.Error("builder could not take the lock")
			}
			close(held)
			<-release
			c.Exec("SELECT pg_advisory_unlock(hashtextextended(?, 0))", "metacore.dispatch.due_index:"+dispatch.DefaultTableName)
			return nil
		})
	}()
	<-held

	start := time.Now()
	if err := dispatch.Migrate(open(), ""); err != nil {
		t.Fatalf("Migrate while another replica builds: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Migrate waited %s on the other builder", d)
	}
	if id, valid := oid(); id != before || valid {
		t.Fatalf("index was touched while another session held the build lock (oid %d -> %d, valid=%v)", before, id, valid)
	}

	close(release)
	<-finished
	if err := dispatch.Migrate(open(), ""); err != nil {
		t.Fatal(err)
	}
	if _, valid := oid(); !valid {
		t.Fatal("stale invalid index not healed once the lock is free")
	}
}

// Several replicas starting together converge on one valid index, no errors.
func TestDurablePG_DueIndexConcurrentStartersConverge(t *testing.T) {
	open := pgSchemaDB(t)
	first := open()
	// The table is created once (concurrent CREATE TABLE races are not what is
	// under test); the index is dropped so every starter has to build it.
	if err := dispatch.Migrate(first, ""); err != nil {
		t.Fatal(err)
	}
	if err := first.Exec("DROP INDEX idx_" + dispatch.DefaultTableName + "_due").Error; err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for i := 0; i < 6; i++ {
		db := open()
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- dispatch.Migrate(db, "")
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Migrate: %v", err)
		}
	}
	if err := dispatch.Migrate(open(), ""); err != nil {
		t.Fatal(err)
	}
	var valid []bool
	first.Raw("SELECT i.indisvalid FROM pg_index i WHERE i.indexrelid = to_regclass(?)", "idx_"+dispatch.DefaultTableName+"_due").Scan(&valid)
	if len(valid) != 1 || !valid[0] {
		t.Fatalf("index state after concurrent starters: %v", valid)
	}
}
