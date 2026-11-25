package v3

import (
	"fmt"
	"regexp"
	"strings"
)

// Navigation sections and anchors (RFC SDK-0001 "owner + plugs", part B).
//
// The host owns a CLOSED list of sidebar sections; no manifest creates,
// renames or reorders them. A domain owner publishes nav anchors
// (extension_points.nav_anchors) inside a section and other addons plug items
// into them (items[].attach_to + as) instead of growing their own menu. The
// pure resolver that turns installed manifests into the sidebar tree lives in
// the navigation package; this file holds the vocabulary and the per-manifest
// validation shared by every host.

// Sidebar sections, in sidebar order.
const (
	NavSectionHome          = "home"
	NavSectionSales         = "sales"
	NavSectionCustomers     = "customers"
	NavSectionCatalog       = "catalog"
	NavSectionOperations    = "operations"
	NavSectionFinance       = "finance"
	NavSectionCommunication = "communication"
	NavSectionInsights      = "insights"
	// NavSectionSettings lives outside the main sidebar (settings menu).
	NavSectionSettings = "settings"
)

// NavSections is the closed, ordered list of sidebar sections (without
// settings). Opening a new section takes an RFC.
var NavSections = []string{
	NavSectionHome, NavSectionSales, NavSectionCustomers, NavSectionCatalog,
	NavSectionOperations, NavSectionFinance, NavSectionCommunication, NavSectionInsights,
}

// Plug-in kinds (items[].as).
const (
	NavAsTab     = "tab"
	NavAsView    = "view"
	NavAsAction  = "action"
	NavAsPanel   = "panel"
	NavAsFilter  = "filter"
	NavAsMode    = "mode"
	NavAsSetting = "setting"
)

// NavAsValues lists every items[].as value.
var NavAsValues = []string{NavAsTab, NavAsView, NavAsAction, NavAsPanel, NavAsFilter, NavAsMode, NavAsSetting}

// Fallbacks of an attached item whose anchor is absent.
const (
	NavFallbackSection = "section"
	NavFallbackHide    = "hide"
)

// DefaultNavOrder is the order of a group, anchor or item that sets none.
const DefaultNavOrder = 1000

// RuleNavTargetUnknown: a group's legacy target maps to no section; it is
// placed in "operations". Warning-only (RFC SDK-0001 NAV-06).
const RuleNavTargetUnknown = "nav_target_unknown"

// ValidNavSection reports whether s is a sidebar section or "settings".
func ValidNavSection(s string) bool {
	return s == NavSectionSettings || NavSectionIndex(s) >= 0
}

// NavSectionIndex is the sidebar position of a section (settings sorts last,
// after the sidebar), or -1 when s is not a section.
func NavSectionIndex(s string) int {
	for i, v := range NavSections {
		if v == s {
			return i
		}
	}
	if s == NavSectionSettings {
		return len(NavSections)
	}
	return -1
}

// ValidNavAs reports whether as is an items[].as value.
func ValidNavAs(as string) bool {
	for _, v := range NavAsValues {
		if v == as {
			return true
		}
	}
	return false
}

// NavFallbackAllowed reports whether fallback "section" makes sense for as:
// tab, view, filter and mode can live as an entry of their own; action, panel
// and setting mean nothing without the anchor's resource.
func NavFallbackAllowed(as string) bool {
	switch as {
	case NavAsTab, NavAsView, NavAsFilter, NavAsMode:
		return true
	}
	return false
}

// DefaultNavFallback is the fallback of an attached item that declares none
// (RFC SDK-0001 §5.3, decision A4).
func DefaultNavFallback(as string) string {
	if NavFallbackAllowed(as) {
		return NavFallbackSection
	}
	return NavFallbackHide
}

// EffectiveNavFallback is fallback, or the default for as when empty.
func EffectiveNavFallback(as, fallback string) string {
	if fallback != "" {
		return fallback
	}
	return DefaultNavFallback(as)
}

// EffectiveNavOrder dereferences an optional order (nil = DefaultNavOrder).
func EffectiveNavOrder(o *int) int {
	if o == nil {
		return DefaultNavOrder
	}
	return *o
}

// NavAnchorSection is the section prefix of an anchor key ("sales" for
// "sales.order", "home" for "home").
func NavAnchorSection(key string) string {
	if i := strings.Index(key, "."); i >= 0 {
		return key[:i]
	}
	return key
}

