package guest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// fakeHost simula data_query: tabla de n filas con id 1..n, página máx 200,
// orden por id y `id > cursor`, como el host real.
type fakeHost struct {
	n        int
	reqs     []map[string]any
	override func(i int) []byte // si devuelve != nil, es la respuesta
}

func (f *fakeHost) call(req []byte) []byte {
	var r struct {
		Table   string         `json:"table"`
		Where   map[string]any `json:"where"`
		Limit   float64        `json:"limit"`
		OrderBy string         `json:"order_by"`
	}
	_ = json.Unmarshal(req, &r)
	var raw map[string]any
	_ = json.Unmarshal(req, &raw)
	f.reqs = append(f.reqs, raw)
	if f.override != nil {
		if b := f.override(len(f.reqs) - 1); b != nil {
			return b
		}
	}
	limit := int(r.Limit)
	if limit > 200 {
		limit = 200
	}
	after := 0
	if g, ok := r.Where["id"].(map[string]any); ok {
		after = int(g["gt"].(float64))
	}
	rows := []map[string]any{}
	for id := after + 1; id <= f.n && len(rows) < limit; id++ {
		rows = append(rows, map[string]any{"id": id, "k": r.Where["k"]})
	}
	b, _ := json.Marshal(map[string]any{"success": true, "data": map[string]any{"rows": rows}})
	return b
}

func TestQueryPagesExhausts(t *testing.T) {
	for _, n := range []int{0, 1, 199, 200, 201, 450, 600} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			h := &fakeHost{n: n}
			rows, absent, err := queryPages(h.call, "t", Q{Where: map[string]any{"k": "v"}}, "")
			if err != nil || absent != nil || len(rows) != n {
				t.Fatalf("rows=%d absent=%v err=%v", len(rows), absent, err)
			}
			for i, r := range rows {
				if r["id"].(json.Number).String() != fmt.Sprint(i+1) {
					t.Fatalf("orden roto en %d", i)
				}
			}
			for _, r := range h.reqs {
				if r["order_by"] != "id" || r["limit"].(float64) > 200 {
					t.Fatalf("request mal formado: %v", r)
				}
				if _, ok := r["on_absent"]; ok {
					t.Fatalf("Query no manda on_absent")
				}
			}
		})
	}
}

func TestQueryPagesMaxRows(t *testing.T) {
	h := &fakeHost{n: 450}
	rows, _, err := queryPages(h.call, "t", Q{MaxRows: 250}, "")
	if !errors.Is(err, ErrTruncated) || len(rows) != 250 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	// Tope exacto al tamaño de la tabla: no es truncado.
	h = &fakeHost{n: 250}
	rows, _, err = queryPages(h.call, "t", Q{MaxRows: 250}, "")
	if err != nil || len(rows) != 250 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	// Página pequeña: pide MaxRows+1.
	h = &fakeHost{n: 10}
	_, _, _ = queryPages(h.call, "t", Q{MaxRows: 5}, "")
	if h.reqs[0]["limit"].(float64) != 6 {
		t.Fatalf("limit = %v", h.reqs[0]["limit"])
	}
}

func TestQueryPagesRejects(t *testing.T) {
	h := &fakeHost{}
	for _, tc := range []struct {
		table string
		q     Q
	}{
		{"", Q{}},
		{"t", Q{MaxRows: -1}},
		{"t", Q{Where: map[string]any{"id": "x"}}},
	} {
		_, _, err := queryPages(h.call, tc.table, tc.q, "")
		var de *DataQueryError
		if !errors.As(err, &de) || de.Code != "invalid_query" {
			t.Fatalf("%+v: %v", tc, err)
		}
	}
	if len(h.reqs) != 0 {
		t.Fatal("no debía llamar al host")
	}
}

func TestQueryPagesRowWithoutID(t *testing.T) {
	h := &fakeHost{override: func(int) []byte {
		rows := make([]map[string]any, 200)
		for i := range rows {
			rows[i] = map[string]any{"x": i}
		}
		b, _ := json.Marshal(map[string]any{"success": true, "data": map[string]any{"rows": rows}})
		return b
	}}
	_, _, err := queryPages(h.call, "t", Q{}, "")
	var de *DataQueryError
	if !errors.As(err, &de) || de.Code != "invalid_query" {
		t.Fatalf("err = %v", err)
	}
}

