package v3

// UI surfaces an addon declares beyond the dynamic table: federated pages
// (frontend.pages[]), declarative views per nav entry (NavItem.View) and
// detail panels per model (Model.Detail). Declarative first: a view or a panel
// ships zero JavaScript in the addon (the SDK renders it, loaded lazily per
// type); a federated page is the escape hatch for interactive flows the
// declarative grammar cannot express. See docs/ui-pages-and-views.md.

// Frontend page layouts. The wire keeps the names Frontend.Layout already
// published; docs and UI call them "embedded" and "full screen".
const (
	// PageLayoutShell mounts the page inside the host chrome (sidebar,
	// header, breadcrumbs): an embedded page.
	PageLayoutShell = "shell"
	// PageLayoutImmersive gives the page the whole viewport (POS, kiosk,
	// kitchen display).
	PageLayoutImmersive = "immersive"
)

// Limits of frontend.pages[].
const (
	// MaxFrontendPages caps the pages one addon declares.
	MaxFrontendPages = 32
	// MaxPageBudgetKB is the largest per-page JavaScript budget (gzip KB of
	// the page's own chunks, shares excluded) an addon may declare. The hub
	// enforces min(budget_kb, MaxPageBudgetKB) on publish.
	MaxPageBudgetKB = 150
	// MaxPageBreadcrumbs caps the static breadcrumb trail of a page.
	MaxPageBreadcrumbs = 4
)

// PageShares is the closed set of lazy shared modules a page may ask the host
// for (frontend.pages[].shares), mapped to the host capability that proves the
// host provides it. The remote never bundles its own copy: a host without the
// capability refuses the install instead of loading two MapLibre/recharts.
var PageShares = map[string]string{
	"@asteby/metacore-maps":   "ui.maps",
	"maplibre-gl":             "ui.maps",
	"@asteby/metacore-charts": "ui.charts",
	"recharts":                "ui.charts",
}

// View types a NavItem may declare. ViewTypeTable and ViewTypeKanban predate
// the declarative views and need no host capability; the rest are rendered by
// a lazily loaded SDK renderer and require host capability "ui.views.<type>".
const (
	ViewTypeTable    = "table"
	ViewTypeKanban   = "kanban"
	ViewTypeMap      = "map"
	ViewTypeTimeline = "timeline"
	ViewTypeCalendar = "calendar"
	ViewTypeChart    = "chart"
	ViewTypeSplit    = "split"
)

// DeclarativeViewTypes lists the view types that need a host renderer
// (capability "ui.views.<type>"), in documentation order.
var DeclarativeViewTypes = []string{ViewTypeMap, ViewTypeTimeline, ViewTypeCalendar, ViewTypeChart, ViewTypeSplit}

// Detail panel kinds (Model.Detail.Panels[].Kind).
const (
	PanelKindMap      = "map"
	PanelKindTimeline = "timeline"
	PanelKindChart    = "chart"
	PanelKindRelation = "relation"
	PanelKindStats    = "stats"
	PanelKindCustom   = "custom"
)

// FrontendPage is one federated page of the addon's remote. Each page has its
// own Expose (a separate chunk with its own budget), so one remote serves many
// screens without sibling manifests. The host resolves where to mount it
// BEFORE downloading the remote: Layout "shell" mounts it embedded under
// /a/<addon>/<path>, "immersive" full screen under /addons/<addon>/<path>.
// A nav entry links to it with url "addon://<addon>/<path>".
type FrontendPage struct {
	// Key is the page's stable id, unique within the addon (snake_case).
	Key string `json:"key"`
	// Path is the URL segment(s) under the addon base, lowercase kebab-case
	// ("control-tower", "reports/daily"). Deep links below it reach the page.
	Path string `json:"path"`
	// Expose is the federated module that default-exports the page component
	// (e.g. "./pages/control-tower"). Must differ from Frontend.Expose.
	Expose string `json:"expose"`
	// Layout is "shell" (embedded) or "immersive" (full screen). Empty =
	// Frontend.Layout, then "shell".
	Layout string `json:"layout,omitempty"`
	// Title is the i18n key of the page title (header, document title).
	Title string `json:"title"`
	// Icon is an optional lucide icon name.
	Icon string `json:"icon,omitempty"`
	// Permission gates the page: the host does not mount it (its own 403)
	// when the viewer lacks it. Same vocabulary as NavItem.Permission.
	Permission string `json:"permission,omitempty"`
	// Requires declares the data surface the page calls, projected to
	// capability strings exactly like NavItem.Requires (Acceder expansion).
	Requires []NavRequire `json:"requires,omitempty"`
	// Shares lists lazy host-provided modules the page imports (closed set:
	// see PageShares). Each implies a host capability (ui.maps, ui.charts).
	Shares []string `json:"shares,omitempty"`
	// BudgetKB is the page's own JavaScript budget in gzip KB, shares
	// excluded. 0 = the default (MaxPageBudgetKB). The hub rejects a publish
	// whose page chunk exceeds it.
	BudgetKB int `json:"budget_kb,omitempty"`
	// Breadcrumbs is the static trail shown before the page title. Empty =
	// derived from the navigation entry that links to the page.
	Breadcrumbs []PageBreadcrumb `json:"breadcrumbs,omitempty"`
}

