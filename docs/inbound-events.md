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

## Endurecimiento (revisión adversarial)

- **Nonces.** El caché es por organización (`MaxNonces` por org, 20000 por omisión; `MaxOrgs`) con expiración O(1) amortizada (cola FIFO por org, sin recorrer el mapa). Una organización que llena su ventana solo se bloquea a sí misma. Un verificador que implemente `PreparedVerifier` (`Prepare` autentica sin efectos y devuelve `commit`) hace que el nonce se consuma recién después del limitador por organización, `Content-Type`, esquema, llave y alcance: un 415/422/429 no gasta nonce, y una petición sin firma válida nunca toca el caché. Un `Verifier` simple sigue funcionando (autentica y consume de una vez).
- **Limitador.** `TokenBucket` desaloja por LRU al llenarse `MaxKeys` (nunca rechaza una clave nueva solo por tabla llena). La llave pre-auth agrupa IPv6 por /64 (`LimiterKeyForIP`). La IP del cliente es `RemoteAddr`; `X-Forwarded-For` solo se honra si el par está en `Config.TrustedProxies` (`ClientIPFrom` toma la entrada más a la derecha que no sea proxy de confianza). Sin proxies de confianza, XFF se ignora porque es falsificable. `Config.ClientIP` sigue permitiendo reemplazarlo.
- **Alcance (RFC 0001).** Si el contrato declara `scope.branch`/`scope.entity`, el evento DEBE traerlos (422 `scope_required`). `DynamicExecutor.EntityScope` es obligatorio cuando hay entidad: sin resolutor responde 500 (mal configurado) y si el resolutor devuelve `ok=false`, 403 `scope_unresolved`. La búsqueda de `update`/`upsert` ignora filas fuera del alcance (se crea una propia, jamás se modifica la de otra entidad) y una sucursal fuera del alcance es 403 `scope_forbidden`.
- **Upsert.** Se serializa por (org, modelo, valores de `match`) dentro del proceso (solo optimización; con guarda la garantía es el candado de fila, ver "Atomicidad"). Entre réplicas **el modelo debe tener un índice único sobre los campos de `match`** (por organización): quien pierde la carrera falla al crear, relee y actualiza a la ganadora.
- **`match` válido al registrar.** `New`, `Handler.Check()` y `Handler.RegisterContract(ctx, c)` consultan `ContractChecker` (lo implementa `DynamicExecutor`): cada campo de `match` debe ser una columna declarada del modelo; si no, error `ErrInvalidContract` al arrancar, aunque la tabla esté vacía. La comparación de la fila encontrada es por tipo (números numéricamente, cadenas exactas). Requiere que el modelo esté registrado antes de cargar los contratos.
- **Recibos.** `Finish` queda para compatibilidad; el manejador usa `AttemptFinisher.FinishAttempt` (ambos stores) con cerca por intento: un proceso lento tras vencer el arriendo no pisa al que reclamó. Los rechazos (`rejected`) se acotan por organización (`MaxRejected`, 500 por omisión) y se podan los más viejos. `ReceiptFilter.EventKey` exige `Contract` en todos los stores (`ErrEventKeyNeedsContract`).
- **Números.** Valores que no caben en `float64` (`1e999`) se rechazan con 422 (`number_range`).
- **Contrato desconocido.** Se autentica antes de revelar si existe: sin firma válida la respuesta es 401 para contratos conocidos y desconocidos por igual. Con firma válida se responde 404 `unknown_contract`. El perfil usado es `Config.DefaultAuth`; vacío = los contratos desconocidos siempre responden 401.

## Cadena de acciones (RFC 0003 §2.4)

Un contrato puede declarar `actions` en lugar de `route` (son excluyentes; sin
`actions` todo se comporta como antes). La lista de acciones es **cerrada**:

| `do` | Campos | Efecto |
|---|---|---|
| `record.create` / `record.update` / `record.upsert` | `model`, `mapping`, `match`, `defaults`, `scope` | reusa el `Executor` (mismo muro de organización y de modelo) |
| `record.delete` | `model`, `mapping` (solo campos de `match`), `match`, `scope`, `mode`, `max_rows` | retira filas elegidas SOLO por `match`; borrado lógico por omisión (ver abajo) |
| `record.action` | `model`, `mapping` (solo `match`), `match`, `action`, `params`, `on_ineligible`, `scope`, `max_rows` | ejecuta una acción del modelo con `ingest: true` sobre las filas elegidas por `match` (ver abajo) |
| `record.transition` | `model`, `mapping` (solo `match`), `match`, `to`, `scope`, `max_rows` | mueve el `stage_field` a un estado de la máquina del modelo |
| `record.restore` | `model`, `mapping` (solo `match`), `match`, `scope`, `max_rows` | revierte un borrado lógico |
| `event.emit` | `name`, `payload` | `EventEmitter.Emit` (puerto mínimo; este paquete no importa `webhooks`) |
| `automation.enqueue` | `name`, `payload` | `AutomationIngester.IngestEvent`, que el host adapta a `automation.Scheduler.Ingest` |
| `notify` | `template`, `payload` (parámetros) | `Notifier.Notify`; el host renderiza con `notifications.Render` |
| `command.run` | `command`, `mapping`, `op` | `CommandRegistry` existente |

No hay acción HTTP saliente: eso son suscripciones de salida.

```json
{ "key": "crm-cliente-nuevo", "version": 1, "auth": "hmac_canonical_v1",
  "idempotency": "{id}", "json_schema": { "...": "..." }, "on_error": "stop",
  "actions": [
    { "do": "record.upsert", "model": "customers", "match": ["external_ref"],
      "mapping": { "external_ref": "{{payload.id}}", "name": "{{payload.nombre}}" } },
    { "do": "event.emit", "name": "customers.customer.imported", "payload": { "id": "{{result.id}}" } },
    { "do": "notify", "template": "cliente_importado", "when": { "path": "result.created", "eq": true } }
  ] }
```

**Plantillas `{{ref}}`.** `payload.<ruta>` (cuerpo), `event.key|contract|version|org`,
`result.id|created` (resultado de la última acción de registro/comando; `event.emit`,
`notify` y `automation.enqueue` no lo cambian). Un valor `"{{x}}"` completo conserva su
tipo JSON; mezclado con texto produce cadena. Se validan **al registrar**: una ruta
`payload.*` debe estar declarada en `properties` del esquema (un escalar no tiene hijos;
si el esquema es opaco por `$ref/allOf/anyOf/oneOf` se acepta), `result.*` no existe en la
primera acción, un campo ajeno a la acción, un `match` fuera del `mapping`, más de 10
acciones o un `do` desconocido fallan con `ErrInvalidContract`. En ejecución un valor
ausente omite el campo (valor completo) o falla con `template_value_missing` (mezclado).

**`when`.** Exactamente UN operador por condición, sobre `payload.*`, `event.*` o `result.*`
(mismos operadores y semántica que `webhooks.Condition`, solo escalares):
`{"path": "...", "eq": <escalar>}`, `{"path": "...", "ne": <escalar>}`,
`{"path": "...", "in": [<escalar>, ...]}` (1 a 50 valores) o `{"path": "...", "exists": true|false}`.
Ruta ausente: `eq` e `in` son falsos y **`ne` es verdadero** (igual que en webhooks); use `exists`
para exigir presencia. Los números se comparan por valor (`1` = `1.0`) y nunca igualan a una
cadena (`1` ≠ `"1"`). Se valida al registrar: dos operadores, ninguno, `in` vacío o de más de 50
valores, valores no escalares o una ruta `payload.*` fuera del esquema fallan con
`ErrInvalidContract` (`actions[i] (...): invalid_condition: when: ...`). Falso = paso `skipped`.
Ejemplo (despublicar en `deleted`, upsert en `created|updated`):
`{"do":"record.upsert", ..., "when":{"path":"payload.action","in":["created","updated"]}}` y
`{"do":"record.delete", ..., "when":{"path":"payload.action","eq":"deleted"}}`.

**`on_error`.** `stop` (por omisión) o `continue`, por contrato o por acción. Con `continue`
el paso queda `error` en el recibo y el evento termina `applied`.

**Recibo e idempotencia.** Las acciones corren dentro del mismo recibo: duplicado ->
`replayed` sin repetir nada (misma llave con otro cuerpo -> `409 idempotency_conflict`).
Tras cada acción se guarda su progreso (`Receipt.Progress`: índice, `do`, estado, `record_id`,
`created`, código de error; nunca valores del cuerpo). Si una falla, el recibo queda
`failed`, la respuesta trae `error.failed_action` y **el mismo evento reintentado reanuda**
en esa acción, sin repetir las ya aplicadas (y con su `result`). Un reintento con otro
cuerpo tras un efecto parcial es `409`. Si el contrato cambia de `do` entre intentos:
`409 contract_changed`. Garantía: *al menos una vez* para la acción que murió entre su
efecto y el guardado del progreso; `event.emit`, `notify` y `automation.enqueue` reciben
una `IdempotencyKey` estable (`<recibo>:<índice>`) para que el destino deduplique.
Si el proceso se cae con el recibo `processing`, se reanuda al vencer el arriendo.

