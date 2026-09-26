package query

// ProjectMaps keeps only the named keys (plus "id") on each row.
// An empty field list returns items unchanged. The projection happens
// after the row is loaded: it shrinks the JSON payload and does not
// change the SQL.
func ProjectMaps(items []map[string]any, fields []string) []map[string]any {
	if len(fields) == 0 || len(items) == 0 {
		return items
	}
	keep := map[string]struct{}{"id": {}}
	for _, f := range fields {
		if f != "" {
			keep[f] = struct{}{}
		}
	}
	if len(keep) == 1 {
		return items
	}
	out := make([]map[string]any, len(items))
	for i, item := range items {
		row := make(map[string]any, len(keep))
		for k, v := range item {
			if _, ok := keep[k]; ok {
				row[k] = v
			}
		}
		out[i] = row
	}
	return out
}
