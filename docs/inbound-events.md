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
- **Upsert.** Se serializa por (org, modelo, valores de `match`) dentro del proceso. Entre réplicas **el modelo debe tener un índice único sobre los campos de `match`** (por organización): quien pierde la carrera falla al crear, relee y actualiza a la ganadora.
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

**`when`.** `{"path": "...", "eq": <escalar>}` o `{"path": "...", "exists": true|false}`;
falso = paso `skipped`.

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
