package query

import (
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/modelbase"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// testRow is a minimal GORM model used by the execution tests below.
// Column names line up 1:1 with testMeta() so the builder's whitelisting
// matches the real schema.
type testRow struct {
	ID        uint   `gorm:"primaryKey"`
	Name      string `gorm:"column:name"`
	Status    string `gorm:"column:status"`
	Amount    int    `gorm:"column:amount"`
	CreatedAt string `gorm:"column:created_at"`
}

func (testRow) TableName() string { return "test_rows" }

// testMeta returns a TableMetadata matching testRow. Declared here (not
// as a package-level var) so each test gets a pristine copy — tests
// occasionally mutate meta to verify edge cases.
func testMeta() *modelbase.TableMetadata {
	return &modelbase.TableMetadata{
		Title: "Test",
		Columns: []modelbase.ColumnDef{
			{Key: "id", Label: "ID", Type: "number", Sortable: true},
			{Key: "name", Label: "Name", Type: "text", Sortable: true, Filterable: true},
			{Key: "status", Label: "Status", Type: "text", Filterable: true},
			{Key: "amount", Label: "Amount", Type: "number", Sortable: true, Filterable: true},
			{Key: "created_at", Label: "Created", Type: "date", Sortable: true, Filterable: true},
		},
		SearchColumns: []string{"name", "status"},
	}
}

// openDryDB returns a GORM handle in DryRun mode. DryRun builds the SQL
// but never sends it to the driver, so tests can assert on the rendered
// statement without needing a dialect that understands every operator
// (SQLite lacks native ILIKE, which the real code emits).
func openDryDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		DryRun: true,
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open dry db: %v", err)
	}
	return db
}

