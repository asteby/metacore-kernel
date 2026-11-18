package inbound

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/dynamic"
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
//
// The declaration is SELF-REPORTED, hence a trust boundary: a wrapper that
// embeds *DynamicExecutor inherits SupportsDryRun() == true and may still
// override Execute to write. Do not embed it to wrap Execute; if you must, also
// override SupportsDryRun to answer false (the contract is then refused). As
// defence in depth, DynamicExecutor.Execute hands a dry-run operation a
// READ-ONLY store (see roStore): even code that forgets to look at
// Operation.DryRun cannot reach Create/Update/Delete/UpdateGuarded/DeleteSealed/
// ExecAction/Restore (or the HardDeleter) from inside DynamicExecutor.
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

// ColumnChange is one column of a dry-run step. To carries the value the event
// would write only when the column may be disclosed (deny by default, see
// "What a dry run may show"); otherwise Redacted is true and the column is
// reported by name alone.
//
// From (the value the row has NOW) is data of the database, not of the event:
// it is set ONLY in the answer of Handler.DryRun (a panel session) and only when
// the host gate allows it (DynamicExecutor.Authorize answering "read"). It never
// reaches the HTTP answer of a signed event, a receipt or a replay: the Handler
// strips it from every persisted or externally returned step.
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
	// OnSimulated: the step matches on the id of a row that an earlier step of the
	// chain only WOULD have created; there is no row to read, so the report is a
	// forecast, not a verified outcome.
	OnSimulated bool
}

// FidelityPartial is the Fidelity of every dry-run answer: the report is a
// forecast built from the same match, scope, type, required, `when`, guard and
// Authorize checks as the real run, NOT a full replay. It does not evaluate
// Protected/Generated/Readonly writes, uniqueness, reference existence,
// declarative validation rules, before/after hooks, approvals or the stage
// machine on create, so `would_apply` does not promise that the real run
// applies (see docs/inbound-events.md, "Qué NO se verifica").
const FidelityPartial = "partial"

// errDryWriteBlocked is what a write attempted through the read-only store of a
// dry run answers (500 internal to the caller; the Handler logs the code).
var errDryWriteBlocked = &ExecError{Kind: KindInternal, Code: "dry_run_write_blocked"}

// roStore is the store a dry run gets: every read passes through, every write
// fails. It is the last line of defence behind the `if op.DryRun` branches: a
// code path that forgets one still cannot write.
type roStore struct{ dynStore }

func (roStore) Create(context.Context, string, modelbase.AuthUser, map[string]any) (map[string]any, error) {
	return nil, errDryWriteBlocked
}

func (roStore) Update(context.Context, string, modelbase.AuthUser, uuid.UUID, map[string]any) (map[string]any, error) {
	return nil, errDryWriteBlocked
}

func (roStore) Delete(context.Context, string, modelbase.AuthUser, uuid.UUID) error {
	return errDryWriteBlocked
}

func (roStore) UpdateGuarded(context.Context, string, modelbase.AuthUser, uuid.UUID, map[string]any, *dynamic.WriteGuard) (map[string]any, error) {
	return nil, errDryWriteBlocked
}

func (roStore) DeleteSealed(context.Context, string, modelbase.AuthUser, uuid.UUID, map[string]any, *dynamic.WriteGuard) error {
	return errDryWriteBlocked
}

func (roStore) ExecAction(context.Context, string, modelbase.AuthUser, uuid.UUID, string, map[string]any) (dynamic.ActionResult, error) {
	return dynamic.ActionResult{}, errDryWriteBlocked
}

