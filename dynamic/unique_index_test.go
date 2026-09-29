package dynamic

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
)

// pgUniqueSetup creates rx_registers_<sfx> on Postgres and a Service whose
// rx_registers model writes there under rxUniqueRules.
func pgUniqueSetup(t *testing.T) (*Service, *gorm.DB, string) {
	t.Helper()
	db, sfx := pgTestDB(t)
	table := "rx_registers_" + sfx
	mustExec(t, db, fmt.Sprintf(`CREATE TABLE %s (id uuid PRIMARY KEY, organization_id uuid, created_at timestamptz, updated_at timestamptz,
		deleted_at timestamptz, created_by_id uuid, branch_id uuid, code text, name text, active boolean)`, table))
	t.Cleanup(func() { db.Exec("DROP TABLE IF EXISTS " + table) })
	modelbase.Register("rx_registers", func() modelbase.ModelDefiner { return &RxRegister{} })
	svc := New(Config{
		DB:       db,
		Metadata: metadata.New(metadata.Config{CacheTTL: -1}),
		TableNameResolver: func(_ context.Context, model string) (string, bool) {
			return table, model == "rx_registers"
		},
		ConstraintResolver: func(_ context.Context, model string) (*ModelConstraints, bool) {
			return &ModelConstraints{Rules: rxUniqueRules}, model == "rx_registers"
		},
	})
	return svc, db, table
}

func mustExecArgs(t *testing.T, db *gorm.DB, sql string, args ...any) {
	t.Helper()
	if err := db.Exec(sql, args...).Error; err != nil {
		t.Fatalf("exec %s: %v", sql, err)
	}
}

func pgInsertRegister(t *testing.T, db *gorm.DB, table string, org uuid.UUID, branch, code string, active bool) string {
	t.Helper()
	id := uuid.NewString()
	mustExecArgs(t, db, fmt.Sprintf(`INSERT INTO %s (id, organization_id, branch_id, code, name, active) VALUES (?, ?, ?, ?, 'x', ?)`, table),
		id, org, branch, code, active)
	return id
}

func TestUniqueIndexName_DeterministicAndBounded(t *testing.T) {
	r := rxUniqueRules[0]
	a, b := UniqueIndexName("public.cash_registers", r), UniqueIndexName("cash_registers", r)
	if a != b || a[:len("uq_cash_registers_")] != "uq_cash_registers_" {
		t.Fatalf("names %q %q", a, b)
	}
	other := r
	other.Where = map[string]any{"active": false}
	if UniqueIndexName("cash_registers", other) == a {
		t.Fatal("a different where must name a different index")
	}
	long := UniqueIndexName("a_very_long_table_name_that_goes_on_and_on_and_on_and_on_forever", r)
	if len(long) > 63 {
		t.Fatalf("name %q exceeds 63 bytes", long)
	}
}

// Two concurrent creates of the same tuple: exactly one wins, with and
// without the materialized index (the advisory lock serializes them).
func TestUniqueRule_ConcurrentCreatesOnlyOneWins(t *testing.T) {
	for _, indexed := range []bool{false, true} {
		t.Run(fmt.Sprintf("indexed=%v", indexed), func(t *testing.T) {
			svc, db, table := pgUniqueSetup(t)
			user := newUser(uuid.New())
			if indexed {
				st := MaterializeUniqueRules(context.Background(), db, table, rxUniqueRules)
				if len(st) != 1 || st[0].Enforcement != UniqueEnforcedByIndex {
					t.Fatalf("materialize: %+v", st)
				}
			}
			branch := uuid.NewString()
			const n = 8
			var wg sync.WaitGroup
			errs := make([]error, n)
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					_, errs[i] = svc.Create(context.Background(), "rx_registers", user,
						map[string]any{"branch_id": branch, "code": "CJ1", "name": "Caja", "active": true})
				}(i)
			}
			wg.Wait()
			ok := 0
			for _, err := range errs {
				var ue *UniqueViolationError
				switch {
				case err == nil:
					ok++
				case !errors.As(err, &ue):
					t.Fatalf("want a unique violation, got %v", err)
				}
			}
			if ok != 1 {
				t.Fatalf("%d creates succeeded, want exactly 1", ok)
			}
		})
	}
}

func TestMaterializeUnique_CleanTableBuildsIndexThatBlocksRawWriters(t *testing.T) {
	_, db, table := pgUniqueSetup(t)
	ctx := context.Background()
	org, branch := uuid.New(), uuid.NewString()
	pgInsertRegister(t, db, table, org, branch, "CJ1", true)
	pgInsertRegister(t, db, table, org, branch, "CJ1", false) // inactive: outside where
	st := MaterializeUniqueRules(ctx, db, table, rxUniqueRules)
	if len(st) != 1 || st[0].Enforcement != UniqueEnforcedByIndex || st[0].BlockedGroups != 0 || st[0].Error != "" {
		t.Fatalf("status %+v", st)
	}
	// Idempotent.
	if again := MaterializeUniqueRules(ctx, db, table, rxUniqueRules); again[0].Enforcement != UniqueEnforcedByIndex || again[0].Error != "" {
		t.Fatalf("second run %+v", again)
	}
	// A raw writer (bypassing the kernel) now hits the index…
	err := db.Exec(fmt.Sprintf(`INSERT INTO %s (id, organization_id, branch_id, code, active) VALUES (?, ?, ?, 'CJ1', true)`, table), uuid.New(), org, branch).Error
	if err == nil {
		t.Fatal("a raw duplicate must violate the materialized index")
	}
	// …and the kernel maps that 23505 back to the rule.
	if ue := uniqueIndexViolation(err, table, rxUniqueRules); !errors.Is(ue, ErrConstraintViolation) {
		t.Fatalf("23505 not mapped to the rule: %v", ue)
	}
	// Outside the partial predicate nothing collides: another org, empty code,
	// an inactive or soft-deleted row.
	pgInsertRegister(t, db, table, uuid.New(), branch, "CJ1", true)
	pgInsertRegister(t, db, table, org, branch, "", true)
	pgInsertRegister(t, db, table, org, branch, "", true)
	mustExecArgs(t, db, fmt.Sprintf(`INSERT INTO %s (id, organization_id, branch_id, code, active, deleted_at) VALUES (?, ?, ?, 'CJ1', true, now())`, table), uuid.New(), org, branch)
}

