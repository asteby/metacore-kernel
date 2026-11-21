package v3

import (
	"fmt"
	"strings"
)

// validateModelView checks Model.view: one SELECT, and the model shape a
// read-only view can honour.
func validateModelView(mi int, mod Model) []string {
	if mod.View == nil {
		return nil
	}
	var errs []string
	at := fmt.Sprintf("models[%d].view", mi)
	sql := strings.TrimSpace(mod.View.SQL)
	switch {
	case sql == "":
		errs = append(errs, at+".sql is required")
	case strings.Contains(sql, ";"):
		errs = append(errs, at+".sql must be a single statement without \";\"")
	case !strings.HasPrefix(strings.ToLower(sql), "select") && !strings.HasPrefix(strings.ToLower(sql), "with"):
		errs = append(errs, at+".sql must be a SELECT (or WITH ... SELECT)")
	case strings.Contains(sql, "--") || strings.Contains(sql, "/*"):
		errs = append(errs, at+".sql must not contain SQL comments")
	}
	has := map[string]bool{}
	for _, c := range mod.Columns {
		has[c.Name] = true
	}
	for _, need := range []string{"id", "organization_id"} {
		if !has[need] {
			errs = append(errs, fmt.Sprintf("%s: the model must declare the %q column the SELECT exposes", at, need))
		}
	}
	if mod.AppendOnly {
		errs = append(errs, at+" cannot be combined with append_only (a view is already read-only)")
	}
	if mod.StageField != "" || len(mod.Stages) > 0 || len(mod.Transitions) > 0 || len(mod.OnTransition) > 0 {
		errs = append(errs, at+" cannot be combined with a stage machine")
	}
	if mod.Extends != "" || len(mod.Extensions) > 0 {
		errs = append(errs, at+" cannot be combined with extends/extensions")
	}
	if mod.Seed != nil {
		errs = append(errs, at+" cannot be combined with seed")
	}
	return errs
}
