package dispatch

import (
	"strings"
	"testing"
)

func TestDueIndexPlan(t *testing.T) {
	const tbl = "kernel_event_deliveries"
	cases := []struct {
		name                string
		dialect             string
		inTx, exists, valid bool
		wantPrefixes        []string
	}{
		{"sqlite keeps the plain statement", "sqlite", false, false, false, []string{"CREATE INDEX IF NOT EXISTS "}},
		{"postgres missing => concurrent create", "postgres", false, false, false, []string{"CREATE INDEX CONCURRENTLY IF NOT EXISTS "}},
		{"postgres valid => concurrent create is an idempotent no-op", "postgres", false, true, true, nil},
		{"postgres invalid => drop then recreate", "postgres", false, true, false,
			[]string{"DROP INDEX CONCURRENTLY IF EXISTS ", "CREATE INDEX CONCURRENTLY IF NOT EXISTS "}},
		{"postgres inside a tx cannot use CONCURRENTLY", "postgres", true, false, false, []string{"CREATE INDEX IF NOT EXISTS "}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := dueIndexPlan(c.dialect, tbl, c.inTx, c.exists, c.valid)
			if len(got) != len(c.wantPrefixes) {
				t.Fatalf("plan = %q, want %d statement(s)", got, len(c.wantPrefixes))
			}
			for i, p := range c.wantPrefixes {
				if !strings.HasPrefix(got[i], p) {
					t.Fatalf("stmt %d = %q, want prefix %q", i, got[i], p)
				}
			}
		})
	}
}

func TestPgIndexRefQualifiesBySchema(t *testing.T) {
	if got := pgIndexRef("tenant.deliveries", "idx_x"); got != "tenant.idx_x" {
		t.Fatal(got)
	}
	if got := pgIndexRef("deliveries", "idx_x"); got != "idx_x" {
		t.Fatal(got)
	}
}
