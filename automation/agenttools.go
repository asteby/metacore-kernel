package automation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/agent/contextmgr"
	"github.com/asteby/metacore-kernel/agent/tools"
	"github.com/asteby/metacore-kernel/genui"
)

// AgentToolsConfig customises the agent-facing tools.
type AgentToolsConfig struct {
	// Examples are host-specific spec examples appended to the description of
	// crear_automatizacion (e.g. a daily news digest with the host's tools).
	Examples []string
	// ToolsHint lists, in one line, the catalog tools the model may compose.
	ToolsHint string
}

// AgentTools returns the tools the agent uses to manage automations. Every
// tool acts only for the person in the conversation (agentCtx.UserID) and
// never activates anything on its own: creations and agent edits are
// proposals the person confirms (except trivial personal reminders).
func (s *Service) AgentTools(cfg AgentToolsConfig) []tools.BaseTool {
	return []tools.BaseTool{
		&createTool{s: s, cfg: cfg},
		&listTool{s: s},
		&editTool{s: s},
		&simpleTool{s: s, name: "pausar_automatizacion", desc: "Pausa una automatización activa del usuario (deja de correr hasta reanudarla).", op: s.Pause},
		&simpleTool{s: s, name: "reanudar_automatizacion", desc: "Reanuda una automatización pausada o detenida por error.", op: s.Resume},
		&simpleTool{s: s, name: "eliminar_automatizacion", desc: "Elimina (archiva) una automatización o herramienta dinámica del usuario. Úsala solo si la persona lo pidió explícitamente.", op: s.Archive},
		&dryRunTool{s: s},
	}
}

func identityOf(agentCtx *contextmgr.ConversationContext) (Identity, error) {
	if agentCtx == nil || agentCtx.OrganizationID == uuid.Nil || agentCtx.UserID == uuid.Nil {
		return Identity{}, ErrForbidden
	}
	return Identity{OrganizationID: agentCtx.OrganizationID, UserID: agentCtx.UserID, SessionID: agentCtx.ConversationID}, nil
}

func idParam(params map[string]any) (uuid.UUID, error) {
	raw := strings.TrimSpace(fmt.Sprint(params["id"]))
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, invalid("indica el id de la automatización (usa listar_automatizaciones)")
	}
	return id, nil
}

// specParam accepts the spec as an object or as a JSON string.
func specParam(params map[string]any) (json.RawMessage, error) {
	v, ok := params["spec"]
	if !ok || v == nil {
		return nil, invalid("spec is missing (automation/v1 object)")
	}
	if s, ok := v.(string); ok {
		return json.RawMessage(s), nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, invalid("spec ilegible")
	}
	return b, nil
}

// agentError converts domain errors into a result the model can act on (it
// sees what was wrong and can fix the spec) instead of a generic failure.
func agentError(err error) (any, error) {
	switch {
	case errors.Is(err, ErrInvalid), errors.Is(err, ErrLimit), errors.Is(err, ErrBadState), errors.Is(err, ErrConflict):
		return map[string]any{"ok": false, "error": err.Error()}, nil
	}
	return nil, err
}

const specGuide = `La spec es JSON automation/v1:
{"schema_version":"automation/v1","name":"…","description":"…",
 "trigger":{"type":"cron","cron":"0 7 * * *","timezone":"America/Mexico_City"} | {"type":"once","at":"2026-10-14T09:00","timezone":"…"} | {"type":"interval","every":"6h"} | {"type":"event","event":"nombre.evento","where":[{"field":"campo","op":"eq","value":"…"}]},
 "steps":[ … máximo 8 … ]}
Pasos permitidos (no hay HTTP, SQL ni código):
- {"id":"buscar","type":"tool","tool":"<herramienta del catálogo>","args":{…}}  → produce buscar.items y buscar.count
- {"id":"nuevos","type":"filter","from":"buscar.items","unseen":true,"where":[…],"limit":10}  → unseen descarta lo ya enviado
- {"id":"hay","type":"condition","if":{"field":"nuevos.count","op":"gt","value":0}}  → si es falso termina sin enviar ni gastar
- {"id":"texto","type":"summarize","from":"nuevos.items","mode":"template","template":"{{count}} nuevos:\n{{lista}}"}  (mode "llm" solo si la persona pidió un resumen redactado)
- {"id":"enviar","type":"deliver","channels":["in_app","push"],"title":"…","body":"{{texto.text}}","items_from":"nuevos.items","link":"/ruta"}
Operadores: eq ne contains not_contains gt gte lt lte in is_empty is_not_empty. Frecuencia mínima 15 min. Entrega siempre a la propia persona.`

