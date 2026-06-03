package dynamic

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
)

// ---------------------------------------------------------------------------
// Pure detection unit tests (no DB)
// ---------------------------------------------------------------------------

func TestIsFileColumn(t *testing.T) {
	cases := []struct {
		key, typ, cellStyle string
		want                bool
	}{
		{"logo", "text", "image", true},        // explicit cellStyle hint
		{"attachment", "text", "file", true},   // explicit file hint
		{"gallery", "text", "media-gallery", true},
		{"photos", "media-gallery", "", true},  // hint in Type
		{"avatar", "text", "", true},           // name heuristic fallback
		{"image", "text", "", true},            // name heuristic fallback
		{"name", "text", "", false},            // plain text column
		{"price", "number", "currency", false}, // unrelated rich renderer
		{"status", "text", "status", false},
	}
	for _, c := range cases {
		if got := isFileColumn(c.key, c.typ, c.cellStyle); got != c.want {
			t.Errorf("isFileColumn(%q,%q,%q)=%v want %v", c.key, c.typ, c.cellStyle, got, c.want)
		}
	}
}

func TestExtractFileValues(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want []string
	}{
		{"string", "/storage/Brand/x.jpeg", []string{"/storage/Brand/x.jpeg"}},
		{"empty string", "  ", nil},
		{"nil", nil, nil},
		{
			"gallery objects",
			[]any{
				map[string]any{"url": "/storage/Brand/a.png"},
				map[string]any{"url": "/storage/Brand/b.png"},
			},
			[]string{"/storage/Brand/a.png", "/storage/Brand/b.png"},
		},
		{"gallery strings", []any{"/storage/x.png", "/storage/y.png"}, []string{"/storage/x.png", "/storage/y.png"}},
		{"[]string", []string{"/storage/z.png"}, []string{"/storage/z.png"}},
		{"non-file value", 42, nil},
	}
	for _, c := range cases {
		got := extractFileValues(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: extractFileValues(%#v)=%v want %v", c.name, c.in, got, c.want)
		}
	}
}

func fileMeta() *modelbase.TableMetadata {
	return &modelbase.TableMetadata{
		Columns: []modelbase.ColumnDef{
			{Key: "name", Type: "text"},
			{Key: "logo", Type: "text", CellStyle: "image"},
			{Key: "gallery", Type: "text", CellStyle: "media-gallery"},
		},
	}
}

func sortedEqual(a, b []string) bool {
	ac := append([]string(nil), a...)
	bc := append([]string(nil), b...)
	sort.Strings(ac)
	sort.Strings(bc)
	return reflect.DeepEqual(ac, bc)
}

func TestCollectOrphanedFromDelete(t *testing.T) {
	snapshot := map[string]any{
		"name": "Acme",
		"logo": "/storage/Brand/logo.jpeg",
		"gallery": []any{
			map[string]any{"url": "/storage/Brand/g1.png"},
			map[string]any{"url": "/storage/Brand/g2.png"},
		},
	}
	got := collectOrphanedFromDelete(snapshot, fileMeta())
	want := []string{"/storage/Brand/logo.jpeg", "/storage/Brand/g1.png", "/storage/Brand/g2.png"}
	if !sortedEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}

	// No file columns → nothing collected.
	plain := &modelbase.TableMetadata{Columns: []modelbase.ColumnDef{{Key: "name", Type: "text"}}}
	if got := collectOrphanedFromDelete(snapshot, plain); got != nil {
		t.Fatalf("plain model: got %v want nil", got)
	}
}

