package dynamic

import (
	"testing"

	"github.com/asteby/metacore-kernel/modelbase"
)

func TestApplyClientColumnLabels(t *testing.T) {
	cols := []exportCol{
		{Key: "name", Label: "models.x.name"},
		{Key: "user.avatar", Label: "Avatar"},
	}
	applyClientColumnLabels(`{"name":"Nombre","user_avatar":"Usuario"}`, cols)
	if cols[0].Label != "Nombre" {
		t.Fatalf("name label = %q, want Nombre", cols[0].Label)
	}
	if cols[1].Label != "Usuario" {
		t.Fatalf("avatar label = %q, want Usuario (underscore alias)", cols[1].Label)
	}
}

func TestExportColsFromMetaSkipsHiddenAndImages(t *testing.T) {
	all, byKey := exportColsFromMeta([]modelbase.ColumnDef{
		{Key: "name", Label: "Nombre", Type: "text"},
		{Key: "photo", Label: "Foto", Type: "image"},
		{Key: "user.avatar", Label: "Usuario", Type: "image", CellStyle: "avatar", Tooltip: "user.name"},
		{Key: "secret", Label: "Secreto", Hidden: true},
		{Key: "actions", Label: "Acciones"},
	})
	if len(all) != 2 {
		t.Fatalf("got %d cols, want 2 (name + avatar)", len(all))
	}
	if _, ok := byKey["name"]; !ok {
		t.Fatal("missing name in byKey")
	}
	if _, ok := byKey["user_avatar"]; !ok {
		t.Fatal("missing underscore alias for dotted key")
	}
}

func TestFormatExportCellAvatarAndRelations(t *testing.T) {
	row := map[string]any{
		"user": map[string]any{
			"name":  "Ana",
			"email": "ana@example.com",
			"avatar": "a.png",
		},
		"specialties": []any{
			map[string]any{"name": "Cardiología"},
			map[string]any{"name": "Pediatría"},
		},
		"active": true,
	}

	avatar := formatExportCell(exportCol{
		Key:         "user.avatar",
		CellStyle:   "avatar",
		Tooltip:     "user.name",
		Description: "user.email",
	}, row)
	if avatar != "Ana <ana@example.com>" {
		t.Fatalf("avatar = %q", avatar)
	}

	rels := formatExportCell(exportCol{
		Key:          "specialties",
		CellStyle:    "relation-badge-list",
		DisplayField: "name",
	}, row)
	if rels != "Cardiología, Pediatría" {
		t.Fatalf("relations = %q", rels)
	}

	b := formatExportCell(exportCol{
		Key:  "active",
		Type: "boolean",
		Options: map[string]string{
			"true":  "Sí",
			"false": "No",
		},
	}, row)
	if b != "Sí" {
		t.Fatalf("bool = %q, want Sí", b)
	}
}

func TestFormatExportCellUsesMetadataLabelFallbackViaKey(t *testing.T) {
	// Headers are applied separately; this asserts readable nested maps.
	got := stringifyReadable(map[string]any{"name": "Clínica Norte"})
	if got != "Clínica Norte" {
		t.Fatalf("readable = %q", got)
	}
}
