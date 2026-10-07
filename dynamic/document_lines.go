package dynamic

// document_lines.go — the write side of document_forms lines and the
// «crear desde» (create from another document) pending quantity.
//
// A document type with a lines step posts its lines nested under the lines
// field (e.g. `items: [...]`). The generic Create used to drop them (the write
// plane ignores one_to_many), so a factura born from the editor kept its header
// and lost every renglón. When the host wires a DocumentLinesResolver, Create
// and Update write those lines as rows of the line model (the one_to_many
// relation named like the lines field), each through the regular Create so the
// line formulas, rollups, hooks and events run as for any other row.
//
// A type's `sources` name the documents it can be created from. A source that
// declares `line_link_field` (the line column holding the source line id)
// gets, from the runtime and not from the client:
//
//   - SourceLines: the source lines with their pending quantity = source
//     quantity − what the live documents of this model already consumed
//     (lines pointing at the same source line, minus the documents in an
//     exclude_states state), or the source's own remaining_qty_field.
//   - a save check: Create/Update reject (422, message in Spanish) a line
//     whose quantity exceeds what is pending, so a double invoice cannot be
//     forced by editing the payload. Update excludes the document's own lines.
//
// A document may also keep its lines in a json COLUMN of its own (a credit
// note's `lines`, written by the addon's submit_action) instead of a line
// model. Those lines are not rows to write — the field stays in the payload as
// any other column — but they still consume their source: SourceLines reads
// the live documents linked to the source (the source's `link_field`) and sums
// the quantity of their json lines per `line_link_field`. The save check of
// such documents is the addon's (its submit_action handler).
//
// Without a resolver (or for a model without document_forms) nothing changes.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/modelbase"
)

// DocumentLinesSpec is the executable form of a model's document_forms lines.
type DocumentLinesSpec struct {
	// TypeField is the column that discriminates the document type ("" = one type).
	TypeField string
	Types     []DocumentLinesType
}

// DocumentLinesType is one document type whose lines are written as rows.
type DocumentLinesType struct {
	// Value is what TypeField carries for this type.
	Value string
	// Field is the payload key holding the lines (e.g. "items").
	Field string
	// LineModel / ForeignKey: the one_to_many relation named Field.
	LineModel  string
	ForeignKey string
	// JSONColumn: the lines live in the document's own json column named
	// Field (no line model). They are not written as rows; they only count
	// as consumption of their sources.
	JSONColumn bool
	Sources    []DocumentLineSource
	// Fields are the header fields of the type whose static options carry a
	// `when`: the save re-checks them (option_when.go).
	Fields []manifest.FieldDef
}

// DocumentLineSource is one «crear desde» origin with its pending-quantity rule.
type DocumentLineSource struct {
	Key   string
	Label string
	// Model is the source document model; LinesModel / LinesForeignKey its
	// one_to_many lines relation (the manifest's `lines`).
	Model           string
	LinesModel      string
	LinesForeignKey string
	// QtyField is the source line quantity column (default "quantity").
	QtyField string
	// LineLinkField is the column of this document's line model holding the
	// source line id. Empty = no pending quantity and no save check.
	LineLinkField string
	// LinkField is the header column of this document holding the source
	// document id (the manifest's `link_field`). Json-column lines find the
	// documents that consume a source through it.
	LinkField string
	// RemainingQtyField, when set, is read from the source line instead of
	// computing the consumption.
	RemainingQtyField string
	// ExcludeStates are this document's states that do not consume the source;
	// StateField is the column they are read from.
	ExcludeStates []string
	StateField    string
	// OptionFilter is the source's option_filter: the rule its picker uses to
	// offer only the documents one may create from (a sent quote not yet
	// converted). Create/Update re-check the picked document (LinkField)
	// against it, so a hidden source cannot be forced through the payload.
	OptionFilter modelbase.OptionFilter
}

// tracksRemaining reports whether the runtime computes and enforces the
// pending quantity for this source.
func (s DocumentLineSource) tracksRemaining() bool {
	return s.LineLinkField != "" && s.LinesModel != "" && isSafeIdent(s.LineLinkField)
}

func (s DocumentLineSource) qtyField() string {
	if s.QtyField != "" {
		return s.QtyField
	}
	return "quantity"
}

