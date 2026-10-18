package dataload

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func newRegistry() *Registry {
	return &Registry{Store: NewMemoryProfileStore(), Now: newClock().Now}
}

func baseCatalog(version string) Profile {
	p := catalogProfile()
	p.Name, p.Version = "catalogo", version
	return p
}

func publishBase(t *testing.T, g *Registry, version string) StoredProfile {
	t.Helper()
	p := baseCatalog(version)
	sp, err := g.Publish(context.Background(), StoredProfile{ProfileKey: ProfileKey{Name: "catalogo", Version: version}, Profile: &p, CreatedBy: "plataforma"})
	if err != nil {
		t.Fatal(err)
	}
	return sp
}

func TestRegistry_ResolvesBaseProfilePlusOrgOverridesWithVersionAndFingerprint(t *testing.T) {
	g, ctx := newRegistry(), context.Background()
	base := publishBase(t, g, "1")
	if base.Fingerprint == "" || base.CreatedAt.IsZero() {
		t.Fatalf("la publicación debe sellar huella y fecha: %+v", base)
	}
	ov := &Overrides{Org: "org-a", Rename: map[string][]string{"name": {"Título"}}, IgnoreHeaders: []string{"Meta: *"}, Defaults: map[string]any{"currency": "MXN"}}
	if _, err := g.Publish(ctx, StoredProfile{ProfileKey: ProfileKey{Org: "org-a", Name: "catalogo", Version: "a1"}, Base: &ProfileKey{Name: "catalogo", Version: "1"}, Overrides: ov}); err != nil {
		t.Fatal(err)
	}

	a, err := g.Resolve(ctx, "org-a", "catalogo", "")
	if err != nil {
		t.Fatal(err)
	}
	if a.Key.Version != "a1" || a.Key.Org != "org-a" || len(a.Chain) != 2 || a.Chain[1] != base.ProfileKey {
		t.Fatalf("a = %+v", a.Key)
	}
	if f, _ := a.Profile.field("name"); !contains(f.From, "Título") || a.Profile.Defaults["currency"] != "MXN" || len(a.Profile.IgnoreHeaders) != 1 {
		t.Fatalf("los overrides de la organización deben estar aplicados: %+v", a.Profile)
	}
	// Otra organización sin derivado usa el catálogo base tal cual.
	b, err := g.Resolve(ctx, "org-b", "catalogo", "")
	if err != nil || b.Key.Org != "" || b.Key.Version != "1" || len(b.Chain) != 1 {
		t.Fatalf("b = %+v %v", b.Key, err)
	}
	if a.Fingerprint == b.Fingerprint || a.Fingerprint == "" || a.OverridesHash == "" || b.OverridesHash != "" {
		t.Fatalf("la huella distingue lo resuelto: %q %q", a.Fingerprint, b.Fingerprint)
	}
	// Resolver dos veces es estable.
	again, _ := g.Resolve(ctx, "org-a", "catalogo", "a1")
	if again.Fingerprint != a.Fingerprint {
		t.Fatal("la resolución debe ser reproducible")
	}
}

func TestRegistry_PublishedVersionsAreImmutable(t *testing.T) {
	g, ctx := newRegistry(), context.Background()
	first := publishBase(t, g, "1")
	// Lo mismo otra vez: no-op, misma huella.
	same := publishBase(t, g, "1")
	if same.Fingerprint != first.Fingerprint {
		t.Fatal("republicar igual no cambia nada")
	}
	// Otro contenido bajo la misma versión: error.
	p := baseCatalog("1")
	p.Fields = append(p.Fields, Field{Target: "extra", From: []string{"Extra"}})
	_, err := g.Publish(ctx, StoredProfile{ProfileKey: ProfileKey{Name: "catalogo", Version: "1"}, Profile: &p})
	if !errors.Is(err, ErrProfileVersionExists) {
		t.Fatalf("err = %v", err)
	}
	// Una versión nueva sí, y la última es la que se resuelve sin pedir versión.
	p2 := baseCatalog("2")
	p2.Fields = append(p2.Fields, Field{Target: "extra", From: []string{"Extra"}})
	if _, err := g.Publish(ctx, StoredProfile{ProfileKey: ProfileKey{Name: "catalogo", Version: "2"}, Profile: &p2}); err != nil {
		t.Fatal(err)
	}
	latest, _ := g.Resolve(ctx, "org-a", "catalogo", "")
	old, _ := g.Resolve(ctx, "org-a", "catalogo", "1")
	if latest.Key.Version != "2" || old.Key.Version != "1" || latest.Fingerprint == old.Fingerprint {
		t.Fatalf("latest=%s old=%s", latest.Key.Version, old.Key.Version)
	}
}

