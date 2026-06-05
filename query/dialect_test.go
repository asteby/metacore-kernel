package query

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// --- ParseOpsFilterValue: per-operator decoding -------------------------

func TestParseOpsFilterValue_Operators(t *testing.T) {
	mustFloat := func(f float64) *float64 { return &f }

	cases := []struct {
		name string
		raw  string
		want Filter
	}{
		{"exact default", "active", Filter{Op: OpEq, Value: "active"}},
		{"eq explicit strips operator", "eq:10", Filter{Op: OpEq, Value: "10"}},
		{"eq uppercase", "EQ:active", Filter{Op: OpEq, Value: "active"}},
		{"neq", "neq:5", Filter{Op: OpNeq, Value: "5"}},
		{"ne alias", "NE:x", Filter{Op: OpNeq, Value: "x"}},
		{"in", "IN:a,b,c", Filter{Op: OpIn, Value: []string{"a", "b", "c"}}},
		{"in lowercase op", "in:a,b", Filter{Op: OpIn, Value: []string{"a", "b"}}},
		{"not_in", "NOT_IN:x,y", Filter{Op: OpNotIn, Value: []string{"x", "y"}}},
		{"like", "LIKE:foo", Filter{Op: OpLike, Value: "foo"}},
		{"ilike", "ILIKE:bar", Filter{Op: OpUnaccentIlike, Value: "bar"}},
		{"contains alias of ilike", "CONTAINS:bar", Filter{Op: OpUnaccentIlike, Value: "bar"}},
		{"contains lowercase", "contains:bar", Filter{Op: OpUnaccentIlike, Value: "bar"}},
		{"gt", "GT:10", Filter{Op: OpGt, Value: float64(10)}},
		{"lt", "LT:5.5", Filter{Op: OpLt, Value: float64(5.5)}},
		{"gte", "GTE:1", Filter{Op: OpNumGte, Value: float64(1)}},
		{"lte", "LTE:99", Filter{Op: OpNumLte, Value: float64(99)}},
		{"range both", "RANGE:5,100", Filter{Op: OpNumRange, Value: [2]*float64{mustFloat(5), mustFloat(100)}}},
		{"range lower only", "RANGE:5,", Filter{Op: OpNumRange, Value: [2]*float64{mustFloat(5), nil}}},
		{"range upper only", "RANGE:,100", Filter{Op: OpNumRange, Value: [2]*float64{nil, mustFloat(100)}}},
		{"null bare", "NULL", Filter{Op: OpNull}},
		{"null colon", "NULL:", Filter{Op: OpNull}},
		{"not_null bare", "NOT_NULL", Filter{Op: OpNotNull}},
		{"unknown op falls back to eq literal", "FOO:bar", Filter{Op: OpEq, Value: "FOO:bar"}},
		{"empty", "", Filter{Op: OpEq, Value: ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseOpsFilterValue(tc.raw)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ParseOpsFilterValue(%q) = %+v, want %+v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestParseOpsFilterValue_NumericNonParseableIsNoop(t *testing.T) {
	for _, raw := range []string{"GT:abc", "LT:", "GTE:x", "LTE:y"} {
		got := ParseOpsFilterValue(raw)
		if got.Op != FilterOp("noop") {
			t.Errorf("ParseOpsFilterValue(%q).Op = %q, want noop", raw, got.Op)
		}
	}
}

// Ops parity: a RANGE without a comma must be DROPPED (noop), not applied as a
// single-sided `>=`. Matches ops query_sorting.go which requires len(parts)==2.
func TestParseOpsFilterValue_RangeWithoutCommaIsNoop(t *testing.T) {
	for _, raw := range []string{"RANGE:5", "RANGE:", "RANGE:abc"} {
		got := ParseOpsFilterValue(raw)
		if got.Op != FilterOp("noop") {
			t.Errorf("ParseOpsFilterValue(%q).Op = %q, want noop (comma required)", raw, got.Op)
		}
	}
}

func TestParseOpsFilterValue_DateRange(t *testing.T) {
	got := ParseOpsFilterValue("2024-01-01_2024-03-31")
	if got.Op != OpDateRange {
		t.Fatalf("Op = %q, want date_range", got.Op)
	}
	bounds, ok := got.Value.([2]time.Time)
	if !ok {
		t.Fatalf("Value type = %T, want [2]time.Time", got.Value)
	}
	wantStart := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	if !bounds[0].Equal(wantStart) {
		t.Errorf("start = %v, want %v", bounds[0], wantStart)
	}
	// End snapped to end of day.
	if bounds[1].Hour() != 23 || bounds[1].Minute() != 59 || bounds[1].Second() != 59 {
		t.Errorf("end not snapped to EOD: %v", bounds[1])
	}
	if bounds[1].Day() != 31 || bounds[1].Month() != time.March {
		t.Errorf("end date = %v, want 2024-03-31", bounds[1])
	}
}

func TestParseOpsFilterValue_NotADateRange(t *testing.T) {
	// Too short / wrong separator / extra underscores should not be a date
	// range (ops splits on "_" and requires EXACTLY two parts).
	for _, raw := range []string{"2024-01-01", "2024-01-01:2024-03-31", "2024-01-01_2024-03-31_x", "2024-01-01_2024-03-31_2024-04-01"} {
		got := ParseOpsFilterValue(raw)
		if got.Op == OpDateRange {
			t.Errorf("ParseOpsFilterValue(%q) wrongly parsed as date_range", raw)
		}
	}
}

func TestParseOpsFilterValue_TruncatesAndStripsControl(t *testing.T) {
	long := strings.Repeat("a", MaxFilterValueLength+50)
	got := ParseOpsFilterValue(long)
	if v := got.Value.(string); len(v) != MaxFilterValueLength {
		t.Errorf("value len = %d, want %d", len(v), MaxFilterValueLength)
	}
	ctrl := ParseOpsFilterValue("ab\x00c\x07d")
	if v := ctrl.Value.(string); v != "abcd" {
		t.Errorf("control chars not stripped: %q", v)
	}
	// Accents preserved.
	acc := ParseOpsFilterValue("piñón")
	if v := acc.Value.(string); v != "piñón" {
		t.Errorf("accents stripped: %q", v)
	}
}

// --- applyOneFilter: SQL emission for ops operators ---------------------

func TestApplyOneFilter_OpsOperators_SQL(t *testing.T) {
	mustFloat := func(f float64) *float64 { return &f }

	cases := []struct {
		name        string
		col         string
		f           Filter
		wantSQLPart string
	}{
		{"not_in", "status", Filter{Op: OpNotIn, Value: []string{"a", "b"}}, "status NOT IN"},
		{"like", "name", Filter{Op: OpLike, Value: "foo"}, `name LIKE`},
		{"unaccent ilike", "name", Filter{Op: OpUnaccentIlike, Value: "bar"}, "unaccent(name) ILIKE unaccent("},
		{"gt", "amount", Filter{Op: OpGt, Value: float64(10)}, "amount > 10"},
		{"lt", "amount", Filter{Op: OpLt, Value: float64(10)}, "amount < 10"},
		{"num gte", "amount", Filter{Op: OpNumGte, Value: float64(10)}, "amount >= 10"},
		{"num lte", "amount", Filter{Op: OpNumLte, Value: float64(10)}, "amount <= 10"},
		{"num range both", "amount", Filter{Op: OpNumRange, Value: [2]*float64{mustFloat(5), mustFloat(100)}}, "amount >= 5"},
		{"null", "deleted_at", Filter{Op: OpNull}, "deleted_at IS NULL"},
		{"not null", "deleted_at", Filter{Op: OpNotNull}, "deleted_at IS NOT NULL"},
		{"jsonb eq", "fiscal_data", Filter{Op: OpJSONBEq, Value: JSONBFilter{Key: "rfc", Val: "XAXX"}}, "fiscal_data->>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := openDryDB(t)
			db = applyOneFilter(db, tc.col, tc.f)
			sql := renderSQL(t, db)
			if !strings.Contains(sql, tc.wantSQLPart) {
				t.Fatalf("SQL %q does not contain %q", sql, tc.wantSQLPart)
			}
		})
	}
}

func TestApplyOneFilter_LikeEmitsEscapeClause(t *testing.T) {
	for _, f := range []Filter{
		{Op: OpLike, Value: "foo"},
		{Op: OpUnaccentIlike, Value: "bar"},
	} {
		db := openDryDB(t)
		db = applyOneFilter(db, "name", f)
		sql := renderSQL(t, db)
		if !strings.Contains(sql, `ESCAPE '\'`) {
			t.Errorf("op %q missing ESCAPE clause: %q", f.Op, sql)
		}
	}
}

func TestApplyOneFilter_NumRangeUpperOnly(t *testing.T) {
	upper := 100.0
	db := openDryDB(t)
	db = applyOneFilter(db, "amount", Filter{Op: OpNumRange, Value: [2]*float64{nil, &upper}})
	sql := renderSQL(t, db)
	if strings.Contains(sql, ">=") {
		t.Errorf("upper-only range should not emit >=: %q", sql)
	}
	if !strings.Contains(sql, "amount <= 100") {
		t.Errorf("missing upper bound: %q", sql)
	}
}

func TestApplyOneFilter_DateRange_SQL(t *testing.T) {
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2024, 3, 31, 23, 59, 59, 0, time.UTC)
	db := openDryDB(t)
	db = applyOneFilter(db, "created_at", Filter{Op: OpDateRange, Value: [2]time.Time{start, end}})
	sql := renderSQL(t, db)
	if !strings.Contains(sql, "created_at >=") || !strings.Contains(sql, "created_at <=") {
		t.Fatalf("date range SQL missing bounds: %q", sql)
	}
}

func TestApplyOneFilter_NoopOpDropsClause(t *testing.T) {
	db := openDryDB(t)
	before := renderSQL(t, openDryDB(t))
	db = applyOneFilter(db, "amount", Filter{Op: FilterOp("noop")})
	after := renderSQL(t, db)
	if before != after {
		t.Errorf("noop op should not change SQL: before=%q after=%q", before, after)
	}
}
