// Package ingest is the host-neutral engine of the model upsert sink
// (RFC-04, docs/rfcs/2026-10-22-hub-primitivos): a signed batch of
// {op, cursor, data|key} items is decoded, checked against the model's
// `ingest` block (v3.IngestSpec) and applied through a small Store the host
// implements on top of its own data service.
//
// What lives here: body decoding with the size and batch limits, the key /
// field allowlist, the cursor staleness rule, the counters and the per-item
// error codes. What does NOT: signature verification (webhookin), HTTP, the
// database and its transaction. The host opens one transaction per batch (and
// a SAVEPOINT per item if its Store needs one) and passes a Store bound to it
// and to the organization taken from the route, never from the body.
//
// Contract (stable): Decode, Apply, CompareCursor, Batch, Item, Result,
// ItemError, Store and the error codes.
package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// Limits of RFC-04 section 2.1.
const (
	// MaxBodyBytes is the largest accepted request body (1 MiB).
	MaxBodyBytes = 1 << 20
	// DefaultMaxBatch applies when IngestSpec.MaxBatch is 0.
	DefaultMaxBatch = 500
	// HardMaxBatch is the ceiling of IngestSpec.MaxBatch.
	HardMaxBatch = 1000
)

// Item operations.
const (
	OpUpsert = "upsert"
	OpDelete = "delete"
)

// Per-item error codes (ItemError.Code). They never abort the batch.
const (
	CodeValidation          = "validation"
	CodeUnknownField        = "unknown_field"
	CodeMissingKey          = "missing_key"
	CodeConstraintViolation = "constraint_violation"
)

// Errors that abort the whole batch (a host maps them to 413 / 400).
var (
	ErrBodyTooLarge  = errors.New("ingest: body exceeds 1 MiB")
	ErrBatchTooLarge = errors.New("ingest: too many items in the batch")
	ErrBadBody       = errors.New("ingest: malformed body")
	ErrBadSpec       = errors.New("ingest: invalid ingest spec")
)

// Errors a Store returns to flag a per-item rejection instead of an
// infrastructure failure. Any other Store error aborts the batch.
var (
	// ErrValidation: the model's own validation rejected the row.
	ErrValidation = errors.New("ingest: row failed validation")
	// ErrConstraint: a constraint, rule or unique index rejected the row.
	ErrConstraint = errors.New("ingest: row violates a constraint")
)

// Item is one entry of the request body.
type Item struct {
	Op     string         `json:"op"`
	Cursor any            `json:"cursor,omitempty"`
	Data   map[string]any `json:"data,omitempty"`
	Key    map[string]any `json:"key,omitempty"`
}

// Batch is the decoded request body.
type Batch struct {
	Items []Item `json:"items"`
}

// ItemError describes one rejected item.
type ItemError struct {
	Index int    `json:"index"`
	Code  string `json:"code"`
	Field string `json:"field,omitempty"`
}

// Result is the response data of RFC-04 section 2.2.
type Result struct {
	Applied      int         `json:"applied"`
	Deleted      int         `json:"deleted"`
	SkippedStale int         `json:"skipped_stale"`
	Rejected     int         `json:"rejected"`
	Errors       []ItemError `json:"errors"`
	// HighWater is the largest cursor applied (upserted or deleted) in THIS
	// batch, as the sender wrote it; nil when the model has no cursor or
	// nothing was applied.
	HighWater any `json:"high_water"`
}

// Store is what the host implements over its data service, already bound to
// the organization of the route and to the system principal. Find must return
// only live rows (deleted_at IS NULL) and should lock the row (FOR UPDATE).
type Store interface {
	// Find returns the live row matching key, or found=false.
	Find(ctx context.Context, key map[string]any) (row map[string]any, found bool, err error)
	// Create inserts a row built from values (key + fields + cursor).
	Create(ctx context.Context, values map[string]any) error
	// Update writes values over the existing row.
	Update(ctx context.Context, existing map[string]any, values map[string]any) error
	// Delete soft-deletes the existing row. Never physical.
	Delete(ctx context.Context, existing map[string]any) error
}

// MaxBatch returns the effective batch limit of spec.
func MaxBatch(spec v3.IngestSpec) int {
	if spec.MaxBatch <= 0 {
		return DefaultMaxBatch
	}
	if spec.MaxBatch > HardMaxBatch {
		return HardMaxBatch
	}
	return spec.MaxBatch
}

