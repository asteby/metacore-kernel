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
