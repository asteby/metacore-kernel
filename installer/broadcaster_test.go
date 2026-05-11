package installer

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/bundle"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/google/uuid"
)

// newTestManifest returns a manifest with enough surface area that JSON
// marshal exercises every common field — useful for hash-stability tests.
func newTestManifest() manifest.Manifest {
	return manifest.Manifest{
		Key:         "demo",
		Name:        "Demo",
		Description: "demo addon",
		Version:     "1.0.0",
		Category:    "utility",
	}
}

// newTestBundle wraps the canonical test manifest in a *bundle.Bundle with no
// EntryDigests so the marshal-fallback path is exercised when callers don't
// populate them.
func newTestBundle() *bundle.Bundle {
	return &bundle.Bundle{Manifest: newTestManifest()}
}

// fakeManifestSignature exists only to flip the Signature pointer non-nil so
// TestManifestHash_IgnoresSignature can prove the strip-before-hash contract.
// Returning the same struct shape kept manifest_test small without exporting
// a constructor from the manifest package.
func fakeManifestSignature() *manifest.Signature {
	return &manifest.Signature{
		DeveloperID:   "dev-1",
		DeveloperName: "Demo Dev",
		Verified:      true,
		SignedAt:      "2026-05-11T00:00:00Z",
		Algorithm:     "ed25519",
		Digest:        "deadbeef",
		Value:         "cafebabe",
	}
}

// recordingBroadcaster captures every event the installer routes through it
// so tests can assert call counts and field equality without a live ws.Hub.
type recordingBroadcaster struct {
	mu     sync.Mutex
	events []ManifestChangeEvent
	err    error // when set, Broadcast returns it to exercise the log-and-continue path
}

func (r *recordingBroadcaster) Broadcast(_ context.Context, evt ManifestChangeEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, evt)
	return r.err
}

func (r *recordingBroadcaster) snapshot() []ManifestChangeEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ManifestChangeEvent, len(r.events))
	copy(out, r.events)
	return out
}

// TestMaybeBroadcastManifestChange_FirstInstall verifies the empty-OldHash
// signal a host's WebSocket consumer relies on to distinguish "addon just
// installed" from "addon hot-swapped" — the kernel must not collapse the two.
func TestMaybeBroadcastManifestChange_FirstInstall(t *testing.T) {
	rec := &recordingBroadcaster{}
	evt := ManifestChangeEvent{
		OrgID:     uuid.New(),
		AddonKey:  "demo",
		OldHash:   "",
		NewHash:   "sha256:abc",
		Version:   "1.0.0",
		Timestamp: time.Now(),
	}
	maybeBroadcastManifestChange(context.Background(), rec, evt)
	got := rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("want 1 event, got %d", len(got))
	}
	if got[0].OldHash != "" {
		t.Errorf("OldHash: got %q, want empty", got[0].OldHash)
	}
	if got[0].NewHash != "sha256:abc" {
		t.Errorf("NewHash: got %q, want sha256:abc", got[0].NewHash)
	}
	if got[0].AddonKey != "demo" {
		t.Errorf("AddonKey: got %q", got[0].AddonKey)
	}
}

// TestMaybeBroadcastManifestChange_NoChangeIsSilent guarantees the frontend
// is not woken up to re-read a cache that's already correct — the kernel only
// emits when the hash actually flips.
func TestMaybeBroadcastManifestChange_NoChangeIsSilent(t *testing.T) {
	rec := &recordingBroadcaster{}
	evt := ManifestChangeEvent{
		OrgID:    uuid.New(),
		AddonKey: "demo",
		OldHash:  "sha256:abc",
		NewHash:  "sha256:abc",
		Version:  "1.0.0",
	}
	maybeBroadcastManifestChange(context.Background(), rec, evt)
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("want 0 events on unchanged hash, got %d (%+v)", len(got), got)
	}
}

// TestMaybeBroadcastManifestChange_HashFlipped checks the hot-swap path: when
// a reinstall produces a different manifest hash the broadcaster must see
// both old and new values in the same event so the frontend can decide what
// (if any) animation/feedback to show.
func TestMaybeBroadcastManifestChange_HashFlipped(t *testing.T) {
	rec := &recordingBroadcaster{}
	orgID := uuid.New()
	evt := ManifestChangeEvent{
		OrgID:    orgID,
		AddonKey: "demo",
		OldHash:  "sha256:old",
		NewHash:  "sha256:new",
		Version:  "1.1.0",
	}
	maybeBroadcastManifestChange(context.Background(), rec, evt)
	got := rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("want 1 event, got %d", len(got))
	}
	if got[0].OldHash != "sha256:old" || got[0].NewHash != "sha256:new" {
		t.Errorf("old/new mismatch: %+v", got[0])
	}
	if got[0].OrgID != orgID {
		t.Errorf("OrgID: got %v, want %v", got[0].OrgID, orgID)
	}
	if got[0].Version != "1.1.0" {
		t.Errorf("Version: got %q", got[0].Version)
	}
}