// CheckSpec reports whether spec is usable at runtime (the validator already
// enforces the same rules at publish time; this is the runtime backstop).
func CheckSpec(spec v3.IngestSpec) error {
	if n := len(spec.Key); n < 1 || n > 4 {
		return fmt.Errorf("%w: key needs 1..4 columns, got %d", ErrBadSpec, n)
	}
	if spec.OnDelete != "" && spec.OnDelete != "soft" && spec.OnDelete != "ignore" {
		return fmt.Errorf("%w: on_delete %q", ErrBadSpec, spec.OnDelete)
	}
	return nil
}

// Decode parses and bounds a request body. Numbers stay json.Number so a
// 64-bit cursor survives. Unknown top-level keys are rejected.
func Decode(body []byte, spec v3.IngestSpec) (Batch, error) {
	if err := CheckSpec(spec); err != nil {
		return Batch{}, err
	}
	if len(body) > MaxBodyBytes {
		return Batch{}, ErrBodyTooLarge
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	var b Batch
	if err := dec.Decode(&b); err != nil {
		return Batch{}, fmt.Errorf("%w: %v", ErrBadBody, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Batch{}, fmt.Errorf("%w: trailing data", ErrBadBody)
	}
	if len(b.Items) == 0 {
		return Batch{}, fmt.Errorf("%w: items is empty", ErrBadBody)
	}
	if len(b.Items) > MaxBatch(spec) {
		return Batch{}, fmt.Errorf("%w: %d > %d", ErrBatchTooLarge, len(b.Items), MaxBatch(spec))
	}
	return b, nil
}

// cursorVal is a normalized cursor: either an integer or an instant.
type cursorVal struct {
	isTime bool
	n      int64
	t      time.Time
}

func normalizeCursor(v any) (cursorVal, error) {
	switch x := v.(type) {
	case nil:
		return cursorVal{}, errors.New("cursor is null")
	case json.Number:
		n, err := x.Int64()
		if err != nil {
			return cursorVal{}, fmt.Errorf("cursor %q is not an integer", x)
		}
		return cursorVal{n: n}, nil
	case int:
		return cursorVal{n: int64(x)}, nil
	case int32:
		return cursorVal{n: int64(x)}, nil
	case int64:
		return cursorVal{n: x}, nil
	case float64:
		if x != math.Trunc(x) || math.Abs(x) > 1<<53 {
			return cursorVal{}, fmt.Errorf("cursor %v is not a safe integer", x)
		}
		return cursorVal{n: int64(x)}, nil
	case time.Time:
		return cursorVal{isTime: true, t: x}, nil
	case string:
		t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(x))
		if err != nil {
			return cursorVal{}, fmt.Errorf("cursor %q is not RFC 3339", x)
		}
		return cursorVal{isTime: true, t: t}, nil
	default:
		return cursorVal{}, fmt.Errorf("cursor has unsupported type %T", v)
	}
}

// CompareCursor orders two cursors: -1, 0 or +1. Both must be integers or both
// instants (int, int64, float64, json.Number, or time.Time and RFC 3339 string).
func CompareCursor(a, b any) (int, error) {
	ca, err := normalizeCursor(a)
	if err != nil {
		return 0, err
	}
	cb, err := normalizeCursor(b)
	if err != nil {
		return 0, err
	}
	if ca.isTime != cb.isTime {
		return 0, errors.New("cursors of different kinds (integer vs instant)")
	}
	if ca.isTime {
		switch {
		case ca.t.Before(cb.t):
			return -1, nil
		case ca.t.After(cb.t):
			return 1, nil
		}
		return 0, nil
	}
	switch {
	case ca.n < cb.n:
		return -1, nil
	case ca.n > cb.n:
		return 1, nil
	}
	return 0, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// writable reports whether the sink may write column col: never a managed
// column, the key and (when declared) the cursor are always writable, anything
// else only when Fields is empty (all non-managed) or lists it.
func writable(spec v3.IngestSpec, col string) bool {
	if v3.ManagedColumn(col) {
		return false
	}
	if contains(spec.Key, col) || (spec.Cursor != "" && col == spec.Cursor) {
		return true
	}
	return len(spec.Fields) == 0 || contains(spec.Fields, col)
}

// keyOf extracts the natural key from src, or the first missing column.
func keyOf(spec v3.IngestSpec, src map[string]any) (map[string]any, string) {
	key := make(map[string]any, len(spec.Key))
	for _, c := range spec.Key {
		v, ok := src[c]
		if !ok || v == nil {
			return nil, c
		}
		if s, isStr := v.(string); isStr && strings.TrimSpace(s) == "" {
			return nil, c
		}
		key[c] = v
	}
	return key, ""
}

// Apply runs every item in order. Per-item problems are reported in
// Result.Errors and never abort the batch; only a non-sentinel Store error
// does, returning what was applied so far together with the error (the host
// rolls the transaction back). ctx cancellation also aborts.
func Apply(ctx context.Context, spec v3.IngestSpec, b Batch, st Store) (Result, error) {
	res := Result{Errors: []ItemError{}}
	if err := CheckSpec(spec); err != nil {
		return res, err
	}
	if len(b.Items) > MaxBatch(spec) {
		return res, ErrBatchTooLarge
	}
	var high any
	reject := func(i int, code, field string) {
		res.Rejected++
		res.Errors = append(res.Errors, ItemError{Index: i, Code: code, Field: field})
	}
	bump := func(c any) {
		if spec.Cursor == "" || c == nil {
			return
		}
		if high == nil {
			high = c
			return
		}
		if cmp, err := CompareCursor(c, high); err == nil && cmp > 0 {
			high = c
		}
	}
	// storeErr classifies a Store failure: nil means "item rejected, go on".
	storeErr := func(i int, err error) error {
		switch {
		case errors.Is(err, ErrConstraint):
			reject(i, CodeConstraintViolation, "")
			return nil
		case errors.Is(err, ErrValidation):
			reject(i, CodeValidation, "")
			return nil
		}
		return err
	}
	abort := func(err error) (Result, error) {
		res.HighWater = high
		return res, err
	}

	for i, it := range b.Items {
		if err := ctx.Err(); err != nil {
			return abort(err)
		}
		switch it.Op {
		case OpUpsert:
			if len(it.Data) == 0 {
				reject(i, CodeValidation, "data")
				continue
			}
			bad := ""
			for col := range it.Data {
				if !writable(spec, col) && (bad == "" || col < bad) {
					bad = col
				}
			}
			if bad != "" {
				reject(i, CodeUnknownField, bad)
				continue
			}
			key, missing := keyOf(spec, it.Data)
			if key == nil {
				reject(i, CodeMissingKey, missing)
				continue
			}
			values := make(map[string]any, len(it.Data)+1)
			for c, v := range it.Data {
				values[c] = v
			}
			var cur any
			if spec.Cursor != "" {
				cur = it.Cursor
				if cur == nil {
					cur = it.Data[spec.Cursor]
				}
				if _, err := normalizeCursor(cur); err != nil {
					reject(i, CodeValidation, spec.Cursor)
					continue
				}
				values[spec.Cursor] = cur
			}
			row, found, err := st.Find(ctx, key)
			if err != nil {
				return abort(err)
			}
			if !found {
				if err := st.Create(ctx, values); err != nil {
					if e := storeErr(i, err); e != nil {
						return abort(e)
					}
					continue
				}
				res.Applied++
				bump(cur)
				continue
			}
			if spec.Cursor != "" && row[spec.Cursor] != nil {
				cmp, err := CompareCursor(cur, row[spec.Cursor])
				if err != nil {
					reject(i, CodeValidation, spec.Cursor)
					continue
				}
				if cmp <= 0 {
					res.SkippedStale++
					continue
				}
			}
			if err := st.Update(ctx, row, values); err != nil {
				if e := storeErr(i, err); e != nil {
					return abort(e)
				}
				continue
			}
			res.Applied++
			bump(cur)

		case OpDelete:
			key, missing := keyOf(spec, it.Key)
			if key == nil {
				reject(i, CodeMissingKey, missing)
				continue
			}
			if spec.OnDelete == "ignore" {
				continue
			}
			row, found, err := st.Find(ctx, key)
			if err != nil {
				return abort(err)
			}
			if !found {
				continue
			}
			if spec.Cursor != "" && it.Cursor != nil && row[spec.Cursor] != nil {
				cmp, err := CompareCursor(it.Cursor, row[spec.Cursor])
				if err != nil {
					reject(i, CodeValidation, spec.Cursor)
					continue
				}
				if cmp <= 0 {
					res.SkippedStale++
					continue
				}
			}
			if err := st.Delete(ctx, row); err != nil {
				if e := storeErr(i, err); e != nil {
					return abort(e)
				}
				continue
			}
			res.Deleted++
			bump(it.Cursor)

		default:
			reject(i, CodeValidation, "op")
		}
	}
	res.HighWater = high
	return res, nil
}
