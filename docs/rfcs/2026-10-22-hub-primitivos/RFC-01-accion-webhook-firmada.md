# RFC-01: acción declarativa `handler.type: webhook`, firmada y con resultado real

- **Prioridad:** Fase 0 = P0 (M1). Fases 1-2 = P1 (M2).
- **Tamaño:** Fase 0 S (ops). Fase 1 S (kernel, contrato). Fase 2 M (kernel runtime + ops wiring).
- **Depende de:** nada para la Fase 0. La Fase 2 depende del contrato v0.230.0 (PLAN §2).

## 1. Problema (evidencia)

1. **Falso éxito.** El manifest v3 declara `handler: {type: "webhook", url: "..."}` y la UI dice «Éxito»
   sin que salga ninguna petición [S] (el núcleo simulado registró 0).
   - El kernel **pierde la URL** al convertir v3 → host: `handlerToTrigger` devuelve
     `&ActionTrigger{Type: "webhook"}` sin URL (`manifest/from_v3.go:1564`); `manifest.ActionTrigger` no
     tiene campo para ella (`manifest/manifest.go:909-927`) [V].
   - OPS solo despacha webhooks del mapa legado `rec.Hooks["Model::action"]`
     (`services/addon_service.go:1367`), que un v3 nunca llena; la acción cae en la rama final
     `InvokeWasmAction` (`:1467`), que sin export devuelve el no-op `{"Message":"OK"}`
     (`services/wasm_action_kernel.go:129-134,182`) [V].
   - La misma clase de bug existe para `do: "webhook:<key>"` en `schedules`, `webhooks` entrantes y
     `on_transition`: OPS registra `logSkipDispatcher` (`services/pipeline_runtime.go:165,170`), que
     registra en el log y responde como si nada [V].
2. **Sin firma, secreto ni actor.** El interceptor legado de OPS hace un `POST` desnudo con
   `X-Addon-Key` y un cuerpo `{addon_key, org_id, record_id, payload}` (`services/addon_service.go:1816-1860`)
   [V]. El adaptador firmado del kernel (`bridge/webhook_adapter.go`) existe, pero **cae a sin firma**
   ante cualquier error de firma (`:74`) y su secreto es el de la instalación, no uno por destino [V].
3. **URL literal.** La URL va escrita en el manifest firmado: no puede cambiar por instancia (staging,
   producción, desarrollo) sin republicar [S].
4. Lo que **sí existe** y se reutiliza [V]:
   - Allowlist de salida: capability `http:fetch` con host exacto, `*.dominio` o
     `connector:<conector>.<credencial>` (host leído de la configuración del conector de la org),
     compilado en `security.Capabilities` (`security/context.go:41-60,112-140`); `CanFetch`/`CanFetchHosts`
     con guarda SSRF (`:95-110,172-180`).
   - Cliente de red con `DialControl` posterior a DNS (rebinding, IPs exóticas) y allowlist en
     redirecciones: `netguard.NewClient(Options{AllowHosts})` (`netguard/netguard.go:101-185`).
   - Firma HMAC con nonce: `security.Signer.SignWithNonce` firma
     `"<ts>.<METHOD>.<path>.<nonce>.<sha256(body)>"` y emite `X-Metacore-Timestamp`, `X-Metacore-Nonce`,
     `X-Metacore-Signature` (`security/hmac.go:47-66`); `VerifyWithNonce` + `NonceCache` del lado receptor.
   - Secretos cifrados por org: credenciales `type: "secret"` de `connectors[]`, resueltas por
     `connectors.Resolver.Secret(ctx, org, "<conector>.<credencial>")` (el mismo que usa
     `webhooks[].secret_ref`, `runtime/webhookin/webhookin.go:70-76`). Los secretos de conector nunca se
     devuelven a la UI (ops `handlers/integrations_values.go:97-110`).
   - Identidad Ed25519 de la instancia (ops `services/instanceidentity/service.go:181-204`), que el
     núcleo ya conoce (tabla `ops_instances`). Pero su firma v1 cubre solo `sha256(body)\nts`: **no
     está ligada a método, host ni ruta**, así que no sirve tal cual para acciones (se podría reenviar a
     otra ruta del mismo host dentro de la ventana de 5 min).

## 2. Diseño

### 2.1 Manifest (campos nuevos en `Handler`, solo válidos en `contributions.actions[]`)

