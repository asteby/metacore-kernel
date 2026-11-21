package dynamic

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
	"github.com/asteby/metacore-kernel/query"
)

func TestIsRelativeStorageKey(t *testing.T) {
	cases := map[string]bool{
		"products/a.jpg":         true,
		"a.png":                  true,
		"":                       false,
		"   ":                    false,
		"/storage/a.jpg":         false,
		"https://cdn.test/a.jpg": false,
		"data:image/png;base64,": false,
		"../secret":              false,
		"a/../../b":              false,
		`\win\a.jpg`:             false,
	}
	for in, want := range cases {
		if got := isRelativeStorageKey(in); got != want {
			t.Errorf("isRelativeStorageKey(%q) = %v, want %v", in, got, want)
		}
	}
}

type mediaThing struct {
	modelbase.BaseUUIDModel
	Name   string `json:"name"`
	Image  string `json:"image"`
	Cover  string `json:"cover"`
	Avatar string `json:"avatar"`
}

func (mediaThing) TableName() string { return "media_things" }
func (mediaThing) DefineTable() modelbase.TableMetadata {
	return modelbase.TableMetadata{
		Title: "Media things",
		Columns: []modelbase.ColumnDef{
			{Key: "name", Label: "Name"},
			// Opted in: relative keys resolve to URLs.
			{Key: "image", Label: "Image", CellStyle: "image", StyleConfig: map[string]any{StorageKeyConfigKey: true}},
			// Image column WITHOUT the opt-in: left untouched.
			{Key: "cover", Label: "Cover", CellStyle: "image"},
			// Opt-in on a non-file column is ignored.
			{Key: "avatar_note", Label: "Note", StyleConfig: map[string]any{StorageKeyConfigKey: true}},
			// Folder declared by the model: the resolver sees "avatars/<value>".
			{Key: "avatar", Label: "Avatar", CellStyle: "image", StyleConfig: map[string]any{StorageKeyConfigKey: true, StoragePrefixConfigKey: "/avatars/"}},
		},
	}
}
func (mediaThing) DefineModal() modelbase.ModalMetadata { return modelbase.ModalMetadata{Title: "m"} }

func TestListResolvesStorageKeyColumns(t *testing.T) {
	db := setupTestDB(t)
	db.Exec(`CREATE TABLE media_things (id TEXT PRIMARY KEY, organization_id TEXT, created_by_id TEXT,
		created_at DATETIME, updated_at DATETIME, deleted_at DATETIME, name TEXT, image TEXT, cover TEXT, avatar TEXT)`)
	modelbase.Register("media_things", func() modelbase.ModelDefiner { return &mediaThing{} })
	var seen []string
	svc := New(Config{
		AllowUnconfiguredPermissions: true, DB: db, Metadata: metadata.New(metadata.Config{CacheTTL: -1}),
		MediaURLResolver: func(_ context.Context, _ modelbase.AuthUser, key string) string {
			seen = append(seen, key)
			if key == "missing.jpg" {
				return ""
			}
			return "https://cdn.test/" + key
		},
	})
	user := newUser(uuid.New())
	ctx := context.Background()
	for _, in := range []map[string]any{
		{"name": "a", "image": "products/a.jpg", "cover": "products/c.jpg", "avatar": "me.png"},
		{"name": "b", "image": "https://other.test/b.jpg"},
		{"name": "c", "image": "/storage/c.jpg"},
		{"name": "d", "image": "missing.jpg"},
		{"name": "e", "image": ""},
	} {
		if _, err := svc.Create(ctx, "media_things", user, in); err != nil {
			t.Fatalf("create %v: %v", in["name"], err)
		}
	}
	rows, _, err := svc.List(ctx, "media_things", user, query.Params{Page: 1, PerPage: 50})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	covers := map[string]string{}
	avatars := map[string]string{}
	for _, r := range rows {
		got[r["name"].(string)], _ = r["image"].(string)
		covers[r["name"].(string)], _ = r["cover"].(string)
		avatars[r["name"].(string)], _ = r["avatar"].(string)
	}
	want := map[string]string{
		"a": "https://cdn.test/products/a.jpg", // resolved
		"b": "https://other.test/b.jpg",        // absolute: untouched
		"c": "/storage/c.jpg",                  // rooted: untouched
		"d": "missing.jpg",                     // resolver declined: raw value kept
		"e": "",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("row %s image = %q, want %q", k, got[k], v)
		}
	}
	if avatars["a"] != "https://cdn.test/avatars/me.png" {
		t.Errorf("storage_prefix: avatar = %q", avatars["a"])
	}
	if covers["a"] != "products/c.jpg" {
		t.Errorf("column without opt-in must stay raw, got %q", covers["a"])
	}
	for _, k := range seen {
		if !isRelativeStorageKey(k) {
			t.Errorf("resolver received non-relative value %q", k)
		}
	}

	// Get keeps the raw key so an edit form round-trips the stored value.
	var idA string
	for _, r := range rows {
		if r["name"] == "a" {
			idA, _ = r["id"].(string)
		}
	}
	one, err := svc.Get(ctx, "media_things", user, uuid.MustParse(idA))
	if err != nil {
		t.Fatal(err)
	}
	if one["image"] != "products/a.jpg" {
		t.Errorf("Get must return the raw key, got %v", one["image"])
	}
}
