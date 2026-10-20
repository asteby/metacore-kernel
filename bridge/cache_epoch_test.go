package bridge

import (
	"context"
	"testing"

	"github.com/asteby/metacore-kernel/cachecoherence"
	"github.com/asteby/metacore-kernel/ws"
	"github.com/google/uuid"
)

func TestPublishCacheEpoch_OrgScopedGoesToOrgUsers(t *testing.T) {
	org := uuid.New()
	u1, u2, other := uuid.New(), uuid.New(), uuid.New()
	var gotUsers []uuid.UUID
	var gotMsg ws.Message
	ev := cachecoherence.Event{Seq: 7, Reason: "addon.upgrade", OrgID: org.String(), AddonKey: "crm",
		Epoch: cachecoherence.Epoch{Global: 100, Org: 2, Addon: 3}}
	publishCacheEpoch(context.Background(), ev,
		func(_ context.Context, o uuid.UUID) ([]uuid.UUID, error) {
			if o != org {
				t.Fatalf("resolved wrong org %s", o)
			}
			return []uuid.UUID{u1, u2}, nil
		},
		func() []uuid.UUID { return []uuid.UUID{other} },
		func(u []uuid.UUID, m ws.Message) { gotUsers, gotMsg = u, m })
	if len(gotUsers) != 2 || gotMsg.Type != WSCacheEpochChangedType {
		t.Fatalf("users=%v msg=%+v", gotUsers, gotMsg)
	}
	p := gotMsg.Payload.(map[string]any)
	if p["epoch"] != ev.Epoch.Token() || p["addonKey"] != "crm" || p["orgId"] != org.String() || p["reason"] != "addon.upgrade" {
		t.Fatalf("payload = %v", p)
	}
}

func TestPublishCacheEpoch_GlobalGoesToEveryConnectedUser(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	var gotUsers []uuid.UUID
	publishCacheEpoch(context.Background(), cachecoherence.Event{Reason: "flush", Epoch: cachecoherence.Epoch{Global: 5}},
		func(context.Context, uuid.UUID) ([]uuid.UUID, error) {
			t.Fatal("org resolver used for a global bump")
			return nil, nil
		},
		func() []uuid.UUID { return []uuid.UUID{a, b} },
		func(u []uuid.UUID, _ ws.Message) { gotUsers = u })
	if len(gotUsers) != 2 {
		t.Fatalf("users = %v", gotUsers)
	}
}
