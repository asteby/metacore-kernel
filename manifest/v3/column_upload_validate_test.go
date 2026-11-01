package v3

import (
	"strings"
	"testing"
)

// columnUploadManifest builds a minimal addon whose ProductImage model carries
// the given url column (with its upload block) plus width/height/size/mime
// siblings, so Parse runs the JSON schema and the cross-field validation.
func columnUploadManifest(urlCol string) []byte {
	return []byte(`{
      "apiVersion": "asteby.com/v3",
      "kind": "Addon",
      "metadata": {"key": "products", "name": "Products", "version": "1.0.0"},
      "compatibility": {"requires": [{"key": "kernel", "version": ">=3.0.0 <4.0.0"}]},
      "models": [{
        "key": "ProductImage", "table": "product_images",
        "columns": [
          {"name": "organization_id", "type": "uuid"},
          ` + urlCol + `,
          {"name": "width", "type": "integer"},
          {"name": "height", "type": "integer"},
          {"name": "bytes", "type": "bigint"},
          {"name": "mime_type", "type": "text"},
          {"name": "caption", "type": "text"},
          {"name": "ratio", "type": "numeric"}
        ]
      }]
    }`)
}

func TestColumnUpload_Validation(t *testing.T) {
	cases := []struct {
		name    string
		col     string
		wantErr string // "" = valid
	}{
		{"valido completo", `{"name":"url","type":"text","widget":"image","upload":{"accept":"image/*","max_size":10485760,"multiple":true,"fills":{"width":"width","height":"height","size":"bytes","mime":"mime_type"}}}`, ""},
		{"valido solo accept", `{"name":"url","type":"text","widget":"upload","upload":{"accept":".pdf"}}`, ""},
		{"valido widget file", `{"name":"url","type":"text","widget":"file","upload":{}}`, ""},
		{"sin upload sigue igual", `{"name":"url","type":"text","widget":"image"}`, ""},
		{"sin widget", `{"name":"url","type":"text","upload":{"accept":"image/*"}}`, "only allowed on columns with widget image|upload|file"},
		{"widget incorrecto", `{"name":"url","type":"text","widget":"textarea","upload":{"accept":"image/*"}}`, "only allowed on columns with widget"},
		{"max_size negativo", `{"name":"url","type":"text","widget":"image","upload":{"max_size":-1}}`, "max_size"},
		{"clave de fills desconocida", `{"name":"url","type":"text","widget":"image","upload":{"fills":{"depth":"width"}}}`, ""}, // lo corta el schema
		{"fills a columna inexistente", `{"name":"url","type":"text","widget":"image","upload":{"fills":{"width":"nope"}}}`, `"nope" is not a declared column`},
		{"fills a si misma", `{"name":"url","type":"text","widget":"image","upload":{"fills":{"mime":"url"}}}`, "cannot point at the column itself"},
		{"fills duplicado", `{"name":"url","type":"text","widget":"image","upload":{"fills":{"width":"width","height":"width"}}}`, "both target column"},
		{"width a texto", `{"name":"url","type":"text","widget":"image","upload":{"fills":{"width":"caption"}}}`, "must be a integer|bigint column"},
		{"width a numeric", `{"name":"url","type":"text","widget":"image","upload":{"fills":{"width":"ratio"}}}`, "must be a integer|bigint column"},
		{"mime a integer", `{"name":"url","type":"text","widget":"image","upload":{"fills":{"mime":"width"}}}`, "must be a text column"},
		{"clave extra en upload", `{"name":"url","type":"text","widget":"image","upload":{"crop":true}}`, ""}, // lo corta el schema
	}
	schemaOnly := map[string]bool{"clave de fills desconocida": true, "clave extra en upload": true}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(columnUploadManifest(tc.col))
			if schemaOnly[tc.name] {
				if err == nil {
					t.Fatal("el schema debia rechazar la clave desconocida")
				}
				return
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("debia ser valido: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, quiero %q", err, tc.wantErr)
			}
		})
	}
}

func TestColumnUpload_ParsesFields(t *testing.T) {
	m, err := Parse(columnUploadManifest(`{"name":"url","type":"text","widget":"image","upload":{"accept":"image/*","max_size":1024,"multiple":true,"fills":{"width":"width"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	u := m.Models[0].Columns[1].Upload
	if u == nil || u.Accept != "image/*" || u.MaxSize != 1024 || !u.Multiple || u.Fills["width"] != "width" {
		t.Fatalf("upload = %+v", u)
	}
}
