package staleplan

// Postgres-backed tests. They need a disposable server:
//
//	docker run -d -p 127.0.0.1:55447:5432 -e POSTGRES_PASSWORD=pw -e POSTGRES_DB=t pgvector/pgvector:pg17
//	TEST_POSTGRES_DSN="host=127.0.0.1 port=55447 user=postgres password=pw dbname=t sslmode=disable" go test ./database/staleplan
//
// They reproduce SQLSTATE 0A000 exactly the way production hit it: a connection
// holds a prepared statement, a migration changes the statement's result
// type, and the same query runs again.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/asteby/metacore-kernel/cachecoherence"
	"github.com/jackc/pgx/v5"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func dsn(t *testing.T) string {
	t.Helper()
	d := os.Getenv("TEST_POSTGRES_DSN")
	if d == "" {
		t.Skip("TEST_POSTGRES_DSN not set: skipping Postgres stale-plan test")
	}
	return d
}

func suffix() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// openDB opens a single-connection pool (so the prepared statement and the
// ALTER are guaranteed to share the connection, as in a busy production pool
// where every connection ends up with the statement).
func openDB(t *testing.T, prepare, install bool) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(postgres.Open(dsn(t)), &gorm.Config{
		PrepareStmt: prepare,
		Logger:      logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	sdb, _ := db.DB()
	sdb.SetMaxOpenConns(1)
	if install {
		if err := Install(db); err != nil {
			t.Fatalf("install: %v", err)
		}
	}
	t.Cleanup(func() { _ = sdb.Close() })
	return db
}

func mkTable(t *testing.T, db *gorm.DB) string {
	t.Helper()
	tbl := "sp_" + suffix()
	if err := db.Exec(`CREATE TABLE ` + tbl + ` (id int primary key, v int)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO ` + tbl + ` VALUES (1, 10)`).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec(`DROP TABLE IF EXISTS ` + tbl) })
	return tbl
}

func count(t *testing.T, db *gorm.DB, tbl string) int64 {
	t.Helper()
	var n int64
	if err := db.Raw(`SELECT count(*) FROM ` + tbl).Scan(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func readRows(db *gorm.DB, tbl string) error {
	rows, err := db.Raw(`SELECT * FROM `+tbl+` WHERE id = ?`, 1).Rows()
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
	}
	return rows.Err()
}

// BEFORE: without the wrapper, PrepareStmt pools fail forever. This is the
// production incident (POST de dirección -> 500 until OPS restarts).
func TestBefore_PrepareStmtPoolFailsForever(t *testing.T) {
	db := openDB(t, true, false)
	tbl := mkTable(t, db)
	if err := readRows(db, tbl); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`ALTER TABLE ` + tbl + ` ALTER COLUMN v TYPE text`).Error; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		err := readRows(db, tbl)
		if err == nil || !strings.Contains(err.Error(), "cached plan must not change result type") {
			t.Fatalf("attempt %d: want 0A000, got %v", i, err)
		}
		t.Logf("BEFORE attempt %d: %v", i, err)
	}
}

// AFTER: with the wrapper the first post-migration query succeeds, with no restart.
func TestAfter_RetryHealsRows(t *testing.T) {
	db := openDB(t, true, true)
	tbl := mkTable(t, db)
	if err := readRows(db, tbl); err != nil {
		t.Fatal(err)
	}
	before := GetStats()
	db.Exec(`ALTER TABLE ` + tbl + ` ALTER COLUMN v TYPE text`)
	if err := readRows(db, tbl); err != nil {
		t.Fatalf("AFTER: query must heal transparently, got %v", err)
	}
	after := GetStats()
	if after.Recovered != before.Recovered+1 || after.Detected != before.Detected+1 {
		t.Fatalf("stats: %+v -> %+v", before, after)
	}
	for i := 0; i < 3; i++ {
		if err := readRows(db, tbl); err != nil {
			t.Fatalf("later attempt %d: %v", i, err)
		}
	}
	t.Logf("AFTER: healed; stats %+v", after)
}

func TestAfter_RetryHealsScanAndRow(t *testing.T) {
	db := openDB(t, true, true)
	tbl := mkTable(t, db)
	type row struct {
		ID int
		V  string
	}
	var r row
	if err := db.Raw(`SELECT * FROM `+tbl+` WHERE id = ?`, 1).Scan(&r).Error; err != nil {
		t.Fatal(err)
	}
	db.Exec(`ALTER TABLE ` + tbl + ` ALTER COLUMN v TYPE text`)
	if err := db.Raw(`SELECT * FROM `+tbl+` WHERE id = ?`, 1).Scan(&r).Error; err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if r.V != "10" {
		t.Fatalf("row = %+v", r)
	}
	// Row() (QueryRowContext)
	var x int
	if err := db.Raw(`SELECT id FROM `+tbl+` WHERE id = ?`, 1).Row().Scan(&x); err != nil {
		t.Fatal(err)
	}
	db.Exec(`ALTER TABLE ` + tbl + ` ADD COLUMN extra int`)
	var id int
	if err := db.Raw(`SELECT id FROM `+tbl+` WHERE id = ? LIMIT 1`, 1).Row().Scan(&id); err != nil {
		t.Fatalf("Row: %v", err)
	}
}

