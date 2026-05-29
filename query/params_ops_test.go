package query

import (
	"reflect"
	"testing"
)

func TestParseOpsFromValues_FlatOperators(t *testing.T) {
	vals := map[string][]string{
		"f_status": {"IN:active,archived"},
		"f_amount": {"GT:100"},
		"f_name":   {"ILIKE:wid"},
	}
	p, err := ParseOpsFromValues(vals)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got := p.Filters["status"]; got.Op != OpIn {
		t.Errorf("status op = %q, want in", got.Op)
	}
	if got := p.Filters["amount"]; got.Op != OpGt || got.Value.(float64) != 100 {
		t.Errorf("amount = %+v, want gt 100", got)
	}
	if got := p.Filters["name"]; got.Op != OpUnaccentIlike {
		t.Errorf("name op = %q, want unaccent_ilike", got.Op)
	}
}

func TestParseOpsFromValues_JSONBPath(t *testing.T) {
	p, err := ParseOpsFromValues(map[string][]string{
		"f_fiscal_data.rfc": {"XAXX010101000"},
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	f, ok := p.Filters["fiscal_data"]
	if !ok {
		t.Fatalf("no fiscal_data filter; filters=%+v", p.Filters)
	}
	if f.Op != OpJSONBEq {
		t.Fatalf("op = %q, want jsonb_eq", f.Op)
	}
	jf := f.Value.(JSONBFilter)
	if jf.Key != "rfc" || jf.Val != "XAXX010101000" {
		t.Errorf("jsonb = %+v", jf)
	}
	// JSONB path must NOT be promoted to a relation filter.
	if len(p.RelationFilters) != 0 {
		t.Errorf("jsonb path leaked into relation filters: %+v", p.RelationFilters)
	}
}

func TestParseOpsFromValues_RelationFilter(t *testing.T) {
	p, err := ParseOpsFromValues(map[string][]string{
		"f_product_variant.name": {"ILIKE:abc"},
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(p.RelationFilters) != 1 {
		t.Fatalf("relation filters = %+v", p.RelationFilters)
	}
	rf := p.RelationFilters[0]
	if rf.Relation != "product_variant" || rf.Field != "name" || rf.Op != OpUnaccentIlike {
		t.Errorf("relation filter = %+v", rf)
	}
}

func TestParseOpsFromValues_NativeMultiValueIsIn(t *testing.T) {
	p, err := ParseOpsFromValues(map[string][]string{
		"f_status": {"a", "b", "c"},
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	f := p.Filters["status"]
	if f.Op != OpIn {
		t.Fatalf("op = %q, want in", f.Op)
	}
	if !reflect.DeepEqual(f.Value, []string{"a", "b", "c"}) {
		t.Errorf("value = %+v", f.Value)
	}
}

func TestParseOpsFromValues_PaginationAndSort(t *testing.T) {
	p, err := ParseOpsFromValues(map[string][]string{
		"page":     {"3"},
		"per_page": {"50"},
		"sortBy":   {"created_at"},
		"order":    {"asc"},
		"search":   {"hello"},
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if p.Page != 3 || p.PerPage != 50 || p.SortBy != "created_at" || p.Order != "asc" || p.Search != "hello" {
		t.Errorf("params = %+v", p)
	}
}

// WithFilterDialect (standalone, no ops promotions) routes flat values
// through the custom decoder while keeping ParseFromMap's structure.
func TestBuilder_ParseValues_CustomDialect(t *testing.T) {
	// A trivial custom dialect that forces every value to OpNotNull.
	custom := func(raw string) Filter { return Filter{Op: OpNotNull} }
	b := New(testMeta(), WithFilterDialect(custom))
	p, err := b.ParseValues(map[string][]string{"f_status": {"whatever"}})
	if err != nil {
		t.Fatal(err)
	}
	if p.Filters["status"].Op != OpNotNull {
		t.Errorf("custom dialect not applied: %+v", p.Filters["status"])
	}
}

// Builder.ParseValues routes through the ops parser only when opted in.
func TestBuilder_ParseValues_DialectRouting(t *testing.T) {
	vals := map[string][]string{"f_amount": {"GT:10"}}

	// Default: ParseFromMap → GT is not a known default op, value becomes
	// "GT:10" split on ':' → op "gt" (string OpGte? no, OpGt not in default
	// set) → default dialect maps unknown op to eq literal? Actually default
	// parseFilterValue does not know "gt"; it falls to default eq literal.
	defBuilder := New(testMeta())
	defP, _ := defBuilder.ParseValues(vals)
	if defP.Filters["amount"].Op != OpEq {
		t.Errorf("default dialect amount op = %q, want eq (literal)", defP.Filters["amount"].Op)
	}

	// Ops dialect: GT parsed numerically.
	opsBuilder := New(testMeta(), WithOpsDialect())
	opsP, _ := opsBuilder.ParseValues(vals)
	if opsP.Filters["amount"].Op != OpGt {
		t.Errorf("ops dialect amount op = %q, want gt", opsP.Filters["amount"].Op)
	}
}
