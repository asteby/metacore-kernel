package host

import (
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/lifecycle"
	"github.com/asteby/metacore-kernel/manifest"
)

func installedManifestsHost(t *testing.T) (*Host, *gorm.DB) {
	t.Helper()
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
	return h, db
}

func seedInstall(t *testing.T, db *gorm.DB, org uuid.UUID, key, version, status string) {
	t.Helper()
	if err := db.Exec(`INSERT INTO metacore_installations (id, organization_id, addon_key, version, status, source) VALUES (?,?,?,?,?,?)`,
		uuid.NewString(), org.String(), key, version, status, "bundle").Error; err != nil {
		t.Fatal(err)
	}
}

func versions(ms []manifest.Manifest) map[string]string {
	out := map[string]string{}
	for _, m := range ms {
		out[m.Key] = m.Version
	}
	return out
}

// La tabla (fuente de verdad) manda: memoria vieja => se reporta la versión de
// la fila, sin mutar el registro compartido.
func TestInstalledManifests_DriftReportsTableVersion(t *testing.T) {
	h, db := installedManifestsHost(t)
	h.RegisterCompiled("integration_mercadolibre", &lifecycle.ManifestOnly{Data: manifest.Manifest{Key: "integration_mercadolibre", Version: "0.5.1"}})
	org := uuid.New()
	seedInstall(t, db, org, "integration_mercadolibre", "0.5.2", "enabled")

	for i := 0; i < 2; i++ { // la segunda llamada no debe cambiar el resultado (rate-limit solo afecta al log)
		got, err := h.InstalledManifests(org)
		if err != nil {
			t.Fatal(err)
		}
		if v := versions(got)["integration_mercadolibre"]; v != "0.5.2" {
			t.Fatalf("call %d: version = %q, want 0.5.2 (table)", i, v)
		}
	}
	lc, _ := h.Lifecycles.Get("integration_mercadolibre")
	if v := lc.Manifest().Version; v != "0.5.1" {
		t.Fatalf("shared registry mutated: version = %q, want 0.5.1", v)
	}
}

func TestInstalledManifests_NoDrift(t *testing.T) {
	h, db := installedManifestsHost(t)
	h.RegisterCompiled("channel_orders", &lifecycle.ManifestOnly{Data: manifest.Manifest{Key: "channel_orders", Version: "0.8.1"}})
	org := uuid.New()
	seedInstall(t, db, org, "channel_orders", "0.8.1", "enabled")

	got, err := h.InstalledManifests(org)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Version != "0.8.1" {
		t.Fatalf("got %+v", versions(got))
	}
	if _, loaded := h.driftWarned.Load("drift:channel_orders:0.8.1:0.8.1"); loaded {
		t.Fatal("no drift must not warn")
	}
}

// Instalación enabled sin lifecycle en memoria: se omite (no hay manifest que
// servir), no falla, y las demás siguen; una deshabilitada nunca aparece.
func TestInstalledManifests_NoLifecycleIsOmitted(t *testing.T) {
	h, db := installedManifestsHost(t)
	h.RegisterCompiled("known", &lifecycle.ManifestOnly{Data: manifest.Manifest{Key: "known", Version: "1.0.0"}})
	org := uuid.New()
	seedInstall(t, db, org, "known", "1.0.1", "enabled")
	seedInstall(t, db, org, "ghost", "2.0.0", "enabled")
	seedInstall(t, db, org, "off", "3.0.0", "disabled")
	h.RegisterCompiled("off", &lifecycle.ManifestOnly{Data: manifest.Manifest{Key: "off", Version: "3.0.0"}})

	got, err := h.InstalledManifests(org)
	if err != nil {
		t.Fatal(err)
	}
	v := versions(got)
	if len(v) != 1 || v["known"] != "1.0.1" {
		t.Fatalf("got %+v, want only known@1.0.1", v)
	}
	if _, loaded := h.driftWarned.Load("missing:ghost"); !loaded {
		t.Fatal("missing lifecycle must be logged (rate-limited)")
	}
}
