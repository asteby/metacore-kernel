package installer

import (
	"testing"

	"github.com/asteby/metacore-kernel/bundle"
	"github.com/asteby/metacore-kernel/manifest"
)

// TestFrontendAssetDigest_SortedAndStable verifies that the helper hands
// the consumer a deterministic asset list (sorted by path) plus a digest
// that's stable across runs — both prerequisites for the SDK to compare
// "did anything user-facing change" across events.
func TestFrontendAssetDigest_SortedAndStable(t *testing.T) {
	b := &bundle.Bundle{
		Manifest: manifest.Manifest{
			Frontend: &manifest.FrontendSpec{Format: "federation", Entry: "frontend/remoteEntry.js"},
		},
		Frontend: map[string][]byte{
			"frontend/remoteEntry.js":     []byte("entry"),
			"frontend/assets/index.css":   []byte("css"),
			"frontend/assets/chunk-1.js":  []byte("chunk"),
		},
	}
	paths1, hash1 := frontendAssetDigest(b)
	paths2, hash2 := frontendAssetDigest(b)
	if hash1 == "" {
		t.Fatal("expected non-empty content hash")
	}
	if hash1 != hash2 {
		t.Fatalf("hash not stable across calls: %s vs %s", hash1, hash2)
	}
	if len(paths1) != 3 {
		t.Fatalf("expected 3 paths, got %v", paths1)
	}
	// Sorted output.
	if paths1[0] != "frontend/assets/chunk-1.js" ||
		paths1[1] != "frontend/assets/index.css" ||
		paths1[2] != "frontend/remoteEntry.js" {
		t.Fatalf("paths not sorted: %v", paths1)
	}
	for i := range paths1 {
		if paths1[i] != paths2[i] {
			t.Fatalf("path slice not stable: %v vs %v", paths1, paths2)
		}
	}
}

// TestFrontendAssetDigest_ContentChangeFlipsHash protects against the
// "manifest hash flipped but content hash didn't" path the SDK relies on
// to skip a federation reload.
func TestFrontendAssetDigest_ContentChangeFlipsHash(t *testing.T) {
	base := &bundle.Bundle{
		Manifest: manifest.Manifest{Frontend: &manifest.FrontendSpec{Format: "federation"}},
		Frontend: map[string][]byte{"frontend/remoteEntry.js": []byte("v1")},
	}
	mutated := &bundle.Bundle{
		Manifest: manifest.Manifest{Frontend: &manifest.FrontendSpec{Format: "federation"}},
		Frontend: map[string][]byte{"frontend/remoteEntry.js": []byte("v2")},
	}
	_, h1 := frontendAssetDigest(base)
	_, h2 := frontendAssetDigest(mutated)
	if h1 == h2 {
		t.Fatalf("hash didn't flip on content change: %s", h1)
	}
}

// TestFrontendAssetDigest_RenameFlipsHash ensures a path rename (with
// identical bytes) still flips the hash. Without this, the SDK could
// fail to invalidate a renamed asset URL.
func TestFrontendAssetDigest_RenameFlipsHash(t *testing.T) {
	a := &bundle.Bundle{
		Manifest: manifest.Manifest{Frontend: &manifest.FrontendSpec{Format: "federation"}},
		Frontend: map[string][]byte{"frontend/old.js": []byte("same")},
	}
	b := &bundle.Bundle{
		Manifest: manifest.Manifest{Frontend: &manifest.FrontendSpec{Format: "federation"}},
		Frontend: map[string][]byte{"frontend/new.js": []byte("same")},
	}
	_, h1 := frontendAssetDigest(a)
	_, h2 := frontendAssetDigest(b)
	if h1 == h2 {
		t.Fatalf("rename must flip hash: %s == %s", h1, h2)
	}
}

// TestFrontendAssetDigest_EmptyBundle returns nil/"" so the broadcaster
// emits a clean uninstall-shaped payload without bogus paths.
func TestFrontendAssetDigest_EmptyBundle(t *testing.T) {
	cases := []*bundle.Bundle{
		nil,
		{},
		{Manifest: manifest.Manifest{Frontend: nil}},
		{Manifest: manifest.Manifest{Frontend: &manifest.FrontendSpec{Format: "script"}}},
		{Manifest: manifest.Manifest{Frontend: &manifest.FrontendSpec{Format: "federation"}}, Frontend: nil},
	}
	for i, b := range cases {
		paths, hash := frontendAssetDigest(b)
		if len(paths) != 0 || hash != "" {
			t.Fatalf("case %d: expected empty, got paths=%v hash=%q", i, paths, hash)
		}
	}
}

// TestChangeActionConstants protects the stringer values consumers rely
// on — flipping these breaks the SDK reducer.
func TestChangeActionConstants(t *testing.T) {
	if string(ChangeActionInstall) != "install" {
		t.Fatalf("ChangeActionInstall = %q", ChangeActionInstall)
	}
	if string(ChangeActionUpgrade) != "upgrade" {
		t.Fatalf("ChangeActionUpgrade = %q", ChangeActionUpgrade)
	}
	if string(ChangeActionUninstall) != "uninstall" {
		t.Fatalf("ChangeActionUninstall = %q", ChangeActionUninstall)
	}
}
