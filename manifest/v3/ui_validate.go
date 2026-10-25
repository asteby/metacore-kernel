package v3

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/asteby/metacore-kernel/capability"
)

var (
	pageKeyRe    = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	pagePathRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*(/[a-z0-9][a-z0-9-]*)*$`)
	pageExposeRe = regexp.MustCompile(`^\./[A-Za-z0-9_-]+(/[A-Za-z0-9_-]+)*$`)
	panelKeyRe   = pageKeyRe

	mapFits        = map[string]struct{}{"": {}, "data": {}, "org_default": {}}
	mapLayerKinds  = map[string]struct{}{"points": {}, "zones": {}}
	chartKinds     = map[string]struct{}{"bar": {}, "line": {}, "area": {}, "pie": {}, "donut": {}}
	panelSpans     = map[string]struct{}{"": {}, "full": {}, "half": {}}
	statFormats    = map[string]struct{}{"": {}, "number": {}, "currency": {}, "percent": {}, "compact": {}, "date": {}, "datetime": {}, "relative_date": {}}
	declarativeSet = map[string]struct{}{ViewTypeMap: {}, ViewTypeTimeline: {}, ViewTypeCalendar: {}, ViewTypeChart: {}, ViewTypeSplit: {}}
)

// Limits of the declarative UI grammar. Kept small on purpose: a view that
// needs more is a federated page.
const (
	maxMapPopup    = 8
	maxMapLayers   = 8
	maxPanels      = 12
	maxStatItems   = 8
	maxRelationRow = 50
)

// IsDeclarativeView reports whether viewType is one of DeclarativeViewTypes.
func IsDeclarativeView(viewType string) bool {
	_, ok := declarativeSet[viewType]
	return ok
}

// modelShape is what UI validation knows about one model of the addon.
type modelShape struct {
	cols map[string]struct{}
	rels map[string]string // relation name → child model key
}

// uiChecker accumulates UI-grammar errors. lookup resolves the addon's own
// models; nil (the legacy carrier, cross-addon models) skips column and
// relation existence checks but keeps every structural rule.
type uiChecker struct {
	lookup    func(model string) (modelShape, bool)
	federated bool
	errs      []string
}

func (c *uiChecker) addf(format string, a ...any) {
	c.errs = append(c.errs, fmt.Sprintf(format, a...))
}

func (c *uiChecker) model(key string) (modelShape, bool) {
	if c.lookup == nil || key == "" {
		return modelShape{}, false
	}
	return c.lookup(key)
}

// column reports a column that is not declared on an own model. Kernel-owned
// columns (id, organization_id, audit columns) are always accepted.
func (c *uiChecker) column(where, model, col string) {
	if col == "" {
		return
	}
	ms, ok := c.model(model)
	if !ok {
		return
	}
	if _, ok := ms.cols[col]; ok || col == "id" || kernelOwnedColumns[col] {
		return
	}
	c.addf("%s %q is not a column of model %q", where, col, model)
}

// relation resolves a relation of an own model to its child model key. ok is
// false when the model is unknown to the checker (no error) or the relation is
// missing (error reported).
func (c *uiChecker) relation(where, model, name string) (string, bool) {
	if name == "" {
		return "", false
	}
	ms, ok := c.model(model)
	if !ok {
		return "", false
	}
	child, ok := ms.rels[name]
	if !ok {
		c.addf("%s %q is not a relation of model %q (declare it in models[].relations)", where, name, model)
		return "", false
	}
	return child, true
}

// ValidateFrontendUI checks frontend.sdk and frontend.pages[] (format, expose
// and layout are the frontend's own). Errors are prefixed "frontend.". Used by
// the v3 validator and by the legacy carrier's Validate.
func ValidateFrontendUI(format, expose, sdk string, pages []FrontendPage) []string {
	var errs []string
	addf := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }
	if sdk != "" {
		if _, err := semver.NewConstraint(sdk); err != nil {
			addf("frontend.sdk %q is not a valid semver range (e.g. \"^4.0.0\"): %v", sdk, err)
		}
	}
	if len(pages) == 0 {
		return errs
	}
	if format != "federation" {
		addf("frontend.pages requires frontend.format=federation (each page is an expose of the remote), got %q", format)
	}
	if len(pages) > MaxFrontendPages {
		addf("frontend.pages declares %d pages (max %d)", len(pages), MaxFrontendPages)
	}
	keys := map[string]int{}
	paths := map[string]int{}
	for i, p := range pages {
		where := fmt.Sprintf("frontend.pages[%d]", i)
		switch {
		case p.Key == "":
			addf("%s.key is required", where)
		case !pageKeyRe.MatchString(p.Key) || len(p.Key) > 64:
			addf("%s.key %q must be snake_case (^[a-z][a-z0-9_]*$, max 64)", where, p.Key)
		default:
			if j, dup := keys[p.Key]; dup {
				addf("%s.key %q is already used by frontend.pages[%d]", where, p.Key, j)
			}
			keys[p.Key] = i
		}
		switch {
		case p.Path == "":
			addf("%s.path is required (the URL segment under the addon, e.g. \"control-tower\")", where)
		case !pagePathRe.MatchString(p.Path) || len(p.Path) > 128:
			addf("%s.path %q must be lowercase kebab-case segments without a leading slash (e.g. \"control-tower\" or \"reports/daily\"); the host mounts it under /a/<addon>/ or /addons/<addon>/", where, p.Path)
		default:
			if j, dup := paths[p.Path]; dup {
				addf("%s.path %q is already used by frontend.pages[%d]", where, p.Path, j)
			}
			paths[p.Path] = i
		}
		switch {
		case p.Expose == "":
			addf("%s.expose is required (the federated module of the page, e.g. \"./pages/control-tower\")", where)
		case !pageExposeRe.MatchString(p.Expose):
			addf("%s.expose %q must be a federation module name like \"./pages/control-tower\"", where, p.Expose)
		case expose != "" && p.Expose == expose:
			addf("%s.expose %q reuses frontend.expose (the headless register module); give the page its own expose", where, p.Expose)
		}
		switch p.Layout {
		case "", PageLayoutShell, PageLayoutImmersive:
		default:
			addf("%s.layout %q is not one of shell|immersive (shell = embedded in the host chrome, immersive = full screen)", where, p.Layout)
		}
		if strings.TrimSpace(p.Title) == "" {
			addf("%s.title is required (i18n key of the page title)", where)
		}
		seenShare := map[string]struct{}{}
		for si, sh := range p.Shares {
			if _, ok := PageShares[sh]; !ok {
				addf("%s.shares[%d] %q is not a host-provided share (want one of %s)", where, si, sh, strings.Join(sortedKeys(PageShares), "|"))
			}
			if _, dup := seenShare[sh]; dup {
				addf("%s.shares[%d] %q is duplicated", where, si, sh)
			}
			seenShare[sh] = struct{}{}
		}
		if p.BudgetKB < 0 || p.BudgetKB > MaxPageBudgetKB {
			addf("%s.budget_kb %d is out of range: 1..%d KB gzip (0 = the default %d)", where, p.BudgetKB, MaxPageBudgetKB, MaxPageBudgetKB)
		}
		if len(p.Breadcrumbs) > MaxPageBreadcrumbs {
			addf("%s.breadcrumbs declares %d crumbs (max %d)", where, len(p.Breadcrumbs), MaxPageBreadcrumbs)
		}
		for bi, b := range p.Breadcrumbs {
			if strings.TrimSpace(b.Title) == "" {
				addf("%s.breadcrumbs[%d].title is required", where, bi)
			}
		}
		for ri, r := range p.Requires {
			if strings.TrimSpace(r.Model) == "" || len(r.Actions) == 0 {
				addf("%s.requires[%d] needs model and at least one action", where, ri)
			}
		}
	}
	return errs
}

// ValidateNavView checks the structural rules of one nav entry's view_type +
// view pair (no column checks). where is the nav item path.
func ValidateNavView(where, model, viewType string, view *NavView) []string {
	c := &uiChecker{}
	c.navView(where, model, viewType, view)
	return c.errs
}

// ValidateModelDetail checks the structural rules of a model's detail panels
// (no column checks). federated reports whether the addon declares a
// federation frontend (required by kind=custom panels).
func ValidateModelDetail(where, model string, d *ModelDetail, federated bool) []string {
	c := &uiChecker{federated: federated}
	c.detail(where, model, d)
	return c.errs
}

// validateUI runs the UI grammar over a v3 manifest with column and relation
// checks against the addon's own models.
func validateUI(m *Manifest, colsByModel map[string]map[string]struct{}) []string {
	shapes := make(map[string]modelShape, len(m.Models))
	for _, mod := range m.Models {
		rels := make(map[string]string, len(mod.Relations))
		for _, r := range mod.Relations {
			rels[r.Name] = r.Through
		}
		shapes[mod.Key] = modelShape{cols: colsByModel[mod.Key], rels: rels}
	}
	c := &uiChecker{
		lookup: func(key string) (modelShape, bool) {
			ms, ok := shapes[key]
			return ms, ok
		},
		federated: m.Frontend != nil && m.Frontend.Format == "federation",
	}
	if f := m.Frontend; f != nil {
		c.errs = append(c.errs, ValidateFrontendUI(f.Format, f.Expose, f.SDK, f.Pages)...)
	}
	for mi, mod := range m.Models {
		if mod.Detail != nil {
			c.detail(fmt.Sprintf("models[%d].detail", mi), mod.Key, mod.Detail)
		}
	}
	if m.Contributions != nil {
		var walk func(items []NavItem, path string)
		walk = func(items []NavItem, path string) {
			for i, it := range items {
				where := fmt.Sprintf("%s[%d]", path, i)
				c.navView(where, it.Model, it.ViewType, it.View)
				if len(it.Items) > 0 {
					walk(it.Items, where+".items")
				}
			}
		}
		for gi, g := range m.Contributions.Navigation {
			walk(g.Items, fmt.Sprintf("contributions.navigation[%d].items", gi))
		}
	}
	return c.errs
}

func (c *uiChecker) navView(where, model, viewType string, view *NavView) {
	if !IsDeclarativeView(viewType) {
		if view != nil {
			shown := viewType
			if shown == "" {
				shown = ViewTypeTable
			}
			c.addf("%s.view is only read for view_type map|timeline|calendar|chart|split (view_type is %q)", where, shown)
		}
		return
	}
	if model == "" {
		c.addf("%s declares view_type=%s but no model (a declarative view renders that model's rows)", where, viewType)
	}
	for _, name := range view.declared() {
		if name != viewType {
			c.addf("%s.view.%s is declared but view_type is %q; only view.%s is read", where, name, viewType, viewType)
		}
	}
	vw := where + ".view." + viewType
	switch viewType {
	case ViewTypeMap:
		if view == nil || view.Map == nil {
			c.addf("%s declares view_type=map but no view.map (it names the coordinate columns)", where)
			return
		}
		c.mapView(vw, model, view.Map, false)
	case ViewTypeTimeline:
		if view == nil || view.Timeline == nil {
			c.addf("%s declares view_type=timeline but no view.timeline (it names date_field and title_field)", where)
			return
		}
		c.timelineView(vw, model, view.Timeline, false)
	case ViewTypeCalendar:
		if view == nil || view.Calendar == nil {
			c.addf("%s declares view_type=calendar but no view.calendar (it names start_field)", where)
			return
		}
		c.calendarView(vw, model, view.Calendar)
	case ViewTypeChart:
		if view == nil || view.Chart == nil {
			c.addf("%s declares view_type=chart but no view.chart (kind + query)", where)
			return
		}
		c.chartView(vw, model, view.Chart, false)
	case ViewTypeSplit:
		if view != nil && view.Split != nil {
			for i, col := range view.Split.Columns {
				c.column(fmt.Sprintf("%s.columns[%d]", vw, i), model, col)
			}
			if len(view.Split.Panels) > 0 {
				c.panels(vw+".panels", model, view.Split.Panels)
			}
		}
	}
}

// declared lists the view blocks present, in a fixed order.
func (v *NavView) declared() []string {
	if v == nil {
		return nil
	}
	var out []string
	if v.Map != nil {
		out = append(out, ViewTypeMap)
	}
	if v.Timeline != nil {
		out = append(out, ViewTypeTimeline)
	}
	if v.Calendar != nil {
		out = append(out, ViewTypeCalendar)
	}
	if v.Chart != nil {
		out = append(out, ViewTypeChart)
	}
	if v.Split != nil {
		out = append(out, ViewTypeSplit)
	}
	return out
}

func (c *uiChecker) point(where, model string, p *MapPoint) {
	if p.Lat == "" {
		c.addf("%s.lat is required (latitude column)", where)
	}
	if p.Lng == "" {
		c.addf("%s.lng is required (longitude column)", where)
	}
	c.column(where+".lat", model, p.Lat)
	c.column(where+".lng", model, p.Lng)
}

func (c *uiChecker) mapView(where, model string, mv *MapView, inPanel bool) {
	switch {
	case mv.Point != nil && mv.Geometry != "":
		c.addf("%s declares both point and geometry; use one", where)
	case mv.Point == nil && mv.Geometry == "" && !(inPanel && mv.Route != nil):
		c.addf("%s needs point {lat, lng} or geometry (a GeoJSON column)", where)
	}
	if mv.Point != nil {
		c.point(where+".point", model, mv.Point)
	}
	c.column(where+".geometry", model, mv.Geometry)
	c.column(where+".color_by", model, mv.ColorBy)
	if len(mv.Popup) > maxMapPopup {
		c.addf("%s.popup lists %d columns (max %d)", where, len(mv.Popup), maxMapPopup)
	}
	for i, col := range mv.Popup {
		c.column(fmt.Sprintf("%s.popup[%d]", where, i), model, col)
	}
	if _, ok := mapFits[mv.Fit]; !ok {
		c.addf("%s.fit %q is not one of data|org_default", where, mv.Fit)
	}
	if len(mv.Layers) > maxMapLayers {
		c.addf("%s.layers declares %d layers (max %d)", where, len(mv.Layers), maxMapLayers)
	}
	for i, l := range mv.Layers {
		lw := fmt.Sprintf("%s.layers[%d]", where, i)
		if _, ok := mapLayerKinds[l.Kind]; !ok {
			c.addf("%s.kind %q is not one of points|zones", lw, l.Kind)
		}
		if l.Model == "" {
			c.addf("%s.model is required", lw)
		}
		switch l.Kind {
		case "points":
			if l.Point == nil {
				c.addf("%s is kind=points and needs point {lat, lng}", lw)
			}
		case "zones":
			if l.Geometry == "" {
				c.addf("%s is kind=zones and needs geometry (a GeoJSON polygon column)", lw)
			}
		}
		if l.Point != nil {
			c.point(lw+".point", l.Model, l.Point)
		}
		c.column(lw+".geometry", l.Model, l.Geometry)
		c.column(lw+".label", l.Model, l.Label)
		c.column(lw+".color_by", l.Model, l.ColorBy)
	}
	if r := mv.Route; r != nil {
		rw := where + ".route"
		if !inPanel {
			c.addf("%s is only valid in a detail panel (it draws one record's ordered child points)", rw)
			return
		}
		if r.Relation == "" {
			c.addf("%s.relation is required", rw)
		}
		child, ok := c.relation(rw+".relation", model, r.Relation)
		if r.Point == nil {
			c.addf("%s.point {lat, lng} is required (the child's coordinate columns)", rw)
		} else if ok {
			c.point(rw+".point", child, r.Point)
		}
		if r.OrderBy == "" {
			c.addf("%s.order_by is required (the child column that orders the points)", rw)
		} else if ok {
			c.column(rw+".order_by", child, r.OrderBy)
		}
	}
}

func (c *uiChecker) timelineView(where, model string, tv *TimelineView, inPanel bool) {
	if tv.DateField == "" {
		c.addf("%s.date_field is required", where)
	}
	if tv.TitleField == "" {
		c.addf("%s.title_field is required", where)
	}
	fields := []struct{ name, val string }{
		{"date_field", tv.DateField}, {"title_field", tv.TitleField}, {"status_field", tv.StatusField},
		{"description_field", tv.DescriptionField}, {"group_by", tv.GroupBy},
	}
	if !inPanel {
		if tv.Source != nil {
			c.addf("%s.source is only valid in a detail panel (a nav timeline lists the model's own rows)", where)
		}
		for _, f := range fields {
			c.column(where+"."+f.name, model, f.val)
		}
		return
	}
	src := tv.Source
	sw := where + ".source"
	switch {
	case src == nil:
		c.addf("%s is required in a detail panel: source.relation (child rows) or source.capability (a capability query)", sw)
		return
	case src.Relation != "" && src.Capability != "":
		c.addf("%s declares both relation and capability; use one", sw)
		return
	case src.Relation == "" && src.Capability == "":
		c.addf("%s needs relation or capability", sw)
		return
	}
	if src.Relation != "" {
		if len(src.Input) > 0 || src.Items != "" {
			c.addf("%s: input and items are only read with capability", sw)
		}
		if child, ok := c.relation(sw+".relation", model, src.Relation); ok {
			for _, f := range fields {
				c.column(where+"."+f.name, child, f.val)
			}
		}
		return
	}
	c.capabilitySource(where, model, src, fields)
}

// capabilitySource checks a timeline fed by a capability query: the key, the
// record→input mapping and, for a well-known contract, that Items is an array
// of objects in its output carrying the named fields.
func (c *uiChecker) capabilitySource(where, model string, src *TimelineSource, fields []struct{ name, val string }) {
	sw := where + ".source"
	if !capability.ValidKey(src.Capability) {
		c.addf("%s.capability %q must be a dotted lowercase capability key (e.g. commerce.shipping.track)", sw, src.Capability)
		return
	}
	if src.Items == "" {
		c.addf("%s.items is required: the array field of the capability output holding the entries (e.g. \"events\")", sw)
	}
	targets := make([]string, 0, len(src.Input))
	for k := range src.Input {
		targets = append(targets, k)
	}
	sort.Strings(targets)
	for _, k := range targets {
		expr := src.Input[k]
		if strings.HasPrefix(expr, "payload.") {
			c.addf("%s.input[%q] %q: a timeline reads the record; use record.<column> or const:<literal>", sw, k, expr)
			continue
		}
		if err := capability.ValidateSource(expr); err != nil {
			c.addf("%s.input[%q]: %v", sw, k, err)
			continue
		}
		if col, ok := strings.CutPrefix(expr, "record."); ok {
			c.column(fmt.Sprintf("%s.input[%q]", sw, k), model, col)
		}
	}
	contract, known := capability.Lookup(src.Capability)
	if !known {
		return
	}
	if !contract.IsQuery() {
		c.addf("%s.capability %q is an action contract; a timeline needs a query contract with an output", sw, src.Capability)
		return
	}
	for _, k := range targets {
		if !contract.Has(k) {
			c.addf("%s.input key %q is not a field of capability %q (fields: %s)", sw, k, contract.Key, strings.Join(contract.Fields(), ", "))
		}
	}
	for _, req := range contract.Required {
		if _, ok := src.Input[req]; !ok {
			c.addf("%s.input must map required field %q of capability %q", sw, req, contract.Key)
		}
	}
	if src.Items == "" {
		return
	}
	f, ok := contract.Output.Field(src.Items)
	if !ok || f.Type != capability.TypeArray || f.Of == nil {
		c.addf("%s.items %q is not an array of objects in the output of %q", sw, src.Items, contract.Key)
		return
	}
	names := make([]string, 0, len(f.Of.Fields))
	for _, ff := range f.Of.Fields {
		names = append(names, ff.Name)
	}
	for _, fl := range fields {
		if fl.val == "" {
			continue
		}
		if _, ok := f.Of.Field(fl.val); !ok {
			c.addf("%s.%s %q is not a field of %s %s[] (fields: %s)", where, fl.name, fl.val, contract.Key, src.Items, strings.Join(names, ", "))
		}
	}
}

func (c *uiChecker) calendarView(where, model string, cv *CalendarView) {
	if cv.StartField == "" {
		c.addf("%s.start_field is required", where)
	}
	c.column(where+".start_field", model, cv.StartField)
	c.column(where+".end_field", model, cv.EndField)
	c.column(where+".title_field", model, cv.TitleField)
	c.column(where+".color_by", model, cv.ColorBy)
	c.column(where+".all_day_field", model, cv.AllDayField)
}

func (c *uiChecker) chartView(where, model string, cv *ChartView, inPanel bool) {
	if _, ok := chartKinds[cv.Kind]; !ok {
		c.addf("%s.kind %q is not one of bar|line|area|pie|donut", where, cv.Kind)
	}
	if cv.Format != "" {
		if _, ok := dashFormats[cv.Format]; !ok {
			c.addf("%s.format %q is not one of number|currency|percent|compact", where, cv.Format)
		}
	}
	if cv.Query == nil {
		c.addf("%s.query is required (same shape as a dashboard widget query)", where)
		return
	}
	q := cv.Query
	c.errs = append(c.errs, validateWidgetQuery(where, cv.Kind, q)...)
	switch {
	case !inPanel && cv.Relation != "":
		c.addf("%s.relation is only valid in a detail panel (it scopes the chart to the record's children)", where)
	case !inPanel:
		if q.Model != "" && model != "" && bareModel(q.Model) != bareModel(model) {
			c.addf("%s.query.model %q must be the nav entry's model %q (a chart view aggregates the rows it lists)", where, q.Model, model)
		}
	case cv.Relation == "":
		c.addf("%s.relation is required in a detail panel (the chart aggregates the record's children)", where)
	default:
		if child, ok := c.relation(where+".relation", model, cv.Relation); ok && q.Model != "" && q.Model != child {
			c.addf("%s.query.model %q must be %q, the child model of relation %q", where, q.Model, child, cv.Relation)
		}
	}
	qw := where + ".query"
	c.column(qw+".field", q.Model, q.Field)
	c.column(qw+".group_by", q.Model, q.GroupBy)
	c.column(qw+".label_field", q.Model, q.LabelField)
	c.column(qw+".date_field", q.Model, q.DateField)
}

func bareModel(key string) string {
	if i := strings.LastIndex(key, "."); i >= 0 {
		return key[i+1:]
	}
	return key
}

func (c *uiChecker) detail(where, model string, d *ModelDetail) {
	if d == nil {
		return
	}
	if len(d.Panels) == 0 {
		c.addf("%s.panels must declare at least one panel (omit detail for the default page)", where)
		return
	}
	c.panels(where+".panels", model, d.Panels)
}

func (c *uiChecker) panels(where, model string, panels []DetailPanel) {
	if len(panels) > maxPanels {
		c.addf("%s declares %d panels (max %d)", where, len(panels), maxPanels)
	}
	keys := map[string]int{}
	for i, p := range panels {
		pw := fmt.Sprintf("%s[%d]", where, i)
		switch {
		case p.Key == "":
			c.addf("%s.key is required", pw)
		case !panelKeyRe.MatchString(p.Key):
			c.addf("%s.key %q must be snake_case", pw, p.Key)
		default:
			if j, dup := keys[p.Key]; dup {
				c.addf("%s.key %q is already used by %s[%d]", pw, p.Key, where, j)
			}
			keys[p.Key] = i
		}
		if strings.TrimSpace(p.Title) == "" {
			c.addf("%s.title is required (i18n key of the panel heading)", pw)
		}
		if _, ok := panelSpans[p.Span]; !ok {
			c.addf("%s.span %q is not one of full|half", pw, p.Span)
		}
		blocks := map[string]bool{
			PanelKindMap: p.Map != nil, PanelKindTimeline: p.Timeline != nil, PanelKindChart: p.Chart != nil,
			PanelKindRelation: p.Relation != nil, PanelKindStats: p.Stats != nil, "expose": p.Expose != "",
		}
		want := p.Kind
		if want == PanelKindCustom {
			want = "expose"
		}
		for _, name := range []string{PanelKindMap, PanelKindTimeline, PanelKindChart, PanelKindRelation, PanelKindStats, "expose"} {
			if blocks[name] && name != want {
				c.addf("%s declares %s but kind is %q; only the block named like the kind is read", pw, name, p.Kind)
			}
		}
		switch p.Kind {
		case PanelKindMap:
			if p.Map == nil {
				c.addf("%s is kind=map and needs map", pw)
			} else {
				c.mapView(pw+".map", model, p.Map, true)
			}
		case PanelKindTimeline:
			if p.Timeline == nil {
				c.addf("%s is kind=timeline and needs timeline", pw)
			} else {
				c.timelineView(pw+".timeline", model, p.Timeline, true)
			}
		case PanelKindChart:
			if p.Chart == nil {
				c.addf("%s is kind=chart and needs chart", pw)
			} else {
				c.chartView(pw+".chart", model, p.Chart, true)
			}
		case PanelKindRelation:
			if p.Relation == nil {
				c.addf("%s is kind=relation and needs relation {name}", pw)
				continue
			}
			rw := pw + ".relation"
			if p.Relation.Via != "" {
				c.addf("%s.via is only valid in contributions.inject[] panels; a model's own panel names a relation with name", rw)
			}
			if p.Relation.Name == "" {
				c.addf("%s.name is required", rw)
			}
			if p.Relation.Limit < 0 || p.Relation.Limit > maxRelationRow {
				c.addf("%s.limit %d is out of range 1..%d (0 = default 10)", rw, p.Relation.Limit, maxRelationRow)
			}
			if child, ok := c.relation(rw+".name", model, p.Relation.Name); ok {
				for ci, col := range p.Relation.Columns {
					c.column(fmt.Sprintf("%s.columns[%d]", rw, ci), child, col)
				}
			}
		case PanelKindStats:
			if p.Stats == nil || len(p.Stats.Items) == 0 {
				c.addf("%s is kind=stats and needs stats.items", pw)
				continue
			}
			if len(p.Stats.Items) > maxStatItems {
				c.addf("%s.stats.items declares %d figures (max %d)", pw, len(p.Stats.Items), maxStatItems)
			}
			for si, it := range p.Stats.Items {
				sw := fmt.Sprintf("%s.stats.items[%d]", pw, si)
				if it.Field == "" {
					c.addf("%s.field is required", sw)
				}
				c.column(sw+".field", model, it.Field)
				if _, ok := statFormats[it.Format]; !ok {
					c.addf("%s.format %q is not one of number|currency|percent|compact|date|datetime|relative_date", sw, it.Format)
				}
			}
		case PanelKindCustom:
			switch {
			case p.Expose == "":
				c.addf("%s is kind=custom and must declare expose (the federated component)", pw)
			case !pageExposeRe.MatchString(p.Expose):
				c.addf("%s.expose %q must be a federation module name like \"./panels/route-map\"", pw, p.Expose)
			}
			if !c.federated {
				c.addf("%s is kind=custom and requires a top-level frontend block with format=federation", pw)
			}
		default:
			c.addf("%s.kind %q is not one of map|timeline|chart|relation|stats|custom", pw, p.Kind)
		}
	}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
