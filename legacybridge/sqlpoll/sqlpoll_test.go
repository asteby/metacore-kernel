package sqlpoll

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	lb "github.com/asteby/metacore-kernel/legacybridge"
	"github.com/google/uuid"
	_ "github.com/mattn/go-sqlite3"
)

const org = "org-1"

var t0 = time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)

func legacyDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", fmt.Sprintf("file:sqlpoll_%s?mode=memory&cache=shared&_busy_timeout=5000", uuid.NewString()[:8]))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	for _, q := range []string{
		`CREATE TABLE product_categories (id INTEGER PRIMARY KEY, category TEXT, updated_at DATETIME)`,
		`CREATE TABLE products (id INTEGER PRIMARY KEY, sku TEXT, name TEXT, purchase_price TEXT, category_id INTEGER, active INTEGER, barcode TEXT, branch_id INTEGER, updated_at DATETIME)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func stamp(sec int) string {
	return t0.Add(time.Duration(sec) * time.Second).Format("2006-01-02 15:04:05")
}

func exec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

func profile() Profile {
	return Profile{System: "7l", Streams: []Stream{
		{Table: "product_categories", Entity: "categories", UpdatedAt: "updated_at",
			Columns: []Column{{From: "category", To: "name"}}},
		{Table: "products", Entity: "products", UpdatedAt: "updated_at", BranchColumn: "branch_id",
			Columns: []Column{
				{From: "sku", To: "sku"}, {From: "name", To: "name"}, {From: "purchase_price", To: "cost_price"},
				{From: "category_id", To: "category_id", Kind: KindRef, RefTable: "product_categories", RefEntity: "categories", Optional: true},
				{From: "active", To: "is_active", Kind: KindBool},
				{From: "barcode", To: "barcode", Kind: KindMeta}, {From: "id", To: "legacy_id", Kind: KindMeta},
				{To: "unit_of_measure", Kind: KindConst, Value: "unit"},
			}},
	}}
}

func newSource(t *testing.T, db *sql.DB, now time.Time) *Source {
	t.Helper()
	s, err := New(db, SQLite, profile())
	if err != nil {
		t.Fatal(err)
	}
	s.Now = func() time.Time { return now }
	s.Settle = 5 * time.Second
	return s
}

// memSink guarda la última raíz por (entidad, id local).
type memSink struct {
	mu   sync.Mutex
	rows map[string]map[string]any
	dead map[string]bool
}

func newMemSink() *memSink {
	return &memSink{rows: map[string]map[string]any{}, dead: map[string]bool{}}
}

func (m *memSink) DeriveID(org, entity, ext string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(org+"|"+entity+"|"+ext)).String()
}
func (m *memSink) Exists(_ context.Context, _, entity, local string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.rows[entity+"|"+local]
	return ok && !m.dead[entity+"|"+local], nil
}
func (m *memSink) Upsert(_ context.Context, w lb.Write) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[w.Envelope.Entity+"|"+w.LocalID] = w.Envelope.Aggregate.Root
	delete(m.dead, w.Envelope.Entity+"|"+w.LocalID)
	return nil
}
func (m *memSink) Delete(_ context.Context, w lb.Write) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dead[w.Envelope.Entity+"|"+w.LocalID] = true
	return nil
}
func (m *memSink) get(entity, ext string) (map[string]any, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := m.DeriveID(org, entity, ext)
	r, ok := m.rows[entity+"|"+id]
	return r, ok && !m.dead[entity+"|"+id]
}

type env struct {
	store *lb.MemoryStore
	sink  *memSink
	ap    *lb.Applier
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{store: lb.NewMemoryStore(), sink: newMemSink()}
	ap, err := lb.NewApplier(lb.Config{Store: e.store, Sink: e.sink, BranchEntity: "branches"})
	if err != nil {
		t.Fatal(err)
	}
	e.ap = ap
	// La sucursal 1 ya existe en el host (carga de sucursales).
	_ = e.sink.Upsert(context.Background(), lb.Write{Envelope: lb.Envelope{Entity: "branches"}, LocalID: e.sink.DeriveID(org, "branches", "7l:branches:1")})
	return e
}

func seed(t *testing.T, db *sql.DB) {
	exec(t, db, `INSERT INTO product_categories (id, category, updated_at) VALUES (1, 'Llantas', ?), (2, 'Rines', ?)`, stamp(1), stamp(1))
	for i := 1; i <= 7; i++ {
		exec(t, db, `INSERT INTO products (id, sku, name, purchase_price, category_id, active, barcode, branch_id, updated_at) VALUES (?, ?, ?, '100.50', ?, 1, ?, 1, ?)`,
			i, fmt.Sprintf("SKU-%d", i), fmt.Sprintf("Producto %d", i), 1+i%2, fmt.Sprintf("750%d", i), stamp(2+i/3))
	}
}

func total(reps []StreamReport, o lb.Outcome) int {
	n := 0
	for _, r := range reps {
		n += r.Counts[o]
	}
	return n
}

func TestProfile_RejectsUnsafeIdentifiers(t *testing.T) {
	bad := []Profile{
		{System: "7l", Streams: []Stream{{Table: "products; DROP TABLE x", Entity: "p", Columns: []Column{{From: "a", To: "a"}}}}},
		{System: "7l", Streams: []Stream{{Table: "products", Entity: "p", UpdatedAt: "updated_at`", Columns: []Column{{From: "a", To: "a"}}}}},
		{System: "7l", Streams: []Stream{{Table: "products", Entity: "p", Columns: []Column{{From: "a) OR (1=1", To: "a"}}}}},
		{System: "7l", Streams: []Stream{{Table: "products", Entity: "p", Columns: []Column{{From: "c", To: "c", Kind: KindRef, RefTable: "x\"y", RefEntity: "x"}}}}},
		{System: "7:l", Streams: []Stream{{Table: "products", Entity: "p", Columns: []Column{{From: "a", To: "a"}}}}},
		{System: "7l", Streams: []Stream{{Table: "products", Entity: "p", Columns: []Column{{From: "a", To: "a", Kind: "sql"}}}}},
		{System: "7l", Streams: []Stream{{Table: "products", Entity: "p", Columns: []Column{{From: "a", To: "a"}}}, {Table: "products", Entity: "q", Columns: []Column{{From: "a", To: "a"}}}}},
	}
	for i, p := range bad {
		if _, err := p.Normalize(); err == nil {
			t.Errorf("perfil %d debió rechazarse", i)
		}
	}
	if _, err := profile().Normalize(); err != nil {
		t.Fatal(err)
	}
}

