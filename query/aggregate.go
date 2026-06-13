package query

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
)

// TimeRange is a named relative window the aggregation engine resolves into a
// concrete [Start, End) interval at execution time. The empty value (the zero
// TimeRange{}) means "all time" — no date bounds are applied.
//
// The supported tokens mirror the v3 dashboard contract's query.range enum:
//
//	last_7_days | last_30_days | last_12_months | this_month | this_year |
//	this_day | all
//
// Resolve interprets them against a reference "now" (always the caller's
// clock) so tests are deterministic.
type TimeRange string

// TimeRange tokens. These are the stable wire values a host threads from
// query.range. RangeAll (and the empty string) disable date bounds entirely.
const (
	RangeLast7Days    TimeRange = "last_7_days"
	RangeLast30Days   TimeRange = "last_30_days"
	RangeLast12Months TimeRange = "last_12_months"
	RangeThisMonth    TimeRange = "this_month"
	RangeThisYear     TimeRange = "this_year"
	RangeThisDay      TimeRange = "this_day"
	RangeAll          TimeRange = "all"
)

// Interval is the time-series bucket width for a date_field aggregation.
type Interval string

// Interval tokens, mirroring the v3 query.interval enum.
const (
	IntervalDay   Interval = "day"
	IntervalWeek  Interval = "week"
	IntervalMonth Interval = "month"
)

// AggregateSpec is the host-side, org-scoped aggregation request the dashboard
// engine executes. The host resolves a v3 widget's WidgetQuery into this shape
// (model key → logical Table) and hands it to Aggregate. Every string field
// that crosses into raw SQL (Table, Field, GroupBy, DateField and every Where
// key) is validated against the safe-identifier rule before use; an unsafe or
// unknown identifier is dropped/rejected, never interpolated.
type AggregateSpec struct {
	// Table is the logical table the host resolved from the model key. Required.
	Table string
	// OrgID scopes every query: the engine always adds `organization_id = ?`.
	// Required (an empty OrgID still filters, matching no rows for a real org).
	OrgID string
	// Aggregate is the aggregate function: count|sum|avg|min|max. Empty means
	// count. count ignores Field.
	Aggregate string
	// Field is the column the aggregate runs over. Required unless count.
	Field string
	// Where carries the same operators as the list builder
	// (eq/neq/gt/gte/lt/lte/contains). Values are Go scalars (bool, float64,
	// string) — the host decodes the manifest's json.RawMessage map into this.
	// A bare scalar value means equality.
	Where map[string]any
	// GroupBy buckets the result by a column. Mutually informative with
	// DateField: if both are set DateField (the time series) wins.
	GroupBy string
	// LabelField names a column whose value labels each GroupBy bucket. When it
	// is a foreign key the host may post-resolve readable names; the engine
	// itself returns the raw key as the label and leaves enrichment to the
	// optional LabelResolver.
	LabelField string
	// DateField drives a time series bucketed by Interval within Range.
	DateField string
	// Interval is the time-series bucket width: day|week|month.
	Interval Interval
	// Range is the time window applied to DateField (and to the date filter on a
	// scalar/group_by query when set). Empty / RangeAll disables date bounds.
	Range TimeRange
	// Order sorts GroupBy buckets by value: asc|desc. Ignored for a time series
	// (always chronological). Default desc.
	Order string
	// Limit caps the number of GroupBy buckets returned. Clamped to 1..24.
	// Ignored for a time series (every bucket in Range is returned).
	Limit int

	// LabelResolver, when non-nil, maps a bucket key (the GroupBy value) to a
	// human-readable label — used to turn a foreign-key id into a name. It is
	// called once per distinct bucket key; returning ("", false) leaves the
	// label equal to the key. Optional; the engine works without it.
	LabelResolver func(ctx context.Context, keys []string) map[string]string

	// Now, when non-zero, overrides the reference clock used to resolve Range.
	// Tests set it for determinism; production leaves it zero (time.Now()).
	Now time.Time
}

// AggregateBucket is one row of a grouped/time-series aggregate.
type AggregateBucket struct {
	// Key is the group_by value or the time bucket (ISO date for a series).
	Key string `json:"key"`
	// Label is the human-readable bucket label (ref-resolved when applicable),
	// falling back to Key.
	Label string `json:"label"`
	// Value is the aggregate for the bucket.
	Value float64 `json:"value"`
}

