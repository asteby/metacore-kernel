package agent

import (
	"context"
	"fmt"
	"log"

	"github.com/asteby/metacore-kernel/agent/contextmgr"

	"github.com/google/uuid"
)

// SpeechSynthesizer turns text into a playable audio URL (the send_audio
// tool's port). Implemented by the host over its TTS provider.
type SpeechSynthesizer interface {
	TextToSpeech(ctx context.Context, text string) (audioURL string, err error)
}

// ===== HERRAMIENTAS ESPECÍFICAS =====

// SearchKnowledgeTool busca en la base de conocimiento
type SearchKnowledgeTool struct {
	knowledgeService KnowledgeService
}

type KnowledgeService interface {
	SearchSimple(ctx context.Context, query string, agentID uuid.UUID, maxResults int, filters map[string]interface{}) (interface{}, error)
}

func NewSearchKnowledgeTool(ks KnowledgeService) *SearchKnowledgeTool {
	return &SearchKnowledgeTool{knowledgeService: ks}
}

func (t *SearchKnowledgeTool) Name() string {
	return "search_knowledge"
}

func (t *SearchKnowledgeTool) Category() string {
	return "knowledge" // Categoría: knowledge, communication, context, workflow
}

func (t *SearchKnowledgeTool) Description() string {
	return "Busca información en la base de conocimiento sobre productos, servicios, precios, etc. Usa esta herramienta cuando necesites información específica para responder al usuario."
}

func (t *SearchKnowledgeTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"query": map[string]interface{}{
			"type":        "string",
			"description": "La consulta de búsqueda. Sé específico sobre qué información necesitas.",
			"required":    true,
		},
		"max_results": map[string]interface{}{
			"type":        "integer",
			"description": "Número máximo de resultados (default: 3)",
			"required":    false,
		},
	}
}

func (t *SearchKnowledgeTool) Execute(ctx context.Context, params map[string]interface{}, agentCtx *contextmgr.ConversationContext) (interface{}, error) {
	query, ok := params["query"].(string)
	if !ok || query == "" {
		return nil, fmt.Errorf("query parameter is required")
	}

	maxResults := 5
	if mr, ok := params["max_results"].(float64); ok {
		maxResults = int(mr)
	}

	// Extraer filtros adicionales de los parámetros
	filters := make(map[string]interface{})
	for key, value := range params {
		if key != "query" && key != "max_results" {
			filters[key] = value
		}
	}

	result, err := t.knowledgeService.SearchSimple(ctx, query, agentCtx.AgentID, maxResults, filters)
	if err != nil {
		return nil, err
	}

	// Si el resultado es un KnowledgeResult, formatearlo
	if kr, ok := result.(*KnowledgeResult); ok {
		return kr.FormatForAgent(), nil
	}

	return result, nil
}

// GetConversationContextTool obtiene el contexto de la conversación
type GetConversationContextTool struct{}

func NewGetConversationContextTool() *GetConversationContextTool {
	return &GetConversationContextTool{}
}

func (t *GetConversationContextTool) Name() string {
	return "get_conversation_context"
}

func (t *GetConversationContextTool) Category() string {
	return "context"
}

func (t *GetConversationContextTool) Description() string {
	return "Obtiene el contexto de la conversación actual: imágenes ya enviadas, temas mencionados, preferencias del usuario. Usa esto para evitar repetir información."
}

func (t *GetConversationContextTool) Parameters() map[string]interface{} {
	return map[string]interface{}{} // No requiere parámetros
}

func (t *GetConversationContextTool) Execute(ctx context.Context, params map[string]interface{}, agentCtx *contextmgr.ConversationContext) (interface{}, error) {
	return map[string]interface{}{
		"sent_images":       agentCtx.SentImages,
		"mentioned_topics":  agentCtx.MentionedTopics,
		"sent_products":     agentCtx.SentProducts,
		"message_count":     agentCtx.MessageCount,
		"conversation_flow": agentCtx.ConversationFlow,
	}, nil
}

// SendImageTool marca una imagen para ser enviada
type SendImageTool struct{}

func NewSendImageTool() *SendImageTool {
	return &SendImageTool{}
}

func (t *SendImageTool) Name() string {
	return "send_image"
}

func (t *SendImageTool) Category() string {
	return "communication"
}

func (t *SendImageTool) Description() string {
	return "Marca una imagen para ser enviada al usuario. Usa esto cuando encuentres imágenes relevantes en la base de conocimiento y quieras mostrarlas."
}

func (t *SendImageTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"image_url": map[string]interface{}{
			"type":        "string",
			"description": "URL de la imagen a enviar (ej: /storage/knowledge/img.jpg)",
			"required":    true,
		},
		"caption": map[string]interface{}{
			"type":        "string",
			"description": "Texto descriptivo para la imagen (opcional)",
			"required":    false,
		},
	}
}