func TestPoll_OrdersByUpdatedAtAndKeyRespectsSettleAndPages(t *testing.T) {
	db := legacyDB(t)
	seed(t, db)
	// Recién modificada: dentro de la ventana de asentamiento, no se lee aún.
	exec(t, db, `INSERT INTO products (id, sku, name, purchase_price, category_id, active, branch_id, updated_at) VALUES (99, 'SKU-99', 'Nuevo', '1', 1, 1, 1, ?)`, stamp(98))
	s := newSource(t, db, t0.Add(100*time.Second))
	s.PageSize = 3
	var got []string
	cur := ""
	for i := 0; i < 10; i++ {
		b, err := s.Poll(context.Background(), "products", cur)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range b.Envelopes {
			got = append(got, e.ExternalID)
		}
		if len(b.Envelopes) == 0 {
			if b.Cursor != cur {
				t.Fatal("un lote vacío no mueve el cursor")
			}
			break
		}
		if lb.CompareCursors(b.Cursor, cur) <= 0 {
			t.Fatalf("el cursor no avanzó: %s → %s", cur, b.Cursor)
		}
		cur = b.Cursor
	}
	want := "7l:products:1,7l:products:2,7l:products:3,7l:products:4,7l:products:5,7l:products:6,7l:products:7"
	if strings.Join(got, ",") != want {
		t.Fatalf("orden = %v", got)
	}
	b, _ := s.Poll(context.Background(), "products", "")
	e := b.Envelopes[0]
	if e.BranchExternalID != "7l:branches:1" || e.Requires[0].ExternalID != "7l:product_categories:2" ||
		e.Aggregate.Root["category_id"] != "7l:product_categories:2" || e.Aggregate.Root["is_active"] != true ||
		e.Aggregate.Root["unit_of_measure"] != "unit" || e.Aggregate.Root["metadata"].(map[string]any)["barcode"] != "7501" {
		t.Fatalf("envelope = %+v", e)
	}
	if !strings.HasPrefix(e.SourceVersion, "2026-10-18T10:00:02Z#") {
		t.Fatalf("versión = %s", e.SourceVersion)
	}
}

