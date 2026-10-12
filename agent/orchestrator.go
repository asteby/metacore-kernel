package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/asteby/metacore-kernel/agent/contextmgr"
	"github.com/asteby/metacore-kernel/agent/tools"

	"github.com/google/uuid"
)

// StreamCallback emits real-time activity events during agent processing.
// Used to stream tool calls and progress to the frontend via WebSocket.
type StreamCallback func(sessionID uuid.UUID, event AIActivityEvent)

// AIActivityEvent represents a real-time activity update from the AI
type AIActivityEvent struct {
	SessionID string `json:"session_id"`
	Type      string `json:"type"` // "thinking", "tool_call", "tool_result", "done"
	Tool      string `json:"tool,omitempty"`
	Message   string `json:"message"`
	Success   bool   `json:"success,omitempty"`
}

// AIService is the model port of the orchestrator loop: one turn with tools.
type AIService interface {
	GenerateChatWithTools(ctx context.Context, messages []map[string]interface{}, systemPrompt string, tools []map[string]interface{}) (*AIResponse, error)
}

// AIResponse is one model turn.
type AIResponse struct {
	Message   string
	ToolCalls []tools.ToolCall
	Finished  bool
}

// AgentResponse is what a finished turn produced.
type AgentResponse struct {
	Message       string
	Images        []ImageToSend
	Audio         []AudioToSend
	ShouldDisable bool
	Metadata      map[string]interface{}
	// Embeds are clickable record/view cards for query results.
	Embeds []ChatEmbed
}

// ImageToSend is an image queued for the reply.
type ImageToSend struct {
	URL     string
	Caption string
}

// AudioToSend is an audio clip queued for the reply.
type AudioToSend struct {
	URL string
}

// OrchestratorConfig wires an AgentOrchestrator to the host.
type OrchestratorConfig struct {
	// Tools are the base tools; each turn works on a Clone.
	Tools *tools.ToolRegistry
	// Context keeps per-conversation state.
	Context *contextmgr.Manager
	// AI runs one model turn.
	AI AIService
	// Models resolves model keys and labels for activity trails and embeds.
	// Optional; nil disables model-aware labels and record embeds.
	Models ModelRegistry
	// Catalog supplies navigation icons for the activity trail. Optional.
	Catalog ModelCatalog
	// MaxIterations bounds the reasoning loop (default 12).
	MaxIterations int
}

// AgentOrchestrator runs the autonomous tool-using loop: it feeds the model
// the conversation, executes the tools it asks for, and stops on a final
// answer. Everything host-specific (system prompt, per-agent tools, org AI
// runtime on the context) is prepared by the caller and passed to Run.
type AgentOrchestrator struct {
	toolRegistry   *tools.ToolRegistry
	contextMgr     *contextmgr.Manager
	aiService      AIService
	embeds         *EmbedBuilder
	catalog        ModelCatalog
	models         ModelRegistry
	maxIterations  int
	streamCallback StreamCallback
}

// NewAgentOrchestrator builds an orchestrator from cfg.
func NewAgentOrchestrator(cfg OrchestratorConfig) *AgentOrchestrator {
	max := cfg.MaxIterations
	if max <= 0 {
		max = 12
	}
	reg := cfg.Tools
	if reg == nil {
		reg = tools.NewToolRegistry()
	}
	o := &AgentOrchestrator{
		toolRegistry:  reg,
		contextMgr:    cfg.Context,
		aiService:     cfg.AI,
		catalog:       cfg.Catalog,
		models:        cfg.Models,
		maxIterations: max,
	}
	if cfg.Models != nil {
		o.embeds = NewEmbedBuilder(cfg.Models)
	}
	return o
}

// SetStreamCallback sets the callback for real-time activity streaming
func (o *AgentOrchestrator) SetStreamCallback(cb StreamCallback) {
	o.streamCallback = cb
}

// emitActivity sends an activity event if a callback is set
func (o *AgentOrchestrator) emitActivity(sessionID uuid.UUID, eventType, tool, message string, success bool) {
	if o.streamCallback != nil {
		o.streamCallback(sessionID, AIActivityEvent{
			SessionID: sessionID.String(),
			Type:      eventType,
			Tool:      tool,
			Message:   message,
			Success:   success,
		})
	}
}

// Turn is one user message to process.
type Turn struct {
	ConversationID uuid.UUID
	// AgentID tags the response metadata and the conversation context.
	AgentID uuid.UUID
	// UserMessage is the incoming text.
	UserMessage string
	// SystemPrompt is the host-authored prompt WITHOUT the tools section; the
	// runtime appends the tool list and the closing rules.
	SystemPrompt string
	// ExtraTools are registered on top of the base tools for this turn only
	// (per-agent dynamic tools, per-turn guides).
	ExtraTools []tools.BaseTool
}

// ProcessMessage runs one turn. The caller stamps the context with whatever
// the model layer needs (org AI runtime, billing identity) before calling.
func (o *AgentOrchestrator) ProcessMessage(ctx context.Context, turn Turn) (*AgentResponse, error) {
	startTime := time.Now()
	log.Printf("🤖 Agent: Processing message for conversation %s", turn.ConversationID)
	o.emitActivity(turn.ConversationID, "thinking", "", "sparkles|Recibí tu solicitud…", true)

	// 1. Registry: base + per-turn tools.
	registry := o.toolRegistry.Clone()
	for _, t := range turn.ExtraTools {
		registry.Register(t)
	}
	// Guides the browser announced this turn (dynamic catalog).
	if gt := GuideTurnFrom(ctx); gt != nil && len(gt.catalog) > 0 {
		registry.Register(NewStartGuideTool(gt))
	}

	// 2. Conversation context.
	convCtx := o.contextMgr.GetContext(turn.ConversationID)
	convCtx.AgentID = turn.AgentID
	convCtx.MessageCount++

	// 3. System prompt with the tools section.
	systemPrompt := AppendToolsSection(turn.SystemPrompt, registry)

	// 4. History.
	messages := o.buildMessageHistory(convCtx, turn.UserMessage)
	convCtx.History = append(convCtx.History, contextmgr.Message{Role: "user", Content: turn.UserMessage})

	// 5. Reasoning loop.
	response, err := o.agentLoop(ctx, messages, systemPrompt, convCtx, registry)
	if err != nil {
		return nil, fmt.Errorf("agent loop failed: %w", err)
	}

	// 6. Tracking metadata.
	response.Metadata["conversation_id"] = turn.ConversationID
	response.Metadata["agent_id"] = turn.AgentID
	response.Metadata["sender_type"] = "agent"

	// 7. Persist the updated context.
	if response.Message != "" {
		convCtx.History = append(convCtx.History, contextmgr.Message{Role: "assistant", Content: response.Message})
	}
	o.contextMgr.SaveContext(convCtx)

	log.Printf("✅ Agent: Completed in %v", time.Since(startTime))
	return response, nil
}

