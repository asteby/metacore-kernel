package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/asteby/metacore-kernel/agent/contextmgr"
)

// ToolType is the kind of integration a ToolDefinition describes.
type ToolType string

// Tool types.
const (
	ToolTypeWebhook ToolType = "webhook"
	ToolTypeMCP     ToolType = "mcp"
	ToolTypeAPI     ToolType = "api"
	ToolTypeQuery   ToolType = "query"
)

// ToolInputParam defines one input parameter the model must extract from the
// conversation.
type ToolInputParam struct {
	Name           string `json:"name"`            // variable name (e.g. "rfc")
	Type           string `json:"type"`            // string, number, date, boolean, email, phone
	Description    string `json:"description"`     // what the model should look for
	Required       bool   `json:"required"`        // whether the parameter is required
	Example        string `json:"example"`         // example value
	ExtractionHint string `json:"extraction_hint"` // how to extract the value
	DefaultValue   string `json:"default_value"`   // used when not provided
	Validation     string `json:"validation"`      // regex or validation rule
	Normalize      string `json:"normalize"`       // uppercase, lowercase, order_id, alphanumeric, trim
	FormatPattern  string `json:"format_pattern"`  // e.g. "XXX-0000-XXXX"
}

// ToolDefinition is the host-neutral description of a tenant-configured tool
// (a webhook or API the agent may call). Hosts persist these in their own
// tables and hand them to NewDynamicTool.
type ToolDefinition struct {
	Name        string
	Description string
	Type        ToolType
	Category    string

	Endpoint   string
	Method     string
	Headers    map[string]interface{}
	AuthType   string // none, bearer, api_key, basic
	AuthConfig map[string]interface{}

	// Parameters is the body template for requests.
	Parameters      map[string]interface{}
	ResponseMapping map[string]interface{}
	InputSchema     []ToolInputParam
	BodyTemplate    string // JSON template with {{variables}}

	Timeout    int // seconds
	RetryCount int
}

// DynamicTool adapts a ToolDefinition to the BaseTool interface. HTTP calls go
// through the egress guard, so a tenant cannot aim a tool at internal addresses.
type DynamicTool struct {
	tool *ToolDefinition
}

// NewDynamicTool wraps a definition as a tool.
func NewDynamicTool(tool *ToolDefinition) *DynamicTool {
	cp := *tool
	if cp.Method == "" {
		cp.Method = http.MethodPost
	}
	return &DynamicTool{tool: &cp}
}

func (t *DynamicTool) Name() string {
	return sanitizeToolName(t.tool.Name)
}

// sanitizeToolName convierte el nombre a formato válido para LLMs (sin espacios, alfanumérico)
func sanitizeToolName(name string) string {
	// Convertir a minúsculas y reemplazar espacios por guiones bajos
	name = strings.ToLower(name)
	name = strings.ReplaceAll(name, " ", "_")

	// Remover caracteres no válidos (solo permitir a-z, 0-9, _, -, ., :)
	re := regexp.MustCompile(`[^a-z0-9_\-\.\:]`)
	name = re.ReplaceAllString(name, "")

	// Asegurar que empieza con letra o guión bajo
	if len(name) > 0 && !regexp.MustCompile(`^[a-z_]`).MatchString(name) {
		name = "_" + name
	}

	// Limitar a 64 caracteres
	if len(name) > 64 {
		name = name[:64]
	}

	return name
}

func (t *DynamicTool) Description() string {
	desc := t.tool.Description

	// Enriquecer descripción con info de parámetros requeridos para la IA
	if len(t.tool.InputSchema) > 0 {
		requiredParams := []string{}
		for _, p := range t.tool.InputSchema {
			if p.Required {
				hint := ""
				if p.ExtractionHint != "" {
					hint = fmt.Sprintf(" (%s)", p.ExtractionHint)
				}
				requiredParams = append(requiredParams, fmt.Sprintf("%s%s", p.Name, hint))
			}
		}
		if len(requiredParams) > 0 {
			desc += fmt.Sprintf(" REQUIERE: %s. Si el usuario no proporciona estos datos, PREGUNTA por ellos antes de ejecutar.", strings.Join(requiredParams, ", "))
		}
	}

	return desc
}

func (t *DynamicTool) Category() string {
	return t.tool.Category
}