// NavSectionForTarget maps a legacy NavGroup.target onto a section (RFC
// SDK-0001 §7): "sidebar.<x>" (or a bare "<x>") is x when x is a section,
// "sidebar.sales_orders" is sales, empty is operations (known), anything
// else is operations with known=false (warning nav_target_unknown).
func NavSectionForTarget(target string) (section string, known bool) {
	t := strings.ToLower(strings.TrimSpace(target))
	if t == "" {
		return NavSectionOperations, true
	}
	t = strings.TrimPrefix(t, "sidebar.")
	if t == "sales_orders" {
		return NavSectionSales, true
	}
	if ValidNavSection(t) {
		return t, true
	}
	return NavSectionOperations, false
}

// EffectiveNavSection is the section a group lands in: Section when set,
// else the mapping of Target.
func EffectiveNavSection(section, target string) string {
	if section != "" {
		return section
	}
	s, _ := NavSectionForTarget(target)
	return s
}

var (
	navAnchorKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)?$`)
	navItemKeyRe   = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
)

// ValidNavAnchorKey reports whether key has the anchor shape and a known
// section prefix.
func ValidNavAnchorKey(key string) bool {
	return navAnchorKeyRe.MatchString(key) && ValidNavSection(NavAnchorSection(key))
}

// validateNavAnchors checks extension_points.nav_anchors and the new
// navigation fields (group key/section/order, item key/attach_to/as/fallback)
// of one manifest. A manifest that uses none of them gets no error; the only
// finding a legacy manifest can get is the nav_target_unknown warning.
func validateNavAnchors(m *Manifest, warn func(msg string)) []string {
	var errs []string
	models := map[string]bool{}
	for _, md := range m.Models {
		models[md.Key] = true
	}
	pages := map[string]bool{}
	if m.Contributions != nil {
		for _, p := range m.Contributions.Pages {
			pages[p.Key] = true
		}
	}
	anchors := map[string]NavAnchor{}
	if m.ExtensionPoints != nil {
		for i, a := range m.ExtensionPoints.NavAnchors {
			w := fmt.Sprintf("extension_points.nav_anchors[%d] (%s)", i, a.Key)
			if !navAnchorKeyRe.MatchString(a.Key) {
				errs = append(errs, w+`.key must be "<section>.<name>" in lower snake case`)
			}
			if !ValidNavSection(a.Section) {
				errs = append(errs, fmt.Sprintf("%s.section %q is not one of %s|%s", w, a.Section, strings.Join(NavSections, "|"), NavSectionSettings))
			} else if NavAnchorSection(a.Key) != a.Section {
				errs = append(errs, fmt.Sprintf("%s.key must start with its section %q", w, a.Section))
			}
			if _, dup := anchors[a.Key]; dup {
				errs = append(errs, w+": duplicate anchor key")
			}
			anchors[a.Key] = a
			n := 0
			for _, v := range []string{a.Model, a.Page, a.URL} {
				if v != "" {
					n++
				}
			}
			if n != 1 {
				errs = append(errs, w+": exactly one of model, page or url is required (the anchor's main resource)")
			}
			if a.Model != "" && !models[a.Model] {
				errs = append(errs, fmt.Sprintf("%s.model %q is not a model of this addon (an anchor is published by the owner of its model)", w, a.Model))
			}
			if a.Page != "" && !pages[a.Page] {
				errs = append(errs, fmt.Sprintf("%s.page %q does not match any contributions.pages[].key", w, a.Page))
			}
			if len(a.Accepts) == 0 {
				errs = append(errs, w+".accepts must list at least one of "+strings.Join(NavAsValues, "|"))
			}
			seen := map[string]bool{}
			for _, as := range a.Accepts {
				if !ValidNavAs(as) {
					errs = append(errs, fmt.Sprintf("%s.accepts: %q is not one of %s", w, as, strings.Join(NavAsValues, "|")))
				}
				if seen[as] {
					errs = append(errs, fmt.Sprintf("%s.accepts: %q is listed twice", w, as))
				}
				seen[as] = true
			}
		}
	}
	if m.Contributions == nil {
		return errs
	}
	groupKeys := map[string]bool{}
	itemKeys := map[string]bool{}
	for gi, g := range m.Contributions.Navigation {
		gw := fmt.Sprintf("contributions.navigation[%d]", gi)
		if g.Key != "" {
			if !strings.HasPrefix(g.Key, m.Metadata.Key+".") || !navItemKeyRe.MatchString(strings.TrimPrefix(g.Key, m.Metadata.Key+".")) {
				errs = append(errs, fmt.Sprintf(`%s.key %q must be "%s.<name>" in lower snake case`, gw, g.Key, m.Metadata.Key))
			}
			if groupKeys[g.Key] {
				errs = append(errs, fmt.Sprintf("%s.key %q is used by another group", gw, g.Key))
			}
			groupKeys[g.Key] = true
		}
		if g.Section != "" && !ValidNavSection(g.Section) {
			errs = append(errs, fmt.Sprintf("%s.section %q is not one of %s|%s", gw, g.Section, strings.Join(NavSections, "|"), NavSectionSettings))
		}
		if g.Target != "" {
			mapped, known := NavSectionForTarget(g.Target)
			switch {
			case g.Section != "" && (!known || mapped != g.Section):
				errs = append(errs, fmt.Sprintf("%s: target %q and section %q disagree (target maps to %q); drop target", gw, g.Target, g.Section, mapped))
			case g.Section == "" && !known:
				warn(fmt.Sprintf("[%s] %s.target %q maps to no section; it is placed in %q (declare section)", RuleNavTargetUnknown, gw, g.Target, NavSectionOperations))
			}
		}
		var walk func(items []NavItem, path string, top bool)
		walk = func(items []NavItem, path string, top bool) {
			for ii, it := range items {
				w := fmt.Sprintf("%s[%d]", path, ii)
				if it.Key != "" {
					if !navItemKeyRe.MatchString(it.Key) {
						errs = append(errs, fmt.Sprintf("%s.key %q must be lower snake case", w, it.Key))
					}
					if itemKeys[it.Key] {
						errs = append(errs, fmt.Sprintf("%s.key %q is used by another item of this addon", w, it.Key))
					}
					itemKeys[it.Key] = true
				}
				errs = append(errs, validateNavAttach(w, it, top, anchors)...)
				walk(it.Items, w+".items", false)
			}
		}
		walk(g.Items, gw+".items", true)
	}
	return errs
}

func validateNavAttach(w string, it NavItem, top bool, own map[string]NavAnchor) []string {
	var errs []string
	if it.AttachTo == "" {
		if it.As != "" {
			errs = append(errs, w+".as is only valid with attach_to")
		}
		if it.Fallback != "" {
			errs = append(errs, w+".fallback is only valid with attach_to")
		}
		return errs
	}
	if !top {
		errs = append(errs, w+".attach_to is only valid on a group's top-level items")
	}
	if !ValidNavAnchorKey(it.AttachTo) {
		errs = append(errs, fmt.Sprintf(`%s.attach_to %q must be an anchor key "<section>.<name>" with section in %s|%s`, w, it.AttachTo, strings.Join(NavSections, "|"), NavSectionSettings))
	}
	if it.As == "" {
		errs = append(errs, w+".as is required with attach_to ("+strings.Join(NavAsValues, "|")+")")
	} else if !ValidNavAs(it.As) {
		errs = append(errs, fmt.Sprintf("%s.as %q is not one of %s", w, it.As, strings.Join(NavAsValues, "|")))
	}
	switch it.Fallback {
	case "", NavFallbackHide:
	case NavFallbackSection:
		if it.As != "" && !NavFallbackAllowed(it.As) {
			errs = append(errs, fmt.Sprintf(`%s.fallback "section" is not allowed with as %q (only tab|view|filter|mode); use "hide"`, w, it.As))
		}
	default:
		errs = append(errs, fmt.Sprintf("%s.fallback %q is not one of section|hide", w, it.Fallback))
	}
	switch it.As {
	case NavAsAction:
		if len(it.Actions) == 0 {
			errs = append(errs, w+`: as "action" needs actions[] (keys of contributions.actions)`)
		}
	case NavAsMode:
		if it.Page == "" {
			errs = append(errs, w+`: as "mode" needs page (a full-screen page)`)
		}
	}
	// An anchor of this same manifest is known here; anchors of other addons
	// are checked at install time (navigation.CheckInstalled).
	if a, ok := own[it.AttachTo]; ok && it.As != "" && !acceptsAs(a, it.As) {
		errs = append(errs, fmt.Sprintf("%s: anchor %q does not accept as %q (accepts %s)", w, it.AttachTo, it.As, strings.Join(a.Accepts, "|")))
	}
	return errs
}

// AcceptsAs reports whether the anchor accepts plug-ins of kind as.
func (a NavAnchor) AcceptsAs(as string) bool { return acceptsAs(a, as) }

func acceptsAs(a NavAnchor, as string) bool {
	for _, v := range a.Accepts {
		if v == as {
			return true
		}
	}
	return false
}