// openLiveDB returns an executable SQLite DB with test_rows migrated
// and seeded. Only used for tests that exercise a live Count/Find path.
func openLiveDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open live db: %v", err)
	}
	if err := db.AutoMigrate(&testRow{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	seed := []testRow{
		{Name: "alpha", Status: "active", Amount: 10, CreatedAt: "2024-01-01"},
		{Name: "beta", Status: "active", Amount: 50, CreatedAt: "2024-02-01"},
		{Name: "gamma", Status: "archived", Amount: 100, CreatedAt: "2024-03-01"},
		{Name: "delta", Status: "active", Amount: 200, CreatedAt: "2024-04-01"},
		{Name: "epsilon", Status: "archived", Amount: 500, CreatedAt: "2024-05-01"},
	}
	if err := db.Create(&seed).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	return db
}

// renderSQL executes the GORM statement builder in DryRun and returns the
// fully bound SQL string. Used by TestApply_* to assert on the emitted
// clauses without running them.
func renderSQL(t *testing.T, db *gorm.DB) string {
	t.Helper()
	stmt := db.Find(&[]testRow{}).Statement
	return db.Dialector.Explain(stmt.SQL.String(), stmt.Vars...)
}

// -----------------------------------------------------------------
// New / whitelisting
// -----------------------------------------------------------------

func TestNew_NilMeta(t *testing.T) {
	b := New(nil)
	if b == nil {
		t.Fatal("want non-nil Builder for nil meta")
	}
	if len(b.allowed) != 0 {
		t.Errorf("allowed = %v, want empty", b.allowed)
	}
}

func TestNew_BuildsAllowedSet(t *testing.T) {
	b := New(testMeta())
	for _, want := range []string{"id", "name", "status", "amount", "created_at"} {
		if _, ok := b.allowed[want]; !ok {
			t.Errorf("allowed missing %q", want)
		}
	}
}

func TestNew_DropsUnsafeColumnNames(t *testing.T) {
	meta := &modelbase.TableMetadata{
		Columns: []modelbase.ColumnDef{
			{Key: "ok_col"},
			{Key: "bad col"},   // space
			{Key: "1bad"},      // starts with digit
			{Key: "drop--"},    // SQL comment
			{Key: ""},          // empty
		},
	}
	b := New(meta)
	if _, ok := b.allowed["ok_col"]; !ok {
		t.Errorf("ok_col should be allowed")
	}
	if len(b.allowed) != 1 {
		t.Errorf("allowed = %v, want only ok_col", b.allowed)
	}
}

func TestNew_SearchableIntersectsAllowed(t *testing.T) {
	meta := &modelbase.TableMetadata{
		Columns: []modelbase.ColumnDef{
			{Key: "name"},
			{Key: "status"},
		},
		SearchColumns: []string{"name", "ghost_col", "status"}, // ghost_col not in columns
	}
	b := New(meta)
	if len(b.searchable) != 2 {
		t.Fatalf("searchable = %v, want 2", b.searchable)
	}
	if b.searchable[0] != "name" || b.searchable[1] != "status" {
		t.Errorf("searchable = %v", b.searchable)
	}
}

// -----------------------------------------------------------------
// Apply — sort
// -----------------------------------------------------------------

func TestApply_SortWhitelisted(t *testing.T) {
	b := New(testMeta())
	db := openDryDB(t)
	q := b.Apply(db.Model(&testRow{}), Params{SortBy: "name", Order: "asc"})
	sql := renderSQL(t, q)
	if !strings.Contains(sql, "ORDER BY name asc") {
		t.Errorf("want ORDER BY name asc, got %q", sql)
	}
}

func TestApply_SortDefaultsToDesc(t *testing.T) {
	b := New(testMeta())
	db := openDryDB(t)
	q := b.Apply(db.Model(&testRow{}), Params{SortBy: "name", Order: "invalid"})
	sql := renderSQL(t, q)
	if !strings.Contains(sql, "ORDER BY name desc") {
		t.Errorf("want desc fallback, got %q", sql)
	}
}

func TestApply_SortUnknownColumnDropped(t *testing.T) {
	b := New(testMeta())
	db := openDryDB(t)
	q := b.Apply(db.Model(&testRow{}), Params{SortBy: "evil_col", Order: "asc"})
	sql := renderSQL(t, q)
	if strings.Contains(strings.ToUpper(sql), "ORDER BY") {
		t.Errorf("unknown column should be dropped, got %q", sql)
	}
}

func TestApply_SortRejectsSQLInjection(t *testing.T) {
	b := New(testMeta())
	// SortBy contains a semicolon / SQL fragment. Even though it's not in
	// the whitelist, this double-guards that isSafeIdent catches it.
	db := openDryDB(t)
	q := b.Apply(db.Model(&testRow{}), Params{SortBy: "name; DROP TABLE users", Order: "asc"})
	sql := renderSQL(t, q)
	if strings.Contains(strings.ToLower(sql), "drop table") {
		t.Fatalf("SQLi leaked into emitted SQL: %q", sql)
	}
}

// -----------------------------------------------------------------
// Apply — filters
// -----------------------------------------------------------------

func TestApply_FilterEq(t *testing.T) {
	b := New(testMeta())
	db := openDryDB(t)
	q := b.Apply(db.Model(&testRow{}), Params{
		Filters: map[string]Filter{"status": {Op: OpEq, Value: "active"}},
	})
	sql := renderSQL(t, q)
	if !strings.Contains(sql, "status = ") {
		t.Errorf("want status =, got %q", sql)
	}
	if !strings.Contains(sql, "active") {
		t.Errorf("want active in SQL, got %q", sql)
	}
}

func TestApply_FilterIn(t *testing.T) {
	b := New(testMeta())
	db := openDryDB(t)
	q := b.Apply(db.Model(&testRow{}), Params{
		Filters: map[string]Filter{"status": {Op: OpIn, Value: []string{"active", "archived"}}},
	})
	sql := renderSQL(t, q)
	if !strings.Contains(strings.ToUpper(sql), "IN (") {
		t.Errorf("want IN clause, got %q", sql)
	}
}

func TestApply_FilterIlike(t *testing.T) {
	b := New(testMeta())
	db := openDryDB(t)
	q := b.Apply(db.Model(&testRow{}), Params{
		Filters: map[string]Filter{"name": {Op: OpIlike, Value: "alpha"}},
	})
	sql := renderSQL(t, q)
	if !strings.Contains(strings.ToUpper(sql), "ILIKE") {
		t.Errorf("want ILIKE, got %q", sql)
	}
	if !strings.Contains(sql, "%alpha%") {
		t.Errorf("want wildcard-wrapped pattern, got %q", sql)
	}
}

func TestApply_FilterGteLte(t *testing.T) {
	b := New(testMeta())
	db := openDryDB(t)
	q := b.Apply(db.Model(&testRow{}), Params{
		Filters: map[string]Filter{
			"amount": {Op: OpGte, Value: "10"},
		},
	})
	if !strings.Contains(renderSQL(t, q), "amount >= ") {
		t.Errorf("want amount >=")
	}

	q2 := b.Apply(db.Model(&testRow{}), Params{
		Filters: map[string]Filter{
			"amount": {Op: OpLte, Value: "100"},
		},
	})
	if !strings.Contains(renderSQL(t, q2), "amount <= ") {
		t.Errorf("want amount <=")
	}
}

func TestApply_FilterRangeBothSides(t *testing.T) {
	b := New(testMeta())
	db := openDryDB(t)
	q := b.Apply(db.Model(&testRow{}), Params{
		Filters: map[string]Filter{
			"amount": {Op: OpRange, Value: [2]string{"10", "100"}},
		},
	})
	sql := renderSQL(t, q)
	if !strings.Contains(strings.ToUpper(sql), "BETWEEN") {
		t.Errorf("want BETWEEN, got %q", sql)
	}
}

func TestApply_FilterRangeOneSide(t *testing.T) {
	b := New(testMeta())
	db := openDryDB(t)
	q := b.Apply(db.Model(&testRow{}), Params{
		Filters: map[string]Filter{
			"amount": {Op: OpRange, Value: [2]string{"", "100"}},
		},
	})
	sql := renderSQL(t, q)
	if strings.Contains(strings.ToUpper(sql), "BETWEEN") {
		t.Errorf("one-sided range should not emit BETWEEN, got %q", sql)
	}
	if !strings.Contains(sql, "amount <= ") {
		t.Errorf("want amount <=, got %q", sql)
	}
}

func TestApply_FilterUnknownColumnDropped(t *testing.T) {
	b := New(testMeta())
	db := openDryDB(t)
	q := b.Apply(db.Model(&testRow{}), Params{
		Filters: map[string]Filter{"ghost": {Op: OpEq, Value: "boo"}},
	})
	sql := renderSQL(t, q)
	if strings.Contains(sql, "ghost") {
		t.Errorf("unknown filter should be dropped, got %q", sql)
	}
}

func TestApply_FilterUnsafeIdentDropped(t *testing.T) {
	// Build a Builder that has the unsafe key in its allowed set (simulate
	// a meta leak) — applyFilters' isSafeIdent check must still catch it.
	b := &Builder{
		meta:    testMeta(),
		allowed: map[string]struct{}{"evil; drop": {}},
	}
	db := openDryDB(t)
	q := b.Apply(db.Model(&testRow{}), Params{
		Filters: map[string]Filter{"evil; drop": {Op: OpEq, Value: "x"}},
	})
	sql := renderSQL(t, q)
	if strings.Contains(strings.ToLower(sql), "drop") {
		t.Errorf("unsafe column leaked: %q", sql)
	}
}

func TestApply_EmptyFilterValueSkipped(t *testing.T) {
	b := New(testMeta())
	db := openDryDB(t)
	q := b.Apply(db.Model(&testRow{}), Params{
		Filters: map[string]Filter{"name": {Op: OpEq, Value: ""}},
	})
	sql := renderSQL(t, q)
	if strings.Contains(sql, "name = ") {
		t.Errorf("empty filter should be skipped, got %q", sql)
	}
}

// -----------------------------------------------------------------
// Apply — search
// -----------------------------------------------------------------

func TestApply_SearchFansOutOverSearchColumns(t *testing.T) {
	b := New(testMeta())
	db := openDryDB(t)
	q := b.Apply(db.Model(&testRow{}), Params{Search: "widget"})
	sql := renderSQL(t, q)
	if !strings.Contains(strings.ToUpper(sql), "ILIKE") {
		t.Errorf("want ILIKE, got %q", sql)
	}
	if !strings.Contains(sql, "name ILIKE") || !strings.Contains(sql, "status ILIKE") {
		t.Errorf("want both search columns, got %q", sql)
	}
	if !strings.Contains(strings.ToUpper(sql), " OR ") {
		t.Errorf("want OR between search columns, got %q", sql)
	}
}

func TestApply_SearchEscapesWildcards(t *testing.T) {
	b := New(testMeta())
	db := openDryDB(t)
	q := b.Apply(db.Model(&testRow{}), Params{Search: "50%_off"})
	sql := renderSQL(t, q)
	// Literal %, _ should appear escaped in the bound pattern.
	if !strings.Contains(sql, `\%`) || !strings.Contains(sql, `\_`) {
		t.Errorf("wildcards not escaped: %q", sql)
	}
}

func TestApply_SearchNoSearchColumns_NoOp(t *testing.T) {
	meta := testMeta()
	meta.SearchColumns = nil
	b := New(meta)
	db := openDryDB(t)
	q := b.Apply(db.Model(&testRow{}), Params{Search: "widget"})
	sql := renderSQL(t, q)
	if strings.Contains(strings.ToUpper(sql), "ILIKE") {
		t.Errorf("no searchable → no ILIKE, got %q", sql)
	}
}

// -----------------------------------------------------------------
// Pagination / PageMeta
// -----------------------------------------------------------------

func TestPaginate_DefaultPage(t *testing.T) {
	b := New(testMeta())
	db := openDryDB(t)
	q := b.Paginate(db.Model(&testRow{}), Params{})
	sql := renderSQL(t, q)
	if !strings.Contains(sql, "LIMIT 15") {
		t.Errorf("want LIMIT 15, got %q", sql)
	}
}

func TestPaginate_CustomPage(t *testing.T) {
	b := New(testMeta())
	db := openDryDB(t)
	q := b.Paginate(db.Model(&testRow{}), Params{Page: 3, PerPage: 25})
	sql := renderSQL(t, q)
	if !strings.Contains(sql, "LIMIT 25") {
		t.Errorf("want LIMIT 25, got %q", sql)
	}
	if !strings.Contains(sql, "OFFSET 50") {
		t.Errorf("want OFFSET 50, got %q", sql)
	}
}

func TestPaginate_ClampsPerPage(t *testing.T) {
	b := New(testMeta())
	db := openDryDB(t)
	q := b.Paginate(db.Model(&testRow{}), Params{PerPage: 99999})
	sql := renderSQL(t, q)
	if !strings.Contains(sql, "LIMIT 200") {
		t.Errorf("want LIMIT 200 (clamped), got %q", sql)
	}
}

func TestPageMeta_LastPage(t *testing.T) {
	b := New(testMeta())
	cases := []struct {
		total    int64
		perPage  int
		wantLast int
	}{
		{0, 15, 1},
		{10, 15, 1},
		{15, 15, 1},
		{16, 15, 2},
		{150, 15, 10},
		{151, 15, 11},
	}
	for _, tc := range cases {
		m := b.PageMeta(tc.total, Params{PerPage: tc.perPage})
		if m.LastPage != tc.wantLast {
			t.Errorf("total=%d perPage=%d → LastPage=%d, want %d", tc.total, tc.perPage, m.LastPage, tc.wantLast)
		}
	}
}

func TestPageMeta_UsesDefaults(t *testing.T) {
	b := New(testMeta())
	m := b.PageMeta(100, Params{})
	if m.Page != DefaultPage || m.PerPage != DefaultPerPage {
		t.Errorf("defaults not applied: %+v", m)
	}
}

// -----------------------------------------------------------------
// Live execution against SQLite — only operators SQLite supports.
// -----------------------------------------------------------------

func TestBuilder_LiveCountAndPaginate(t *testing.T) {
	db := openLiveDB(t)
	b := New(testMeta())

	params := Params{
		Page:    1,
		PerPage: 2,
		Filters: map[string]Filter{
			"status": {Op: OpEq, Value: "active"},
		},
	}

	base := b.Apply(db.Model(&testRow{}), params)
	total, err := b.Count(base, params)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if total != 3 {
		t.Errorf("total = %d, want 3 (three active rows)", total)
	}

	var rows []testRow
	if err := b.Paginate(base, params).Find(&rows).Error; err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("len(rows) = %d, want 2 (per_page)", len(rows))
	}

	// Page 2 should yield the third active row.
	params.Page = 2
	var page2 []testRow
	if err := b.Paginate(base, params).Find(&page2).Error; err != nil {
		t.Fatalf("Find page2: %v", err)
	}
	if len(page2) != 1 {
		t.Errorf("len(page2) = %d, want 1", len(page2))
	}
}

