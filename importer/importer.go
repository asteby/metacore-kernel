package importer

import (
	"bytes"
	"crypto/rand"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/asteby/metacore-kernel/modelbase"
	"github.com/gofiber/fiber/v3"
	"github.com/xuri/excelize/v2"
)

// This file is the spreadsheet-import engine: it turns an uploaded CSV / XLSX /
// JSON file into records for Service.Create, and turns a model's ImportSpec
// into the template users download to produce that file. Both directions read
// the SAME spec, so a column cannot be renamed in the template without the
// parser following along.
//
// Everything here is model-agnostic. Domain specifics (which columns exist,
// what they are called, which value is generated when blank) live in the
// model's ImportSpec — never in this package.

// Generator produces a value for a column left blank by the user.
type Generator func() (any, error)

var (
	genMu      sync.RWMutex
	generators = map[string]Generator{
		// random_secret is the default credential generator: a 32-char hex
		// string. Models use it for password columns so a blank cell yields an
		// unguessable value the user resets via the standard recovery flow,
		// instead of an account with a predictable password.
		"random_secret": func() (any, error) {
			buf := make([]byte, 16)
			if _, err := io.ReadFull(rand.Reader, buf); err != nil {
				return nil, err
			}
			return hex.EncodeToString(buf), nil
		},
	}
)

// RegisterGenerator makes a named generator available to every model's
// ImportSpec. Registering twice under the same name replaces the previous one.
func RegisterGenerator(name string, gen Generator) {
	if name == "" || gen == nil {
		return
	}
	genMu.Lock()
	defer genMu.Unlock()
	generators[name] = gen
}

func lookupGenerator(name string) (Generator, bool) {
	genMu.RLock()
	defer genMu.RUnlock()
	gen, ok := generators[name]
	return gen, ok
}

// RowIssue is one problem found in one uploaded row. Row is 1-based over the
// DATA rows as the user sees them in the spreadsheet (header row excluded), so
// it can be quoted back in the UI without off-by-one translation.
type RowIssue struct {
	Row     int    `json:"row"`
	Column  string `json:"column,omitempty"`
	Message string `json:"message"`
}

// BuildRecord maps one raw row (keyed by whatever headers the file had)
// onto the record passed to Service.Create, resolving aliases, applying
// generators and validating cell values against the spec.
func BuildRecord(spec modelbase.ImportSpec, raw map[string]any) (map[string]any, []RowIssue) {
	index := spec.HeaderIndex()
	// Collapse the raw row onto column keys first, so a value found under an
	// alias is indistinguishable from one found under the canonical header.
	byKey := make(map[string]string, len(spec.Columns))
	for header, value := range raw {
		col, ok := index[normalizeHeader(header)]
		if !ok {
			continue // unknown columns are ignored, not fatal
		}
		if s := strings.TrimSpace(stringify(value)); s != "" {
			byKey[col.Key] = s
		}
	}

	record := make(map[string]any, len(spec.Columns))
	issues := make([]RowIssue, 0)
	for _, col := range spec.Columns {
		value, present := byKey[col.Key]
		if !present {
			if col.Generator != "" {
				if gen, ok := lookupGenerator(col.Generator); ok {
					generated, err := gen()
					if err != nil {
						issues = append(issues, RowIssue{Column: col.Header, Message: fmt.Sprintf("no se pudo generar el valor: %v", err)})
						continue
					}
					setPath(record, col.Key, generated)
					continue
				}
				// Unknown generator: fall through to the Required check rather
				// than accepting the row. Swallowing it here would let a
				// required column through empty on a typo'd generator name and
				// surface as an opaque database error much later.
			}
			if col.Required {
				issues = append(issues, RowIssue{Column: col.Header, Message: fmt.Sprintf("falta '%s'", col.Header)})
			}
			continue
		}
		coerced, err := coerceCell(col, value)
		if err != nil {
			issues = append(issues, RowIssue{Column: col.Header, Message: err.Error()})
			continue
		}
		setPath(record, col.Key, coerced)
	}
	if len(issues) > 0 {
		return nil, issues
	}
	return record, nil
}