// Parameters genera el JSON Schema para el LLM basado en InputSchema o variables del template
func (t *DynamicTool) Parameters() map[string]interface{} {
	properties := make(map[string]interface{})
	required := []string{}

	// PRIORIDAD 1: Usar InputSchema si está definido (más rico en información)
	if len(t.tool.InputSchema) > 0 {
		for _, param := range t.tool.InputSchema {
			// Construir descripción enriquecida para la IA
			desc := param.Description
			if param.ExtractionHint != "" {
				desc += fmt.Sprintf(" EXTRACCIÓN: %s.", param.ExtractionHint)
			}
			if param.Example != "" {
				desc += fmt.Sprintf(" Ejemplo: %s.", param.Example)
			}

			// Mapear tipo de InputSchema a JSON Schema type
			jsonType := mapParamTypeToJSONSchema(param.Type)

			paramSchema := map[string]interface{}{
				"type":        jsonType,
				"description": desc,
			}

			// Añadir default si existe
			if param.DefaultValue != "" {
				paramSchema["default"] = param.DefaultValue
			}

			// Marcar como requerido internamente para el registry
			if param.Required {
				paramSchema["required"] = true
				required = append(required, param.Name)
			}

			properties[param.Name] = paramSchema
		}

		return properties
	}

	// FALLBACK: Extraer variables del template si no hay InputSchema
	variables := make(map[string]bool)
	extractVariables(t.tool.Endpoint, variables)
	if t.tool.BodyTemplate != "" {
		extractVariables(t.tool.BodyTemplate, variables)
	}
	if t.tool.Parameters != nil {
		jsonBody, _ := json.Marshal(t.tool.Parameters)
		extractVariables(string(jsonBody), variables)
	}

	for varName := range variables {
		properties[varName] = map[string]interface{}{
			"type":        "string",
			"description": fmt.Sprintf("Parámetro %s (extraído del mensaje del usuario)", varName),
			"required":    true,
		}
		required = append(required, varName)
	}

	return properties
}

// mapParamTypeToJSONSchema convierte tipos de InputSchema a JSON Schema
func mapParamTypeToJSONSchema(paramType string) string {
	switch paramType {
	case "number":
		return "number"
	case "boolean":
		return "boolean"
	case "date", "email", "phone", "string":
		return "string"
	default:
		return "string"
	}
}

// extractVariables busca patrones {{variable}} y las añade al mapa
func extractVariables(text string, vars map[string]bool) {
	re := regexp.MustCompile(`\{\{([a-zA-Z0-9_]+)\}\}`)
	matches := re.FindAllStringSubmatch(text, -1)
	for _, match := range matches {
		if len(match) > 1 {
			vars[match[1]] = true
		}
	}
}

func (t *DynamicTool) Execute(ctx context.Context, params map[string]interface{}, agentCtx *contextmgr.ConversationContext) (interface{}, error) {
	// Normalizar parámetros antes de ejecutar
	normalizedParams := t.normalizeParameters(params)

	switch t.tool.Type {
	case ToolTypeWebhook, ToolTypeAPI:
		return t.executeHTTP(ctx, normalizedParams)
	case ToolTypeQuery:
		return t.executeSQL(ctx, normalizedParams)
	case ToolTypeMCP:
		return nil, fmt.Errorf("MCP tools not implemented yet")
	default:
		return nil, fmt.Errorf("unsupported tool type: %s", t.tool.Type)
	}
}

// normalizeParameters aplica reglas de normalización a los parámetros según InputSchema
func (t *DynamicTool) normalizeParameters(params map[string]interface{}) map[string]interface{} {
	if len(t.tool.InputSchema) == 0 {
		return params
	}

	normalized := make(map[string]interface{})
	for k, v := range params {
		normalized[k] = v
	}

	for _, schema := range t.tool.InputSchema {
		val, exists := normalized[schema.Name]
		if !exists {
			continue
		}

		strVal, ok := val.(string)
		if !ok {
			continue
		}

		// Aplicar normalización según el tipo configurado
		normalizedVal := normalizeValue(strVal, schema.Normalize, schema.FormatPattern)
		if normalizedVal != strVal {
			log.Printf("[DynamicTool] Normalized %s: '%s' -> '%s'", schema.Name, strVal, normalizedVal)
		}
		normalized[schema.Name] = normalizedVal
	}

	return normalized
}

