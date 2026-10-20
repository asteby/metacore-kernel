// Package staleplan makes the data layer survive server-side plan state that
// went out of date underneath it.
//
// # The failure
//
// PostgreSQL keeps, per connection, the description (parameter and RESULT
// column types) of every named prepared statement. When a migration then
// changes what `SELECT *` or `RETURNING *` produces (ALTER TABLE ... TYPE,
// ADD COLUMN, DROP COLUMN), the next execution of that statement fails with
//
//	ERROR: cached plan must not change result type (SQLSTATE 0A000)
//
// pgx's own statement cache heals itself after ONE failure per statement per
// connection. A host that opens GORM with PrepareStmt: true (OPS does) holds
// explicit *sql.Stmt handles instead, and nothing ever invalidates them: every
// query that touches the altered table keeps failing until the process is
// restarted. Pooler-side DISCARD ALL / DEALLOCATE has the mirror failure:
// `prepared statement "..." does not exist` (SQLSTATE 26000).
//
// # What this package does
//
//  1. [Classify] / [IsStalePlan] recognise exactly those errors.
//  2. [Install] wraps the GORM connection pool so a statement that fails with
//     one is retried ONCE, after the stale statement handles were dropped
//     ([ResetStatements]).
//  3. [Reset] (registered with cachecoherence at order OrderDatabase) drops
//     them proactively whenever an addon changes, so the failure normally
//     never happens at all; the retry is the safety net.
//  4. [Dialector] optionally gives every pooled connection an epoch check on
//     reuse (database/sql ResetSession) that runs DEALLOCATE ALL when the
//     connection predates the last bump.
//
// # Retry guarantee (read this before touching it)
//
// A retry never duplicates an effect, for reads and writes alike, because of
// where the server raises these errors and what we are willing to retry:
//
//   - 0A000 "cached plan must not change result type" is raised while the
//     cached plan is revalidated, before the executor produces a single row or
//     touches a single tuple; 26000 is raised at Bind, before execution. The
//     failed statement therefore had no effect.
//   - We retry only a statement that ran in autocommit (it is atomic: if it
//     failed it left nothing behind), or the FIRST statement of a transaction
//     (the aborted transaction is rolled back and replayed from BEGIN; nothing
//     preceded the statement so nothing is lost or repeated). A failure in the
//     second or later statement of a transaction is returned to the caller,
//     because the earlier statements' effects were rolled back with the abort
//     and silently replaying only the failed one would be wrong; whole
//     transactions can opt into a replay with [RetryTx].
//   - At most ONE retry per statement, never after the context is done.
//   - Statements that report any other error, including 40001/40P01 and
//     connection loss (where the outcome is unknown), are never retried here.
package staleplan

import (
	"errors"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// Kind classifies a stale-plan error.
type Kind string

const (
	// KindNone: not a stale-plan error.
	KindNone Kind = ""
	// KindResultTypeChanged: SQLSTATE 0A000, "cached plan must not change result type".
	KindResultTypeChanged Kind = "result_type_changed"
	// KindStatementMissing: SQLSTATE 26000, the named statement is gone.
	KindStatementMissing Kind = "statement_missing"
	// KindStatementExists: SQLSTATE 42P05, the name is taken by a stale statement.
	KindStatementExists Kind = "statement_exists"
)

const (
	msgResultType = "cached plan must not change result type"
)

// Classify reports which stale-plan failure err is, or KindNone.
func Classify(err error) Kind {
	if err == nil {
		return KindNone
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		switch pe.Code {
		case "0A000":
			if strings.Contains(pe.Message, msgResultType) {
				return KindResultTypeChanged
			}
		case "26000":
			if strings.Contains(pe.Message, "prepared statement") {
				return KindStatementMissing
			}
		case "42P05":
			if strings.Contains(pe.Message, "prepared statement") {
				return KindStatementExists
			}
		}
		return KindNone
	}
	// Errors that crossed a boundary that flattened the type (a wrapped
	// string, a driver that only keeps the text).
	msg := err.Error()
	switch {
	case strings.Contains(msg, msgResultType):
		return KindResultTypeChanged
	case strings.Contains(msg, "prepared statement") && strings.Contains(msg, "does not exist"):
		return KindStatementMissing
	case strings.Contains(msg, "prepared statement") && strings.Contains(msg, "already exists"):
		return KindStatementExists
	}
	return KindNone
}

// IsStalePlan reports whether err is one of the recoverable plan-state errors.
func IsStalePlan(err error) bool { return Classify(err) != KindNone }
