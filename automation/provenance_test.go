package automation

import (
	"context"
	"testing"

	"github.com/asteby/metacore-kernel/provenance"
	"github.com/google/uuid"
)

func TestRunProvenance(t *testing.T) {
	owner, caller, session := uuid.New(), uuid.New(), uuid.New()
	a := &Automation{ID: uuid.New(), Name: "Resumen diario", CreatedBy: owner}
	run := &Run{ID: uuid.New()}

	// Scheduled run: the agent on behalf of the owner, automated.
	p, ok := provenance.FromContext(runProvenance(context.Background(), "Asistente", a, Identity{UserID: owner}, run, false))
	if !ok || p.ActorKind != provenance.ActorAgent || p.ActorName != "Asistente" || p.OnBehalfOf == nil || *p.OnBehalfOf != owner {
		t.Fatalf("scheduled: %+v", p)
	}
	if p.Mode != provenance.ModeAutomated || p.Via != provenance.ViaAutomation || p.RefKind != provenance.RefAutomationRun || p.RefID != run.ID.String() || p.Origin != "Resumen diario" {
		t.Fatalf("scheduled refs: %+v", p)
	}

	// Dynamic tool from the chat: on behalf of the caller, via chat, session.
	p, _ = provenance.FromContext(runProvenance(context.Background(), "Asistente", a, Identity{UserID: caller, SessionID: session}, run, true))
	if *p.OnBehalfOf != caller || p.Via != provenance.ViaChat || p.SessionID == nil || *p.SessionID != session {
		t.Fatalf("tool: %+v", p)
	}

	// An upstream provenance (approved proposal) keeps its actor and mode.
	up := provenance.WithContext(context.Background(), provenance.Assisted("Asistente", caller, provenance.ViaChat))
	p, _ = provenance.FromContext(runProvenance(up, "Asistente", a, Identity{UserID: caller}, run, true))
	if p.Mode != provenance.ModeAssisted || p.RefKind != provenance.RefAutomationRun {
		t.Fatalf("upstream: %+v", p)
	}
}
