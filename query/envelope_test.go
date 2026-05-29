package query

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOpsMeta_Fields(t *testing.T) {
	b := New(testMeta())
	// total=53, page 2, perPage 15 → offset 15, from 16, to 30 (full page).
	m := b.OpsMeta(53, 15, Params{Page: 2, PerPage: 15})
	if m.Total != 53 {
		t.Errorf("total = %d", m.Total)
	}
	if m.CurrentPage != 2 || m.Page != 2 {
		t.Errorf("page/current_page = %d/%d", m.Page, m.CurrentPage)
	}
	if m.PerPage != 15 {
		t.Errorf("per_page = %d", m.PerPage)
	}
	if m.LastPage != 4 { // ceil(53/15)=4
		t.Errorf("last_page = %d, want 4", m.LastPage)
	}
	if m.From != 16 {
		t.Errorf("from = %d, want 16", m.From)
	}
	if m.To != 30 {
		t.Errorf("to = %d, want 30", m.To)
	}
}

func TestOpsMeta_ShortFinalPage(t *testing.T) {
	b := New(testMeta())
	// total=53, page 4, perPage 15 → offset 45, 8 rows → from 46, to 53.
	m := b.OpsMeta(53, 8, Params{Page: 4, PerPage: 15})
	if m.From != 46 || m.To != 53 {
		t.Errorf("from/to = %d/%d, want 46/53", m.From, m.To)
	}
}

func TestOpsMeta_EmptyResult(t *testing.T) {
	b := New(testMeta())
	m := b.OpsMeta(0, 0, Params{Page: 1, PerPage: 15})
	if m.From != 0 || m.To != 0 {
		t.Errorf("from/to should be 0 on empty: %d/%d", m.From, m.To)
	}
	if m.LastPage != 1 {
		t.Errorf("last_page on empty = %d, want 1", m.LastPage)
	}
}

func TestOpsMeta_UnknownRowCount(t *testing.T) {
	b := New(testMeta())
	// rowCount=0 but total>0 and offset<total → assume full page clamped.
	m := b.OpsMeta(10, 0, Params{Page: 1, PerPage: 15})
	if m.From != 1 || m.To != 10 {
		t.Errorf("from/to = %d/%d, want 1/10", m.From, m.To)
	}
}

// The DEFAULT PageMeta must serialise EXACTLY as before: no current_page,
// from, or to keys leak in when OpsMeta was not used.
func TestPageMeta_DefaultJSONUnchanged(t *testing.T) {
	b := New(testMeta())
	m := b.PageMeta(42, Params{Page: 2, PerPage: 15})
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, leaked := range []string{`"current_page"`, `"from"`, `"to"`} {
		if strings.Contains(s, leaked) {
			t.Errorf("default PageMeta JSON leaked %q: %s", leaked, s)
		}
	}
	want := `{"total":42,"page":2,"per_page":15,"last_page":3}`
	if s != want {
		t.Errorf("default PageMeta JSON = %s, want %s", s, want)
	}
}
