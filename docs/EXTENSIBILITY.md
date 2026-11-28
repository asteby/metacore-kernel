# Extensibilidad entre addons

Contrato para **autores de addons**: cómo un addon se engancha a otro (UI, eventos,
datos) sin acoplarse a su esquema físico, y qué garantiza el kernel cuando lo hace.
Es el modelo ya acordado; se implementa por releases.

> **Lee primero la columna "Estado".** Este documento mezcla lo que existe hoy con lo
> que llega después. Un campo marcado como futuro **no se puede declarar todavía**: el
> manifest v3 es estricto (`additionalProperties: false`) y el hub rechaza lo
> desconocido. No lo uses hasta que el release indicado esté publicado y el hub lo
> acepte.

Documentos relacionados: [`permissions.md`](permissions.md) (capabilities),
[`capabilities-commerce.md`](capabilities-commerce.md) (contratos de dominio),
[`dynamic-actions.md`](dynamic-actions.md) (acciones), [`lifecycle-hooks.md`](lifecycle-hooks.md).

## Estados y releases

| Marca | Significado |
|---|---|
| **Existe** | Disponible en kernel v0.236.0 (`origin/main`). Úsalo. |
| **R1** | Release R1 del kernel, "contratos y bus" (Ola 1). **Sin cambios de schema** del manifest: lo que cambia es el comportamiento del bus y de la resolución. |
| **R2** | Release R2, "paquete de extensibilidad" (Ola 3). **El único bump de schema**: todos los campos nuevos llegan juntos. |
| **Ola 4** | Host (ops), SDK y addons dueños sirven y publican lo declarado en R2. |
| **Ola 6** | Cierre: se impone lo que antes era aviso. |

Las fechas de cada ola las fija el roadmap; el orden es el de la tabla.

## Principios

1. **El host define los puntos; los addons los llenan.** Cada modelo dinámico trae slots genéricos de serie.
2. **Declarativo primero, federado como escape.** Lo que se pueda decir con `DetailPanel`, `DashboardWidget`, `Action` u `OptionMetric` se dice en el manifest: el hub lo valida y el RBAC lo controla. El componente federado (`kind: custom`) es la salida de emergencia, con el mismo envoltorio de permisos.
3. **Nunca escribir en la tabla de otro.** Leer: contrato publicado. Escribir: acción o capacidad del dueño. Reaccionar: evento.
4. **Desinstalar es la operación de referencia.** Todo lo inyectado se sirve por instalación y por org al consultar. Nada se copia en el dueño; al desinstalar desaparece.
5. **Un solo paquete de cambios de schema** (R2).

## 1. Qué usar para cada necesidad

| Necesidad | Usa | No uses |
|---|---|---|
| Mostrar un dato de otro addon en la ficha o en la lista | `contributions.inject` `kind: column` / `panel` (**R2**) | copiar el dato en la tabla del dueño |
| Leer registros de otro addon dentro de un handler | `contract:read` (**R2**) | `db:read <tabla ajena>` |
| Pedir un cálculo o un dato de un tercero (tarifas, impuestos, rastreo) | capability query (**Existe**) | leer la tabla del proveedor |
| Filtrar u ordenar por un dato ajeno, o agregados pesados | read model local alimentado por eventos | columna en vivo |
| Cambiar un dato de otro addon | acción del dueño o capacidad | `db:write <tabla ajena>` |
| Reaccionar a algo que pasó | suscripción a un evento de dominio o de contrato | sondear la tabla |

Hoy, mientras llegan los contratos de lectura, `db:read` / `db:write` sobre tablas ajenas
**sigue permitido**; `tools/manifestcheck` lo reporta como aviso por addon (no bloquea).
El bloqueo de los grants nuevos llega en la **Ola 6**, paquete por paquete.

## 2. Slots de UI: `contributions.inject` (**R2**, sirve en **Ola 4**)

### 2.1 Destinos (`target`)

