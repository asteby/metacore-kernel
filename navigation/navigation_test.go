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

// TestBuildCarriesViewType verifies the v3 kanban hint (ViewType/GroupBy)
// survives aggregation + JSON marshaling. Two sibling entries on the SAME model
// that differ only by view_type (github's Board=kanban vs Issues=table) must
// reach the host carrying their view so it can encode a DISTINCT ?view= URL per
// entry — otherwise both collapse to the same /m/<model> href and the sidebar
// highlights both at once (the "both green" bug) and clicking Board never opens
// the kanban surface.
func TestBuildCarriesViewType(t *testing.T) {
	contribs := []Contribution{
		{
			AddonKey: "integration_github",
			Groups: []manifest.NavGroup{
				{
					Title: "integration_github.nav.group",
					Items: []manifest.NavItem{
						{Title: "Board", Model: "Issue", ViewType: "kanban", GroupBy: "stage"},
						{Title: "Issues", Model: "Issue", ViewType: "table"},
					},
				},
			},
		},
	}

	groups := Build(nil, contribs)
	if len(groups) != 1 || len(groups[0].Items) != 2 {
		t.Fatalf("expected 1 group / 2 items, got %d groups", len(groups))
	}
	board, issues := groups[0].Items[0], groups[0].Items[1]
	if board.ViewType != "kanban" || board.GroupBy != "stage" {
		t.Errorf("board = %+v, want view_type=kanban group_by=stage", board)
	}
	if issues.ViewType != "table" {
		t.Errorf("issues view_type = %q, want table", issues.ViewType)
	}

	// JSON wire shape must use snake_case keys the host reader expects, and omit
	// group_by when empty.
	b, _ := json.Marshal(board)
	var decoded map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["view_type"] != "kanban" {
		t.Errorf("expected JSON view_type=kanban, got %s", string(b))
	}
	if decoded["group_by"] != "stage" {
		t.Errorf("expected JSON group_by=stage, got %s", string(b))
	}
	bi, _ := json.Marshal(issues)
	var di map[string]any
	_ = json.Unmarshal(bi, &di)
	if _, present := di["group_by"]; present {
		t.Errorf("table item must omit group_by, got %s", string(bi))
	}
}
