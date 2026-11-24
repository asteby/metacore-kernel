# Contrato de capacidad `ai.complete`

Referencia del contrato `ai.complete` del paquete `capability`. Diseño y motivos:
[RFC 0007](rfc/0007-capacidad-ai-complete.md).

A diferencia de `geo.*` o `commerce.*`, **ningún addon la provee**: la implementa el
**host** (`Contract.HostImplemented`), que corre la inferencia en su pasarela de IA
(OPS: el Hub, con la identidad Ed25519 de la instancia) y la cobra en **Pulse**, el
monedero de IA de la organización (1 Spark = USD 0.01). Un addon que declare
`ai.complete` en `provides_capabilities` no pasa la validación del manifiesto.

Los addons nunca llevan llaves de LLM.

## Reglas

| Concepto | Regla |
|---|---|
| **Permiso** | `{"kind":"ai:complete","target":"<feature>","reason":"<finalidad>"}`. `target` = clave de feature (`^[a-z][a-z0-9_]{0,47}$`, sin comodines); `reason` obligatorio (lo ve el administrador al instalar). Implica `capability:query ai.complete`. |
| **Límites** | Bloque opcional `ai` del manifiesto (`capability.AISpec`): `monthly_sparks_cap` del addon y, por feature, `max_tier`, `max_tokens`, `monthly_sparks_cap`, `personal_data`. Cero = sin límite propio: aplica la configuración del host (`capability.AIDefaults`). |
| **Cardinalidad / tiempo** | `one`, sin proveedor que elegir; tiempo límite `capability.AITimeout` (120 s). No es `Idempotent` en el sentido del Executor: `idempotency_key` es opcional pero recomendada. |
| **Estados** | Sin Pulse, tope alcanzado, presupuesto agotado, límite de tasa o Hub caído son **resultados** (`status`), nunca errores. Los únicos errores son `invalid_input` y `forbidden`. |
| **Quién paga** | La org del usuario o de la invocación. El addon no puede elegirla. Patrocinio solo por configuración del host y con autorización del Hub (ver RFC §6). |
| **Privacidad** | Hook de redacción del host antes de enviar (`capability.AIRedactor`, falla cerrada); `personal_data` pide redacción al Hub; el Hub no guarda texto de llamadas de addons; el host tampoco. |

## Manifiesto

```json
{
  "capabilities": [
    { "kind": "ai:complete", "target": "ticket_triage", "reason": "Clasificar y resumir tickets entrantes" }
  ],
  "ai": {
    "monthly_sparks_cap": 300,
    "features": [
      { "key": "ticket_triage", "max_tier": "fast", "max_tokens": 600, "monthly_sparks_cap": 120, "personal_data": true }
    ]
  }
}
```

Validación (`manifest.Validate`, `v3.Validate`, misma función `capability.ValidateAIDeclaration`):
cada feature de `ai.features` debe estar concedida por un `ai:complete`, sin duplicados;
`max_tier` ∈ `fast | balanced | heavy`; topes y `max_tokens` ≥ 0; el tope de una feature
no supera el del addon. `manifest.Manifest.AIPolicy()` da la política resultante.

## Formas compartidas

### AIMessage

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `role` | string: system \| user \| assistant | sí | |
| `content` | string | sí | texto del turno |

### AIResponseFormat

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `type` | string: text \| json_object \| json_schema | sí | con JSON, el host valida que la respuesta sea un objeto JSON |
| `name` | string | | etiqueta de un json_schema |
| `schema` | object | | JSON Schema; solo con `json_schema` |

### AIUsage

| Campo | Tipo | Req. |
|---|---|---|
| `input_tokens` | integer | sí |
| `output_tokens` | integer | sí |

## Contrato

### `ai.complete`

Consulta · cardinalidad **one** · implementada por el host · tiempo límite **120 s**.

Entrada (`capability.AIRequest`):

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `feature` | string, patrón `^[a-z][a-z0-9_]{0,47}$` | sí | feature concedida por `ai:complete`; atribuye el gasto y elige los límites |
| `messages` | array<AIMessage> | uno de messages / prompt | al menos un turno `user` |
| `prompt` | string | uno de messages / prompt | un solo turno de usuario |
| `system` | string | | instrucción de sistema, va primero |
| `tier` | string: fast \| balanced \| heavy | | recortado al `max_tier` de la feature (y del Hub); vacío = defecto del host o, si no hay, el `max_tier` de la feature |
| `response_format` | AIResponseFormat | | |
| `max_tokens` | integer | | recortado al `max_tokens` de la feature; 0 = el de la feature o el del host |
| `temperature` | number | | 0..2 |
| `idempotency_key` | string | | ≤ 128; misma clave = mismo resultado, se cobra una vez |

