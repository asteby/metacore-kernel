package wasm

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/asteby/metacore-kernel/security"
	"gorm.io/gorm"
)

// db_exec host import limits. Mirror db_query (docs/wasm-abi.md § 10.5).
const (
	dbExecMaxSQLBytes  = 16 * 1024
	dbExecMaxArgs      = 64
	dbExecMaxRespBytes = 8 * 1024 * 1024
	dbExecDeadline     = 5 * time.Second
)

// executeDBExec is the inner pure-Go path the wasm host import calls into for
// mutating SQL. It mirrors executeDBQuery but with the `db:write` capability
// and reuses the action handler's open *gorm.DB transaction (`tx`) when one
// is stashed on the invocation context. When `tx` is nil and a standalone
// `db` handle is provided, the host opens its own short-lived transaction so
// a driver error rolls back cleanly. All failures surface inside the JSON
// envelope; the function never returns an error — the wire shape matches
// the rest of the kernel ({success, data, meta}).
func executeDBExec(
	ctx context.Context,
	tx *gorm.DB,
	db *gorm.DB,
	addonKey string,
	enforcer *security.Enforcer,
	sqlText string,
	argsJSON []byte,
) []byte {
	start := time.Now()
	schema := AddonSchema(addonKey)
	durMs := func() int64 { return time.Since(start).Milliseconds() }

	// Prefer the action handler's tx so the guest's writes piggy-back on
	// the surrounding action transaction; fall back to a fresh tx on the
	// standalone db only when no action transaction is in flight.
	conn := tx
	standalone := false
	if conn == nil {
		conn = db
		standalone = true
	}
	if conn == nil {
		return dbExecErr(schema, "db_unavailable",
			"host has no *gorm.DB configured", durMs())
	}

	if len(sqlText) > dbExecMaxSQLBytes {
		return dbExecErr(schema, "invalid_sql",
			fmt.Sprintf("sql exceeds %d byte cap", dbExecMaxSQLBytes), durMs())
	}
	if err := validateMutationOnly(sqlText); err != nil {
		return dbExecErr(schema, "invalid_sql", err.Error(), durMs())
	}

	args, err := decodeDBArgs(argsJSON)
	if err != nil {
		return dbExecErr(schema, "arg_decode", err.Error(), durMs())
	}
	if len(args) > dbExecMaxArgs {
		return dbExecErr(schema, "arg_decode",
			fmt.Sprintf("argument count exceeds %d", dbExecMaxArgs), durMs())
	}

	if enforcer != nil {
		if err := enforcer.CheckCapability(addonKey, "db:write", schema+".*"); err != nil {
			return dbExecErr(schema, "forbidden", err.Error(), durMs())
		}
	}

	execCtx, cancel := context.WithTimeout(ctx, dbExecDeadline)
	defer cancel()

	work := conn.WithContext(execCtx)
	if standalone {
		work = work.Begin()
		if work.Error != nil {
			return dbExecErr(schema, "db_error", work.Error.Error(), durMs())
		}
	}

	if err := work.Exec(fmt.Sprintf(
		`SET LOCAL search_path TO %s, public`, quoteIdent(schema),
	)).Error; err != nil {
		if standalone {
			_ = work.Rollback()
		}
		return dbExecErr(schema, "db_error",
			"set search_path: "+err.Error(), durMs())
	}

	res := work.Exec(sqlText, args...)
	if res.Error != nil {
		if standalone {
			_ = work.Rollback()
		}
		return dbExecErr(schema, "db_error", res.Error.Error(), durMs())
	}
	rowsAffected := res.RowsAffected

	if standalone {
		if err := work.Commit().Error; err != nil {
			return dbExecErr(schema, "db_error", err.Error(), durMs())
		}
	}

	env, _ := json.Marshal(map[string]any{
		"success": true,
		"data": map[string]any{
			"rowsAffected": rowsAffected,
		},
		"meta": map[string]any{
			"schema":     schema,
			"durationMs": durMs(),
		},
	})
	if len(env) > dbExecMaxRespBytes {
		return dbExecErr(schema, "db_error",
			"response exceeds size cap", durMs())
	}
	return env
}

func dbExecErr(schema, code, message string, durationMs int64) []byte {
	b, _ := json.Marshal(map[string]any{
		"success": false,
		"error":   map[string]any{"code": code, "message": message},
		"meta": map[string]any{
			"schema":     schema,
			"durationMs": durationMs,
		},
	})
	return b
}

// validateMutationOnly mirrors validateSelectOnly but inverted: only mutating
// statements (INSERT/UPDATE/DELETE/MERGE) reach the driver. Multi-statement
// payloads, DDL, privilege and tx-control verbs are rejected at the string
// layer — defence in depth alongside the capability check.
func validateMutationOnly(sqlText string) error {
	trimmed := strings.TrimSpace(sqlText)
	trimmed = strings.TrimRight(trimmed, ";")
	trimmed = strings.TrimSpace(trimmed)
	if trimmed == "" {
		return fmt.Errorf("empty statement")
	}

	naked := stripSQLLiterals(trimmed)
	if strings.Contains(naked, ";") {
		return fmt.Errorf("multiple statements not allowed")
	}

	leading := firstWordUpper(naked)
	switch leading {
	case "INSERT", "UPDATE", "DELETE", "MERGE":
		// allowed
	default:
		return fmt.Errorf("only INSERT/UPDATE/DELETE/MERGE allowed (got %q)", leading)
	}

	for _, kw := range bannedDBExecKeywords {
		if matchWholeWord(naked, kw) {
			return fmt.Errorf("banned keyword: %s", kw)
		}
	}

	low := strings.ToLower(naked)
	if matchWholeWord(low, "information_schema") ||
		strings.Contains(low, "pg_catalog") ||
		regexp.MustCompile(`(?i)\bpg_[a-z_]+\b`).MatchString(low) {
		return fmt.Errorf("introspection schemas are disabled")
	}

	return nil
}

// bannedDBExecKeywords forbids DDL, privilege, replication and tx-control
// verbs inside an otherwise-mutating statement. SELECT is intentionally NOT
// in the list — it is a legitimate source for INSERT…SELECT and for CTE /
// subqueries inside UPDATE / DELETE. SET is also excluded because UPDATE …
// SET col = … is the canonical update form.
var bannedDBExecKeywords = []string{
	"CREATE", "DROP", "ALTER", "TRUNCATE",
	"GRANT", "REVOKE", "CALL", "DO",
	"LISTEN", "NOTIFY", "COPY",
	"BEGIN", "COMMIT", "ROLLBACK", "SAVEPOINT", "RELEASE",
}
