package agent

import (
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Compact tool results: rows as a Markdown table with UI labels, a single
// record as key-value lines, aggregates as a small table. Each rendering
// stays under a byte budget and ends with a summary and, when more rows
// exist, the offset of the next page. The model reads ~3-5x fewer tokens than
// the JSON of the same rows.

// DefaultCompactMaxBytes is the text budget of a compact result: under the
// copilot's per-result cap (DefaultCopilotMaxToolResultBytes), so the
// orchestrator never has to cut it blindly.
const DefaultCompactMaxBytes = DefaultCopilotMaxToolResultBytes - 400

// DefaultCompactRows is how many rows a table shows by default.
const DefaultCompactRows = 20

// TableOptions tune RenderRowsTable.
type TableOptions struct {
	// Columns are the columns to show, in order. Empty picks up to
	// MaxAutoColumns of the model's most informative fields present in rows.
	Columns []string
	// MaxRows caps the rows shown (default 20).
	MaxRows int
	// MaxBytes caps the rendered text (default DefaultCompactMaxBytes).
	MaxBytes int
	// MaxAutoColumns caps the automatically picked columns (default 8).
	MaxAutoColumns int
	// Total is the number of matching rows (<= 0: unknown, len(rows) assumed).
	Total int
	// Offset is the offset of rows[0] in the full result.
	Offset int
	// RefNames maps a many2one column to id -> display name.
	RefNames map[string]map[string]string
}

// RenderRowsTable renders rows of model m as a Markdown table.
func RenderRowsTable(m CatalogModel, rows []map[string]any, opt TableOptions) string {
	if opt.MaxRows <= 0 {
		opt.MaxRows = DefaultCompactRows
	}
	if opt.MaxBytes <= 0 {
		opt.MaxBytes = DefaultCompactMaxBytes
	}
	if opt.MaxAutoColumns <= 0 {
		opt.MaxAutoColumns = 8
	}
	total := opt.Total
	if total < opt.Offset+len(rows) {
		total = opt.Offset + len(rows)
	}
	label := modelDisplay(m)
	if len(rows) == 0 {
		if opt.Offset > 0 {
			return fmt.Sprintf("%s: sin más registros después de offset=%d (total %d).", label, opt.Offset, total)
		}
		return label + ": 0 registros con esos filtros."
	}
	cols := opt.Columns
	if len(cols) == 0 {
		cols = autoColumns(m, rows, opt.MaxAutoColumns)
	}
	if !containsString(cols, "id") {
		cols = append([]string{"id"}, cols...)
	}

	var b strings.Builder
	header := make([]string, len(cols))
	for i, c := range cols {
		header[i] = columnHeader(m, c)
	}
	b.WriteString("| " + strings.Join(header, " | ") + " |\n|")
	b.WriteString(strings.Repeat("---|", len(cols)))
	b.WriteByte('\n')
	reserve := 160 // summary line
	shown := 0
	for _, row := range rows {
		if shown >= opt.MaxRows {
			break
		}
		cells := make([]string, len(cols))
		for i, c := range cols {
			cells[i] = formatCell(m, c, row[c], opt.RefNames)
		}
		line := "| " + strings.Join(cells, " | ") + " |\n"
		if b.Len()+len(line)+reserve > opt.MaxBytes && shown > 0 {
			break
		}
		b.WriteString(line)
		shown++
	}
	from, to := opt.Offset+1, opt.Offset+shown
	switch {
	case to < total:
		fmt.Fprintf(&b, "%s: %d–%d de %d. next: offset=%d", label, from, to, total, to)
	case opt.Offset > 0:
		fmt.Fprintf(&b, "%s: %d–%d de %d (fin).", label, from, to, total)
	default:
		fmt.Fprintf(&b, "%s: %d registro%s.", label, total, plural(total))
	}
	return b.String()
}

// RenderRecordKV renders one record as "- Etiqueta: valor" lines, every
// non-empty field of the card first, then any other column.
func RenderRecordKV(m CatalogModel, row map[string]any, refNames map[string]map[string]string, maxBytes int) string {
	if maxBytes <= 0 {
		maxBytes = DefaultCompactMaxBytes
	}
	var b strings.Builder
	b.WriteString(modelDisplay(m))
	if id := scalarText(row["id"]); id != "" {
		b.WriteString(" [" + id + "]")
	}
	b.WriteByte('\n')
	seen := map[string]bool{"id": true}
	write := func(col string) {
		seen[col] = true
		v, ok := row[col]
		if !ok || isEmptyValue(v) {
			return
		}
		line := "- " + columnHeader(m, col) + ": " + formatCell(m, col, v, refNames) + "\n"
		if b.Len()+len(line) > maxBytes {
			return
		}
		b.WriteString(line)
	}
	for _, f := range m.Fields {
		write(f.Name)
	}
	rest := make([]string, 0, len(row))
	for k := range row {
		if !seen[k] && (!cardSkipColumns[k] || k == "created_at" || k == "updated_at") {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		write(k)
	}
	return strings.TrimRight(b.String(), "\n")
}

// RenderAggregateTable renders grouped aggregate rows.
func RenderAggregateTable(m CatalogModel, t QueryTree, rows []map[string]any, refNames map[string]map[string]string, maxBytes int) string {
	if maxBytes <= 0 {
		maxBytes = DefaultCompactMaxBytes
	}
	cols := append(append([]string(nil), t.GroupBy...), aggregateAliases(t)...)
	label := modelDisplay(m)
	if len(t.GroupBy) == 0 && len(rows) <= 1 {
		var parts []string
		for _, a := range t.Aggregates {
			v := any(nil)
			if len(rows) == 1 {
				v = rows[0][a.As]
			}
			parts = append(parts, a.As+" = "+formatNumberish(v))
		}
		return label + ": " + strings.Join(parts, ", ")
	}
	var b strings.Builder
	header := make([]string, len(cols))
	for i, c := range cols {
		header[i] = columnHeader(m, c)
	}
	b.WriteString("| " + strings.Join(header, " | ") + " |\n|" + strings.Repeat("---|", len(cols)) + "\n")
	shown := 0
	for _, row := range rows {
		cells := make([]string, len(cols))
		for i, c := range cols {
			if i < len(t.GroupBy) {
				cells[i] = formatCell(m, c, row[c], refNames)
			} else {
				cells[i] = formatNumberish(row[c])
			}
		}
		line := "| " + strings.Join(cells, " | ") + " |\n"
		if b.Len()+len(line)+120 > maxBytes && shown > 0 {
			break
		}
		b.WriteString(line)
		shown++
	}
	if shown < len(rows) {
		fmt.Fprintf(&b, "%s: %d de %d grupos (pide menos grupos o filtra).", label, shown, len(rows))
	} else {
		fmt.Fprintf(&b, "%s: %d grupo%s.", label, len(rows), plural(len(rows)))
	}
	return b.String()
}

func aggregateAliases(t QueryTree) []string {
	out := make([]string, len(t.Aggregates))
	for i, a := range t.Aggregates {
		out[i] = a.As
	}
	return out
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func modelDisplay(m CatalogModel) string {
	if m.Label != "" {
		return m.Label + " (" + m.Name + ")"
	}
	return m.Name
}

func columnHeader(m CatalogModel, col string) string {
	if f, ok := m.field(col); ok && f.Label != "" {
		return escapeCell(f.Label)
	}
	return col
}

// autoColumns picks the most informative fields that have a value in rows,
// in card order.
func autoColumns(m CatalogModel, rows []map[string]any, n int) []string {
	present := func(name string) bool {
		for _, r := range rows {
			if v, ok := r[name]; ok && !isEmptyValue(v) {
				return true
			}
		}
		return false
	}
	search := map[string]bool{}
	for _, s := range m.SearchIn {
		search[s] = true
	}
	type cand struct {
		idx, prio int
		name      string
	}
	var cs []cand
	for i, f := range m.Fields {
		if f.Rel == "o2m" || f.Rel == "m2m" || f.Type == "json" || f.Type == "img" || f.Type == "text" || !present(f.Name) {
			continue
		}
		cs = append(cs, cand{i, fieldPriority(f, search), f.Name})
	}
	if len(cs) == 0 {
		// No card metadata: the row's own columns, sorted.
		var keys []string
		for k := range rows[0] {
			if k != "id" && !cardSkipColumns[k] {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		if len(keys) > n {
			keys = keys[:n]
		}
		return keys
	}
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].prio > cs[j].prio })
	if len(cs) > n {
		cs = cs[:n]
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].idx < cs[j].idx })
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.name
	}
	return out
}