// coerceCell converts a spreadsheet cell (always text) into the Go value
// the column's type implies, rejecting what cannot be converted.
func coerceCell(col modelbase.ImportColumn, value string) (any, error) {
	switch col.Type {
	case "number":
		n, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return nil, fmt.Errorf("'%s' debe ser un número (recibido: %q)", col.Header, value)
		}
		return n, nil
	case "boolean":
		switch strings.ToLower(value) {
		case "true", "1", "sí", "si", "yes", "x":
			return true, nil
		case "false", "0", "no":
			return false, nil
		}
		return nil, fmt.Errorf("'%s' debe ser sí o no (recibido: %q)", col.Header, value)
	case "date":
		// Only unambiguous layouts are accepted, and the value is normalised to
		// ISO before it reaches Create. Supporting both D/M/Y and M/D/Y would
		// silently reinterpret "03/04/2026" depending on layout order, turning
		// April 3rd into March 4th with nothing to warn the user.
		for _, layout := range []string{time.RFC3339, "2006-01-02"} {
			if parsed, err := time.Parse(layout, value); err == nil {
				return parsed.Format("2006-01-02"), nil
			}
		}
		return nil, fmt.Errorf("'%s' debe ser una fecha con formato AAAA-MM-DD (recibido: %q)", col.Header, value)
	case "email":
		at := strings.Index(value, "@")
		if at <= 0 || at == len(value)-1 || strings.Contains(value, " ") {
			return nil, fmt.Errorf("'%s' no es un correo válido (recibido: %q)", col.Header, value)
		}
		return value, nil
	default:
		return value, nil
	}
}

// setPath writes value at a dot-path, creating intermediate maps. The
// dynamic create pipeline also accepts the flat dotted key, but writing the
// nested shape keeps the record readable when it is echoed back in an error.
func setPath(record map[string]any, path string, value any) {
	parts := strings.Split(path, ".")
	if len(parts) == 1 {
		record[path] = value
		return
	}
	cursor := record
	for _, part := range parts[:len(parts)-1] {
		next, ok := cursor[part].(map[string]any)
		if !ok {
			next = map[string]any{}
			cursor[part] = next
		}
		cursor = next
	}
	cursor[parts[len(parts)-1]] = value
}

// TemplateMarkerHeader is the column the generated template adds to tag its own
// guide rows. It carries no data — it exists so the parser can recognise the
// example and hint rows WITHOUT comparing cell values against the spec.
// Matching by content was the earlier approach and it silently dropped real
// data: a user whose row happened to equal the example (a common case for a
// short enum or a placeholder name) had their record discarded with no error.
// A user who deletes this column simply gets their guide rows imported as
// data, which fails loudly instead of vanishing.
const TemplateMarkerHeader = "__metacore_template_row"

// IsTemplateSampleRow reports whether a row is one of the guide rows the
// generated template ships. Users routinely upload the template without
// deleting them; treating them as data produces noisy errors that look like
// the import is broken.
func IsTemplateSampleRow(spec modelbase.ImportSpec, raw map[string]any) bool {
	for header, value := range raw {
		if normalizeHeader(header) != TemplateMarkerHeader {
			continue
		}
		return strings.TrimSpace(stringify(value)) != ""
	}
	return false
}

func normalizeHeader(h string) string {
	return strings.ToLower(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(h), "*")))
}

