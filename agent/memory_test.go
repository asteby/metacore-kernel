package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/agent/contextmgr"
	"github.com/asteby/metacore-kernel/agent/tools"
)

func rec(n int, title, summary string) TurnRecord {
	return TurnRecord{Turn: n, Title: title, Summary: summary}
}

func TestWindowExchangesAdvancesInSteps(t *testing.T) {
	want := map[int]int{0: 3, 1: 3, 3: 3, 4: 4, 5: 5, 6: 3, 7: 4, 8: 5, 9: 3, 30: 3, 31: 4}
	for n, w := range want {
		if got := windowExchanges(n, 3); got != w {
			t.Errorf("n=%d: window %d, want %d", n, got, w)
		}
	}
	// The window start (n - window) only moves every step turns.
	starts := map[int]bool{}
	for n := 6; n <= 8; n++ {
		starts[n-windowExchanges(n, 3)] = true
	}
	if len(starts) != 1 {
		t.Fatalf("window start moved within a step: %v", starts)
	}
}

func TestRenderIndexDeterministicAndCapped(t *testing.T) {
	var turns []TurnRecord
	for i := 1; i <= 60; i++ {
		turns = append(turns, rec(i, fmt.Sprintf("Ventas de la sucursal %d", i), "Total de 4500 MXN en 23 órdenes, la mayor fue Centro con 1200 MXN"))
	}
	a, b := renderIndex(turns, 800), renderIndex(append([]TurnRecord(nil), turns...), 800)
	if a != b {
		t.Fatal("index must be deterministic")
	}
	if !strings.HasPrefix(a, memoryIndexHeader) {
		t.Fatal("header")
	}
	if EstimateTokens(a) > 800 {
		t.Fatalf("index over budget: %d tokens", EstimateTokens(a))
	}
	if !strings.Contains(a, "\n#60 · Ventas de la sucursal 60 → ") || strings.Contains(a, "\n#1 · ") {
		t.Fatalf("the newest lines are kept, the oldest dropped:\n%s", a)
	}
	if !strings.Contains(a, "(#1–#") || !strings.Contains(a, "omitidos") {
		t.Fatal("omitted turns must be announced")
	}
	for _, line := range strings.Split(a, "\n")[2:] {
		if n := len([]rune(line)); n > memoryIndexLineRunes {
			t.Fatalf("line of %d runes: %q", n, line)
		}
	}
	if small := renderIndex(turns[:2], 800); strings.Contains(small, "omitidos") || strings.Count(small, "\n#") != 2 {
		t.Fatalf("small index:\n%s", small)
	}
	if renderIndex(nil, 800) != "" {
		t.Fatal("no turns, no index")
	}
}

func TestTurnTitleAndSummaryFallbacks(t *testing.T) {
	if got := turnTitle("Ventas de octubre", "lo que sea"); got != "Ventas de octubre" {
		t.Fatalf("model title: %q", got)
	}
	if got := turnTitle("", "  ¿cuántas órdenes  de servicio\nabiertas tiene la sucursal Centro hoy en la mañana?"); got != "¿cuántas órdenes de servicio abiertas tiene la sucursal…" {
		t.Fatalf("fallback title: %q", got)
	}
	if got := turnTitle("uno dos tres cuatro cinco seis siete ocho nueve diez", ""); got != "uno dos tres cuatro cinco seis siete ocho…" {
		t.Fatalf("title cap: %q", got)
	}
	if got := turnSummary("", "Hay *23 órdenes* abiertas. La más antigua es de ayer."); got != "Hay 23 órdenes abiertas." {
		t.Fatalf("fallback summary: %q", got)
	}
	if got := turnSummary("", ""); got != "(sin respuesta)" {
		t.Fatalf("empty summary: %q", got)
	}
	if got := turnTitle("", ""); got != "(sin texto)" {
		t.Fatalf("empty title: %q", got)
	}
}

