# RFC 0007 — Capacidad `ai.complete`: IA estándar para cualquier addon, cobrada en Sparks (Pulse)

- **Estado:** propuesto. Este documento es el diseño; la implementación de referencia va en PR separados (kernel, OPS, SDK) que lo citan.
- **Fecha:** 2026-11-24
- **Repos afectados:** `metacore-kernel` (contrato, validación de manifiesto, permiso, compuerta), `ops` (implementación del host sobre `services/hubai`), `metacore-sdk` (`useAI()` / `ai.complete()`), `hub` (`POST /v1/ai/complete` extendido, Sparks), clientes no-OPS (7 Leguas, Laravel).
- **Relacionado:** `hub/docs/SPARKS.md` (Pulse y Ember), `hub/docs/ABY-AI.md` (`/v1/ai/complete`, firma Ed25519), `hub/docs/AI-COMPLETE-CONTRATO.md` (extensión aditiva del endpoint, en curso en la rama `feat/soporte-escalado` del Hub), [capabilities.md](../capabilities.md), [capabilities-geo.md](../capabilities-geo.md), [RFC de pagos](../RFC-PAYMENTS-STANDARD.md) (contratos implementados por el host), [permissions.md](../permissions.md).

## 0. Resumen

Hoy la IA solo la usan piezas del propio host (Aby, BI Pro, imágenes, triage de soporte), cada una con su integración. Un addon (tickets, CRM, inventario…) que quiere «resumir este ticket» o «clasificar este correo» no tiene camino estándar: tendría que traer su propia llave de LLM (prohibido: rompe el control de costos y la facturación) o pedirle al host un endpoint a medida.

Este RFC define **`ai.complete`**: una capacidad **implementada por el host** (no por un addon proveedor) que cualquier addon declara en su manifiesto y usa igual desde su frontend (SDK) o su backend wasm (`capability_query`). El host la resuelve contra el Hub con la identidad Ed25519 de la instancia y el Hub cobra en **Pulse**, el monedero de IA de la cuenta (1 Spark = USD 0.01).

| # | Decisión |
|---|---|
| D1 | **Una sola capacidad, `ai.complete`, implementada por el host.** Mismo registro de contratos que `geo.*` y `commerce.*` (tipos, validación, documentación), con la marca *host-implemented* del ledger de pagos: ningún addon puede declararla en `provides_capabilities`. |
| D2 | **Permiso `ai:complete <feature>` en `capabilities[]`**, con `reason` obligatorio (la finalidad, que ve el administrador al instalar). Sin permiso para esa feature no hay llamada. |
| D3 | **Límites en un bloque `ai` del manifiesto:** tope mensual de Sparks por addon y por feature, tier máximo, `max_tokens` máximo y si la feature manda datos personales. Nada de límites fijos en código: lo que el addon no declara lo pone la configuración del host. |
| D4 | **Sin saldo no es una excepción.** `insufficient_pulse`, tope del addon, tope de la org, límite de tasa o Hub caído son **resultados** (`status`) que el addon pinta («sin créditos de IA este mes») y con los que sigue funcionando. Solo un error de programación (entrada inválida, feature no declarada) es un error. |
| D5 | **El cobro lo hace solo el Hub.** El host no calcula precios: manda `addon_key`, `feature` e `idempotency_key`, y el Hub devuelve `cost_sparks`. El host solo **cuenta** lo gastado por addon para aplicar su tope. |
| D6 | **Nadie fuera del Hub tiene llaves de LLM**: ni instancias, ni addons, ni clientes no-OPS. Todos hablan el mismo protocolo firmado. |
| D7 | **Privacidad por diseño:** un hook de redacción previo en el host, `redact_pii` hacia el Hub y **cero texto de conversación guardado en el Hub** para llamadas de addons (solo metadatos de uso). |
| D8 | **Patrocinio opcional y apagado por defecto:** otra cuenta (p. ej. la organización «asteby» para su mesa de atención) puede pagar la IA de una feature; se configura, nunca se codifica, y lo autoriza el Hub, no la instancia. |

## 1. Lo que ya existe (evidencia)

