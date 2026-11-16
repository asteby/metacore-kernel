package agent

import (
	"context"
	"encoding/json"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/agent/tools"
)

// copilotMemory is the layered memory of a CopilotService (see memory.go).
type copilotMemory struct {
	cfg MemoryConfig
}

func newCopilotMemory(cfg MemoryConfig) *copilotMemory {
	return &copilotMemory{cfg: cfg.withDefaults()}
}

// tools are the memory tools offered on every turn (also the first, so the
// tool block stays byte-identical across the conversation).
func (m *copilotMemory) tools() []tools.BaseTool {
	return []tools.BaseTool{
		NewMemoryFinalAnswerTool(),
		&recallTurnTool{mem: m},
		&rememberFactTool{mem: m},
		&forgetFactTool{mem: m},
	}
}

// sessionTurns lists the session's turns that belong to org+user.
func (m *copilotMemory) sessionTurns(ctx context.Context, sessionID, orgID, userID uuid.UUID) ([]TurnRecord, error) {
	all, err := m.cfg.Store.ListTurns(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	out := all[:0:0]
	for _, t := range all {
		if t.OrganizationID == orgID && t.UserID == userID {
			out = append(out, t)
		}
	}
	return out, nil
}

// embed vectorizes text with the configured Embedder (nil without one or on
// error: callers fall back to the lexical match).
func (m *copilotMemory) embed(text string) []float32 {
	if m.cfg.Embedder == nil || strings.TrimSpace(text) == "" {
		return nil
	}
	vec, err := m.cfg.Embedder.GenerateEmbedding(text)
	if err != nil {
		log.Printf("⚠️ copilot memory: embedding failed, using lexical match: %v", err)
		return nil
	}
	return vec
}

// match finds the turn closest to query: by embedding when vectors exist,
// else lexically. vec is the query's vector (computed when nil and embed).
func (m *copilotMemory) match(query string, vec []float32, candidates []TurnRecord, embed bool) (TurnRecord, bool) {
	if vec == nil && embed {
		vec = m.embed(query)
	}
	if vec != nil {
		if t, ok := bestSemanticTurn(vec, candidates, m.cfg.PrefetchThreshold); ok {
			return t, true
		}
	}
	return bestLexicalTurn(query, candidates, m.cfg.LexicalPrefetchThreshold)
}

// recallPayload is what recall_turn returns for rec, within RecallMaxBytes.
func (m *copilotMemory) recallPayload(rec TurnRecord) map[string]interface{} {
	out := map[string]interface{}{
		"turn":    rec.Turn,
		"title":   rec.Title,
		"summary": rec.Summary,
		"user":    rec.UserMessage,
		"answer":  rec.Answer,
	}
	if len(rec.Facts) > 0 {
		out["facts"] = rec.Facts
	}
	if len(rec.ToolDigests) > 0 {
		out["tool_data"] = rec.ToolDigests
	}
	if !rec.CreatedAt.IsZero() {
		out["at"] = rec.CreatedAt.UTC().Format(time.RFC3339)
	}
	size := func() int { raw, _ := json.Marshal(out); return len(raw) }
	for _, key := range []string{"answer", "user"} {
		over := size() - m.cfg.RecallMaxBytes
		if over <= 0 {
			break
		}
		s := out[key].(string)
		keep := len(s) - over - 32
		if keep < 0 {
			keep = 0
		}
		out[key] = truncateBytesUTF8(s, keep) + "… [recortado]"
	}
	return out
}

// truncateBytesUTF8 cuts s to at most n bytes without splitting a rune.
func truncateBytesUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && n < len(s) && (s[n]&0xC0) == 0x80 {
		n--
	}
	return s[:n]
}

// memoryTurn is the layered memory of one copilot turn: what is sent
// (preamble, turn-context block, window size) and what is captured.
type memoryTurn struct {
	mem       *copilotMemory
	sessionID uuid.UUID
	orgID     uuid.UUID
	userID    uuid.UUID
	userText  string
	vec       []float32

	// turns are the session's finished turns; next is this turn's number.
	turns []TurnRecord
	next  int
	// window is the planned number of earlier exchanges sent verbatim.
	window int
	// contextBlock goes in the turn context after the date line.
	contextBlock string

	// captured during the turn
	title, summary string
	facts          []string
	digests        []string
}

