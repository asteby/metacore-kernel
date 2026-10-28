package automation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/genui"
)

// View is the API/agent representation of an automation.
type View struct {
	Automation
	Spec             json.RawMessage `json:"spec"`
	TriggerLabel     string          `json:"trigger_label"`
	Authorship       string          `json:"authorship"` // "Ana Pérez con Asistente" | "Ana Pérez"
	CreatorName      string          `json:"creator_name,omitempty"`
	AuthorizedByName string          `json:"authorized_by_name,omitempty"`
	UsesLLM          bool            `json:"uses_llm"`
	Channels         []string        `json:"channels,omitempty"`
	Tools            []string        `json:"tools,omitempty"`
}

// RunView is the API representation of a run.
type RunView struct {
	Run
	Steps  json.RawMessage `json:"steps,omitempty"`
	Output json.RawMessage `json:"output,omitempty"`
}

// Views renders automations with names resolved in one query.
func (s *Service) Views(ctx context.Context, list []Automation) []View {
	ids := map[uuid.UUID]bool{}
	for _, a := range list {
		ids[a.CreatedBy] = true
		if a.AuthorizedBy != nil {
			ids[*a.AuthorizedBy] = true
		}
	}
	names := map[uuid.UUID]string{}
	if s.names != nil && len(list) > 0 {
		keys := make([]uuid.UUID, 0, len(ids))
		for id := range ids {
			keys = append(keys, id)
		}
		names = s.names.Names(ctx, list[0].OrganizationID, keys)
	}
	out := make([]View, 0, len(list))
	for _, a := range list {
		out = append(out, s.view(a, names))
	}
	return out
}

// ViewOf renders one automation.
func (s *Service) ViewOf(ctx context.Context, a *Automation) View {
	return s.Views(ctx, []Automation{*a})[0]
}

func (s *Service) view(a Automation, names map[uuid.UUID]string) View {
	v := View{Automation: a, Spec: json.RawMessage(a.Spec)}
	spec, err := a.ParsedSpec()
	if err == nil {
		v.TriggerLabel = describeTrigger(spec.Trigger, s.policy.DefaultTimezone, s.policy.EventLabels)
		v.UsesLLM = spec.UsesLLM()
		seen := map[string]bool{}
		for _, st := range spec.Steps {
			if st.Type == StepTool && !seen[st.Tool] {
				v.Tools = append(v.Tools, st.Tool)
				seen[st.Tool] = true
			}
			for _, ch := range st.Channels {
				if !seen["ch:"+ch] {
					v.Channels = append(v.Channels, ch)
					seen["ch:"+ch] = true
				}
			}
		}
	}
	v.CreatorName = names[a.CreatedBy]
	if a.AuthorizedBy != nil {
		v.AuthorizedByName = names[*a.AuthorizedBy]
	}
	person := v.CreatorName
	if person == "" {
		person = "Persona"
	}
	v.Authorship = person
	if a.AssistedBy != "" {
		v.Authorship = person + " con " + a.AssistedBy
	}
	return v
}

// RunViews renders runs.
func RunViews(runs []Run) []RunView {
	out := make([]RunView, 0, len(runs))
	for _, r := range runs {
		rv := RunView{Run: r}
		if r.Steps != "" {
			rv.Steps = json.RawMessage(r.Steps)
		}
		if r.Output != "" {
			rv.Output = json.RawMessage(r.Output)
		}
		out = append(out, rv)
	}
	return out
}

var statusLabels = map[string]string{
	StatusProposed: "Propuesta", StatusActive: "Activa", StatusPaused: "Pausada",
	StatusError: "Detenida por error", StatusCompleted: "Completada", StatusArchived: "Eliminada",
}

var statusTones = map[string]string{
	StatusProposed: "warning", StatusActive: "success", StatusPaused: "muted",
	StatusError: "danger", StatusCompleted: "info", StatusArchived: "muted",
}

var channelLabels = map[string]string{
	ChannelInApp: "Campana", ChannelPush: "Push", ChannelEmail: "Correo", ChannelWhatsApp: "WhatsApp",
}

// StatusLabel is the Spanish label of a status.
func StatusLabel(st string) string {
	if l, ok := statusLabels[st]; ok {
		return l
	}
	return st
}

