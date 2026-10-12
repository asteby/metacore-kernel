package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

type fakeBrand struct{ p Personality }

func (f fakeBrand) PersonalityFor(uuid.UUID) Personality { return f.p }

type fixedClock struct{ t time.Time }

func (f fixedClock) Now(_, _ uuid.UUID) time.Time { return f.t }

func newCopilot(t *testing.T, ai AIService, mut func(*CopilotConfig)) (*CopilotService, *CRUDService) {
	t.Helper()
	crud, _ := newCRUD(t, nil)
	o := newTurnOrchestrator(t, ai)
	cfg := CopilotConfig{
		Orchestrator: o, CRUD: crud, Proposals: NewProposalStore(),
		Brand: fakeBrand{Personality{Name: "Llantonio", SystemPrompt: "Eres Llantonio.", PlatformName: "Pitsline", InternalNames: []string{"ops", "asteby"}, Addendum: "Habla de llantas."}},
		Clock: fixedClock{time.Date(2026, 10, 11, 9, 30, 0, 0, time.FixedZone("America/Mexico_City", -6*3600))},
	}
	if mut != nil {
		mut(&cfg)
	}
	svc, err := NewCopilotService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return svc, crud
}

func TestNewCopilotServiceValidates(t *testing.T) {
	if _, err := NewCopilotService(CopilotConfig{Brand: fakeBrand{}}); err == nil {
		t.Fatal("orchestrator required")
	}
	if _, err := NewCopilotService(CopilotConfig{Orchestrator: &AgentOrchestrator{}}); err == nil {
		t.Fatal("brand required")
	}
}

func TestBrandClauseNeverLeaksInternalNames(t *testing.T) {
	for _, name := range []string{"", "Ops", " ASTEBY "} {
		got := BrandClause(Personality{PlatformName: name, InternalNames: []string{"ops", "asteby"}})
		if !strings.Contains(got, "di \"el sistema\"") {
			t.Errorf("%q must not be offered as a brand: %s", name, got)
		}
	}
	if got := BrandClause(Personality{PlatformName: "Pitsline", InternalNames: []string{"ops"}}); !strings.Contains(got, "Pitsline") {
		t.Fatal(got)
	}
	if PersonalityClause(Personality{}) != "" || !strings.Contains(PersonalityClause(Personality{Addendum: " tono "}), "tono") {
		t.Fatal("addendum clause")
	}
}

func TestSystemPromptAssembly(t *testing.T) {
	svc, _ := newCopilot(t, &captureAI{}, nil)
	p, pers := svc.SystemPrompt(uuid.New(), uuid.New(), true)
	if pers.Name != "Llantonio" {
		t.Fatal("personality not returned")
	}
	order := []string{"Eres Llantonio.", "Pitsline", "PERSONALIDAD", "America/Mexico_City", "2026-10-11 09:30", "MODO LIVE"}
	last := -1
	for _, w := range order {
		i := strings.Index(p, w)
		if i < 0 || i < last {
			t.Fatalf("%q missing or out of order in:\n%s", w, p)
		}
		last = i
	}
	if strings.Contains(p, "MODO LIVE") != true {
		t.Fatal("live suffix")
	}
	if p2, _ := svc.SystemPrompt(uuid.New(), uuid.New(), false); strings.Contains(p2, "MODO LIVE") {
		t.Fatal("live suffix must be opt-in")
	}
}

