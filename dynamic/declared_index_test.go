package dynamic

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/asteby/metacore-kernel/manifest"
)

func declaredDefs() []manifest.ModelDefinition {
	return []manifest.ModelDefinition{{
		ModelKey: "Lead", TableName: "crm_leads",
		Columns: []manifest.ColumnDef{{Name: "customer_id", Type: "uuid"}},
		Indices: []manifest.IndexDef{
			{Name: "crm_leads_org_customer_idx", Columns: []string{"organization_id", "customer_id"}},
			{Name: "crm_leads_org_customer_uq", Columns: []string{"organization_id", "customer_id"}, Unique: true},
			{Name: "crm_leads_ext_partial", Columns: []string{"organization_id", "customer_id"}, Where: "customer_id IS NOT NULL"},
			{Name: "crm_leads_single", Columns: []string{"customer_id"}},
		},
	}, {
		ModelKey: "Conversation", TableName: "conversations",
		Indices: []manifest.IndexDef{{Name: "conversations_org_customer_idx", Columns: []string{"organization_id", "customer_id"}}},
	}}
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestDeclaredIndexPlanOnlyCompositeNonUnique(t *testing.T) {
	got := DeclaredIndexPlan(declaredDefs()[0])
	if len(got) != 1 || got[0].Name != "crm_leads_org_customer_idx" {
		t.Fatalf("plan = %+v", got)
	}
	if p := DeclaredIndexPlan(manifest.ModelDefinition{View: "SELECT 1", Indices: declaredDefs()[0].Indices}); p != nil {
		t.Fatalf("a view has no declared indexes: %+v", p)
	}
}

func TestDeclaredIndexSQL(t *testing.T) {
	create, drop, err := declaredIndexSQL("public", "crm_leads", "crm_leads_org_customer_idx",
		manifest.IndexDef{Columns: []string{"organization_id", "customer_id"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := `CREATE INDEX CONCURRENTLY IF NOT EXISTS "crm_leads_org_customer_idx" ON "public"."crm_leads" USING btree ("organization_id", "customer_id")`; create != want {
		t.Fatalf("create:\n got %s\nwant %s", create, want)
	}
	if want := `DROP INDEX CONCURRENTLY IF EXISTS "public"."crm_leads_org_customer_idx"`; drop != want {
		t.Fatalf("drop:\n got %s\nwant %s", drop, want)
	}
	create, _, err = declaredIndexSQL("addon_crm", "t", "t_ab", manifest.IndexDef{Columns: []string{"a", "b"}, Method: "BRIN"})
	if err != nil || !strings.Contains(create, `ON "addon_crm"."t" USING brin ("a", "b")`) {
		t.Fatalf("method/schema: %q %v", create, err)
	}
}

func TestDeclaredIndexSQLRejectsUnsafeInput(t *testing.T) {
	ok := manifest.IndexDef{Columns: []string{"a", "b"}}
	cases := []struct {
		name, schema, table, index string
		ix                         manifest.IndexDef
	}{
		{"schema quote", `public"; DROP TABLE x; --`, "t", "i", ok},
		{"table space", "public", "t x", "i", ok},
		{"index upper", "public", "t", "Idx", ok},
		{"index too long", "public", "t", strings.Repeat("a", 64), ok},
		{"column injection", "public", "t", "i", manifest.IndexDef{Columns: []string{"a", "b) WHERE (true"}}},
		{"column expression", "public", "t", "i", manifest.IndexDef{Columns: []string{"a", "lower(b)"}}},
		{"single column", "public", "t", "i", manifest.IndexDef{Columns: []string{"a"}}},
		{"repeated column", "public", "t", "i", manifest.IndexDef{Columns: []string{"a", "a"}}},
		{"method", "public", "t", "i", manifest.IndexDef{Columns: []string{"a", "b"}, Method: "btree; DROP"}},
		{"empty schema", "", "t", "i", ok},
	}
	for _, c := range cases {
		if create, _, err := declaredIndexSQL(c.schema, c.table, c.index, c.ix); err == nil {
			t.Errorf("%s: accepted, rendered %q", c.name, create)
		}
	}
	if _, err := declaredIdent(`x"y`); !errors.Is(err, ErrDeclaredIndexIdentifier) {
		t.Fatalf("want ErrDeclaredIndexIdentifier, got %v", err)
	}
}

func TestPlanDeclaredIndexesOnlyAndCoreTables(t *testing.T) {
	res, pending := planDeclaredIndexes(declaredDefs(), DeclaredIndexOptions{
		Schema: "public", CoreTables: []string{"conversations"},
	})
	if len(res) != 2 || len(pending) != 1 || res[pending[0]].Name != "crm_leads_org_customer_idx" {
		t.Fatalf("res=%+v pending=%v", res, pending)
	}
	if res[1].State != DeclaredIndexSkipped || res[1].Reason != DeclaredIndexReasonCoreTable {
		t.Fatalf("core table not skipped: %+v", res[1])
	}
	res, pending = planDeclaredIndexes(declaredDefs(), DeclaredIndexOptions{
		Schema: "public", OnlyIndexes: []string{" conversations_org_customer_idx "},
	})
	if len(pending) != 1 || res[pending[0]].Name != "conversations_org_customer_idx" ||
		res[0].State != DeclaredIndexSkipped || res[0].Reason != DeclaredIndexReasonNotInOnly {
		t.Fatalf("only filter: res=%+v pending=%v", res, pending)
	}
	// The lock key is per schema+index, so the same name in two schemas does
	// not serialize, and two replicas on the same index do.
	if res[pending[0]].lockKey != "metacore.declared_index:public.conversations_org_customer_idx" {
		t.Fatalf("lock key %q", res[pending[0]].lockKey)
	}
}

// fakeDeclaredDB is an in-memory catalog: tables (schema.table -> relkind and
// columns) and indexes (schema.name). exec interprets the two statements the
// builder emits.
type fakeDeclaredDB struct {
	kinds   map[string]string
	cols    map[string]map[string]bool
	idx     map[string]declaredIndexInfo
	locked  map[string]bool // held by ANOTHER session
	mine    map[string]bool
	execs   []string
	onExec  func(stmt string) error
	invalid bool // CREATE leaves the index INVALID
}

func newFakeDeclaredDB() *fakeDeclaredDB {
	return &fakeDeclaredDB{
		kinds:  map[string]string{"public.crm_leads": "r", "addon_crm.crm_leads": "r", "public.v": "v"},
		cols:   map[string]map[string]bool{"public.crm_leads": {"id": true, "organization_id": true, "customer_id": true}},
		idx:    map[string]declaredIndexInfo{},
		locked: map[string]bool{}, mine: map[string]bool{},
	}
}

func (f *fakeDeclaredDB) relKind(_ context.Context, s, r string) (string, error) {
	return f.kinds[s+"."+r], nil
}
func (f *fakeDeclaredDB) columns(_ context.Context, s, t string) (map[string]bool, error) {
	return f.cols[s+"."+t], nil
}
func (f *fakeDeclaredDB) index(_ context.Context, s, n string) (declaredIndexInfo, error) {
	return f.idx[s+"."+n], nil
}
func (f *fakeDeclaredDB) tryLock(_ context.Context, k string) (bool, error) {
	if f.locked[k] {
		return false, nil
	}
	f.mine[k] = true
	return true, nil
}
func (f *fakeDeclaredDB) unlock(k string) { delete(f.mine, k) }
func (f *fakeDeclaredDB) exec(_ context.Context, stmt string) error {
	f.execs = append(f.execs, stmt)
	if f.onExec != nil {
		if err := f.onExec(stmt); err != nil {
			return err
		}
	}
	if strings.HasPrefix(stmt, "CREATE INDEX ") && !strings.HasPrefix(stmt, "CREATE INDEX CONCURRENTLY ") {
		panic("plain CREATE INDEX emitted: " + stmt)
	}
	key := "public.crm_leads_org_customer_idx"
	switch {
	case strings.HasPrefix(stmt, "DROP INDEX CONCURRENTLY"):
		delete(f.idx, key)
	case strings.HasPrefix(stmt, "CREATE INDEX CONCURRENTLY"):
		if _, ok := f.idx[key]; !ok {
			f.idx[key] = declaredIndexInfo{exists: true, kind: "i", valid: !f.invalid, table: "crm_leads"}
		}
	}
	return nil
}

func runFake(t *testing.T, ctx context.Context, f *fakeDeclaredDB, opts DeclaredIndexOptions) DeclaredIndexResult {
	t.Helper()
	if opts.Schema == "" {
		opts.Schema = "public"
	}
	res, pending := planDeclaredIndexes(declaredDefs()[:1], opts)
	runDeclaredIndexes(ctx, f, res, pending, opts, quietLog())
	if len(f.mine) != 0 {
		t.Fatalf("advisory lock leaked: %v", f.mine)
	}
	return res[0]
}

func TestEnsureDeclaredIndexBuildsMissing(t *testing.T) {
	f := newFakeDeclaredDB()
	r := runFake(t, context.Background(), f, DeclaredIndexOptions{})
	if r.State != DeclaredIndexBuilt || len(f.execs) != 1 || !strings.HasPrefix(f.execs[0], "CREATE INDEX CONCURRENTLY IF NOT EXISTS") {
		t.Fatalf("state=%s execs=%v err=%v", r.State, f.execs, r.Err)
	}
	// Second run: valid index, nothing executed.
	f.execs = nil
	if r := runFake(t, context.Background(), f, DeclaredIndexOptions{}); r.State != DeclaredIndexExists || len(f.execs) != 0 {
		t.Fatalf("rerun state=%s execs=%v", r.State, f.execs)
	}
}

func TestEnsureDeclaredIndexRebuildsInvalid(t *testing.T) {
	f := newFakeDeclaredDB()
	f.idx["public.crm_leads_org_customer_idx"] = declaredIndexInfo{exists: true, kind: "i", valid: false, table: "crm_leads"}
	r := runFake(t, context.Background(), f, DeclaredIndexOptions{})
	if r.State != DeclaredIndexRebuilt || len(f.execs) != 2 ||
		!strings.HasPrefix(f.execs[0], "DROP INDEX CONCURRENTLY IF EXISTS") ||
		!strings.HasPrefix(f.execs[1], "CREATE INDEX CONCURRENTLY IF NOT EXISTS") {
		t.Fatalf("state=%s execs=%v", r.State, f.execs)
	}
	if !f.idx["public.crm_leads_org_customer_idx"].valid {
		t.Fatal("index not valid after rebuild")
	}
}

func TestEnsureDeclaredIndexBuildEndsInvalidIsDropped(t *testing.T) {
	f := newFakeDeclaredDB()
	f.invalid = true
	r := runFake(t, context.Background(), f, DeclaredIndexOptions{})
	if r.State != DeclaredIndexFailed || len(f.execs) != 2 || !strings.HasPrefix(f.execs[1], "DROP INDEX CONCURRENTLY") {
		t.Fatalf("state=%s execs=%v", r.State, f.execs)
	}
	if _, ok := f.idx["public.crm_leads_org_customer_idx"]; ok {
		t.Fatal("INVALID leftover kept")
	}
}

func TestEnsureDeclaredIndexCreateErrorDropsLeftover(t *testing.T) {
	f := newFakeDeclaredDB()
	f.onExec = func(stmt string) error {
		if strings.HasPrefix(stmt, "CREATE") {
			f.idx["public.crm_leads_org_customer_idx"] = declaredIndexInfo{exists: true, kind: "i", table: "crm_leads"}
			return errors.New("canceling statement due to lock timeout")
		}
		return nil
	}
	r := runFake(t, context.Background(), f, DeclaredIndexOptions{})
	if r.State != DeclaredIndexFailed || !strings.Contains(r.Error, "lock timeout") || len(f.execs) != 2 {
		t.Fatalf("state=%s err=%q execs=%v", r.State, r.Error, f.execs)
	}
}

func TestEnsureDeclaredIndexBusyWhenLocked(t *testing.T) {
	f := newFakeDeclaredDB()
	f.locked["metacore.declared_index:public.crm_leads_org_customer_idx"] = true
	// INVALID while another replica builds it: must NOT be dropped.
	f.idx["public.crm_leads_org_customer_idx"] = declaredIndexInfo{exists: true, kind: "i", table: "crm_leads"}
	if r := runFake(t, context.Background(), f, DeclaredIndexOptions{}); r.State != DeclaredIndexBusy || len(f.execs) != 0 {
		t.Fatalf("state=%s execs=%v", r.State, f.execs)
	}
}

func TestEnsureDeclaredIndexDryRun(t *testing.T) {
	f := newFakeDeclaredDB()
	r := runFake(t, context.Background(), f, DeclaredIndexOptions{DryRun: true})
	if r.State != DeclaredIndexPlanned || len(f.execs) != 0 || len(r.Statements) != 1 ||
		!strings.HasPrefix(r.Statements[0], "CREATE INDEX CONCURRENTLY") {
		t.Fatalf("state=%s execs=%v stmts=%v", r.State, f.execs, r.Statements)
	}
	f.idx["public.crm_leads_org_customer_idx"] = declaredIndexInfo{exists: true, kind: "i", table: "crm_leads"}
	r = runFake(t, context.Background(), f, DeclaredIndexOptions{DryRun: true})
	if r.State != DeclaredIndexPlanned || len(r.Statements) != 2 || !strings.HasPrefix(r.Statements[0], "DROP INDEX CONCURRENTLY") || len(f.execs) != 0 {
		t.Fatalf("dry-run of an INVALID index: state=%s stmts=%v execs=%v", r.State, r.Statements, f.execs)
	}
}

func TestEnsureDeclaredIndexSchemaAndTableChecks(t *testing.T) {
	// The table lives in addon_crm too, but the run targets public only; a
	// missing column there fails instead of building on the wrong table.
	f := newFakeDeclaredDB()
	f.cols["public.crm_leads"] = map[string]bool{"organization_id": true}
	if r := runFake(t, context.Background(), f, DeclaredIndexOptions{}); r.State != DeclaredIndexFailed || !strings.Contains(r.Error, `"customer_id"`) || len(f.execs) != 0 {
		t.Fatalf("missing column: %+v execs=%v", r, f.execs)
	}
	f = newFakeDeclaredDB()
	if r := runFake(t, context.Background(), f, DeclaredIndexOptions{Schema: "addon_missing"}); r.State != DeclaredIndexSkipped || r.Reason != DeclaredIndexReasonTableMissing {
		t.Fatalf("missing table: %+v", r)
	}
	f = newFakeDeclaredDB()
	f.idx["public.crm_leads_org_customer_idx"] = declaredIndexInfo{exists: true, kind: "i", valid: true, table: "other"}
	if r := runFake(t, context.Background(), f, DeclaredIndexOptions{}); r.State != DeclaredIndexFailed || len(f.execs) != 0 {
		t.Fatalf("name taken by an index on another table: %+v", r)
	}
	f = newFakeDeclaredDB()
	f.idx["public.crm_leads_org_customer_idx"] = declaredIndexInfo{exists: true, kind: "r"}
	if r := runFake(t, context.Background(), f, DeclaredIndexOptions{}); r.State != DeclaredIndexFailed || len(f.execs) != 0 {
		t.Fatalf("name taken by a table: %+v", r)
	}
}

func TestEnsureDeclaredIndexCanceledStopsClean(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := newFakeDeclaredDB()
	if r := runFake(t, ctx, f, DeclaredIndexOptions{}); r.State != DeclaredIndexCanceled || len(f.execs) != 0 {
		t.Fatalf("state=%s execs=%v", r.State, f.execs)
	}
	// Cancelled during the build: canceled, no cleanup attempted (next run heals).
	ctx, cancel = context.WithCancel(context.Background())
	f = newFakeDeclaredDB()
	f.onExec = func(string) error {
		f.idx["public.crm_leads_org_customer_idx"] = declaredIndexInfo{exists: true, kind: "i", table: "crm_leads"}
		cancel()
		return context.Canceled
	}
	if r := runFake(t, ctx, f, DeclaredIndexOptions{}); r.State != DeclaredIndexCanceled || len(f.execs) != 1 {
		t.Fatalf("state=%s execs=%v", r.State, f.execs)
	}
}

// Off Postgres (and with a bad schema) nothing runs and nothing panics: the
// report says why, the caller is never failed.
func TestEnsureDeclaredIndexesNeverFailsCaller(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	rep := EnsureDeclaredIndexes(context.Background(), db, declaredDefs(), DeclaredIndexOptions{Schema: "public", Logger: quietLog()})
	if rep.Count(DeclaredIndexSkipped) != 2 || rep.Results[0].Reason != DeclaredIndexReasonDialect {
		t.Fatalf("sqlite: %+v", rep)
	}
	rep = EnsureDeclaredIndexes(context.Background(), db, declaredDefs(), DeclaredIndexOptions{Schema: `pub"lic`, Logger: quietLog()})
	if !errors.Is(rep.Err, ErrDeclaredIndexIdentifier) || rep.Count(DeclaredIndexFailed) != 2 {
		t.Fatalf("bad schema: %+v", rep)
	}
	rep = EnsureDeclaredIndexes(context.Background(), nil, declaredDefs(), DeclaredIndexOptions{Schema: "public"})
	if rep.Count(DeclaredIndexSkipped) != 2 || rep.Err == nil {
		t.Fatalf("nil db: %+v", rep)
	}
	if rep := EnsureDeclaredIndexes(context.Background(), db, nil, DeclaredIndexOptions{Schema: "public"}); len(rep.Results) != 0 || rep.Err != nil {
		t.Fatalf("no defs: %+v", rep)
	}
}
