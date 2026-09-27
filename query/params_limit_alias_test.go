package query

import "testing"

// POS and addon clients send `limit` as the page size. Both parsers ignored it
// and answered DefaultPerPage rows (a treasury total summed 15 payments).
func TestLimitIsPerPageAlias(t *testing.T) {
	parsers := map[string]func(map[string][]string) (Params, error){
		"canonical": ParseFromMap,
		"ops":       ParseOpsFromValues,
	}
	for name, parse := range parsers {
		p, err := parse(map[string][]string{"limit": {"120"}})
		if err != nil || p.PerPage != 120 {
			t.Errorf("%s: limit=120 → per_page %d (%v), want 120", name, p.PerPage, err)
		}
		p, _ = parse(map[string][]string{"limit": {"5000"}})
		if p.PerPage != MaxPerPage {
			t.Errorf("%s: limit=5000 → per_page %d, want the %d cap", name, p.PerPage, MaxPerPage)
		}
		p, _ = parse(map[string][]string{"per_page": {"30"}, "limit": {"120"}})
		if p.PerPage != 30 {
			t.Errorf("%s: per_page wins over limit, got %d", name, p.PerPage)
		}
		if _, isFilter := p.Filters["limit"]; isFilter {
			t.Errorf("%s: limit must not become a column filter", name)
		}
	}
}
