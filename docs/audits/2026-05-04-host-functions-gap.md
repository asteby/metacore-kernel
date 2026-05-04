# Auditoría host functions WASM — gap frente a la visión

- **Fecha:** 2026-05-04
- **Archivos auditados:**
  - [`runtime/wasm/abi.go`](../../runtime/wasm/abi.go) (convención ABI v1)
  - [`runtime/wasm/capabilities.go`](../../runtime/wasm/capabilities.go) (registro del módulo `metacore_host`)
- **Visión de referencia:** add-ons WASM verificados por hash que (a) consultan/mutan modelos a través del kernel (sin DSN propio), (b) publican en el event bus interno y (c) resuelven options de relaciones para llenar los selects que el frontend deriva del manifest.
- **Alcance:** sólo el inventario de host imports en el módulo `metacore_host`. No se modifica código en esta tarea — el output es la firma propuesta para cada nueva host function y su política de capabilities.

## Estado actual — host imports presentes

`registerHostModule` (`runtime/wasm/capabilities.go:46`) expone hoy **tres** funciones bajo el módulo `metacore_host`:

| # | Nombre       | Firma WASM                                                                 | Política / capability check                                  | Devuelve                                                  |
|---|--------------|----------------------------------------------------------------------------|--------------------------------------------------------------|-----------------------------------------------------------|
| 1 | `log`        | `(msgPtr i32, msgLen i32) -> ()`                                           | ninguna (always-on, prefijado con `addon=` e `installation=`) | nada                                                      |
| 2 | `env_get`    | `(keyPtr i32, keyLen i32) -> i64`                                          | ninguna (lee `invocation.settings`, scope per-instalación)   | `(ptr<<32)\|len` con el valor; `0` si la key no existe    |
| 3 | `http_fetch` | `(urlPtr, urlLen, methodPtr, methodLen, bodyPtr, bodyLen i32) -> i64`      | `security.Capabilities.CanFetch(url)` + SSRF guard (`security/context.go:67`) | `(ptr<<32)\|len` con JSON `{status, body}` o `{error, message}` |

ABI común (`abi.go:10-24`):

- Memory + `alloc(size i32) i32` exportadas por el guest.
- Convención de retorno: `i64` empaquetado `(ptr<<32) | len`. `0` ⇒ vacío/sin payload.
- Errores: el host serializa `{"error":"<code>","message":"<msg>"}` con `jsonError` (`capabilities.go:169`) y lo escribe en el guest. **No es** el envelope unificado `{success, data, meta}`; ver gap abajo.

## Capabilities que ya existen en `security.Capabilities` pero **no** tienen host import

`security/context.go` ya compila las siguientes políticas a partir del manifest del addon, pero nunca se invocan desde el módulo `metacore_host`:

| Capability kind (manifest) | Compila en `Capabilities` field | Método disponible              | Consumido por host import? |
|----------------------------|---------------------------------|--------------------------------|----------------------------|
| `db:read`                  | `dbRead []string`               | `CanReadModel(model)`          | **No** — falta `db_query`  |
| `db:write`                 | `dbWrite []string`              | `CanWriteModel(model)`         | **No** — falta `db_exec`   |
| `http:fetch`               | `httpHost []string`             | `CanFetch(url)`                | sí (`http_fetch`)          |
| `event:emit`               | `eventPub []string`             | `CanEmit(event)`               | **No** — falta `event_emit` |
| `event:subscribe`          | `eventSub []string`             | `CanSubscribe(event)`          | fuera de scope (pull-model) |

Es decir: la mitad de la matriz de seguridad ya está implementada pero los guests no pueden ejercerla. Hoy un addon WASM que necesite leer un modelo está obligado a salir por `http_fetch` contra el propio kernel — irónico y, peor, inseguro porque el SSRF guard rechaza loopback y tendría que abrirse explícitamente.

## Gap frente a la visión

Bloque ① — **explícitamente requeridos por la tarea**

