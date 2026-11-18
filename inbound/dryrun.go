package inbound

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/modelbase"
)

// Dry-run (shadow) mode. A contract with Contract.DryRun, or a call to
// Handler.DryRun, evaluates an event end to end (signature, schema, scope,
// idempotency, match, columns, `when`, order guard) and reports what it WOULD
// do, without writing a row, running an action, taking a write lock or
// publishing an event. Nothing here relaxes a control: the Executor still
// applies Authorize, the entity scope and the limits; it only stops short of
// the write.

// DryRunSupporter is an optional Executor port: an Executor that implements it
// and answers true honours Operation.DryRun (it never writes when it is set). A
// dry-run contract is refused at registration otherwise (fail closed), and the
// Handler never hands a dry-run Operation to an Executor without it. DynamicExecutor
// supports it.
type DryRunSupporter interface {
	SupportsDryRun() bool
}

// SupportsDryRun implements DryRunSupporter.
func (d *DynamicExecutor) SupportsDryRun() bool { return true }

func executorDryRuns(ex Executor) bool {
	s, ok := ex.(DryRunSupporter)
	return ok && s.SupportsDryRun()
}

// ErrDryRunUnsupported: a dry run needs an Executor that implements
// DryRunSupporter.
var ErrDryRunUnsupported = invalidf("the Executor does not declare dry-run support (implement DryRunSupporter)")

// ColumnChange is one column of a dry-run step. From / To carry the values only
// when the column may be disclosed; otherwise Redacted is true and the column
// is reported by name alone (never a value of a protected, hidden, read-gated
// or personal column, nor a non-scalar one).
type ColumnChange struct {
	From     any  `json:"from,omitempty"`
	To       any  `json:"to,omitempty"`
	Redacted bool `json:"redacted,omitempty"`
}

// WouldResult is what a dry-run operation would have done.
type WouldResult struct {
	// Would: create | update | upsert-create | upsert-update | delete | restore |
	// transition | action (and would_emit | would_notify | would_enqueue |
	// would_command for a suppressed side effect, named by Target).
	Would  string
	Target string
	// Rows is how many rows it would touch; State the lifecycle value read from
	// the row; ParamKeys the keys of a record.action payload.
	Rows      int
	State     string
	ParamKeys []string
	Changes   map[string]ColumnChange
	// Noop: it would change nothing (no column differs, already live, no row).
	Noop bool
}

// wouldKind names a write of record.create|update|upsert.
func wouldKind(op Op, create bool) string {
	switch {
	case op == OpUpsert && create:
		return "upsert-create"
	case op == OpUpsert:
		return "upsert-update"
	case create:
		return "create"
	}
	return "update"
}

func sortedKeys(m map[string]any) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// personalHints are fragments of a column name that mark a column as personal
// or free text: their values are never shown by a dry run. The metadata has no
// per-column sensitivity, so this is a floor; DynamicExecutor.DryRunRedact lets
// a host add its own rule.
var personalHints = []string{
	"email", "mail", "phone", "tel", "mobile", "celular", "whatsapp", "fax",
	"name", "nombre", "apellido", "address", "direccion", "street", "calle", "zip", "postal",
	"rfc", "curp", "dni", "nif", "ssn", "tax_id", "passport", "pasaporte", "birth", "nacim",
	"password", "passwd", "secret", "token", "card", "tarjeta", "iban", "clabe", "account", "cuenta",
	"salary", "sueldo", "note", "nota", "comment", "comentario", "descr", "observ", "remark",
	"message", "mensaje",
}

func personalColumn(key string) bool {
	k := strings.ToLower(key)
	for _, h := range personalHints {
		if strings.Contains(k, h) {
			return true
		}
	}
	return false
}

// discloses reports whether the dry run may show the values of a column.
func (d *DynamicExecutor) discloses(model, key string, meta *modelbase.TableMetadata) bool {
	if meta == nil || personalColumn(key) {
		return false
	}
	if d.DryRunRedact != nil && d.DryRunRedact(model, key) {
		return false
	}
	for _, c := range meta.Columns {
		if c.Key != key {
			continue
		}
		return !c.Protected && !c.Hidden && c.ReadPermission == "" && c.Generated == ""
	}
	return false // a column the metadata does not declare
}

const maxShownRunes = 200

// shown renders a scalar for a dry-run report; ok is false for anything else
// (maps, lists, unknown types), which is reported as redacted.
func shown(v any) (any, bool) {
	switch t := canonValue(v).(type) {
	case nil:
		return nil, true
	case string:
		if r := []rune(t); len(r) > maxShownRunes {
			return string(r[:maxShownRunes]) + "...", true
		}
		return t, true
	case bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, json.Number:
		return t, true
	case time.Time:
		return t.UTC().Format(time.RFC3339Nano), true
	}
	return nil, false
}