func TestRegistry_DerivedVersionIsPinnedToItsBaseVersion(t *testing.T) {
	g, ctx := newRegistry(), context.Background()
	publishBase(t, g, "1")
	ov := &Overrides{Rename: map[string][]string{"name": {"Título"}}}
	if _, err := g.Publish(ctx, StoredProfile{ProfileKey: ProfileKey{Org: "org-a", Name: "catalogo", Version: "a1"}, Base: &ProfileKey{Name: "catalogo", Version: "1"}, Overrides: ov}); err != nil {
		t.Fatal(err)
	}
	before, _ := g.Resolve(ctx, "org-a", "catalogo", "a1")
	// La plataforma publica v2 de la base con un campo nuevo: a1 no cambia.
	p2 := baseCatalog("2")
	p2.Fields = append(p2.Fields, Field{Target: "extra", From: []string{"Extra"}})
	if _, err := g.Publish(ctx, StoredProfile{ProfileKey: ProfileKey{Name: "catalogo", Version: "2"}, Profile: &p2}); err != nil {
		t.Fatal(err)
	}
	after, _ := g.Resolve(ctx, "org-a", "catalogo", "a1")
	if after.Fingerprint != before.Fingerprint {
		t.Fatal("una base nueva no debe mover una versión ya publicada")
	}
	// La organización adopta v2 publicando a2.
	if _, err := g.Publish(ctx, StoredProfile{ProfileKey: ProfileKey{Org: "org-a", Name: "catalogo", Version: "a2"}, Base: &ProfileKey{Name: "catalogo", Version: "2"}, Overrides: ov}); err != nil {
		t.Fatal(err)
	}
	a2, _ := g.Resolve(ctx, "org-a", "catalogo", "")
	if _, ok := a2.Profile.field("extra"); !ok || a2.Key.Version != "a2" {
		t.Fatalf("a2 = %+v", a2.Key)
	}
	// Una derivada puede apoyarse en una versión propia anterior.
	if _, err := g.Publish(ctx, StoredProfile{ProfileKey: ProfileKey{Org: "org-a", Name: "catalogo", Version: "a3"}, Base: &ProfileKey{Org: "org-a", Name: "catalogo", Version: "a2"}, Overrides: &Overrides{Defaults: map[string]any{"x": 1}}}); err != nil {
		t.Fatal(err)
	}
	a3, err := g.Resolve(ctx, "org-a", "catalogo", "a3")
	if err != nil || len(a3.Chain) != 3 || a3.Profile.Defaults["x"] != 1 {
		t.Fatalf("a3 = %+v %v", a3.Chain, err)
	}
}

func TestRegistry_OrganizationsAreIsolated(t *testing.T) {
	g, ctx := newRegistry(), context.Background()
	publishBase(t, g, "1")
	if _, err := g.Publish(ctx, StoredProfile{ProfileKey: ProfileKey{Org: "org-a", Name: "catalogo", Version: "a1"}, Base: &ProfileKey{Name: "catalogo", Version: "1"}, Overrides: &Overrides{Defaults: map[string]any{"secreto": "a"}}}); err != nil {
		t.Fatal(err)
	}
	b, err := g.Resolve(ctx, "org-b", "catalogo", "a1")
	if !errors.Is(err, ErrProfileNotFound) {
		t.Fatalf("org-b no debe ver lo de org-a: %+v %v", b.Key, err)
	}
	if _, err := g.Publish(ctx, StoredProfile{ProfileKey: ProfileKey{Org: "org-b", Name: "x", Version: "1"}, Base: &ProfileKey{Org: "org-a", Name: "catalogo", Version: "a1"}}); err == nil {
		t.Fatal("una organización no puede apoyarse en la versión de otra")
	}
	keys, _ := g.Versions(ctx, "org-b")
	for _, k := range keys {
		if k.Org == "org-a" {
			t.Fatalf("Versions filtró lo de otra organización: %v", keys)
		}
	}
	keysA, _ := g.Versions(ctx, "org-a")
	if len(keysA) != 2 {
		t.Fatalf("org-a ve el catálogo base y lo suyo: %v", keysA)
	}
}

func TestRegistry_RejectsInvalidPublications(t *testing.T) {
	g, ctx := newRegistry(), context.Background()
	publishBase(t, g, "1")
	p := baseCatalog("1")
	k := func(org, v string) ProfileKey { return ProfileKey{Org: org, Name: "catalogo", Version: v} }
	cases := map[string]StoredProfile{
		"sin nada":                {ProfileKey: k("o", "x")},
		"perfil y base":           {ProfileKey: k("o", "x"), Profile: &p, Base: &ProfileKey{Name: "catalogo", Version: "1"}},
		"nombre distinto":         {ProfileKey: ProfileKey{Org: "o", Name: "otro", Version: "1"}, Profile: &p},
		"base sin versión":        {ProfileKey: k("o", "x"), Base: &ProfileKey{Name: "catalogo"}},
		"base inexistente":        {ProfileKey: k("o", "x"), Base: &ProfileKey{Name: "catalogo", Version: "99"}},
		"propia base":             {ProfileKey: k("o", "x"), Base: &ProfileKey{Org: "o", Name: "catalogo", Version: "x"}},
		"override de otra org":    {ProfileKey: k("o", "x"), Base: &ProfileKey{Name: "catalogo", Version: "1"}, Overrides: &Overrides{Org: "otra"}},
		"override roto":           {ProfileKey: k("o", "x"), Base: &ProfileKey{Name: "catalogo", Version: "1"}, Overrides: &Overrides{Rename: map[string][]string{"no_existe": {"X"}}}},
		"overrides en completo":   {ProfileKey: k("o", "y"), Profile: func() *Profile { q := baseCatalog("y"); return &q }(), Overrides: &Overrides{}},
		"extracción de otro perf": {ProfileKey: k("o", "x"), Base: &ProfileKey{Name: "catalogo", Version: "1"}, Extract: &ExtractDoc{Schema: ExtractSchema, Profile: ProfileRef{Name: "otro", Version: "1"}, Text: ExtractText{Source: "name"}}},
	}
	for name, sp := range cases {
		if _, err := g.Publish(ctx, sp); err == nil {
			t.Errorf("%s: debía rechazarse", name)
		}
	}
	list, _ := g.Store.List(ctx, "o", "")
	if len(list) != 0 {
		t.Fatalf("no debe quedar nada de lo rechazado: %v", list)
	}
}

