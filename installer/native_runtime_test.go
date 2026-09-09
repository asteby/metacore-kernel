package installer

import (
	"context"
	"errors"
	"testing"

	"github.com/asteby/metacore-kernel/bundle"
	"github.com/asteby/metacore-kernel/lifecycle"
	"github.com/asteby/metacore-kernel/runtime/native"
	"github.com/google/uuid"
)

type recordingNativeRuntime struct{ calls int }

func (r *recordingNativeRuntime) Ensure(context.Context, *bundle.Bundle, Installation) error {
	r.calls++
	return nil
}

func TestUpgradeEnsuresNativeServiceBeforeVersionCommit(t *testing.T) {
	db := upgradeTestDB(t)
	orgID := uuid.New()
	seedInstallation(t, db, orgID, "connector_whatsapp", "1.0.0", nil)
	recorder := &recordingNativeRuntime{}
	i := &Installer{
		DB: db, KernelVersion: "3.0.0", Lifecycles: lifecycle.NewRegistry(),
		AllowUnsigned: true, schemaApplier: &recordingApplier{},
		Broadcaster: NoopBroadcaster{}, NativeRuntime: recorder,
	}
	b := makeBundle("connector_whatsapp", "1.1.0", nil)
	b.Manifest.NativeService = &native.Spec{
		Entrypoint: "backend/connector", Scope: native.ScopeInstance,
		Health:    native.HealthCheck{Path: "/health"},
		Resources: native.ResourceLimits{MemoryMB: 512, CPUQuotaMCPU: 500, PIDs: 128},
		Network:   native.NetworkPolicy{Egress: []string{"web.whatsapp.com:443"}},
	}
	row, err := i.Upgrade(context.Background(), orgID, b)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if recorder.calls != 1 || row.Version != "1.1.0" {
		t.Fatalf("native ensure/version = %d/%s, want 1/1.1.0", recorder.calls, row.Version)
	}
}

func TestUpgradeWithoutNativeAdapterKeepsPreviousVersion(t *testing.T) {
	db := upgradeTestDB(t)
	orgID := uuid.New()
	seedInstallation(t, db, orgID, "connector_whatsapp", "1.0.0", nil)
	i := &Installer{
		DB: db, KernelVersion: "3.0.0", Lifecycles: lifecycle.NewRegistry(),
		AllowUnsigned: true, schemaApplier: &recordingApplier{}, Broadcaster: NoopBroadcaster{},
	}
	b := makeBundle("connector_whatsapp", "1.1.0", nil)
	b.Manifest.NativeService = &native.Spec{
		Entrypoint: "backend/connector", Scope: native.ScopeInstance,
		Health:    native.HealthCheck{Path: "/health"},
		Resources: native.ResourceLimits{MemoryMB: 512, CPUQuotaMCPU: 500, PIDs: 128},
		Network:   native.NetworkPolicy{Egress: []string{"web.whatsapp.com:443"}},
	}
	_, err := i.Upgrade(context.Background(), orgID, b)
	if !errors.Is(err, ErrNativeRuntimeUnavailable) {
		t.Fatalf("expected unavailable runtime, got %v", err)
	}
	var row Installation
	if err := db.Where("organization_id = ? AND addon_key = ?", orgID, "connector_whatsapp").Take(&row).Error; err != nil {
		t.Fatalf("reload installation: %v", err)
	}
	if row.Version != "1.0.0" {
		t.Fatalf("failed native upgrade committed version %s", row.Version)
	}
}

func TestUnsupportedNativeRuntimeFailsClosed(t *testing.T) {
	err := (UnsupportedNativeServiceRuntime{}).Ensure(context.Background(), &bundle.Bundle{}, Installation{})
	if !errors.Is(err, ErrNativeRuntimeUnavailable) {
		t.Fatalf("expected ErrNativeRuntimeUnavailable, got %v", err)
	}
}

func TestWithNativeRuntime(t *testing.T) {
	r := &recordingNativeRuntime{}
	i := (&Installer{}).WithNativeRuntime(r)
	if i.NativeRuntime != r {
		t.Fatalf("runtime adapter was not installed: %T", i.NativeRuntime)
	}
	i.WithNativeRuntime(nil)
	if _, ok := i.NativeRuntime.(UnsupportedNativeServiceRuntime); !ok {
		t.Fatalf("nil must restore fail-closed adapter, got %T", i.NativeRuntime)
	}
}