// beginTurn loads the memory of the session and plans this turn.
func (m *copilotMemory) beginTurn(ctx context.Context, sessionID, orgID, userID uuid.UUID, userText string) *memoryTurn {
	mt := &memoryTurn{mem: m, sessionID: sessionID, orgID: orgID, userID: userID, userText: userText}
	turns, err := m.sessionTurns(ctx, sessionID, orgID, userID)
	if err != nil {
		log.Printf("⚠️ copilot memory: loading turns failed: %v", err)
	}
	mt.turns = turns
	last := 0
	if len(turns) > 0 {
		last = turns[len(turns)-1].Turn
	}
	mt.next = last + 1
	mt.window = windowExchanges(last, m.cfg.RecentExchanges)

	facts, err := m.cfg.Store.ListFacts(ctx, orgID, userID)
	if err != nil {
		log.Printf("⚠️ copilot memory: loading facts failed: %v", err)
	}
	sortFacts(facts)
	if len(facts) > m.cfg.MaxFacts {
		facts = facts[:m.cfg.MaxFacts]
	}
	mt.vec = m.embed(userText)
	mt.contextBlock = mt.renderContext(facts)
	return mt
}

// indexed are the turns older than the window (they go in the index).
func (mt *memoryTurn) indexed(keptExchanges int) []TurnRecord {
	cut := mt.next - 1 - keptExchanges
	var out []TurnRecord
	for _, t := range mt.turns {
		if t.Turn <= cut {
			out = append(out, t)
		}
	}
	return out
}

// preamble renders the index of the turns older than the kept window.
func (mt *memoryTurn) preamble(keptExchanges int) string {
	return renderIndex(mt.indexed(keptExchanges), mt.mem.cfg.IndexTokenBudget)
}

// renderContext is the per-turn memory block: durable facts, the data of the
// previous turn, and an older turn related to the new message (prefetch).
func (mt *memoryTurn) renderContext(facts []MemoryFact) string {
	var parts []string
	if f := renderFacts(facts, mt.mem.cfg.FactsTokenBudget); f != "" {
		parts = append(parts, f)
	}
	if n := len(mt.turns); n > 0 {
		prev := mt.turns[n-1]
		if d := renderTurnData("[Datos del turno #"+strconv.Itoa(prev.Turn)+"]", prev, mt.mem.cfg.TurnDataTokenBudget); d != "" {
			parts = append(parts, d)
		}
	}
	if older := mt.indexed(mt.window); len(older) > 0 {
		if rel, ok := mt.mem.match(mt.userText, mt.vec, older, false); ok {
			label := "[Turno relacionado #" + strconv.Itoa(rel.Turn) + "] " + oneLine(rel.Title, 80) + " → " + oneLine(rel.Summary, 160) +
				" (recall_turn turn=" + strconv.Itoa(rel.Turn) + " para el detalle)"
			if d := renderTurnData(label, rel, mt.mem.cfg.TurnDataTokenBudget); d != "" {
				parts = append(parts, d)
			} else {
				parts = append(parts, label)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// observe captures final_answer's memory fields and the tool digests.
func (mt *memoryTurn) observe(call tools.ToolCall, res tools.ToolResult) {
	if call.ToolName == "final_answer" {
		if !res.Success {
			return
		}
		if s, ok := call.Parameters["title"].(string); ok && strings.TrimSpace(s) != "" {
			mt.title = s
		}
		if s, ok := call.Parameters["summary"].(string); ok && strings.TrimSpace(s) != "" {
			mt.summary = s
		}
		if f := cleanFacts(stringList(call.Parameters["facts"])); len(f) > 0 {
			mt.facts = f
		}
		return
	}
	if memoryInternalTools[call.ToolName] || len(mt.digests) >= memoryMaxDigests {
		return
	}
	mt.digests = append(mt.digests, digestToolCall(call, res))
}

// record is the TurnRecord of the finished turn.
func (mt *memoryTurn) record(answer string, now time.Time) TurnRecord {
	return TurnRecord{
		SessionID:      mt.sessionID,
		OrganizationID: mt.orgID,
		UserID:         mt.userID,
		Turn:           mt.next,
		Title:          turnTitle(mt.title, mt.userText),
		Summary:        turnSummary(mt.summary, answer),
		Facts:          mt.facts,
		ToolDigests:    mt.digests,
		UserMessage:    oneLineKeepBreaks(mt.userText, memoryStoredRunes),
		Answer:         oneLineKeepBreaks(answer, memoryStoredRunes),
		Embedding:      mt.vec,
		CreatedAt:      now.UTC(),
	}
}

// finish stores the turn. Errors are logged: memory never fails a turn.
func (mt *memoryTurn) finish(ctx context.Context, answer string) {
	if err := mt.mem.cfg.Store.SaveTurn(ctx, mt.record(answer, time.Now())); err != nil {
		log.Printf("⚠️ copilot memory: saving turn #%d failed: %v", mt.next, err)
	}
}

// oneLineKeepBreaks trims s and caps it at max runes, keeping line breaks.
func oneLineKeepBreaks(s string, max int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return s
}

// joinTurnContext appends the memory block to the date line.
func joinTurnContext(base, memory string) string {
	switch {
	case memory == "":
		return base
	case base == "":
		return memory
	}
	return base + "\n" + memory
}