| # | Host function pendiente | Para qué                                                                                                                           | Capability que reusa |
|---|-------------------------|------------------------------------------------------------------------------------------------------------------------------------|----------------------|
| 1 | `db_query`              | leer filas del modelo a través del kernel (filter/sort/page/search), sin que el addon vea SQL ni DSN                                | `CanReadModel`       |
| 2 | `db_exec`               | crear/actualizar/borrar/upsertear filas siguiendo el envelope del kernel                                                            | `CanWriteModel`      |
| 3 | `event_emit`            | publicar al event bus interno con guard de capability (no escapar el bus vía `http_fetch` contra un webhook)                        | `CanEmit`            |
| 4 | `options_resolve`       | resolver opciones de una relación (FK target → `[{value, label}]`) para los selects que el frontend deriva del manifest             | `CanReadModel` (sobre el modelo target del `Ref`) |

Bloque ② — **observaciones de coherencia** (no son host functions nuevas, pero el rewrite de las cuatro de arriba debe absorberlas)

- Los retornos actuales no usan el envelope `{success, data, meta}`. Las cuatro nuevas funciones **deben** envelopearse para que el guest tenga un único parser. Propuesta: `jsonError` se reemplaza por `jsonEnvelope(success bool, data any, meta any)`.
- `env_get` y `options_resolve` cumplen roles distintos y no deben fusionarse: `env_get` lee settings de la instalación (string opaco), `options_resolve` resuelve filas de un modelo referenciado.
- `event_emit` necesita un `eventID` retornable (uuid) para que el addon pueda correlacionar respuestas — alinear con `bus.Publish` cuando exista.

## Firmas propuestas

Todas siguen la convención ABI v1 (`abi.go:10`): pointers `i32`, retornos empaquetados `i64`. Todos los payloads que cruzan la frontera son JSON UTF-8 — el guest serializa con su SDK, el host parsea con `encoding/json`. Toda respuesta es el envelope `{success, data, meta}`; `meta` puede ir vacío.

### 1. `db_query`

```wat
(import "metacore_host" "db_query"
  (func $db_query
    (param $modelPtr i32) (param $modelLen i32)
    (param $queryPtr i32) (param $queryLen i32)
    (result i64)))
```

- **`model`** — string, ej. `"orders"` o `"addon_tickets.comments"`.
- **`query`** — JSON:
  ```json
  {
    "filter": { "...": "..." },
    "sort":   [{ "field": "created_at", "dir": "desc" }],
    "limit":  50,
    "offset": 0,
    "fields": ["id", "status"],
    "search": "abc"
  }
  ```
- **Política:** `inv.caps.CanReadModel(model)` → si falla, envelope `{success:false, data:{error:"forbidden", ...}}`.
- **Retorno:** `(ptr<<32)|len` con
  ```json
  { "success": true,
    "data":    [ { "id": "...", "...": "..." } ],
    "meta":    { "total": 123, "limit": 50, "offset": 0 } }
  ```
- **Notas de implementación:** el handler resuelve `model` contra el catálogo del kernel, aplica RLS por `installation_id`, y sirve via la misma capa que las rutas dinámicas — no se abre acceso directo al pool.

### 2. `db_exec`

```wat
(import "metacore_host" "db_exec"
  (func $db_exec
    (param $modelPtr   i32) (param $modelLen   i32)
    (param $opPtr      i32) (param $opLen      i32)
    (param $payloadPtr i32) (param $payloadLen i32)
    (result i64)))
```

- **`model`** — igual que `db_query`.
- **`op`** — uno de `"create" | "update" | "delete" | "upsert"`. Cualquier otro valor ⇒ envelope `{success:false, data:{error:"bad_request", ...}}`.
- **`payload`** — JSON. Para `update`/`delete`/`upsert` debe incluir el campo PK (default `id`):
  ```json
  { "id": "...", "fields": { "...": "..." } }
  ```
  Para `create`: `{ "fields": { ... } }`.
- **Política:** `inv.caps.CanWriteModel(model)`.
- **Retorno:** envelope con `data` = fila resultante (`create`/`update`/`upsert`) o `{ "deleted": 1 }` (`delete`).
- **Notas:** el kernel ejecuta dentro de una tx; el guest no maneja transacciones explícitas (consistente con la decisión de no exponer SQL).

### 3. `event_emit`

```wat
(import "metacore_host" "event_emit"
  (func $event_emit
    (param $eventPtr   i32) (param $eventLen   i32)
    (param $payloadPtr i32) (param $payloadLen i32)
    (result i64)))
```