func TestDigestToolCall(t *testing.T) {
	call := tools.ToolCall{ToolName: "query_data", Parameters: map[string]interface{}{
		"model": "orders", "limit": float64(10), "filters": map[string]interface{}{"status": "open"},
		"zeta": "z", "extra": "e",
	}}
	got := digestToolCall(call, tools.ToolResult{Success: true, Result: map[string]interface{}{
		"data": []interface{}{1, 2, 3}, "total": float64(23), "sum_amount": 4500.5, "status": "ok",
	}})
	want := `query_data(extra=e, filters={"status":"open"}, limit=10, model=orders, …) → total=23; 3 filas; sum_amount=4500.50`
	if got != want {
		t.Fatalf("digest\n got %q\nwant %q", got, want)
	}
	if again := digestToolCall(call, tools.ToolResult{Success: true, Result: map[string]interface{}{
		"data": []interface{}{1, 2, 3}, "total": float64(23), "sum_amount": 4500.5, "status": "ok",
	}}); again != got {
		t.Fatal("digest must be deterministic")
	}
	if e := digestToolCall(tools.ToolCall{ToolName: "x"}, tools.ToolResult{Error: "model not found"}); e != "x() → error: model not found" {
		t.Fatalf("error digest %q", e)
	}
	if l := digestToolCall(tools.ToolCall{ToolName: "list"}, tools.ToolResult{Success: true, Result: []interface{}{1, 2}}); l != "list() → 2 filas" {
		t.Fatalf("list digest %q", l)
	}
	long := digestToolCall(tools.ToolCall{ToolName: "q", Parameters: map[string]interface{}{"q": strings.Repeat("a", 500)}},
		tools.ToolResult{Success: true, Result: strings.Repeat("b", 500)})
	if len([]rune(long)) > memoryDigestRunes {
		t.Fatalf("digest not capped: %d", len([]rune(long)))
	}
}

func TestShapeHistory(t *testing.T) {
	var msgs []map[string]interface{}
	for i := 1; i <= 6; i++ {
		msgs = append(msgs, msg("user", fmt.Sprintf("u%d", i)), msg("assistant", fmt.Sprintf("a%d", i)))
	}
	msgs = append(msgs, msg("user", "now"))
	var gotKept int
	out := shapeHistory(msgs, 3, func(kept int) string { gotKept = kept; return "INDEX" })
	if gotKept != 3 || len(out) != 2+6+1 {
		t.Fatalf("kept %d, len %d", gotKept, len(out))
	}
	if out[0]["content"] != "INDEX" || out[0]["role"] != "user" || out[1]["role"] != "assistant" || out[2]["content"] != "u4" || out[len(out)-1]["content"] != "now" {
		t.Fatalf("shape %v", out)
	}
	if msgs[0]["content"] != "u1" {
		t.Fatal("input mutated")
	}
	// Empty preamble: no synthetic pair. No limit: everything.
	if out := shapeHistory(msgs, 0, func(int) string { return "" }); len(out) != len(msgs) {
		t.Fatalf("no limit: %d", len(out))
	}
	// Leading assistant messages never open the history.
	odd := []map[string]interface{}{msg("assistant", "x"), msg("user", "u1"), msg("assistant", "a1"), msg("user", "now")}
	if out := shapeHistory(odd, 5, nil); out[0]["content"] != "u1" {
		t.Fatalf("leading assistant kept: %v", out)
	}
	if out := shapeHistory([]map[string]interface{}{msg("user", "only")}, 3, func(k int) string {
		if k != 0 {
			t.Fatalf("kept %d", k)
		}
		return "IDX"
	}); len(out) != 3 || out[2]["content"] != "only" {
		t.Fatalf("only current: %v", out)
	}
}

func memCtx(conv, org, user uuid.UUID) *contextmgr.ConversationContext {
	return &contextmgr.ConversationContext{ConversationID: conv, OrganizationID: org, UserID: user}
}

