package dyntest

import (
	"testing"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// s5s6ManifestJSON exercises the S5 (option/select/field visuals + image
// widget) and S6 (column display i18n) contract additions in one manifest:
//
//   - Product.image_url declares widget:"image" (S5.3 — image form field).
//   - Product.sku declares an i18n KEY label (S6 — must round-trip so the
//     host transformer can resolve it, not show the raw column name).
//   - the "set_status" action's "status" field carries STATIC options with
//     icon/color/image visuals (S5.1).
//   - the "set_status" action's "product_id" field is a dynamic_select with
//     label_image/label_icon/label_color mapping a remote column (S5.2).
const s5s6ManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "store", "name": "Store", "version": "0.1.0" },
  "compatibility": { "requires": [ { "key": "kernel", "version": ">=0.1.0" } ] },
  "models": [
    {
      "key": "Product",
      "table": "addon_store_products",
      "label": "Products",
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true },
        { "name": "organization_id", "type": "uuid", "not_null": true },
        { "name": "sku", "type": "text", "label": "models.store.product.sku" },
        { "name": "name", "type": "text" },
        { "name": "image_url", "type": "text", "display": "image", "widget": "image" }
      ]
    }
  ],
  "contributions": {
    "actions": [
      {
        "key": "set_status",
        "label": "Set status",
        "target_model": "Product",
        "handler": { "type": "wasm", "function": "SetStatus" },
        "fields": [
          {
            "key": "status",
            "type": "select",
            "options": [
              { "value": "active", "label": "Active", "icon": "Check", "color": "#16a34a" },
              { "value": "discontinued", "label": "Discontinued", "color": "#dc2626", "image": "https://cdn/x.png" }
            ]
          },
          {
            "key": "product_id",
            "type": "dynamic_select",
            "ref": "Product",
            "label_image": "image_url",
            "label_icon": "icon",
            "label_color": "status_color"
          }
        ]
      }
    ]
  }
}`

func TestFieldImageDisplayI18nParseAndMap(t *testing.T) {
	m, err := v3.Parse([]byte(s5s6ManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse rejected S5/S6 manifest: %v", err)
	}

	// --- S5.3: image widget on a text column round-trips to the form field. ---
	host := manifest.FromV3(m)
	var prod manifest.ModelDefinition
	for _, d := range host.ModelDefinitions {
		if d.ModelKey == "Product" {
			prod = d
		}
	}
	cols := map[string]manifest.ColumnDef{}
	for _, c := range prod.Columns {
		cols[c.Name] = c
	}
	if cols["image_url"].Widget != "image" {
		t.Errorf("image_url.Widget = %q, want image", cols["image_url"].Widget)
	}
	if cols["image_url"].CellStyle != "image" {
		t.Errorf("image_url.CellStyle = %q, want image", cols["image_url"].CellStyle)
	}
	// Derived form field honours the declared image widget.
	formFields := dynamic.DeriveFormFields(prod)
	ff := map[string]string{}
	for _, f := range formFields {
		ff[f.Key] = f.Type
	}
	if ff["image_url"] != "image" {
		t.Errorf("derived image_url form field type = %q, want image", ff["image_url"])
	}

	// --- S6: i18n key label on sku survives the conversion + derivation. ---
	if cols["sku"].Label != "models.store.product.sku" {
		t.Errorf("legacy sku.Label = %q, want the declared i18n key", cols["sku"].Label)
	}
	tableCols := dynamic.DeriveTableColumns(prod)
	tlabel := map[string]string{}
	for _, c := range tableCols {
		tlabel[c.Key] = c.Label
	}
	if tlabel["sku"] != "models.store.product.sku" {
		t.Errorf("derived sku column label = %q, want the i18n key (so the host transformer can resolve it)", tlabel["sku"])
	}
	// A column with NO declared label still falls back to the humanized name.
	if tlabel["name"] != "Name" {
		t.Errorf("derived name column label = %q, want humanized fallback \"Name\"", tlabel["name"])
	}

	// --- S5.1 + S5.2: action field visuals carry through FromV3. ---
	var action manifest.ActionDef
	for _, a := range host.Actions["Product"] {
		if a.Key == "set_status" {
			action = a
		}
	}
	fields := map[string]manifest.FieldDef{}
	for _, f := range action.Fields {
		fields[f.Key] = f
	}

	// Static option visuals.
	statusOpts := map[string]manifest.Option{}
	for _, o := range fields["status"].Options {
		statusOpts[o.Value] = o
	}
	if statusOpts["active"].Icon != "Check" || statusOpts["active"].Color != "#16a34a" {
		t.Errorf("active option icon/color = %q/%q, want Check/#16a34a", statusOpts["active"].Icon, statusOpts["active"].Color)
	}
	if statusOpts["discontinued"].Image != "https://cdn/x.png" || statusOpts["discontinued"].Color != "#dc2626" {
		t.Errorf("discontinued option image/color = %q/%q", statusOpts["discontinued"].Image, statusOpts["discontinued"].Color)
	}

	// Remote-model label visuals on the dynamic_select.
	pid := fields["product_id"]
	if pid.LabelImage != "image_url" || pid.LabelIcon != "icon" || pid.LabelColor != "status_color" {
		t.Errorf("product_id label_* = %q/%q/%q, want image_url/icon/status_color",
			pid.LabelImage, pid.LabelIcon, pid.LabelColor)
	}
}
