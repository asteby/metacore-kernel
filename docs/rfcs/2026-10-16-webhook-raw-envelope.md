# RFC: webhooks entrantes con cuerpo exacto, petición completa y respuesta del addon

- Estado: implementado en el PR que trae este archivo (kernel), pendiente de tag + bump en ops.
- Fecha: 2026-10-16
- Motivación: integrar pasarelas de pago (Mercado Pago, Stripe, PayPal, Conekta,
  Aplazo, Kueski, Openpay) como addons wasm. Es el bloqueador K3 del plan de pagos MX.

## 1. Problema

| # | Síntoma | Causa raíz |
|---|---------|------------|
| a | Las firmas de Stripe (`t.payload`), PayPal (CRC32) y Conekta (RSA) nunca validan en el guest | `runtime/webhookin` entregaba el cuerpo como `json.RawMessage` dentro de un mapa; al serializarlo Go lo compacta y escapa HTML (`{\n "a":"<x>&"}` → `{"a":"\u003cx\u003e\u0026"}`). Lo mismo en `runtime/apiroute` (`Request.Body`). |
| b | Mercado Pago `x-signature` no se puede verificar | firma `data.id` (query) + `x-request-id` + `ts`; el handler sólo veía el cuerpo |
| c | Aplazo exige `{"status":true}`, Kueski un JWT firmado, Openpay devolver el código de verificación | `Receiver.Dispatch` sólo devuelve `error`; el addon no puede dar forma a la respuesta HTTP |

## 2. Payload del handler (`webhookin.BuildPayload`)

```json
{
  "org": "<uuid>",
  "webhook": "<route key>",
  "path": "/mercadopago/payments",
  "method": "POST",
  "query": {"data.id": ["123"], "type": ["payment"]},
  "headers": {"x-signature": "ts=…,v1=…", "x-request-id": "…", "content-type": "application/json"},
  "raw_body_b64": "<base64 estándar de los bytes exactos>",
  "body": {"...": "copia de conveniencia; NO usar para firmas"},
  "received_at": "2026-10-16T12:00:00.000000005Z"
}
```

- `org`, `webhook`, `path`, `body` son los campos originales (compatibilidad).
- `query` nunca es nulo (`{}` sin query). `method` es `POST` si el host no lo indica.
- `headers`: claves en minúscula, valores repetidos unidos con `", "`. Sólo pasan los de
  la lista permitida:
  - `webhookin.DefaultHeaders`: `content-type`, `user-agent`, `x-request-id`,
    `x-signature`, `x-signature-256`, `x-hub-signature-256`, `stripe-signature`,
    `paypal-transmission-id`, `paypal-transmission-time`, `paypal-transmission-sig`,
    `paypal-cert-url`, `paypal-auth-algo`, `digest`, `authorization`, `x-kueski-*`,
    `x-aplazo-*`, `webhook-signature`, `webhook-timestamp`, `webhook-id`.
  - más `webhooks[].headers` del manifiesto (minúsculas, `*` final = prefijo).
  - `authorization` sólo cuando `verify` es `""` o `"addon"`; en rutas `hmac-sha256`/`bearer`
    nunca sale del kernel. `cookie`, `set-cookie` y `proxy-authorization` nunca pasan.
    El kernel no registra valores de cabeceras.
- `raw_body_b64` es la única fuente válida para verificar firmas.
- `raw_body` es un alias de compatibilidad: el cuerpo exacto como texto, presente sólo si los
  bytes son UTF-8 válido (ausente, nunca vacío, en cuerpos binarios). Verificar la firma sobre
  `raw_body_b64`.
- Los backends deben serializar con `webhookin.EncodePayload` (sin escape HTML).

## 3. `verify: "addon"`

Nuevo esquema: el kernel no verifica ni necesita secreto; el handler verifica con
`raw_body_b64`/`query`/`headers` y rechaza respondiendo un sobre con `status: 401`.
`secret_ref` es opcional (si se declara, se valida contra los conectores). Es el estándar
para pagos.

## 4. API Go

```go
type Inbound struct {
    Path    string
    Method  string
    Query   url.Values
    Headers http.Header
    Body    []byte
}

type Response struct {
    Envelope bool              // la salida coincide con el sobre
    Status   int
    Headers  map[string]string // filtradas a webhookin.ResponseHeaders
    Body     json.RawMessage
    Output   []byte            // salida cruda del handler
}
func (Response) BodyBytes() []byte

func (*Receiver) DispatchRequest(ctx, orgID, Inbound) (Response, error)
func (*Receiver) Dispatch(ctx, orgID, path, headers, body) error // sin cambios

type ResultDispatcher interface {
    Dispatcher
    DispatchResult(ctx, orgID, target, payload) ([]byte, error)
}
```

- Si el despachador de la ruta implementa `ResultDispatcher`, su salida vuelve en
  `Response.Output`. Si es un objeto JSON con sólo `status` (entero 100–599, obligatorio),
  `headers` y `body`, `Envelope` es `true` y el host responde con eso. Cabeceras de
  respuesta permitidas: `Content-Type`, `Cache-Control`, `Retry-After` (sin CR/LF).
  Con un `Content-Type` no JSON y `body` string, `BodyBytes` devuelve el texto tal cual
  (p. ej. el JWT de Kueski en `text/plain`).
- Si la salida no es un sobre, el host conserva su respuesta por defecto.

## 5. `runtime/apiroute`

`Request` gana `RawBody` (`raw_body_b64`, base64 de los bytes exactos, omitido sin cuerpo)
y `Request.Encode()`, que serializa sin escape HTML. `Body` sigue siendo JSON de
conveniencia; un cuerpo no JSON ya no se escapa al envolverlo en string.

## 6. Fuera de alcance

Tabla host de deduplicación por `event_id` y modo de ack asíncrono (resto de K3) quedan
para el host.
