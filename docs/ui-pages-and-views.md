# Páginas federadas, vistas declarativas y paneles de detalle

Contrato de manifest (v3, aditivo) para las superficies de UI de un addon más
allá de la tabla dinámica. El kernel define la gramática, la valida, la proyecta
al host y decide qué capacidades de host exige; **no** pinta nada: los
renderers viven en el SDK y el montaje en el host.

Regla rectora: **declarativo primero, federado después.** Cada nivel cuesta más
JavaScript que el anterior; se sube de nivel solo cuando el anterior no alcanza.

| Nivel | Qué es | JS del addon | Manifest |
|---|---|---|---|
| A. Vista declarativa | `map`, `timeline`, `calendar`, `chart`, `split` sobre un modelo | 0 | `NavItem.view_type` + `NavItem.view` |
| B. Panel declarativo | Bloques del detalle de un registro | 0 | `models[].detail.panels[]` |
| C. Pieza federada en un panel | Un componente propio dentro de un detalle | solo esa pieza | panel `kind: custom` + `expose` |
| D. Página embebida | Flujo propio dentro del chrome del host | una ruta (≤ 150 KB gzip) | `frontend.pages[]` con `layout: shell` |
| E. Página a pantalla completa | POS, kiosco, KDS | la app | `frontend.pages[]` con `layout: immersive` |

## Ejemplo completo

[`spec/v3/examples/addon-ui-example.json`](spec/v3/examples/addon-ui-example.json)
(addon `shipping`) usa todo: vistas split/map/calendar/chart/timeline, kanban,
paneles map (con ruta), timeline por capacidad, relation y stats, y una página
embebida "Torre de envíos". La prueba `TestValidate_RepoExamples` lo valida.

## `frontend.pages[]`

```jsonc
"frontend": {
  "entry": "/api/metacore/addons/shipping/frontend/dist/remoteEntry.js",
  "format": "federation",           // obligatorio con pages[]
  "expose": "./register",           // headless (acciones, slots): sin cambios
  "layout": "shell",                // layout por omisión de las páginas
  "load": "route",
  "sdk": "^4.0.0",                  // rango de @asteby/metacore-sdk del build
  "pages": [{
    "key": "control_tower",         // snake_case, único
    "path": "control-tower",        // kebab-case, sin "/" inicial, único
    "expose": "./pages/control-tower", // ≠ frontend.expose
    "layout": "shell",              // shell = embebida | immersive = pantalla completa
    "title": "shipping.pages.control_tower",
    "icon": "Map",
    "permission": "shipping.shipment.read",
    "requires": [{ "model": "Shipment", "actions": ["index"] }],
    "shares": ["@asteby/metacore-maps"],
    "budget_kb": 120,               // gzip, sin shares; 0/omitido = 150
    "breadcrumbs": [{ "title": "shipping.nav.group" }]
  }]
}
```

- **Nombres en el wire**: `shell | immersive` (los mismos de `frontend.layout`,
  ya publicados). Docs y UI dicen "embebida" y "pantalla completa"; no hay
  alias (`embedded`, `fullscreen` se rechazan).
- **Ruteo**: el host decide dónde montar **antes** de bajar el remote.
  `layout` efectivo = el de la página, si no `frontend.layout`, si no `shell`
  (`FrontendPage.EffectiveLayout`). Embebida: `/a/<addon>/<path>/*`; pantalla
  completa: `/addons/<addon>/<path>/*`. Un nav enlaza con
  `"url": "addon://<addon>/<path>"`; `FrontendSpec.Page(path)` resuelve la
  página.
- **Permisos**: `permission` y `requires` usan el vocabulario de
  `NavItem` (`requires` se proyecta a `modelo.acción` igual que
  `v3.ProjectNavRequires`). El host no monta la página sin permiso (403 propio).
- **Shares** (conjunto cerrado, `v3.PageShares`): `@asteby/metacore-maps`,
  `maplibre-gl` → `ui.maps`; `@asteby/metacore-charts`, `recharts` →
  `ui.charts`. El remote nunca empaqueta su copia.
- **Presupuesto**: `budget_kb` ≤ `v3.MaxPageBudgetKB` (150). El hub lo aplica
  al publicar (fase hub).
