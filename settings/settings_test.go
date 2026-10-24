package settings_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/migrations"
	"github.com/asteby/metacore-kernel/settings"
)

var defs = []manifest.SettingDef{
	{Key: "mode", Type: "select", DefaultValue: "basic", Options: []manifest.Option{{Value: "basic"}, {Value: "pro"}}},
	{Key: "limit", Type: "integer", DefaultValue: float64(10), Scope: manifest.SettingScopeBranch},
	{Key: "printer", Type: "string", Scope: manifest.SettingScopeBranch},
	{Key: "enabled", Type: "boolean"},
	{Key: "token", Type: "secret"},
	{Key: "api_key", Type: "string", WriteOnly: true},
	{Key: "legacy_secret", Type: "string", Secret: true},
}

type fixture struct {
	db  *gorm.DB
	st  *settings.Store
	rs  *settings.Resolver
	org uuid.UUID
	pub *recorder
}

type recorder struct {
	mu     sync.Mutex
	events []settings.Changed
}

func (r *recorder) Publish(_ context.Context, emitter, event string, _ uuid.UUID, p any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if emitter != "kernel" || event != settings.EventSettingsChanged {
		panic("unexpected emitter/event")
	}
	r.events = append(r.events, p.(settings.Changed))
	return nil
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	// The real migration, so the SQL and the gorm model cannot drift.
	if err := (migrations.Runner{}).Up(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	src := settings.DefsFunc(func(context.Context, uuid.UUID, string) ([]manifest.SettingDef, error) { return defs, nil })
	pub := &recorder{}
	return &fixture{db: db, st: settings.NewStore(db, src, pub), rs: settings.NewResolver(db, src), org: uuid.New(), pub: pub}
}

func (f *fixture) write(t *testing.T, kind, id string, patch settings.Values, rev *int64) int64 {
	t.Helper()
	r, err := f.st.Write(context.Background(), f.org, "acme", kind, id, patch, rev, uuid.New())
	if err != nil {
		t.Fatalf("write %s/%s %v: %v", kind, id, patch, err)
	}
	return r
}

func TestPrecedence_DefaultOrgBranch(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	branch := uuid.NewString()

	eff, _, err := f.rs.Resolve(ctx, f.org, "acme", settings.Scope{BranchID: branch})
	if err != nil {
		t.Fatal(err)
	}
	if eff.Values["limit"] != float64(10) || eff.Sources["limit"] != settings.SourceDefault || eff.Values["mode"] != "basic" {
		t.Fatalf("defaults: %+v", eff)
	}
	if _, ok := eff.Values["printer"]; ok {
		t.Fatal("key without default or value must be absent")
	}

	f.write(t, settings.ScopeOrg, "", settings.Values{"limit": 20, "mode": "pro"}, nil)
	eff, _, _ = f.rs.Resolve(ctx, f.org, "acme", settings.Scope{BranchID: branch})
	if eff.Values["limit"] != float64(20) || eff.Sources["limit"] != settings.SourceOrg || eff.Values["mode"] != "pro" {
		t.Fatalf("org over default: %+v", eff)
	}

	f.write(t, settings.ScopeBranch, branch, settings.Values{"limit": 5, "printer": "front"}, nil)
	eff, rev, _ := f.rs.Resolve(ctx, f.org, "acme", settings.Scope{BranchID: branch})
	if eff.Values["limit"] != float64(5) || eff.Sources["limit"] != settings.SourceBranch || eff.Values["printer"] != "front" {
		t.Fatalf("branch over org: %+v", eff)
	}
	if rev != eff.OrgRev+eff.BranchRev || rev != 2 {
		t.Fatalf("rev = %d (%+v)", rev, eff)
	}

	// Another branch and "no branch" keep seeing the org value.
	other, _, _ := f.rs.Resolve(ctx, f.org, "acme", settings.Scope{BranchID: uuid.NewString()})
	none, _, _ := f.rs.Resolve(ctx, f.org, "acme", settings.Scope{})
	if other.Values["limit"] != float64(20) || none.Values["limit"] != float64(20) {
		t.Fatalf("other=%v none=%v", other.Values, none.Values)
	}

	// Clearing the override (nil) falls back to the org value.
	f.write(t, settings.ScopeBranch, branch, settings.Values{"limit": nil}, nil)
	eff, _, _ = f.rs.Resolve(ctx, f.org, "acme", settings.Scope{BranchID: branch})
	if eff.Values["limit"] != float64(20) {
		t.Fatalf("after clear: %v", eff.Values)
	}
}

func TestDefaultsAreNeverMaterialized(t *testing.T) {
	f := newFixture(t)
	if _, _, err := f.rs.Resolve(context.Background(), f.org, "acme", settings.Scope{}); err != nil {
		t.Fatal(err)
	}
	f.write(t, settings.ScopeOrg, "", settings.Values{"enabled": true}, nil)
	vals, _, _ := f.st.Read(context.Background(), f.org, "acme", settings.ScopeOrg, "")
	if len(vals) != 1 || vals["enabled"] != true {
		t.Fatalf("stored: %v", vals)
	}
	var n int64
	f.db.Model(&settings.Row{}).Count(&n)
	if n != 1 {
		t.Fatalf("rows = %d", n)
	}
}

func TestUndeclaredBranchKeyIgnoredByResolver(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	branch := uuid.NewString()
	// "mode" is org-scoped; a branch row smuggling it (or an unknown key)
	// directly in the table must not affect the result.
	f.db.Create(&settings.Row{OrgID: f.org, AddonKey: "acme", ScopeKind: "branch", ScopeID: branch,
		Values: settings.Values{"mode": "pro", "ghost": "x"}, Rev: 1})
	eff, _, err := f.rs.Resolve(ctx, f.org, "acme", settings.Scope{BranchID: branch})
	if err != nil {
		t.Fatal(err)
	}
	if eff.Values["mode"] != "basic" {
		t.Fatalf("org-scoped key overridden by branch: %v", eff.Values)
	}
	if _, ok := eff.Values["ghost"]; ok {
		t.Fatal("unknown key leaked")
	}
}

func TestWriteRejectsBranchWriteOfOrgScopedKeyAndUnknown(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, err := f.st.Write(ctx, f.org, "acme", settings.ScopeBranch, "b1", settings.Values{"mode": "pro"}, nil, uuid.Nil)
	if !errors.Is(err, settings.ErrNotBranchScoped) {
		t.Fatalf("err = %v", err)
	}
	_, err = f.st.Write(ctx, f.org, "acme", settings.ScopeOrg, "", settings.Values{"nope": 1}, nil, uuid.Nil)
	if !errors.Is(err, settings.ErrUnknownKey) {
		t.Fatalf("err = %v", err)
	}
	_, err = f.st.Write(ctx, f.org, "acme", settings.ScopeBranch, "", settings.Values{"limit": 1}, nil, uuid.Nil)
	if !errors.Is(err, settings.ErrInvalidScope) {
		t.Fatalf("err = %v", err)
	}
	_, err = f.st.Write(ctx, f.org, "acme", settings.ScopeOrg, "x", settings.Values{"mode": "pro"}, nil, uuid.Nil)
	if !errors.Is(err, settings.ErrInvalidScope) {
		t.Fatalf("err = %v", err)
	}
}

func TestRevisionAndConflict(t *testing.T) {
	f := newFixture(t)
	zero := int64(0)
	r1 := f.write(t, settings.ScopeOrg, "", settings.Values{"enabled": true}, &zero)
	if r1 != 1 {
		t.Fatalf("r1 = %d", r1)
	}
	// Creating again as "no row yet" conflicts.
	if _, err := f.st.Write(context.Background(), f.org, "acme", settings.ScopeOrg, "", settings.Values{"enabled": false}, &zero, uuid.Nil); !errors.Is(err, settings.ErrConflict) {
		t.Fatalf("err = %v", err)
	}
	r2 := f.write(t, settings.ScopeOrg, "", settings.Values{"enabled": false}, &r1)
	if r2 != 2 {
		t.Fatalf("r2 = %d", r2)
	}
	// Stale etag.
	if _, err := f.st.Write(context.Background(), f.org, "acme", settings.ScopeOrg, "", settings.Values{"enabled": true}, &r1, uuid.Nil); !errors.Is(err, settings.ErrConflict) {
		t.Fatalf("stale err = %v", err)
	}
	vals, rev, _ := f.st.Read(context.Background(), f.org, "acme", settings.ScopeOrg, "")
	if rev != 2 || vals["enabled"] != false {
		t.Fatalf("state changed by a rejected write: %v rev=%d", vals, rev)
	}
}

func TestConcurrentWritersWithoutEtagAllLand(t *testing.T) {
	f := newFixture(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = f.st.Write(context.Background(), f.org, "acme", settings.ScopeOrg, "", settings.Values{"limit": i}, nil, uuid.Nil)
		}(i)
	}
	wg.Wait()
	_, rev, _ := f.st.Read(context.Background(), f.org, "acme", settings.ScopeOrg, "")
	if rev < 1 {
		t.Fatalf("rev = %d", rev)
	}
}

