package agent

import (
	"context"
	"strings"
	"testing"
)

func TestNewGuideTurnSanitizesCatalog(t *testing.T) {
	turn := NewGuideTurn([]GuideDescriptor{
		{ID: "synth.create.customers", Title: "Crear cliente", Description: "Te guío  para\ncrear un cliente"},
		{ID: "synth.create.customers", Title: "duplicado"},
		{ID: "", Title: "sin id"},
		{ID: "sin.titulo"},
		{ID: "long", Title: strings.Repeat("x", 500)},
	})
	if len(turn.catalog) != 2 {
		t.Fatalf("catalog = %#v", turn.catalog)
	}
	if turn.catalog[0].Description != "Te guío para crear un cliente" {
		t.Fatalf("description not normalized: %q", turn.catalog[0].Description)
	}
	if n := len([]rune(turn.catalog[1].Title)); n > maxGuideTextRunes+1 {
		t.Fatalf("title not clipped: %d runes", n)
	}
}

func TestStartGuideToolOnlyAcceptsAdvertisedIDs(t *testing.T) {
	turn := NewGuideTurn([]GuideDescriptor{{ID: "synth.create.customers", Title: "Crear cliente"}})
	tool := NewStartGuideTool(turn)

	if _, has := tool.Parameters()["guide_id"].(map[string]interface{})["enum"]; has {
		t.Fatal("guide_id must not carry an enum of the catalog")
	}
	if strings.Contains(tool.Description(), "synth.create.customers") {
		t.Fatalf("description must not list the catalog: %s", tool.Description())
	}

	if _, err := tool.Execute(context.Background(), map[string]interface{}{"guide_id": "inventado"}, nil); err == nil {
		t.Fatal("unknown guide must fail")
	}
	if turn.Chosen() != nil {
		t.Fatal("failed call must not choose a guide")
	}
	if _, err := tool.Execute(context.Background(), map[string]interface{}{"guide_id": "synth.create.customers"}, nil); err != nil {
		t.Fatal(err)
	}
	if got := turn.Chosen(); got == nil || got.ID != "synth.create.customers" || got.Confidence != 1 {
		t.Fatalf("chosen = %#v", got)
	}
}

func TestGuideTurnTravelsInContext(t *testing.T) {
	turn := NewGuideTurn(nil)
	if GuideTurnFrom(WithGuideTurn(context.Background(), turn)) != turn {
		t.Fatal("turn lost in context")
	}
	if GuideTurnFrom(context.Background()) != nil {
		t.Fatal("no turn expected")
	}
	var nilTurn *GuideTurn
	if nilTurn.Chosen() != nil {
		t.Fatal("nil turn has no choice")
	}
}
