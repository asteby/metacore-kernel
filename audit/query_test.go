package audit

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// seed inserts an Entry directly into the table for query tests.
func seed(t *testing.T, db *gorm.DB, e Entry) {
	t.Helper()
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now().UTC()
	}
	if err := db.Table(DefaultTableName).Create(&e).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func TestQuery_OrgScopeAndFilters(t *testing.T) {
	db := setupTestDB(t, DefaultTableName)
	orgA, orgB := uuid.New(), uuid.New()
	actor := uuid.New()
	corr := uuid.New()

	seed(t, db, Entry{OrganizationID: orgA, Model: "products", Action: "created", ActorID: &actor})
	seed(t, db, Entry{OrganizationID: orgA, Model: "products", Action: "updated", CorrelationID: &corr, RecordID: "r1"})
	seed(t, db, Entry{OrganizationID: orgA, Model: "orders", Action: "deleted"})
	seed(t, db, Entry{OrganizationID: orgB, Model: "products", Action: "created"}) // other tenant

	q := NewQuery(db)
	ctx := context.Background()

	// Org scope: orgA sees 3, never orgB's row.
	rows, total, err := q.List(ctx, nil, QueryParams{OrganizationID: orgA})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 3 || len(rows) != 3 {
		t.Fatalf("org scope: total=%d len=%d, want 3/3", total, len(rows))
	}

	// Model filter.
	rows, total, _ = q.List(ctx, nil, QueryParams{OrganizationID: orgA, Model: "products"})
	if total != 2 {
		t.Errorf("model filter total=%d, want 2", total)
	}

	// Action filter.
	_, total, _ = q.List(ctx, nil, QueryParams{OrganizationID: orgA, Action: "deleted"})
	if total != 1 {
		t.Errorf("action filter total=%d, want 1", total)
	}

	// Actor filter.
	_, total, _ = q.List(ctx, nil, QueryParams{OrganizationID: orgA, ActorID: &actor})
	if total != 1 {
		t.Errorf("actor filter total=%d, want 1", total)
	}

	// Correlation filter.
	_, total, _ = q.List(ctx, nil, QueryParams{OrganizationID: orgA, CorrelationID: &corr})
	if total != 1 {
		t.Errorf("correlation filter total=%d, want 1", total)
	}

	// Record filter.
	_, total, _ = q.List(ctx, nil, QueryParams{OrganizationID: orgA, RecordID: "r1"})
	if total != 1 {
		t.Errorf("record filter total=%d, want 1", total)
	}
}

func TestQuery_RequiresOrg(t *testing.T) {
	db := setupTestDB(t, DefaultTableName)
	q := NewQuery(db)
	if _, _, err := q.List(context.Background(), nil, QueryParams{}); err == nil {
		t.Fatalf("expected error when OrganizationID is nil")
	}
}

func TestQuery_PaginationAndOrder(t *testing.T) {
	db := setupTestDB(t, DefaultTableName)
	org := uuid.New()
	base := time.Now().UTC()
	// 5 entries, ascending time; newest must come first.
	for i := 0; i < 5; i++ {
		seed(t, db, Entry{
			OrganizationID: org,
			Model:          "products",
			Action:         "created",
			RecordID:       string(rune('a' + i)),
			OccurredAt:     base.Add(time.Duration(i) * time.Minute),
		})
	}

	q := NewQuery(db)
	ctx := context.Background()

	rows, total, err := q.List(ctx, nil, QueryParams{OrganizationID: org, Page: 1, PerPage: 2})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 5 {
		t.Errorf("total=%d, want 5", total)
	}
	if len(rows) != 2 {
		t.Fatalf("page size=%d, want 2", len(rows))
	}
	// Newest-first: the last-seeded record ('e') is first.
	if rows[0].RecordID != "e" {
		t.Errorf("first row record=%q, want 'e' (newest-first)", rows[0].RecordID)
	}

	// Page 3 with perPage 2 → 1 remaining row.
	rows, _, _ = q.List(ctx, nil, QueryParams{OrganizationID: org, Page: 3, PerPage: 2})
	if len(rows) != 1 {
		t.Errorf("page 3 len=%d, want 1", len(rows))
	}
}

func TestQuery_TimeWindow(t *testing.T) {
	db := setupTestDB(t, DefaultTableName)
	org := uuid.New()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	seed(t, db, Entry{OrganizationID: org, Model: "m", Action: "created", OccurredAt: t0})
	seed(t, db, Entry{OrganizationID: org, Model: "m", Action: "created", OccurredAt: t0.AddDate(0, 0, 5)})
	seed(t, db, Entry{OrganizationID: org, Model: "m", Action: "created", OccurredAt: t0.AddDate(0, 0, 10)})

	from := t0.AddDate(0, 0, 3)
	to := t0.AddDate(0, 0, 6)
	q := NewQuery(db)
	_, total, err := q.List(context.Background(), nil, QueryParams{
		OrganizationID: org, From: &from, To: &to,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 {
		t.Errorf("time window total=%d, want 1 (only day+5)", total)
	}
}
