package dynamic

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/asteby/metacore-kernel/importer"
	"github.com/asteby/metacore-kernel/query"
	"github.com/gofiber/fiber/v3"
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
func (h *Handler) exportData(c fiber.Ctx) error {
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
	items, _, err := h.service.List(c, model, u, params)
	if err != nil {
		return h.handleError(c, err)
	}

	records := make([][]string, 0, len(items)+1)
	records = append(records, headers)
	for _, row := range items {
		rec := make([]string, len(headers))
		for i, key := range headers {
			rec[i] = stringify(row[key])
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
	prepared, err := h.prepareImport(c)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{
		"success": len(prepared.Issues) == 0,
		"data": fiber.Map{
			"rowCount": len(prepared.Records),
			"skipped":  prepared.Skipped,
			"sample":   firstN(prepared.Records, 5),
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
	prepared, err := h.prepareImport(c)
	if err != nil {
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
func (h *Handler) prepareImport(c fiber.Ctx) (*importer.Prepared, error) {
	model := c.Params("model")
	spec, err := h.service.ImportSpec(c, model)
	if err != nil {
		return nil, h.handleError(c, err)
	}
	if len(spec.Columns) == 0 {
		return nil, respondErr(c, fiber.StatusUnprocessableEntity, "model has no importable columns")
	}
	rows, err := importer.ReadRows(c)
	if err != nil {
		return nil, respondErr(c, fiber.StatusBadRequest, err.Error())
	}
	if len(rows) == 0 {
		return nil, respondErr(c, fiber.StatusBadRequest, "the file contains no data rows")
	}
	prepared, err := importer.Prepare(spec, rows)
	if err != nil {
		return nil, respondErr(c, fiber.StatusUnprocessableEntity, err.Error())
	}
	return prepared, nil
}

// exportHeaders resolves the column list for a CSV export. `columns` query
// param wins (comma-separated). Falls back to the model's TableMetadata
// columns. Always honours the order the caller provides — apps map this
// 1:1 to spreadsheet columns.
func exportHeaders(c fiber.Ctx, h *Handler, model string) ([]string, error) {
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
	meta, err := h.service.TableMetadata(c, model)
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
