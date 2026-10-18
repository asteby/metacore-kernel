package legacybridge

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Cabeceras del contrato HTTP del puente (en los dos sentidos).
const (
	HeaderIdempotencyKey = "X-Idempotency-Key"
	HeaderTimestamp      = "X-Bridge-Timestamp"
	HeaderSignature      = "X-Bridge-Signature"
	HeaderCommand        = "X-Bridge-Command"
)

// MaxClockSkew es la diferencia máxima aceptada entre X-Bridge-Timestamp y el reloj local.
const MaxClockSkew = 5 * time.Minute

// Sign firma body con HMAC-SHA256 sobre "<timestamp>.<body>" y devuelve
// "sha256=<hex>".
func Sign(secret []byte, timestamp string, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(timestamp))
	m.Write([]byte("."))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// ErrBadSignature: firma ausente, inválida o fuera de la ventana de tiempo.
var ErrBadSignature = errors.New("legacybridge: firma inválida")

// Verify comprueba la firma y que timestamp (segundos Unix) esté dentro de
// MaxClockSkew de now. Compara en tiempo constante.
func Verify(secret []byte, timestamp, signature string, body []byte, now time.Time) error {
	if len(secret) == 0 || timestamp == "" || signature == "" {
		return ErrBadSignature
	}
	sec, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return ErrBadSignature
	}
	if d := now.Sub(time.Unix(sec, 0)); d > MaxClockSkew || d < -MaxClockSkew {
		return fmt.Errorf("%w: timestamp fuera de ventana", ErrBadSignature)
	}
	if !hmac.Equal([]byte(Sign(secret, timestamp, body)), []byte(strings.TrimSpace(signature))) {
		return ErrBadSignature
	}
	return nil
}

// HTTPConnector entrega comandos por HTTPS: POST <BaseURL>/<ruta del comando>
// con Authorization: Bearer <Token>, X-Idempotency-Key y firma HMAC.
//
// Clasificación de la respuesta:
//   - 2xx: entregado. El cuerpo se lee como {"data":{"external_id","source_version"}}
//     o con esos campos en la raíz.
//   - 408, 425, 429 y 5xx: transitorio (429/503 respetan Retry-After).
//   - 401/403: transitorio (credencial rotada o mal configurada; se reintenta y
//     termina en muertos si no se arregla).
//   - Otro 4xx: permanente (validación): el comando va a muertos.
type HTTPConnector struct {
	BaseURL string
	Token   string
	Secret  []byte
	Client  *http.Client
	// Path traduce el Kind a la ruta. Por omisión "customers.upsert" → "customers/upsert".
	Path func(kind string) string
	Now  func() time.Time
}

// MaxResponseBytes acota el cuerpo que se guarda de una respuesta.
const MaxResponseBytes = 64 << 10

func (h *HTTPConnector) Send(ctx context.Context, c Command) (Delivery, error) {
	if h.BaseURL == "" {
		return Delivery{}, Permanent(errors.New("legacybridge: HTTPConnector sin BaseURL"))
	}
	path := strings.ReplaceAll(c.Kind, ".", "/")
	if h.Path != nil {
		path = h.Path(c.Kind)
	}
	url := strings.TrimRight(h.BaseURL, "/") + "/" + strings.TrimLeft(path, "/")
	body := []byte(c.Payload)
	if len(body) == 0 {
		body = []byte(`{}`)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Delivery{}, Permanent(err)
	}
	now := time.Now
	if h.Now != nil {
		now = h.Now
	}
	ts := strconv.FormatInt(now().Unix(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set(HeaderIdempotencyKey, c.IdempotencyKey)
	req.Header.Set(HeaderCommand, c.Kind)
	if h.Token != "" {
		req.Header.Set("Authorization", "Bearer "+h.Token)
	}
	if len(h.Secret) > 0 {
		req.Header.Set(HeaderTimestamp, ts)
		req.Header.Set(HeaderSignature, Sign(h.Secret, ts, body))
	}
	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return Delivery{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return parseDelivery(raw), nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable:
		err := fmt.Errorf("legacybridge: %s respondió %d: %s", c.Kind, resp.StatusCode, snippet(raw))
		if d := retryAfterHeader(resp.Header.Get("Retry-After")); d > 0 {
			return Delivery{}, RetryAfter(err, d)
		}
		return Delivery{}, err
	case resp.StatusCode >= 500, resp.StatusCode == http.StatusRequestTimeout, resp.StatusCode == http.StatusTooEarly,
		resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return Delivery{}, fmt.Errorf("legacybridge: %s respondió %d: %s", c.Kind, resp.StatusCode, snippet(raw))
	default:
		return Delivery{}, Permanent(fmt.Errorf("legacybridge: %s rechazado %d: %s", c.Kind, resp.StatusCode, snippet(raw)))
	}
}

func parseDelivery(raw []byte) Delivery {
	d := Delivery{}
	if len(raw) > 0 && json.Valid(raw) {
		d.Body = json.RawMessage(raw)
	}
	var env struct {
		Data struct {
			ExternalID    string `json:"external_id"`
			SourceVersion string `json:"source_version"`
		} `json:"data"`
		ExternalID    string `json:"external_id"`
		SourceVersion string `json:"source_version"`
	}
	if json.Unmarshal(raw, &env) == nil {
		d.ExternalID, d.SourceVersion = env.Data.ExternalID, env.Data.SourceVersion
		if d.ExternalID == "" {
			d.ExternalID = env.ExternalID
		}
		if d.SourceVersion == "" {
			d.SourceVersion = env.SourceVersion
		}
	}
	return d
}

func retryAfterHeader(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if s, err := strconv.Atoi(v); err == nil && s > 0 {
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
