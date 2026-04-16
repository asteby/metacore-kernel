package installer

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestConvertLegacyInstallation_Minimal verifies the happy path: a legacy row
// with all required fields maps to a kernel Installation with equivalent
// columns and the F1.5 "legacy" source marker.
func TestConvertLegacyInstallation_Minimal(t *testing.T) {
	orgID := uuid.New()
	instID := uuid.New()
	installed := time.Date(2024, 11, 1, 12, 0, 0, 0, time.UTC)
	enabled := installed.Add(time.Hour)
	in := legacyInstallation{
		ID:             instID,
		OrganizationID: orgID,
		AddonKey:       "fiscal_mexico",
		Version:        "1.2.3",
		Status:         "enabled",
		InstalledAt:    installed,
		EnabledAt:      &enabled,
	}
	got, err := convertLegacyInstallation(in)
	if err != nil {
		t.Fatalf("convertLegacyInstallation: %v", err)
	}
	if got.ID != instID {
		t.Errorf("ID: got %v, want %v", got.ID, instID)
	}
	if got.OrganizationID != orgID {
		t.Errorf("OrganizationID: got %v, want %v", got.OrganizationID, orgID)
	}
	if got.AddonKey != "fiscal_mexico" {
		t.Errorf("AddonKey: got %q, want %q", got.AddonKey, "fiscal_mexico")
	}
	if got.Version != "1.2.3" {
		t.Errorf("Version: got %q", got.Version)
	}
	if got.Status != "enabled" {
		t.Errorf("Status: got %q", got.Status)
	}
	if got.Source != "legacy" {
		t.Errorf("Source: got %q, want %q", got.Source, "legacy")
	}
	if !got.InstalledAt.Equal(installed) {
		t.Errorf("InstalledAt: got %v, want %v", got.InstalledAt, installed)
	}
	if got.EnabledAt == nil || !got.EnabledAt.Equal(enabled) {
		t.Errorf("EnabledAt: got %v", got.EnabledAt)
	}
	if got.DisabledAt != nil {
		t.Errorf("DisabledAt: expected nil, got %v", got.DisabledAt)
	}
	if got.Settings == nil {
		t.Error("Settings: expected initialized empty map, got nil")
	}
}

// TestConvertLegacyInstallation_DefaultsStatus checks that a blank legacy
// status falls back to "enabled" rather than propagating an empty string
// into the kernel table (where it would fail a NOT NULL DEFAULT).
func TestConvertLegacyInstallation_DefaultsStatus(t *testing.T) {
	in := legacyInstallation{
		OrganizationID: uuid.New(),
		AddonKey:       "inventory",
		InstalledAt:    time.Now(),
	}
	got, err := convertLegacyInstallation(in)
	if err != nil {
		t.Fatalf("convertLegacyInstallation: %v", err)
	}
	if got.Status != "enabled" {
		t.Errorf("Status default: got %q, want %q", got.Status, "enabled")
	}
}

// TestConvertLegacyInstallation_FillsInstalledAt ensures a zero-valued
// installed_at gets stamped with now() so the kernel NOT NULL column is
// satisfied even if legacy data was missing it.
func TestConvertLegacyInstallation_FillsInstalledAt(t *testing.T) {
	in := legacyInstallation{
		OrganizationID: uuid.New(),
		AddonKey:       "crm",
	}
	before := time.Now().Add(-time.Second)
	got, err := convertLegacyInstallation(in)
	if err != nil {
		t.Fatalf("convertLegacyInstallation: %v", err)
	}
	if got.InstalledAt.Before(before) {
		t.Errorf("InstalledAt should be stamped with ~now, got %v", got.InstalledAt)
	}
}

// TestConvertLegacyInstallation_Validation rejects rows missing required
// identifiers — better to fail loudly than silently create orphan kernel
// rows during the migration window.
func TestConvertLegacyInstallation_Validation(t *testing.T) {
	cases := []struct {
		name string
		in   legacyInstallation
	}{
		{"missing org", legacyInstallation{AddonKey: "k"}},
		{"missing key", legacyInstallation{OrganizationID: uuid.New()}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := convertLegacyInstallation(tc.in); err == nil {
				t.Errorf("expected error for %s, got nil", tc.name)
			}
		})
	}
}

// TestSyncFromLegacy_DBIntegration is intentionally skipped: the metacore
// module doesn't pull in a SQLite GORM driver (kernel stays Postgres-only to
// avoid CGO / extra deps), and this is a unit test suite. The full round-trip
// behaviour of SyncFromLegacy + BackfillSecrets is covered by the runbook
// staging exercise documented in docs/migration-from-ops.md.
//
// TODO(metacore-f1.5): re-enable once a non-CGO SQLite or an ephemeral
// Postgres helper lands in the test harness.
func TestSyncFromLegacy_DBIntegration(t *testing.T) {
	t.Skip("no in-memory DB driver available in kernel go.mod; covered in staging runbook")
}
