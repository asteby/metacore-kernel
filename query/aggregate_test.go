package query

import (
	"context"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// aggRow is the fixture model the aggregation tests run over. It carries the
// two framework columns the engine always filters on (organization_id,
// deleted_at) plus a numeric measure, a grouping dimension and a timestamp.
type aggRow struct {
	ID             uint    `gorm:"primaryKey"`
	OrganizationID string  `gorm:"column:organization_id"`
	DeletedAt      *string `gorm:"column:deleted_at"`
	WarehouseID    string  `gorm:"column:warehouse_id"`
	Status         string  `gorm:"column:status"`
	IsActive       bool    `gorm:"column:is_active"`
	Quantity       float64 `gorm:"column:quantity"`
	CreatedAt      string  `gorm:"column:created_at"`
}

func (aggRow) TableName() string { return "agg_rows" }

const orgA = "org-a"
const orgB = "org-b"

func openAggDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(&aggRow{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	del := "2024-01-01"
	rows := []aggRow{
		// org A, live
		{OrganizationID: orgA, WarehouseID: "w1", Status: "ok", IsActive: true, Quantity: 10, CreatedAt: "2024-01-15"},
		{OrganizationID: orgA, WarehouseID: "w1", Status: "ok", IsActive: true, Quantity: 5, CreatedAt: "2024-02-10"},
		{OrganizationID: orgA, WarehouseID: "w2", Status: "ok", IsActive: false, Quantity: 20, CreatedAt: "2024-02-20"},
		{OrganizationID: orgA, WarehouseID: "w2", Status: "low", IsActive: true, Quantity: 3, CreatedAt: "2024-03-05"},
		// org A, soft-deleted — must be excluded everywhere
		{OrganizationID: orgA, WarehouseID: "w1", Status: "ok", IsActive: true, Quantity: 999, CreatedAt: "2024-03-09", DeletedAt: &del},
		// org B — must be excluded by org scope
		{OrganizationID: orgB, WarehouseID: "w9", Status: "ok", IsActive: true, Quantity: 777, CreatedAt: "2024-01-01"},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	return db
}

func TestAggregate_ScalarSum_OrgScopedSoftDelete(t *testing.T) {
	db := openAggDB(t)
	res, err := Aggregate(context.Background(), db, AggregateSpec{
		Table:     "agg_rows",
		OrgID:     orgA,
		Aggregate: "sum",
		Field:     "quantity",
	})
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if res.Scalar == nil {
		t.Fatalf("want scalar, got %+v", res)
	}
	// 10+5+20+3 = 38 (the 999 deleted row and the 777 org-B row excluded).
	if *res.Scalar != 38 {
		t.Errorf("scalar sum = %v, want 38", *res.Scalar)
	}
}

func TestAggregate_ScalarCount(t *testing.T) {
	db := openAggDB(t)
	res, err := Aggregate(context.Background(), db, AggregateSpec{
		Table:     "agg_rows",
		OrgID:     orgA,
		Aggregate: "count",
	})
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if res.Scalar == nil || *res.Scalar != 4 {
		t.Errorf("count = %+v, want scalar 4", res)
	}
}

func TestAggregate_ScalarCount_WhereEqAndOp(t *testing.T) {
	db := openAggDB(t)
	// count where is_active = true AND quantity < 5  → only the qty=3 active row.
	res, err := Aggregate(context.Background(), db, AggregateSpec{
		Table:     "agg_rows",
		OrgID:     orgA,
		Aggregate: "count",
		Where: map[string]any{
			"is_active": true,
			"quantity":  map[string]any{"lt": 5.0},
		},
	})
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if res.Scalar == nil || *res.Scalar != 1 {
		t.Errorf("count = %+v, want 1", res)
	}
}

func TestAggregate_ScalarAvgMinMax(t *testing.T) {
	db := openAggDB(t)
	cases := []struct {
		fn   string
		want float64
	}{
		{"avg", 9.5}, // (10+5+20+3)/4
		{"min", 3},
		{"max", 20},
	}
	for _, c := range cases {
		res, err := Aggregate(context.Background(), db, AggregateSpec{
			Table: "agg_rows", OrgID: orgA, Aggregate: c.fn, Field: "quantity",
		})
		if err != nil {
			t.Fatalf("%s: %v", c.fn, err)
		}
		if res.Scalar == nil || *res.Scalar != c.want {
			t.Errorf("%s = %+v, want %v", c.fn, res, c.want)
		}
	}
}

func TestAggregate_GroupBy_OrderedAndClamped(t *testing.T) {
	db := openAggDB(t)
	res, err := Aggregate(context.Background(), db, AggregateSpec{
		Table:     "agg_rows",
		OrgID:     orgA,
		Aggregate: "sum",
		Field:     "quantity",
		GroupBy:   "warehouse_id",
		Order:     "desc",
	})
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if len(res.Buckets) != 2 {
		t.Fatalf("buckets = %+v, want 2", res.Buckets)
	}
	// w2 = 20+3 = 23, w1 = 10+5 = 15. desc → w2 first.
	if res.Buckets[0].Key != "w2" || res.Buckets[0].Value != 23 {
		t.Errorf("bucket[0] = %+v, want w2=23", res.Buckets[0])
	}
	if res.Buckets[1].Key != "w1" || res.Buckets[1].Value != 15 {
		t.Errorf("bucket[1] = %+v, want w1=15", res.Buckets[1])
	}
	// Label defaults to key when no resolver.
	if res.Buckets[0].Label != "w2" {
		t.Errorf("label = %q, want w2", res.Buckets[0].Label)
	}
}

func TestAggregate_GroupBy_AscAndLimit(t *testing.T) {
	db := openAggDB(t)
	res, err := Aggregate(context.Background(), db, AggregateSpec{
		Table: "agg_rows", OrgID: orgA, Aggregate: "sum", Field: "quantity",
		GroupBy: "warehouse_id", Order: "asc", Limit: 1,
	})
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if len(res.Buckets) != 1 {
		t.Fatalf("buckets = %+v, want 1 (limit)", res.Buckets)
	}
	// asc → smallest first = w1 (15).
	if res.Buckets[0].Key != "w1" {
		t.Errorf("bucket = %+v, want w1", res.Buckets[0])
	}
}

func TestAggregate_GroupBy_LabelResolver(t *testing.T) {
	db := openAggDB(t)
	res, err := Aggregate(context.Background(), db, AggregateSpec{
		Table: "agg_rows", OrgID: orgA, Aggregate: "count",
		GroupBy: "warehouse_id",
		LabelResolver: func(_ context.Context, keys []string) map[string]string {
			return map[string]string{"w1": "Bodega Norte", "w2": "Bodega Sur"}
		},
	})
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	got := map[string]string{}
	for _, b := range res.Buckets {
		got[b.Key] = b.Label
	}
	if got["w1"] != "Bodega Norte" || got["w2"] != "Bodega Sur" {
		t.Errorf("labels = %+v, want resolved names", got)
	}
}

func TestAggregate_TimeSeries_BackfillsEmptyBuckets(t *testing.T) {
	db := openAggDB(t)
	// Reference "now" = 2024-03-31 so last_12_months covers Apr-2023..Mar-2024,
	// 12 monthly buckets. org A live rows fall in Jan/Feb/Mar 2024.
	now := time.Date(2024, 3, 31, 12, 0, 0, 0, time.UTC)
	res, err := Aggregate(context.Background(), db, AggregateSpec{
		Table: "agg_rows", OrgID: orgA, Aggregate: "count",
		DateField: "created_at", Interval: IntervalMonth, Range: RangeLast12Months,
		Now: now,
	})
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if len(res.Buckets) != 12 {
		t.Fatalf("buckets = %d, want 12 (gap-free year)", len(res.Buckets))
	}
	// Buckets are chronological; verify the populated months and a zero-fill.
	byKey := map[string]float64{}
	for _, b := range res.Buckets {
		byKey[b.Key] = b.Value
	}
	if byKey["2024-01-01"] != 1 { // one Jan row (qty 10)
		t.Errorf("2024-01 = %v, want 1", byKey["2024-01-01"])
	}
	if byKey["2024-02-01"] != 2 { // two Feb rows
		t.Errorf("2024-02 = %v, want 2", byKey["2024-02-01"])
	}
	if byKey["2024-03-01"] != 1 { // one March live row (the 999 is deleted)
		t.Errorf("2024-03 = %v, want 1", byKey["2024-03-01"])
	}
	if _, ok := byKey["2023-06-01"]; !ok || byKey["2023-06-01"] != 0 {
		t.Errorf("2023-06 should be a zero-filled bucket, got %v ok=%v", byKey["2023-06-01"], ok)
	}
}

func TestAggregate_TimeSeries_Daily(t *testing.T) {
	db := openAggDB(t)
	now := time.Date(2024, 3, 9, 12, 0, 0, 0, time.UTC)
	res, err := Aggregate(context.Background(), db, AggregateSpec{
		Table: "agg_rows", OrgID: orgA, Aggregate: "count",
		DateField: "created_at", Interval: IntervalDay, Range: RangeLast7Days,
		Now: now,
	})
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if len(res.Buckets) != 7 {
		t.Fatalf("buckets = %d, want 7 days", len(res.Buckets))
	}
	byKey := map[string]float64{}
	for _, b := range res.Buckets {
		byKey[b.Key] = b.Value
	}
	// 2024-03-05 has the qty=3 live row; the 2024-03-09 row is soft-deleted.
	if byKey["2024-03-05"] != 1 {
		t.Errorf("2024-03-05 = %v, want 1", byKey["2024-03-05"])
	}
	if byKey["2024-03-09"] != 0 {
		t.Errorf("2024-03-09 = %v, want 0 (deleted row excluded)", byKey["2024-03-09"])
	}
}

func TestAggregate_InvalidIdentifiersRejected(t *testing.T) {
	db := openAggDB(t)
	cases := []AggregateSpec{
		{Table: "agg; DROP TABLE x", OrgID: orgA, Aggregate: "count"},
		{Table: "agg_rows", OrgID: orgA, Aggregate: "sum", Field: "1bad"},
		{Table: "agg_rows", OrgID: orgA, Aggregate: "count", GroupBy: "bad col"},
		{Table: "agg_rows", OrgID: orgA, Aggregate: "count", DateField: "x;y", Interval: IntervalDay, Range: RangeLast7Days},
		{Table: "agg_rows", OrgID: orgA, Aggregate: "median", Field: "quantity"},
		{Table: "agg_rows", OrgID: orgA, Aggregate: "sum"}, // sum with no field
	}
	for i, spec := range cases {
		if _, err := Aggregate(context.Background(), db, spec); err == nil {
			t.Errorf("case %d: want error for %+v", i, spec)
		}
	}
}

func TestClampLimit(t *testing.T) {
	cases := map[int]int{0: 24, -5: 24, 1: 1, 8: 8, 24: 24, 99: 24}
	for in, want := range cases {
		if got := clampLimit(in); got != want {
			t.Errorf("clampLimit(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestResolveRange_PreviousPeriod(t *testing.T) {
	now := time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC)
	start, end, ok := resolveRange(RangeThisMonth, now)
	if !ok {
		t.Fatal("this_month should be bounded")
	}
	if !start.Equal(time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("this_month start = %v", start)
	}
	if !end.Equal(time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("this_month end = %v", end)
	}
	ps, pe, pok := PreviousRange(RangeThisMonth, now)
	if !pok {
		t.Fatal("previous of this_month should be bounded")
	}
	// previous window has the same duration, immediately before.
	if !pe.Equal(start) {
		t.Errorf("previous end %v should equal current start %v", pe, start)
	}
	if pe.Sub(ps) != end.Sub(start) {
		t.Errorf("previous duration %v != current %v", pe.Sub(ps), end.Sub(start))
	}
	// all → unbounded, no previous.
	if _, _, ok := PreviousRange(RangeAll, now); ok {
		t.Error("all should have no previous period")
	}
}