// normalizeValue aplica una regla de normalización a un valor
func normalizeValue(value, normalizeRule, formatPattern string) string {
	if normalizeRule == "" && formatPattern == "" {
		return value
	}

	result := strings.TrimSpace(value)

	// Aplicar regla de normalización
	switch normalizeRule {
	case "uppercase":
		result = strings.ToUpper(result)
	case "lowercase":
		result = strings.ToLower(result)
	case "alphanumeric":
		// Solo mantener letras y números
		re := regexp.MustCompile(`[^a-zA-Z0-9]`)
		result = re.ReplaceAllString(result, "")
	case "alphanumeric_upper":
		re := regexp.MustCompile(`[^a-zA-Z0-9]`)
		result = strings.ToUpper(re.ReplaceAllString(result, ""))
	case "order_id":
		// Normalización especial para IDs de pedido tipo FDX-1234-A1B2
		// 1. Limpiar caracteres no alfanuméricos
		re := regexp.MustCompile(`[^a-zA-Z0-9]`)
		clean := strings.ToUpper(re.ReplaceAllString(result, ""))

		// 2. Si tiene formato pattern, aplicarlo
		if formatPattern != "" {
			result = applyFormatPattern(clean, formatPattern)
		} else {
			// Formato por defecto: XXX-XXXX-XXXX (3-4-4)
			if len(clean) >= 11 {
				result = clean[:3] + "-" + clean[3:7] + "-" + clean[7:11]
			} else if len(clean) >= 7 {
				result = clean[:3] + "-" + clean[3:7] + "-" + clean[7:]
			} else {
				result = strings.ToUpper(result)
			}
		}
	case "trim":
		// Ya se aplicó al inicio
	}

	// Aplicar formato pattern si existe y no se aplicó aún
	if formatPattern != "" && normalizeRule != "order_id" {
		result = applyFormatPattern(result, formatPattern)
	}

	return result
}

// applyFormatPattern aplica un patrón de formato a un valor
// Pattern: "XXX-0000-XXXX" donde X=letra/número, 0=número, guiones son literales
func applyFormatPattern(value, pattern string) string {
	if pattern == "" {
		return value
	}

	// Limpiar el valor (solo alfanumérico)
	re := regexp.MustCompile(`[^a-zA-Z0-9]`)
	cleanValue := strings.ToUpper(re.ReplaceAllString(value, ""))

	// Contar posiciones esperadas en el patrón (X y 0)
	expectedLen := 0
	for _, char := range pattern {
		if char == 'X' || char == 'x' || char == '0' {
			expectedLen++
		}
	}

	// Si el valor limpio no tiene suficientes caracteres, retornar en mayúsculas
	if len(cleanValue) < expectedLen {
		return strings.ToUpper(value)
	}

	result := ""
	valueIdx := 0

	for _, char := range pattern {
		if valueIdx >= len(cleanValue) {
			// Si ya consumimos todo el valor, agregar separadores restantes
			if char == '-' || char == '_' || char == '.' || char == '/' || char == ' ' {
				result += string(char)
			}
			continue
		}

		switch char {
		case 'X', 'x', '0': // Placeholder para caracter del valor
			result += string(cleanValue[valueIdx])
			valueIdx++
		case '-', '_', '.', '/', ' ': // Separadores literales
			result += string(char)
			// Ignorar otros caracteres en el patrón (no son válidos)
		}
	}

	return result
}