```json
{
  "connectors": [{
    "key": "trust_core",
    "label": "hub_fleet.connector.trust_core",
    "auth": "token",
    "credentials": [
      { "key": "base_url", "type": "string", "required": true, "validation": "^https://" },
      { "key": "signing_secret", "type": "secret", "required": true }
    ]
  }],
  "capabilities": [
    { "kind": "http:fetch", "target": "connector:trust_core.base_url",
      "reason": "Pedir al núcleo de confianza la revocación de una licencia." }
  ],
  "contributions": {
    "actions": [{
      "key": "request_revoke",
      "target_model": "Instance",
      "placement": "row",
      "requires_state": ["active"],
      "fields": [{ "key": "reason", "type": "string", "widget": "textarea", "required": true }],
      "handler": {
        "type": "webhook",
        "url_ref": "trust_core.base_url",
        "path": "/core/v1/fleet/instances/{record.instance_id}/revoke-request",
        "method": "POST",
        "signing": { "scheme": "metacore-hmac-v1", "secret_ref": "trust_core.signing_secret" },
        "send_record": ["instance_id", "status"],
        "timeout_ms": 10000,
        "retries": 2,
        "on_success": { "set": { "license_status": "revoke_requested" } }
      }
    }]
  }
}
```

| Campo | Tipo | Regla |
|---|---|---|
| `url` | string | Existente. URL absoluta `https://`. Excluyente con `url_ref`/`url_setting`. |
| `url_ref` | `"<conector>.<credencial>"` | **Nuevo.** Base URL leída de la credencial del conector de la org. Exige `http:fetch` `connector:<misma ref>`. |
| `url_setting` | clave de `settings[]` | **Nuevo.** Base URL leída de un setting `type: "string"` del addon (lo pedido: «URL desde un setting»). Exige al menos un `http:fetch` con host fijo; el host resuelto debe estar en esa lista. |
| `path` | string | **Nuevo.** Ruta que se concatena a la base; plantillas `{record.<columna>}` y `{payload.<campo>}` con `url.PathEscape`. Las columnas se validan contra el modelo destino al publicar. |
| `method` | `POST\|PUT\|PATCH\|DELETE` | **Nuevo.** Por omisión `POST`. |
| `signing.scheme` | `metacore-hmac-v1` \| `instance-ed25519-v1` | **Nuevo.** HMAC con secreto por destino, o firma con la llave de la instancia (sin secreto compartido; la usa el plano de control hacia el núcleo, que ya conoce la llave pública). |
| `signing.secret_ref` | `"<conector>.<credencial>"` | Obligatorio con `metacore-hmac-v1`; prohibido con `instance-ed25519-v1`. **Nunca** un setting: los settings de addon viven en `addon_installations.settings` y no tienen el tratamiento de secreto de los conectores [?]. |
| `send_record` | `[]string` | **Nuevo.** Columnas del registro que viajan en el cuerpo. Por omisión solo `id`. Evita filtrar la fila entera a un tercero. |
| `timeout_ms` | 1000..30000 | **Nuevo.** Por omisión 10000. Es el presupuesto **total** (incluye reintentos). |
| `retries` | 0..3 | **Nuevo.** Por omisión 2. Solo ante error de red, 429 o 5xx; nunca ante 4xx. |
| `on_success.set` | mapa | **Nuevo.** Mismo contrato que `set_fields` (RFC-02 §2.2). Se aplica solo con 2xx, en la misma transacción que la auditoría. |

Fuera de las acciones (`tools`, `subscriptions`, `provides_capabilities`) estos campos se rechazan en
esta versión: el despacho asíncrono con reintentos largos (outbox) es otra RFC.

### 2.2 Petición que sale

Cuerpo (JSON, sin escape HTML, como `webhookin.EncodePayload`):

```json
{
  "v": 1,
  "delivery_id": "6f1c…",
  "occurred_at": "2026-10-22T18:04:05Z",
  "addon": "hub_fleet",
  "model": "Instance",
  "action": "request_revoke",
  "org_id": "…",
  "installation_id": "…",
  "actor": { "id": "…", "email": "staff@asteby.com", "roles": ["hub_operator"] },
  "reason": "Cliente dio de baja el servicio",
  "record": { "id": "…", "instance_id": "inst_123", "status": "active" },
  "payload": { "reason": "Cliente dio de baja el servicio" }
}
```

Cabeceras: `Content-Type: application/json`, `User-Agent: metacore-action/1`,
`X-Metacore-Delivery: <delivery_id>` (estable entre reintentos), `X-Metacore-Event:
<addon>.<Model>.<action>` y las de firma:

- `metacore-hmac-v1`: las de `Signer.SignWithNonce` (`X-Metacore-Timestamp`, `X-Metacore-Nonce`,
  `X-Metacore-Signature: sha256=<hex>`). El receptor verifica con `VerifyWithNonce` (ventana 5 min) y un
  `NonceCache`.
