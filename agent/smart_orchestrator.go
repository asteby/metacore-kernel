package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/asteby/metacore-kernel/agent/contextmgr"

	"github.com/google/uuid"
)

// AgentProfile is the host-neutral view of one specialist agent.
type AgentProfile struct {
	ID          uuid.UUID
	Name        string
	Description string
	Status      string
	// Tools are the names of the tools the agent has enabled.
	Tools []string
}

// OrchestratorRef identifies the orchestrator a message is routed through.
type OrchestratorRef struct {
	ID   uuid.UUID
	Name string
}

// AgentDirectory lists the specialists behind an orchestrator. Implemented by
// the host over its agents table.
type AgentDirectory interface {
	AgentsFor(orchestratorID uuid.UUID) ([]AgentProfile, error)
}

// AgentRunner runs one turn as the selected specialist (typically by building
// the agent's system prompt and tools and calling AgentOrchestrator).
type AgentRunner interface {
	RunAgent(ctx context.Context, userMessage string, conversationID uuid.UUID, agent AgentProfile) (*AgentResponse, error)
}

// SmartOrchestrator implements AI-driven agent selection and routing: it
// analyzes the user message and decides which specialist agent should answer.
type SmartOrchestrator struct {
	directory  AgentDirectory
	runner     AgentRunner
	contextMgr *contextmgr.Manager
	aiService  AIService
}

// AgentCapability represents an agent's capabilities for AI analysis
type AgentCapability struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Specialties []string  `json:"specialties"`
	Tools       []string  `json:"tools"`
	Score       float64   `json:"score,omitempty"`
}

// RoutingDecision represents the AI's decision on which agent to use
type RoutingDecision struct {
	SelectedAgentID   uuid.UUID  `json:"selected_agent_id"`
	SelectedAgentName string     `json:"selected_agent_name"`
	Confidence        float64    `json:"confidence"`
	Reasoning         string     `json:"reasoning"`
	ShouldHandoff     bool       `json:"should_handoff"`
	FallbackAgentID   *uuid.UUID `json:"fallback_agent_id,omitempty"`
}

// OrchestratorContext tracks agent routing history for a conversation
type OrchestratorContext struct {
	ConversationID     uuid.UUID              `json:"conversation_id"`
	OrchestratorID     uuid.UUID              `json:"orchestrator_id"`
	CurrentAgentID     *uuid.UUID             `json:"current_agent_id,omitempty"`
	PreviousAgentIDs   []uuid.UUID            `json:"previous_agent_ids"`
	RoutingHistory     []RoutingDecision      `json:"routing_history"`
	ConversationTopics []string               `json:"conversation_topics"`
	LastRoutingTime    time.Time              `json:"last_routing_time"`
	Metadata           map[string]interface{} `json:"metadata"`
}

// NewSmartOrchestrator creates a smart orchestrator.
func NewSmartOrchestrator(directory AgentDirectory, runner AgentRunner, contextMgr *contextmgr.Manager, aiService AIService) *SmartOrchestrator {
	return &SmartOrchestrator{directory: directory, runner: runner, contextMgr: contextMgr, aiService: aiService}
}

// ProcessMessage handles a message through the smart orchestrator
// It decides which agent should handle the message and routes accordingly
func (so *SmartOrchestrator) ProcessMessage(
	ctx context.Context,
	userMessage string,
	conversationID uuid.UUID,
	orchestrator OrchestratorRef,
) (*AgentResponse, error) {
	startTime := time.Now()
	log.Printf("🎭 SmartOrchestrator: Processing message for conversation %s", conversationID)

	// 1. Load available agents for this orchestrator
	agents, err := so.loadOrchestratorAgents(orchestrator.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to load orchestrator agents: %w", err)
	}

	if len(agents) == 0 {
		return nil, fmt.Errorf("no agents configured for orchestrator %s", orchestrator.Name)
	}

	log.Printf("🎭 SmartOrchestrator: Found %d agents available", len(agents))

	// 2. Get orchestrator context (routing history)
	orchCtx := so.getOrchestratorContext(conversationID, orchestrator.ID)

	// 3. Build agent capabilities for AI analysis
	capabilities := so.buildAgentCapabilities(agents)

	// 4. Use AI to select the best agent
	decision, err := so.selectAgentWithAI(ctx, userMessage, capabilities, orchCtx)
	if err != nil {
		log.Printf("⚠️ AI agent selection failed, using fallback: %v", err)
		decision = so.fallbackSelection(agents, orchCtx)
	}

	log.Printf("🎯 SmartOrchestrator: Selected agent '%s' (confidence: %.2f%%)",
		decision.SelectedAgentName, decision.Confidence*100)
	log.Printf("💭 Reasoning: %s", decision.Reasoning)

	// 5. Update orchestrator context
	orchCtx.CurrentAgentID = &decision.SelectedAgentID
	orchCtx.RoutingHistory = append(orchCtx.RoutingHistory, *decision)
	orchCtx.LastRoutingTime = time.Now()
	so.saveOrchestratorContext(orchCtx)

	// 6. Find and use the selected agent
	var selectedAgent AgentProfile
	for _, agent := range agents {
		if agent.ID == decision.SelectedAgentID {
			selectedAgent = agent
			break
		}
	}

	if selectedAgent.ID == uuid.Nil {
		return nil, fmt.Errorf("no agent selected for orchestrator %s", orchestrator.Name)
	}

	// 7. Run the selected specialist
	response, err := so.runner.RunAgent(ctx, userMessage, conversationID, selectedAgent)
	if err != nil {
		return nil, fmt.Errorf("agent processing failed: %w", err)
	}

	// 8. Add orchestrator metadata to response
	if response.Metadata == nil {
		response.Metadata = make(map[string]interface{})
	}
	response.Metadata["orchestrator_id"] = orchestrator.ID
	response.Metadata["orchestrator_name"] = orchestrator.Name
	response.Metadata["routed_to_agent"] = decision.SelectedAgentName
	response.Metadata["routing_confidence"] = decision.Confidence
	response.Metadata["routing_reasoning"] = decision.Reasoning

	log.Printf("✅ SmartOrchestrator: Completed in %v", time.Since(startTime))
	return response, nil
}

