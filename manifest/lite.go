package manifest

import v3 "github.com/asteby/metacore-kernel/manifest/v3"

// Lite returns a shell-oriented copy of m: identity, frontend, navigation and
// a slim model_definitions index (model_key/table_name/label only). Heavy
// blocks (columns, actions, hooks, i18n, connectors, documents, …) are dropped
// so GET /metacore/manifests?lite=1 stays small with large fleets.
//
// The host shell needs the slim model index to resolve v3 nav `{model}` →
// `/m/<table>` and frontend.load policy; it does NOT need column schemas
// (those arrive via /metadata/table/:model on demand).
//
// frontend.pages[] rides along slimmed to what the sidebar and the router
// need before downloading the remote (key, path, expose, layout, title, icon,
// permission); nav entries keep view_type but drop the view block, which the
// host serves with the table metadata.
func Lite(m Manifest) Manifest {
	out := m
	out.Actions = nil
	out.Hooks = nil
	out.LifecycleHooks = nil
	out.Tools = nil
	out.Capabilities = nil
	out.Permissions = nil
	out.I18n = nil
	out.Settings = nil
	out.Extensions = nil
	out.Events = nil
	out.Signature = nil
	out.Routes = nil
	out.Connectors = nil
	out.Schedules = nil
	out.Webhooks = nil
	out.EdgeDevices = nil
	out.Documents = nil
	out.Screenshots = nil
	out.Features = nil
	out.Readme = ""
	out.MetadataI18n = nil

	if m.Frontend != nil {
		fe := *m.Frontend
		fe.Pages = litePages(m.Frontend.Pages)
		out.Frontend = &fe
	}
	out.Navigation = liteNavigation(m.Navigation)

	if m.Backend != nil {
		out.Backend = &BackendSpec{
			Runtime: m.Backend.Runtime,
			Entry:   m.Backend.Entry,
		}
	}

	if len(m.ModelDefinitions) > 0 {
		slim := make([]ModelDefinition, len(m.ModelDefinitions))
		for i, d := range m.ModelDefinitions {
			slim[i] = ModelDefinition{
				TableName:  d.TableName,
				ModelKey:   d.ModelKey,
				Label:      d.Label,
				OrgScoped:  d.OrgScoped,
				SoftDelete: d.SoftDelete,
			}
		}
		out.ModelDefinitions = slim
	}
	return out
}

// litePages keeps the routing/gating fields of each page; requires, shares,
// budget and breadcrumbs stay in the full manifest.
func litePages(in []v3.FrontendPage) []v3.FrontendPage {
	if len(in) == 0 {
		return nil
	}
	out := make([]v3.FrontendPage, len(in))
	for i, p := range in {
		out[i] = v3.FrontendPage{
			Key:        p.Key,
			Path:       p.Path,
			Expose:     p.Expose,
			Layout:     p.Layout,
			Title:      p.Title,
			Icon:       p.Icon,
			Permission: p.Permission,
		}
	}
	return out
}

// liteNavigation copies the nav tree without the declarative view blocks. It
// never mutates the input (Lite returns a copy).
func liteNavigation(in []NavGroup) []NavGroup {
	if in == nil {
		return nil
	}
	out := make([]NavGroup, len(in))
	for i, g := range in {
		g.Items = liteNavItems(g.Items)
		out[i] = g
	}
	return out
}

func liteNavItems(in []NavItem) []NavItem {
	if in == nil {
		return nil
	}
	out := make([]NavItem, len(in))
	for i, it := range in {
		it.View = nil
		it.Items = liteNavItems(it.Items)
		out[i] = it
	}
	return out
}

// LiteAll maps Lite over a slice.
func LiteAll(in []Manifest) []Manifest {
	if len(in) == 0 {
		return in
	}
	out := make([]Manifest, len(in))
	for i := range in {
		out[i] = Lite(in[i])
	}
	return out
}
