package dynamic

import (
	"fmt"
	"sort"
	"strings"

	"github.com/asteby/metacore-kernel/manifest"
)

// CanonicalColumnTypes is the single allowlist of logical column types the
// kernel accepts in a v3 manifest. It is the union of:
//
//   - the JSON Schema enum (manifest/v3/schema/manifest-v3.schema.json): uuid,
//     text, integer, bigint, numeric, boolean, timestamp, timestamptz, date,
//     json, jsonb, vector;
//   - the real aliases pgColumnType + columnGoType already accept today:
//     string, int, decimal, float, double, bool, datetime,
//     "timestamp with time zone".
//
// Parameterized Postgres-native forms — numeric(p,s) / decimal(p,s),
// varchar(n) / char(n) / character[ varying](n) and vector(n) — are ALSO
// accepted but are not enumerated here; they are validated structurally by
// parameterizedColumnType (see ValidateColumnType).
//
// This slice is documentation + the error message surface only. The authority
// on what is accepted is pgColumnType: ValidateColumnType delegates to it so
// the validator and the DDL emitter can NEVER diverge.
var CanonicalColumnTypes = []string{
	"uuid",
	"string",
	"text",
	"int",
	"integer",
	"bigint",
	"numeric",
	"decimal",
	"float",
	"double",
	"boolean",
	"bool",
	"timestamp",
	"timestamptz",
	"datetime",
	"timestamp with time zone",
	"date",
	"json",
	"jsonb",
	"vector",
}

// ValidateColumnType reports whether t is a column type the kernel can
// materialise. It is the SINGLE source of truth for the type allowlist,
// consumed by the hub publish gate and any host that validates a manifest
// before install.
//
// It is implemented by delegating to pgColumnType — the exact function the DDL
// plane uses to emit a column's SQL type — so a type that validates here is
// guaranteed to also produce DDL, and the two can never drift. Both the plain
// aliases (CanonicalColumnTypes) and the parameterized forms
// (numeric(p,s)/varchar(n)/vector(n)) are accepted; everything else is
// rejected with a clear, enumerated error.
func ValidateColumnType(t string) error {
	if strings.TrimSpace(t) == "" {
		return fmt.Errorf("empty column type; allowed: %s", strings.Join(sortedCanonical(), ", "))
	}
	// pgColumnType is the authority: it accepts the canonical aliases AND the
	// parameterized forms, and errors on anything else. Reusing it here makes
	// ValidateColumnType and the DDL emitter share one allowlist by construction.
	if _, err := pgColumnType(manifest.ColumnDef{Type: t}); err != nil {
		return fmt.Errorf("unknown column type %q; allowed: %s (plus parameterized forms numeric(p,s), varchar(n), vector(n))",
			t, strings.Join(sortedCanonical(), ", "))
	}
	return nil
}

func sortedCanonical() []string {
	out := append([]string(nil), CanonicalColumnTypes...)
	sort.Strings(out)
	return out
}
