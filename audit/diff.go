package audit

import "encoding/json"

// changeSkipFields are excluded from the field-level diff: pure timestamp/
// soft-delete churn is not a meaningful business change.
var changeSkipFields = map[string]bool{
	"updated_at": true,
	"deleted_at": true,
}

// mapToJSONString marshals a map to a *string for a JSONB column. Returns nil
// when the map is nil or empty (the "snapshot unavailable" sentinel the kernel
// uses for deletes where the row was already gone).
func mapToJSONString(m map[string]any) *string {
	if len(m) == 0 {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	s := string(b)
	return &s
}

// computeChanges derives the per-field diff between before and after maps,
// producing a JSONB string of `{field: {before, after}}` for every field that
// actually changed. Returns nil when either side is absent (create/delete) or
// when the only differences are skipped timestamp fields.
//
// Ported from ops' computeChanges (services/activity_log.go), adapted to take
// map[string]any directly instead of pre-serialised JSON strings.
func computeChanges(before, after map[string]any) *string {
	if before == nil || after == nil {
		return nil
	}

	changes := make(map[string]map[string]any)
	for key, afterVal := range after {
		if changeSkipFields[key] {
			continue
		}
		beforeVal, exists := before[key]
		if !exists {
			changes[key] = map[string]any{"before": nil, "after": afterVal}
			continue
		}
		bJSON, _ := json.Marshal(beforeVal)
		aJSON, _ := json.Marshal(afterVal)
		if string(bJSON) != string(aJSON) {
			changes[key] = map[string]any{"before": beforeVal, "after": afterVal}
		}
	}

	if len(changes) == 0 {
		return nil
	}
	b, err := json.Marshal(changes)
	if err != nil {
		return nil
	}
	s := string(b)
	return &s
}