- **Hub:** `POST /v1/ai/complete` (`hub/backend/internal/api/ai_complete.go`): firma Ed25519 de instancia (`message = sha256(body || "\n" || timestamp)`, `X-Signature: ed25519:<b64>`, `X-Signature-Timestamp`, `X-Instance-Pubkey` en hex), cadena de proveedores barata primero, tiers `fast | balanced | heavy`, uso en `ai_usage_events`, débito de Pulse (`enforceAIBudget`, `ai_budget.go`) con `insufficient_pulse` (402), `budget_exceeded` y `budget_disabled` (429), `banned` (403), `unavailable` (503). Errores con forma `{"error":{"code","message"}}`.
- **Extensión aditiva en curso (Hub, `feat/soporte-escalado`):** `ai.Request` ya trae `addon_key`, `feature`, `idempotency_key`, `response_format` (`json_object`) y `redact_pii`; la respuesta gana `usage`, `cost_sparks`, `model`, `request_id`; `recordAIUsageEvent(..., keepText=false)` deja vacíos `query_preview`/`reply_preview`. Este RFC **se alinea** con esos nombres; no los redefine.
- **Sparks (`hub/docs/SPARKS.md`):** Pulse = solo IA, se reinicia cada mes (500 de regalo); Ember = addons de pago y nunca financia IA.
- **OPS:** `services/hubai` (cliente firmado de `/v1/ai/complete`), `services/ai_hub_text.go` (`GenerateText`, usa `hubai`), `services/hubsparks` (monedero; `GET /api/sparks/wallet` ya expone Pulse), `services/bi/hub_bridge.go` (BI Pro), `models.UsageMetric` (contador mensual por org y métrica, sin escritores hoy), `POST /api/capabilities/query` (consultas de capacidad desde la UI) y el import wasm `capability_query` con `Query.Caller` sellado por el host.
- **Kernel:** registro de contratos (`capability.Register`, `Lookup`), permisos `capability:query`, `edge:command`, `domain:manage` como precedentes de permisos con objetivo, y contratos implementados por el host (`payments/ledger.HostQuerier`) como precedente de «el host contesta en lugar de un addon proveedor».
- **SDK:** `HostApi.capability(key, input)` (`@asteby/metacore-sdk`) y `createHostApi` (`@asteby/metacore-runtime-react`).

## 2. Lo que falta (problema)

1. **No hay contrato de IA para addons.** Cada pieza que usa IA arma su propio request a `hubai` o directamente al Hub.
2. **No hay declaración ni consentimiento.** Un addon no puede decir «uso IA para esto», ni el administrador verlo al instalar, ni el host negarlo si no lo declaró.
3. **No hay atribución ni tope por addon.** Todo el gasto cae en la org sin saber qué addon lo hizo; un addon con un bucle puede vaciar el Pulse del mes de toda la cuenta.
4. **El fallo de saldo se trata como excepción.** `hubai` convierte 402 en `ErrNotEntitled` y todo lo demás en `ErrUnavailable`: se pierde el `code` (no se distingue `insufficient_pulse` de `budget_exceeded` ni de `rate_limited`).
5. **No hay camino estándar para clientes no-OPS** (7 Leguas en Laravel): cada uno reimplementaría la firma y el manejo de errores.
6. **Hay duplicación en OPS** (ver §10): tres transportes firmados al Hub y dos clientes distintos de `/v1/ai/complete`.

## 3. Flujo y quién cobra a quién

```
 Addon (frontend)                      Addon (backend wasm)
   useAI().complete(req)                 guest.AIComplete(req)
        │ POST /api/ai/complete            │ capability_query {capability:"ai.complete"}
        │ {addon, ...req}                  │ (Caller = addon, sellado por el host)
        ▼                                  ▼
 ┌──────────────────────── Host (OPS) ─────────────────────────┐
 │ capability.AIGate                                           │
 │  1. valida la entrada (contrato ai.complete)                │
 │  2. política del manifiesto: ai:complete <feature>, límites │
 │  3. tope mensual del addon / feature (usage_metrics)        │
 │  4. hook de redacción (opcional, configurable)              │
 │  5. AIProvider → services/hubai (firma Ed25519)             │
 │  6. registra el gasto (usage_metrics) y la auditoría        │
 └───────────────────────────┬─────────────────────────────────┘
                             │ POST /v1/ai/complete (firmado)
                             │ {org_id, addon_key, feature, idempotency_key, tier,
                             │  messages, response_format, redact_pii, [sponsor_org_id]}
                             ▼
 ┌──────────────────────── Hub ────────────────────────────────┐
 │ verifica firma → instancia → org (o patrocinadora)          │
 │ presupuesto de org (enabled, max_tier, cents cap)           │
 │ Pulse de la org que paga ≥ 1 → si no: 402 insufficient_pulse│
 │ idempotencia (instancia, idempotency_key)                   │
 │ cadena de proveedores barata primero                        │
 │ debita Pulse = cost_sparks; ai_usage_events SIN texto       │
 └───────────────────────────┬─────────────────────────────────┘
                             ▼
               Proveedor LLM (groq, gemini, …, anthropic)
                 ← llaves solo en el Hub

 Cliente no-OPS (7 Leguas, Laravel) ── mismo POST firmado ──► Hub
```

