package query

import (
	"errors"
	"testing"
)

func TestParseFromMap_Defaults(t *testing.T) {
	p, err := ParseFromMap(map[string][]string{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Page != DefaultPage {
		t.Errorf("Page = %d, want %d", p.Page, DefaultPage)
	}
	if p.PerPage != DefaultPerPage {
		t.Errorf("PerPage = %d, want %d", p.PerPage, DefaultPerPage)
	}
	if p.SortBy != "" {
		t.Errorf("SortBy = %q, want empty", p.SortBy)
	}
	if p.Order != "" {
		t.Errorf("Order = %q, want empty", p.Order)
	}
	if p.Search != "" {
		t.Errorf("Search = %q, want empty", p.Search)
	}
	if len(p.Filters) != 0 {
		t.Errorf("Filters = %v, want empty", p.Filters)
	}
}

func TestParseFromMap_AllFields(t *testing.T) {
	p, err := ParseFromMap(map[string][]string{
		"page":     {"3"},
		"per_page": {"50"},
		"sortBy":   {"created_at"},
		"order":    {"ASC"},
		"search":   {"widget"},
		"f_status": {"eq:active"},
		"f_tags":   {"in:a,b,c"},
		"f_amount": {"range:10|100"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Page != 3 {
		t.Errorf("Page = %d, want 3", p.Page)
	}
	if p.PerPage != 50 {
		t.Errorf("PerPage = %d, want 50", p.PerPage)
	}
	if p.SortBy != "created_at" {
		t.Errorf("SortBy = %q, want created_at", p.SortBy)
	}
	if p.Order != "asc" {
		t.Errorf("Order = %q, want asc", p.Order)
	}
	if p.Search != "widget" {
		t.Errorf("Search = %q, want widget", p.Search)
	}
	if f, ok := p.Filters["status"]; !ok || f.Op != OpEq || f.Value.(string) != "active" {
		t.Errorf("filter status wrong: %+v", f)
	}
	if f, ok := p.Filters["tags"]; !ok || f.Op != OpIn {
		t.Errorf("filter tags wrong: %+v", f)
	} else {
		vals := f.Value.([]string)
		if len(vals) != 3 || vals[0] != "a" || vals[2] != "c" {
			t.Errorf("tags values = %v", vals)
		}
	}
	if f, ok := p.Filters["amount"]; !ok || f.Op != OpRange {
		t.Errorf("filter amount wrong: %+v", f)
	} else {
		rng := f.Value.([2]string)
		if rng[0] != "10" || rng[1] != "100" {
			t.Errorf("range = %v", rng)
		}
	}
}

func TestParseFromMap_PerPageClamp(t *testing.T) {
	p, err := ParseFromMap(map[string][]string{"per_page": {"999999"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.PerPage != MaxPerPage {
		t.Errorf("PerPage = %d, want %d", p.PerPage, MaxPerPage)
	}
}

func TestParseFromMap_BadPerPage_FallsBackToDefault(t *testing.T) {
	p, err := ParseFromMap(map[string][]string{"per_page": {"abc"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.PerPage != DefaultPerPage {
		t.Errorf("PerPage = %d, want %d", p.PerPage, DefaultPerPage)
	}
}

func TestParseFromMap_BadPage_FallsBackToDefault(t *testing.T) {
	p, err := ParseFromMap(map[string][]string{"page": {"zzz"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Page != DefaultPage {
		t.Errorf("Page = %d, want %d", p.Page, DefaultPage)
	}
}

func TestParseFromMap_ZeroPage_UsesDefault(t *testing.T) {
	p, err := ParseFromMap(map[string][]string{"page": {"0"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Page != DefaultPage {
		t.Errorf("Page = %d, want %d (zero should fall back)", p.Page, DefaultPage)
	}
}

func TestParseFromMap_InvalidOrder_Dropped(t *testing.T) {
	p, err := ParseFromMap(map[string][]string{"order": {"sideways"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Order != "" {
		t.Errorf("Order = %q, want empty (invalid should be dropped)", p.Order)
	}
}

func TestParseFromMap_ShorthandFilter_DefaultsToEq(t *testing.T) {
	p, err := ParseFromMap(map[string][]string{"f_status": {"active"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	f := p.Filters["status"]
	if f.Op != OpEq || f.Value.(string) != "active" {
		t.Errorf("shorthand filter wrong: %+v", f)
	}
}

func TestParseFromMap_SearchTruncation(t *testing.T) {
	long := make([]byte, MaxSearchTermLength+50)
	for i := range long {
		long[i] = 'a'
	}
	p, err := ParseFromMap(map[string][]string{"search": {string(long)}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(p.Search) != MaxSearchTermLength {
		t.Errorf("Search len = %d, want %d", len(p.Search), MaxSearchTermLength)
	}
}

func TestParseFromMap_EmptyFilterKey_Ignored(t *testing.T) {
	p, err := ParseFromMap(map[string][]string{"f_": {"foo"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(p.Filters) != 0 {
		t.Errorf("Filters = %v, want empty", p.Filters)
	}
}

func TestParseFilterValue_OperatorParsing(t *testing.T) {
	cases := []struct {
		raw string
		op  FilterOp
	}{
		{"eq:foo", OpEq},
		{"ilike:foo", OpIlike},
		{"in:a,b", OpIn},
		{"gte:10", OpGte},
		{"lte:20", OpLte},
		{"range:1|2", OpRange},
		{"bogus:v", OpEq}, // unknown op → whole literal
		{"plain", OpEq},
	}
	for _, tc := range cases {
		f := parseFilterValue(tc.raw)
		if f.Op != tc.op {
			t.Errorf("parseFilterValue(%q).Op = %q, want %q", tc.raw, f.Op, tc.op)
		}
	}
}

func TestParseFilterValue_InTrimsAndDropsEmpty(t *testing.T) {
	f := parseFilterValue("in: a , ,b , c ")
	vals := f.Value.([]string)
	if len(vals) != 3 || vals[0] != "a" || vals[1] != "b" || vals[2] != "c" {
		t.Errorf("in values = %v", vals)
	}
}

func TestParseFilterValue_RangeOneSide(t *testing.T) {
	f := parseFilterValue("range:|100")
	rng := f.Value.([2]string)
	if rng[0] != "" || rng[1] != "100" {
		t.Errorf("range = %v", rng)
	}
}

func TestParseFromMap_ErrSentinel(t *testing.T) {
	// ErrInvalidParam is exported; we can't easily trigger it via
	// ParseFromMap today because page <1 falls back. Verify it still
	// matches errors.Is semantics for callers.
	wrapped := errors.New("wrapper: " + ErrInvalidParam.Error())
	if errors.Is(wrapped, ErrInvalidParam) {
		t.Errorf("should not match wrapped-by-string")
	}
	if !errors.Is(ErrInvalidParam, ErrInvalidParam) {
		t.Errorf("identity check failed")
	}
}