- `instance-ed25519-v1`: cadena canónica nueva, **ligada a host y ruta**:
  `"asteby-req-v1\n<METHOD>\n<host>\n<path?query>\n<ts>\n<nonce>\n<hex sha256(body)>"`; cabeceras
  `X-Asteby-Signature: ed25519:<base64>`, `X-Asteby-Timestamp`, `X-Asteby-Nonce`, `X-Instance-Pubkey`.
  El kernel define la interfaz `security.RequestSigner` y el verificador
  `security.VerifyInstanceRequest(pub, method, host, pathQuery, body, ts, nonce, sig, maxSkew)`; OPS la
  implementa con `instanceidentity`; el núcleo (hub) la verifica contra la llave registrada.

`delivery_id`: si la acción declara `idempotency.key_field` y el payload trae valor,
`uuidv5(org|model|record|action|key)`; si no, un UUID por invocación reutilizado en los reintentos. El
receptor debe deduplicar por `X-Metacore-Delivery`.

### 2.3 Resultado visible

| Respuesta del destino | Respuesta de la acción | UI |
|---|---|---|
| 2xx con JSON `{message?, data?}` | 200 `{success:true, message, data}` | toast de éxito con `message` si viene |
| 2xx sin JSON | 200 `{success:true}` | éxito genérico |
| 4xx | 422 `{success:false, code:"webhook_rejected", message, meta:{status}}` | error con el `error.message`/`message` del destino (máx. 300 caracteres, sin cabeceras) |
| 5xx / 429 / red, tras reintentos | 502 `{code:"webhook_unavailable"}` | «El servicio no respondió; intenta de nuevo» |
| presupuesto agotado | 504 `{code:"webhook_timeout"}` | idem |
| URL/host fuera de allowlist, credencial ausente | 422 `{code:"webhook_misconfigured", message:"…qué falta…"}` | error accionable para el admin |

Cuerpo de respuesta leído hasta 64 KiB. Redirecciones: **no** se siguen (un 3xx es 4xx a efectos de
resultado).

### 2.4 Auditoría y evento

Tras la respuesta (éxito o fallo), el host publica el evento canónico `<addon>.<Model>.<action>` con
`actor`, `reason` (si `reason_required.actions` lo pide), `delivery_id`, `status`, `duration_ms`
(sin cuerpo). Va al historial `/api/activity`. `on_success.set` produce además el `…updated` normal.

## 3. Fases

### Fase 0 (ops, sin kernel): nunca más un falso éxito

- `services/addon_service.go`: si `actionDef.Trigger.Type == "webhook"` y no hay `rec.Hooks`, registrar
  un handler que devuelve error tipado `handler_not_dispatchable` (HTTP 501, mensaje «Esta acción no
  tiene un despachador disponible en esta versión») en vez de caer en `InvokeWasmAction`.
- `InvokeWasmAction` sigue con su no-op **solo** cuando el manifest no declaró handler (`Trigger == nil`).
- `services/pipeline_runtime.go`: `logSkipDispatcher` devuelve error (`webhookin` responde 501; el
  scheduler marca el disparo como fallido y lo registra) en lugar de éxito.
- Test de regresión que reproduce el spike: acción v3 webhook → 501, no 200.

### Fase 1 (kernel, contrato; entra en v0.230.0)

`manifest/v3/types.go` (`Handler`), ambas copias del schema (`manifest/v3/schema/` y `docs/spec/v3/`,
byte-idénticas), `manifest/v3/validate.go` (reglas de §2.1), `manifest/manifest.go` (`ActionTrigger`
gana `URL`, `URLRef`, `URLSetting`, `Path`, `Method`, `Signing`, `SendRecord`, `TimeoutMS`, `Retries`,
`OnSuccess`), `manifest/from_v3.go` (`handlerToTrigger` los copia), `manifest/validate.go`
(`validateActionTrigger` caso `webhook`: exactamente una fuente de URL, https, signing coherente),
`modelbase/metadata.go` (`ActionTrigger` con las mismas etiquetas JSON para el round-trip).

Regla de compatibilidad: exigir `signing` en una acción webhook es endurecer → se registra como regla
tolerada `webhook_action_signing` en `manifest/v3/compat.go` (aviso una minor, luego error). Hoy **ningún**
manifest de `addons/origin/main` usa `handler.type: webhook` ni `do: "webhook:…"` [V], así que el riesgo
real es nulo.

### Fase 2 (kernel runtime + ops)

- Kernel, paquete nuevo `runtime/webhookout` (host-neutral):
  `Invoke(ctx, Request{Trigger, AddonKey, Model, Action, OrgID, InstallationID, Actor, Reason, Record,
  Payload}, Deps{Secrets connectors.Resolver, Settings SettingsReader, Caps *security.Capabilities,
  Signer security.RequestSigner, AllowInsecure bool}) (Result, error)`. Resuelve URL, verifica
  allowlist (`CanFetch`/`CanFetchHosts`), construye el cuerpo, firma, despacha con
  `netguard.NewClient` (sin redirecciones), reintenta con backoff 250 ms → 1 s dentro del presupuesto y
  mapea el resultado (§2.3). Implementa además `dynamic.ActionDispatcher` para hosts que usan
  `dynamic.Service.ExecAction`.
