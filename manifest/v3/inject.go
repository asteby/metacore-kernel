package v3

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Inject kinds (contributions.inject[].kind). The body block carries the same
// name as the kind, except tab, which reuses the panel body.
const (
	InjectKindPanel  = "panel"
	InjectKindTab    = "tab"
	InjectKindColumn = "column"
	InjectKindBadge  = "badge"
	InjectKindFilter = "filter"
	InjectKindWidget = "widget"
)

// SlotsCatalogVersion is the version of the host slot catalog InjectSlots
// describes. A new slot is an additive bump (slots@2); renaming or removing
// one is a breaking change.
const SlotsCatalogVersion = "slots@1"

// Order bands of contributions.inject[]: the owner of the target model uses
// 0..InjectOwnerOrderMax, any other contributor InjectContributorOrderMin and
// up. Lower renders first. Ties break on (addon key, inject key).
const (
	InjectOwnerOrderMax       = 199
	InjectContributorOrderMin = 200
)

// MaxInjectPerAddon caps contributions.inject[] per addon. Every computed
// column or badge costs the host one grouped query per page it serves, so the
// list is bounded (also in the schema, maxItems) against a hostile manifest.
const MaxInjectPerAddon = 64

// Inject is one declarative contribution to a model another addon owns (or, for
// the owner itself, to one of its own models).
//
// Exactly one body is read, the one the kind names: panel|tab → Panel,
// column → Column, badge → Badge, filter → Filter, widget → Widget. The host
// serves it per org and per installation when it answers the target model's
// metadata, so nothing is copied into the owner and uninstalling the
// contributor removes it. Edits to the owner's rows are NOT injectable: they
// stay with models[].extensions[] and its opt-in.
type Inject struct {
	// Key is unique within the addon (snake_case). The contribution is known
	// to the host as "<addon>:<key>"; a column is exposed as "<addon>__<key>".
	Key string `json:"key"`
	// Kind is panel|tab|column|badge|filter|widget.
	Kind string `json:"kind"`
	// Target is the model receiving the contribution: a concrete
	// "<addon>.<Model>" ("customers.SalesOrder") or a role "@<domain>.<entity>"
	// ("@commerce.order") that resolves to the model its owner published as the
	// read contract commerce.order@1. Optional only for kind=widget (the
	// dashboard has no model).
	Target string `json:"target,omitempty"`
	// Slot is the host slot the body mounts in, from the slots@1 catalog (see
	// InjectSlots). Default: the first slot valid for the kind.
	Slot string `json:"slot,omitempty"`
	// Order positions the contribution within the slot, ascending. Owner of
	// the target: 0..199. Any other contributor: 200 and up (default 200).
	// A pointer so an explicit 0 is distinguishable from absent.
	Order *int `json:"order,omitempty"`
	// Permission optionally hides the contribution from viewers without this
	// permission key (declared by the contributor in rbac.permissions).
	Permission string `json:"permission,omitempty"`
	// Condition gates the contribution server-side per org. See Condition.
	Condition *Condition `json:"condition,omitempty"`

	// Panel is the body of kind=panel|tab: the same DetailPanel the owner's
	// models[].detail uses, except a relation panel names its source with
	// relation.via instead of relation.name.
	Panel *DetailPanel `json:"panel,omitempty"`
	// Column is the body of kind=column.
	Column *InjectColumn `json:"column,omitempty"`
	// Badge is the body of kind=badge.
	Badge *InjectBadge `json:"badge,omitempty"`
	// Filter is the body of kind=filter.
	Filter *InjectFilter `json:"filter,omitempty"`
	// Widget is the body of kind=widget: the same DashboardWidget as
	// contributions.dashboard[].
	Widget *DashboardWidget `json:"widget,omitempty"`
}