**Quién cobra a quién:**

| Flujo | Paga | Cobra | Unidad |
|---|---|---|---|
| Proveedor LLM → Asteby | Asteby (Hub) | proveedor | USD, contrato del proveedor |
| Cuenta (org) → Asteby | el Pulse de la org (o de la patrocinadora) | Hub | Sparks (`cost_sparks`) |
| Addon → nadie | el addon **no** paga ni cobra; solo consume del Pulse de la org donde está instalado, hasta su tope | — | — |
| Host (OPS) | no cobra ni fija precios; atribuye y aplica topes | — | — |

El precio por llamada es del Hub (modelo real, tokens, caché). El host solo ve `cost_sparks` en la respuesta y lo suma al contador del addon.

## 4. Contrato de la capacidad `ai.complete`

Registrada en `capability` (kernel) como consulta (`kind: query`, cardinalidad `one`), **implementada por el host** (`HostImplemented: true`). Evoluciona con las reglas del paquete: solo campos opcionales nuevos, valores de enum al final y contratos nuevos.

### 4.1 Entrada

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `feature` | string `^[a-z][a-z0-9_]{0,47}$` | sí | feature declarada en el manifiesto (`ai:complete <feature>`); atribuye el gasto y elige los límites |
| `messages` | array de `{role, content}` | uno de `messages` / `prompt` | `role`: `system \| user \| assistant`; `content`: texto |
| `prompt` | string | uno de `messages` / `prompt` | atajo de un solo turno de usuario |
| `system` | string | | instrucción de sistema (se antepone) |
| `tier` | `fast \| balanced \| heavy` | | intención de costo/calidad; el host lo recorta al `max_tier` de la feature y el Hub al de la org. Vacío = tier por defecto de la configuración del host |
| `response_format` | `{type, name?, schema?}` | | `type`: `text` (defecto) \| `json_object` \| `json_schema` (con `schema` = JSON Schema). Con JSON el host valida que la salida sea un objeto JSON |
| `max_tokens` | integer ≥ 0 | | recortado al `max_tokens` de la feature; 0 = el de la feature o el de la configuración |
| `temperature` | number 0..2 | | |
| `idempotency_key` | string ≤ 128 | | la misma clave devuelve el mismo resultado **sin volver a cobrar** (§7). El SDK genera una por intento |

No viajan en la entrada: `org`, `addon`, usuario ni patrocinadora. El host los sella desde la sesión o la invocación; el addon no puede elegirlos.

### 4.2 Salida

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `status` | enum (§4.3) | sí | `ok` o el motivo por el que no hubo respuesta |
| `content` | string | | texto de la respuesta (`status: ok`) |
| `json` | object | | la respuesta ya decodificada cuando `response_format` es JSON |
| `usage` | `{input_tokens, output_tokens}` | | |
| `cost_sparks` | integer | | lo que debitó el Hub (0 en un `replay` o sin llamada) |
| `model` | string | | modelo real que contestó (informativo, nunca para decidir) |
| `request_id` | string | | id del Hub, para soporte y auditoría |
| `message` | string | | texto para mostrar cuando `status ≠ ok` |
| `retry_after_seconds` | integer | | con `rate_limited` |
| `billed_to` | `org \| sponsor` | | quién pagó |
| `replay` | boolean | | respuesta repetida por `idempotency_key` |

### 4.3 Estados (resultados normales, no excepciones)

