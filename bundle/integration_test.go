package bundle_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/asteby/metacore-kernel/bundle"
	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/manifest"
)

// TestWriteReadRoundTrip builds a bundle in memory, serializes it via Write,
// reads it back with Read and verifies the Manifest validates against the
// kernel API version, that migrations round-trip in deterministic lexical
// order, and that frontend + readme content survive byte-for-byte.
func TestWriteReadRoundTrip(t *testing.T) {
	orig := &bundle.Bundle{
		Manifest: manifest.Manifest{
			Key:         "demo",
			Name:        "Demo",
			Description: "round-trip fixture",
			Version:     "1.2.3",
			Category:    "utility",
			Icon:        "Package",
			Kernel:      ">=2.0.0 <3.0.0",
			ModelDefinitions: []manifest.ModelDefinition{
				{
					TableName: "demo_items",
					ModelKey:  "demo_items",
					Label:     "Items",
					Columns: []manifest.ColumnDef{
						{Name: "title", Type: "string", Size: 120, Required: true},
					},
				},
			},
			Capabilities: []manifest.Capability{
				{Kind: "db:read", Target: "users", Reason: "names"},
			},
			Frontend: &manifest.FrontendSpec{Entry: "./re.js", Format: "federation", Expose: "./plugin"},
		},
		// Intentionally out of order — Read must sort them.
		Migrations: []dynamic.File{
			{Version: "0002_seed", SQL: "-- seed\n"},
			{Version: "0001_init", SQL: "CREATE TABLE demo_items ();\n"},
		},
		Frontend: map[string][]byte{
			"frontend/remoteEntry.js": []byte("console.log('hi');"),
			"frontend/assets/a.css":   []byte("body{}"),
		},
		Readme: "# Demo\n",
	}

	var buf bytes.Buffer
	if err := bundle.Write(&buf, orig); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Persist to a tempdir file to also confirm file-backed usage works.
	tmp := filepath.Join(t.TempDir(), "demo-1.2.3.tar.gz")
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	f, err := os.Open(tmp)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()

	got, err := bundle.Read(f, 0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if err := got.Manifest.Validate(manifest.APIVersion); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got.Manifest.Key != orig.Manifest.Key || got.Manifest.Version != orig.Manifest.Version {
		t.Fatalf("manifest mismatch: got %+v", got.Manifest)
	}
	if len(got.Migrations) != 2 {
		t.Fatalf("want 2 migrations, got %d", len(got.Migrations))
	}
	// Read enforces lexical order — 0001 must precede 0002.
	if got.Migrations[0].Version != "0001_init" || got.Migrations[1].Version != "0002_seed" {
		t.Fatalf("migrations out of order: %+v", got.Migrations)
	}
	if got.Migrations[0].SQL != orig.Migrations[1].SQL {
		t.Fatalf("migration 0001 SQL mismatch")
	}
	if string(got.Frontend["frontend/remoteEntry.js"]) != "console.log('hi');" {
		t.Fatalf("frontend payload corrupted: %q", got.Frontend["frontend/remoteEntry.js"])
	}
	if got.Readme != "# Demo\n" {
		t.Fatalf("readme mismatch: %q", got.Readme)
	}
}

// TestWriteDeterministic verifies two Writes of the same Bundle yield
// byte-identical output (reproducible builds).
func TestWriteDeterministic(t *testing.T) {
	b := &bundle.Bundle{
		Manifest: manifest.Manifest{
			Key: "demo", Name: "Demo", Version: "1.0.0", Category: "utility",
			Kernel: ">=2.0.0 <3.0.0",
			ModelDefinitions: []manifest.ModelDefinition{{
				TableName: "t", ModelKey: "t", Label: "T",
				Columns: []manifest.ColumnDef{{Name: "x", Type: "string"}},
			}},
		},
		Migrations: []dynamic.File{{Version: "0001_init", SQL: "-- noop"}},
		Frontend:   map[string][]byte{"frontend/x.js": []byte("x")},
	}
	var a, c bytes.Buffer
	if err := bundle.Write(&a, b); err != nil {
		t.Fatal(err)
	}
	if err := bundle.Write(&c, b); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), c.Bytes()) {
		t.Fatalf("bundle output not deterministic: %d vs %d bytes", a.Len(), c.Len())
	}
}