// BuildTemplate renders the downloadable XLSX for a spec: a data sheet
// with headers, an example row and a hints row, plus an instructions sheet
// when the spec provides one.
func BuildTemplate(spec modelbase.ImportSpec, title string) ([]byte, error) {
	f := excelize.NewFile()
	defer func() { _ = f.Close() }()

	sheet := spec.SheetName
	if sheet == "" {
		sheet = title
	}
	if sheet == "" {
		sheet = "Datos"
	}
	sheet = sanitizeSheetName(sheet)

	idx, err := f.NewSheet(sheet)
	if err != nil {
		return nil, fmt.Errorf("crear hoja: %w", err)
	}
	f.SetActiveSheet(idx)
	if sheet != "Sheet1" {
		_ = f.DeleteSheet("Sheet1")
	}

	headerStyle, _ := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Color: "#FFFFFF"},
		Fill:      excelize.Fill{Type: "pattern", Color: []string{"#1F4E79"}, Pattern: 1},
		Alignment: &excelize.Alignment{Horizontal: "center", Vertical: "center"},
	})
	exampleStyle, _ := f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Color: "#9CA3AF", Size: 11},
	})
	hintStyle, _ := f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Italic: true, Color: "#6B7280", Size: 10},
		Fill: excelize.Fill{Type: "pattern", Color: []string{"#F3F4F6"}, Pattern: 1},
	})

	for i, col := range spec.Columns {
		cell, _ := excelize.CoordinatesToCellName(i+1, 1)
		_ = f.SetCellValue(sheet, cell, col.TemplateHeader())
		_ = f.SetCellStyle(sheet, cell, cell, headerStyle)

		exampleCell, _ := excelize.CoordinatesToCellName(i+1, 2)
		_ = f.SetCellValue(sheet, exampleCell, col.Example)
		_ = f.SetCellStyle(sheet, exampleCell, exampleCell, exampleStyle)

		hintCell, _ := excelize.CoordinatesToCellName(i+1, 3)
		_ = f.SetCellValue(sheet, hintCell, col.Hint)
		_ = f.SetCellStyle(sheet, hintCell, hintCell, hintStyle)

		colName, _ := excelize.ColumnNumberToName(i + 1)
		_ = f.SetColWidth(sheet, colName, colName, 32)
	}
	// Marker column: tags the two guide rows so the parser can drop them by
	// declaration instead of guessing from their content. Hidden so it does not
	// distract from the columns the user has to fill.
	markerCol := len(spec.Columns) + 1
	markerCell, _ := excelize.CoordinatesToCellName(markerCol, 1)
	_ = f.SetCellValue(sheet, markerCell, TemplateMarkerHeader)
	exampleMarker, _ := excelize.CoordinatesToCellName(markerCol, 2)
	_ = f.SetCellValue(sheet, exampleMarker, "example")
	hintMarker, _ := excelize.CoordinatesToCellName(markerCol, 3)
	_ = f.SetCellValue(sheet, hintMarker, "hint")
	if markerName, err := excelize.ColumnNumberToName(markerCol); err == nil {
		_ = f.SetColVisible(sheet, markerName, false)
	}

	_ = f.SetRowHeight(sheet, 1, 24)

	if len(spec.Instructions) > 0 {
		readme := "Instrucciones"
		if _, err := f.NewSheet(readme); err == nil {
			titleStyle, _ := f.NewStyle(&excelize.Style{
				Font: &excelize.Font{Bold: true, Size: 14, Color: "#1F4E79"},
			})
			bodyStyle, _ := f.NewStyle(&excelize.Style{
				Font:      &excelize.Font{Size: 11, Color: "#111827"},
				Alignment: &excelize.Alignment{WrapText: true, Vertical: "top"},
			})
			_ = f.SetColWidth(readme, "A", "A", 90)
			_ = f.SetCellValue(readme, "A1", title)
			_ = f.SetCellStyle(readme, "A1", "A1", titleStyle)
			for i, line := range spec.Instructions {
				cell := fmt.Sprintf("A%d", i+3)
				_ = f.SetCellValue(readme, cell, line)
				_ = f.SetCellStyle(readme, cell, cell, bodyStyle)
			}
		}
	}

	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		return nil, fmt.Errorf("escribir xlsx: %w", err)
	}
	return buf.Bytes(), nil
}

// sanitizeSheetName trims a title down to what Excel accepts as a sheet name.
func sanitizeSheetName(name string) string {
	replacer := strings.NewReplacer("[", "", "]", "", ":", "", "*", "", "?", "", "/", "", "\\", "")
	out := strings.TrimSpace(replacer.Replace(name))
	if len([]rune(out)) > 31 {
		out = string([]rune(out)[:31])
	}
	if out == "" {
		out = "Datos"
	}
	return out
}