// executeHTTP ejecuta una llamada HTTP (Webhook/API)
func (t *DynamicTool) executeHTTP(ctx context.Context, params map[string]interface{}) (interface{}, error) {
	tool := t.tool

	// Never log the endpoint, parameters, headers or bodies: they carry the
	// tool's credentials (auth headers, API keys in query strings) and the
	// conversation's personal data.
	log.Printf("[DynamicTool] Executing HTTP tool: %s (%s)", tool.Name, tool.Method)

	if tool.Endpoint == "" {
		return nil, fmt.Errorf("endpoint is required")
	}

	// 1. Reemplazar variables en la URL
	endpoint := applyTemplate(tool.Endpoint, params)

	// 2. Preparar Body
	var body io.Reader
	var bodyStr string
	if tool.Method != "GET" {
		if tool.BodyTemplate != "" {
			bodyStr = applyTemplate(tool.BodyTemplate, params)
		} else if tool.Parameters != nil {
			// Fallback a Parameters map if BodyTemplate is empty
			jsonTemplate, err := json.Marshal(tool.Parameters)
			if err != nil {
				return nil, fmt.Errorf("invalid body template: %v", err)
			}
			bodyStr = applyTemplate(string(jsonTemplate), params)
		}

		if bodyStr != "" {
			body = strings.NewReader(bodyStr)
		}
	}

	// 3. Crear Request
	timeout := time.Duration(tool.Timeout) * time.Second
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, tool.Method, endpoint, body)
	if err != nil {
		log.Printf("[DynamicTool] ERROR creating request: %v", err)
		return nil, fmt.Errorf("error creating request: %v", err)
	}

	// 4. Headers
	req.Header.Set("Content-Type", "application/json")
	if tool.Headers != nil {
		for key, val := range tool.Headers {
			if strVal, ok := val.(string); ok {
				// También aplicamos template a headers (ej: Authorization: Bearer {{token}})
				req.Header.Set(key, applyTemplate(strVal, params))
			}
		}
	}

	// 5. Auth
	if tool.AuthType != "" && tool.AuthType != "none" && tool.AuthConfig != nil {
		log.Printf("[DynamicTool] Applying auth type: %s", tool.AuthType)
		applyAuth(req, tool.AuthType, tool.AuthConfig)
	}

	// 6. Ejecutar
	// Tenant-configured destination: guard against SSRF into the platform's
	// own network (DB, internal services, cloud metadata).
	client := NewGuardedHTTPClient(timeout)
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, ErrBlockedEgress) {
			return nil, ErrBlockedEgress
		}
		log.Printf("[DynamicTool] ERROR request failed for tool %s: %v", tool.Name, err)
		return nil, fmt.Errorf("request failed: %v", err)
	}
	defer resp.Body.Close()

	log.Printf("[DynamicTool] Response status: %d %s", resp.StatusCode, resp.Status)

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		log.Printf("[DynamicTool] ERROR reading response: %v", err)
		return nil, fmt.Errorf("error reading response: %v", err)
	}

	// 7. Parsear respuesta
	var result interface{}
	if err := json.Unmarshal(respBody, &result); err != nil {
		result = string(respBody) // Fallback a texto
	}

	if resp.StatusCode >= 400 {
		log.Printf("[DynamicTool] ERROR HTTP status %d", resp.StatusCode)
		// Return structured error with full details
		errorDetails := map[string]interface{}{
			"status_code":    resp.StatusCode,
			"status":         resp.Status,
			"response_body":  result,
			"request_url":    endpoint,
			"request_method": tool.Method,
		}
		if bodyStr != "" {
			errorDetails["request_body"] = bodyStr
		}
		errorJSON, _ := json.Marshal(errorDetails)
		return result, fmt.Errorf("%s", string(errorJSON))
	}

	log.Printf("[DynamicTool] Tool executed successfully")
	return result, nil
}

// executeSQL ejecuta una consulta SQL
func (t *DynamicTool) executeSQL(_ context.Context, _ map[string]interface{}) (interface{}, error) {
	// Raw, tenant-authored SQL on the shared connection is a cross-tenant read
	// primitive (see ErrQueryToolsDisabled). Disabled unconditionally.
	return nil, ErrQueryToolsDisabled
}

// applyTemplate reemplaza {{key}} con val en el texto
func applyTemplate(template string, params map[string]interface{}) string {
	re := regexp.MustCompile(`\{\{([a-zA-Z0-9_]+)\}\}`)
	return re.ReplaceAllStringFunc(template, func(match string) string {
		key := match[2 : len(match)-2] // quitar {{ y }}
		if val, ok := params[key]; ok {
			return fmt.Sprintf("%v", val)
		}
		return match // dejar igual si no se encuentra
	})
}

func applyAuth(req *http.Request, authType string, config map[string]interface{}) {
	switch authType {
	case "bearer":
		if token, ok := config["token"].(string); ok {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	case "api_key":
		if key, ok := config["key"].(string); ok {
			headerName := "X-API-Key"
			if name, ok := config["header_name"].(string); ok {
				headerName = name
			}
			req.Header.Set(headerName, key)
		}
	case "basic":
		if user, ok := config["username"].(string); ok {
			if pass, ok := config["password"].(string); ok {
				req.SetBasicAuth(user, pass)
			}
		}
	}
}