**Origen y bucles.** Todo lo que causa la cadena lleva `Origin = "inbound:<contrato>"`
(`Operation.Origin`, `EmitRequest.Origin`, `NotifyRequest.Origin`, `AutomationEvent.Origin`,
`inbound.OriginFrom(ctx)`). `event.emit` además escribe dentro del **cuerpo** del evento
saliente (y por tanto firmado con él) `_origin` y `_hop` (saltos acumulados, +1 por emisión);
los nombres `_origin`/`_hop` están reservados. Un endpoint de entrada los lee y los quita
antes de validar el esquema (valores mal formados: `422 invalid_origin`) y responde
`200 {"status":"ignored"}` sin efecto (recibo `rejected`/`origin_loop`) si el origen es su
propio contrato, coincide con `ignore_origins` (exacto o `prefijo*`) o `_hop >= Config.MaxHops`
(3 por omisión). Ya **no** se confía en ninguna cabecera (iría sin firmar). El emisor/entrega
debe enviar el cuerpo tal cual; las suscripciones de salida pueden excluir el origen con
`inbound.OriginExcluded(origin, []string{"inbound:*"})`. Contratos de ida y vuelta (A emite
hacia B y B hacia A) deben listarse mutuamente en `ignore_origins`; el límite de saltos es la
red de seguridad si no lo hacen.

**`record.create` y reintentos.** `record.create` exige `idempotency_field`: columna del
modelo (no mapeada) donde se guarda `<recibo>:<índice>`. La creación corre como *upsert* sobre esa
columna, de modo que si la escritura confirmó pero falló el guardado del progreso (o hubo un
timeout tras el commit) el reintento encuentra la fila y no la duplica. En ese reintento
`result.created` es `false`. `Operation.IdempotencyKey` lleva la misma llave a los comandos.

**`record.delete` (un `<modelo>.deleted` del sistema origen).** Retira filas; no tiene id ni
modo "borra todo":

```json
{"do": "record.delete", "model": "customers", "match": ["external_ref"],
 "mapping": {"external_ref": "{{payload.id}}"},
 "when": {"path": "payload.kind", "eq": "deleted"}, "mode": "soft", "max_rows": 1}
```

- **Selección solo por `match`.** `match` es obligatorio, cada campo debe estar en `mapping` y el `mapping`
  no puede traer nada más (no se escribe nada); no existe selección por id ni sin filtro. Cada campo de `match`
  debe ser columna declarada del modelo (`CheckContract`). `defaults` e `idempotency_field` no aplican.
- **Mismo gate que `record.update`.** Corre por el mismo `Executor`: organización de la ruta firmada (otra
  organización es invisible: `not_found`), alcance entidad/sucursal y modelo dinámico resuelto. El host puede
  además cerrar la puerta con `DynamicExecutor.Authorize(ctx, org, modelo, accion)`: lista blanca de modelos de
  addons habilitados con escritura declarada y permiso `<modelo>.delete` de quien guarda la entrada. Se
  consulta ANTES de tocar el modelo (también para `create`/`update`/`upsert`); `false` responde `403
  forbidden`. Sin `Authorize` rige la regla histórica (la ruta firmada es la autorización).
- **`mode`.** `soft` (por omisión) llama a `dynamic.Service.Delete` (tombstone con `deleted_at`, hooks y
  evento canónico `deleted`); el modelo debe tener `deleted_at` (`TableMetadata.Audit.DeletedAt`), si no
  falla al registrar y en ejecución (`422 soft_delete_unsupported`): nunca cae a borrado físico. `hard` está
  **deshabilitado** salvo que el host entregue `DynamicExecutor.HardDeleter` Y liste el modelo en
  `HardDeleteModels`; si no, error al registrar y `403 hard_delete_disabled`.
- **Límite de filas.** `max_rows` (1 por omisión, techo 100 en el contrato; `DynamicExecutor.MaxDeleteRows`
  baja el techo del host, 100 por omisión). Si hay más coincidencias que el tope, NO se borra nada:
  `409 too_many_matches`.
- **Idempotente.** Sin coincidencias (ya borrado, otra organización) no es error: el paso queda `done` con
  `not_found: true` y el recibo `applied`. Con borrado, el paso trae `affected` y `record_id` (el primero).
  Pasos siguientes pueden usar `{{result.id}}` y `{{result.affected}}` (p. ej. `when result.affected eq 1`
  antes de un `event.emit`).