// AggregateResult is what Aggregate returns. Exactly one of Scalar / Buckets is
// populated: Scalar for a plain aggregate (no group_by, no date_field), Buckets
// for a grouped or time-series aggregate.
type AggregateResult struct {
	// Scalar is the single aggregate value when there is no group_by/date_field.
	Scalar *float64 `json:"scalar,omitempty"`
	// Buckets is the per-group / per-interval series otherwise.
	Buckets []AggregateBucket `json:"buckets,omitempty"`
}

// aggMinLimit / aggMaxLimit bound the number of buckets a grouped aggregate
// returns (contract §2: clamp 1..24).
const (
	aggMinLimit = 1
	aggMaxLimit = 24
)

// Aggregate runs the host-side dashboard aggregation described by spec against
// db. It is always org-scoped (`organization_id = ?`) and soft-delete aware
// (`deleted_at IS NULL`).
//
//   - No GroupBy and no DateField → a scalar aggregate (Result.Scalar).
//   - DateField set → a time series bucketed by Interval within Range, with
//     empty buckets back-filled to 0 (Result.Buckets, chronological).
//   - GroupBy set (and no DateField) → one bucket per distinct value, ordered
//     by value (Order), clamped to Limit (Result.Buckets).
//
// count uses COUNT(*); sum/avg/min/max run over Field. Every identifier is
// validated with the package's safe-ident rule before it reaches raw SQL.
func Aggregate(ctx context.Context, db *gorm.DB, spec AggregateSpec) (AggregateResult, error) {
	var zero AggregateResult
	if db == nil {
		return zero, fmt.Errorf("query.Aggregate: nil db")
	}
	if !isSafeIdent(spec.Table) {
		return zero, fmt.Errorf("query.Aggregate: invalid table %q", spec.Table)
	}

	fn := strings.ToLower(strings.TrimSpace(spec.Aggregate))
	if fn == "" {
		fn = "count"
	}
	if _, ok := supportedAggregates[AggregateFunc(fn)]; !ok {
		return zero, fmt.Errorf("query.Aggregate: unsupported aggregate %q", spec.Aggregate)
	}
	if fn != "count" {
		if spec.Field == "" {
			return zero, fmt.Errorf("query.Aggregate: aggregate %q requires a field", fn)
		}
		if !isSafeIdent(spec.Field) {
			return zero, fmt.Errorf("query.Aggregate: invalid field %q", spec.Field)
		}
	}

	switch {
	case spec.DateField != "":
		return aggregateSeries(ctx, db, spec, fn)
	case spec.GroupBy != "":
		return aggregateGroup(ctx, db, spec, fn)
	default:
		return aggregateScalar(ctx, db, spec, fn)
	}
}

// aggExpr builds the SELECT aggregate expression for fn. count → COUNT(*);
// everything else → FN(field). field is assumed already validated.
func aggExpr(fn, field string) string {
	if fn == "count" {
		return "COUNT(*)"
	}
	return fmt.Sprintf("%s(%s)", strings.ToUpper(fn), field)
}

// baseQuery starts a *gorm.DB bound to the spec's table with the always-on
// org-scope + soft-delete filters and the spec's Where clauses applied. It does
// NOT apply date bounds (callers add those where they belong).
func baseQuery(db *gorm.DB, spec AggregateSpec) *gorm.DB {
	q := db.Table(spec.Table).
		Where("organization_id = ?", spec.OrgID).
		Where("deleted_at IS NULL")
	return applyWhereMap(q, spec.Where)
}

// applyWhereMap layers the spec's Where map onto q, reusing applyOneFilter so
// the operator semantics match the list builder exactly. A bare scalar value
// means equality; a {op: value} object selects the operator. Unsafe column
// names and unsupported operators are dropped silently (the kernel's "garbage
// in → safe degrade" policy).
func applyWhereMap(q *gorm.DB, where map[string]any) *gorm.DB {
	if len(where) == 0 {
		return q
	}
	// Stable column order so emitted SQL is deterministic (tests, caching).
	cols := make([]string, 0, len(where))
	for col := range where {
		cols = append(cols, col)
	}
	sort.Strings(cols)
	for _, col := range cols {
		if !isSafeIdent(col) {
			continue
		}
		v := where[col]
		// Booleans bind natively so the clause matches across dialects (sqlite
		// stores 0/1; Postgres coerces a Go bool to its boolean type). Routing a
		// bool through the string eq path would emit `col = 'true'`, which sqlite
		// never matches.
		if b, ok := v.(bool); ok {
			q = q.Where(fmt.Sprintf("%s = ?", col), b)
			continue
		}
		q = applyOneFilter(q, col, whereFilter(v))
	}
	return q
}

