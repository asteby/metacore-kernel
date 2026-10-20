package legacybridge

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/dataload"
)

const org = "org-1"

type applyEnv struct {
	store  Store
	sink   *fakeSink
	review *dataload.MemoryReviewStore
	clk    *clock
	ap     *Applier
}

func newApplyEnv(t *testing.T, store Store, mut ...func(*Config)) *applyEnv {
	t.Helper()
	e := &applyEnv{store: store, sink: newFakeSink(), review: dataload.NewMemoryReviewStore(), clk: newClock()}
	cfg := Config{Store: store, Sink: e.sink, Review: e.review, BranchEntity: "branches", Now: e.clk.Now,
		Backoff: func(int) time.Duration { return time.Minute }}
	if g, ok := store.(*GormStore); ok {
		cfg.Atomic = GormAtomic(g.db)
		g.Now = e.clk.Now
	}
	for _, m := range mut {
		m(&cfg)
	}
	ap, err := NewApplier(cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.ap = ap
	return e
}

func order(ext string, sec int, hash string, lines ...map[string]any) Envelope {
	return Envelope{Source: "erp", Entity: "sales.SalesOrder", ExternalID: ext, Op: OpUpsert, SourceVersion: ver(sec, hash),
		Aggregate:        Aggregate{Root: map[string]any{"folio": ext, "h": hash}, Children: map[string][]map[string]any{"lines": lines}},
		Facets:           []Facet{{Entity: "customers.Invoice", ExternalID: ext + ":ar", Row: map[string]any{"total": 10}}},
		Requires:         []Dependency{{Entity: "customers.Customer", ExternalID: "erp:clients:1"}},
		BranchExternalID: "erp:branches:1"}
}

func (e *applyEnv) seedBranchAndCustomer() {
	e.sink.seed(org, "branches", "erp:branches:1")
	e.sink.seed(org, "customers.Customer", "erp:clients:1")
}

func reviewCodes(t *testing.T, r *dataload.MemoryReviewStore) map[string]string {
	t.Helper()
	list, err := r.List(context.Background(), org, dataload.ReviewFilter{})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range list {
		out[e.Key] = e.Items[0].Code
	}
	return out
}

// Las pruebas del aplicador corren igual sobre MemoryStore y GormStore (SQLite).
func eachStore(t *testing.T, fn func(t *testing.T, s Store)) {
	t.Run("memory", func(t *testing.T) { fn(t, NewMemoryStore()) })
	t.Run("sqlite", func(t *testing.T) { fn(t, gormStore(t, sqliteDB(t))) })
	t.Run("postgres", func(t *testing.T) { fn(t, gormStore(t, postgresDB(t))) })
}

func TestApply_UpsertMapsRootAndFacetsThenIsIdempotent(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		e := newApplyEnv(t, s)
		e.seedBranchAndCustomer()
		ctx := context.Background()
		env := order("erp:orders:7", 1, "a", map[string]any{"sku": "X"})
		r := e.ap.Apply(ctx, org, env)
		if r.Outcome != OutcomeApplied || r.LocalID != e.sink.DeriveID(org, env.Entity, env.ExternalID) {
			t.Fatalf("%+v", r)
		}
		ref, ok, _ := s.GetRef(ctx, org, "erp", "customers.Invoice", "erp:orders:7:ar")
		if !ok || ref.SourceVersion != env.SourceVersion || ref.LocalID == "" {
			t.Fatalf("faceta sin mapeo: %+v", ref)
		}
		if got := e.ap.Apply(ctx, org, env); got.Outcome != OutcomeDuplicate {
			t.Fatalf("reenvío = %s", got.Outcome)
		}
		// Misma huella con versión nueva (el origen tocó updated_at sin cambiar nada).
		same := env
		same.SourceVersion = ver(5, "a")
		if got := e.ap.Apply(ctx, org, same); got.Outcome != OutcomeDuplicate {
			t.Fatalf("misma huella = %s", got.Outcome)
		}
		if root, _, _ := s.GetRef(ctx, org, "erp", env.Entity, env.ExternalID); root.SourceVersion != same.SourceVersion {
			t.Fatal("la versión avanza aunque no se reescriba")
		}
		if len(e.sink.writes) != 1 {
			t.Fatalf("escrituras = %d, quiero 1", len(e.sink.writes))
		}
	})
}

