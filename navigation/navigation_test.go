package navigation

import (
	"encoding/json"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
)

// TestBuildCarriesFilter verifies that a static per-status Filter declared on a
// manifest NavItem survives the aggregation into the serialized navigation tree
// and through JSON marshaling. Without this, an addon that publishes one nav
// entry per status (all pointing at the same model) collapses to identical URLs
// host-side and every entry highlights at once ("multi-active" sidebar bug).
func TestBuildCarriesFilter(t *testing.T) {
	contribs := []Contribution{
		{
			AddonKey: "workshop",
			Groups: []manifest.NavGroup{
				{
					Title: "workshop.nav.group",
					Items: []manifest.NavItem{
						{Title: "workshop.nav.orders", Model: "WorkOrder"},
						{Title: "workshop.nav.open", Model: "WorkOrder", Filter: map[string]string{"status": "open"}},
						{
							Title: "workshop.nav.parent",
							Items: []manifest.NavItem{
								{Title: "workshop.nav.completed", Model: "WorkOrder", Filter: map[string]string{"status": "completed"}},
							},
						},
					},
				},
			},
		},
	}

	groups := Build(nil, contribs)
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	items := groups[0].Items
	if len(items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(items))
	}

	// Unfiltered entry keeps an empty filter.
	if items[0].Filter != nil {
		t.Errorf("unfiltered item should have nil Filter, got %v", items[0].Filter)
	}
	// Filtered entry carries its status verbatim.
	if got := items[1].Filter["status"]; got != "open" {
		t.Errorf("expected Filter[status]=open, got %q", got)
	}
	// Nested children carry the filter too.
	if got := items[2].Items[0].Filter["status"]; got != "completed" {
		t.Errorf("expected nested Filter[status]=completed, got %q", got)
	}

	// JSON round-trip: the wire shape the frontend consumes must include
	// "filter" only when present (omitempty), so the host can deep-link a
	// distinct ?f_<col>=eq:<val> URL per entry.
	b, err := json.Marshal(items[1])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	filter, ok := decoded["filter"].(map[string]any)
	if !ok {
		t.Fatalf("expected filter object in JSON, got %s", string(b))
	}
	if filter["status"] != "open" {
		t.Errorf("expected JSON filter.status=open, got %v", filter["status"])
	}

	b0, _ := json.Marshal(items[0])
	var decoded0 map[string]any
	_ = json.Unmarshal(b0, &decoded0)
	if _, present := decoded0["filter"]; present {
		t.Errorf("unfiltered item must omit filter key, got %s", string(b0))
	}
}
