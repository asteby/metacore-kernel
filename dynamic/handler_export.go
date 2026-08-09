package dynamic

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/asteby/metacore-kernel/importer"
	"github.com/asteby/metacore-kernel/modelbase"
	"github.com/asteby/metacore-kernel/query"
	"github.com/gofiber/fiber/v3"
)

// exportLimit caps the number of rows a single export request can stream.
// Tuned to comfortably hold a typical CRM table in memory; apps with bigger
// datasets should plug an async export path on top of `dynamic.Service`.
const exportLimit = 100_000

// exportCol is one spreadsheet column: key for value lookup, label for the
// header row, plus the TableMetadata cues the cell formatter needs.
type exportCol struct {
	Key          string
	Label        string
	Type         string
	CellStyle    string
	DisplayField string
	RelationPath string
	Tooltip      string
	Description  string
	Options      map[string]string
}

// exportData handles GET /dynamic/:model/export?format=csv&columns=a,b,c
// Streams every row visible to the caller, encoded as CSV. Format defaults
// to CSV; `columns` (optional) restricts the output to a subset and
// preserves their order. Falls back to the model's TableMetadata column
// order when omitted.
//
// Headers prefer human labels:
//  1. `column_labels` query (JSON key→label from the SDK ExportDialog —
//     resolves BOTH core DefineTable text and manifest i18n keys on the
//     frontend, where addon bundles live).
//  2. ColumnDef.Label from TableMetadata (host may have already localized).
//  3. The column key as last resort.
//
// Cell values are flattened to spreadsheet-friendly text (relation lists,
// avatar name/email, booleans, dates) — not raw JSON dumps.
func (h *Handler) exportData(c fiber.Ctx) error {
	u := h.user(c)
	if u == nil {
		return respondErr(c, fiber.StatusUnauthorized, "not authenticated")
	}
	model := c.Params("model")
	cols, err := resolveExportColumns(c, h, model)
	if err != nil {
		return h.handleError(c, err)
	}
	if len(cols) == 0 {
		return respondErr(c, fiber.StatusBadRequest, "no exportable columns")
	}

	params, err := query.ParseFiber(c)
	if err != nil {
		return respondErr(c, fiber.StatusBadRequest, err.Error())
	}
	params.Page = 1
	params.PerPage = exportLimit
	items, _, err := h.service.List(c, model, u, params)
	if err != nil {
		return h.handleError(c, err)
	}

	records := make([][]string, 0, len(items)+1)
	headers := make([]string, len(cols))
	for i, col := range cols {
		headers[i] = col.Label
	}
	records = append(records, headers)
	for _, row := range items {
		rec := make([]string, len(cols))
		for i, col := range cols {
			rec[i] = formatExportCell(col, row)
		}
		records = append(records, rec)
	}
	// BOM included so Excel opens the export as UTF-8 rather than the local
	// ANSI codepage, which mangles accented values.
	data, err := importer.WriteCSVWithBOM(records)
	if err != nil {
		return respondErr(c, fiber.StatusInternalServerError, err.Error())
	}

	c.Set(fiber.HeaderContentType, "text/csv; charset=utf-8")
	c.Set(fiber.HeaderContentDisposition, "attachment; filename=\""+model+".csv\"")
	return c.Send(data)
}

// exportTemplate handles GET /dynamic/:model/export/template — the file users
// fill in and feed back through importData. Format follows `?format=`: xlsx
// (default) renders the model's ImportSpec with an example row, hints and an
// instructions sheet; csv emits just the header row for tooling that wants a
// flat file.
func (h *Handler) exportTemplate(c fiber.Ctx) error {
	model := c.Params("model")
	spec, err := h.service.ImportSpec(c, model)
	if err != nil {
		return h.handleError(c, err)
	}
	if len(spec.Columns) == 0 {
		return respondErr(c, fiber.StatusUnprocessableEntity, "model has no importable columns")
	}

	if strings.EqualFold(c.Query("format"), "csv") {
		headers := make([]string, 0, len(spec.Columns))
		for _, col := range spec.Columns {
			headers = append(headers, col.TemplateHeader())
		}
		// BOM included so Excel opens the file as UTF-8; the parser strips it
		// on the way back in (see importer.StripBOM).
		data, err := importer.WriteCSVWithBOM([][]string{headers})
		if err != nil {
			return respondErr(c, fiber.StatusInternalServerError, err.Error())
		}
		c.Set(fiber.HeaderContentType, "text/csv; charset=utf-8")
		c.Set(fiber.HeaderContentDisposition, "attachment; filename=\""+model+"-template.csv\"")
		return c.Send(data)
	}

	title := model
	if meta, err := h.service.TableMetadata(c, model); err == nil && meta.Title != "" {
		title = meta.Title
	}
	data, err := importer.BuildTemplate(spec, title)
	if err != nil {
		return respondErr(c, fiber.StatusInternalServerError, err.Error())
	}
	c.Set(fiber.HeaderContentType, "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	c.Set(fiber.HeaderContentDisposition, "attachment; filename=\""+model+"-template.xlsx\"")
	return c.Send(data)
}

