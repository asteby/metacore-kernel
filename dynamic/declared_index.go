package dynamic

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/manifest"
)

// Declared indexes are the COMPOSITE, NON-UNIQUE indexes without a `where`
// that a manifest declares in models[].indices[]. The transactional DDL plane
// (indexStatementsWithPrefix, run by CreateTable/ToDDL on every install, boot
// and upgrade) never emits them: a plain CREATE INDEX takes a SHARE lock that
// blocks every INSERT/UPDATE/DELETE on the table for the whole build. Only
// EnsureDeclaredIndexes builds them, with CREATE INDEX CONCURRENTLY, one at a
// time, outside any transaction. See docs/adr/0007-indices-declarados-concurrentes.md.

// States of a DeclaredIndexResult.
const (
	// DeclaredIndexPlanned: dry-run; Statements is what a real run would execute.
	DeclaredIndexPlanned = "planned"
	// DeclaredIndexExists: a VALID index with that name is already on the table.
	DeclaredIndexExists = "exists"
	// DeclaredIndexBuilt: the index was missing and was built.
	DeclaredIndexBuilt = "built"
	// DeclaredIndexRebuilt: the index was INVALID (interrupted CONCURRENTLY
	// build); it was dropped and built again.
	DeclaredIndexRebuilt = "rebuilt"
	// DeclaredIndexBusy: another session (a replica) holds this index's build
	// lock. Not a failure: the index is theirs to finish; check again later.
	DeclaredIndexBusy = "busy"
	// DeclaredIndexSkipped: not attempted; Reason says why.
	DeclaredIndexSkipped = "skipped"
	// DeclaredIndexFailed: attempted (or invalid input); Error says why. Any
	// INVALID leftover is dropped when possible, otherwise the next run heals it.
	DeclaredIndexFailed = "failed"
	// DeclaredIndexCanceled: the context ended before or during this index.
	DeclaredIndexCanceled = "canceled"
)

// Reasons of a skipped DeclaredIndexResult.
const (
	DeclaredIndexReasonNotInOnly     = "not_in_only"    // OnlyIndexes is set and does not name it
	DeclaredIndexReasonCoreTable     = "core_table"     // the table is in CoreTables: not the addon's
	DeclaredIndexReasonTableMissing  = "table_missing"  // no table with that name in Schema
	DeclaredIndexReasonNotTable      = "not_a_table"    // the relation is a view or similar
	DeclaredIndexReasonDialect       = "dialect"        // not Postgres
	DeclaredIndexReasonInTransaction = "in_transaction" // db is a transaction: CONCURRENTLY impossible
	DeclaredIndexReasonNoConnection  = "no_connection"  // nil db or no dedicated connection
)

var (
	// ErrDeclaredIndexInTx: EnsureDeclaredIndexes was handed a transaction.
	// CREATE INDEX CONCURRENTLY cannot run in one, and a plain CREATE INDEX
	// would block writes, so nothing is built.
	ErrDeclaredIndexInTx = errors.New("dynamic: declared indexes are built CONCURRENTLY and cannot run inside a transaction")
	// ErrDeclaredIndexIdentifier: a schema, table, index or column name is not
	// a plain lower-case SQL identifier.
	ErrDeclaredIndexIdentifier = errors.New("dynamic: invalid identifier for a declared index")
)

// DeclaredIndexOptions configure EnsureDeclaredIndexes. The zero value of
// every field but Schema is a safe default.
type DeclaredIndexOptions struct {
	// Schema is where the host materializes these models' tables: "public" in
	// a host that serves addon models from public (ops), SharedSchemaName(key)
	// ("addon_<key>") in a kernel-only host. Required. A table is looked up
	// ONLY in this schema; a same-named table elsewhere is never touched.
	Schema string
	// OnlyIndexes, when non-empty, limits the run to these index names
	// (gradual rollout). The rest are reported as skipped (not_in_only).
	OnlyIndexes []string
	// CoreTables are table names in Schema the addon does NOT own (host core
	// models that share the name, e.g. ops' public.conversations). Indexes on
	// them are skipped (core_table) and never built.
	CoreTables []string
	// LockTimeout bounds every lock wait of the build (the initial SHARE
	// UPDATE EXCLUSIVE lock and the waits for older transactions). On timeout
	// the index fails and is retried by the next run. 0 = 5s.
	LockTimeout time.Duration
	// StatementTimeout bounds each statement. 0 = no limit (a CONCURRENTLY
	// build does not block writes, and it overrides a short server default).
	StatementTimeout time.Duration
	// DryRun reads the catalog and returns the plan (state planned/exists and
	// Statements) without taking locks or running DDL.
	DryRun bool
	// Logger receives one line per index acted on. Nil = slog.Default().
	Logger *slog.Logger
}