func TestBuilder_LiveInFilter(t *testing.T) {
	db := openLiveDB(t)
	b := New(testMeta())
	params := Params{
		Filters: map[string]Filter{
			"status": {Op: OpIn, Value: []string{"archived"}},
		},
	}
	q := b.Apply(db.Model(&testRow{}), params)
	total, err := b.Count(q, params)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if total != 2 {
		t.Errorf("total = %d, want 2 archived rows", total)
	}
}

func TestBuilder_LiveRangeFilter(t *testing.T) {
	db := openLiveDB(t)
	b := New(testMeta())
	params := Params{
		Filters: map[string]Filter{
			"amount": {Op: OpRange, Value: [2]string{"50", "200"}},
		},
	}
	q := b.Apply(db.Model(&testRow{}), params)
	total, err := b.Count(q, params)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if total != 3 {
		t.Errorf("total = %d, want 3 (amount between 50 and 200)", total)
	}
}

func TestBuilder_LiveSortAsc(t *testing.T) {
	db := openLiveDB(t)
	b := New(testMeta())
	params := Params{SortBy: "amount", Order: "asc", PerPage: 100}
	q := b.Paginate(b.Apply(db.Model(&testRow{}), params), params)
	var rows []testRow
	if err := q.Find(&rows).Error; err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(rows) != 5 {
		t.Fatalf("len(rows) = %d, want 5", len(rows))
	}
	if rows[0].Amount != 10 || rows[4].Amount != 500 {
		t.Errorf("asc order violated: %+v", rows)
	}
}

func TestBuilder_CountDoesNotMutateBase(t *testing.T) {
	db := openLiveDB(t)
	b := New(testMeta())
	params := Params{
		Filters: map[string]Filter{"status": {Op: OpEq, Value: "active"}},
		PerPage: 10,
	}
	base := b.Apply(db.Model(&testRow{}), params)

	total, err := b.Count(base, params)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if total != 3 {
		t.Fatalf("total = %d, want 3", total)
	}

	// Now use the same base for a Find — if Count mutated it via
	// Limit(-1)/Offset(-1), Find would return all rows. We expect the
	// base to still be filtered by status=active.
	var rows []testRow
	if err := b.Paginate(base, params).Find(&rows).Error; err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(rows) != 3 {
		t.Errorf("after Count, Find got %d rows, want 3 (base mutated)", len(rows))
	}
	for _, r := range rows {
		if r.Status != "active" {
			t.Errorf("base filter lost: %+v", r)
		}
	}
}
