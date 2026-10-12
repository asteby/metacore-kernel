package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// CapabilityDescriptor is one thing the copilot may do for a user, with its
// risk and whether it needs approval first.
type CapabilityDescriptor struct {
	ID                   string         `json:"id"`
	Owner                string         `json:"owner"`
	Title                string         `json:"title"`
	Description          string         `json:"description"`
	Kind                 string         `json:"kind"` // answer | guide | act | transact
	Risk                 string         `json:"risk"` // none | low | medium | high | critical
	Permission           string         `json:"permission,omitempty"`
	Model                string         `json:"model,omitempty"`
	Action               string         `json:"action,omitempty"`
	RequiresConfirmation bool           `json:"requires_confirmation"`
	Reversible           bool           `json:"reversible"`
	InputSchema          map[string]any `json:"input_schema,omitempty"`
}

func capabilitySlug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	return strings.NewReplacer(" ", "_", "/", "_", "::", ".").Replace(value)
}

// maxCapabilityClause caps how many capabilities reach the prompt.
const maxCapabilityClause = 120

// CapabilityRegistry projects what a user can do: CRUD capabilities from the
// models they may reach (permission-filtered, module-gated) plus the
// higher-level capabilities enabled modules declare. The browser may add
// presentation capabilities (guides/targets) but cannot grant data mutations.
type CapabilityRegistry struct {
	CRUD *CRUDService
	// Gate limits declared capabilities to enabled modules. Optional.
	Gate ModuleGate
	// Declared supplies manifest-declared capabilities. Optional.
	Declared DeclaredCapabilityProvider
	// Named checks the permission a declared capability names. Optional;
	// when nil, declared capabilities that name a permission are dropped
	// (fail closed).
	Named NamedPermissionChecker
}

// List returns the authoritative capabilities for orgID/userID.
func (r *CapabilityRegistry) List(orgID, userID uuid.UUID) ([]CapabilityDescriptor, error) {
	if r == nil || r.CRUD == nil {
		return nil, fmt.Errorf("capability registry unavailable")
	}
	identity := &AgentIdentity{OrganizationID: orgID, UserID: userID}
	available, err := r.CRUD.ListModels(identity)
	if err != nil {
		return nil, err
	}

	result := make([]CapabilityDescriptor, 0, len(available)*4)
	for _, item := range available {
		model, _ := item["model"].(string)
		if model == "" {
			continue
		}
		title, _ := item["title"].(string)
		if title == "" {
			title = model
		}
		owner := "core"
		if r.CRUD.cfg.Gate != nil {
			if o := r.CRUD.cfg.Gate.OwnerOf(model); o != "" {
				owner = o
			}
		}
		slug := capabilitySlug(model)
		add := func(action, kind, risk string, confirm, reversible bool) {
			if err := r.CRUD.CheckPermission(identity, model, action); err != nil {
				return
			}
			verb := map[string]string{"index": "Consultar", "create": "Crear", "update": "Editar", "delete": "Eliminar"}[action]
			result = append(result, CapabilityDescriptor{
				ID: owner + "." + slug + "." + action, Owner: owner,
				Title:       verb + " " + title,
				Description: verb + " registros de " + title + " respetando permisos y alcance de organización.",
				Kind:        kind, Risk: risk, Permission: slug + "." + action, Model: model, Action: action,
				RequiresConfirmation: confirm, Reversible: reversible,
				InputSchema: map[string]any{"type": "object", "additionalProperties": true},
			})
		}
		add("index", "answer", "none", false, true)
		add("create", "transact", "medium", true, true)
		add("update", "transact", "medium", true, true)
		add("delete", "transact", "high", true, false)
	}

	if r.Declared == nil {
		return result, nil
	}
	declared, err := r.Declared.DeclaredCapabilities(orgID)
	if err != nil {
		return result, nil // CRUD capabilities stay valid without manifests
	}
	enabled := map[string]bool{}
	if r.Gate != nil {
		if mods, err := r.Gate.Enabled(orgID); err == nil {
			for _, m := range mods {
				enabled[m.Key] = true
			}
		}
	}
	for _, d := range declared {
		if d.ID == "" || d.Title == "" || d.Description == "" {
			continue
		}
		if r.Gate != nil && !enabled[d.Owner] {
			continue
		}
		if d.Permission != "" {
			if r.Named == nil {
				continue
			}
			if ok, err := r.Named.HasPermission(orgID, userID, d.Permission); err != nil || !ok {
				continue
			}
		}
		switch d.Kind {
		case "answer", "guide", "act", "transact":
		default:
			continue
		}
		if d.Kind == "transact" && !d.RequiresConfirmation {
			continue
		}
		switch d.Risk {
		case "none", "low", "medium", "high", "critical":
		default:
			continue
		}
		result = append(result, d)
	}
	return result, nil
}

// PromptClause renders the capabilities as the authoritative registry block
// appended to the system prompt ("" when there is nothing to say).
func (r *CapabilityRegistry) PromptClause(orgID, userID uuid.UUID) string {
	caps, err := r.List(orgID, userID)
	if err != nil || len(caps) == 0 {
		return ""
	}
	if len(caps) > maxCapabilityClause {
		caps = caps[:maxCapabilityClause]
	}
	type compact struct {
		ID      string `json:"id"`
		Model   string `json:"model,omitempty"`
		Action  string `json:"action,omitempty"`
		Risk    string `json:"risk"`
		Confirm bool   `json:"confirm"`
	}
	out := make([]compact, 0, len(caps))
	for _, c := range caps {
		out = append(out, compact{ID: c.ID, Model: c.Model, Action: c.Action, Risk: c.Risk, Confirm: c.RequiresConfirmation})
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return "\n\nREGISTRO AUTORITATIVO DE CAPACIDADES PARA ESTE USUARIO:\n" + string(raw) +
		"\nUsa solo modelos/acciones presentes aquí. confirm=true significa que debes crear una propuesta y esperar aprobación; nunca ejecutes directamente."
}
