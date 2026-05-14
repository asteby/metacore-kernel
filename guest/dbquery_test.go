package guest

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestDecodeDbQueryEnvelope_SuccessHappyPath(t *testing.T) {
	raw := mustMarshalAny(t, map[string]any{
		"success": true,
		"data": map[string]any{
			"rows": []map[string]any{
				{"id": float64(1), "title": "first"},
				{"id": float64(2), "title": "second"},
			},
			"rowCount": 2,
			"columns": []map[string]any{
				{"name": "id", "type": "int8"},
				{"name": "title", "type": "text"},
			},
		},
		"meta": map[string]any{
			"schema":     "addon_tickets",
			"durationMs": 7,
			"truncated":  false,
		},
	})
	got, err := decodeDbQueryEnvelope(raw)
	if err != nil {
		t.Fatalf("decodeDbQueryEnvelope: %v", err)
	}
	if got.RowCount != 2 {
		t.Errorf("RowCount = %d, want 2", got.RowCount)
	}
	if len(got.Rows) != 2 {
		t.Fatalf("Rows = %d, want 2", len(got.Rows))
	}
	if got.Rows[0]["title"] != "first" {
		t.Errorf("row[0].title = %v, want first", got.Rows[0]["title"])
	}
	if len(got.Columns) != 2 || got.Columns[0].Name != "id" || got.Columns[0].Type != "int8" {
		t.Errorf("Columns mismatch: %+v", got.Columns)
	}
	if got.Meta.Schema != "addon_tickets" {
		t.Errorf("Meta.Schema = %q, want addon_tickets", got.Meta.Schema)
	}
	if got.Meta.DurationMs != 7 {
		t.Errorf("Meta.DurationMs = %d, want 7", got.Meta.DurationMs)
	}
}

func TestDecodeDbQueryEnvelope_ForbiddenIsTypedError(t *testing.T) {
	raw := mustMarshalAny(t, map[string]any{
		"success": false,
		"error": map[string]any{
			"code":    "forbidden",
			"message": "addon \"tickets\" lacks db:read \"billing.invoices\"",
		},
		"meta": map[string]any{
			"schema":     "addon_tickets",
			"durationMs": 1,
		},
	})
	_, err := decodeDbQueryEnvelope(raw)
	if err == nil {
		t.Fatal("expected error envelope to surface")
	}
	var qErr *DbQueryError
	if !errors.As(err, &qErr) {
		t.Fatalf("not *DbQueryError: %T", err)
	}
	if qErr.Code != "forbidden" {
		t.Errorf("Code = %q, want forbidden", qErr.Code)
	}
	if qErr.Meta.Schema != "addon_tickets" {
		t.Errorf("Meta.Schema = %q, want addon_tickets", qErr.Meta.Schema)
	}
}

func TestDecodeDbQueryEnvelope_EmptyRowsIsSuccess(t *testing.T) {
	raw := mustMarshalAny(t, map[string]any{
		"success": true,
		"data": map[string]any{
			"rows":     []map[string]any{},
			"rowCount": 0,
			"columns": []map[string]any{
				{"name": "id", "type": "int8"},
			},
		},
		"meta": map[string]any{
			"schema":     "addon_tickets",
			"durationMs": 2,
		},
	})
	got, err := decodeDbQueryEnvelope(raw)
	if err != nil {
		t.Fatalf("decodeDbQueryEnvelope: %v", err)
	}
	if got.RowCount != 0 {
		t.Errorf("RowCount = %d, want 0", got.RowCount)
	}
	if got.Rows == nil {
		// Empty slice vs nil — JSON decoded `[]` to a non-nil empty
		// slice, which is what consumers want.
		t.Errorf("Rows is nil, want non-nil empty slice")
	}
	if len(got.Columns) != 1 {
		t.Errorf("Columns = %d, want 1", len(got.Columns))
	}
}

func TestDecodeDbQueryEnvelope_EmptyBufferIsTypedDbError(t *testing.T) {
	// Host always allocates an envelope for db_query (see § 9.1).
	// Empty buffer is a wire anomaly — surface as typed `db_error`.
	_, err := decodeDbQueryEnvelope(nil)
	if err == nil {
		t.Fatal("expected error on empty buffer")
	}
	var qErr *DbQueryError
	if !errors.As(err, &qErr) {
		t.Fatalf("not *DbQueryError: %T", err)
	}
	if qErr.Code != "db_error" {
		t.Errorf("Code = %q, want db_error", qErr.Code)
	}
}

func TestDecodeDbQueryEnvelope_RowLimitExceeded(t *testing.T) {
	raw := mustMarshalAny(t, map[string]any{
		"success": false,
		"error": map[string]any{
			"code":    "row_limit_exceeded",
			"message": "result set exceeds 10000 rows",
		},
		"meta": map[string]any{
			"schema":     "addon_reports",
			"durationMs": 950,
		},
	})
	_, err := decodeDbQueryEnvelope(raw)
	var qErr *DbQueryError
	if !errors.As(err, &qErr) {
		t.Fatalf("not *DbQueryError: %T", err)
	}
	if qErr.Code != "row_limit_exceeded" {
		t.Errorf("Code = %q, want row_limit_exceeded", qErr.Code)
	}
}

func TestDecodeDbQueryEnvelope_ErrorWithoutCodeFallsBackToUnknown(t *testing.T) {
	raw := mustMarshalAny(t, map[string]any{
		"success": false,
		"meta":    map[string]any{"schema": "addon_x"},
	})
	_, err := decodeDbQueryEnvelope(raw)
	var qErr *DbQueryError
	if !errors.As(err, &qErr) {
		t.Fatalf("not *DbQueryError: %T", err)
	}
	if qErr.Code != "unknown" {
		t.Errorf("Code = %q, want unknown", qErr.Code)
	}
}

func TestDecodeDbQueryEnvelope_MalformedJSON(t *testing.T) {
	_, err := decodeDbQueryEnvelope([]byte(`{not json`))
	if err == nil {
		t.Fatal("expected decode error")
	}
	var qErr *DbQueryError
	if errors.As(err, &qErr) {
		t.Errorf("malformed JSON surfaced as DbQueryError: %v", qErr)
	}
}

func TestEncodeQueryArgs_NilIsZeroBuffer(t *testing.T) {
	buf, err := encodeQueryArgs(nil)
	if err != nil {
		t.Fatalf("encodeQueryArgs(nil): %v", err)
	}
	if buf != nil {
		t.Errorf("nil args produced %q, want nil buffer", string(buf))
	}
}

func TestEncodeQueryArgs_RoundTrip(t *testing.T) {
	buf, err := encodeQueryArgs([]any{1, "two", true, nil})
	if err != nil {
		t.Fatalf("encodeQueryArgs: %v", err)
	}
	var decoded []any
	if err := json.Unmarshal(buf, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded) != 4 {
		t.Fatalf("decoded = %v, want 4 items", decoded)
	}
	if decoded[1] != "two" || decoded[2] != true {
		t.Errorf("round-trip mismatch: %v", decoded)
	}
}

func TestDbQueryError_Error(t *testing.T) {
	var nilErr *DbQueryError
	if got := nilErr.Error(); got == "" {
		t.Errorf("nil receiver returned empty string")
	}
	e := &DbQueryError{Code: "forbidden", Message: "no cap"}
	if got := e.Error(); got != "db_query: forbidden: no cap" {
		t.Errorf("unexpected: %q", got)
	}
	e2 := &DbQueryError{Code: "query_timeout"}
	if got := e2.Error(); got != "db_query: query_timeout" {
		t.Errorf("unexpected: %q", got)
	}
}
