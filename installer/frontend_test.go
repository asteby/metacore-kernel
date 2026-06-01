package installer

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFrontend_WritesFilesIdempotently(t *testing.T) {
	base := t.TempDir()
	files := map[string][]byte{
		"frontend/remoteEntry.js":  []byte("// remote entry"),
		"frontend/assets/main.js":  []byte("console.log('main')"),
		"frontend/assets/app.css":  []byte("body{color:red}"),
	}

	if err := WriteFrontend(base, "tickets", files); err != nil {
		t.Fatalf("WriteFrontend: %v", err)
	}

	cases := map[string]string{
		filepath.Join(base, "tickets", "remoteEntry.js"):      "// remote entry",
		filepath.Join(base, "tickets", "assets", "main.js"):   "console.log('main')",
		filepath.Join(base, "tickets", "assets", "app.css"):   "body{color:red}",
	}
	for path, want := range cases {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if string(got) != want {
			t.Errorf("%s: got %q want %q", path, got, want)
		}
	}

	// idempotent re-write with new content
	files["frontend/remoteEntry.js"] = []byte("// v2")
	if err := WriteFrontend(base, "tickets", files); err != nil {
		t.Fatalf("WriteFrontend (2nd): %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(base, "tickets", "remoteEntry.js"))
	if string(got) != "// v2" {
		t.Errorf("idempotent overwrite failed: got %q", got)
	}
}

// TestWriteFrontend_AtomicReplaceRemovesStaleChunks pins the upgrade behaviour:
// content-hashed chunks from a previous version must NOT survive a re-write.
// This is the regression guard for the "host serves the old federated addon UI
// after an upgrade" bug — stale register-*.js chunks accumulating on disk.
func TestWriteFrontend_AtomicReplaceRemovesStaleChunks(t *testing.T) {
	base := t.TempDir()

	// v1 build: remoteEntry references an old hashed chunk.
	v1 := map[string][]byte{
		"frontend/remoteEntry.js":            []byte(`import "./assets/register-OLD.js"`),
		"frontend/assets/register-OLD.js":    []byte(`/* UUID de cuenta */`),
	}
	if err := WriteFrontend(base, "accounting_lite", v1); err != nil {
		t.Fatalf("WriteFrontend v1: %v", err)
	}

	// v2 build: NEW hashed chunk name, remoteEntry references it.
	v2 := map[string][]byte{
		"frontend/remoteEntry.js":            []byte(`import "./assets/register-NEW.js"`),
		"frontend/assets/register-NEW.js":    []byte(`/* Buscar diario */`),
	}
	if err := WriteFrontend(base, "accounting_lite", v2); err != nil {
		t.Fatalf("WriteFrontend v2: %v", err)
	}

	// The stale chunk must be gone.
	stale := filepath.Join(base, "accounting_lite", "assets", "register-OLD.js")
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale chunk %s should have been removed, stat err=%v", stale, err)
	}
	// The new chunk must be present.
	fresh := filepath.Join(base, "accounting_lite", "assets", "register-NEW.js")
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("new chunk %s missing: %v", fresh, err)
	}
	// remoteEntry must reference the new chunk only.
	re, _ := os.ReadFile(filepath.Join(base, "accounting_lite", "remoteEntry.js"))
	if string(re) != `import "./assets/register-NEW.js"` {
		t.Errorf("remoteEntry not refreshed: got %q", re)
	}
}

// TestWriteFrontend_RejectsEmptyKey guards the RemoveAll: an empty/relative key
// must never resolve `dir` to basePath itself and wipe sibling addons.
func TestWriteFrontend_RejectsEmptyKey(t *testing.T) {
	base := t.TempDir()
	sibling := filepath.Join(base, "other_addon", "remoteEntry.js")
	if err := os.MkdirAll(filepath.Dir(sibling), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sibling, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"", ".", "../escape", "/abs", "nested/key"} {
		if err := WriteFrontend(base, key, map[string][]byte{"frontend/x.js": {1}}); err == nil {
			t.Errorf("key %q: expected error, got nil", key)
		}
	}
	// The sibling addon must be untouched.
	if got, _ := os.ReadFile(sibling); string(got) != "keep me" {
		t.Errorf("sibling addon was clobbered: got %q", got)
	}
}

func TestWriteFrontend_EmptyBaseNoop(t *testing.T) {
	if err := WriteFrontend("", "tickets", map[string][]byte{"a": {1}}); err != nil {
		t.Fatalf("empty base should be no-op, got %v", err)
	}
}

func TestWriteFrontend_RejectsTraversal(t *testing.T) {
	base := t.TempDir()
	err := WriteFrontend(base, "tickets", map[string][]byte{
		"frontend/../../etc/passwd": []byte("evil"),
	})
	if err == nil {
		t.Fatal("expected traversal error, got nil")
	}
}

func TestFrontendDir(t *testing.T) {
	got := FrontendDir("/srv/metacore", "tickets")
	want := filepath.Join("/srv/metacore", "tickets")
	if got != want {
		t.Errorf("FrontendDir: got %q want %q", got, want)
	}
}
