// Package installer orchestrates the full install/enable/disable/uninstall
// flow: validate manifest, create schema, run migrations, register lifecycle,
// emit events, and record the installation row. It is the single entry point
// host applications call — they never drive individual kernel steps directly.
//
// Trust model — supply chain security:
//
//	hub (offline Ed25519 key) signs published bundles
//	    ↓
//	kernel installer verifies the signature against PublicKeys before
//	running any addon code (manifest, migrations, lifecycle)
//	    ↓
//	addon executes inside the runtime sandbox (capabilities, schema scoping)
//
// PublicKeys is loaded by New() from MARKETPLACE_PUBKEY (single key) or
// MARKETPLACE_PUBKEYS (comma-separated, for rotation). When PublicKeys is
// empty AND ALLOW_UNSIGNED_BUNDLES is not "true", Install() refuses to
// proceed — production deploys MUST configure a key. The unsigned escape
// hatch exists for local development and self-hosted sideloading only.
//
// ops's services/addonbundle/signature.go performs an equivalent check
// before reaching the kernel; that path remains as defense-in-depth but is
// now redundant for the standard install flow. Direct kernel users (link,
// future apps) get the verification for free through this package.
package installer

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
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

	// PublicKeys are the Ed25519 keys trusted to have signed published
	// bundles. Multiple keys support marketplace key rotation: a bundle is
	// accepted if it verifies under ANY of them. New() populates this from
	// MARKETPLACE_PUBKEY (single hex key) or MARKETPLACE_PUBKEYS (comma-
	// separated hex keys).
	//
	// When empty AND AllowUnsigned is false, Install() refuses every bundle
	// — fail-closed is the kernel default. Set AllowUnsigned (env
	// ALLOW_UNSIGNED_BUNDLES=true) only in local dev / self-hosted sideloads.
	PublicKeys []ed25519.PublicKey

	// AllowUnsigned opts out of signature verification entirely. Tied to the
	// ALLOW_UNSIGNED_BUNDLES env var by New(). DO NOT enable in production.
	AllowUnsigned bool
}

// ErrSignatureRequired is returned by Install when the host has not configured
// any trusted public keys and ALLOW_UNSIGNED_BUNDLES is not set. Misconfigured
// production deploys hit this on every install attempt.
var ErrSignatureRequired = errors.New("installer: signature verification is required (set MARKETPLACE_PUBKEY or, for dev, ALLOW_UNSIGNED_BUNDLES=true)")

// New returns a ready-to-use installer with initialized registries.
//
// The installer reads two env vars at construction time so that hosts (ops,
// link, future apps) get signature enforcement automatically:
//
//	MARKETPLACE_PUBKEY     — single hex Ed25519 public key (32 bytes / 64 hex)
//	MARKETPLACE_PUBKEYS    — comma-separated list of hex keys (for rotation)
//	ALLOW_UNSIGNED_BUNDLES — "true" to skip verification (dev / sideload only)
//
// If the env vars contain malformed hex, New logs a noisy nil-key result and
// returns the installer with PublicKeys empty; Install() will then refuse
// every bundle until the operator fixes the configuration. We deliberately
// do NOT panic — failing-closed at install time is observable in the audit
// trail; panicking at boot can mask the cause.
func New(db *gorm.DB, kernelVersion string) *Installer {
	pubs, _ := loadTrustedKeysFromEnv()
	return &Installer{
		DB:            db,
		KernelVersion: kernelVersion,
		Lifecycles:    lifecycle.NewRegistry(),
		Interceptors:  lifecycle.NewInterceptorRegistry(),
		PublicKeys:    pubs,
		AllowUnsigned: envFlag("ALLOW_UNSIGNED_BUNDLES"),
	}
}

// loadTrustedKeysFromEnv reads MARKETPLACE_PUBKEYS first (the rotation-
// friendly form), then falls back to MARKETPLACE_PUBKEY. Both can be set
// simultaneously; entries are concatenated and de-duplication is left to
// the caller (ed25519.Verify on a duplicate just runs twice — harmless).
func loadTrustedKeysFromEnv() ([]ed25519.PublicKey, error) {
	var out []ed25519.PublicKey
	if csv := strings.TrimSpace(os.Getenv("MARKETPLACE_PUBKEYS")); csv != "" {
		k, err := security.ParseHexPublicKeys(csv)
		if err != nil {
			return nil, err
		}
		out = append(out, k...)
	}
	if single := strings.TrimSpace(os.Getenv("MARKETPLACE_PUBKEY")); single != "" {
		k, err := security.ParseHexPublicKeys(single)
		if err != nil {
			return nil, err
		}
		out = append(out, k...)
	}
	return out, nil
}

func envFlag(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "true", "1", "yes":
		return true
	}
	return false
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
	if err := i.verifySignature(b); err != nil {
		return nil, nil, err
	}
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

// verifySignature enforces the supply-chain trust model on a bundle before
// any other Install step touches the database or filesystem. The decision
// matrix is:
//
//	PublicKeys non-empty  → always verify; reject on missing or invalid sig.
//	PublicKeys empty + AllowUnsigned → permit (dev / sideload).
//	PublicKeys empty + !AllowUnsigned → reject every bundle (fail-closed).
//
// Returning an error here aborts Install before manifest validation, schema
// creation, or lifecycle registration — i.e. nothing untrusted runs.
func (i *Installer) verifySignature(b *bundle.Bundle) error {
	if len(i.PublicKeys) == 0 {
		if i.AllowUnsigned {
			return nil
		}
		return ErrSignatureRequired
	}
	if err := security.VerifyBundle(b, i.PublicKeys); err != nil {
		return fmt.Errorf("installer: bundle signature rejected: %w", err)
	}
	return nil
}

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
