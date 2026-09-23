package installer

import (
	"context"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/lifecycle"
	"github.com/google/uuid"
)

// An addon declaring compatibility.requires "metacore-kernel" >=0.154.1 must
// be refused by an older host on UPGRADE (and install) with a clear message,
// instead of installing and breaking at runtime (QA 0922: customers 0.62.0).
func TestUpgrade_RefusesAddonRequiringNewerKernelRuntime(t *testing.T) {
	db := upgradeTestDB(t)
	orgID := uuid.New()
	seedInstallation(t, db, orgID, "demo", "1.0.0", nil)
	inst := &Installer{
		DB: db, KernelVersion: "3.0.0", RuntimeVersion: "0.152.0",
		Lifecycles: lifecycle.NewRegistry(), AllowUnsigned: true,
		schemaApplier: &recordingApplier{}, Broadcaster: NoopBroadcaster{},
	}
	b := makeBundle("demo", "1.1.0", nil)
	b.Manifest.Runtime = ">=0.154.1"

	_, err := inst.Upgrade(context.Background(), orgID, b)
	if err == nil || !strings.Contains(err.Error(), "metacore-kernel >=0.154.1") || !strings.Contains(err.Error(), "0.152.0") {
		t.Fatalf("want a runtime refusal naming both versions, got %v", err)
	}

	inst.RuntimeVersion = "0.154.1"
	if _, err := inst.Upgrade(context.Background(), orgID, b); err != nil {
		t.Fatalf("a satisfying host must upgrade: %v", err)
	}
}
