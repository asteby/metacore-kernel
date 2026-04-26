// Package installer orchestrates the full install/enable/disable/uninstall
// flow: validate manifest, create schema, run migrations, register lifecycle,
// emit events, and record the installation row. It is the single entry point
// host applications call — they never drive individual kernel steps directly.
package installer

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/asteby/metacore-kernel/bundle"
	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/lifecycle"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/security"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Installation is the kernel's persisted record. Host apps may extend it.
type Installation struct {
	ID             uuid.UUID      `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	OrganizationID uuid.UUID      `gorm:"type:uuid;not null;uniqueIndex:idx_org_addon" json:"organization_id"`
	AddonKey       string         `gorm:"size:100;not null;uniqueIndex:idx_org_addon" json:"addon_key"`
	Version        string         `gorm:"size:40;not null" json:"version"`
	Status         string         `gorm:"size:20;not null;default:'enabled'" json:"status"`
	Source         string         `gorm:"size:20;not null" json:"source"` // compiled | bundle | federated
	SecretHash     string         `gorm:"size:128" json:"-"`              // hash of install-time secret
	// SecretEnc stores the per-installation HMAC secret encrypted with the
	// host's master key (AES-256-GCM, base64). Only populated when the
	// Installer is constructed with a MasterKey. Hosts with a KMS can ignore
	// this and supply their own SecretResolver.
	SecretEnc      string         `gorm:"size:512;column:secret_enc" json:"-"`
	Settings       map[string]any `gorm:"serializer:json;type:jsonb" json:"settings"`
	InstalledAt    time.Time      `gorm:"autoCreateTime" json:"installed_at"`
	EnabledAt      *time.Time     `json:"enabled_at,omitempty"`
	DisabledAt     *time.Time     `json:"disabled_at,omitempty"`
}

func (Installation) TableName() string { return "metacore_installations" }

// Installer wires the collaborators the kernel needs.
type Installer struct {
	DB            *gorm.DB
	KernelVersion string
	Lifecycles    *lifecycle.Registry
	Interceptors  *lifecycle.InterceptorRegistry

	// FrontendBasePath, when non-empty, is the on-disk root under which
	// federation artifacts shipped inside a bundle are materialized by
	// Install() — see WriteFrontend. Hosts that do not serve addon frontends
	// from disk (e.g. CDN-only deployments) leave this empty.
	FrontendBasePath string

	// MasterKey (32 bytes) enables at-rest encryption of per-installation
	// HMAC secrets into Installation.SecretEnc. When nil the kernel never
	// persists the cleartext secret — callers must build their own
	// SecretResolver (e.g. backed by a KMS).
	MasterKey []byte
}

// New returns a ready-to-use installer with initialized registries.
func New(db *gorm.DB, kernelVersion string) *Installer {
	return &Installer{
		DB:            db,
		KernelVersion: kernelVersion,
		Lifecycles:    lifecycle.NewRegistry(),
		Interceptors:  lifecycle.NewInterceptorRegistry(),
	}
}

// Install applies a bundle to the given organization. It:
//  1. validates the manifest
//  2. creates the addon schema and runs migrations
//  3. creates tables from model_definitions (idempotent)
//  4. registers a declarative lifecycle (if no compiled one was pre-registered)
//  5. writes the metacore_installations row with a new per-install secret
//
// Returns the per-installation secret (caller shares only with the addon).
func (i *Installer) Install(orgID uuid.UUID, b *bundle.Bundle) (*Installation, []byte, error) {
	if err := b.Manifest.Validate(i.KernelVersion); err != nil {
		return nil, nil, err
	}
	if err := i.DB.AutoMigrate(&Installation{}); err != nil {
		return nil, nil, err
	}
	iso := dynamic.ParseIsolation(b.Manifest.TenantIsolation)
	if err := dynamic.EnsureSchema(i.DB, b.Manifest.Key, orgID, iso); err != nil {
		return nil, nil, err
	}
	if err := dynamic.Apply(i.DB, b.Manifest.Key, orgID, iso, b.Migrations); err != nil {
		return nil, nil, err
	}
	for _, def := range b.Manifest.ModelDefinitions {
		if err := dynamic.CreateTable(i.DB, b.Manifest.Key, orgID, iso, def); err != nil {
			return nil, nil, err
		}
		if err := dynamic.SyncSchema(i.DB, b.Manifest.Key, orgID, iso, def); err != nil {
			return nil, nil, err
		}
	}
	if _, ok := i.Lifecycles.Get(b.Manifest.Key); !ok {
		i.Lifecycles.Register(b.Manifest.Key, &lifecycle.ManifestOnly{Data: b.Manifest})
	}
	lc, _ := i.Lifecycles.Get(b.Manifest.Key)
	if err := lc.OnInstall(i.DB, orgID); err != nil {
		return nil, nil, fmt.Errorf("OnInstall: %w", err)
	}
	if err := lc.OnEnable(i.DB, orgID); err != nil {
		return nil, nil, fmt.Errorf("OnEnable: %w", err)
	}
	secret, err := newSecret()
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	inst := &Installation{
		OrganizationID: orgID,
		AddonKey:       b.Manifest.Key,
		Version:        b.Manifest.Version,
		Status:         "enabled",
		Source:         sourceOf(b.Manifest),
		SecretHash:     hashSecret(secret),
		Settings:       defaultSettings(b.Manifest),
		EnabledAt:      &now,
	}
	if len(i.MasterKey) == 32 {
		enc, err := security.Encrypt(i.MasterKey, secret)
		if err != nil {
			return nil, nil, fmt.Errorf("encrypt secret: %w", err)
		}
		inst.SecretEnc = enc
	}
	if err := i.DB.Create(inst).Error; err != nil {
		return nil, nil, err
	}
	// Materialize federation artifacts to disk so the host's static route can
	// serve them. Non-federation bundles (no frontend, or "script" format) are
	// a no-op. An empty FrontendBasePath opts the host out entirely.
	if b.Manifest.Frontend != nil && b.Manifest.Frontend.Format == "federation" {
		if err := WriteFrontend(i.FrontendBasePath, b.Manifest.Key, b.Frontend); err != nil {
			return nil, nil, fmt.Errorf("WriteFrontend: %w", err)
		}
	}
	return inst, secret, nil
}

// RotateSecret generates a fresh per-installation HMAC secret, re-hashes it,
// re-encrypts it with the master key (if configured) and returns the new
// cleartext — which the caller MUST redistribute to the addon backend.
// Invalidating the old secret is atomic: once the DB row updates, all
// subsequent signed calls use the new secret; the old one stops verifying.
func (i *Installer) RotateSecret(orgID uuid.UUID, addonKey string) ([]byte, error) {
	secret, err := newSecret()
	if err != nil {
		return nil, err
	}
	updates := map[string]any{"secret_hash": hashSecret(secret)}
	if len(i.MasterKey) == 32 {
		enc, err := security.Encrypt(i.MasterKey, secret)
		if err != nil {
			return nil, fmt.Errorf("encrypt rotated secret: %w", err)
		}
		updates["secret_enc"] = enc
	}
	res := i.DB.Model(&Installation{}).
		Where("organization_id = ? AND addon_key = ?", orgID, addonKey).
		Updates(updates)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, fmt.Errorf("rotate: no installation for %s", addonKey)
	}
	return secret, nil
}

// Enable marks an existing installation as enabled and fires OnEnable.
func (i *Installer) Enable(orgID uuid.UUID, addonKey string) error {
	lc, ok := i.Lifecycles.Get(addonKey)
	if !ok {
		return fmt.Errorf("addon %q not registered", addonKey)
	}
	if err := lc.OnEnable(i.DB, orgID); err != nil {
		return err
	}
	now := time.Now()
	return i.DB.Model(&Installation{}).
		Where("organization_id = ? AND addon_key = ?", orgID, addonKey).
		Updates(map[string]any{"status": "enabled", "enabled_at": now, "disabled_at": nil}).Error
}

// Disable flips the row to disabled and fires OnDisable.
func (i *Installer) Disable(orgID uuid.UUID, addonKey string) error {
	lc, ok := i.Lifecycles.Get(addonKey)
	if !ok {
		return fmt.Errorf("addon %q not registered", addonKey)
	}
	if err := lc.OnDisable(i.DB, orgID); err != nil {
		return err
	}
	now := time.Now()
	return i.DB.Model(&Installation{}).
		Where("organization_id = ? AND addon_key = ?", orgID, addonKey).
		Updates(map[string]any{"status": "disabled", "disabled_at": now}).Error
}

// Uninstall removes an addon from an organization. If dropSchema is true the
// addon's Postgres schema is destroyed — caller is responsible for confirming
// destructive intent with the admin.
//
// For schema-per-tenant addons the per-org schema is always dropped (the
// only way to reclaim storage); for shared addons the global schema is only
// dropped when no org still has the addon installed.
func (i *Installer) Uninstall(orgID uuid.UUID, addonKey string, dropSchema bool) error {
	lc, ok := i.Lifecycles.Get(addonKey)
	var iso dynamic.Isolation = dynamic.IsolationShared
	if ok {
		iso = dynamic.ParseIsolation(lc.Manifest().TenantIsolation)
		if err := lc.OnDisable(i.DB, orgID); err != nil {
			return err
		}
		if err := lc.OnUninstall(i.DB, orgID); err != nil {
			return err
		}
	}
	if err := i.DB.Where("organization_id = ? AND addon_key = ?", orgID, addonKey).
		Delete(&Installation{}).Error; err != nil {
		return err
	}
	i.Interceptors.UnregisterAddon(addonKey)
	if !dropSchema {
		return nil
	}
	if iso == dynamic.IsolationPerTenant {
		// Per-tenant schema only belongs to this org → always safe to drop.
		if err := dynamic.DropSchema(i.DB, addonKey, orgID, iso); err != nil {
			return err
		}
	} else {
		// Shared schema — only drop when no other org still has it installed.
		var remaining int64
		i.DB.Model(&Installation{}).Where("addon_key = ?", addonKey).Count(&remaining)
		if remaining == 0 {
			if err := dynamic.DropSchema(i.DB, addonKey, orgID, iso); err != nil {
				return err
			}
		}
	}
	return nil
}

// SignerFor rebuilds the HMAC signer for a known secret. Hosts persist the
// cleartext secret in a secrets manager and only store the hash here.
func SignerFor(secret []byte) *security.Signer { return security.NewSigner(secret) }

func newSecret() ([]byte, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	out := make([]byte, hex.EncodedLen(len(buf)))
	hex.Encode(out, buf)
	return out, nil
}

func hashSecret(secret []byte) string {
	// Real SHA-256 over the full secret. Stored for integrity/rotation — an
	// attacker with DB access sees only the digest, never partial plaintext.
	sum := sha256.Sum256(secret)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func defaultSettings(m manifest.Manifest) map[string]any {
	out := map[string]any{}
	for _, s := range m.Settings {
		if s.DefaultValue != nil {
			out[s.Key] = s.DefaultValue
		}
	}
	return out
}

func sourceOf(m manifest.Manifest) string {
	if m.Frontend != nil && m.Frontend.Format == "federation" {
		return "federated"
	}
	return "bundle"
}
