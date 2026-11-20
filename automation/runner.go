package automation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/asteby/metacore-kernel/provenance"
	"hash/fnv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/genui"
	"github.com/asteby/metacore-kernel/outbox"
	"github.com/asteby/metacore-kernel/runtime/flow"
)

// Node types registered in the restricted engine.
const (
	nodeTool      flow.NodeType = "automation.tool"
	nodeFilter    flow.NodeType = "automation.filter"
	nodeCondition flow.NodeType = "automation.condition"
	nodeTemplate  flow.NodeType = "automation.template"
	nodeSummarize flow.NodeType = "automation.summarize"
	nodeDeliver   flow.NodeType = "automation.deliver"
)

// runEngine wraps a runtime/flow Engine whose registry holds ONLY the trigger
// node and the automation nodes: http_request, webhook, delay, wait, loop and
// every other built-in are removed, so a spec can never reach them.
type runEngine struct {
	engine *flow.Engine
}

func newRunEngine(s *Service) *runEngine {
	e := flow.NewEngine(flow.Config{MaxNodeExecs: 20, TestTimeout: s.policy.RunTimeout})
	reg := e.Registry()
	for _, t := range reg.Types() {
		if t != flow.NodeTypeTrigger {
			reg.Unregister(t)
		}
	}
	reg.Register(nodeTool, toolNode{})
	reg.Register(nodeFilter, filterNode{})
	reg.Register(nodeCondition, conditionNode{})
	reg.Register(nodeTemplate, templateNode{})
	reg.Register(nodeSummarize, summarizeNode{})
	reg.Register(nodeDeliver, deliverNode{})
	return &runEngine{engine: e}
}

// RunOutput is what a run produced (stored as JSON on the run row).
type RunOutput struct {
	Title          string         `json:"title,omitempty"`
	Text           string         `json:"text,omitempty"`
	Items          []DeliveryItem `json:"items,omitempty"`
	Channels       []string       `json:"channels,omitempty"`
	DeliveredSteps []string       `json:"delivered_steps,omitempty"`
	Notes          []string       `json:"notes,omitempty"`
	Result         any            `json:"result,omitempty"` // dynamic tools: final step output
}

// stepTrace is the compact per-node trace stored on the run.
type stepTrace struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Status     string `json:"status"`
	DurationMS int64  `json:"duration_ms"`
	Count      *int   `json:"count,omitempty"`
	Error      string `json:"error,omitempty"`
}

// runEnv is the per-run state nodes reach through the context.
type runEnv struct {
	svc       *Service
	auto      *Automation
	spec      *Spec
	id        Identity
	runID     uuid.UUID
	slot      string
	dryRun    bool
	tools     map[string]CatalogTool
	delivered map[string]bool // steps already delivered (retry of the same slot)

	mu        sync.Mutex
	out       RunOutput
	skipped   bool
	llmUsed   bool
	cacheHit  bool
	inputHash string
	inTok     int
	outTok    int
	cost      float64
	sent      int
	permanent error
	budgetHit bool
	maxTokens int
	maxCost   float64
	steps     []stepTrace
}

type envKey struct{}

func envFrom(ctx context.Context) *runEnv {
	e, _ := ctx.Value(envKey{}).(*runEnv)
	return e
}

func (e *runEnv) note(n string) {
	e.mu.Lock()
	e.out.Notes = append(e.out.Notes, n)
	e.mu.Unlock()
}

// compile turns the linear spec into a runtime/flow DAG.
func compile(a *Automation, spec *Spec) *flow.Flow {
	f := &flow.Flow{ID: a.ID, OrganizationID: a.OrganizationID, Name: a.Name, Status: flow.FlowStatusActive, TriggerType: flow.TriggerManual}
	f.Nodes = append(f.Nodes, flow.FlowNode{ID: "$start", Type: flow.NodeTypeTrigger})
	prev := "$start"
	for _, st := range spec.Steps {
		st := st
		f.Nodes = append(f.Nodes, flow.FlowNode{
			ID:   st.ID,
			Type: flow.NodeType("automation." + st.Type),
			Data: flow.FlowNodeData{Label: st.ID, Config: map[string]any{"step": &st}},
		})
		f.Edges = append(f.Edges, flow.FlowEdge{ID: prev + "->" + st.ID, Source: prev, Target: st.ID})
		prev = st.ID
	}
	return f
}

func stepOf(node *flow.FlowNode) *Step {
	st, _ := node.Data.Config["step"].(*Step)
	return st
}

// ---- outbox entry point ------------------------------------------------------