func TestSecretsAndReservedKeysRejected(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, k := range []string{"token", "api_key", "legacy_secret"} {
		_, err := f.st.Write(ctx, f.org, "acme", settings.ScopeOrg, "", settings.Values{k: "s3cr3t"}, nil, uuid.Nil)
		if !errors.Is(err, settings.ErrSecretKey) {
			t.Fatalf("%s: err = %v", k, err)
		}
		if err != nil && !contains(err.Error(), "org_integrations") {
			t.Fatalf("error must point to org_integrations: %v", err)
		}
	}
	for _, k := range []string{"_capability_defaults", "_anything"} {
		_, err := f.st.Write(ctx, f.org, "acme", settings.ScopeOrg, "", settings.Values{k: 1}, nil, uuid.Nil)
		if !errors.Is(err, settings.ErrReservedKey) {
			t.Fatalf("%s: err = %v", k, err)
		}
	}
	// A bad key voids the whole patch.
	_, err := f.st.Write(ctx, f.org, "acme", settings.ScopeOrg, "", settings.Values{"enabled": true, "token": "x"}, nil, uuid.Nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if vals, rev, _ := f.st.Read(ctx, f.org, "acme", settings.ScopeOrg, ""); rev != 0 || len(vals) != 0 {
		t.Fatalf("partial write: %v rev=%d", vals, rev)
	}
}

func TestResolverNeverExposesSecretsEvenIfStored(t *testing.T) {
	f := newFixture(t)
	f.db.Create(&settings.Row{OrgID: f.org, AddonKey: "acme", ScopeKind: "org", Values: settings.Values{"token": "leak"}, Rev: 1})
	eff, _, _ := f.rs.Resolve(context.Background(), f.org, "acme", settings.Scope{})
	if _, ok := eff.Values["token"]; ok {
		t.Fatal("secret leaked through Resolve")
	}
}

func TestValueTypeValidation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	bad := []settings.Values{
		{"mode": "ultra"}, {"mode": 3}, {"limit": "ten"}, {"limit": 1.5}, {"enabled": "yes"},
	}
	for _, p := range bad {
		if _, err := f.st.Write(ctx, f.org, "acme", settings.ScopeOrg, "", p, nil, uuid.Nil); !errors.Is(err, settings.ErrInvalidValue) {
			t.Fatalf("%v: err = %v", p, err)
		}
	}
}