// loadOrchestratorAgents loads all agents associated with an orchestrator
func (so *SmartOrchestrator) loadOrchestratorAgents(orchestratorID uuid.UUID) ([]AgentProfile, error) {
	return so.directory.AgentsFor(orchestratorID)
}

// buildAgentCapabilities creates a structured representation of agent capabilities
func (so *SmartOrchestrator) buildAgentCapabilities(agents []AgentProfile) []AgentCapability {
	capabilities := make([]AgentCapability, len(agents))

	for i, agent := range agents {
		// Extract specialties from description
		specialties := so.extractSpecialties(agent.Description)

		// Get tool names
		tools := append([]string(nil), agent.Tools...)

		capabilities[i] = AgentCapability{
			ID:          agent.ID,
			Name:        agent.Name,
			Description: agent.Description,
			Specialties: specialties,
			Tools:       tools,
		}
	}

	return capabilities
}

// extractSpecialties extracts key specialties from agent description
func (so *SmartOrchestrator) extractSpecialties(description string) []string {
	// Simple keyword extraction - could be enhanced with NLP
	keywords := []string{
		"ventas", "soporte", "técnico", "facturación", "consultas",
		"pedidos", "productos", "precios", "reclamos", "información",
		"reservas", "citas", "envíos", "devoluciones", "pagos",
		"sales", "support", "technical", "billing", "orders",
		"products", "prices", "complaints", "shipping", "returns",
	}

	found := []string{}
	descLower := strings.ToLower(description)
	for _, kw := range keywords {
		if strings.Contains(descLower, kw) {
			found = append(found, kw)
		}
	}

	return found
}

// selectAgentWithAI uses AI to determine the best agent for the message
func (so *SmartOrchestrator) selectAgentWithAI(
	ctx context.Context,
	userMessage string,
	capabilities []AgentCapability,
	orchCtx *OrchestratorContext,
) (*RoutingDecision, error) {
	// Build the AI prompt for agent selection
	systemPrompt := so.buildRoutingSystemPrompt(capabilities, orchCtx)

	messages := []map[string]interface{}{
		{
			"role":    "user",
			"content": userMessage,
		},
	}

	// Define routing tool
	routingTools := []map[string]interface{}{
		{
			"name":        "route_to_agent",
			"description": "Selecciona el agente más adecuado para manejar este mensaje del usuario",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"agent_id": map[string]interface{}{
						"type":        "string",
						"description": "UUID del agente seleccionado",
					},
					"agent_name": map[string]interface{}{
						"type":        "string",
						"description": "Nombre del agente seleccionado",
					},
					"confidence": map[string]interface{}{
						"type":        "number",
						"description": "Nivel de confianza en la selección (0.0 a 1.0)",
					},
					"reasoning": map[string]interface{}{
						"type":        "string",
						"description": "Explicación breve de por qué este agente es el mejor para esta consulta",
					},
					"should_handoff": map[string]interface{}{
						"type":        "boolean",
						"description": "Si se debe transferir a humano en lugar de usar un agente",
					},
				},
				"required": []string{"agent_id", "agent_name", "confidence", "reasoning"},
			},
		},
	}

	// Call AI with routing tool
	aiResp, err := so.aiService.GenerateChatWithTools(ctx, messages, systemPrompt, routingTools)
	if err != nil {
		return nil, fmt.Errorf("AI service failed: %w", err)
	}

	// Parse the tool call response
	if len(aiResp.ToolCalls) > 0 {
		for _, tc := range aiResp.ToolCalls {
			if tc.ToolName == "route_to_agent" {
				return so.parseRoutingDecision(tc.Parameters, capabilities)
			}
		}
	}

	// Fallback: try to parse from message if no tool call
	return so.parseRoutingFromMessage(aiResp.Message, capabilities)
}