// ---- crear_automatizacion ---------------------------------------------------

type createTool struct {
	s   *Service
	cfg AgentToolsConfig
}

func (t *createTool) Name() string     { return "crear_automatizacion" }
func (t *createTool) Category() string { return "data" }
func (t *createTool) Description() string {
	d := "Crea una automatización («recuérdame…», «cada mañana dame…», «avísame cuando…») o una herramienta dinámica (tipo=herramienta: composición reutilizable de consultas con inputs tipados, sin trigger ni deliver). " +
		"Queda como PROPUESTA que la persona confirma en la tarjeta (los recordatorios personales simples quedan activos). Devuelve la tarjeta y una vista previa. " +
		"Usa la zona horaria de la persona y fechas absolutas. " + specGuide
	if t.cfg.ToolsHint != "" {
		d += "\nHerramientas que puedes componer: " + t.cfg.ToolsHint
	}
	if len(t.cfg.Examples) > 0 {
		d += "\nEjemplos:\n" + strings.Join(t.cfg.Examples, "\n")
	}
	return d
}
func (t *createTool) Parameters() map[string]any {
	return map[string]any{
		"spec": map[string]any{"type": "object", "description": "Especificación automation/v1 completa", "required": true},
		"tipo": map[string]any{"type": "string", "description": "automatizacion (por defecto) o herramienta", "enum": []string{"automatizacion", "herramienta"}},
	}
}
func (t *createTool) Execute(ctx context.Context, params map[string]any, agentCtx *contextmgr.ConversationContext) (any, error) {
	id, err := identityOf(agentCtx)
	if err != nil {
		return nil, err
	}
	raw, err := specParam(params)
	if err != nil {
		return agentError(err)
	}
	kind := KindAutomation
	if fmt.Sprint(params["tipo"]) == "herramienta" {
		kind = KindTool
	}
	var session *uuid.UUID
	if id.SessionID != uuid.Nil {
		sid := id.SessionID
		session = &sid
	}
	a, err := t.s.Create(ctx, id, CreateInput{Kind: kind, Spec: raw, Origin: OriginChat, Assisted: true, SessionID: session})
	if err != nil {
		return agentError(err)
	}
	var preview *RunOutput
	if kind == KindAutomation {
		if _, out, err := t.s.RunNow(ctx, id, a.ID, true); err == nil {
			preview = out
		}
	}
	v := t.s.ViewOf(ctx, a)
	msg := "Propuesta creada: la persona debe confirmarla en la tarjeta para activarla."
	if a.Status == StatusActive {
		msg = "Recordatorio activo: " + v.TriggerLabel + "."
	}
	return map[string]any{
		"ok": true, "mensaje": msg,
		"automatizacion": summary(v),
		"ui":             t.s.CardUI(v, preview),
	}, nil
}

func summary(v View) map[string]any {
	m := map[string]any{"id": v.ID.String(), "nombre": v.Name, "estado": v.Status, "estado_texto": StatusLabel(v.Status), "cuando": v.TriggerLabel, "autoria": v.Authorship}
	if v.ToolName != nil {
		m["herramienta"] = *v.ToolName
	}
	if v.NextRunAt != nil {
		m["proxima_corrida"] = v.NextRunAt.Format("2006-01-02T15:04:05Z07:00")
	}
	if v.StatusReason != "" {
		m["motivo"] = v.StatusReason
	}
	return m
}

