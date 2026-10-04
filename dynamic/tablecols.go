package dynamic

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gorm.io/gorm"
)

// Live table-column introspection for the audit-column standard.
//
// The struct a host registers (BuildStructType) carries every standard audit
// column, but a table that already existed before the standard may lack some of
// them (ops' SyncDynamicTableSchema only guarantees deleted_at/created_by_id).
// Writing a column that is not there fails with SQLSTATE 42703, so the Service
// asks the database which columns the table really has and degrades: it stamps
// and writes only the audit columns that exist. EnsureAuditColumns remains the
// way to bring a table to the standard; once it ran the columns start being
// stamped.
//
// The answer is cached per (Service, table) for liveColumnsTTL and invalidated
// globally by InvalidateTableColumns (called by EnsureAuditColumns, SyncSchema
// and CreateTable). When the columns cannot be introspected (non-Postgres
// dialect, no information_schema, table not found) the answer is "unknown" and
// the Service keeps its original behaviour.

// liveColumnsTTL bounds how long a cached column set is trusted, so DDL applied
// by a host outside the kernel is picked up without a restart.
const liveColumnsTTL = time.Minute

// columnsGen is bumped by InvalidateTableColumns; entries from an older
// generation are refetched.
var columnsGen atomic.Uint64

// InvalidateTableColumns drops every cached table-column set (all Services).
// Hosts that alter a table's columns by their own DDL may call it to make the
// runtime see the change immediately; otherwise it is seen within the TTL.
func InvalidateTableColumns() { columnsGen.Add(1) }

type liveColsEntry struct {
	cols map[string]struct{} // nil = unknown
	at   time.Time
	gen  uint64
}

type liveColsCache struct {
	mu sync.RWMutex
	m  map[string]liveColsEntry
}

func (c *liveColsCache) get(table string) (liveColsEntry, bool) {
	c.mu.RLock()
	e, ok := c.m[table]
	c.mu.RUnlock()
	if !ok || e.gen != columnsGen.Load() || time.Since(e.at) > liveColumnsTTL {
		return liveColsEntry{}, false
	}
	return e, true
}

func (c *liveColsCache) put(table string, e liveColsEntry) {
	c.mu.Lock()
	if c.m == nil {
		c.m = map[string]liveColsEntry{}
	}
	c.m[table] = e
	c.mu.Unlock()
}

// liveColumns returns the column names the table really has, or nil when they
// cannot be determined.
func (s *Service) liveColumns(ctx context.Context, table string) map[string]struct{} {
	if table == "" {
		return nil
	}
	gen := columnsGen.Load()
	if e, ok := s.colCache.get(table); ok {
		return e.cols
	}
	var cols map[string]struct{}
	if s.db.Dialector != nil {
		switch s.db.Dialector.Name() {
		case "postgres":
			cols = introspectColumns(ctx, s.db, table)
		case "sqlite":
			cols = introspectSQLiteColumns(ctx, s.db, table)
		}
	}
	s.colCache.put(table, liveColsEntry{cols: cols, at: time.Now(), gen: gen})
	return cols
}

// introspectColumns reads information_schema for `table` ("schema.table" or a
// bare name resolved through the search_path). nil on any failure or when the
// table is not found.
func introspectColumns(ctx context.Context, db *gorm.DB, table string) map[string]struct{} {
	var rows []string
	var err error
	if i := strings.IndexByte(table, '.'); i > 0 {
		schema, name := strings.Trim(table[:i], `"`), strings.Trim(table[i+1:], `"`)
		err = db.WithContext(ctx).Raw(`SELECT column_name FROM information_schema.columns
			WHERE table_schema = ? AND table_name = ?`, schema, name).Scan(&rows).Error
	} else {
		err = db.WithContext(ctx).Raw(`SELECT column_name FROM information_schema.columns
			WHERE table_name = ? AND table_schema = ANY (current_schemas(false))`, strings.Trim(table, `"`)).Scan(&rows).Error
	}
	if err != nil || len(rows) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(rows))
	for _, r := range rows {
		out[r] = struct{}{}
	}
	return out
}

// absentAuditColumns lists the standard audit columns the model struct carries
// but the table lacks. Nil when unknown or when nothing is missing.
func (s *Service) absentAuditColumns(ctx context.Context, table string, instance any) []string {
	live := s.liveColumns(ctx, table)
	if live == nil {
		return nil
	}
	var absent []string
	have := structColumnSet(instance)
	for _, c := range auditInputKeys {
		if _, inStruct := have[c]; !inStruct {
			continue
		}
		if _, ok := live[c]; !ok {
			absent = append(absent, c)
		}
	}
	return absent
}

// hasLiveColumn reports whether the table has col; true when unknown.
func (s *Service) hasLiveColumn(ctx context.Context, table, col string) bool {
	live := s.liveColumns(ctx, table)
	if live == nil {
		return true
	}
	_, ok := live[col]
	return ok
}

// tableDB is db.Table(table) adapted to the table's real shape: when the model
// struct soft-deletes (gorm.DeletedAt) but the table has no deleted_at, GORM's
// `deleted_at IS NULL` filter would fail, so the handle is Unscoped (the table
// behaves as it did before the audit standard: reads see every row, Delete
// removes the row).
func (s *Service) tableDB(ctx context.Context, db *gorm.DB, table string, instance any) *gorm.DB {
	q := db.Table(table)
	if _, has := structColumnSet(instance)[ColDeletedAt]; has && !s.hasLiveColumn(ctx, table, ColDeletedAt) {
		q = q.Unscoped()
	}
	return q
}

// writeDB is tableDB for INSERT/UPDATE handles: it also omits the unsent
// nullable text fields (omitUnsentNullableText) and every audit column the table
// lacks, so the statement never names a column that is not there.
func (s *Service) writeDB(ctx context.Context, db *gorm.DB, table string, instance any, input map[string]any) *gorm.DB {
	q := s.tableDB(ctx, db, table, instance)
	omit := unsentNullableStringFields(ctx, q, table, instance, input)
	for _, c := range s.absentAuditColumns(ctx, table, instance) {
		omit = append(omit, c)
	}
	if len(omit) > 0 {
		return q.Omit(omit...)
	}
	return q
}

// introspectSQLiteColumns reads the table's columns through the pragma_table_info
// table-valued function; the table name is a bound parameter (never interpolated).
// nil on failure, for a qualified name, or when the table does not exist.
func introspectSQLiteColumns(ctx context.Context, db *gorm.DB, table string) map[string]struct{} {
	table = strings.Trim(table, `"`+"`")
	if table == "" || strings.ContainsAny(table, ".\x00") {
		return nil
	}
	var rows []string
	if err := db.WithContext(ctx).Raw(`SELECT name FROM pragma_table_info(?)`, table).Scan(&rows).Error; err != nil || len(rows) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(rows))
	for _, r := range rows {
		out[r] = struct{}{}
	}
	return out
}
