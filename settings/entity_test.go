package settings_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/migrations"
	"github.com/asteby/metacore-kernel/settings"
)

// Entity layer (RFC 0001 §3.5): default < org < entity < branch (< market).
// A scope:"entity" key reads entity then org. A scope:"branch" key also
// INHERITS its entity's override, so an entity can set one value for all of its
// branches and a branch can still override it. Org-scoped keys ignore the
// entity layer entirely.

var entityDefs = []manifest.SettingDef{
	{Key: "mode", Type: "select", DefaultValue: "basic", Options: []manifest.Option{{Value: "basic"}, {Value: "pro"}}},
	{Key: "series", Type: "string", DefaultValue: "A", Scope: manifest.SettingScopeEntity},
	{Key: "printer", Type: "string", Scope: manifest.SettingScopeBranch},
}

func newEntityFixture(t *testing.T) (*settings.Store, *settings.Resolver, uuid.UUID) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	if err := (migrations.Runner{}).Up(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	src := settings.DefsFunc(func(context.Context, uuid.UUID, string) ([]manifest.SettingDef, error) { return entityDefs, nil })
	return settings.NewStore(db, src, nil), settings.NewResolver(db, src), uuid.New()
}

func TestEntityScopePrecedence(t *testing.T) {
	st, rs, org := newEntityFixture(t)
	ctx := context.Background()
	write := func(kind, id string, v settings.Values) {
		t.Helper()
		if _, err := st.Write(ctx, org, "acme", kind, id, v, nil, uuid.New()); err != nil {
			t.Fatalf("write %s/%s %v: %v", kind, id, v, err)
		}
	}
	entA, entB, branch := uuid.NewString(), uuid.NewString(), uuid.NewString()

	// Default, then org.
	eff, _, _ := rs.Resolve(ctx, org, "acme", settings.Scope{EntityID: entA})
	if eff.Values["series"] != "A" || eff.Sources["series"] != settings.SourceDefault {
		t.Fatalf("default: %v %v", eff.Values["series"], eff.Sources["series"])
	}
	write(settings.ScopeOrg, "", settings.Values{"series": "ORG"})

	// An entity override wins over the org value, for that entity only.
	write(settings.ScopeEntity, entA, settings.Values{"series": "SL"})
	eff, _, _ = rs.Resolve(ctx, org, "acme", settings.Scope{EntityID: entA})
	if eff.Values["series"] != "SL" || eff.Sources["series"] != settings.SourceEntity {
		t.Fatalf("entity A: %v %v", eff.Values["series"], eff.Sources["series"])
	}
	for _, sc := range []settings.Scope{{EntityID: entB}, {}} {
		eff, _, _ = rs.Resolve(ctx, org, "acme", sc)
		if eff.Values["series"] != "ORG" || eff.Sources["series"] != settings.SourceOrg {
			t.Fatalf("scope %+v must keep the org value (another entity's override never leaks): %v %v", sc, eff.Values["series"], eff.Sources["series"])
		}
	}

	// A branch-scoped key inherits the entity default; the branch can override it.
	write(settings.ScopeEntity, entA, settings.Values{"printer": "entity-printer"})
	eff, _, _ = rs.Resolve(ctx, org, "acme", settings.Scope{EntityID: entA, BranchID: branch})
	if eff.Values["printer"] != "entity-printer" || eff.Sources["printer"] != settings.SourceEntity {
		t.Fatalf("branch key must inherit its entity's value: %v %v", eff.Values["printer"], eff.Sources["printer"])
	}
	write(settings.ScopeBranch, branch, settings.Values{"printer": "branch-printer"})
	eff, _, _ = rs.Resolve(ctx, org, "acme", settings.Scope{EntityID: entA, BranchID: branch})
	if eff.Values["printer"] != "branch-printer" || eff.Sources["printer"] != settings.SourceBranch {
		t.Fatalf("branch override must win over its entity: %v %v", eff.Values["printer"], eff.Sources["printer"])
	}

	// Clearing the entity override restores the org value.
	write(settings.ScopeEntity, entA, settings.Values{"series": nil})
	eff, _, _ = rs.Resolve(ctx, org, "acme", settings.Scope{EntityID: entA})
	if eff.Values["series"] != "ORG" {
		t.Fatalf("after clear: %v", eff.Values["series"])
	}
}

func TestEntityWriteRules(t *testing.T) {
	st, _, org := newEntityFixture(t)
	ctx := context.Background()
	ent := uuid.NewString()

	// An org-scoped key has no entity layer.
	if _, err := st.Write(ctx, org, "acme", settings.ScopeEntity, ent, settings.Values{"mode": "pro"}, nil, uuid.New()); !errors.Is(err, settings.ErrNotEntityScoped) {
		t.Fatalf("org key in the entity layer: %v", err)
	}
	// Both an entity key and a branch key (an entity-wide default) may be written.
	for _, v := range []settings.Values{{"series": "X"}, {"printer": "p"}} {
		if _, err := st.Write(ctx, org, "acme", settings.ScopeEntity, ent, v, nil, uuid.New()); err != nil {
			t.Fatalf("entity write %v: %v", v, err)
		}
	}
	// The entity layer needs an id.
	if _, err := st.Write(ctx, org, "acme", settings.ScopeEntity, "", settings.Values{"series": "X"}, nil, uuid.New()); !errors.Is(err, settings.ErrInvalidScope) {
		t.Fatalf("entity needs an id: %v", err)
	}
	// An entity key is not a branch override.
	if _, err := st.Write(ctx, org, "acme", settings.ScopeBranch, uuid.NewString(), settings.Values{"series": "X"}, nil, uuid.New()); !errors.Is(err, settings.ErrNotBranchScoped) {
		t.Fatalf("entity key in the branch layer: %v", err)
	}
}

func TestWasmResolverReadsTheEntityFromTheContext(t *testing.T) {
	st, rs, org := newEntityFixture(t)
	ctx := context.Background()
	ent := uuid.NewString()
	if _, err := st.Write(ctx, org, "acme", settings.ScopeEntity, ent, settings.Values{"series": "SL"}, nil, uuid.New()); err != nil {
		t.Fatal(err)
	}
	w := settings.WasmResolver{R: rs}

	// Neither the entity id nor a resolved scope on ctx: the legacy resolution.
	got, err := w.ResolveSettings(ctx, org, "acme", "", map[string]string{})
	if err != nil || got["series"] != "A" {
		t.Fatalf("no entity on ctx must see the default, got %v %v", got["series"], err)
	}
	// The entity the HTTP layer asked for.
	got, _ = w.ResolveSettings(dynamic.WithEntityID(ctx, ent), org, "acme", "", map[string]string{})
	if got["series"] != "SL" {
		t.Fatalf("entity id on ctx: %v", got["series"])
	}
	// The scope the host resolved (what the wasm host stamps).
	got, _ = w.ResolveSettings(dynamic.WithEntityScope(ctx, dynamic.EntityScope{EntityID: ent, Restricted: true}), org, "acme", "", map[string]string{})
	if got["series"] != "SL" {
		t.Fatalf("resolved scope on ctx: %v", got["series"])
	}
}
