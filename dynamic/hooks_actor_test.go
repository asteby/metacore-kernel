package dynamic

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/google/uuid"
)

// payloadInvoker keeps the last envelope a manifest hook produced.
type payloadInvoker struct{ last map[string]any }

func (p *payloadInvoker) Run(_ context.Context, _ string, _ uuid.UUID, _ string, _ manifest.Manifest, payload []byte) error {
	p.last = nil
	return json.Unmarshal(payload, &p.last)
}

func TestManifestHooks_EnvelopeCarriesActor(t *testing.T) {
	reg := NewHookRegistry()
	inv := &payloadInvoker{}
	reg.RegisterManifestHooks("demo", newManifestWithCRUDHooks("tickets",
		"before_create", "after_create", "before_update", "after_update", "before_delete", "after_delete"), inv)

	userID := uuid.New()
	hc := HookContext{Model: "tickets", User: &fakeUser{id: userID, orgID: uuid.New()}}
	// The client-supplied row can carry actor_id: it stays under `input`, the
	// root is the host's.
	input := map[string]any{"actor_id": "spoofed", "user_id": "spoofed"}

	runs := map[string]func(ctx context.Context) error{
		"before_create": func(ctx context.Context) error { return reg.runBeforeCreate(ctx, hc, input) },
		"after_create":  func(ctx context.Context) error { return reg.runAfterCreate(ctx, hc, input) },
		"before_update": func(ctx context.Context) error { return reg.runBeforeUpdate(ctx, hc, "id-1", input) },
		"after_update":  func(ctx context.Context) error { return reg.runAfterUpdate(ctx, hc, input) },
		"before_delete": func(ctx context.Context) error { return reg.runBeforeDelete(ctx, hc, "id-1") },
		"after_delete":  func(ctx context.Context) error { return reg.runAfterDelete(ctx, hc, "id-1") },
	}
	ctxActor := uuid.NewString()
	for ev, run := range runs {
		// No ctx actor: falls back to the request user.
		if err := run(context.Background()); err != nil {
			t.Fatalf("%s: %v", ev, err)
		}
		if inv.last["actor_id"] != userID.String() || inv.last["user_id"] != userID.String() {
			t.Errorf("%s: actor = %v / %v, want request user %s", ev, inv.last["actor_id"], inv.last["user_id"], userID)
		}
		// ctx actor (WithActorID) wins.
		if err := run(WithActorID(context.Background(), ctxActor)); err != nil {
			t.Fatalf("%s: %v", ev, err)
		}
		if inv.last["actor_id"] != ctxActor || inv.last["user_id"] != ctxActor {
			t.Errorf("%s: actor = %v / %v, want ctx actor %s", ev, inv.last["actor_id"], inv.last["user_id"], ctxActor)
		}
	}
}

func TestManifestHooks_NoActorWritesNothing(t *testing.T) {
	reg := NewHookRegistry()
	inv := &payloadInvoker{}
	reg.RegisterManifestHooks("demo", newManifestWithCRUDHooks("tickets", "after_create"), inv)

	for name, hc := range map[string]HookContext{
		"nil user":         {Model: "tickets"},
		"zero user id":     {Model: "tickets", User: &fakeUser{orgID: uuid.New()}},
		"system principal": {Model: "tickets", User: &fakeUser{id: SystemActorID, orgID: uuid.New()}},
	} {
		if err := reg.runAfterCreate(context.Background(), hc, map[string]any{}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, ok := inv.last["actor_id"]; ok {
			t.Errorf("%s: actor_id written: %v", name, inv.last)
		}
		if _, ok := inv.last["user_id"]; ok {
			t.Errorf("%s: user_id written: %v", name, inv.last)
		}
	}
}