// CardUI renders an automation as a generative-UI card; proposals carry the
// confirmation form (actions automations.confirm / automations.discard that
// the host maps to its API).
func (s *Service) CardUI(v View, preview *RunOutput) *genui.Spec {
	fields := []genui.Field{
		{Label: "Cuándo", Value: v.TriggerLabel},
		{Label: "Autoría", Value: v.Authorship},
	}
	if len(v.Tools) > 0 {
		fields = append(fields, genui.Field{Label: "Consulta", Value: strings.Join(v.Tools, ", ")})
	}
	if len(v.Channels) > 0 {
		chs := make([]string, 0, len(v.Channels))
		for _, c := range v.Channels {
			if l, ok := channelLabels[c]; ok {
				chs = append(chs, l)
			} else {
				chs = append(chs, c)
			}
		}
		fields = append(fields, genui.Field{Label: "Canales", Value: strings.Join(chs, ", ")})
	}
	ia := "No (sin costo de IA)"
	if v.UsesLLM {
		ia = "Sí, resumen con modelo económico y tope por corrida"
	}
	fields = append(fields, genui.Field{Label: "Usa IA", Value: ia})
	if v.NextRunAt != nil {
		fields = append(fields, genui.Field{Label: "Próxima corrida", Value: v.NextRunAt.In(s.loc()).Format("02/01/2006 15:04")})
	}
	if v.StatusReason != "" {
		fields = append(fields, genui.Field{Label: "Motivo", Value: v.StatusReason})
	}
	title := v.Name
	if v.Kind == KindTool && v.ToolName != nil {
		title += " (" + *v.ToolName + ")"
	}
	blocks := []genui.Block{}
	if v.Status == StatusProposed {
		blocks = append(blocks, genui.Block{
			Type:        genui.BlockConfirm,
			Title:       title,
			Description: firstNonEmpty(v.Description, "Propuesta de automatización. Revísala antes de activarla: nada se envía hasta que la confirmes."),
			Badges:      []genui.Badge{{Label: StatusLabel(v.Status), Tone: statusTones[v.Status]}},
			Fields:      fields,
			Actions: []genui.Action{
				{ID: "automations.confirm", Label: "Confirmar y activar", Variant: "primary", Params: map[string]any{"id": v.ID.String()}},
				{ID: "automations.discard", Label: "Descartar", Variant: "ghost", Params: map[string]any{"id": v.ID.String()}},
			},
		})
	} else {
		blocks = append(blocks, genui.Block{
			Type: genui.BlockCard, Title: title,
			Body:   v.Description,
			Badges: []genui.Badge{{Label: StatusLabel(v.Status), Tone: statusTones[v.Status]}},
			Fields: fields, Link: s.managePath + "?id=" + v.ID.String(),
		})
	}
	if preview != nil {
		blocks = append(blocks, PreviewBlocks(preview)...)
	}
	return &genui.Spec{Version: genui.Version, Blocks: blocks}
}

// previewItems caps the items shown in a chat card.
const previewItems = 5

// PreviewBlocks renders a (dry-run) output: the message and its items.
func PreviewBlocks(out *RunOutput) []genui.Block {
	var blocks []genui.Block
	if out.Text != "" || out.Title != "" {
		text := out.Text
		if len(out.Items) > 0 {
			// The items go in the links block below: keep only the lead line.
			text, _, _ = strings.Cut(text, "\n")
		}
		blocks = append(blocks, genui.Block{Type: genui.BlockText, Tone: "muted",
			Text: truncate(strings.TrimSpace("Vista previa (no se envió nada) — "+out.Title+"\n"+text), genui.MaxText)})
	}
	if len(out.Items) > 0 {
		items := make([]genui.Item, 0, len(out.Items))
		for i, it := range out.Items {
			if i == previewItems {
				break
			}
			gi := genui.Item{Title: truncate(it.Title, genui.MaxLabel), Date: it.Date, Description: truncate(it.Text, 160)}
			if genui.SafeLink(it.URL) {
				gi.URL = it.URL
			}
			items = append(items, gi)
		}
		title := fmt.Sprintf("%d elementos", len(out.Items))
		if len(out.Items) > len(items) {
			title = fmt.Sprintf("%d elementos (se muestran %d)", len(out.Items), len(items))
		}
		blocks = append(blocks, genui.Block{Type: genui.BlockLinks, Title: title, Items: items})
	}
	for _, n := range out.Notes {
		blocks = append(blocks, genui.Block{Type: genui.BlockText, Tone: "info", Text: n})
	}
	return blocks
}

// ListUI renders a list of automations as a table.
func (s *Service) ListUI(views []View) *genui.Spec {
	if len(views) == 0 {
		return &genui.Spec{Version: genui.Version, Blocks: []genui.Block{{Type: genui.BlockText, Tone: "muted", Text: "No tienes automatizaciones."}}}
	}
	rows := make([]map[string]any, 0, len(views))
	for _, v := range views {
		next := ""
		if v.NextRunAt != nil {
			next = v.NextRunAt.In(s.loc()).Format("02/01 15:04")
		}
		rows = append(rows, map[string]any{"nombre": truncate(v.Name, 120), "cuando": truncate(v.TriggerLabel, 160), "estado": StatusLabel(v.Status), "proxima": next})
	}
	if len(rows) > genui.MaxItems {
		rows = rows[:genui.MaxItems]
	}
	return &genui.Spec{Version: genui.Version, Blocks: []genui.Block{{
		Type: genui.BlockTable, Title: "Tus automatizaciones",
		Columns: []genui.Column{{Key: "nombre", Label: "Nombre"}, {Key: "cuando", Label: "Cuándo"}, {Key: "estado", Label: "Estado"}, {Key: "proxima", Label: "Próxima"}},
		Rows:    rows,
	}}}
}

func (s *Service) loc() *time.Location {
	loc, err := time.LoadLocation(s.policy.DefaultTimezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