// AppendToolsSection renders the tool list and the closing behavior rules
// after the host's system prompt.
func AppendToolsSection(prompt string, registry *tools.ToolRegistry) string {
	var sb strings.Builder
	sb.WriteString(prompt)
	sb.WriteString("\n\n## HERRAMIENTAS DISPONIBLES\n")
	for _, tool := range registry.List() {
		sb.WriteString(fmt.Sprintf("\n- **%s**: %s", tool.Name(), tool.Description()))
		params := tool.Parameters()
		if len(params) == 0 {
			continue
		}
		sb.WriteString("\n  Parámetros:")

		var properties map[string]interface{}
		var required []string
		if p, ok := params["properties"].(map[string]interface{}); ok && params["type"] == "object" {
			properties = p
			if req, ok := params["required"].([]string); ok {
				required = req
			}
		} else {
			properties = params
		}
		for paramName, paramSchema := range properties {
			schema, ok := paramSchema.(map[string]interface{})
			if !ok {
				continue
			}
			paramType, _ := schema["type"].(string)
			paramDesc, _ := schema["description"].(string)
			isReq, _ := schema["required"].(bool)
			if !isReq {
				for _, r := range required {
					if r == paramName {
						isReq = true
						break
					}
				}
			}
			reqMark := ""
			if isReq {
				reqMark = " *REQUERIDO*"
			}
			sb.WriteString(fmt.Sprintf("\n    • %s (%s)%s: %s", paramName, paramType, reqMark, paramDesc))
		}
	}
	sb.WriteString("\n\nSi la pregunta se responde sin datos de la organización (quién eres, cómo se hace algo, un saludo), responde directo con final_answer. " +
		"Si hace falta un registro, un conteo o un cambio, llama la herramienta y no afirmes el resultado antes. " +
		"Prohibido narrar el siguiente paso (\"voy a…\", \"ahora creo…\"): o llamas la herramienta, o respondes.")
	return sb.String()
}

// modelLabel / modelIcon / embeds adapt the optional ports.
func (o *AgentOrchestrator) modelLabel(model string) string {
	if o.catalog != nil {
		return ModelLabel(o.catalog, model)
	}
	if o.models != nil {
		if model == "" {
			return "datos"
		}
		if l := o.models.ModelLabel(o.models.ResolveModelKey(model)); l != "" {
			return l
		}
	}
	if model == "" {
		return "datos"
	}
	return humanizeSnake(model)
}

func (o *AgentOrchestrator) modelIcon(model string) string {
	return ModelIcon(o.catalog, model)
}

func (o *AgentOrchestrator) embedsFromToolResult(toolName string, result interface{}, params map[string]interface{}) []ChatEmbed {
	if o.embeds != nil {
		return o.embeds.FromToolResult(toolName, result, params)
	}
	// Charts and images need no model registry.
	return NewEmbedBuilder(noModels{}).FromToolResult(toolName, result, params)
}

// noModels is the identity ModelRegistry used when the host provides none.
type noModels struct{}

func (noModels) ResolveModelKey(name string) string       { return name }
func (noModels) ModelLabel(modelKey string) string        { return humanizeSnake(modelKey) }
func (noModels) FieldLabel(_, _, raw string) string       { return raw }
func (noModels) EnumValueLabel(_, _, value string) string { return value }