// whereFilter turns one Where value into a query.Filter. A map (the
// {op: value} form) selects the operator; anything else is treated as an
// equality value. Operator/value typing matches applyOneFilter's expectations:
// eq/neq/contains carry strings; gt/gte/lt/lte carry float64.
func whereFilter(v any) Filter {
	if m, ok := v.(map[string]any); ok && len(m) == 1 {
		for op, val := range m {
			switch strings.ToLower(op) {
			case "eq":
				return Filter{Op: OpEq, Value: toFilterString(val)}
			case "neq":
				return Filter{Op: OpNeq, Value: toFilterString(val)}
			case "contains":
				return Filter{Op: OpIlike, Value: toFilterString(val)}
			case "gt":
				return Filter{Op: OpGt, Value: toFloat(val)}
			case "gte":
				return Filter{Op: OpNumGte, Value: toFloat(val)}
			case "lt":
				return Filter{Op: OpLt, Value: toFloat(val)}
			case "lte":
				return Filter{Op: OpNumLte, Value: toFloat(val)}
			}
		}
	}
	// Bare scalar → equality. Booleans and numbers stringify so the eq clause
	// binds a comparable value (Postgres coerces '1'/'true' against the column
	// type; for booleans we emit the literal Postgres understands).
	return Filter{Op: OpEq, Value: toFilterString(v)}
}

// toFilterString renders a Where scalar as the string applyOneFilter's eq/neq
// path expects. Booleans become "true"/"false"; integers stay integral (no
// "5.000000"); everything else uses the default Go formatting.
func toFilterString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%v", t)
	case int:
		return fmt.Sprintf("%d", t)
	case int64:
		return fmt.Sprintf("%d", t)
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", t)
	}
}

// toFloat coerces a numeric Where value to float64 for the comparison
// operators. Non-numeric values yield 0 (the clause still binds, degrading
// safely rather than panicking).
func toFloat(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case int64:
		return float64(t)
	default:
		return 0
	}
}

// aggregateScalar runs a single-value aggregate (no group_by, no series). When
// Range is set and a DateField-less scalar nonetheless wants a window, the host
// supplies the date filter via Where; the scalar path itself applies no date
// bounds.
func aggregateScalar(ctx context.Context, db *gorm.DB, spec AggregateSpec, fn string) (AggregateResult, error) {
	q := baseQuery(db, spec).WithContext(ctx).Select(aggExpr(fn, spec.Field) + " AS value")
	var out struct{ Value *float64 }
	if err := q.Scan(&out).Error; err != nil {
		return AggregateResult{}, fmt.Errorf("query.Aggregate scalar: %w", err)
	}
	v := 0.0
	if out.Value != nil {
		v = *out.Value
	}
	return AggregateResult{Scalar: &v}, nil
}

// aggregateGroup runs a grouped aggregate: one bucket per distinct GroupBy
// value, ordered by value, clamped to Limit. The LabelField (if a safe ident
// and distinct from GroupBy) is selected as the label; otherwise the label
// defaults to the key, optionally enriched by spec.LabelResolver.
func aggregateGroup(ctx context.Context, db *gorm.DB, spec AggregateSpec, fn string) (AggregateResult, error) {
	if !isSafeIdent(spec.GroupBy) {
		return AggregateResult{}, fmt.Errorf("query.Aggregate: invalid group_by %q", spec.GroupBy)
	}
	limit := clampLimit(spec.Limit)
	order := "DESC"
	if strings.EqualFold(spec.Order, "asc") {
		order = "ASC"
	}

	expr := aggExpr(fn, spec.Field)
	q := baseQuery(db, spec).WithContext(ctx).
		Select(fmt.Sprintf("%s AS bucket_key, %s AS value", spec.GroupBy, expr)).
		Group(spec.GroupBy).
		Order(fmt.Sprintf("value %s", order)).
		Limit(limit)

	type row struct {
		BucketKey *string
		Value     float64
	}
	var rows []row
	if err := q.Scan(&rows).Error; err != nil {
		return AggregateResult{}, fmt.Errorf("query.Aggregate group: %w", err)
	}

	buckets := make([]AggregateBucket, 0, len(rows))
	keys := make([]string, 0, len(rows))
	for _, r := range rows {
		k := ""
		if r.BucketKey != nil {
			k = *r.BucketKey
		}
		buckets = append(buckets, AggregateBucket{Key: k, Label: k, Value: r.Value})
		keys = append(keys, k)
	}

	// Optional readable-name resolution for foreign-key group_by columns.
	if spec.LabelResolver != nil && len(keys) > 0 {
		if labels := spec.LabelResolver(ctx, keys); len(labels) > 0 {
			for i := range buckets {
				if lbl, ok := labels[buckets[i].Key]; ok && lbl != "" {
					buckets[i].Label = lbl
				}
			}
		}
	}
	return AggregateResult{Buckets: buckets}, nil
}