// formatCell renders one value: many2one as "Name [id]", enum as its label,
// bool as sí/no, dates trimmed, text clipped and escaped.
func formatCell(m CatalogModel, col string, v any, refNames map[string]map[string]string) string {
	v = derefValue(v)
	if isEmptyValue(v) {
		return ""
	}
	f, hasField := m.field(col)
	if hasField && f.Rel == "m2o" {
		id := scalarText(v)
		if name := refNames[col][id]; name != "" {
			return escapeCell(ClipRunes(name, 50)) + " [" + id + "]"
		}
		return id
	}
	if hasField && len(f.Enum) > 0 {
		s := scalarText(v)
		for _, o := range f.Enum {
			if o.Value == s && o.Label != "" {
				return escapeCell(o.Label)
			}
		}
		return escapeCell(s)
	}
	switch t := v.(type) {
	case bool:
		if t {
			return "sí"
		}
		return "no"
	case time.Time:
		if t.Hour() == 0 && t.Minute() == 0 && t.Second() == 0 {
			return t.Format("2006-01-02")
		}
		return t.Format("2006-01-02 15:04")
	case float64, float32, int, int64, int32, uint, uint64:
		return formatNumberish(t)
	case []byte:
		return escapeCell(ClipRunes(string(t), 60))
	case map[string]any, []any:
		return "…"
	}
	s := scalarText(v)
	if hasField && (f.Type == "date" || f.Type == "dt") {
		return trimTimestamp(s)
	}
	if ts := trimTimestamp(s); ts != s {
		return ts
	}
	return escapeCell(ClipRunes(s, 60))
}

