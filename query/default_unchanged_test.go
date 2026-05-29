package query

import (
	"reflect"
	"testing"
)

// TestDefault_NewWithNoOptionsIdenticalToBare proves the variadic New is a
// pure superset: New(meta) and New(meta) with zero options produce builders
// that whitelist the same columns, search the same columns, and are NOT in
// ops-parse mode.
func TestDefault_NewWithNoOptionsIdenticalToBare(t *testing.T) {
	a := New(testMeta())
	b := New(testMeta())
	if !reflect.DeepEqual(a.allowed, b.allowed) {
		t.Errorf("allowed sets differ: %v vs %v", a.allowed, b.allowed)
	}
	if !reflect.DeepEqual(a.searchable, b.searchable) {
		t.Errorf("searchable differ: %v vs %v", a.searchable, b.searchable)
	}
	if a.opsParse || b.opsParse {
		t.Errorf("default builder must not be in ops-parse mode")
	}
	if a.dialect != nil {
		t.Errorf("default builder must have nil dialect")
	}
}

// TestDefault_ParseValuesEqualsParseFromMap proves that when no dialect is
// configured, Builder.ParseValues returns exactly what ParseFromMap returns
// — the default wire-syntax is untouched.
func TestDefault_ParseValuesEqualsParseFromMap(t *testing.T) {
	vals := map[string][]string{
		"page":     {"2"},
		"per_page": {"30"},
		"sortBy":   {"name"},
		"order":    {"asc"},
		"search":   {"foo"},
		"f_status": {"in:a,b,c"},
		"f_amount": {"gte:10"},
		"f_name":   {"ilike:wid"},
	}
	b := New(testMeta())
	got, err := b.ParseValues(vals)
	if err != nil {
		t.Fatalf("ParseValues err: %v", err)
	}
	want, err := ParseFromMap(vals)
	if err != nil {
		t.Fatalf("ParseFromMap err: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseValues != ParseFromMap:\n got=%+v\nwant=%+v", got, want)
	}
}

// TestDefault_ApplySQLUnchanged proves the SQL emitted by Apply for a
// default-parsed request is identical whether or not the ops dialect option
// was passed — Apply never consults the dialect. Both builders parse with
// the DEFAULT parser here (ParseFromMap), so the only variable is the
// option, which must not affect Apply output.
func TestDefault_ApplySQLUnchanged(t *testing.T) {
	vals := map[string][]string{
		"sortBy":   {"amount"},
		"order":    {"desc"},
		"f_status": {"eq:active"},
	}
	params, _ := ParseFromMap(vals)

	bare := New(testMeta()).WithTableName("test_rows")
	withOpt := New(testMeta(), WithOpsDialect()).WithTableName("test_rows")

	sqlBare := renderSQL(t, bare.Apply(openDryDB(t), params))
	sqlOpt := renderSQL(t, withOpt.Apply(openDryDB(t), params))
	if sqlBare != sqlOpt {
		t.Errorf("Apply SQL differs with option:\n bare=%q\n opt =%q", sqlBare, sqlOpt)
	}
}