- **Límites**: 32 páginas, 4 migas.
- **`frontend.sdk`**: rango semver; `EvaluateCompatibility` lo compara con
  `HostCapabilityProfile.SDKVersion` (campo `frontend.sdk`; sin versión del
  host falla cerrado). Convive con `compatibility.requires` `"sdk"`.
- **Manifest lite**: `pages[]` viaja recortado a `key, path, expose, layout,
  title, icon, permission` (≤ 300 B por página, probado). `requires`,
  `shares`, `budget_kb` y `breadcrumbs` solo en el manifest completo.
- `registerRoute` en runtime sigue funcionando: `pages[]` vacío = comportamiento
  anterior.

## Vistas declarativas (`view_type` + `view`)

`view_type` es un conjunto cerrado: `table` (por omisión), `kanban` (sin cambios,
`group_by`), y las declarativas `map`, `timeline`, `calendar`, `chart`, `split`.
Las declarativas exigen `model` y se configuran con **solo** el bloque `view.<tipo>`
correspondiente (otro bloque es error). `view` en `table`/`kanban` es error.

El bloque `view` viaja con la metadata de tabla bajo demanda (el host lo obtiene
con `Manifest.NavView(model, viewType)`); el manifest lite solo lleva
`view_type`.

| Tipo | Campos | Reglas |
|---|---|---|
| `map` | `point {lat, lng}` \| `geometry`, `cluster`, `color_by`, `popup[]` (≤ 8), `layers[]` (≤ 8: `kind points\|zones`, `model`, `point`/`geometry`, `icon`, `label`, `color_by`), `fit` (`data`\|`org_default`) | exactamente uno de `point`/`geometry`; `route` solo en paneles |
| `timeline` | `date_field`, `title_field`, `status_field?`, `description_field?`, `group_by?` | en nav lista las filas del modelo; `source` solo en paneles |
| `calendar` | `start_field`, `end_field?`, `title_field?`, `color_by?`, `all_day_field?` | |
| `chart` | `kind` (`bar\|line\|area\|pie\|donut`), `query` (= `WidgetQuery` del tablero), `format?` | mismas reglas que un widget; `query.model` = el modelo del nav |
| `split` | `columns?`, `panels?` | maestro-detalle; por omisión columnas del nav y paneles del modelo |

Las columnas se verifican contra el modelo propio (más `id`, `organization_id`
y las columnas de auditoría del kernel). Modelos de otro addon
(`"locations.Branch"`) no se verifican.

## Paneles de detalle (`models[].detail.panels[]`)

```jsonc
"detail": { "panels": [
  { "key": "route", "kind": "map", "title": "shipping.panel.route",
    "map": { "point": { "lat": "dest_lat", "lng": "dest_lng" },
             "route": { "relation": "events", "point": { "lat": "lat", "lng": "lng" }, "order_by": "at" } } },
  { "key": "tracking", "kind": "timeline", "title": "shipping.panel.tracking", "span": "half",
    "timeline": { "date_field": "at", "title_field": "description", "status_field": "status",
      "source": { "capability": "commerce.shipping.track",
                  "input": { "tracking_number": "record.tracking_number", "carrier": "record.carrier" },
                  "items": "events" } } },
  { "key": "history", "kind": "relation", "title": "…", "relation": { "name": "events", "limit": 20 } },
  { "key": "figures", "kind": "stats", "title": "…", "stats": { "items": [{ "field": "cost", "format": "currency" }] } },
  { "key": "custom", "kind": "custom", "title": "…", "expose": "./panels/route-planner" }
] }
```

- `kind` elige el bloque leído: `map`, `timeline`, `chart`, `relation`,
  `stats`, o `custom` → `expose` (requiere `frontend` con `format:
  federation`). Otro bloque presente es error. ≤ 12 paneles, `key` única,
  `span` `full|half`.
- `map` en panel: el punto del registro y/o `route` (hijos ordenados por una
  relación); con `route` el punto propio es opcional.
