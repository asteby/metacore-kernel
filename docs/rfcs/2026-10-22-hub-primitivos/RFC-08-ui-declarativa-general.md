# RFC-08: huecos generales de UI declarativa (fecha relativa, avatar de marca, KPIs)

- **Prioridad:** §1-§2 P0 (M1, solo SDK); §3 P1 (paridad de conteos de M1).
- **Tamaño:** §1 S, §2 S, §3 S.
- **Fuente de requisitos:** hub `feat/panel-ux` `docs/HUB-OPS-UX-REQUISITOS.md` («último reporte hace X»,
  avatar con logo/color de la instancia e iniciales solo si no hay marca, KPI clicables que fijan el filtro
  en la URL, instancias de desarrollo excluidas por omisión).

## 0. Lo que ya existe (descartado como primitivo nuevo)

| Requisito | Ya cubierto por | Evidencia |
|---|---|---|
| Filtros, búsqueda, orden y página en la URL | `DynamicTable` `enableUrlSync` (`f_*`, `sortBy`, `order`, `page`, `per_page`, `search`) | SDK `runtime-react/src/dynamic-table.tsx:600-700` [V] |
| Filtro por omisión (`environment = production`) | `NavItem.filter` (cosmético) o `locked_scope` (seguridad) | `manifest/v3/types.go:2061-2073` [V] |
| Insignias de estado con color | `display: status/badge` + `options[].color`, o `stage_field`/`stages` | spike [S]; `types.go:840-869` [V] |
| KPIs y gráficas del Resumen | `contributions.dashboard[]` (`DashboardWidget` + `WidgetQuery`: count/sum/avg/min/max, `where`, `group_by`, `date_field`, `interval`, `range`, `compare`) y ops lo sirve | `types.go:1946-2035`, ops `handlers/dashboard_widgets.go` [V] |
| Mapa | `NavItem.view_type: map` + `view.map` (`MapView`) | `types.go:2075-2084` [V] |

## 1. Columna «hace X» (`display: "relative_date"`)

**Problema.** `formatDateCell` solo formatea absoluto (`runtime-react/src/dynamic-columns.tsx:504-540`;
caso `date|datetime|timestamp` en `:1204-1221`) [V]. `formatDistanceToNow` ya se usa en el historial
(`activity-timeline.tsx:216`), no en la tabla.

**Diseño** (solo SDK: `Column.display` es texto libre en el schema y el validador legado no lo
restringe, `manifest/validate.go:157-190` valida `widget`, no `display` [V] → **sin bump de kernel**):

```json
{ "name": "last_seen_at", "type": "timestamp", "label": "hub_fleet.field.last_seen_at",
  "display": "relative_date",
  "display_config": { "fresh_within": "15m", "stale_after": "1h", "tooltip": "absolute" } }
```

- Celda: «hace 3 min» (`date-fns`, locale del usuario), tooltip con la fecha absoluta en la zona de la org.
- `fresh_within` / `stale_after` (duraciones `30s|15m|6h|7d`, opcionales): tono verde / ámbar / rojo
  (un punto de color antes del texto). Sin ellos, texto neutro.
- Se refresca cada 60 s sin refetch (re-render local).
- `summary` del detalle y la exportación siguen usando el valor absoluto.

Esto cubre «último reporte hace X» y la señal visual de «en línea / caída / apagada» sin columna
calculada. Si el estado de salud tiene que ser **filtrable** (KPI «caídas»), lo calcula la fuente (el
núcleo, que tiene la telemetría) y llega por el sumidero (RFC-04) como columna `health`: el kernel no
tiene, a propósito, fórmulas dependientes de `now()` (las `formulas` son aritmética pura de la fila,
`types.go:937-963`).

## 2. Avatar de marca como tipo de columna

**Problema.** El caso `avatar|creator|user` ya pinta imagen + nombre + subtítulo e iniciales de respaldo
(`dynamic-columns.tsx:1224-1287`, `AvatarCell` `:855-882`, `resolveAvatarSrc` `:669-689`) [V], pero:
la imagen solo sale del valor de la columna o de un hermano `<padre>.avatar|photo`; el respaldo usa
siempre el color primario; no hay forma de declarar «color de marca de la fila».

**Diseño** (solo SDK, aditivo en `display_config` del caso `avatar`):

