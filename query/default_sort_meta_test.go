package query

import (
	"strings"
	"testing"
)

// TableMetadata.DefaultSortBy overrides the newest-first timestamp fallback,
// but never an explicit, whitelisted sortBy, and never a column outside the
// whitelist.
func TestApply_MetaDefaultSort(t *testing.T) {
	cases := []struct {
		name          string
		sortBy, order string
		params        Params
		want          string
	}{
		{"declared asc", "name", "asc", Params{}, "ORDER BY name asc"},
		{"declared, order defaults to desc", "amount", "", Params{}, "ORDER BY amount desc"},
		{"unknown sortBy falls back to declared", "name", "asc", Params{SortBy: "evil_col"}, "ORDER BY name asc"},
		{"explicit sortBy wins", "name", "asc", Params{SortBy: "amount", Order: "asc"}, "ORDER BY amount asc"},
		{"undeclared column ignored", "secret", "asc", Params{}, "ORDER BY created_at desc"},
		{"unsafe ident ignored", "name;drop", "asc", Params{}, "ORDER BY created_at desc"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			meta := testMeta()
			meta.DefaultSortBy, meta.DefaultSortOrder = c.sortBy, c.order
			db := openDryDB(t)
			sql := renderSQL(t, New(meta).Apply(db.Model(&testRow{}), c.params))
			if !strings.Contains(sql, c.want) {
				t.Errorf("want %q, got %q", c.want, sql)
			}
		})
	}
}
