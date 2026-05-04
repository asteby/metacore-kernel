# Auditoría `manifest.ActionDef` — gap para `Trigger.Type=wasm` con `RunInTx`

- **Fecha:** 2026-05-04
- **Archivos auditados:**
  - [`manifest/manifest.go`](../../manifest/manifest.go) (`type ActionDef`, `type BackendSpec`, `type HookDef`)
  - [`manifest/validate.go`](../../manifest/validate.go) (`validateBackend`)
  - [`bridge/actions.go`](../../bridge/actions.go) (`signedActionInterceptor`)
  - [`runtime/wasm/wasm.go`](../../runtime/wasm/wasm.go) (`Host.Invoke`)
  - [`lifecycle/interceptor.go`](../../lifecycle/interceptor.go) (`ActionContext`, `InterceptorRegistry`)
- **Visión de referencia:** un addon declara una acción y elige cómo se ejecuta — webhook remoto (legacy), evento del bus interno, **o función WASM in-process verificada por hash**. Cuando la acción muta datos del propio modelo o de una tabla del addon, debe poder envolverse en una transacción única para que el rollback sea atómico junto a la mutación que la disparó.
- **Alcance:** sólo `manifest.ActionDef` y la cadena de dispatch que la consume. No se modifica código en esta tarea — el output es el inventario de campos que faltan y la propuesta de un struct `Trigger` para cubrir el gap sin romper addons existentes.

## Estado actual

`manifest.ActionDef` (`manifest/manifest.go:218-229`) declara hoy:

```go
type ActionDef struct {
    Key            string     `json:"key"`
    Name           string     `json:"name"`
    Label          string     `json:"label"`
    Icon           string     `json:"icon,omitempty"`
    Fields         []FieldDef `json:"fields,omitempty"`
    RequiresState  []string   `json:"requiresState,omitempty"`
    Confirm        bool       `json:"confirm,omitempty"`
    ConfirmMessage string     `json:"confirmMessage,omitempty"`
    Modal          string     `json:"modal,omitempty"` // slot name for a custom modal
}
```

Es decir: el struct sólo describe **la cara UI** (botón, modal y/o formulario con `Fields`). El **dispatch** está completamente implícito y siempre va por webhook:

1. El bridge resuelve `manifest.Hooks["<model>::<action>"]` para encontrar la URL (`bridge/actions.go:75-83`).
2. Si la URL existe + hay dispatcher, instala `signedActionInterceptor` (`bridge/actions.go:138-183`) que POSTea el payload firmado con HMAC.
3. Si la URL no existe, registra un interceptor no-op para que el botón siga apareciendo (`bridge/actions.go:130-134`).
4. **No hay otra rama**: aunque el manifest declare `Backend.Runtime="wasm"`, las acciones siguen yendo por webhook. La única ruta que activa WASM hoy son los hooks de ciclo de vida (no las acciones de UI).

`validateBackend` (`manifest/validate.go:106-138`) cruza `Hooks` ↔ `Backend.Exports` cuando `runtime=wasm`, pero **no** cruza `Actions[*].Key` ↔ `Backend.Exports`. Resultado: una acción no puede pedir explícitamente "ejecutame en WASM dentro de una transacción".

Otros precedentes del propio manifest que ya resolvieron piezas equivalentes y conviene reusar para no inventar naming nuevo:

- `HookDef` (`manifest.go:243-248`) ya tiene `Async bool` y `Priority int` — el `Trigger` puede heredar la convención.
- `HookTarget` (`manifest.go:251-256`) ya define `Type string` (`url` / `function` / `prompt`) — base directa para `Trigger.Type`.
- `BackendSpec` (`manifest.go:136-143`) ya define `Exports []string`, `MemoryLimitMB`, `TimeoutMs` — el `Trigger` para `Type="wasm"` puede sobrescribir el timeout pero reusar la lista de exports como single source of truth.
- `ToolDef` (`manifest.go:187-202`) ya tiene `Endpoint`, `Method`, `Timeout`, `CacheTTL` — confirma el patrón "campos de dispatch dentro del def de la acción".