| `status` | Origen | Qué hace el addon |
|---|---|---|
| `ok` | — | usa `content` / `json` |
| `insufficient_pulse` | Hub 402 | muestra «sin créditos de IA este mes» y sigue sin IA |
| `quota_exceeded` | host: tope mensual del addon o de la feature | igual; el administrador puede subir el tope |
| `budget_exceeded` | Hub 429: tope en dinero de la org | igual |
| `rate_limited` | Hub/host 429 | reintentar después de `retry_after_seconds` |
| `disabled` | Hub `budget_disabled`/`banned`, IA apagada en el host | ocultar la función de IA |
| `no_wallet` | Hub: la org no tiene monedero | igual que `disabled` |
| `unavailable` | Hub 5xx, red, host sin Hub, redacción fallida | reintento opcional; nunca bloquear el flujo |
| `invalid_output` | el modelo no devolvió el JSON pedido | reintento con otra clave o seguir sin IA (los Sparks sí se gastaron) |

Los **errores** (no estados) se reservan para fallos del programador y usan los códigos del contrato: `invalid_input` (entrada fuera del contrato) y `forbidden` (feature no declarada o addon sin permiso).

## 5. Declaración en el manifiesto del addon

```json
{
  "capabilities": [
    { "kind": "ai:complete", "target": "ticket_triage",
      "reason": "Clasificar y resumir tickets entrantes para asignarlos más rápido" },
    { "kind": "ai:complete", "target": "reply_draft",
      "reason": "Proponer un borrador de respuesta que el agente revisa antes de enviar" }
  ],
  "ai": {
    "monthly_sparks_cap": 300,
    "features": [
      { "key": "ticket_triage", "max_tier": "fast", "max_tokens": 600,
        "monthly_sparks_cap": 120, "personal_data": true },
      { "key": "reply_draft", "max_tier": "balanced", "max_tokens": 1200 }
    ]
  }
}
```

Reglas de validación (kernel, en el manifiesto del host y en v3):

- `ai:complete` exige `target` = clave de feature (`^[a-z][a-z0-9_]{0,47}$`, sin comodines) y `reason` no vacío: **la finalidad es obligatoria**.
- Cada `ai.features[].key` es única y debe estar concedida por un `ai:complete` (no se limita lo que no se pidió).
- `max_tier` ∈ `fast | balanced | heavy`; topes y `max_tokens` ≥ 0 (0 = sin tope propio).
- El tope de una feature no puede superar el tope del addon cuando ambos están puestos.
- Ningún addon puede declarar `ai.complete` en `provides_capabilities` (es del host).
- `ai:complete` implica `capability:query ai.complete` para el import wasm: no se pide dos veces.

El tope efectivo es el **menor** entre el del manifiesto y el que fije la organización en la configuración del host; un addon sin `ai` usa los valores por defecto de la configuración del host (no hay números fijos en el kernel).

## 6. Metering, cuotas y patrocinio

**Tres niveles, de afuera hacia adentro**, y el más estricto gana:

1. **Org (Hub):** Pulse del mes (`insufficient_pulse`), presupuesto en dinero (`monthly_cents_cap` → `budget_exceeded`), `max_tier`, pausa (`budget_disabled`). Ya existe.
2. **Addon (host):** `ai.monthly_sparks_cap` del manifiesto o de la configuración de la org → `quota_exceeded`.
3. **Feature (host):** `ai.features[].monthly_sparks_cap`, `max_tier`, `max_tokens`.

**Medición en el host:** el host suma `cost_sparks` por `(org, addon, mes)` y `(org, addon, feature, mes)`. En OPS reusa `usage_metrics` (contador mensual por org y métrica; métricas `ai.<addon>` y `ai.<addon>.<feature>`), sin tabla nueva. El periodo es el mes calendario UTC, igual que el reinicio de Pulse. El host decide con el gasto ya registrado: una llamada en curso puede pasar el tope por su propio costo (como mucho una llamada); es preferible a reservar Sparks por adelantado.

**La fuente de verdad del dinero es el Hub** (`ai_usage_events`, ledger de Sparks). El contador del host es para topes y para mostrar «este addon gastó N este mes»; si difieren, manda el Hub.

**Patrocinio (opcional, apagado por defecto).** Una cuenta patrocinadora paga la IA de otra para una feature concreta. Caso del dueño: la organización **«asteby»** paga la IA de la mesa de atención que corre en su propia org, o la de otra cuenta a la que da soporte. Reglas:

- Se **configura** en el host (OPS: `AI_SPONSOR_ORG_ID` y `AI_SPONSORED_FEATURES` con pares `addon.feature`); nunca se codifica un id ni un nombre de org.
- El host solo **pide** el patrocinio (`sponsor_org_id` en el request). **El Hub lo autoriza** contra una concesión explícita de la patrocinadora (org beneficiaria, addon, feature, tope); sin concesión responde como si no hubiera patrocinio. Así una instancia no puede cargarle IA a una cuenta ajena.
- El resultado dice `billed_to: sponsor`. El tope del addon en el host se sigue aplicando.
- **Pendiente en el Hub:** el campo `sponsor_org_id` y la tabla de concesiones no existen todavía; hasta entonces el host no lo manda y todo lo paga la org.

## 7. Idempotencia

- El SDK genera una `idempotency_key` por intento lógico (UUID) y la reusa en sus reintentos; el guest wasm la deriva de lo que procesa (p. ej. `ticket:<id>:triage:<versión>`).
- El Hub guarda `(instancia, idempotency_key)` y, si vuelve a llegar, devuelve el mismo resultado con `replay: true` y **sin débito** (extensión aditiva del Hub).
- Sin clave no hay idempotencia: dos llamadas cobran dos veces. Por eso el contrato la recomienda y el SDK la pone siempre.

## 8. Auditoría y límites de tasa

- **Auditoría en el host:** cada llamada deja una línea `audit ai.complete org=… addon=… feature=… status=… cost_sparks=… request_id=…` (mismo formato que `capability_query`) y el contador en `usage_metrics`. **Nunca** el texto.
- **Auditoría en el Hub:** `ai_usage_events` con `addon_key`, `feature`, modelo, tokens, `cost_sparks`, latencia y error; para llamadas de addons `query_preview` y `reply_preview` quedan vacíos (`keepText=false`).
- **Tasa:** el Hub limita por instancia/org (`rate_limited`, 429). El host puede limitar por addon con su propio limitador (configuración); un 429 del Hub se traduce a `status: rate_limited` con `retry_after_seconds`.

## 9. Privacidad, consentimiento y redacción

- **Consentimiento:** el administrador ve, al instalar, cada `ai:complete` con su `reason` y si la feature declara `personal_data: true`. Instalar el addon es aceptar ese uso; quitar el permiso apaga la feature.
- **Hook de redacción previo:** `capability.AIRedactor` corre en el host **antes** de mandar nada al Hub (enmascarar correos, teléfonos, RFC/CURP, tarjetas). Si falla, la llamada **no sale** (`status: unavailable`): falla cerrada.
- **`redact_pii`:** si la feature declara `personal_data`, el host manda `redact_pii: true` y el Hub aplica su propia redacción antes del proveedor.
- **Retención:** el Hub **no guarda texto** de llamadas de addons (solo metadatos de uso). El host no guarda prompts ni respuestas; si el addon quiere guardar la respuesta, es dato del addon en su propio esquema, con sus reglas.
- **Proveedores:** el Hub solo usa proveedores con política de no entrenar con datos de API; la lista vive en el Hub y se documenta allí.
- **Observación:** hoy `ABY-AI.md` dice que el cockpit lista «qué se consultó» (preview de la última pregunta). Eso es aceptable para Aby (producto del host con su aviso), **no** para addons: el Hub debe usar `keepText=false` siempre que `addon_key` venga lleno.

## 10. Consolidación en OPS (sin romper nada)

Duplicación encontrada:

| Pieza | Qué hace | Duplica |
|---|---|---|
| `services/hubai.Client.Complete` | POST firmado a `/v1/ai/complete` | — (es la referencia) |
| `services/bi.ClientHubBridge.do` + `Complete` | POST firmado propio + su propio `/v1/ai/complete` (feature `bi_pro`) | la firma y el cliente de `hubai` |
| `services/hubsparks.Client.do` | GET/POST firmado a Sparks | la firma |
| `services/ai_hub_text.go` (`GenerateText`) | un turno de texto sobre `hubai` | nada (ya reusa `hubai`) |

Propuesta (PR aparte, después de este RFC): (1) extraer la firma y el envío JSON a un único transporte (`services/hubsign` o un método exportado de `hubai`) que usen los tres; (2) que `bi.ClientHubBridge.Complete` llame a `hubai.Client.Complete` con `feature: "bi_pro"`; (3) que `GenerateText` y BI pasen por `capability.AIGate` con una política del host, para que todo el gasto de IA quede atribuido igual. El adaptador de este RFC ya **no** agrega un cuarto transporte: extiende `hubai` de forma aditiva (campos nuevos y error tipado `*hubai.HubError` que sigue envolviendo `ErrUnavailable`/`ErrNotEntitled`).