// ParseXLSX reads the first sheet of a workbook, treating the first row
// as headers.
func ParseXLSX(r io.Reader) ([]map[string]any, error) {
	f, err := excelize.OpenReader(r)
	if err != nil {
		return nil, fmt.Errorf("leer xlsx: %w", err)
	}
	defer func() { _ = f.Close() }()

	sheets := f.GetSheetList()
	if len(sheets) == 0 {
		return nil, fmt.Errorf("el archivo no tiene hojas")
	}
	rows, err := f.GetRows(sheets[0])
	if err != nil {
		return nil, fmt.Errorf("leer filas: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	headers := rows[0]
	if len(headers) > 0 {
		headers[0] = StripBOM(headers[0])
	}
	out := make([]map[string]any, 0, len(rows)-1)
	for _, rec := range rows[1:] {
		row := make(map[string]any, len(headers))
		empty := true
		for i, key := range headers {
			if i < len(rec) {
				row[key] = rec[i]
				if strings.TrimSpace(rec[i]) != "" {
					empty = false
				}
			}
		}
		if empty {
			continue // trailing blank rows are an artifact of editing, not data
		}
		out = append(out, row)
	}
	return out, nil
}

func ParseJSON(body []byte) ([]map[string]any, error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil, nil
	}
	if body[0] == '[' {
		var rows []map[string]any
		if err := json.Unmarshal(body, &rows); err != nil {
			return nil, err
		}
		return rows, nil
	}
	var envelope struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, err
	}
	return envelope.Data, nil
}

// utf8BOM is what Excel writes at the head of a CSV it saves, and what it
// expects in order to open one as UTF-8 instead of the local ANSI codepage.
// Both halves of the round-trip must handle it: we emit it (see
// WriteCSVWithBOM) and we strip it here. Without this, a template downloaded,
// edited in Excel and re-uploaded loses its accented headers — "Años" arrives
// mangled, stops matching its column, and every required column reports as
// missing.
const utf8BOM = "\ufeff"

// StripBOM removes a leading UTF-8 byte-order mark, if present.
func StripBOM(s string) string {
	return strings.TrimPrefix(s, utf8BOM)
}

// WriteCSVWithBOM encodes rows as a CSV prefixed with the UTF-8 BOM so Excel
// opens it in the right codepage.
func WriteCSVWithBOM(records [][]string) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(utf8BOM)
	w := csv.NewWriter(&buf)
	for _, rec := range records {
		if err := w.Write(rec); err != nil {
			return nil, fmt.Errorf("csv encode: %w", err)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return nil, fmt.Errorf("csv flush: %w", err)
	}
	return buf.Bytes(), nil
}

