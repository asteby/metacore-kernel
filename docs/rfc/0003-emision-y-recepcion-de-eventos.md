# RFC 0003 — Emisión y recepción genérica de eventos entre Ops y sistemas externos

- **Estado:** propuesto. Solo diseño: este documento no cambia código.
- **Fecha:** 2026-11-17
- **Repos afectados:** `metacore-kernel` (`webhooks`, `outbox`, `events`, `genui`), `metacore-sdk` (renderizador), `ops` (monta el servicio y la pantalla), `addons` (retira `erp_bridge`), `7leguas-v2` (como sistema externo de referencia, en sus dos sentidos).
- **Relacionado:** [RFC 0001](0001-entidad-y-alcance.md), `docs/events-bus.md`, `docs/background-jobs.md`, `7leguas-v2/docs/CONTRATO_ECOMMERCE_OPS.md`, `addons/docs/RFC-INVENTARIO-SYNC-OPS-V2.md`.

## 0. Resumen

Hoy conectar Ops con un sistema externo (ERP, CRM, contabilidad, un cliente de otra industria) exige **un addon por destino**. El caso que lo dejó claro: `erp_bridge` entrega pedidos a 7 Leguas V2 con una cola, firma y reintentos que **ya existen en el kernel** (`webhooks`, `outbox`), y solo habla de pedidos.

Este RFC propone que emitir y recibir eventos sea una **capacidad de plataforma declarativa**: se configura con datos, sirve para cualquier evento (creación de clientes, pedidos, pagos, existencias, citas…) y para cualquier industria, y no lleva código por destino. Vale en **las dos puntas**: Ops emite y recibe, y el sistema externo (V2) también emite y recibe con el mismo modelo.

| # | Decisión |
|---|---|
| D1 | Una **suscripción de emisión** declara: qué eventos, a qué destino, con qué filtro, qué mapeo de cuerpo y qué perfil de firma. Es un dato, no un addon. |
| D2 | El kernel reutiliza `webhooks` (entrega, reintento, bitácora, repetición) y `outbox` (cola duradera) y suma lo que falta: puente desde el bus, transformación, perfiles de firma, idempotencia y filtros. |
| D3 | Los **perfiles de firma** (HMAC canónico de V2, HMAC simple, Bearer) son datos con una lista cerrada de algoritmos; no se ejecuta código del cliente. |
| D4 | La **transformación** es declarativa (mapeo de rutas, constantes, tablas de traducción), validada al guardar, sin lenguaje de programación. |
| D5 | La **recepción** es el espejo: un **endpoint de entrada** firmado que acepta un evento, lo valida contra un contrato y lo enruta a un **comando registrado** (crear/actualizar un registro dinámico). Nada de rutas fijas por tipo. |
| D6 | Una **pantalla declarativa** (`genui`) administra suscripciones y entradas: casillas para eventos y canales, no JSON a mano. Se arregla una vez y mejora a todos los addons. |
| D7 | Los destinos concretos (V2, un ERP X) son **presets**: paquetes de datos versionados. |
| D8 | `erp_bridge` se retira cuando existan D1–D6; su configuración migra a un preset. |
| D9 | **Entradas y salidas son dos caras del mismo panel** (modelo Bitrix24): una **salida** avisa a una URL cuando pasa algo en Ops; una **entrada** le da a un tercero una URL con credencial, y lo que llega **dispara acciones** declaradas (ver §2.4). Ambas comparten catálogo de eventos, mapeo, firma, bitácora y repetición. |

## 1. Lo que existe hoy (evidencia en el código)

- `webhooks/` (outbound): modelo `Webhook{URL, Events, Secret, RetryMax, TimeoutSec, Owner}`, entrega asíncrona con reintento exponencial, firma HMAC por webhook, bitácora (`WebhookDelivery`), `Test`, `Replay`, `Trigger(event, ownerType, ownerID, payload)`. **No transforma el cuerpo, no tiene perfiles de firma, ni filtros, ni llave de idempotencia por evento.**
- `outbox/`: cola duradera y transaccional (`metacore_outbox_jobs`) con reintento, `Permanent(err)` a dead-letter y llave de idempotencia (`job.ID`).
- `events/` + `docs/events-bus.md`: bus at-least-once con `kernel_event_outbox` y libro de entregas por `delivery_id`.
- `genui/`: especificación declarativa cerrada (`genui/v1`) con renderizador en el SDK.
- `ops` **no monta** `webhooks` ni `outbox` (no hay referencias en `backend/`). El kernel los tiene; la instancia no.
- `addons/packages/erp_bridge`: reimplementa cola, firma y reintentos como addon WASM, solo para pedidos.
- `7leguas-v2`: API de canales v1 con endpoints fijos por tipo (pedidos, devoluciones, publicaciones, banners) y `webhook_url` por cliente para eventos hacia afuera; firma HMAC canónica `"{ts}.{nonce}.{METHOD}.{path}.{sha256(body)}"` con ventana de ±300 s y nonce de 10 min; idempotencia por (canal, `external_reference`).

