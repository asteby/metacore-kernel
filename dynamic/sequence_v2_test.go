package dynamic

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm/logger"
)

func withClock(t *testing.T, at time.Time) *time.Time {
	t.Helper()
	cur := at
	old := sequenceNow
	sequenceNow = func() time.Time { return cur }
	t.Cleanup(func() { sequenceNow = old })
	return &cur
}

func TestFormatSequenceAt_Tokens(t *testing.T) {
	at := time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC)
	got := FormatSequenceAt("{scope}-{yyyy}{mm}{dd}-{seq:04}/{yy}", 7, at, "A1")
	if want := "A1-20260309-0007/26"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	// Sin fecha ni scope, los tokens quedan tal cual (compat con FormatSequence).
	if got := FormatSequence("X-{seq:03}", 5); got != "X-005" {
		t.Fatalf("legacy format: %q", got)
	}
}

func folioV2(spec manifest.SequenceDef) *ModelSequences {
	spec.Key = "folio"
	return &ModelSequences{Sequences: []manifest.SequenceDef{spec}, ColumnBindings: map[string]string{"name": "folio"}}
}

// P4: un contador por valor de columna (emisor) y que reinicia por periodo.
func TestSequencesV2_ScopeColumnAndPeriod(t *testing.T) {
	clock := withClock(t, time.Date(2026, 12, 31, 10, 0, 0, 0, time.UTC))
	db := setupTestDB(t)
	svc := sequenceService(t, db, folioV2(manifest.SequenceDef{
		Scope: "column", ScopeColumn: "product_specs", Period: "year", Format: "{scope}-{yyyy}-{seq:03}",
	}))
	user := newUser(uuid.New())
	mk := func(emisor string) string {
		out, err := svc.Create(context.Background(), "test_products", user, map[string]any{"price": 1.0, "product_specs": emisor})
		if err != nil {
			t.Fatal(err)
		}
		return out["name"].(string)
	}
	for i, want := range []string{"EMI1-2026-001", "EMI1-2026-002"} {
		if got := mk("EMI1"); got != want {
			t.Fatalf("emisor 1 #%d: %q want %q", i, got, want)
		}
	}
	if got := mk("EMI2"); got != "EMI2-2026-001" {
		t.Fatalf("emisor 2 owns its own series: %q", got)
	}
	*clock = time.Date(2027, 1, 1, 0, 0, 1, 0, time.UTC)
	if got := mk("EMI1"); got != "EMI1-2027-001" {
		t.Fatalf("a new year restarts the counter: %q", got)
	}
}

func TestSequencesV2_NextSequenceScoped(t *testing.T) {
	withClock(t, time.Date(2026, 5, 4, 0, 0, 0, 0, time.UTC))
	db := setupTestDB(t)
	svc := sequenceService(t, db, folioV2(manifest.SequenceDef{
		Scope: "column", ScopeColumn: "product_specs", Period: "day", Format: "{scope}-{dd}-{seq:02}",
	}))
	user := newUser(uuid.New())
	if _, err := svc.NextSequenceScoped(context.Background(), user, "test_products", "folio", ""); err == nil {
		t.Fatal("scope=column without scope_value must fail")
	}
	for _, want := range []string{"C1-04-01", "C1-04-02"} {
		got, err := svc.NextSequenceScoped(context.Background(), user, "test_products", "folio", "C1")
		if err != nil || got != want {
			t.Fatalf("got %q err %v want %q", got, err, want)
		}
	}
}

// gapless: el import sequence_next se niega; el numero solo sale por la columna.
func TestSequencesV2_GaplessRefusesStandaloneNext(t *testing.T) {
	db := setupTestDB(t)
	svc := sequenceService(t, db, folioV2(manifest.SequenceDef{Gapless: true, Format: "G-{seq:03}"}))
	if _, err := svc.NextSequence(context.Background(), newUser(uuid.New()), "test_products", "folio"); err == nil {
		t.Fatal("gapless sequence must not be drawn through sequence_next")
	}
}

// Un INSERT fallido libera el folio gapless; el autocommit (por defecto) lo quema.
func TestSequencesV2_GaplessRollbackReleasesNumber(t *testing.T) {
	for _, tc := range []struct {
		name    string
		gapless bool
		second  string
	}{
		{"gapless", true, "F-0002"},
		{"gap-tolerant", false, "F-0003"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := setupTestDB(t)
			svc := sequenceService(t, db, folioV2(manifest.SequenceDef{Gapless: tc.gapless, Format: "F-{seq:04}"}))
			user := newUser(uuid.New())
			ctx := context.Background()
			id := uuid.New().String()
			if out, err := svc.Create(ctx, "test_products", user, map[string]any{"id": id, "price": 1.0}); err != nil || out["name"] != "F-0001" {
				t.Fatalf("first: %v %v", out, err)
			}
			// Mismo id => el INSERT revienta DESPUES de numerar.
			if _, err := svc.Create(ctx, "test_products", user, map[string]any{"id": id, "price": 2.0}); err == nil {
				t.Fatal("duplicate id insert must fail")
			}
			out, err := svc.Create(ctx, "test_products", user, map[string]any{"price": 3.0})
			if err != nil || out["name"] != tc.second {
				t.Fatalf("next folio = %v (err %v), want %s", out["name"], err, tc.second)
			}
		})
	}
}