- **Concreto:** `"<addon>.<Model>"`, p. ej. `customers.SalesOrder`. Siempre la clave calificada, no la pelada (la clave pelada de `actions[].target_model` **Existe** y sigue funcionando, pero es ambigua si dos addons tienen un modelo con el mismo nombre o si el dueño se registra después; el validador recomendará la calificada).
- **Por rol:** `"@commerce.order"` resuelve al modelo que el dueño publicó como contrato de lectura `commerce.order@1` (sección 4). Recomendado para comercio: tu addon no queda atado a `customers`.

### 2.2 Catálogo de slots genéricos (`slots@1`, definido en el kernel, renderizado por el SDK)

| Slot | Dónde | Cuerpo admitido |
|---|---|---|
| `record.panels` | Ficha del registro, bloque de paneles | `DetailPanel` (`map`, `timeline`, `chart`, `relation`, `stats`, `custom`) |
| `record.tabs` | Pestañas de la ficha | `DetailPanel` o vista hija |
| `record.header.badges` | Chips del encabezado | badge (campo del modelo propio unido por FK + `values` -> tono) |
| `record.actions` | Menú y barra de la ficha | `Action` |
| `list.columns` | Columnas de la tabla | columna calculada |
| `list.filters` | Filtros fijos de la lista | filtro sobre la columna calculada |
| `form.sections` | Secciones del formulario | campos de `extensions[]` (requiere opt-in) |
| `document.<region>` | Las 6 regiones de `DocumentRegion` | las mismas, ahora declarativas |
| `dashboard.widgets` | Tablero | `DashboardWidget` |

El dueño puede publicar slots **propios** con `extension_points.slot_kinds[] {name, props_schema}` (**Existe** en el schema; hoy el hub no valida que una contribución apunte a un slot publicado, en R2/Ola 4 sí).

### 2.3 Cómo declara el contribuyente

Un único campo nuevo, `contributions.inject[]`, con discriminador `kind ∈ {panel, tab, column, badge, filter, widget}`:

```jsonc
"contributions": {
  "inject": [
    { "key": "shipment_panel", "kind": "panel", "target": "@commerce.order", "slot": "record.tabs",
      "order": 300, "permission": "shipments.shipment.read",
      "condition": { "addon_installed": "shipments" },
      "panel": { "kind": "relation", "title": "shipments.panel.title",
                 "relation": { "via": "Shipment.sales_order_id", "columns": ["status","carrier","tracking_number"] } } },
    { "key": "shipping_status", "kind": "column", "target": "@commerce.order", "slot": "list.columns",
      "column": { "label": "shipments.col.status", "from": "Shipment", "foreign_key": "sales_order_id",
                  "pick": "latest", "field": "status", "order_by": "updated_at", "display": "badge" } }
  ]
}
```

- El cuerpo **reutiliza tipos que ya existen**. La única forma nueva es `column` con `pick: latest|first|agg` y `foreign_key` hacia el destino.
- La FK de una `relation` aportada vive **en el modelo del contribuyente** (`Shipment.sales_order_id`), nunca en el del dueño.
- Las columnas aportadas se resuelven con **una** consulta agrupada por página (`WHERE fk IN (:ids)`, acotada a la org). Para ordenar o filtrar listas grandes por ellas usa un read model local (sección 4.3), no la consulta en vivo.
- Los **campos editables** dentro del modelo ajeno siguen siendo `models[].extensions[]` (**Existe**), con opt-in del dueño en `extension_points.model_extensions_accepted`. Es lo único que escribe en la fila del dueño.

### 2.4 Opt-in del dueño

| Tipo | ¿Opt-in? |
|---|---|
| panel, tab, badge, column, widget, action | **No**, abierto por defecto. El dueño puede **cerrar** un modelo con `extension_points.sealed: ["Model"]` (**R2**). |
| field (`extensions[]`) | **Sí** (`model_extensions_accepted`, **Existe**) |
| slot propio (`slot_kinds`) | Implícito: lo publicó el dueño |

### 2.5 Orden y conflictos (bandas)