// importValidate handles POST /dynamic/:model/import/validate — parses the
// uploaded file and reports row-by-row issues WITHOUT touching the DB, so the
// user fixes a spreadsheet before any partial write happens.
func (h *Handler) importValidate(c fiber.Ctx) error {
	if h.user(c) == nil {
		return respondErr(c, fiber.StatusUnauthorized, "not authenticated")
	}
	// Check `prepared`, NOT the error: prepareImport writes its own HTTP
	// response and returns whatever c.JSON() returned, which is nil on a
	// successful write. Branching on the error would sail past a rejected
	// upload and dereference a nil Prepared.
	prepared, spec, err := h.prepareImport(c)
	if prepared == nil {
		return err
	}
	return c.JSON(fiber.Map{
		"success": len(prepared.Issues) == 0,
		"data": fiber.Map{
			"rowCount": len(prepared.Records),
			"skipped":  prepared.Skipped,
			"sample":   firstN(importer.RedactGenerated(spec, prepared.Records), 5),
			"errors":   prepared.Issues,
		},
	})
}

// importData handles POST /dynamic/:model/import — validates the upload the
// same way importValidate does, then creates one record per valid row through
// the regular Service.Create pipeline (so permissions, hooks and validation
// all run). Rows that failed validation are reported alongside rows the
// database rejected; one bad row never blocks the rest.
func (h *Handler) importData(c fiber.Ctx) error {
	u := h.user(c)
	if u == nil {
		return respondErr(c, fiber.StatusUnauthorized, "not authenticated")
	}
	model := c.Params("model")
	prepared, _, err := h.prepareImport(c)
	if prepared == nil {
		return err
	}

	created := 0
	failures := make([]map[string]any, 0, len(prepared.Issues))
	for _, issue := range prepared.Issues {
		failures = append(failures, map[string]any{
			"row":    issue.Row,
			"column": issue.Column,
			"error":  issue.Message,
		})
	}
	for i, record := range prepared.Records {
		if _, err := h.service.Create(c, model, u, record); err != nil {
			// The record is deliberately NOT echoed back: it carries generator
			// output (for a spec using `random_secret`, the account's plaintext
			// password) plus whatever PII the row held, and this response is
			// rendered in a browser and often logged. The row number is what
			// the user needs to find the offending line in their file.
			failures = append(failures, map[string]any{
				"row":   prepared.RowNumbers[i],
				"error": err.Error(),
			})
			continue
		}
		created++
	}

	status := fiber.StatusOK
	if len(failures) > 0 && created == 0 {
		status = fiber.StatusUnprocessableEntity
	}
	return c.Status(status).JSON(fiber.Map{
		"success": len(failures) == 0 && created > 0,
		"data": fiber.Map{
			"created":  created,
			"failed":   len(failures),
			"skipped":  prepared.Skipped,
			"failures": failures,
		},
	})
}

// prepareImport is the shared front half of validate and import: one parse,
// one spec, one set of rules — all of it living in the reusable `importer`
// engine so hosts that have not yet adopted dynamic.Service still run the
// exact same code. The returned error is already an HTTP response.
func (h *Handler) prepareImport(c fiber.Ctx) (*importer.Prepared, modelbase.ImportSpec, error) {
	model := c.Params("model")
	spec, err := h.service.ImportSpec(c, model)
	if err != nil {
		return nil, spec, h.handleError(c, err)
	}
	if len(spec.Columns) == 0 {
		return nil, spec, respondErr(c, fiber.StatusUnprocessableEntity, "model has no importable columns")
	}
	rows, err := importer.ReadRows(c)
	if err != nil {
		return nil, spec, respondErr(c, fiber.StatusBadRequest, err.Error())
	}
	if len(rows) == 0 {
		return nil, spec, respondErr(c, fiber.StatusBadRequest, "the file contains no data rows")
	}
	prepared, err := importer.Prepare(spec, rows)
	if err != nil {
		return nil, spec, respondErr(c, fiber.StatusUnprocessableEntity, err.Error())
	}
	return prepared, spec, nil
}

