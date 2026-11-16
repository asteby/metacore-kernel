package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/agent/tools"
)

func searchFixture() *GuideTurn {
	return NewGuideTurn([]GuideDescriptor{
		{ID: "synth.create.customers", Title: "Crear cliente", Description: "Te guío para dar de alta un cliente nuevo"},
		{ID: "synth.list.customers", Title: "Ver clientes", Description: "Abre la lista de clientes y sus filtros"},
		{ID: "billing.invoice.new", Title: "Emitir factura", Description: "Facturación electrónica: timbra una factura"},
		{ID: "inventory.adjust", Title: "Ajustar inventario", Description: "Corrige existencias de un almacén"},
		{ID: "settings.users.invite", Title: "Invitar usuario", Description: "Agrega a una persona a la organización"},
		{ID: "pos.sale", Title: "Registrar venta en caja", Description: "Cobra una venta en el punto de venta"},
	})
}

func guideIDs(gs []GuideDescriptor) []string {
	out := make([]string, len(gs))
	for i, g := range gs {
		out[i] = g.ID
	}
	return out
}

func TestGuideSearchRanksByKeywords(t *testing.T) {
	turn := searchFixture()
	cases := []struct {
		query, first string
	}{
		{"¿Cómo creo un CLIENTE?", "synth.create.customers"}, // case, punctuation, stopwords
		{"facturación", "billing.invoice.new"},               // accent-insensitive, derived form
		{"facturas", "billing.invoice.new"},                  // plural
		{"inventario almacen", "inventory.adjust"},           // almacén without accent
		{"invitar usuarios", "settings.users.invite"},
		{"venta caja", "pos.sale"},
		{"customers", "synth.create.customers"}, // id words count too
	}
	for _, c := range cases {
		got := turn.Search(c.query, 8)
		if len(got) == 0 || got[0].ID != c.first {
			t.Errorf("Search(%q) = %v, want %s first", c.query, guideIDs(got), c.first)
		}
	}
	if got := turn.Search("de la para el", 8); len(got) != 0 {
		t.Errorf("stopwords alone must match nothing: %v", guideIDs(got))
	}
	if got := turn.Search("nómina", 8); len(got) != 0 {
		t.Errorf("unrelated query must match nothing: %v", guideIDs(got))
	}
	var nilTurn *GuideTurn
	if nilTurn.Search("x", 8) != nil {
		t.Error("nil turn")
	}
}

func TestGuideSearchCapsResultsAndKeepsCatalogOrderOnTies(t *testing.T) {
	var catalog []GuideDescriptor
	for i := 0; i < 20; i++ {
		catalog = append(catalog, GuideDescriptor{ID: fmt.Sprintf("synth.list.m%02d", i), Title: fmt.Sprintf("Ver reporte %02d", i)})
	}
	got := NewGuideTurn(catalog).Search("reporte", maxGuideSearchResults)
	if len(got) != maxGuideSearchResults || got[0].ID != "synth.list.m00" || got[7].ID != "synth.list.m07" {
		t.Fatalf("got %v", guideIDs(got))
	}
}

func TestSearchGuidesToolReturnsCompactLines(t *testing.T) {
	turn := searchFixture()
	out, err := NewSearchGuidesTool(turn).Execute(context.Background(), map[string]interface{}{"query": "crear cliente"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	lines := out.(map[string]interface{})["guides"].([]string)
	if len(lines) == 0 || len(lines) > maxGuideSearchResults || !strings.HasPrefix(lines[0], "synth.create.customers — Crear cliente: ") {
		t.Fatalf("lines %v", lines)
	}
	for _, l := range lines {
		if n := len([]rune(l)); n > 220 {
			t.Errorf("line too long (%d): %s", n, l)
		}
	}

	out, _ = NewSearchGuidesTool(turn).Execute(context.Background(), map[string]interface{}{"query": "nómina"}, nil)
	if m := out.(map[string]interface{}); len(m["guides"].([]string)) != 0 || !strings.Contains(m["note"].(string), "Ninguna") {
		t.Fatalf("no match: %v", m)
	}
	out, _ = NewSearchGuidesTool(NewGuideTurn(nil)).Execute(context.Background(), map[string]interface{}{"query": "cliente"}, nil)
	if m := out.(map[string]interface{}); !strings.Contains(m["note"].(string), "No hay guías") {
		t.Fatalf("empty catalog: %v", m)
	}
}

func TestStartGuideUnknownIDSuggestsClosest(t *testing.T) {
	turn := searchFixture()
	_, err := NewStartGuideTool(turn).Execute(context.Background(), map[string]interface{}{"guide_id": "synth.new.customers"}, nil)
	if err == nil || !strings.Contains(err.Error(), "synth.create.customers (Crear cliente)") || !strings.Contains(err.Error(), "search_guides") {
		t.Fatalf("want suggestions, got %v", err)
	}
	_, err = NewStartGuideTool(turn).Execute(context.Background(), map[string]interface{}{"guide_id": "zzz"}, nil)
	if err == nil || !strings.Contains(err.Error(), "search_guides") {
		t.Fatalf("want a pointer to search_guides, got %v", err)
	}
	_, err = NewStartGuideTool(NewGuideTurn(nil)).Execute(context.Background(), map[string]interface{}{"guide_id": "x"}, nil)
	if err == nil || !strings.Contains(err.Error(), "no hay guías") {
		t.Fatalf("empty catalog: %v", err)
	}
	if turn.Chosen() != nil {
		t.Fatal("failed calls must not choose a guide")
	}
}

// The guide tools' definitions must not depend on the catalog: the tools
// block is part of the cached prompt prefix.
func TestGuideToolDefinitionsIgnoreCatalog(t *testing.T) {
	defs := func(turn *GuideTurn) string {
		reg := tools.NewToolRegistry()
		reg.Register(NewStartGuideTool(turn))
		reg.Register(NewSearchGuidesTool(turn))
		raw, err := json.Marshal(reg.ToOpenAIFormat())
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	a, b, c := defs(searchFixture()), defs(NewGuideTurn(nil)), defs(NewGuideTurn(fixtureGuides(60, "b")))
	if a != b || a != c {
		t.Fatalf("guide tool definitions vary with the catalog:\n%s\n%s", a, c)
	}
}

func TestTurnRegistryAddsGuideToolsWhenTurnAttached(t *testing.T) {
	o := NewAgentOrchestrator(OrchestratorConfig{})
	if _, ok := o.turnRegistry(context.Background(), Turn{}).Get("search_guides"); ok {
		t.Fatal("no guide turn, no guide tools")
	}
	reg := o.turnRegistry(WithGuideTurn(context.Background(), NewGuideTurn(nil)), Turn{})
	for _, name := range []string{"start_guide", "search_guides"} {
		if _, ok := reg.Get(name); !ok {
			t.Fatalf("%s missing", name)
		}
	}
	if _, ok := o.toolRegistry.Get("start_guide"); ok {
		t.Fatal("guide tools leaked into the base registry")
	}
}