## 11. Protocolo de cable para clientes no-OPS

Un cliente que no es OPS (7 Leguas en Laravel) habla **directo con el Hub**, con su propia identidad Ed25519 de instancia (la que el Hub empareja igual que a una instancia OPS). No hay llave de LLM en el cliente.

```
POST {HUB}/v1/ai/complete
Content-Type: application/json
X-Instance-Pubkey: <hex de la llave pública, 32 bytes>
X-Signature-Timestamp: <segundos unix>
X-Signature: ed25519:<base64(ed25519_sign(priv, sha256(body || "\n" || timestamp)))>

{ "org_id": "<uuid>", "tier": "fast", "addon_key": "7l_tickets", "feature": "ticket_triage",
  "idempotency_key": "ticket:123:triage:v1", "max_tokens": 600,
  "response_format": {"type": "json_object"}, "redact_pii": true,
  "metadata": {"product": "<producto del Hub>"},
  "messages": [{"role":"user","content":[{"type":"text","text":"..."}]}] }
```

Respuestas: 200 con `{request_id, model, content:[{type:"text",text}], usage, cost_sparks}`; error con `{"error":{"code","message"}}` y los códigos de §4.3 (`insufficient_pulse` 402, `budget_exceeded`/`budget_disabled`/`rate_limited` 429, `banned` 403, `invalid_request` 400, `unavailable` 503). El cliente traduce el `code` a `status` igual que el host (tabla de §4.3) y **nunca** lanza excepción por saldo.

Fragmento PHP/Laravel (referencia; libsodium viene con PHP ≥ 7.2):

```php
$body = json_encode($payload, JSON_UNESCAPED_UNICODE | JSON_UNESCAPED_SLASHES);
$ts   = (string) time();
$sig  = sodium_crypto_sign_detached(hash('sha256', $body . "\n" . $ts, true), $secretKey); // 64 bytes
$res  = Http::withHeaders([
    'X-Instance-Pubkey'     => bin2hex($publicKey),
    'X-Signature-Timestamp' => $ts,
    'X-Signature'           => 'ed25519:' . base64_encode($sig),
])->withBody($body, 'application/json')->timeout(120)->post(rtrim($hub, '/') . '/v1/ai/complete');

if ($res->successful()) {
    $text = collect($res->json('content'))->where('type', 'text')->pluck('text')->implode('');
    return ['status' => 'ok', 'content' => $text, 'cost_sparks' => $res->json('cost_sparks')];
}
$code = $res->json('error.code') ?? 'unavailable';
return ['status' => match ($code) {
    'insufficient_pulse', 'budget_exceeded', 'rate_limited', 'no_wallet' => $code,
    'budget_disabled', 'banned' => 'disabled',
    default => 'unavailable',
}, 'message' => $res->json('error.message')];
```

Importante: se firma **exactamente** el cuerpo que se manda (por eso `withBody`, no `post($array)`), y la marca de tiempo debe estar dentro de la ventana de repetición del Hub. La guía completa está en [docs/ai-cable-cliente-no-ops.md](../ai-cable-cliente-no-ops.md) (PR del kernel).

## 12. Por qué no hay llaves de LLM en instancias ni addons

1. **Costo y cobro:** con la llave en el Hub, cada llamada pasa por el débito de Pulse; con una llave local el gasto sería invisible y no facturable.
2. **Seguridad:** un addon es código de terceros; una llave en su configuración se filtra con un `console.log` o un `http_fetch`. Las instancias son appliances en servidores de clientes.
3. **Cambio de proveedor sin redeploy:** el Hub elige proveedor y modelo por tier y cae al siguiente si uno falla; las instancias piden intención (`tier`), no modelo.
4. **Privacidad uniforme:** redacción, retención y política de proveedores se aplican en un solo lugar.
5. **Sin «fallback» local:** si el Hub no responde, el resultado es `unavailable`, nunca una llamada directa al proveedor (misma regla que ya sigue `hubai`).

## 13. Fases y criterios de aceptación

