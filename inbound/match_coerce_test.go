package inbound

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
)

// A match value that arrives with another JSON type than its column (a number
// for a text column, a numeric string for an integer one) must behave as if it
// had arrived with the right type. The bug: GormTombstones bound the number as
// is and Postgres refused it ("unable to encode 7 into text format for text"),
// a 500; SQLite's flexible typing hid it. Runs on SQLite (CGO) and on Postgres
// (TEST_POSTGRES_DSN, the CI job "postgres").

const mcModel = "inbound_match_coerce"

type mcRow struct {
	ID             uuid.UUID      `json:"id" gorm:"type:uuid;primaryKey"`
	OrganizationID uuid.UUID      `json:"organization_id" gorm:"type:uuid;index"`
	CreatedByID    *uuid.UUID     `json:"created_by_id,omitempty" gorm:"type:uuid"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	DeletedAt      gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
	ExternalID     string         `json:"external_id" gorm:"size:64"`
	Source         string         `json:"source" gorm:"size:32"`
	Seq            int            `json:"seq"`
	Note           string         `json:"note" gorm:"size:64"`
	SourceAt       *time.Time     `json:"source_at"`
}

func (r *mcRow) BeforeCreate(*gorm.DB) error {
	if r.ID == uuid.Nil {
		r.ID = uuid.New()
	}
	return nil
}

func (mcRow) TableName() string { return mcModel }
func (mcRow) DefineTable() modelbase.TableMetadata {
	return modelbase.TableMetadata{
		Title: "Match coerce",
		Columns: []modelbase.ColumnDef{
			{Key: "external_id", Label: "External", Type: "text"},
			{Key: "source", Label: "Source", Type: "text"},
			{Key: "seq", Label: "Seq", Type: "number"},
			{Key: "note", Label: "Note", Type: "text"},
			{Key: "source_at", Label: "Source at", Type: "datetime"},
		},
		SearchColumns: []string{"external_id"},
	}
}
func (mcRow) DefineModal() modelbase.ModalMetadata {
	return modelbase.ModalMetadata{Title: "Match coerce"}
}

const (
	mcSQLite = `CREATE TABLE inbound_match_coerce (id TEXT PRIMARY KEY, organization_id TEXT, created_by_id TEXT,
		created_at DATETIME, updated_at DATETIME, deleted_at DATETIME, external_id TEXT, source TEXT, seq INTEGER, note TEXT, source_at DATETIME)`
	mcPG = `CREATE TABLE inbound_match_coerce (id uuid PRIMARY KEY, organization_id uuid, created_by_id uuid,
		created_at timestamptz, updated_at timestamptz, deleted_at timestamptz, external_id text, source text, seq integer, note text, source_at timestamptz)`
)

func eachMatchCoerceDB(t *testing.T, fn func(t *testing.T, e *mcEnv)) {
	t.Helper()
	run := func(t *testing.T, db *gorm.DB, ddl string) {
		t.Helper()
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatal(err)
		}
		fn(t, newMCEnv(t, db))
	}
	t.Run("SQLite", func(t *testing.T) { run(t, openSQLite(t), mcSQLite) })
	t.Run("Postgres", func(t *testing.T) { run(t, postgresDB(t), mcPG) })
}

type mcEnv struct {
	t   *testing.T
	db  *gorm.DB
	ex  *DynamicExecutor
	org uuid.UUID
}

func newMCEnv(t *testing.T, db *gorm.DB) *mcEnv {
	t.Helper()
	modelbase.Register(mcModel, func() modelbase.ModelDefiner { return &mcRow{} })
	svc := dynamic.New(dynamic.Config{
		AllowUnconfiguredPermissions: true, DB: db, Metadata: metadata.New(metadata.Config{CacheTTL: -1}),
		Bus: &atomBus{}, DisableEventOutbox: true,
	})
	tomb := GormTombstones{DB: db, Table: func(string) string { return mcModel }}
	return &mcEnv{t: t, db: db, org: uuid.New(), ex: &DynamicExecutor{Service: svc, Tombstones: tomb}}
}

var mcMatch = []string{"external_id", "source"}

// upsertOp is the i-th event of the external id ext (any JSON type).
func (e *mcEnv) upsertOp(ext any, note string, i int) Operation {
	at := atomAt(i)
	return Operation{Org: e.org, Route: Route{Model: mcModel, Op: OpUpsert, Match: mcMatch},
		Values: map[string]any{"external_id": ext, "source": "v2", "note": note, "source_at": at},
		Guard:  &OperationGuard{Field: "source_at", Op: GuardGT, Value: at, Match: mcMatch}}
}

func (e *mcEnv) deleteOp(ext any, i int) Operation {
	at := atomAt(i)
	return Operation{Org: e.org, Route: Route{Model: mcModel, Op: OpDelete, Match: mcMatch},
		Values: map[string]any{"external_id": ext, "source": "v2"},
		Guard:  &OperationGuard{Field: "source_at", Op: GuardGT, Value: at, Match: mcMatch}}
}

func (e *mcEnv) restoreOp(ext any) Operation {
	return Operation{Org: e.org, Route: Route{Model: mcModel, Op: OpRestore, Match: mcMatch},
		Values: map[string]any{"external_id": ext, "source": "v2"}}
}

func (e *mcEnv) live(where string, args ...any) int {
	e.t.Helper()
	var n int64
	args = append([]any{e.org}, args...)
	if err := e.db.Model(&mcRow{}).Where("organization_id = ? AND "+where, args...).Count(&n).Error; err != nil {
		e.t.Fatal(err)
	}
	return int(n)
}

func (e *mcEnv) note(ext string) string {
	e.t.Helper()
	var r mcRow
	if err := e.db.Where("organization_id = ? AND external_id = ?", e.org, ext).Take(&r).Error; err != nil {
		e.t.Fatal(err)
	}
	return r.Note
}

func TestMatchNumberForTextColumnUpsertsOnce(t *testing.T) {
	eachMatchCoerceDB(t, func(t *testing.T, e *mcEnv) {
		ctx := context.Background()
		out, err := e.ex.Execute(ctx, e.upsertOp(float64(7), "a", 1))
		if err != nil || !out.Created {
			t.Fatalf("first upsert must create the row: %+v %v", out, err)
		}
		out, err = e.ex.Execute(ctx, e.upsertOp(float64(7), "b", 2))
		if err != nil || out.Created || out.Stale {
			t.Fatalf("second upsert must update, not create: %+v %v", out, err)
		}
		if n := e.live("external_id = ?", "7"); n != 1 {
			t.Fatalf("live rows for \"7\" = %d, want 1 (no duplicate)", n)
		}
		if got := e.note("7"); got != "b" {
			t.Fatalf("note = %q, want b", got)
		}
		// The same key spelled as text finds the same row.
		out, err = e.ex.Execute(ctx, e.upsertOp("7", "c", 3))
		if err != nil || out.Created {
			t.Fatalf("text spelling must hit the same row: %+v %v", out, err)
		}
		// A fraction keeps its digits, a big number does not go exponential.
		if _, err = e.ex.Execute(ctx, e.upsertOp(7.5, "x", 1)); err != nil {
			t.Fatal(err)
		}
		if _, err = e.ex.Execute(ctx, e.upsertOp(float64(1e21), "y", 1)); err != nil {
			t.Fatal(err)
		}
		if e.live("external_id = ?", "7.5") != 1 || e.live("external_id = ?", "1000000000000000000000") != 1 {
			t.Fatal("canonical text of the numbers was not stored")
		}
	})
}

func TestMatchNumberForTextColumnDeleteAndTombstone(t *testing.T) {
	eachMatchCoerceDB(t, func(t *testing.T, e *mcEnv) {
		ctx := context.Background()
		if _, err := e.ex.Execute(ctx, e.upsertOp(float64(7), "a", 1)); err != nil {
			t.Fatal(err)
		}
		out, err := e.ex.Execute(ctx, e.deleteOp(float64(7), 2))
		if err != nil || out.Affected != 1 {
			t.Fatalf("delete with a numeric match must retire the row: %+v %v", out, err)
		}
		if n := e.live("external_id = ?", "7"); n != 0 {
			t.Fatalf("live rows after delete = %d", n)
		}
		// A late event older than the tombstone must not resurrect the row; this
		// is the tombstone query that Postgres refused.
		out, err = e.ex.Execute(ctx, e.upsertOp(float64(7), "late", 1))
		if err != nil || !out.Stale {
			t.Fatalf("the tombstone must block the late event: %+v %v", out, err)
		}
		if n := e.live("external_id = ?", "7"); n != 0 {
			t.Fatalf("the late event resurrected the row (%d)", n)
		}
		// A newer one does create it again.
		out, err = e.ex.Execute(ctx, e.upsertOp(float64(7), "new", 3))
		if err != nil || !out.Created {
			t.Fatalf("a newer event must recreate the row: %+v %v", out, err)
		}
	})
}

func TestMatchNumberForTextColumnRestore(t *testing.T) {
	eachMatchCoerceDB(t, func(t *testing.T, e *mcEnv) {
		ctx := context.Background()
		if _, err := e.ex.Execute(ctx, e.upsertOp(float64(8), "a", 1)); err != nil {
			t.Fatal(err)
		}
		if _, err := e.ex.Execute(ctx, e.deleteOp(float64(8), 2)); err != nil {
			t.Fatal(err)
		}
		out, err := e.ex.Execute(ctx, e.restoreOp(float64(8)))
		if err != nil || out.Affected != 1 {
			t.Fatalf("restore with a numeric match: %+v %v", out, err)
		}
		if n := e.live("external_id = ?", "8"); n != 1 {
			t.Fatalf("live rows after restore = %d", n)
		}
	})
}

func TestMatchNumericStringForIntegerColumn(t *testing.T) {
	eachMatchCoerceDB(t, func(t *testing.T, e *mcEnv) {
		ctx := context.Background()
		op := func(seq any, note string) Operation {
			return Operation{Org: e.org, Route: Route{Model: mcModel, Op: OpUpsert, Match: []string{"seq", "source"}},
				Values: map[string]any{"seq": seq, "source": "v3", "note": note}}
		}
		out, err := e.ex.Execute(ctx, op("42", "a"))
		if err != nil || !out.Created {
			t.Fatalf("first upsert: %+v %v", out, err)
		}
		for _, seq := range []any{"42", float64(42), " 42 "} {
			out, err = e.ex.Execute(ctx, op(seq, "b"))
			if err != nil || out.Created {
				t.Fatalf("upsert with seq %#v must update: %+v %v", seq, out, err)
			}
		}
		if n := e.live("seq = ? AND source = ?", 42, "v3"); n != 1 {
			t.Fatalf("rows with seq 42 = %d, want 1", n)
		}
		del := Operation{Org: e.org, Route: Route{Model: mcModel, Op: OpDelete, Match: []string{"seq", "source"}},
			Values: map[string]any{"seq": "42", "source": "v3"}}
		out, err = e.ex.Execute(ctx, del)
		if err != nil || out.Affected != 1 {
			t.Fatalf("delete with a string number: %+v %v", out, err)
		}
	})
}

func TestMatchValueNotConvertibleIsInvalid(t *testing.T) {
	eachMatchCoerceDB(t, func(t *testing.T, e *mcEnv) {
		ctx := context.Background()
		for name, op := range map[string]Operation{
			"text for integer": {Org: e.org, Route: Route{Model: mcModel, Op: OpUpsert, Match: []string{"seq"}},
				Values: map[string]any{"seq": "abc"}},
			"decimal comma for integer": {Org: e.org, Route: Route{Model: mcModel, Op: OpDelete, Match: []string{"seq"}},
				Values: map[string]any{"seq": "1,5"}},
			"object for text": {Org: e.org, Route: Route{Model: mcModel, Op: OpUpsert, Match: []string{"external_id"}},
				Values: map[string]any{"external_id": map[string]any{"a": 1}}},
			"array for text": {Org: e.org, Route: Route{Model: mcModel, Op: OpRestore, Match: []string{"external_id"}},
				Values: map[string]any{"external_id": []any{1}}},
			"bool for integer": {Org: e.org, Route: Route{Model: mcModel, Op: OpUpsert, Match: []string{"seq"}},
				Values: map[string]any{"seq": true}},
		} {
			_, err := e.ex.Execute(ctx, op)
			ee := execErr(t, err)
			if ee.Kind != KindInvalid || ee.Code != CodeMatchValueInvalid {
				t.Errorf("%s: kind=%v code=%q, want invalid/%s", name, ee.Kind, ee.Code, CodeMatchValueInvalid)
			}
		}
		if n := e.live("1 = 1"); n != 0 {
			t.Fatalf("a rejected event wrote %d rows", n)
		}
	})
}

func TestMatchCoerceValue(t *testing.T) {
	at := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		class string
		in    any
		want  any
		bad   bool
	}{
		{classText, float64(7), "7", false},
		{classText, int64(7), "7", false},
		{classText, 7.25, "7.25", false},
		{classText, float64(1e21), "1000000000000000000000", false},
		{classText, json.Number("12"), "12", false},
		{classText, true, "true", false},
		{classText, "x", "x", false},
		{classText, uuid.MustParse("11111111-1111-1111-1111-111111111111"), "11111111-1111-1111-1111-111111111111", false},
		{classText, map[string]any{}, nil, true},
		{classText, []any{1}, nil, true},
		{classNumber, "42", int64(42), false},
		{classNumber, " 42 ", int64(42), false},
		{classNumber, "4.5", 4.5, false},
		{classNumber, float64(42), int64(42), false},
		{classNumber, 4.5, 4.5, false},
		{classNumber, json.Number("9"), int64(9), false},
		{classNumber, "abc", nil, true},
		{classNumber, "NaN", nil, true},
		{classNumber, true, nil, true},
		{classNumber, map[string]any{}, nil, true},
		{classBool, "true", true, false},
		{classBool, float64(0), false, false},
		{classBool, float64(2), nil, true},
		{classBool, "maybe", nil, true},
		{classTime, "2026-03-01T10:00:00Z", at, false},
		{classTime, "2026-03-01", time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), false},
		{classTime, float64(5), nil, true},
		{classNumber, nil, nil, false},
		{classNumber, "  ", "  ", false},
	} {
		got, err := coerceMatchValue(c.class, c.in)
		if (err != nil) != c.bad {
			t.Errorf("coerce(%s, %#v): err=%v, want bad=%v", c.class, c.in, err, c.bad)
			continue
		}
		if c.bad {
			continue
		}
		if tw, isT := c.want.(time.Time); isT {
			if tg, ok := got.(time.Time); !ok || !tg.Equal(tw) {
				t.Errorf("coerce(%s, %#v) = %#v, want %v", c.class, c.in, got, tw)
			}
			continue
		}
		if got != c.want {
			t.Errorf("coerce(%s, %#v) = %#v, want %#v", c.class, c.in, got, c.want)
		}
	}
}

func TestMatchClassOfColumnTypes(t *testing.T) {
	for in, want := range map[string]string{
		"text": classText, "string": classText, "uuid": classText, "varchar(64)": classText, "character varying(9)": classText,
		"number": classNumber, "integer": classNumber, "bigint": classNumber, "numeric(18,4)": classNumber, "decimal": classNumber,
		"boolean": classBool, "bool": classBool,
		"date": classTime, "datetime": classTime, "timestamptz": classTime,
		"": "", "jsonb": "", "vector(3)": "",
	} {
		if got := matchClass(in); got != want {
			t.Errorf("matchClass(%q) = %q, want %q", in, got, want)
		}
	}
}

// For a column whose type the metadata does not tell, the typed comparison
// still reads the event with the kind of the stored value.
func TestMatchEqualToleratesTheEventsJSONType(t *testing.T) {
	for name, c := range map[string][2]any{
		"number vs text row":   {"7", float64(7)},
		"text vs int row":      {int64(7), "7"},
		"text vs float row":    {float64(7), "7.0"},
		"bool text vs bool":    {true, "true"},
		"date text vs instant": {time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), "2026-03-01"},
	} {
		if !matchEqual(c[0], c[1]) {
			t.Errorf("%s must be equal", name)
		}
	}
	for name, c := range map[string][2]any{
		"other number":     {"7", float64(8)},
		"other text":       {int64(7), "8"},
		"word vs number":   {int64(7), "seven"},
		"nil row":          {nil, float64(7)},
		"text vs text":     {"7", "07"},
		"number vs object": {"7", map[string]any{}},
	} {
		if matchEqual(c[0], c[1]) {
			t.Errorf("%s must differ", name)
		}
	}
	// valuesEqual itself stays strict.
	if valuesEqual("5", int64(5)) {
		t.Error("valuesEqual must stay typed")
	}
}