// A write must be applied exactly once even though its first attempt failed.
func TestWriteNotDuplicated_Autocommit(t *testing.T) {
	db := openDB(t, true, true)
	tbl := mkTable(t, db)
	ins := func(id int) error {
		var out []map[string]any
		return db.Raw(`INSERT INTO `+tbl+` (id, v) VALUES (?, ?) RETURNING *`, id, 1).Scan(&out).Error
	}
	if err := ins(2); err != nil {
		t.Fatal(err)
	}
	db.Exec(`ALTER TABLE ` + tbl + ` ADD COLUMN w int DEFAULT 7`) // RETURNING * now has another column
	before := GetStats()
	if err := ins(3); err != nil {
		t.Fatalf("insert after ALTER: %v", err)
	}
	if got := GetStats().Recovered; got != before.Recovered+1 {
		t.Fatalf("expected a recovered retry, stats %+v", GetStats())
	}
	if n := count(t, db, tbl); n != 3 { // ids 1, 2, 3 -- a double apply would be a PK violation or a 4th row
		t.Fatalf("rows = %d, want 3", n)
	}
	// Non-idempotent write: a counter incremented exactly once.
	if err := db.Exec(`UPDATE ` + tbl + ` SET v = v + 1 WHERE id = 1`).Error; err != nil {
		t.Fatal(err)
	}
	db.Exec(`ALTER TABLE ` + tbl + ` ADD COLUMN z int`)
	var out []map[string]any
	if err := db.Raw(`UPDATE ` + tbl + ` SET v = v + 1 WHERE id = 1 RETURNING *`).Scan(&out).Error; err != nil {
		t.Fatal(err)
	}
	var v int
	db.Raw(`SELECT v FROM ` + tbl + ` WHERE id = 1`).Scan(&v)
	if v != 12 {
		t.Fatalf("v = %d, want 12 (10 + exactly two increments)", v)
	}
}

// gorm wraps Create/Update/Delete in its own default transaction; the first
// statement of it is replayed on a fresh transaction.
//
// (pgx statement-cache mode: inside a transaction GORM's PrepareStmt prepares
// fresh handles per transaction, so the stale description lives in pgx's cache.)
func TestWriteNotDuplicated_GormDefaultTx(t *testing.T) {
	db := openDB(t, false, true)
	tbl := mkTable(t, db)
	type Row struct {
		ID int `gorm:"primaryKey"`
		V  string
	}
	create := func(id int) error {
		var out []map[string]any
		return db.Transaction(func(tx *gorm.DB) error {
			return tx.Raw(`INSERT INTO `+tbl+` (id, v) VALUES (?, ?) RETURNING *`, id, "1").Scan(&out).Error
		})
	}
	if err := create(2); err != nil {
		t.Fatal(err)
	}
	db.Exec(`ALTER TABLE ` + tbl + ` ADD COLUMN w int`)
	before := GetStats()
	if err := create(3); err != nil {
		t.Fatalf("first statement of tx must be replayed: %v", err)
	}
	if GetStats().Recovered != before.Recovered+1 {
		t.Fatalf("stats %+v", GetStats())
	}
	if n := count(t, db, tbl); n != 3 {
		t.Fatalf("rows = %d, want 3", n)
	}
}

// A failure on a LATER statement of a transaction is not silently retried
// (its predecessors were rolled back); RetryTx replays the whole unit once.
func TestLaterStatementNotRetried_RetryTxReplays(t *testing.T) {
	db := openDB(t, false, true)
	tbl := mkTable(t, db)
	audit := "spa_" + suffix()
	db.Exec(`CREATE TABLE ` + audit + ` (n int)`)
	t.Cleanup(func() { db.Exec(`DROP TABLE IF EXISTS ` + audit) })

	work := func(tx *gorm.DB) error {
		if err := tx.Exec(`INSERT INTO ` + audit + ` VALUES (1)`).Error; err != nil {
			return err
		}
		var out []map[string]any
		return tx.Raw(`SELECT * FROM ` + tbl).Scan(&out).Error
	}
	if err := db.Transaction(work); err != nil {
		t.Fatal(err)
	}
	db.Exec(`DELETE FROM ` + audit)
	db.Exec(`ALTER TABLE ` + tbl + ` ADD COLUMN w int`)

	before := GetStats()
	err := db.Transaction(work)
	if err == nil || !IsStalePlan(err) {
		t.Fatalf("2nd statement must surface the error, got %v", err)
	}
	if n := count(t, db, audit); n != 0 {
		t.Fatalf("audit rows after failed tx = %d, want 0 (rolled back)", n)
	}
	if GetStats().Skipped != before.Skipped+1 {
		t.Fatalf("expected a skipped occurrence, stats %+v", GetStats())
	}

	// Make the statement stale again for RetryTx.
	db.Exec(`ALTER TABLE ` + tbl + ` ADD COLUMN w2 int`)
	if err := RetryTx(db, work); err != nil {
		t.Fatalf("RetryTx: %v", err)
	}
	if n := count(t, db, audit); n != 1 {
		t.Fatalf("audit rows = %d, want exactly 1 (effects of the first attempt rolled back, replay applied once)", n)
	}
}

