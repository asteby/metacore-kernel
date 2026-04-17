package metadata

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/modelbase"
)

// fakeModel is a minimal ModelDefiner used by tests. Each test uses a unique
// key so the global modelbase registry stays reproducible across runs.
type fakeModel struct {
	key   string
	title string
}

func (f *fakeModel) TableName() string { return f.key }

func (f *fakeModel) DefineTable() modelbase.TableMetadata {
	return modelbase.TableMetadata{
		Title: f.title,
		Columns: []modelbase.ColumnDef{
			{Key: "id", Label: "ID", Type: "text"},
			{Key: "name", Label: "Name", Type: "text"},
		},
	}
}

func (f *fakeModel) DefineModal() modelbase.ModalMetadata {
	return modelbase.ModalMetadata{
		Title: f.title,
		Fields: []modelbase.FieldDef{
			{Key: "name", Label: "Name", Type: "text", Required: true},
		},
	}
}

// registerFakeModel registers a fresh fakeModel under a test-local key and
// returns that key. Each call uses a unique suffix so parallel-running tests
// do not collide on the global registry.
func registerFakeModel(t *testing.T, title string) string {
	t.Helper()
	key := fmt.Sprintf("metadata_test_%s_%d", t.Name(), time.Now().UnixNano())
	// Capture by value so each factory call returns an independent instance.
	titleCopy := title
	modelbase.Register(key, func() modelbase.ModelDefiner {
		return &fakeModel{key: key, title: titleCopy}
	})
	return key
}

func TestService_GetTable_ReturnsRegisteredMetadata(t *testing.T) {
	svc := New(Config{CacheTTL: time.Minute})
	key := registerFakeModel(t, "Users")

	meta, err := svc.GetTable(context.Background(), key)
	if err != nil {
		t.Fatalf("GetTable: unexpected error: %v", err)
	}
	if meta.Title != "Users" {
		t.Fatalf("expected title Users, got %q", meta.Title)
	}
	if len(meta.Columns) != 2 {
		t.Fatalf("expected 2 columns, got %d", len(meta.Columns))
	}
}

func TestService_GetModal_ReturnsRegisteredMetadata(t *testing.T) {
	svc := New(Config{CacheTTL: time.Minute})
	key := registerFakeModel(t, "Products")

	meta, err := svc.GetModal(context.Background(), key)
	if err != nil {
		t.Fatalf("GetModal: unexpected error: %v", err)
	}
	if meta.Title != "Products" {
		t.Fatalf("expected title Products, got %q", meta.Title)
	}
	if len(meta.Fields) != 1 {
		t.Fatalf("expected 1 field, got %d", len(meta.Fields))
	}
}

func TestService_UnknownModel_ReturnsErrModelNotFound(t *testing.T) {
	svc := New(Config{CacheTTL: time.Minute})

	_, err := svc.GetTable(context.Background(), "nope_does_not_exist")
	if !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("expected ErrModelNotFound, got %v", err)
	}
	_, err = svc.GetModal(context.Background(), "nope_does_not_exist")
	if !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("expected ErrModelNotFound, got %v", err)
	}
}

func TestService_EmptyKey_ReturnsErrModelNotFound(t *testing.T) {
	svc := New(Config{CacheTTL: time.Minute})

	if _, err := svc.GetTable(context.Background(), ""); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("expected ErrModelNotFound on empty key, got %v", err)
	}
}

func TestService_CacheHitsSecondCall(t *testing.T) {
	key := registerFakeModel(t, "Cached")

	// Register a counting wrapper to detect how many times the factory runs.
	// We replace the previous registration with one that increments a counter.
	var calls int64
	modelbase.Register(key, func() modelbase.ModelDefiner {
		atomic.AddInt64(&calls, 1)
		return &fakeModel{key: key, title: "Cached"}
	})

	svc := New(Config{CacheTTL: time.Minute})

	if _, err := svc.GetTable(context.Background(), key); err != nil {
		t.Fatalf("first GetTable: %v", err)
	}
	if _, err := svc.GetTable(context.Background(), key); err != nil {
		t.Fatalf("second GetTable: %v", err)
	}

	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Fatalf("expected factory to be called once (cache hit on 2nd), got %d", got)
	}
}

func TestService_InvalidateModel_EvictsEntry(t *testing.T) {
	key := registerFakeModel(t, "EvictMe")

	var calls int64
	modelbase.Register(key, func() modelbase.ModelDefiner {
		atomic.AddInt64(&calls, 1)
		return &fakeModel{key: key, title: "EvictMe"}
	})

	svc := New(Config{CacheTTL: time.Minute})

	if _, err := svc.GetTable(context.Background(), key); err != nil {
		t.Fatalf("first: %v", err)
	}
	svc.InvalidateModel(key)
	if _, err := svc.GetTable(context.Background(), key); err != nil {
		t.Fatalf("second: %v", err)
	}

	if got := atomic.LoadInt64(&calls); got != 2 {
		t.Fatalf("expected factory to be called twice (invalidated), got %d", got)
	}
}

