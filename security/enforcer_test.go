package security_test

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/security"
)

func buildCaps(t *testing.T) *security.Capabilities {
	t.Helper()
	return security.Compile("billing", []manifest.Capability{
		{Kind: "db:read", Target: "orders"},
		{Kind: "http:fetch", Target: "api.stripe.com"},
	})
}

func TestEnforcer_ShadowLogsButReturnsNil(t *testing.T) {
	caps := buildCaps(t)
	var buf bytes.Buffer
	e := security.NewEnforcer(func(key string) *security.Capabilities { return caps })
	e.Logger = log.New(&buf, "", 0)
	e.SetMode(security.ModeShadow)

	if err := e.CheckCapability("billing", "db:write", "orders"); err != nil {
		t.Fatalf("shadow should not return error, got %v", err)
	}
	if !strings.Contains(buf.String(), "metacore.capability.violation") {
		t.Errorf("missing violation log: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "caller=") {
		t.Errorf("missing caller location in log: %q", buf.String())
	}
}

func TestEnforcer_EnforceReturnsError(t *testing.T) {
	caps := buildCaps(t)
	e := security.NewEnforcer(func(key string) *security.Capabilities { return caps })
	e.Logger = log.New(&bytes.Buffer{}, "", 0)
	e.SetMode(security.ModeEnforce)

	var observed bool
	e.OnViolation = func(addon, kind, target, caller string, err error) {
		observed = true
		if addon != "billing" || kind != "http:fetch" {
			t.Errorf("unexpected violation fields: %s/%s", addon, kind)
		}
	}

	err := e.CheckCapability("billing", "http:fetch", "https://evil.example.com")
	if err == nil {
		t.Fatal("enforce mode should return error")
	}
	if !observed {
		t.Error("OnViolation not invoked")
	}
}

func TestEnforcer_AllowedCapabilityNoLog(t *testing.T) {
	caps := buildCaps(t)
	var buf bytes.Buffer
	e := security.NewEnforcer(func(key string) *security.Capabilities { return caps })
	e.Logger = log.New(&buf, "", 0)
	e.SetMode(security.ModeEnforce)

	if err := e.CheckCapability("billing", "db:read", "orders"); err != nil {
		t.Fatalf("allowed call returned error: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("no log expected on allowed call, got %q", buf.String())
	}
}

func TestModeFromEnv_DefaultsToEnforce(t *testing.T) {
	cases := map[string]security.Mode{
		"":        security.ModeEnforce, // unset/empty => enforce (D5)
		"1":       security.ModeEnforce,
		"true":    security.ModeEnforce,
		"enforce": security.ModeEnforce,
		"typo":    security.ModeEnforce, // unknown values fail closed
		"shadow":  security.ModeShadow,
		"SHADOW":  security.ModeShadow,
		" 0 ":     security.ModeShadow,
		"false":   security.ModeShadow,
		"off":     security.ModeShadow,
	}
	for v, want := range cases {
		t.Setenv("METACORE_ENFORCE", v)
		if got := security.ModeFromEnv(); got != want {
			t.Errorf("METACORE_ENFORCE=%q: got %s want %s", v, got, want)
		}
	}
}

func TestNewEnforcer_DefaultBlocksViolation(t *testing.T) {
	t.Setenv("METACORE_ENFORCE", "")
	caps := buildCaps(t)
	e := security.NewEnforcer(func(string) *security.Capabilities { return caps })
	e.Logger = log.New(&bytes.Buffer{}, "", 0)
	if e.Mode() != security.ModeEnforce {
		t.Fatalf("default mode = %s, want enforce", e.Mode())
	}
	if err := e.CheckCapability("billing", "db:write", "orders"); err == nil {
		t.Fatal("default enforcer must deny an undeclared capability")
	}
	// Unknown addon is also denied by default.
	e2 := security.NewEnforcer(func(string) *security.Capabilities { return nil })
	e2.Logger = log.New(&bytes.Buffer{}, "", 0)
	if err := e2.CheckCapability("ghost", "db:read", "x"); err == nil {
		t.Fatal("unknown addon must be denied by default")
	}
	t.Setenv("METACORE_ENFORCE", "shadow")
	e3 := security.NewEnforcer(func(string) *security.Capabilities { return caps })
	e3.Logger = log.New(&bytes.Buffer{}, "", 0)
	if err := e3.CheckCapability("billing", "db:write", "orders"); err != nil {
		t.Fatalf("shadow must not block: %v", err)
	}
}