// DocumentLinesResolver returns a model's DocumentLinesSpec. Host-wired from
// the addon registry (see DeriveDocumentLines); nil / ok=false = the lines
// field is left alone, as before.
type DocumentLinesResolver func(ctx context.Context, model string) (*DocumentLinesSpec, bool)

// DeriveDocumentLines builds the spec of def from its document_forms and
// relations. lookup resolves another model of the registry by the name the
// manifest uses (a source model). A type whose lines field is a one_to_many
// relation writes its lines as rows; one whose lines field is a json column of
// the model keeps them there (JSONColumn) and only serves the pending quantity
// of its sources. Any other type is skipped (its lines have nowhere to go).
// Nil when nothing applies.
func DeriveDocumentLines(def manifest.ModelDefinition, lookup func(model string) (manifest.ModelDefinition, bool)) *DocumentLinesSpec {
	df := def.DocumentForms
	if df == nil {
		return nil
	}
	spec := &DocumentLinesSpec{TypeField: df.TypeField}
	stateField := stateColumnOf(def)
	for _, t := range df.Types {
		if !t.Lines.Enabled() {
			continue
		}
		field := t.Lines.Field
		if field == "" {
			field = df.LinesField
		}
		if field == "" {
			field = "lines"
		}
		value := t.Value
		if value == "" {
			value = t.Key
		}
		var dt DocumentLinesType
		if rel, ok := oneToMany(def.Relations, field); ok {
			dt = DocumentLinesType{Value: value, Field: field, LineModel: rel.Through, ForeignKey: rel.ForeignKey}
		} else if jsonColumn(def.Columns, field) {
			dt = DocumentLinesType{Value: value, Field: field, JSONColumn: true}
		} else {
			continue
		}
		dt.Fields = optionWhenFields(t.Fields)
		for _, src := range t.Sources {
			ls := DocumentLineSource{
				Key: src.Key, Label: src.Label, Model: src.Model,
				QtyField: src.QtyField, LineLinkField: src.LineLinkField, LinkField: src.LinkField,
				RemainingQtyField: src.RemainingQtyField,
				ExcludeStates:     append([]string(nil), src.ExcludeStates...),
				StateField:        stateField,
			}
			if len(src.OptionFilter) > 0 {
				var f modelbase.OptionFilter
				if json.Unmarshal(src.OptionFilter, &f) == nil {
					ls.OptionFilter = f
				}
			}
			if lookup != nil {
				if sdef, ok := lookup(src.Model); ok {
					if srel, ok := oneToMany(sdef.Relations, src.Lines); ok {
						ls.LinesModel, ls.LinesForeignKey = srel.Through, srel.ForeignKey
					}
				}
			}
			dt.Sources = append(dt.Sources, ls)
		}
		spec.Types = append(spec.Types, dt)
	}
	if len(spec.Types) == 0 {
		return nil
	}
	return spec
}

func oneToMany(rels []manifest.RelationDef, name string) (manifest.RelationDef, bool) {
	for _, r := range rels {
		if r.Name == name && (r.Kind == "" || strings.EqualFold(r.Kind, "one_to_many")) &&
			r.Through != "" && isSafeIdent(r.ForeignKey) {
			return r, true
		}
	}
	return manifest.RelationDef{}, false
}

// jsonColumn reports whether cols declare name as a json/jsonb column.
func jsonColumn(cols []manifest.ColumnDef, name string) bool {
	for _, c := range cols {
		if c.Name == name {
			t := strings.ToLower(strings.TrimSpace(c.Type))
			return t == "json" || t == "jsonb"
		}
	}
	return false
}

// stateColumnOf is the column a document's state is read from: the stage
// field, else a `state` or `status` column.
func stateColumnOf(def manifest.ModelDefinition) string {
	if def.StageField != "" {
		return def.StageField
	}
	for _, want := range []string{"state", "status"} {
		for _, c := range def.Columns {
			if c.Name == want {
				return want
			}
		}
	}
	return ""
}

// typeFor picks the document type of a payload: by TypeField, else the only
// (or first) type.
func (sp *DocumentLinesSpec) typeFor(input map[string]any) *DocumentLinesType {
	if sp == nil || len(sp.Types) == 0 {
		return nil
	}
	if sp.TypeField != "" {
		if v, ok := input[sp.TypeField]; ok && v != nil {
			want := fmt.Sprint(v)
			for i := range sp.Types {
				if sp.Types[i].Value == want {
					return &sp.Types[i]
				}
			}
		}
	}
	return &sp.Types[0]
}

