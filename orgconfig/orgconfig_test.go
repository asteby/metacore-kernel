package orgconfig_test

import (
	"context"
	"errors"
	"testing"

	"github.com/asteby/metacore-kernel/bridge"
	"github.com/asteby/metacore-kernel/lifecycle"
	"github.com/asteby/metacore-kernel/orgconfig"
	"github.com/asteby/metacore-kernel/runtime/wasm"
	"github.com/google/uuid"
)

func TestResolveNoProvider(t *testing.T) {
	_, err := orgconfig.Resolve(context.Background(), uuid.New())
	if !errors.Is(err, orgconfig.ErrUnavailable) {
		t.Fatalf("want ErrUnavailable, got %v", err)
	}
	if _, err := orgconfig.Resolve(context.Background(), uuid.Nil); !errors.Is(err, orgconfig.ErrNoOrg) {
		t.Fatalf("want ErrNoOrg, got %v", err)
	}
}

func fake(a, b uuid.UUID) wasm.ContextProviderFn {
	return func(_ context.Context, org, _ uuid.UUID, want wasm.HostContextScopes) (*wasm.HostContext, error) {
		if !want.OrgConfig {
			return nil, errors.New("org_config scope not requested")
		}
		switch org {
		case a:
			return &wasm.HostContext{Org: wasm.OrgConfig{CurrencyCode: "USD"}}, nil
		case b:
			return &wasm.HostContext{Org: wasm.OrgConfig{CurrencyCode: "COP"}}, nil
		}
		return nil, nil // unknown org: empty, no default
	}
}

func TestSingleSourceAndTenancy(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	ctx := orgconfig.WithProvider(context.Background(), wasm.OrgConfigProvider(fake(a, b)))

	ac := &bridge.ActionContext{OrgID: a}
	cfg, err := ac.OrgConfig(ctx)
	if err != nil || cfg.CurrencyCode != "USD" {
		t.Fatalf("org A: %+v %v", cfg, err)
	}
	cfg, _ = (&bridge.ActionContext{OrgID: b}).OrgConfig(ctx)
	if cfg.CurrencyCode != "COP" {
		t.Fatalf("org B leaked/wrong: %+v", cfg)
	}

	lc := &lifecycle.ActionContext{Ctx: ctx, OrgID: a}
	if cfg, err := lc.OrgConfig(); err != nil || cfg.CurrencyCode != "USD" {
		t.Fatalf("lifecycle: %+v %v", cfg, err)
	}
	// unconfigured org: empty, no regional default
	cfg, err = orgconfig.Resolve(ctx, uuid.New())
	if err != nil || cfg.CurrencyCode != "" {
		t.Fatalf("unconfigured: %+v %v", cfg, err)
	}
}

func TestNoProviderViaActionContext(t *testing.T) {
	if _, err := (&bridge.ActionContext{OrgID: uuid.New()}).OrgConfig(context.Background()); !errors.Is(err, orgconfig.ErrUnavailable) {
		t.Fatal(err)
	}
	if _, err := (&lifecycle.ActionContext{OrgID: uuid.New()}).OrgConfig(); !errors.Is(err, orgconfig.ErrUnavailable) {
		t.Fatal(err)
	}
	if wasm.OrgConfigProvider(nil) != nil {
		t.Fatal("nil fn must give nil provider")
	}
}
