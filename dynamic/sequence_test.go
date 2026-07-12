package dynamic

import (
	"context"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

func TestFormatSequence(t *testing.T) {
	cases := []struct {
		format string
		n      int64
		want   string
	}{
		{"A-{seq:06}", 42, "A-000042"},
		{"{seq}", 7, "7"},
		{"REM/{seq:03}/2026", 5, "REM/005/2026"},
		{"", 9, "9"},
		{"A-{seq:06}", 1234567, "A-1234567"},
	}
	for _, c := range cases {
		if got := FormatSequence(c.format, c.n); got != c.want {
			t.Errorf("FormatSequence(%q, %d) = %q, want %q", c.format, c.n, got, c.want)
		}
	}
}

// sequenceService builds a Service whose SequenceResolver returns the given
// config for test_products (and nothing for other models).
func sequenceService(t *testing.T, db *gorm.DB, ms *ModelSequences) *Service {
	t.Helper()
	modelbase.Register("test_products", func() modelbase.ModelDefiner { return &TestProduct{} })
	meta := metadata.New(metadata.Config{CacheTTL: -1})
	return New(Config{
		DB:       db,
		Metadata: meta,
		SequenceResolver: func(_ context.Context, model string) (*ModelSequences, bool) {
			if model == "test_products" {
				return ms, true
			}
			return nil, false
		},
	})
}

func folioConfig() *ModelSequences {
	return &ModelSequences{
		Sequences: []manifest.SequenceDef{
			{Key: "folio", Scope: "org", Format: "F-{seq:04}"},
		},
		ColumnBindings: map[string]string{"name": "folio"},
	}
}

func TestSequences_CreateStampsBoundColumn(t *testing.T) {
	db := setupTestDB(t)
	svc := sequenceService(t, db, folioConfig())
	user := newUser(uuid.New())

	for i, want := range []string{"F-0001", "F-0002", "F-0003"} {
		out, err := svc.Create(context.Background(), "test_products", user, map[string]any{"price": float64(i)})
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		if got := out["name"]; got != want {
			t.Errorf("create %d: name = %v, want %q", i, got, want)
		}
	}
}

func TestSequences_ExplicitValueWins(t *testing.T) {
	db := setupTestDB(t)
	svc := sequenceService(t, db, folioConfig())
	user := newUser(uuid.New())

	out, err := svc.Create(context.Background(), "test_products", user, map[string]any{
		"name":  "LEGACY-99",
		"price": 1.0,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := out["name"]; got != "LEGACY-99" {
		t.Errorf("name = %v, want LEGACY-99 (explicit folio must win)", got)
	}
	// The counter must NOT have advanced for the explicit row.
	out2, err := svc.Create(context.Background(), "test_products", user, map[string]any{"price": 2.0})
	if err != nil {
		t.Fatalf("create 2: %v", err)
	}
	if got := out2["name"]; got != "F-0001" {
		t.Errorf("name = %v, want F-0001 (explicit value must not consume the counter)", got)
	}
}

func TestSequences_SeriesArePerOrg(t *testing.T) {
	db := setupTestDB(t)
	svc := sequenceService(t, db, folioConfig())
	userA := newUser(uuid.New())
	userB := newUser(uuid.New())

	outA, err := svc.Create(context.Background(), "test_products", userA, map[string]any{"price": 1.0})
	if err != nil {
		t.Fatalf("create A: %v", err)
	}
	outB, err := svc.Create(context.Background(), "test_products", userB, map[string]any{"price": 1.0})
	if err != nil {
		t.Fatalf("create B: %v", err)
	}
	if outA["name"] != "F-0001" || outB["name"] != "F-0001" {
		t.Errorf("each org must own its series: got A=%v B=%v, want F-0001 both", outA["name"], outB["name"])
	}
}

func TestNextSequence_FormatsAndValidates(t *testing.T) {
	db := setupTestDB(t)
	svc := sequenceService(t, db, folioConfig())
	user := newUser(uuid.New())

	got, err := svc.NextSequence(context.Background(), user, "test_products", "folio")
	if err != nil {
		t.Fatalf("NextSequence: %v", err)
	}
	if got != "F-0001" {
		t.Errorf("NextSequence = %q, want F-0001", got)
	}
	if _, err := svc.NextSequence(context.Background(), user, "test_products", "nope"); err == nil {
		t.Error("unknown sequence key must error")
	}
	if _, err := svc.NextSequence(context.Background(), user, "other_model", "folio"); err == nil {
		t.Error("model without sequences must error")
	}
}

func TestNextSequenceValue_MonotonicWithoutGapsPerScope(t *testing.T) {
	db := setupTestDB(t)
	orgID := uuid.New()
	if err := ensureSequenceTable(db); err != nil {
		t.Fatalf("ensure table: %v", err)
	}

	for want := int64(1); want <= 20; want++ {
		v, err := nextSequenceValue(context.Background(), db, orgID, "", "test_products", "folio")
		if err != nil {
			t.Fatalf("next %d: %v", want, err)
		}
		if v != want {
			t.Fatalf("value = %d, want %d (monotonic, no gaps)", v, want)
		}
	}
	// A different scope_value (branch) owns its own series.
	v, err := nextSequenceValue(context.Background(), db, orgID, uuid.NewString(), "test_products", "folio")
	if err != nil {
		t.Fatalf("branch-scoped next: %v", err)
	}
	if v != 1 {
		t.Fatalf("branch-scoped series must start at 1, got %d", v)
	}
}