// source finds a source by key across the types.
func (sp *DocumentLinesSpec) source(key string) (*DocumentLinesType, *DocumentLineSource) {
	if sp == nil {
		return nil, nil
	}
	for i := range sp.Types {
		for j := range sp.Types[i].Sources {
			if sp.Types[i].Sources[j].Key == key {
				return &sp.Types[i], &sp.Types[i].Sources[j]
			}
		}
	}
	return nil, nil
}

// documentLines is the lines payload taken out of a Create/Update input.
type documentLines struct {
	typ  *DocumentLinesType
	rows []map[string]any
	// pos is each row's index in the posted array (sections/notes included),
	// the index the client's error keys refer to.
	pos []int
}

func (s *Service) resolveDocumentLines(ctx context.Context, model string) *DocumentLinesSpec {
	if s.documentLines == nil {
		return nil
	}
	sp, ok := s.documentLines(ctx, model)
	if !ok {
		return nil
	}
	return sp
}

// takeDocumentLines removes the lines field from input (it is not a column)
// and returns its rows. nil when the model has no lines spec or the payload
// does not carry the field (an Update without lines leaves them untouched).
func (s *Service) takeDocumentLines(ctx context.Context, model string, input map[string]any) (*documentLines, error) {
	sp := s.resolveDocumentLines(ctx, model)
	typ := sp.typeFor(input)
	if typ == nil || typ.JSONColumn {
		// Json-column lines are a column of the document: they stay in input.
		return nil, nil
	}
	raw, ok := input[typ.Field]
	if !ok {
		return nil, nil
	}
	delete(input, typ.Field)
	rows, pos, err := lineRows(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrInvalidInput, typ.Field, err)
	}
	return &documentLines{typ: typ, rows: rows, pos: pos}, nil
}

// lineRows accepts the lines as a JSON array of objects; section / note rows
// (kind != item) are presentation only and are not written.
func lineRows(raw any) ([]map[string]any, []int, error) {
	if raw == nil {
		return nil, nil, nil
	}
	v := reflect.ValueOf(raw)
	if v.Kind() != reflect.Slice {
		return nil, nil, fmt.Errorf("expected a list of lines")
	}
	out := make([]map[string]any, 0, v.Len())
	pos := make([]int, 0, v.Len())
	for i := 0; i < v.Len(); i++ {
		m, ok := v.Index(i).Interface().(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("line %d is not an object", i+1)
		}
		if k, ok := m["kind"].(string); ok && k != "" && k != "item" {
			continue
		}
		out = append(out, m)
		pos = append(pos, i)
	}
	return out, pos, nil
}

// at is the posted index of the i-th written row (i itself without pos).
func (dl *documentLines) at(i int) int {
	if dl != nil && i < len(dl.pos) {
		return dl.pos[i]
	}
	return i
}

// ---- pending quantity ---------------------------------------------------------

// SourceLines serves the lines of a source document with their pending
// quantity, for the «crear desde» prefill. Each row is the source line plus
// source_line_id, source_quantity, consumed_quantity and remaining_quantity.
// excludeDocID (optional) leaves a document's own lines out of the consumption
// (editing a draft). The caller needs list access on the source line model.
func (s *Service) SourceLines(ctx context.Context, model string, user modelbase.AuthUser, sourceKey, sourceID, excludeDocID string) ([]map[string]any, error) {
	sp := s.resolveDocumentLines(ctx, model)
	typ, src := sp.source(sourceKey)
	if src == nil || src.LinesModel == "" || !isSafeIdent(src.LinesForeignKey) {
		return nil, ErrRecordNotFound
	}
	if _, err := uuid.Parse(sourceID); err != nil {
		return nil, ErrInvalidID
	}
	rows, err := s.fetchRows(ctx, user, src.LinesModel, src.LinesForeignKey, []any{sourceID}, true)
	if err != nil {
		return nil, err
	}
	consumed := map[string]float64{}
	if src.tracksRemaining() && src.RemainingQtyField == "" {
		if typ.JSONColumn {
			consumed, err = s.consumedByJSONLines(ctx, user, model, typ, src, sourceID, excludeDocID)
		} else {
			consumed, err = s.consumedBySource(ctx, user, model, typ, src, rowIDs(rows), excludeDocID)
		}
		if err != nil {
			return nil, err
		}
	}
	return pendingLines(rows, consumed, *src), nil
}