func (s *Service) handleRunJob(ctx context.Context, job outbox.Job) error {
	var rj runJob
	if err := job.Decode(&rj); err != nil {
		return outbox.Permanent(err)
	}
	final := job.Attempts >= job.MaxAttempts
	_, err := s.execute(ctx, rj, job.Attempts, final)
	if errors.Is(err, errPermanentRun) {
		return outbox.Permanent(err)
	}
	return err
}

// execute runs one slot of an automation (idempotent per slot).
func (s *Service) execute(ctx context.Context, rj runJob, attempt int, final bool) (*Run, error) {
	var a Automation
	if err := s.db.WithContext(ctx).Where("id = ?", rj.AutomationID).First(&a).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil // deleted: nothing to do
		}
		return nil, err
	}
	if a.Status != StatusActive {
		return nil, nil // paused / archived after the slot was enqueued
	}
	now := s.now().UTC()
	run := &Run{ID: uuid.New(), AutomationID: a.ID, OrganizationID: a.OrganizationID, SlotKey: rj.Slot,
		Trigger: rj.Trigger, Status: RunRunning, Steps: "[]", Output: "{}", Attempt: attempt, StartedAt: now}
	res := s.db.WithContext(ctx).Clauses(onConflictNothing).Create(run)
	if res.Error != nil {
		return nil, res.Error
	}
	delivered := map[string]bool{}
	if res.RowsAffected == 0 {
		var existing Run
		if err := s.db.WithContext(ctx).Where("automation_id = ? AND slot_key = ?", a.ID, rj.Slot).First(&existing).Error; err != nil {
			return nil, err
		}
		if existing.Status == RunCompleted || existing.Status == RunSkipped {
			return &existing, nil // the slot already ran: never twice
		}
		var prev RunOutput
		_ = json.Unmarshal([]byte(existing.Output), &prev)
		for _, st := range prev.DeliveredSteps {
			delivered[st] = true
		}
		existing.Status, existing.Attempt, existing.Error = RunRunning, attempt, ""
		s.db.WithContext(ctx).Model(&Run{}).Where("id = ?", existing.ID).Updates(map[string]any{"status": RunRunning, "attempt": attempt, "error": ""})
		run = &existing
	}
	// Daily cap per automation.
	if s.runsToday(ctx, a.ID, now) > s.limits(&a).MaxRunsPerDay {
		s.finishRun(ctx, &a, run, nil, fmt.Errorf("%w: maximum runs per day reached", ErrBudget), true)
		return run, nil
	}
	spec, err := a.ParsedSpec()
	if err != nil {
		s.finishRun(ctx, &a, run, nil, err, true)
		return run, errPermanentRun
	}
	trigger := map[string]any{"type": rj.Trigger, "slot": rj.Slot}
	if rj.Event != nil {
		trigger["event"] = rj.Event
	}
	env, err := s.run(ctx, &a, spec, Identity{OrganizationID: a.OrganizationID, UserID: a.CreatedBy}, run, false, trigger, nil, delivered)
	if err != nil && !final && env != nil && env.permanent == nil && !errors.Is(err, ErrBudget) {
		// Transient: record the attempt and let the outbox retry with backoff.
		s.saveProgress(ctx, run, env, err)
		return run, err
	}
	s.finishRun(ctx, &a, run, env, err, final || (env != nil && env.permanent != nil))
	if env != nil && env.permanent != nil {
		return run, errPermanentRun
	}
	return run, nil
}

// limits merges spec limits with the policy (the smaller wins).
func (s *Service) limits(a *Automation) Limits {
	l := Limits{MaxTokensPerRun: s.policy.MaxTokensPerRun, MaxCostUSDPerRun: s.policy.MaxCostUSDPerRun, MaxRunsPerDay: s.policy.MaxRunsPerDay}
	if spec, err := a.ParsedSpec(); err == nil && spec.Limits != nil {
		if v := spec.Limits.MaxTokensPerRun; v > 0 && v < l.MaxTokensPerRun {
			l.MaxTokensPerRun = v
		}
		if v := spec.Limits.MaxCostUSDPerRun; v > 0 && v < l.MaxCostUSDPerRun {
			l.MaxCostUSDPerRun = v
		}
		if v := spec.Limits.MaxRunsPerDay; v > 0 && v < l.MaxRunsPerDay {
			l.MaxRunsPerDay = v
		}
	}
	return l
}

func (s *Service) runsToday(ctx context.Context, autoID uuid.UUID, now time.Time) int {
	var n int64
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	s.db.WithContext(ctx).Model(&Run{}).Where("automation_id = ? AND started_at >= ? AND dry_run = ?", autoID, day, false).Count(&n)
	return int(n)
}

// orgSpendToday sums model spend of the org since UTC midnight.
func (s *Service) orgSpendToday(ctx context.Context, orgID uuid.UUID) float64 {
	now := s.now().UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	var total float64
	s.db.WithContext(ctx).Model(&Run{}).Select("COALESCE(SUM(cost_usd), 0)").
		Where("organization_id = ? AND started_at >= ?", orgID, day).Scan(&total)
	return total
}