- `order` es un entero, **menor primero** (igual que `SlotContribution.order` y `DashboardWidget.order`).
- **Bandas:** el dueño usa **0-199**; los contribuyentes, **desde 200**.
- **Empate:** `(addon_key, key)` en orden lexicográfico (determinista).
- **Acción primaria:** la del dueño gana siempre. Una acción aportada queda como máximo `secondary`, salvo que el dueño la nombre en `extension_points.primary_from` (**R2**).
- **Identificadores:** toda contribución es `<addon>:<key>`; las columnas aportadas se exponen como `<addon>__<key>`, así no chocan con las del dueño.
- **Override por org (Ola 4):** `ui_contribution_overrides (org, target, contribution_id, hidden, order)`, desde "Personalizar vista". Los presets solo la siembran.

### 2.6 Permisos de una contribución

Para mostrarse deben cumplirse las cuatro condiciones:

1. El visitante puede leer el registro destino.
2. Tiene la `permission` de la contribución: una clave **del contribuyente**, declarada en su `rbac.permissions` y asignable en la matriz rol x módulo x acción.
3. Se cumple la `condition` (addon instalado, conector conectado, campo del registro).
4. Los datos del panel se piden a los endpoints **del contribuyente**, que aplican **sus** permisos de modelo.

El host filtra en el servidor y el SDK vuelve a filtrar en el cliente.

### 2.7 Ciclo de vida de una contribución

| Momento | Qué pasa |
|---|---|
| Publicar (hub, **R2**) | Valida schema; que el destino exista con la versión que piden los `compatibility.requires` (`optional: true` si el destino es opcional); que el slot exista; que `column.from` y `foreign_key` sean del contribuyente; que la `permission` esté declarada. |
| Instalar / habilitar | Registra las contribuciones en `(org, addon)`. No toca datos ni schema del dueño, salvo `extensions[]`. |
| Servir | Por org e instalación, en cada consulta de metadata. Si el destino no existe en esa org la contribución queda **inerte con un diagnóstico**, no da error. |
| Actualizar | Si el destino pasa a otra versión mayor del contrato y no la soportas, el preflight avisa y la contribución queda inerte, nunca rota. |
| Desinstalar | Desaparece del registro, se ejecuta `Bus.Unsubscribe`, las entregas `pending` del addon pasan a `cancelled`. Los datos de `extensions[]` quedan en la bolsa pero no se muestran; "Purgar datos" (`dropSchema`) borra además las claves. |

Un addon que use `inject[]` declara `compatibility.requires` de host `ui.inject@1` (`host_capabilities`, **Existe**) para que un ops viejo simplemente no lo instale.

**Lo que existe hoy de esta sección:** acciones cruzadas con `contributions.actions[].target_model` (clave pelada), `models[].extensions[]` con opt-in, `Model.Detail.Panels` **solo para modelos propios**, y `contributions.slots[]` (que ningún host monta todavía).

## 3. Bus de eventos

### 3.1 Qué existe hoy y sus límites

El bus de dominio (`events`), el despacho a suscriptores (`dispatch`) y el libro de entregas
(`dispatch_deliveries`: `delivery_id = sha256(evento, occurrence_id, suscripción)`, estados `pending / delivered / dead`) **existen**. Los eventos CRUD canónicos (`<addon>.<Model>.<acción>`) pasan por un outbox duradero (`kernel_event_outbox`) y se entregan con reintentos y backoff.

Límites **reales hoy**, que tu código debe tolerar:

- `event_emit` desde un guest wasm **publica de inmediato y no es duradero ni transaccional**: sale aunque la invocación falle después, y se pierde si el proceso cae antes del fan-out.
- Las entregas `pending` **no se retoman** tras un reinicio.
- **Sin orden** garantizado entre dos eventos del mismo registro.
- El nombre en `extension_points.events[]` debe ser exactamente `<addon>.<evento>`; sin versión ni schema validado.

Los cambios siguientes (**R1** para el comportamiento, **R2** para el schema) corrigen esto sin que los addons existentes cambien de API.

### 3.2 Envelope `metacore.event/1` (**R1**)

Un solo envelope para el CRUD canónico y el de dominio:

```
id (uuidv7) · name · version (int) · source (addon_key | "kernel") · org_id · occurred_at
aggregate {type, id} · sequence (por agregado, monotónico) · correlation_id · causation_id
actor_id · branch_id · idempotency_key? · test_mode? · data {...}
```