func (roStore) Restore(context.Context, string, modelbase.AuthUser, uuid.UUID) (map[string]any, error) {
	return nil, errDryWriteBlocked
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

// What a dry run may show: DENY BY DEFAULT.
//
// The table metadata carries no per-column sensitivity, so a column NAME is never
// a guarantee (a personal value lives in `razon_social`, `contacto`, `ciudad`
// as easily as in `email`). The report therefore reveals the value of a column
// only when ALL of these hold:
//
//  1. the column is declared, and is not Protected, Hidden, read-gated
//     (ReadPermission) or Generated;
//  2. DynamicExecutor.DryRunRedact does not veto it;
//  3. either the host listed it in DynamicExecutor.DryRunAllow (any scalar), or
//     its declared TYPE is one that cannot carry free text AND the value itself
//     has that shape: a number, a boolean, a date/datetime, a uuid / reference to
//     another table, or a value that is one of the column's declared options
//     (select, status, stage); and
//  4. (type path only) its name does not look personal (personalHints /
//     personalTokens: a second barrier, NOT the guarantee).
//
// Everything else (text, string, textarea, email, phone, password, json, an
// undeclared or unknown type) is reported as {redacted:true}: the column name
// alone.

// personalHints are fragments of a column name that mark a column as personal
// or free text, in Spanish and English (second barrier of the type path; see
// above). Substring match: keep the entries long enough not to hit innocent
// names; the short ones are in personalTokens.
var personalHints = []string{
	"email", "mail", "phone", "tel", "mobile", "celular", "whatsapp", "fax",
	"name", "nombre", "apellido", "address", "direccion", "street", "calle", "zip", "postal",
	"rfc", "curp", "dni", "nif", "ssn", "tax_id", "passport", "pasaporte", "birth", "nacim",
	"password", "passwd", "secret", "token", "card", "tarjeta", "iban", "clabe", "account", "cuenta",
	"salary", "sueldo", "note", "nota", "comment", "comentario", "descr", "observ", "remark",
	"message", "mensaje",
	"razon", "social", "cliente", "customer", "client", "contact", "titular", "holder", "beneficiar",
	"empresa", "company", "ciudad", "colonia", "municipio", "neighborhood", "suburb",
	"ubicacion", "location", "geo", "latitud", "longitud", "latitude", "longitude", "imss",
	"api_key", "apikey", "secret_key", "private_key", "access_key", "cvv", "cvc",
	"usuario", "user", "sender", "remitente", "destinatario", "recipient", "owner", "propietario",
}

// personalTokens are short words that mark a personal column only as a WHOLE
// token of the name (split on non-alphanumerics): `lat` in `lat`/`lat_deg`,
// never in `plate` or `template`.
var personalTokens = map[string]bool{
	"lat": true, "lng": true, "lon": true, "nss": true, "ine": true, "pin": true,
	"otp": true, "ip": true, "city": true, "key": true, "rut": true, "cui": true,
}

func personalColumn(key string) bool {
	k := strings.ToLower(key)
	for _, h := range personalHints {
		if strings.Contains(k, h) {
			return true
		}
	}
	for _, t := range strings.FieldsFunc(k, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	}) {
		if personalTokens[t] {
			return true
		}
	}
	return false
}

// disclosure is how the value of a column may be shown.
type disclosure int

const (
	discDeny  disclosure = iota // name only
	discType                    // only when the declared type and the value are safe
	discAllow                   // any scalar (the host allowed it, or the stage field)
)

// disclosure decides how column key of model may be shown. stateKey is the stage
// field of a transition ("" otherwise): its value is a lifecycle state.
func (d *DynamicExecutor) disclosure(model, key, stateKey string, meta *modelbase.TableMetadata) (modelbase.ColumnDef, disclosure) {
	var col modelbase.ColumnDef
	if meta == nil {
		return col, discDeny
	}
	found := false
	for _, c := range meta.Columns {
		if c.Key == key {
			col, found = c, true
			break
		}
	}
	// A column the metadata does not declare, or a gated one, is never shown.
	if !found || col.Protected || col.Hidden || col.ReadPermission != "" || col.Generated != "" {
		return col, discDeny
	}
	if d.DryRunRedact != nil && d.DryRunRedact(model, key) {
		return col, discDeny
	}
	if d.DryRunAllow != nil && d.DryRunAllow(model, key) {
		return col, discAllow
	}
	if stateKey != "" && key == stateKey {
		return col, discAllow
	}
	if personalColumn(key) {
		return col, discDeny
	}
	return col, discType
}

// safeShape reports whether value v, of column c, has a shape that cannot carry
// free text (see the rules above).
func safeShape(c modelbase.ColumnDef, v any) bool {
	if v == nil {
		return true
	}
	cv := canonValue(v)
	t := strings.ToLower(strings.TrimSpace(c.Type))
	if t == "uuid" || c.Ref != "" {
		s, ok := cv.(string)
		if !ok {
			return false
		}
		_, err := uuid.Parse(s)
		return err == nil
	}
	if len(c.Options) > 0 {
		s, ok := scalarText(cv)
		if !ok {
			return false
		}
		for _, o := range c.Options {
			if os, ok := scalarText(o.Value); ok && os == s {
				return true
			}
		}
		return false
	}
	switch matchClass(t) {
	case classNumber:
		switch n := cv.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, json.Number:
			return true
		case string:
			_, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
			return err == nil
		}
	case classBool:
		_, ok := cv.(bool)
		return ok
	case classTime:
		switch tv := cv.(type) {
		case time.Time:
			return true
		case string:
			_, ok := parseMatchInstant(tv)
			return ok
		}
	}
	return false
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

// shownIn is shown, subject to the disclosure of the column.
func shownIn(c modelbase.ColumnDef, mode disclosure, v any) (any, bool) {
	if mode == discDeny || (mode == discType && !safeShape(c, v)) {
		return nil, false
	}
	return shown(v)
}