// agentLoop ejecuta el loop de razonamiento del agente
func (o *AgentOrchestrator) agentLoop(ctx context.Context, messages []map[string]interface{}, systemPrompt string, convCtx *contextmgr.ConversationContext, registry *tools.ToolRegistry) (*AgentResponse, error) {
	toolDefs := registry.ToOpenAIFormat()

	response := &AgentResponse{
		Images:   []ImageToSend{},
		Metadata: make(map[string]interface{}),
	}

	// Per-turn budget: the synthesis and last-word turns extend it for this
	// turn only (bumping o.maxIterations leaked into every later turn).
	maxIter := o.maxIterations
	// lastToolFailure names the most recent tool that failed, so a provider
	// error right after it is reported with the tool instead of hiding it.
	var lastToolFailure *tools.ToolResult

	for iteration := 0; iteration < maxIter; iteration++ {
		log.Printf("🔄 Agent iteration %d/%d", iteration+1, maxIter)
		o.emitActivity(convCtx.ConversationID, "thinking", "",
			fmt.Sprintf("cog|Pensando… (paso %d)", iteration+1), true)

		// Llamar al LLM con tools
		aiResp, err := o.aiService.GenerateChatWithTools(ctx, messages, systemPrompt, toolDefs)
		if err != nil {
			if lastToolFailure != nil {
				err = &ToolTurnError{Tool: lastToolFailure.ToolName, ToolErr: lastToolFailure.Error, Err: err}
			}
			return nil, fmt.Errorf("AI service failed: %w", err)
		}
		aiResp = unwrapSyntheticFinalAnswer(aiResp, messages)

		// Si no hay tool calls, el agente terminó — salvo narración sin acción (Aby).
		if len(aiResp.ToolCalls) == 0 {
			// Limpiar respuesta: eliminar JSON envuelto, tags de pensamiento y markdown
			response.Message = CleanAgentResponse(aiResp.Message)

			// Haiku/sonnet a veces narran "voy a revisar…" sin llamar tools.
			// OJO: el nudge inserta un user "[sistema]…"; lastRealUserContent lo
			// salta — si usáramos el último user a ciegas, isChitchat([sistema])
			// era true y el forzado se apagaba en la 2ª iteración (bug Aby).
			userMsg := lastRealUserContent(messages)
			deferred := isDeferredNarration(response.Message)
			nudged := hasSystemNudge(messages)
			ungrounded := claimsUngroundedOrgData(response.Message) && !hasToolRoleMessages(messages)
			// Text with no tool call is the answer. Interrupt once, and only for two
			// failures: narrating an action, or stating org facts with no tool
			// evidence. A repeated invented number is dropped. A repeated
			// "voy a…" closes the turn instead of burning the iteration budget.
			if ungrounded && nudged {
				log.Printf("⚠️ Agent: dropping ungrounded org claim after nudge")
				response.Message = "No consulté el sistema, así que no puedo afirmar cifras ni registros. Dime qué quieres ver y lo busco."
				break
			}
			needsNudge := len(toolDefs) > 0 && !isChitchat(userMsg) && !nudged && (deferred || ungrounded)
			if needsNudge && iteration < maxIter-1 {
				thought := strings.TrimSpace(response.Message)
				if deferred && thought != "" {
					o.emitActivity(convCtx.ConversationID, "thinking", "",
						"brain|"+truncateRunes(thought, 140), true)
				} else {
					o.emitActivity(convCtx.ConversationID, "thinking", "",
						"brain|Ejecutando herramientas…", true)
				}
				messages = append(messages,
					map[string]interface{}{"role": "assistant", "content": response.Message},
					map[string]interface{}{"role": "user", "content": orchestratorNudge(deferred, hasToolRoleMessages(messages))},
				)
				response.Message = ""
				continue
			}

			// Second "voy a…" does not earn another 10 turns. If tools already
			// returned data, one synthesis pass with tools off; otherwise stop.
			// Narration nobody could push past — chitchat, no tools on offer —
			// is still the model's reply: deliver it rather than a generic failure.
			if deferred && (nudged || needsNudge) {
				log.Printf("⚠️ Agent: discarding deferred narration (tools=%v)", hasToolRoleMessages(messages))
				if hasToolRoleMessages(messages) && toolDefs != nil {
					log.Printf("⚠️ Agent: forcing synthesis turn from tool results")
					messages = append(messages,
						map[string]interface{}{"role": "assistant", "content": response.Message},
						map[string]interface{}{
							"role": "user",
							"content": "[sistema] Ya tienes resultados de herramientas en el historial. " +
								"Responde AHORA al usuario con un resumen claro (totales, conteos, insights). " +
								"PROHIBIDO decir \"voy a…\" / \"déjame…\". NO uses herramientas.",
						},
					)
					response.Message = ""
					toolDefs = nil
					maxIter++
					continue
				}
				if hasToolRoleMessages(messages) {
					if fb := o.buildFallbackFromToolResults(messages, registry); fb != "" {
						response.Message = fb
					} else {
						response.Message = "Consulté el sistema pero no pude armar la respuesta final. ¿Repetimos con un poco más de detalle?"
					}
				} else {
					response.Message = "No pude ejecutar la acción con las herramientas. ¿Lo intentamos de nuevo con un poco más de detalle?"
				}
			}

			// Si mensaje vacío después de ejecutar tools, reintentar con delay
			if response.Message == "" && len(response.Images) == 0 && iteration > 0 {
				// Verificar si hay resultados de herramientas en el historial
				hasToolResults := false
				for _, msg := range messages {
					if msg["role"] == "tool" {
						hasToolResults = true
						break
					}
				}

				if hasToolResults {
					// Retry sin límite fijo - basado en tiempo máximo (60s)
					maxRetryDuration := 60 * time.Second
					startTime := time.Now()
					retryCount := 0
					baseDelay := 3 * time.Second

					for time.Since(startTime) < maxRetryDuration {
						retryCount++
						// Delay exponencial con cap en 15s
						delay := baseDelay * time.Duration(retryCount)
						if delay > 15*time.Second {
							delay = 15 * time.Second
						}

						elapsed := time.Since(startTime).Seconds()
						remaining := maxRetryDuration.Seconds() - elapsed
						log.Printf("⏳ LLM empty response, retry %d in %v (%.0fs remaining)", retryCount, delay, remaining)
						time.Sleep(delay)

						// Reintentar llamada al LLM
						retryResp, retryErr := o.aiService.GenerateChatWithTools(ctx, messages, systemPrompt, toolDefs)
						if retryErr != nil {
							log.Printf("⚠️ Retry %d error: %v", retryCount, retryErr)
							continue
						}

						if retryResp.Message != "" {
							response.Message = retryResp.Message
							log.Printf("✅ Retry %d succeeded with message after %.1fs", retryCount, time.Since(startTime).Seconds())
							break
						}

						// Si devuelve tool calls en el retry, procesarlos
						if len(retryResp.ToolCalls) > 0 {
							log.Printf("🔧 Retry returned tool calls, processing...")
							aiResp = retryResp
							goto processToolCalls
						}
					}

					// Si agotamos el tiempo, usar fallback formateado
					if response.Message == "" {
						log.Printf("⚠️ Retry timeout (%.1fs), using formatted fallback", time.Since(startTime).Seconds())
						response.Message = o.buildFallbackFromToolResults(messages, registry)
					}
				}
			}

			// Si hay imágenes sin mensaje, está bien
			if response.Message == "" && len(response.Images) > 0 {
				log.Printf("� Images will be sent without additional text")
			}
			break
		}

	processToolCalls:

		// Añadir mensaje del asistente con tool_calls al historial (requerido por OpenAI-compatible APIs)
		assistantToolCalls := make([]map[string]interface{}, len(aiResp.ToolCalls))
		for i, tc := range aiResp.ToolCalls {
			argsJSON, _ := json.Marshal(tc.Parameters)
			assistantToolCalls[i] = map[string]interface{}{
				"id":   tc.ID,
				"type": "function",
				"function": map[string]interface{}{
					"name":      tc.ToolName,
					"arguments": string(argsJSON),
				},
			}
		}
		messages = append(messages, map[string]interface{}{
			"role":       "assistant",
			"content":    aiResp.Message,
			"tool_calls": assistantToolCalls,
		})

		// Emit tool call events for real-time streaming to frontend
		for _, tc := range aiResp.ToolCalls {
			label := o.describeToolCall(tc.ToolName, tc.Parameters)
			o.emitActivity(convCtx.ConversationID, "tool_call", tc.ToolName, label, true)
		}

		// Ejecutar tool calls
		toolResults := o.executeToolCalls(ctx, aiResp.ToolCalls, convCtx, registry)

		// Procesar resultados
		shouldContinue := true
		for _, result := range toolResults {
			log.Printf("🔧 Processing tool result: %s, success=%v, tool_call_id=%s", result.ToolName, result.Success, result.ToolCallID)
			if !result.Success && result.ToolName != "final_answer" {
				failed := result
				lastToolFailure = &failed
			}
			params := map[string]interface{}{}
			for _, call := range aiResp.ToolCalls {
				if call.ID == result.ToolCallID {
					params = call.Parameters
					break
				}
			}
			o.emitActivity(convCtx.ConversationID, "tool_result", result.ToolName,
				o.toolResultLabel(result.ToolName, params, result.Success), result.Success)

			if result.Success {
				response.Embeds = append(response.Embeds, o.embedsFromToolResult(result.ToolName, result.Result, params)...)
			}

			// Verificar si es final_answer
			if result.ToolName == "final_answer" && result.Success {
				log.Printf("🔧 Found final_answer tool")
				for _, call := range aiResp.ToolCalls {
					if call.ID != result.ToolCallID {
						continue
					}
					msg, _ := call.Parameters["message"].(string)
					cleanedMsg := CleanAgentResponse(msg)
					if cleanedMsg == "" {
						cleanedMsg = msg
					}
					// Never treat "voy a…" as a finished answer — keep the loop going.
					if isDeferredNarration(cleanedMsg) {
						log.Printf("⚠️ Ignoring deferred final_answer; continuing agent loop")
						o.emitActivity(convCtx.ConversationID, "thinking", "",
							"brain|"+truncateRunes(cleanedMsg, 140), true)
						messages = append(messages, map[string]interface{}{
							"role": "user",
							"content": "[sistema] final_answer con intención no cuenta. Llama query_data/aggregate_data/create_record " +
								"ahora con models[].model exacto. Luego sí final_answer con el resultado.",
						})
						shouldContinue = true
						break
					}
					response.Message = cleanedMsg
					log.Printf("✅ Set final_answer message: %s", response.Message)
					shouldContinue = false
					break
				}
			}

			// Verificar si es handoff_to_human
			if result.ToolName == "handoff_to_human" && result.Success {
				if resultMap, ok := result.Result.(map[string]interface{}); ok {
					if action, ok := resultMap["action"].(string); ok && action == "DISABLE_BOT" {
						response.ShouldDisable = true
					}
					if msg, ok := resultMap["message"].(string); ok {
						response.Message = CleanAgentResponse(msg)
						shouldContinue = false
					}
				} else {
					// Fallback: Check input parameters
					for _, call := range aiResp.ToolCalls {
						if call.ID == result.ToolCallID {
							// Check for explicit message param
							if msg, ok := call.Parameters["message"].(string); ok && msg != "" {
								response.Message = CleanAgentResponse(msg)
								shouldContinue = false
							} else if reason, ok := call.Parameters["reason"].(string); ok && reason != "" {
								// If no message but reason is provided, use reason as message for user
								response.Message = fmt.Sprintf("Transferring to human agent: %s", reason)
								shouldContinue = false
							}
							break
						}
					}
				}
			}

			// Verificar si es send_image
			if result.ToolName == "send_image" && result.Success {
				if resultMap, ok := result.Result.(map[string]interface{}); ok {
					status, _ := resultMap["status"].(string)
					// Añadir imagen si está queued O si fue skipped (ya enviada)
					if status == "queued" || status == "skipped" {
						imageURL, _ := resultMap["image_url"].(string)
						if imageURL == "" {
							// Fallback: buscar en el resultado
							if url, ok := resultMap["image_url"].(string); ok {
								imageURL = url
							}
						}

						if imageURL != "" {
							img := ImageToSend{
								URL: imageURL,
							}
							if caption, ok := resultMap["caption"].(string); ok {
								img.Caption = caption
							}

							// Solo añadir si no está duplicada en esta respuesta
							isDuplicate := false
							for _, existingImg := range response.Images {
								if existingImg.URL == imageURL {
									isDuplicate = true
									break
								}
							}

							if !isDuplicate {
								response.Images = append(response.Images, img)
								log.Printf("📷 Image queued for response: %s", imageURL)
							}
						}
					}
				}
			}

			// Verificar si es send_audio (Nuevo)
			if result.ToolName == "send_audio" && result.Success {
				if resultMap, ok := result.Result.(map[string]interface{}); ok {
					audioURL, _ := resultMap["audio_url"].(string)
					if audioURL != "" {
						// Solo añadir si no está duplicado
						isDuplicate := false
						for _, existingAudio := range response.Audio {
							if existingAudio.URL == audioURL {
								isDuplicate = true
								break
							}
						}

						if !isDuplicate {
							response.Audio = append(response.Audio, AudioToSend{
								URL: audioURL,
							})
							log.Printf("🎵 Audio queued for response: %s", audioURL)
						}
					}
				}
			}

			// Formatear resultado según el tipo de herramienta (centralizado en registry)
			var formattedResult string
			if registry.IsActionTool(result.ToolName) {
				// Para herramientas de acción, mensaje simple (no JSON)
				if result.Success {
					formattedResult = fmt.Sprintf("%s ejecutado correctamente", result.ToolName)
				} else {
					formattedResult = fmt.Sprintf("Error en %s: %s", result.ToolName, result.Error)
				}
			} else {
				// Para herramientas informativas, usar formato completo
				formattedResult = result.FormatForLLM()
			}
			log.Printf("📥 Tool Output for %s: %s", result.ToolName, truncateLog(formattedResult, 200))

			// Añadir resultado al historial para próxima iteración (formato OpenAI tool)
			messages = append(messages, map[string]interface{}{
				"role":         "tool",
				"tool_call_id": result.ToolCallID,
				"name":         result.ToolName,
				"content":      formattedResult,
			})
		}

		if !shouldContinue {
			break
		}

		// Si llegamos al máximo de iteraciones, intentar un último razonamiento sin permitir más tools
		if iteration == maxIter-1 && shouldContinue {
			log.Printf("⚠️ Max iterations reached, forcing final answer turn")
			// Añadir una instrucción forzada en el historial
			messages = append(messages, map[string]interface{}{
				"role":    "user",
				"content": "Suficiente investigación. Responde al usuario ahora con la mejor información que tengas disponible. NO uses herramientas.",
			})
			maxIter++
			toolDefs = nil // No permitir más herramientas en el último turno
		}

		if iteration == maxIter-1 && response.Message == "" {
			response.Message = "He analizado tu solicitud pero necesito ser más específico para darte una recomendación exacta. ¿Podrías darme más detalles sobre el uso final?"
		}
	}

	// Limpiar respuesta final: eliminar JSON envuelto, tags de pensamiento y markdown
	response.Message = CleanAgentResponse(response.Message)
	response.Embeds = DedupeEmbeds(response.Embeds)

	return response, nil
}