// buildRoutingSystemPrompt creates the system prompt for agent routing
func (so *SmartOrchestrator) buildRoutingSystemPrompt(capabilities []AgentCapability, orchCtx *OrchestratorContext) string {
	prompt := `Eres un orquestador inteligente de agentes. Tu tarea es analizar el mensaje del usuario y seleccionar el agente más adecuado para manejarlo.

## AGENTES DISPONIBLES
`
	for _, cap := range capabilities {
		prompt += fmt.Sprintf(`
### %s (ID: %s)
- **Descripción**: %s
- **Especialidades**: %s
- **Herramientas**: %s
`,
			cap.Name,
			cap.ID.String(),
			cap.Description,
			strings.Join(cap.Specialties, ", "),
			strings.Join(cap.Tools, ", "),
		)
	}

	prompt += `
## REGLAS DE ROUTING

1. **Analiza el mensaje del usuario**:
   - Identifica el tema principal (ventas, soporte, información, etc.)
   - Detecta la intención (consulta, queja, compra, etc.)
   - Considera el tono (urgente, casual, formal)

2. **Selecciona el mejor agente**:
   - Prioriza agentes cuya descripción coincida con el tema
   - Considera las herramientas disponibles del agente
   - Si hay duda, prefiere el agente más general

3. **Casos especiales**:
   - Si el usuario pide hablar con humano: should_handoff = true
   - Si el mensaje es muy ambiguo: usa el agente con descripción más general
   - Si es una queja seria: considera transferir a humano

4. **Confianza**:
   - Alta (0.8-1.0): El mensaje claramente corresponde a un agente
   - Media (0.5-0.7): El mensaje podría ser manejado por varios agentes
   - Baja (0.3-0.5): No hay un agente ideal, usar el más general

USA la herramienta route_to_agent para indicar tu decisión.`

	// Add routing history context
	if len(orchCtx.RoutingHistory) > 0 {
		prompt += "\n\n## HISTORIAL DE ROUTING RECIENTE\n"
		for i := len(orchCtx.RoutingHistory) - 1; i >= 0 && i >= len(orchCtx.RoutingHistory)-3; i-- {
			h := orchCtx.RoutingHistory[i]
			prompt += fmt.Sprintf("- Agente: %s (confianza: %.0f%%)\n", h.SelectedAgentName, h.Confidence*100)
		}
		prompt += "\nConsidera mantener continuidad si el tema es el mismo.\n"
	}

	return prompt
}

// parseRoutingDecision parses the AI's routing decision from tool parameters
func (so *SmartOrchestrator) parseRoutingDecision(params map[string]interface{}, capabilities []AgentCapability) (*RoutingDecision, error) {
	decision := &RoutingDecision{}

	if agentID, ok := params["agent_id"].(string); ok {
		parsed, err := uuid.Parse(agentID)
		if err != nil {
			// Try to find by name
			if name, ok := params["agent_name"].(string); ok {
				for _, cap := range capabilities {
					if strings.EqualFold(cap.Name, name) {
						decision.SelectedAgentID = cap.ID
						decision.SelectedAgentName = cap.Name
						break
					}
				}
			}
			if decision.SelectedAgentID == uuid.Nil {
				return nil, fmt.Errorf("invalid agent_id: %s", agentID)
			}
		} else {
			decision.SelectedAgentID = parsed
		}
	}

	if name, ok := params["agent_name"].(string); ok {
		decision.SelectedAgentName = name
	}

	if confidence, ok := params["confidence"].(float64); ok {
		decision.Confidence = confidence
	} else {
		decision.Confidence = 0.7 // Default confidence
	}

	if reasoning, ok := params["reasoning"].(string); ok {
		decision.Reasoning = reasoning
	}

	if handoff, ok := params["should_handoff"].(bool); ok {
		decision.ShouldHandoff = handoff
	}

	// Validate agent exists
	found := false
	for _, cap := range capabilities {
		if cap.ID == decision.SelectedAgentID {
			found = true
			decision.SelectedAgentName = cap.Name
			break
		}
	}

	if !found && len(capabilities) > 0 {
		// Fallback to first agent
		decision.SelectedAgentID = capabilities[0].ID
		decision.SelectedAgentName = capabilities[0].Name
		decision.Reasoning = "Fallback: agente no encontrado, usando agente predeterminado"
		decision.Confidence = 0.5
	}

	return decision, nil
}

