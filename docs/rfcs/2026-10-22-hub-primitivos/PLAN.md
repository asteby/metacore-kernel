# Plan de implementación: primitivos para el Hub sobre OPS

Acompaña a las RFC-01..08 de esta carpeta. Prioriza lo que desbloquea **M1** (`hub_fleet` de solo
lectura) frente a M2 (licencias y catálogo) y M3 (retiro del front viejo).

## 1. Qué necesita cada hito

| Hito | Imprescindible | Recomendado para salir | Puede esperar |
|---|---|---|---|
| **M1** arranque | O-0 (fail-closed), O-0b (instalación limpia), A-0 (lint), S-0 (fecha relativa + avatar), O-L (login por `brand.json`), A-1a (adopción sin kernel) | — | — |
| **M1** salida | — | K-0 → K-3 → O-3 (sumidero + `exclusive`), K-4 → O-5 (KPI 24 h y enlace), A-1b | — |
| **M2** | K-1 → O-1 (acción webhook firmada), K-2 → O-2 + S-1 (`set_fields` + confirmación escrita), N-1 (núcleo verifica), H-3 (`unlisted`) | K-5 → O-4 (permisos del addon en auditoría) | `enforce` de O-4 |
| **M3** | — | — | quitar `bridge.SignedWebhookInterceptor` sin firma; promover reglas toleradas a error |

M1 **arranca** sin tocar el kernel: la réplica entra por la API de datos con un token `sk_` (demostrado
en el spike) y el rol `hub_operator` no recibe `update` sobre `Instance` (RFC-05 §2, opción a). El
sumidero (RFC-04) es el criterio de **salida** de M1 porque cierra G4 de verdad y retira el `sk_`.

## 2. Orden de bumps

Regla: el **contrato** (schema + validadores + conversión) y el **runtime** salen en tags distintos.
Hub, `manifestcheck` y `addon-preflight` solo validan → necesitan el contrato. OPS ejecuta → necesita el
runtime. Así hay **una** cadena de bumps de validadores para todos los primitivos (mitiga el riesgo
«cadena de pins atrasa M2» del documento de migración).

```
Ola 0  (sin kernel)  O-0, O-0b, A-0, S-0, O-L, H-3, A-1a ............ en paralelo, ya
Ola 1  K-0 contrato ──► tag v0.230.0
Ola 2  B-1a addons: manifestcheck + addon-preflight → v0.230.0   (un PR; check-hub-kernel-compat.sh)
       B-1b hub: backend/go.mod → v0.230.0, desplegado y verificado   ← GATE para publicar addons
       K-1, K-2, K-3, K-4, K-5 runtime (paralelo; K-3 rebasa sobre K-2) ──► tag v0.231.0
       S-1 SDK (paralelo desde Ola 1; solo depende de los nombres del contrato) ──► release SDK
Ola 3  O-B ops: backend/go.mod → v0.231.0 (solo el bump + arreglos de compilación)
       O-1, O-2, O-3, O-4, O-5, O-6 (paralelo, rebasados sobre O-B) + O-F (frontend sube el SDK)
       N-1 núcleo (hub): verificación de firmas + replicador al sumidero (cuando exista el núcleo, M0)
Ola 4  A-1b hub_fleet adopta los campos nuevos (solo con hub y ops desplegados)
```