// executeToolCalls ejecuta múltiples llamadas a herramientas
func (o *AgentOrchestrator) executeToolCalls(ctx context.Context, calls []tools.ToolCall, convCtx *contextmgr.ConversationContext, registry *tools.ToolRegistry) []tools.ToolResult {
	log.Printf("🔧 executeToolCalls: processing %d calls", len(calls))
	results := make([]tools.ToolResult, len(calls))

	for i, call := range calls {
		log.Printf("🔧 Executing tool[%d]: %s (id=%s)", i, call.ToolName, call.ID)
		results[i] = registry.ExecuteTool(ctx, call, convCtx)
		log.Printf("🔧 Tool result[%d]: %s, success=%v", i, results[i].ToolName, results[i].Success)
	}

	return results
}

// buildMessageHistory construye el historial de mensajes
func (o *AgentOrchestrator) buildMessageHistory(convCtx *contextmgr.ConversationContext, currentMessage string) []map[string]interface{} {
	messages := []map[string]interface{}{}

	for _, historyMsg := range convCtx.History {
		messages = append(messages, map[string]interface{}{
			"role":    historyMsg.Role,
			"content": historyMsg.Content,
		})
	}

	shouldAppend := true
	if len(messages) > 0 {
		lastMsg := messages[len(messages)-1]
		if lastMsg["role"] == "user" && lastMsg["content"] == currentMessage {
			shouldAppend = false
		}
	}

	userMsg := map[string]interface{}{
		"role":    "user",
		"content": currentMessage,
	}
	// Multimodal: attach local image bytes for Claude vision (Aby attachments).
	if convCtx.Metadata != nil {
		if paths, ok := convCtx.Metadata[MetaImagePaths].([]string); ok && len(paths) > 0 {
			userMsg["images"] = paths
			delete(convCtx.Metadata, MetaImagePaths)
		}
	}

	if shouldAppend {
		messages = append(messages, userMsg)
	} else if paths, ok := userMsg["images"]; ok {
		messages[len(messages)-1]["images"] = paths
	}

	log.Printf("📚 Built history: %d previous messages + 1 current", len(messages)-1)
	return messages
}

// introspectionTools are schema/catalog probes — never show raw dumps to the user.
var introspectionTools = map[string]bool{
	"list_models":    true,
	"describe_model": true,
}

