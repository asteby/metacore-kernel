package modelbase

import "testing"

// specModel declares its own import spec; derivedModel does not, so it must
// fall back to the projection of its form fields.
type specModel struct{}

func (specModel) DefineImport() ImportSpec {
	return ImportSpec{
		Columns: []ImportColumn{{Key: "user.email", Header: "Email", Required: true}},
		MaxRows: 5,
	}
}

func TestDeriveImportSpecSkipsNonImportableFields(t *testing.T) {
	modal := ModalMetadata{Fields: []FieldDef{
		{Key: "name", Label: "Nombre", Required: true},
		{Key: "avatar", Label: "Foto", Type: "image"},
		{Key: "specialty_id", Label: "Especialidad", Type: "dynamic_select"},
		{Key: "bio", Label: "Biografía", Type: "textarea", Placeholder: "Texto libre"},
	}}

	spec := DeriveImportSpec(modal)

	if len(spec.Columns) != 2 {
		t.Fatalf("columns: got %d want 2 (%+v)", len(spec.Columns), spec.Columns)
	}
	if spec.Columns[0].Header != "Nombre" || !spec.Columns[0].Required {
		t.Errorf("first column: got %+v", spec.Columns[0])
	}
	if spec.Columns[1].Hint != "Texto libre" {
		t.Errorf("placeholder should become the hint: got %q", spec.Columns[1].Hint)
	}
}

func TestDeriveImportSpecAcceptsFieldKeyAsAlias(t *testing.T) {
	spec := DeriveImportSpec(ModalMetadata{Fields: []FieldDef{
		{Key: "license_number", Label: "Cédula profesional"},
	}})

	col, ok := spec.HeaderIndex()[normalizeHeader("license_number")]
	if !ok {
		t.Fatal("raw field key must resolve, so an exported CSV feeds back in")
	}
	if col.Header != "Cédula profesional" {
		t.Errorf("alias resolved to the wrong column: %+v", col)
	}
}

func TestResolveImportSpecPrefersTheModelDeclaration(t *testing.T) {
	modal := ModalMetadata{Fields: []FieldDef{{Key: "name", Label: "Nombre"}}}

	spec := ResolveImportSpec(specModel{}, modal)

	if len(spec.Columns) != 1 || spec.Columns[0].Key != "user.email" {
		t.Fatalf("declaration must win over derivation: %+v", spec.Columns)
	}
	if spec.Limit() != 5 {
		t.Errorf("Limit: got %d want 5", spec.Limit())
	}
}

func TestResolveImportSpecFallsBackWhenUndeclared(t *testing.T) {
	spec := ResolveImportSpec(struct{}{}, ModalMetadata{Fields: []FieldDef{
		{Key: "name", Label: "Nombre"},
	}})

	if len(spec.Columns) != 1 || spec.Columns[0].Header != "Nombre" {
		t.Fatalf("derived spec expected: %+v", spec.Columns)
	}
	if spec.Limit() != DefaultImportMaxRows {
		t.Errorf("Limit: got %d want %d", spec.Limit(), DefaultImportMaxRows)
	}
}

func TestHeaderIndexIgnoresCaseAndRequiredMarker(t *testing.T) {
	spec := ImportSpec{Columns: []ImportColumn{
		{Key: "user.email", Header: "Email", Required: true, Aliases: []string{"Correo electrónico"}},
	}}
	idx := spec.HeaderIndex()

	for _, header := range []string{"Email *", "  email  ", "EMAIL", "correo electrónico"} {
		if _, ok := idx[normalizeHeader(header)]; !ok {
			t.Errorf("header %q must resolve", header)
		}
	}
	if got := spec.Columns[0].TemplateHeader(); got != "Email *" {
		t.Errorf("TemplateHeader: got %q want %q", got, "Email *")
	}
}

// addonLikeModel stands in for a ModelDefiner a host synthesises from a
// manifest: no hand-written DefineImport, just the decoded spec embedded.
type addonLikeModel struct {
	StaticImportSpec
}

func TestStaticImportSpecCoversTheManifestPath(t *testing.T) {
	model := addonLikeModel{StaticImportSpec{Spec: ImportSpec{
		Columns: []ImportColumn{{Key: "sku", Header: "SKU", Required: true}},
		MaxRows: 20,
	}}}

	// The modal passed here is the derived fallback; the declared spec must win,
	// exactly as it does for a compiled Go model.
	spec := ResolveImportSpec(model, ModalMetadata{Fields: []FieldDef{{Key: "other", Label: "Otro"}}})

	if len(spec.Columns) != 1 || spec.Columns[0].Key != "sku" {
		t.Fatalf("manifest-declared spec must win: %+v", spec.Columns)
	}
	if spec.Limit() != 20 {
		t.Errorf("Limit: got %d want 20", spec.Limit())
	}
}