// CON-01: 50 creaciones concurrentes => 50 folios unicos (y, gapless, 1..50).
func TestSequencesV2_CON01_ConcurrentCreates(t *testing.T) {
	run := func(t *testing.T, db *gorm.DB, gapless bool) {
		svc := sequenceService(t, db, folioV2(manifest.SequenceDef{Gapless: gapless, Format: "C-{seq:03}"}))
		org := uuid.New()
		const n = 50
		var (
			wg    sync.WaitGroup
			mu    sync.Mutex
			names []string
			errs  []error
		)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				out, err := svc.Create(context.Background(), "test_products", newUser(org), map[string]any{"price": 1.0})
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					errs = append(errs, err)
					return
				}
				names = append(names, out["name"].(string))
			}()
		}
		wg.Wait()
		if len(errs) > 0 {
			t.Fatalf("%d creates failed, first: %v", len(errs), errs[0])
		}
		seen := map[string]bool{}
		for _, nm := range names {
			if seen[nm] {
				t.Fatalf("duplicate folio %s", nm)
			}
			seen[nm] = true
		}
		if len(seen) != n {
			t.Fatalf("got %d unique folios, want %d", len(seen), n)
		}
		if gapless {
			sort.Strings(names)
			for i, nm := range names {
				if want := fmt.Sprintf("C-%03d", i+1); nm != want {
					t.Fatalf("gapless folio #%d = %s, want %s", i, nm, want)
				}
			}
		}
	}
	for _, gapless := range []bool{false, true} {
		t.Run(fmt.Sprintf("SQLite_gapless=%v", gapless), func(t *testing.T) {
			db := setupTestDB(t)
			sqlDB, _ := db.DB()
			sqlDB.SetMaxOpenConns(1)
			run(t, db, gapless)
		})
		t.Run(fmt.Sprintf("Postgres_gapless=%v", gapless), func(t *testing.T) {
			dsn := os.Getenv("TEST_POSTGRES_DSN")
			if dsn == "" {
				t.Skip("TEST_POSTGRES_DSN not set")
			}
			db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
			if err != nil {
				t.Fatal(err)
			}
			sqlDB, _ := db.DB()
			sqlDB.SetMaxOpenConns(20)
			mustExec(t, db, `CREATE TABLE IF NOT EXISTS test_products (
				id uuid PRIMARY KEY, organization_id uuid, created_by_id uuid,
				created_at timestamptz, updated_at timestamptz, deleted_at timestamptz,
				name text, price double precision, product_specs text)`)
			run(t, db, gapless)
		})
	}
}

// assign_on: transition:<stage> numera al entrar a la etapa, no al crear.
type transitionSeqFixture struct {
	svc  *Service
	user *fakeUser
}

func newTransitionSeqFixture(t *testing.T) *transitionSeqFixture {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{DisableForeignKeyConstraintWhenMigrating: true})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	db.Exec(`CREATE TABLE IF NOT EXISTS test_orders (
		id TEXT PRIMARY KEY, organization_id TEXT, created_by_id TEXT,
		created_at DATETIME, updated_at DATETIME, deleted_at DATETIME,
		reference TEXT, status TEXT)`)
	modelbase.Register("test_orders", func() modelbase.ModelDefiner { return &TestOrder{} })
	sm := &StageMachine{
		Field:  "status",
		Stages: []manifest.StageDef{{Key: "draft"}, {Key: "posted"}, {Key: "cancelled"}},
		Transitions: []manifest.TransitionDef{
			{From: "draft", To: "posted"}, {From: "draft", To: "cancelled"},
		},
	}
	ms := &ModelSequences{
		Sequences:      []manifest.SequenceDef{{Key: "ref", Gapless: true, AssignOn: "transition:posted", Format: "P-{seq:04}"}},
		ColumnBindings: map[string]string{"reference": "ref"},
	}
	svc := New(Config{
		AllowUnconfiguredPermissions: true, DB: db,
		Metadata:             metadata.New(metadata.Config{CacheTTL: -1}),
		StageMachineResolver: func(_ context.Context, m string) (*StageMachine, bool) { return sm, m == "test_orders" },
		SequenceResolver: func(_ context.Context, m string) (*ModelSequences, bool) {
			return ms, m == "test_orders"
		},
	})
	return &transitionSeqFixture{svc: svc, user: newUser(uuid.New())}
}

