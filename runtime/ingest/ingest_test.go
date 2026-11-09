package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// memStore is an in-memory Store keyed by the "instance_id" column.
type memStore struct {
	rows    map[string]map[string]any
	deleted map[string]bool
	failOn  map[string]error // instance_id -> error returned by Create/Update
	fatal   error            // returned by Find when set
}

func newMem() *memStore {
	return &memStore{rows: map[string]map[string]any{}, deleted: map[string]bool{}, failOn: map[string]error{}}
}

func (m *memStore) Find(_ context.Context, key map[string]any) (map[string]any, bool, error) {
	if m.fatal != nil {
		return nil, false, m.fatal
	}
	id := fmt.Sprint(key["instance_id"])
	r, ok := m.rows[id]
	if !ok || m.deleted[id] {
		return nil, false, nil
	}
	return r, true, nil
}

func (m *memStore) Create(_ context.Context, v map[string]any) error {
	id := fmt.Sprint(v["instance_id"])
	if err := m.failOn[id]; err != nil {
		return err
	}
	m.rows[id] = clone(v)
	return nil
}

func (m *memStore) Update(_ context.Context, existing, v map[string]any) error {
	id := fmt.Sprint(existing["instance_id"])
	if err := m.failOn[id]; err != nil {
		return err
	}
	for c, x := range v {
		m.rows[id][c] = x
	}
	return nil
}

func (m *memStore) Delete(_ context.Context, existing map[string]any) error {
	m.deleted[fmt.Sprint(existing["instance_id"])] = true
	return nil
}