func TestApply_OrderPerRowDiscardsStaleAndReplacesChildren(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		e := newApplyEnv(t, s)
		e.seedBranchAndCustomer()
		ctx := context.Background()
		e.ap.Apply(ctx, org, order("erp:orders:7", 5, "b", map[string]any{"sku": "X"}, map[string]any{"sku": "Y"}))
		if got := e.ap.Apply(ctx, org, order("erp:orders:7", 3, "a", map[string]any{"sku": "X"})); got.Outcome != OutcomeStale {
			t.Fatalf("versión vieja = %s", got.Outcome)
		}
		newer := order("erp:orders:7", 9, "c", map[string]any{"sku": "X"})
		if got := e.ap.Apply(ctx, org, newer); got.Outcome != OutcomeApplied {
			t.Fatalf("versión nueva = %s", got.Outcome)
		}
		last := e.sink.writes[len(e.sink.writes)-1]
		if n := len(last.Envelope.Aggregate.Children["lines"]); n != 1 || last.Created {
			t.Fatalf("el sink recibe el conjunto completo de hijos (1) como actualización: %d, created=%v", n, last.Created)
		}
		// Otra fila no depende del orden de la primera.
		if got := e.ap.Apply(ctx, org, order("erp:orders:8", 1, "a")); got.Outcome != OutcomeApplied {
			t.Fatalf("otra fila = %s", got.Outcome)
		}
	})
}

func TestApply_WaitsForDependenciesThenAppliesOrGoesToReview(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		e := newApplyEnv(t, s)
		ctx := context.Background()
		e.sink.seed(org, "branches", "erp:branches:1")
		env := order("erp:orders:7", 1, "a")
		if got := e.ap.Apply(ctx, org, env); got.Outcome != OutcomeWaiting {
			t.Fatalf("sin cliente = %s", got.Outcome)
		}
		if n, _ := s.CountPending(ctx, org, "erp"); n != 1 {
			t.Fatalf("en espera = %d", n)
		}
		if len(e.sink.writes) != 0 {
			t.Fatal("nunca se escribe sin el padre (sin filas fantasma)")
		}
		// Aún no vence: RetryWaiting no lo toma.
		if res, _ := e.ap.RetryWaiting(ctx, 10); len(res) != 0 {
			t.Fatalf("reintentó antes de tiempo: %+v", res)
		}
		// Llega el cliente por el puente.
		cust := Envelope{Source: "erp", Entity: "customers.Customer", ExternalID: "erp:clients:1", Op: OpUpsert, SourceVersion: ver(1, "c"),
			Aggregate: Aggregate{Root: map[string]any{"name": "C"}}}
		if got := e.ap.Apply(ctx, org, cust); got.Outcome != OutcomeApplied {
			t.Fatalf("cliente = %s", got.Outcome)
		}
		e.clk.Add(2 * time.Minute)
		res, err := e.ap.RetryWaiting(ctx, 10)
		if err != nil || len(res) != 1 || res[0].Outcome != OutcomeApplied {
			t.Fatalf("reintento = %+v, %v", res, err)
		}
		if n, _ := s.CountPending(ctx, org, "erp"); n != 0 {
			t.Fatal("aplicado sale de la espera")
		}

		// Otro que nunca recibe su padre: tras MaxWait va a revisión.
		lost := order("erp:orders:9", 1, "a")
		lost.Requires = []Dependency{{Entity: "products", ExternalID: "erp:products:404"}}
		e.ap.Apply(ctx, org, lost)
		e.clk.Add(25 * time.Hour)
		res, _ = e.ap.RetryWaiting(ctx, 10)
		if len(res) != 1 || res[0].Outcome != OutcomeReview {
			t.Fatalf("vencido = %+v", res)
		}
		if reviewCodes(t, e.review)["erp|erp:orders:9"] != ReviewCodeDependencyMissing {
			t.Fatalf("revisión = %v", reviewCodes(t, e.review))
		}
		if n, _ := s.CountPending(ctx, org, "erp"); n != 0 {
			t.Fatal("vencido sale de la espera")
		}
	})
}

func TestApply_UnmappedBranchWaits(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		e := newApplyEnv(t, s)
		e.sink.seed(org, "customers.Customer", "erp:clients:1")
		got := e.ap.Apply(context.Background(), org, order("erp:orders:7", 1, "a"))
		if got.Outcome != OutcomeWaiting {
			t.Fatalf("sucursal sin mapear = %s (no se puede decidir el dueño)", got.Outcome)
		}
	})
}