// A Bump clears the handles BEFORE anything fails: no error at all.
func TestBumpPreventsTheFailure(t *testing.T) {
	db := openDB(t, true, true)
	tbl := mkTable(t, db)
	if err := readRows(db, tbl); err != nil {
		t.Fatal(err)
	}
	db.Exec(`ALTER TABLE ` + tbl + ` ALTER COLUMN v TYPE text`)
	before := GetStats()
	if _, err := cachecoherence.BumpContext(context.Background(), "test.migrate"); err != nil {
		t.Fatal(err)
	}
	if err := readRows(db, tbl); err != nil {
		t.Fatalf("after Bump: %v", err)
	}
	after := GetStats()
	if after.Detected != before.Detected {
		t.Fatalf("Bump should have prevented the error entirely: %+v -> %+v", before, after)
	}
	if after.Resets <= before.Resets {
		t.Fatalf("no reset recorded: %+v", after)
	}
}

func TestInstallIdempotentAndTransparent(t *testing.T) {
	db := openDB(t, true, true)
	if err := Install(db); err != nil {
		t.Fatal(err)
	}
	if _, ok := db.ConnPool.(*pool); !ok {
		t.Fatal("not wrapped")
	}
	if _, ok := db.ConnPool.(*pool).inner.(*pool); ok {
		t.Fatal("double wrapped")
	}
	sdb, err := db.DB()
	if err != nil || sdb == nil {
		t.Fatalf("db.DB() through the wrapper: %v", err)
	}
	// nested transaction (savepoint) still works
	tbl := mkTable(t, db)
	err = db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`INSERT INTO ` + tbl + ` VALUES (5, 5)`).Error; err != nil {
			return err
		}
		return tx.Transaction(func(tx2 *gorm.DB) error {
			return tx2.Exec(`INSERT INTO ` + tbl + ` VALUES (6, 6)`).Error
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, tbl); n != 3 {
		t.Fatalf("rows = %d", n)
	}
}

// Without PrepareStmt pgx heals after one failure; the Dialector's per-connection
// generation check makes even that one failure unnecessary after a Reset.
func TestDialectorDeallocatesOnReuse(t *testing.T) {
	d, err := Dialector(dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(d, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sdb, _ := db.DB()
	sdb.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sdb.Close() })
	if err := Install(db); err != nil {
		t.Fatal(err)
	}
	tbl := mkTable(t, db)
	if err := readRows(db, tbl); err != nil {
		t.Fatal(err)
	}
	db.Exec(`ALTER TABLE ` + tbl + ` ALTER COLUMN v TYPE text`)
	before := GetStats()
	Reset()
	if err := readRows(db, tbl); err != nil {
		t.Fatal(err)
	}
	after := GetStats()
	if after.Detected != before.Detected {
		t.Fatalf("an error still happened: %+v", after)
	}
	if after.ConnsDeallocated <= before.ConnsDeallocated {
		t.Fatalf("connection was not deallocated on reuse: %+v", after)
	}
}

// A pooler (or DISCARD ALL / DEALLOCATE ALL) that wiped the server-side
// statements: `prepared statement "..." does not exist` (26000).
func TestAfter_RetryHealsDeallocatedStatement(t *testing.T) {
	db := openDB(t, true, true)
	tbl := mkTable(t, db)
	if err := readRows(db, tbl); err != nil {
		t.Fatal(err)
	}
	// Raw conn: DEALLOCATE ALL behind the back of database/sql and pgx.
	sdb, _ := db.DB()
	conn, err := sdb.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), `DEALLOCATE ALL`); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	before := GetStats()
	if err := readRows(db, tbl); err != nil {
		t.Fatalf("26000 must heal: %v", err)
	}
	if GetStats().Recovered != before.Recovered+1 {
		t.Fatalf("expected one recovery, stats %+v", GetStats())
	}
}