- **`event`** — nombre canónico del evento, ej. `"orders.created"`. Sigue la convención del manifest (`event:emit` capability target).
- **`payload`** — JSON arbitrario; el bus lo trata como blob.
- **Política:** `inv.caps.CanEmit(event)`.
- **Retorno:** envelope con
  ```json
  { "success": true,
    "data":    { "event_id": "<uuid>", "subscribers": 3 },
    "meta":    {} }
  ```
- **Notas:** `subscribers` es informativo (cuántos handlers se notificarán). El handler interno empuja al bus con `installation_id`, `addon_key` y `correlation_id` agregados como headers — el addon no los suministra.

### 4. `options_resolve`

```wat
(import "metacore_host" "options_resolve"
  (func $options_resolve
    (param $refPtr   i32) (param $refLen   i32)
    (param $queryPtr i32) (param $queryLen i32)
    (result i64)))
```

- **`ref`** — modelo target de la relación, ej. `"users"` o `"addon_billing.plans"`. Se interpreta igual que `manifest.ColumnDef.Ref`.
- **`query`** — JSON:
  ```json
  { "search": "abc",
    "limit":  20,
    "value_field": "id",
    "label_field": "name" }
  ```
  Todo opcional. `value_field`/`label_field` por defecto a `id` y `display_field` resuelto del manifest del modelo target.
- **Política:** `inv.caps.CanReadModel(ref)`. Sin esta capability el addon no puede listar opciones (consistente con la decisión de tratar relaciones como lecturas).
- **Retorno:** envelope con
  ```json
  { "success": true,
    "data":    [ { "value": "<uuid>", "label": "Alice" }, ... ],
    "meta":    { "total": 8, "limit": 20 } }
  ```
- **Notas:** intencionalmente **no** acepta `filter` libre — los selects derivados del manifest no necesitan filtrado server-side complejo y abrirlo invitaría a usar `options_resolve` como un `db_query` paralelo. Si un addon necesita filtros ricos, usa `db_query`.

## Cambios colaterales que la implementación va a requerir

Estos no son host functions, pero el PR que añada las cuatro de arriba necesariamente los toca. Listados acá para que la PR no llegue con sorpresas:

1. **Helper `jsonEnvelope(success, data, meta)`** en `capabilities.go` que reemplace `jsonError`. `http_fetch` debe migrar para usarlo (breaking del shape de error que devuelve hoy — ningún addon lo consume aún, ventana abierta).
2. **Acceso al catálogo de modelos** desde `invocation`: hoy `invocation` lleva `addonKey`, `installation`, `settings`, `caps`, `logger`. Hay que agregar un `dao` o `kernelClient` que las cuatro nuevas funciones consultan. El registro lo hace `Host` al construir, no se filtra por contexto.
3. **Catálogo del bus** (`bus.Publish`) — si todavía no existe la abstracción, `event_emit` se bloquea hasta que aparezca. Si está, basta inyectarlo en `Host` análogo al `dao`.
4. **Tests**: cada función nueva necesita (a) un test que verifique que la capability falta ⇒ envelope `{success:false}` y (b) un test que verifique que la capability presente ⇒ envelope `{success:true}`. Patrón ya establecido en `wasm_test.go`.
5. **Documentación de SDK guest**: el binding del lado guest (Rust/Go/AssemblyScript) debe agregar wrappers tipados — fuera de scope de esta auditoría.

## Resumen

- **Host imports presentes:** 3 (`log`, `env_get`, `http_fetch`).
- **Host imports requeridos por la visión y faltantes:** 4 (`db_query`, `db_exec`, `event_emit`, `options_resolve`).
- **Capabilities ya compiladas pero sin host import:** `db:read`, `db:write`, `event:emit` (la wiring es trivial — los métodos `CanReadModel`/`CanWriteModel`/`CanEmit` existen en `security/context.go`).
- **Pre-requisito de implementación:** envelope unificado `{success, data, meta}` y acceso al catálogo de modelos + event bus desde `invocation`.
- **Riesgo si no se implementa:** los add-ons WASM siguen forzados a salir por `http_fetch` para hablar con el propio kernel, lo que (a) rompe el modelo de aislamiento, (b) duplica lógica de auth y (c) hace el event bus inalcanzable desde guest code.
