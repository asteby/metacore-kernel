package manifest_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
	"github.com/asteby/metacore-kernel/modelbase"
)

const uploadModel = `{
  "key": "ProductImage", "table": "product_images",
  "columns": [
    { "name": "id", "type": "uuid", "primary_key": true },
    { "name": "organization_id", "type": "uuid", "not_null": true },
    { "name": "url", "type": "text", "widget": "image", "display": "image_stack",
      "upload": { "accept": "image/*", "max_size": 10485760, "multiple": true,
                  "fills": { "width": "width", "height": "height" } } },
    { "name": "width", "type": "integer" },
    { "name": "height", "type": "integer", "readonly": true },
    { "name": "caption", "type": "text" }
  ]
}`

func uploadHost(t *testing.T, model string) manifest.Manifest {
	t.Helper()
	m, err := v3.Parse(recordPresentationManifest(model, ""))
	if err != nil {
		t.Fatalf("v3.Parse: %v", err)
	}
	return manifest.FromV3(m)
}

func TestFromV3_ColumnUpload_ProjectsToMetadata(t *testing.T) {
	host := uploadHost(t, uploadModel)
	md := host.ModelDefinitions[0]

	cols := map[string]modelbase.ColumnDef{}
	for _, c := range dynamic.DeriveTableColumns(md) {
		cols[c.Key] = c
	}
	u := cols["url"].Upload
	if u == nil || u.Accept != "image/*" || u.MaxSize != 10485760 || !u.Multiple ||
		u.Fills["width"] != "width" || u.Fills["height"] != "height" {
		t.Fatalf("tabla: upload = %+v", u)
	}
	if cols["caption"].Upload != nil {
		t.Fatal("una columna sin upload no debe llevarlo")
	}

	fields := map[string]modelbase.FieldDef{}
	for _, f := range dynamic.DeriveFormFields(md) {
		fields[f.Key] = f
	}
	fu := fields["url"].Upload
	if fu == nil || fu.Accept != "image/*" || !fu.Multiple || fu.Fills["width"] != "width" {
		t.Fatalf("formulario: upload = %+v", fu)
	}

	raw, _ := json.Marshal(fields["url"])
	for _, want := range []string{`"upload":{`, `"accept":"image/*"`, `"max_size":10485760`, `"multiple":true`, `"fills":{`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("JSON servido sin %s: %s", want, raw)
		}
	}
}

// Las columnas destino de fills salen readonly aunque el manifest no lo diga;
// el resto del formulario no cambia.
func TestDeriveFormFields_UploadFillTargetsAreReadonly(t *testing.T) {
	md := uploadHost(t, uploadModel).ModelDefinitions[0]
	fields := map[string]modelbase.FieldDef{}
	for _, f := range dynamic.DeriveFormFields(md) {
		fields[f.Key] = f
	}
	if !fields["width"].Readonly {
		t.Fatal("width (destino de fills, sin readonly en el manifest) debe servirse readonly")
	}
	if !fields["height"].Readonly {
		t.Fatal("height debe seguir readonly")
	}
	if fields["url"].Readonly || fields["caption"].Readonly {
		t.Fatalf("url/caption no son derivadas: url=%v caption=%v", fields["url"].Readonly, fields["caption"].Readonly)
	}
}

// Compatibilidad: sin upload nada cambia y la clave no aparece en el payload.
func TestFromV3_ColumnUpload_OmittedWhenUnset(t *testing.T) {
	plain := `{ "key": "ProductImage", "table": "product_images", "columns": [
    { "name": "id", "type": "uuid", "primary_key": true },
    { "name": "organization_id", "type": "uuid", "not_null": true },
    { "name": "url", "type": "text", "widget": "image" },
    { "name": "width", "type": "integer" } ] }`
	md := uploadHost(t, plain).ModelDefinitions[0]
	for _, f := range dynamic.DeriveFormFields(md) {
		if f.Readonly || f.Upload != nil {
			t.Fatalf("%s cambio sin upload: %+v", f.Key, f)
		}
	}
	raw, _ := json.Marshal(dynamic.DeriveTableColumns(md))
	if strings.Contains(string(raw), `"upload"`) {
		t.Fatalf("la clave upload no debe aparecer: %s", raw)
	}
}

// El plano legacy rechaza lo mismo que v3 (manifest construido sin pasar por v3).
func TestValidate_ColumnUploadLegacyPlane(t *testing.T) {
	good := uploadHost(t, uploadModel)
	if err := good.Validate("3.5.0"); err != nil && strings.Contains(err.Error(), "upload") {
		t.Fatalf("manifest valido rechazado: %v", err)
	}
	cases := map[string]func(c *manifest.ColumnDef){
		"widget":      func(c *manifest.ColumnDef) { c.Widget = "textarea" },
		"max_size":    func(c *manifest.ColumnDef) { c.Upload.MaxSize = -5 },
		"inexistente": func(c *manifest.ColumnDef) { c.Upload.Fills["width"] = "nope" },
		"tipo":        func(c *manifest.ColumnDef) { c.Upload.Fills["width"] = "caption" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			h := uploadHost(t, uploadModel)
			for i := range h.ModelDefinitions[0].Columns {
				if h.ModelDefinitions[0].Columns[i].Name == "url" {
					mutate(&h.ModelDefinitions[0].Columns[i])
				}
			}
			err := h.Validate("3.5.0")
			if err == nil || !strings.Contains(err.Error(), "upload") {
				t.Fatalf("debia fallar por upload: %v", err)
			}
		})
	}
}