## 2. Modelo

### 2.1 Suscripción de emisión

```json
{
  "key": "v2-clientes",
  "events": ["customers.customer.created", "customers.customer.updated"],
  "filter": { "all": [ { "path": "payload.branch_id", "in": ["…"] } ] },
  "target": { "url": "https://sandbox.7leguas.com.mx/api/channels/v1/customers", "method": "POST" },
  "signing": { "profile": "hmac_canonical_v1", "secret_ref": "…", "key_ref": "…" },
  "transform": { "body": { "external_reference": "{{event.aggregate_id}}", "name": "{{payload.name}}" } },
  "idempotency": "{{event.id}}",
  "delivery": { "retry_max": 8, "timeout_sec": 15, "ordering": "per_aggregate" },
  "enabled": false
}
```

- `events`: nombres del bus, con patrón (`customers.*`).
- `filter`: condiciones sobre el evento (igualdad, `in`, existencia); sin código.
- `transform`: sustitución de rutas del evento, constantes y **tablas de traducción** (`{"web": "b2c"}`). Se valida al guardar contra un esquema; una ruta inexistente falla al guardar, no al entregar.
- `idempotency`: plantilla de la llave que viaja al destino; la misma llave en un reintento.
- `ordering`: `none` o `per_aggregate` (los eventos de un mismo agregado salen en secuencia).
- `enabled` apagado por omisión: lo que llega **se retiene**, no se pierde (comportamiento ya probado en `erp_bridge`).

### 2.2 Perfiles de firma (lista cerrada)

| Perfil | Qué firma |
|---|---|
| `hmac_simple` | `HMAC-SHA256(body)` en una cabecera configurable. |
| `hmac_canonical_v1` | `"{ts}.{nonce}.{METHOD}.{path}.{sha256(body)}"` con `ts` y `nonce` en cabeceras (el de V2). |
| `bearer` | Token en `Authorization`. |
| `bearer+hmac_canonical_v1` | Ambos (V2 exige los dos). |

Añadir un perfil nuevo es un cambio del kernel con pruebas, no configuración del cliente.

### 2.3 Recepción (espejo)

Un **endpoint de entrada** por organización y por contrato:

```
POST /api/inbound/{org}/{contract}      (firmado; mismo catálogo de perfiles)
```

- El **contrato** (`customers.stock_changed`, `orders.status_changed`…) declara un esquema JSON y el **comando** al que se enruta: crear/actualizar un registro del modelo dinámico indicado, con un mapeo declarativo campo a campo.
- Idempotente por llave de evento; el duplicado responde `replayed`.
- Un contrato nuevo se agrega con datos y un comando registrado, sin una ruta nueva por tipo.

Esto cubre lo que hoy falta en `erp_bridge`: existencias (V2 → Ops), estado y guía del pedido, publicaciones y banners.

### 2.4 Acciones al entrar (entradas que hacen cosas)

Un contrato de entrada no solo escribe un registro: declara una **cadena corta de acciones** que corren cuando el evento llega y es válido.

```json
{
  "key": "crm-cliente-nuevo",
  "schema": { "…": "…" },
  "idempotency": "{{payload.id}}",
  "actions": [
    { "do": "record.upsert", "model": "customers", "match": ["external_ref"], "mapping": { "name": "{{payload.nombre}}", "email": "{{payload.correo}}" } },
    { "do": "event.emit", "name": "customers.customer.imported", "payload": { "id": "{{result.id}}" } },
    { "do": "notify", "template": "cliente_importado", "when": { "path": "result.created", "eq": true } }
  ],
  "on_error": "stop"
}
```

- **Lista cerrada de acciones** (`record.create|update|upsert`, `event.emit`, `notify`, `command.run`, y `automation.enqueue` cuando exista el disparador por evento en `automation`). Añadir una acción nueva es un cambio del kernel con pruebas; la cadena la escribe el cliente como dato.
- Cada acción ve `payload`, `event` y el `result` de la anterior; sin ejecutar código del cliente.
- Las acciones corren **dentro del mismo recibo idempotente**: un duplicado no repite ninguna. Si una acción falla, el recibo queda `failed` y se puede **repetir** desde la pantalla.
- `event.emit` permite **encadenar**: una entrada puede provocar una salida (un CRM crea un cliente → Ops lo registra → emite `customers.customer.created` hacia el ERP).