func TestSequencesV2_AssignOnTransition(t *testing.T) {
	f := newTransitionSeqFixture(t)
	svc, user := f.svc, f.user
	ctx := context.Background()
	mk := func() uuid.UUID {
		out, err := svc.Create(ctx, "test_orders", user, map[string]any{"status": "draft"})
		if err != nil {
			t.Fatal(err)
		}
		if out["reference"] != "" && out["reference"] != nil {
			t.Fatalf("a draft must not be numbered, got %v", out["reference"])
		}
		return uuid.MustParse(out["id"].(string))
	}
	a, b, c := mk(), mk(), mk()
	post := func(id uuid.UUID) string {
		out, err := svc.Update(ctx, "test_orders", user, id, map[string]any{"status": "posted"})
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprint(out["reference"])
	}
	if got := post(b); got != "P-0001" {
		t.Fatalf("first posted = %s", got)
	}
	if _, err := svc.Update(ctx, "test_orders", user, c, map[string]any{"status": "cancelled"}); err != nil {
		t.Fatal(err)
	}
	if got := post(a); got != "P-0002" {
		t.Fatalf("a cancelled draft must not burn a number; got %s", got)
	}
}

// {scope} con la columna de scope vacia: 422, nunca "{scope}-001".
func TestSequencesV2_EmptyScopeTokenRejected(t *testing.T) {
	db := setupTestDB(t)
	svc := sequenceService(t, db, folioV2(manifest.SequenceDef{
		Scope: "column", ScopeColumn: "product_specs", Format: "{scope}-{seq:03}",
	}))
	_, err := svc.Create(context.Background(), "test_products", newUser(uuid.New()), map[string]any{"price": 1.0})
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("empty scope with {scope}: %v, want validation error", err)
	}
	if _, ok := ve.Fields["product_specs"]; !ok {
		t.Fatalf("error must name the scope column: %+v", ve.Fields)
	}
}

// Un scope largo + periodo desborda varchar(64) en Postgres: se reemplaza por
// un hash estable (misma entrada => misma serie).
func TestSequencesV2_LongScopeIsHashed(t *testing.T) {
	long := strings.Repeat("x", 90)
	spec := manifest.SequenceDef{Scope: "column", ScopeColumn: "c", Period: "day", Format: "{seq}"}
	at := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	a, err := resolvePartition(spec, "", map[string]any{"c": long}, at)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := resolvePartition(spec, "", map[string]any{"c": long}, at)
	c, _ := resolvePartition(spec, "", map[string]any{"c": long + "y"}, at)
	if len(a.value) > 64 || a.value != b.value || a.value == c.value {
		t.Fatalf("hash partition: %q %q %q", a.value, b.value, c.value)
	}
}

// Un POST que crea directo en la etapa de assign_on recibe su folio al crear.
func TestSequencesV2_CreateDirectlyInAssignOnStage(t *testing.T) {
	f := newTransitionSeqFixture(t)
	out, err := f.svc.Create(context.Background(), "test_orders", f.user, map[string]any{"status": "posted"})
	if err != nil {
		t.Fatal(err)
	}
	if out["reference"] != "P-0001" {
		t.Fatalf("born in posted must be numbered, got %v", out["reference"])
	}
}

// En gapless un folio explicito del cliente se rechaza (protected_field).
func TestSequencesV2_GaplessRejectsExplicitFolio(t *testing.T) {
	db := setupTestDB(t)
	svc := sequenceService(t, db, folioV2(manifest.SequenceDef{Gapless: true, Format: "G-{seq:03}"}))
	_, err := svc.Create(context.Background(), "test_products", newUser(uuid.New()), map[string]any{"name": "HACK-1", "price": 1.0})
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("explicit gapless folio: %v, want protected_field", err)
	}
}

// Config.SequenceClock corta el periodo en el calendario de la organizacion.
func TestSequencesV2_SequenceClockHook(t *testing.T) {
	db := setupTestDB(t)
	svc := sequenceService(t, db, folioV2(manifest.SequenceDef{Period: "day", Format: "{yyyy}{mm}{dd}-{seq:02}"}))
	// 2026-03-09 23:30 UTC es 2026-03-10 en una zona UTC+2.
	svc.sequenceClock = func(context.Context, uuid.UUID) time.Time {
		return time.Date(2026, 3, 9, 23, 30, 0, 0, time.UTC).In(time.FixedZone("x", 2*3600))
	}
	got, err := svc.NextSequence(context.Background(), newUser(uuid.New()), "test_products", "folio")
	if err != nil || got != "20260310-01" {
		t.Fatalf("org-calendar day: %q %v", got, err)
	}
}
