package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/manifest"
)

// loadInventoryFixture reads the real inventory v3 manifest copied from the
// asteby-hq/addons monorepo. Using a real manifest guarantees the dual-read
// path parses production-authored v3 documents end to end.
func loadInventoryFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "inventory_v3_manifest.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

// packManifest wraps a manifest.json payload in a minimal single-entry tar.gz
// so it can be fed through the public Read path.
func packManifest(t *testing.T, data []byte) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name:     "manifest.json",
		Mode:     0o644,
		Size:     int64(len(data)),
		Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatalf("tar write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gz close: %v", err)
	}
	return &buf
}

// TestParseManifestV3 verifies that parseManifest detects a v3 manifest by its
// apiVersion, validates it against the v3 contract, and maps it into the legacy
// manifest.Manifest with the identity, tenancy and model fields the kernel
// consumers read.
func TestParseManifestV3(t *testing.T) {
	data := loadInventoryFixture(t)

	var m manifest.Manifest
	if err := parseManifest(data, &m); err != nil {
		t.Fatalf("parseManifest v3: %v", err)
	}

	if m.Key != "inventory" {
		t.Errorf("Key = %q, want %q", m.Key, "inventory")
	}
	if m.Name != "Inventory" {
		t.Errorf("Name = %q, want %q", m.Name, "Inventory")
	}
	if m.Version != "0.1.0" {
		t.Errorf("Version = %q, want %q", m.Version, "0.1.0")
	}
	if m.Category != "foundation.commerce" {
		t.Errorf("Category = %q, want %q", m.Category, "foundation.commerce")
	}
	if m.TenantIsolation != "shared" {
		t.Errorf("TenantIsolation = %q, want %q", m.TenantIsolation, "shared")
	}
	if m.Kernel != ">=3.0.0 <4.0.0" {
		t.Errorf("Kernel = %q, want %q", m.Kernel, ">=3.0.0 <4.0.0")
	}
	if m.IconType != "lucide" || m.IconSlug != "Boxes" || m.IconColor != "#0EA5E9" {
		t.Errorf("Icon = {%q,%q,%q}, want {lucide,Boxes,#0EA5E9}", m.IconType, m.IconSlug, m.IconColor)
	}

	if len(m.ModelDefinitions) != 8 {
		t.Fatalf("ModelDefinitions = %d, want 8", len(m.ModelDefinitions))
	}

	// Spot-check the Product model: managed columns stripped, OrgScoped +
	// SoftDelete derived, business columns preserved with the unique flag
	// folded back from a single-column unique index where applicable.
	var product *manifest.ModelDefinition
	for i := range m.ModelDefinitions {
		if m.ModelDefinitions[i].ModelKey == "Product" {
			product = &m.ModelDefinitions[i]
			break
		}
	}
	if product == nil {
		t.Fatal("Product model not mapped")
	}
	if product.TableName != "products" {
		t.Errorf("Product.TableName = %q, want products", product.TableName)
	}
	if !product.OrgScoped {
		t.Error("Product.OrgScoped = false, want true (organization_id column present)")
	}
	if !product.SoftDelete {
		t.Error("Product.SoftDelete = false, want true (deleted_at column present)")
	}
	var sawSKU bool
	for _, c := range product.Columns {
		switch c.Name {
		case "id", "organization_id", "created_at", "updated_at", "deleted_at":
			t.Errorf("managed column %q should have been stripped from ModelDefinition", c.Name)
		case "sku":
			sawSKU = true
			if c.Type != "text" || !c.Required {
				t.Errorf("sku col = %+v, want type=text required=true", c)
			}
		}
	}
	if !sawSKU {
		t.Error("sku column missing from Product")
	}

	// Navigation must round-trip with one group of seven items.
	if len(m.Navigation) != 1 {
		t.Fatalf("Navigation groups = %d, want 1", len(m.Navigation))
	}
	if len(m.Navigation[0].Items) != 7 {
		t.Errorf("Navigation items = %d, want 7", len(m.Navigation[0].Items))
	}

	// Settings map across.
	if len(m.Settings) != 2 {
		t.Errorf("Settings = %d, want 2", len(m.Settings))
	}

	// Lifecycle install/uninstall/enable/disable become wasm hook targets.
	for _, event := range []string{"install", "uninstall", "enable", "disable"} {
		hooks := m.LifecycleHooks[event]
		if len(hooks) != 1 {
			t.Errorf("LifecycleHooks[%q] = %d, want 1", event, len(hooks))
			continue
		}
		if hooks[0].Target.Type != "wasm" {
			t.Errorf("LifecycleHooks[%q].Target.Type = %q, want wasm", event, hooks[0].Target.Type)
		}
	}

	// i18n is keyed by locale with empty inner maps (paths load at runtime).
	if len(m.I18n) != 2 {
		t.Errorf("I18n locales = %d, want 2", len(m.I18n))
	}
}

// TestParseManifestV2Unchanged confirms the legacy path is untouched: a v2
// manifest (no apiVersion) still unmarshals directly into manifest.Manifest.
func TestParseManifestV2Unchanged(t *testing.T) {
	legacy := []byte(`{"key":"legacy","name":"Legacy","version":"1.0.0","tenant_isolation":"schema-per-tenant"}`)
	var m manifest.Manifest
	if err := parseManifest(legacy, &m); err != nil {
		t.Fatalf("parseManifest v2: %v", err)
	}
	if m.Key != "legacy" || m.Version != "1.0.0" {
		t.Errorf("v2 manifest not parsed: %+v", m)
	}
	if m.TenantIsolation != "schema-per-tenant" {
		t.Errorf("TenantIsolation = %q, want schema-per-tenant", m.TenantIsolation)
	}
}

// TestReadV3Bundle exercises the full Read path with a v3 manifest packed into
// a real tar.gz, proving the gap that previously failed end to end is closed
// and the mapped Manifest is usable by downstream consumers (e.g. the
// dynamic.ParseIsolation the installer feeds from TenantIsolation).
func TestReadV3Bundle(t *testing.T) {
	data := loadInventoryFixture(t)

	b, err := Read(packManifest(t, data), 0)
	if err != nil {
		t.Fatalf("Read v3 bundle: %v", err)
	}
	if b.Manifest.Key != "inventory" {
		t.Errorf("Manifest.Key = %q, want inventory", b.Manifest.Key)
	}
	if got := dynamic.ParseIsolation(b.Manifest.TenantIsolation); got != dynamic.IsolationShared {
		t.Errorf("ParseIsolation = %q, want shared", got)
	}
	if len(b.Manifest.ModelDefinitions) == 0 {
		t.Error("expected at least one ModelDefinition for a fixture with models")
	}
}