La meta de esta auditoría es agregar **un único campo `Trigger *TriggerDef`** dentro de `ActionDef` que centralice el cómo y deje el resto de campos para describir el qué (UI + intent).

## Tabla `presente | falta | propuesta`

Bloque ① — **explícitamente requeridos por la tarea (Trigger.Type=wasm con RunInTx)**

| # | Capacidad | Presente hoy | Falta | Propuesta de campo |
|---|-----------|--------------|-------|--------------------|
| 1 | Tipo de dispatch explícito | No (siempre webhook implícito vía `manifest.Hooks`) | Sí | `Trigger.Type string` con enum: `webhook`, `wasm`, `event`, `none` (UI-only / link), `prompt` (LLM tool reuso). Default `webhook` si `manifest.Hooks` resuelve, si no `none`. Reusa el naming de `HookTarget.Type`. |
| 2 | Símbolo WASM a invocar | No (ni siquiera dispatch a WASM) | Sí | `Trigger.Function string` — debe estar en `manifest.Backend.Exports`. Validador cruza esto en `validate.go` igual que ya lo hace para hooks de ciclo de vida. |
| 3 | Envolver en transacción DB | No (`signedActionInterceptor` no abre tx) | Sí | `Trigger.RunInTx bool`. Cuando `true`: el kernel abre `db.Transaction(...)` antes de invocar el guest, expone host functions `db_query`/`db_exec` que usan `tx` en vez de `db`, commit on success / rollback on error o panic. |
| 4 | Plumb de `tx` a la invocación WASM | No (`Host.Invoke` recibe sólo `installation` + `payload`) | Sí | Extender `wasm.invocation` (`runtime/wasm/capabilities.go:22-28`) con `tx *gorm.DB` además de `orgID`, `userID`. Nueva firma sugerida: `Host.InvokeInTx(ctx, tx, installation, addonKey, funcName, payload, settings, principal)` — método paralelo, no breaking. |
| 5 | Aislamiento del payload de la acción | Parcial — `marshalActionBody` arma `{record_id, payload, hook, org_id}` (`bridge/actions.go:188-196`) pero asume webhook | Sí | Definir `WasmActionEnvelope` JSON con shape `{record_id, payload, principal:{org_id, user_id, locale}, action:{model, key}, idempotency_key}`. Mismo shape que el body del webhook + bloque `principal` para que el guest pueda autorizar sin re-querear. |
| 6 | Respuesta del guest → cliente UI | Parcial — webhook devuelve `string(respBody)` opaco | Sí | Estandarizar return como envelope kernel `{success, data, meta}` (memoria `feedback_kernel_handler_response_shape.md`). Cuando `Trigger.Type="wasm"` el host parsea el `i64` packed, des-marshalliza JSON, y si `success=false` hace rollback (sólo si `RunInTx=true`). |
| 7 | Idempotencia para retries con tx | No | Sí | `Trigger.IdempotencyKey string` (campo expr-style: `"$.payload.client_request_id"`). El kernel deduplica por `(installation_id, action_key, key)` durante 24h en una tabla `metacore_action_idempotency`. Imprescindible si commit ok pero respuesta se pierde — sin esto el cliente reintenta y duplica. |

Bloque ② — **derivados / detectados durante la auditoría**