// trimTimestamp shortens RFC3339 timestamps ("2026-11-15T00:00:00Z" ->
// "2026-11-15", "2026-11-15T10:42:31.5-06:00" -> "2026-11-15 10:42").
func trimTimestamp(s string) string {
	if len(s) < 19 || s[4] != '-' || s[7] != '-' || (s[10] != 'T' && s[10] != ' ') || s[13] != ':' {
		return s
	}
	if s[11:19] == "00:00:00" {
		return s[:10]
	}
	return s[:10] + " " + s[11:16]
}

func formatNumberish(v any) string {
	v = derefValue(v)
	switch t := v.(type) {
	case nil:
		return "0"
	case float64:
		if t == math.Trunc(t) && math.Abs(t) < 1e15 {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', 2, 64)
	case float32:
		return formatNumberish(float64(t))
	case []byte:
		return formatNumberish(string(t))
	case string:
		if n, err := strconv.ParseFloat(t, 64); err == nil {
			return formatNumberish(n)
		}
		return escapeCell(t)
	}
	return escapeCell(fmt.Sprint(v))
}

func scalarText(v any) string {
	v = derefValue(v)
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []byte:
		return string(t)
	case fmt.Stringer:
		return t.String()
	}
	return fmt.Sprint(v)
}

func isEmptyValue(v any) bool {
	v = derefValue(v)
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(t) == ""
	case []byte:
		return len(t) == 0
	}
	return false
}

func escapeCell(s string) string {
	s = strings.ReplaceAll(s, "|", "/")
	s = strings.ReplaceAll(s, "\r", " ")
	return strings.ReplaceAll(s, "\n", " ")
}

// clipBytes cuts s to at most n bytes on a rune boundary.
func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

// derefValue unwraps pointers some drivers return for computed columns
// (aggregates scanned into a map come back as *int64, *float64…).
func derefValue(v any) any {
	for v != nil {
		rv := reflect.ValueOf(v)
		if rv.Kind() != reflect.Pointer {
			return v
		}
		if rv.IsNil() {
			return nil
		}
		v = rv.Elem().Interface()
	}
	return v
}

// derefRows unwraps pointer values of rows in place.
func derefRows(rows []map[string]any) {
	for _, r := range rows {
		for k, v := range r {
			r[k] = derefValue(v)
		}
	}
}
