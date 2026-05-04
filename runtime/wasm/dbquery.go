package wasm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/asteby/metacore-kernel/security"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// db_query host import limits. Documented in docs/wasm-abi.md § 9.5.
const (
	dbQueryMaxSQLBytes  = 16 * 1024
	dbQueryMaxArgs      = 64
	dbQueryMaxRows      = 10_000
	dbQueryMaxRespBytes = 8 * 1024 * 1024
	dbQueryDeadline     = 5 * time.Second
)

// AddonSchema returns the canonical Postgres schema name an addon's tables
// live in. Identifier-safe by construction: manifest.Validate already
// enforces `^[a-z][a-z0-9_]{1,63}$` on addon keys.
func AddonSchema(addonKey string) string {
	return "addon_" + strings.ToLower(addonKey)
}

// executeDBQuery is the inner pure-Go path the wasm host import calls into.
// It never returns an error — all failures surface inside the JSON envelope
// the guest receives, mirroring the shape used by http_fetch / event_emit.
//
// Defense in depth:
//  1. validateSelectOnly rejects obvious mutations and multi-statement
//     payloads at the string layer.
//  2. The Enforcer (when wired) gates the call against
//     `db:read addon_<key>.*` per security.Capabilities.
//  3. SET LOCAL search_path scopes bare-name lookups to the addon schema for
//     the duration of the surrounding transaction.
//
// Cross-schema references (`public.foo`, `billing.invoices`) are NOT walked
// in v1 — that requires a real Postgres AST parser and is tracked in
// docs/wasm-abi.md § 9.7. Until v1.2, an addon that wants public reads must
// declare an explicit `db:read public.<table>` capability AND the implicit
// own-schema search_path will mask it on bare names.
func executeDBQuery(
	ctx context.Context,
	db *gorm.DB,
	addonKey string,
	enforcer *security.Enforcer,
	sqlText string,
	argsJSON []byte,
) []byte {
	start := time.Now()
	schema := AddonSchema(addonKey)
	durMs := func() int64 { return time.Since(start).Milliseconds() }

	if db == nil {
		return dbQueryErr(schema, "db_unavailable",
			"host has no *gorm.DB configured", durMs())
	}
	if len(sqlText) > dbQueryMaxSQLBytes {
		return dbQueryErr(schema, "invalid_sql",
			fmt.Sprintf("sql exceeds %d byte cap", dbQueryMaxSQLBytes), durMs())
	}
	if err := validateSelectOnly(sqlText); err != nil {
		return dbQueryErr(schema, "invalid_sql", err.Error(), durMs())
	}

	args, err := decodeDBArgs(argsJSON)
	if err != nil {
		return dbQueryErr(schema, "arg_decode", err.Error(), durMs())
	}
	if len(args) > dbQueryMaxArgs {
		return dbQueryErr(schema, "arg_decode",
			fmt.Sprintf("argument count exceeds %d", dbQueryMaxArgs), durMs())
	}

	if enforcer != nil {
		if err := enforcer.CheckCapability(addonKey, "db:read", schema+".*"); err != nil {
			return dbQueryErr(schema, "forbidden", err.Error(), durMs())
		}
	}

	queryCtx, cancel := context.WithTimeout(ctx, dbQueryDeadline)
	defer cancel()

	tx := db.WithContext(queryCtx).Begin()
	if tx.Error != nil {
		return dbQueryErr(schema, "db_error", tx.Error.Error(), durMs())
	}

	if err := tx.Exec(fmt.Sprintf(
		`SET LOCAL search_path TO %s, public`, quoteIdent(schema),
	)).Error; err != nil {
		_ = tx.Rollback()
		return dbQueryErr(schema, "db_error",
			"set search_path: "+err.Error(), durMs())
	}

	rows, err := tx.Raw(sqlText, args...).Rows()
	if err != nil {
		_ = tx.Rollback()
		return dbQueryErr(schema, "db_error", err.Error(), durMs())
	}

	cols, err := rows.Columns()
	if err != nil {
		_ = rows.Close()
		_ = tx.Rollback()
		return dbQueryErr(schema, "db_error", err.Error(), durMs())
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
			_ = tx.Rollback()
			return dbQueryErr(schema, "db_error", err.Error(), durMs())
		}
		row := make(map[string]any, len(cols))
		for i, c := range cols {
			row[c] = jsonifyDBVal(vals[i])
		}
		rowsOut = append(rowsOut, row)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		_ = tx.Rollback()
		return dbQueryErr(schema, "db_error", err.Error(), durMs())
	}
	_ = rows.Close()

	if err := tx.Commit().Error; err != nil {
		return dbQueryErr(schema, "db_error", err.Error(), durMs())
	}

	if truncated {
		return dbQueryErr(schema, "row_limit_exceeded",
			fmt.Sprintf("result exceeded %d rows", dbQueryMaxRows), durMs())
	}

	colMeta := make([]map[string]any, len(cols))
	for i, c := range cols {
		m := map[string]any{"name": c}
		if i < len(colTypes) && colTypes[i] != nil {
			if t := colTypes[i].DatabaseTypeName(); t != "" {
				m["type"] = strings.ToLower(t)
			}
		}
		colMeta[i] = m
	}

	env, _ := json.Marshal(map[string]any{
		"success": true,
		"data": map[string]any{
			"rows":     rowsOut,
			"rowCount": len(rowsOut),
			"columns":  colMeta,
		},
		"meta": map[string]any{
			"schema":     schema,
			"durationMs": durMs(),
			"truncated":  false,
		},
	})
	if len(env) > dbQueryMaxRespBytes {
		return dbQueryErr(schema, "row_limit_exceeded",
			"response exceeds size cap", durMs())
	}
	return env
}

