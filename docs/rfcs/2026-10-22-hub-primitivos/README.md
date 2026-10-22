# RFC: primitivos de kernel, OPS y SDK para el Hub sobre OPS

- **Estado:** propuesto (diseño, sin implementación).
- **Fecha:** 2026-10-22.
- **Origen:** ADR `asteby-hq/hub` `docs/ADR-HUB-SOBRE-OPS.md` (rama `docs/adr-hub-sobre-ops`), §9.2 «Faltó»;
  issue asteby-hq/ops#2054; spike `asteby-hq/addons` rama `spike/hub-fleet` (PR borrador #2185);
  requisitos de UX `asteby-hq/hub` rama `feat/panel-ux` `docs/HUB-OPS-UX-REQUISITOS.md`.
- **Base leída:** kernel `origin/main` `267fd6c` (v0.229.4); ops `origin/main` `4d19636a` (pin kernel
  v0.229.4); hub `origin/main` (pin kernel v0.225.0); addons `origin/main` `7af4d56c` (manifestcheck y
  addon-preflight en v0.222.0); SDK `origin/main` `4a2bcc47`.
- **Regla de diseño:** todo lo que aquí se propone es general (sirve a cualquier addon declarativo: un
  ERP, un panel interno, una integración). Nada es específico del Hub; el Hub es el primer consumidor.

Marcas: **[V]** verificado en código; **[S]** observado en el spike; **[E]** estimado; **[?]** por
confirmar al implementar.

## Documentos

| # | Documento | Qué resuelve | Hueco del spike |
|---|---|---|---|
| 01 | [RFC-01-accion-webhook-firmada.md](RFC-01-accion-webhook-firmada.md) | Acción `handler.type: webhook` que de verdad se despacha, firmada, con URL desde conector o setting, allowlist, reintentos y resultado real. Fase 0: **fail-closed** de todo disparador que hoy da falso éxito. | G1, G2 |
| 02 | [RFC-02-accion-set-fields.md](RFC-02-accion-set-fields.md) | Acción declarativa que cambia campos (`set_fields`), con precondición de estado, confirmación escrita, motivo, auditoría y evento. | G5 + requisito «confirmación fuerte» |
| 03 | [RFC-03-visibilidad-unlisted.md](RFC-03-visibilidad-unlisted.md) | `unlisted` y visibilidad por versión **en el hub**, sin tocar el manifest firmado. | G7 |
| 04 | [RFC-04-sumidero-ingest-upsert.md](RFC-04-sumidero-ingest-upsert.md) | Sumidero entrante firmado que hace upsert idempotente por llave natural con cursor (`Model.ingest` + `do: "upsert:<Model>"`). | G6 (+ G4) |
| 05 | [RFC-05-readonly-servidor.md](RFC-05-readonly-servidor.md) | `readonly` en servidor: **ya existe** como `protected`; se ajusta con `ingest.exclusive` y un lint. | G4 |
| 06 | [RFC-06-permisos-restrictivos.md](RFC-06-permisos-restrictivos.md) | Permisos por omisión: reinterpretación de G10 (el log es del addon como principal, no del RBAC de usuarios) y validación de claves de permiso. | G10 |
| 07 | [RFC-07-login-por-marca.md](RFC-07-login-por-marca.md) | Copy y hero del login por preset/marca; logo ancho. | G9 |
| 08 | [RFC-08-ui-declarativa-general.md](RFC-08-ui-declarativa-general.md) | Huecos generales de UI: fecha relativa, avatar de marca, KPIs (rango 24 h, `now` relativo, enlace a lista filtrada). Se descartan los que ya existen (filtros en URL, filtro por omisión, widgets). | requisitos UX |
| — | [PLAN.md](PLAN.md) | Prioridades, orden de bumps, olas y **Briefs de implementación** para agentes en paralelo. | — |

## Resumen de prioridad y tamaño

Tamaños: **S** ≤ 2 días, **M** ≤ 1 semana, **L** > 1 semana (un agente con revisión).

| Prioridad | Primitivo / capacidad | Repos | Tamaño | Bump de kernel |
|---|---|---|---|---|
| **P0 (M1)** | Fail-closed de disparadores sin despacho (RFC-01 §Fase 0) | ops | S | no |
| **P0 (M1)** | `addon_installations` en base limpia + índice parcial calificado (no es primitivo; bug de ops del spike, G3/G11) | ops | S | no |
| **P0 (M1)** | Lint de manifest para addons privados: claves de permiso que resuelven, `readonly` sin `protected` en réplica, webhook con URL literal `http` (RFC-05, RFC-06 §A) | addons / hub-addons `manifestcheck` | S | no |
| **P0 (M1)** | Fecha relativa y avatar de marca en la tabla (RFC-08 §1-2) | SDK | S | no |
| **P0 (M1)** | Login por marca (RFC-07) | kernel (contrato `Branding`) + ops + SDK | S-M | sí (en el contrato v0.230.0) |
| **P1 (salida de M1)** | Sumidero `upsert` con cursor + `ingest.exclusive` (RFC-04/05) | kernel + ops | M-L | sí |
| **P1 (salida de M1)** | KPIs: rango `last_24_hours`/`last_hour`, `now-<dur>` en `where`, `link` del widget (RFC-08 §3) | kernel + ops + SDK | S | sí |
| **P1 (M2)** | Acción webhook firmada (RFC-01 §Fases 1-2) | kernel + ops + hub (núcleo verifica) | M | sí |
| **P1 (M2)** | Acción `set_fields` + confirmación escrita (RFC-02) | kernel + ops + SDK | M | sí |
| **P1 (M2)** | Permisos del addon derivados de `capabilities` (RFC-06 §B), en modo auditoría | kernel + ops | S | sí (código, sin schema) |
| **P2 (M2)** | `unlisted` y visibilidad por versión (RFC-03) | hub | S | **no** |