```json
{ "name": "brand_name", "type": "text", "label": "hub_fleet.field.brand",
  "display": "avatar",
  "display_config": { "image_field": "brand_logo_url", "color_field": "brand_color",
                      "subtitle_field": "customer", "shape": "rounded" } }
```

- `image_field`: columna hermana con la URL/ruta del logo (misma resolución `base_path` que hoy).
- `color_field`: hex de la fila para el fondo del respaldo (contraste AA calculado; iniciales en tinta
  clara u oscura).
- `subtitle_field`: alternativa explícita a `description`.
- `shape`: `rounded` (por omisión, logos) | `circle` (personas).
- Sin `image_field` ni valor: iniciales sobre `color_field`; sin ambos: comportamiento actual.

## 3. KPIs: ventanas cortas, `now` relativo y enlace a la lista filtrada

**Problema.**
- `WidgetQuery.range` es un enum sin ventanas cortas: `last_7_days|last_30_days|last_12_months|
  this_month|this_year|this_day|all` (schema `WidgetQuery.range`; `query/aggregate.go:19-40`) [V]. El
  criterio de salida de M1 pide «instancias vivas 24 h / 7 d» y el Resumen «en línea / caídas».
- `where` compara con literales (`types.go:2008-2013`): no se puede decir «`last_seen_at` ≥ ahora − 15 min».
- Un KPI no puede llevar a la lista filtrada («KPI clicables que fijan el filtro en la URL»):
  `DashboardWidget` no tiene enlace (`types.go:1946-2000`) [V].

**Diseño** (kernel: contrato v0.230.0 + `query/aggregate.go`; ops sirve; SDK pinta):

```json
"contributions": { "dashboard": [{
  "key": "instances_online",
  "title": "hub_fleet.kpi.online",
  "kind": "stat",
  "query": {
    "model": "Instance",
    "aggregate": "count",
    "where": { "environment": "production", "last_seen_at": { "gte": "$now-15m" } }
  },
  "link": { "model": "Instance", "filter": { "environment": "production" } }
}, {
  "key": "instances_active_24h",
  "title": "hub_fleet.kpi.active_24h",
  "kind": "stat",
  "query": { "model": "Instance", "aggregate": "count", "date_field": "last_seen_at",
             "range": "last_24_hours", "where": { "environment": "production" } }
}]}
```

| Campo | Regla |
|---|---|
| `range` | Enum + `last_hour`, `last_24_hours`. `compare: previous_period` funciona igual (ventana anterior del mismo largo). |
| valores `"$now-<dur>"` / `"$now"` en `where` | Solo con operadores `gt|gte|lt|lte` sobre columnas `timestamp*`/`date`; `<dur>` = `\d+(m|h|d)`, máx. `400d`. Validado al publicar (columna de tipo fecha, token bien formado). El servidor sustituye por `now()` de la base. |
| `link` | `{ "model": "<Model propio>", "filter": { "<col>": "<valor>" } }` o `{ "url": "/m/<tabla>?…" }` (ruta relativa interna, nunca absoluta). El SDK navega a la lista con `f_<col>=<valor>`. |

Archivos: `manifest/v3/types.go` (`DashboardWidget.Link`, `WidgetLink`), schema ×2 (`range` enum,
`link`), `manifest/v3/validate.go`/`ui_validate.go` (tokens y `link`; el validador legado
`manifest/validate.go` no valida `dashboard` [V]), `query/aggregate.go` (rangos nuevos y sustitución de `$now`), ops (`services` del catálogo de
widgets pasa `link` y resuelve `$now` a través del kernel), SDK (widget clicable).

## 4. Compatibilidad, pruebas y seguridad

- §1-§2: aditivos en el SDK; manifests viejos no cambian; un SDK viejo muestra la fecha absoluta y el
  avatar actual (degradación aceptable). Pruebas: snapshot de celda con y sin umbrales, `color_field`
  con contraste, sin imagen.
- §3: un hub viejo rechaza `link`/`last_24_hours` → bump previo. Pruebas: agregación con `$now-15m`
  (reloj fijo), `last_24_hours` en límites, token en columna no fecha → error de validación, `link` a
  modelo ajeno → error, `url` absoluta → error.
- Seguridad: `$now` lo resuelve la base (sin entrada del cliente); `link.url` solo rutas internas
  (evita *open redirect*); el KPI respeta el filtro por org y `permission` del widget como hoy.
