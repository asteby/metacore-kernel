package legacybridge

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/outbox"
	"gorm.io/gorm"
)

// recorder es un Connector de prueba: falla según el guion y registra el orden.
type recorder struct {
	mu    sync.Mutex
	sent  []string // IdempotencyKey en orden de entrega
	tries map[string]int
	plan  map[string][]error // por IdempotencyKey: errores de los primeros intentos
	reply map[string]Delivery
}

func newRecorder() *recorder {
	return &recorder{tries: map[string]int{}, plan: map[string][]error{}, reply: map[string]Delivery{}}
}

func (r *recorder) Send(_ context.Context, c Command) (Delivery, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.tries[c.IdempotencyKey]
	r.tries[c.IdempotencyKey] = n + 1
	if errs := r.plan[c.IdempotencyKey]; n < len(errs) && errs[n] != nil {
		return Delivery{}, errs[n]
	}
	r.sent = append(r.sent, c.IdempotencyKey)
	return r.reply[c.IdempotencyKey], nil
}

func (r *recorder) order() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.sent...)
}

type outboundEnv struct {
	db    *gorm.DB
	store *GormStore
	jobs  *outbox.Service
	out   *Outbound
	conn  *recorder
	dead  chan Command
}

func newOutboundEnv(t *testing.T, db *gorm.DB, mut ...func(*OutboundConfig)) *outboundEnv {
	t.Helper()
	e := &outboundEnv{db: db, store: gormStore(t, db), conn: newRecorder(), dead: make(chan Command, 16)}
	jobs, err := outbox.New(db, outbox.Config{Workers: 2, PollInterval: 10 * time.Millisecond, ShutdownGrace: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	e.jobs = jobs
	cfg := OutboundConfig{Store: e.store, Outbox: jobs, Connector: e.conn, MaxAttempts: 3,
		Backoff: func(int) time.Duration { return 20 * time.Millisecond },
		OnDead:  func(_ context.Context, c Command) { e.dead <- c }}
	for _, m := range mut {
		m(&cfg)
	}
	out, err := NewOutbound(cfg)
	if err != nil {
		t.Fatal(err)
	}
	out.Register()
	e.out = out
	return e
}

func (e *outboundEnv) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	if err := e.jobs.Start(ctx); err != nil {
		t.Fatal(err)
	}
	go e.out.Run(ctx, 20*time.Millisecond)
	t.Cleanup(func() { cancel(); _ = e.jobs.Shutdown(context.Background()) })
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no se cumplió: %s", what)
}

func cmd(agg, key string) Command {
	return Command{Org: org, System: "erp", Kind: "customers.upsert", AggregateKey: agg, IdempotencyKey: key, Payload: json.RawMessage(`{"k":"` + key + `"}`)}
}

func eachDB(t *testing.T, fn func(t *testing.T, db *gorm.DB)) {
	t.Run("sqlite", func(t *testing.T) { fn(t, sqliteDB(t)) })
	t.Run("postgres", func(t *testing.T) { fn(t, postgresDB(t)) })
}

func TestOutbound_DeliversInOrderPerAggregateAndRetriesWithBackoff(t *testing.T) {
	eachDB(t, func(t *testing.T, db *gorm.DB) {
		e := newOutboundEnv(t, db)
		ctx := context.Background()
		// a1 falla dos veces (transitorio): a2 espera detrás; b1 no se bloquea.
		e.conn.plan["a1"] = []error{RetryAfter(errors.New("503"), 300*time.Millisecond), errors.New("timeout")}
		for _, c := range []Command{cmd("A", "a1"), cmd("A", "a2"), cmd("B", "b1")} {
			if _, err := e.out.Publish(ctx, c); err != nil {
				t.Fatal(err)
			}
		}
		e.start(t)
		waitFor(t, "tres entregas", func() bool {
			n, _ := e.store.CountCommands(ctx, org, "erp", CommandSent)
			return n == 3
		})
		got := e.conn.order()
		if len(got) != 3 {
			t.Fatalf("entregas duplicadas: %v", got)
		}
		pos := map[string]int{}
		for i, k := range got {
			pos[k] = i
		}
		if pos["a1"] > pos["a2"] {
			t.Fatalf("orden por agregado roto: %v", got)
		}
		if pos["b1"] > pos["a1"] {
			t.Fatalf("B no debe esperar a los reintentos de A: %v", got)
		}
		list, _ := e.store.ListCommands(ctx, CommandFilter{Org: org, Status: CommandSent})
		for _, c := range list {
			if c.IdempotencyKey == "a1" && c.Attempts != 3 {
				t.Fatalf("a1 intentos = %d", c.Attempts)
			}
		}
	})
}

func TestOutbound_IdempotencyKeyDedupes(t *testing.T) {
	eachDB(t, func(t *testing.T, db *gorm.DB) {
		e := newOutboundEnv(t, db)
		ctx := context.Background()
		first, err := e.out.Publish(ctx, cmd("A", "k1"))
		if err != nil {
			t.Fatal(err)
		}
		again, err := e.out.Publish(ctx, cmd("A", "k1"))
		if err != nil || again.ID != first.ID {
			t.Fatalf("repetido = %+v, %v", again, err)
		}
		if n, _ := e.store.CountCommands(ctx, org, "erp", ""); n != 1 {
			t.Fatalf("comandos = %d", n)
		}
		second, _ := e.out.Publish(ctx, cmd("A", "k2"))
		if second.Seq != first.Seq+1 {
			t.Fatalf("seq = %d tras %d", second.Seq, first.Seq)
		}
	})
}

func TestOutbound_PermanentErrorDeadLettersAndBlocksAggregateUntilRetry(t *testing.T) {
	eachDB(t, func(t *testing.T, db *gorm.DB) {
		e := newOutboundEnv(t, db)
		ctx := context.Background()
		e.conn.plan["a1"] = []error{Permanent(errors.New("422 rfc inválido"))}
		a1, _ := e.out.Publish(ctx, cmd("A", "a1"))
		_, _ = e.out.Publish(ctx, cmd("A", "a2"))
		e.start(t)
		select {
		case d := <-e.dead:
			if d.ID != a1.ID || d.Attempts != 1 {
				t.Fatalf("muerto = %+v", d)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("a1 no llegó a muertos")
		}
		time.Sleep(100 * time.Millisecond)
		if n, _ := e.out.Sweep(ctx, 10); n != 0 {
			t.Fatalf("un agregado bloqueado no se despierta (%d)", n)
		}
		if len(e.conn.order()) != 0 {
			t.Fatalf("a2 salió con a1 muerto: %v", e.conn.order())
		}
		if _, err := e.out.RetryDead(ctx, a1.ID); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "a1 y a2 entregados", func() bool { return len(e.conn.order()) == 2 })
		if got := e.conn.order(); got[0] != "a1" || got[1] != "a2" {
			t.Fatalf("orden = %v", got)
		}
	})
}

func TestOutbound_MaxAttemptsDeadLettersAndDiscardUnblocks(t *testing.T) {
	eachDB(t, func(t *testing.T, db *gorm.DB) {
		e := newOutboundEnv(t, db)
		ctx := context.Background()
		boom := errors.New("500")
		e.conn.plan["a1"] = []error{boom, boom, boom, boom}
		a1, _ := e.out.Publish(ctx, cmd("A", "a1"))
		_, _ = e.out.Publish(ctx, cmd("A", "a2"))
		e.start(t)
		select {
		case d := <-e.dead:
			if d.Attempts != 3 {
				t.Fatalf("intentos = %d", d.Attempts)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("a1 no agotó intentos")
		}
		if _, err := e.out.Discard(ctx, a1.ID, "operador"); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "a2 entregado", func() bool { return len(e.conn.order()) == 1 })
		got, _, _ := e.store.GetCommand(ctx, a1.ID)
		if got.Status != CommandDiscarded {
			t.Fatalf("a1 = %s", got.Status)
		}
		if _, err := e.out.Discard(ctx, a1.ID, "x"); err == nil {
			t.Fatal("solo se descarta un dead")
		}
	})
}

func TestOutbound_DeliveryRecordsRefForEchoSuppression(t *testing.T) {
	eachDB(t, func(t *testing.T, db *gorm.DB) {
		e := newOutboundEnv(t, db)
		ctx := context.Background()
		local := "22222222-2222-2222-2222-222222222222"
		c := cmd("customers:"+local, "occ-1")
		c.Entity, c.LocalID = "customers.Customer", local
		e.conn.reply["occ-1"] = Delivery{ExternalID: "erp:clients:8301", SourceVersion: ver(30, "z")}
		_, _ = e.out.Publish(ctx, c)
		e.start(t)
		waitFor(t, "entregado", func() bool { return len(e.conn.order()) == 1 })
		waitFor(t, "ref registrada", func() bool {
			r, ok, _ := e.store.GetRefByLocal(ctx, org, "erp", "customers.Customer", local)
			return ok && r.ExternalID == "erp:clients:8301" && r.State == RefPendingLegacy && r.CreatedIn == OwnerLocal
		})
		// La réplica que vuelve con esa versión solo confirma.
		ap, _ := NewApplier(Config{Store: e.store, Sink: newFakeSink(), Atomic: GormAtomic(db)})
		echo := Envelope{Source: "erp", Entity: "customers.Customer", ExternalID: "erp:clients:8301", Op: OpUpsert,
			SourceVersion: ver(30, "z"), Aggregate: Aggregate{Root: map[string]any{}}}
		if got := ap.Apply(ctx, org, echo); got.Outcome != OutcomeConfirmed || got.LocalID != local {
			t.Fatalf("eco = %+v", got)
		}
	})
}

func TestOutbound_PublishJoinsCallerTransaction(t *testing.T) {
	db := sqliteDB(t)
	e := newOutboundEnv(t, db)
	ctx := context.Background()
	rollback := errors.New("rollback")
	err := GormAtomic(db)(ctx, func(ctx context.Context) error {
		if _, err := e.out.Publish(ctx, cmd("A", "tx1")); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	if n, _ := e.store.CountCommands(ctx, org, "erp", ""); n != 0 {
		t.Fatal("el comando se revierte con la transacción del negocio")
	}
	jobs, _, _ := e.jobs.List(ctx, outbox.Filter{})
	if len(jobs) != 0 {
		t.Fatal("la bomba también se revierte")
	}
}

func TestRouter_FollowsOwnershipAndIgnoresBridgeWrites(t *testing.T) {
	db := sqliteDB(t)
	e := newOutboundEnv(t, db)
	ctx := context.Background()
	translate := func(kind string) Translate {
		return func(_ context.Context, ev dynamic.CanonicalEvent) ([]Command, error) {
			b, _ := json.Marshal(ev.After)
			return []Command{{Kind: kind, Payload: b}}, nil
		}
	}
	r, err := NewRouter(RouterConfig{System: "erp", Store: e.store, Outbound: e.out, Routes: []Route{
		{Model: "customers", Actions: []string{"created", "updated"}, Direction: ToOwner, Translate: translate("customers.upsert")},
		{Model: "stock_levels", Direction: Replicate, Translate: translate("stock.levels")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	_ = e.store.PutRule(ctx, OwnershipRule{Org: org, System: "erp", Branch: "b5", Owner: OwnerLocal})

	ev := dynamic.CanonicalEvent{ID: "e1", OccurrenceID: "occ-1", Model: "customers", Action: "created", BranchID: "b1", After: map[string]any{"id": "c1"}}
	got, err := r.Handle(ctx, org, ev)
	if err != nil || len(got) != 1 || got[0].IdempotencyKey != "occ-1:customers.upsert:0" || got[0].AggregateKey != "customers:c1" {
		t.Fatalf("hacia el dueño legado = %+v, %v", got, err)
	}
	// Re-publicación del mismo evento (relay del outbox): no duplica.
	again, _ := r.Handle(ctx, org, ev)
	if len(again) != 1 || again[0].ID != got[0].ID {
		t.Fatal("re-entrega del mismo occurrence no duplica")
	}
	// Una acción fuera de la ruta no produce nada.
	if out, _ := r.Handle(ctx, org, dynamic.CanonicalEvent{ID: "e2", Model: "customers", Action: "deleted", BranchID: "b1"}); len(out) != 0 {
		t.Fatal("deleted no está en la ruta")
	}
	// Existencias: en b1 (dueño legado) no se replican; en b5 (cortada) sí.
	if out, _ := r.Handle(ctx, org, dynamic.CanonicalEvent{ID: "e3", Model: "stock_levels", Action: "updated", BranchID: "b1"}); len(out) != 0 {
		t.Fatal("réplica solo cuando el dueño es el host")
	}
	if out, _ := r.Handle(ctx, org, dynamic.CanonicalEvent{ID: "e4", Model: "stock_levels", Action: "updated", BranchID: "b5"}); len(out) != 1 {
		t.Fatal("réplica de la sucursal cortada")
	}
	// Lo que escribió el propio puente no vuelve al legado.
	if out, _ := r.Handle(ctx, org, dynamic.CanonicalEvent{ID: "e5", Model: "customers", Action: "updated", BranchID: "b1", CorrelationID: CorrelationPrefix + "erp"}); len(out) != 0 {
		t.Fatal("eco del puente")
	}
	if _, err := NewRouter(RouterConfig{System: "erp", Store: e.store, Outbound: e.out, Routes: []Route{{Model: "x", Translate: translate("x"), Direction: "both"}}}); err == nil {
		t.Fatal("dirección inválida")
	}
}
