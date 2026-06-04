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
// PublicKeys is loaded by New() in priority order:
//
//  1. MARKETPLACE_PUBKEY (single key) or MARKETPLACE_PUBKEYS (comma-
//     separated, for rotation) — explicit operator pin. Customer-on-VPS
//     and air-gapped deployments that want a deterministic trust anchor
//     set this.
//  2. {MARKETPLACE_URL}/v1/marketplace/pubkey — fetched at construction
//     time when no env key is pinned. This is the central marketplace
//     trust anchor (Let's Encrypt-style: ONE hub-owned key counter-signs
//     every bundle, so consumers trust ONE key instead of the union of
//     every developer pubkey). MARKETPLACE_URL defaults to
//     https://hub.asteby.com so the SaaS path needs zero env config. See
//     central_trust.go for the fetch semantics.
//
// When PublicKeys ends up empty AND ALLOW_UNSIGNED_BUNDLES is not "true",
// Install() refuses to proceed — production deploys MUST resolve at least
// one trust anchor. The unsigned escape hatch exists for local development
// and self-hosted sideloading only.
//
// ops's services/addonbundle/signature.go performs an equivalent check
// before reaching the kernel; that path remains as defense-in-depth but is
// now redundant for the standard install flow. Direct kernel users (link,
// future apps) get the verification for free through this package.
package installer

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/asteby/metacore-kernel/bundle"
	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/i18n"
	"github.com/asteby/metacore-kernel/lifecycle"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/obs"
	"github.com/asteby/metacore-kernel/security"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Installation is the kernel's persisted record. Host apps may extend it.