// InjectColumn is a computed list column: one value per target row, read from
// a model of THIS addon joined by a foreign key to the target row. The host
// resolves it with one grouped query per page, scoped to the org.
type InjectColumn struct {
	// Label is the i18n key of the column heading.
	Label string `json:"label"`
	// From is the Key of one of THIS manifest's models holding the rows.
	From string `json:"from"`
	// ForeignKey is the From column holding the target row's id.
	ForeignKey string `json:"foreign_key"`
	// Pick selects the value: "latest" (the row with the greatest OrderBy),
	// "first" (the smallest OrderBy, or the oldest row when OrderBy is
	// omitted) or "agg" (an aggregate over all matching rows).
	Pick string `json:"pick"`
	// Field is the From column shown (pick latest|first) or aggregated
	// (pick agg, unless Aggregate is count).
	Field string `json:"field,omitempty"`
	// OrderBy is the From column that orders rows for pick latest|first.
	// Required for latest.
	OrderBy string `json:"order_by,omitempty"`
	// Aggregate for pick agg: count|sum|avg|min|max (same vocabulary as
	// OptionMetric.Aggregate).
	Aggregate string `json:"aggregate,omitempty"`
	// Where narrows the rows with equality predicates (null means IS NULL),
	// like OptionMetric.Where. Names must be From columns.
	Where map[string]any `json:"where,omitempty"`
	// Display is text|badge|date|datetime|number|currency. Default text.
	Display string `json:"display,omitempty"`
}

// InjectBadge is a chip in the target record's header: the value of a column
// of THIS addon's model, joined by a foreign key to the target row, mapped to
// a tone.
type InjectBadge struct {
	// Label is the optional i18n key shown before the value.
	Label string `json:"label,omitempty"`
	// From is the Key of one of THIS manifest's models.
	From string `json:"from"`
	// ForeignKey is the From column holding the target row's id.
	ForeignKey string `json:"foreign_key"`
	// Field is the From column whose latest value is shown.
	Field string `json:"field"`
	// Values maps a Field value to a tone: success|warning|danger|info|neutral.
	Values map[string]string `json:"values,omitempty"`
}

// InjectFilter is a fixed filter in the target's list over a computed column
// this same manifest injects.
type InjectFilter struct {
	// Column is the Key of a contributions.inject[] entry of kind=column.
	Column string `json:"column"`
	// Label is the optional i18n key of the filter control.
	Label string `json:"label,omitempty"`
}

// InjectSlot is one entry of the slots@1 catalog.
type InjectSlot struct {
	// Name is the slot, e.g. "record.tabs".
	Name string
	// Kinds are the inject kinds the slot accepts.
	Kinds []string
}

// injectSlots is the slots@1 catalog an inject may mount in. The host owns the
// catalog; slots whose body is not an inject (record.actions takes
// contributions.actions[]; form.sections takes extensions[]; document.<region>
// takes document regions) are intentionally absent here.
var injectSlots = []InjectSlot{
	{"record.panels", []string{InjectKindPanel}},
	{"record.tabs", []string{InjectKindTab, InjectKindPanel}},
	{"record.header.badges", []string{InjectKindBadge}},
	{"list.columns", []string{InjectKindColumn}},
	{"list.filters", []string{InjectKindFilter}},
	{"dashboard.widgets", []string{InjectKindWidget}},
}

// InjectSlots returns a copy of the slots@1 catalog contributions.inject[] may
// mount in, in a stable order.
func InjectSlots() []InjectSlot {
	out := make([]InjectSlot, len(injectSlots))
	for i, s := range injectSlots {
		out[i] = InjectSlot{Name: s.Name, Kinds: append([]string(nil), s.Kinds...)}
	}
	return out
}

var (
	injectKinds = []string{InjectKindPanel, InjectKindTab, InjectKindColumn, InjectKindBadge, InjectKindFilter, InjectKindWidget}

	// concreteTargetRe is "<addon>.<Model>"; roleTargetRe is "@<domain>.<entity>".
	concreteTargetRe = regexp.MustCompile(`^([a-z][a-z0-9_-]*)\.([A-Z][A-Za-z0-9]*)$`)
	roleTargetRe     = regexp.MustCompile(`^@[a-z][a-z0-9_]*\.[a-z][a-z0-9_]*$`)
	viaRe            = regexp.MustCompile(`^([A-Z][A-Za-z0-9]*)\.([a-z][a-z0-9_]*)$`)

	injectPicks    = map[string]struct{}{"latest": {}, "first": {}, "agg": {}}
	injectDisplays = map[string]struct{}{"": {}, "text": {}, "badge": {}, "date": {}, "datetime": {}, "number": {}, "currency": {}}
	injectTones    = map[string]struct{}{"success": {}, "warning": {}, "danger": {}, "info": {}, "neutral": {}}
	injectAggs     = map[string]struct{}{"count": {}, "sum": {}, "avg": {}, "min": {}, "max": {}}
)