// clampLimit applies the contract's 1..24 bucket clamp. A zero/negative limit
// means "use the max".
func clampLimit(n int) int {
	if n <= 0 {
		return aggMaxLimit
	}
	if n < aggMinLimit {
		return aggMinLimit
	}
	if n > aggMaxLimit {
		return aggMaxLimit
	}
	return n
}

// aggregateSeries runs a time-series aggregate: date_trunc(Interval, DateField)
// within Range, back-filling every empty bucket with 0 so the SDK draws a
// continuous line/area. Buckets are returned chronologically.
func aggregateSeries(ctx context.Context, db *gorm.DB, spec AggregateSpec, fn string) (AggregateResult, error) {
	if !isSafeIdent(spec.DateField) {
		return AggregateResult{}, fmt.Errorf("query.Aggregate: invalid date_field %q", spec.DateField)
	}
	interval := normalizeInterval(spec.Interval)

	now := spec.Now
	if now.IsZero() {
		now = time.Now()
	}
	start, end, bounded := resolveRange(spec.Range, now)

	q := baseQuery(db, spec).WithContext(ctx)
	if bounded {
		q = q.Where(fmt.Sprintf("%s >= ? AND %s < ?", spec.DateField, spec.DateField), start, end)
	}

	truncExpr := dateTruncExpr(db, interval, spec.DateField)
	expr := aggExpr(fn, spec.Field)
	q = q.Select(fmt.Sprintf("%s AS bucket_key, %s AS value", truncExpr, expr)).
		Group(truncExpr).
		Order("bucket_key ASC")

	// bucket_key is scanned as a string for dialect portability: sqlite's
	// date()/strftime returns text, and the Postgres date_trunc timestamp is
	// likewise readable as text by the driver. We parse the leading YYYY-MM-DD
	// and re-floor it to the interval so realized rows align with the synthetic
	// gap-free sequence.
	type row struct {
		BucketKey *string
		Value     float64
	}
	var rows []row
	if err := q.Scan(&rows).Error; err != nil {
		return AggregateResult{}, fmt.Errorf("query.Aggregate series: %w", err)
	}

	have := make(map[string]float64, len(rows))
	for _, r := range rows {
		if r.BucketKey == nil || *r.BucketKey == "" {
			continue
		}
		t, ok := parseBucketKey(*r.BucketKey)
		if !ok {
			continue
		}
		have[truncateTime(t, interval).Format("2006-01-02")] = r.Value
	}

	// Build the full, gap-free sequence of buckets across Range.
	var buckets []AggregateBucket
	if bounded {
		for cur := truncateTime(start, interval); cur.Before(end); cur = stepInterval(cur, interval) {
			key := cur.Format("2006-01-02")
			buckets = append(buckets, AggregateBucket{Key: key, Label: key, Value: have[key]})
		}
	} else {
		// Unbounded ("all"): we cannot enumerate every bucket without a floor,
		// so we return exactly the realized buckets in chronological order
		// (no synthetic zero-fill — there is no window to fill).
		keys := make([]string, 0, len(have))
		for k := range have {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			buckets = append(buckets, AggregateBucket{Key: k, Label: k, Value: have[k]})
		}
	}
	return AggregateResult{Buckets: buckets}, nil
}

