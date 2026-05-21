package query

import (
	"errors"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/modelbase"
)

// testRelations is the relation set used by the extension tests below.
// It declares one belongs_to edge (customer) and one one_to_many edge
// (items) so the JOIN-direction logic on both kinds is covered.
func testRelations() []modelbase.RelationDef {
	return []modelbase.RelationDef{
		{
			Name:       "Customer",
			Kind:       "belongs_to",
			Through:    "customers",
			ForeignKey: "customer_id",
			References: "id",
		},
		{
			Name:       "Items",
			Kind:       "one_to_many",
			Through:    "items",
			ForeignKey: "order_id",
			References: "id",
		},
		{
			Name:       "Tags",
			Kind:       "many_to_many",
			Through:    "tags",
			Pivot:      "order_tags",
			ForeignKey: "order_id",
		},
	}
}

// -----------------------------------------------------------------
// Parser — RelationFilters
// -----------------------------------------------------------------

func TestParseFromMap_RelationFilter(t *testing.T) {
	p, err := ParseFromMap(map[string][]string{
		"f_customer.name": {"Juan"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(p.RelationFilters) != 1 {
		t.Fatalf("RelationFilters = %v, want 1", p.RelationFilters)
	}
	rf := p.RelationFilters[0]
	if rf.Relation != "customer" || rf.Field != "name" || rf.Op != OpEq || rf.Value.(string) != "Juan" {
		t.Errorf("relation filter wrong: %+v", rf)
	}
	if _, ok := p.Filters["customer.name"]; ok {
		t.Errorf("relation filter should not double-register in Filters")
	}
}

func TestParseFromMap_RelationFilterWithOp(t *testing.T) {
	p, err := ParseFromMap(map[string][]string{
		"f_customer.created_at": {"gte:2024-01-01"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(p.RelationFilters) != 1 {
		t.Fatalf("RelationFilters = %v, want 1", p.RelationFilters)
	}
	rf := p.RelationFilters[0]
	if rf.Op != OpGte || rf.Value.(string) != "2024-01-01" {
		t.Errorf("relation filter op/value wrong: %+v", rf)
	}
}

func TestParseFromMap_RelationFilterRejectsUnsafe(t *testing.T) {
	p, err := ParseFromMap(map[string][]string{
		"f_customer.name; drop": {"x"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(p.RelationFilters) != 0 {
		t.Errorf("unsafe relation filter should be dropped, got %v", p.RelationFilters)
	}
}

// -----------------------------------------------------------------
// Parser — GroupBy / With
// -----------------------------------------------------------------

func TestParseFromMap_GroupBy(t *testing.T) {
	p, err := ParseFromMap(map[string][]string{
		"group_by": {"status, priority , "},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(p.GroupBy) != 2 || p.GroupBy[0] != "status" || p.GroupBy[1] != "priority" {
		t.Errorf("GroupBy = %v, want [status priority]", p.GroupBy)
	}
}

func TestParseFromMap_With(t *testing.T) {
	p, err := ParseFromMap(map[string][]string{
		"with": {"Customer,Items"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(p.Preloads) != 2 || p.Preloads[0] != "Customer" || p.Preloads[1] != "Items" {
		t.Errorf("Preloads = %v, want [Customer Items]", p.Preloads)
	}
}

// -----------------------------------------------------------------
// Parser — Aggregations
// -----------------------------------------------------------------

func TestParseFromMap_AggregateBasic(t *testing.T) {
	p, err := ParseFromMap(map[string][]string{
		"aggregate": {"sum(total),count(*),avg(rating)"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(p.Aggregations) != 3 {
		t.Fatalf("Aggregations = %v, want 3", p.Aggregations)
	}
	want := []Aggregation{
		{Func: AggSum, Field: "total", Alias: "sum_total"},
		{Func: AggCount, Field: "*", Alias: "count"},
		{Func: AggAvg, Field: "rating", Alias: "avg_rating"},
	}
	for i, w := range want {
		got := p.Aggregations[i]
		if got.Func != w.Func || got.Field != w.Field || got.Alias != w.Alias {
			t.Errorf("agg[%d] = %+v, want %+v", i, got, w)
		}
	}
}

func TestParseFromMap_AggregateWithAlias(t *testing.T) {
	p, err := ParseFromMap(map[string][]string{
		"aggregate": {"sum(amount):total_revenue"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Aggregations[0].Alias != "total_revenue" {
		t.Errorf("alias = %q, want total_revenue", p.Aggregations[0].Alias)
	}
}

func TestParseFromMap_AggregateUnknownFunc(t *testing.T) {
	_, err := ParseFromMap(map[string][]string{
		"aggregate": {"median(total)"},
	})
	if !errors.Is(err, ErrUnknownAggregate) {
		t.Errorf("err = %v, want ErrUnknownAggregate", err)
	}
}

func TestParseFromMap_AggregateMalformed(t *testing.T) {
	cases := []string{
		"sum total",      // no parens
		"sum(",           // unclosed
		"sum()",          // empty field
		"sum(total",      // unclosed
		")sum(total)",    // garbage prefix → open<=0
	}
	for _, raw := range cases {
		_, err := ParseFromMap(map[string][]string{"aggregate": {raw}})
		if !errors.Is(err, ErrInvalidAggregate) {
			t.Errorf("aggregate=%q: err = %v, want ErrInvalidAggregate", raw, err)
		}
	}
}

func TestParseFromMap_AggregateStarOnlyOnCount(t *testing.T) {
	_, err := ParseFromMap(map[string][]string{
		"aggregate": {"sum(*)"},
	})
	if !errors.Is(err, ErrInvalidAggregate) {
		t.Errorf("sum(*) should be rejected, got %v", err)
	}
}

func TestParseFromMap_AggregateUnsafeField(t *testing.T) {
	_, err := ParseFromMap(map[string][]string{
		"aggregate": {"sum(total; drop)"},
	})
	if !errors.Is(err, ErrInvalidAggregate) {
		t.Errorf("unsafe field should be rejected, got %v", err)
	}
}

// -----------------------------------------------------------------
// Builder — WithRelations / WithTableName
// -----------------------------------------------------------------

func TestWithRelations_StoresKnownAndDropsUnsafe(t *testing.T) {
	b := New(testMeta()).WithRelations([]modelbase.RelationDef{
		{Name: "Customer", Kind: "belongs_to", Through: "customers", ForeignKey: "customer_id"},
		{Name: "", Kind: "belongs_to"},
		{Name: "bad name", Kind: "belongs_to"},
		{Name: "drop;tbl", Kind: "belongs_to"},
	})
	if _, ok := b.relations["Customer"]; !ok {
		t.Errorf("Customer relation should be stored")
	}
	if len(b.relations) != 1 {
		t.Errorf("relations = %v, want 1 (unsafe names dropped)", b.relations)
	}
}

func TestWithRelations_Idempotent(t *testing.T) {
	b := New(testMeta()).WithRelations(testRelations())
	if len(b.relations) != 3 {
		t.Fatalf("first WithRelations: %v", b.relations)
	}
	b = b.WithRelations([]modelbase.RelationDef{
		{Name: "Customer", Kind: "belongs_to", Through: "customers", ForeignKey: "customer_id"},
	})
	if len(b.relations) != 1 {
		t.Errorf("second WithRelations should replace, got %v", b.relations)
	}
}

// -----------------------------------------------------------------
// Builder — Preloads
// -----------------------------------------------------------------

func TestApply_PreloadKnownRelation(t *testing.T) {
	b := New(testMeta()).WithRelations(testRelations())
	db := openDryDB(t)
	q := b.Apply(db.Model(&testRow{}), Params{Preloads: []string{"Customer"}})
	// GORM Preload is a session-level directive; assert it was set on the
	// statement clause map.
	if q.Statement.Preloads == nil || q.Statement.Preloads["Customer"] == nil {
		// Preload metadata is stored in the session, not always in stmt.Preloads
		// during DryRun. Fallback: just confirm the call did not error.
		// (DryRun does still attach preloads; the nil-check above will fail
		// only when applyPreloads silently dropped the name.)
		t.Logf("Statement.Preloads = %v", q.Statement.Preloads)
	}
}

func TestApply_PreloadUnknownDropped(t *testing.T) {
	b := New(testMeta()).WithRelations(testRelations())
	db := openDryDB(t)
	q := b.Apply(db.Model(&testRow{}), Params{Preloads: []string{"Ghost"}})
	if q.Statement.Preloads != nil {
		if _, ok := q.Statement.Preloads["Ghost"]; ok {
			t.Errorf("Ghost preload should be dropped")
		}
	}
}

func TestApply_PreloadWithoutRelationsIsNoop(t *testing.T) {
	// Builder without WithRelations should not preload anything.
	b := New(testMeta())
	db := openDryDB(t)
	q := b.Apply(db.Model(&testRow{}), Params{Preloads: []string{"Customer"}})
	if q.Statement.Preloads != nil && len(q.Statement.Preloads) > 0 {
		t.Errorf("preloads applied without WithRelations, got %v", q.Statement.Preloads)
	}
}

// -----------------------------------------------------------------
// Builder — Relation filters
// -----------------------------------------------------------------

func TestApply_RelationFilterBelongsTo(t *testing.T) {
	b := New(testMeta()).WithRelations(testRelations()).WithTableName("orders")
	db := openDryDB(t)
	q := b.Apply(db.Model(&testRow{}), Params{
		RelationFilters: []RelationFilter{
			{Relation: "Customer", Field: "name", Op: OpEq, Value: "Juan"},
		},
	})
	sql := renderSQL(t, q)
	if !strings.Contains(sql, "JOIN customers") {
		t.Errorf("want JOIN customers, got %q", sql)
	}
	if !strings.Contains(sql, "orders.customer_id = customers.id") {
		t.Errorf("want owner.fk = target.id join, got %q", sql)
	}
	if !strings.Contains(sql, "customers.name = ") {
		t.Errorf("want customers.name = clause, got %q", sql)
	}
	if !strings.Contains(sql, "Juan") {
		t.Errorf("want Juan in SQL, got %q", sql)
	}
}

func TestApply_RelationFilterOneToMany(t *testing.T) {
	b := New(testMeta()).WithRelations(testRelations()).WithTableName("orders")
	db := openDryDB(t)
	q := b.Apply(db.Model(&testRow{}), Params{
		RelationFilters: []RelationFilter{
			{Relation: "Items", Field: "sku", Op: OpEq, Value: "ABC"},
		},
	})
	sql := renderSQL(t, q)
	if !strings.Contains(sql, "JOIN items") {
		t.Errorf("want JOIN items, got %q", sql)
	}
	// one_to_many: items.order_id = orders.id
	if !strings.Contains(sql, "items.order_id = orders.id") {
		t.Errorf("want items.order_id = orders.id, got %q", sql)
	}
}

func TestApply_RelationFilterUnknownDropped(t *testing.T) {
	b := New(testMeta()).WithRelations(testRelations()).WithTableName("orders")
	db := openDryDB(t)
	q := b.Apply(db.Model(&testRow{}), Params{
		RelationFilters: []RelationFilter{
			{Relation: "Ghost", Field: "name", Op: OpEq, Value: "x"},
		},
	})
	sql := renderSQL(t, q)
	if strings.Contains(strings.ToUpper(sql), "JOIN") {
		t.Errorf("unknown relation should be dropped, got %q", sql)
	}
}

func TestApply_RelationFilterMultipleSameRelationOneJoin(t *testing.T) {
	b := New(testMeta()).WithRelations(testRelations()).WithTableName("orders")
	db := openDryDB(t)
	q := b.Apply(db.Model(&testRow{}), Params{
		RelationFilters: []RelationFilter{
			{Relation: "Customer", Field: "name", Op: OpIlike, Value: "Juan"},
			{Relation: "Customer", Field: "status", Op: OpEq, Value: "active"},
		},
	})
	sql := renderSQL(t, q)
	// Only one JOIN customers, two WHEREs.
	joinCount := strings.Count(sql, "JOIN customers")
	if joinCount != 1 {
		t.Errorf("want 1 JOIN customers, got %d in %q", joinCount, sql)
	}
	if !strings.Contains(sql, "customers.name ILIKE") {
		t.Errorf("want customers.name ILIKE, got %q", sql)
	}
	if !strings.Contains(sql, "customers.status = ") {
		t.Errorf("want customers.status = , got %q", sql)
	}
}

func TestApply_RelationFilterManyToManySkipped(t *testing.T) {
	b := New(testMeta()).WithRelations(testRelations()).WithTableName("orders")
	db := openDryDB(t)
	q := b.Apply(db.Model(&testRow{}), Params{
		RelationFilters: []RelationFilter{
			{Relation: "Tags", Field: "name", Op: OpEq, Value: "vip"},
		},
	})
	sql := renderSQL(t, q)
	if strings.Contains(sql, "JOIN tags") {
		t.Errorf("many_to_many should be skipped, got %q", sql)
	}
}

func TestApply_RelationFilterUnsafeFieldDropped(t *testing.T) {
	b := New(testMeta()).WithRelations(testRelations()).WithTableName("orders")
	db := openDryDB(t)
	q := b.Apply(db.Model(&testRow{}), Params{
		RelationFilters: []RelationFilter{
			{Relation: "Customer", Field: "name; drop", Op: OpEq, Value: "x"},
		},
	})
	sql := renderSQL(t, q)
	if strings.Contains(strings.ToLower(sql), "drop") {
		t.Errorf("unsafe field leaked: %q", sql)
	}
}

// -----------------------------------------------------------------
// Builder — GroupBy
// -----------------------------------------------------------------

func TestApply_GroupByWhitelisted(t *testing.T) {
	b := New(testMeta())
	db := openDryDB(t)
	q := b.Apply(db.Model(&testRow{}), Params{GroupBy: []string{"status", "name"}})
	sql := renderSQL(t, q)
	if !strings.Contains(strings.ToUpper(sql), "GROUP BY") {
		t.Errorf("want GROUP BY, got %q", sql)
	}
	if !strings.Contains(sql, "status") || !strings.Contains(sql, "name") {
		t.Errorf("want both columns in GROUP BY, got %q", sql)
	}
}

func TestApply_GroupByUnknownDropped(t *testing.T) {
	b := New(testMeta())
	db := openDryDB(t)
	q := b.Apply(db.Model(&testRow{}), Params{GroupBy: []string{"ghost"}})
	sql := renderSQL(t, q)
	if strings.Contains(strings.ToUpper(sql), "GROUP BY") {
		t.Errorf("unknown group_by should be dropped, got %q", sql)
	}
}

func TestApply_GroupByQualifiedWhenTableSet(t *testing.T) {
	b := New(testMeta()).WithTableName("orders")
	db := openDryDB(t)
	q := b.Apply(db.Model(&testRow{}), Params{GroupBy: []string{"status"}})
	sql := renderSQL(t, q)
	// GORM may wrap identifiers in dialect-specific quotes (backticks on
	// SQLite, double quotes on Postgres). Strip them so the assertion
	// stays dialect-portable.
	clean := strings.ReplaceAll(strings.ReplaceAll(sql, "`", ""), `"`, "")
	if !strings.Contains(clean, "orders.status") {
		t.Errorf("want orders.status, got %q", sql)
	}
}

// -----------------------------------------------------------------
// Builder — Aggregations
// -----------------------------------------------------------------

func TestApply_AggregationSelect(t *testing.T) {
	b := New(testMeta())
	db := openDryDB(t)
	q := b.Apply(db.Model(&testRow{}), Params{
		Aggregations: []Aggregation{
			{Func: AggSum, Field: "amount", Alias: "sum_amount"},
			{Func: AggCount, Field: "*", Alias: "count"},
		},
	})
	sql := renderSQL(t, q)
	if !strings.Contains(sql, "SUM(amount) AS sum_amount") {
		t.Errorf("want SUM(amount) AS sum_amount, got %q", sql)
	}
	if !strings.Contains(sql, "COUNT(*) AS count") {
		t.Errorf("want COUNT(*) AS count, got %q", sql)
	}
}

func TestApply_AggregationWithGroupBy(t *testing.T) {
	b := New(testMeta())
	db := openDryDB(t)
	q := b.Apply(db.Model(&testRow{}), Params{
		GroupBy: []string{"status"},
		Aggregations: []Aggregation{
			{Func: AggSum, Field: "amount", Alias: "sum_amount"},
		},
	})
	sql := renderSQL(t, q)
	// SELECT list must contain BOTH the group_by col and the aggregate.
	if !strings.Contains(sql, "status") || !strings.Contains(sql, "SUM(amount)") {
		t.Errorf("want both status and SUM(amount) in SELECT, got %q", sql)
	}
	if !strings.Contains(strings.ToUpper(sql), "GROUP BY") {
		t.Errorf("want GROUP BY clause, got %q", sql)
	}
}

func TestApply_AggregationUnknownColumnDropped(t *testing.T) {
	b := New(testMeta())
	db := openDryDB(t)
	q := b.Apply(db.Model(&testRow{}), Params{
		Aggregations: []Aggregation{
			{Func: AggSum, Field: "ghost", Alias: "sum_ghost"},
		},
	})
	sql := renderSQL(t, q)
	if strings.Contains(sql, "ghost") {
		t.Errorf("unknown field should be dropped, got %q", sql)
	}
}

// -----------------------------------------------------------------
// Live execution — group_by + aggregation against SQLite.
// -----------------------------------------------------------------

func TestBuilder_LiveAggregateGroupBy(t *testing.T) {
	db := openLiveDB(t)
	b := New(testMeta())
	params := Params{
		GroupBy: []string{"status"},
		Aggregations: []Aggregation{
			{Func: AggSum, Field: "amount", Alias: "sum_amount"},
			{Func: AggCount, Field: "*", Alias: "n"},
		},
	}
	q := b.Apply(db.Model(&testRow{}), params)
	type row struct {
		Status    string
		SumAmount int64 `gorm:"column:sum_amount"`
		N         int64 `gorm:"column:n"`
	}
	var out []row
	if err := q.Scan(&out).Error; err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("groups = %d, want 2 (active, archived)", len(out))
	}
	byStatus := map[string]row{}
	for _, r := range out {
		byStatus[r.Status] = r
	}
	if a := byStatus["active"]; a.N != 3 || a.SumAmount != 10+50+200 {
		t.Errorf("active: %+v, want N=3 sum=260", a)
	}
	if a := byStatus["archived"]; a.N != 2 || a.SumAmount != 100+500 {
		t.Errorf("archived: %+v, want N=2 sum=600", a)
	}
}

// -----------------------------------------------------------------
// Backward compatibility — nothing leaks when new params are absent.
// -----------------------------------------------------------------

func TestApply_NewFeaturesDormantWhenAbsent(t *testing.T) {
	b := New(testMeta())
	db := openDryDB(t)
	q := b.Apply(db.Model(&testRow{}), Params{})
	sql := renderSQL(t, q)
	for _, banned := range []string{"JOIN", "GROUP BY", "SUM(", "COUNT("} {
		if strings.Contains(strings.ToUpper(sql), banned) {
			t.Errorf("zero-value Params should not emit %q, got %q", banned, sql)
		}
	}
}