func TestRecallTurnTool(t *testing.T) {
	m := newCopilotMemory(MemoryConfig{})
	ctx := context.Background()
	conv, org, user := uuid.New(), uuid.New(), uuid.New()
	for i := 1; i <= 5; i++ {
		_ = m.cfg.Store.SaveTurn(ctx, TurnRecord{SessionID: conv, OrganizationID: org, UserID: user, Turn: i,
			Title: fmt.Sprintf("Tema %d", i), Summary: "resumen", UserMessage: fmt.Sprintf("pregunta %d", i), Answer: fmt.Sprintf("respuesta %d", i),
			ToolDigests: []string{fmt.Sprintf("query_data(model=m%d) → 3 filas", i)}})
	}
	_ = m.cfg.Store.SaveTurn(ctx, TurnRecord{SessionID: conv, OrganizationID: org, UserID: user, Turn: 6,
		Title: "Inventario de llantas Michelin", Summary: "Quedan 14 llantas", UserMessage: "¿cuántas llantas michelin quedan en bodega?", Answer: "Quedan 14."})
	// A row of another user in the same session id must never be recalled.
	_ = m.cfg.Store.SaveTurn(ctx, TurnRecord{SessionID: conv, OrganizationID: org, UserID: uuid.New(), Turn: 7, Title: "ajeno"})

	tool := &recallTurnTool{mem: m}
	out, err := tool.Execute(ctx, map[string]interface{}{"turn": float64(3)}, memCtx(conv, org, user))
	if err != nil {
		t.Fatal(err)
	}
	p := out.(map[string]interface{})
	if p["turn"] != 3 || p["user"] != "pregunta 3" || p["answer"] != "respuesta 3" || p["tool_data"].([]string)[0] != "query_data(model=m3) → 3 filas" {
		t.Fatalf("recall #3: %v", p)
	}
	out, err = tool.Execute(ctx, map[string]interface{}{"query": "llantas Michelin en bodega"}, memCtx(conv, org, user))
	if err != nil || out.(map[string]interface{})["turn"] != 6 {
		t.Fatalf("recall by query: %v %v", out, err)
	}
	if _, err := tool.Execute(ctx, map[string]interface{}{"turn": float64(7)}, memCtx(conv, org, user)); err == nil {
		t.Fatal("another user's turn must not be recalled")
	}
	if _, err := tool.Execute(ctx, map[string]interface{}{}, memCtx(conv, org, user)); err == nil {
		t.Fatal("turn or query required")
	}

	// The payload is capped.
	small := newCopilotMemory(MemoryConfig{RecallMaxBytes: 500})
	p = small.recallPayload(TurnRecord{Turn: 1, UserMessage: strings.Repeat("ñ", 2000), Answer: strings.Repeat("x", 4000)})
	raw, _ := json.Marshal(p)
	if len(raw) > 560 || !strings.Contains(p["answer"].(string), "[recortado]") {
		t.Fatalf("recall payload %d bytes", len(raw))
	}
}

func TestLongTermFactsOrderCapAndForget(t *testing.T) {
	m := newCopilotMemory(MemoryConfig{MaxFacts: 3})
	ctx := context.Background()
	conv, org, user := uuid.New(), uuid.New(), uuid.New()
	remember, forget := &rememberFactTool{mem: m}, &forgetFactTool{mem: m}
	for _, f := range []string{"reporta en MXN", "su sucursal es Centro", "prefiere listas cortas"} {
		if _, err := remember.Execute(ctx, map[string]interface{}{"fact": f}, memCtx(conv, org, user)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond) // distinct created_at
	}
	if out, _ := remember.Execute(ctx, map[string]interface{}{"fact": "Reporta en MXN"}, memCtx(conv, org, user)); out.(map[string]interface{})["status"] != "already_known" {
		t.Fatal("duplicates are not stored twice")
	}
	if _, err := remember.Execute(ctx, map[string]interface{}{"fact": "otro"}, memCtx(conv, org, user)); err == nil {
		t.Fatal("cap must be enforced")
	}
	facts, _ := m.cfg.Store.ListFacts(ctx, org, user)
	got := renderFacts(facts, 300)
	want := "[Memoria del usuario] Datos y preferencias durables (respétalos; forget_fact si dejan de valer):\n1. reporta en MXN\n2. su sucursal es Centro\n3. prefiere listas cortas"
	if got != want {
		t.Fatalf("facts\n got %q\nwant %q", got, want)
	}
	if capped := renderFacts(facts, EstimateTokens(want)-7); strings.Contains(capped, "3. ") || !strings.Contains(capped, "1. ") {
		t.Fatalf("token cap: %q", capped)
	}
	// Other users never see these facts.
	if other, _ := m.cfg.Store.ListFacts(ctx, org, uuid.New()); len(other) != 0 {
		t.Fatal("facts leaked across users")
	}
	if out, _ := forget.Execute(ctx, map[string]interface{}{"fact": "2"}, memCtx(conv, org, user)); out.(map[string]interface{})["fact"] != "su sucursal es Centro" {
		t.Fatalf("forget by number: %v", out)
	}
	if out, _ := forget.Execute(ctx, map[string]interface{}{"fact": "listas"}, memCtx(conv, org, user)); out.(map[string]interface{})["status"] != "forgotten" {
		t.Fatalf("forget by text: %v", out)
	}
	facts, _ = m.cfg.Store.ListFacts(ctx, org, user)
	if len(facts) != 1 || facts[0].Text != "reporta en MXN" {
		t.Fatalf("left %v", facts)
	}
}