- **Contrato de `Executor` propio.** `Operation` trae `Route.Op = OpDelete`, `DeleteMode` y `MaxRows`; debe
  devolver `Outcome{Affected, RecordID}` o `Outcome{NotFound: true}`. `dynStore` ganó `Delete`.
- **Valores de `match` vacíos o nulos (fix de seguridad).** Un valor de `match` ausente o `null` falla con `422 match_value_missing`; una cadena vacía o solo espacios con `422 match_value_empty`, SIEMPRE antes de consultar (también en `record.update`/`record.upsert`). Motivo: la capa de consulta descarta un filtro `eq` sobre `""` (sin `WHERE`) y devolvería filas sin filtrar. Cero numérico y `false` siguen siendo valores válidos.
- **Registro.** `DynamicExecutor.CheckContract` exige `MaxDeleteRows <= 199` (la página de consulta se recorta a 200 y se lista `límite+1`) y rechaza modelos `append_only`. La razón de borrado obligatoria no está en el metadato del modelo: sigue fallando en ejecución como `422 rejected_by_model`. Limitación: tras un borrado, si el origen recrea una fila con el mismo `match` y se reintenta el recibo viejo, se borra la nueva (el borrado es por `match`, no por id).
- **Ejecutor propio.** Debe implementar `OpSupporter` (`SupportsOp(Op) bool`) y devolver `true` para `OpDelete`; si no, el contrato con `record.delete` se rechaza al registrar. `DynamicExecutor` la implementa.

**`record.action`, `record.transition`, `record.restore` (llevar un registro a cualquier estado del modelo).**
Tres acciones para que la ingesta mueva un registro a un estado o acción que el modelo YA declara, sin una
acción nueva por caso. Reutilizan lo que existe (`dynamic.Service.ExecAction`, la máquina de estados que valida
`Service.Update`, `Service.Restore`) y los muros de `record.delete`: filas elegidas SOLO por `match`
(obligatorio y no vacío; cada campo en `mapping` y el `mapping` sin nada más), organización y alcance de la ruta
firmada, `max_rows` (1 por omisión, techo 100 y `MaxDeleteRows`; más coincidencias = `409 too_many_matches` sin
tocar nada), valores de `match` ausentes/nulos/vacíos rechazados (`match_value_missing`/`match_value_empty`),
`Authorize` y el recibo idempotente (`replayed`). `record.delete` no cambia; activar/desactivar es un
`record.update {active:false}`.

```json
{"do": "record.action", "model": "orders", "match": ["external_ref"],
 "mapping": {"external_ref": "{{payload.id}}"},
 "action": "mark_paid", "params": {"reference": "{{payload.payment_ref}}"}, "on_ineligible": "noop"}
{"do": "record.transition", "model": "orders", "match": ["external_ref"],
 "mapping": {"external_ref": "{{payload.id}}"}, "to": "shipped"}
{"do": "record.restore", "model": "orders", "match": ["external_ref"],
 "mapping": {"external_ref": "{{payload.id}}"}}
```

- **`record.action {action, params, on_ineligible}`.** Por cada fila llama a `ExecAction`. **Lista blanca
  cerrada por omisión:** solo corren las acciones del modelo con `ingest: true` en el manifest (campo
  opcional de la acción; porque entre las acciones declaradas hay capturas de pago y operaciones
  destructivas). Quedan SIEMPRE fuera las que tienen `approval`, `supervisor_policy` o `confirm_typed` y las de
  `placement` `table`/`create` (el manifest v3 rechaza `ingest: true` combinado con ellas; el ejecutor lo
  vuelve a comprobar). `params` son plantillas y es el payload de la acción: al registrar, `CheckContract`
  rechaza una acción inexistente o sin `ingest: true` (`action_not_ingestable`), un `params` fuera de los
  `Fields` de la acción (solo `reason` se acepta siempre; tampoco la llave de idempotencia, que pone el kernel)
  y `max_rows > 1` si la acción no declara `idempotency`. `RequiresState` lo aplica `ExecAction` (sobre el
  `stage_field` o `status`/`state`): una fila que no lo cumple con `on_ineligible: noop` (por omisión) deja el
  paso `done` con `noop: true` y `reason: "state_not_eligible"`; con `fail`, `409 state_not_eligible`. Si la
  acción declara `Idempotency`, el kernel pasa `params[KeyField] = <recibo>:<acción>:<fila>`, así que un
  reintento reproduce la respuesta guardada en vez de volver a despachar. Si ya se despachó sobre varias filas y
  una falla, el reintento retoma el paso completo: use `max_rows: 1` o una acción con idempotencia. Errores:
  `422 action_not_ingestable`, `422 invalid_transition` (si la acción mueve la etapa), `422 action_declined`
  (el despachador respondió sin éxito; nunca se reenvía su mensaje), `409 idempotency_in_flight`. Una
  aprobación pendiente no puede ocurrir (la acción no tiene `approval`); si llegara, es un error de servidor
  (`approval_unexpected`), nunca un paso aplicado. Sin coincidencias: `404 record_not_found` (un evento que
  llega antes que su registro no se da por aplicado; se reintenta).
