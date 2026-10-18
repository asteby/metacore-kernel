package dataload_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/asteby/metacore-kernel/dataload"
	"github.com/asteby/metacore-kernel/i18n"
)

// El perfil de ejemplo (examples/catalogo.profile.json) es genérico: ningún giro
// ni plataforma de origen. Una organización lo ajusta con sus Overrides
// (examples/catalogo.overrides.json) sin tocar el perfil base.
func Example() {
	pb, _ := os.ReadFile("examples/catalogo.profile.json")
	prof, err := dataload.ParseProfile(pb)
	if err != nil {
		panic(err)
	}
	var ov dataload.Overrides
	ob, _ := os.ReadFile("examples/catalogo.overrides.json")
	if err := json.Unmarshal(ob, &ov); err != nil {
		panic(err)
	}

	// Un archivo grande se lee en streaming (JSON líneas aquí; NewCSVStreamSource
	// para CSV): Opener se llama una vez por pasada.
	body := []byte(`{"SKU":"a-1","Nombre":"  Taza  ","Precio de lista":"120","Tipo":"simple"}
{"SKU":"a-2","Nombre":"Playera","Precio de lista":"250","Tipo":"variable"}
{"SKU":"a-2","Nombre":"Playera repetida","Tipo":"variable"}
{"Nombre":"Sin SKU"}
`)
	src := dataload.NewJSONLinesSource("catalogo.jsonl", func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	})

	ctx := i18n.WithLanguage(context.Background(), "en")
	plan, err := dataload.StreamPlan(ctx, dataload.PlanRequest{
		Org: "org-ejemplo", Profile: prof, Overrides: &ov, Source: src, Target: dataload.NewMemoryTarget(),
	}, dataload.StreamOptions{OnPage: func(ctx context.Context, page []dataload.RowPlan) error {
		for _, rp := range page {
			for _, is := range rp.Issues {
				fmt.Printf("fila %d [%s] %s\n", rp.Row, is.Code, dataload.LocalizeIssue(ctx, is))
			}
		}
		return nil
	}})
	if err != nil {
		panic(err)
	}
	fmt.Println("create:", plan.Counts[dataload.ActionCreate], "duplicate:", plan.Counts[dataload.ActionDuplicate], "invalid:", plan.Counts[dataload.ActionInvalid])
	// Output:
	// fila 3 [dataload.row.duplicate] repeated identity; row 2 is used
	// fila 4 [importer.required] 'SKU' missing
	// create: 2 duplicate: 1 invalid: 1
}
