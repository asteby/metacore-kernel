# RFC: el estándar genui. Mapas de valores, fuentes vivas, activos inyectables e interactividad con filtrado cruzado

- Estado: **Propuesto**. Este PR es solo documentación y no trae código.
- Fecha: 2026-11-19.
- Origen: la petición del dueño, que dice textualmente:

  > «eso de presencia se ve raro: sin mapear traducción, ícono, animaciones, estados; fetch automático o ingesta live por SSE; generative UI debería soportar muchísimas cosas en cuanto a código: insertar archivos CSS, JS y metadatos, para no ser redundante»

  Después se pidió agregar la interactividad del mapa, los filtros en línea compartidos y el *drill-through* (B4).
- Bases revisadas (`origin/main`, leídas con `git show` cuando la copia local estaba atrasada):

  | Repo | Commit |
  |---|---|
  | `metacore-kernel` | 332767a (v0.282.0, #639) |
  | `asteby-ops` | 1e2df9d (#2383), en el worktree `wt-ops-widget-link`, que está exactamente en ese commit |
  | `metacore-sdk` | 26d7fdd3 (#1512) |
  | `hub-addons` | c92f503 (`hub_fleet` 0.4.1) |
  | `hub` | f5ea519 (#828), el replicador |
  | Trabajo en curso, sin commits | `wt-kernel-contrast` (rama `feat/colorx-contrast`, paquete `colorx/` sin commitear) y `wt-sdk-contrast` (rama `feat/contrast-primitive`, cambios sin commitear en `packages/theme/src/contrast.ts`) |

- Convención de marcas (la misma del RFC #633):
  - **[H]** es un hecho verificado en el código, con su ruta;
  - **[I]** es un dato que me dio el dueño o el coordinador y que no reverifiqué;
  - **[S]** es un supuesto, o una afirmación sobre terceros sin verificar;
  - **[P]** es una propuesta.
- Documentos relacionados:
  - el RFC #633 (unidades organizacionales, dimensiones y `scope`), en borrador;
  - `docs/agent-show-ui.md`;
  - `docs/DEMO-CUSTOM-FRONTEND.md` en `hub-addons@feat/demo-custom-frontend` y ops#2381;
  - `colorx/doc.go` en `wt-kernel-contrast`.

## 0. Resumen

Hoy **el mismo estado se declara en cuatro sitios, con cuatro sintaxis y cuatro vocabularios de tono**, y aun así el widget «Presencia de la flota» muestra las claves crudas `stale` y `live_24h` y una fila sin nombre (A.1, A.2). La causa no es un bug puntual: falta un primitivo. Este RFC propone cuatro primitivos encadenados, que se declaran una vez y se consumen igual en la página del sidebar, en el Tablero y en el chat de IA:

| | Primitivo | Qué resuelve | Reusa |
|---|---|---|---|
| **B1** | **Mapas de valores** (`contributions.value_maps[]` y `field.value_map`) | Etiqueta i18n, tono, ícono, `motion`, orden y respaldo para vacío o desconocido de cada valor. Un solo resolutor en Go y en TS, con vectores compartidos. | `column.tones/labels/icons` (#639), `map.status_tones`, `FieldOption`, `makeEnumLabelResolver` de Ops y los tokens de contraste (`colorx`, `metacore-theme/contrast`) |
| **B2** | **Fuentes vivas** (`live` por fuente de datos y por widget) | Actualización por evento (`refetch`, `patch` o `append`) con degradación a *polling* (`refresh_s`), estado accesible e ingesta que **sí** publica. | Bus WS `DATA_EVENT` de Ops (aislado por organización, coalescido y con límite de tasa), `eventlog` del kernel (`Last-Event-ID`), `connectBus` y `refetchIntervalMs` |
| **B3** | **Activos y código inyectable** (`contributions.assets[]` y `contributions.custom_blocks[]`) | CSS, módulos JS, wasm, fuentes e imágenes con SRI **obligatorio**, alcance (`@layer`, Shadow DOM, iframe o Worker según el riesgo), presupuesto, caché por hash, capacidades que se convierten en CSP y Permissions-Policy, firma, y metadatos que la IA descubre. | `frontend`, `slots[]` con `page.block:`, nodo `ext`, `frontend.pages[]`, `AddonSlot`, `ensureRemoteIntegrity` y `security.VerifyBundle` |
| **B4** | **Interactividad y filtrado cruzado** (`filter_bar`, `state.filters`, `cross_filter`, `link.derive`) | Filtros en línea derivados del esquema del modelo, compartidos por todos los bloques de la pantalla; clic en leyenda o segmento para filtrar; *drill-through* sin repetir el `where`; URL compartible. Un filtro nunca amplía el alcance. | `state`/`url_state` (`?s.*`), `map.select`, `tabs.state`, `WidgetLink`, la gramática `where` de `WidgetQuery` y `scope` (#633) |

La cadena queda así:

- **primitivo**: el mapa de valores, la fuente viva y el activo;
- **bloque**: la tabla, el mapa, la gráfica o el `ext`, que referencian el primitivo;
- **widget**: `kind:block` sobre un bloque con nombre;
- **tablero y página**;
- **addon**: declara todo lo anterior con contratos versionados.

Nada del core usa vocabulario de retail ni de MX: los ejemplos son del hub (`hub_fleet`) porque ahí está la evidencia.

---

## A. Inventario de lo que existe hoy [H]

### A.1 El síntoma en producción [I]

`hub.asteby.com` → Tablero → «Presencia de la flota». Es el widget `instances_by_presence`, de `kind: donut`, y se muestra como lista o *donut*. Lo que se ve:

- las filas `stale` y `live_24h`, en crudo;
- una fila sin nombre, que corresponde a 9 instancias con `presence` vacío;
- ningún ícono, ningún tono y ninguna animación.

La leyenda del mapa dice «Live 24h», porque el SDK humaniza el valor; no lo traduce.

### A.2 Por qué sale así, eslabón por eslabón

1. **Valores reales** [H]. Los escribe el replicador del hub, en `hub/backend/cmd/hub-replicator/contract.go:316-324`:
   - `live_24h` si `Alive24`;
   - `live_7d` si `Alive7`;
   - si no, `stale`.

   Las pruebas están en `geo_test.go:34`. **No** escribe `online`, `down` ni `off`: esos son valores de otro campo, `status` (`contract.go:220-251`).
2. **Modelo** [H]. En `hub-addons/packages/hub_fleet/manifest.json` (0.4.1), líneas 446-452, `presence` es `{"type":"text","display":"badge"}` **sin `options`**. En cambio, `status` (líneas 116-144) sí trae `options` con `label` y `color`, que son nombres de paleta (`green`/`red`/`gray`/`slate`).
3. **Widget agregado** [H]. En `manifest.json:2790-2805`, `instances_by_presence` está declarado así:

   ```json
   {"kind":"donut","query":{"model":"HubInstance","aggregate":"count",
     "where":{"environment":"production","is_retired":false},"group_by":"presence"}}
   ```

   Ops resuelve la etiqueta de cada grupo en `backend/services/dashboard_catalog.go:629-639`:
   - por FK, con `makeLabelResolver`;
   - por `options` declaradas, con `makeEnumLabelResolver` (línea 666), que traduce o humaniza;
   - si la columna no tiene ninguna de las dos cosas, la etiqueta es la clave cruda. Así lo hace el kernel en `query/aggregate.go:440`: `AggregateBucket{Key: k, Label: k}`.

   `presence` no tiene `options`, así que sale crudo. `WidgetData.Series` es `[{key,label,value}]` (`dashboard_catalog.go:93`) y **no lleva tono ni ícono**. El SDK pinta `{pt.label}` tal cual en `runtime-react/src/widgets/renderers.tsx:92` y en `charts/src/bodies.tsx:96,156,174`.
4. **Mapa** [H]. En el bloque `fleet_map` (`manifest.json:3494-3527`) los campos son `"status":"presence"` y
   `"status_tones":{"online":"success","down":"danger","off":"muted","stale":"warning"}`. **Solo coincide `stale`.** Los *locales* del addon definen `map.status.online|stale|down|off` (`locales/es-MX.json:320-328`), pero **no** `map.status.live_24h` ni `map.status.live_7d`. El SDK resuelve el texto en `packages/maps/src/field-labels.ts:45-56`, en este orden:
   - `status_labels` del bloque;
   - `map.status.<v>`;
   - `humanize(v)`, y de ahí sale «Live 24h».

   El grupo vacío se pinta como `'—'` (`maps/src/genui.tsx:96-101`). Los colores del mapa son hex fijos de la paleta Okabe-Ito (`maps/src/genui.tsx:29-36`, `TONE_COLORS`), **no tokens del tema**.
5. **Tabla** [H]. La tabla `live_events` (`manifest.json:3550-3644`) sí usa `variant`/`tones`/`labels`/`icons` por columna (`kind`, `outcome`), con lo que trajo kernel v0.282.0 (#639) y SDK #1510.
6. **Vacío** [S]. No sé por qué 9 instancias tienen `presence` vacío. El replicador siempre escribe uno de los tres valores. La hipótesis es que son filas anteriores al campo, o filas que entraron por el *sink* `data` antes de #828. Hay que verificarlo en la base de datos del hub (G, paso 0).

### A.3 El mismo concepto «presentación por valor», hoy en cinco formas [H]

| # | Dónde | Sintaxis | Vocabulario de tono | Archivo |
|---|---|---|---|---|
| 1 | Campo del modelo | `options[]{value,label,icon,color,image,when}` | `color` = **CSS o nombre de paleta** (`green`, `#hex`) | kernel `manifest/v3/types.go:2808-2815` |
| 2 | Columna genui | `variant` + `tones`/`labels`/`icons` (mapas `valor → x`, hasta 32 entradas) | `default/muted/info/success/warning/danger` | `genui/v2/schema/genui-v2.schema.json:366-408` |
| 3 | Mapa genui | `status_tones` (hasta 16 entradas) | el mismo `$defs/tone` | `genui-v2.schema.json:3284-3294` |
| 3b | Mapa en el SDK | `status_labels`/`field_labels` | — | `metacore-sdk/packages/maps/src/field-labels.ts:45`. **No está en el esquema del kernel** (`block_map` tiene `additionalProperties:false`): el kernel rechazaría un documento que lo use. Es una **deriva** entre contratos. |
| 4 | Selector de opciones | `option_display.badges[].values{v:{text,tone}}` y `trailing[].tones[]` | `success/warning/danger/info/neutral` (**`neutral`** no existe en genui) | `manifest/v3/option_display.go:73-114` |
| 5 | Widgets agregados | nada: solo `label`, que sale de `options` si existen | — | `dashboard_catalog.go:629-680` |

Esto viola tres reglas del dueño a la vez: hay redundancia, hay colores hardcodeados (`color: "green"` y los hex del mapa), y el estado no es una estructura compartida.

### A.4 genui/v2 y manifest v3 (kernel) [H]

- **Tono**: `$defs/tone` tiene 6 valores (`genui-v2.schema.json:67-78`).
- **Íconos**: `$defs/iconName` es alfanumérico y lo resuelve el host (línea 1458).
- **Fuentes**: `$defs/dataSource` (líneas 1377-1415) tiene `maxProperties: 2` y es **una** de `q` | `plan(+args)` | `widget` | `inline`.
  - **`args` solo acepta escalares**: no admite `{$ref:"state..."}`. Hoy **no hay forma declarativa de que el estado de la página filtre una fuente del servidor**. Importa para B4.
  - No hay ningún campo `live` ni `refresh`.
- **Estado**:
  - `state` vive en la raíz del documento.
  - Las acciones `set` (`path: state.*`, línea 1081) y `refresh` (`source`, línea 1141) existen.
  - `map.select` es un `statePath` (línea 3329) y `tabs.state` es un `bindingPath`.
  - **`url_state` no está en el esquema del kernel.** El SDK sí lo tiene: `GenUIBodyDoc.url_state?: string[]` (`genui/src/host/contract.ts:97`), con prefijo `s.` (`host/url-state.ts:9`). Es otra deriva.
- **`table.filters`**: es un arreglo de claves (`maxItems: 12`, líneas 2448-2454). No encontré quién lo consuma en `packages/genui/src` del SDK, fuera de los tipos generados.
- **Personalización**:
  - `block_ext` (`ext: <addon>.<nombre>@<mayor>`, `props` solo escalares, `fallback`) en las líneas 3126-3160;
  - `block_slot` en la línea 3092;
  - `section.mode` (`declarative|hybrid|custom`) en la línea 1891.
- **Manifest v3**:
  - `DashboardWidget` (`manifest/v3/types.go:2040-2120`): `kind` es `stat|bar|line|area|pie|donut|list|progress|block|custom`, más `Block`, `UI`, `Link *WidgetLink` y `RefreshS`. Este último está documentado como «*backup* poll», con piso de 10 (`ui.go:46-48`).
  - `WidgetQuery.Where` acepta `eq|neq|gt|gte|lt|lte|contains` y los *tokens* `$now` / `$now-<dur>` (`types.go:2128-2136`).
  - `WidgetLink{model, filter}` o `{url}` (`types.go:2160-2168`, esquema `$defs/WidgetLink` en `manifest-v3.schema.json:3154`).
- **Bloques con nombre**: `BlockDef` en `contributions.blocks[]`, con `{type:"ref"}` expandido por `ExpandBlockRefs` **antes** de genui (`manifest/v3/block_refs.go:13-70`). Es el patrón «se define una vez» que B1 y B3 copian.
- **Frontend**:
  - `Frontend{entry,format,expose,integrity,container,layout,load,sdk,pages}` (`types.go:568-593`). **`Integrity` es opcional** («*optional but recommended*»).
  - `FrontendPage` incluye `budget_kb` (máximo `MaxPageBudgetKB = 150`, `ui.go:27-30`) y `shares`.
  - `SlotContribution{slot_kind,entry,props,timeout_ms,fallback}` (`types.go:2290-2312`).
- **Firma**: `security.VerifyBundle` verifica Ed25519 sobre el *bundle* (`security/signature.go:64-71`, `installer/installer.go:1813`).
- **Ingesta**: `webhooks[].do = "upsert:<Model>"` con `Model.ingest` (`types.go:374, 843`). `runtime/ingest.Apply` devuelve un `Result{Applied, Deleted, SkippedStale, Rejected, Errors, HighWater}` (`runtime/ingest/ingest.go:95-105`) **sin lista de cambios**. Tampoco publica: el paquete no tiene ninguna referencia a bus ni a eventos.
- **Eventos**:
  - `dynamic.CanonicalEvent{Model,Action,BranchID,Before,After,...}` (`dynamic/canonical_events.go:37`) lo publica el CRUD (`dynamic/service.go:1152, 1555, 1690`).
  - `eventlog` es un registro persistente por organización, con secuencia y semántica `Last-Event-ID` para SSE (`eventlog/doc.go:1-30`, `service.go:25-27`).
  - `ws.HubOf` está indexado **por usuario** (`ws/hub.go:55`).
- **IA**:
  - `show_ui`, `list_blocks` y `list_widgets` están en `agent/show_ui_tool.go:451-501`.
  - `WidgetInfo{Key,Title,Kind,Description}` está en `agent/ui_host.go:95-103`.
  - `AgentProfile` deja fuera `ext`, `slot` y `component` (CHANGELOG v0.281).
- **Contraste (en curso, sin commitear)**: `wt-kernel-contrast/colorx` expone `Contrast`, `OnColor(bg, OnOptions)`, `EnsureContrast`, `PickBacking` e `ImageTone`, con vectores en `testdata/contrast-vectors.json` compartidos byte a byte con el SDK (`colorx/doc.go`).

### A.5 SDK (`metacore-sdk@26d7fdd3`) [H]

- **Tabla**:
  - `genui/src/table-view.tsx:42-53` (`labelFor`): si `t` devuelve la propia clave, humaniza. Ojo: el esquema dice «muestra el valor crudo», así que código y contrato no coinciden.
  - En la línea 12, los booleanos se pintan como `'Sí'`/`'No'`, **en español y hardcodeados**.
  - Los tonos están en `genui/src/tones.ts` (`BADGE_VARIANT`, `TEXT_TONE`).
- **Humanizar**: hay **dos** `humanize` con reglas distintas, uno en `genui/src/table-view.tsx:15` y otro en `maps/src/field-labels.ts:14`.
- **Tablero**:
  - `runtime-react/src/dashboard-grid.tsx:106-133` carga los datos una vez por conjunto de claves, sin *polling*.
  - `WidgetKind` en el SDK **no incluye `block`** (`dashboard-types.ts:9-18`): Ops lo maneja aparte.
- **Fuentes**: `genui/src/host/use-sources.ts:17-60` y `host/body.tsx:27-28` (`refetchIntervalMs`, «*poll* de respaldo»). Las etiquetas por defecto de la línea 10 están en español y hardcodeadas.
- **Contrato del host**:
  - `genui/src/host/contract.ts:12` define `GENUI_HOST_API = 'genui.host/v1'`.
  - En las líneas 61-91 está `GenUIHostAdapter{sources,can,onAction,onNavigate,t,locale,currency,ctx,resolveIcon,urlState,connectBus,customBlocks,lazyBlocks,allowExternalLinks,labels}`.
  - La firma es `connectBus?: (bus: PageBus, scope: GenUIScope) => void | (() => void)`.
  - **genui no trae ningún cliente SSE ni WS.**
- **Tiempo real en otros paquetes**:
  - `notifications/src/sse.ts:18-64` usa `EventSource` y pasa `last_event_id` y `access_token` **como parámetros de la URL**.
  - `websocket/src/provider.tsx:22,56-58` hace *backoff* exponencial con tope de 30 s.
- **Inyección**:
  - `runtime-react/src/slots/addon-slot.tsx:40-146`: *timeout* entre 1 y 30 s, `fallback hide|notice`, rango del SDK y `requireIntegrity`, que **por defecto es `false`**.
  - `sdk/src/integrity.ts:158` (`ensureRemoteIntegrity`).
  - `genui/src/page/custom.ts`: `block@1`, `defineBlock`. Su comentario dice: «La carga federada, SRI, iframe y presupuesto NO están aquí».
  - **No hay** inyección de CSS, `@layer`, Shadow DOM ni iframe *sandbox* para addons.
- **Tokens**:
  - `theme/styles/tokens.css:10-15,169-174` define `--success`, `--warning`, `--info` y sus `*-foreground`, además de `--destructive`.
  - **No existen `--tone-*` ni `--on-tone-*`**, ni un token `--danger` o `--muted` de tono.
  - El primitivo de contraste es `@asteby/metacore-theme/contrast` (`ensureContrast`, `mostReadable`). `onColor` y `pickBacking` están **sin commitear** en `wt-sdk-contrast`. No existe un paquete `@asteby/metacore-color`.
- **Movimiento**:
  - `maps/src/theme.ts:41` y `runtime-react/src/motion.ts:25-35` implementan `prefersReducedMotion`.
  - Hay `animate-pulse` **sin** `motion-reduce:` en `genui/src/page/blocks.tsx:259,525`, `charts/src/lazy.tsx:27` y `agent-ui-result.tsx:210`.
- **Chat**: `agent-workspace/src/workspace/agent-ui-result.tsx:22-118` pinta con el mismo `GenUIBody`, pero sin `connectBus`, `customBlocks` ni `urlState`, y con `stripCustom`.

### A.6 Ops (`asteby-ops@1e2df9d`) [H]

- **Bus WS** (`backend/services/websocket/data_events.go`):
  - `DATA_EVENT{OrgID,Addon,Model,Table,Action,ID,StageFrom,StageTo,ActorID,At,Fields,Coalesced}` (líneas 58-86). **Lleva identidad y nombres de campo, no valores.**
  - `SUBSCRIBE {models}`.
  - Coalesce cada 150 ms por `(org, model, id)` y limita a 200 eventos por segundo por conexión y 64 modelos por conexión (líneas 88-131).
  - El reparto es **solo por organización** (`flushDataEvents`, línea 303). **No hay filtro por sucursal** en el socket.
  - El token viaja en la URL (`/ws`, `server.go:3054`).
- **Puente**: `services/realtime_data_bridge.go:88-134` reenvía `CanonicalEvent` a `DATA_EVENT`.
- **Cliente**:
  - `frontend/src/lib/realtime/host-realtime.ts:150-378`: conteo de referencias por modelo, y `resyncAll()` al reconectar o al recibir `DATA_EVENTS_DROPPED`.
  - `websocket-provider.tsx:145-159`: reconexión cada 3 s fijos, infinita, **sin *jitter* ni *backoff***.
  - El WS **no tiene `Last-Event-ID`**.
- **SSE**: `handlers/notification_stream.go:26-70` no manda `id:` ni hace *replay*. El único cliente SSE con `Last-Event-ID` y *backoff* ×1,5 es el de Ops hacia el hub (`services/hubevents/client.go:165-197`).
- **Host genui**:
  - `frontend/src/features/metacore/genui-host.tsx:77-163` implementa `genui.host/v1`, con `connectBus` hacia `connectHostPageBus` (`page-host-events.ts:44`).
  - Ese bus es **solo DOM local** (`host.record_mutated`) y **no lo alimenta el WS**.
  - El host genui **no expone tiempo real**.
- **`refresh_s`**:
  - Solo se usa en los widgets `kind:block` (`frontend/src/features/dashboard/block-widget.tsx:83`, limitado a 10..3600 s en `block-spec.ts:13-24`).
  - Las páginas declarativas no hacen *polling* (`declarative-addon-page.tsx:47`).
- **Ingesta `upsert:`**:
  - `services/addon_ingest.go:241-320` ejecuta `kingest.Apply` en una transacción con `gormIngestStore`, que escribe directo en la tabla (líneas 384-416).
  - **No publica nada**: no hay `CanonicalEvent`, ni `DATA_EVENT`, ni bitácora de actividad.
  - El CRUD normal sí publica (`handlers/dynamic.go:297-332`).
- **Replicador**: el del hub hace *polling* cada **5 s** (`hub/backend/cmd/hub-replicator/main.go:95`). Tiene dos *sinks*: `data` (CRUD, que publica) e `ingest` (`upsert:`, que no publica). Que el hub en producción use `ingest` lo informó el dueño [I] y no lo verifiqué en el despliegue.
- **Alcance**: los widgets aplican `DataScopeFrom` / `ResolveScope` (`dashboard_catalog.go:355-360`).
- **Federación**:
  - `host-slot-catalog.ts:97-103`: un addon **privado** sin `integrity` no se monta. Los públicos sí se montan sin `integrity`. Por ejemplo, `addons/packages/bi/manifest.json:50-56` no declara `integrity`.
  - `page-custom-blocks.tsx`: contrato `ops.page-block/v1`, con la declaración `block@1` metida como STUB en `slots[].props.block` (línea 21).
- **CSP**:
  - Sobre la SPA solo hay `Content-Security-Policy-Report-Only` (`frontend/Dockerfile:54`, `deploy/nginx/ops.asteby.com.conf:108`), con `script-src 'self' 'wasm-unsafe-eval'; connect-src 'self' https: wss:`.
  - **No hay `Permissions-Policy`.**
- **IA**: `services/agent/ui_tools.go:154-161` y `ui_host.go:96-186`. `ListWidgets` solo devuelve los widgets visibles y con datos.
- ***Drill-through***: Ops todavía no proyecta ni pinta `WidgetLink` [I]. Otro agente lo implementa en la rama `feat/dashboard-widget-link` con el contrato actual. Este RFC **extiende** ese trabajo y no lo duplica (B4.6).

### A.7 Addons [H]

- **`hub_fleet` 0.4.1**:
  - no declara `frontend` ni `assets`;
  - sus *webhooks* son `upsert:HubInstance|HubInstanceEvent|HubRollout|...` (`manifest.json:4117` en adelante);
  - los *locales* `es-MX` y `en-US` no tienen claves `live_24h`/`live_7d`.
- **`demo_custom_frontend`** (`hub-addons@feat/demo-custom-frontend`):
  - `frontend.integrity` sigue en `"sha384-PENDING-BUILD"`;
  - declara el *slot* `page.block:demo_custom_frontend.live_clock@1`, un `ext` en una `section` con `mode: hybrid`, y un widget `kind:block`;
  - `tools/integrity.mjs` calcula sha384 **solo** de `remoteEntry.js` y exige un sufijo de *hash* en los nombres de `dist/assets`;
  - el documento enumera seis límites (`:42-59`), entre ellos que `page.block:` no está en el catálogo de *slots* del kernel y que el `ext` no aparece en el chat.
- **`addons`** (repo de negocio):
  - `accounting-lite/manifest.json:845-886` usa `options` con `color` e `icon`;
  - `customers/manifest.json:606-670` y `products/manifest.json:106-145` usan `option_display` con tonos.
  - Ningún addon usa `status_tones` ni `variant`.

---

## B. Diseño

Estos principios valen para los cuatro primitivos [P]:

1. **Aditivo y versionado.** Ningún campo existente cambia de significado. Las formas viejas se proyectan a la nueva con un *lint* de deprecación y no se eliminan en esta versión mayor.
2. **Un motor por regla.** Cada regla tiene una implementación en Go (kernel) y su espejo en TS (SDK), probadas con **vectores de conformidad compartidos byte a byte**, como ya hacen `colorx` y `genui/v2/testdata/conformance`.
3. **Los *refs* solo viven en el manifiesto.** Igual que `ExpandBlockRefs`, el host expande las referencias **antes** de servir el documento. El navegador recibe un documento genui autocontenido, y el chat recibe el mismo.
4. **El cliente nunca es la frontera de seguridad.** El alcance, los permisos y la organización se aplican en el servidor en cada lectura. Los eventos vivos no transportan datos.

### B1. Mapas de valores

#### B1.1 Esquema en el manifiesto [P]

`contributions.value_maps[]` es nuevo. El esquema va en `manifest-v3.schema.json` como `$defs/ValueMap`:

```json
{
  "key": "presence",
  "title": "hub_fleet.value_map.presence",
  "values": [
    { "value": "live_24h", "label": "hub_fleet.presence.live_24h", "tone": "success", "icon": "Radio",    "motion": "pulse", "order": 10,
      "description": "hub_fleet.presence.live_24h.hint" },
    { "value": "live_7d",  "label": "hub_fleet.presence.live_7d",  "tone": "info",    "icon": "Clock",    "order": 20 },
    { "value": "stale",    "label": "hub_fleet.presence.stale",    "tone": "warning", "icon": "WifiOff",  "order": 30 }
  ],
  "empty":   { "label": "hub_fleet.presence.none", "tone": "muted", "icon": "CircleDashed", "order": 90 },
  "unknown": { "label": "value_map.unknown",       "tone": "muted", "icon": "CircleHelp",   "order": 99, "show_raw": true },
  "aliases": { "online": "live_24h" }
}
```

En el modelo se agrega un campo nuevo a `ColumnDef`, y sustituye a `options` para la presentación:

```json
{ "name": "presence", "type": "text", "label": "hub_fleet.field.presence", "display": "badge", "value_map": "presence" }
```

La referencia del campo puede ser `"presence"` (del propio addon) o `"<addon>.<key>"`, en cuyo caso exige ese addon en `compatibility.requires`. Es la misma gramática que `BlockDef`.

**Reglas de validación** (kernel, `manifest/v3/value_maps.go`; el esquema cubre la forma y Go las reglas cruzadas) [P]:

| Regla | Código estable |
|---|---|
| `key` en snake_case, único en el addon; como máximo 64 mapas por addon | `value_map.key` |
| `values`: de 1 a 64 entradas; `value` de 1 a 64 caracteres con el patrón `^[A-Za-z0-9_.:-]{1,64}$` (el mismo de `tones` en #639); sin duplicados, ni contando los alias | `value_map.value` |
| `label` es una **clave i18n** que debe existir en el `default_locale` del addon. Otro locale sin la clave produce un aviso, no un error | `value_map.label_missing` |
| `tone` pertenece a `$defs/tone`. Prohibidos los colores literales | `value_map.tone` |
| `icon` cumple `$defs/iconName` | `value_map.icon` |
| `motion` es uno de `none|pulse|spin|blink|bounce` (por defecto `none`) | `value_map.motion` |
| Como máximo 2 valores con `motion ≠ none` por mapa, porque un mapa donde todo se mueve no comunica nada | `value_map.motion_budget` |
| `empty` y `unknown` son **obligatorios cuando el campo admite nulos o no tiene `enum`**. El resolutor nunca devuelve una etiqueta vacía | `value_map.fallback_required` |
| `aliases` solo apunta a valores declarados; como máximo 16 | `value_map.alias` |
| Un `value_map` en un campo que también tiene `options` con texto distinto produce un aviso: gana `value_map` | `value_map.options_conflict` |
| Una referencia que no se resuelve | `value_map.ref_unresolved` |

#### B1.2 En el documento genui/v2 [P]

`value_maps` pasa a ser un diccionario en la raíz del documento, igual que `data`:

```json
"value_maps": { "presence": { "values": [ ... ], "empty": { ... }, "unknown": { ... } } }
```

- En el documento, las etiquetas son `$defs/i18nLabel`: un literal o `{"$t"}`.
- Lo referencian, con `"value_map": "<nombre>"`:
  - `column` (tabla y `series` de gráfica, porque las dos usan `$defs/column`);
  - `block_map`, a nivel del bloque (aplica a `fields.status` y a la leyenda);
  - `block_list`, en `items[].badge`;
  - `block_metric` y `kpi_card`, en el valor de estado;
  - `stats`;
  - `timeline`, en `items[].status`.
- El host **llena** el diccionario expandiendo `value_map` del manifiesto. El autor escribe `"value_map": "hub_fleet.presence"` y el host deja `value_maps["hub_fleet.presence"]` en el documento, como hace `ExpandBlockRefs` con `data`. También se aceptan mapas en línea, porque la IA y los documentos sin manifiesto los necesitan.
- **Auto-resolución por columna.** Si la fuente es `q:"t:<Model>..."`, `r:<Model>/<id>` o `widget` con `group_by`, el host **conoce el modelo** y adjunta él solo el `value_map` de cada columna que lo tenga, aunque el documento no lo nombre. Esto es lo que elimina la redundancia: se declara **una vez en el campo** y la tabla, el mapa, la lista, la gráfica, la exportación y el chat lo heredan. Hacerlo explícito es opcional y sirve para valores calculados o para fuentes `inline`.

#### B1.3 Cómo unifica lo existente, sin romper [P]

| Forma actual | Se proyecta como | Precedencia |
|---|---|---|
| `column.tones/labels/icons` (#639) | Un mapa en línea anónimo, como azúcar sintáctico | Las entradas por valor de la columna **ganan** sobre el `value_map` referenciado, para poder ajustar por pantalla. Hay un *lint* si las dos cosas coinciden en un 100 % (redundantes). |
| `map.status_tones` | Lo mismo, aplicado a `fields.status` | Igual. Se agrega `status_labels`/`status_icons` **al esquema del kernel** para cerrar la deriva A.3-3b, pero se recomienda `value_map`. |
| `FieldOption{label,icon,color}` | Si el campo no declara `value_map`, el host **deriva** uno de sus `options`. `color` se mapea a tono con una tabla fija y documentada: `green/emerald/teal→success`, `red/rose→danger`, `amber/yellow/orange→warning`, `blue/sky/indigo→info`, `gray/slate/zinc→muted`; un hex o cualquier otra cosa → `default`, con aviso. | `value_map` > `options` derivado |
| `option_display.badges[].values` y `tones[].tone` | Pueden referenciar `"value_map"` en lugar de `values`. `neutral` queda como alias de `muted` (aviso). | — |
| Widget agregado (`group_by`) | `WidgetData.Series[]` gana campos **aditivos**: `{key, label, value, tone?, icon?, motion?, order?, empty?}`, resueltos en el servidor por el mismo resolutor. `makeEnumLabelResolver` pasa a ser un caso del resolutor. | — |

El resolutor es **uno**:

- Go: `kernel/valuemap.Resolve(m ValueMap, raw any, t Translator) Presentation`;
- TS: `@asteby/metacore-genui` `resolveValue(map, raw, t)`.

Su salida es `{label, tone, icon, motion, order, raw, kind: "value"|"alias"|"empty"|"unknown"}`. Reglas:

- `null`, `""` o solo espacios dan `empty`.
- Lo que no coincide, ni por alias, da `unknown`. Con `show_raw`, la etiqueta es «Desconocido (`xyz`)», con interpolación i18n `{raw}`.
- Las etiquetas que no se traducen caen al `humanize` **único**: hoy hay dos y se fusionan en uno solo en `genui`, que `maps` reutiliza.
- El orden de los grupos y de la leyenda es `order` y, como desempate, el conteo descendente.
- Los booleanos se resuelven con el mapa implícito `value_map.boolean` del kernel, con claves `value_map.boolean.true/false`. Así se elimina el `'Sí'/'No'` hardcodeado de `table-view.tsx:12`.

Los vectores viven en `valuemap/testdata/vectors.json` (kernel) y se copian al SDK con un *lock*, como `genui/v2/testdata/conformance.lock`.

#### B1.4 Tono, contraste y movimiento [P]

- **Tokens.** `metacore-theme` genera `--tone-<t>` y `--on-tone-<t>` para los 6 tonos (`default, muted, info, success, warning, danger`), en claro y en oscuro.
  - `--on-tone-<t>` sale de `onColor(--tone-<t>, {min: 4.5})`. Esa función es el «único punto de decisión» del primitivo de contraste en curso (`wt-sdk-contrast`). Su espejo Go, `colorx.OnColor`, se usa donde el color se decide en el servidor (PDF o correo).
  - Para insignias con fondo atenuado se agrega `--tone-<t>-subtle` y su `--on-tone-<t>-subtle`, calculado con `ensureContrast(fg, subtleBg, 4.5)`.
  - El fondo de 14 % que hoy usa `badge.tsx:20-29` se recalcula con estos tokens.
  - `maps/src/genui.tsx` deja `TONE_COLORS` (los hex Okabe-Ito) y lee los tokens. La propiedad de daltonismo se conserva: los tonos del tema se eligen con esa restricción, y el tono **nunca** es el único canal, porque el ícono y la etiqueta siempre acompañan (WCAG 1.4.1).
  - Los tokens de hoy (`--success`, `--warning`, `--info`, `--destructive`) quedan como alias de los nuevos.
- **`motion`**. Es un enum cerrado (`none|pulse|spin|blink|bounce`) y el host lo implementa con clases propias. El addon no escribe ni `keyframes` ni CSS.
  - `prefers-reduced-motion` es **obligatorio**. La animación solo existe dentro de `@media (prefers-reduced-motion: no-preference)`. Con `reduce`, cada movimiento se reemplaza por un **equivalente estático** para no perder la información: `pulse` pasa a anillo doble, `spin` a ícono de reloj de arena estático, `blink` y `bounce` a borde grueso.
  - Por defecto, el movimiento termina en 5 s o menos, salvo `spin`, que se usa como indicador de actividad en curso. Mi lectura de WCAG 2.2.2 «Pausar, detener, ocultar» es esta [S]: se pide revisión de accesibilidad.
  - `blink` va a 1 Hz o menos, lejos del umbral de 3 destellos por segundo (WCAG 2.3.1).
  - Una prueba de lint del SDK falla si encuentra `animate-*` sin `motion-reduce:`. Esto corrige de paso los cuatro casos de A.5.
- **Accesibilidad.** La insignia lleva `aria-label` = etiqueta y, si existe, `description`. El ícono es `aria-hidden`. El color no es el único canal.

#### B1.5 Responsabilidades por capa

| Capa | Trabajo |
|---|---|
| Kernel | `$defs/ValueMap` en los dos esquemas; validación (B1.1); paquete `valuemap` (resolutor y vectores); `value_maps` y `value_map` en genui/v2; expansión en `ExpandBlockRefs` / `ExpandDocBlockRefs`; `AggregateBucket` aditivo (`Tone`, `Icon`, `Motion`, `Order`); mapa `boolean` base; claves `value_map.*` en los catálogos i18n del kernel |
| SDK | `resolveValue` y el `humanize` único; `ValueBadge`/`ValueIcon` compartidos por tabla, mapa, lista, leyenda, gráfica y `metric`; tokens `--tone-*`/`--on-tone-*`; clases de `motion` con reducción obligatoria; *lint* de `motion-reduce` |
| Ops | Expansión y auto-resolución por columna en `/api/q`, en `widgets/data` y en las páginas; `WidgetData.Series` aditivo; `makeEnumLabelResolver` delega en `valuemap`; el chat recibe el documento ya expandido |
| Addon | Declarar `value_maps[]` y `field.value_map`; agregar las claves i18n; quitar `status_tones` y `tones`/`labels` redundantes cuando el *lint* lo indique |

### B2. Fuentes vivas

#### B2.1 Esquema [P]

En la **fuente** genui/v2 (`$defs/dataSource`, `maxProperties` sube de 2 a 3 y `live` puede acompañar a `q`, `widget` o `plan`; nunca a `inline`):

```json
"data": {
  "fleet": {
    "q": "t:HubInstance?environment=production&is_retired=false&limit=200",
    "live": { "on": ["HubInstance"], "mode": "patch", "fields": ["presence", "status", "ops_version", "lat", "lng"],
              "debounce_ms": 1000, "fallback_poll_s": 60 }
  },
  "events": {
    "q": "t:HubInstanceEvent?sortBy=occurred_at&order=desc&limit=8",
    "live": { "on": ["HubInstanceEvent"], "mode": "append", "actions": ["created"], "cap": 8 }
  },
  "presence": { "widget": "hub_fleet.dashboard.instances_by_presence", "live": { "on": ["HubInstance"], "mode": "refetch", "fields": ["presence"] } }
}
```

En el **widget** del manifiesto (`DashboardWidget.live`) se usa el mismo objeto, y `refresh_s` pasa a significar **solo** el *polling* de respaldo, como ya lo describe su comentario:

```json
{ "key": "instances_by_presence", "kind": "donut", "query": { "...": "..." },
  "live": { "on": ["HubInstance"], "mode": "refetch", "fields": ["presence"] }, "refresh_s": 60 }
```

`$defs/live`:

| Campo | Tipo | Regla |
|---|---|---|
| `on` | `string[]`, de 1 a 4 | Claves de modelo (`Model` o `<addon>.Model`). El modelo debe ser legible por la fuente: el validador verifica que una fuente `q:t:X` incluya `X` o un modelo relacionado declarado. |
| `mode` | `refetch|patch|append` | `refetch` vuelve a ejecutar la fuente y es obligatorio para fuentes agregadas (`widget`, `plan` y `q` con `aggregate`). `patch` relee **por id** las filas afectadas y las fusiona; exige que la fuente tenga `id`. `append` antepone las filas creadas y recorta a `cap`. |
| `actions` | subconjunto de `created|updated|deleted` | Por defecto, las tres |
| `fields` | `key[]`, como máximo 16 | Solo reacciona si algún campo cambiado (`DataEvent.Fields`) está en la lista. Así se evita la tormenta de `last_report_at` del replicador (B2.4). |
| `debounce_ms` | 250..10000, por defecto 1000 | Coalescencia en el cliente, encima de los 150 ms del servidor |
| `min_interval_ms` | ≥ 2000 (piso del host) | Límite de tasa por fuente |
| `cap` | 1..200 | Solo con `append` |
| `fallback_poll_s` | ≥ 10 | *Polling* mientras esté desconectado. En un widget, es `refresh_s`. |

Límites de documento:

- como máximo 8 fuentes vivas, que es el `maxProperties` de `data`;
- el `AgentProfile` admite como máximo 2 fuentes vivas y solo `refetch`.

#### B2.2 Transporte [P]

El contrato del host se extiende a **`genui.host/v1` con la capacidad opcional `live`**. Es aditivo: un host que no la implementa degrada a *polling*.

```ts
interface GenUILiveCapability {
  subscribe(req: { models: string[]; actions?: Action[]; fields?: string[]; scope: GenUIScope },
            onEvent: (ev: LiveEvent) => void,
            onState: (s: 'live' | 'connecting' | 'reconnecting' | 'offline' | 'polling') => void): () => void
}
type LiveEvent = { model: string; action: 'created'|'updated'|'deleted'|'resync'; id?: string; fields?: string[]; at: string; seq?: string }
```

- **Ops, fase 1.** Implementa `live` sobre el **WS `DATA_EVENT` que ya existe** (`hostRealtime`), que ya aísla por organización, coalesce, limita la tasa, cuenta referencias y hace `resync`. No hace falta transporte nuevo para la SPA.
- **SSE** (`GET /api/live/stream?models=...`) es para los hosts sin WS: *embeds*, el chat en otra superficie, el *widget* público o `curl`.
  - Se apoya en el `eventlog` del kernel, que ya tiene secuencia por organización y semántica `Last-Event-ID`.
  - Manda `id: <org_seq>`, `retry: <ms>` y un `: ping` cada 25 s, y lee el **encabezado** `Last-Event-ID`.
  - El *token* no va en la URL: va en *cookie* `HttpOnly` o en un *ticket* de un solo uso (`POST /api/live/ticket` → `?t=<ticket>` válido 30 s). Corrige el patrón de `notifications/src/sse.ts:18`.
  - Ventana de *replay*: 10 min o 5 000 eventos. Fuera de ella, el servidor manda `event: resync`.
- **Reconexión** (los dos transportes): *backoff* exponencial con *full jitter*, `delay = random(0, min(30 s, 1 s × 2^n))`, sin límite de intentos y con reinicio tras 60 s estables. Sustituye los 3 s fijos de `websocket-provider.tsx:145-159`. Al reconectar:
  - SSE reanuda por `Last-Event-ID`;
  - WS hace `resync`, que equivale a `refetch` de cada fuente viva. Ya existe.
- **Degradación.** Si pasan más de 30 s en `offline` o `reconnecting`, el cliente pasa a `polling` con `fallback_poll_s` y vuelve a `live` al reconectar. Si la pestaña está oculta (`visibilitychange`), se pausan *refetch* y *polling*, y al volver se hace un único `resync`.

#### B2.3 Estado accesible [P]

- Cada bloque con fuente viva expone `ctx.live.<fuente>` (`live|reconnecting|offline|polling`).
- El marco `card` del host pinta un `LiveIndicator`, que es un mapa de valores **del sistema** (`value_map.live_state`): `live → success/pulse`, `reconnecting → warning/spin`, `offline → danger`, `polling → muted`. Se reutiliza B1: no es un componente con reglas propias.
- `role="status"` y `aria-live="polite"` anuncian **solo los cambios de estado de la conexión**, nunca cada evento.
- Una opción por usuario «Pausar en vivo» congela las fuentes (WCAG 2.2.2).

#### B2.4 Backend: que la ingesta publique [P]

Este cambio es obligatorio: sin él, `hub_fleet` nunca tendrá datos vivos con el *sink* `ingest`.

1. **Kernel** `runtime/ingest`: `Result` gana `Changes []Change{Key map[string]any; ID string; Action string; Fields []string}`. Es aditivo. `Fields` sale del *diff* real entre `existing` y `values`, y un *update* sin cambios **no** genera `Change`. `Apply` sigue sin publicar: el host publica **después del *commit***, para no repetir el bug que corrige #588.
2. **Manifiesto**: `IngestSpec.publish`, con valores `none|realtime|canonical`. El valor por defecto es `realtime`:
   - `realtime` publica solo hacia `DATA_EVENT`, sin despertar automatizaciones;
   - `canonical` publica un `CanonicalEvent` completo, con `source: "ingest"`, y entra a suscriptores, auditoría y *webhooks* salientes.
   - Además, `IngestSpec.live_ignore: ["last_report_at", ...]` excluye campos de latido del cálculo de `Fields`. Si después del filtro no queda ningún campo, el cambio no se publica.
3. **Ops** `addon_ingest.go`: después de `tx.Commit()` publica cada `Change` con `PublishDataEvent`, que no bloquea y ya coalesce. Con `canonical`, también pasa por `eventBus`.
   - Si un lote tiene más de 200 cambios, se publica **un** `DATA_EVENT{Action:"resync", Model}` en lugar de N eventos.
   - Cada cambio publicado se escribe también en el `eventlog` (para SSE y *replay*), con una retención acotada (§D).

#### B2.5 Seguridad del canal vivo [P]

- **Los eventos no transportan datos**: solo `model`, `action`, `id` y nombres de campo, como el `DATA_EVENT` actual. Toda fila llega por una lectura autorizada (`/api/q` o `r:`), que aplica organización, permisos y `scope` (#633). `patch` sobre una fila fuera del alcance devuelve 404 y se ignora. **Un evento nunca amplía lo visible.**
- **Fuga residual dentro de la organización.** Hoy el socket no filtra por sucursal, así que un usuario restringido ve **ids y nombres de campo** de filas de otras sucursales. Propuesta: el reparto evalúa `allowed` (#633) contra el `BranchID` del evento, que `CanonicalEvent` ya trae, y descarta lo que esté fuera. Los modelos sin sucursal se reparten igual que hoy. Queda como decisión F-7.
- **Autorización por suscripción.** `SUBSCRIBE` (WS) y `?models=` (SSE) se cruzan contra el permiso `<table>.index` del usuario. Un modelo sin permiso se rechaza en silencio con `DATA_SUBSCRIBE_DENIED`.

#### B2.6 Una sola declaración para todas las superficies

`live` vive **en la fuente del bloque con nombre** (`contributions.blocks[].ui.data`), así que la página del sidebar, el widget `kind:block` y el `{type:"ref"}` del chat heredan el mismo comportamiento.

- **Página del sidebar.** Ops conecta `live` en `declarative-addon-page.tsx`, que hoy no hace *polling*.
- **Tablero.** `block-widget.tsx` usa `live` y deja `refresh_s` solo como respaldo.
- **Chat.** `agent-ui-result` recibe una capacidad `live` **restringida**:
  - solo `refetch`;
  - solo mientras el mensaje está visible (`IntersectionObserver`) y durante 15 minutos como máximo;
  - el `show_ui` del modelo **no** puede declarar `live` en fuentes `inline`.

### B3. Activos y código inyectable

#### B3.1 Esquema [P]

`contributions.assets[]` es nuevo. Cada activo se declara **una vez** y lo usan una página, un *widget*, un bloque custom o el chat:

```json
"assets": [
  { "key": "fleet_css", "type": "css", "src": "assets/fleet.3f9a1c2e.css",
    "integrity": "sha384-…", "bytes": 3812, "scope": "layer" },
  { "key": "globe", "type": "module", "src": "assets/globe.8b21aa04.js",
    "integrity": "sha384-…", "bytes": 61234, "isolation": "iframe",
    "capabilities": { "reads": ["data.fleet"], "writes": ["state.filters.country_iso"], "events": ["globe.select"],
                      "network": [], "permissions": [] } },
  { "key": "geo_wasm", "type": "wasm", "src": "assets/geo.c0ffee12.wasm", "integrity": "sha384-…", "bytes": 210000, "isolation": "worker" },
  { "key": "brand_font", "type": "font", "src": "assets/inter-var.11aa22bb.woff2", "integrity": "sha384-…", "bytes": 98000 },
  { "key": "empty_art", "type": "image", "src": "assets/empty.5e5e5e5e.svg", "integrity": "sha384-…", "bytes": 2100 }
]
```

`contributions.custom_blocks[]` es nuevo y **reemplaza al STUB** `slots[].props.block`. Es la propuesta del propio `DEMO-CUSTOM-FRONTEND.md:63-68`, unificada con los activos:

```json
"custom_blocks": [
  { "name": "hub_fleet.fleet_globe@1", "asset": "globe", "expose": "./blocks/FleetGlobe",
    "title": "hub_fleet.block.globe", "description": "hub_fleet.block.globe.desc", "icon": "Globe2",
    "props": { "type": "object", "additionalProperties": false,
               "properties": { "rotate": { "type": "boolean" }, "focus": { "type": "string", "maxLength": 2 } } },
    "reads": ["data.fleet", "state.filters"], "writes": ["state.filters.country_iso"], "data": ["fleet"],
    "events": ["globe.select"], "value_maps": ["presence"], "styles": ["fleet_css"],
    "surfaces": ["page", "widget", "chat"],
    "a11y": { "summary": "hub_fleet.block.globe.summary", "keyboard": true, "role": "application" },
    "analytics": { "events": ["globe.select", "globe.rotate"] },
    "fallback": { "ref": "fleet_map" },
    "min_host": "genui.host/v1.2" }
]
```

| Campo | Regla |
|---|---|
| `type` | `css` \| `module` (ES module; **no** se admite `script` clásico nuevo) \| `wasm` (solo en `worker`) \| `font` (`woff2`) \| `image` (`png`, `webp`, `avif`, o `svg` saneado en la publicación) |
| `src` | Ruta relativa al *bundle*, con *hash* de contenido en el nombre (el mismo patrón de 8 caracteres que ya exige `tools/integrity.mjs`). Prohibidas las URL absolutas. |
| `integrity` | **Obligatorio en todos los activos y en todos los addons**, públicos y privados. Hoy solo es obligatorio para los privados (A.6). Se acepta `sha384` o `sha512`. El *hub* recalcula el valor al publicar y rechaza la diferencia. |
| `bytes` | Lo declara el autor y el *hub* lo verifica. Presupuestos por defecto, revisables: CSS 64 KB gzip por addon; `module` usa `budget_kb` (máximo 150, como `MaxPageBudgetKB`); `wasm` 1 MB; fuente 256 KB; imagen 512 KB; total del addon 2 MB. |
| `scope` (CSS) | `layer` (por defecto; ver B3.2) \| `shadow` (solo dentro de un `custom_block`) \| `global`, que solo se permite a addons de **primera parte** firmados con la clave de plataforma |
| `isolation` (`module`) | `inline` (mismo *realm*, federado como hoy; solo para publicadores de confianza con *bundle* firmado) \| `iframe` (por defecto para terceros) \| `worker` (sin DOM, para cálculo) |
| `capabilities` | `reads`, `writes`, `events` (las mismas gramáticas que `block@1`), `network` (orígenes `https://` exactos, como máximo 8), `permissions` (subconjunto de `geolocation`, `clipboard-write`, `fullscreen`). Nada más. |
| `min_host` | La versión mínima del contrato del host. Si el host es más viejo, se usa `fallback`. |

#### B3.2 Alcance y aislamiento según el riesgo [P]

| Nivel | Qué ejecuta | Aislamiento | Quién |
|---|---|---|---|
| 0 | Nada: genui declarativo con B1, B2 y B4 | El contrato | Cualquiera, incluida la IA |
| 1 | CSS | El *hub* lo parsea al publicar y rechaza `@import`, `url()` hacia fuera de los activos propios, `@font-face` externo, `expression()`, `behavior` y `-moz-binding`. El host lo envuelve en `@layer addon.<key>` y `@scope ([data-addon="<key>"])`. Las variables solo se aceptan con el prefijo `--addon-<key>-*` o leyendo tokens (`var(--tone-*)`). La capa `addon` se declara **antes** que las del host, para que el host gane siempre [S: verificar el orden con las capas de Tailwind v4 que usa Ops]. | Cualquier addon |
| 2a | `module` `inline` | Federación actual: `AddonSlot`, SRI, rango del SDK, *timeout*, `ErrorBoundary`, contexto `block@1`. **Comparte el *realm***: la CSP no puede limitar la red de un módulo concreto. | Primera parte o publicador verificado, con *bundle* firmado (`security.VerifyBundle`) |
| 2b | `module` `iframe` | `<iframe sandbox="allow-scripts">`, **sin** `allow-same-origin`, servido desde un origen aparte (`addons-sandbox.<host>`) con una CSP **generada a partir de `capabilities`**: `default-src 'none'; script-src 'self'; connect-src <network>; style-src 'self'`. El atributo `allow=` del iframe sale de `capabilities.permissions`. El puente `postMessage` implementa `block@1` (`props`, `data` ya resuelto por el host con los permisos del usuario, `emit` solo de `writes`/`events`) con validación de esquema en cada mensaje. El tamaño se ajusta por `ResizeObserver` y el foco pasa por el puente. | Terceros (es el valor por defecto) |
| 3 | `wasm` / `module` `worker` | Un *Worker* dedicado, sin DOM, con `data` de entrada y filas o valores de salida (por ejemplo, un *clustering* geográfico) | Cualquiera |

Encabezados del documento principal (Ops):

- pasar la CSP de `Report-Only` a **aplicada** en dos pasos: primero `report-only` con la política nueva durante 2 semanas, luego aplicada;
- agregar `Permissions-Policy: camera=(), microphone=(), geolocation=(self "https://addons-sandbox.<host>"), payment=()`. Hoy no existe ninguna.

#### B3.3 Caché, firma y versión [P]

- **URL inmutable** por *hash*: `/api/metacore/addons/<key>/assets/<sha384-base64url>.<ext>`, con `Cache-Control: public, max-age=31536000, immutable`.
- `modulepreload` con `integrity`. Ya existe en `ensureRemoteIntegrity`, y se extiende a CSS (`<link rel=stylesheet integrity>`) y a fuentes.
- **Firma.** El *bundle* firmado (Ed25519, `security.VerifyBundle`) cubre el manifiesto, y el manifiesto lista cada `integrity`. Con eso la cadena de confianza llega a cada activo sin firmarlos uno por uno. El host rechaza un activo cuyo *hash* no esté en el manifiesto firmado.
- **`min_host` y `sdk`.** El host compara con su `HostCapabilityProfile` antes de descargar y, si no es compatible, usa `fallback` (un bloque declarativo de nivel 0). Nunca muestra un error en blanco.

#### B3.4 Metadatos y descubrimiento por la IA [P]

- `list_blocks` devuelve los bloques con nombre **y** los `custom_blocks` permitidos para la superficie `chat`. `BlockInfo` gana `{kind:"custom", title, description, icon, props (JSON Schema), surfaces, a11y.summary}`.
- La IA compone con `{type:"ext", ext:"hub_fleet.fleet_globe@1", props:{...}}`, y `show_ui` valida `props` contra el esquema declarado.
- `AgentProfile` **solo** admite `ext` de nivel 2b o 3, nunca de 2a, porque el chat es la superficie con más entradas no confiables.
- El texto de `description` y `a11y.summary` es una clave i18n del addon: se traduce en el idioma del usuario y la IA lo ve en el suyo.
- `analytics.events` es la lista cerrada de eventos que el bloque puede emitir hacia la analítica del host. El host los etiqueta con `addon`, `block`, `surface` y `org`, y nunca reenvía datos de filas.

#### B3.5 Relación con lo actual y antirredundancia

| Hoy | Con B3 |
|---|---|
| `frontend{entry,integrity?}`, un contenedor por addon | Igual, pero `integrity` pasa a ser obligatorio. `frontend.entry` es un activo implícito de `type:module`, `isolation:inline`. |
| `slots[]` `page.block:<a>.<n>@1` + `props.block` (STUB) | `custom_blocks[]`. El *slot* `page.block:` se **genera** a partir de él (Ops ya lo consume) y el kernel valida de forma cruzada que cada `ext` y cada `component` tengan su `custom_block` (corrige los límites 2 y 3 de la demo). |
| `frontend.pages[]` | Se mantiene: una página federada completa es un activo `module` con `expose`. `shares` y `budget_kb` no cambian. |
| Widget `kind:custom` + `expose`, que hoy no monta el *bundle* (límite 1 de la demo) | Se depreca en favor de `kind:block` + `{type:"ext"}`: **un solo camino** |

Un `custom_block` declara `value_maps` y `styles`: **reutiliza** los mapas de valores y el CSS del addon, en vez de redefinir colores o etiquetas dentro del JS. El puente `block@1` le entrega `resolveValue` ya ligado.

### B4. Interactividad y filtrado cruzado

#### B4.1 La base real [H]

- `state` raíz, con la acción `set` hacia `state.*`.
- `map.select` (`statePath`), `tabs.state` (`bindingPath`) y `predicate`/`when` en todos los bloques.
- El estado en la URL `?s.*` existe en el SDK (`host/url-state.ts`, `GenUIBodyDoc.url_state`) y el host Ops lo implementa (`urlState`). **Falta en el esquema del kernel.**
- `PageBus` y `connectBus`.
- `table.filters` está declarado en el esquema y no encontré quién lo consuma.
- `WidgetQuery.Where` tiene operadores y `$now-<dur>`.
- `WidgetLink`.
- En la página `fleet_map` de `hub_fleet` (`manifest.json:3295` en adelante) la fuente se filtra con la URL de `q`: `t:HubInstance?environment=production&is_retired=false&limit=200`.

**Hueco principal:** `dataSource.args` solo admite escalares. El estado no llega al servidor, así que el filtrado cruzado sobre volúmenes grandes es imposible de forma declarativa.

#### B4.2 Una sola gramática de filtro [P]

`$defs/filterSpec` va en genui/v2 y se comparte con `WidgetQuery.Where`, `WidgetLink.filter`, `NavItem.filter` y la URL de listas:

```json
{ "presence": { "in": ["live_24h", "live_7d"] }, "country_iso": "MX",
  "last_report_at": { "gte": "$now-24h" }, "q": { "contains": "acme" } }
```

- Los operadores son los actuales (`eq|neq|gt|gte|lt|lte|contains`) más **`in`** (como máximo 50 valores) e **`is_empty`** (booleano). `is_empty` es necesario para filtrar la fila «vacía» de B1.
- `$now` y `$now-<dur>` funcionan en todos los lugares donde se acepte un filtro, con la misma sustitución por el reloj de la base de datos que ya hace el kernel para los widgets.
- Hay **un evaluador en Go** (servidor) y **su espejo en TS** (cliente), con vectores compartidos. El de TS solo se usa en el modo `client` y en la reevaluación en vivo (B4.7).

#### B4.3 Bloque `filter_bar` y estado compartido [P]

```json
{ "type": "filter_bar", "id": "fb", "model": "hub_fleet.HubInstance", "state": "state.filters",
  "fields": ["presence", "country_iso", "ops_version", "channel", "last_report_at", { "key": "q", "kind": "search", "over": ["name", "host"] }],
  "chips": true, "clear": true, "summary": "hub_fleet.filters.summary" }
```

- **Los controles se derivan del esquema del modelo**: el addon no los redeclara. El host resuelve la metadata del modelo (`GetDynamicMetadata`) y elige el control:

  | Tipo de campo | Control |
  |---|---|
  | `value_map` u `options` | Selección múltiple con etiqueta, ícono y tono de B1, más una opción «Vacío» si el mapa declara `empty` |
  | `boolean` | Tres estados |
  | `number` / `money` | Rango |
  | `date` / `timestamp` | Rango relativo, con los mismos *presets* de `WidgetQuery.Range` |
  | `ref` (FK) | Búsqueda con `/options`, que ya existe |
  | `text` | `contains`, con *debounce* de 300 ms |

  Una entrada objeto (`{key, kind, over}`) solo sirve para controles compuestos, como la búsqueda en varias columnas.
- **El estado es uno**: `state.filters`, con la forma de un `filterSpec`. Cualquier bloque lo lee.
- **Las fuentes se ligan al estado** con un campo nuevo en `dataSource`:

  ```json
  "fleet": { "q": "t:HubInstance?environment=production&is_retired=false&limit=200",
             "filter": { "from": "state.filters", "mode": "auto" } }
  ```

  - `filter.from` es un `statePath`. `filter.only` / `filter.except` son listas de claves y sirven, por ejemplo, para que un KPI de total ignore `presence`.
  - `mode` es `server`, `client` o `auto`:
    - `server` compone el filtro con la consulta: en `q` lo agrega a los parámetros del *token* `t:` y en `widget` lo agrega al cuerpo de `widgets/data` (`{keys, scope, filters}`). La gramática exacta de filtros con operadores dentro de `t:` está **[S] por verificar** en el parser de `/api/q`; hoy solo vi igualdades `col=valor`.
    - `client` filtra las filas ya cargadas con el evaluador TS. Solo es válido si la fuente trae `limit` de 500 o menos.
    - `auto` usa `client` cuando la fuente devolvió menos filas que su `limit`, porque el conjunto está completo, y `server` en cualquier otro caso.
- **URL compartible.** Se agrega `url_state: ["filters", "selection"]` al esquema del kernel (cierra la deriva), y queda `?s.filters=<json codificado>`, con el *encoder* del SDK. El enlace es compartible y reproducible; el receptor ve la intersección con **su** alcance.
- **Alcance (#633).** El `filterSpec` se combina con `ScopeFilter` en el servidor mediante `ApplyScope`, **siempre con AND**. Un filtro nunca amplía. Un valor fuera de `allowed` se descarta y el bloque muestra el aviso «filtro fuera de tu alcance». Las dimensiones de `scope` (`market`, `org_unit`, `date`) se dibujan en `ScopeBar`, no en `filter_bar`, para no duplicar: `filter_bar` es para los campos del modelo.

#### B4.4 Filtrado cruzado bidireccional [P]

`cross_filter` se agrega a `map`, `chart` (segmentos y barras), `list`, `stats`, `metric`, `kpi_card` y a la leyenda del mapa:

```json
"cross_filter": { "field": "presence", "state": "state.filters", "toggle": true, "multi": true }
```

- Un clic o `Enter` sobre un segmento, una burbuja o un elemento de la leyenda escribe `state.filters.presence = {in:[v]}`. Con `toggle`, si el valor ya estaba, lo quita. `multi` permite acumular con Ctrl o Shift.
- **Bidireccional.** Todo bloque con `cross_filter` sobre el mismo `field` marca lo seleccionado (`aria-pressed=true`) y atenúa lo demás con un `--tone-muted` de opacidad accesible.
- **Chips.** `filter_bar` con `chips: true` muestra los filtros activos, cada uno como un botón «Quitar filtro: Presencia = En línea», más «Limpiar filtros».
- `map.select` sigue siendo la **selección** de un punto (el detalle), que es distinta del **filtro**: no se mezclan.

**Accesibilidad:**

- Los elementos de la leyenda y los segmentos son `button` con `aria-pressed`.
- El mapa conserva su `summary` obligatorio y una tabla alternativa (las pestañas `map|cards|table` ya existen en `hub_fleet`).
- Cuando cambian los filtros, un `aria-live="polite"` anuncia «N resultados».
- El foco no se pierde al refrescar.

#### B4.5 Que los filtros viajen a la consulta [P]

- `/api/q`, `/api/dashboard/widgets/data` y las exportaciones aceptan el `filterSpec` canónico y lo compilan con **el mismo** compilador que `WidgetQuery.Where`, que ya existe en el kernel.
- La clave de caché de los widgets suma `hash(filterSpec canónico)`, igual que el `scope` del RFC #633.
- Límites:
  - como máximo 12 claves de filtro;
  - como máximo 50 valores en `in`;
  - un filtro sobre una columna que no está en el modelo o no es filtrable responde `filter_field_unknown`.

#### B4.6 Drill-through [P]

En este punto hay que separar lo que existe, lo que se está haciendo y lo que propone este RFC.

- **Existe en el kernel** [H]: `WidgetLink{model, filter}` o `{url}`.
- **Se está implementando** [I], por otro agente en `feat/dashboard-widget-link`: Ops proyecta `link` en `WidgetSpec` y lo pinta como clic hacia `/m/<model>?f_<col>=<v>`. Este RFC no duplica ese trabajo; lo extiende en forma aditiva.
- **Lo que agrega este RFC:**

  ```json
  "link": { "model": "HubInstance", "derive": true, "by_series": true }
  ```

  - `derive: true` toma el filtro **del `where` del propio widget** (`query.where`), así que no se repite. Exige que `link.model == query.model`.
  - `by_series: true` agrega a ese filtro el segmento sobre el que se hizo clic: `group_by = bucket.key`, o `is_empty: true` si el segmento es la fila vacía.
  - `filter` explícito, si también está, se combina con AND y gana sobre el derivado en la misma clave.
  - **Filtros relativos** (`$now-24h`): se recomienda que la URL de listas acepte la gramática canónica, `f_last_report_at=gte:$now-24h`, y que el servidor la sustituya con el mismo motor. Así el enlace sigue siendo correcto si se abre mañana: no se congela una fecha. La alternativa (F-11) es usar columnas booleanas derivadas como `alive_24h`, que `hub_fleet` ya tiene, pero eso acopla cada ventana a una columna y no escala.
  - **En páginas genui**: la acción `navigate` gana `route.filters_from: "state.filters"`, que lleva el filtro activo de la pantalla a la lista completa con un solo clic («Ver en tabla»).

#### B4.7 Datos vivos y filtros juntos [P]

| `mode` | Con un filtro activo |
|---|---|
| `refetch` | Vuelve a ejecutar la fuente con el filtro activo |
| `patch` | Relee la fila por id con el filtro aplicado en el servidor. Si ya no cumple, sale de la vista; si ahora cumple, entra. En el modo `client`, se reevalúa con el evaluador TS. |
| `append` | Inserta la fila nueva solo si cumple el filtro |

Además:

- Los conteos de la leyenda y de los *chips* se recalculan en el mismo ciclo.
- Si cambia el filtro mientras hay un *refetch* en curso, el *refetch* se aborta con `AbortSignal` (ya existe en `SourceRequest`).

---

## C. Matriz de responsabilidades y fases

| Fase | Entrega | Kernel | SDK | Ops | Hub / addon | Tamaño | Riesgo | Releases y reinicios |
|---|---|---|---|---|---|---|---|---|
| **0** | Arreglo de hoy con el contrato actual (G, pasos 0-2) | — | — | — | `hub_fleet` 0.4.2: `options` en `presence`, `status_tones` y claves `map.status.live_*` corregidas | S | Bajo | Publicar `hub_fleet` y reinstalar el addon en el hub. **No** requiere reiniciar Ops. |
| **1** | B1 mapas de valores y tokens de tono | `valuemap`, esquemas, expansión, `AggregateBucket` aditivo (v0.283) | `resolveValue`, `ValueBadge`, `humanize` único, `--tone-*`/`--on-tone-*` (con `onColor` del primitivo de contraste ya *mergeado*), `motion` y su *lint* | Expansión y auto-resolución, `Series` aditivo, adopción de `ValueBadge` | `hub_fleet` 0.5.0 | M | Medio: toca todos los *renderers* | Kernel, SDK (genui, maps, theme, runtime-react) y Ops; *deploy* de Ops con reinicio del backend |
| **2** | B2 en vivo, transporte WS | `$defs/live`, `ingest.Result.Changes`, `IngestSpec.publish/live_ignore` (v0.284) | Capacidad `live` en `GenUIBody`, `LiveIndicator`, *backoff* con *jitter* | Publicación desde `addon_ingest.go` tras el *commit*, `hostRealtime` hacia `live`, filtro por `allowed` en el reparto, autorización de `SUBSCRIBE` | `hub_fleet` 0.6.0 (`live` en los bloques) | M | Medio: tormenta de eventos (mitigada con `fields`, `live_ignore` y *resync* por lote) | Kernel, SDK y Ops, más reinicio del backend. El replicador **no** cambia. |
| **3** | B4 filtros y *drill-through* | `filterSpec` (+`in`, `is_empty`), `filter_bar`, `cross_filter`, `url_state`, `dataSource.filter`, `link.derive/by_series` (v0.285) | `FilterBar`, *chips*, `cross_filter` en mapa, gráfica y lista, evaluador TS | `/api/q` y `widgets/data` con `filters`; URL de listas con `$now`; extensión del *drill-through* que se está implementando | `hub_fleet` 0.7.0 | L | Medio: rendimiento de consultas filtradas (índices) | Kernel, SDK y Ops |
| **4** | B2 SSE + `eventlog` | Ninguno nuevo (`eventlog` ya existe) | Cliente SSE con `Last-Event-ID` por encabezado | `/api/live/stream`, *ticket*, retención | — | M | Bajo | Ops |
| **5** | B3 activos, `custom_blocks`, iframe *sandbox* | Esquemas, validación cruzada `ext`↔`custom_block`, `integrity` obligatorio (con ventana de compatibilidad) | Puente `block@1` sobre `postMessage`, carga de CSS y fuentes con SRI | Origen *sandbox*, CSP generada, `Permissions-Policy`, CSP aplicada | El *hub* recalcula `integrity`, parsea CSS y sanea SVG | L | Alto: superficie de seguridad nueva | Todos. El origen *sandbox* necesita DNS y certificado. |

El orden lo fija el valor visible y la dependencia:

- 0 → 1 arregla lo que el dueño ve.
- 2 depende de 1 para `LiveIndicator`.
- 3 depende de 1 para los controles de `enum`.
- 5 al final, por riesgo.

## D. Seguridad y modelo de amenazas

| Amenaza | Vector | Mitigación |
|---|---|---|
| XSS por CSS | `url(javascript:)`, `expression()`, exfiltración por selectores de atributo (`input[value^=a]{background:url(...)}`) | Parseo al publicar, `url()` solo hacia activos propios con *hash*, CSP `img-src`/`font-src 'self'`, `@scope` al subárbol del addon |
| *Clickjacking* o suplantación visual por CSS | Un addon pinta encima de la barra del host (`position:fixed`, `z-index` alto) | `@scope` + capa de menor prioridad; `position:fixed` y `z-index` mayor a 10 se rechazan fuera de `scope: shadow` [P] |
| XSS o robo de sesión por JS | Un módulo `inline` lee `localStorage` o llama a la API con la sesión | El nivel 2a solo se permite con *bundle* firmado por un publicador verificado. Terceros siempre en iframe sin `allow-same-origin` y desde otro origen. El *token* deja de viajar en la URL (B2.2). |
| Exfiltración de datos | Un bloque recibe filas y las envía fuera | Iframe con `connect-src` = `capabilities.network` (por defecto `'none'`). `data` solo trae las fuentes declaradas en `reads`, resueltas con los permisos del usuario. La analítica solo lleva nombres de eventos. |
| Cadena de suministro | Dependencia comprometida en el *bundle*, o un CDN que cambia el archivo | SRI obligatorio en **todo** activo, URL por *hash*, firma Ed25519 del manifiesto, recálculo del *hash* en el *hub* y *build* reproducible (Node 24, ya pedido por `integrity.mjs`) |
| DoS por SSE o WS | Muchas conexiones o suscripciones, tormenta de ingesta | Límites actuales (200 eventos/s y 64 modelos por conexión, *inbox* de 4 096 con descarte contado) más: como máximo 3 conexiones vivas por usuario, *ticket* de un solo uso, *resync* por lote mayor a 200, `min_interval_ms` por fuente, pausa con la pestaña oculta y *backoff* con *jitter* (evita la manada al reiniciar Ops) |
| Fuga entre organizaciones | Un evento o una fila de otra organización | El reparto ya es por `OrgID` (`flushDataEvents`); `channelAllowed` compara con el JWT. Los eventos no traen datos y toda lectura pasa por `ApplyScope`. Hay pruebas de aislamiento en la matriz de CI. |
| Fuga entre sucursales | Ids y nombres de campo de otras sucursales por el socket | Filtro de reparto por `allowed` (B2.5, F-7) |
| Inyección por filtros | `filterSpec` con columnas o operadores arbitrarios | Lista blanca de columnas filtrables desde la metadata del modelo, compilación parametrizada (el mismo motor de `Where`), límites de B4.5 |
| Elevación por `ref` | La IA referencia un bloque o mapa que el usuario no puede ver | `ExpandRefs` falla si no está en el catálogo filtrado (ya es así para bloques); se extiende a `value_maps` y `custom_blocks` |
| Riesgo de accesibilidad | Animaciones que provocan crisis | `motion` cerrado, `reduce` obligatorio, `blink` a 1 Hz o menos, *lint* en CI |

## E. Comparativa con alternativas [S]

Ninguna de estas afirmaciones sobre terceros está verificada en esta revisión. Son el estado del arte tal como lo conozco y hay que revisarlas antes de usarlas en público.

| Criterio | SAP Fiori elements [S] | Oracle VB / APEX [S] | Bitrix24 [S] | Retool / Appsmith [S] | Grafana / Metabase / Superset [S] | **Este estándar** [P] |
|---|---|---|---|---|---|---|
| Presentación por valor | Anotaciones OData `UI.Criticality` y `Common.Text` por propiedad, definidas una vez en el modelo | Por componente | Por campo de lista | Expresiones JS por componente (`{{ }}`) | *Value mappings* por panel (Grafana), no por modelo | `value_map` en el campo, heredado por todas las superficies, incluido el chat de IA; tono semántico con contraste calculado; `motion` accesible |
| En vivo | Push por WebSocket, depende de la app | Depende de la app | Eventos del portal | Recarga de *query* o *polling* | *Polling* del panel; *streaming* en algunos *datasources* | Declarativo por fuente: `refetch|patch|append`, degradación, estado accesible, sin datos en el evento |
| Código inyectado | Extensiones UI5 en el *realm* | JS en página | Apps en iframe | JS por todos lados, mismo *realm* | *Plugins* firmados (Grafana) | Escalera de riesgo de 0 a 3, SRI obligatorio, CSP derivada de las capacidades, firma, *fallback* declarativo |
| Filtrado cruzado | *Smart filter bar* derivada de anotaciones | Facetas | Filtros de lista | Manual, con *bindings* | Nativo (Metabase y Superset) | Derivado del modelo, compartido por bloques, del lado del servidor con alcance, URL compartible y también en el chat |
| IA | Copilotos sobre Fiori (Joule) | — | CoPilot | Generación de apps | — | La IA compone **el mismo** contrato validado (`show_ui` + `list_blocks`) con perfil restringido y auditoría |

La diferencia que reivindicamos es una sola: **una declaración, todas las superficies, con el mismo validador en Go y en TS**. Es una afirmación de diseño [P]. Que ningún competidor la cumpla es un supuesto [S].

## F. Decisiones abiertas para el dueño

1. **¿El `value_map` se liga al campo del modelo, que se auto-resuelve en todas partes, o solo se referencia de forma explícita?** Recomendación: **ligarlo al campo**, con la referencia explícita como opcional. Es lo que elimina la redundancia.
2. **¿`options[]` sigue existiendo junto a `value_map`?** Recomendación: sí. `options` es la lista de valores **permitidos** (validación y formularios) y `value_map` es la **presentación**. Si un campo solo tiene `options`, el host deriva el mapa (B1.3). Para la presentación, `options.color` se depreca.
3. **Vocabulario de tono: ¿se queda en 6 o se agrega `accent`?** Recomendación: 6 tonos. `neutral` queda como alias de `muted` y no se agregan más hasta que haya un caso de uso.
4. **¿Ofrecemos `motion` desde el principio?** Recomendación: sí, con los 5 valores y como máximo 2 por mapa. Con `reduce` hay sustitución estática obligatoria y el movimiento se detiene en 5 s o menos, salvo `spin`.
5. **Transporte vivo para la SPA: ¿el WS `DATA_EVENT` actual o SSE nuevo?** Recomendación: **WS en la fase 2**, porque ya existe y aísla por organización, y **SSE en la fase 4** para *embeds* y hosts externos.
6. **¿Qué publica la ingesta por defecto?** Recomendación: `realtime`, que no despierta automatizaciones; `canonical` como opción explícita por modelo.
7. **¿Filtramos el socket por sucursal (`allowed`)?** Recomendación: sí, cuando exista `allowed` (#633). Antes de eso, aceptar el riesgo de que se vean solo ids.
8. **¿`patch` relee por id o el evento trae los valores?** Recomendación: **relee**, y el evento nunca trae datos. Cuesta una lectura extra y a cambio no hay fuga.
9. **¿`integrity` obligatorio también para addons públicos?** Recomendación: sí, con una ventana de compatibilidad de una versión menor del kernel (aviso y luego error) y el recálculo hecho por el *hub*.
10. **¿Aislamiento por defecto para terceros: iframe o *realm* compartido?** Recomendación: **iframe *sandbox* desde otro origen**. El *realm* compartido queda solo para primera parte y publicadores verificados con firma.
11. ***Drill-through* con ventanas relativas: ¿`$now` en la URL de listas o columnas booleanas derivadas?** Recomendación: **`$now` en la URL**, con una sola gramática de filtro.
12. **Filtrado `auto` cliente/servidor: ¿con qué umbral?** Recomendación: cliente solo si el conjunto vino completo (filas devueltas menores que `limit`) y `limit` es 500 o menos. Si no, servidor.
13. **¿`live` y `ext` en el chat de IA?** Recomendación: `live` solo con `refetch` y solo mientras el mensaje está visible (15 minutos como máximo); `ext` solo de nivel 2b o 3.
14. **¿Se depreca el widget `kind:custom` + `expose`?** Recomendación: sí, a favor de `kind:block` + `ext`. Un solo camino.
15. **¿Se aplica ya la CSP (hoy en `Report-Only`) y se agrega `Permissions-Policy`?** Recomendación: sí. Dos semanas en `report-only` con la política nueva y luego aplicada, antes de la fase 5.

## G. Migración de `hub_fleet` paso a paso

| Paso | Versión | Cambio | Requiere |
|---|---|---|---|
| 0 | — | **Diagnóstico** [S]: en la base de datos del hub, `SELECT presence, count(*) FROM hub_fleet_hub_instances GROUP BY 1` (tabla por verificar) y averiguar el origen de las 9 vacías. Confirmar qué *sink* usa el replicador en producción (`REPLICATOR_SINK`). | Acceso de solo lectura |
| 1 | `hub_fleet` 0.4.2 | Arreglo con el contrato **actual**, sin primitivos nuevos. (a) `presence` gana `options` `live_24h`/`live_7d`/`stale` con `label` `hub_fleet.presence.*` y `color` (`green`/`blue`/`amber`): con eso Ops traduce el *donut* por `makeEnumLabelResolver`. (b) `status_tones` del bloque `fleet_map` pasa a `{"live_24h":"success","live_7d":"info","stale":"warning"}`. (c) Claves `map.status.live_24h|live_7d` en es-MX y en-US. (d) Si el diagnóstico lo confirma, *backfill* de las vacías con el replicador (`-resync`) [S]. | Publicar y reinstalar el addon. **Deja tres declaraciones del mismo estado**: es deuda explícita que el paso 2 elimina. |
| 2 | `hub_fleet` 0.5.0, kernel ≥ v0.283, SDK genui/maps/theme con B1, Ops con B1 | `contributions.value_maps[presence]` (B1.1, con `empty` «Sin datos» y `unknown`) y `presence.value_map: "presence"`. **Se eliminan** `options` de `presence`, `status_tones` de `fleet_map` y las claves `map.status.live_*`. La tabla de presencia de la página, el *donut*, la leyenda del mapa y el `live_events` (`kind` y `outcome` pasan a `value_maps` `event_kind` y `event_outcome`, y la tabla los referencia) quedan con **una declaración por estado**. `compatibility.requires.kernel` sube a `>=3.2.0` [S: confirmar la correspondencia entre v0.283 y la versión de compatibilidad]. | Kernel, SDK y Ops primero; después el addon |
| 3 | `hub_fleet` 0.6.0, kernel ≥ v0.284, Ops con B2 | `HubInstance.ingest.publish: "realtime"` con `live_ignore: ["last_report_at","updated_at"]`, y lo mismo en `HubInstanceEvent`. `live` en las fuentes de `fleet_map` (`patch`), `live_events` (`append`, `cap: 8`) y en el widget `instances_by_presence` (`refetch`, `fields:["presence"]`). `refresh_s` baja su papel a respaldo (60). | Desplegar Ops (que publica tras la ingesta) **antes** de la versión del addon. El replicador no cambia. |
| 4 | `hub_fleet` 0.7.0, kernel ≥ v0.285, Ops con B4 | `filter_bar` en la página `fleet_map` (`presence`, `country_iso`, `ops_version`, `channel`, `last_report_at`, `q`), `filter.from: state.filters` en las fuentes, `cross_filter` en la leyenda del mapa y en el *donut*, `url_state: ["filters"]`. Widgets `online_now`, `down_now` e `instances_by_presence` con `link: {model:"HubInstance", derive:true, by_series:true}`. Esto **reemplaza** los `link` explícitos que agregue el trabajo de *drill-through* en curso. | Kernel, SDK y Ops |
| 5 | Opcional | Si se quiere un globo 3D: `custom_blocks[fleet_globe@1]` con `isolation: iframe`, `fallback: {ref:"fleet_map"}`, `value_maps: ["presence"]`. Sin esto, todo lo anterior funciona con nivel 0. | Fase 5 |

### G.1 Antes y después: el bloque `fleet_map` [P]

Antes (0.4.1), lo que se declara hoy:

```json
{ "type": "map", "source": "data.fleet", "fields": { "status": "presence", "...": "..." },
  "group_by": "status", "legend": true,
  "status_tones": { "online": "success", "down": "danger", "off": "muted", "stale": "warning" } }
```

A esto se suman `HubInstance.presence` sin `options`, el *donut* sin etiquetas y las claves `map.status.*` que no coinciden.

Después (0.7.0):

```json
{ "type": "map", "source": "data.fleet", "fields": { "status": "presence", "...": "..." },
  "group_by": "status", "legend": true,
  "cross_filter": { "field": "presence", "state": "state.filters", "toggle": true } }
```

El tono, la etiqueta, el ícono, el `motion`, el orden y el valor vacío salen del `value_map` del **campo** `presence`, y ese mismo mapa usan el *donut* del Tablero, la tabla de la página, el filtro y el chat de IA.

---

## Referencias de código

- Kernel:
  - `genui/v2/schema/genui-v2.schema.json` (`tone` L67, `column` L349-408, `dataSource` L1377-1415, `block_ext` L3126, `block_map` L3244-3364, `action_set` L1081, `action_refresh` L1141);
  - `manifest/v3/types.go` (L374, L385, L568-593, L843, L2040-2168, L2290-2312, L2808-2815);
  - `manifest/v3/ui.go` (L27-48, L117-160);
  - `manifest/v3/block_refs.go`;
  - `manifest/v3/option_display.go`;
  - `runtime/ingest/ingest.go` (L95-119, L309);
  - `dynamic/canonical_events.go:37`;
  - `eventlog/doc.go`;
  - `ws/hub.go`;
  - `agent/show_ui_tool.go`;
  - `agent/ui_host.go`;
  - `security/signature.go:64-71`.
- Kernel, trabajo en curso: `C:\Projects\wt-kernel-contrast\colorx\` (sin commitear).
- SDK:
  - `packages/genui/src/{table-view.tsx,tones.ts,map-points.ts,host/contract.ts,host/url-state.ts,host/use-sources.ts,host/body.tsx,page/custom.ts,page/blocks.tsx}`;
  - `packages/maps/src/{field-labels.ts,genui.tsx,palette.ts,theme.ts}`;
  - `packages/runtime-react/src/{dashboard-types.ts,dashboard-grid.tsx,widgets/renderers.tsx,slots/addon-slot.tsx,motion.ts}`;
  - `packages/sdk/src/integrity.ts`;
  - `packages/theme/{styles/tokens.css,src/contrast.ts}`;
  - `packages/notifications/src/sse.ts`;
  - `packages/agent-workspace/src/workspace/agent-ui-result.tsx`.
- SDK, trabajo en curso: `C:\Projects\wt-sdk-contrast\packages\theme\src\contrast.ts` (sin commitear).
- Ops (`wt-ops-widget-link` en 1e2df9d):
  - `backend/services/websocket/data_events.go`;
  - `backend/services/realtime_data_bridge.go`;
  - `backend/services/addon_ingest.go`;
  - `backend/services/dashboard_catalog.go`;
  - `backend/handlers/notification_stream.go`;
  - `backend/services/hubevents/client.go`;
  - `frontend/src/features/metacore/{genui-host.tsx,page-host-events.ts,page-custom-blocks.tsx,host-slot-catalog.ts}`;
  - `frontend/src/lib/realtime/host-realtime.ts`;
  - `frontend/src/context/websocket-provider.tsx`;
  - `frontend/src/features/dashboard/{block-widget.tsx,block-spec.ts}`;
  - `frontend/Dockerfile:54`.
- Hub: `backend/cmd/hub-replicator/{contract.go:316-324,main.go:95}`.
- hub-addons:
  - `packages/hub_fleet/manifest.json` (0.4.1: L116-144, L446-452, L2790-2805, L3295, L3494-3527, L3550-3644, L4117);
  - `packages/hub_fleet/locales/*.json:320-328`;
  - `feat/demo-custom-frontend:docs/DEMO-CUSTOM-FRONTEND.md`.