// fakeEmbedder maps texts to fixed vectors by keyword.
type fakeEmbedder struct{ calls int }

func (f *fakeEmbedder) GenerateEmbedding(text string) ([]float32, error) {
	f.calls++
	l := strings.ToLower(text)
	switch {
	case strings.Contains(l, "llanta") || strings.Contains(l, "neumático"):
		return []float32{1, 0.1, 0}, nil
	case strings.Contains(l, "factura") || strings.Contains(l, "cobro"):
		return []float32{0, 1, 0.1}, nil
	}
	return []float32{0.1, 0.1, 1}, nil
}

func (f *fakeEmbedder) GenerateBatchEmbeddings(texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i], _ = f.GenerateEmbedding(t)
	}
	return out, nil
}

func TestPrefetchAttachesRelatedOlderTurn(t *testing.T) {
	ctx := context.Background()
	conv, org, user := uuid.New(), uuid.New(), uuid.New()
	seed := func(m *copilotMemory) {
		texts := []string{"¿Cuántas llantas quedan en bodega?", "Muéstrame las facturas pendientes", "hola", "otra cosa", "algo más", "y otra"}
		for i, txt := range texts {
			vec, _ := (&fakeEmbedder{}).GenerateEmbedding(txt)
			r := TurnRecord{SessionID: conv, OrganizationID: org, UserID: user, Turn: i + 1, Title: txt, Summary: "resumen " + txt, UserMessage: txt,
				Facts: []string{fmt.Sprintf("dato del turno %d", i+1)}}
			if m.cfg.Embedder != nil {
				r.Embedding = vec
			}
			_ = m.cfg.Store.SaveTurn(ctx, r)
		}
	}

	emb := &fakeEmbedder{}
	sem := newCopilotMemory(MemoryConfig{Embedder: emb})
	seed(sem)
	mt := sem.beginTurn(ctx, conv, org, user, "y los neumáticos de la otra marca?")
	if !strings.Contains(mt.contextBlock, "[Turno relacionado #1]") || !strings.Contains(mt.contextBlock, "dato del turno 1") {
		t.Fatalf("semantic prefetch missing:\n%s", mt.contextBlock)
	}
	if emb.calls != 1 {
		t.Fatalf("one embedding per turn, got %d", emb.calls)
	}
	// Turns inside the recent window are not prefetched (they are verbatim).
	mt = sem.beginTurn(ctx, conv, org, user, "otra cosa")
	if strings.Contains(mt.contextBlock, "Turno relacionado #4]") {
		t.Fatalf("window turn prefetched:\n%s", mt.contextBlock)
	}

	lex := newCopilotMemory(MemoryConfig{})
	seed(lex)
	mt = lex.beginTurn(ctx, conv, org, user, "vuelve a revisar las facturas pendientes por favor")
	if !strings.Contains(mt.contextBlock, "[Turno relacionado #2]") {
		t.Fatalf("lexical prefetch missing:\n%s", mt.contextBlock)
	}
	mt = lex.beginTurn(ctx, conv, org, user, "gracias")
	if strings.Contains(mt.contextBlock, "Turno relacionado") {
		t.Fatalf("unrelated message prefetched:\n%s", mt.contextBlock)
	}
	// The previous turn's data always rides along.
	if !strings.Contains(mt.contextBlock, "[Datos del turno #6]\n- dato del turno 6") {
		t.Fatalf("previous turn data missing:\n%s", mt.contextBlock)
	}
}