// buildFallbackFromToolResults construye una respuesta a partir de resultados de herramientas
// cuando el LLM no genera un mensaje (rate limits, errores, etc)
func (o *AgentOrchestrator) buildFallbackFromToolResults(messages []map[string]interface{}, registry *tools.ToolRegistry) string {
	// OpenAI-compatible history uses role "tool" (see executeToolCalls). Older
	// paths may still emit "function" — accept both. Looking only for "function"
	// made fallback always empty on VPS → "no pude armar la respuesta final"
	// even after successful aggregate_data.
	var parts []string
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		role, _ := msg["role"].(string)
		if role != "tool" && role != "function" {
			continue
		}

		toolName, _ := msg["name"].(string)
		content, _ := msg["content"].(string)
		if content == "" {
			continue
		}

		if registry != nil && registry.IsActionTool(toolName) {
			continue
		}
		// Never dump schema introspection to the chat (was leaking Go map[…] dumps).
		if introspectionTools[toolName] {
			continue
		}

		if data, ok := parseToolResultPayload(content); ok {
			parts = append([]string{o.formatToolResultForUser(toolName, data)}, parts...)
		} else if cleaned := o.cleanToolResult(content); cleaned != "" && !looksLikeGoMapDump(cleaned) {
			parts = append([]string{cleaned}, parts...)
		}
		if len(parts) >= 4 {
			break
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return scrubGoMapDump(strings.TrimSpace(strings.Join(parts, "\n\n")))
}

// parseToolResultPayload extracts JSON from "Resultado de foo:\n{...}" blobs.
func parseToolResultPayload(content string) (interface{}, bool) {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, false
	}
	try := []string{content}
	if i := strings.Index(content, "\n"); i >= 0 {
		try = append([]string{strings.TrimSpace(content[i+1:])}, try...)
	}
	if i := strings.Index(content, "{"); i >= 0 {
		try = append([]string{content[i:]}, try...)
	}
	if i := strings.Index(content, "["); i >= 0 {
		try = append([]string{content[i:]}, try...)
	}
	for _, cand := range try {
		if cand == "" {
			continue
		}
		var data interface{}
		if err := json.Unmarshal([]byte(cand), &data); err == nil {
			return data, true
		}
	}
	return nil, false
}

// formatToolResultForUser formatea un resultado de herramienta de forma amigable.
// NUNCA usa fmt %v sobre maps (produce "map[columns:[map[name:…]]]" en el chat).
func (o *AgentOrchestrator) formatToolResultForUser(toolName string, data interface{}) string {
	switch toolName {
	case "list_models":
		return summarizeListModels(data)
	case "describe_model":
		return summarizeDescribeModel(data)
	case "create_record", "update_record", "delete_record":
		return summarizeMutationResult(toolName, data)
	}
	switch v := data.(type) {
	case map[string]interface{}:
		if s, ok := o.summarizeQueryResult(v); ok {
			return s
		}
		return o.formatMapResult(v)
	case []interface{}:
		if len(v) == 0 {
			return "No se encontraron resultados."
		}
		return o.formatArrayResult(v)
	default:
		return humanScalar(data)
	}
}

func summarizeListModels(data interface{}) string {
	m, ok := data.(map[string]interface{})
	if !ok {
		return "Módulos consultados."
	}
	total := m["total"]
	models, _ := m["models"].([]interface{})
	var names []string
	for i, raw := range models {
		if i >= 8 {
			break
		}
		row, _ := raw.(map[string]interface{})
		if row == nil {
			continue
		}
		label := firstString(row, "title", "label", "name", "model")
		if label != "" {
			names = append(names, label)
		}
	}
	sb := strings.Builder{}
	sb.WriteString(fmt.Sprintf("Hay **%v** modelos disponibles", total))
	if len(names) > 0 {
		sb.WriteString(": " + strings.Join(names, ", "))
		if len(models) > len(names) {
			sb.WriteString(fmt.Sprintf("… (+%d)", len(models)-len(names)))
		}
	}
	sb.WriteString(".")
	return sb.String()
}

func summarizeDescribeModel(data interface{}) string {
	m, ok := data.(map[string]interface{})
	if !ok {
		return "Estructura del modelo consultada."
	}
	title := firstString(m, "title", "label", "name", "model")
	if title == "" {
		title = "el modelo"
	}
	cols := 0
	if c, ok := m["columns"].([]interface{}); ok {
		cols = len(c)
	} else if f, ok := m["fields"].([]interface{}); ok {
		cols = len(f)
	}
	if cols > 0 {
		return fmt.Sprintf("Revisé la estructura de **%s** (%d campos).", title, cols)
	}
	return fmt.Sprintf("Revisé la estructura de **%s**.", title)
}

func summarizeMutationResult(toolName string, data interface{}) string {
	m, _ := data.(map[string]interface{})
	status, _ := m["status"].(string)
	model := firstString(m, "model_title", "model")
	switch status {
	case "pending_approval":
		if model != "" {
			return fmt.Sprintf("Propuesta lista para **%s** — revisa y acepta o rechaza en la tarjeta.", model)
		}
		return "Propuesta lista — revisa y acepta o rechaza en la tarjeta."
	default:
		if toolName == "create_record" {
			return "Registro creado."
		}
		if toolName == "update_record" {
			return "Registro actualizado."
		}
		if toolName == "delete_record" {
			return "Registro eliminado."
		}
		return "Acción procesada."
	}
}

// maxQueryRecordLinks bounds the record links a query summary spells out.
const maxQueryRecordLinks = 3

// summarizeQueryResult turns a query payload ({model, count, records}) into
// one sentence: the model's localized label, a human count and links to the
// records. formatMapResult rendered it key by key ("Count: 1", "Records:",
// "Model: warehouses"), which read as a raw tool dump in the chat.
func (o *AgentOrchestrator) summarizeQueryResult(m map[string]interface{}) (string, bool) {
	model, _ := m["model"].(string)
	recs, hasRecs := m["records"].([]interface{})
	_, hasCount := m["count"]
	if strings.TrimSpace(model) == "" || (!hasRecs && !hasCount) {
		return "", false
	}
	key := o.resolveKey(model)
	if key == "" {
		key = model
	}
	label := o.modelLabel(key)

	total := len(recs)
	if c, ok := m["count"].(float64); ok && int(c) > total {
		total = int(c)
	}
	if total == 0 {
		return fmt.Sprintf("No encontré registros en **%s**.", label), true
	}

	links := make([]string, 0, maxQueryRecordLinks)
	for _, raw := range recs {
		if len(links) >= maxQueryRecordLinks {
			break
		}
		row, _ := raw.(map[string]interface{})
		name := rowOneLiner(row)
		if name == "" {
			continue
		}
		if id := humanScalar(row["id"]); id != "" {
			name = fmt.Sprintf("[%s](/m/%s/%s)", name, key, url.PathEscape(id))
		}
		links = append(links, name)
	}

	noun := "registros"
	if total == 1 {
		noun = "registro"
	}
	out := fmt.Sprintf("Encontré **%d** %s en **%s**", total, noun, label)
	if len(links) == 0 {
		return out + ".", true
	}
	out += ": " + strings.Join(links, ", ")
	if total > len(links) {
		out += fmt.Sprintf(" y %d más", total-len(links))
	}
	return out + ".", true
}

func firstString(m map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func humanScalar(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%g", t)
	case bool:
		if t {
			return "Sí"
		}
		return "No"
	case map[string]interface{}, []interface{}:
		return "" // never dump nested structures with %v
	default:
		s := fmt.Sprintf("%v", t)
		if looksLikeGoMapDump(s) {
			return ""
		}
		return s
	}
}