func (t *SendImageTool) Execute(ctx context.Context, params map[string]interface{}, agentCtx *contextmgr.ConversationContext) (interface{}, error) {
	imageURL, ok := params["image_url"].(string)
	if !ok || imageURL == "" {
		return nil, fmt.Errorf("image_url parameter is required")
	}

	caption := ""
	if c, ok := params["caption"].(string); ok {
		caption = c
	}

	// Verificar si ya fue enviada
	// if agentCtx.HasSentImage(imageURL) {
	// 	return map[string]interface{}{
	// 		"status":  "skipped",
	// 		"message": "Esta imagen ya fue enviada anteriormente",
	// 	}, nil
	// }

	// Marcar como enviada
	agentCtx.AddSentImage(imageURL)

	return map[string]interface{}{
		"status":    "queued",
		"image_url": imageURL,
		"caption":   caption,
		"message":   "Imagen marcada para envío",
	}, nil
}

// HandoffToHumanTool transfiere la conversación a un humano
type HandoffToHumanTool struct{}

func NewHandoffToHumanTool() *HandoffToHumanTool {
	return &HandoffToHumanTool{}
}

func (t *HandoffToHumanTool) Name() string {
	return "handoff_to_human"
}

func (t *HandoffToHumanTool) Category() string {
	return "workflow"
}

func (t *HandoffToHumanTool) Description() string {
	return "Transfiere la conversación a un agente humano. Usa esto cuando: 1) El usuario lo solicite explícitamente, 2) No puedas resolver la consulta, 3) La situación requiera atención humana (quejas, problemas complejos)."
}

func (t *HandoffToHumanTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"reason": map[string]interface{}{
			"type":        "string",
			"description": "Razón del traspaso (para contexto del agente humano)",
			"required":    true,
		},
	}
}

func (t *HandoffToHumanTool) Execute(ctx context.Context, params map[string]interface{}, agentCtx *contextmgr.ConversationContext) (interface{}, error) {
	reason, ok := params["reason"].(string)
	if !ok || reason == "" {
		reason = "Usuario solicitó atención humana"
	}

	return map[string]interface{}{
		"status":  "handoff_initiated",
		"reason":  reason,
		"message": "La conversación será transferida a un agente humano",
		"action":  "DISABLE_BOT", // Tag especial para desactivar bot
	}, nil
}

// FinalAnswerTool proporciona la respuesta final al usuario
type FinalAnswerTool struct{}

func NewFinalAnswerTool() *FinalAnswerTool {
	return &FinalAnswerTool{}
}

func (t *FinalAnswerTool) Name() string {
	return "final_answer"
}

func (t *FinalAnswerTool) Category() string {
	return "communication"
}

func (t *FinalAnswerTool) Description() string {
	return "Proporciona la respuesta final al usuario. NUNCA uses tablas ni # encabezados. Usa un formato de chat limpio: *negrita* para resaltar, _cursiva_ y • para listas."
}

func (t *FinalAnswerTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"message": map[string]interface{}{
			"type":        "string",
			"description": "El mensaje final para el usuario. Usa formato limpio: *negrita*, _cursiva_, • listas",
			"required":    true,
		},
	}
}

func (t *FinalAnswerTool) Execute(ctx context.Context, params map[string]interface{}, agentCtx *contextmgr.ConversationContext) (interface{}, error) {
	message, ok := params["message"].(string)
	if !ok || message == "" {
		return nil, fmt.Errorf("message parameter is required")
	}

	return map[string]interface{}{
		"status":  "complete",
		"message": message,
	}, nil
}

// SendAudioTool genera un audio TTS y lo marca para enviar
type SendAudioTool struct {
	speech SpeechSynthesizer
}

func NewSendAudioTool(speech SpeechSynthesizer) *SendAudioTool {
	return &SendAudioTool{speech: speech}
}

func (t *SendAudioTool) Name() string {
	return "send_audio"
}

func (t *SendAudioTool) Category() string {
	return "communication"
}

func (t *SendAudioTool) Description() string {
	return "Convierte un texto corto en un mensaje de voz (audio) y lo envía al usuario. Útil para dar un toque humano o cuando el usuario solicita específicamente un audio/nota de voz."
}

func (t *SendAudioTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"text": map[string]interface{}{
			"type":        "string",
			"description": "El texto que será convertido a audio (mensaje de voz). Sé breve y natural.",
			"required":    true,
		},
	}
}

func (t *SendAudioTool) Execute(ctx context.Context, params map[string]interface{}, agentCtx *contextmgr.ConversationContext) (interface{}, error) {
	text, ok := params["text"].(string)
	if !ok || text == "" {
		return nil, fmt.Errorf("text parameter is required")
	}

	// Generar audio via Deepgram
	audioURL, err := t.speech.TextToSpeech(ctx, text)
	if err != nil {
		log.Printf("❌ SendAudioTool: Error generating TTS: %v", err)
		return nil, fmt.Errorf("failed to generate audio: %w", err)
	}

	return map[string]interface{}{
		"status":    "queued",
		"audio_url": audioURL,
		"text":      text,
		"message":   "Audio generado y marcado para envío",
	}, nil
}