- **`record.transition {to}`.** Azúcar sobre `record.update` que escribe el `stage_field` del modelo (la columna
  NO se elige) y deja que `Service.Update` valide la transición y dispare sus hooks. Solo en modelos con
  máquina de estados activa y `to` debe ser un estado de la máquina (`CheckContract`). Se juzgan todas las filas
  antes de escribir ninguna: una transición no declarada es `422 invalid_transition` y no escribe nada;
  `from == to` es `done` con `noop: true` (`reason: "already_in_state"`). Sin coincidencias: `404
  record_not_found`.
- **`record.restore`.** Inverso de borrado: busca entre las lápidas (mismo `match`, organización y alcance de
  `record.delete`; `GormTombstones`/`DynamicExecutor.Tombstones`, obligatorio al registrar) y llama a
  `Service.Restore`. El modelo debe tener `deleted_at` y no ser `append_only`. Sin lápidas: `done` con
  `not_found: true` (como `record.delete`), o `noop: true` con `reason: "already_live"` si la fila ya está viva
  (un reintento converge). Una fila viva con el mismo `match` junto a la lápida cuenta contra `max_rows`: con
  el tope por omisión no se restaura para no duplicar la llave (`409 too_many_matches`).
- **`Authorize`.** Recibe `action:<key>`, `transition:<estado>` o `restore` como acción (sin `Authorize`
  decide el host, como siempre). Se consulta antes de leer filas.
- **Resultado para los pasos siguientes.** `result.id` (primera fila), `result.affected` (filas cambiadas) y
  `result.noop`; el recibo guarda `noop`/`reason`/`affected` por paso.
- **Dry-run (contrato preparado).** `inbound` aún no tiene modo dry-run. `Operation.DryRun` ya lo respeta el
  ejecutor para `record.action`: resuelve filas, estado y parámetros y devuelve `Outcome.WouldDispatch`
  (`action`, `record_id`, `rows`, `state`, `eligible`, `params`) SIN llamar nunca a `ExecAction`. Cuando exista
  el modo, solo hay que poner `DryRun` en la `Operation`.
- **Catálogo de destinos (`/targets`).** `Target` gana `actions_available[]` (`key`, `label`, `params`,
  `requires_state`, `permission`: solo las acciones `ingest: true` ejecutables; `IngestActions(modelo, defs)`),
  `stage` (`field`, `stages[]`, `transitions[]`, o `null`; `StageOf`), `restorable` (`Restorable(meta)`) e
  `ingest_cursor` (el `Model.ingest.cursor` del manifest, lo pone el host). Fixture del SDK:
  `inbound/testdata/targets.json`.
- **Ejecutor propio.** Debe implementar `OpSupporter` y devolver `true` para `OpAction`, `OpTransition` y
  `OpRestore` según las acciones que declaren los contratos (mismo fail-closed al registrar). Recibe
  `Operation{Route.Op, Route.Match, Values (el match), ActionKey, Params, OnIneligible, To, MaxRows, DryRun}` y
  devuelve `Outcome{RecordID, Affected, Noop, Reason, NotFound}`. `dynStore` (interno) ganó `ExecAction`,
  `Restore`, `ActionDef` y `ActiveStageMachine` (`dynamic.Service` ya los expone).

**Guarda de orden `guard` (eventos tardíos o reintentados).** Cada acción `record.*` puede declarar
`"guard": {"field": "source_updated_at", "path": "payload.updated_at", "op": "gte"}`. `field` es una columna
escribible del modelo, `path` una ruta `payload.*` con un instante RFC 3339 o un entero (versión) y `op` es
`gte` o `gt`. Se aplica el evento solo si `evento op fila[field]`; si no, el paso queda `skipped` con
`error_code: "stale_event"`, no se escribe nada y el recibo sigue `applied` (reutiliza `ingest.CompareCursor`
para fechas y enteros).

