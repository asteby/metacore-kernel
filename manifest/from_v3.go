package manifest

import (
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// kernelRequirementKey is the reserved compatibility.requires[] key whose
// version range the kernel reads as the legacy `Manifest.Kernel` field.
const kernelRequirementKey = "kernel"

// managedColumns are the physical columns the kernel's dynamic schema layer
// injects automatically for every addon table (see dynamic.CreateTable). A v3
// model declares these explicitly to document its full physical shape, but the
// legacy ModelDefinition path must NOT re-declare them or CreateTable would
// emit duplicate column definitions ("id" twice, etc.). We strip them during
// mapping and instead translate their presence into the legacy flags
// (organization_id → OrgScoped, deleted_at → SoftDelete).
var managedColumns = map[string]struct{}{
	"id":         {},
	"created_at": {},
	"updated_at": {},
}

// FromV3 maps a validated v3 manifest into the legacy manifest.Manifest shape
// so the ~14 existing kernel consumers (installer, lifecycle, bridge, dynamic
// hooks, host, httpx/metacore, …) keep working unchanged during the 3.x
// dual-read window. It is intentionally conservative: fields with a clean v3
// equivalent are populated; fields with no faithful mapping (or that no kernel
// consumer reads) are left zero-valued rather than fabricated.
//
// The direction is strictly one-way: the manifest package imports manifest/v3,
// never the reverse, so the v3 contract stays free of legacy coupling.
func FromV3(m *v3.Manifest) Manifest {
	if m == nil {
		return Manifest{}
	}

	out := Manifest{
		Key:         m.Metadata.Key,
		Name:        m.Metadata.Name,
		Description: m.Metadata.Description,
		Version:     m.Metadata.Version,
		Category:    m.Metadata.Category,
		Author:      m.Metadata.Author,
		Website:     m.Metadata.Website,
		License:     m.Metadata.License,
		Readme:      m.Metadata.Readme,
		Screenshots: m.Metadata.Screenshots,
		Features:    m.Metadata.Features,
	}

	// Icon triplet. v3 carries the structured {type, slug, color}; the legacy
	// single-string Icon stays empty (consumers prefer the triplet).
	if m.Metadata.Icon != nil {
		out.IconType = m.Metadata.Icon.Type
		out.IconSlug = m.Metadata.Icon.Slug
		out.IconColor = m.Metadata.Icon.Color
	}

	// Kernel semver range lives under compatibility.requires[key=="kernel"].
	for _, r := range m.Compatibility.Requires {
		if r.Key == kernelRequirementKey {
			out.Kernel = r.Version
			break
		}
	}

	// Tenancy → TenantIsolation. nil tenancy maps to "" which the kernel and
	// dynamic.ParseIsolation both treat as the "shared" default.
	if m.Tenancy != nil {
		out.TenantIsolation = m.Tenancy.Isolation
	}

	out.Capabilities = mapCapabilities(m.Capabilities)
	out.ModelDefinitions = mapModels(m.Models)
	out.Navigation = mapNavigation(m)
	out.Settings = mapSettings(m.Settings)
	out.Actions = mapActions(m)
	out.Tools = mapTools(m)
	out.Events = mapEvents(m)
	out.LifecycleHooks = mapLifecycle(m.Lifecycle)
	out.I18n = mapI18n(m.I18n)
	out.Signature = mapSignature(m.Signature)

	return out
}

// mapCapabilities is a 1:1 field copy — both shapes are {Kind, Target, Reason}.
func mapCapabilities(in []v3.Capability) []Capability {
	if len(in) == 0 {
		return nil
	}
	out := make([]Capability, 0, len(in))
	for _, c := range in {
		out = append(out, Capability{Kind: c.Kind, Target: c.Target, Reason: c.Reason})
	}
	return out
}

// mapModels translates v3 Models into legacy ModelDefinitions. The kernel's
// dynamic.CreateTable auto-injects id/organization_id/created_at/updated_at/
// deleted_at, so those columns are stripped here and surfaced as OrgScoped /
// SoftDelete flags instead. v3 ForeignKeys do not map cleanly onto legacy
// owner-rooted RelationDef shapes (one_to_many / many_to_many), so Relations
// is left empty (flat tables) rather than guessed.
func mapModels(in []v3.Model) []ModelDefinition {
	if len(in) == 0 {
		return nil
	}
	out := make([]ModelDefinition, 0, len(in))
	for _, m := range in {
		def := ModelDefinition{
			TableName: m.Table,
			ModelKey:  m.Key,
			Label:     m.Label,
		}
		// Index of single-column index declarations so we can fold the
		// unique/index hint back onto the matching ColumnDef.
		uniqueCols := map[string]struct{}{}
		indexCols := map[string]struct{}{}
		for _, idx := range m.Indices {
			if len(idx.Columns) != 1 {
				continue // composite indices have no ColumnDef equivalent.
			}
			if idx.Unique {
				uniqueCols[idx.Columns[0]] = struct{}{}
			} else {
				indexCols[idx.Columns[0]] = struct{}{}
			}
		}
		for _, c := range m.Columns {
			switch c.Name {
			case "organization_id":
				def.OrgScoped = true
				continue
			case "deleted_at":
				def.SoftDelete = true
				continue
			}
			if _, managed := managedColumns[c.Name]; managed {
				continue
			}
			col := ColumnDef{
				Name:     c.Name,
				Type:     c.Type,
				Required: c.NotNull,
				Default:  c.Default,
			}
			if _, ok := uniqueCols[c.Name]; ok {
				col.Unique = true
			}
			if _, ok := indexCols[c.Name]; ok {
				col.Index = true
			}
			def.Columns = append(def.Columns, col)
		}
		out = append(out, def)
	}
	return out
}

// mapNavigation copies contributions.navigation field-by-field. NavGroup and
// NavItem have the same shape in both contracts.
func mapNavigation(m *v3.Manifest) []NavGroup {
	if m.Contributions == nil || len(m.Contributions.Navigation) == 0 {
		return nil
	}
	out := make([]NavGroup, 0, len(m.Contributions.Navigation))
	for _, g := range m.Contributions.Navigation {
		out = append(out, NavGroup{
			Title:  g.Title,
			Icon:   g.Icon,
			Target: g.Target,
			Items:  mapNavItems(g.Items),
		})
	}
	return out
}

func mapNavItems(in []v3.NavItem) []NavItem {
	if len(in) == 0 {
		return nil
	}
	out := make([]NavItem, 0, len(in))
	for _, it := range in {
		out = append(out, NavItem{
			Title:      it.Title,
			URL:        it.URL,
			Icon:       it.Icon,
			Model:      it.Model,
			Permission: it.Permission,
			Items:      mapNavItems(it.Items),
		})
	}
	return out
}

// mapSettings maps v3 settings into legacy SettingDef. v3 {key,type,label,
// default,options} → legacy {Key,Type,Label,DefaultValue,Options}. v3 has no
// Secret flag, so legacy Secret stays false.
func mapSettings(in []v3.Setting) []SettingDef {
	if len(in) == 0 {
		return nil
	}
	out := make([]SettingDef, 0, len(in))
	for _, s := range in {
		def := SettingDef{
			Key:          s.Key,
			Label:        s.Label,
			Type:         s.Type,
			DefaultValue: s.Default,
		}
		for _, o := range s.Options {
			def.Options = append(def.Options, Option{Value: o.Value, Label: o.Label})
		}
		out = append(out, def)
	}
	return out
}

// mapActions projects contributions.actions into the legacy Actions map keyed
// by target model (the same "<model>::<action>" addressing the bridge expects).
// Actions with no target model are bucketed under "" so they still register.
func mapActions(m *v3.Manifest) map[string][]ActionDef {
	if m.Contributions == nil || len(m.Contributions.Actions) == 0 {
		return nil
	}
	out := map[string][]ActionDef{}
	for _, a := range m.Contributions.Actions {
		def := ActionDef{
			Key:   a.Key,
			Name:  a.Key,
			Label: a.Label,
		}
		switch a.Handler.Type {
		case "wasm":
			def.Trigger = &ActionTrigger{Type: "wasm", Export: a.Handler.Function}
		case "webhook":
			def.Trigger = &ActionTrigger{Type: "webhook"}
		}
		out[a.TargetModel] = append(out[a.TargetModel], def)
	}
	return out
}

// mapTools projects contributions.tools into legacy ToolDef. v3 tools are leaner
// ({key, description, input_schema, handler}) than the rich legacy ToolDef; only
// the fields with a clean equivalent are populated.
func mapTools(m *v3.Manifest) []ToolDef {
	if m.Contributions == nil || len(m.Contributions.Tools) == 0 {
		return nil
	}
	out := make([]ToolDef, 0, len(m.Contributions.Tools))
	for _, t := range m.Contributions.Tools {
		td := ToolDef{
			ID:          t.Key,
			Name:        t.Key,
			Description: t.Description,
		}
		if t.Handler.Type == "webhook" {
			td.Endpoint = t.Handler.URL
		}
		out = append(out, td)
	}
	return out
}

// mapEvents derives the legacy Events list from the events this addon publishes
// (extension_points.events) plus its event:emit capabilities. Both are sources
// of truth in v3; the union keeps the legacy advisory drift-check happy.
func mapEvents(m *v3.Manifest) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(name string) {
		if name == "" {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	if m.ExtensionPoints != nil {
		for _, e := range m.ExtensionPoints.Events {
			add(e.Name)
		}
	}
	for _, c := range m.Capabilities {
		if c.Kind == "event:emit" {
			add(c.Target)
		}
	}
	return out
}

// mapLifecycle translates the v3 Lifecycle block into the legacy
// LifecycleHooks map. v3 declares a single handler function name per
// transition (install/uninstall/enable/disable) dispatched against the addon's
// wasm module; the legacy map keys those transitions to a HookDef list with a
// wasm HookTarget. The v3 upgrade ladder (UpgradeStep[]) has no single-target
// legacy equivalent and is left unmapped (the installer's upgrade path reads
// migrations, not these hooks).
func mapLifecycle(lc *v3.Lifecycle) map[string][]HookDef {
	if lc == nil {
		return nil
	}
	out := map[string][]HookDef{}
	add := func(event, fn string) {
		if fn == "" {
			return
		}
		out[event] = append(out[event], HookDef{
			Event:  event,
			Target: HookTarget{Type: "wasm", Function: fn},
		})
	}
	add("install", lc.Install)
	add("uninstall", lc.Uninstall)
	add("enable", lc.Enable)
	add("disable", lc.Disable)
	if len(out) == 0 {
		return nil
	}
	return out
}

// mapI18n maps the v3 i18n bundle pointer into the legacy lang-keyed map. The
// v3 bundles reference file PATHS only — the actual translation strings live in
// the bundle files and are loaded at runtime, NOT carried in the manifest. So
// we produce a map keyed by locale with empty inner maps purely so length /
// presence checks on Manifest.I18n behave, without fabricating any translation
// content.
func mapI18n(i *v3.I18n) map[string]map[string]string {
	if i == nil || len(i.Bundles) == 0 {
		return nil
	}
	out := make(map[string]map[string]string, len(i.Bundles))
	for _, b := range i.Bundles {
		if b.Locale == "" {
			continue
		}
		out[b.Locale] = map[string]string{}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// mapSignature maps the v3 detached-signature block into the legacy Signature.
// The shapes overlap only on Algorithm / Value / SignedAt; v3 has no developer
// identity / verified / digest / per-entry checksums, so those legacy fields
// stay zero (the security package treats an absent/empty Value as "unsigned").
func mapSignature(s *v3.Signature) *Signature {
	if s == nil {
		return nil
	}
	return &Signature{
		Algorithm: s.Algorithm,
		Value:     s.Value,
		SignedAt:  s.SignedAt,
	}
}