// run executes the compiled flow. inputs is set for dynamic tools.
func (s *Service) run(ctx context.Context, a *Automation, spec *Spec, id Identity, run *Run, dryRun bool, trigger map[string]any, inputs map[string]any, delivered map[string]bool) (*runEnv, error) {
	lim := s.limits(a)
	env := &runEnv{svc: s, auto: a, spec: spec, id: id, runID: run.ID, slot: run.SlotKey, dryRun: dryRun,
		tools: map[string]CatalogTool{}, delivered: delivered, maxTokens: lim.MaxTokensPerRun, maxCost: lim.MaxCostUSDPerRun}
	if env.delivered == nil {
		env.delivered = map[string]bool{}
	}
	if s.catalog != nil {
		for _, t := range s.catalog.List(ctx, id) {
			env.tools[t.Name] = t
		}
	}
	vars := map[string]any{"$trigger": trigger}
	if ev, ok := trigger["event"].(map[string]any); ok {
		vars["event"] = ev["payload"]
	}
	if inputs != nil {
		vars["inputs"] = inputs
	}
	f := compile(a, spec)
	for k, v := range vars {
		f.Variables = append(f.Variables, flow.FlowVariable{Name: k, Value: v})
	}
	rctx := context.WithValue(runProvenance(ctx, s.policy.AssistantName, a, id, run, inputs != nil), envKey{}, env)
	res, err := s.engine.engine.ExecuteSync(rctx, f, trigger, nil)
	if err != nil {
		return env, err
	}
	env.trace(res)
	if env.permanent != nil {
		return env, env.permanent
	}
	if env.budgetHit {
		return env, ErrBudget
	}
	switch res.Status {
	case "completed":
		if inputs != nil && len(spec.Steps) > 0 {
			last := spec.Steps[len(spec.Steps)-1].ID
			out := map[string]any{}
			for k, v := range res.Variables {
				if strings.HasPrefix(k, last+".") {
					out[strings.TrimPrefix(k, last+".")] = v
				}
			}
			if l, ok := out["items"].([]any); ok {
				env.out.Items = toDeliveryItems(l, 10)
				delete(out, "items")
				out["items"] = len(l)
			}
			if t, ok := out["text"].(string); ok {
				env.out.Text = t
			}
			env.out.Result = out
		}
		return env, nil
	case "timeout":
		if ctx.Err() != nil {
			return env, ctx.Err()
		}
		return env, errors.New("the run exceeded the maximum time")
	default:
		msg := res.Error
		if res.ErrorNode != "" {
			msg = res.ErrorNode + ": " + msg
		}
		return env, errors.New(msg)
	}
}

func (e *runEnv) trace(res *flow.TestResult) {
	if res == nil {
		return
	}
	tr := make([]stepTrace, 0, len(res.NodeResults))
	for _, n := range res.NodeResults {
		if n.NodeType == flow.NodeTypeTrigger {
			continue
		}
		t := stepTrace{ID: n.NodeID, Type: strings.TrimPrefix(string(n.NodeType), "automation."), Status: string(n.Status), DurationMS: n.Duration, Error: n.Error}
		if c, ok := n.Output["count"].(int); ok {
			t.Count = &c
		}
		tr = append(tr, t)
	}
	e.mu.Lock()
	e.steps = tr
	e.mu.Unlock()
}

func (e *runEnv) takeTrace() []stepTrace {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.steps
}

// saveProgress persists a failed attempt that will be retried.
func (s *Service) saveProgress(ctx context.Context, run *Run, env *runEnv, err error) {
	s.db.WithContext(ctx).Model(&Run{}).Where("id = ?", run.ID).Updates(map[string]any{
		"status": RunFailed, "error": truncate(err.Error(), 1000), "output": jsonOf(env.out), "steps": jsonOf(env.takeTrace()),
	})
}