// DeclaredIndexResult reports one declared index.
type DeclaredIndexResult struct {
	Model   string   `json:"model,omitempty"`
	Schema  string   `json:"schema"`
	Table   string   `json:"table"`
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
	State   string   `json:"state"`
	Reason  string   `json:"reason,omitempty"`
	// Statements are the DDL statements executed (or, in a dry-run, planned).
	Statements []string      `json:"statements,omitempty"`
	Duration   time.Duration `json:"duration_ns,omitempty"`
	Error      string        `json:"error,omitempty"`
	Err        error         `json:"-"`

	create, drop, lockKey string
}

// DeclaredIndexReport is what EnsureDeclaredIndexes returns. It never fails
// the caller: a run-wide condition is in Error/Err and every index carries its
// own state.
type DeclaredIndexReport struct {
	Schema  string                `json:"schema"`
	DryRun  bool                  `json:"dry_run"`
	Results []DeclaredIndexResult `json:"results"`
	Error   string                `json:"error,omitempty"`
	Err     error                 `json:"-"`
}

// Count returns how many results are in state.
func (r DeclaredIndexReport) Count(state string) int {
	n := 0
	for _, x := range r.Results {
		if x.State == state {
			n++
		}
	}
	return n
}

// DeclaredIndexPlan returns the indexes of def that only EnsureDeclaredIndexes
// builds: composite (2+ columns), non-unique and without a where. Pure.
func DeclaredIndexPlan(def manifest.ModelDefinition) []manifest.IndexDef {
	if def.View != "" {
		return nil
	}
	var out []manifest.IndexDef
	for _, ix := range def.Indices {
		if ix.Unique || ix.Where != "" || len(ix.Columns) < 2 {
			continue
		}
		out = append(out, ix)
	}
	return out
}

// declaredIdentRe is the only identifier shape accepted: what manifest
// validation allows for tables/columns, within Postgres' 63-byte limit.
var declaredIdentRe = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

func declaredIdent(s string) (string, error) {
	if !declaredIdentRe.MatchString(s) {
		return "", fmt.Errorf("%w: %q", ErrDeclaredIndexIdentifier, s)
	}
	return `"` + s + `"`, nil
}

var declaredIndexMethods = map[string]bool{"btree": true, "hash": true, "gin": true, "gist": true, "brin": true}

// declaredIndexName is the declared name, or idx_<table>_<cols> when the
// manifest left it empty.
func declaredIndexName(table string, ix manifest.IndexDef) string {
	if ix.Name != "" {
		return ix.Name
	}
	return "idx_" + table + "_" + strings.Join(ix.Columns, "_")
}

// declaredIndexSQL renders the CONCURRENTLY create and drop statements. Every
// identifier is validated and quoted; there is no free-form SQL.
func declaredIndexSQL(schema, table, name string, ix manifest.IndexDef) (create, drop string, err error) {
	qs, err := declaredIdent(schema)
	if err != nil {
		return "", "", err
	}
	qt, err := declaredIdent(table)
	if err != nil {
		return "", "", err
	}
	qn, err := declaredIdent(name)
	if err != nil {
		return "", "", err
	}
	if len(ix.Columns) < 2 {
		return "", "", fmt.Errorf("dynamic: declared index %q needs 2+ columns", name)
	}
	cols := make([]string, len(ix.Columns))
	seen := map[string]bool{}
	for i, c := range ix.Columns {
		if seen[c] {
			return "", "", fmt.Errorf("dynamic: declared index %q repeats column %q", name, c)
		}
		seen[c] = true
		if cols[i], err = declaredIdent(c); err != nil {
			return "", "", err
		}
	}
	method := strings.ToLower(ix.Method)
	if method == "" {
		method = "btree"
	}
	if !declaredIndexMethods[method] {
		return "", "", fmt.Errorf("dynamic: declared index %q has unsupported method %q", name, ix.Method)
	}
	create = fmt.Sprintf(`CREATE INDEX CONCURRENTLY IF NOT EXISTS %s ON %s.%s USING %s (%s)`,
		qn, qs, qt, method, strings.Join(cols, ", "))
	drop = fmt.Sprintf(`DROP INDEX CONCURRENTLY IF EXISTS %s.%s`, qs, qn)
	return create, drop, nil
}

