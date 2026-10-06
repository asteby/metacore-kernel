package host

import (
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/lifecycle"
	"github.com/asteby/metacore-kernel/manifest"
)

// Una entrada de menú con `condition: {addon_installed: X}` solo se sirve a la
// org que tiene X habilitado. Host.Navigation usaba navigation.Build (sin
// predicado) y la servía siempre: con facturación apagada seguía el «PPD sin
// complemento de pago» de customers en el menú de Facturas.
func TestNavigation_ConditionUsesEnabledAddons(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE metacore_installations (
		id text PRIMARY KEY, organization_id text NOT NULL, addon_key text NOT NULL,
		version text NOT NULL, status text NOT NULL DEFAULT "enabled", source text NOT NULL,
		secret_hash text, secret_enc text, settings text, manifest_hash text,
		installed_at datetime, enabled_at datetime, disabled_at datetime)`).Error; err != nil {
		t.Fatal(err)
	}
	h, err := New(Config{DB: db})
	if err != nil {
		t.Fatal(err)
	}
	h.RegisterCompiled("customers", &lifecycle.ManifestOnly{Data: manifest.Manifest{
		Key: "customers",
		Navigation: []manifest.NavGroup{{Title: "Ventas", Items: []manifest.NavItem{
			{Title: "Facturas", Model: "Invoice"},
			{Title: "PPD sin REP", Model: "Invoice", Condition: &manifest.ConditionDef{AddonInstalled: "fiscal_mexico"}},
		}}},
	}})
	h.RegisterCompiled("fiscal_mexico", &lifecycle.ManifestOnly{Data: manifest.Manifest{Key: "fiscal_mexico"}})

	org := uuid.New()
	insert := func(key, status string) {
		if err := db.Exec(`INSERT INTO metacore_installations (id, organization_id, addon_key, version, status, source) VALUES (?,?,?,?,?,?)`,
			uuid.NewString(), org.String(), key, "1.0.0", status, "bundle").Error; err != nil {
			t.Fatal(err)
		}
	}
	titles := func() []string {
		groups, err := h.Navigation(org, nil)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, g := range groups {
			for _, it := range g.Items {
				out = append(out, it.Title)
			}
		}
		return out
	}

	insert("customers", "enabled")
	insert("fiscal_mexico", "disabled")
	if got := titles(); len(got) != 1 || got[0] != "Facturas" {
		t.Fatalf("fiscal_mexico apagado: menú = %v, want [Facturas]", got)
	}
	if err := db.Exec(`UPDATE metacore_installations SET status = 'enabled' WHERE addon_key = 'fiscal_mexico'`).Error; err != nil {
		t.Fatal(err)
	}
	if got := titles(); len(got) != 2 {
		t.Fatalf("fiscal_mexico habilitado: menú = %v, want las dos entradas", got)
	}
}
