package dynamic

import (
	"context"
	"testing"

	"github.com/asteby/metacore-kernel/provenance"
	"github.com/google/uuid"
)

func TestCanonicalEventCarriesProvenance(t *testing.T) {
	if provenanceFromContext(context.Background()) != nil {
		t.Fatal("no provenance in ctx → nil")
	}
	u := uuid.New()
	ctx := provenance.WithContext(context.Background(), provenance.Assisted("Asistente", u, provenance.ViaChat))
	p := provenanceFromContext(ctx)
	if p == nil || p.Mode != provenance.ModeAssisted || *p.AuthorizedBy != u {
		t.Fatalf("got %+v", p)
	}
}