El host estampa los campos de identidad. **Tú solo pones** `name`, `version`, `aggregate` y `data`.

### 3.3 Garantías (**R1**): at-least-once + idempotencia

1. **Emitir al confirmar.** El `event_emit` del guest deja de publicar al instante: acumula los eventos de la invocación y, **solo si termina con `success:true`**, los escribe en `kernel_event_outbox` y los publica. Con un trap o `success:false` se descartan. (Cambia el *momento* de publicar, no la API. No dependas de que un suscriptor corra *antes* de que termine tu propia invocación.)
2. **Entrega duradera.** El libro pasa a ser una cola (`SKIP LOCKED`, lease, `next_attempt_at`, backoff, `dead`). Las `pending` se retoman tras un reinicio.
3. **Idempotencia del consumidor.** El host garantiza "una vez por `delivery_id`" en su lado, pero **tu handler debe ser idempotente** frente a efectos externos. El guest recibe `event.id` y `delivery_id`. El SDK guest trae `Once(ctx, key, fn)` y `IsStale` (**Ola 4**).
4. **Orden opcional por agregado.** `subscriptions[].ordering: "aggregate"` (**R2**) hace que el worker no entregue una si hay otra anterior (`sequence` menor) `pending` para la misma `(suscripción, aggregate.id)`. Por defecto **no hay orden**: descarta lo atrasado comparando `sequence`.
5. **Reintentos.** Respuesta del guest `{success:false, retryable:true, retry_after_ms?}` (**R1**). `success:false` sin `retryable` va directo a `dead` (es lo que pasa hoy). Un trap o timeout se reintenta con backoff. `handler.retries` **Existe**.
6. **DLQ y replay.** Las filas `dead` + el evento `kernel.dispatch.delivery_dead` **Existen**. `Replay(org, subscription, filter{from,to,name})` (**R1**) re-encola con `generation+1` dentro del `delivery_id`. Pantalla "Eventos fallidos" en ops (**Ola 4**). Un **suscriptor nuevo** que necesita la historia usa `backfills[]` (**Existe**), no el replay.

### 3.4 Nombres, versiones y schema

