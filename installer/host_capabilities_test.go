package installer

import (
	"context"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/lifecycle"
	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
	"github.com/google/uuid"
)

// An addon whose UI needs a host capability (a declarative map view) is
// refused on upgrade by a host that lists its capabilities without it; a host
// that does not configure HostCapabilities keeps the old behaviour.
func TestUpgrade_RefusesAddonNeedingMissingUICapability(t *testing.T) {
	db := upgradeTestDB(t)
	orgID := uuid.New()
	seedInstallation(t, db, orgID, "demo", "1.0.0", nil)
	inst := &Installer{
		DB: db, KernelVersion: "3.0.0",
		HostCapabilities: []string{"wasm-runtime"},
		Lifecycles:       lifecycle.NewRegistry(), AllowUnsigned: true,
		schemaApplier: &recordingApplier{}, Broadcaster: NoopBroadcaster{},
	}
	b := makeBundle("demo", "1.1.0", nil)
	b.Manifest.Navigation = []manifest.NavGroup{{Title: "g", Items: []manifest.NavItem{{
		Title: "map", Model: "Order", ViewType: "map",
		View: &v3.NavView{Map: &v3.MapView{Point: &v3.MapPoint{Lat: "lat", Lng: "lng"}}},
	}}}}

	_, err := inst.Upgrade(context.Background(), orgID, b)
	if err == nil || !strings.Contains(err.Error(), `"ui.views.map"`) {
		t.Fatalf("want a refusal naming ui.views.map, got %v", err)
	}

	inst.HostCapabilities = append(inst.HostCapabilities, manifest.UIViewCapability("map"))
	if _, err := inst.Upgrade(context.Background(), orgID, b); err != nil {
		t.Fatalf("a host with the capability must upgrade: %v", err)
	}
}

func TestInstaller_NilHostCapabilitiesSkipsTheCheck(t *testing.T) {
	inst := &Installer{}
	m := &manifest.Manifest{HostCapabilities: []string{"ui.maps"}}
	if err := inst.checkHostCapabilities(m); err != nil {
		t.Fatalf("nil HostCapabilities must skip the check: %v", err)
	}
	inst.HostCapabilities = []string{}
	if err := inst.checkHostCapabilities(m); err == nil {
		t.Fatal("an empty (non-nil) capability set must refuse a declared capability")
	}
}