func clone(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func spec() v3.IngestSpec {
	return v3.IngestSpec{
		Key:       []string{"instance_id"},
		Cursor:    "source_seq",
		Fields:    []string{"name", "license_status", "source_seq"},
		OnDelete:  "soft",
		MaxBatch:  3,
		Exclusive: true,
	}
}

func decode(t *testing.T, s v3.IngestSpec, body string) Batch {
	t.Helper()
	b, err := Decode([]byte(body), s)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	return b
}

func TestApply_CreateUpdateStaleDelete(t *testing.T) {
	st := newMem()
	s := spec()
	ctx := context.Background()

	b := decode(t, s, `{"items":[
	  {"op":"upsert","cursor":10,"data":{"instance_id":"i1","name":"A","license_status":"active"}},
	  {"op":"upsert","cursor":11,"data":{"instance_id":"i2","name":"B"}}]}`)
	res, err := Apply(ctx, s, b, st)
	if err != nil || res.Applied != 2 || res.Rejected != 0 {
		t.Fatalf("create: %+v err=%v", res, err)
	}
	if fmt.Sprint(res.HighWater) != "11" {
		t.Fatalf("high water = %v", res.HighWater)
	}

	// Equal and lower cursors are stale; a higher one updates.
	b = decode(t, s, `{"items":[
	  {"op":"upsert","cursor":10,"data":{"instance_id":"i1","name":"OLD"}},
	  {"op":"upsert","cursor":9,"data":{"instance_id":"i1","name":"OLDER"}},
	  {"op":"upsert","cursor":12,"data":{"instance_id":"i1","name":"NEW"}}]}`)
	res, err = Apply(ctx, s, b, st)
	if err != nil || res.Applied != 1 || res.SkippedStale != 2 {
		t.Fatalf("update/stale: %+v err=%v", res, err)
	}
	if st.rows["i1"]["name"] != "NEW" || fmt.Sprint(st.rows["i1"]["source_seq"]) != "12" {
		t.Fatalf("row not updated: %#v", st.rows["i1"])
	}

	// Delete: stale one is skipped, newer one soft-deletes, a missing row is a no-op.
	b = decode(t, s, `{"items":[
	  {"op":"delete","cursor":5,"key":{"instance_id":"i1"}},
	  {"op":"delete","cursor":13,"key":{"instance_id":"i1"}},
	  {"op":"delete","cursor":14,"key":{"instance_id":"ghost"}}]}`)
	res, err = Apply(ctx, s, b, st)
	if err != nil || res.Deleted != 1 || res.SkippedStale != 1 || res.Rejected != 0 {
		t.Fatalf("delete: %+v err=%v", res, err)
	}
	if !st.deleted["i1"] {
		t.Fatal("i1 must be soft-deleted")
	}
	if fmt.Sprint(res.HighWater) != "13" {
		t.Fatalf("high water = %v", res.HighWater)
	}
}

func TestApply_MixedBatchRejectsOnlyTheBadItems(t *testing.T) {
	st := newMem()
	st.failOn["bad"] = ErrConstraint
	st.failOn["inv"] = ErrValidation
	s := spec()
	s.MaxBatch = 20
	b := decode(t, s, `{"items":[
	  {"op":"upsert","cursor":1,"data":{"instance_id":"ok","name":"A"}},
	  {"op":"upsert","cursor":2,"data":{"instance_id":"x","secret":"no"}},
	  {"op":"upsert","cursor":3,"data":{"instance_id":"x","organization_id":"other"}},
	  {"op":"upsert","cursor":4,"data":{"name":"nokey"}},
	  {"op":"upsert","data":{"instance_id":"nocursor"}},
	  {"op":"upsert","cursor":"abc","data":{"instance_id":"badcursor"}},
	  {"op":"upsert","cursor":7,"data":{"instance_id":"bad"}},
	  {"op":"upsert","cursor":8,"data":{"instance_id":"inv"}},
	  {"op":"delete","key":{}},
	  {"op":"explode"},
	  {"op":"upsert","cursor":9}]}`)
	res, err := Apply(context.Background(), s, b, st)
	if err != nil {
		t.Fatalf("item problems must not abort: %v", err)
	}
	if res.Applied != 1 || res.Rejected != 10 {
		t.Fatalf("counts: %+v", res)
	}
	type ce struct {
		idx   int
		code  string
		field string
	}
	want := []ce{
		{1, CodeUnknownField, "secret"}, {2, CodeUnknownField, "organization_id"},
		{3, CodeMissingKey, "instance_id"}, {4, CodeValidation, "source_seq"},
		{5, CodeValidation, "source_seq"}, {6, CodeConstraintViolation, ""},
		{7, CodeValidation, ""}, {8, CodeMissingKey, "instance_id"},
		{9, CodeValidation, "op"}, {10, CodeValidation, "data"},
	}
	if len(res.Errors) != len(want) {
		t.Fatalf("errors: %+v", res.Errors)
	}
	for i, w := range want {
		g := res.Errors[i]
		if g.Index != w.idx || g.Code != w.code || g.Field != w.field {
			t.Fatalf("error[%d] = %+v, want %+v", i, g, w)
		}
	}
	if _, ok := st.rows["ok"]; !ok {
		t.Fatal("the valid item must still be applied")
	}
}

func TestApply_ManagedColumnsNeverWritable(t *testing.T) {
	s := v3.IngestSpec{Key: []string{"instance_id"}} // no Fields = every non-managed column
	st := newMem()
	for _, col := range []string{"id", "organization_id", "created_at", "updated_by_id", "deleted_at"} {
		b := Batch{Items: []Item{{Op: OpUpsert, Data: map[string]any{"instance_id": "i", col: "x"}}}}
		res, err := Apply(context.Background(), s, b, st)
		if err != nil || res.Rejected != 1 || res.Errors[0].Code != CodeUnknownField || res.Errors[0].Field != col {
			t.Fatalf("%s: %+v err=%v", col, res, err)
		}
	}
	b := Batch{Items: []Item{{Op: OpUpsert, Data: map[string]any{"instance_id": "i", "anything": "ok"}}}}
	if res, _ := Apply(context.Background(), s, b, st); res.Applied != 1 {
		t.Fatalf("a non-managed column is writable when Fields is empty: %+v", res)
	}
}

func TestApply_WithoutCursorLastWriteWins(t *testing.T) {
	s := v3.IngestSpec{Key: []string{"instance_id"}, Fields: []string{"name"}}
	st := newMem()
	b := Batch{Items: []Item{
		{Op: OpUpsert, Data: map[string]any{"instance_id": "i", "name": "1"}},
		{Op: OpUpsert, Data: map[string]any{"instance_id": "i", "name": "2"}},
	}}
	res, err := Apply(context.Background(), s, b, st)
	if err != nil || res.Applied != 2 || res.SkippedStale != 0 || res.HighWater != nil {
		t.Fatalf("%+v err=%v", res, err)
	}
	if st.rows["i"]["name"] != "2" {
		t.Fatalf("last write must win: %#v", st.rows["i"])
	}
}

func TestApply_OnDeleteIgnore(t *testing.T) {
	s := spec()
	s.OnDelete = "ignore"
	st := newMem()
	st.rows["i1"] = map[string]any{"instance_id": "i1"}
	b := Batch{Items: []Item{{Op: OpDelete, Cursor: json.Number("5"), Key: map[string]any{"instance_id": "i1"}}}}
	res, err := Apply(context.Background(), s, b, st)
	if err != nil || res.Deleted != 0 || st.deleted["i1"] {
		t.Fatalf("on_delete ignore must not delete: %+v err=%v", res, err)
	}
}

func TestApply_InfrastructureErrorAborts(t *testing.T) {
	st := newMem()
	st.fatal = errors.New("db down")
	b := Batch{Items: []Item{{Op: OpUpsert, Cursor: json.Number("1"), Data: map[string]any{"instance_id": "i"}}}}
	if _, err := Apply(context.Background(), spec(), b, st); err == nil || !strings.Contains(err.Error(), "db down") {
		t.Fatalf("want the store error, got %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Apply(ctx, spec(), b, newMem()); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestApply_TimestampCursor(t *testing.T) {
	s := v3.IngestSpec{Key: []string{"instance_id"}, Cursor: "seen_at"}
	st := newMem()
	ctx := context.Background()
	b := Batch{Items: []Item{{Op: OpUpsert, Cursor: "2026-10-22T18:00:00Z", Data: map[string]any{"instance_id": "i"}}}}
	if res, err := Apply(ctx, s, b, st); err != nil || res.Applied != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	// Same instant written with another offset is not newer.
	b = Batch{Items: []Item{{Op: OpUpsert, Cursor: "2026-10-22T19:00:00+01:00", Data: map[string]any{"instance_id": "i"}}}}
	if res, err := Apply(ctx, s, b, st); err != nil || res.SkippedStale != 1 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestCompareCursor(t *testing.T) {
	if c, err := CompareCursor(json.Number("9007199254740993"), json.Number("9007199254740992")); err != nil || c != 1 {
		t.Fatalf("64-bit cursors must keep precision: %d %v", c, err)
	}
	if c, _ := CompareCursor(1, int64(1)); c != 0 {
		t.Fatalf("equal ints: %d", c)
	}
	if _, err := CompareCursor(1, "2026-10-22T18:00:00Z"); err == nil {
		t.Fatal("integer vs instant must error")
	}
	if _, err := CompareCursor(1.5, 1); err == nil {
		t.Fatal("non-integer float must error")
	}
}

func TestDecode_Limits(t *testing.T) {
	s := spec() // MaxBatch 3
	if _, err := Decode([]byte(`{"items":[{"op":"upsert"},{"op":"upsert"},{"op":"upsert"},{"op":"upsert"}]}`), s); !errors.Is(err, ErrBatchTooLarge) {
		t.Fatalf("want ErrBatchTooLarge, got %v", err)
	}
	if _, err := Decode(make([]byte, MaxBodyBytes+1), s); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("want ErrBodyTooLarge, got %v", err)
	}
	for name, body := range map[string]string{
		"not json":      `nope`,
		"unknown key":   `{"items":[{"op":"upsert"}],"organization_id":"x"}`,
		"empty items":   `{"items":[]}`,
		"trailing data": `{"items":[{"op":"upsert"}]} {}`,
	} {
		if _, err := Decode([]byte(body), s); !errors.Is(err, ErrBadBody) {
			t.Fatalf("%s: want ErrBadBody, got %v", name, err)
		}
	}
	if _, err := Decode([]byte(`{"items":[{"op":"upsert"}]}`), v3.IngestSpec{}); !errors.Is(err, ErrBadSpec) {
		t.Fatalf("want ErrBadSpec for an empty key, got %v", err)
	}
}

func TestMaxBatch(t *testing.T) {
	for in, want := range map[int]int{0: 500, -1: 500, 10: 10, 1000: 1000, 5000: 1000} {
		if got := MaxBatch(v3.IngestSpec{MaxBatch: in}); got != want {
			t.Fatalf("MaxBatch(%d)=%d want %d", in, got, want)
		}
	}
}