// finishRun stores the final state of a run and updates the automation
// counters, failure streak and lifecycle (one-shots complete, repeated
// failures or budget overruns auto-pause).
func (s *Service) finishRun(ctx context.Context, a *Automation, run *Run, env *runEnv, runErr error, final bool) {
	now := s.now().UTC()
	status := RunCompleted
	if runErr != nil {
		status = RunFailed
	} else if env != nil && env.skipped {
		status = RunSkipped
	}
	upd := map[string]any{"status": status, "finished_at": now, "duration_ms": now.Sub(run.StartedAt).Milliseconds(), "error": ""}
	if runErr != nil {
		upd["error"] = truncate(runErr.Error(), 1000)
	}
	if env != nil {
		upd["output"] = jsonOf(env.out)
		upd["steps"] = jsonOf(env.takeTrace())
		upd["llm_used"], upd["cache_hit"], upd["input_hash"] = env.llmUsed, env.cacheHit, env.inputHash
		upd["input_tokens"], upd["output_tokens"], upd["cost_usd"], upd["deliveries"] = env.inTok, env.outTok, env.cost, env.sent
	}
	s.db.WithContext(ctx).Model(&Run{}).Where("id = ?", run.ID).Updates(upd)
	if run.DryRun {
		return
	}
	au := map[string]any{"last_run_at": now, "last_status": status, "runs_count": gorm.Expr("runs_count + 1"), "updated_at": now}
	if env != nil {
		au["tokens_total"] = gorm.Expr("tokens_total + ?", env.inTok+env.outTok)
		au["cost_usd_total"] = gorm.Expr("cost_usd_total + ?", env.cost)
	}
	reason := ""
	switch {
	case runErr == nil:
		au["consecutive_failures"] = 0
		if a.TriggerType == TriggerOnce {
			au["status"] = StatusCompleted
			au["next_run_at"] = nil
		}
	case errors.Is(runErr, ErrBudget) || (env != nil && env.permanent != nil):
		reason = runErr.Error()
	case final:
		if a.ConsecutiveFailures+1 >= s.policy.MaxConsecutiveFailures {
			reason = fmt.Sprintf("%d fallos seguidos: %s", a.ConsecutiveFailures+1, runErr.Error())
		}
		au["consecutive_failures"] = gorm.Expr("consecutive_failures + 1")
	}
	if reason != "" {
		au["status"] = StatusError
		au["status_reason"] = truncate(reason, 300)
		au["next_run_at"] = nil
	}
	s.db.WithContext(ctx).Model(&Automation{}).Where("id = ?", a.ID).Updates(au)
	if reason != "" {
		s.audit(ctx, a, uuid.Nil, "error", reason)
	}
}

// ---- run now / dry run / dynamic tools ---------------------------------------

// RunNow executes an automation immediately. dryRun runs every step but
// delivers nothing and marks nothing as delivered: it returns the preview of
// what would be sent (the agent only ever uses dry runs).
func (s *Service) RunNow(ctx context.Context, id Identity, autoID uuid.UUID, dryRun bool) (*Run, *RunOutput, error) {
	a, err := s.load(ctx, id, autoID)
	if err != nil {
		return nil, nil, err
	}
	if a.Kind != KindAutomation {
		return nil, nil, ErrBadState
	}
	if !dryRun && a.Status != StatusActive && a.Status != StatusPaused {
		return nil, nil, ErrBadState
	}
	if a.Status == StatusArchived {
		return nil, nil, ErrBadState
	}
	spec, err := a.ParsedSpec()
	if err != nil {
		return nil, nil, err
	}
	now := s.now().UTC()
	actor := id.UserID
	run := &Run{ID: uuid.New(), AutomationID: a.ID, OrganizationID: a.OrganizationID, SlotKey: "manual:" + uuid.NewString(),
		Trigger: TriggerManual, Status: RunRunning, Steps: "[]", Output: "{}", DryRun: dryRun, Attempt: 1, StartedAt: now, ActorID: &actor}
	if err := s.db.WithContext(ctx).Create(run).Error; err != nil {
		return nil, nil, err
	}
	trigger := map[string]any{"type": TriggerManual, "slot": run.SlotKey}
	if spec.Trigger != nil && spec.Trigger.Type == TriggerEvent {
		trigger["event"] = map[string]any{"id": "prueba", "name": spec.Trigger.Event, "payload": map[string]any{}}
	}
	// Runs always act as the creator, whoever presses the button.
	env, runErr := s.run(ctx, a, spec, Identity{OrganizationID: a.OrganizationID, UserID: a.CreatedBy}, run, dryRun, trigger, nil, nil)
	s.finishRun(ctx, a, run, env, runErr, true)
	var fresh Run
	s.db.WithContext(ctx).Where("id = ?", run.ID).First(&fresh)
	var out *RunOutput
	if env != nil {
		out = &env.out
	}
	if runErr != nil {
		return &fresh, out, runErr
	}
	return &fresh, out, nil
}

