package installer

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/bundle"
	"github.com/asteby/metacore-kernel/lifecycle"
)

// embeddedBundle builds the bytes a host would embed (go:embed) and reads them
// back the way the host does at boot.
func embeddedBundle(t *testing.T, key, version string) (*bundle.Bundle, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if err := bundle.Write(&buf, makeBundle(key, version, nil)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	b, err := bundle.Read(bytes.NewReader(buf.Bytes()), 0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return b, buf.Bytes()
}

func TestBuiltin_VerifyMatrix(t *testing.T) {
	b, raw := embeddedBundle(t, "native_mod", "1.0.0")
	digest := BuiltinDigest(raw)

	t.Run("empty registry admits nothing", func(t *testing.T) {
		i := &Installer{}
		if err := i.verifyBuiltin(b); !errors.Is(err, ErrBuiltinNotTrusted) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("registered exact digest is admitted", func(t *testing.T) {
		i := &Installer{}
		if err := i.TrustBuiltin("native_mod", "1.0.0", digest); err != nil {
			t.Fatal(err)
		}
		if err := i.verifyBuiltin(b); err != nil {
			t.Fatalf("trusted builtin rejected: %v", err)
		}
	})

	t.Run("another version of a trusted key is not admitted", func(t *testing.T) {
		i := &Installer{}
		_ = i.TrustBuiltin("native_mod", "1.0.0", digest)
		other, _ := embeddedBundle(t, "native_mod", "1.0.1")
		if err := i.verifyBuiltin(other); !errors.Is(err, ErrBuiltinNotTrusted) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("same key and version but different bytes is rejected", func(t *testing.T) {
		i := &Installer{}
		_ = i.TrustBuiltin("native_mod", "1.0.0", digest)
		tampered := *b
		tampered.Raw = append(append([]byte(nil), raw...), 0)
		if err := i.verifyBuiltin(&tampered); !errors.Is(err, ErrBuiltinNotTrusted) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("trusted Raw paired with a forged manifest is rejected", func(t *testing.T) {
		i := &Installer{}
		_ = i.TrustBuiltin("native_mod", "1.0.0", digest)
		forged := *b
		forged.RawManifest = []byte(`{"forged":true}`)
		if err := i.verifyBuiltin(&forged); !errors.Is(err, ErrBuiltinNotTrusted) {
			t.Fatalf("got %v", err)
		}
		other, _ := embeddedBundle(t, "other_mod", "1.0.0")
		other.Raw = raw
		if err := i.verifyBuiltin(other); !errors.Is(err, ErrBuiltinNotTrusted) {
			t.Fatalf("a bundle claiming another key with trusted bytes: %v", err)
		}
	})

	t.Run("no Raw, nil bundle", func(t *testing.T) {
		i := &Installer{}
		_ = i.TrustBuiltin("native_mod", "1.0.0", digest)
		noRaw := *b
		noRaw.Raw = nil
		if err := i.verifyBuiltin(&noRaw); !errors.Is(err, ErrBuiltinNotTrusted) {
			t.Fatalf("no Raw: %v", err)
		}
		if err := i.verifyBuiltin(nil); !errors.Is(err, ErrBuiltinNotTrusted) {
			t.Fatalf("nil: %v", err)
		}
	})

	t.Run("TrustBuiltin validates and never rebinds a digest", func(t *testing.T) {
		i := &Installer{}
		if err := i.TrustBuiltin("", "1.0.0", digest); err == nil {
			t.Fatal("empty key must be refused")
		}
		if err := i.TrustBuiltin("native_mod", "1.0.0", "not-hex"); err == nil {
			t.Fatal("bad digest must be refused")
		}
		if err := i.TrustBuiltin("native_mod", "1.0.0", digest); err != nil {
			t.Fatal(err)
		}
		if err := i.TrustBuiltin("native_mod", "1.0.0", digest); err != nil {
			t.Fatalf("idempotent re-registration: %v", err)
		}
		if err := i.TrustBuiltin("native_mod", "1.0.0", BuiltinDigest([]byte("other"))); err == nil {
			t.Fatal("rebinding a trusted digest must be refused")
		}
	})
}

// The registry never helps the signed paths: a marketplace/sideload bundle is
// still judged by its signature, whatever digests are registered.
func TestBuiltin_NeverAdmitsASignedPathBundle(t *testing.T) {
	b, raw := embeddedBundle(t, "native_mod", "1.0.0")
	i := &Installer{} // no keys, AllowUnsigned off: fail-closed
	_ = i.TrustBuiltin("native_mod", "1.0.0", BuiltinDigest(raw))

	if _, _, err := i.Install(uuid.New(), b); !errors.Is(err, ErrSignatureRequired) {
		t.Fatalf("Install must still require a signature, got %v", err)
	}
	if _, err := i.Upgrade(context.Background(), uuid.New(), b); !errors.Is(err, ErrSignatureRequired) {
		t.Fatalf("Upgrade must still require a signature, got %v", err)
	}
	// And the builtin entry points refuse what is not registered.
	other, _ := embeddedBundle(t, "marketplace_mod", "1.0.0")
	if _, _, err := i.InstallBuiltin(uuid.New(), other); !errors.Is(err, ErrBuiltinNotTrusted) {
		t.Fatalf("InstallBuiltin of an unregistered bundle: %v", err)
	}
	if _, err := i.UpgradeBuiltin(context.Background(), uuid.New(), other); !errors.Is(err, ErrBuiltinNotTrusted) {
		t.Fatalf("UpgradeBuiltin of an unregistered bundle: %v", err)
	}
}

func builtinUpgradeInstaller(db *gorm.DB) *Installer {
	return &Installer{
		DB:            db,
		KernelVersion: "2.0.0",
		Lifecycles:    lifecycle.NewRegistry(),
		HookRunner:    lifecycle.NewHookRunner(nil),
		schemaApplier: &recordingApplier{},
		Broadcaster:   NoopBroadcaster{},
	}
}

func TestBuiltin_UpgradeEndToEnd(t *testing.T) {
	db := upgradeTestDB(t)
	orgID := uuid.New()
	seedInstallation(t, db, orgID, "native_mod", "1.0.0", nil)
	db.Model(&Installation{}).Where("addon_key = ?", "native_mod").Update("source", SourceBuiltin)

	v2, raw := embeddedBundle(t, "native_mod", "1.1.0")
	i := builtinUpgradeInstaller(db)

	// Not registered: refused, the row is untouched.
	if _, err := i.UpgradeBuiltin(context.Background(), orgID, v2); !errors.Is(err, ErrBuiltinNotTrusted) {
		t.Fatalf("unregistered: %v", err)
	}
	var row Installation
	db.Where("addon_key = ?", "native_mod").Take(&row)
	if row.Version != "1.0.0" {
		t.Fatalf("an untrusted upgrade changed the row: %+v", row)
	}

	// The signed path (here with signatures waived, as in the other upgrade
	// tests) must not take over a builtin installation.
	i.AllowUnsigned = true
	if _, err := i.Upgrade(context.Background(), orgID, v2); !errors.Is(err, ErrBuiltinSourceMismatch) {
		t.Fatalf("signed Upgrade over a builtin row: %v", err)
	}
	i.AllowUnsigned = false

	if err := i.TrustBuiltin("native_mod", "1.1.0", BuiltinDigest(raw)); err != nil {
		t.Fatal(err)
	}
	got, err := i.UpgradeBuiltin(context.Background(), orgID, v2)
	if err != nil {
		t.Fatalf("UpgradeBuiltin: %v", err)
	}
	if got.Version != "1.1.0" {
		t.Fatalf("version = %q", got.Version)
	}
	db.Where("addon_key = ?", "native_mod").Take(&row)
	if row.Source != SourceBuiltin || row.Version != "1.1.0" {
		t.Fatalf("row after builtin upgrade: %+v", row)
	}
}

func TestBuiltin_DoesNotTakeOverASignedInstallation(t *testing.T) {
	db := upgradeTestDB(t)
	orgID := uuid.New()
	seedInstallation(t, db, orgID, "native_mod", "1.0.0", nil) // Source "bundle"
	v2, raw := embeddedBundle(t, "native_mod", "1.1.0")
	i := builtinUpgradeInstaller(db)
	_ = i.TrustBuiltin("native_mod", "1.1.0", BuiltinDigest(raw))
	if _, err := i.UpgradeBuiltin(context.Background(), orgID, v2); !errors.Is(err, ErrBuiltinSourceMismatch) {
		t.Fatalf("builtin Upgrade over a signed row: %v", err)
	}
}