func TestQueryPagesAbsence(t *testing.T) {
	envs := map[string]string{
		"nuevo": `{"success":false,"error":{"code":"source_unavailable","details":{"source":"t","reason":"addon_not_installed","degradable":true}}}`,
		"viejo": `{"success":false,"error":{"code":"db_error","message":"relation \"t\" does not exist (SQLSTATE 42P01)"}}`,
		"meta":  `{"success":true,"data":{"rows":[]},"meta":{"absent":{"reason":"addon_disabled"}}}`,
	}
	want := map[string]string{"nuevo": ReasonAddonNotInstalled, "viejo": ReasonTableMissing, "meta": ReasonAddonDisabled}
	for name, env := range envs {
		t.Run(name, func(t *testing.T) {
			h := &fakeHost{override: func(int) []byte { return []byte(env) }}
			rows, absent, err := queryPages(h.call, "t", Q{}, "empty")
			if err != nil || len(rows) != 0 || absent == nil || absent.Reason != want[name] || absent.Source != "t" {
				t.Fatalf("rows=%v absent=%+v err=%v", rows, absent, err)
			}
			if h.reqs[0]["on_absent"] != "empty" {
				t.Fatalf("falta on_absent: %v", h.reqs[0])
			}
		})
	}
}

func TestQueryPagesAbsenceMidPaginationIsAbsent(t *testing.T) {
	h := &fakeHost{n: 500, override: func(i int) []byte {
		if i == 1 {
			return []byte(`{"success":false,"error":{"code":"db_error","message":"x (SQLSTATE 42P01)"}}`)
		}
		return nil
	}}
	rows, absent, err := queryPages(h.call, "t", Q{}, "")
	if err != nil || absent == nil || rows != nil {
		t.Fatalf("rows=%d absent=%v err=%v", len(rows), absent, err)
	}
}

func TestQueryPagesHostErrors(t *testing.T) {
	h := &fakeHost{override: func(int) []byte {
		return []byte(`{"success":false,"error":{"code":"forbidden","message":"sin db:read"}}`)
	}}
	_, absent, err := queryPages(h.call, "t", Q{}, "empty")
	var de *DataQueryError
	if absent != nil || !errors.As(err, &de) || de.Code != "forbidden" || de.Absent != nil {
		t.Fatalf("absent=%v err=%v", absent, err)
	}
	// db_error transitorio (sin SQLSTATE de ausencia) NO se degrada.
	h = &fakeHost{override: func(int) []byte {
		return []byte(`{"success":false,"error":{"code":"db_error","message":"context deadline exceeded"}}`)
	}}
	_, absent, err = queryPages(h.call, "t", Q{}, "empty")
	if absent != nil || !errors.As(err, &de) || de.Code != "db_error" {
		t.Fatalf("absent=%v err=%v", absent, err)
	}
	// Respuesta ilegible.
	h = &fakeHost{override: func(int) []byte { return []byte("zzz") }}
	_, _, err = queryPages(h.call, "t", Q{}, "")
	if !errors.As(err, &de) || de.Code != "decode" {
		t.Fatalf("err = %v", err)
	}
}

func TestDataQueryErrorUnwrapsAbsence(t *testing.T) {
	var err error = &DataQueryError{Code: CodeSourceUnavailable, Absent: &Absence{Source: "t", Reason: ReasonAddonNotInstalled}}
	var a *Absence
	if !errors.As(err, &a) || a.Reason != ReasonAddonNotInstalled {
		t.Fatal("errors.As(*Absence) falló")
	}
}

func TestDecodeDataQueryPageArrayShape(t *testing.T) {
	rows, _, err := decodeDataQueryPage("t", []byte(`{"success":true,"data":[{"id":1}]}`))
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
}

func TestQueryPagesNoIDColumnIsUsageError(t *testing.T) {
	for _, env := range []string{
		`{"success":false,"error":{"code":"source_unavailable","message":"ERROR: column \"id\" does not exist (SQLSTATE 42703)","details":{"source":"t","reason":"schema_mismatch","sqlstate":"42703","degradable":true}}}`,
		`{"success":false,"error":{"code":"db_error","message":"ERROR: column t.id does not exist (SQLSTATE 42703)"}}`,
	} {
		h := &fakeHost{override: func(int) []byte { return []byte(env) }}
		rows, absent, err := queryPages(h.call, "t", Q{}, "empty")
		var de *DataQueryError
		if absent != nil || rows != nil || !errors.As(err, &de) || de.Code != "invalid_query" {
			t.Fatalf("absent=%v err=%v", absent, err)
		}
	}
	// 42703 de OTRA columna sigue siendo ausencia (schema_mismatch).
	h := &fakeHost{override: func(int) []byte {
		return []byte(`{"success":false,"error":{"code":"db_error","message":"ERROR: column \"status\" does not exist (SQLSTATE 42703)"}}`)
	}}
	_, absent, err := queryPages(h.call, "t", Q{}, "empty")
	if err != nil || absent == nil || absent.Reason != ReasonSchemaMismatch {
		t.Fatalf("absent=%v err=%v", absent, err)
	}
}