// resolveExportColumns builds the ordered export column list from
// TableMetadata, optional `columns` selection, and optional `column_labels`
// overlay from the SDK.
func resolveExportColumns(c fiber.Ctx, h *Handler, model string) ([]exportCol, error) {
	meta, err := h.service.TableMetadata(c, model)
	if err != nil {
		return nil, err
	}
	all, byKey := exportColsFromMeta(meta.Columns)

	rawSel := strings.TrimSpace(c.Query("columns"))
	var out []exportCol
	if rawSel == "" {
		out = all
	} else {
		parts := strings.Split(rawSel, ",")
		out = make([]exportCol, 0, len(parts))
		seen := map[string]bool{}
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			ec, ok := byKey[p]
			if !ok {
				ec = exportCol{Key: p, Label: humanizeExportKey(p)}
			}
			if seen[ec.Key] {
				continue
			}
			seen[ec.Key] = true
			out = append(out, ec)
		}
		if len(out) == 0 {
			out = all
		}
	}
	applyClientColumnLabels(c.Query("column_labels"), out)
	return out, nil
}

// exportColsFromMeta projects TableMetadata columns into exportCols, skipping
// hidden / actions / pure-image columns that are useless in a spreadsheet.
func exportColsFromMeta(cols []modelbase.ColumnDef) ([]exportCol, map[string]exportCol) {
	all := make([]exportCol, 0, len(cols))
	byKey := make(map[string]exportCol, len(cols)*2)
	for _, col := range cols {
		if col.Hidden || col.Key == "" || col.Key == "actions" {
			continue
		}
		// Pure image columns (no avatar-with-name) are useless in a spreadsheet.
		if (col.Type == "image" || col.Type == "media-gallery") && col.CellStyle != "avatar" {
			continue
		}
		label := strings.TrimSpace(col.Label)
		if label == "" {
			label = col.Key
		}
		ec := exportCol{
			Key:          col.Key,
			Label:        label,
			Type:         col.Type,
			CellStyle:    col.CellStyle,
			DisplayField: col.DisplayField,
			RelationPath: col.RelationPath,
			Tooltip:      col.Tooltip,
			Description:  col.Description,
			Options:      optionsMapFromDefs(col.Options),
		}
		all = append(all, ec)
		byKey[col.Key] = ec
		byKey[strings.ReplaceAll(col.Key, ".", "_")] = ec
	}
	return all, byKey
}

func optionsMapFromDefs(opts []modelbase.OptionDef) map[string]string {
	if len(opts) == 0 {
		return nil
	}
	out := make(map[string]string, len(opts))
	for _, o := range opts {
		val := fmt.Sprintf("%v", o.Value)
		if val != "" && o.Label != "" {
			out[val] = o.Label
		}
	}
	return out
}

// applyClientColumnLabels overlays labels sent by the SDK ExportDialog
// (`column_labels` JSON map key→localized label). That lets CSV headers follow
// the UI locale for BOTH core human text and manifest i18n keys (resolved on
// the frontend, where addon bundles live).
func applyClientColumnLabels(raw string, cols []exportCol) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil || len(m) == 0 {
		return
	}
	for i := range cols {
		if label, ok := m[cols[i].Key]; ok && strings.TrimSpace(label) != "" {
			cols[i].Label = strings.TrimSpace(label)
			continue
		}
		alt := strings.ReplaceAll(cols[i].Key, ".", "_")
		if label, ok := m[alt]; ok && strings.TrimSpace(label) != "" {
			cols[i].Label = strings.TrimSpace(label)
		}
	}
}

func humanizeExportKey(key string) string {
	key = strings.ReplaceAll(key, "_", " ")
	key = strings.ReplaceAll(key, ".", " ")
	return strings.TrimSpace(key)
}

// formatExportCell turns a row value into a spreadsheet-friendly string using
// the column's table metadata (same cues the UI uses to render the cell).
func formatExportCell(col exportCol, row map[string]any) string {
	// Avatar / user columns: prefer name + email over the image filename.
	if col.CellStyle == "avatar" || (col.Tooltip != "" && strings.Contains(col.Key, "avatar")) {
		namePath := firstNonEmpty(col.Tooltip, strings.Replace(col.Key, ".avatar", ".name", 1))
		name := stringify(extractByDotPath(row, namePath))
		email := ""
		if col.Description != "" {
			email = stringify(extractByDotPath(row, col.Description))
		}
		switch {
		case name != "" && email != "":
			return name + " <" + email + ">"
		case name != "":
			return name
		case email != "":
			return email
		}
	}

	raw := extractByDotPath(row, col.Key)

	if col.Type == "boolean" || col.CellStyle == "boolean" {
		return formatExportBool(raw, col.Options)
	}

	if len(col.Options) > 0 {
		if label, ok := col.Options[fmt.Sprintf("%v", raw)]; ok {
			return label
		}
	}

	if col.CellStyle == "relation-badge-list" || col.Type == "relation-badge-list" || col.DisplayField != "" {
		if joined := joinRelationLabels(raw, col.RelationPath, firstNonEmpty(col.DisplayField, "name", "title", "label")); joined != "" {
			return joined
		}
	}

	if col.Type == "date" || col.Type == "datetime" || col.CellStyle == "date" {
		if s := formatExportDate(raw); s != "" {
			return s
		}
	}

	return stringifyReadable(raw)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func formatExportBool(v any, options map[string]string) string {
	truthy := false
	switch t := v.(type) {
	case nil:
		return ""
	case bool:
		truthy = t
	case string:
		truthy = strings.EqualFold(t, "true") || t == "1"
	case float64:
		truthy = t != 0
	case int:
		truthy = t != 0
	case int64:
		truthy = t != 0
	default:
		truthy = fmt.Sprintf("%v", v) == "true"
	}
	key := "false"
	if truthy {
		key = "true"
	}
	if label, ok := options[key]; ok {
		return label
	}
	return strconv.FormatBool(truthy)
}

func formatExportDate(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return ""
		}
		// Keep YYYY-MM-DD when present; trim time for date-looking values.
		if len(s) >= 10 && s[4] == '-' && s[7] == '-' {
			if len(s) == 10 || strings.Contains(s, "T") || strings.Contains(s, " ") {
				return s[:10]
			}
		}
		return s
	default:
		return ""
	}
}

