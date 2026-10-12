package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/asteby/metacore-kernel/agent/tools"
	"log"
)

// AIServiceAdapter adapts a ChatModel (OpenAI-shaped function calling) to the
// AIService port the orchestrator loop consumes.
type AIServiceAdapter struct {
	model ChatModel
}

// NewAIServiceAdapter wraps a ChatModel.
func NewAIServiceAdapter(model ChatModel) *AIServiceAdapter {
	return &AIServiceAdapter{model: model}
}

// GenerateChatWithTools genera una respuesta con soporte para tools usando function calling nativo
func (a *AIServiceAdapter) GenerateChatWithTools(ctx context.Context, messages []map[string]interface{}, systemPrompt string, tools []map[string]interface{}) (*AIResponse, error) {
	// Usar function calling nativo del AIService
	result, err := a.model.ChatWithFunctions(ctx, messages, systemPrompt, tools)
	if err != nil {
		return nil, err
	}

	// Parsear respuesta en formato OpenAI
	log.Printf("🔍 DEBUG: Result keys: %v", getMapKeys(result))
	aiResp := a.parseOpenAIResponse(result)
	log.Printf("🔍 DEBUG: aiResp.ToolCalls count: %d", len(aiResp.ToolCalls))
	for i, tc := range aiResp.ToolCalls {
		log.Printf("🔍 DEBUG: aiResp.ToolCalls[%d]: id=%s, name=%s", i, tc.ID, tc.ToolName)
	}

	return aiResp, nil
}

// buildToolInstructions construye instrucciones sobre cómo usar los tools
func (a *AIServiceAdapter) buildToolInstructions(tools []map[string]interface{}) string {
	instructions := `
## CÓMO USAR HERRAMIENTAS

Para usar una herramienta, responde EXACTAMENTE en este formato JSON:

{
  "tool_calls": [
    {
      "tool_name": "nombre_de_la_herramienta",
      "parameters": {
        "param1": "valor1",
        "param2": "valor2"
      }
    }
  ]
}

IMPORTANTE:
- Puedes llamar múltiples herramientas en un solo turno
- Después de recibir los resultados, puedes llamar más herramientas si necesitas
- **REGLA DE IMÁGENES**: Si search_knowledge retorna imágenes (file_url), DEBES llamar send_image para CADA imagen
- Cuando tengas toda la información, usa final_answer para responder al usuario
- NO respondas directamente al usuario sin usar final_answer
- NO digas "aquí tienes una imagen" - solo usa send_image y la imagen se enviará automáticamente

EJEMPLO DE USO CORRECTO CON IMÁGENES:
1. Usuario pregunta: "Tienes imágenes?"
2. Llamas: search_knowledge con query relevante
3. Resultado incluye: file_url: "/storage/knowledge/imagen.jpg"
4. DEBES llamar: send_image con image_url="/storage/knowledge/imagen.jpg" y caption="Descripción"
5. Luego llamas: final_answer con tu respuesta de texto

HERRAMIENTAS DISPONIBLES:
`

	for _, tool := range tools {
		name, _ := tool["name"].(string)
		desc, _ := tool["description"].(string)
		params, _ := tool["parameters"].(map[string]interface{})

		instructions += fmt.Sprintf("\n### %s\n%s\n", name, desc)

		if props, ok := params["properties"].(map[string]interface{}); ok {
			instructions += "Parámetros:\n"
			for paramName, paramSchema := range props {
				if schemaMap, ok := paramSchema.(map[string]interface{}); ok {
					paramDesc, _ := schemaMap["description"].(string)
					paramType, _ := schemaMap["type"].(string)
					instructions += fmt.Sprintf("  - %s (%s): %s\n", paramName, paramType, paramDesc)
				}
			}
		}
	}

	instructions += "\n\n**RECUERDA**: Si search_knowledge retorna file_url, USA send_image para cada imagen antes de final_answer."

	return instructions
}