func TestMemoryFinalAnswerSchemaAndCapture(t *testing.T) {
	reg := tools.NewToolRegistry()
	reg.Register(NewMemoryFinalAnswerTool())
	defs := reg.ToOpenAIFormat()
	params := defs[0]["parameters"].(map[string]interface{})
	props := params["properties"].(map[string]interface{})
	if defs[0]["name"] != "final_answer" || props["facts"] == nil || props["title"] == nil || props["summary"] == nil {
		t.Fatalf("schema %v", defs[0])
	}
	if req := params["required"].([]string); len(req) != 1 || req[0] != "message" {
		t.Fatalf("only message is required: %v", req)
	}
	if props["facts"].(map[string]interface{})["items"] == nil {
		t.Fatal("array needs items")
	}

	mt := &memoryTurn{mem: newCopilotMemory(MemoryConfig{}), next: 4, userText: "¿cuántas órdenes hay?"}
	mt.observe(tools.ToolCall{ToolName: "query_data", Parameters: map[string]interface{}{"model": "orders"}}, tools.ToolResult{Success: true, Result: map[string]interface{}{"total": float64(23)}})
	mt.observe(tools.ToolCall{ToolName: "recall_turn"}, tools.ToolResult{Success: true})
	mt.observe(tools.ToolCall{ToolName: "final_answer", Parameters: map[string]interface{}{
		"message": "Hay 23.", "title": "Órdenes abiertas", "summary": "Hay 23 órdenes abiertas",
		"facts": []interface{}{"23 órdenes abiertas", "23 órdenes abiertas", " "},
	}}, tools.ToolResult{Success: true})
	r := mt.record("Hay 23.", time.Now())
	if r.Turn != 4 || r.Title != "Órdenes abiertas" || r.Summary != "Hay 23 órdenes abiertas" || len(r.Facts) != 1 ||
		len(r.ToolDigests) != 1 || r.ToolDigests[0] != "query_data(model=orders) → total=23" {
		t.Fatalf("record %+v", r)
	}
}

func TestGormTurnStoreRoundTrip(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := MigrateTurnMemory(db); err != nil {
		t.Fatal(err)
	}
	if err := MigrateTurnMemory(db); err != nil {
		t.Fatal("migration must be idempotent:", err)
	}
	s := NewGormTurnStore(db)
	ctx := context.Background()
	conv, org, user := uuid.New(), uuid.New(), uuid.New()
	for _, n := range []int{2, 1} {
		if err := s.SaveTurn(ctx, TurnRecord{SessionID: conv, OrganizationID: org, UserID: user, Turn: n, Title: fmt.Sprint("t", n),
			Facts: []string{"f"}, ToolDigests: []string{"d"}, Embedding: []float32{0.5, 1}, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	// Upsert of the same turn.
	if err := s.SaveTurn(ctx, TurnRecord{SessionID: conv, OrganizationID: org, UserID: user, Turn: 2, Title: "t2b"}); err != nil {
		t.Fatal(err)
	}
	turns, err := s.ListTurns(ctx, conv)
	if err != nil || len(turns) != 2 || turns[0].Turn != 1 || turns[1].Title != "t2b" || turns[0].Facts[0] != "f" || turns[0].Embedding[1] != 1 {
		t.Fatalf("turns %+v err %v", turns, err)
	}
	base := time.Now().UTC().Truncate(time.Second)
	f2 := MemoryFact{ID: uuid.New(), OrganizationID: org, UserID: user, Text: "segundo", CreatedAt: base.Add(time.Second)}
	f1 := MemoryFact{ID: uuid.New(), OrganizationID: org, UserID: user, Text: "primero", CreatedAt: base}
	_ = s.SaveFact(ctx, f2)
	_ = s.SaveFact(ctx, f1)
	facts, err := s.ListFacts(ctx, org, user)
	if err != nil || len(facts) != 2 || facts[0].Text != "primero" {
		t.Fatalf("facts %+v err %v", facts, err)
	}
	_ = s.DeleteFact(ctx, org, uuid.New(), f1.ID) // other user: no-op
	_ = s.DeleteFact(ctx, org, user, f2.ID)
	if facts, _ = s.ListFacts(ctx, org, user); len(facts) != 1 || facts[0].ID != f1.ID {
		t.Fatalf("after delete %+v", facts)
	}
}