La org, el addon, el usuario y la patrocinadora **no** son entrada: los pone el host.

Salida (`capability.AIResult`):

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `status` | string: ok \| insufficient_pulse \| quota_exceeded \| budget_exceeded \| rate_limited \| disabled \| no_wallet \| unavailable \| invalid_output | sí | ver tabla de estados |
| `content` | string | | respuesta (status ok) |
| `json` | object | | la respuesta decodificada con un `response_format` JSON |
| `usage` | AIUsage | sí | |
| `cost_sparks` | integer | sí | Sparks de Pulse debitados (0 en replay o sin llamada) |
| `model` | string | | informativo |
| `request_id` | string | | id del Hub |
| `message` | string | | motivo legible cuando status ≠ ok |
| `retry_after_seconds` | integer | | con `rate_limited` |
| `billed_to` | string: org \| sponsor | | |
| `replay` | boolean | | repetido por `idempotency_key` |

Estados:

| `status` | Origen | Qué hace el addon |
|---|---|---|
| `ok` | — | usa `content` / `json` |
| `insufficient_pulse` | Hub 402 | «sin créditos de IA este mes»; sigue sin IA |
| `quota_exceeded` | host: tope del addon o de la feature | igual |
| `budget_exceeded` | Hub 429 (presupuesto en dinero de la org) | igual |
| `rate_limited` | Hub/host 429 | reintentar tras `retry_after_seconds` |
| `disabled` | Hub `budget_disabled` / `banned`, IA apagada en el host | ocultar la función |
| `no_wallet` | Hub: la org no tiene monedero | igual que `disabled` |
| `unavailable` | Hub 5xx, red, host sin pasarela, redacción fallida | reintento opcional; nunca bloquear |
| `invalid_output` | el modelo no devolvió el JSON pedido (los Sparks sí se gastaron) | reintentar con otra clave o seguir sin IA |

Errores (`Contract.Errors`): `invalid_input` (entrada fuera del contrato) y `forbidden`
(feature no concedida, addon no instalado o sin addon que llama).

## Piezas del kernel para el host

| Pieza | Para qué |
|---|---|
| `capability.AIProvider` | interfaz que implementa la pasarela del host (OPS: sobre `services/hubai`) |
| `capability.AIPolicySource` | política por (org, addon): manifiesto + configuración de la org (`manifest.Manifest.AIPolicy`, `capability.LowerCap`) |
| `capability.AIUsageMeter` | gasto por (org, addon, feature, mes UTC) para los topes (`capability.AIPeriod`) |
| `capability.AIRedactor` | hook de redacción previa, falla cerrada |
| `capability.AIGate` | el flujo completo: valida → política → límites → topes → redacción → proveedor → JSON → medición y auditoría (sin texto) |
| `capability.AIHostQuerier` | contesta `ai.complete` en el import wasm `capability_query` y pasa lo demás al querier del host |
| `security.Capabilities.CanCompleteAI` | comprobación directa del permiso `ai:complete` |
| `capability/aitest` | dobles de prueba: `Provider`, `Meter`, `Policies` |
| `guest.AIComplete` | ayudante TinyGo para backends wasm |

Cableado mínimo en un host:

```go
gate := &capability.AIGate{
    Provider: myGateway,               // capability.AIProvider
    Policies: myInstalledAddons,       // capability.AIPolicySource
    Meter:    myCounters,              // capability.AIUsageMeter (nil = sin topes del host)
    Redact:   myRedactor,              // opcional
    Defaults: capability.AIDefaults{Tier: cfg.Tier, MaxTokens: cfg.MaxTokens, MonthlySparksCap: cfg.AddonCap},
    Logf:     log.Printf,
}
wasmHost.WithCapabilityQuerier(capability.AIHostQuerier{Inner: existingQuerier, Gate: gate})
// HTTP del frontend: gate.Complete(ctx, capability.AICall{Org: org, Addon: addon, User: user, Request: req})
```

## Clientes no-OPS

Un cliente que no corre el kernel (Laravel) habla directo con el Hub con el mismo protocolo
firmado: [ai-cable-cliente-no-ops.md](ai-cable-cliente-no-ops.md).