// consumedByJSONLines sums, per source line, the quantity of the json lines of
// this model's documents linked to sourceID (header column src.LinkField),
// skipping the documents in an excluded state and excludeDocID. A source
// without a safe link_field cannot be traced: nothing is consumed.
func (s *Service) consumedByJSONLines(ctx context.Context, user modelbase.AuthUser, model string, typ *DocumentLinesType, src *DocumentLineSource, sourceID, excludeDocID string) (map[string]float64, error) {
	if src.LinkField == "" || !isSafeIdent(src.LinkField) {
		return map[string]float64{}, nil
	}
	docs, err := s.fetchRows(ctx, user, model, src.LinkField, []any{sourceID}, false)
	if err != nil {
		return nil, err
	}
	return sumJSONLines(docs, typ.Field, *src, excludeDocID), nil
}

// sumJSONLines adds the quantity of the json lines of docs per source line.
func sumJSONLines(docs []map[string]any, field string, src DocumentLineSource, excludeDocID string) map[string]float64 {
	skip := map[string]struct{}{}
	for _, st := range src.ExcludeStates {
		skip[st] = struct{}{}
	}
	out := map[string]float64{}
	for _, d := range docs {
		if excludeDocID != "" && fmt.Sprint(d["id"]) == excludeDocID {
			continue
		}
		if src.StateField != "" && d[src.StateField] != nil {
			if _, excluded := skip[fmt.Sprint(d[src.StateField])]; excluded {
				continue
			}
		}
		for _, l := range jsonLineRows(d[field]) {
			link := linkValue(l, src.LineLinkField)
			if link == "" {
				continue
			}
			out[link] += toFloat(l["quantity"])
		}
	}
	return out
}

// jsonLineRows decodes a json lines cell (array, JSON text or raw bytes) into
// its objects. Anything unreadable is no lines.
func jsonLineRows(v any) []map[string]any {
	var raw []byte
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		raw = []byte(t)
	case []byte:
		raw = t
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return nil
		}
		raw = b
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		// A json string holding the array (double-encoded) is still lines.
		var inner string
		if json.Unmarshal(raw, &inner) != nil || json.Unmarshal([]byte(inner), &rows) != nil {
			return nil
		}
	}
	return rows
}

// pendingLines annotates source line rows with their pending quantity.
func pendingLines(rows []map[string]any, consumed map[string]float64, src DocumentLineSource) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		id := fmt.Sprint(r["id"])
		qty := toFloat(r[src.qtyField()])
		used := consumed[id]
		remaining := qty - used
		if src.RemainingQtyField != "" {
			remaining = toFloat(r[src.RemainingQtyField])
			used = qty - remaining
		}
		if remaining < 0 {
			remaining = 0
		}
		row := make(map[string]any, len(r)+4)
		for k, v := range r {
			row[k] = v
		}
		row["source_line_id"] = id
		row["source_quantity"] = qty
		row["consumed_quantity"] = roundQty(used)
		row["remaining_quantity"] = roundQty(remaining)
		out = append(out, row)
	}
	return out
}

// sumConsumed adds the quantity of this document's lines per source line,
// skipping lines of documents in an excluded state and of excludeDocID.
// headerState maps document id → state ("" when unknown).
func sumConsumed(lines []map[string]any, headerState map[string]string, src DocumentLineSource, fk, excludeDocID string) map[string]float64 {
	skip := map[string]struct{}{}
	for _, s := range src.ExcludeStates {
		skip[s] = struct{}{}
	}
	out := map[string]float64{}
	for _, l := range lines {
		doc := fmt.Sprint(l[fk])
		if excludeDocID != "" && doc == excludeDocID {
			continue
		}
		st, known := headerState[doc]
		if !known {
			continue // the document is gone (deleted) — it no longer consumes
		}
		if _, excluded := skip[st]; excluded {
			continue
		}
		out[fmt.Sprint(l[src.LineLinkField])] += toFloat(l["quantity"])
	}
	return out
}