func TestMaterializeUnique_DuplicatesKeepAppModeUntilResolved(t *testing.T) {
	svc, db, table := pgUniqueSetup(t)
	ctx := context.Background()
	org, branch := uuid.New(), uuid.NewString()
	pgInsertRegister(t, db, table, org, branch, "CJ1", true)
	dup := pgInsertRegister(t, db, table, org, branch, "CJ1", true)

	st := MaterializeUniqueRules(ctx, db, table, rxUniqueRules)
	if len(st) != 1 || st[0].Enforcement != UniqueEnforcedByApp || st[0].BlockedGroups != 1 || st[0].Error != "" {
		t.Fatalf("with duplicates: %+v", st)
	}
	if exists, _, _ := indexValid(ctx, db, "", st[0].Index); exists {
		t.Fatal("no index may be left behind while duplicates exist")
	}
	user := newUser(org)
	rep, err := svc.UniqueViolations(ctx, "rx_registers", user)
	if err != nil || len(rep) != 1 || rep[0].DuplicateGroups != 1 || rep[0].Enforcement != UniqueEnforcedByApp || len(rep[0].Groups[0].IDs) != 2 {
		t.Fatalf("report: %+v %v", rep, err)
	}
	// Resolve through the kernel (deactivate the duplicate), then retry.
	if _, err := svc.Update(ctx, "rx_registers", user, uuid.MustParse(dup), map[string]any{"active": false}); err != nil {
		t.Fatal(err)
	}
	after, err := svc.MaterializeUniqueIndexes(ctx, "rx_registers", user)
	if err != nil || len(after) != 1 || after[0].Enforcement != UniqueEnforcedByIndex {
		t.Fatalf("retry: %+v %v", after, err)
	}
	rep, _ = svc.UniqueViolations(ctx, "rx_registers", user)
	if rep[0].DuplicateGroups != 0 || rep[0].Enforcement != UniqueEnforcedByIndex || len(rep[0].Groups) != 0 {
		t.Fatalf("report after: %+v", rep)
	}
}

func TestMaterializeUnique_RebuildsInvalidIndexAndWorksInsideTransaction(t *testing.T) {
	_, db, table := pgUniqueSetup(t)
	ctx := context.Background()
	name := UniqueIndexName(table, rxUniqueRules[0])
	// Simulate an interrupted CONCURRENTLY build: an INVALID index by that name.
	mustExec(t, db, fmt.Sprintf(`CREATE UNIQUE INDEX %q ON %s (code)`, name, table))
	mustExecArgs(t, db, `UPDATE pg_index SET indisvalid = false WHERE indexrelid = to_regclass(?)`, fmt.Sprintf("%q", name))
	err := db.Transaction(func(tx *gorm.DB) error {
		st := MaterializeUniqueRules(ctx, tx, table, rxUniqueRules)
		if len(st) != 1 || st[0].Enforcement != UniqueEnforcedByIndex {
			return fmt.Errorf("status %+v", st)
		}
		// The caller's transaction is still usable.
		return tx.Exec("SELECT 1").Error
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, valid, _ := indexValid(ctx, db, "", name); !valid {
		t.Fatal("index not rebuilt valid")
	}
}

// The report is per organization (SQLite path: no index, still reported).
func TestUniqueViolations_ReportIsOrgScoped(t *testing.T) {
	svc, _, user, _, legacy := rxUniqueSetup(t)
	ctx := context.Background()
	rep, err := svc.UniqueViolations(ctx, "rx_registers", user)
	if err != nil || len(rep) != 1 {
		t.Fatalf("report: %+v %v", rep, err)
	}
	g := rep[0]
	if g.DuplicateGroups != 1 || g.Enforcement != UniqueEnforcedByApp || g.Field != "code" || len(g.Groups) != 1 || g.Groups[0].Count != 2 || g.Groups[0].Values["code"] != "CJ1" {
		t.Fatalf("group: %+v", g)
	}
	found := false
	for _, id := range g.Groups[0].IDs {
		found = found || id == legacy
	}
	if !found {
		t.Fatalf("ids %v miss %s", g.Groups[0].IDs, legacy)
	}
	other, err := svc.UniqueViolations(ctx, "rx_registers", newUser(uuid.New()))
	if err != nil || other[0].DuplicateGroups != 0 || len(other[0].Groups) != 0 {
		t.Fatalf("another org must see nothing: %+v %v", other, err)
	}
	// SQLite has no partial unique index: materialize reports app, no error.
	st, err := svc.MaterializeUniqueIndexes(ctx, "rx_registers", user)
	if err != nil || len(st) != 1 || st[0].Enforcement != UniqueEnforcedByApp || st[0].Error != "" {
		t.Fatalf("sqlite materialize: %+v %v", st, err)
	}
}