## 3. El mismo modelo en 7 Leguas V2 (sin nada hardcodeado)

V2 usa el mismo patrón en sus dos sentidos:

- **Entrada:** hoy los endpoints de `/api/channels/v1` son fijos por tipo. Se agrega un **receptor genérico de eventos** que enruta por **contrato declarado en configuración** (como `config/dynamic.php`): tipo de evento → modelo, validación y mapeo. Los endpoints actuales siguen funcionando (compatibilidad) y se pueden expresar como contratos.
- **Salida:** `webhook_url` por cliente deja de ser un campo suelto y pasa a **suscripciones declarativas** (eventos, filtro, transformación, firma), con el mismo esquema que en Ops. La bitácora y la repetición manual se comparten.
- **Alimentación dinámica:** un cliente nuevo (otra empresa, otra industria) se conecta **creando datos**, sin migraciones ni controladores.

## 4. Pantalla declarativa

Una pantalla *Integraciones → Eventos* (generada con `genui`), con tres pestañas, **Salidas**, **Entradas** y **Registro**:

- **Salidas:** elige addon y evento, pone la URL de destino y la credencial; define filtro y mapeo.
- **Entradas:** crea una entrada con nombre; el sistema genera su **URL y credencial** (se muestra una sola vez), se elige el contrato o se arma uno de plantilla, y se arma la **cadena de acciones** con bloques (sin escribir JSON).
- **Registro:** entregas y recepciones juntas, con estado, repetición y filtros (SmartDataTable por metadatos).

Detalle común:

- Casillas de eventos agrupadas por dominio (clientes, pedidos, pagos…), buscables.
- Selector de destino desde presets; editor de mapeo de campos con vista previa del cuerpo sobre un evento de ejemplo.
- Prueba de entrega (`Test`), bitácora con repetición (`Replay`) y estado de la cola.
- Los mapas de canal/cuenta son **listas con casillas y desplegables**; el JSON queda como vista avanzada.

## 5. Presets

Un preset es un paquete versionado de datos: suscripciones, contratos de entrada, mapeos y perfil de firma. Ejemplo: **ERP V2**. Se publican en el hub y se aplican con un clic; no ejecutan código.

## 6. Seguridad

- **SSRF:** destinos validados con `netguard` (https, dominio, sin IP ni redes internas), como ya hace `erp_bridge`.
- **Secretos:** nunca se guardan en la suscripción; se referencian por conector/secreto cifrado y no se muestran ni registran.
- **Datos personales:** el payload retenido en cola es personal; se borra al entregarse y su lectura exige un permiso aparte (patrón de `ERPDeliveryPayload`).
- **Sin ejecución de código del cliente:** la transformación es declarativa; los perfiles de firma son una lista cerrada.
- **Aislamiento por organización y alcance** (RFC 0001): una suscripción solo ve eventos de su organización y de las sucursales/entidades de su alcance.

## 7. Plan por fases

1. **F1 — Montar:** `ops` monta `webhooks` y `outbox` y expone el servicio. Prueba de aislamiento por organización.
2. **F2 — Puente del bus:** evento del bus → `Trigger`, con filtro e idempotencia. Primer evento de punta a punta: **creación de cliente**.
3. **F3 — Transformación y firma:** mapeo declarativo y perfiles `hmac_canonical_v1` y `bearer`. Con esto, pedidos a V2 sin `erp_bridge`.
4. **F4 — Recepción:** endpoint de entrada y contratos; existencias y estados de V2 hacia Ops.
5. **F5 — Pantalla** con `genui` y presets; migrar la configuración de `erp_bridge` y retirarlo.
6. **F6 — V2:** receptor genérico y suscripciones declarativas.

Cada fase trae su prueba y queda detrás de una bandera; sin configuración nada cambia.

## 8. Preguntas abiertas

1. ¿La suscripción es de **organización** o puede tener alcance de **entidad/sucursal** (RFC 0001 D5)? Propuesta: hereda la cadena `sucursal → entidad → organización`.
2. ¿`ordering: per_aggregate` justifica una cola por agregado o basta una llave de partición sobre `outbox`?
3. ¿La transformación necesita funciones (formato de fecha, mayúsculas) o basta con rutas, constantes y tablas? Propuesta: lista cerrada y pequeña de funciones puras.
4. Retención y volumen de la bitácora de entregas con datos personales.
5. Convivencia con `connector_leguas_v2` (hub-addons #29): su lógica de mapeo pasa a ser un preset.