// InvokeTool runs an active dynamic tool for the person talking to the agent.
func (s *Service) InvokeTool(ctx context.Context, id Identity, toolName string, args map[string]any) (*RunOutput, error) {
	var a Automation
	err := s.db.WithContext(ctx).Where("organization_id = ? AND kind = ? AND tool_name = ? AND status = ?",
		id.OrganizationID, KindTool, toolName, StatusActive).First(&a).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	spec, err := a.ParsedSpec()
	if err != nil {
		return nil, err
	}
	inputs, err := bindInputs(spec.Inputs, args)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	actor := id.UserID
	run := &Run{ID: uuid.New(), AutomationID: a.ID, OrganizationID: a.OrganizationID, SlotKey: "tool:" + uuid.NewString(),
		Trigger: TriggerManual, Status: RunRunning, Steps: "[]", Output: "{}", Attempt: 1, StartedAt: now, ActorID: &actor}
	if err := s.db.WithContext(ctx).Create(run).Error; err != nil {
		return nil, err
	}
	// A dynamic tool runs as the caller: it can only read what they can read.
	env, runErr := s.run(ctx, &a, spec, id, run, true, map[string]any{"type": "tool"}, inputs, nil)
	s.finishRun(ctx, &a, run, env, runErr, true)
	if runErr != nil {
		return nil, runErr
	}
	return &env.out, nil
}