// ActionTargetProblem checks the shape of an action's target_model. A bare
// model key (the action's own addon) is accepted unchanged for compatibility;
// anything that looks qualified ("." in it) or by role ("@" prefix) must be a
// well-formed "<addon>.<Model>" or "@<domain>.<entity>". It returns "" when
// acceptable.
func ActionTargetProblem(target string) string {
	if target == "" || (!strings.HasPrefix(target, "@") && !strings.Contains(target, ".")) {
		return ""
	}
	return injectTargetProblem(target)
}

func injectTargetProblem(target string) string {
	if concreteTargetRe.MatchString(target) || roleTargetRe.MatchString(target) {
		return ""
	}
	return fmt.Sprintf("target %q must be a concrete \"<addon>.<Model>\" (e.g. customers.SalesOrder) or a role \"@<domain>.<entity>\" (e.g. @commerce.order)", target)
}

// InjectEnv is what ValidateInject knows about the declaring manifest.
type InjectEnv struct {
	// AddonKey is the declaring addon's key (metadata.key / legacy key).
	AddonKey string
	// Cols indexes the addon's OWN models by model key to their declared
	// columns.
	Cols map[string]map[string]struct{}
	// Federated reports a top-level frontend block with format=federation
	// (needed by kind=custom panels and widgets).
	Federated bool
	// ValidateWidget checks a kind=widget body with the dashboard rules. nil
	// skips it (the legacy carrier has no dashboard).
	ValidateWidget func(where string, w DashboardWidget) []string
	// Permissions are the keys declared in rbac.permissions. When non-nil, a
	// contribution permission under this addon's own prefix must be one of
	// them (a typo would hide the contribution from every role). nil skips
	// the check (the legacy carrier).
	Permissions map[string]struct{}
}

// maxInjectWhere caps the equality predicates of a computed column.
const maxInjectWhere = 16

// OwnModelOfTarget reports the model key a target names when it is a concrete
// "<addon>.<Model>" of the addon addonKey itself, and "" otherwise (a bare
// key, a role, or another addon's model).
func OwnModelOfTarget(addonKey, target string) string {
	if addonKey == "" {
		return ""
	}
	if mm := concreteTargetRe.FindStringSubmatch(target); mm != nil && mm[1] == addonKey {
		return mm[2]
	}
	return ""
}