func TestApply_LocalOwnerIsAConflict(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		e := newApplyEnv(t, s)
		e.seedBranchAndCustomer()
		ctx := context.Background()
		branchID := e.sink.DeriveID(org, "branches", "erp:branches:1")
		if err := s.PutRule(ctx, OwnershipRule{Org: org, System: "erp", Branch: branchID, Owner: OwnerLocal, Note: "flip", UpdatedBy: "u1"}); err != nil {
			t.Fatal(err)
		}
		got := e.ap.Apply(ctx, org, order("erp:orders:7", 1, "a"))
		if got.Outcome != OutcomeConflict {
			t.Fatalf("sucursal cortada = %s", got.Outcome)
		}
		if reviewCodes(t, e.review)["erp|erp:orders:7"] != ReviewCodeOwnerConflict {
			t.Fatal("el conflicto va a revisión")
		}
		if len(e.sink.writes) != 0 {
			t.Fatal("no se escribe sobre un alcance del host")
		}
		// La fase cambia el resultado: una regla de fase F0 devuelve el alcance al legado.
		_ = s.PutRule(ctx, OwnershipRule{Org: org, System: "erp", Entity: "sales.SalesOrder", Phase: "F0", Owner: OwnerLegacy})
		_ = s.SetPhase(ctx, org, "erp", "F0", "u1")
		if got := e.ap.Apply(ctx, org, order("erp:orders:7", 2, "b")); got.Outcome != OutcomeApplied {
			t.Fatalf("en F0 = %s", got.Outcome)
		}
	})
}

func TestApply_EchoOfOwnCommandOnlyConfirms(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		e := newApplyEnv(t, s)
		ctx := context.Background()
		local := "11111111-1111-1111-1111-111111111111"
		// El comando customers.upsert se entregó: el legado asignó erp:clients:8301 en la versión v.
		v := ver(30, "z")
		if err := s.PutRef(ctx, Ref{Org: org, System: "erp", Entity: "customers.Customer", ExternalID: "erp:clients:8301",
			LocalID: local, CreatedIn: OwnerLocal, SourceVersion: v, State: RefPendingLegacy}); err != nil {
			t.Fatal(err)
		}
		echo := Envelope{Source: "erp", Entity: "customers.Customer", ExternalID: "erp:clients:8301", Op: OpUpsert, SourceVersion: v,
			Aggregate: Aggregate{Root: map[string]any{"name": "N"}}}
		if got := e.ap.Apply(ctx, org, echo); got.Outcome != OutcomeConfirmed || got.LocalID != local {
			t.Fatalf("eco = %+v", got)
		}
		ref, _, _ := s.GetRef(ctx, org, "erp", "customers.Customer", "erp:clients:8301")
		if ref.State != RefSynced || ref.LocalID != local {
			t.Fatalf("ref = %+v", ref)
		}
		if len(e.sink.writes) != 0 {
			t.Fatal("el eco no reescribe la fila")
		}
		// Un cambio posterior en el legado sí se aplica, sobre el MISMO id local (no duplica).
		later := echo
		later.SourceVersion = ver(40, "w")
		later.Aggregate.Root = map[string]any{"name": "N2"}
		if got := e.ap.Apply(ctx, org, later); got.Outcome != OutcomeApplied || got.LocalID != local {
			t.Fatalf("cambio posterior = %+v", got)
		}
	})
}

func TestApply_DeleteSoftDeletesAndMarksRef(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		e := newApplyEnv(t, s)
		ctx := context.Background()
		cust := Envelope{Source: "erp", Entity: "customers.Customer", ExternalID: "erp:clients:5", Op: OpUpsert, SourceVersion: ver(1, "a"),
			Aggregate: Aggregate{Root: map[string]any{"name": "C"}}}
		e.ap.Apply(ctx, org, cust)
		del := Envelope{Source: "erp", Entity: "customers.Customer", ExternalID: "erp:clients:5", Op: OpDelete, SourceVersion: ver(2, "d")}
		if got := e.ap.Apply(ctx, org, del); got.Outcome != OutcomeDeleted {
			t.Fatalf("delete = %s", got.Outcome)
		}
		ref, _, _ := s.GetRef(ctx, org, "erp", "customers.Customer", "erp:clients:5")
		if ref.State != RefDeleted {
			t.Fatalf("ref = %+v", ref)
		}
		if exists, _ := e.sink.Exists(ctx, org, "customers.Customer", ref.LocalID); exists {
			t.Fatal("borrado lógico")
		}
		// Re-creado en el origen con el mismo id: vuelve.
		cust.SourceVersion = ver(3, "a")
		if got := e.ap.Apply(ctx, org, cust); got.Outcome != OutcomeApplied {
			t.Fatalf("re-creado = %s", got.Outcome)
		}
	})
}