// formatMapResult formatea un mapa como respuesta legible (sin dumps Go).
func (o *AgentOrchestrator) formatMapResult(data map[string]interface{}) string {
	var sb strings.Builder

	priorityFields := []string{"value", "count", "sum", "avg", "id", "nombre", "name", "estado", "status", "state", "total", "fecha", "date", "message"}
	shown := make(map[string]bool)

	for _, field := range priorityFields {
		if val, ok := data[field]; ok && val != nil {
			hs := humanScalar(val)
			if hs == "" {
				continue
			}
			sb.WriteString(fmt.Sprintf("- **%s:** %s\n", capitalizeFirst(field), hs))
			shown[field] = true
		}
	}

	for key, val := range data {
		if shown[key] || key == "columns" || key == "fields" || key == "schema" {
			continue
		}
		switch v := val.(type) {
		case []interface{}:
			if len(v) == 0 {
				continue
			}
			// Skip schema-like arrays (column defs)
			if isSchemaRowArray(v) {
				continue
			}
			sb.WriteString(fmt.Sprintf("\n**%s:**\n", capitalizeFirst(key)))
			limit := len(v)
			if limit > 5 {
				limit = 5
			}
			for i := 0; i < limit; i++ {
				item := v[i]
				if itemMap, ok := item.(map[string]interface{}); ok {
					line := rowOneLiner(itemMap)
					if line != "" {
						sb.WriteString("  - " + line + "\n")
					}
				} else if hs := humanScalar(item); hs != "" {
					sb.WriteString("  - " + hs + "\n")
				}
			}
			if len(v) > 5 {
				sb.WriteString(fmt.Sprintf("  - … y %d más\n", len(v)-5))
			}
		case map[string]interface{}:
			// Skip nested maps (schemas, embeds)
			continue
		default:
			if hs := humanScalar(val); hs != "" {
				sb.WriteString(fmt.Sprintf("- **%s:** %s\n", capitalizeFirst(key), hs))
			}
		}
	}

	result := strings.TrimSpace(sb.String())
	if result == "" {
		return "Información procesada correctamente."
	}
	return result
}

func isSchemaRowArray(v []interface{}) bool {
	if len(v) == 0 {
		return false
	}
	row, ok := v[0].(map[string]interface{})
	if !ok {
		return false
	}
	_, hasName := row["name"]
	_, hasType := row["type"]
	_, hasColumn := row["column"]
	return hasName && (hasType || hasColumn)
}

func rowOneLiner(m map[string]interface{}) string {
	if producto, ok := m["producto"].(string); ok {
		cantidad, _ := m["cantidad"].(float64)
		precio, _ := m["precio"].(float64)
		return fmt.Sprintf("%v × %s ($%.2f c/u)", int(cantidad), producto, precio)
	}
	parts := make([]string, 0, 3)
	for _, k := range []string{"name", "nombre", "title", "label", "code", "sku", "number", "folio", "state", "status"} {
		if hs := humanScalar(m[k]); hs != "" {
			parts = append(parts, hs)
			if len(parts) >= 3 {
				break
			}
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " · ")
}

// formatArrayResult formatea un array de resultados
func (o *AgentOrchestrator) formatArrayResult(data []interface{}) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Se encontraron **%d** resultados:\n\n", len(data)))

	for i, item := range data {
		if i >= 5 {
			sb.WriteString(fmt.Sprintf("\n… y %d más", len(data)-5))
			break
		}
		if itemMap, ok := item.(map[string]interface{}); ok {
			line := rowOneLiner(itemMap)
			if line == "" {
				line = o.formatMapResult(itemMap)
			}
			sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, line))
		} else if hs := humanScalar(item); hs != "" {
			sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, hs))
		}
	}
	return strings.TrimSpace(sb.String())
}

func looksLikeGoMapDump(s string) bool {
	s = strings.TrimSpace(s)
	return strings.Contains(s, "map[") && (strings.Contains(s, "columns:") || strings.Contains(s, "map[name:") || strings.Contains(s, "]interface"))
}

// scrubGoMapDump strips accidental Go fmt dumps from user-visible text.
func scrubGoMapDump(s string) string {
	if !looksLikeGoMapDump(s) {
		return s
	}
	// Drop lines that look like Go map dumps; keep human prose.
	var keep []string
	for _, line := range strings.Split(s, "\n") {
		if looksLikeGoMapDump(line) || strings.HasPrefix(strings.TrimSpace(line), "map[") {
			continue
		}
		keep = append(keep, line)
	}
	out := strings.TrimSpace(strings.Join(keep, "\n"))
	if out == "" {
		return "Consulté el sistema. ¿Quieres que continúe con el siguiente paso?"
	}
	return out
}

// cleanToolResult limpia el resultado de herramienta para mostrar al usuario
func (o *AgentOrchestrator) cleanToolResult(content string) string {
	// Remover prefijos técnicos
	content = strings.TrimPrefix(content, "Resultado de ")

	// Si contiene "Información encontrada", extraer solo eso
	if idx := strings.Index(content, "Información encontrada"); idx >= 0 {
		content = content[idx:]
	}

	return strings.TrimSpace(content)
}

