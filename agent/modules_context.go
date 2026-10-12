package agent

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// InstalledModulesContext grounds the assistant's self-described abilities in
// the modules the org actually has enabled, instead of assuming a vertical.
// Returns "" when nothing is enabled or the gate cannot say.
func InstalledModulesContext(gate ModuleGate, orgID uuid.UUID) string {
	if gate == nil {
		return ""
	}
	mods, err := gate.Enabled(orgID)
	if err != nil || len(mods) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n## MÓDULOS INSTALADOS (tus capacidades reales)\n")
	sb.WriteString("La organización tiene instalados estos módulos. SOLO ofrece ayuda sobre lo que existe acá; si te piden algo de un módulo no instalado, di que no está disponible en esta instancia.\n")
	for _, m := range mods {
		name := m.Name
		if name == "" {
			name = m.Key
		}
		sb.WriteString(fmt.Sprintf("- **%s**", name))
		if m.Description != "" {
			sb.WriteString(": " + m.Description)
		}
		if len(m.Capabilities) > 0 {
			sb.WriteString(fmt.Sprintf(" (entidades: %s)", strings.Join(m.Capabilities, ", ")))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// PromptSection is one block of host-owned prompt context (currency, product
// catalog, contact data capture...). The runtime assembles sections in order.
type PromptSection interface {
	// Section returns the text to append ("" to skip).
	Section(orgID uuid.UUID) string
}

// PromptSectionFunc adapts a function to PromptSection.
type PromptSectionFunc func(orgID uuid.UUID) string

// Section implements PromptSection.
func (f PromptSectionFunc) Section(orgID uuid.UUID) string { return f(orgID) }

// CatalogContext builds the "what exists here" context appended to an agent's
// system prompt: the installed modules first, then the host's own sections.
type CatalogContext struct {
	Gate     ModuleGate
	Sections []PromptSection
}

// Build returns the full context for orgID.
func (c CatalogContext) Build(orgID uuid.UUID) string {
	var sb strings.Builder
	sb.WriteString(InstalledModulesContext(c.Gate, orgID))
	for _, s := range c.Sections {
		if s == nil {
			continue
		}
		sb.WriteString(s.Section(orgID))
	}
	return sb.String()
}

// CurrencyClause renders the currency instructions for the prompt.
func CurrencyClause(name, symbol string) string {
	info := "La moneda configurada es el Dólar ($)."
	if symbol != "" {
		info = fmt.Sprintf("La moneda de la organización es %s (%s). Formatea montos monetarios como %s100.00. No menciones el nombre de la moneda a menos que te pregunten; simplemente usa el símbolo al dar precios de forma natural.", name, symbol, symbol)
	}
	return "\n## CONFIGURACIÓN DE MONEDA\n- " + info + "\n"
}