| # | Capacidad | Presente hoy | Falta | Propuesta de campo |
|---|-----------|--------------|-------|--------------------|
| 8 | Fase de ejecución (before / after / instead-of) | Implícita: siempre `after` (interceptor pattern, `bridge/actions.go:1-9`) | Sí | `Trigger.Phase string` enum `before`, `after`, `instead_of`. `before`+`RunInTx` permite validar/transformar el row dentro de la misma tx que la mutación; `instead_of` sustituye al dispatcher CRUD del kernel (útil para acciones tipo "cancelar pedido" que tienen su propia escritura compleja). |
| 9 | Timeout per-action vs per-backend | Sólo `BackendSpec.TimeoutMs` global (`manifest.go:142`) | Sí | `Trigger.TimeoutMs int` (override). Default = `BackendSpec.TimeoutMs`. Necesario porque acciones interactivas suelen tener SLAs más cortos que jobs en background. |
| 10 | Memory cap per-action | Sólo `BackendSpec.MemoryLimitMB` global | Nice-to-have | `Trigger.MemoryLimitMB int`. Permite que una acción "exportar PDF" se permita más RAM que una "marcar leído". |
| 11 | Async / fire-and-forget | No (siempre síncrono en bridge actual) | Sí | `Trigger.Async bool` — reusa naming de `HookDef.Async`. Cuando `true` el kernel encola en `metacore_jobs` y devuelve `{success:true, data:{job_id}}`. **Mutuamente excluyente con `RunInTx=true`**: no se puede confirmar atomicidad si la ejecución es diferida — el validador rechaza la combinación. |
| 12 | Validación cross-field en validador | Parcial (`validateBackend` sólo cruza Hooks ↔ Exports) | Sí | Añadir en `validate.go`: si `action.Trigger.Type="wasm"` ⇒ `Backend.Runtime="wasm"` AND `action.Trigger.Function ∈ Backend.Exports`; si `RunInTx=true` ⇒ `Async=false` AND `Phase ≠ "after"` (ver nota 8); si `Type="event"` ⇒ `Trigger.Event` no vacío y declarado en `manifest.Events`. |
| 13 | Capability gating de la propia acción | No (cualquier user con permiso de modelo puede disparar) | Sí | `Trigger.RequiredCapability string` — clave de `Capability.Kind`. Permite "sólo admins pueden disparar refund" sin código en el guest. Distinto de `RequiresState` que filtra por estado del row. |
| 14 | Audit del invocation | No | Sí | `Trigger.Audited bool` — cuando `true` el bridge inserta en `metacore_audit` con `(action, principal, payload_hash, result_status, latency_ms)`. Imprescindible para acciones financieras / con `RunInTx`. |
| 15 | Despachar a evento del bus interno | No (sólo webhook) | Sí | `Trigger.Type="event"` + `Trigger.Event string` (key dentro de `manifest.Events`). Permite "click → emit → N consumers" sin acoplar a WASM. Consistente con el event bus que ya describe `feedback_kernel_handler_response_shape` y la auditoría de `dynamic-events`. |
| 16 | Retornar mutaciones del row al cliente | No (UI hace refetch a ciegas) | Sí | `Trigger.ReturnsRow bool` — cuando `true` el kernel responde `{success, data:{row, meta}}` con el row tras la mutación, ahorrando un round-trip. Para `RunInTx=true` el row se lee dentro de la misma tx para evitar lecturas sucias. |
| 17 | Prompt para confirmación contextual | Sólo `Confirm bool`+`ConfirmMessage string` (estáticos) | Nice-to-have | `Trigger.ConfirmExpr string` — expresión sobre el row (`row.amount > 10000`) para confirmar sólo en casos de alto riesgo. |
| 18 | Mapeo de errores a UX | No (errores raw del guest) | Sí | `Trigger.ErrorMap map[string]string` — `{"INSUFFICIENT_BALANCE": "errors.balance.low"}` para que el frontend traduzca códigos del guest a strings i18n. |
| 19 | Rate limit por usuario / org | No | Nice-to-have | `Trigger.RateLimit *RateLimitDef` con `PerUser int`, `PerOrg int`, `WindowSec int`. Crítico para acciones que llaman `http_fetch` a APIs caras. |
| 20 | Compensación on-rollback | No | Nice-to-have | `Trigger.CompensateFunction string` — símbolo WASM que se invoca si la tx hace rollback **después** del commit lógico (sagas). Marca para v2 — requiere outbox table. |

## Propuesta de struct `Trigger`

Se agrega un único campo `Trigger *TriggerDef` a `ActionDef` (puntero → omitido cuando no se setea preserva el comportamiento legacy webhook). Naming en línea con `HookTarget`, `BackendSpec` y `ToolDef`.