// TestMaybeBroadcastManifestChange_BroadcasterErrorIsSwallowed protects the
// install contract: a flaky WebSocket consumer or downstream hub MUST NOT
// roll back a successful install. The function must log and return without
// panicking.
func TestMaybeBroadcastManifestChange_BroadcasterErrorIsSwallowed(t *testing.T) {
	rec := &recordingBroadcaster{err: errors.New("hub closed")}
	evt := ManifestChangeEvent{
		OrgID:    uuid.New(),
		AddonKey: "demo",
		OldHash:  "",
		NewHash:  "sha256:abc",
	}
	// If this panics, the test fails. Defer-recover would also work but a
	// direct call is the natural assertion.
	maybeBroadcastManifestChange(context.Background(), rec, evt)
	if got := rec.snapshot(); len(got) != 1 {
		t.Fatalf("broadcaster should still be invoked even when it errors; got %d events", len(got))
	}
}

// TestMaybeBroadcastManifestChange_NilBroadcasterIsSafe documents the
// permissive contract: a host that wipes the broadcaster mid-flight (e.g. to
// disable broadcasting after a panic in the hub) must not crash Install.
func TestMaybeBroadcastManifestChange_NilBroadcasterIsSafe(t *testing.T) {
	maybeBroadcastManifestChange(context.Background(), nil, ManifestChangeEvent{
		OrgID:    uuid.New(),
		AddonKey: "demo",
		OldHash:  "",
		NewHash:  "sha256:abc",
	})
}

// TestMaybeBroadcastManifestChange_EmptyNewHashIsSilent exercises the defensive
// branch that handles "we couldn't compute the hash" — better to skip the
// broadcast than push a useless empty-hash event downstream.
func TestMaybeBroadcastManifestChange_EmptyNewHashIsSilent(t *testing.T) {
	rec := &recordingBroadcaster{}
	maybeBroadcastManifestChange(context.Background(), rec, ManifestChangeEvent{
		OrgID:    uuid.New(),
		AddonKey: "demo",
		OldHash:  "",
		NewHash:  "",
	})
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("empty NewHash should be silent, got %d events", len(got))
	}
}

// TestWithBroadcaster_NilRevertsToNoop asserts the Law 2 default-restoration
// contract: passing nil to WithBroadcaster must NOT leave the Installer with
// a nil interface field (which would NPE inside Install). It should drop back
// to NoopBroadcaster instead.
func TestWithBroadcaster_NilRevertsToNoop(t *testing.T) {
	i := &Installer{Broadcaster: &recordingBroadcaster{}}
	i.WithBroadcaster(nil)
	if _, ok := i.Broadcaster.(NoopBroadcaster); !ok {
		t.Fatalf("nil broadcaster should fall back to NoopBroadcaster, got %T", i.Broadcaster)
	}
}

// TestNoopBroadcaster_ReturnsNil locks in the zero-cost default contract so
// callers can chain it without error handling.
func TestNoopBroadcaster_ReturnsNil(t *testing.T) {
	if err := (NoopBroadcaster{}).Broadcast(context.Background(), ManifestChangeEvent{}); err != nil {
		t.Fatalf("NoopBroadcaster.Broadcast: %v", err)
	}
}

// TestManifestHash_IgnoresSignature locks in the contract that re-signing a
// manifest (different DeveloperID / SignedAt / Value) MUST NOT flip the hash
// — otherwise every release would look like a hot-swap to the frontend.
func TestManifestHash_IgnoresSignature(t *testing.T) {
	base := newTestManifest()
	withSig := base
	withSig.Signature = fakeManifestSignature()
	a := manifestHash(base)
	b := manifestHash(withSig)
	if a == "" || a != b {
		t.Fatalf("hash should ignore Signature: %q vs %q", a, b)
	}
}

// TestManifestHash_ContentChangeFlipsHash protects against the inverse: any
// non-Signature edit (e.g. bumping Version or adding a tool) must produce a
// different hash so the frontend actually invalidates its metadata cache.
func TestManifestHash_ContentChangeFlipsHash(t *testing.T) {
	base := newTestManifest()
	mutated := base
	mutated.Version = "1.0.1"
	if manifestHash(base) == manifestHash(mutated) {
		t.Fatal("manifest hash must change when Version bumps")
	}
}

// TestManifestHashFromBundle_PrefersEntryDigest exercises the fast path that
// reuses the SHA-256 the bundle reader already computed — Install must not
// pay the cost of a re-marshal on every reinstall.
func TestManifestHashFromBundle_PrefersEntryDigest(t *testing.T) {
	b := newTestBundle()
	b.EntryDigests = map[string]string{
		"manifest.json": "deadbeef" + "0000000000000000000000000000000000000000000000000000000000",
	}
	got := manifestHashFromBundle(b)
	want := "sha256:deadbeef" + "0000000000000000000000000000000000000000000000000000000000"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestManifestHashFromBundle_FallsBackToMarshal covers in-memory bundles
// (tests, compiled addons) that have no EntryDigests — the kernel must still
// produce a stable hash so the broadcast contract holds.
func TestManifestHashFromBundle_FallsBackToMarshal(t *testing.T) {
	b := newTestBundle()
	b.EntryDigests = nil
	got := manifestHashFromBundle(b)
	if got == "" {
		t.Fatal("expected non-empty hash from marshal fallback")
	}
	// Re-running must be deterministic.
	if got2 := manifestHashFromBundle(b); got != got2 {
		t.Fatalf("hash unstable across calls: %q vs %q", got, got2)
	}
}