## Orden de bumps (resumen; detalle en PLAN.md §2)

Clave del plan: **separar el contrato del runtime.** El hub, `manifestcheck` y `addon-preflight` solo
necesitan el *schema* (validan), no el runtime. Un único PR de contrato (todos los campos nuevos de
RFC-01/02/04/06/07/08, ambas copias del schema, `v3/validate.go`, `manifest/validate.go`, `from_v3.go`,
`modelbase`) sale primero como **v0.230.0**. Los PR de runtime (despachador webhook, `set_fields`,
`ingest`, rangos de agregación) son archivos distintos, van en paralelo y salen como **v0.230.x / v0.231.0**.

```
kernel v0.230.0 (contrato)
  ├─► addons: tools/manifestcheck + tools/addon-preflight (mismo PR; check-hub-kernel-compat.sh en verde)
  ├─► hub: backend/go.mod → desplegado   ← antes de publicar CUALQUIER addon que use un campo nuevo
  └─► (hub-addons/manifestcheck si D5 crea ese repo)
kernel v0.231.0 (runtime)
  └─► ops: backend/go.mod (+ wiring)  ← necesita también el SDK nuevo para el render
SDK (runtime-react / auth): release propio, en paralelo al kernel (sin dependencia de Go)
  └─► ops: frontend bump del SDK
addons (hub_fleet …): adoptan los campos solo cuando hub + ops desplegados
```

El orden pedido (kernel → manifestcheck + addon-preflight → ops → hub → SDK) se respeta, con dos
precisiones que vienen de incidentes reales (`backfills`, `connector_connected`, `provides_options`,
`Column.visibility`): el **hub debe estar desplegado** con el kernel nuevo **antes** de que un addon
publique el campo (falla tarde y en silencio), y el SDK se publica **antes** del bump de frontend de ops
que lo consume.

## Lo que se descartó tras leer el código

| Candidato | Veredicto | Evidencia |
|---|---|---|
| `readonly` en servidor como campo nuevo | Ya existe: `Column.protected` (kernel v0.136.0, #356) rechaza la escritura genérica sin excepción por rol. Se ajusta, no se crea. | `manifest/v3/types.go:1257`, `dynamic/validate.go:176` [V] |
| Allowlist de hosts para el webhook | Ya existe: capability `http:fetch` con host exacto, `*.dominio` o `connector:<c>.<cred>`, y `Capabilities.CanFetch` con guarda SSRF. Se reutiliza. | `security/context.go:41-60,95-110,112-140` [V] |
| Firma HMAC con nonce | Ya existe: `security.Signer.SignWithNonce` (ts + método + ruta + nonce + sha256 del cuerpo) y `NonceCache`. Se reutiliza. | `security/hmac.go:47`, `security/nonce.go` [V] |
| Precondición de estado | Ya existe: `requires_state` se hace cumplir en servidor (422 `invalid_state`). | ops `handlers/dynamic.go:3184` [V][S] |
| Motivo obligatorio y su auditoría | Ya existe: `Model.reason_required.actions` + `CanonicalEvent.Reason`. Se reutiliza en RFC-02. | `manifest/v3/types.go:806,1382` [V] |
| Filtros persistentes en la URL | Ya existe: `DynamicTable` con `enableUrlSync` lee/escribe `f_*`, `sortBy`, `page`. | SDK `runtime-react/src/dynamic-table.tsx:613-660` [V] |
| Filtro por omisión (`environment = production`) | Ya existe: `NavItem.filter` (y `locked_scope` si debe ser de seguridad). | `manifest/v3/types.go:2069` [V] |
| Widgets / KPIs del Resumen | Ya existen: `DashboardWidget` + `WidgetQuery` (count/sum/avg, `group_by`, `range`, `compare`) y ops los sirve. Solo faltan rango de 24 h, `now` relativo y enlace (RFC-08 §3). | `manifest/v3/types.go:1946-2035`, ops `handlers/dashboard_widgets.go` [V] |
| `unlisted` en el manifest | No hace falta tocar el contrato firmado: la visibilidad es estado mutable del catálogo, como los *grants*. Va solo al hub (RFC-03). | `manifest/v3/types.go:586-597` (el propio comentario), hub `store/models.go:420` [V] |