// consumedBySource reads the lines of this model's documents that point at the
// given source lines and sums their quantity per source line.
func (s *Service) consumedBySource(ctx context.Context, user modelbase.AuthUser, model string, typ *DocumentLinesType, src *DocumentLineSource, sourceLineIDs []any, excludeDocID string) (map[string]float64, error) {
	if len(sourceLineIDs) == 0 {
		return map[string]float64{}, nil
	}
	if typ.JSONColumn {
		return nil, fmt.Errorf("%w: %s keeps its lines in a json column; use consumedByJSONLines", ErrInvalidInput, model)
	}
	lines, err := s.fetchRows(ctx, user, typ.LineModel, src.LineLinkField, sourceLineIDs, false)
	if err != nil {
		return nil, err
	}
	docIDs := map[string]struct{}{}
	var ids []any
	for _, l := range lines {
		d := fmt.Sprint(l[typ.ForeignKey])
		if _, seen := docIDs[d]; !seen && d != "" && d != "<nil>" {
			docIDs[d] = struct{}{}
			ids = append(ids, d)
		}
	}
	states := map[string]string{}
	if len(ids) > 0 {
		docs, err := s.fetchRows(ctx, user, model, "id", ids, false)
		if err != nil {
			return nil, err
		}
		for _, d := range docs {
			st := ""
			if src.StateField != "" && d[src.StateField] != nil {
				st = fmt.Sprint(d[src.StateField])
			}
			states[fmt.Sprint(d["id"])] = st
		}
	}
	return sumConsumed(lines, states, *src, typ.ForeignKey, excludeDocID), nil
}

