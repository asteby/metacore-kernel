# Recepción genérica de eventos (`inbound`)

Implementa la fase F4 del RFC 0003 (§2.3 y §6): un solo endpoint
`POST /api/inbound/{org}/{contract}` que recibe eventos firmados de sistemas
externos y los aplica sobre el modelo dinámico **solo con datos**. Un contrato
nuevo no agrega rutas ni código.

## Contrato (datos)

```json
{
  "key": "stock.changed",
  "version": 1,
  "json_schema": { "type": "object", "required": ["event_id","sku","warehouse","quantity"],
                   "additionalProperties": false,
                   "properties": { "event_id": {"type":"string"}, "sku": {"type":"string"},
                                   "warehouse": {"type":"string"}, "quantity": {"type":"integer","minimum":0} } },
  "route": {
    "model": "inventory", "op": "upsert",
    "mapping":  { "sku": "sku", "warehouse": "warehouse", "quantity": "quantity" },
    "defaults": { "source": "inbound" },
    "match":    ["sku", "warehouse"],
    "scope":    { "branch": "branch" }
  },
  "idempotency": "{event_id}",
  "auth": "hmac_canonical_v1",
  "log": { "key_in_clear": false }
}
```

- `route.model` + `op` (`create|update|upsert`) o `route.command` (nombre de un
  comando ya registrado por el host en `CommandRegistry`). Exactamente uno.
- `mapping`: campo destino -> ruta en el cuerpo (`a.b`, `items.0.sku`). Una
  fuente ausente se omite (la obligatoriedad la decide el esquema). Los destinos
  `id`, `organization_id`, `tenant_id`, `created_by_id`, `created_at`,
  `updated_at` y `deleted_at` están prohibidos: la organización **nunca** sale
  del cuerpo.
- `match`: campos (claves de `mapping`) que identifican la fila para
  `update`/`upsert`. Se filtra con igualdad y cada fila devuelta se
  re-verifica: si un campo no es filtrable el ejecutor se niega (500 genérico)
  en vez de actualizar una fila cualquiera.
- `idempotency`: plantilla con `{ruta}` y `{header:Nombre}`; debe referenciar al
  menos un campo. Con valores no escalares, ausentes o de más de 256 bytes el
  evento se rechaza (422 `invalid_event_key`).
- `json_schema`: draft 2020-12. Los `$ref` remotos o de archivo se rechazan al
  registrar (no hay SSRF ni lectura de disco por un contrato).
- Versiones: sin cabecera gana la más alta; `X-Contract-Version: N` fija una.
- `scope.branch` / `scope.entity` (RFC 0001): se pasan al contexto
  (`dynamic.WithBranchID` / `WithEntityID`); si el host da
  `DynamicExecutor.EntityScope` y la sucursal queda fuera del alcance de la
  entidad, el evento se rechaza (403 `scope_forbidden`).

Los contratos se cargan con `Registry.Register` o `Registry.LoadJSON`
(objeto o arreglo; ideal para presets del hub). `Handler.Check()` revisa que
cada contrato tenga perfil de autenticación, ejecutor o comando.

## Montaje en un host

```go
reg := inbound.NewRegistry()
_ = reg.LoadJSON(contractsJSON)

rcpt, _ := inbound.NewGormReceiptStore(db) // tabla metacore_inbound_receipts

cmds := inbound.NewCommandRegistry()
cmds.Register("orders.set_status", func(ctx context.Context, op inbound.Operation) (inbound.Outcome, error) { ... })

h, err := inbound.New(inbound.Config{
    Contracts: reg,
    Verifiers: map[string]inbound.Verifier{"hmac_canonical_v1": verifier}, // perfiles de `webhooks`
    Receipts:  rcpt,
    Executor:  &inbound.DynamicExecutor{Service: dyn},
    Commands:  cmds,
    Limiter:        &inbound.TokenBucket{Rate: 50, Burst: 100}, // por organización
    PreAuthLimiter: &inbound.TokenBucket{Rate: 20, Burst: 40},  // por IP, antes de autenticar
})
mux.Handle("POST /api/inbound/", h) // net/http; en Fiber, adaptor.HTTPHandler(h)
```

`Verifier` es el puerto mínimo (`Verify(ctx, VerifyRequest) error`). Los perfiles
de producción (`hmac_simple`, `hmac_canonical_v1`, `bearer`,
`bearer+hmac_canonical_v1`) los publica el paquete `webhooks`; `HMACCanonicalV1`
de este paquete es una implementación autónoma con el formato
`"{ts}.{nonce}.{METHOD}.{path}.{sha256hex(body)}"`, ventana ±300 s y nonce de
10 min por organización (cabeceras `X-Metacore-Timestamp|Nonce|Signature`). La
ruta firmada es `r.URL.Path` tal cual llega: el host no debe reescribirla.