// ---- listar -----------------------------------------------------------------

type listTool struct{ s *Service }

func (t *listTool) Name() string     { return "listar_automatizaciones" }
func (t *listTool) Category() string { return "data" }
func (t *listTool) Description() string {
	return "Lista las automatizaciones y herramientas dinámicas de la persona con su estado, cuándo corren y la próxima corrida."
}
func (t *listTool) Parameters() map[string]any {
	return map[string]any{"estado": map[string]any{"type": "string", "description": "proposed, active, paused, error o completed (opcional)"}}
}
func (t *listTool) Execute(ctx context.Context, params map[string]any, agentCtx *contextmgr.ConversationContext) (any, error) {
	id, err := identityOf(agentCtx)
	if err != nil {
		return nil, err
	}
	st, _ := params["estado"].(string)
	list, err := t.s.List(ctx, id, ListFilter{Status: st})
	if err != nil {
		return nil, err
	}
	views := t.s.Views(ctx, list)
	out := make([]map[string]any, 0, len(views))
	for _, v := range views {
		out = append(out, summary(v))
	}
	return map[string]any{"total": len(out), "automatizaciones": out, "ui": t.s.ListUI(views)}, nil
}

// ---- editar -----------------------------------------------------------------

type editTool struct{ s *Service }

func (t *editTool) Name() string     { return "editar_automatizacion" }
func (t *editTool) Category() string { return "data" }
func (t *editTool) Description() string {
	return "Cambia la spec de una automatización de la persona (envía la spec completa nueva). Si estaba activa vuelve a PROPUESTA y la persona debe confirmar el cambio."
}
func (t *editTool) Parameters() map[string]any {
	return map[string]any{
		"id":   map[string]any{"type": "string", "description": "id de la automatización", "required": true},
		"spec": map[string]any{"type": "object", "description": "Spec automation/v1 completa", "required": true},
	}
}
func (t *editTool) Execute(ctx context.Context, params map[string]any, agentCtx *contextmgr.ConversationContext) (any, error) {
	id, err := identityOf(agentCtx)
	if err != nil {
		return nil, err
	}
	autoID, err := idParam(params)
	if err != nil {
		return agentError(err)
	}
	raw, err := specParam(params)
	if err != nil {
		return agentError(err)
	}
	a, err := t.s.Update(ctx, id, autoID, UpdateInput{Spec: raw, Assisted: true})
	if err != nil {
		return agentError(err)
	}
	v := t.s.ViewOf(ctx, a)
	return map[string]any{"ok": true, "mensaje": "Cambio propuesto: requiere confirmación.", "automatizacion": summary(v), "ui": t.s.CardUI(v, nil)}, nil
}

// ---- pausar / reanudar / eliminar ---------------------------------------------

type simpleTool struct {
	s          *Service
	name, desc string
	op         func(ctx context.Context, id Identity, autoID uuid.UUID) (*Automation, error)
}

func (t *simpleTool) Name() string        { return t.name }
func (t *simpleTool) Category() string    { return "data" }
func (t *simpleTool) Description() string { return t.desc }
func (t *simpleTool) Parameters() map[string]any {
	return map[string]any{"id": map[string]any{"type": "string", "description": "id de la automatización", "required": true}}
}
func (t *simpleTool) Execute(ctx context.Context, params map[string]any, agentCtx *contextmgr.ConversationContext) (any, error) {
	id, err := identityOf(agentCtx)
	if err != nil {
		return nil, err
	}
	autoID, err := idParam(params)
	if err != nil {
		return agentError(err)
	}
	a, err := t.op(ctx, id, autoID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return map[string]any{"ok": false, "error": err.Error()}, nil
		}
		return agentError(err)
	}
	v := t.s.ViewOf(ctx, a)
	return map[string]any{"ok": true, "automatizacion": summary(v), "ui": t.s.CardUI(v, nil)}, nil
}

// ---- ejecutar ahora (dry-run) -------------------------------------------------

type dryRunTool struct{ s *Service }