func bindInputs(decl []InputParam, args map[string]any) (map[string]any, error) {
	out := map[string]any{}
	for _, in := range decl {
		v, ok := args[in.Name]
		if !ok || v == nil || v == "" {
			if in.Default != nil {
				out[in.Name] = in.Default
				continue
			}
			if in.Required {
				return nil, invalid("falta %q", in.Name)
			}
			continue
		}
		if err := checkType(in.Name, map[string]any{"type": in.Type, "enum": toAny(in.Enum)}, v); err != nil {
			return nil, invalid("%v", err)
		}
		out[in.Name] = v
	}
	return out, nil
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// ---- nodes -----------------------------------------------------------------

type toolNode struct{}

func (toolNode) Name() string { return "automation.tool" }
func (toolNode) Execute(ctx context.Context, node *flow.FlowNode, ec *flow.ExecutionContext) (*flow.NodeResult, error) {
	env, st := envFrom(ctx), stepOf(node)
	if env == nil || st == nil {
		return nil, errors.New("contexto de corrida ausente")
	}
	tool, ok := env.tools[st.Tool]
	if !ok {
		env.permanent = fmt.Errorf("%w: tool %q is no longer allowed", errPermanentRun, st.Tool)
		return nil, env.permanent
	}
	args := make(map[string]any, len(st.Args))
	for k, v := range st.Args {
		args[k] = resolveValue(ec, v)
	}
	if env.svc.catalog == nil {
		return nil, errors.New("no tool catalog")
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	raw, err := env.svc.catalog.Call(cctx, env.id, st.Tool, args)
	if err != nil {
		if errors.Is(err, ErrForbidden) || strings.Contains(strings.ToLower(err.Error()), "permiso") {
			env.permanent = fmt.Errorf("%w: %v", errPermanentRun, err)
			return nil, env.permanent
		}
		return nil, err
	}
	result := normalize(raw)
	items := []any{}
	if tool.ItemsPath != "" {
		if v, ok := lookup(result, tool.ItemsPath); ok {
			if list, ok := v.([]any); ok {
				items = list
			}
		}
	} else if m, ok := result.(map[string]any); ok {
		items = []any{m}
	}
	for _, it := range items {
		annotate(it, tool)
	}
	return &flow.NodeResult{Output: map[string]any{"result": result, "items": items, "count": len(items)}}, nil
}

// annotate adds _key/_title/_url/_date/_text to an item using the catalog
// mapping, so later steps (dedupe, rendering) work for any tool.
func annotate(it any, tool CatalogTool) {
	m, ok := it.(map[string]any)
	if !ok {
		return
	}
	get := func(path string) string {
		if path == "" {
			return ""
		}
		v, ok := lookup(m, path)
		if !ok || v == nil {
			return ""
		}
		return fmt.Sprint(v)
	}
	m["_key"] = get(tool.ItemKey)
	if m["_key"] == "" {
		m["_key"] = hashOf(jsonOf(m))
	}
	m["_title"] = get(tool.ItemTitle)
	m["_url"] = get(tool.ItemURL)
	m["_date"] = get(tool.ItemDate)
	m["_text"] = get(tool.ItemText)
}

type filterNode struct{}

func (filterNode) Name() string { return "automation.filter" }
func (filterNode) Execute(ctx context.Context, node *flow.FlowNode, ec *flow.ExecutionContext) (*flow.NodeResult, error) {
	env, st := envFrom(ctx), stepOf(node)
	if env == nil || st == nil {
		return nil, errors.New("contexto de corrida ausente")
	}
	items := listOf(ec, st.From)
	kept := make([]any, 0, len(items))
	for _, it := range items {
		m, _ := it.(map[string]any)
		if conditionsMatch(st.Where, m) {
			kept = append(kept, it)
		}
	}
	if st.Unseen && len(kept) > 0 {
		keys := make([]string, 0, len(kept))
		for _, it := range kept {
			if m, ok := it.(map[string]any); ok {
				keys = append(keys, fmt.Sprint(m["_key"]))
			}
		}
		var seen []string
		if err := env.svc.db.WithContext(ctx).Model(&DeliveryRecord{}).Where("automation_id = ? AND item_key IN ?", env.auto.ID, keys).
			Pluck("item_key", &seen).Error; err != nil {
			return nil, err
		}
		done := map[string]bool{}
		for _, k := range seen {
			done[k] = true
		}
		fresh := kept[:0]
		for _, it := range kept {
			if m, ok := it.(map[string]any); ok && done[fmt.Sprint(m["_key"])] {
				continue
			}
			fresh = append(fresh, it)
		}
		kept = fresh
	}
	if st.Limit > 0 && len(kept) > st.Limit {
		kept = kept[:st.Limit]
	}
	return &flow.NodeResult{Output: map[string]any{"items": kept, "count": len(kept)}}, nil
}

type conditionNode struct{}

func (conditionNode) Name() string { return "automation.condition" }
func (conditionNode) Execute(ctx context.Context, node *flow.FlowNode, ec *flow.ExecutionContext) (*flow.NodeResult, error) {
	env, st := envFrom(ctx), stepOf(node)
	if env == nil || st == nil || st.If == nil {
		return nil, errors.New("contexto de corrida ausente")
	}
	v, _ := ec.Get(st.If.Field)
	ok := compare(v, st.If.Op, st.If.Value)
	if !ok {
		env.mu.Lock()
		env.skipped = true
		env.mu.Unlock()
		return &flow.NodeResult{Output: map[string]any{"passed": false}, Stop: true}, nil
	}
	return &flow.NodeResult{Output: map[string]any{"passed": true}}, nil
}

type templateNode struct{}

func (templateNode) Name() string { return "automation.template" }
func (templateNode) Execute(ctx context.Context, node *flow.FlowNode, ec *flow.ExecutionContext) (*flow.NodeResult, error) {
	st := stepOf(node)
	if st == nil {
		return nil, errors.New("paso ausente")
	}
	return &flow.NodeResult{Output: map[string]any{"text": render(ec, st.Template, nil)}}, nil
}

type summarizeNode struct{}

func (summarizeNode) Name() string { return "automation.summarize" }
func (summarizeNode) Execute(ctx context.Context, node *flow.FlowNode, ec *flow.ExecutionContext) (*flow.NodeResult, error) {
	env, st := envFrom(ctx), stepOf(node)
	if env == nil || st == nil {
		return nil, errors.New("contexto de corrida ausente")
	}
	items := toDeliveryItems(listOf(ec, st.From), 25)
	extra := map[string]any{"count": len(items), "lista": bulletList(items)}
	tpl := st.Template
	if tpl == "" {
		tpl = "{{count}} elementos:\n{{lista}}"
	}
	deterministic := render(ec, tpl, extra)
	if st.Mode != "llm" {
		return &flow.NodeResult{Output: map[string]any{"text": deterministic, "count": len(items), "llm": false}}, nil
	}
	if len(items) == 0 {
		return &flow.NodeResult{Output: map[string]any{"text": deterministic, "count": 0, "llm": false}}, nil
	}
	if env.dryRun {
		// Previews never spend tokens.
		env.note("Template preview: in the real run this step is summarized with AI (low-cost model, capped).")
		return &flow.NodeResult{Output: map[string]any{"text": deterministic, "count": len(items), "llm": false}}, nil
	}
	hash := hashOf(env.auto.ID.String() + "|" + st.Instructions + "|" + jsonOf(items))
	env.inputHash = hash
	// Cache: the same input already summarised by a previous run is reused.
	var cached []Run
	env.svc.db.WithContext(ctx).Where("automation_id = ? AND input_hash = ? AND llm_used = ? AND status IN ?",
		env.auto.ID, hash, true, []string{RunCompleted, RunSkipped}).Order("started_at DESC").Limit(1).Find(&cached)
	if len(cached) == 1 {
		var prev RunOutput
		if json.Unmarshal([]byte(cached[0].Output), &prev) == nil && prev.Text != "" {
			env.cacheHit, env.llmUsed = true, true
			return &flow.NodeResult{Output: map[string]any{"text": prev.Text, "count": len(items), "llm": true, "cache": true}}, nil
		}
	}
	p := env.svc.policy
	if env.svc.summarizer == nil {
		env.note("Summary without a language model (no provider connected): the template was used.")
		return &flow.NodeResult{Output: map[string]any{"text": deterministic, "count": len(items), "llm": false}}, nil
	}
	if p.DailyBudgetUSD <= 0 || env.svc.orgSpendToday(ctx, env.auto.OrganizationID)+env.maxCost > p.DailyBudgetUSD {
		env.note("Daily AI budget exhausted or not configured: the template was used.")
		return &flow.NodeResult{Output: map[string]any{"text": deterministic, "count": len(items), "llm": false}}, nil
	}
	maxTok := st.MaxTokens
	if maxTok <= 0 || maxTok > env.maxTokens {
		maxTok = env.maxTokens
	}
	sctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	res, err := env.svc.summarizer.Summarize(sctx, SummarizeRequest{
		OrganizationID: env.auto.OrganizationID, UserID: env.id.UserID, Model: p.DefaultModel,
		Instructions: st.Instructions, Items: items, MaxTokens: maxTok,
	})
	if err != nil || strings.TrimSpace(res.Text) == "" {
		env.note("The model did not respond: the template was used.")
		return &flow.NodeResult{Output: map[string]any{"text": deterministic, "count": len(items), "llm": false}}, nil
	}
	env.mu.Lock()
	env.llmUsed = true
	env.inTok += res.InputTokens
	env.outTok += res.OutputTokens
	env.cost += res.CostUSD
	over := env.cost > env.maxCost || env.inTok+env.outTok > env.maxTokens*4
	env.mu.Unlock()
	if over {
		env.budgetHit = true
		return nil, ErrBudget
	}
	return &flow.NodeResult{Output: map[string]any{"text": strings.TrimSpace(res.Text), "count": len(items), "llm": true}}, nil
}

type deliverNode struct{}

func (deliverNode) Name() string { return "automation.deliver" }
func (deliverNode) Execute(ctx context.Context, node *flow.FlowNode, ec *flow.ExecutionContext) (*flow.NodeResult, error) {
	env, st := envFrom(ctx), stepOf(node)
	if env == nil || st == nil {
		return nil, errors.New("contexto de corrida ausente")
	}
	var items []DeliveryItem
	var raw []any
	if st.ItemsFrom != "" {
		raw = listOf(ec, st.ItemsFrom)
		items = toDeliveryItems(raw, 10)
	}
	extra := map[string]any{"count": len(items), "lista": bulletList(items)}
	title := render(ec, st.Title, extra)
	body := render(ec, st.Body, extra)
	if strings.TrimSpace(body) == "" {
		body = bulletList(items)
	}
	link := render(ec, st.Link, extra)
	if link != "" && !genui.SafeLink(link) {
		link = ""
	}
	env.mu.Lock()
	env.out.Title, env.out.Text, env.out.Items, env.out.Channels = title, body, items, st.Channels
	env.mu.Unlock()
	if env.dryRun {
		return &flow.NodeResult{Output: map[string]any{"sent": 0, "count": len(items), "dry_run": true}}, nil
	}
	if env.delivered[st.ID] {
		return &flow.NodeResult{Output: map[string]any{"sent": 0, "count": len(items), "already": true}}, nil
	}
	if env.svc.deliverer == nil {
		return nil, errors.New("sin canal de entrega configurado")
	}
	db := env.svc.db.WithContext(ctx)
	// Claim the item keys first (dedupe across runs); release them if the
	// host could not queue anything, so the next run retries them.
	var claimed []string
	now := env.svc.now().UTC()
	for _, it := range items {
		res := db.Clauses(onConflictNothing).Create(&DeliveryRecord{AutomationID: env.auto.ID, ItemKey: it.Key, OrganizationID: env.auto.OrganizationID, RunID: env.runID, CreatedAt: now})
		if res.Error != nil {
			return nil, res.Error
		}
		if res.RowsAffected == 1 {
			claimed = append(claimed, it.Key)
		}
	}
	sent, err := env.svc.deliverer.Deliver(ctx, Delivery{
		OrganizationID: env.auto.OrganizationID, UserID: env.auto.CreatedBy, AutomationID: env.auto.ID, RunID: env.runID,
		Channels: st.Channels, Title: title, Body: body, Link: link, Items: items,
		Tag: "automation-" + env.auto.ID.String() + "-" + hashOf(env.slot),
	})
	if err != nil {
		if len(claimed) > 0 {
			db.Where("automation_id = ? AND item_key IN ?", env.auto.ID, claimed).Delete(&DeliveryRecord{})
		}
		return nil, fmt.Errorf("entrega: %w", err)
	}
	env.mu.Lock()
	env.sent += sent
	env.out.DeliveredSteps = append(env.out.DeliveredSteps, st.ID)
	out := jsonOf(env.out)
	env.mu.Unlock()
	// Persist right away: a retry of this slot must not deliver twice.
	db.Model(&Run{}).Where("id = ?", env.runID).Updates(map[string]any{"output": out, "deliveries": env.sent})
	return &flow.NodeResult{Output: map[string]any{"sent": sent, "count": len(items)}}, nil
}

// ---- value helpers -------------------------------------------------------------

// resolveValue keeps the type of a single-variable template ("{{inputs.n}}")
// and renders mixed templates as text.
func resolveValue(ec *flow.ExecutionContext, v any) any {
	s, ok := v.(string)
	if !ok {
		return v
	}
	if m := templateVarRe.FindStringSubmatch(s); m != nil && strings.TrimSpace(s) == m[0] {
		if val, ok := ec.Get(m[1]); ok {
			return val
		}
		return ""
	}
	return render(ec, s, nil)
}

// render interpolates {{path}} with context values (scalars only; lists render
// as their size). Unknown variables render empty — never the raw template.
func render(ec *flow.ExecutionContext, tpl string, extra map[string]any) string {
	if tpl == "" {
		return ""
	}
	return templateVarRe.ReplaceAllStringFunc(tpl, func(m string) string {
		key := templateVarRe.FindStringSubmatch(m)[1]
		var v any
		var ok bool
		if extra != nil {
			v, ok = extra[key]
		}
		if !ok {
			v, ok = ec.Get(key)
		}
		if !ok || v == nil {
			return ""
		}
		switch x := v.(type) {
		case []any:
			return fmt.Sprint(len(x))
		case map[string]any:
			return ""
		case float64:
			if x == float64(int64(x)) {
				return fmt.Sprint(int64(x))
			}
		}
		return fmt.Sprint(v)
	})
}

func listOf(ec *flow.ExecutionContext, path string) []any {
	v, ok := ec.Get(path)
	if !ok {
		return nil
	}
	if l, ok := v.([]any); ok {
		return l
	}
	return nil
}

func toDeliveryItems(list []any, max int) []DeliveryItem {
	out := make([]DeliveryItem, 0, len(list))
	for _, it := range list {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		d := DeliveryItem{Key: str(m["_key"]), Title: str(m["_title"]), URL: str(m["_url"]), Date: str(m["_date"]), Text: str(m["_text"])}
		if d.Title == "" {
			d.Title = truncate(jsonOf(m), 120)
		}
		if d.URL != "" && !genui.SafeLink(d.URL) {
			d.URL = ""
		}
		out = append(out, d)
		if len(out) >= max {
			break
		}
	}
	return out
}

func bulletList(items []DeliveryItem) string {
	var b strings.Builder
	for _, it := range items {
		b.WriteString("• ")
		b.WriteString(it.Title)
		if it.Date != "" {
			b.WriteString(" (" + it.Date + ")")
		}
		if it.URL != "" {
			b.WriteString(" " + it.URL)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func str(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

// normalize converts any tool result into plain JSON values (map[string]any,
// []any, float64, string, bool, nil).
func normalize(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}

func lookup(root any, path string) (any, bool) {
	cur := root
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[part]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// compare extends flow.CompareValues with "in" (value is a list).
func compare(actual any, op string, expected any) bool {
	if op == "in" {
		list, ok := expected.([]any)
		if !ok {
			return false
		}
		for _, e := range list {
			if flow.CompareValues(actual, "eq", e) {
				return true
			}
		}
		return false
	}
	if op == "matches" {
		return false // not allowed in automations (schema rejects it too)
	}
	if l, ok := actual.([]any); ok && (op == "gt" || op == "gte" || op == "lt" || op == "lte" || op == "eq" || op == "ne") {
		actual = len(l)
	}
	return flow.CompareValues(actual, op, expected)
}

func hashOf(s string) string {
	h := fnv.New64a()
	h.Write([]byte(s))
	return fmt.Sprintf("%016x", h.Sum64())
}

// runProvenance stamps what a run writes (through the host catalog, the
// deliverer, notifications) as the agent acting on behalf of the automation
// owner — or of the person calling a dynamic tool from the chat. Nobody
// approved each run, so the mode is automated; a caller that already put a
// provenance in ctx (e.g. an approved proposal) keeps it and only gains the
// run reference.
func runProvenance(ctx context.Context, agent string, a *Automation, id Identity, run *Run, tool bool) context.Context {
	ref := func(p provenance.Provenance) provenance.Provenance {
		return p.WithRef(provenance.RefAutomationRun, run.ID.String())
	}
	if _, ok := provenance.FromContext(ctx); ok {
		return provenance.Update(ctx, ref)
	}
	via := provenance.ViaAutomation
	if tool {
		via = provenance.ViaChat
	}
	p := provenance.Agent(agent, via).OnBehalf(id.UserID).WithSession(id.SessionID).WithOrigin(a.Name)
	return provenance.WithContext(ctx, ref(p))
}
