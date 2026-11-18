package v3

import (
	"errors"
	"fmt"
	"strings"

	genuiv2 "github.com/asteby/metacore-kernel/genui/v2"
)

// validatePages checks contributions.pages[] and the NavItem.Page references.
//
// The body is delegated to genui/v2 (genui/v2.Parse), which owns the closed
// block catalog (grid, tabs, stats, chart, model_view, view, section, ext...),
// MaxDepth, MaxBlocks, MaxBytes and the reference grammar; an unknown block is
// rejected there with code genui.unknown_block. Nothing of that schema is
// duplicated here.
//
// genui/v2 already accepts the page blocks the SDK implements (extended tabs,
// section with mode/component, grid.areas/mobile, kpi_card, map.select).
//
// TODO(manifest-pages, fase 2): the blocks proposed but not implemented yet
// (ranking, range, filter_bar, badge, params with $ref to state) are still
// rejected until the schema gains them; and cross-checks between the body and
// the manifest (widget/model/action/panel references, component against
// contributions.blocks[], blocks[].permission) need the schema first.
func validatePages(m *Manifest) []string {
	if m.Contributions == nil {
		return nil
	}
	var errs []string
	known := make(map[string]struct{}, len(m.Contributions.Pages))
	pages := m.Contributions.Pages
	if len(pages) > MaxContributionPages {
		errs = append(errs, fmt.Sprintf("contributions.pages has %d entries; the maximum is %d", len(pages), MaxContributionPages))
	}
	for i, p := range pages {
		id := p.Key
		if id == "" {
			id = fmt.Sprintf("#%d", i)
		}
		where := fmt.Sprintf("contributions.pages[%s]", id)

		switch {
		case p.Key == "":
			errs = append(errs, where+".key is required")
		case !pageKeyRe.MatchString(p.Key):
			errs = append(errs, fmt.Sprintf("%s.key %q must be snake_case (^[a-z][a-z0-9_]*$)", where, p.Key))
		default:
			if _, dup := known[p.Key]; dup {
				errs = append(errs, fmt.Sprintf("%s.key %q is duplicated within the addon", where, p.Key))
			}
			known[p.Key] = struct{}{}
		}
		if p.Title == "" {
			errs = append(errs, where+".title is required (an i18n key)")
		}
		if p.Permission != "" && !PermissionKeyRe.MatchString(p.Permission) {
			errs = append(errs, fmt.Sprintf("%s.permission %q is not a valid permission key (want e.g. \"<addon>.<resource>.<action>\")", where, p.Permission))
		}
		errs = append(errs, validatePageBody(where, p.UI)...)
	}

	var walk func(items []NavItem, path string)
	walk = func(items []NavItem, path string) {
		for i, it := range items {
			w := fmt.Sprintf("%s[%d]", path, i)
			if it.Page != "" {
				if _, ok := known[it.Page]; !ok {
					errs = append(errs, fmt.Sprintf("%s.page %q does not match any contributions.pages[].key", w, it.Page))
				}
				var clash []string
				if it.URL != "" {
					clash = append(clash, "url")
				}
				if it.Model != "" {
					clash = append(clash, "model")
				}
				if it.ViewType != "" {
					clash = append(clash, "view_type")
				}
				if it.View != nil {
					clash = append(clash, "view")
				}
				if it.GroupBy != "" {
					clash = append(clash, "group_by")
				}
				if len(clash) > 0 {
					errs = append(errs, fmt.Sprintf("%s.page cannot be combined with %s (a page entry has its own identity)", w, strings.Join(clash, ", ")))
				}
			}
			if len(it.Items) > 0 {
				walk(it.Items, w+".items")
			}
		}
	}
	for gi, g := range m.Contributions.Navigation {
		walk(g.Items, fmt.Sprintf("contributions.navigation[%d].items", gi))
	}
	return errs
}

// validatePageBody validates one page body with genui/v2 and requires
// i18n:"keys" (page text is always an i18n key of the addon).
func validatePageBody(where string, ui []byte) []string {
	if len(ui) == 0 || string(ui) == "null" {
		return []string{where + ".ui is required (a genui/v2 document)"}
	}
	if len(ui) > MaxPageUIBytes {
		return []string{fmt.Sprintf("%s.ui is %d bytes; the maximum is %d", where, len(ui), MaxPageUIBytes)}
	}
	doc, err := genuiv2.Parse(ui, genuiv2.Options{})
	if err != nil {
		var ge *genuiv2.Error
		if errors.As(err, &ge) {
			loc := ge.Path
			if loc == "" {
				loc = "$"
			}
			return []string{fmt.Sprintf("%s.ui is not a valid genui/v2 document: %s at %s: %s", where, ge.Code, loc, ge.Msg)}
		}
		return []string{fmt.Sprintf("%s.ui is not a valid genui/v2 document: %v", where, err)}
	}
	if doc["i18n"] != "keys" {
		return []string{where + `.ui.i18n must be "keys" (page text is an i18n key, never a literal)`}
	}
	return nil
}