// dryChanges lists the columns the write would change: every column of vals for
// a create (before nil), the ones that differ from the row for an update.
func (d *DynamicExecutor) dryChanges(model string, meta *modelbase.TableMetadata, before, vals map[string]any) map[string]ColumnChange {
	out := make(map[string]ColumnChange, len(vals))
	for k, v := range vals {
		if before != nil {
			if cur, has := before[k]; has && ((cur == nil && v == nil) || matchEqual(cur, v)) {
				continue
			}
		}
		if !d.discloses(model, k, meta) {
			out[k] = ColumnChange{Redacted: true}
			continue
		}
		to, ok := shown(v)
		var from any
		fromOK := true
		if before != nil {
			from, fromOK = shown(before[k])
		}
		if !ok || !fromOK {
			out[k] = ColumnChange{Redacted: true}
			continue
		}
		out[k] = ColumnChange{From: from, To: to}
	}
	return out
}

// dryCoerce checks every value against the type of its column (the same
// conversion the match values get) and returns the converted values; a value
// that cannot be converted is a 422 rejected_by_model, as the real write would
// answer.
func dryCoerce(meta *modelbase.TableMetadata, vals map[string]any) (map[string]any, error) {
	if meta == nil {
		return vals, nil
	}
	classes := columnClasses(meta)
	out := make(map[string]any, len(vals))
	for k, v := range vals {
		out[k] = v
		cl := classes[k]
		if cl == "" || v == nil {
			continue
		}
		nv, err := coerceMatchValue(cl, v)
		if err != nil {
			return nil, &ExecError{Kind: KindInvalid, Code: "rejected_by_model", Err: err}
		}
		out[k] = nv
	}
	return out, nil
}

// dryRequired refuses a create that leaves a required column empty.
func dryRequired(meta *modelbase.TableMetadata, vals map[string]any) error {
	if meta == nil {
		return nil
	}
	for _, c := range meta.Columns {
		if !c.Required || c.Default != nil || c.Readonly || c.Generated != "" || SystemFields[c.Key] || c.Key == branchColumn {
			continue
		}
		v, ok := vals[c.Key]
		if s, isStr := v.(string); !ok || v == nil || (isStr && strings.TrimSpace(s) == "") {
			return &ExecError{Kind: KindInvalid, Code: "rejected_by_model"}
		}
	}
	return nil
}

// wouldWrite is the dry-run twin of create / writeRow: row is the existing row
// (nil: it would be created). Nothing is written.
func (d *DynamicExecutor) wouldWrite(ctx context.Context, st dynStore, op Operation, row map[string]any, kind string) (Outcome, error) {
	meta, err := st.TableMetadata(ctx, op.Route.Model)
	if err != nil {
		return Outcome{}, mapDynamicErr(err)
	}
	vals := op.Values
	if row == nil {
		vals = withDefaults(op.CreateDefaults, op.Values)
	}
	vals, err = dryCoerce(meta, vals)
	if err != nil {
		return Outcome{}, err
	}
	if row == nil {
		if err := dryRequired(meta, vals); err != nil {
			return Outcome{}, err
		}
	}
	w := &WouldResult{Would: kind, Rows: 1, Changes: d.dryChanges(op.Route.Model, meta, row, vals)}
	out := Outcome{Created: row == nil, Would: w}
	if row != nil {
		id, rerr := rowID(row)
		if rerr != nil {
			return Outcome{}, rerr
		}
		out.RecordID = id.String()
		w.Noop = len(w.Changes) == 0
	}
	return out, nil
}

// wouldTransition is the dry-run twin of the write loop of doTransition: n rows
// are judged (row i is get(i)); rows already in the target state are no-ops.
func (d *DynamicExecutor) wouldTransition(ctx context.Context, st dynStore, op Operation, field string, n int, get func(i int) (uuid.UUID, string, bool)) Outcome {
	meta, _ := st.TableMetadata(ctx, op.Route.Model)
	first, _, _ := get(0)
	out := Outcome{RecordID: first.String()}
	w := &WouldResult{Would: "transition"}
	var from string
	for i := 0; i < n; i++ {
		_, f, noop := get(i)
		if noop {
			continue
		}
		if w.Rows == 0 {
			from = f
		}
		w.Rows++
	}
	out.Affected = w.Rows
	if w.Rows == 0 {
		out.Noop, out.Reason, w.Noop = true, CodeAlreadyInState, true
	} else {
		w.Changes = d.dryChanges(op.Route.Model, meta, map[string]any{field: from}, map[string]any{field: op.To})
	}
	out.Would = w
	return out
}