// ValidateInject enforces contributions.inject[] and the sealed/primary_from
// lists of extension_points. It is the single rule set shared by the v3
// validator and the legacy carrier's Validate. Errors name the path, e.g.
// "contributions.inject[shipment_panel].order 100 is below 200 ...".
func ValidateInject(env InjectEnv, inject []Inject, ep *ExtensionPoints) []string {
	var errs []string
	if len(inject) > MaxInjectPerAddon {
		errs = append(errs, fmt.Sprintf("contributions.inject has %d entries; at most %d per addon", len(inject), MaxInjectPerAddon))
		return errs
	}
	seen := make(map[string]struct{}, len(inject))
	// columnTargets maps each kind=column key to its target, so a filter
	// can only sit on the list that shows its column.
	columnTargets := map[string]string{}
	for _, in := range inject {
		if in.Kind == InjectKindColumn && in.Key != "" {
			if _, dup := columnTargets[in.Key]; !dup {
				columnTargets[in.Key] = in.Target
			}
		}
	}
	for i, in := range inject {
		id := in.Key
		if id == "" {
			id = fmt.Sprintf("#%d", i)
		}
		where := fmt.Sprintf("contributions.inject[%s]", id)
		addf := func(format string, a ...any) { errs = append(errs, where+"."+fmt.Sprintf(format, a...)) }

		switch {
		case in.Key == "":
			addf("key is required")
		case !pageKeyRe.MatchString(in.Key):
			addf("key %q must be snake_case", in.Key)
		default:
			if _, dup := seen[in.Key]; dup {
				addf("key %q is duplicated within the addon", in.Key)
			}
			seen[in.Key] = struct{}{}
		}

		if !validInjectKind(in.Kind) {
			if in.Kind == "" {
				addf("kind is required (one of %s)", strings.Join(injectKinds, "|"))
			} else {
				addf("kind %q is not one of %s", in.Kind, strings.Join(injectKinds, "|"))
			}
			continue
		}

		// Target: concrete or role. Owner = a concrete target of this addon.
		owner := false
		switch {
		case in.Target == "" && in.Kind == InjectKindWidget:
		case in.Target == "":
			addf("target is required (\"<addon>.<Model>\" or \"@<domain>.<entity>\")")
		default:
			if msg := injectTargetProblem(in.Target); msg != "" {
				addf("%s", msg)
			} else if mm := concreteTargetRe.FindStringSubmatch(in.Target); mm != nil && mm[1] == env.AddonKey {
				owner = true
				if _, ok := env.Cols[mm[2]]; !ok {
					addf("target %q names this addon but %q is not one of its models", in.Target, mm[2])
				}
			}
		}

		// Slot from the catalog, compatible with the kind.
		if in.Slot != "" {
			if !injectSlotKnown(in.Slot) {
				addf("slot %q is not in the %s catalog (%s)", in.Slot, SlotsCatalogVersion, strings.Join(injectSlotNames(), "|"))
			} else if !injectSlotAccepts(in.Slot, in.Kind) {
				addf("slot %q does not accept kind=%s (it accepts %s)", in.Slot, in.Kind, strings.Join(injectSlotKinds(in.Slot), "|"))
			}
		}

		// Order bands.
		if in.Order != nil {
			o := *in.Order
			switch {
			case o < 0:
				addf("order %d must not be negative", o)
			case owner && o > InjectOwnerOrderMax:
				addf("order %d is above %d: the owner of the target uses the 0-%d band, contributors from %d", o, InjectOwnerOrderMax, InjectOwnerOrderMax, InjectContributorOrderMin)
			case !owner && o < InjectContributorOrderMin:
				addf("order %d is below %d: orders 0-%d are reserved for the owner of the target model; a contributor uses %d or more", o, InjectContributorOrderMin, InjectOwnerOrderMax, InjectContributorOrderMin)
			}
		}

		if in.Permission != "" && !PermissionKeyRe.MatchString(in.Permission) {
			addf("permission %q must match %s", in.Permission, PermissionKeyRe)
		} else if in.Permission != "" && env.Permissions != nil && env.AddonKey != "" && strings.HasPrefix(in.Permission, env.AddonKey+".") {
			if _, ok := env.Permissions[in.Permission]; !ok {
				addf("permission %q is not declared in rbac.permissions", in.Permission)
			}
		}

		// Exactly the body the kind names.
		bodies := map[string]bool{
			"panel": in.Panel != nil, "column": in.Column != nil, "badge": in.Badge != nil,
			"filter": in.Filter != nil, "widget": in.Widget != nil,
		}
		want := in.Kind
		if want == InjectKindTab {
			want = "panel"
		}
		if !bodies[want] {
			addf("kind %q needs the %q block", in.Kind, want)
		}
		for _, name := range []string{"panel", "column", "badge", "filter", "widget"} {
			if bodies[name] && name != want {
				addf("declares %q but kind is %q; only the %q block is read", name, in.Kind, want)
			}
		}

		own := func(field, model string) map[string]struct{} {
			cols, ok := env.Cols[model]
			if !ok {
				addf("%s %q is not one of this addon's models (the foreign key must live on the contributor's model)", field, model)
			}
			return cols
		}
		col := func(field, model string, cols map[string]struct{}, name string) {
			if name == "" || cols == nil {
				return
			}
			if _, ok := cols[name]; ok || name == "id" || kernelOwnedColumns[name] {
				return
			}
			addf("%s %q is not a column of model %q", field, name, model)
		}

		switch want {
		case "panel":
			if in.Panel != nil {
				errs = append(errs, injectPanel(env, where+".panel", in, owner)...)
			}
		case "column":
			if c := in.Column; c != nil {
				if strings.TrimSpace(c.Label) == "" {
					addf("column.label is required (i18n key)")
				}
				cols := own("column.from", c.From)
				col("column.foreign_key", c.From, cols, c.ForeignKey)
				if c.ForeignKey == "" {
					addf("column.foreign_key is required")
				}
				if _, ok := injectPicks[c.Pick]; !ok {
					addf("column.pick %q is not one of latest|first|agg", c.Pick)
				}
				if _, ok := injectDisplays[c.Display]; !ok {
					addf("column.display %q is not one of text|badge|date|datetime|number|currency", c.Display)
				}
				switch c.Pick {
				case "latest", "first":
					if c.Field == "" {
						addf("column.field is required for pick=%s", c.Pick)
					}
					if c.Pick == "latest" && c.OrderBy == "" {
						addf("column.order_by is required for pick=latest (the From column that says which row is the latest)")
					}
					if c.Aggregate != "" {
						addf("column.aggregate is only valid with pick=agg")
					}
				case "agg":
					if _, ok := injectAggs[c.Aggregate]; !ok {
						addf("column.aggregate %q is not one of count|sum|avg|min|max (required for pick=agg)", c.Aggregate)
					} else if c.Aggregate != "count" && c.Field == "" {
						addf("column.field is required for pick=agg with aggregate=%s", c.Aggregate)
					}
					if c.OrderBy != "" {
						addf("column.order_by is only valid with pick=latest|first")
					}
				}
				col("column.field", c.From, cols, c.Field)
				col("column.order_by", c.From, cols, c.OrderBy)
				if len(c.Where) > maxInjectWhere {
					addf("column.where has %d predicates; at most %d", len(c.Where), maxInjectWhere)
				}
				for _, k := range sortedAnyKeys(c.Where) {
					col("column.where", c.From, cols, k)
					switch c.Where[k].(type) {
					case nil, string, bool, float64, int, int64:
					default:
						addf("column.where[%q] must be a string, number, boolean or null (equality only)", k)
					}
				}
			}
		case "badge":
			if b := in.Badge; b != nil {
				cols := own("badge.from", b.From)
				if b.ForeignKey == "" {
					addf("badge.foreign_key is required")
				}
				if b.Field == "" {
					addf("badge.field is required")
				}
				col("badge.foreign_key", b.From, cols, b.ForeignKey)
				col("badge.field", b.From, cols, b.Field)
				vals := make([]string, 0, len(b.Values))
				for v := range b.Values {
					vals = append(vals, v)
				}
				sort.Strings(vals)
				for _, v := range vals {
					if _, ok := injectTones[b.Values[v]]; !ok {
						addf("badge.values[%q] %q is not one of success|warning|danger|info|neutral", v, b.Values[v])
					}
				}
			}
		case "filter":
			if f := in.Filter; f != nil {
				if f.Column == "" {
					addf("filter.column is required (the key of a kind=column inject)")
				} else if ct, ok := columnTargets[f.Column]; !ok {
					addf("filter.column %q is not the key of a kind=column entry in contributions.inject[]", f.Column)
				} else if ct != in.Target {
					addf("filter.column %q targets %q but the filter targets %q: a filter sits on the list that shows its column", f.Column, ct, in.Target)
				}
			}
		case "widget":
			if in.Widget != nil && env.ValidateWidget != nil {
				errs = append(errs, env.ValidateWidget(where+".widget", *in.Widget)...)
			}
		}
	}

	if ep != nil {
		for i, s := range ep.Sealed {
			if _, ok := env.Cols[s]; !ok {
				errs = append(errs, fmt.Sprintf("extension_points.sealed[%d] %q is not one of this addon's models", i, s))
			}
		}
		for i, k := range ep.PrimaryFrom {
			if !addonKeyRe.MatchString(k) {
				errs = append(errs, fmt.Sprintf("extension_points.primary_from[%d] %q must be an addon key (%s)", i, k, addonKeyRe))
			}
		}
	}
	return errs
}