// declaredIndexLockKey is the per-index advisory lock key (hashed by Postgres
// with hashtextextended). Shared by every replica.
func declaredIndexLockKey(schema, name string) string {
	return "metacore.declared_index:" + schema + "." + name
}

// planDeclaredIndexes expands defs into one result per declared index. The
// ones still pending (State == "") are returned by index.
func planDeclaredIndexes(defs []manifest.ModelDefinition, opts DeclaredIndexOptions) ([]DeclaredIndexResult, []int) {
	only := map[string]bool{}
	for _, n := range opts.OnlyIndexes {
		if n = strings.TrimSpace(n); n != "" {
			only[n] = true
		}
	}
	core := map[string]bool{}
	for _, t := range opts.CoreTables {
		core[strings.TrimSpace(t)] = true
	}
	var out []DeclaredIndexResult
	var pending []int
	for _, def := range defs {
		for _, ix := range DeclaredIndexPlan(def) {
			name := declaredIndexName(def.TableName, ix)
			r := DeclaredIndexResult{
				Model: def.ModelKey, Schema: opts.Schema, Table: def.TableName, Name: name,
				Columns: append([]string(nil), ix.Columns...),
			}
			switch {
			case len(only) > 0 && !only[name]:
				r.State, r.Reason = DeclaredIndexSkipped, DeclaredIndexReasonNotInOnly
			case core[def.TableName]:
				r.State, r.Reason = DeclaredIndexSkipped, DeclaredIndexReasonCoreTable
			default:
				create, drop, err := declaredIndexSQL(opts.Schema, def.TableName, name, ix)
				if err != nil {
					r.State = DeclaredIndexFailed
					r.Err, r.Error = err, err.Error()
				} else {
					r.create, r.drop, r.lockKey = create, drop, declaredIndexLockKey(opts.Schema, name)
				}
			}
			out = append(out, r)
			if r.State == "" {
				pending = append(pending, len(out)-1)
			}
		}
	}
	return out, pending
}