- `timeline` en panel exige `source`: `relation` (filas hijas; los campos se
  verifican en el modelo hijo) **o** `capability` + `input` + `items`. La
  entrada solo lee el registro (`record.<columna>` o `const:<literal>`). Si el
  contrato es conocido (`capability.Contracts`): debe ser `query`, `input` debe
  mapear sus campos requeridos y solo campos suyos, `items` debe ser un arreglo
  de objetos de su salida y `date_field`/`title_field`/… campos de esos objetos.
  El host ejecuta la consulta con los permisos del usuario y caché corta.
- `chart` en panel exige `relation`; `query.model` debe ser el hijo de esa
  relación (el host filtra por la llave foránea).
- `relation`: `name` de `models[].relations`, `columns` del hijo, `limit`
  1..50.
- `stats`: 1..8 cifras, `field` del modelo, `format`
  `number|currency|percent|compact|date|datetime|relative_date`.

## Capacidades de host

Un host declara lo que implementa en `HostCapabilityProfile.Capabilities`. El
manifest puede declarar capacidades explícitas en
`compatibility.host_capabilities` (v3, nuevo; se proyecta a
`Manifest.HostCapabilities`), pero las de UI **se infieren** de lo que el
manifest usa (`Manifest.RequiredHostCapabilities`):

| Capacidad | La exige |
|---|---|
| `ui.pages` | cualquier `frontend.pages[]` |
| `ui.embedded_pages` | una página con layout efectivo `shell` |
| `ui.views.map` / `.timeline` / `.calendar` / `.chart` / `.split` | `view_type` declarativo; paneles `map`/`timeline`/`chart` |
| `ui.detail_panels` | `models[].detail.panels` o paneles de `split` |
| `ui.maps` / `ui.charts` | `shares` de mapas / gráficos |

`EvaluateCompatibility` reporta cada faltante como `IssueMissingCapability`
(campo `host_capabilities`) con el motivo: *manifest uses navigation view_type
"map", which needs host capability "ui.views.map"; this host does not provide
it — upgrade the host first*. Así un host viejo rechaza la instalación en vez
de pintar una tabla vacía o no montar una página.

`installer.Installer.HostCapabilities` (opcional): si no es nil, `Install` y
`Upgrade` rechazan con `Manifest.CheckHostCapabilities`. Nil conserva el
comportamiento anterior. Un host que lo configure debe listar **todas** sus
capacidades (también `wasm-runtime`, `webhooks`… si sus addons las declaran).

## Compatibilidad

- Todo es aditivo: manifests publicados siguen validando (prueba
  `TestUIGrammarIsAdditive` y la batería existente). Las reglas nuevas solo
  aplican a campos nuevos, así que no necesitan ventana de compatibilidad.
- La validación es cerrada: JSON Schema por tipo (`FrontendPage`, `NavView`,
  `MapView`, `TimelineView`, `CalendarView`, `ChartView`, `SplitView`,
  `ModelDetail`, `DetailPanel`…, `additionalProperties: false`) más reglas
  cruzadas en Go con rutas exactas (`contributions.navigation[0].items[1].view.map.point.lat
  "latitude" is not a column of model "Shipment"`).
- El portador legacy (`manifest.Manifest`) recibe `FrontendSpec.SDK`,
  `FrontendSpec.Pages`, `NavItem.View` y `ModelDefinition.Detail` (tipos v3 tal
  cual) y su `Validate` aplica las reglas estructurales
  (`v3.ValidateFrontendUI`, `v3.ValidateNavView`, `v3.ValidateModelDetail`).
- Hosts que no entienden un `view_type` siguen cayendo a `table`, pero con las
  capacidades inferidas no deberían llegar a instalar ese addon.

## Fuera del kernel

- Renderers declarativos, `AddonPageProps`, hooks de host y presupuesto por
  expose: SDK.
- Rutas `/a/$key/$` y `/addons/$key/$`, sidebar por layout, servir `view` y
  `detail` en la metadata de tabla, CSP: host (ops).
- Validar que cada `pages[].expose` exista en el remote y medir el chunk contra
  `budget_kb`: hub al publicar.
- Implementación de `geo.geocode` / `geo.route`: addons proveedores (ver
  [capabilities-geo.md](capabilities-geo.md)).
