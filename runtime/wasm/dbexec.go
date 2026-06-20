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
//
// Defense in depth (mirrors db_query — see docs/wasm-abi.md § 10.3):
//  1. validateMutationOnly rejects DDL, multi-statement payloads, banned
//     keywords, and the introspection schemas at the string layer.
//  2. extractMutationRelations parses the SQL with libpg_query and pulls
//     every referenced (schema, table) out of the AST, tagging the DML
//     target as `db:write` and read-only sources (UPDATE.FROM, DELETE.USING,
//     MERGE source, INSERT.SELECT, RETURNING / WHERE subqueries, CTE
//     bodies) as `db:read`. Each cross-schema reference is gated against the
//     addon's compiled capability list individually; a parse failure
//     rejects the statement instead of degrading to "permit".
//  3. SET LOCAL search_path scopes bare-name lookups to the addon schema for
//     the duration of the surrounding transaction.
func executeDBExec(
	ctx context.Context,
	tx *gorm.DB,
	db *gorm.DB,
	addonKey string,
	searchSchema string,
	enforcer *security.Enforcer,
	sqlText string,
	argsJSON []byte,
) []byte {
	start := time.Now()
	// schema authorises the capability gate (always the addon's own schema).
	// searchSchema scopes the runtime search_path — defaults to schema, but the
	// embedder may route bare names elsewhere (ops → public) via WithExecSchema.
	schema := AddonSchema(addonKey)
	if searchSchema == "" {
		searchSchema = schema
	}
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

	// AST-level cross-schema gate. The pre-v0.10.3 path only checked
	// `db:write addon_<key>.*` and trusted `SET LOCAL search_path` to scope
	// bare names — a guest writing `UPDATE public.users SET role = 'admin'`
	// (or any cross-schema mutation) bypassed the gate entirely. We now parse
	// the payload with libpg_query and gate each referenced relation
	// individually: the DML target under `db:write`, read-only sources
	// (UPDATE.FROM, DELETE.USING, MERGE source, INSERT.SELECT, RETURNING /
	// WHERE subqueries, CTE bodies) under `db:read`. Parse failure rejects
	// with `invalid_sql` — never degrades to permit.
	relations, parseErr := extractMutationRelations(sqlText)
	if parseErr != nil {
		return dbExecErr(schema, "invalid_sql", parseErr.Error(), durMs())
	}
	if enforcer != nil {
		// Implicit own-schema gate stays — denies entirely-unregistered
		// addons (no capabilities resolver hit) even when the parsed SQL
		// only references the addon's own schema.
		if err := enforcer.CheckCapability(addonKey, "db:write", schema+".*"); err != nil {
			return dbExecErr(schema, "forbidden", err.Error(), durMs())
		}
		for _, rel := range relations {
			if err := gateRelation(enforcer, addonKey, schema, rel); err != nil {
				return dbExecErr(schema, "forbidden", err.Error(), durMs())
			}
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
		`SET LOCAL search_path TO %s, public`, quoteIdent(searchSchema),
	)).Error; err != nil {
		if standalone {
			_ = work.Rollback()
		}
		return dbExecErr(schema, "db_error",
			"set search_path: "+err.Error(), durMs())
	}

	// Detect a RETURNING clause and route the statement through the Rows
	// path when present. Postgres only surfaces the rows produced by an
	// INSERT/UPDATE/DELETE … RETURNING via a result-set cursor; calling
	// db.Exec discards them and only reports RowsAffected. The ABI doc
	// (§ 10.4) has always documented `data.rows`/`data.columns` on the
	// success envelope when RETURNING is present, but the kernel silently
	// dropped the rows until v0.11.0 closed the gap.
	//
	// Detection is regex-over-stripped-literals so a column literally named
	// `'RETURNING'` or a comment containing the word does not false-trigger
	// the routing. A real Postgres AST parser is the long-term plan
	// (docs/wasm-abi.md § 9.7).
	hasReturning := containsReturning(sqlText)

	if hasReturning {
		rows, err := work.Raw(sqlText, args...).Rows()
		if err != nil {
			if standalone {
				_ = work.Rollback()
			}
			return dbExecErr(schema, "db_error", err.Error(), durMs())
		}
		cols, err := rows.Columns()
		if err != nil {
			_ = rows.Close()
			if standalone {
				_ = work.Rollback()
			}
			return dbExecErr(schema, "db_error", err.Error(), durMs())
		}
		colTypes, _ := rows.ColumnTypes()
		rowsOut := make([]map[string]any, 0)
		truncated := false
		for rows.Next() {
			if len(rowsOut) >= dbQueryMaxRows {
				truncated = true
				break
			}
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				_ = rows.Close()
				if standalone {
					_ = work.Rollback()
				}
				return dbExecErr(schema, "db_error", err.Error(), durMs())
			}
			row := make(map[string]any, len(cols))
			for i, c := range cols {
				row[c] = jsonifyDBVal(vals[i])
			}
			rowsOut = append(rowsOut, row)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			if standalone {
				_ = work.Rollback()
			}
			return dbExecErr(schema, "db_error", err.Error(), durMs())
		}
		_ = rows.Close()
		if truncated {
			if standalone {
				_ = work.Rollback()
			}
			return dbExecErr(schema, "row_limit_exceeded",
				fmt.Sprintf("RETURNING produced more than %d rows", dbQueryMaxRows),
				durMs())
		}
		if standalone {
			if err := work.Commit().Error; err != nil {
				return dbExecErr(schema, "db_error", err.Error(), durMs())
			}
		}
		colMeta := make([]map[string]any, len(cols))
		for i, c := range cols {
			cm := map[string]any{"name": c}
			if i < len(colTypes) && colTypes[i] != nil {
				if t := colTypes[i].DatabaseTypeName(); t != "" {
					cm["type"] = strings.ToLower(t)
				}
			}
			colMeta[i] = cm
		}
		env, _ := json.Marshal(map[string]any{
			"success": true,
			"data": map[string]any{
				"rowsAffected": int64(len(rowsOut)),
				"rows":         rowsOut,
				"columns":      colMeta,
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

// containsReturning reports whether a mutation statement carries a
// top-level RETURNING clause. It strips single-quoted literals (so a
// payload that contains the word `RETURNING` inside a string does not
// false-trigger) and then matches the keyword as a whole word.
//
// libpg_query is not a kernel dependency (the doc § 9.7 tracks the
// long-term plan to adopt one); regex over stripped literals is the
// same approach validateMutationOnly uses for its banned-keyword scan,
// so the two stay consistent until the AST parser lands.
func containsReturning(sqlText string) bool {
	trimmed := strings.TrimSpace(sqlText)
	trimmed = strings.TrimRight(trimmed, ";")
	naked := stripSQLLiterals(trimmed)
	return matchWholeWord(naked, "RETURNING")
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
		// allowed at the string layer
	case "WITH":
		// Postgres allows `WITH cte AS (…) INSERT|UPDATE|DELETE|MERGE …`
		// where the top-level statement after the WITH clause is a DML.
		// The AST top-level type still resolves to InsertStmt / UpdateStmt
		// / DeleteStmt / MergeStmt, which is enforced inside
		// extractMutationRelations once we've parsed. We allow the leading
		// WITH here and let the AST layer reject pure-SELECT wraps so
		// db_exec can't be used as a SELECT bypass.
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