// parseRoutingFromMessage attempts to parse routing from AI message (fallback)
func (so *SmartOrchestrator) parseRoutingFromMessage(message string, capabilities []AgentCapability) (*RoutingDecision, error) {
	// Simple fallback: try to match agent names in the message
	messageLower := strings.ToLower(message)

	for _, cap := range capabilities {
		if strings.Contains(messageLower, strings.ToLower(cap.Name)) {
			return &RoutingDecision{
				SelectedAgentID:   cap.ID,
				SelectedAgentName: cap.Name,
				Confidence:        0.6,
				Reasoning:         "Agente mencionado en respuesta de IA",
			}, nil
		}
	}

	// Ultimate fallback: use first agent
	if len(capabilities) > 0 {
		return &RoutingDecision{
			SelectedAgentID:   capabilities[0].ID,
			SelectedAgentName: capabilities[0].Name,
			Confidence:        0.4,
			Reasoning:         "Fallback: no se pudo determinar agente óptimo",
		}, nil
	}

	return nil, fmt.Errorf("no agents available for routing")
}

// fallbackSelection provides a simple fallback when AI routing fails
func (so *SmartOrchestrator) fallbackSelection(agents []AgentProfile, orchCtx *OrchestratorContext) *RoutingDecision {
	// If there was a previous agent, continue with it
	if orchCtx.CurrentAgentID != nil {
		for _, agent := range agents {
			if agent.ID == *orchCtx.CurrentAgentID {
				return &RoutingDecision{
					SelectedAgentID:   agent.ID,
					SelectedAgentName: agent.Name,
					Confidence:        0.5,
					Reasoning:         "Continuando con agente anterior (fallback)",
				}
			}
		}
	}

	// Otherwise use first agent
	if len(agents) > 0 {
		return &RoutingDecision{
			SelectedAgentID:   agents[0].ID,
			SelectedAgentName: agents[0].Name,
			Confidence:        0.4,
			Reasoning:         "Usando agente predeterminado (fallback)",
		}
	}

	return &RoutingDecision{
		ShouldHandoff: true,
		Reasoning:     "No hay agentes disponibles",
	}
}

// getOrchestratorContext retrieves or creates orchestrator context for a conversation
func (so *SmartOrchestrator) getOrchestratorContext(convID, orchID uuid.UUID) *OrchestratorContext {
	// Try to load from conversation context metadata
	convCtx := so.contextMgr.GetContext(convID)

	if convCtx.Metadata != nil {
		if orchCtxData, ok := convCtx.Metadata["orchestrator_context"]; ok {
			if data, err := json.Marshal(orchCtxData); err == nil {
				var orchCtx OrchestratorContext
				if json.Unmarshal(data, &orchCtx) == nil {
					return &orchCtx
				}
			}
		}
	}

	// Create new context
	return &OrchestratorContext{
		ConversationID:     convID,
		OrchestratorID:     orchID,
		PreviousAgentIDs:   []uuid.UUID{},
		RoutingHistory:     []RoutingDecision{},
		ConversationTopics: []string{},
		Metadata:           make(map[string]interface{}),
	}
}

// saveOrchestratorContext saves orchestrator context to conversation
func (so *SmartOrchestrator) saveOrchestratorContext(orchCtx *OrchestratorContext) {
	convCtx := so.contextMgr.GetContext(orchCtx.ConversationID)

	if convCtx.Metadata == nil {
		convCtx.Metadata = make(map[string]interface{})
	}

	// Keep only last 10 routing decisions to avoid context bloat
	if len(orchCtx.RoutingHistory) > 10 {
		orchCtx.RoutingHistory = orchCtx.RoutingHistory[len(orchCtx.RoutingHistory)-10:]
	}

	convCtx.Metadata["orchestrator_context"] = orchCtx
	so.contextMgr.SaveContext(convCtx)
}

// GetAgentStats returns statistics about the agents behind an orchestrator.
func (so *SmartOrchestrator) GetAgentStats(orchestratorID uuid.UUID) map[string]interface{} {
	agents, err := so.directory.AgentsFor(orchestratorID)
	if err != nil {
		return map[string]interface{}{"error": err.Error()}
	}
	agentStats := make([]map[string]interface{}, 0, len(agents))
	for _, agent := range agents {
		agentStats = append(agentStats, map[string]interface{}{
			"id":          agent.ID,
			"name":        agent.Name,
			"description": agent.Description,
			"status":      agent.Status,
			"tools_count": len(agent.Tools),
		})
	}
	return map[string]interface{}{
		"orchestrator_id": orchestratorID,
		"total_agents":    len(agents),
		"agents":          agentStats,
	}
}