// dryChanges lists the columns the write would change: every column of vals for
// a create (before nil), the ones that differ from the row for an update. The
// current value of the row (From) is carried only when reveal is true, which
// the executor grants to Handler.DryRun alone (see revealFrom); stateKey is the
// stage field of a transition.
func (d *DynamicExecutor) dryChanges(model string, meta *modelbase.TableMetadata, before, vals map[string]any, reveal bool, stateKey string) map[string]ColumnChange {
	out := make(map[string]ColumnChange, len(vals))
	for k, v := range vals {
		if before != nil {
			if cur, has := before[k]; has && ((cur == nil && v == nil) || matchEqual(cur, v)) {
				continue
			}
		}
		col, mode := d.disclosure(model, k, stateKey, meta)
		to, ok := shownIn(col, mode, v)
		if !ok {
			out[k] = ColumnChange{Redacted: true}
			continue
		}
		ch := ColumnChange{To: to}
		if before != nil && reveal {
			from, fromOK := shownIn(col, mode, before[k])
			if !fromOK {
				out[k] = ColumnChange{Redacted: true}
				continue
			}
			ch.From = from
		}
		out[k] = ch
	}
	return out
}

// revealFrom reports whether the report of this operation may carry the CURRENT
// value of a row (ColumnChange.From). Only Handler.DryRun (a panel session, never
// a signed external event) sets Operation.DryRunReveal, and even then the host
// must have wired Authorize and let it answer "read" for the model: without a
// read gate the executor would be reading rows as the system caller on behalf of
// whoever holds the organization.
func (d *DynamicExecutor) revealFrom(ctx context.Context, op Operation) bool {
	return op.DryRun && op.DryRunReveal && d.Authorize != nil && d.Authorize(ctx, op.Org, op.Route.Model, "read")
}

// dryPlaceholderPrefix starts the id a simulated create hands to the later steps
// of a chain ({{result.id}}). It is a syntactically valid uuid (version 0: a
// random uuid never has it), so it passes the uuid/match checks, and a step that
// matches on it is judged against a row that does not exist yet.
const dryPlaceholderPrefix = "00000000-0000-0000-d12e-"

func dryPlaceholderID(i int) string { return fmt.Sprintf("%s%012x", dryPlaceholderPrefix, i) }

// matchesPlaceholder reports whether a match value of op is a simulated id.
func matchesPlaceholder(op Operation) bool {
	for _, f := range op.Route.Match {
		if s, ok := op.Values[f].(string); ok && strings.HasPrefix(s, dryPlaceholderPrefix) {
			return true
		}
	}
	return false
}

// wouldOnSimulated is the report of a step whose match names a row that an
// earlier step of the chain only WOULD have created: there is nothing to read, so
// it is neither a failure nor a verified outcome (WouldResult.OnSimulated).
func (d *DynamicExecutor) wouldOnSimulated(ctx context.Context, st dynStore, op Operation) (Outcome, error) {
	w := &WouldResult{Rows: 1, OnSimulated: true}
	switch op.Route.Op {
	case OpDelete:
		w.Would = "delete"
	case OpAction:
		w.Would, w.Target = "action", op.ActionKey
		w.ParamKeys = sortedKeys(op.Params)
	case OpTransition:
		w.Would = "transition"
	case OpRestore:
		w.Would = "restore"
	default:
		meta, err := st.TableMetadata(ctx, op.Route.Model)
		if err != nil {
			return Outcome{}, mapDynamicErr(err)
		}
		vals, err := dryCoerce(meta, op.Values)
		if err != nil {
			return Outcome{}, err
		}
		w.Would = wouldKind(op.Route.Op, false)
		w.Changes = d.dryChanges(op.Route.Model, meta, nil, vals, false, "")
	}
	return Outcome{Would: w}, nil
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
	w := &WouldResult{Would: kind, Rows: 1, Changes: d.dryChanges(op.Route.Model, meta, row, vals, d.revealFrom(ctx, op), "")}
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
		w.Changes = d.dryChanges(op.Route.Model, meta, map[string]any{field: from}, map[string]any{field: op.To}, d.revealFrom(ctx, op), field)
	}
	out.Would = w
	return out
}

// simulatedAction is the Authorize action of a step that runs on a simulated row
// (the same strings the real operations ask).
func simulatedAction(op Operation) string {
	switch op.Route.Op {
	case OpDelete:
		return "delete"
	case OpAction:
		return "action:" + op.ActionKey
	case OpTransition:
		return "transition:" + op.To
	case OpRestore:
		return "restore"
	}
	return "update"
}
