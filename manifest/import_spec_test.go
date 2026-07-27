package manifest

import (
	"encoding/json"
	"testing"

	"github.com/asteby/metacore-kernel/manifest/v3"
	"github.com/asteby/metacore-kernel/modelbase"
)

// TestImportSpecCarrierMatchesTheServedContract is the guard that keeps the two
// kinds of model — addon-owned (manifest) and compiled (Go struct) — feeding
// ONE import engine. The carrier never converts field by field at serve time:
// it round-trips through JSON into modelbase.ImportSpec, so a tag renamed on
// either side silently dropping a column is exactly what this test catches.
func TestImportSpecCarrierMatchesTheServedContract(t *testing.T) {
	carrier := ImportSpecDef{
		MaxRows:      50,
		SheetName:    "Médicos",
		Instructions: []string{"Una fila por doctor."},
		Columns: []ImportColumnDef{{
			Key:       "user.email",
			Header:    "Email",
			Aliases:   []string{"Correo"},
			Required:  true,
			Type:      "email",
			Example:   "ana@correo.com",
			Hint:      "Se usa para iniciar sesión",
			Generator: "random_secret",
		}},
	}

	raw, err := json.Marshal(carrier)
	if err != nil {
		t.Fatalf("marshal carrier: %v", err)
	}
	var served modelbase.ImportSpec
	if err := json.Unmarshal(raw, &served); err != nil {
		t.Fatalf("unmarshal into modelbase.ImportSpec: %v", err)
	}

	if served.MaxRows != 50 || served.SheetName != "Médicos" || len(served.Instructions) != 1 {
		t.Fatalf("spec-level fields lost in transit: %+v", served)
	}
	if len(served.Columns) != 1 {
		t.Fatalf("columns lost in transit: %+v", served.Columns)
	}
	got := served.Columns[0]
	want := modelbase.ImportColumn{
		Key:       "user.email",
		Header:    "Email",
		Aliases:   []string{"Correo"},
		Required:  true,
		Type:      "email",
		Example:   "ana@correo.com",
		Hint:      "Se usa para iniciar sesión",
		Generator: "random_secret",
	}
	if got.Key != want.Key || got.Header != want.Header || got.Required != want.Required ||
		got.Type != want.Type || got.Example != want.Example || got.Hint != want.Hint ||
		got.Generator != want.Generator || len(got.Aliases) != 1 || got.Aliases[0] != "Correo" {
		t.Errorf("column changed in transit:\n got %+v\nwant %+v", got, want)
	}
}

func TestMapImportSpecProjectsTheV3Block(t *testing.T) {
	got := mapImportSpec(&v3.ImportSpec{
		MaxRows:   10,
		SheetName: "Datos",
		Columns: []v3.ImportColumn{
			{Key: "name", Header: "Nombre", Required: true, Aliases: []string{"Nombre completo"}},
		},
	})

	if got == nil {
		t.Fatal("mapImportSpec dropped a declared block")
	}
	if got.MaxRows != 10 || got.SheetName != "Datos" || len(got.Columns) != 1 {
		t.Fatalf("projection lost fields: %+v", got)
	}
	if got.Columns[0].Header != "Nombre" || !got.Columns[0].Required ||
		len(got.Columns[0].Aliases) != 1 {
		t.Errorf("column projection: %+v", got.Columns[0])
	}
}

func TestMapImportSpecKeepsUndeclaredModelsOnTheDerivedPath(t *testing.T) {
	if got := mapImportSpec(nil); got != nil {
		t.Errorf("nil in must stay nil out, so the kernel derives the spec; got %+v", got)
	}
}
