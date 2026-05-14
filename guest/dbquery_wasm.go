//go:build wasm || wasip1

package guest

// hostDbQuery is the raw host import. Signature mirrors
// docs/wasm-abi.md § 9.1.
//
//go:wasmimport metacore_host db_query
func hostDbQuery(sqlPtr, sqlLen, argsPtr, argsLen uint32) uint64

// DbQuery runs a read-only SQL statement scoped to the addon's
// schema. Returns the decoded rows + column metadata on success or a
// typed `*DbQueryError` on failure (see docs/wasm-abi.md § 9.4 for
// the codes). The host enforces the SELECT-only contract,
// cross-schema capability gating, row limit (10 000) and per-call
// deadline (5 s) — see § 9.2 - 9.5.
//
// Args use Postgres-style positional placeholders (`$1`, `$2`, …).
// Allowed types follow § 9.6: nil, bool, integers, floats, strings,
// `{"$bytes":..}`, `{"$uuid":..}`, `{"$ts":..}`. Bare arrays /
// objects are rejected by the host with `arg_decode`.
//
// Memory contract: the SQL string and args buffer are read
// synchronously by the host before this function returns; both are
// safe to reuse.
func DbQuery(sql string, args ...any) (QueryResult, error) {
	argsJSON, err := encodeQueryArgs(args)
	if err != nil {
		return QueryResult{}, &DbQueryError{
			Code:    "arg_decode",
			Message: err.Error(),
		}
	}
	sqlPtr, sqlLen := stringRef(sql)
	argsPtr, argsLen := bytesRef(argsJSON)
	packed := hostDbQuery(sqlPtr, sqlLen, argsPtr, argsLen)
	envelope := readPacked(packed)
	return decodeDbQueryEnvelope(envelope)
}