- **Opt-in por dato, solo por ausencia.** Si el valor del payload está ausente o es `null` explícito, o
  `fila[field]` es `null`, el evento se aplica. Un valor PRESENTE que no es instante RFC 3339 (con zona) ni
  entero (`""`, `"N/A"`, `"2024-01-01"`, `"5"`, `1.5`, booleano) es un fallo del productor y NO se aplica:
  `422 guard_value_invalid` antes de cualquier efecto (el recibo queda `failed`).
- **Valores del futuro.** Un instante mayor que ahora + `DynamicExecutor.GuardMaxSkew` (24 h por omisión;
  negativo desactiva la cota) falla con `422 guard_value_future`: firmado con el año 9999 congelaría la fila y
  todo evento legítimo posterior quedaría `stale_event`. Los enteros (versiones) NO se acotan porque no hay un
  reloj contra el cual compararlos; ponles `maximum` en el JSON Schema del contrato.
- **Lectura de la BD.** Antes de comparar se normalizan los valores de la fila: `[]byte`, texto SQL con offsets
  cortos (`+00`) o sin zona (UTC), `time.Time` en cualquier zona y `"5"` de una columna entera. Los instantes
  se comparan al microsegundo (la precisión de un `timestamp` de Postgres: un evento con nanosegundos empata
  con su copia guardada). Una columna `null` significa «sin valor» y aplica; un valor NO nulo ilegible o de otra
  clase que el del evento (entero contra instante, texto basura) falla cerrado con `409
  guard_row_value_unreadable` y no escribe nada. Los campos de `match` se comparan normalizados (uuid como
  `[16]byte`/`[]byte`/mayúsculas, instantes con zona).
- **Empates.** Con `gte` un evento igual al valor guardado se aplica; con `gt` no. Con segundos de precisión
  el empate es frecuente: un `updated` del mismo segundo que el borrado resucita la fila con `gte`. Recomendación:
  `gte` en `record.delete` (converge si se reintenta) y, en `upsert`/`create`, `gt` cuando el origen pueda emitir
  borrado y alta en el mismo instante (reenviar un evento ya aplicado queda `stale_event`, que es inocuo) o,
  mejor, un entero de versión.
- **Fila viva** (`update`/`upsert`/`delete`): compara contra `fila[field]`. Un `record.create` (que corre como
  upsert sobre su `idempotency_field`) no se compara con la fila hallada por esa columna: es su propio intento.
- **Sin fila viva** (`upsert`/`create`, modelo con borrado lógico): busca la lápida más reciente (borradas,
  misma organización, mismo `match`, la sucursal de la operación —`scope.branch` o el `branch_id` mapeado— y el
  alcance de la entidad). Todos esos filtros se aplican en SQL antes del `Limit` y se ordena por borrado más
  reciente: lápidas de otra sucursal no bloquean ni desplazan la propia. Si tiene `field` y el evento es más
  viejo, queda `stale_event` y la fila NO resucita; si no, se crea. Si el `create` pierde una carrera por índice
  único, el ganador se relee y pasa por la misma guarda y la misma búsqueda de lápidas antes de actualizarlo.
- **`record.delete` con guarda.** Con la fila viva y el evento viejo no borra. Si no, escribe
  `fila[field] = valor del evento` y hace el borrado lógico, de modo que la lápida queda sellada con la fecha
  del borrado (un `updated` posterior más viejo queda `stale_event`). El sello es un `UPDATE` por
  `dynamic.Service` (hooks before-update, validación, auditoría; NO publica `updated`, ver «Atomicidad»), así que el borrado lógico con guarda exige que
  `Authorize` conceda `"delete"` Y `"update"` (`403 forbidden` antes de cualquier efecto); sin valor de guarda
  o en modo `hard` solo hace falta `"delete"`.