func TestSync_InitialLoadIsIdempotentAndPicksUpChanges(t *testing.T) {
	db := legacyDB(t)
	seed(t, db)
	e := newEnv(t)
	s := newSource(t, db, t0.Add(time.Hour))
	s.PageSize = 2
	ctx := context.Background()
	reps, err := s.Sync(ctx, org, e.store, e.ap)
	if err != nil {
		t.Fatal(err)
	}
	if got := total(reps, lb.OutcomeApplied); got != 9 {
		t.Fatalf("aplicados = %d (%+v)", got, reps)
	}
	if r, ok := e.sink.get("products", "7l:products:3"); !ok || r["name"] != "Producto 3" {
		t.Fatalf("producto 3 = %v", r)
	}
	// Segunda corrida: nada nuevo.
	reps, _ = s.Sync(ctx, org, e.store, e.ap)
	if total(reps, lb.OutcomeApplied)+total(reps, lb.OutcomeDuplicate) != 0 {
		t.Fatalf("la segunda corrida releyó: %+v", reps)
	}
	exec(t, db, `UPDATE products SET name = 'Producto 3 bis', updated_at = ? WHERE id = 3`, stamp(50))
	reps, _ = s.Sync(ctx, org, e.store, e.ap)
	if total(reps, lb.OutcomeApplied) != 1 {
		t.Fatalf("cambio no aplicado: %+v", reps)
	}
	if r, _ := e.sink.get("products", "7l:products:3"); r["name"] != "Producto 3 bis" {
		t.Fatalf("producto 3 = %v", r)
	}
}

// flaky aplica la mitad de un lote y falla (como una caída a media corrida).
type flaky struct {
	ap    *lb.Applier
	fails int
}

func (f *flaky) Ingest(ctx context.Context, org string, b lb.Batch) (lb.IngestResult, error) {
	if f.fails > 0 {
		f.fails--
		half := b
		half.Envelopes = b.Envelopes[:len(b.Envelopes)/2]
		half.Cursor = ""
		_, _ = f.ap.Ingest(ctx, org, half)
		return lb.IngestResult{}, errors.New("conexión perdida")
	}
	return f.ap.Ingest(ctx, org, b)
}

func TestSync_ResumesAfterCrashWithoutLosingOrDuplicating(t *testing.T) {
	db := legacyDB(t)
	seed(t, db)
	e := newEnv(t)
	s := newSource(t, db, t0.Add(time.Hour))
	s.PageSize = 4
	ctx := context.Background()
	f := &flaky{ap: e.ap}
	reps, _ := s.Sync(ctx, org, e.store, f, "product_categories")
	if reps[0].Error != "" {
		t.Fatal(reps[0].Error)
	}
	f.fails = 1
	reps, _ = s.Sync(ctx, org, e.store, f, "products")
	if reps[0].Error == "" {
		t.Fatal("la caída debió reportarse")
	}
	if cur, _ := e.store.GetCursor(ctx, org, "7l", "products"); cur != "" {
		t.Fatalf("el cursor avanzó tras la caída: %s", cur)
	}
	reps, _ = s.Sync(ctx, org, e.store, f, "products")
	if reps[0].Error != "" || reps[0].Counts[lb.OutcomeDuplicate] != 2 || reps[0].Counts[lb.OutcomeApplied] != 5 {
		t.Fatalf("reanudación = %+v", reps[0])
	}
	for i := 1; i <= 7; i++ {
		if _, ok := e.sink.get("products", fmt.Sprintf("7l:products:%d", i)); !ok {
			t.Fatalf("falta el producto %d", i)
		}
	}
}