// joinRelationLabels flattens arrays of related records into "A, B, C".
// relationPath (e.g. "Specialty") selects a nested object on each item when set.
func joinRelationLabels(v any, relationPath, displayField string) string {
	items := asAnySlice(v)
	if len(items) == 0 {
		if m, ok := asStringMap(v); ok {
			if s := pickDisplay(m, relationPath, displayField); s != "" {
				return s
			}
		}
		return ""
	}
	parts := make([]string, 0, len(items))
	seen := map[string]bool{}
	for _, item := range items {
		m, ok := asStringMap(item)
		if !ok {
			s := strings.TrimSpace(stringify(item))
			if s != "" && !seen[s] {
				seen[s] = true
				parts = append(parts, s)
			}
			continue
		}
		s := pickDisplay(m, relationPath, displayField)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		parts = append(parts, s)
	}
	return strings.Join(parts, ", ")
}

func pickDisplay(m map[string]any, relationPath, displayField string) string {
	target := m
	if relationPath != "" {
		nested := firstMap(m, relationPath, strings.ToLower(relationPath), snakeCase(relationPath))
		if nested != nil {
			target = nested
		}
	}
	if displayField != "" {
		if s := strings.TrimSpace(stringify(target[displayField])); s != "" {
			return s
		}
	}
	for _, k := range []string{"name", "title", "label", "email"} {
		if s := strings.TrimSpace(stringify(target[k])); s != "" {
			return s
		}
	}
	return ""
}

func firstMap(m map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		if k == "" {
			continue
		}
		if nested, ok := asStringMap(m[k]); ok {
			return nested
		}
	}
	return nil
}

func snakeCase(s string) string {
	if s == "" {
		return s
	}
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r + ('a' - 'A'))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func asAnySlice(v any) []any {
	switch t := v.(type) {
	case nil:
		return nil
	case []any:
		return t
	case []map[string]any:
		out := make([]any, len(t))
		for i := range t {
			out[i] = t[i]
		}
		return out
	default:
		return nil
	}
}

func asStringMap(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

// extractByDotPath supports keys like "user.name" by walking nested maps.
func extractByDotPath(row map[string]any, path string) any {
	if path == "" {
		return nil
	}
	if !strings.Contains(path, ".") {
		return row[path]
	}
	parts := strings.Split(path, ".")
	var cur any = row
	for _, p := range parts {
		m, ok := asStringMap(cur)
		if !ok {
			return nil
		}
		cur = m[p]
	}
	return cur
}

func stringify(v any) string {
	switch val := v.(type) {
	case nil:
		return ""
	case string:
		return val
	case []byte:
		return string(val)
	case bool:
		return strconv.FormatBool(val)
	case float64:
		if val == float64(int64(val)) {
			return strconv.FormatInt(int64(val), 10)
		}
		return strconv.FormatFloat(val, 'f', -1, 64)
	case int:
		return strconv.Itoa(val)
	case int64:
		return strconv.FormatInt(val, 10)
	case json.Number:
		return val.String()
	default:
		return fmt.Sprint(v)
	}
}

// stringifyReadable never dumps raw JSON arrays/objects into the cell —
// it prefers common label fields, otherwise an empty string.
func stringifyReadable(v any) string {
	switch val := v.(type) {
	case nil:
		return ""
	case map[string]any:
		for _, k := range []string{"name", "title", "label", "email", "slug"} {
			if s := strings.TrimSpace(stringify(val[k])); s != "" {
				return s
			}
		}
		return ""
	case []any, []map[string]any:
		return joinRelationLabels(val, "", "name")
	default:
		return stringify(v)
	}
}

func firstN[T any](items []T, n int) []T {
	if len(items) <= n {
		return items
	}
	return items[:n]
}