// capitalizeFirst capitaliza la primera letra
func capitalizeFirst(s string) string {
	if len(s) == 0 {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// truncateLog trunca un string para logging
func truncateLog(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// stripThinkingTags elimina los tags <think>...</think> de DeepSeek-R1
func stripThinkingTags(text string) string {
	re := regexp.MustCompile(`(?s)<think>.*?</think>`)
	text = re.ReplaceAllString(text, "")
	return strings.TrimSpace(text)
}

// CleanMarkdownForChat normalizes agent replies for GFM UIs (Aby / react-markdown).
// IMPORTANT: keep **bold** — converting ** → * (WhatsApp style) makes GFM render italics.
// WhatsApp outbound must call FormatMarkdownForWhatsApp separately.
func CleanMarkdownForChat(text string) string {
	// Drop table separator rows if the LLM ignored "no tables" guidance.
	tableSeparatorRegex := regexp.MustCompile(`(?m)^[|:\-\s]+$`)
	text = tableSeparatorRegex.ReplaceAllString(text, "")

	lines := strings.Split(text, "\n")
	cleaned := []string{}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			// Convert header to GFM bold (not single-asterisk italics).
			headerText := strings.TrimLeft(trimmed, "# ")
			if headerText != "" {
				cleaned = append(cleaned, "**"+headerText+"**")
			}
			continue
		}
		if strings.HasPrefix(trimmed, "|") && strings.HasSuffix(trimmed, "|") {
			line = strings.Trim(line, "|")
			line = strings.ReplaceAll(line, " | ", " • ")
		}

		cleaned = append(cleaned, line)
	}

	result := strings.Join(cleaned, "\n")

	multiNewlineRegex := regexp.MustCompile(`\n{3,}`)
	result = multiNewlineRegex.ReplaceAllString(result, "\n\n")

	return strings.TrimSpace(result)
}

// FormatMarkdownForWhatsApp converts GFM **bold** to WhatsApp *bold*.
func FormatMarkdownForWhatsApp(text string) string {
	text = regexp.MustCompile(`\*\*([^*]+)\*\*`).ReplaceAllString(text, "*$1*")
	return text
}

// extractMessageFromJSON intenta extraer un campo "message" de un JSON
func extractMessageFromJSON(text string) string {
	text = strings.TrimSpace(text)

	var data map[string]interface{}
	if err := json.Unmarshal([]byte(text), &data); err == nil {
		if msg, ok := data["message"].(string); ok {
			log.Printf("🔍 Extracted message from JSON response")
			return msg
		}
	}
	return text
}

// CleanAgentResponse limpia la respuesta del agente para UIs con Markdown GFM
// (Aby / react-markdown). NO convierte ** → * — eso era para WhatsApp y rompía
// la negrita. WhatsApp outbound usa FormatMarkdownForWhatsApp al enviar.
func CleanAgentResponse(text string) string {
	if text == "" {
		return text
	}
	extracted := extractMessageFromJSON(text)
	if extracted != text {
		text = extracted
	}
	text = stripThinkingTags(text)
	text = scrubGoMapDump(text)
	return strings.TrimSpace(text)
}

// ParseToolCallsFromJSON parsea tool calls desde JSON
func ParseToolCallsFromJSON(data string) ([]tools.ToolCall, error) {
	var calls []tools.ToolCall
	if err := json.Unmarshal([]byte(data), &calls); err != nil {
		return nil, err
	}
	return calls, nil
}

// getMapKeys retorna las keys de un mapa para debugging
func getMapKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// describeToolCall generates a human-readable description + icon of what the AI is doing.
// Returns "icon|message" format. Frontend splits on "|" to render icon + text separately.
// Icons prefer the module's nav/metadata lucide slug (e.g. shopping-cart for ventas).
func (o *AgentOrchestrator) describeToolCall(toolName string, params map[string]interface{}) string {
	model, _ := params["model"].(string)
	query, _ := params["query"].(string)
	modelLabel := o.modelLabel(model)
	modelIcon := o.modelIcon(model)

	switch toolName {
	case "list_models":
		return "layout-grid|Explorando módulos disponibles"
	case "describe_model":
		return modelIcon + "|Analizando estructura de " + modelLabel
	case "query_data":
		if search, ok := params["search"].(string); ok && search != "" {
			return modelIcon + "|Buscando en " + modelLabel + ": \"" + search + "\""
		}
		return modelIcon + "|Consultando " + modelLabel
	case "vector_search":
		return modelIcon + "|Búsqueda semántica en " + modelLabel + ": \"" + query + "\""
	case "aggregate_data":
		fn, _ := params["function"].(string)
		labels := map[string]string{"count": "conteo", "sum": "suma", "avg": "promedio", "min": "mínimo", "max": "máximo"}
		if l, ok := labels[fn]; ok {
			fn = l
		}
		return modelIcon + "|Calculando " + fn + " en " + modelLabel
	case "create_record":
		return modelIcon + "|Creando registro en " + modelLabel
	case "update_record":
		return modelIcon + "|Actualizando registro en " + modelLabel
	case "delete_record":
		return modelIcon + "|Eliminando registro de " + modelLabel
	case "search_knowledge":
		return "book-open|Buscando en base de conocimiento"
	case "search_products":
		return "package-search|Buscando productos"
	case "final_answer":
		return "message-square|Preparando respuesta"
	case "calculator":
		return "calculator|Calculando"
	case "send_image":
		return "image|Enviando imagen"
	case "send_audio":
		return "volume-2|Generando audio"
	case "handoff_to_human":
		return "user|Transfiriendo a humano"
	case "update_contact_info":
		return "contact|Actualizando información de contacto"
	case "get_conversation_context":
		return "history|Revisando contexto"
	default:
		return "cog|Ejecutando " + toolName
	}
}

func humanToolName(toolName string) string {
	labels := map[string]string{
		"list_models":              "Explorar módulos",
		"describe_model":           "Estructura del modelo",
		"query_data":               "Consulta",
		"vector_search":            "Búsqueda semántica",
		"aggregate_data":           "Agregación",
		"create_record":            "Crear registro",
		"start_guide":              "Guía en pantalla",
		"update_record":            "Actualizar registro",
		"delete_record":            "Eliminar registro",
		"search_knowledge":         "Base de conocimiento",
		"search_products":          "Productos",
		"final_answer":             "Respuesta",
		"calculator":               "Calculadora",
		"get_conversation_context": "Contexto",
	}
	if l, ok := labels[toolName]; ok {
		return l
	}
	return toolName
}

// toolResultLabel builds the trail line after a tool finishes, with translated model + icon.
func (o *AgentOrchestrator) toolResultLabel(toolName string, params map[string]interface{}, success bool) string {
	status := "listo"
	if !success {
		status = "error"
	}
	model, _ := params["model"].(string)
	base := humanToolName(toolName)
	icon := "cog"
	if model != "" {
		label := o.modelLabel(model)
		icon = o.modelIcon(model)
		switch toolName {
		case "describe_model":
			base = "Estructura de " + label
		case "query_data", "vector_search", "aggregate_data":
			base = base + " · " + label
		case "create_record", "update_record", "delete_record":
			base = base + " · " + label
		}
	} else if toolName == "list_models" {
		icon = "layout-grid"
	}
	return icon + "|" + base + " — " + status
}

func lastUserContent(messages []map[string]interface{}) string {
	return lastRealUserContent(messages)
}

// unwrapSyntheticFinalAnswer turns a final_answer back into plain text only when
// publishing it would be wrong: the model narrated an action, or it stated org
// facts with no tool evidence. A finished explanation is the answer.
func unwrapSyntheticFinalAnswer(aiResp *AIResponse, messages []map[string]interface{}) *AIResponse {
	if aiResp == nil || len(aiResp.ToolCalls) != 1 {
		return aiResp
	}
	tc := aiResp.ToolCalls[0]
	if tc.ToolName != "final_answer" {
		return aiResp
	}
	msg, _ := tc.Parameters["message"].(string)
	if msg == "" {
		msg = aiResp.Message
	}
	// Always unwrap deferred narration — prior tools must not lock in "voy a…".
	if isDeferredNarration(msg) {
		log.Printf("⚠️ Unwrapping deferred final_answer (id=%s) to keep agent loop running", tc.ID)
		aiResp.Message = msg
		aiResp.ToolCalls = nil
		return aiResp
	}
	if hasToolRoleMessages(messages) || isChitchat(lastRealUserContent(messages)) {
		return aiResp
	}
	if !claimsUngroundedOrgData(msg) {
		return aiResp
	}
	// Invented count/mutation, or the same claim repeated after the one nudge.
	log.Printf("⚠️ Unwrapping ungrounded final_answer (id=%s) — no tool evidence", tc.ID)
	aiResp.Message = msg
	aiResp.ToolCalls = nil
	return aiResp
}

func orchestratorNudge(deferred, hasToolResults bool) string {
	if !deferred {
		return "[sistema] No afirmes cifras ni registros de la organización sin un resultado de herramienta. " +
			"Si hace falta un dato, llama la herramienta ahora. Si la pregunta no es de datos, responde sin inventar números."
	}
	nudge := "[sistema] Esa frase es intención, no una respuesta. Llama la herramienta ahora, sin narrar el siguiente paso. "
	if hasToolResults {
		nudge += "Ya hay resultados: usa query_data, aggregate_data o create_record con models[].model exacto " +
			"(ej. customers, invoices). Nunca uses el title i18n como model."
	} else {
		nudge += "Empieza con list_models o describe_model."
	}
	return nudge
}

var (
	orgCountForward = regexp.MustCompile(`(?i)\b\d{1,6}\b.{0,48}\b(?:sucursales|sucursal|clientes|cliente|facturas|factura|ventas|venta|productos|producto|pedidos|pedido|registros|registro|ordenes|órdenes|orden|almacenes|almacén|almacen|empleados|empleado|cotizaciones|cotizacion|cotización)\b`)
	orgCountReverse = regexp.MustCompile(`(?i)\b(?:tienes|tenemos|hay|existen|encontr[eé]|el total|en total|suman)\b.{0,24}\b(?:\d{1,6}|un|una|dos|tres|cuatro|cinco|seis|siete|ocho|nueve|diez)\b`)
	orgMutation     = regexp.MustCompile(`(?i)\b(?:he creado|acabo de crear|ya cre[eé]|cre[eé] (?:el|la|un|una|los|las)|actualic[eé]|elimin[eé]|registr[eé] (?:el|la|un|una)|borr[eé] (?:el|la|un|una)|di de alta|di de baja)\b`)
)

// claimsUngroundedOrgData is true when the reply states a count or a mutation
// the tools have not produced. Explanations that merely mention the entity
// ("puedo ayudarte con sucursales") are not claims.
func claimsUngroundedOrgData(msg string) bool {
	m := strings.TrimSpace(msg)
	if m == "" || isDeferredNarration(m) {
		return false
	}
	return orgCountForward.MatchString(m) || orgCountReverse.MatchString(m) || orgMutation.MatchString(m)
}

// lastRealUserContent returns the latest real user turn, skipping internal
// "[sistema]…" nudges we inject to force tool use.
func lastRealUserContent(messages []map[string]interface{}) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i]["role"] != "user" {
			continue
		}
		s, ok := messages[i]["content"].(string)
		if !ok {
			continue
		}
		s = strings.TrimSpace(s)
		if s == "" || strings.HasPrefix(s, "[sistema]") {
			continue
		}
		return s
	}
	return ""
}