```go
// ActionDef declara una acción que la UI puede invocar sobre un row del modelo.
// La cara UI vive en (Label, Icon, Fields, Modal, Confirm*); el plano de
// dispatch vive en Trigger. Cuando Trigger es nil el kernel mantiene el
// comportamiento legacy: resolver `manifest.Hooks["<model>::<action>"]` y
// dispararlo como webhook firmado.
type ActionDef struct {
    Key            string      `json:"key"`
    Name           string      `json:"name"`
    Label          string      `json:"label"`
    Icon           string      `json:"icon,omitempty"`
    Fields         []FieldDef  `json:"fields,omitempty"`
    RequiresState  []string    `json:"requiresState,omitempty"`
    Confirm        bool        `json:"confirm,omitempty"`
    ConfirmMessage string      `json:"confirmMessage,omitempty"`
    Modal          string      `json:"modal,omitempty"`

    // Trigger declara cómo despacha el kernel cuando el usuario invoca la
    // acción. Nil = legacy webhook implícito vía manifest.Hooks.
    Trigger *TriggerDef `json:"trigger,omitempty"`
}

// TriggerDef es la unión discriminada por Type. Sólo los campos relevantes
// al Type elegido se leen — el validador rechaza combinaciones inválidas.
type TriggerDef struct {
    // Type selecciona el dispatcher.
    //   "webhook"    — POST firmado a `manifest.Hooks["<model>::<action>"]`
    //                  o a Trigger.URL si está set (override).
    //   "wasm"       — invoca Trigger.Function en el módulo del addon
    //                  (manifest.Backend.Runtime debe ser "wasm" y la
    //                  función debe estar en Backend.Exports).
    //   "event"      — emite Trigger.Event en el bus interno; los
    //                  consumers se resuelven en runtime.
    //   "none"       — UI-only; útil cuando la acción es sólo un link o
    //                  un modal sin reacción server-side.
    Type string `json:"type"`

    // Phase define cuándo corre la acción respecto a la mutación del row.
    //   "before"     — antes de persistir; el guest puede mutar el payload.
    //   "after"      — después de persistir (default).
    //   "instead_of" — reemplaza al dispatcher CRUD del kernel.
    Phase string `json:"phase,omitempty"`

    // RunInTx envuelve la invocación en una transacción DB. Sólo válido para
    // Type="wasm" con Async=false; el validador rechaza otras combinaciones.
    // Cuando true el kernel:
    //   - abre db.Transaction(...) antes de invocar el guest
    //   - expone host functions db_query/db_exec que usan tx en vez de db
    //   - commit si el guest devuelve {success:true}
    //   - rollback si el guest devuelve {success:false}, paniquea, excede
    //     timeout o memoria
    RunInTx bool `json:"run_in_tx,omitempty"`

    // Type="wasm"
    Function       string `json:"function,omitempty"`        // export name; debe estar en Backend.Exports
    TimeoutMs      int    `json:"timeout_ms,omitempty"`      // override de Backend.TimeoutMs
    MemoryLimitMB  int    `json:"memory_limit_mb,omitempty"` // override de Backend.MemoryLimitMB

    // Type="webhook"
    URL    string `json:"url,omitempty"`    // override de manifest.Hooks
    Method string `json:"method,omitempty"` // default POST

    // Type="event"
    Event string `json:"event,omitempty"` // debe estar en manifest.Events

    // Comportamiento general (cualquier Type).
    Async              bool              `json:"async,omitempty"`               // mutuamente excluyente con RunInTx
    IdempotencyKey     string            `json:"idempotency_key,omitempty"`     // expr sobre el payload, ej: "$.payload.client_request_id"
    RequiredCapability string            `json:"required_capability,omitempty"` // clave de Capability.Kind
    Audited            bool              `json:"audited,omitempty"`             // log en metacore_audit
    ReturnsRow         bool              `json:"returns_row,omitempty"`         // incluye el row mutado en data
    ConfirmExpr        string            `json:"confirm_expr,omitempty"`        // confirmación contextual (ej: "row.amount > 10000")
    ErrorMap           map[string]string `json:"error_map,omitempty"`           // {code → i18n key}
    Priority           int               `json:"priority,omitempty"`            // orden cuando múltiples triggers escuchan el mismo evento
}
```

## Recomendación de roadmap