type Installation struct {
	ID uuid.UUID `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	// The composite unique index is named table-specifically (NOT the bare
	// "idx_org_addon") because Postgres index names are unique per schema: a
	// host that also owns an addon_installations/marketplace_installations table
	// with its own "idx_org_addon" would collide, and CREATE INDEX IF NOT EXISTS
	// / GORM's HasIndex both match by NAME — so the index here would silently
	// never get created, breaking the ON CONFLICT upsert with 42P10.
	OrganizationID uuid.UUID `gorm:"type:uuid;not null;uniqueIndex:uq_metacore_installations_org_addon" json:"organization_id"`
	AddonKey       string    `gorm:"size:100;not null;uniqueIndex:uq_metacore_installations_org_addon" json:"addon_key"`
	Version        string    `gorm:"size:40;not null" json:"version"`
	Status         string    `gorm:"size:20;not null;default:'enabled'" json:"status"`
	Source         string    `gorm:"size:20;not null" json:"source"` // compiled | bundle | federated
	SecretHash     string    `gorm:"size:128" json:"-"`              // hash of install-time secret
	// SecretEnc stores the per-installation HMAC secret encrypted with the
	// host's master key (AES-256-GCM, base64). Only populated when the
	// Installer is constructed with a MasterKey. Hosts with a KMS can ignore
	// this and supply their own SecretResolver.
	SecretEnc string         `gorm:"size:512;column:secret_enc" json:"-"`
	Settings  map[string]any `gorm:"serializer:json;type:jsonb" json:"settings"`
	// ManifestHash is the SHA-256 of the manifest as it landed on disk. The
	// installer recomputes it on every Install and compares against the
	// previous value to drive ManifestChangeBroadcaster hot-swap signals so
	// the frontend can invalidate its metadata-cache without polling. Empty
	// on legacy rows that pre-date this column — they are auto-populated on
	// the first reinstall.
	ManifestHash string     `gorm:"size:80;column:manifest_hash" json:"manifest_hash,omitempty"`
	InstalledAt  time.Time  `gorm:"autoCreateTime" json:"installed_at"`
	EnabledAt    *time.Time `json:"enabled_at,omitempty"`
	DisabledAt   *time.Time `json:"disabled_at,omitempty"`
}

func (Installation) TableName() string { return "metacore_installations" }

// installationOrgAddonIndex is the table-specific name of the composite unique
// index. It MUST stay in sync with the struct tag above. It is deliberately not
// the bare "idx_org_addon": that name is commonly used by host-owned tables
// (e.g. ops' addon_installations) and Postgres index names are unique per
// schema, so a generic name silently no-ops here (IF NOT EXISTS / HasIndex match
// by name) and leaves the ON CONFLICT upsert without its constraint → 42P10.
const installationOrgAddonIndex = "uq_metacore_installations_org_addon"

// ensureInstallationUniqueIndex idempotently creates the composite unique index
// over (organization_id, addon_key) that Install's ON CONFLICT upsert targets.
// It does NOT rely on GORM's AutoMigrate index reconciliation, which has been
// observed to leave a pre-existing table (created by an older kernel) without
// this index — making the upsert fail with Postgres 42P10. The index columns
// (not its name) are what ON CONFLICT matches, so any unique index over exactly
// these two columns satisfies the clause. Table name and columns are trusted
// literals — no injection surface.
func ensureInstallationUniqueIndex(db *gorm.DB) error {
	tbl := (Installation{}).TableName()
	switch db.Dialector.Name() {
	case "postgres", "sqlite", "sqlite3":
		return db.Exec(fmt.Sprintf(
			"CREATE UNIQUE INDEX IF NOT EXISTS %s ON %s (organization_id, addon_key)",
			installationOrgAddonIndex, tbl)).Error
	default:
		// Dialects without a portable CREATE INDEX IF NOT EXISTS (e.g. MySQL):
		// fall back to GORM's migrator, which builds the index from the struct
		// tag when absent.
		m := db.Migrator()
		if m.HasIndex(&Installation{}, installationOrgAddonIndex) {
			return nil
		}
		return m.CreateIndex(&Installation{}, installationOrgAddonIndex)
	}
}

// Installer wires the collaborators the kernel needs.
type Installer struct {
	DB            *gorm.DB
	KernelVersion string
	Lifecycles    *lifecycle.Registry
	Interceptors  *lifecycle.InterceptorRegistry

	// HookRunner reads manifest.LifecycleHooks at install/enable/disable/
	// uninstall and dispatches each declared HookDef through its registered
	// HookDispatchers. Optional: a nil runner skips lifecycle-event hook
	// dispatch entirely (the pre-implementation behaviour). When set, hosts
	// register dispatchers for "wasm" and "webhook" before calling Install.
	HookRunner *lifecycle.HookRunner

	// DynamicHooks is the dynamic.HookRegistry the installer projects
	// manifest CRUD hooks into. Optional: when nil the installer skips the
	// CRUD plane wiring. Hosts wiring both fields get full lifecycle +
	// CRUD coverage; hosts that only want lifecycle (install/enable/...)
	// can leave DynamicHooks nil.
	DynamicHooks *dynamic.HookRegistry

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
	// (in priority order):
	//
	//   1. MARKETPLACE_PUBKEY / MARKETPLACE_PUBKEYS env (explicit pins).
	//   2. The central marketplace pubkey fetched from
	//      {MARKETPLACE_URL}/v1/marketplace/pubkey when no env key is set.
	//      MARKETPLACE_URL defaults to https://hub.asteby.com.
	//
	// When empty AND AllowUnsigned is false, Install() refuses every bundle
	// — fail-closed is the kernel default. Set AllowUnsigned (env
	// ALLOW_UNSIGNED_BUNDLES=true) only in local dev / self-hosted sideloads.
	//
	// Hosts that need to add a key at runtime (rotation without restart)
	// call AppendTrustedPubKey on the live Installer.
	PublicKeys []ed25519.PublicKey

	// AllowUnsigned opts out of signature verification entirely. Tied to the
	// ALLOW_UNSIGNED_BUNDLES env var by New(). DO NOT enable in production.
	AllowUnsigned bool

	// schemaApplier runs the dynamic schema / migration steps an Upgrade
	// performs (EnsureSchema → Apply → CreateTable/SyncSchema). Production
	// hosts leave this nil and the real Postgres-backed helpers fire. Tests
	// (sqlite-only) inject a stub to assert the Upgrade fire-site without a
	// real Postgres instance. Unexported on purpose — the field is an
	// implementation detail of the test harness, not a public extension
	// point.
	schemaApplier schemaApplier

	// Broadcaster is invoked after a successful Install whenever the
	// computed manifest hash differs from what was previously persisted (or
	// when there is no previous record — the first install also fires an
	// event with OldHash=""). Defaults to NoopBroadcaster so hosts that do
	// not surface a real-time channel keep working unchanged.
	//
	// Wired via WithBroadcaster (Law 2 — opinionated default, pluggable
	// escape hatch). The installer never imports kernel/ws directly; the
	// host glue in bridge/installer_broadcaster.go is what closes the loop.
	Broadcaster ManifestChangeBroadcaster

	// CompiledHandlers is the optional host registry the installer cross-checks
	// declared `handler.type=compiled` symbols against at Install/Upgrade time
	// (S7 bootstrap validation). When set, an addon that references a compiled
	// handler the host has not registered fails the install with a clear error
	// instead of installing and then 403-ing on first dispatch. When nil
	// (default) the installer downgrades to a logged warning per declared
	// compiled handler, so hosts that have not adopted the registry keep
	// working unchanged. Wired via WithCompiledHandlers. See compiled_handlers.go.
	CompiledHandlers CompiledHandlerRegistry

	// BackendRuntime materialises an addon's in-process backend (today: WASM)
	// after the database state has been persisted. Defaults to a no-op so
	// hosts that don't run in-process backends (CLI tools, schema-only smoke
	// tests) keep working unchanged. Hosts that do run WASM wire an adapter
	// over host.LoadWASMFromBundle via WithBackendRuntime. See
	// backend_runtime.go for the contract.
	BackendRuntime BackendRuntimeLoader
}

// ErrSignatureRequired is returned by Install when the host has not configured
// any trusted public keys and ALLOW_UNSIGNED_BUNDLES is not set. Misconfigured
// production deploys hit this on every install attempt.
var ErrSignatureRequired = errors.New("installer: signature verification is required (set MARKETPLACE_URL to a reachable hub, or MARKETPLACE_PUBKEY explicitly, or — for dev — ALLOW_UNSIGNED_BUNDLES=true)")

// ErrNotInstalled is returned by Upgrade when the (orgID, addonKey) pair has
// no row in metacore_installations. Hosts surface it as "install first" guidance.
var ErrNotInstalled = errors.New("installer: addon is not installed for this organization")

// ErrCannotDowngrade is returned by Upgrade when newBundle.Manifest.Version
// sorts strictly lower than the currently-installed version. Downgrades are
// refused by default because rolled-back migrations are an addon-author
// concern, not a kernel responsibility — the kernel cannot safely "undo" a
// schema migration that the new (older) manifest no longer ships. Hosts that
// need downgrade semantics (e.g. rollback after a bad release) Uninstall +
// Install at their own risk.
var ErrCannotDowngrade = errors.New("installer: cannot downgrade addon (new version is older than installed version)")

// ErrSameVersionUpgrade is returned by Upgrade when newBundle.Manifest.Version
// equals the currently-installed version. Re-installing the same version is
// a no-op upgrade and the caller almost certainly meant Install (which is
// idempotent for unchanged manifests) or a forced reinstall.
var ErrSameVersionUpgrade = errors.New("installer: upgrade target version matches installed version (use Install for reinstall)")

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
	// Central trust anchor: when no key is pinned via env we try to fetch
	// the hub's marketplace pubkey at boot. Best-effort + logged — see
	// central_trust.go for the contract.
	pubs = loadCentralPubKeyIfNeeded(pubs)
	return &Installer{
		DB:             db,
		KernelVersion:  kernelVersion,
		Lifecycles:     lifecycle.NewRegistry(),
		Interceptors:   lifecycle.NewInterceptorRegistry(),
		PublicKeys:     pubs,
		AllowUnsigned:  envFlag("ALLOW_UNSIGNED_BUNDLES"),
		Broadcaster:    NoopBroadcaster{},
		BackendRuntime: NoopBackendRuntimeLoader{},
	}
}

// WithBackendRuntime swaps the installer's backend runtime loader and
// returns the receiver so callers can chain it on construction:
//
//	inst := installer.New(db, "2.0.0").
//	    WithBackendRuntime(bridge.NewWASMBackendLoader(hostApp))
//
// Passing nil reverts to NoopBackendRuntimeLoader so a host can opt out
// at runtime without panicking the install flow. The default zero-value
// installer also keeps working — Install / Upgrade only invoke the
// loader when manifest.Backend.Runtime selects an in-process runtime.
func (i *Installer) WithBackendRuntime(l BackendRuntimeLoader) *Installer {
	if l == nil {
		i.BackendRuntime = NoopBackendRuntimeLoader{}
		return i
	}
	i.BackendRuntime = l
	return i
}

// WithBroadcaster swaps the installer's manifest-change broadcaster and
// returns the receiver so callers can chain it on construction:
//
//	inst := installer.New(db, "2.0.0").
//	    WithBroadcaster(bridge.NewWSManifestBroadcaster(hub))
//
// Passing nil reverts to NoopBroadcaster so a host can opt out at runtime
// without panicking the install flow.
func (i *Installer) WithBroadcaster(b ManifestChangeBroadcaster) *Installer {
	if b == nil {
		i.Broadcaster = NoopBroadcaster{}
		return i
	}
	i.Broadcaster = b
	return i
}

// WithCompiledHandlers wires the host's compiled-handler registry so the
// installer hard-validates declared `handler.type=compiled` symbols at install
// time (S7). Passing nil disables the gate (the installer falls back to a
// logged warning per declared compiled handler). Returns the receiver so it
// chains on construction:
//
//	inst := installer.New(db, "2.0.0").
//	    WithCompiledHandlers(installer.CompiledHandlerRegistryFunc(ops.HasCompiledHandler))
func (i *Installer) WithCompiledHandlers(r CompiledHandlerRegistry) *Installer {
	i.CompiledHandlers = r
	return i
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
//  6. broadcasts a ManifestChangeEvent when the manifest hash changes
//     (first install too — OldHash is empty in that case) so frontends can
//     drop their metadata-cache without polling. Broadcaster errors are
//     logged and do NOT roll back the install.
//
// Returns the per-installation secret (caller shares only with the addon).
func (i *Installer) Install(orgID uuid.UUID, b *bundle.Bundle) (*Installation, []byte, error) {
	if err := i.verifySignature(b); err != nil {
		return nil, nil, err
	}
	warnings, err := b.Manifest.ValidateAdvisory(i.KernelVersion)
	if err != nil {
		return nil, nil, err
	}
	for _, w := range warnings {
		slog.Warn("manifest.advisory",
			"addon", b.Manifest.Key,
			"version", b.Manifest.Version,
			"warning", w)
	}
	// S7 bootstrap gate: every declared compiled handler must resolve against
	// the host registry (when wired) BEFORE any DB/schema state is created, so
	// a missing implementation fails the install fast instead of leaking into a
	// runtime 403. A nil registry downgrades this to a logged warning inside
	// validateCompiledHandlers.
	if err := validateCompiledHandlers(b, i.CompiledHandlers); err != nil {
		return nil, nil, err
	}
	if err := i.DB.AutoMigrate(&Installation{}); err != nil {
		return nil, nil, err
	}
	// Self-heal the composite unique index the upsert below depends on. GORM's
	// AutoMigrate does NOT reliably add a multi-column `uniqueIndex` to a table
	// that already exists (observed on prod: metacore_installations carried only
	// its primary key). The idempotent persist further down uses ON CONFLICT
	// (organization_id, addon_key), and Postgres rejects that with 42P10 ("no
	// unique or exclusion constraint matching the ON CONFLICT specification")
	// unless a unique index over exactly those columns exists. Ensure it
	// explicitly so the installer is correct regardless of how — or by which
	// kernel version — the table was first created.
	if err := ensureInstallationUniqueIndex(i.DB); err != nil {
		return nil, nil, fmt.Errorf("ensure installation unique index: %w", err)
	}
	// Snapshot the previous manifest hash BEFORE we run the rest of Install
	// so a post-persist comparison can distinguish "fresh install"
	// (OldHash="") from "hash unchanged" from "hot-swap".
	prevHash, _ := loadPrevManifestHash(i.DB, orgID, b.Manifest.Key)
	newHash := manifestHashFromBundle(b)
	iso := dynamic.ParseIsolation(b.Manifest.TenantIsolation)
	if err := dynamic.EnsureSchema(i.DB, b.Manifest.Key, orgID, iso); err != nil {
		return nil, nil, err
	}
	// Materialise the model_definitions tables (the manifest is the schema
	// source of truth) BEFORE running the SQL migrations. An additive migration
	// that ALTERs a model table must find it already present; running migrations
	// first (the old order) broke installs whose 00N migration altered a table
	// the migrations themselves didn't (re)create — e.g. addon-pos 002 ALTERing
	// addon_pos.pos_sessions, addon-inventory 002 ALTERing fulfillments. With
	// metadata-driven creation first, every ALTER lands on an existing table and
	// the migrations are pure additive alignment on top.
	for _, def := range b.Manifest.ModelDefinitions {
		if err := dynamic.CreateTable(i.DB, b.Manifest.Key, orgID, iso, def); err != nil {
			return nil, nil, err
		}
		if err := dynamic.SyncSchema(i.DB, b.Manifest.Key, orgID, iso, def); err != nil {
			return nil, nil, err
		}
	}
	if err := dynamic.Apply(i.DB, b.Manifest.Key, orgID, iso, b.Migrations); err != nil {
		return nil, nil, err
	}
	// Install doubles as the marketplace "Actualizar" (upgrade) path — the
	// embed re-runs Install with the NEW bundle rather than calling Upgrade.
	// Refresh the in-memory lifecycle so its Manifest().Version reflects the
	// new bundle: this registry is the SINGLE source the host's
	// /metacore/manifests endpoint reports installed versions from, and a stale
	// entry here is exactly what left the marketplace "Actualización disponible"
	// badge stuck on the old version after an upgrade (addon_manifests + the
	// host registries updated, but this lifecycle didn't).
	refreshDeclarativeLifecycle(i.Lifecycles, b.Manifest)
	lc, _ := i.Lifecycles.Get(b.Manifest.Key)
	if err := lc.OnInstall(i.DB, orgID); err != nil {
		return nil, nil, fmt.Errorf("OnInstall: %w", err)
	}
	if err := i.runLifecycleHooks(context.Background(), b.Manifest, orgID, lifecycle.HookEventInstall); err != nil {
		return nil, nil, fmt.Errorf("lifecycle hook install: %w", err)
	}
	if err := lc.OnEnable(i.DB, orgID); err != nil {
		return nil, nil, fmt.Errorf("OnEnable: %w", err)
	}
	if err := i.runLifecycleHooks(context.Background(), b.Manifest, orgID, lifecycle.HookEventEnable); err != nil {
		return nil, nil, fmt.Errorf("lifecycle hook enable: %w", err)
	}
	// Wire the manifest's CRUD hooks (before_*/after_*) into the dynamic
	// engine's HookRegistry. Idempotent — the registry first removes any
	// previous registration for this addon before re-adding the new shape,
	// so reinstalls don't double-fire.
	if i.DynamicHooks != nil && i.HookRunner != nil {
		i.DynamicHooks.RegisterManifestHooks(b.Manifest.Key, b.Manifest, i.HookRunner)
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
		ManifestHash:   newHash,
		EnabledAt:      &now,
	}
	if len(i.MasterKey) == 32 {
		enc, err := security.Encrypt(i.MasterKey, secret)
		if err != nil {
			return nil, nil, fmt.Errorf("encrypt secret: %w", err)
		}
		inst.SecretEnc = enc
	}
	// Idempotent persist. Because Install is also the upgrade path (see the
	// lifecycle refresh above), an installation row may already exist for this
	// (org, addon) — a blind Create would violate the idx_org_addon unique
	// index and the re-install would fail. Upsert instead: refresh the
	// versioned columns (so metacore_installations.Version tracks the new
	// bundle, keeping telemetry/rollback consistent with the lifecycle) while
	// PRESERVING the install-time secret, per-org settings, id and created_at
	// (those columns are intentionally absent from DoUpdates).
	if err := i.DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "organization_id"}, {Name: "addon_key"}},
		DoUpdates: clause.AssignmentColumns([]string{"version", "manifest_hash", "status", "source", "enabled_at"}),
	}).Create(inst).Error; err != nil {
		return nil, nil, err
	}
	// Persist any i18n catalog the manifest declares so consumers (frontend
	// translator, hub catalog renderer, conversational hosts) can look the
	// strings up via i18n.GetAddonI18n. Idempotent — re-running Install on
	// the same manifest is a deterministic upsert. We log-and-continue on
	// error because i18n is a nice-to-have (the addon stays installed even
	// if string registration hiccups); a hard failure here would mask a
	// successful schema/lifecycle install.
	if err := i18n.RegisterAddonI18n(context.Background(), i.DB, b.Manifest.Key, b.Manifest); err != nil {
		slog.Warn("installer.i18n_register_failed",
			"addon_key", b.Manifest.Key,
			"err", err.Error())
	}
	// Materialize federation artifacts to disk so the host's static route can
	// serve them. Non-federation bundles (no frontend, or "script" format) are
	// a no-op. An empty FrontendBasePath opts the host out entirely.
	if b.Manifest.Frontend != nil && b.Manifest.Frontend.Format == "federation" {
		if err := WriteFrontend(i.FrontendBasePath, b.Manifest.Key, b.Frontend); err != nil {
			return nil, nil, fmt.Errorf("WriteFrontend: %w", err)
		}
	}
	// Materialise the addon's in-process backend runtime when the manifest
	// declares one (WASM today; "binary" reserved). The loader reads the
	// declared entry path (defaulting to backend/backend.wasm) out of
	// bundle.Backend, registers it on the host's runtime, and cross-checks
	// the declared exports — the same fields the v0.18.0 action-trigger
	// validator gates on. Skipped silently when the manifest has no
	// backend block or selects the legacy "webhook" runtime.
	if needsBackendRuntime(b.Manifest.Backend) {
		loader := i.BackendRuntime
		if loader == nil {
			loader = NoopBackendRuntimeLoader{}
		}
		if err := loader.LoadFromBundle(context.Background(), b); err != nil {
			return nil, nil, fmt.Errorf("LoadBackendRuntime: %w", err)
		}
	}
	// Broadcast the manifest-hash change (or first-install signal). Errors
	// are logged but never fail the install — the WebSocket fan-out is a
	// nice-to-have, not part of the install contract.
	action := ChangeActionInstall
	if prevHash != "" {
		// Same-org reinstall: from the consumer's perspective this is an
		// upgrade-shaped event (the version may not have moved but the
		// manifest/bundle did). Uninstall has its own branch in Uninstall.
		action = ChangeActionUpgrade
	}
	assetPaths, contentHash := frontendAssetDigest(b)
	maybeBroadcastManifestChange(context.Background(), i.Broadcaster, ManifestChangeEvent{
		OrgID:       orgID,
		AddonKey:    b.Manifest.Key,
		OldHash:     prevHash,
		NewHash:     newHash,
		Version:     b.Manifest.Version,
		Timestamp:   now,
		Action:      action,
		AssetPaths:  assetPaths,
		ContentHash: contentHash,
	})
	return inst, secret, nil
}

// loadPrevManifestHash reads the previous manifest hash for (orgID, addonKey)
// directly from metacore_installations. Returns ("", nil) when there's no
// prior row — the caller distinguishes "first install" from "hash unchanged"
// off the empty string and the new hash respectively. Errors are propagated
// to the caller but Install treats them as "no prior hash known" so a flaky
// read can't block an install.
func loadPrevManifestHash(db *gorm.DB, orgID uuid.UUID, addonKey string) (string, error) {
	if db == nil {
		return "", nil
	}
	var row struct {
		ManifestHash string `gorm:"column:manifest_hash"`
	}
	err := db.Table((Installation{}).TableName()).
		Select("manifest_hash").
		Where("organization_id = ? AND addon_key = ?", orgID, addonKey).
		Take(&row).Error
	if err == gorm.ErrRecordNotFound {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return row.ManifestHash, nil
}

// manifestHashFromBundle returns the canonical SHA-256 fingerprint of an
// addon's manifest. It prefers the digest the bundle reader already computed
// over manifest.json (zero-cost on the install hot path) and falls back to a
// fresh marshal+hash when the bundle was constructed in-memory (e.g. tests
// or compiled-in addons). The Signature field is stripped before hashing so
// re-signing the same manifest does not appear as a hot-swap.
func manifestHashFromBundle(b *bundle.Bundle) string {
	if b == nil {
		return ""
	}
	if dig, ok := b.EntryDigests["manifest.json"]; ok && dig != "" {
		return "sha256:" + dig
	}
	return manifestHash(b.Manifest)
}

// manifestHash returns "sha256:<hex>" over a stable JSON encoding of m. The
// Signature field is zeroed first because the kernel hashes the *contents*
// the frontend cares about — re-signing the same manifest with a new
// timestamp must NOT flip the hash and cause spurious cache invalidations.
func manifestHash(m manifest.Manifest) string {
	m.Signature = nil
	raw, err := json.Marshal(m)
	if err != nil {
		// json.Marshal on the manifest type cannot fail in practice — all
		// fields are JSON-clean. The empty hash falls through as "" so the
		// broadcaster treats it as "no hash known" rather than panicking.
		return ""
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// maybeBroadcastManifestChange forwards the event to the configured
// broadcaster whenever the new hash differs from the previous one. Pure
// helper extracted so unit tests can exercise the diff logic without a DB
// or a bundle pipeline. The function is intentionally non-fatal: a nil
// broadcaster or a broadcast error is logged and swallowed.
func maybeBroadcastManifestChange(ctx context.Context, bcast ManifestChangeBroadcaster, evt ManifestChangeEvent) {
	if bcast == nil {
		return
	}
	if evt.NewHash == "" {
		// No hash known (in-memory bundle without EntryDigests AND a
		// manifest that failed to marshal — should be impossible). Skip
		// the broadcast rather than spam consumers with empty events.
		return
	}
	if evt.OldHash == evt.NewHash {
		return
	}
	if err := bcast.Broadcast(ctx, evt); err != nil {
		obs.Warn(ctx, "installer.manifest_broadcast_failed",
			slog.String("addon_key", evt.AddonKey),
			slog.String("org_id", evt.OrgID.String()),
			slog.String("err", err.Error()),
		)
	}
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
	if err := i.runLifecycleHooks(context.Background(), lc.Manifest(), orgID, lifecycle.HookEventEnable); err != nil {
		return fmt.Errorf("lifecycle hook enable: %w", err)
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
	if err := i.runLifecycleHooks(context.Background(), lc.Manifest(), orgID, lifecycle.HookEventDisable); err != nil {
		return fmt.Errorf("lifecycle hook disable: %w", err)
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
		if err := i.runLifecycleHooks(context.Background(), lc.Manifest(), orgID, lifecycle.HookEventDisable); err != nil {
			return fmt.Errorf("lifecycle hook disable: %w", err)
		}
		if err := lc.OnUninstall(i.DB, orgID); err != nil {
			return err
		}
		if err := i.runLifecycleHooks(context.Background(), lc.Manifest(), orgID, lifecycle.HookEventUninstall); err != nil {
			return fmt.Errorf("lifecycle hook uninstall: %w", err)
		}
	}
	if err := i.DB.Where("organization_id = ? AND addon_key = ?", orgID, addonKey).
		Delete(&Installation{}).Error; err != nil {
		return err
	}
	i.Interceptors.UnregisterAddon(addonKey)
	// Drop any manifest-driven CRUD hooks the installer projected at
	// Install time. Safe to call even when the addon never registered any
	// (UnregisterAddon is a no-op for unknown keys).
	if i.DynamicHooks != nil {
		i.DynamicHooks.UnregisterAddon(addonKey)
	}
	// Drop the persisted i18n catalog so removed addons stop surfacing
	// stale translations. Best-effort: a transient DB error here doesn't
	// invalidate the rest of the uninstall.
	if err := i18n.UnregisterAddonI18n(context.Background(), i.DB, addonKey); err != nil {
		slog.Warn("installer.i18n_unregister_failed",
			"addon_key", addonKey,
			"err", err.Error())
	}
	// Broadcast uninstall so frontends drop their cached federated module
	// and stop probing for assets that no longer exist. We do not have a
	// bundle in hand here (uninstall takes only an addon key), so we fire
	// the event with empty AssetPaths/ContentHash — the Action="uninstall"
	// is sufficient signal for the consumer to evict.
	maybeBroadcastManifestChange(context.Background(), i.Broadcaster, ManifestChangeEvent{
		OrgID:     orgID,
		AddonKey:  addonKey,
		OldHash:   "",
		NewHash:   "uninstalled",
		Version:   "",
		Timestamp: time.Now(),
		Action:    ChangeActionUninstall,
	})
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

// Upgrade applies a newer bundle on top of an existing installation.
//
// Pre-flight checks (fail-closed before any state mutation):
//
//  1. Signature verification on the new bundle — same gate as Install.
//  2. Manifest validation against KernelVersion (advisory warnings logged,
//     hard errors abort).
//  3. The (orgID, addonKey) row must exist — otherwise ErrNotInstalled.
//  4. newBundle.Manifest.Version MUST be strict-greater than the installed
//     version (semver). Equal version returns ErrSameVersionUpgrade; lower
//     version returns ErrCannotDowngrade. The kernel does not "undo"
//     migrations — downgrades are a host concern.
//
// Flow once preflight passes:
//
//	a. Dispatch lifecycle_hooks["upgrade"] (BEFORE) with payload
//	   {event, addon_key, org_id, from_version, to_version}. A non-nil
//	   error aborts: the installed row remains untouched, no migrations
//	   are applied, no bundle artifacts are replaced.
//	b. Apply migrations declared in the new bundle. dynamic.Apply is
//	   idempotent — migrations the previous version already ran are
//	   skipped by (addon_key, version) lookup; only new files execute.
//	   Migration count is captured in the post-hook payload as
//	   migrations_applied.
//	c. CreateTable / SyncSchema each model definition in the new manifest
//	   (additive — old tables/columns are not dropped to preserve data).
//	d. Replace the lifecycle.Addon registration with the new manifest
//	   (so subsequent Enable/Disable/CRUD see the new declarations).
//	e. Re-project manifest CRUD hooks (UnregisterAddon + RegisterManifestHooks
//	   on the dynamic.HookRegistry) so before_*/after_* dispatchers point
//	   at the new declarations.
//	f. Materialise new frontend artifacts to disk (federation only). The
//	   old assets are overwritten in place — atomic per-file write.
//	g. Update metacore_installations row (version, manifest_hash,
//	   settings merge: new defaults add, existing user values keep).
//	h. Dispatch lifecycle_hooks["upgrade"] AFTER with the same payload
//	   shape plus migrations_applied.
//	i. Broadcast a ManifestChangeEvent (hash will diverge by construction).
//
// On any error AFTER the BEFORE hook has fired but BEFORE the version row
// has been committed, Upgrade attempts a best-effort rollback: the row is
// not yet updated so the installation continues to read as the old version.
// Applied migrations are NOT rolled back (Postgres does not undo DDL on a
// per-version basis); however a future Upgrade attempt with the same target
// version will skip them as already-applied. The error is returned wrapped
// with the failed stage for operator triage.
//
// Upgrade does NOT change the per-installation secret. Rotation is an
// orthogonal concern — call RotateSecret if the new bundle introduces new
// signed-webhook endpoints that need a fresh key.
func (i *Installer) Upgrade(ctx context.Context, orgID uuid.UUID, newBundle *bundle.Bundle) (*Installation, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if newBundle == nil {
		return nil, fmt.Errorf("installer.Upgrade: bundle is required")
	}
	if err := i.verifySignature(newBundle); err != nil {
		return nil, err
	}
	warnings, err := newBundle.Manifest.ValidateAdvisory(i.KernelVersion)
	if err != nil {
		return nil, err
	}
	for _, w := range warnings {
		slog.Warn("manifest.advisory",
			"addon", newBundle.Manifest.Key,
			"version", newBundle.Manifest.Version,
			"warning", w)
	}
	// S7 bootstrap gate (symmetric with Install): a version bump must not
	// introduce a compiled handler the host cannot resolve. Validated before
	// the BEFORE upgrade hook fires / any schema work runs. Nil registry → warn.
	if err := validateCompiledHandlers(newBundle, i.CompiledHandlers); err != nil {
		return nil, err
	}

	// Pre-flight: the installation row must exist; capture its current
	// version so we can guard against same-version / downgrade attempts
	// before doing any DB work.
	var existing Installation
	err = i.DB.Where("organization_id = ? AND addon_key = ?", orgID, newBundle.Manifest.Key).
		Take(&existing).Error
	if err == gorm.ErrRecordNotFound {
		return nil, ErrNotInstalled
	}
	if err != nil {
		return nil, fmt.Errorf("installer.Upgrade: lookup installation: %w", err)
	}
	fromVersion := existing.Version
	toVersion := newBundle.Manifest.Version
	if err := guardUpgradeVersion(fromVersion, toVersion); err != nil {
		return nil, err
	}

	// Build the lifecycle payload once — the BEFORE hook reads it as-is;
	// the AFTER hook re-marshals with migrations_applied attached.
	beforePayload := map[string]any{
		"event":        lifecycle.HookEventUpgrade,
		"addon_key":    newBundle.Manifest.Key,
		"org_id":       orgID.String(),
		"from_version": fromVersion,
		"to_version":   toVersion,
		"phase":        "before",
	}
	if err := i.runLifecycleHooksWithPayload(ctx, newBundle.Manifest, orgID, lifecycle.HookEventUpgrade, beforePayload); err != nil {
		return nil, fmt.Errorf("lifecycle hook upgrade (before): %w", err)
	}

	// Apply migrations and schema changes. dynamic.Apply is keyed on
	// (addon_key, version) so previously-applied files are skipped — only
	// new files declared in the upgrade bundle execute.
	iso := dynamic.ParseIsolation(newBundle.Manifest.TenantIsolation)
	applier := i.schemaApplier
	if applier == nil {
		applier = defaultSchemaApplier{}
	}
	migrationsApplied, err := countAppliedMigrations(i.DB, newBundle.Manifest.Key, newBundle.Migrations)
	if err != nil {
		return nil, fmt.Errorf("installer.Upgrade: count migrations: %w", err)
	}
	if err := applier.ApplyForUpgrade(i.DB, orgID, iso, newBundle); err != nil {
		return nil, fmt.Errorf("installer.Upgrade: apply schema: %w", err)
	}

	// Replace the lifecycle.Addon binding with the new manifest. Compiled
	// addons that registered themselves before Install retain their custom
	// implementation — we only swap when the previous entry was a
	// ManifestOnly (declarative) shim.
	refreshDeclarativeLifecycle(i.Lifecycles, newBundle.Manifest)

	// Re-project manifest CRUD hooks. Idempotent: RegisterManifestHooks
	// first removes the previous registration for this addon, so the new
	// before_*/after_* shape replaces the old one cleanly.
	if i.DynamicHooks != nil && i.HookRunner != nil {
		i.DynamicHooks.RegisterManifestHooks(newBundle.Manifest.Key, newBundle.Manifest, i.HookRunner)
	}

	// Materialise federation artifacts. Federation paths are deterministic
	// per addon key so the new files overwrite the previous version's files
	// in-place. Non-federation bundles skip this.
	if newBundle.Manifest.Frontend != nil && newBundle.Manifest.Frontend.Format == "federation" {
		if err := WriteFrontend(i.FrontendBasePath, newBundle.Manifest.Key, newBundle.Frontend); err != nil {
			return nil, fmt.Errorf("installer.Upgrade: WriteFrontend: %w", err)
		}
	}
	// Reload the addon's in-process backend so the new module replaces the
	// previous compile. wasm.Host.Load already swaps the cached entry and
	// disposes stale per-installation instances; we just need to call it
	// with the new bundle's bytes. Skipped silently when the new manifest
	// has no backend or runtime="webhook".
	if needsBackendRuntime(newBundle.Manifest.Backend) {
		loader := i.BackendRuntime
		if loader == nil {
			loader = NoopBackendRuntimeLoader{}
		}
		if err := loader.LoadFromBundle(ctx, newBundle); err != nil {
			return nil, fmt.Errorf("installer.Upgrade: LoadBackendRuntime: %w", err)
		}
	}

	// Persist the version bump. Settings merge keeps user-tuned values from
	// the old installation and ADDS defaults for any new settings declared
	// by the new manifest — addon authors don't have to teach every host to
	// run a migration just to surface a new toggle.
	prevHash := existing.ManifestHash
	newHash := manifestHashFromBundle(newBundle)
	mergedSettings := mergeSettings(existing.Settings, defaultSettings(newBundle.Manifest))
	now := time.Now()
	// Persist via .Save on the loaded struct so gorm honours the JSON
	// serializer on Settings instead of stringifying the map literal. A
	// raw `.Updates(map[…])` bypasses the field-level serializer and
	// writes "map[k:v]" — which then fails to round-trip on read.
	existing.Version = toVersion
	existing.ManifestHash = newHash
	existing.Settings = mergedSettings
	if err := i.DB.Save(&existing).Error; err != nil {
		return nil, fmt.Errorf("installer.Upgrade: persist row: %w", err)
	}

	// Refresh the persisted i18n catalog so an upgrade picks up renamed /
	// added / removed translation keys. The store purges-then-upserts the
	// addon's rows, so removed keys disappear in lockstep with the new
	// version. Log-and-continue: a stale catalog is preferable to rolling
	// back a successful schema upgrade.
	if err := i18n.RegisterAddonI18n(ctx, i.DB, newBundle.Manifest.Key, newBundle.Manifest); err != nil {
		slog.Warn("installer.upgrade.i18n_register_failed",
			"addon_key", newBundle.Manifest.Key,
			"err", err.Error())
	}

	// AFTER hook fires once the row has committed — its payload includes
	// migrations_applied so audit pipelines can correlate schema drift with
	// the version bump.
	afterPayload := map[string]any{
		"event":              lifecycle.HookEventUpgrade,
		"addon_key":          newBundle.Manifest.Key,
		"org_id":             orgID.String(),
		"from_version":       fromVersion,
		"to_version":         toVersion,
		"phase":              "after",
		"migrations_applied": migrationsApplied,
	}
	if err := i.runLifecycleHooksWithPayload(ctx, newBundle.Manifest, orgID, lifecycle.HookEventUpgrade, afterPayload); err != nil {
		// AFTER errors are logged but do NOT roll back — the upgrade
		// has committed and rolling back DDL is unsafe. Matches the
		// install/enable contract where post-state hooks are best-effort.
		slog.Warn("installer.upgrade.after_hook_failed",
			"addon_key", newBundle.Manifest.Key,
			"org_id", orgID.String(),
			"from_version", fromVersion,
			"to_version", toVersion,
			"err", err.Error())
	}

	upgradeAssets, upgradeHash := frontendAssetDigest(newBundle)
	maybeBroadcastManifestChange(ctx, i.Broadcaster, ManifestChangeEvent{
		OrgID:       orgID,
		AddonKey:    newBundle.Manifest.Key,
		OldHash:     prevHash,
		NewHash:     newHash,
		Version:     toVersion,
		Timestamp:   now,
		Action:      ChangeActionUpgrade,
		AssetPaths:  upgradeAssets,
		ContentHash: upgradeHash,
	})

	return &existing, nil
}

// guardUpgradeVersion enforces strict-greater semver ordering on the upgrade
// target. Non-semver versions on either side fall back to lexicographic
// string compare so legacy installations with calendar-versioned addons
// ("2025.10.01") still benefit from same-version / downgrade guards.
func guardUpgradeVersion(from, to string) error {
	fv, fErr := semver.NewVersion(from)
	tv, tErr := semver.NewVersion(to)
	if fErr == nil && tErr == nil {
		cmp := tv.Compare(fv)
		switch {
		case cmp == 0:
			return ErrSameVersionUpgrade
		case cmp < 0:
			return ErrCannotDowngrade
		}
		return nil
	}
	// Lexicographic fallback — never as accurate as semver but the only
	// thing the kernel can do for arbitrary version strings.
	switch {
	case from == to:
		return ErrSameVersionUpgrade
	case to < from:
		return ErrCannotDowngrade
	}
	return nil
}

// countAppliedMigrations reports how many of the supplied migration files
// will actually execute on the next dynamic.Apply call — i.e. those that
// are not yet recorded in metacore_addon_migrations. The count goes into
// the upgrade AFTER-hook payload so audit consumers can see exactly how
// much schema work a version bump performed.
//
// Failure modes are non-fatal: a DB error returns (0, err) and Upgrade
// surfaces it to the caller; a missing table (first-ever migration) is
// auto-created by dynamic.Apply later in the flow and reads as zero applied
// rows here, so we treat ErrRecordNotFound / "no such table" as zero.
func countAppliedMigrations(db *gorm.DB, addonKey string, files []dynamic.File) (int, error) {
	if len(files) == 0 {
		return 0, nil
	}
	versions := make([]string, 0, len(files))
	for _, f := range files {
		versions = append(versions, f.Version)
	}
	var alreadyApplied int64
	err := db.Model(&dynamic.Migration{}).
		Where("addon_key = ? AND version IN ?", addonKey, versions).
		Count(&alreadyApplied).Error
	if err != nil {
		// Migration table not yet auto-migrated (first-ever migration for
		// any addon) — treat as zero already-applied. Both sqlite and
		// postgres surface this with substring "no such table" / "does not
		// exist"; matching on substrings is more robust than driver-
		// specific error codes for a non-fatal hint.
		es := err.Error()
		if strings.Contains(es, "no such table") || strings.Contains(es, "does not exist") {
			return len(files), nil
		}
		return 0, err
	}
	return len(files) - int(alreadyApplied), nil
}

// mergeSettings combines the previously-persisted settings with the new
// manifest's defaults. User-set values (anything already present in `old`)
// win — the upgrade must NOT clobber a tenant's customisation. Defaults
// that did not exist before are added so addon authors can ship new
// toggles without per-tenant migrations.
func mergeSettings(old, newDefaults map[string]any) map[string]any {
	out := make(map[string]any, len(old)+len(newDefaults))
	for k, v := range newDefaults {
		out[k] = v
	}
	for k, v := range old {
		out[k] = v
	}
	return out
}

// SignerFor rebuilds the HMAC signer for a known secret. Hosts persist the
// cleartext secret in a secrets manager and only store the hash here.
func SignerFor(secret []byte) *security.Signer { return security.NewSigner(secret) }

// runLifecycleHooks dispatches the `event`-family hooks (install, enable,
// disable, uninstall, upgrade) declared in the manifest. A nil HookRunner
// or absent declaration is a silent no-op so legacy hosts and legacy
// manifests both keep working unchanged.
//
// The payload is the JSON envelope `{event, addon_key, org_id, version}` —
// addons receive enough context to disambiguate which install just landed
// without having to read kernel internals. The shape mirrors the CRUD hook
// adapter so guest authors can reuse a single decoder.
func (i *Installer) runLifecycleHooks(ctx context.Context, m manifest.Manifest, orgID uuid.UUID, event string) error {
	if i.HookRunner == nil {
		return nil
	}
	payload, err := json.Marshal(map[string]any{
		"event":     event,
		"addon_key": m.Key,
		"org_id":    orgID.String(),
		"version":   m.Version,
	})
	if err != nil {
		// Marshal of a fixed shape with primitive types cannot fail in
		// practice; fall back to a nil payload rather than aborting the
		// install on an impossible error.
		payload = nil
	}
	return i.HookRunner.Run(ctx, m.Key, orgID, event, m, payload)
}

// runLifecycleHooksWithPayload is the sibling helper Upgrade uses to ship
// extra context (from_version, to_version, phase, migrations_applied) into
// the hook payload — runLifecycleHooks's fixed shape is intentionally minimal
// for the symmetric install/enable/disable/uninstall events. The caller owns
// every key in `payload`; this helper just marshals and dispatches.
func (i *Installer) runLifecycleHooksWithPayload(ctx context.Context, m manifest.Manifest, orgID uuid.UUID, event string, payload map[string]any) error {
	if i.HookRunner == nil {
		return nil
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		// Same rationale as runLifecycleHooks: encoding a map of
		// JSON-clean primitives cannot fail in practice; fall back to a
		// nil payload rather than aborting the upgrade.
		raw = nil
	}
	return i.HookRunner.Run(ctx, m.Key, orgID, event, m, raw)
}

// refreshDeclarativeLifecycle re-registers the addon's lifecycle binding with
// the new manifest IFF the current entry is a declarative ManifestOnly shim
// (or absent). Compiled addons that registered a custom lifecycle.Addon keep
// it untouched. Both Install (the de-facto upgrade path the marketplace embed
// uses) and Upgrade call this so the in-memory Lifecycles registry — the single
// source the host's /metacore/manifests endpoint reports installed versions
// from — never reports a stale version after a re-install/upgrade.
func refreshDeclarativeLifecycle(reg *lifecycle.Registry, m manifest.Manifest) {
	if lc, ok := reg.Get(m.Key); ok {
		if _, isManifestOnly := lc.(*lifecycle.ManifestOnly); !isManifestOnly {
			return // compiled custom lifecycle — preserve it
		}
	}
	reg.Register(m.Key, &lifecycle.ManifestOnly{Data: m})
}

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

// frontendAssetDigest returns the sorted bundle-relative paths of every
// frontend asset and a SHA-256 hex digest covering their concatenated
// bytes in path order. It is the seam ManifestChangeEvent uses so a
// frontend consumer can:
//
//   - invalidate exact asset URLs without guessing paths (paths flow
//     verbatim into the event);
//   - skip a full module reload when only the backend bundle rotated
//     (ContentHash unchanged signals "user-facing surface is the same").
//
// Empty for non-federation bundles, missing frontends, and uninstall
// events. Returned slice is freshly allocated so callers can append to it
// without aliasing bundle internals.
func frontendAssetDigest(b *bundle.Bundle) ([]string, string) {
	if b == nil || b.Manifest.Frontend == nil || b.Manifest.Frontend.Format != "federation" || len(b.Frontend) == 0 {
		return nil, ""
	}
	paths := make([]string, 0, len(b.Frontend))
	for k := range b.Frontend {
		paths = append(paths, k)
	}
	sort.Strings(paths)
	h := sha256.New()
	for _, p := range paths {
		// Include the path itself in the hash so a rename without bytes
		// change still flips ContentHash — frontends watch the right thing.
		h.Write([]byte(p))
		h.Write([]byte{0})
		h.Write(b.Frontend[p])
	}
	return paths, hex.EncodeToString(h.Sum(nil))
}

// needsBackendRuntime reports whether the manifest's Backend block selects
// an in-process runtime that warrants calling BackendRuntimeLoader. The
// legacy "webhook" runtime (or a nil block) is dispatched over HTTP and
// needs no runtime materialisation; "wasm" and the reserved "binary"
// runtimes do. Centralised here so Install / Upgrade stay symmetric and
// future runtime kinds only add to this predicate.
func needsBackendRuntime(spec *manifest.BackendSpec) bool {
	if spec == nil {
		return false
	}
	switch spec.Runtime {
	case "", "webhook":
		return false
	default:
		return true
	}
}

func sourceOf(m manifest.Manifest) string {
	if m.Frontend != nil && m.Frontend.Format == "federation" {
		return "federated"
	}
	return "bundle"
}

// schemaApplier abstracts the dynamic-schema mutations Upgrade performs so
// tests can exercise the lifecycle fire-site without spinning up a Postgres
// instance (kernel/dynamic only supports Postgres syntax). Production keeps
// the default applier which delegates to the dynamic package verbatim.
type schemaApplier interface {
	ApplyForUpgrade(db *gorm.DB, orgID uuid.UUID, iso dynamic.Isolation, b *bundle.Bundle) error
}

// defaultSchemaApplier is the production implementation — runs EnsureSchema,
// then CreateTable / SyncSchema for every model definition (the manifest is the
// schema source of truth), and ONLY THEN dynamic.Apply over the bundle's
// migrations. Materialising the model tables before the SQL migrations means an
// additive migration that ALTERs a model table always finds it present —
// running migrations first broke installs/upgrades whose 00N migration altered
// a table the migrations themselves didn't (re)create (addon-pos 002 →
// pos_sessions, addon-inventory 002 → fulfillments). The migrations become pure
// additive alignment on top of the metadata-driven schema.
type defaultSchemaApplier struct{}

func (defaultSchemaApplier) ApplyForUpgrade(db *gorm.DB, orgID uuid.UUID, iso dynamic.Isolation, b *bundle.Bundle) error {
	if err := dynamic.EnsureSchema(db, b.Manifest.Key, orgID, iso); err != nil {
		return fmt.Errorf("EnsureSchema: %w", err)
	}
	for _, def := range b.Manifest.ModelDefinitions {
		if err := dynamic.CreateTable(db, b.Manifest.Key, orgID, iso, def); err != nil {
			return fmt.Errorf("CreateTable %s: %w", def.ModelKey, err)
		}
		if err := dynamic.SyncSchema(db, b.Manifest.Key, orgID, iso, def); err != nil {
			return fmt.Errorf("SyncSchema %s: %w", def.ModelKey, err)
		}
	}
	if err := dynamic.Apply(db, b.Manifest.Key, orgID, iso, b.Migrations); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}