func TestRegistry_ExtractTravelsWithTheProfileAndIsInheritedByDerivedVersions(t *testing.T) {
	g, ctx := newRegistry(), context.Background()
	prof, ex := loadLista(t)
	if _, err := g.Publish(ctx, StoredProfile{ProfileKey: ProfileKey{Name: prof.Name, Version: prof.Version}, Profile: &prof, Extract: &ex.Doc}); err != nil {
		t.Fatal(err)
	}
	base, _ := g.Resolve(ctx, "org-a", prof.Name, "")
	if base.Extract == nil || base.Extract.Hash() != ex.Hash() {
		t.Fatal("la extracción viaja con la versión")
	}
	if _, err := g.Publish(ctx, StoredProfile{ProfileKey: ProfileKey{Org: "org-a", Name: prof.Name, Version: "a1"}, Base: &ProfileKey{Name: prof.Name, Version: prof.Version},
		Overrides: &Overrides{Rename: map[string][]string{"sku": {"Código"}}}}); err != nil {
		t.Fatal(err)
	}
	derived, _ := g.Resolve(ctx, "org-a", prof.Name, "")
	if derived.Extract == nil || derived.Extract.Hash() != ex.Hash() || derived.Fingerprint == base.Fingerprint {
		t.Fatalf("la derivada hereda la extracción pero su huella cambia por los overrides")
	}
	// La derivada puede traer su propio documento (mismo perfil).
	doc := ex.Doc
	doc.Text.DropTokens = []string{"-", "--"}
	if _, err := g.Publish(ctx, StoredProfile{ProfileKey: ProfileKey{Org: "org-a", Name: prof.Name, Version: "a2"}, Base: &ProfileKey{Name: prof.Name, Version: prof.Version}, Extract: &doc}); err != nil {
		t.Fatal(err)
	}
	a2, _ := g.Resolve(ctx, "org-a", prof.Name, "a2")
	if a2.Extract.Hash() == ex.Hash() {
		t.Fatal("el documento propio manda")
	}
}

func TestRegistry_ResolvedProfileDrivesARunAndLeavesItsVersionInTheLedger(t *testing.T) {
	g, ctx := newRegistry(), context.Background()
	prof, ex := loadLista(t)
	if _, err := g.Publish(ctx, StoredProfile{ProfileKey: ProfileKey{Name: prof.Name, Version: prof.Version}, Profile: &prof, Extract: &ex.Doc}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Publish(ctx, StoredProfile{ProfileKey: ProfileKey{Org: "org-a", Name: prof.Name, Version: "a1"}, Base: &ProfileKey{Name: prof.Name, Version: prof.Version}, Overrides: &Overrides{Rename: map[string][]string{"sku": {"Código"}}}}); err != nil {
		t.Fatal(err)
	}
	r := newRig(t)
	body := listaCSV(listaRows()[:2]...)
	// org-a usa su derivada: el encabezado "Código" también vale.
	resolved, err := g.Resolve(ctx, "org-a", prof.Name, "")
	if err != nil {
		t.Fatal(err)
	}
	custom := strings.Replace(string(body), "Articulo", "Código", 1)
	run := startAndWait(t, r, resolved.Request("org-a", "u1", NewCSVSource("lista.csv", []byte(custom))))
	if run.State != RunSucceeded || run.Progress.Created != 2 || run.Registry == nil || run.Registry.Key.Version != "a1" || run.Registry.Fingerprint != resolved.Fingerprint || run.ExtractHash == "" {
		t.Fatalf("run = %+v %+v", run.Progress, run.Registry)
	}
	// Con el perfil base, el encabezado "Código" no existe: el mismo archivo no es válido.
	baseRes, _ := g.Resolve(ctx, "org-b", prof.Name, "")
	plan, err := BuildPlan(ctx, PlanRequest{Org: "org-b", Profile: baseRes.Profile, Extract: baseRes.Extract, Source: NewCSVSource("x", []byte(custom)), Target: NewMemoryTarget()})
	if err != nil || plan.Counts[ActionInvalid] != 2 {
		t.Fatalf("%v %v", err, plan.Counts)
	}
}