// injectPanel validates the DetailPanel body of a panel|tab inject. Panels
// aimed at this addon's own model get the full column-level checks of
// models[].detail; against a foreign or role target the columns belong to a
// model this manifest cannot see, so only the structure is checked (the hub
// resolves the rest against the owner's contract when publishing).
func injectPanel(env InjectEnv, where string, in Inject, owner bool) []string {
	p := *in.Panel
	model := ""
	if mm := concreteTargetRe.FindStringSubmatch(in.Target); owner && mm != nil {
		model = mm[2]
	}
	c := &uiChecker{
		lookup: func(key string) (modelShape, bool) {
			cols, ok := env.Cols[key]
			return modelShape{cols: cols, rels: map[string]string{}}, ok
		},
		federated: env.Federated,
	}
	if p.Kind != PanelKindRelation {
		c.panels(where, model, []DetailPanel{p})
		return stripPanelIndex(c.errs, where)
	}
	// relation: the source is relation.via ("Shipment.sales_order_id") on a
	// model of THIS addon; relation.name belongs to the owner's own relations.
	add := func(format string, a ...any) { c.addf(format, a...) }
	switch {
	case p.Key == "" || !panelKeyRe.MatchString(p.Key):
		add("%s.key %q must be snake_case", where, p.Key)
	}
	if strings.TrimSpace(p.Title) == "" {
		add("%s.title is required (i18n key of the panel heading)", where)
	}
	if _, ok := panelSpans[p.Span]; !ok {
		add("%s.span %q is not one of full|half", where, p.Span)
	}
	for name, present := range map[string]bool{"map": p.Map != nil, "timeline": p.Timeline != nil, "chart": p.Chart != nil, "stats": p.Stats != nil, "expose": p.Expose != ""} {
		if present {
			add("%s declares %s but kind is \"relation\"; only the block named like the kind is read", where, name)
		}
	}
	r := p.Relation
	if r == nil {
		add("%s is kind=relation and needs relation {via}", where)
		return c.errs
	}
	rw := where + ".relation"
	if r.Name != "" {
		add("%s.name is not valid in an inject panel: use via (\"<Model>.<foreign_key>\" of this addon)", rw)
	}
	if r.Limit < 0 || r.Limit > maxRelationRow {
		add("%s.limit %d is out of range 1..%d (0 = default 10)", rw, r.Limit, maxRelationRow)
	}
	mm := viaRe.FindStringSubmatch(r.Via)
	if mm == nil {
		add("%s.via %q must be \"<Model>.<foreign_key>\" of this addon (e.g. Shipment.sales_order_id)", rw, r.Via)
		return c.errs
	}
	cols, ok := env.Cols[mm[1]]
	if !ok {
		add("%s.via model %q is not one of this addon's models (the foreign key must live on the contributor's model)", rw, mm[1])
		return c.errs
	}
	if _, ok := cols[mm[2]]; !ok {
		add("%s.via column %q is not a column of model %q", rw, mm[2], mm[1])
	}
	for ci, name := range r.Columns {
		if _, ok := cols[name]; !ok && name != "id" && !kernelOwnedColumns[name] {
			add("%s.columns[%d] %q is not a column of model %q", rw, ci, name, mm[1])
		}
	}
	return c.errs
}

// stripPanelIndex rewrites the "<where>[0]" the shared panel checker emits for
// its one-element list into plain "<where>".
func stripPanelIndex(errs []string, where string) []string {
	for i, e := range errs {
		errs[i] = strings.ReplaceAll(e, where+"[0]", where)
	}
	return errs
}

func validInjectKind(k string) bool {
	for _, x := range injectKinds {
		if x == k {
			return true
		}
	}
	return false
}

func injectSlotKnown(name string) bool {
	for _, s := range injectSlots {
		if s.Name == name {
			return true
		}
	}
	return false
}

func injectSlotAccepts(name, kind string) bool {
	for _, k := range injectSlotKinds(name) {
		if k == kind {
			return true
		}
	}
	return false
}

func injectSlotKinds(name string) []string {
	for _, s := range injectSlots {
		if s.Name == name {
			return s.Kinds
		}
	}
	return nil
}

func injectSlotNames() []string {
	out := make([]string, len(injectSlots))
	for i, s := range injectSlots {
		out[i] = s.Name
	}
	return out
}

func sortedAnyKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
