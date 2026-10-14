package claude

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// Los modelos Claude de la familia 5.x rechazan los parámetros de muestreo
// (`temperature`, `top_p`, `top_k`) con un 400 ("'temperature' is deprecated
// for this model"). langchaingo serializa `temperature` siempre (el campo no
// lleva omitempty) y `top_p` cuando es distinto de cero, así que cualquier
// cliente construido sobre él falla contra esos modelos. Este archivo decide
// qué modelos los aceptan y ofrece un RoundTripper que los retira del cuerpo
// antes de enviarlo.

// samplingParams son las llaves que se eliminan cuando el modelo no las admite.
var samplingParams = []string{"temperature", "top_p", "top_k"}

// samplingRejectMinMajor es la primera generación de Claude que rechaza los
// parámetros de muestreo.
const samplingRejectMinMajor = 5

var (
	samplingOverridesMu sync.RWMutex
	samplingOverrides   = map[string]bool{}
)

// SetModelSamplingSupport registra explícitamente si un modelo acepta
// temperature/top_p/top_k. Tiene prioridad sobre la regla por familia; sirve
// para modelos con alias propios (proxies, Bedrock, etc.) o para corregir la
// regla sin publicar una versión nueva. El nombre se compara sin mayúsculas.
func SetModelSamplingSupport(model string, supported bool) {
	key := strings.ToLower(strings.TrimSpace(model))
	if key == "" {
		return
	}
	samplingOverridesMu.Lock()
	samplingOverrides[key] = supported
	samplingOverridesMu.Unlock()
}

// ModelAcceptsSampling indica si el modelo acepta temperature/top_p/top_k.
// Primero consulta las capacidades registradas con SetModelSamplingSupport;
// si no hay registro, los modelos Claude con generación ≥5 no los aceptan y
// todo lo demás (Claude 3.x/4.x, modelos de otros proveedores, "auto") sí.
func ModelAcceptsSampling(model string) bool {
	key := strings.ToLower(strings.TrimSpace(model))
	if key == "" {
		return true
	}
	samplingOverridesMu.RLock()
	v, ok := samplingOverrides[key]
	samplingOverridesMu.RUnlock()
	if ok {
		return v
	}
	major, ok := claudeMajorVersion(key)
	if !ok {
		return true
	}
	return major < samplingRejectMinMajor
}

// claudeMajorVersion extrae la generación de un id de modelo Claude:
// "claude-opus-5-5" → 5, "claude-sonnet-4-5-20250929" → 4,
// "claude-3-5-sonnet-latest" → 3, "us.anthropic.claude-opus-5-5-v1:0" → 5.
// Devuelve false si el id no es de Claude o no trae una versión reconocible.
func claudeMajorVersion(model string) (int, bool) {
	i := strings.Index(model, "claude")
	if i < 0 {
		return 0, false
	}
	rest := model[i+len("claude"):]
	tokens := strings.FieldsFunc(rest, func(r rune) bool {
		return r == '-' || r == '.' || r == '_' || r == ':' || r == '@' || r == '/' || r == '[' || r == ']'
	})
	for _, tok := range tokens {
		if len(tok) > 2 { // fechas (20250929) u otros sufijos, no una versión
			if _, err := strconv.Atoi(tok); err == nil {
				return 0, false
			}
			continue
		}
		n, err := strconv.Atoi(tok)
		if err != nil {
			continue
		}
		return n, true
	}
	return 0, false
}

// StripUnsupportedSampling quita temperature/top_p/top_k de un cuerpo JSON de
// petición (Anthropic Messages u OpenAI chat-completions) cuando su campo
// "model" no los acepta. Devuelve el cuerpo original y false si no cambió
// nada o si el cuerpo no es un objeto JSON.
func StripUnsupportedSampling(body []byte) ([]byte, bool) {
	if len(bytes.TrimSpace(body)) == 0 {
		return body, false
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, false
	}
	var model string
	if raw, ok := payload["model"]; ok {
		_ = json.Unmarshal(raw, &model)
	}
	if ModelAcceptsSampling(model) {
		return body, false
	}
	changed := false
	for _, k := range samplingParams {
		if _, ok := payload[k]; ok {
			delete(payload, k)
			changed = true
		}
	}
	if !changed {
		return body, false
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return body, false
	}
	return out, true
}

// samplingSanitizer reescribe el cuerpo de las peticiones POST JSON para
// retirar los parámetros de muestreo que el modelo destino rechaza.
type samplingSanitizer struct {
	base http.RoundTripper
}

// NewSamplingSanitizer envuelve base (http.DefaultTransport si es nil) con un
// RoundTripper que aplica StripUnsupportedSampling a cada POST JSON. Úsalo en
// el http.Client de langchaingo (anthropic.WithHTTPClient /
// openai.WithHTTPClient) cuando el modelo puede ser Claude 5.x.
func NewSamplingSanitizer(base http.RoundTripper) http.RoundTripper {
	return &samplingSanitizer{base: base}
}

func (t *samplingSanitizer) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	req2, err := sanitizeSamplingRequest(req)
	if err != nil {
		return nil, err
	}
	return base.RoundTrip(req2)
}

// sanitizeSamplingRequest devuelve req sin cambios o un clon con el cuerpo
// reescrito. Nunca muta la petición original (contrato de RoundTripper).
func sanitizeSamplingRequest(req *http.Request) (*http.Request, error) {
	if req.Method != http.MethodPost || req.Body == nil || req.Body == http.NoBody {
		return req, nil
	}
	if ct := req.Header.Get("Content-Type"); ct != "" && !strings.Contains(strings.ToLower(ct), "json") {
		return req, nil
	}
	raw, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil {
		return nil, err
	}
	body, changed := StripUnsupportedSampling(raw)
	req2 := req.Clone(req.Context())
	if !changed {
		body = raw
	}
	req2.Body = io.NopCloser(bytes.NewReader(body))
	req2.ContentLength = int64(len(body))
	req2.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	if changed {
		req2.Header.Del("Content-Length")
	}
	return req2, nil
}
