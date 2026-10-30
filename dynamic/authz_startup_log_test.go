package dynamic

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/metadata"
)

// captureNewLog builds a Service without a permission service and returns
// what New wrote to the standard logger.
func captureNewLog(t *testing.T, cfg Config) string {
	t.Helper()
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })

	cfg.DB = setupTestDB(t)
	cfg.Metadata = metadata.New(metadata.Config{CacheTTL: -1})
	New(cfg)
	return buf.String()
}

const authzDisabledWarning = "authorization DISABLED for the dynamic runtime"

func TestNewWarnsAuthorizationDisabledByDefault(t *testing.T) {
	out := captureNewLog(t, Config{})
	if !strings.Contains(out, authzDisabledWarning) {
		t.Fatalf("without PermissionsEnforcedByHost the warning must stay; log:\n%s", out)
	}
	if strings.Contains(out, "delegated to the host") {
		t.Fatalf("the delegated line must not appear by default; log:\n%s", out)
	}
}

func TestNewDoesNotWarnWhenHostEnforcesPermissions(t *testing.T) {
	out := captureNewLog(t, Config{PermissionsEnforcedByHost: true})
	if strings.Contains(out, authzDisabledWarning) {
		t.Fatalf("a host that authorizes on its own must not get the warning; log:\n%s", out)
	}
	if !strings.Contains(out, "dynamic: authorization delegated to the host") {
		t.Fatalf("want the one-line informational log; log:\n%s", out)
	}
}

// The flag is log-only: RequirePermissions keeps failing closed and its own
// log line wins, and the decision without it stays fail-open.
func TestPermissionsEnforcedByHostDoesNotChangeTheDecision(t *testing.T) {
	out := captureNewLog(t, Config{PermissionsEnforcedByHost: true, RequirePermissions: true})
	if !strings.Contains(out, "every dynamic request will be DENIED") {
		t.Fatalf("RequirePermissions must keep its log; log:\n%s", out)
	}

	svc := serviceWithoutPerms(t, false)
	svc2 := New(Config{DB: setupTestDB(t), Metadata: metadata.New(metadata.Config{CacheTTL: -1}), PermissionsEnforcedByHost: true})
	if svc.requirePerms != svc2.requirePerms || svc2.perms != nil {
		t.Fatal("PermissionsEnforcedByHost must not alter the authorization wiring")
	}
}