Las operaciones `model` corren por `dynamic.Service` con
`dynamic.NewSystemCaller(org)` donde `org` sale de la ruta ya autenticada (el
secreto es por organización). Se aplican aislamiento por organización,
validación, hooks y eventos; se omite solo la compuerta de permisos de usuario
(la firma es la autorización). Para otro principal, `DynamicExecutor.Caller`.

## Respuestas

Éxito (200): `{"status":"applied|replayed","receipt_id","record_id","created"}`.

Error (siempre el mismo sobre): `{"error":{"code","message","details":[{"path","rule"}]}}`.

| HTTP | `code` | Cuándo |
|---|---|---|
| 400 | `malformed_json` | JSON roto o con datos sobrantes |
| 401 | `unauthorized` | firma/token/ventana/nonce inválido u organización desconocida (sin distinguir) |
| 403 | `scope_forbidden` / `forbidden` | fuera del alcance de entidad/sucursal |
| 404 | `unknown_contract` / `not_found` / `record_not_found` | contrato, ruta o fila inexistente |
| 405 | `method_not_allowed` | no es POST |
| 409 | `idempotency_conflict` | misma llave con otro cuerpo |
| 409 | `in_progress` | la llave se está procesando (reintentar; `Retry-After`) |
| 409 | `state_conflict`, `ambiguous_match` | conflicto con el estado del modelo |
| 413 | `payload_too_large` | más de `MaxBodyBytes` (1 MiB por omisión) |
| 415 | `unsupported_media_type` | no es `application/json` |
| 422 | `invalid_schema` | no cumple el esquema; `details` trae solo ruta JSON y palabra clave, **nunca valores** |
| 422 | `invalid_event_key` / `rejected_by_model` | llave no derivable / el modelo lo rechazó |
| 429 | `rate_limited` | límite de tasa |
| 500 | `internal_error` | cualquier fallo interno; el texto real nunca sale |

## Idempotencia y bitácora

La llave del evento se reserva de forma atómica (índice único; `ON CONFLICT DO
NOTHING`), por `organización + contrato + llave`:

- Aplicado + mismo cuerpo -> `200 replayed` con el mismo `receipt_id`/`record_id`; el efecto no se repite.
- Aplicado + otro cuerpo -> `409 idempotency_conflict`.
- En proceso dentro del arriendo (60 s) -> `409 in_progress`; vencido (proceso caído) o `failed` -> se reclama de nuevo (`attempts+1`).
- Esquema inválido o llave no derivable -> recibo `rejected` sin reservar: el evento corregido se aplica.

`metacore_inbound_receipts` guarda estado, código de error (nunca texto de
error), contrato/versión, hash del cuerpo, hash de la llave, id del registro y
intentos. **No guarda el cuerpo.** La llave solo se guarda en claro si el
contrato lo pide (`log.key_in_clear`); por omisión es un hash porque suele
llevar correos o teléfonos. Consulta: `Handler.Receipts(ctx, org, ReceiptFilter{Contract, Status, EventKey})`
(siempre acotada a una organización; `EventKey` en claro se hashea para buscar).
Las peticiones sin autenticar no escriben en la bitácora.

## Seguridad

- Perfiles de firma de lista cerrada; el cliente no ejecuta código: la transformación es solo `mapping`/`defaults`.
- Un fallo de firma no consume el nonce; el caché de nonces tiene tope y falla cerrado.
- Límite de cuerpo con `http.MaxBytesReader` (también sin `Content-Length`), tipo de contenido obligatorio.
- Sin limitador compartido en el repo: `TokenBucket` es por proceso (acotado en claves, falla cerrado). Con varias réplicas, limitar también en el borde.
- Los errores internos, de modelo y pánicos de comandos se registran solo por tipo/código, jamás por texto (puede traer datos personales).
- Un comando es de confianza del host: se ejecuta con timeout (15 s) y `recover`, pero el kernel no puede impedir que ignore el contexto.

## Límites conocidos

- `upsert` es buscar-y-luego-escribir; dos eventos distintos concurrentes sobre la misma fila nueva pueden chocar. Definir un índice único sobre los campos `match` (el segundo falla y se reintenta como actualización).
- El recibo `processing` si el proceso muere se recupera tras el arriendo, no antes.