// EnsureDeclaredIndexes builds the declared indexes (DeclaredIndexPlan) of
// defs that are missing or INVALID in opts.Schema:
//
//   - Postgres only, and never inside a transaction (ErrDeclaredIndexInTx):
//     it never falls back to a plain CREATE INDEX;
//   - one index at a time, on ONE dedicated connection, with lock_timeout
//     (opts.LockTimeout) and statement_timeout (opts.StatementTimeout) set on
//     that session and reset afterwards;
//   - per index, a session advisory lock (pg_try_advisory_lock): held by
//     another replica = busy, skipped;
//   - the table must exist in opts.Schema (and not be in opts.CoreTables) and
//     carry every column; a valid index with that name on that table = exists;
//     an INVALID one is dropped CONCURRENTLY and rebuilt; a build that ends
//     INVALID is dropped and reported failed;
//   - a cancelled ctx stops before the next index (results: canceled); an
//     INVALID leftover of an interrupted build is healed by the next run.
//
// It never fails the caller: everything is in the returned report.
func EnsureDeclaredIndexes(ctx context.Context, db *gorm.DB, defs []manifest.ModelDefinition, opts DeclaredIndexOptions) DeclaredIndexReport {
	if opts.LockTimeout <= 0 {
		opts.LockTimeout = 5 * time.Second
	}
	if opts.StatementTimeout < 0 {
		opts.StatementTimeout = 0
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	rep := DeclaredIndexReport{Schema: opts.Schema, DryRun: opts.DryRun}
	results, pending := planDeclaredIndexes(defs, opts)
	rep.Results = results
	if _, err := declaredIdent(opts.Schema); err != nil {
		// Every index already failed in the plan with the same error.
		rep.Err, rep.Error = err, err.Error()
		return rep
	}
	if len(pending) == 0 {
		return rep
	}
	skipAll := func(reason string, err error) DeclaredIndexReport {
		for _, i := range pending {
			rep.Results[i].State, rep.Results[i].Reason = DeclaredIndexSkipped, reason
		}
		if err != nil {
			rep.Err, rep.Error = err, err.Error()
		}
		return rep
	}
	if db == nil || db.Dialector == nil {
		return skipAll(DeclaredIndexReasonNoConnection, errors.New("dynamic: nil db"))
	}
	if db.Dialector.Name() != "postgres" {
		return skipAll(DeclaredIndexReasonDialect, nil)
	}
	if opts.DryRun {
		runDeclaredIndexes(ctx, pgDeclaredIndexDB{db: db}, rep.Results, pending, opts, log)
		return rep
	}
	if gormInTx(db) {
		return skipAll(DeclaredIndexReasonInTransaction, ErrDeclaredIndexInTx)
	}
	connErr := db.WithContext(ctx).Connection(func(c *gorm.DB) error {
		s := pgDeclaredIndexDB{db: c}
		if err := s.setTimeouts(ctx, opts.LockTimeout, opts.StatementTimeout); err != nil {
			return err
		}
		defer s.resetTimeouts()
		runDeclaredIndexes(ctx, s, rep.Results, pending, opts, log)
		return nil
	})
	if connErr != nil {
		for _, i := range pending {
			if rep.Results[i].State == "" {
				if ctx.Err() != nil {
					rep.Results[i].State = DeclaredIndexCanceled
					continue
				}
				rep.Results[i].State, rep.Results[i].Reason = DeclaredIndexSkipped, DeclaredIndexReasonNoConnection
				rep.Results[i].Err, rep.Results[i].Error = connErr, connErr.Error()
			}
		}
		rep.Err, rep.Error = connErr, connErr.Error()
	}
	return rep
}

func gormInTx(db *gorm.DB) bool {
	if db.Statement == nil || db.Statement.ConnPool == nil {
		return false
	}
	_, ok := db.Statement.ConnPool.(gorm.TxCommitter)
	return ok
}

// declaredIndexDB is the catalog + DDL surface the builder needs; the
// Postgres implementation is pgDeclaredIndexDB (a fake drives the unit tests).
type declaredIndexDB interface {
	// relKind is pg_class.relkind of schema.rel ("" when absent).
	relKind(ctx context.Context, schema, rel string) (string, error)
	columns(ctx context.Context, schema, table string) (map[string]bool, error)
	index(ctx context.Context, schema, name string) (declaredIndexInfo, error)
	tryLock(ctx context.Context, key string) (bool, error)
	unlock(key string)
	exec(ctx context.Context, stmt string) error
}

// declaredIndexInfo describes the relation named like the index in a schema.
type declaredIndexInfo struct {
	exists bool
	kind   string // pg_class.relkind; "i" for a plain index
	valid  bool
	table  string // table the index is on
}

func runDeclaredIndexes(ctx context.Context, s declaredIndexDB, results []DeclaredIndexResult, pending []int, opts DeclaredIndexOptions, log *slog.Logger) {
	cols := map[string]map[string]bool{}
	kinds := map[string]string{}
	for n, i := range pending {
		if ctx.Err() != nil {
			for _, j := range pending[n:] {
				results[j].State = DeclaredIndexCanceled
			}
			return
		}
		r := &results[i]
		start := time.Now()
		ensureOneDeclaredIndex(ctx, s, r, opts.DryRun, cols, kinds)
		if !opts.DryRun {
			r.Duration = time.Since(start)
		}
		if r.Err != nil && r.Error == "" {
			r.Error = r.Err.Error()
		}
		attrs := []any{"schema", r.Schema, "table", r.Table, "index", r.Name, "state", r.State}
		switch r.State {
		case DeclaredIndexFailed:
			log.Warn("dynamic.declared_index.failed", append(attrs, "err", r.Error)...)
		case DeclaredIndexBuilt, DeclaredIndexRebuilt:
			log.Info("dynamic.declared_index."+r.State, append(attrs, "duration", r.Duration)...)
		case DeclaredIndexBusy, DeclaredIndexCanceled:
			log.Info("dynamic.declared_index."+r.State, attrs...)
		case DeclaredIndexSkipped:
			log.Debug("dynamic.declared_index.skipped", append(attrs, "reason", r.Reason)...)
		}
	}
}

func ensureOneDeclaredIndex(ctx context.Context, s declaredIndexDB, r *DeclaredIndexResult, dry bool, colCache map[string]map[string]bool, kindCache map[string]string) {
	fail := func(err error) { r.State, r.Err = DeclaredIndexFailed, err }
	kind, ok := kindCache[r.Table]
	if !ok {
		var err error
		if kind, err = s.relKind(ctx, r.Schema, r.Table); err != nil {
			fail(err)
			return
		}
		kindCache[r.Table] = kind
	}
	switch kind {
	case "":
		r.State, r.Reason = DeclaredIndexSkipped, DeclaredIndexReasonTableMissing
		return
	case "r", "p":
	default:
		r.State, r.Reason = DeclaredIndexSkipped, DeclaredIndexReasonNotTable
		return
	}
	have, ok := colCache[r.Table]
	if !ok {
		var err error
		if have, err = s.columns(ctx, r.Schema, r.Table); err != nil {
			fail(err)
			return
		}
		colCache[r.Table] = have
	}
	for _, c := range r.Columns {
		if !have[c] {
			fail(fmt.Errorf("dynamic: column %q is not on %s.%s", c, r.Schema, r.Table))
			return
		}
	}
	info, err := s.index(ctx, r.Schema, r.Name)
	if err != nil {
		fail(err)
		return
	}
	if err := declaredIndexConflict(r, info); err != nil {
		fail(err)
		return
	}
	if info.exists && info.valid {
		r.State = DeclaredIndexExists
		return
	}
	if dry {
		if info.exists {
			r.Statements = []string{r.drop, r.create}
		} else {
			r.Statements = []string{r.create}
		}
		r.State = DeclaredIndexPlanned
		return
	}

	got, err := s.tryLock(ctx, r.lockKey)
	if err != nil {
		fail(err)
		return
	}
	if !got {
		r.State = DeclaredIndexBusy
		return
	}
	defer s.unlock(r.lockKey)
	// Re-read under the lock: another replica may have finished meanwhile.
	if info, err = s.index(ctx, r.Schema, r.Name); err != nil {
		fail(err)
		return
	}
	if err := declaredIndexConflict(r, info); err != nil {
		fail(err)
		return
	}
	if info.exists && info.valid {
		r.State = DeclaredIndexExists
		return
	}
	rebuilt := false
	if info.exists {
		r.Statements = append(r.Statements, r.drop)
		if err := s.exec(ctx, r.drop); err != nil {
			declaredIndexFailOrCancel(ctx, r, err)
			return
		}
		rebuilt = true
	}
	r.Statements = append(r.Statements, r.create)
	if err := s.exec(ctx, r.create); err != nil {
		if ctx.Err() != nil {
			// Interrupted: the INVALID leftover is healed by the next run.
			r.State, r.Err = DeclaredIndexCanceled, err
			return
		}
		r.State, r.Err = DeclaredIndexFailed, err
		dropInvalidLeftover(ctx, s, r)
		return
	}
	after, err := s.index(ctx, r.Schema, r.Name)
	if err != nil {
		fail(err)
		return
	}
	if !after.exists || !after.valid {
		r.State, r.Err = DeclaredIndexFailed, fmt.Errorf("dynamic: index %s.%s is not valid after CREATE INDEX CONCURRENTLY", r.Schema, r.Name)
		dropInvalidLeftover(ctx, s, r)
		return
	}
	r.State = DeclaredIndexBuilt
	if rebuilt {
		r.State = DeclaredIndexRebuilt
	}
}

// declaredIndexConflict refuses a name taken by something other than an
// index on r.Table (IF NOT EXISTS would silently keep it).
func declaredIndexConflict(r *DeclaredIndexResult, info declaredIndexInfo) error {
	if !info.exists {
		return nil
	}
	if info.kind != "i" {
		return fmt.Errorf("dynamic: %s.%s exists and is not an index (relkind %q)", r.Schema, r.Name, info.kind)
	}
	if info.table != r.Table {
		return fmt.Errorf("dynamic: index %s.%s exists on table %q, not %q", r.Schema, r.Name, info.table, r.Table)
	}
	return nil
}

func declaredIndexFailOrCancel(ctx context.Context, r *DeclaredIndexResult, err error) {
	r.Err = err
	if ctx.Err() != nil {
		r.State = DeclaredIndexCanceled
		return
	}
	r.State = DeclaredIndexFailed
}

// dropInvalidLeftover drops an INVALID index a failed build left behind (an
// invalid index is still maintained on every write). Best effort: on failure
// the next run drops it under the lock.
func dropInvalidLeftover(ctx context.Context, s declaredIndexDB, r *DeclaredIndexResult) {
	info, err := s.index(ctx, r.Schema, r.Name)
	if err != nil || !info.exists || info.valid || info.kind != "i" || info.table != r.Table {
		return
	}
	r.Statements = append(r.Statements, r.drop)
	if err := s.exec(ctx, r.drop); err != nil {
		r.Err = errors.Join(r.Err, fmt.Errorf("drop invalid leftover: %w", err))
	}
}