func TestApply_InvalidAndRefConflictGoToReview(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		e := newApplyEnv(t, s)
		ctx := context.Background()
		if got := e.ap.Apply(ctx, org, Envelope{Source: "erp", Entity: "x", ExternalID: "erp:x:1"}); got.Outcome != OutcomeReview {
			t.Fatalf("inválido = %s", got.Outcome)
		}
		// El id local derivado ya está mapeado a otro external_id (duplicado lógico).
		taken := e.sink.DeriveID(org, "customers.Customer", "erp:clients:2")
		_ = s.PutRef(ctx, Ref{Org: org, System: "erp", Entity: "customers.Customer", ExternalID: "erp:clients:OTRO", LocalID: taken})
		dup := Envelope{Source: "erp", Entity: "customers.Customer", ExternalID: "erp:clients:2", Op: OpUpsert, SourceVersion: ver(1, "a"),
			Aggregate: Aggregate{Root: map[string]any{}}}
		if got := e.ap.Apply(ctx, org, dup); got.Outcome != OutcomeReview {
			t.Fatalf("conflicto de mapeo = %s", got.Outcome)
		}
		codes := reviewCodes(t, e.review)
		if codes["erp|erp:x:1"] != ReviewCodeInvalid || codes["erp|erp:clients:2"] != ReviewCodeRefConflict {
			t.Fatalf("revisión = %v", codes)
		}
	})
}

func TestIngest_CursorAdvancesOnlyWithoutInfraErrors(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		e := newApplyEnv(t, s)
		e.seedBranchAndCustomer()
		ctx := context.Background()
		b := Batch{Source: "erp", Cursor: ver(10, ""), Envelopes: []Envelope{order("erp:orders:1", 1, "a"), order("erp:orders:2", 2, "a")}}
		got, err := e.ap.Ingest(ctx, org, b)
		if err != nil || !got.Advanced || got.Cursor != b.Cursor || got.Counts[OutcomeApplied] != 2 {
			t.Fatalf("%+v %v", got, err)
		}
		// Un cursor viejo no retrocede.
		old := Batch{Source: "erp", Cursor: ver(5, "")}
		if got, _ := e.ap.Ingest(ctx, org, old); got.Advanced || got.Cursor != b.Cursor {
			t.Fatalf("retrocedió: %+v", got)
		}
		// Falla de infraestructura: el cursor no avanza y el lote se puede reintentar.
		e.sink.fail = errors.New("db caída")
		failing := Batch{Source: "erp", Cursor: ver(20, ""), Envelopes: []Envelope{order("erp:orders:3", 3, "a")}}
		got, _ = e.ap.Ingest(ctx, org, failing)
		if got.Advanced || got.Counts[OutcomeError] != 1 {
			t.Fatalf("con error: %+v", got)
		}
		e.sink.fail = nil
		if got, _ := e.ap.Ingest(ctx, org, failing); !got.Advanced || got.Counts[OutcomeApplied] != 1 {
			t.Fatalf("reintento: %+v", got)
		}
		// Un error del dato (Invalid) va a revisión y no detiene el cursor.
		e.sink.fail = Invalid(errors.New("columna x: valor ilegible"))
		poison := Batch{Source: "erp", Cursor: ver(30, ""), Envelopes: []Envelope{order("erp:orders:4", 4, "a")}}
		if got, _ := e.ap.Ingest(ctx, org, poison); !got.Advanced || got.Counts[OutcomeReview] != 1 {
			t.Fatalf("fila envenenada: %+v", got)
		}
		if reviewCodes(t, e.review)["erp|erp:orders:4"] != ReviewCodeInvalid {
			t.Fatalf("revisión = %v", reviewCodes(t, e.review))
		}
		e.sink.fail = nil
		// Un envelope de otro origen dentro del lote va a revisión.
		mixed := Batch{Source: "erp", Envelopes: []Envelope{{Source: "otro", Entity: "x", ExternalID: "1", Op: OpDelete, SourceVersion: "v"}}}
		if got, _ := e.ap.Ingest(ctx, org, mixed); got.Counts[OutcomeReview] != 1 {
			t.Fatalf("origen mezclado: %+v", got)
		}
	})
}