1. **v1 (mínimo viable, no breaking):** agregar `TriggerDef` con `Type`, `Function`, `RunInTx`, `Phase`, `URL`, `Method`, `Event`, `Async`, `TimeoutMs`. Default a `webhook` legacy cuando `Trigger=nil`. Cubre el caso del enunciado (`Type="wasm"` + `RunInTx=true`) sin romper addons existentes.
2. **v1.1:** runtime — extender `wasm.Host` con `InvokeInTx` y agregar host imports `db_query`/`db_exec` que consultan `invocation.tx`. Bridge nuevo `wasmActionInterceptor` paralelo a `signedActionInterceptor`. Validador cross-checks (gap #12).
3. **v1.2:** `IdempotencyKey`, `Audited`, `ReturnsRow`, `RequiredCapability`, `ErrorMap`. Requieren tablas/servicios ya existentes (`metacore_audit`, capability service).
4. **v1.3:** `MemoryLimitMB` per-action (requiere instances per-action si se quiere strict — alternativa: validar al cargar y rechazar guests cuyo límite declarado supere el solicitado).
5. **v2 (breaking, MAJOR bump APIVersion 3.0.0):** deprecar `manifest.Hooks` para acciones (queda sólo para lifecycle hooks de modelo). Acciones declaran su dispatch siempre vía `Trigger`. Sagas / `CompensateFunction` (gap #20).

## Impactos en consumers

- **TS SDK (`@asteby/metacore-sdk`):** regenerar tipos (`npm run gen:manifest`). `TriggerDef` opcional → sin breaking. Conviene exponer un helper `defineWasmAction({key, function, runInTx, ...})` para evitar typos en strings.
- **Bridge (`bridge/actions.go`):** dividir `buildInterceptor` en una factory por `Trigger.Type`. El path legacy (sin `Trigger`) se mantiene literalmente igual; los nuevos paths `wasm` / `event` se montan como ramas adicionales.
- **WASM runtime (`runtime/wasm/wasm.go`, `capabilities.go`):** método nuevo `InvokeInTx` con firma extendida; nuevas host functions `db_query` / `db_exec` (cross-referencia con `2026-05-04-host-functions-gap.md` que ya identifica este mismo gap). El `invocation` carry sumará `tx *gorm.DB`, `orgID`, `userID` para que `db_*` puedan filtrar por organización (RLS-friendly).
- **Validador (`manifest/validate.go`):** sumar reglas — gap #12. Tests en `validate_test.go` por cada combinación (`Trigger.Type="wasm"` sin `Backend`, `RunInTx=true`+`Async=true`, `Function` no exportada, etc.).
- **Idempotencia:** nueva tabla `metacore_action_idempotency(installation_id, action_key, idem_key, response_jsonb, expires_at)` con índice único. Migration en `installer/`.
- **Audit:** reusa `metacore_audit` ya existente; el bridge inserta una fila con `(action, principal, payload_hash, status, latency_ms)` cuando `Trigger.Audited=true`.
- **Renovate / Changesets:** los pasos 1-3 son `minor` (`APIVersion` queda en 2.x); el paso 5 es `major` (bump a 3.0.0) y dispara nota de migración para todas las apps.

## Próximos pasos sugeridos (no en esta tarea)

- RFC corto en `docs/rfcs/` con un addon ejemplo (`refunds`) que use `Trigger.Type="wasm"`+`RunInTx=true` para validar la ergonomía end-to-end (UI → bridge → wasm → tx commit/rollback → response).
- PoC del paso 1+2 en `feature/action-trigger-v1` con tests en `manifest/validate_test.go` y un fixture WASM mínimo en `runtime/wasm/wasm_test.go` que ejercite `InvokeInTx` con un rollback voluntario del guest.
- Coordinar con `link` (que es hoy el principal consumer de bridge) para confirmar que el path legacy `Trigger=nil` no requiere ningún cambio en sus addons existentes.
- Cross-check con `2026-05-04-host-functions-gap.md`: las host functions `db_query` / `db_exec` que ahí se proponen son **prerequisito** del paso v1.1 de este roadmap — convendría implementar ambos audits en una sola feature branch.