// EffectiveLayout resolves the page layout: its own, else the addon's
// Frontend.Layout, else "shell".
func (p FrontendPage) EffectiveLayout(addonLayout string) string {
	switch {
	case p.Layout != "":
		return p.Layout
	case addonLayout != "":
		return addonLayout
	}
	return PageLayoutShell
}

// PageBreadcrumb is one static breadcrumb of a page.
type PageBreadcrumb struct {
	// Title is the i18n key shown.
	Title string `json:"title"`
	// URL is an optional host URL the crumb links to.
	URL string `json:"url,omitempty"`
}

// NavView configures the declarative renderer a NavItem's ViewType selects.
// Exactly the block matching ViewType is read; the others must be absent. It
// travels in the served table metadata (on demand), never in the lite
// manifest: the shell only needs ViewType to build the link.
type NavView struct {
	Map      *MapView      `json:"map,omitempty"`
	Timeline *TimelineView `json:"timeline,omitempty"`
	Calendar *CalendarView `json:"calendar,omitempty"`
	Chart    *ChartView    `json:"chart,omitempty"`
	Split    *SplitView    `json:"split,omitempty"`
}

// MapView renders a model's rows (or, in a detail panel, one record) as map
// features. Coordinates come from Point (two numeric columns) or Geometry (a
// GeoJSON column), never both.
type MapView struct {
	// Point names the latitude/longitude columns.
	Point *MapPoint `json:"point,omitempty"`
	// Geometry names a GeoJSON (jsonb) column.
	Geometry string `json:"geometry,omitempty"`
	// Cluster groups nearby points.
	Cluster bool `json:"cluster,omitempty"`
	// ColorBy names a column whose option colours tint each feature.
	ColorBy string `json:"color_by,omitempty"`
	// Popup lists the columns shown when a feature is selected (max 8).
	Popup []string `json:"popup,omitempty"`
	// Layers adds features from other models (branches, coverage zones).
	Layers []MapLayer `json:"layers,omitempty"`
	// Route draws the record's ordered child points (detail panels only).
	Route *MapRoute `json:"route,omitempty"`
	// Fit is the initial viewport: "data" (default, fit the features) or
	// "org_default" (the organization's default area).
	Fit string `json:"fit,omitempty"`
}

// MapPoint names a latitude and a longitude column (WGS84 degrees).
type MapPoint struct {
	Lat string `json:"lat"`
	Lng string `json:"lng"`
}

// MapLayer is an extra feature layer from another model, own or another
// addon's ("locations.Branch").
type MapLayer struct {
	// Kind is "points" (needs Point) or "zones" (needs Geometry polygons).
	Kind  string `json:"kind"`
	Model string `json:"model"`
	// Point / Geometry are columns of Model.
	Point    *MapPoint `json:"point,omitempty"`
	Geometry string    `json:"geometry,omitempty"`
	// Icon is an optional lucide icon name for points.
	Icon string `json:"icon,omitempty"`
	// Label names a column of Model used as the feature label.
	Label string `json:"label,omitempty"`
	// ColorBy names a column of Model whose option colours tint features.
	ColorBy string `json:"color_by,omitempty"`
}

// MapRoute draws a line through a record's child rows in order (e.g. a
// shipment's scans): Relation is one of the model's relations, Point names
// the child's coordinate columns and OrderBy the child column that orders them.
type MapRoute struct {
	Relation string    `json:"relation"`
	Point    *MapPoint `json:"point"`
	OrderBy  string    `json:"order_by"`
}

// TimelineView renders dated entries in order. On a nav entry the entries are
// the model's rows; in a detail panel they come from Source.
type TimelineView struct {
	// DateField orders and dates each entry. Required.
	DateField string `json:"date_field"`
	// TitleField is each entry's heading. Required.
	TitleField string `json:"title_field"`
	// StatusField, DescriptionField and GroupBy are optional.
	StatusField      string `json:"status_field,omitempty"`
	DescriptionField string `json:"description_field,omitempty"`
	GroupBy          string `json:"group_by,omitempty"`
	// Source is where a detail panel reads its entries (required there,
	// invalid on a nav entry).
	Source *TimelineSource `json:"source,omitempty"`
}