func TestService_InvalidateCache_EvictsEverything(t *testing.T) {
	k1 := registerFakeModel(t, "A")
	k2 := registerFakeModel(t, "B")

	svc := New(Config{CacheTTL: time.Minute})

	_, _ = svc.GetTable(context.Background(), k1)
	_, _ = svc.GetTable(context.Background(), k2)

	if n := svc.cache.len(); n < 2 {
		t.Fatalf("expected >=2 cached entries, got %d", n)
	}

	svc.InvalidateCache()

	if n := svc.cache.len(); n != 0 {
		t.Fatalf("InvalidateCache must drop everything, got %d", n)
	}
}

func TestService_TableTransformer_AppliesInOrder(t *testing.T) {
	key := registerFakeModel(t, "Base")
	svc := New(Config{CacheTTL: 0}) // disable cache to re-run transformers each call

	svc.WithTableTransformer(func(_ context.Context, _ string, m *modelbase.TableMetadata) error {
		m.Title = m.Title + "+first"
		return nil
	})
	svc.WithTableTransformer(func(_ context.Context, _ string, m *modelbase.TableMetadata) error {
		m.Title = m.Title + "+second"
		return nil
	})

	meta, err := svc.GetTable(context.Background(), key)
	if err != nil {
		t.Fatalf("GetTable: %v", err)
	}
	want := "Base+first+second"
	if meta.Title != want {
		t.Fatalf("expected %q, got %q", want, meta.Title)
	}
}

func TestService_ModalTransformer_AppliesInOrder(t *testing.T) {
	key := registerFakeModel(t, "M")
	svc := New(Config{CacheTTL: 0})

	svc.WithModalTransformer(func(_ context.Context, _ string, m *modelbase.ModalMetadata) error {
		m.Title = m.Title + "!"
		return nil
	})

	meta, err := svc.GetModal(context.Background(), key)
	if err != nil {
		t.Fatalf("GetModal: %v", err)
	}
	if meta.Title != "M!" {
		t.Fatalf("expected M!, got %q", meta.Title)
	}
}

func TestService_TableTransformer_ErrorPropagates(t *testing.T) {
	key := registerFakeModel(t, "X")
	svc := New(Config{CacheTTL: time.Minute})

	boom := errors.New("boom")
	svc.WithTableTransformer(func(_ context.Context, _ string, _ *modelbase.TableMetadata) error {
		return boom
	})

	_, err := svc.GetTable(context.Background(), key)
	if !errors.Is(err, boom) {
		t.Fatalf("expected wrapped boom error, got %v", err)
	}
}

func TestService_GetAll_ReturnsRegisteredModels(t *testing.T) {
	k := registerFakeModel(t, "InAll")
	svc := New(Config{CacheTTL: time.Minute})

	all, err := svc.GetAll(context.Background())
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if _, ok := all.Tables[k]; !ok {
		t.Fatalf("GetAll.Tables missing key %q", k)
	}
	if _, ok := all.Modals[k]; !ok {
		t.Fatalf("GetAll.Modals missing key %q", k)
	}
	if all.Version == "" {
		t.Fatalf("GetAll.Version must be non-empty")
	}
}

func TestService_GetAll_VersionBumpsOnInvalidate(t *testing.T) {
	registerFakeModel(t, "V")
	svc := New(Config{CacheTTL: time.Minute})

	all1, err := svc.GetAll(context.Background())
	if err != nil {
		t.Fatalf("first GetAll: %v", err)
	}
	svc.InvalidateCache()
	all2, err := svc.GetAll(context.Background())
	if err != nil {
		t.Fatalf("second GetAll: %v", err)
	}

	if all1.Version == all2.Version {
		t.Fatalf("expected version token to change after InvalidateCache")
	}
}

func TestService_DefaultCacheTTL(t *testing.T) {
	svc := New(Config{})
	if svc.Config().CacheTTL != DefaultCacheTTL {
		t.Fatalf("expected DefaultCacheTTL, got %v", svc.Config().CacheTTL)
	}
}

func TestService_NegativeTTLDisablesCache(t *testing.T) {
	svc := New(Config{CacheTTL: -1})
	if svc.Config().CacheTTL != 0 {
		t.Fatalf("expected negative TTL to clamp to 0, got %v", svc.Config().CacheTTL)
	}

	key := registerFakeModel(t, "NoCache")
	var calls int64
	modelbase.Register(key, func() modelbase.ModelDefiner {
		atomic.AddInt64(&calls, 1)
		return &fakeModel{key: key, title: "NoCache"}
	})

	_, _ = svc.GetTable(context.Background(), key)
	_, _ = svc.GetTable(context.Background(), key)

	if got := atomic.LoadInt64(&calls); got != 2 {
		t.Fatalf("expected factory called twice without cache, got %d", got)
	}
}
