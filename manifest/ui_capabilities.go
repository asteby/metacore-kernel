package manifest

import (
	"fmt"
	"sort"
	"strings"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// Host capabilities of the UI surfaces (see docs/ui-pages-and-views.md). A host
// lists the ones it implements in HostCapabilityProfile.Capabilities; a
// manifest that uses the matching feature implies them, so an older host
// refuses the install instead of painting an empty table or never mounting a
// page.
const (
	// HostCapUIPages: the host routes frontend.pages[] (any layout).
	HostCapUIPages = "ui.pages"
	// HostCapUIEmbeddedPages: the host mounts layout "shell" pages inside its
	// chrome (/a/<addon>/<path>).
	HostCapUIEmbeddedPages = "ui.embedded_pages"
	// HostCapUIDetailPanels: the host renders models[].detail.panels and
	// split-view panels.
	HostCapUIDetailPanels = "ui.detail_panels"
	// HostCapUIMaps: the host provides the lazy maps share to remotes.
	HostCapUIMaps = "ui.maps"
	// HostCapUICharts: the host provides the lazy charts share to remotes.
	HostCapUICharts = "ui.charts"
	// HostCapUIViewPrefix prefixes the declarative view renderers:
	// "ui.views.map", "ui.views.timeline", "ui.views.calendar",
	// "ui.views.chart", "ui.views.split".
	HostCapUIViewPrefix = "ui.views."
)

// UIViewCapability returns the host capability that renders a declarative
// view type or a detail panel kind backed by one ("map" → "ui.views.map").
func UIViewCapability(viewType string) string { return HostCapUIViewPrefix + viewType }

// impliedHostCapabilities maps each host capability the manifest's content
// requires to the first reason found ("navigation view_type \"map\""). It
// never includes the declared HostCapabilities.
func (m *Manifest) impliedHostCapabilities() map[string]string {
	out := map[string]string{}
	add := func(cap, reason string) {
		if _, ok := out[cap]; !ok {
			out[cap] = reason
		}
	}
	panels := func(where string, ps []v3.DetailPanel) {
		if len(ps) == 0 {
			return
		}
		add(HostCapUIDetailPanels, where)
		for _, p := range ps {
			switch p.Kind {
			case v3.PanelKindMap, v3.PanelKindTimeline, v3.PanelKindChart:
				add(UIViewCapability(p.Kind), fmt.Sprintf("%s kind %q", where, p.Kind))
			}
		}
	}
	var walk func(items []NavItem)
	walk = func(items []NavItem) {
		for _, it := range items {
			if v3.IsDeclarativeView(it.ViewType) {
				add(UIViewCapability(it.ViewType), fmt.Sprintf("navigation view_type %q", it.ViewType))
			}
			if it.View != nil && it.View.Split != nil {
				panels("split view panels", it.View.Split.Panels)
			}
			walk(it.Items)
		}
	}
	for _, g := range m.Navigation {
		walk(g.Items)
	}
	for _, d := range m.ModelDefinitions {
		if d.Detail != nil {
			panels(fmt.Sprintf("model %s detail panels", d.ModelKey), d.Detail.Panels)
		}
	}
	if f := m.Frontend; f != nil {
		for _, p := range f.Pages {
			add(HostCapUIPages, "frontend.pages")
			if p.EffectiveLayout(f.Layout) == v3.PageLayoutShell {
				add(HostCapUIEmbeddedPages, fmt.Sprintf("frontend.pages %q (layout shell)", p.Key))
			}
			for _, sh := range p.Shares {
				if cap, ok := v3.PageShares[sh]; ok {
					add(cap, fmt.Sprintf("frontend.pages %q share %s", p.Key, sh))
				}
			}
		}
	}
	return out
}

// RequiredHostCapabilities returns every host capability the manifest needs:
// the declared HostCapabilities plus the ones implied by what it declares
// (declarative views, detail panels, pages, shares). Sorted, no duplicates.
func (m *Manifest) RequiredHostCapabilities() []string {
	set := map[string]struct{}{}
	for _, c := range m.HostCapabilities {
		set[c] = struct{}{}
	}
	for c := range m.impliedHostCapabilities() {
		set[c] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// CheckHostCapabilities returns an install-blocking error listing every
// required host capability (declared or implied) missing from caps, or nil.
// Installers with a configured capability set call it on install and upgrade.
func (m *Manifest) CheckHostCapabilities(caps []string) error {
	r := m.EvaluateCompatibility(HostCapabilityProfile{Capabilities: caps})
	var msgs []string
	for _, iss := range r.Issues {
		if iss.Kind == IssueMissingCapability {
			msgs = append(msgs, iss.Detail)
		}
	}
	if len(msgs) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(msgs, "; "))
}

// NavView returns the declarative view configuration of the first navigation
// entry for model with that view type, or nil. Hosts use it to serve the
// view block with the model's table metadata.
func (m *Manifest) NavView(model, viewType string) *v3.NavView {
	var find func(items []NavItem) *v3.NavView
	find = func(items []NavItem) *v3.NavView {
		for _, it := range items {
			if it.Model == model && it.ViewType == viewType && it.View != nil {
				return it.View
			}
			if v := find(it.Items); v != nil {
				return v
			}
		}
		return nil
	}
	for _, g := range m.Navigation {
		if v := find(g.Items); v != nil {
			return v
		}
	}
	return nil
}