func dbQueryErr(schema, code, message string, durationMs int64) []byte {
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

// validateSelectOnly applies pragmatic, parser-free safety checks. A real
// Postgres AST is the long-term plan (docs/wasm-abi.md § 9.7); for now this
// rejects multi-statement bodies, non-SELECT leading statements, and any
// occurrence of mutating / privileged keywords as whole words outside
// single-quoted literals.
func validateSelectOnly(sqlText string) error {
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
	if leading != "SELECT" && leading != "WITH" {
		return fmt.Errorf("only SELECT / WITH … SELECT allowed (got %q)", leading)
	}

	for _, kw := range bannedDBQueryKeywords {
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

var bannedDBQueryKeywords = []string{
	"INSERT", "UPDATE", "DELETE", "MERGE",
	"CREATE", "DROP", "ALTER", "TRUNCATE",
	"GRANT", "REVOKE", "CALL", "DO",
	"LISTEN", "NOTIFY", "COPY",
	"BEGIN", "COMMIT", "ROLLBACK", "SAVEPOINT", "RELEASE",
}

func matchWholeWord(s, word string) bool {
	re := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(word) + `\b`)
	return re.MatchString(s)
}

func firstWordUpper(s string) string {
	for _, f := range strings.Fields(s) {
		return strings.ToUpper(f)
	}
	return ""
}

// stripSQLLiterals removes single-quoted string literals so banned-keyword
// scanning doesn't false-positive on data like `'DELETE me'`. Doubled
// single quotes inside a literal (`''`) are SQL escape — kept as a single
// character inside the literal.
func stripSQLLiterals(s string) string {
	var b strings.Builder
	inQ := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\'' {
			if inQ && i+1 < len(s) && s[i+1] == '\'' {
				i++
				continue
			}
			inQ = !inQ
			continue
		}
		if !inQ {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// decodeDBArgs walks a JSON array and returns driver-friendly Go values.
// The marker types ($uuid / $ts / $bytes) keep the host honest about
// non-JSON-native Postgres types — see docs/wasm-abi.md § 9.6.
func decodeDBArgs(raw []byte) ([]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var arr []any
	if err := dec.Decode(&arr); err != nil {
		return nil, fmt.Errorf("args is not a JSON array: %w", err)
	}
	out := make([]any, 0, len(arr))
	for _, a := range arr {
		v, err := decodeOneDBArg(a)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func decodeOneDBArg(a any) (any, error) {
	switch v := a.(type) {
	case nil:
		return nil, nil
	case bool:
		return v, nil
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return i, nil
		}
		if f, err := v.Float64(); err == nil {
			return f, nil
		}
		return nil, fmt.Errorf("invalid numeric arg %q", v)
	case string:
		return v, nil
	case map[string]any:
		if u, ok := v["$uuid"].(string); ok {
			id, err := uuid.Parse(u)
			if err != nil {
				return nil, fmt.Errorf("invalid $uuid %q: %w", u, err)
			}
			return id, nil
		}
		if t, ok := v["$ts"].(string); ok {
			if ts, err := time.Parse(time.RFC3339Nano, t); err == nil {
				return ts, nil
			}
			ts, err := time.Parse(time.RFC3339, t)
			if err != nil {
				return nil, fmt.Errorf("invalid $ts %q: %w", t, err)
			}
			return ts, nil
		}
		if b, ok := v["$bytes"].(string); ok {
			bb, err := base64.StdEncoding.DecodeString(b)
			if err != nil {
				return nil, fmt.Errorf("invalid $bytes: %w", err)
			}
			return bb, nil
		}
		return nil, fmt.Errorf("unsupported object arg (need one of $uuid, $ts, $bytes)")
	default:
		return nil, fmt.Errorf("unsupported arg type %T", v)
	}
}

func jsonifyDBVal(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case []byte:
		// pgx returns text/varchar as []byte; surface as string for JSON.
		// Binary columns ride through this same path — guests that need
		// raw bytes should base64 inside the column itself.
		return string(t)
	case time.Time:
		return t.Format(time.RFC3339Nano)
	default:
		return v
	}
}

// quoteIdent returns a Postgres-safe identifier. addonKey is already
// constrained by manifest.Validate, but this stays defensive in case a
// future caller forgets that.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
