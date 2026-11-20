package agent

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/agent/contextmgr"
)

// MemoryFinalAnswerTool is final_answer with the optional memory fields
// (title, summary, facts). The copilot registers it over the plain
// final_answer when layered memory is on: the model labels its own turn, so
// the index never costs an extra LLM call.
type MemoryFinalAnswerTool struct{ FinalAnswerTool }

// NewMemoryFinalAnswerTool builds the memory-aware final_answer.
func NewMemoryFinalAnswerTool() *MemoryFinalAnswerTool { return &MemoryFinalAnswerTool{} }

// Parameters adds the optional memory fields to final_answer.
func (t *MemoryFinalAnswerTool) Parameters() map[string]interface{} {
	p := t.FinalAnswerTool.Parameters()
	p["title"] = map[string]interface{}{
		"type":        "string",
		"description": "Memoria: tema del turno (≤8 palabras).",
	}
	p["summary"] = map[string]interface{}{
		"type":        "string",
		"description": "Memoria: resultado con cifras clave (≤20 palabras).",
	}
	p["facts"] = map[string]interface{}{
		"type":        "array",
		"items":       map[string]interface{}{"type": "string"},
		"description": "Memoria: datos cortos a recordar (ids, totales).",
	}
	return p
}

// recallTurnTool returns an older turn of the session in full.
type recallTurnTool struct{ mem *copilotMemory }

func (t *recallTurnTool) Name() string     { return "recall_turn" }
func (t *recallTurnTool) Category() string { return "context" }
func (t *recallTurnTool) Description() string {
	return "Recupera un turno anterior (#n del índice de memoria, o query): pregunta, respuesta y datos de sus herramientas."
}
func (t *recallTurnTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"turn":  map[string]interface{}{"type": "integer", "description": "Número #n."},
		"query": map[string]interface{}{"type": "string", "description": "Palabras clave."},
	}
}

func (t *recallTurnTool) Execute(ctx context.Context, params map[string]interface{}, agentCtx *contextmgr.ConversationContext) (interface{}, error) {
	if agentCtx == nil {
		return nil, fmt.Errorf("recall_turn: no conversation")
	}
	turns, err := t.mem.sessionTurns(ctx, agentCtx.ConversationID, agentCtx.OrganizationID, agentCtx.UserID)
	if err != nil {
		return nil, err
	}
	if len(turns) == 0 {
		return map[string]interface{}{"status": "empty", "message": "Aún no hay turnos anteriores en la memoria."}, nil
	}
	if n, ok := intParam(params["turn"]); ok && n > 0 {
		for _, rec := range turns {
			if rec.Turn == n {
				return t.mem.recallPayload(rec), nil
			}
		}
		return nil, fmt.Errorf("no existe el turno #%d (hay del #%d al #%d)", n, turns[0].Turn, turns[len(turns)-1].Turn)
	}
	query, _ := params["query"].(string)
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("pasa turn (número) o query (palabras clave)")
	}
	if rec, ok := t.mem.match(query, nil, turns, true); ok {
		return t.mem.recallPayload(rec), nil
	}
	return map[string]interface{}{"status": "not_found", "message": "Ningún turno anterior coincide con «" + oneLine(query, 80) + "»."}, nil
}

// intParam reads an integer tool parameter (JSON numbers decode as float64).
func intParam(v interface{}) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	case string:
		i, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(n), "#"))
		return i, err == nil
	}
	return 0, false
}

// rememberFactTool stores a durable fact of the user.
type rememberFactTool struct{ mem *copilotMemory }

func (t *rememberFactTool) Name() string     { return "remember_fact" }
func (t *rememberFactTool) Category() string { return "context" }
func (t *rememberFactTool) Description() string {
	return "Guarda un dato o preferencia durable del usuario (ej. \"reporta en la moneda de la organización\"). Solo lo que seguirá valiendo."
}
func (t *rememberFactTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"fact": map[string]interface{}{"type": "string", "description": "Frase corta.", "required": true},
	}
}

func (t *rememberFactTool) Execute(ctx context.Context, params map[string]interface{}, agentCtx *contextmgr.ConversationContext) (interface{}, error) {
	if agentCtx == nil || agentCtx.UserID == uuid.Nil {
		return nil, fmt.Errorf("remember_fact: no user")
	}
	text, _ := params["fact"].(string)
	text = oneLine(text, memoryFactRunes)
	if text == "" {
		return nil, fmt.Errorf("fact is required")
	}
	facts, err := t.mem.cfg.Store.ListFacts(ctx, agentCtx.OrganizationID, agentCtx.UserID)
	if err != nil {
		return nil, err
	}
	for _, f := range facts {
		if strings.EqualFold(f.Text, text) {
			return map[string]interface{}{"status": "already_known", "fact": f.Text}, nil
		}
	}
	if len(facts) >= t.mem.cfg.MaxFacts {
		return nil, fmt.Errorf("ya hay %d datos guardados (máximo); olvida uno con forget_fact antes", len(facts))
	}
	f := MemoryFact{ID: uuid.New(), OrganizationID: agentCtx.OrganizationID, UserID: agentCtx.UserID, Text: text, CreatedAt: time.Now().UTC()}
	if err := t.mem.cfg.Store.SaveFact(ctx, f); err != nil {
		return nil, err
	}
	return map[string]interface{}{"status": "remembered", "fact": text}, nil
}

// forgetFactTool removes a durable fact of the user.
type forgetFactTool struct{ mem *copilotMemory }

func (t *forgetFactTool) Name() string     { return "forget_fact" }
func (t *forgetFactTool) Category() string { return "context" }
func (t *forgetFactTool) Description() string {
	return "Olvida un dato guardado del usuario."
}
func (t *forgetFactTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"fact": map[string]interface{}{"type": "string", "description": "Número o texto del dato.", "required": true},
	}
}

func (t *forgetFactTool) Execute(ctx context.Context, params map[string]interface{}, agentCtx *contextmgr.ConversationContext) (interface{}, error) {
	if agentCtx == nil || agentCtx.UserID == uuid.Nil {
		return nil, fmt.Errorf("forget_fact: no user")
	}
	ref := strings.TrimSpace(fmt.Sprint(params["fact"]))
	if params["fact"] == nil || ref == "" {
		return nil, fmt.Errorf("fact is required")
	}
	facts, err := t.mem.cfg.Store.ListFacts(ctx, agentCtx.OrganizationID, agentCtx.UserID)
	if err != nil {
		return nil, err
	}
	var target *MemoryFact
	if n, ok := intParam(params["fact"]); ok && n >= 1 && n <= len(facts) {
		target = &facts[n-1]
	} else {
		low := strings.ToLower(ref)
		for i := range facts {
			if strings.Contains(strings.ToLower(facts[i].Text), low) {
				target = &facts[i]
				break
			}
		}
	}
	if target == nil {
		return map[string]interface{}{"status": "not_found", "message": "No hay un dato guardado que coincida."}, nil
	}
	if err := t.mem.cfg.Store.DeleteFact(ctx, agentCtx.OrganizationID, agentCtx.UserID, target.ID); err != nil {
		return nil, err
	}
	return map[string]interface{}{"status": "forgotten", "fact": target.Text}, nil
}