func ParseCSV(r io.Reader) ([]map[string]any, error) {
	reader := csv.NewReader(r)
	reader.FieldsPerRecord = -1 // tolerate ragged rows
	records, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("csv decode: %w", err)
	}
	if len(records) == 0 {
		return nil, nil
	}
	headers := records[0]
	if len(headers) > 0 {
		headers[0] = StripBOM(headers[0])
	}
	rows := make([]map[string]any, 0, len(records)-1)
	for _, rec := range records[1:] {
		row := make(map[string]any, len(headers))
		for i, key := range headers {
			if i < len(rec) {
				row[key] = rec[i]
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// ReadRows reads the uploaded payload of an import request and returns its
// rows keyed by the file's own headers. Accepts a multipart `file` field
// (XLSX / CSV / JSON, detected by extension) or a raw JSON / CSV body, so a
// browser upload and a scripted POST hit the same path.
// MaxUploadBytes caps the payload ReadRows will parse. The row cap in Prepare
// only applies AFTER parsing, which is too late: a spreadsheet parser expands
// its input many times over in memory, so an oversized upload can exhaust the
// process before any row count is known. 16 MiB comfortably holds the
// thousand-row imports this path is designed for.
const MaxUploadBytes int64 = 16 << 20

// ErrUploadTooLarge is returned when the payload exceeds MaxUploadBytes.
type ErrUploadTooLarge struct {
	Got   int64
	Limit int64
}

func (e ErrUploadTooLarge) Error() string {
	return fmt.Sprintf("file too large (%d bytes); maximum allowed: %d", e.Got, e.Limit)
}

func ReadRows(c fiber.Ctx) ([]map[string]any, error) {
	contentType := strings.ToLower(c.Get(fiber.HeaderContentType))
	if strings.HasPrefix(contentType, "multipart/form-data") {
		fileHeader, err := c.FormFile("file")
		if err != nil {
			return nil, fmt.Errorf("expected `file` in multipart form: %w", err)
		}
		if fileHeader.Size > MaxUploadBytes {
			return nil, ErrUploadTooLarge{Got: fileHeader.Size, Limit: MaxUploadBytes}
		}
		f, err := fileHeader.Open()
		if err != nil {
			return nil, fmt.Errorf("open uploaded file: %w", err)
		}
		defer f.Close()
		// fileHeader.Size is client-declared; cap the stream itself so a lying
		// Content-Length cannot get past the check above.
		capped := io.LimitReader(f, MaxUploadBytes+1)
		name := strings.ToLower(fileHeader.Filename)
		switch {
		case strings.HasSuffix(name, ".json"):
			raw, err := io.ReadAll(capped)
			if err != nil {
				return nil, fmt.Errorf("read json body: %w", err)
			}
			if int64(len(raw)) > MaxUploadBytes {
				return nil, ErrUploadTooLarge{Got: int64(len(raw)), Limit: MaxUploadBytes}
			}
			return ParseJSON(raw)
		case strings.HasSuffix(name, ".xlsx"), strings.HasSuffix(name, ".xls"):
			return ParseXLSX(capped)
		default:
			return ParseCSV(capped)
		}
	}
	body := c.Body()
	if int64(len(body)) > MaxUploadBytes {
		return nil, ErrUploadTooLarge{Got: int64(len(body)), Limit: MaxUploadBytes}
	}
	if strings.HasPrefix(contentType, "application/json") {
		return ParseJSON(body)
	}
	// Fall back to CSV when the body looks like one.
	return ParseCSV(bytes.NewReader(body))
}

// Prepared is the outcome of parsing + validating an upload: the records ready
// to be created, the spreadsheet row number each came from (so an error can be
// quoted back precisely), the issues that disqualified the other rows, and how
// many template guide rows were ignored.
type Prepared struct {
	Records    []map[string]any
	RowNumbers []int
	Issues     []RowIssue
	Skipped    int
}

// ErrTooManyRows is returned by Prepare when the upload exceeds the spec's
// row cap. Callers map it to their own HTTP status.
type ErrTooManyRows struct {
	Got   int
	Limit int
}

func (e ErrTooManyRows) Error() string {
	return fmt.Sprintf("too many rows (%d); maximum allowed: %d", e.Got, e.Limit)
}

// Prepare turns raw parsed rows into records ready for creation, applying the
// spec's aliases, generators, coercions and row cap. It NEVER writes anything,
// which is what lets the validate endpoint and the import endpoint share one
// implementation and therefore agree on what is valid.
func Prepare(spec modelbase.ImportSpec, rows []map[string]any) (*Prepared, error) {
	if limit := spec.Limit(); len(rows) > limit {
		return nil, ErrTooManyRows{Got: len(rows), Limit: limit}
	}
	out := &Prepared{
		Records:    make([]map[string]any, 0, len(rows)),
		RowNumbers: make([]int, 0, len(rows)),
		Issues:     make([]RowIssue, 0),
	}
	for i, raw := range rows {
		// Row 1 is the header; data rows start at 2 in the user's spreadsheet.
		rowNumber := i + 2
		if IsTemplateSampleRow(spec, raw) {
			out.Skipped++
			continue
		}
		record, issues := BuildRecord(spec, raw)
		if len(issues) > 0 {
			for _, issue := range issues {
				issue.Row = rowNumber
				out.Issues = append(out.Issues, issue)
			}
			continue
		}
		out.Records = append(out.Records, record)
		out.RowNumbers = append(out.RowNumbers, rowNumber)
	}
	return out, nil
}

// stringify renders a parsed cell as the text the spec's coercions expect.
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
		return strconv.FormatFloat(val, 'f', -1, 64)
	case int:
		return strconv.Itoa(val)
	case int64:
		return strconv.FormatInt(val, 10)
	default:
		return fmt.Sprint(v)
	}
}
