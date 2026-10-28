package agent

import (
	"context"
	"testing"

	"github.com/asteby/metacore-kernel/agent/contextmgr"
	"github.com/asteby/metacore-kernel/agent/tools"
	"github.com/asteby/metacore-kernel/provenance"
	"github.com/google/uuid"
)

// An approved proposal is "person with agent": assisted, authorized by the
// person who approved, pointing at the proposal and the session.
func TestApplyProposalStampsAssistedProvenance(t *testing.T) {
	svc, crud := newCopilot(t, &captureAI{}, func(c *CopilotConfig) {
		c.ProposalSummary = func(p *MutationProposal) string { return "Propuso crear " + p.ModelTitle }
		c.ProposalOrigin = "Borrador aprobado en el chat"
	})
	db := crud.cfg.DB
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	if err := provenance.Register(db); err != nil {
		t.Fatal(err)
	}
	org, user, session := uuid.New(), uuid.New(), uuid.New()
	p := &MutationProposal{ID: uuid.New(), OrganizationID: org, UserID: user, SessionID: session, Status: "pending", Action: "create", Model: "customers", ModelTitle: "Clientes", Data: map[string]any{"name": "Ana"}}
	svc.cfg.Proposals.Put(p)
	// The HTTP edge stamped the person (manual); approval must turn it into
	// assisted.
	ctx := provenance.WithContext(context.Background(), provenance.Human(user, provenance.ViaUI))
	got, _, err := svc.ApplyProposalContext(ctx, org, user, p.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	e := provenance.NewStore(db).Created(context.Background(), org, "customers", []string{got.RecordID})[got.RecordID]
	if e.Mode != string(provenance.ModeAssisted) || e.ActorKind != string(provenance.ActorAgent) || e.ActorName != "Llantonio" {
		t.Fatalf("entry: %+v", e)
	}
	if e.AuthorizedBy == nil || *e.AuthorizedBy != user || e.RefKind != provenance.RefProposal || e.RefID != p.ID.String() || e.SessionID == nil || *e.SessionID != session {
		t.Fatalf("refs: %+v", e)
	}
	if e.Summary != "Propuso crear Clientes" || e.Origin != "Borrador aprobado en el chat" {
		t.Fatalf("labels: %+v", e)
	}

	// Approved update by condition is recorded explicitly.
	up := &MutationProposal{ID: uuid.New(), OrganizationID: org, UserID: user, Status: "pending", Action: "update", Model: "customers", RecordID: got.RecordID, Data: map[string]any{"name": "Ana B"}}
	svc.cfg.Proposals.Put(up)
	if _, _, err := svc.ApplyProposalContext(context.Background(), org, user, up.ID, true); err != nil {
		t.Fatal(err)
	}
	h, _ := provenance.NewStore(db).History(context.Background(), org, "customers", got.RecordID, 0)
	if len(h) != 2 || h[1].Action != provenance.ActionUpdated || h[1].Mode != string(provenance.ModeAssisted) {
		t.Fatalf("history: %+v", h)
	}
}

func TestToolCallProvenance(t *testing.T) {
	user, session := uuid.New(), uuid.New()
	conv := &contextmgr.ConversationContext{UserID: user, ConversationID: session}
	call := tools.ToolCall{ID: "call-1", ToolName: "crear_evento_borrador"}

	// Person's manual provenance → agent on behalf of the person.
	ctx := provenance.WithContext(context.Background(), provenance.Human(user, provenance.ViaUI))
	p, ok := provenance.FromContext(toolCallProvenance(ctx, "Asistente", call, conv))
	if !ok || p.ActorKind != provenance.ActorAgent || p.OnBehalfOf == nil || *p.OnBehalfOf != user || p.RefID != "call-1" || p.Mode != provenance.ModeAutomated || p.ActorName != "Asistente" {
		t.Fatalf("chat tool: %+v", p)
	}
	// Upstream agent/automation keeps its actor and gains the ref.
	actx := provenance.WithContext(context.Background(), provenance.Agent("Asistente", provenance.ViaAutomation).WithOrigin("Resumen"))
	p, _ = provenance.FromContext(toolCallProvenance(actx, "Asistente", call, nil))
	if p.Via != provenance.ViaAutomation || p.RefKind != provenance.RefToolCall {
		t.Fatalf("automation tool: %+v", p)
	}
	// Nothing known → untouched.
	if _, ok := provenance.FromContext(toolCallProvenance(context.Background(), "Asistente", call, nil)); ok {
		t.Fatal("no person and no provenance must stay unstamped")
	}
}