func TestOptionalResultNonDegradableIsError(t *testing.T) {
	a := &Absence{Source: "t", Reason: ReasonSchemaMismatch, SQLState: "42703", Degradable: false, Message: "m"}
	res, err := optionalResult(nil, a, nil)
	var de *DataQueryError
	var abs *Absence
	if !errors.As(err, &de) || !errors.As(err, &abs) || res.Absent != nil {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	a.Degradable = true
	res, err = optionalResult(nil, a, nil)
	if err != nil || res.Absent == nil {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	// Via el sobre real: source_unavailable degradable:false.
	h := &fakeHost{override: func(int) []byte {
		return []byte(`{"success":false,"error":{"code":"source_unavailable","details":{"source":"t","reason":"schema_mismatch","sqlstate":"42703","degradable":false}}}`)
	}}
	if _, err := optionalResult(queryPages(h.call, "t", Q{}, "empty")); err == nil {
		t.Fatal("esperaba error")
	}
}

func TestQueryPagesLargeIDsKeepPrecision(t *testing.T) {
	const big = "9007199254740993" // 2^53 + 1
	var seen []string
	calls := 0
	call := func(req []byte) []byte {
		calls++
		var r struct {
			Where map[string]any `json:"where"`
		}
		d := json.NewDecoder(bytes.NewReader(req))
		d.UseNumber()
		_ = d.Decode(&r)
		if g, ok := r.Where["id"].(map[string]any); ok {
			seen = append(seen, fmt.Sprint(g["gt"]))
			return []byte(`{"success":true,"data":{"rows":[]}}`)
		}
		rows := make([]string, 200)
		for i := range rows {
			rows[i] = fmt.Sprintf(`{"id":%d}`, 9007199254740993+i)
		}
		return []byte(`{"success":true,"data":{"rows":[` + strings.Join(rows, ",") + `]}}`)
	}
	rows, _, err := queryPages(call, "t", Q{}, "")
	if err != nil || len(rows) != 200 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	if got := fmt.Sprint(rows[0]["id"]); got != big {
		t.Fatalf("id = %s, quería %s", got, big)
	}
	if len(seen) != 1 || seen[0] != "9007199254741192" {
		t.Fatalf("cursor = %v", seen)
	}
}

func TestQueryPagesCursorMustAdvance(t *testing.T) {
	call := func([]byte) []byte {
		rows := make([]map[string]any, 200)
		for i := range rows {
			rows[i] = map[string]any{"id": 7}
		}
		b, _ := json.Marshal(map[string]any{"success": true, "data": map[string]any{"rows": rows}})
		return b
	}
	_, _, err := queryPages(call, "t", Q{}, "")
	var de *DataQueryError
	if !errors.As(err, &de) || de.Code != "invalid_query" || !strings.Contains(de.Message, "did not advance") {
		t.Fatalf("err = %v", err)
	}
}

func TestQueryPagesNoIDColumnViaMetaAbsentWithoutMessage(t *testing.T) {
	for name, tc := range map[string]struct {
		probe   string
		wantUse bool
	}{
		"id":        {`{"success":false,"error":{"code":"db_error","message":"ERROR: column \"id\" does not exist (SQLSTATE 42703)"}}`, true},
		"otra":      {`{"success":false,"error":{"code":"db_error","message":"ERROR: column \"status\" does not exist (SQLSTATE 42703)"}}`, false},
		"sin texto": {`{"success":false,"error":{"code":"db_error"}}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			h := &fakeHost{}
			h.override = func(int) []byte {
				if _, ok := h.reqs[len(h.reqs)-1]["on_absent"]; ok {
					return []byte(`{"success":true,"data":{"rows":[]},"meta":{"absent":{"source":"t","reason":"schema_mismatch","sqlstate":"42703"}}}`)
				}
				return []byte(tc.probe)
			}
			_, absent, err := queryPages(h.call, "t", Q{}, "empty")
			var de *DataQueryError
			if tc.wantUse {
				if absent != nil || !errors.As(err, &de) || de.Code != "invalid_query" {
					t.Fatalf("absent=%v err=%v", absent, err)
				}
			} else if err != nil || absent == nil || absent.Reason != ReasonSchemaMismatch {
				t.Fatalf("absent=%v err=%v", absent, err)
			}
			if len(h.reqs) != 2 {
				t.Fatalf("esperaba 1 repeticion, hubo %d peticiones", len(h.reqs))
			}
			if _, ok := h.reqs[1]["on_absent"]; ok {
				t.Fatal("la repeticion no debe llevar on_absent")
			}
		})
	}
}

func TestQueryPagesMetaAbsentWithMessageDoesNotRetry(t *testing.T) {
	h := &fakeHost{override: func(int) []byte {
		return []byte(`{"success":true,"data":{"rows":[]},"meta":{"absent":{"source":"t","reason":"schema_mismatch","sqlstate":"42703","message":"ERROR: column \"id\" does not exist (SQLSTATE 42703)"}}}`)
	}}
	_, _, err := queryPages(h.call, "t", Q{}, "empty")
	var de *DataQueryError
	if !errors.As(err, &de) || de.Code != "invalid_query" || len(h.reqs) != 1 {
		t.Fatalf("err=%v reqs=%d", err, len(h.reqs))
	}
}