// Concurrent readers right after a migration changed the result type: all of them
// must succeed (run with -race).
func TestConcurrentReadersDuringMigration(t *testing.T) {
	db := openDB(t, true, true)
	sdb, _ := db.DB()
	sdb.SetMaxOpenConns(8)
	tbl := mkTable(t, db)
	if err := readRows(db, tbl); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 20; j++ {
				if err := readRows(db, tbl); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	// The migration commits first; the readers then all hit their stale
	// statements at once. (A DDL that commits BETWEEN a query's Describe and
	// its Execute is a different, unavoidable race: one bounded retry cannot
	// promise to outlast a migration that is still running.)
	if err := db.Exec(`ALTER TABLE ` + tbl + ` ALTER COLUMN v TYPE text`).Error; err != nil {
		t.Fatal(err)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("reader failed: %v", err)
	}
}

// The per-connection hook (database/sql ResetSession) wipes a BUSY-at-reset
// connection's server-side statements when it comes back after a Reset.
func TestResetSessionHookDeallocatesLaggingConn(t *testing.T) {
	ctx := context.Background()
	c, err := pgx.Connect(ctx, dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	if err := resetSession(ctx, c); err != nil { // stamps the fresh connection
		t.Fatal(err)
	}
	if _, err := c.Prepare(ctx, "sp_named", "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = c.QueryRow(ctx, `SELECT count(*) FROM pg_prepared_statements WHERE name = 'sp_named'`).Scan(&n)
	if n != 1 {
		t.Fatalf("setup: %d", n)
	}
	if err := resetSession(ctx, c); err != nil { // same generation: untouched
		t.Fatal(err)
	}
	_ = c.QueryRow(ctx, `SELECT count(*) FROM pg_prepared_statements WHERE name = 'sp_named'`).Scan(&n)
	if n != 1 {
		t.Fatalf("hook wiped a current connection: %d", n)
	}
	generation.Add(1) // a Reset happened while this connection was busy
	if err := resetSession(ctx, c); err != nil {
		t.Fatal(err)
	}
	_ = c.QueryRow(ctx, `SELECT count(*) FROM pg_prepared_statements WHERE name = 'sp_named'`).Scan(&n)
	if n != 0 {
		t.Fatalf("lagging connection kept %d statements", n)
	}
}

// The wasm data host opens a transaction, runs `SET LOCAL search_path`, then
// the guest's query. The stale statement is therefore the SECOND of its
// transaction; the setup-only prefix is replayed on a fresh transaction.
func TestSetLocalPrefixReplayed(t *testing.T) {
	for _, prepare := range []bool{true, false} {
		db := openDB(t, prepare, true)
		tbl := mkTable(t, db)
		if err := readRows(db, tbl); err != nil {
			t.Fatal(err)
		}
		db.Exec(`ALTER TABLE ` + tbl + ` ALTER COLUMN v TYPE text`)
		before := GetStats()

		tx := db.Begin()
		if tx.Error != nil {
			t.Fatal(tx.Error)
		}
		if err := tx.Exec(`SET LOCAL search_path TO public`).Error; err != nil {
			t.Fatal(err)
		}
		rows, err := tx.Raw(`SELECT * FROM `+tbl+` WHERE id = ?`, 1).Rows()
		if err != nil {
			t.Fatalf("prepare=%v: query after SET LOCAL must heal: %v", prepare, err)
		}
		n := 0
		for rows.Next() {
			n++
		}
		rows.Close()
		// The replayed SET LOCAL is still in force on the new transaction.
		var sp string
		if err := tx.Raw(`SHOW search_path`).Scan(&sp).Error; err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit().Error; err != nil {
			t.Fatal(err)
		}
		if n != 1 || !strings.Contains(sp, "public") {
			t.Fatalf("prepare=%v: rows=%d search_path=%q", prepare, n, sp)
		}
		if GetStats().Detected == before.Detected && prepare {
			t.Fatalf("expected a stale-plan detection with PrepareStmt: %+v", GetStats())
		}
		t.Logf("prepare=%v stats %+v", prepare, GetStats())
	}
}

// A transaction that already read data cannot be restarted behind the
// caller's back: the error surfaces (and nothing is replayed twice).
func TestAfterDataStatementNotRestarted(t *testing.T) {
	db := openDB(t, false, true)
	tbl := mkTable(t, db)
	if err := readRows(db, tbl); err != nil {
		t.Fatal(err)
	}
	db.Exec(`ALTER TABLE ` + tbl + ` ALTER COLUMN v TYPE text`)
	tx := db.Begin()
	var one int
	if err := tx.Raw(`SELECT 1`).Scan(&one).Error; err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Raw(`SELECT * FROM `+tbl+` WHERE id = ?`, 1).Rows()
	if err == nil {
		rows.Close()
	}
	_ = tx.Rollback()
	if err == nil || !IsStalePlan(err) {
		t.Fatalf("a data statement before the failure forbids a silent restart; got %v", err)
	}
}