func TestCollectOrphanedFromUpdate(t *testing.T) {
	before := map[string]any{
		"logo": "/storage/Brand/old.jpeg",
		"gallery": []any{
			map[string]any{"url": "/storage/Brand/keep.png"},
			map[string]any{"url": "/storage/Brand/drop.png"},
		},
	}
	after := map[string]any{
		"logo": "/storage/Brand/new.jpeg", // swapped → old orphaned
		"gallery": []any{
			map[string]any{"url": "/storage/Brand/keep.png"}, // drop.png removed → orphaned
		},
	}
	got := collectOrphanedFromUpdate(before, after, fileMeta())
	want := []string{"/storage/Brand/old.jpeg", "/storage/Brand/drop.png"}
	if !sortedEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestCollectOrphanedFromUpdate_UnchangedKept(t *testing.T) {
	before := map[string]any{"logo": "/storage/Brand/same.jpeg"}
	after := map[string]any{"logo": "/storage/Brand/same.jpeg"}
	if got := collectOrphanedFromUpdate(before, after, fileMeta()); got != nil {
		t.Fatalf("unchanged logo must not orphan, got %v", got)
	}
}

func TestDedupeNonEmpty(t *testing.T) {
	got := dedupeNonEmpty([]string{"a", "", "a", "b", "", "b", "c"})
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if dedupeNonEmpty(nil) != nil {
		t.Fatal("nil input must return nil")
	}
	if dedupeNonEmpty([]string{""}) != nil {
		t.Fatal("all-empty input must return nil")
	}
}

// ---------------------------------------------------------------------------
// Integration: Service.Delete / Service.Update route to the FileDeleter
// ---------------------------------------------------------------------------

// TestBrand is a model with a file/image column (logo, via name heuristic) and
// an explicit media-gallery column (photos, via CellStyle).
type TestBrand struct {
	modelbase.BaseUUIDModel
	Name   string `json:"name" gorm:"size:255"`
	Logo   string `json:"logo" gorm:"size:512"`
	Photos string `json:"photos" gorm:"type:text"` // stored as JSON string; served as gallery
}

func (TestBrand) TableName() string { return "test_brands" }
func (TestBrand) DefineTable() modelbase.TableMetadata {
	return modelbase.TableMetadata{
		Title: "Test Brands",
		Columns: []modelbase.ColumnDef{
			{Key: "name", Label: "Name", Type: "text"},
			{Key: "logo", Label: "Logo", Type: "text", CellStyle: "image"},
			{Key: "photos", Label: "Photos", Type: "text", CellStyle: "media-gallery"},
		},
	}
}
func (TestBrand) DefineModal() modelbase.ModalMetadata {
	return modelbase.ModalMetadata{Title: "Test Brand"}
}

func setupBrandDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := setupTestDB(t) // reuse the sqlite harness from service_test.go
	db.Exec(`CREATE TABLE IF NOT EXISTS test_brands (
		id TEXT PRIMARY KEY,
		organization_id TEXT,
		created_by_id TEXT,
		created_at DATETIME,
		updated_at DATETIME,
		deleted_at DATETIME,
		name TEXT,
		logo TEXT,
		photos TEXT
	)`)
	return db
}

// recordingDeleter captures every FileDeleter invocation.
type recordingDeleter struct {
	calls [][]string
	model string
}

func (r *recordingDeleter) deleter(_ context.Context, model string, paths []string) {
	r.model = model
	r.calls = append(r.calls, paths)
}

func setupBrandService(t *testing.T, db *gorm.DB, fd FileDeleter) *Service {
	t.Helper()
	modelbase.Register("test_brands", func() modelbase.ModelDefiner { return &TestBrand{} })
	meta := metadata.New(metadata.Config{CacheTTL: -1})
	return New(Config{DB: db, Metadata: meta, FileDeleter: fd})
}