Orden pedido: kernel → manifestcheck + addon-preflight → ops → hub → SDK. Se cumple con dos reglas que
vienen de incidentes (`backfills` v0.115, `connector_connected` v0.116, `provides_options` v0.117,
`Column.visibility` addons#1870):

1. **El hub tiene que estar desplegado con v0.230.0 antes de que cualquier addon publique un campo
   nuevo.** Si va atrás, el PR de addons sale verde, el release recibe `400 additional properties … not
   allowed` y el hub sigue sirviendo la versión anterior sin que nadie lo note. Comprobar con
   `gh run view <release> --log | grep -E "HTTP 400|FAILED"` tras el primer publish.
2. **El SDK se publica antes de que ops lo consuma** (O-F), y ops valida su lockfile con
   `pnpm install --frozen-lockfile` (no `--lockfile-only`).

Si D5 crea `asteby-hq/hub-addons` con su propio `manifestcheck`, ese pin se sube en B-1a también.

## 3. Reglas comunes a todos los briefs

- Trabajar **siempre** en un worktree nuevo desde `origin/main` del repo indicado (nunca en el checkout
  principal, nunca en `main`): `git -C <repo> fetch origin && git -C <repo> worktree add
  ~/projects/_worktrees/<rama-corta> -b <rama> origin/main`. No tocar `/home/user/projects/ops` (checkout
  compartido).
- Compilar y probar con `nice -n 19`. Kernel: `GOWORK=off`; los paquetes `runtime/wasm` necesitan CGO.
  Ops: `cd backend && GOWORK=off nice -n 19 go test ./...` (o el subconjunto afectado).
- Commits sin `Co-Authored-By` ni «Generated with». PR con `gh pr create`; **no** mergear sin
  autorización explícita del dueño en el turno.
- Schema del kernel: editar **ambas** copias (`manifest/v3/schema/manifest-v3.schema.json` y
  `docs/spec/v3/manifest-v3.schema.json`) y comprobar `cmp` byte a byte. Todo campo nuevo del contrato
  va en **los dos validadores** (`manifest/v3/validate.go` y `manifest/validate.go`) y en el round-trip
  `from_v3.go` → `manifest.*Def` → `modelbase.*` (mismas etiquetas JSON).
- Endurecer una validación = regla tolerada en `manifest/v3/compat.go` (aviso una minor; ver
  `docs/validation-compat-window.md`).
- Nada de producción ni servidores: pruebas con `httptest`, bases en contenedor propio (como el spike),
  nunca contra el OPS de desarrollo compartido (`:8080`). No imprimir secretos.
- Cada brief lista los archivos que **posee**; si necesita tocar otro, que sea mínimo y lo diga en el PR.

## 4. Briefs de implementación

### O-0 · ops · fail-closed de disparadores sin despachador (RFC-01 Fase 0)

- **Objetivo:** que ninguna acción, schedule, webhook entrante u `on_transition` responda éxito si no
  existe código que lo despache.
- **Repo / rama:** `asteby-hq/ops` · `fix/acciones-fail-closed`.
- **Archivos:** `backend/services/addon_service.go` (solo el bloque de binding de acciones de
  `HotRegisterDeclarative`, ~1340-1475), `backend/services/pipeline_runtime.go` (`logSkipDispatcher`),
  `backend/services/wasm_action_kernel.go` (comentario/condición del no-op), tests nuevos.
- **Hacer:** rama `Trigger.Type == "webhook"` sin `rec.Hooks` → handler que devuelve un error tipado
  `handler_not_dispatchable` mapeado a HTTP 501 por `actionErrorResponse`; idem para `set_fields` (valor
  futuro; aceptar cualquier `Trigger.Type` desconocido como 501). `logSkipDispatcher.Dispatch` devuelve
  error (`ErrNoDispatcher`). El no-op de `InvokeWasmAction` solo cuando `Trigger == nil`.
- **Aceptación:** test que reproduce el spike (acción v3 `handler.type: webhook`, sin wasm) → 501 y 0
  peticiones; acción sin handler (`confirm` UI-only) sigue 200; `do: "webhook:x"` en un schedule queda
  marcado como fallido en el log; suite de `services` y `handlers` en verde.
- **Dependencias:** ninguna. **Prioridad:** P0. **Tamaño:** S.

### O-0b · ops · instalación limpia: `addon_installations` y índice parcial (G3, G11)

- **Objetivo:** que un addon instalado en una base nueva funcione sin crear tablas a mano.
- **Repo / rama:** `asteby-hq/ops` · `fix/instalacion-limpia-addon-installations`.
- **Archivos:** `backend/services/addon_service.go` (solo `IsEnabled`, ~900-940), los lectores de
  `models.AddonInstallation` que decidan la puerta de datos (`handlers/dynamic.go:687`,
  `services/addon_role_catalog.go:~178`), `backend/models/dynamic_unique_indexes.go` (`uniqueIndexDDL`,
  `duplicateGroupsSQL`), tests.
- **Hacer:** reproducir primero en `deploy/selfhost/test/clean-install` (o un test con base vacía). Decidir
  con evidencia: (a) que la puerta lea `metacore_installations` (la que escribe el instalador del kernel)
  con *fallback* a la legada, o (b) crear/migrar `addon_installations` en el arranque. `IsEnabled` no debe
  ignorar el error de la consulta (hoy `count` queda 0 y responde 403). Calificar el esquema del addon en
  el DDL del índice único (`"addon_<key>"."<tabla>"`) en vez de depender del `search_path`.
- **Aceptación:** base limpia + `devctl bootstrap --wasm-addons <addon_de_prueba>` → `GET /api/data/<Model>`
  200 y la acción aparece en la metadata; el índice parcial se crea en `addon_<key>`, no en `public`.
- **Dependencias:** ninguna (toca `addon_service.go` en otra región que O-0; rebasar si chocan).
  **Prioridad:** P0. **Tamaño:** S.

### A-0 · addons · lint de manifest para addons privados (RFC-05 §2, RFC-06 §A)

- **Objetivo:** que los addons privados del Hub nazcan con permisos que resuelven y réplicas protegidas.
- **Repo / rama:** `asteby-hq/addons` · `feat/manifestcheck-lint-privados` (y el mismo código en
  `hub-addons` si D5 lo crea).
- **Archivos:** `tools/manifestcheck/*` (sin cambiar su pin de kernel).
- **Hacer:** reglas `rbac_permission_resolves` (RFC-06 §A), `readonly_without_protected` en modelos con
  `metadata.visibility: private` o con `ingest` (cuando exista), `webhook_literal_http` (handler webhook
  con `url` `http://`). **Error** para manifests `private`; **aviso** para el resto (no romper los 17
  públicos). Mensajes con el arreglo escrito.
- **Aceptación:** `go test ./tools/manifestcheck/...` en verde sobre todo el repo; un fixture con el
  manifest del spike falla con los tres mensajes esperados.
- **Dependencias:** ninguna. **Prioridad:** P0. **Tamaño:** S.

### S-0 · SDK · `display: "relative_date"` y avatar de marca (RFC-08 §1-2)

- **Objetivo:** «último reporte hace X» con tono por frescura y avatar con logo/color de la fila.
- **Repo / rama:** `asteby/metacore-sdk` · `feat/runtime-react-fecha-relativa-avatar-marca`.
- **Archivos:** `packages/runtime-react/src/dynamic-columns.tsx` (caso nuevo `relative_date`; caso
  `avatar`: `image_field`, `color_field`, `subtitle_field`, `shape`), `activity-value-renderer.tsx` si
  reutiliza el formateo, tests en `src/__tests__/`, changeset minor de `@asteby/metacore-runtime-react`.
- **Aceptación:** tests de celda (sin umbrales, `fresh_within`/`stale_after`, valor vacío, tooltip
  absoluto en la zona de la org); avatar con imagen, sin imagen con `color_field` (contraste AA), sin
  nada (comportamiento previo intacto); `pnpm -r test` del paquete en verde; `pnpm install --frozen-lockfile`.
- **Dependencias:** ninguna (sin kernel). **Prioridad:** P0. **Tamaño:** S.

### O-L · ops · login por marca, camino `brand.json` (RFC-07)

- **Objetivo:** que una instancia con marca propia muestre su copy y su logo en el login, sin cifras
  inventadas.
- **Repo / rama:** `asteby-hq/ops` · `feat/login-por-marca`.
- **Archivos:** `backend/services/brandfile/*`, persistencia de `instance_brand` (migración `login jsonb`),
  endpoints que sirven la marca antes del login (`/api/org/branding`, `/api/brand/boot.js`),
  `frontend/src/features/auth/sign-in/index.tsx`, `frontend/src/components/brand-mark.tsx`,
  `docs/BRAND-JSON-POR-ARCHIVO.md`.
- **Hacer:** aceptar el bloque `login` (RFC-07 §2) en `brand.json`; panel derecho con `HeroPanel` /
  `TileGrid` ya publicados en `@asteby/metacore-auth/showcases/blocks`; quitar `stats` por omisión;
  reproducir y arreglar el logo ancho con el SVG del spike (`presets-privados/hub/assets/hub-logo.svg` de
  la rama `spike/hub-fleet`). El camino **preset** (`presetBranding.Login`) queda para O-6.
- **Aceptación:** `brand.json` con `login` → login con ese copy; sin `login` → copy genérico **sin**
  cifras; clave desconocida dentro de `login` → `brandfile: brand.json rechazado field=login.x`; el SVG
  ancho se ve completo en claro y oscuro (captura en el PR).
- **Dependencias:** ninguna. **Prioridad:** P0. **Tamaño:** S-M.

### H-3 · hub · visibilidad `unlisted` y por versión (RFC-03)

- **Objetivo:** tercer nivel de visibilidad y versiones no listadas, sin tocar el manifest.
- **Repo / rama:** `asteby-hq/hub` · `feat/visibilidad-unlisted`.
- **Archivos:** `backend/internal/migrations/sql/00NN_addon_visibility_unlisted.sql` (y `addon_versions.listed`),
  `backend/internal/store/{models.go,gorm_store.go,memory.go}`, `backend/internal/api/{catalog.go,addons_list.go,private_access.go,publish.go,admin.go}`,
  tests (`api/addon_visibility_test.go`).
- **Aceptación:** pruebas de RFC-03 §5 en verde; `public` del manifest no re-lista una fila `unlisted`;
  `private` del manifest la cierra.
- **Dependencias:** ninguna. **Prioridad:** P2 (puede ir en la Ola 0). **Tamaño:** S.

### A-1a · addons · `hub_fleet` sin kernel nuevo (adopción M1)

- **Objetivo:** que el spike quede usable en M1 con lo que ya existe.
- **Repo / rama:** donde decida D5 (`hub-addons` o `addons` `spike/hub-fleet` → rama propia).
- **Hacer:** `rbac.permissions` declaradas con `models` (RFC-06 §A); rol `hub_operator` sin `update`
  sobre `Instance`; `NavItem.filter: {"environment": "production"}` en la entrada por omisión y otra
  entrada «Desarrollo»; `display: relative_date` en `last_seen_at` y `display: avatar` de marca (requiere
  S-0 publicado y ops con ese SDK); quitar la acción `request_revoke` hasta O-1 (o dejarla, ya falla
  con 501 tras O-0).
- **Aceptación:** pasa el lint A-0; con un OPS efímero (receta del spike) la tabla filtra producción por
  omisión y un `hub_operator` recibe 403 al editar.
- **Dependencias:** A-0, S-0 (+ ops con el SDK). **Prioridad:** P0. **Tamaño:** S.

### K-0 · kernel · contrato v0.230.0 (todas las RFC)

- **Objetivo:** un solo PR con **todos** los campos nuevos del manifest, validados en los dos
  validadores y con round-trip completo. Sin runtime.
- **Repo / rama:** `asteby/metacore-kernel` · `feat/v3-contrato-hub-primitivos`.
- **Archivos:** `manifest/v3/types.go`, `manifest/v3/branding.go`, `manifest/v3/validate.go`,
  `manifest/v3/ui_validate.go`, `manifest/v3/compat.go`, `manifest/v3/schema/manifest-v3.schema.json`,
  `docs/spec/v3/manifest-v3.schema.json`, `manifest/manifest.go`, `manifest/from_v3.go`,
  `manifest/validate.go`, `modelbase/metadata.go`, tests `manifest/v3/*_test.go` y
  `manifest/from_v3_*_test.go`, `CHANGELOG.md`.
- **Contenido:** RFC-01 §2.1 (`Handler.url_ref|url_setting|path|method|signing|send_record|timeout_ms|retries|on_success`;
  regla tolerada `webhook_action_signing`); RFC-02 §2.1 (`Handler.type: set_fields`, `Handler.set` con
  tokens, `Action.confirm_typed`, `Action.tone`); RFC-04 §2.1 (`Model.ingest`, `verify:
  metacore-hmac-v1`, `do: upsert:<Model>` y `validHookPrefixes`); RFC-06 §A (regla tolerada
  `rbac_permission_resolves`); RFC-07 (`Branding.login`, schema de `Preset.defaults.branding`); RFC-08 §3
  (`range` + `last_hour|last_24_hours`, `$now-<dur>` en `where`, `DashboardWidget.link`). Los campos de
  despacho del webhook (`url*`, `path`, `signing`) **no** se exponen en `modelbase.ActionMetadata`
  servido (solo `trigger.type`); `set` tampoco.
- **Aceptación:** `cmp` de ambos schemas; tests válidos/inválidos por campo; round-trip v3 →
  `ActionDef`/`ModelDefinition` → JSON → de vuelta sin pérdida; el manifest del spike + los campos nuevos
  valida en estricto; los 17 manifests de `addons/origin/main` siguen validando (correr manifestcheck con
  `replace` local); `GOWORK=off nice -n 19 go test ./manifest/... ./modelbase/...` en verde.
- **Dependencias:** ninguna. **Bloquea:** B-1a, B-1b, K-1..K-5, O-B. **Prioridad:** P1 (empezar ya).
  **Tamaño:** M.

### B-1a · addons · subir kernel a v0.230.0 en manifestcheck y addon-preflight

- **Repo / rama:** `asteby-hq/addons` · `chore/kernel-v0.230.0-validadores`.
- **Archivos:** `tools/manifestcheck/go.mod|go.sum`, `tools/addon-preflight/go.mod|go.sum`.
- **Aceptación:** CI verde incluido `tools/check-hub-kernel-compat.sh` (fallará hasta que B-1b esté
  mergeado y desplegado: es lo esperado; mergear B-1a **después** de B-1b).
- **Dependencias:** tag v0.230.0. **Tamaño:** S.

### B-1b · hub · subir kernel a v0.230.0 y desplegar

- **Repo / rama:** `asteby-hq/hub` · `chore/kernel-v0.230.0`.
- **Archivos:** `backend/go.mod|go.sum` y los arreglos de compilación que salgan.
- **Aceptación:** tests del backend en verde; tras el despliegue (lo hace el dueño/pipeline), un publish
  de prueba de un manifest con un campo nuevo no da 400. Sin tocar nada más.
- **Dependencias:** tag v0.230.0. **Gate de:** cualquier publish de addons con campos nuevos. **Tamaño:** S.

### K-1 · kernel · runtime `runtime/webhookout` (RFC-01 Fase 2)

- **Repo / rama:** `asteby/metacore-kernel` · `feat/runtime-webhookout`.
- **Archivos (propios):** `runtime/webhookout/*` (nuevo), `security/request_signer.go` (nuevo:
  `RequestSigner`, canónica `asteby-req-v1`, `VerifyInstanceRequest`), `dynamic/action.go` (registrar el
  dispatcher `webhook` para `ExecAction`, mínimo), `bridge/webhook_adapter.go` (deprecación documentada;
  quitar el fallback sin firma detrás de una opción).
- **Aceptación:** pruebas de RFC-01 §5 (kernel) en verde; `Invoke` no sigue redirecciones, respeta
  presupuesto, reintenta solo 5xx/429/red, rechaza hosts fuera de allowlist e IP privadas tras DNS salvo
  `AllowInsecure`.
- **Dependencias:** K-0 mergeado. **Tamaño:** M.

### K-2 · kernel · runtime `set_fields` (RFC-02)

- **Repo / rama:** `asteby/metacore-kernel` · `feat/runtime-action-set-fields`.
- **Archivos (propios):** `dynamic/action_set.go` (nuevo), `dynamic/validate.go` (`WithProtectedWrite`),
  `dynamic/service.go` (`WithExpectedState` en `Update`), `dynamic/action.go` (dispatcher `set_fields`),
  tests.
- **Aceptación:** pruebas de RFC-02 §5 (runtime) en verde, incluido el caso concurrente (una 200, otra
  409 `state_changed`) y que una columna `protected` no listada sigue rechazada.
- **Dependencias:** K-0. **Bloquea:** K-3 (usa `WithProtectedWrite`). **Tamaño:** M.

### K-3 · kernel · runtime `ingest` + `metacore-hmac-v1` entrante (RFC-04)

- **Repo / rama:** `asteby/metacore-kernel` · `feat/runtime-ingest-upsert`.
- **Archivos (propios):** `dynamic/ingest.go` (nuevo), `runtime/webhookin/webhookin.go` (esquema nuevo,
  `NonceStore` inyectable), `runtime/webhookin/ingest.go` (nuevo, `IngestDispatcher`), la regla
  `ingest.exclusive` en `dynamic/validate.go` (rebasar sobre K-2).
- **Aceptación:** pruebas de RFC-04 §5 en verde (incluida la regresión exacta de G4 y lotes
  concurrentes sin duplicados).
- **Dependencias:** K-0, K-2 (para `WithProtectedWrite`; se puede desarrollar en paralelo y rebasar).
  **Tamaño:** M-L.

### K-4 · kernel · agregados: rangos cortos y `$now` (RFC-08 §3)

- **Repo / rama:** `asteby/metacore-kernel` · `feat/aggregate-rangos-now`.
- **Archivos (propios):** `query/aggregate.go` y tests.
- **Aceptación:** `last_hour`, `last_24_hours` y `$now-15m` con reloj fijo; `compare: previous_period`
  con las ventanas nuevas.
- **Dependencias:** K-0. **Tamaño:** S.

### K-5 · kernel · permisos del addon desde `capabilities` (RFC-06 §B)

- **Repo / rama:** `asteby/metacore-kernel` · `feat/from-v3-permisos-desde-capabilities`.
- **Archivos (propios):** `manifest/from_v3.go` (solo la derivación de `Permissions`; coordinar si K-0
  sigue abierto), test `manifest/from_v3_permissions_test.go`.
- **Aceptación:** tabla de casos de RFC-06 §3; ningún cambio de schema.
- **Dependencias:** K-0 mergeado (mismo archivo). **Tamaño:** S.

→ Con K-1..K-5 mergeados: **tag v0.231.0**.

### S-1 · SDK · confirmación escrita, `tone: danger`, KPI con enlace, showcase de login (RFC-02, 07, 08)

- **Repo / rama:** `asteby/metacore-sdk` · `feat/runtime-react-confirmacion-escrita`.
- **Archivos:** `packages/runtime-react/src/action-modal-dispatcher.tsx` y el diálogo de confirmación
  (campo «Escribe X», botón deshabilitado, envía `confirm_text`), `types.ts` (`confirmTyped`, `tone`,
  `link` del widget), menú de acciones de fila (agrupa `danger` al final con variante destructiva),
  interpolación `{{record.x}}` en `confirmMessage`, widget clicable; `packages/auth/src/showcases/blocks`
  (`BrandLoginShowcase`). Changesets minor.
- **Aceptación:** tests de cada comportamiento; un `ActionMetadata` sin los campos se ve igual que hoy.
- **Dependencias:** solo los nombres de K-0 (puede empezar en la Ola 1). **Tamaño:** S-M.

### O-B · ops · subir kernel a v0.231.0

- **Repo / rama:** `asteby-hq/ops` · `chore/kernel-v0.231.0`.
- **Archivos:** `backend/go.mod|go.sum` + arreglos de compilación. Nada funcional.
- **Aceptación:** `GOWORK=off nice -n 19 go build ./... && go test ./services/... ./handlers/...` en verde.
- **Dependencias:** tag v0.231.0. **Bloquea:** O-1..O-6. **Tamaño:** S.

### O-1 · ops · acción webhook firmada (RFC-01 Fase 2)

- **Repo / rama:** `asteby-hq/ops` · `feat/accion-webhook-firmada` (sobre O-B).
- **Archivos:** `backend/services/addon_service.go` (rama `webhook` con fuente de URL → `webhookout.Invoke`),
  `backend/services/instanceidentity/*` (implementar `security.RequestSigner`), serialización de
  metadata de acciones (no servir config del handler), `backend/handlers/dynamic.go` (mapeo de los
  códigos `webhook_*`); opcional y en commit aparte: honrar `Action.idempotency` en `ExecuteAction`.
- **Aceptación:** test de integración de RFC-01 §5 (OPS) con receptor `httptest`; con
  `OPS_ENV=development OPS_WEBHOOK_ALLOW_INSECURE=1` funciona contra `127.0.0.1`; sin ellos, rechaza.
- **Dependencias:** O-B, O-2 si se usa `on_success.set`. **Tamaño:** M.

### O-2 · ops · `set_fields` y `confirm_typed` (RFC-02)

- **Repo / rama:** `asteby-hq/ops` · `feat/accion-set-fields` (sobre O-B).
- **Archivos:** `backend/services/addon_service.go` (rama `set_fields`), `backend/handlers/dynamic.go`
  (`confirm_typed` antes del handler para cualquier tipo), `backend/models/models.go` (retirar
  `ToState`/`FromStates` muertos si nadie los usa).
- **Aceptación:** pruebas OPS de RFC-02 §5; el `PUT` genérico sobre una columna `protected` sigue 422.
- **Dependencias:** O-B. **Tamaño:** S.

### O-3 · ops · sumidero `upsert` (RFC-04)

- **Repo / rama:** `asteby-hq/ops` · `feat/sumidero-ingest` (sobre O-B).
- **Archivos:** `backend/services/pipeline_runtime.go` (dispatcher `upsert`, `NonceStore` en Postgres o
  Redis), `backend/handlers/integrations.go` (`GET` de `high_water`, límite 1 MiB, addon habilitado para la
  org, rate limit), migración de la tabla de nonces si va a Postgres.
- **Aceptación:** pruebas OPS de RFC-04 §5 (firma, replay, aislamiento, exclusividad); latencia de un
  lote de 500 filas < 2 s en el contenedor de pruebas.
- **Dependencias:** O-B. **Tamaño:** S-M.

### O-4 · ops · permisos del addon en modo auditoría (RFC-06 §B)

- **Repo / rama:** `asteby-hq/ops` · `feat/permisos-addon-auditoria` (sobre O-B).
- **Archivos:** `backend/services/addon_permissions.go`, tests.
- **Aceptación:** `ADDON_PERMISSION_ENFORCE=audit` (por omisión) no cambia respuestas y registra «habría
  denegado»; `enforce` deniega; v2/compiled sin permisos siguen en legacy.
- **Dependencias:** O-B. **Tamaño:** S.

### O-5 · ops · KPI con enlace y `$now` (RFC-08 §3)

- **Repo / rama:** `asteby-hq/ops` · `feat/kpi-enlace-now` (sobre O-B).
- **Archivos:** servicio del catálogo de widgets (`services/dashboard_catalog*.go`) y
  `handlers/dashboard_widgets.go`: pasar `link`, delegar `range`/`$now` al kernel.
- **Aceptación:** widget con `$now-15m` cuenta bien con reloj fijo; `link` llega al frontend.
- **Dependencias:** O-B; el clic lo pinta S-1. **Tamaño:** S.

### O-6 · ops · login por preset (RFC-07, camino `branding_file`)

- **Repo / rama:** `asteby-hq/ops` · `feat/login-por-preset` (sobre O-B y O-L).
- **Archivos:** `backend/services/preset_branding.go` (`Login`), `org_brandings.login` (migración),
  cascada org → instancia → preset.
- **Aceptación:** preset con `defaults.branding_file` que trae `login` → el login lo muestra en una
  instancia nueva; una marca de org editada a mano no se pisa.
- **Dependencias:** O-B, O-L. **Tamaño:** S.

### O-F · ops · frontend sube el SDK

- **Repo / rama:** `asteby-hq/ops` · `chore/sdk-runtime-react-<versión>`.
- **Archivos:** `frontend/package.json`, `pnpm-lock.yaml`.
- **Aceptación:** `pnpm install --frozen-lockfile` y build en verde; la confirmación escrita, el `tone`
  y la fecha relativa se ven en un OPS efímero.
- **Dependencias:** S-0 y S-1 publicados. **Tamaño:** S.

### N-1 · hub (núcleo) · verificar acciones firmadas y replicar al sumidero

- **Repo / rama:** `asteby-hq/hub` · `feat/nucleo-firma-y-replicador` (cuando exista `cmd/asteby-trust`, M0).
- **Hacer:** en `/core/v1/*`, verificar `metacore-hmac-v1` (`security.Signer.VerifyWithNonce` +
  `NonceCache`) o `instance-ed25519-v1` (`security.VerifyInstanceRequest` contra la llave del plano de
  control en `ops_instances`), deduplicar por `X-Metacore-Delivery`, exigir `actor` y `reason`;
  replicador que lee `GET /core/v1/fleet/changes?since=` y empuja lotes firmados al sumidero, reanudando
  con `high_water`.
- **Aceptación:** pruebas con un OPS efímero (receta del spike): acción → 1 petición verificada; borrar
  la réplica y reanudar desde `high_water = 0` reconstruye la tabla sin duplicados; latencia p95 < 2 min.
- **Dependencias:** K-1 (verificador), O-1, O-3 desplegados en el OPS de pruebas. **Tamaño:** M.

### A-1b · addons · `hub_fleet` con los primitivos nuevos

- **Hacer:** `ingest` + `webhooks[]` `upsert:Instance` (y quitar el `sk_`), `retire` con `set_fields` +
  `confirm_typed` + `tone: danger`, `request_revoke` con `url_ref` + `signing`, KPIs con `last_24_hours`,
  `$now-15m` y `link`, `login` en `defaults/branding.json` del preset.
- **Aceptación:** manifestcheck (v0.230.0) en verde; publish al hub sin 400; flujo completo en un OPS
  efímero con el núcleo simulado.
- **Dependencias:** B-1b desplegado, O-1..O-6 y O-F en el OPS destino. **Tamaño:** S-M.

## 5. Mapa de conflictos (para lanzar en paralelo sin pisarse)

| Archivo | Briefs que lo tocan | Cómo se evita el choque |
|---|---|---|
| kernel `manifest/v3/types.go`, schemas, validadores, `from_v3.go` | K-0 (dueño), K-5 | K-5 empieza cuando K-0 está mergeado |
| kernel `dynamic/validate.go` | K-2 (dueño), K-3 | K-3 rebasa sobre K-2 |
| ops `backend/go.mod` | O-B (dueño) | O-1..O-6 parten de O-B |
| ops `services/addon_service.go` | O-0 (binding de acciones), O-0b (`IsEnabled`), O-1, O-2 (ramas nuevas del binding) | regiones distintas; O-1 y O-2 añaden ramas adyacentes: el segundo en mergear rebasa |
| ops `services/pipeline_runtime.go` | O-0, O-3 | O-3 parte de main con O-0 ya mergeado |
| ops `frontend/src/features/auth/sign-in/index.tsx` | O-L (dueño), O-6 | O-6 sobre O-L |
| SDK `runtime-react/src/dynamic-columns.tsx` | S-0 (dueño) | S-1 no lo toca |
