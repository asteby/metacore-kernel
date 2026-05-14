//go:build wasm || wasip1

package guest

// hostDbExec is the raw host import. Signature mirrors
// docs/wasm-abi.md § 10.1.
//
//go:wasmimport metacore_host db_exec
func hostDbExec(sqlPtr, sqlLen, argsPtr, argsLen uint32) uint64

// DbExec runs a mutating SQL statement (`INSERT`/`UPDATE`/`DELETE`/
// `MERGE`) scoped to the addon's schema. The call piggybacks on the
// action handler's open transaction when the host entered through
// `Host.InvokeInTx` (§ 10.6); a guest invoked via plain `Host.Invoke`
// receives `no_active_tx`.
//
// Returns the decoded `ExecResult` on success — `RowsAffected` for
// every statement, plus `Rows` / `Columns` when the SQL carries a
// `RETURNING` clause (kernel v0.11.0+). On failure returns a typed
// `*DbExecError` whose `Code` field is one of the documented codes
// (§ 10.4). For driver-level violations (`constraint_violation`,
// `serialization_failure`, `db_error`) the host preserves the
// SQLSTATE in `eErr.SQLState`.
//
// Args use Postgres-style positional placeholders (`$1`, `$2`, …);
// the encoding matches § 9.6 verbatim (db_exec reuses db_query's arg
// codec).
//
// Memory contract: the SQL string and args buffer are read
// synchronously by the host before this function returns; both are
// safe to reuse.
func DbExec(sql string, args ...any) (ExecResult, error) {
	argsJSON, err := encodeQueryArgs(args)
	if err != nil {
		return ExecResult{}, &DbExecError{
			Code:    "arg_decode",
			Message: err.Error(),
		}
	}
	sqlPtr, sqlLen := stringRef(sql)
	argsPtr, argsLen := bytesRef(argsJSON)
	packed := hostDbExec(sqlPtr, sqlLen, argsPtr, argsLen)
	envelope := readPacked(packed)
	return decodeDbExecEnvelope(envelope)
}
