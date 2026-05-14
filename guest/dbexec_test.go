package guest

import (
	"errors"
	"testing"
)

func TestDecodeDbExecEnvelope_PlainMutationOnlyRowsAffected(t *testing.T) {
	// Pre-RETURNING shape: `data.rowsAffected` only, `rows` /
	// `columns` omitted. Helper must decode without surfacing a nil-
	// slice surprise.
	raw := mustMarshalAny(t, map[string]any{
		"success": true,
		"data":    map[string]any{"rowsAffected": 5},
		"meta":    map[string]any{"schema": "addon_tickets", "durationMs": 4},
	})
	got, err := decodeDbExecEnvelope(raw)
	if err != nil {
		t.Fatalf("decodeDbExecEnvelope: %v", err)
	}
	if got.RowsAffected != 5 {
		t.Errorf("RowsAffected = %d, want 5", got.RowsAffected)
	}
	if got.Rows != nil {
		t.Errorf("Rows = %v, want nil (no RETURNING)", got.Rows)
	}
	if got.Columns != nil {
		t.Errorf("Columns = %v, want nil (no RETURNING)", got.Columns)
	}
	if got.Meta.Schema != "addon_tickets" {
		t.Errorf("Meta.Schema = %q, want addon_tickets", got.Meta.Schema)
	}
}

func TestDecodeDbExecEnvelope_WithReturningRowsAndColumns(t *testing.T) {
	// Post-v0.11 shape with RETURNING projection.
	raw := mustMarshalAny(t, map[string]any{
		"success": true,
		"data": map[string]any{
			"rowsAffected": 1,
			"rows": []map[string]any{
				{"id": "abc", "status": "refunded"},
			},
			"columns": []map[string]any{
				{"name": "id", "type": "uuid"},
				{"name": "status", "type": "text"},
			},
		},
		"meta": map[string]any{"schema": "addon_refunds", "durationMs": 11},
	})
	got, err := decodeDbExecEnvelope(raw)
	if err != nil {
		t.Fatalf("decodeDbExecEnvelope: %v", err)
	}
	if got.RowsAffected != 1 {
		t.Errorf("RowsAffected = %d, want 1", got.RowsAffected)
	}
	if len(got.Rows) != 1 || got.Rows[0]["status"] != "refunded" {
		t.Errorf("Rows mismatch: %+v", got.Rows)
	}
	if len(got.Columns) != 2 || got.Columns[1].Name != "status" {
		t.Errorf("Columns mismatch: %+v", got.Columns)
	}
}

func TestDecodeDbExecEnvelope_ConstraintViolationPreservesSQLState(t *testing.T) {
	// § 10.4: driver-level violations preserve SQLSTATE in
	// `error.sqlstate`. The helper exposes it on `eErr.SQLState`.
	raw := mustMarshalAny(t, map[string]any{
		"success": false,
		"error": map[string]any{
			"code":     "constraint_violation",
			"message":  "duplicate key value violates unique constraint",
			"sqlstate": "23505",
		},
		"meta": map[string]any{"schema": "addon_tickets", "durationMs": 3},
	})
	_, err := decodeDbExecEnvelope(raw)
	var eErr *DbExecError
	if !errors.As(err, &eErr) {
		t.Fatalf("not *DbExecError: %T", err)
	}
	if eErr.Code != "constraint_violation" {
		t.Errorf("Code = %q, want constraint_violation", eErr.Code)
	}
	if eErr.SQLState != "23505" {
		t.Errorf("SQLState = %q, want 23505", eErr.SQLState)
	}
}

func TestDecodeDbExecEnvelope_SerializationFailureForRetry(t *testing.T) {
	raw := mustMarshalAny(t, map[string]any{
		"success": false,
		"error": map[string]any{
			"code":     "serialization_failure",
			"message":  "could not serialize access due to concurrent update",
			"sqlstate": "40001",
		},
		"meta": map[string]any{"schema": "addon_orders", "durationMs": 22},
	})
	_, err := decodeDbExecEnvelope(raw)
	var eErr *DbExecError
	if !errors.As(err, &eErr) {
		t.Fatalf("not *DbExecError: %T", err)
	}
	if eErr.Code != "serialization_failure" {
		t.Errorf("Code = %q, want serialization_failure", eErr.Code)
	}
	if eErr.SQLState != "40001" {
		t.Errorf("SQLState = %q, want 40001", eErr.SQLState)
	}
}

func TestDecodeDbExecEnvelope_MissingOrgFilterIsTypedError(t *testing.T) {
	raw := mustMarshalAny(t, map[string]any{
		"success": false,
		"error": map[string]any{
			"code":    "missing_org_filter",
			"message": "UPDATE must constrain org_id",
		},
		"meta": map[string]any{"schema": "addon_tickets"},
	})
	_, err := decodeDbExecEnvelope(raw)
	var eErr *DbExecError
	if !errors.As(err, &eErr) {
		t.Fatalf("not *DbExecError: %T", err)
	}
	if eErr.Code != "missing_org_filter" {
		t.Errorf("Code = %q, want missing_org_filter", eErr.Code)
	}
}

func TestDecodeDbExecEnvelope_NoActiveTxIsTypedError(t *testing.T) {
	raw := mustMarshalAny(t, map[string]any{
		"success": false,
		"error": map[string]any{
			"code":    "no_active_tx",
			"message": "db_exec requires Host.InvokeInTx",
		},
		"meta": map[string]any{"schema": "addon_tickets"},
	})
	_, err := decodeDbExecEnvelope(raw)
	var eErr *DbExecError
	if !errors.As(err, &eErr) {
		t.Fatalf("not *DbExecError: %T", err)
	}
	if eErr.Code != "no_active_tx" {
		t.Errorf("Code = %q, want no_active_tx", eErr.Code)
	}
}

func TestDecodeDbExecEnvelope_EmptyBufferIsTypedDbError(t *testing.T) {
	_, err := decodeDbExecEnvelope(nil)
	var eErr *DbExecError
	if !errors.As(err, &eErr) {
		t.Fatalf("not *DbExecError: %T", err)
	}
	if eErr.Code != "db_error" {
		t.Errorf("Code = %q, want db_error", eErr.Code)
	}
}

func TestDecodeDbExecEnvelope_ErrorWithoutCodeFallsBackToUnknown(t *testing.T) {
	raw := mustMarshalAny(t, map[string]any{
		"success": false,
		"meta":    map[string]any{"schema": "addon_x"},
	})
	_, err := decodeDbExecEnvelope(raw)
	var eErr *DbExecError
	if !errors.As(err, &eErr) {
		t.Fatalf("not *DbExecError: %T", err)
	}
	if eErr.Code != "unknown" {
		t.Errorf("Code = %q, want unknown", eErr.Code)
	}
}

func TestDbExecError_Error(t *testing.T) {
	var nilErr *DbExecError
	if got := nilErr.Error(); got == "" {
		t.Errorf("nil receiver returned empty string")
	}
	e := &DbExecError{Code: "constraint_violation", Message: "dup"}
	if got := e.Error(); got != "db_exec: constraint_violation: dup" {
		t.Errorf("unexpected: %q", got)
	}
	e2 := &DbExecError{Code: "no_active_tx"}
	if got := e2.Error(); got != "db_exec: no_active_tx" {
		t.Errorf("unexpected: %q", got)
	}
}