func TestSync_TwoSimultaneousRunsConverge(t *testing.T) {
	db := legacyDB(t)
	seed(t, db)
	e := newEnv(t)
	s := newSource(t, db, t0.Add(time.Hour))
	s.PageSize = 2
	ctx := context.Background()
	var wg sync.WaitGroup
	reps := make([][]StreamReport, 2)
	for i := range reps {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			reps[i], _ = s.Sync(ctx, org, e.store, e.ap)
		}(i)
	}
	wg.Wait()
	applied := total(reps[0], lb.OutcomeApplied) + total(reps[1], lb.OutcomeApplied)
	if applied != 9 {
		t.Fatalf("aplicados entre las dos = %d (cada fila una sola vez)", applied)
	}
	cur, _ := e.store.GetCursor(ctx, org, "7l", "products")
	if !strings.HasPrefix(cur, "2026-10-18T10:00:04Z#") || !strings.HasSuffix(cur, "#00000000000000000007") {
		t.Fatalf("cursor = %s", cur)
	}
}

func TestReconcile_FindsSilentUpdatesMissingRowsAndHardDeletes(t *testing.T) {
	db := legacyDB(t)
	seed(t, db)
	e := newEnv(t)
	s := newSource(t, db, t0.Add(time.Hour))
	ctx := context.Background()
	if _, err := s.Sync(ctx, org, e.store, e.ap); err != nil {
		t.Fatal(err)
	}
	ingest := func(b lb.Batch) error {
		_, err := e.ap.Ingest(ctx, org, b)
		return err
	}
	rep, err := s.Reconcile(ctx, org, "products", e.store, ingest)
	if err != nil || rep.Diffs() != 0 || rep.InSync != 7 {
		t.Fatalf("recién sincronizado: %+v %v", rep, err)
	}
	// UPDATE sin tocar updated_at, borrado en duro y una fila vieja (updated_at
	// anterior al cursor) que la corrida incremental no ve.
	exec(t, db, `UPDATE products SET name = 'Cambiado a mano' WHERE id = 2`)
	exec(t, db, `DELETE FROM products WHERE id = 5`)
	exec(t, db, `INSERT INTO products (id, sku, name, purchase_price, category_id, active, branch_id, updated_at) VALUES (8, 'SKU-8', 'Tardío', '1', 1, 1, 1, ?)`, stamp(0))
	reps, _ := s.Sync(ctx, org, e.store, e.ap, "products")
	if reps[0].Counts[lb.OutcomeApplied] != 0 {
		t.Fatalf("la incremental no debía ver nada: %+v", reps[0])
	}
	rep, err = s.Reconcile(ctx, org, "products", e.store, ingest)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Changed != 1 || rep.Missing != 1 || rep.Deleted != 1 || rep.Emitted != 3 {
		t.Fatalf("reconciliación = %+v", rep)
	}
	if r, _ := e.sink.get("products", "7l:products:2"); r["name"] != "Cambiado a mano" {
		t.Fatalf("producto 2 = %v", r)
	}
	if _, ok := e.sink.get("products", "7l:products:5"); ok {
		t.Fatal("el producto 5 sigue vivo")
	}
	if _, ok := e.sink.get("products", "7l:products:8"); !ok {
		t.Fatal("falta el producto 8")
	}
	rep, _ = s.Reconcile(ctx, org, "products", e.store, ingest)
	if rep.Diffs() != 0 {
		t.Fatalf("segunda reconciliación = %+v", rep)
	}
}

func TestSource_InterpretsWallClockInLocation(t *testing.T) {
	db := legacyDB(t)
	exec(t, db, `INSERT INTO product_categories (id, category, updated_at) VALUES (1, 'A', '2026-10-18 04:00:00')`)
	s := newSource(t, db, time.Date(2026, 10, 18, 10, 0, 30, 0, time.UTC))
	mx, err := time.LoadLocation("America/Mexico_City")
	if err != nil {
		t.Skip("sin tzdata")
	}
	s.Location = mx
	b, err := s.Poll(context.Background(), "product_categories", "")
	if err != nil {
		t.Fatal(err)
	}
	// 04:00 en CDMX (UTC-6) = 10:00Z, ya asentado a las 10:00:30Z.
	if len(b.Envelopes) != 1 || !strings.HasPrefix(b.Envelopes[0].SourceVersion, "2026-10-18T10:00:00Z#") {
		t.Fatalf("lote = %+v", b)
	}
}