- **Atomicidad y varias réplicas (parte B).** La garantía NO es el candado en proceso (solo reduce contención entre
  las peticiones de un proceso que comparten `match`; un delete con otro `match` que el upsert cae en otra cubeta)
  sino la base de datos: la escritura con guarda va por `dynamic.Service.UpdateGuarded` y el borrado con guarda por
  `DeleteSealed`. Ambos abren una transacción, bloquean la fila (`SELECT id … FOR UPDATE` filtrado por organización en Postgres y MySQL/MariaDB: toma el
  candado sin escribir una versión nueva de la fila, así que no dispara triggers de `UPDATE` ni CDC y quien llegue
  después espera al `COMMIT`; una fila de otra organización no se encuentra y no se bloquea. SQLite no tiene
  candados de fila: ahí queda el `UPDATE` sin cambio de la columna guardada, que toma el de escritura de la BD), la
  releen y recién entonces aplican `evento op fila[field]` sobre el valor confirmado. Si otra réplica escribió un
  evento más nuevo entre la lectura y la escritura, el evento viejo queda `stale_event` y no escribe nada ni publica
  eventos; si la fila desapareció entre ambos pasos, el ejecutor la vuelve a juzgar desde cero (hasta 3 veces;
  luego `409 concurrent_modification`). La comparación se hace en Go y no en `WHERE campo <= ?` porque la columna
  puede ser un instante guardado como texto, un entero o `NULL`, y la guarda falla cerrada ante un valor ilegible
  (`guard_row_value_unreadable`); el candado de fila es lo que vuelve segura esa decisión. El sello y el borrado
  lógico van en UNA transacción: si falla cualquiera de los dos se revierte el sello, no queda una fila viva con
  el valor del evento y el reintento converge también con `gt`. **Eventos de `DeleteSealed`: solo `deleted`.** El sello es un detalle de implementación del
  borrado: al publicar, la fila ya está borrada y un suscriptor no podría releerla, así que NO se publica `updated`
  ni corre el hook after-update (antes de este cambio se publicaban `updated` + `deleted`). El `deleted` sale
  después del `COMMIT` y solo si tuvo éxito; su `before` es la fila ya sellada (la columna guardada trae el valor
  del evento) y no trae `after`, igual que cualquier `deleted`. Los hooks before-update, la validación y los
  sellos de auditoría del sello sí corren (dentro de la transacción) y los after-delete, la limpieza de archivos
  y de pivotes son los de `Delete`. Los pasos que `update` hace tras el `COMMIT` (sincronizar pivotes, upsert de
  extensiones, líneas de documento) son de mejor esfuerzo aquí: si uno falla se registra en el log y el borrado
  sigue (`deleted` y limpieza); la fila ya no existe y un reintento solo respondería `not_found`.
  **Conexiones.** Mientras la transacción está abierta TODA consulta (unicidad y referencias de la validación,
  extensiones, introspección de columnas) usa la conexión de la transacción: una escritura con guarda nunca
  necesita una segunda conexión, así que con un pool de una conexión no se bloquea y con N escrituras
  concurrentes sobre un pool de N no hay inanición. **Orden de los hooks.** El juicio que vale es el hecho bajo
  el candado; para no correr los hooks before-delete de un borrado que ya es viejo, `DeleteSealed` hace antes
  de ellos una lectura barata y responde `ErrStaleWrite` si la fila confirmada ya es vieja. Si otra réplica la
  hace vieja entre esa lectura y el candado, los before-delete sí corrieron y el borrado se rechaza igual con
  `ErrStaleWrite`: un hook before-delete debe tolerar un borrado que luego se rechaza (ya debía, porque
  `checkDeletable` o la transacción también pueden fallar después). El hard delete con guarda sigue juzgándose
  solo al listar, fuera del candado. Bajo `REPEATABLE READ` un escritor concurrente que ya cambió la fila puede
  recibir `40001` (serialization failure) en vez de esperar: es propio del nivel de aislamiento. Para crear sigue valiendo el índice único sobre `match`: el
  perdedor de la carrera relee y pasa por la guarda.
- **Registro.** `DynamicExecutor.CheckContract` exige que `field` sea columna escribible (`WritableColumns`) y
  que esté en `mapping` (salvo `record.delete`, que solo exige la columna): `guard_field_not_mapped`. La columna
  guardada se lee de la fila como JSON, así que su campo en el struct del modelo no puede llevar `omitempty` ni
  `omitzero` (salvo que sea puntero) ni `json:"-"`: con `omitempty` y valor 0 la columna desaparece de la fila,
  se lee como `NULL` y nunca bloquea. `dynamic.Service.CheckGuardField(ctx, model, field)` lo comprueba y el
  registro falla con `guard_field_not_comparable` (`UpdateGuarded`/`DeleteSealed` lo verifican también en cada
  llamada con `ErrInvalidInput`). Un
  `upsert`/`create` con `match` sobre un modelo con borrado lógico exige poder leer lápidas
  (`guard_tombstones_unavailable`): el host entrega `DynamicExecutor.Tombstones` (`TombstoneFinder`:
  `Tombstones(ctx, TombstoneQuery)`, que debe aplicar en la BD organización, `match`, sucursal y alcance antes
  del límite) porque `dynamic.Service` lista solo filas vivas. `GormTombstones{DB, Table, OrgColumn}` es una
  implementación: toma la columna de borrado de `Audit.DeletedAt` del metadato, `OrgColumn` (por omisión
  `organization_id`; el metadato no la nombra) y valida los identificadores. `field` no puede ser campo de `match`.
