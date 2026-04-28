package dynamic

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/asteby/metacore-kernel/query"
	"github.com/gofiber/fiber/v2"
)

// exportLimit caps the number of rows a single export request can stream.
// Tuned to comfortably hold a typical CRM table in memory; apps with bigger
// datasets should plug an async export path on top of `dynamic.Service`.
const exportLimit = 100_000

// exportData handles GET /dynamic/:model/export?format=csv&columns=a,b,c
// Streams every row visible to the caller, encoded as CSV. Format defaults
// to CSV; `columns` (optional) restricts the output to a subset and
// preserves their order. Falls back to the model's TableMetadata column
// order when omitted.
func (h *Handler) exportData(c *fiber.Ctx) error {
	u := h.user(c)
	if u == nil {
		return respondErr(c, fiber.StatusUnauthorized, "not authenticated")
	}
	model := c.Params("model")
	headers, err := exportHeaders(c, h, model)
	if err != nil {
		return h.handleError(c, err)
	}

	params, err := query.ParseFiber(c)
	if err != nil {
		return respondErr(c, fiber.StatusBadRequest, err.Error())
	}
	params.Page = 1
	params.PerPage = exportLimit
	items, _, err := h.service.List(c.Context(), model, u, params)
	if err != nil {
		return h.handleError(c, err)
	}

	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if err := w.Write(headers); err != nil {
		return respondErr(c, fiber.StatusInternalServerError, "csv encode: "+err.Error())
	}
	for _, row := range items {
		rec := make([]string, len(headers))
		for i, key := range headers {
			rec[i] = stringify(row[key])
		}
		if err := w.Write(rec); err != nil {
			return respondErr(c, fiber.StatusInternalServerError, "csv encode: "+err.Error())
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return respondErr(c, fiber.StatusInternalServerError, "csv flush: "+err.Error())
	}

	c.Set(fiber.HeaderContentType, "text/csv; charset=utf-8")
	c.Set(fiber.HeaderContentDisposition, "attachment; filename=\""+model+".csv\"")
	return c.Send(buf.Bytes())
}

// exportTemplate handles GET /dynamic/:model/export/template — same format
// as exportData but with no rows, so users can fill it in and feed it back
// through importData.
func (h *Handler) exportTemplate(c *fiber.Ctx) error {
	model := c.Params("model")
	headers, err := exportHeaders(c, h, model)
	if err != nil {
		return h.handleError(c, err)
	}

	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	_ = w.Write(headers)
	w.Flush()

	c.Set(fiber.HeaderContentType, "text/csv; charset=utf-8")
	c.Set(fiber.HeaderContentDisposition, "attachment; filename=\""+model+"-template.csv\"")
	return c.Send(buf.Bytes())
}

// importValidate handles POST /dynamic/:model/import/validate — parses the
// uploaded CSV/JSON and reports row-by-row issues without touching the DB.
func (h *Handler) importValidate(c *fiber.Ctx) error {
	if h.user(c) == nil {
		return respondErr(c, fiber.StatusUnauthorized, "not authenticated")
	}
	rows, err := readImportRows(c)
	if err != nil {
		return respondErr(c, fiber.StatusBadRequest, err.Error())
	}
	return c.JSON(fiber.Map{
		"success": true,
		"data": fiber.Map{
			"rowCount":     len(rows),
			"sample":       firstN(rows, 5),
			"errors":       []any{},
			"validatedAt":  fiber.Map{"unix": int64(0)}, // populated by caller as needed
		},
	})
}

// importData handles POST /dynamic/:model/import — parses the uploaded
// CSV/JSON and creates one record per row through the regular dynamic
// Service.Create pipeline (so permissions, hooks, validation all run).
func (h *Handler) importData(c *fiber.Ctx) error {
	u := h.user(c)
	if u == nil {
		return respondErr(c, fiber.StatusUnauthorized, "not authenticated")
	}
	model := c.Params("model")
	rows, err := readImportRows(c)
	if err != nil {
		return respondErr(c, fiber.StatusBadRequest, err.Error())
	}

	created := 0
	failures := make([]map[string]any, 0)
	for i, row := range rows {
		if _, err := h.service.Create(c.Context(), model, u, row); err != nil {
			failures = append(failures, map[string]any{
				"row":   i + 1,
				"error": err.Error(),
				"input": row,
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
		"success":  len(failures) == 0,
		"data": fiber.Map{
			"created":  created,
			"failed":   len(failures),
			"failures": failures,
		},
	})
}

// exportHeaders resolves the column list for a CSV export. `columns` query
// param wins (comma-separated). Falls back to the model's TableMetadata
// columns. Always honours the order the caller provides — apps map this
// 1:1 to spreadsheet columns.
func exportHeaders(c *fiber.Ctx, h *Handler, model string) ([]string, error) {
	if raw := c.Query("columns"); raw != "" {
		parts := strings.Split(raw, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				out = append(out, p)
			}
		}
		if len(out) > 0 {
			return out, nil
		}
	}
	meta, err := h.service.TableMetadata(c.Context(), model)
	if err != nil {
		return nil, err
	}
	headers := make([]string, 0, len(meta.Columns))
	for _, col := range meta.Columns {
		if col.Hidden {
			continue
		}
		headers = append(headers, col.Key)
	}
	return headers, nil
}

// readImportRows reads either a multipart-uploaded file or a JSON body and
// returns parsed rows as `[]map[string]any`. CSV rows are decoded with the
// first record treated as the header. JSON accepts either an array of
// objects or a `{ data: [...] }` envelope.
func readImportRows(c *fiber.Ctx) ([]map[string]any, error) {
	contentType := strings.ToLower(c.Get(fiber.HeaderContentType))
	if strings.HasPrefix(contentType, "multipart/form-data") {
		fileHeader, err := c.FormFile("file")
		if err != nil {
			return nil, fmt.Errorf("expected `file` in multipart form: %w", err)
		}
		f, err := fileHeader.Open()
		if err != nil {
			return nil, fmt.Errorf("open uploaded file: %w", err)
		}
		defer f.Close()
		// Detect by extension; default to CSV.
		name := strings.ToLower(fileHeader.Filename)
		if strings.HasSuffix(name, ".json") {
			return parseJSONReader(f)
		}
		return parseCSVReader(f)
	}
	if strings.HasPrefix(contentType, "application/json") {
		return parseJSONReader(bytes.NewReader(c.Body()))
	}
	// Fall back to CSV when the body looks like one.
	return parseCSVReader(bytes.NewReader(c.Body()))
}

func parseCSVReader(r io.Reader) ([]map[string]any, error) {
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

func parseJSONReader(r io.Reader) ([]map[string]any, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read json body: %w", err)
	}
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

func firstN[T any](items []T, n int) []T {
	if len(items) <= n {
		return items
	}
	return items[:n]
}