func hasToolRoleMessages(messages []map[string]interface{}) bool {
	for _, msg := range messages {
		if msg["role"] == "tool" {
			return true
		}
	}
	return false
}

func hasSystemNudge(messages []map[string]interface{}) bool {
	for _, msg := range messages {
		if msg["role"] != "user" {
			continue
		}
		s, _ := msg["content"].(string)
		if strings.HasPrefix(strings.TrimSpace(s), "[sistema]") {
			return true
		}
	}
	return false
}

// isDeferredNarration detects "I'll do X / let me check…" replies that must
// never be shown as the final user-facing answer without tool results.
// Includes present-tense announcements like "Ahora creo el cliente…" that the
// model emits right before (or instead of) create_record — those used to slip
// past as final_answer after describe_model and abort the loop.
//
// Only the opening is read: narration announces the next step up front. A
// finished answer can say "antes de que se guarde" or "primero abre…" deep in
// its body (the greeting that listed capabilities matched "antes de " and was
// replaced by the generic failure). A reply that ends by asking the user
// something hands the turn back — it is an answer, never pending work.
func isDeferredNarration(msg string) bool {
	m := strings.ToLower(strings.TrimSpace(msg))
	if m == "" {
		return false
	}
	if strings.HasSuffix(strings.TrimRight(m, " *_)\"'»"), "?") {
		return false
	}
	m = narrationHead(m)
	needles := []string{
		"voy a ", "ahora voy", "ahora revis", "déjame ", "dejame ", "primero ",
		"revisaré", "revisare", "me pongo a", "procedo a",
		"déjame revisar", "dejame revisar", "voy a crear", "voy a buscar",
		"voy a consultar", "voy a describir", "voy a listar", "voy a obtener",
		"necesito revisar", "antes de ", "un momento",
		"déjame ver", "dejame ver", "voy a checar", "voy a comprobar",
		"tienes razón", "perfecto, voy", "ok, voy",
		// Present tense / mid-action (avoid bare "creo" → "creo que…")
		"ahora creo el", "ahora creo la", "ahora creo un", "ahora creo una",
		"ahora te creo", "ahora lo creo", "ahora la creo",
		"creo el ", "creo la ", "creo un ", "creo una ",
		"crearé el", "creare el", "crearé la", "creare la",
		"crearé un", "creare un", "crearé una", "creare una",
		"estoy creando", "paso a crear", "paso a registrar",
		"ahora registro", "ahora actualizo", "ahora elimino", "ahora borro",
		"ahora busco", "ahora consulto", "ahora listo", "ahora obtengo",
	}
	for _, n := range needles {
		if strings.Contains(m, n) {
			return true
		}
	}
	return false
}

// narrationHeadRunes bounds how much of the opening line isDeferredNarration
// reads; announcements ("Perfecto, voy a crear el cliente **Rodrigo**.") fit.
const narrationHeadRunes = 200

// narrationHead is the first non-empty line of msg, capped at narrationHeadRunes.
func narrationHead(msg string) string {
	for _, line := range strings.Split(msg, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			if r := []rune(line); len(r) > narrationHeadRunes {
				return string(r[:narrationHeadRunes])
			}
			return line
		}
	}
	return ""
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

// isChitchat detects greetings / short ack so we don't force tools on "hola".
func isChitchat(msg string) bool {
	m := strings.ToLower(strings.TrimSpace(msg))
	if m == "" || strings.HasPrefix(m, "[sistema]") {
		return true
	}
	// Strip punctuation for matching
	m = strings.Map(func(r rune) rune {
		if strings.ContainsRune("¡!¿?.,;:", r) {
			return -1
		}
		return r
	}, m)
	m = strings.TrimSpace(m)
	shorts := []string{
		"hola", "hi", "hello", "hey", "buenas", "buen dia", "buen día",
		"buenos dias", "buenos días", "buenas tardes", "buenas noches",
		"gracias", "thanks", "ok", "okay", "vale", "listo", "perfecto",
		"si", "sí", "no", "adios", "adiós", "bye", "saludos",
	}
	for _, c := range shorts {
		if m == c {
			return true
		}
	}
	return len([]rune(m)) <= 12 && !strings.ContainsAny(m, "0123456789") &&
		!strings.Contains(m, "crea") && !strings.Contains(m, "cuant") &&
		!strings.Contains(m, "busca") && !strings.Contains(m, "venta") &&
		!strings.Contains(m, "cliente")
}

func (o *AgentOrchestrator) resolveKey(model string) string {
	switch {
	case o.catalog != nil:
		return ResolveModelKey(o.catalog, model)
	case o.models != nil:
		return o.models.ResolveModelKey(model)
	}
	return model
}