func TestIdempotentWriteNoRevBumpNoEvent(t *testing.T) {
	f := newFixture(t)
	r1 := f.write(t, settings.ScopeOrg, "", settings.Values{"limit": 7}, nil)
	n := len(f.pub.events)
	r2 := f.write(t, settings.ScopeOrg, "", settings.Values{"limit": 7}, nil)
	r3 := f.write(t, settings.ScopeOrg, "", settings.Values{"limit": float64(7)}, nil)
	if r1 != 1 || r2 != 1 || r3 != 1 {
		t.Fatalf("revs %d %d %d", r1, r2, r3)
	}
	if len(f.pub.events) != n {
		t.Fatalf("idempotent writes published %d extra events", len(f.pub.events)-n)
	}
	// Clearing a key that was never set creates nothing.
	if r := f.write(t, settings.ScopeBranch, "b9", settings.Values{"printer": nil}, nil); r != 0 {
		t.Fatalf("rev = %d", r)
	}
}

func TestChangedEventCarriesKeysNotValues(t *testing.T) {
	f := newFixture(t)
	f.write(t, settings.ScopeOrg, "", settings.Values{"limit": 3, "enabled": true}, nil)
	if len(f.pub.events) != 1 {
		t.Fatalf("events = %d", len(f.pub.events))
	}
	ev := f.pub.events[0]
	if ev.Rev != 1 || ev.ScopeKind != "org" || ev.AddonKey != "acme" || ev.OrgID != f.org ||
		len(ev.Keys) != 2 || ev.Keys[0] != "enabled" || ev.Keys[1] != "limit" {
		t.Fatalf("event = %+v", ev)
	}
}

func TestWasmResolverLayering(t *testing.T) {
	f := newFixture(t)
	branch := uuid.NewString()
	f.write(t, settings.ScopeOrg, "", settings.Values{"mode": "pro"}, nil)
	f.write(t, settings.ScopeBranch, branch, settings.Values{"limit": 3}, nil)
	w := settings.WasmResolver{R: f.rs}
	base := map[string]string{"mode": "legacy", "limit": "99", "api_key": "kept", "enabled": "true"}
	got, err := w.ResolveSettings(context.Background(), f.org, "acme", branch, base)
	if err != nil {
		t.Fatal(err)
	}
	// stored org/branch override base; base-only keys survive; base
	// beats defaults.
	if got["mode"] != "pro" || got["limit"] != "3" || got["api_key"] != "kept" || got["enabled"] != "true" {
		t.Fatalf("got %v", got)
	}
	if base["mode"] != "legacy" {
		t.Fatal("base mutated")
	}
	// Without a branch the default fills nothing that base already has...
	got, _ = w.ResolveSettings(context.Background(), f.org, "acme", "", base)
	if got["limit"] != "99" {
		t.Fatalf("default must not override base: %v", got)
	}
	// ...but fills a missing key.
	got, _ = w.ResolveSettings(context.Background(), f.org, "acme", "", map[string]string{})
	if got["limit"] != "10" {
		t.Fatalf("default fill: %v", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