// TimelineSource is the origin of a timeline panel's entries: the child rows
// of one of the model's relations, OR the array field Items of a capability
// query's output, called with Input mapped from the record (record.<column> or
// const:<literal>). The host runs the capability query with the viewer's
// permissions and a short cache; the addon copies no data.
type TimelineSource struct {
	Relation   string            `json:"relation,omitempty"`
	Capability string            `json:"capability,omitempty"`
	Input      map[string]string `json:"input,omitempty"`
	Items      string            `json:"items,omitempty"`
}

// CalendarView places a model's rows on a calendar.
type CalendarView struct {
	// StartField is the date/timestamp column each entry starts at. Required.
	StartField string `json:"start_field"`
	// EndField is an optional end column (single-point entries without it).
	EndField string `json:"end_field,omitempty"`
	// TitleField is the entry label (default: the model's display column).
	TitleField string `json:"title_field,omitempty"`
	// ColorBy names a column whose option colours tint entries.
	ColorBy string `json:"color_by,omitempty"`
	// AllDayField names a boolean column marking all-day entries.
	AllDayField string `json:"all_day_field,omitempty"`
}

// ChartView renders one aggregate with the dashboard engine (the same
// WidgetQuery as contributions.dashboard[]): no new aggregation logic.
type ChartView struct {
	// Kind is bar|line|area|pie|donut.
	Kind  string       `json:"kind"`
	Query *WidgetQuery `json:"query"`
	// Format applies to values: number|currency|percent|compact.
	Format string `json:"format,omitempty"`
	// Relation scopes the query to the record's children (detail panels
	// only): Query.Model must be the relation's child model.
	Relation string `json:"relation,omitempty"`
}

// SplitView is master-detail on one screen: the list on one side, the
// selected record's panels on the other.
type SplitView struct {
	// Columns are the list columns (default: NavItem.Columns, then the
	// model's defaults).
	Columns []string `json:"columns,omitempty"`
	// Panels are the detail panels (default: the model's Detail.Panels).
	Panels []DetailPanel `json:"panels,omitempty"`
}

// ModelDetail declares the panels of a model's record detail page, rendered
// by the SDK next to the record's fields (master-detail).
type ModelDetail struct {
	Panels []DetailPanel `json:"panels"`
}

// DetailPanel is one block of a record's detail page. Kind selects the block
// read: map → Map, timeline → Timeline, chart → Chart, relation → Relation,
// stats → Stats, custom → Expose (a federated component; needs a frontend).
type DetailPanel struct {
	// Key is unique within the model (snake_case).
	Key string `json:"key"`
	// Kind is map|timeline|chart|relation|stats|custom.
	Kind string `json:"kind"`
	// Title is the i18n key of the panel heading.
	Title string `json:"title"`
	// Span is the panel width: "full" (default) or "half".
	Span string `json:"span,omitempty"`
	// Permission optionally hides the panel from viewers without it.
	Permission string `json:"permission,omitempty"`

	Map      *MapView       `json:"map,omitempty"`
	Timeline *TimelineView  `json:"timeline,omitempty"`
	Chart    *ChartView     `json:"chart,omitempty"`
	Relation *RelationPanel `json:"relation,omitempty"`
	Stats    *StatsPanel    `json:"stats,omitempty"`
	// Expose is the federated component of a kind=custom panel.
	Expose string `json:"expose,omitempty"`
}

// RelationPanel lists the record's children through one of the model's
// relations.
type RelationPanel struct {
	// Name is the relation (Model.Relations[].Name). Required in
	// models[].detail panels; not used by contributions.inject[] panels.
	Name string `json:"name,omitempty"`
	// Via is used only by contributions.inject[] panels: the CONTRIBUTOR's
	// model and the foreign-key column pointing at the target record
	// ("Shipment.sales_order_id"). The owner never declares a relation to a
	// model it does not know.
	Via string `json:"via,omitempty"`
	// Columns are child columns shown (default: the child's defaults).
	Columns []string `json:"columns,omitempty"`
	// Limit caps the rows shown before "see all" (1..50, default 10).
	Limit int `json:"limit,omitempty"`
}

// StatsPanel shows a few of the record's own values as figures.
type StatsPanel struct {
	Items []StatItem `json:"items"`
}

// StatItem is one figure of a stats panel.
type StatItem struct {
	// Field is a column of the model.
	Field string `json:"field"`
	// Label is an optional i18n key (default: the column label).
	Label string `json:"label,omitempty"`
	// Format: number|currency|percent|compact|date|datetime|relative_date.
	Format string `json:"format,omitempty"`
}
