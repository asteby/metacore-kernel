package bridge

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/installer"
	"github.com/asteby/metacore-kernel/ws"
	"github.com/google/uuid"
)

// capturingPublisher stands in for ws.Hub.SendToUsers so tests can assert
// on the exact set of user IDs and the message shape without a live
// WebSocket transport. It is safe for concurrent use because Broadcast does
// not (today) call the publisher from multiple goroutines, but locking
// keeps the assertion robust against future fan-out changes.
type capturingPublisher struct {
	mu    sync.Mutex
	calls []capturedCall
}

type capturedCall struct {
	UserIDs []uuid.UUID
	Message ws.Message
}

func (c *capturingPublisher) publish(ids []uuid.UUID, msg ws.Message) {
	c.mu.Lock()
	defer c.mu.Unlock()
	idsCopy := make([]uuid.UUID, len(ids))
	copy(idsCopy, ids)
	c.calls = append(c.calls, capturedCall{UserIDs: idsCopy, Message: msg})
}

func (c *capturingPublisher) snapshot() []capturedCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]capturedCall, len(c.calls))
	copy(out, c.calls)
	return out
}

// newTestBroadcaster wires a WSManifestBroadcaster with the test
// publisher seam swapped in. It keeps the real users-resolver so the test
// exercises the same path production uses.
func newTestBroadcaster(t *testing.T, users UsersForOrgFn) (*WSManifestBroadcaster, *capturingPublisher) {
	t.Helper()
	hub := ws.NewHub()
	cap := &capturingPublisher{}
	wb := NewWSManifestBroadcaster(hub, nil, users)
	wb.publish = cap.publish
	return wb, cap
}

// TestWSManifestBroadcaster_PublishesWithExpectedTopic locks in the wire
// contract the frontend depends on: every manifest hot-swap fans out a
// single ws.Message of type ADDON_MANIFEST_CHANGED to every user in the
// affected org, and the payload carries old/new hashes plus the timestamp
// the kernel observed.
func TestWSManifestBroadcaster_PublishesWithExpectedTopic(t *testing.T) {
	orgID := uuid.New()
	userA, userB := uuid.New(), uuid.New()
	resolver := func(_ context.Context, gotOrg uuid.UUID) ([]uuid.UUID, error) {
		if gotOrg != orgID {
			t.Fatalf("resolver invoked with %v, want %v", gotOrg, orgID)
		}
		return []uuid.UUID{userA, userB}, nil
	}
	wb, cap := newTestBroadcaster(t, resolver)

	evt := installer.ManifestChangeEvent{
		OrgID:     orgID,
		AddonKey:  "demo",
		OldHash:   "sha256:old",
		NewHash:   "sha256:new",
		Version:   "1.2.0",
		Timestamp: time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC),
	}
	if err := wb.Broadcast(context.Background(), evt); err != nil {
		t.Fatalf("Broadcast: %v", err)
	}
	calls := cap.snapshot()
	if len(calls) != 1 {
		t.Fatalf("want 1 publish, got %d", len(calls))
	}
	if calls[0].Message.Type != WSManifestChangedType {
		t.Errorf("topic: got %q, want %q", calls[0].Message.Type, WSManifestChangedType)
	}
	payload, ok := calls[0].Message.Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload: got %T, want map[string]any", calls[0].Message.Payload)
	}
	if payload["addonKey"] != "demo" {
		t.Errorf("addonKey: got %v", payload["addonKey"])
	}
	if payload["oldHash"] != "sha256:old" || payload["newHash"] != "sha256:new" {
		t.Errorf("hashes: %+v", payload)
	}
	if payload["version"] != "1.2.0" {
		t.Errorf("version: got %v", payload["version"])
	}
	// Fan-out: every user in the org list received the message.
	gotUsers := map[uuid.UUID]bool{}
	for _, u := range calls[0].UserIDs {
		gotUsers[u] = true
	}
	if !gotUsers[userA] || !gotUsers[userB] {
		t.Errorf("fan-out missed users: got %v, want both %v and %v", calls[0].UserIDs, userA, userB)
	}
}

// TestWSManifestBroadcaster_NoUsersIsCheapNoOp verifies the broadcaster
// stays silent when an org has no online users — pushing to nobody would
// just churn the hub channels and pollute logs.
func TestWSManifestBroadcaster_NoUsersIsCheapNoOp(t *testing.T) {
	resolver := func(context.Context, uuid.UUID) ([]uuid.UUID, error) { return nil, nil }
	wb, cap := newTestBroadcaster(t, resolver)
	err := wb.Broadcast(context.Background(), installer.ManifestChangeEvent{
		OrgID:    uuid.New(),
		AddonKey: "demo",
		NewHash:  "sha256:abc",
	})
	if err != nil {
		t.Fatalf("Broadcast: %v", err)
	}
	if calls := cap.snapshot(); len(calls) != 0 {
		t.Fatalf("publish should be skipped when no users; got %d calls", len(calls))
	}
}

// TestWSManifestBroadcaster_UsersResolverErrorSwallowed mirrors the
// installer contract: a flaky user-lookup must NOT propagate up into the
// install path. The broadcaster logs and returns nil so installer.Install
// continues normally.
func TestWSManifestBroadcaster_UsersResolverErrorSwallowed(t *testing.T) {
	resolver := func(context.Context, uuid.UUID) ([]uuid.UUID, error) {
		return nil, errors.New("db down")
	}
	wb, cap := newTestBroadcaster(t, resolver)
	err := wb.Broadcast(context.Background(), installer.ManifestChangeEvent{
		OrgID:    uuid.New(),
		AddonKey: "demo",
		NewHash:  "sha256:abc",
	})
	if err != nil {
		t.Fatalf("Broadcast should swallow resolver errors, got %v", err)
	}
	if calls := cap.snapshot(); len(calls) != 0 {
		t.Fatalf("publish should not run on resolver error; got %d calls", len(calls))
	}
}

// TestNewWSManifestBroadcaster_PanicsOnNilHub locks in the wiring contract:
// a host that forgets to construct a hub gets a loud panic at boot
// instead of a silent NPE on the first install.
func TestNewWSManifestBroadcaster_PanicsOnNilHub(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for nil hub")
		}
	}()
	NewWSManifestBroadcaster(nil, nil, nil)
}

// TestManifestChangedPayload_ShapeIsStable freezes the camelCase keys the
// frontend metadata-cache reducer reads. Renaming any of them is a breaking
// change that requires a coordinated SDK release.
func TestManifestChangedPayload_ShapeIsStable(t *testing.T) {
	orgID := uuid.New()
	evt := installer.ManifestChangeEvent{
		OrgID:     orgID,
		AddonKey:  "demo",
		OldHash:   "sha256:old",
		NewHash:   "sha256:new",
		Version:   "1.0.0",
		Timestamp: time.Unix(1715472000, 0).UTC(),
	}
	got := manifestChangedPayload(evt)
	for _, want := range []string{"orgId", "addonKey", "oldHash", "newHash", "version", "timestamp"} {
		if _, ok := got[want]; !ok {
			t.Errorf("payload missing key %q (got %+v)", want, got)
		}
	}
	if got["orgId"] != orgID {
		t.Errorf("orgId: got %v, want %v", got["orgId"], orgID)
	}
}