func TestServiceDelete_CallsFileDeleter(t *testing.T) {
	db := setupBrandDB(t)
	rec := &recordingDeleter{}
	svc := setupBrandService(t, db, rec.deleter)
	user := newUser(uuid.New())

	created, err := svc.Create(context.Background(), "test_brands", user, map[string]any{
		"name": "Acme",
		"logo": "/storage/Brand/logo.jpeg",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id, _ := uuid.Parse(created["id"].(string))

	if err := svc.Delete(context.Background(), "test_brands", user, id); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if len(rec.calls) != 1 {
		t.Fatalf("expected 1 FileDeleter call, got %d (%v)", len(rec.calls), rec.calls)
	}
	if !sortedEqual(rec.calls[0], []string{"/storage/Brand/logo.jpeg"}) {
		t.Fatalf("FileDeleter got %v want [/storage/Brand/logo.jpeg]", rec.calls[0])
	}
	if rec.model != "test_brands" {
		t.Fatalf("model = %q want test_brands", rec.model)
	}
}

func TestServiceDelete_NoFileColumnsNoCall(t *testing.T) {
	db := setupTestDB(t)
	rec := &recordingDeleter{}
	// test_products has no file columns.
	modelbase.Register("test_products", func() modelbase.ModelDefiner { return &TestProduct{} })
	meta := metadata.New(metadata.Config{CacheTTL: -1})
	svc := New(Config{DB: db, Metadata: meta, FileDeleter: rec.deleter})
	user := newUser(uuid.New())

	created := createProduct(t, svc, user, "Widget", 1)
	id, _ := uuid.Parse(created["id"].(string))
	if err := svc.Delete(context.Background(), "test_products", user, id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(rec.calls) != 0 {
		t.Fatalf("expected no FileDeleter call for fileless model, got %v", rec.calls)
	}
}

func TestServiceUpdate_CallsFileDeleterOnSwap(t *testing.T) {
	db := setupBrandDB(t)
	rec := &recordingDeleter{}
	svc := setupBrandService(t, db, rec.deleter)
	user := newUser(uuid.New())

	created, err := svc.Create(context.Background(), "test_brands", user, map[string]any{
		"name": "Acme",
		"logo": "/storage/Brand/old.jpeg",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id, _ := uuid.Parse(created["id"].(string))

	if _, err := svc.Update(context.Background(), "test_brands", user, id, map[string]any{
		"logo": "/storage/Brand/new.jpeg",
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	if len(rec.calls) != 1 {
		t.Fatalf("expected 1 FileDeleter call on swap, got %d (%v)", len(rec.calls), rec.calls)
	}
	if !sortedEqual(rec.calls[0], []string{"/storage/Brand/old.jpeg"}) {
		t.Fatalf("FileDeleter got %v want [/storage/Brand/old.jpeg]", rec.calls[0])
	}
}

func TestServiceUpdate_NoCallWhenFileUnchanged(t *testing.T) {
	db := setupBrandDB(t)
	rec := &recordingDeleter{}
	svc := setupBrandService(t, db, rec.deleter)
	user := newUser(uuid.New())

	created, err := svc.Create(context.Background(), "test_brands", user, map[string]any{
		"name": "Acme",
		"logo": "/storage/Brand/keep.jpeg",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id, _ := uuid.Parse(created["id"].(string))

	// Update only the name; logo unchanged → no orphan.
	if _, err := svc.Update(context.Background(), "test_brands", user, id, map[string]any{
		"name": "Renamed",
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(rec.calls) != 0 {
		t.Fatalf("expected no FileDeleter call when file unchanged, got %v", rec.calls)
	}
}

// nilDeleterService confirms a nil FileDeleter is a safe no-op (no panic, no
// extra pre-delete load failures surfaced).
func TestServiceDelete_NilFileDeleterSafe(t *testing.T) {
	db := setupBrandDB(t)
	svc := setupBrandService(t, db, nil)
	user := newUser(uuid.New())

	created, err := svc.Create(context.Background(), "test_brands", user, map[string]any{
		"name": "Acme",
		"logo": "/storage/Brand/logo.jpeg",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id, _ := uuid.Parse(created["id"].(string))
	if err := svc.Delete(context.Background(), "test_brands", user, id); err != nil {
		t.Fatalf("delete with nil deleter: %v", err)
	}
}