// checkDocumentLines rejects lines that ask for more than what is pending on
// their source line. excludeDocID is the document being updated ("" on create).
func (s *Service) checkDocumentLines(ctx context.Context, user modelbase.AuthUser, model string, dl *documentLines, excludeDocID string) error {
	if dl == nil || len(dl.rows) == 0 {
		return nil
	}
	ve := NewValidationError()
	for i := range dl.typ.Sources {
		src := &dl.typ.Sources[i]
		if !src.tracksRemaining() {
			continue
		}
		var ids []any
		seen := map[string]struct{}{}
		for _, r := range dl.rows {
			id := linkValue(r, src.LineLinkField)
			if _, dup := seen[id]; id != "" && !dup {
				seen[id] = struct{}{}
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 {
			continue
		}
		srcRows, err := s.fetchRows(ctx, user, src.LinesModel, "id", ids, false)
		if err != nil {
			return err
		}
		consumed := map[string]float64{}
		if src.RemainingQtyField == "" {
			if consumed, err = s.consumedBySource(ctx, user, model, dl.typ, src, ids, excludeDocID); err != nil {
				return err
			}
		}
		remaining := map[string]float64{}
		for _, p := range pendingLines(srcRows, consumed, *src) {
			remaining[fmt.Sprint(p["source_line_id"])] = toFloat(p["remaining_quantity"])
		}
		addQuantityErrors(ve, dl.typ.Field, dl.rows, dl.pos, remaining, *src)
	}
	return ve.Err()
}

// checkDocumentSources re-checks the source document a «crear desde» payload
// links (the source's link_field in the header) against the source's
// option_filter — the same rule its picker applied client-side. Without it a
// document the picker hides (a quote already converted to a sale, a cancelled
// work order) could still be posted by editing the payload, and its goods
// invoiced twice through two links the pending-quantity check traces apart.
//
// It runs only on the editor's write path (the payload carries the lines,
// dl != nil), like checkDocumentLines. On Update (excludeDocID != "") a link
// that did not change is not re-checked: a source that stopped matching after
// the document was born (the quote was converted later) must not lock its
// edits. A source model the host cannot resolve (its addon is not installed)
// is skipped. A linked id that does not exist in the caller's organization is
// rejected.
func (s *Service) checkDocumentSources(ctx context.Context, user modelbase.AuthUser, model string, dl *documentLines, input map[string]any, excludeDocID string) error {
	if dl == nil || dl.typ == nil {
		return nil
	}
	ve := NewValidationError()
	var current map[string]any
	loaded := false
	for i := range dl.typ.Sources {
		src := &dl.typ.Sources[i]
		if len(src.OptionFilter) == 0 || !isSafeIdent(src.LinkField) {
			continue
		}
		id := linkValue(input, src.LinkField)
		if id == "" {
			continue
		}
		if _, err := uuid.Parse(id); err != nil {
			continue // a malformed id is the write validation's to report
		}
		if excludeDocID != "" {
			if !loaded {
				loaded = true
				rows, err := s.fetchRows(ctx, user, model, "id", []any{excludeDocID}, false)
				if err != nil {
					return err
				}
				if len(rows) > 0 {
					current = rows[0]
				}
			}
			if current != nil && linkValue(current, src.LinkField) == id {
				continue
			}
		}
		if _, ok := s.lookupModel(ctx, src.Model); !ok {
			continue
		}
		rows, err := s.fetchRows(ctx, user, src.Model, "id", []any{id}, false)
		if err != nil {
			return err
		}
		switch {
		case len(rows) == 0:
			ve.Fields = appendFieldError(ve.Fields, src.LinkField, FieldError{
				Code:    "source_not_found",
				Params:  map[string]any{"source": src.Label},
				Message: "No se encontró el documento origen elegido" + sourceSuffix(*src) + ".",
			})
		case !src.OptionFilter.Match(rows[0]):
			ve.Fields = appendFieldError(ve.Fields, src.LinkField, FieldError{
				Code:    "source_not_eligible",
				Params:  map[string]any{"source": src.Label},
				Message: "El documento origen elegido" + sourceSuffix(*src) + " ya no se puede usar (por ejemplo, ya se convirtió, se canceló o cambió de estado). Elige otro.",
			})
		}
	}
	return ve.Err()
}

// addQuantityErrors compares the requested quantity per source line (a source
// line may be split over several lines) with what is pending and records one
// error per offending line, with a message ready to show.
// pos maps each row to its posted index (nil = the row index); the message
// counts renglones (items only), like the form does.
func addQuantityErrors(ve *ValidationError, field string, rows []map[string]any, pos []int, remaining map[string]float64, src DocumentLineSource) {
	asked := map[string]float64{}
	for i, r := range rows {
		id := linkValue(r, src.LineLinkField)
		if id == "" {
			continue
		}
		at := i
		if i < len(pos) {
			at = pos[i]
		}
		key := fmt.Sprintf("%s.%d.quantity", field, at)
		pending, known := remaining[id]
		if !known {
			ve.Fields = appendFieldError(ve.Fields, key, FieldError{
				Code:    "source_line_not_found",
				Params:  map[string]any{"line": i + 1, "source": src.Label},
				Message: fmt.Sprintf("Renglón %d: el renglón de origen ya no existe en %s.", i+1, sourceName(src)),
			})
			continue
		}
		qty := toFloat(r["quantity"])
		asked[id] += qty
		if asked[id] > pending+qtyEpsilon {
			left := pending - (asked[id] - qty)
			if left < 0 {
				left = 0
			}
			ve.Fields = appendFieldError(ve.Fields, key, FieldError{
				Code:   "exceeds_remaining",
				Params: map[string]any{"line": i + 1, "requested": qty, "remaining": roundQty(left), "source": src.Label},
				Message: fmt.Sprintf("Renglón %d: la cantidad %s excede lo pendiente de %s (%s). Ajusta la cantidad; lo demás ya está registrado en otro documento.",
					i+1, fmtQty(qty), sourceName(src), fmtQty(roundQty(left))),
			})
		}
	}
}

func appendFieldError(m map[string][]FieldError, key string, fe FieldError) map[string][]FieldError {
	if m == nil {
		m = map[string][]FieldError{}
	}
	m[key] = append(m[key], fe)
	return m
}

// sourceSuffix names the source kind after "el documento origen elegido"
// (" en «Cotización»"), or nothing when its label is an i18n key.
func sourceSuffix(src DocumentLineSource) string {
	if strings.TrimSpace(src.Label) != "" && !strings.Contains(src.Label, ".") {
		return " en «" + src.Label + "»"
	}
	return ""
}

func sourceName(src DocumentLineSource) string {
	if strings.TrimSpace(src.Label) != "" && !strings.Contains(src.Label, ".") {
		return "«" + src.Label + "»"
	}
	return "el documento origen"
}

// writeDocumentLines creates the lines of a document through the regular
// Create of the line model (formulas, rollups, hooks and events included).
// Returns the ids written so far, so a caller can undo them, and a line's
// validation failure re-keyed under the lines field ("items.2.product_id").
func (s *Service) writeDocumentLines(ctx context.Context, user modelbase.AuthUser, dl *documentLines, docID string) ([]string, error) {
	if dl == nil {
		return nil, nil
	}
	var written []string
	for i, r := range dl.rows {
		row := make(map[string]any, len(r)+1)
		for k, v := range r {
			row[k] = v
		}
		// The client's line id (if any) is not trusted: every line is a new row.
		id := uuid.NewString()
		row["id"] = id
		row[dl.typ.ForeignKey] = docID
		if _, err := s.Create(ctx, dl.typ.LineModel, user, row); err != nil {
			return written, lineError(dl.typ.Field, dl.at(i), i, err)
		}
		written = append(written, id)
	}
	return written, nil
}

// lineError places a line's field errors under "<field>.<i>.<column>" so the
// form marks the line; other errors keep their cause with the line number.
func lineError(field string, at, i int, err error) error {
	var ve *ValidationError
	if errors.As(err, &ve) && !ve.Empty() {
		out := NewValidationError()
		for col, list := range ve.Fields {
			for _, fe := range list {
				out.Fields = appendFieldError(out.Fields, fmt.Sprintf("%s.%d.%s", field, at, col), fe)
			}
		}
		return out
	}
	return fmt.Errorf("renglón %d: %w", i+1, err)
}

// undoDocument removes a document whose lines could not be written, with the
// lines already written: a document must not survive without the lines the
// user saw. Hard delete — its created event was never published.
func (s *Service) undoDocument(ctx context.Context, user modelbase.AuthUser, model, table string, instance any, docID string, dl *documentLines, lineIDs []string) {
	if len(lineIDs) > 0 {
		if inst, ok := s.lookupModel(ctx, dl.typ.LineModel); ok {
			if lt, err := s.tableNameFor(ctx, dl.typ.LineModel, inst); err == nil {
				_ = s.db.WithContext(ctx).Table(lt).Where("id IN ?", lineIDs).Delete(map[string]any{}).Error
			}
		}
	}
	_ = s.db.WithContext(ctx).Table(table).Where("id = ?", docID).Delete(map[string]any{}).Error
}

// replaceDocumentLines swaps a document's lines for the posted ones (Update).
func (s *Service) replaceDocumentLines(ctx context.Context, user modelbase.AuthUser, dl *documentLines, docID string) error {
	if dl == nil {
		return nil
	}
	old, err := s.fetchRows(ctx, user, dl.typ.LineModel, dl.typ.ForeignKey, []any{docID}, false)
	if err != nil {
		return err
	}
	for _, r := range old {
		id, perr := uuid.Parse(fmt.Sprint(r["id"]))
		if perr != nil {
			continue
		}
		if err := s.Delete(ctx, dl.typ.LineModel, user, id); err != nil {
			return err
		}
	}
	_, err = s.writeDocumentLines(ctx, user, dl, docID)
	return err
}

// fetchRows reads the rows of model whose col is one of vals, org-scoped.
// authorize=true also requires list access (data served to the caller);
// internal integrity reads skip it.
func (s *Service) fetchRows(ctx context.Context, user modelbase.AuthUser, model, col string, vals []any, authorize bool) ([]map[string]any, error) {
	if !isSafeIdent(col) {
		return nil, fmt.Errorf("%w: column %q", ErrInvalidInput, col)
	}
	instance, ok := s.lookupModel(ctx, model)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrModelNotFound, model)
	}
	if authorize {
		if err := s.authorize(ctx, user, model, instance, modelbase.AccessList); err != nil {
			return nil, err
		}
	}
	table, err := s.tableNameFor(ctx, model, instance)
	if err != nil {
		return nil, err
	}
	db, err := s.scopeOrDeny(s.tableDB(ctx, s.db.WithContext(ctx), table, instance), instance, user)
	if err != nil {
		return nil, err
	}
	results := reflect.New(reflect.SliceOf(reflect.TypeOf(instance))).Interface()
	if err := db.Where(quoteIdent(col)+" IN ?", vals).Limit(1000).Find(results).Error; err != nil {
		return nil, fmt.Errorf("dynamic: %s lines: %w", model, err)
	}
	return toMapSlice(results), nil
}

func rowIDs(rows []map[string]any) []any {
	out := make([]any, 0, len(rows))
	for _, r := range rows {
		if r["id"] != nil {
			out = append(out, fmt.Sprint(r["id"]))
		}
	}
	return out
}

func linkValue(r map[string]any, field string) string {
	v, ok := r[field]
	if !ok || v == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(v))
}

const qtyEpsilon = 1e-9

func roundQty(v float64) float64 {
	r, _ := strconv.ParseFloat(strconv.FormatFloat(v, 'f', 6, 64), 64)
	return r
}

func fmtQty(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