func (t *dryRunTool) Name() string     { return "ejecutar_automatizacion_ahora" }
func (t *dryRunTool) Category() string { return "data" }
func (t *dryRunTool) Description() string {
	return "Prueba una automatización AHORA en modo simulación: corre las consultas y muestra lo que enviaría, sin enviar nada ni marcar nada como entregado."
}
func (t *dryRunTool) Parameters() map[string]any {
	return map[string]any{"id": map[string]any{"type": "string", "description": "id de la automatización", "required": true}}
}
func (t *dryRunTool) Execute(ctx context.Context, params map[string]any, agentCtx *contextmgr.ConversationContext) (any, error) {
	id, err := identityOf(agentCtx)
	if err != nil {
		return nil, err
	}
	autoID, err := idParam(params)
	if err != nil {
		return agentError(err)
	}
	run, out, err := t.s.RunNow(ctx, id, autoID, true)
	if run == nil && err != nil {
		return agentError(err)
	}
	res := map[string]any{"ok": err == nil, "estado_corrida": run.Status, "simulacion": true}
	if err != nil {
		res["error"] = err.Error()
	}
	if out != nil {
		res["titulo"], res["texto"], res["elementos"] = out.Title, out.Text, len(out.Items)
		if blocks := PreviewBlocks(out); len(blocks) > 0 {
			res["ui"] = &genui.Spec{Version: genui.Version, Blocks: blocks}
		}
	}
	return res, nil
}

// ---- dynamic tools ------------------------------------------------------------

// DynamicTools returns the active dynamic tools of the organisation as agent
// tools for this turn (hosts pass them as per-turn extra tools). They run with
// the identity of the person in the conversation.
func (s *Service) DynamicTools(ctx context.Context, orgID uuid.UUID) []tools.BaseTool {
	if !s.policy.Enabled {
		return nil
	}
	var rows []Automation
	if err := s.db.WithContext(ctx).Where("organization_id = ? AND kind = ? AND status = ?", orgID, KindTool, StatusActive).
		Order("name").Limit(30).Find(&rows).Error; err != nil {
		s.log.Warn("automation: dynamic tools", "err", err)
		return nil
	}
	out := make([]tools.BaseTool, 0, len(rows))
	for _, a := range rows {
		spec, err := a.ParsedSpec()
		if err != nil || a.ToolName == nil {
			continue
		}
		out = append(out, &dynTool{s: s, name: *a.ToolName, spec: spec})
	}
	return out
}

type dynTool struct {
	s    *Service
	name string
	spec *Spec
}

func (t *dynTool) Name() string     { return t.name }
func (t *dynTool) Category() string { return "data" }
func (t *dynTool) Description() string {
	d := t.spec.Description
	if d == "" {
		d = t.spec.Name
	}
	return d + " (herramienta dinámica de la organización; solo lectura)"
}
func (t *dynTool) Parameters() map[string]any {
	p := map[string]any{}
	in := append([]InputParam(nil), t.spec.Inputs...)
	sort.Slice(in, func(i, j int) bool { return in[i].Name < in[j].Name })
	for _, i := range in {
		d := map[string]any{"type": i.Type, "description": i.Description}
		if i.Required {
			d["required"] = true
		}
		if len(i.Enum) > 0 {
			d["enum"] = i.Enum
		}
		p[i.Name] = d
	}
	return p
}
func (t *dynTool) Execute(ctx context.Context, params map[string]any, agentCtx *contextmgr.ConversationContext) (any, error) {
	id, err := identityOf(agentCtx)
	if err != nil {
		return nil, err
	}
	out, err := t.s.InvokeTool(ctx, id, t.name, params)
	if err != nil {
		return agentError(err)
	}
	res := map[string]any{"resultado": out.Result}
	if out.Text != "" {
		res["texto"] = out.Text
	}
	if blocks := PreviewBlocks(&RunOutput{Items: out.Items}); len(blocks) > 0 {
		res["ui"] = &genui.Spec{Version: genui.Version, Blocks: blocks}
	}
	return res, nil
}