func TestProcessCopilotMessageEndToEnd(t *testing.T) {
	ai := &captureAI{steps: []*AIResponse{finalAnswer("c", "Listo.")}}
	var prepared struct {
		org, user uuid.UUID
		live      bool
	}
	meta := json.RawMessage(`{"attachments":[{"url":"/storage/aby/p.png","mime_type":"image/png","name":"p.png"}]}`)
	svc, _ := newCopilot(t, ai, func(c *CopilotConfig) {
		c.PrepareContext = func(ctx context.Context, org, user uuid.UUID, live bool) context.Context {
			prepared.org, prepared.user, prepared.live = org, user, live
			return ctx
		}
		c.Conversations = &fakeConvStore{rows: []json.RawMessage{meta}}
	})
	org, user, session := uuid.New(), uuid.New(), uuid.New()
	pending := &MutationProposal{ID: uuid.New(), OrganizationID: org, UserID: user, SessionID: session, Status: "pending", Action: "create", Model: "customers"}
	svc.cfg.Proposals.Put(pending)

	res, err := svc.ProcessCopilotMessage(context.Background(), org, user, session, "hola", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Message != "Listo." || len(res.Proposals) != 1 || res.Proposals[0].ID != pending.ID {
		t.Fatalf("%+v", res)
	}
	if prepared.org != org || prepared.user != user || !prepared.live {
		t.Fatalf("PrepareContext args %+v", prepared)
	}
	if !strings.Contains(ai.prompts[0], "Eres Llantonio.") || !strings.Contains(ai.prompts[0], "MODO LIVE") {
		t.Fatal("prompt not assembled for the turn")
	}
	ctx := svc.cfg.Orchestrator.contextMgr.GetContext(session)
	if ctx.UserID != user || ctx.OrganizationID != org {
		t.Fatalf("copilot context must carry the human: %+v", ctx)
	}
}

func TestApplyProposalLifecycle(t *testing.T) {
	svc, crud := newCopilot(t, &captureAI{}, nil)
	org, user := uuid.New(), uuid.New()
	mk := func(action string) *MutationProposal {
		p := &MutationProposal{ID: uuid.New(), OrganizationID: org, UserID: user, Status: "pending", Action: action, Model: "customers", ModelTitle: "Clientes", Data: map[string]any{"name": "Ana"}, Embed: map[string]any{}}
		svc.cfg.Proposals.Put(p)
		return p
	}

	create := mk("create")
	if _, _, err := svc.ApplyProposal(org, uuid.New(), create.ID, true); err == nil {
		t.Fatal("another user must not decide the proposal")
	}
	if _, _, err := svc.ApplyProposal(uuid.New(), user, create.ID, true); err == nil {
		t.Fatal("another org must not decide the proposal")
	}
	p, res, err := svc.ApplyProposal(org, user, create.ID, true)
	if err != nil || p.Status != "approved" || res["name"] != "Ana" || p.RecordID == "" || p.Embed["title"] != "Ana" {
		t.Fatalf("create: %+v %v %v", p, res, err)
	}
	if _, res, err := svc.ApplyProposal(org, user, create.ID, true); err != nil || res["status"] != "already_applied" {
		t.Fatalf("double click: %v %v", res, err)
	}
	if _, _, err := svc.ApplyProposal(org, user, create.ID, false); err == nil {
		t.Fatal("rejecting an approved proposal must fail")
	}

	upd := mk("update")
	upd.RecordID = p.RecordID
	upd.Data = map[string]any{"name": "Ana B"}
	if _, res, err := svc.ApplyProposal(org, user, upd.ID, true); err != nil || res["status"] != "updated" {
		t.Fatalf("update: %v %v", res, err)
	}
	rec, err := crud.GetRecord(&AgentIdentity{OrganizationID: org}, "customers", p.RecordID)
	if err != nil || rec["name"] != "Ana B" {
		t.Fatalf("%v %v", rec, err)
	}

	rej := mk("delete")
	if q, _, err := svc.ApplyProposal(org, user, rej.ID, false); err != nil || q.Status != "rejected" {
		t.Fatalf("reject: %v", err)
	}
	if _, _, err := svc.ApplyProposal(org, user, rej.ID, false); err != nil {
		t.Fatal("rejecting twice is idempotent")
	}

	del := mk("delete")
	del.RecordID = p.RecordID
	if _, res, err := svc.ApplyProposal(org, user, del.ID, true); err != nil || res["status"] != "deleted" {
		t.Fatalf("delete: %v %v", res, err)
	}

	bad := mk("explode")
	if _, _, err := svc.ApplyProposal(org, user, bad.ID, true); err == nil {
		t.Fatal("unknown action")
	}
	if bad.Status != "pending" {
		t.Fatal("failed apply must leave the proposal pending")
	}
}

func TestApplyProposalsBatchStopsOnFirstError(t *testing.T) {
	svc, _ := newCopilot(t, &captureAI{}, nil)
	org, user := uuid.New(), uuid.New()
	a := &MutationProposal{ID: uuid.New(), OrganizationID: org, UserID: user, Status: "pending", Action: "create", Model: "customers", Data: map[string]any{"name": "A"}}
	b := &MutationProposal{ID: uuid.New(), OrganizationID: org, UserID: user, Status: "pending", Action: "explode"}
	c := &MutationProposal{ID: uuid.New(), OrganizationID: org, UserID: user, Status: "pending", Action: "create", Model: "customers", Data: map[string]any{"name": "C"}}
	for _, p := range []*MutationProposal{a, b, c} {
		svc.cfg.Proposals.Put(p)
	}
	done, err := svc.ApplyProposalsBatch(org, user, []uuid.UUID{a.ID, b.ID, c.ID}, true)
	if err == nil || len(done) != 1 || c.Status != "pending" {
		t.Fatalf("done=%d err=%v c=%s", len(done), err, c.Status)
	}
	svc.cfg.CRUD = nil
	d := &MutationProposal{ID: uuid.New(), OrganizationID: org, UserID: user, Status: "pending", Action: "create"}
	svc.cfg.Proposals.Put(d)
	if _, _, err := svc.ApplyProposal(org, user, d.ID, true); err == nil {
		t.Fatal("approving without CRUD must fail loudly")
	}
}