// parseBucketKey parses the leading date out of a bucket key string. It accepts
// both a bare "2006-01-02" (sqlite date()) and a fuller timestamp prefix
// ("2006-01-02T15:04:05Z" / "2006-01-02 15:04:05+00") that Postgres date_trunc
// renders, reading only the first 10 characters as the date.
func parseBucketKey(s string) (time.Time, bool) {
	if len(s) < 10 {
		return time.Time{}, false
	}
	t, err := time.Parse("2006-01-02", s[:10])
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// normalizeInterval defaults a blank/unknown interval to month.
func normalizeInterval(in Interval) Interval {
	switch in {
	case IntervalDay, IntervalWeek, IntervalMonth:
		return in
	default:
		return IntervalMonth
	}
}

// dateTruncExpr renders the dialect-appropriate date-bucketing SQL fragment.
// Postgres uses native date_trunc; sqlite (tests) emulates it with strftime so
// the engine is exercisable without a Postgres instance. interval is one of the
// normalized Interval constants.
func dateTruncExpr(db *gorm.DB, interval Interval, col string) string {
	if db != nil && db.Dialector != nil {
		switch db.Dialector.Name() {
		case "sqlite", "sqlite3":
			switch interval {
			case IntervalDay:
				return fmt.Sprintf("date(%s)", col)
			case IntervalWeek:
				// Snap to the Monday of the week (strftime %%W is week-of-year;
				// emulate week-start via day arithmetic on the weekday).
				return fmt.Sprintf("date(%s, 'weekday 1', '-7 days')", col)
			case IntervalMonth:
				return fmt.Sprintf("date(%s, 'start of month')", col)
			}
		}
	}
	// Postgres (and the default): date_trunc returns a timestamp at the bucket
	// floor. The literal interval token is a fixed string, never user input.
	return fmt.Sprintf("date_trunc('%s', %s)", string(interval), col)
}

// truncateTime floors t to the start of its interval bucket (UTC), matching the
// SQL date_trunc so realized rows line up with the synthetic sequence.
func truncateTime(t time.Time, interval Interval) time.Time {
	t = t.UTC()
	switch interval {
	case IntervalDay:
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	case IntervalWeek:
		// ISO week start = Monday. Go's Weekday() has Sunday=0.
		wd := int(t.Weekday())
		if wd == 0 {
			wd = 7
		}
		d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
		return d.AddDate(0, 0, -(wd - 1))
	case IntervalMonth:
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	default:
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
}

// stepInterval advances cur by exactly one bucket of interval.
func stepInterval(cur time.Time, interval Interval) time.Time {
	switch interval {
	case IntervalDay:
		return cur.AddDate(0, 0, 1)
	case IntervalWeek:
		return cur.AddDate(0, 0, 7)
	case IntervalMonth:
		return cur.AddDate(0, 1, 0)
	default:
		return cur.AddDate(0, 1, 0)
	}
}

// resolveRange turns a TimeRange token into a concrete [start, end) interval
// relative to now (UTC). bounded is false for RangeAll / empty / unknown, in
// which case start/end are ignored and no date filter is applied.
func resolveRange(r TimeRange, now time.Time) (start, end time.Time, bounded bool) {
	now = now.UTC()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	switch r {
	case RangeLast7Days:
		return dayStart.AddDate(0, 0, -6), dayStart.AddDate(0, 0, 1), true
	case RangeLast30Days:
		return dayStart.AddDate(0, 0, -29), dayStart.AddDate(0, 0, 1), true
	case RangeLast12Months:
		monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		return monthStart.AddDate(0, -11, 0), monthStart.AddDate(0, 1, 0), true
	case RangeThisMonth:
		monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		return monthStart, monthStart.AddDate(0, 1, 0), true
	case RangeThisYear:
		yearStart := time.Date(now.Year(), 1, 1, 0, 0, 0, 0, time.UTC)
		return yearStart, yearStart.AddDate(1, 0, 0), true
	case RangeThisDay:
		return dayStart, dayStart.AddDate(0, 0, 1), true
	case RangeAll, "":
		return time.Time{}, time.Time{}, false
	default:
		return time.Time{}, time.Time{}, false
	}
}

// PreviousRange returns the window immediately preceding r (same duration),
// used by the compare:{to:"previous_period"} delta. bounded is false when r has
// no bounds (RangeAll / unknown), in which case there is nothing to compare to.
func PreviousRange(r TimeRange, now time.Time) (start, end time.Time, bounded bool) {
	curStart, curEnd, ok := resolveRange(r, now)
	if !ok {
		return time.Time{}, time.Time{}, false
	}
	d := curEnd.Sub(curStart)
	return curStart.Add(-d), curStart, true
}