- **Ejecutor propio.** Debe implementar `GuardSupporter` (`SupportsGuard() bool`), leer `Operation.Guard` y
  devolver `Outcome{Stale: true}`; si no, el contrato con `guard` se rechaza al registrar.

**Defaults solo al crear.** `defaults` (en `route` y en las acciones `record.*`) viajan como
`Operation.CreateDefaults` y el ejecutor los aplica únicamente al crear la fila, nunca al
actualizar (un `status: new` no revierte el estado en cada upsert).

**Cerca de arriendo y alcance en la cadena.** `ReceiptStore.SaveProgress(ctx, id, attempt, steps) (owned, err)`
lleva la misma cerca que `FinishAttempt` (`WHERE id AND attempts AND status='processing'`) y
renueva el arriendo. La cadena lo llama antes y después de cada acción: si otro worker
reclamó el evento, aborta con `409 lease_lost` **antes** de aplicar el siguiente efecto y no
pisa el recibo del nuevo dueño (el cierre usa `FinishAttempt`). El alcance se declara por
acción (`scope.branch`/`scope.entity` en cada `record.*`): si falta en el evento, `422
scope_required` antes de cualquier efecto y antes de consumir el nonce (`PreparedVerifier`);
cada paso pasa `RequireBranch/RequireEntity` al ejecutor (entidad sin resolver -> `403
scope_unresolved`, rama fuera de alcance -> `403 scope_forbidden`). Al registrar
(`New`/`Check`/`RegisterContract`) `ContractChecker` valida también los `match` y el
`idempotency_field` de cada acción `record.*` contra las columnas del modelo.

**Cableado.** `Config.Emitter`, `Config.Notifier`, `Config.Automation`; `New`/`Check` fallan
al arrancar si una cadena usa una acción sin su puerto o un comando no registrado.

## Destinos escribibles (`SystemFields`, `WritableColumns`, `Target`)

Una sola fuente de verdad de «qué columnas puede escribir una entrada»:

- `inbound.SystemFields`: columnas que una entrada jamás escribe aunque el modelo las declare
  (`id`, `organization_id`, `tenant_id`, `branch_id`, `entity_id`, `created_by_id`,
  `created_by`, `updated_by`, `created_at`, `updated_at`, `deleted_at`). Es la unión de la
  lista histórica del kernel y de la compuerta del host OPS; el compilador de contratos
  (`forbidden`) y el catálogo la comparten.
- `WritableColumns(meta)`: columnas del modelo escribibles = fuera de `SystemFields` y sin
  `readonly`, `generated` ni `protected`. Devuelve `[]TargetColumn{Name, Label, Type, Required,
  Unique, Ref, Options, Default}`.
- `MatchCandidates(meta)`: conjuntos de columnas que identifican una fila (columnas únicas e
  índices únicos compuestos), ignorando `organization_id` (el ejecutor ya acota por ella) y
  contando índices parciales (`WHERE deleted_at IS NULL`). Descarta los conjuntos con columnas
  no escribibles. Los índices salen de `TableMetadata.UniqueIndexes`
  (`dynamic.DeriveUniqueIndexes(def)` los proyecta desde `ModelDefinition.Indices`; los únicos
  compuestos sin `where` de un manifest v3 viajan ahora también ahí, solo informativos: el DDL
  no los emite desde `Indices`). El host que arma su `TableMetadata` debe copiarlos.
- `Target` (JSON snake_case: `addon`, `addon_name`, `model`, `label`, `columns`,
  `match_candidates`, `soft_delete`, `actions`, `permissions{create,update,delete}`,
  `scope{branch,entity}`, `guard_candidates`) e interfaz `TargetSource{Targets(ctx, org)}`
  que implementa el host para alimentar el constructor de la UI. Fixture compartida con el SDK:
  `inbound/testdata/targets.json`.

**Registro.** `CheckContract` valida también las claves de `mapping` (de `route` y de las
acciones `record.create|update|upsert`) contra `WritableColumns`: mapear una columna de
sistema, de solo lectura, generada o protegida falla al arrancar con `ErrInvalidContract`.