- OPS: rama nueva en `HotRegisterDeclarative` (`services/addon_service.go`, antes del `else` final):
  `Trigger.Type == "webhook"` con fuente de URL → handler que llama a `webhookout.Invoke` con el
  resolver de `org_integrations`, los settings de la instalación, las capabilities compiladas, el signer
  de `instanceidentity` y `AllowInsecure = OPS_ENV=development && OPS_WEBHOOK_ALLOW_INSECURE=1` (para el
  núcleo simulado en `127.0.0.1`). Errores tipados → `actionErrorResponse` con el código de §2.3.
  `on_success.set` usa el ejecutor de RFC-02.
- OPS: `handlers/metadata.go`/serialización de acciones: **no servir** `url`, `url_ref`, `url_setting`,
  `path`, `signing` al navegador (solo `trigger.type`).
- Kernel: marcar `bridge.SignedWebhookInterceptor` como obsoleto y quitar el *fallback* a sin firma
  (`bridge/webhook_adapter.go:74`) en la siguiente minor.
- Hub/núcleo (consumidor, M2): verificar `metacore-hmac-v1` o `instance-ed25519-v1` en `/core/v1/*`,
  deduplicar por `X-Metacore-Delivery`, exigir `reason` y `actor`.

## 4. Compatibilidad hacia atrás

- `url` literal sigue siendo válido (ahora sí despacha). Manifests sin los campos nuevos no cambian.
- Hosts con kernel viejo: el schema estricto rechaza `url_ref`/`signing` → por eso el hub y los
  validadores suben **antes** de publicar (PLAN §2).
- El mapa legado `hooks` (v2) sigue con su interceptor sin cambios.

## 5. Pruebas

- Kernel `manifest/v3`: casos válidos/ inválidos de §2.1 (dos fuentes de URL, `http://`, `secret_ref`
  con `instance-ed25519`, `path` con columna inexistente, campos en `subscriptions`), y la regla
  tolerada en modo no estricto.
- Kernel `manifest`: round-trip v3 → `ActionDef` → `modelbase.ActionMetadata` → `ActionDef` sin perder
  campos (patrón de `from_v3_action_*_test.go`).
- Kernel `runtime/webhookout` con `httptest`: firma HMAC verificable con `VerifyWithNonce`; firma
  Ed25519 verificable y **rechazada** si se cambia host o ruta; reintento ante 503 y no ante 400;
  presupuesto total respetado; 3xx no seguido; host fuera de allowlist; IP privada tras DNS
  (`netguard`); respuesta > 64 KiB truncada; `delivery_id` estable entre reintentos.
- OPS: test de integración que reproduce el spike con un receptor `httptest` (cuenta peticiones = 1,
  la UI recibe el `message` real; con 500 la acción responde 502 y `on_success.set` no se aplica).

## 6. Seguridad

- **SSRF:** allowlist obligatoria (`http:fetch`), https obligatorio, `DialControl` tras DNS, sin
  redirecciones, puertos libres solo si el patrón los declara. `url_setting` lo escribe un admin de la
  org, por eso el host resuelto **también** debe estar en la allowlist del manifest.
- **Firma y replay:** ts + nonce + ruta + hash del cuerpo; ventana 5 min; `delivery_id` para
  deduplicar efectos. La variante Ed25519 liga además el host (no se puede reenviar a otro destino).
- **Secretos:** solo vía conector (cifrado, nunca devuelto a la UI ni al navegador); jamás en logs. La
  config del handler no se sirve en la metadata.
- **Datos:** `send_record` mínimo por omisión; el actor viaja con id, correo y roles (sin token).
- **RLS:** el registro se carga ya filtrado por org (`handlers/dynamic.go:3127-3145`); el cuerpo lleva
  el `org_id` del contexto, nunca del payload.
- **Efecto externo sin transacción:** si el destino aplicó el efecto y la respuesta se perdió, el
  reintento lleva el mismo `delivery_id`; la idempotencia del receptor es obligatoria (el mismo
  principio que ya rige a los handlers wasm, que tampoco tienen transacción con efectos externos).
- **Idempotencia de la acción:** el `ExecuteAction` de OPS (`handlers/dynamic.go:2908`) **no** honra
  hoy `Action.idempotency` ni `Action.approval` en acciones de fila (solo el `ExecAction` del kernel,
  que OPS no usa para ellas) [V por búsqueda; confirmar al implementar]. No bloquea esta RFC porque el
  `delivery_id` cubre el reintento de red, pero es un hueco de paridad a cerrar (PLAN, brief O-1).