// parseOpenAIResponse parsea la respuesta en formato OpenAI (nativo de Groq/Gemini convertido)
func (a *AIServiceAdapter) parseOpenAIResponse(result map[string]interface{}) *AIResponse {
	aiResp := &AIResponse{
		Message:   "",
		ToolCalls: []tools.ToolCall{},
		Finished:  false,
	}

	choices, ok := result["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		log.Printf("⚠️ No choices in response")
		return aiResp
	}

	choice, ok := choices[0].(map[string]interface{})
	if !ok {
		log.Printf("⚠️ Malformed choice")
		return aiResp
	}
	message, ok := choice["message"].(map[string]interface{})
	if !ok {
		log.Printf("⚠️ No message in choice")
		return aiResp
	}

	// Extraer contenido de texto
	if content, ok := message["content"].(string); ok {
		aiResp.Message = content
	}

	// Extraer tool calls
	tcRaw, tcOk := message["tool_calls"]
	log.Printf("🔧 DEBUG: tool_calls exists=%v, type=%T, value=%v", tcOk, tcRaw, tcRaw)

	// Helper function para procesar tool calls de un slice
	processToolCalls := func(toolCallsRaw []interface{}) {
		for i, tcRaw := range toolCallsRaw {
			tc, ok := tcRaw.(map[string]interface{})
			if !ok {
				log.Printf("⚠️ Cannot cast tool call %d to map[string]interface{}", i)
				continue
			}

			id, _ := tc["id"].(string)
			tcType, _ := tc["type"].(string)

			if tcType == "function" {
				fnData, _ := tc["function"].(map[string]interface{})
				name, _ := fnData["name"].(string)
				argsJSON, _ := fnData["arguments"].(string)

				log.Printf("🔧 ToolCall raw[%d]: id=%q, name=%s, args=%s", i, id, name, argsJSON)

				// Parse arguments JSON - handle escaped unicode
				var params map[string]interface{}
				// First, try to unescape any double-escaped JSON
				unescapedJSON := unescapeJSONString(argsJSON)
				if err := json.Unmarshal([]byte(unescapedJSON), &params); err != nil {
					// Fallback to direct unmarshal
					if err2 := json.Unmarshal([]byte(argsJSON), &params); err2 != nil {
						log.Printf("⚠️ Failed to parse tool arguments: %v (original: %s)", err2, argsJSON)
						continue
					}
				}

				log.Printf("🔧 ToolCall parsed: name=%s, id=%q, params=%+v", name, id, params)

				// Generate ID if empty (some LLMs don't provide it)
				if id == "" {
					id = fmt.Sprintf("call_%s_%d", name, i)
					log.Printf("🔧 Generated missing ID: %s", id)
				}

				aiResp.ToolCalls = append(aiResp.ToolCalls, tools.ToolCall{
					ID:         id,
					ToolName:   name,
					Parameters: params,
				})
			}
		}
		log.Printf("🔧 Detected %d tool calls (native function calling)", len(aiResp.ToolCalls))
	}

	// Try []interface{} first (OpenAI format)
	if toolCallsRaw, ok := tcRaw.([]interface{}); ok && len(toolCallsRaw) > 0 {
		log.Printf("🔧 Found %d tool calls in response (type: []interface{})", len(toolCallsRaw))
		processToolCalls(toolCallsRaw)
	} else if toolCallsMap, ok := tcRaw.([]map[string]interface{}); ok && len(toolCallsMap) > 0 {
		// Also handle []map[string]interface{} (from some LangChain providers)
		log.Printf("🔧 Found %d tool calls in response (type: []map[string]interface{})", len(toolCallsMap))
		// Convert to []interface{} for processing
		toolCallsInterface := make([]interface{}, len(toolCallsMap))
		for i, tc := range toolCallsMap {
			toolCallsInterface[i] = tc
		}
		processToolCalls(toolCallsInterface)
	} else {
		// No native tool calls — plain text reply.
		log.Printf("🔧 DEBUG: tool_calls not slice type, type=%T", tcRaw)
		if aiResp.Message != "" {
			// Do NOT wrap deferred narration ("voy a…", "déjame…") as final_answer:
			// that made the orchestrator think tools ran and skip the force-tools nudge,
			// so Aby published thinking as the user-visible reply.
			if isDeferredNarration(aiResp.Message) {
				log.Printf("💬 Deferred narration without tools — leaving ToolCalls empty for orchestrator nudge")
			} else {
				log.Printf("💬 Direct response (no tool calls) — wrapping as final_answer")
				aiResp.ToolCalls = append(aiResp.ToolCalls, tools.ToolCall{
					ID:       "call_auto_final",
					ToolName: "final_answer",
					Parameters: map[string]interface{}{
						"message": aiResp.Message,
					},
				})
			}
		}
	}

	return aiResp
}

// parseResponse parsea la respuesta del LLM para detectar tool calls (FALLBACK - legacy)
func (a *AIServiceAdapter) parseResponse(responseText string) *AIResponse {
	aiResp := &AIResponse{
		Message:   responseText,
		ToolCalls: []tools.ToolCall{},
		Finished:  false,
	}

	// Intentar parsear como JSON con tool_calls
	var parsed struct {
		ToolCalls []struct {
			ToolName   string                 `json:"tool_name"`
			Parameters map[string]interface{} `json:"parameters"`
		} `json:"tool_calls"`
	}

	if err := json.Unmarshal([]byte(responseText), &parsed); err == nil && len(parsed.ToolCalls) > 0 {
		// Es una llamada a herramientas
		for i, tc := range parsed.ToolCalls {
			aiResp.ToolCalls = append(aiResp.ToolCalls, tools.ToolCall{
				ID:         fmt.Sprintf("call_%d", i),
				ToolName:   tc.ToolName,
				Parameters: tc.Parameters,
			})
		}
		log.Printf("🔧 Detected %d tool calls (legacy JSON parsing)", len(aiResp.ToolCalls))
	} else {
		// Es una respuesta directa (sin herramientas)
		// El LLM respondió sin usar tools - forzar final_answer
		log.Printf("⚠️ LLM responded without tools, wrapping in final_answer")
		aiResp.ToolCalls = append(aiResp.ToolCalls, tools.ToolCall{
			ID:       "call_auto_final",
			ToolName: "final_answer",
			Parameters: map[string]interface{}{
				"message": responseText,
			},
		})
	}

	return aiResp
}

// unescapeJSONString decodifica un string JSON que puede tener escapes Unicode doblemente codificados
func unescapeJSONString(s string) string {
	// First try to unmarshal as a simple JSON string to unescape unicode
	var result string
	if err := json.Unmarshal([]byte(s), &result); err == nil {
		return result
	}
	// If that fails, return original
	return s
}
