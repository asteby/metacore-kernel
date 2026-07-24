package dyntest

import (
	"testing"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// columnRefOptionsManifestJSON declares a model whose columns opt into a
// FK relation picker (ref → dynamic_select) and a static choice list
// (options → select) directly on the column, so the native create/edit form
// renders both without a custom action.
const columnRefOptionsManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "store", "name": "Store", "version": "0.1.0" },
  "compatibility": { "requires": [ { "key": "kernel", "version": ">=0.1.0" } ] },
  "models": [
    {
      "key": "Order",
      "table": "addon_store_orders",
      "label": "Orders",
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true },
        { "name": "code", "type": "text" },
        { "name": "customer_id", "type": "uuid", "ref": "Customer" },
        {
          "name": "status",
          "type": "text",
          "options": [
            { "value": "open", "label": "Open", "color": "#22c55e", "icon": "Circle" },
            { "value": "closed", "label": "Closed", "color": "#ef4444", "image": "closed.png" }
          ]
        }
      ]
    }
  ]
}`

func TestColumnRefOptionsParseAndProject(t *testing.T) {
	m, err := v3.Parse([]byte(columnRefOptionsManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse rejected manifest with column ref/options: %v", err)
	}

	// (a) the v3 typed shape carries ref + options.
	cols3 := map[string]v3.Column{}
	for _, c := range m.Models[0].Columns {
		cols3[c.Name] = c
	}
	if cols3["customer_id"].Ref != "Customer" {
		t.Errorf("v3 customer_id.Ref = %q, want Customer", cols3["customer_id"].Ref)
	}
	if got := len(cols3["status"].Options.Static); got != 2 {
		t.Fatalf("v3 status.Options len = %d, want 2", got)
	}
	if o := cols3["status"].Options.Static[0]; o.Value != "open" || o.Label != "Open" || o.Color != "#22c55e" || o.Icon != "Circle" {
		t.Errorf("v3 status.Options[0] = %+v", o)
	}
	if o := cols3["status"].Options.Static[1]; o.Image != "closed.png" {
		t.Errorf("v3 status.Options[1].Image = %q, want closed.png", o.Image)
	}

	// (b) FromV3 projects ref + options onto the legacy ColumnDef carrier.
	host := manifest.FromV3(m)
	var def manifest.ModelDefinition
	for _, d := range host.ModelDefinitions {
		if d.ModelKey == "Order" {
			def = d
		}
	}
	legacy := map[string]manifest.ColumnDef{}
	for _, c := range def.Columns {
		legacy[c.Name] = c
	}
	if legacy["customer_id"].Ref != "Customer" {
		t.Errorf("legacy customer_id.Ref = %q, want Customer", legacy["customer_id"].Ref)
	}
	if got := len(legacy["status"].Options); got != 2 {
		t.Fatalf("legacy status.Options len = %d, want 2", got)
	}
	if o := legacy["status"].Options[1]; o.Value != "closed" || o.Color != "#ef4444" || o.Image != "closed.png" {
		t.Errorf("legacy status.Options[1] = %+v", o)
	}

	// (c) DeriveTableColumns projects ref + options onto the served column metadata.
	served := dynamic.DeriveTableColumns(def)
	scol := map[string]struct {
		ref     string
		options int
	}{}
	for _, c := range served {
		scol[c.Key] = struct {
			ref     string
			options int
		}{ref: c.Ref, options: len(c.Options)}
	}
	if scol["customer_id"].ref != "Customer" {
		t.Errorf("served table customer_id.Ref = %q, want Customer", scol["customer_id"].ref)
	}
	if scol["status"].options != 2 {
		t.Errorf("served table status.Options len = %d, want 2", scol["status"].options)
	}

	// (d) CRITICAL: DeriveFormFields renders the native create/edit form field
	// as dynamic_select (ref) / select (options) and forwards ref + options so
	// the SDK renders the pickers with NO custom action.
	fields := dynamic.DeriveFormFields(def)
	fmap := map[string]struct {
		ftype   string
		ref     string
		options int
		color   string
		image   string
	}{}
	for _, f := range fields {
		entry := struct {
			ftype   string
			ref     string
			options int
			color   string
			image   string
		}{ftype: f.Type, ref: f.Ref, options: len(f.Options)}
		if len(f.Options) == 2 {
			entry.color = f.Options[0].Color
			entry.image = f.Options[1].Image
		}
		fmap[f.Key] = entry
	}
	if fmap["customer_id"].ftype != "dynamic_select" {
		t.Errorf("form customer_id.Type = %q, want dynamic_select", fmap["customer_id"].ftype)
	}
	if fmap["customer_id"].ref != "Customer" {
		t.Errorf("form customer_id.Ref = %q, want Customer", fmap["customer_id"].ref)
	}
	if fmap["status"].ftype != "select" {
		t.Errorf("form status.Type = %q, want select", fmap["status"].ftype)
	}
	if fmap["status"].options != 2 {
		t.Errorf("form status.Options len = %d, want 2", fmap["status"].options)
	}
	if fmap["status"].color != "#22c55e" || fmap["status"].image != "closed.png" {
		t.Errorf("form status option visuals = color %q / image %q", fmap["status"].color, fmap["status"].image)
	}
}
