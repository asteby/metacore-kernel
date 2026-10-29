package settings_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/settings"
)

// Needs a real Postgres: TEST_POSTGRES_DSN=postgres://... go test ./settings
func pgStore(t *testing.T) (*gorm.DB, *settings.Store) {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(60)
	if err := db.Exec(`CREATE TABLE IF NOT EXISTS metacore_addon_settings (
		org_id UUID NOT NULL, addon_key TEXT NOT NULL,
		scope_kind TEXT NOT NULL CHECK (scope_kind IN ('org','branch','market')),
		scope_id TEXT NOT NULL DEFAULT '', "values" JSONB NOT NULL DEFAULT '{}',
		rev BIGINT NOT NULL DEFAULT 1, updated_by UUID,
		updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (org_id, addon_key, scope_kind, scope_id))`).Error; err != nil {
		t.Fatal(err)
	}
	src := settings.DefsFunc(func(context.Context, uuid.UUID, string) ([]manifest.SettingDef, error) { return defs, nil })
	return db, settings.NewStore(db, src, nil)
}

func TestPG_LastWriterWinsNeverConflicts(t *testing.T) {
	db, s := pgStore(t)
	ctx := context.Background()
	for _, n := range []int{10, 40, 60} {
		org := uuid.New()
		var wg sync.WaitGroup
		var conflicts, others int64
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, err := s.Write(ctx, org, "acme", settings.ScopeOrg, "", settings.Values{"mode": "pro", "enabled": i%2 == 0}, nil, uuid.Nil)
				switch {
				case err == nil:
				case errors.Is(err, settings.ErrConflict):
					atomic.AddInt64(&conflicts, 1)
				default:
					atomic.AddInt64(&others, 1)
					t.Errorf("unexpected: %v", err)
				}
			}(i)
		}
		wg.Wait()
		_, rev, err := s.Read(ctx, org, "acme", settings.ScopeOrg, "")
		if err != nil {
			t.Fatal(err)
		}
		// Writes alternate enabled true/false, so only the first and the first
		// flip bump the revision when nothing changes; the invariant is: no
		// conflicts and rev >= 1.
		if conflicts != 0 || others != 0 || rev < 1 {
			t.Fatalf("n=%d conflicts=%d others=%d rev=%d", n, conflicts, others, rev)
		}
	}
	_ = db
}

func TestPG_LastWriterWinsRevEqualsN(t *testing.T) {
	_, s := pgStore(t)
	ctx := context.Background()
	const n = 50
	org := uuid.New()
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Distinct values each time: every write is a real change.
			if _, err := s.Write(ctx, org, "acme", settings.ScopeBranch, "b1", settings.Values{"limit": float64(i + 1)}, nil, uuid.Nil); err != nil {
				t.Errorf("write %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	_, rev, err := s.Read(ctx, org, "acme", settings.ScopeBranch, "b1")
	if err != nil || rev != n {
		t.Fatalf("rev = %d (want %d) err=%v", rev, n, err)
	}
}

func TestPG_ExpectedRevExactlyOneWinner(t *testing.T) {
	_, s := pgStore(t)
	ctx := context.Background()
	org := uuid.New()
	if _, err := s.Write(ctx, org, "acme", settings.ScopeOrg, "", settings.Values{"mode": "pro"}, nil, uuid.Nil); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var wins, conflicts int64
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			one := int64(1)
			_, err := s.Write(ctx, org, "acme", settings.ScopeOrg, "", settings.Values{"enabled": i%2 == 0, "mode": "basic"}, &one, uuid.Nil)
			switch {
			case err == nil:
				atomic.AddInt64(&wins, 1)
			case errors.Is(err, settings.ErrConflict):
				atomic.AddInt64(&conflicts, 1)
			default:
				t.Errorf("unexpected: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if wins != 1 || conflicts != 19 {
		t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
	}
}

func TestPG_ExpectedRevZeroFirstInsertRace(t *testing.T) {
	_, s := pgStore(t)
	ctx := context.Background()
	org := uuid.New()
	var wg sync.WaitGroup
	var wins int64
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			zero := int64(0)
			if _, err := s.Write(ctx, org, "acme", settings.ScopeOrg, "", settings.Values{"mode": "pro"}, &zero, uuid.Nil); err == nil {
				atomic.AddInt64(&wins, 1)
			} else if !errors.Is(err, settings.ErrConflict) {
				t.Errorf("unexpected: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("wins=%d", wins)
	}
}