- Formato `<dominio>.<entidad>.<verbo>@<major>`.
- **Dominios de contrato** (`commerce.*`, `fiscal.*`, `inventory.*`...): solo los **proveedores** del dominio pueden emitirlos (`security.EmitPolicy`, **Existe**).
- **Eventos propios:** `<addon_key>.<entidad>.<verbo>`. Hoy el patrón del schema exige `<addon>.<evento>` (dos segmentos); los nombres con más puntos y `@N` llegan en **R2**.
- **CRUD canónico** `<addon>.<Model>.<acción>`: es un *feed* de cambios, **inestable entre versiones del dueño**. Solo es estable si lo consumes vía un contrato de lectura (`<contrato>.changed@N`).
- Dentro de una versión mayor **solo cambios aditivos** (campos opcionales); el hub lo comprobará al publicar. Un cambio que rompe obliga a `@2`; el emisor publica `@1` y `@2` en paralelo durante una ventana de deprecación que el hub anuncia.
- **El schema vive en el manifest** (**R2**): `extension_points.events[]` aceptará `version` y `payload` (schema en línea, mismo formato que `capability.Schema`). El kernel conserva en Go solo los contratos transversales del comercio. Un evento nuevo de un addon **no** cuesta un release del kernel.
- **Catálogo en el hub** (**R2**/**Ola 4**): `GET /catalog/events` con nombre, versiones, schema, emisores, suscriptores conocidos y deprecaciones.

### 3.5 Regla de oro: no declares eventos fantasma

> **Un evento semántico solo se declara en `extension_points.events` si lo produce una transición del dueño, un webhook o una llamada a `event_emit` que realmente corre.**

Un nombre declarado que nadie emite engaña a los suscriptores (se quedan esperando para siempre). Casos típicos a evitar: nombres semánticos custom que solo existen en handlers `compiled` (ops no los carga), o que el guest nunca emite porque el host publica el evento CRUD canónico. Si necesitas "pedido pagado", suscríbete al canónico `SalesOrder.updated` o usa el contrato derivado `commerce.orders.paid@1` (**R1/Ola 4**). Cuando ya no se emita, marca la descripción `DEPRECATED` antes de borrar.

### 3.6 Eventos de dominio de comercio

| Evento | Quién lo emite | Estado |
|---|---|---|
| `commerce.payments.confirmed` | proveedores de pago (webhook o sondeo) | **Existe** |
| `commerce.shipping.tracking_updated`, `commerce.fulfillment.updated` | paqueterías y 3PL | **Existen** |
| `commerce.shipments.updated@1` | proveedor de `commerce.shipments.*` (`shipments`) | **R1** |
| `commerce.orders.paid@1` | el kernel, **derivado** del CRUD `SalesOrder.updated` con `payment_status -> paid` mediante una regla declarativa del dueño (`on_transition` / `rules` **Existen**) | **R1** (contrato) + **Ola 4** (regla del dueño) |

## 4. Datos compartidos

Tres niveles; elige según la tabla de la sección 1.

### 4.1 Contrato de lectura publicado, `contract:read` (**R2**; los dueños lo publican en **Ola 4**)

El dueño publica una **proyección estable** de un modelo propio:

```jsonc
"extension_points": { "read_models": [
  { "name": "commerce.order", "version": 1, "model": "SalesOrder",
    "fields": { "id":"id", "number":"number", "state":"state", "payment_status":"payment_status",
                "currency":"currency_code", "total":"total", "customer_id":"customer_id" },
    "relations": { "lines": { "read_model": "commerce.order_line", "foreign_key": "sales_order_id" } },
    "permission": "customers.sales_order.read",
    "actions": ["confirm", "cancel"] } ] }
```

El consumidor lo declara con `capabilities[] {kind: "contract:read", target: "commerce.order@1"}` (valor nuevo del enum **cerrado** de `kind`) y lo usa con el host import `contract_query(name@v, filter, fields, limit, cursor)`.

- El host reescribe la consulta contra la tabla real, la acota **siempre a la org de la invocación** (nunca a un `org_id` que mande el guest), aplica soft-delete y la `permission`, y **pagina de verdad** (a diferencia de `data_query`, hoy clampado a 200 sin paginación).
- El dueño puede renombrar columnas mientras mantenga el mapeo de `fields`. El hub rechazará publicar una versión que quite un campo de un `read_model@N` publicado.
- **Escribir** sigue siendo del dueño: `actions` se invocan por `action_invoke` con el actor original y pasan por sus `constraints`, su máquina de estados y su auditoría.

### 4.2 Capability query (**Existe**)

Para respuestas **con lógica** o de **un tercero**: tarifas, impuestos, rastreo, disponibilidad. Contrato con `Input`/`Output`, cardinalidad `one|all`, por org y mercado. Ver [`capabilities-commerce.md`](capabilities-commerce.md). Varios proveedores y estado `not_configured`: ver sección 5.

### 4.3 Read model local (patrón del consumidor)

Te suscribes a eventos y mantienes **tu propia** tabla (p. ej. `customers` estampa `SalesOrder.shipping_status` al oír `commerce.shipments.updated`). Sirve para filtrar y ordenar listas grandes, joins en caliente, operar offline en el appliance y tableros. No requiere nada del kernel más allá del bus.

## 5. Resolución de proveedores (**R1**)

Hay **un solo** resolvedor en el kernel para tres cosas que hoy eligen "quién atiende": capacidades (`Select`), rutas (`contributions.routes`, p. ej. `order_fulfillment`) y medios de pago. Entrada: `(org, market, domain, match)`.

**Orden de decisión:**

1. Lista explícita por mercado y luego por org (`org_market_providers`, **Ola 2/4**).
2. Prioridad de la ruta o la capacidad declarada en el manifest (`routes[].priority` **Existe**; `Provider.Priority` en `Select` v2, **R1**).
3. La regla de hoy (si no hay prioridades ni estado, el resultado es **idéntico** al actual).

**Solo cuentan proveedores `ready`.** `not_configured`, `unhealthy` y `disabled` se excluyen; si no queda ninguno el resolvedor devuelve `ErrNotConfigured` (**R1**), que la UI traduce en "configura un proveedor". **Un empate falla cerrado** (no elige al azar). `match {market}` ya es texto libre en las rutas, así que warehouse frente a 3PL gana el filtro por mercado y por estado sin campo nuevo. Los guests consultan la lista con el host import `capability_providers` (**R1**).

## 6. Ciclo de vida, permisos y desinstalación (resumen para autores)

- **Instalar:** nada de lo que inyectas toca el schema ni los datos del dueño (salvo `extensions[]`, con opt-in).
- **Permisos:** una contribución usa **tu** clave de permiso; el visitante necesita además poder leer el destino. Un `contract:read` exige que el hub vea el contrato publicado y respeta la `permission` del dueño.
- **Orden:** bandas dueño 0-199 / contribuyentes >= 200, empate `(addon_key, key)`; la acción primaria es siempre del dueño.
- **Desinstalar:** tus contribuciones, suscripciones y entregas `pending` desaparecen; tus datos en `extensions[]` quedan en el dueño hasta una purga explícita.
- **Versiones:** todo es aditivo. `target_model` pelado y `contributions.slots[]` siguen siendo válidos.

## 7. Autoría (**Ola 4**)

- `metacore contracts pull`: baja del hub el catálogo (slots, eventos, read models, capacidades) y genera **tipos Go** para el guest y **TS** para el frontend.
- Helpers del SDK guest: `Emit(ev)` tipado y versionado, `On[T](name@v, fn)`, `Once`, `IsStale`, `contracts.Query[T]`.
- `manifestcheck --explain`: diff legible de lo que tu addon inyecta, dónde, con qué permisos y qué se borra al desinstalar.
- Plantilla `create-addon --extends commerce.order`.

## 8. Qué usar **hoy**, en una lista

1. Para UI en un modelo ajeno: acciones con `target_model` (existente) y, si el dueño lo acepta, `extensions[]`. Espera a R2 para paneles y columnas.
2. Para leer datos de otro addon: capability query si hay un contrato de dominio; si no, `db:read` (genera aviso en CI) y planea migrar a `contract:read`.
3. Para escribir en otro addon: usa `data_mutate` (dispara los eventos canónicos) o la acción del dueño; evita `db:write` ajeno.
4. Para eventos: declara solo los que de verdad emites; hazlos idempotentes en el consumidor; no dependas del orden ni de que el bus sea síncrono.
5. Un evento "semántico" que nadie emite: no lo declares (ver 3.5).

## Resumen de estado

| Pieza | Estado | Llega en |
|---|---|---|
| Acciones con `target_model` ajeno | Existe (clave pelada) | calificada/por rol: R2 |
| `extensions[]` con opt-in | Existe | - |
| `contributions.inject[]`, slots `slots@1`, `sealed`, `primary_from` | No existe | R2 (schema) + Ola 4 (host y SDK) |
| Bus: `event_emit` al confirmar, libro duradero, `Replay`, `retryable` | No existe | R1 |
| Envelope `metacore.event/1` | No existe | R1 |
| Eventos con versión y `payload` en el manifest, `ordering` | No existe | R2 |
| Catálogo `GET /catalog/*` en el hub | No existe | R2 / Ola 4 |
| `read_models` + `contract:read` + `contract_query` | No existe | R2 (+ dueños en Ola 4) |
| Capability query | Existe | - |
| `Select` v2 con prioridad y estado, `ErrNotConfigured`, `capability_providers` | No existe | R1 |
| `org_market_providers` y lista ordenable de proveedores | No existe | Ola 2/4 |
| Aviso de `db:*` ajeno en `manifestcheck` | Existe (informativo) | bloqueo: Ola 6 |
| `ui_contribution_overrides`, pantalla "Eventos fallidos" | No existe | Ola 4 |
| `metacore contracts pull`, helpers `Once`/`IsStale` | No existe | Ola 4 |