| Fase | Entrega | Criterio |
|---|---|---|
| F0 | Hub: extensión aditiva de `/v1/ai/complete` (`addon_key`, `feature`, `idempotency_key`, `response_format`, `redact_pii`, `cost_sparks`, `keepText=false` con `addon_key`) | pruebas del Hub; requests viejos idénticos |
| F1 | Kernel: contrato `ai.complete`, permiso `ai:complete`, bloque `ai`, `AIProvider`, `AIGate`, `AIHostQuerier`, dobles de prueba, `guest.AIComplete`, docs | `go test ./capability ./manifest/... ./security ./guest` |
| F2 | OPS: adaptador sobre `hubai`, topes con `usage_metrics`, `POST /api/ai/complete`, `GET /api/ai/usage`, `ai.complete` en `capability_query` | pruebas con Hub simulado; `insufficient_pulse` llega como `status` |
| F3 | SDK: `useAI()` / `createAIClient()` | `tsc` y pruebas; estados de carga y de saldo |
| F4 | Primer addon (tickets: triage) y cliente 7 Leguas | caso real en una org de prueba con Pulse agotado y con saldo |
| F5 | Hub: patrocinio (`sponsor_org_id` + concesiones) | la org «asteby» paga una feature de otra cuenta solo con concesión |
| F6 | OPS: consolidación de transportes (§10) | sin cambios de comportamiento en Aby, BI e imágenes |

## 14. Alternativas descartadas

- **Un addon proveedor `ai_gateway` que declara `ai.complete`:** tendría que guardar la identidad de la instancia o una llave; la capacidad es del host como el ledger de pagos.
- **Llaves de LLM por org (BYOK):** ya existe para Aby en algunas instalaciones (`org-ai`), pero para addons rompe D5/D6; si algún día se permite, sería en el Hub, no en la instancia.
- **Reservar Sparks antes de llamar:** más exacto en el tope, pero exige un protocolo de reserva/liberación con el Hub para un error de como mucho una llamada.
- **Excepciones para saldo agotado:** obliga a cada addon a envolver todo en try/catch y en la práctica rompe pantallas; un `status` lo vuelve un caso más.

## 15. Riesgos y preguntas abiertas

| Riesgo | Mitigación |
|---|---|
| **Abuso de Pulse** (un addon en bucle vacía el mes) | topes por addon/feature en el host; Pulse y presupuesto en dinero en el Hub; tasa por instancia; `quota_exceeded` corta sin llamar al Hub |
| **Suplantación de addon desde el frontend** (un addon federado manda `addon` de otro) | el frontend corre con el token del usuario y en el mismo navegador; el host solo acepta addons instalados y habilitados en la org y solo features que ese addon declaró, así que como mucho gasta el tope de otro addon; en wasm la identidad la sella el host (`Caller`) y no se puede falsificar |
| **Inyección de prompts desde datos de usuarios** (un ticket que dice «ignora tus instrucciones») | la salida de IA es **sugerencia**, nunca autoridad: no ejecuta acciones ni cambia permisos; `response_format` JSON + validación del lado del addon; el texto de usuario va como turno `user`, nunca dentro de `system`; las features que actúan deben pasar por una acción con su permiso y confirmación humana |
| **Costos** (tier `heavy` por defecto) | `max_tier` por feature; tier vacío = el por defecto de la configuración (barato); el Hub recorta además al `max_tier` de la org |
| **Aislamiento entre addons** | contadores por addon; la política se lee del manifiesto de **ese** addon; un addon no ve el gasto ni las respuestas de otro; las respuestas no se cachean entre addons |
| **Retención** | sin texto en el Hub para addons; sin texto en el host; documentado en el consentimiento |
| **Desfase entre contador del host y Hub** | el Hub manda; el host solo topa y muestra |

Preguntas para el dueño:

1. ¿Tope por defecto (configuración del host) para un addon que no declara `ai.monthly_sparks_cap`? Recomendación: 20 % del Pulse mensual de la org.
2. ¿El administrador de la org puede **subir** el tope del manifiesto o solo bajarlo? Recomendación: solo bajarlo; subirlo es una nueva versión del addon.
3. ¿Patrocinio con concesiones en el Hub (F5) en este trimestre o se deja apagado?
4. ¿Se permite `heavy` a addons de terceros o solo a addons propios de Asteby? Recomendación: solo con revisión en la publicación.
