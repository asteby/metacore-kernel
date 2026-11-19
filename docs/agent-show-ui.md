# `show_ui`: lo que el agente muestra, con un solo renderer

El agente no dibuja gráficas propias ni teclea cifras: compone un documento
**genui/v2** con los MISMOS bloques que usan las páginas y el Tablero (`chart`,
`metric`, `table`, `map`...). Los datos vienen de **fuentes** que el servidor
autoriza con la identidad de la conversación.

```
LLM -> show_ui(doc)
         1. ExpandRefs        (host: catalogo del org/usuario)     {type:"ref"} -> bloques
         2. genuiv2.Parse     (contrato + perfil del agente)       rechaza, nunca arregla
         3. inline declarado  (inline_sources)                     el modelo marca lo que tecleo
         4. AuthorizeSources  (host: org/usuario/scope)            cada q/widget
         5. RecordUI          (host: hash + fuentes + doc)         si falla, no se muestra
         6. *ShowUIResult     (clave "ui") -> embed type:"ui" version genui/v2
```

## Perfil del agente (`genuiv2.AgentProfile()`)

Un perfil **solo estrecha** el contrato: lo que pasa el perfil pasa el contrato
desnudo. `Options.Profile == nil` es el comportamiento de siempre.

| Regla | Valor por defecto |
|---|---|
| Bloques | stack, grid, tabs, section, text, markdown, alert, empty, card, list, links, timeline, diagram, kpi_card, stats, metric, table, chart, map, view, model_view, ref. Fuera: `ext`, `slot`, `settings`, form, wizard, confirm, media, `component` |
| Acciones | navigate (solo rutas internas), open, close, set, refresh, emit (solo ids de `AllowedEmits`, vacío por defecto). Sin `invoke`/`submit` |
| Fuentes | `q` con prefijo `t:`/`r:`, `widget`, `inline`. Sin `plan`. Hook `SourceAllow` del host |
| Cifras | `SourceOnlyBlocks` (chart, table, stats, map, metric, kpi_card): sin `rows`/`items`/`points`/`spark` ni `value` tecleado; la cifra viene de una fuente o de un binding `{"$ref":"data.x.campo"}` |
| Enlaces | externos apagados (`Profile.Options()`) |
| Límites | 64 KB, 24 bloques, profundidad 4, 4 fuentes, 12 acciones, 20 filas inline, 3 gráficas |

Códigos estables: `genuiv2.ProfileCodes()` (`genui.profile.block_not_allowed`,
`.action_not_allowed`, `.emit_not_allowed`, `.source_not_allowed`,
`.literal_data`, `.component_not_allowed`, `.limit.*`). No están en
`AllCodes()` porque no son del contrato. Un host cambia el perfil con
`AgentProfile()` + `Clone()` y `agent.WithUIProfile(p)`; si lo amplía, pásalo
también a `EmbedBuilder.SetUIOptions(p.Options())` para los `ui` v2 de otras
herramientas.

## Lo que implementa el host (`agent/ui_host.go`)

| Puerto | Qué hace |
|---|---|
| `SourceResolver.AuthorizeSources` | Para CADA fuente no inline: `<tabla>.index` del modelo del `q:`, visibilidad del widget, scope activo. `*SourceDeniedError` si no; no revelar si existe |
| `UICatalog.ListBlocks` / `ListWidgets` | Catálogo visible al usuario (alimenta `list_blocks` y `list_widgets`) |
| `UICatalog.ExpandRefs` | Expande `{type:"ref"}`; implementar con `manifestv3.ExpandDocBlockRefs(doc, catalogo)` sobre el catálogo YA filtrado por addons habilitados y permisos |
| `UIAuditor.RecordUI` | Persistir y auditar: `DocHash` (sha256), `Sources`, `Blocks`, `Doc`. También recibe los rechazos (best effort) |

La identidad (`UIIdentity`) sale de `ConversationContext` (org, usuario,
conversación) y de `Metadata["scope"]`; nunca de parámetros que escriba el modelo.

Fallo cerrado: sin `SourceResolver` no se acepta ninguna fuente; si `RecordUI`
falla en un documento aceptado, no se muestra.

## Errores que el modelo puede corregir

Un documento no aceptado devuelve `*agent.UIRejection`, serializado así para el
modelo:

```json
{"show_ui_rejected":{"code":"genui.profile.literal_data","path":"$.blocks[0].rows",
 "message":"...","hint":"no tecleas cifras: declara la fuente...","retryable":true,"max_retries":2}}
```

El host cuenta los reintentos (`errors.As(err, &rej)`; `max_retries` = 2). Al
agotarlos degrada a texto con el `fallback`. Códigos propios de la herramienta:
`ui.no_document`, `ui.refs_unavailable`, `ui.ref_unresolved`,
`ui.inline_undeclared`, `ui.sources_unavailable`, `ui.source_denied`,
`ui.audit_failed`.

## Embed y degradación

`ChatEmbed{Type:"ui", Version:"genui/v2", UIV2: doc, Fallback: "..."}`. El
cliente lee `ui_v2` cuando `version == "genui/v2"` y `ui` (v1) en otro caso.
`DowngradeEmbeds(embeds, caps)` adapta a las capacidades declaradas del
cliente con `genuiv2.Downgrade`: bloque sin soporte -> su `fallback`; cliente
solo v1 -> spec v1; sin salida posible -> `Type:"text"` con el `fallback`
(WhatsApp, voz, clientes viejos).

## Migración desde `show_chart`

`show_chart` queda **deprecado** pero igual. `NewShowChartTool().EmitUI()` lo
traduce (bar, line, area, pie, donut) a un documento v2 con fuente `inline`;
el embed pasa a `ui` v2. Los tipos sin equivalente (stacked, horizontal,
scatter, radar, composed) conservan el embed `chart`. Es una ayuda de
transición: un `inline` no se puede autorizar contra los permisos del usuario.

## Presentación de tablas: feed «En vivo» (`table`)

El bloque `table` admite, todo opcional y aditivo (una tabla sin estas
propiedades se ve igual que antes):

| Propiedad | Dónde | Para qué |
|---|---|---|
| `max_height` (`sm`\|`md`\|`lg`) | bloque | Alto máximo con scroll interno y cabecera fija; la medida en rem la define el host (SDK). Úsalo en widgets y feeds que no deben crecer con las filas |
| `density` (`compact`\|`comfortable`) | bloque | Espaciado de filas; `comfortable` por omisión |
| `variant` (`text`\|`badge`\|`icon_text`) | columna | Presenta el valor como texto (por omisión), insignia con tono o icono + texto |
| `tones` | columna | valor -> tono (`default`, `muted`, `info`, `success`, `warning`, `danger`; el mismo enum que `status_tones` del mapa) |
| `labels` | columna | valor -> texto literal o `{"$t": clave}` que el host traduce con `t` |
| `icons` | columna | valor -> nombre de icono del host (`^[A-Za-z][A-Za-z0-9]{0,63}$`, como `tabs.icon`) |

Reglas: `tones`, `labels` e `icons` solo con `variant` `badge` o `icon_text`
(`genui.invalid_block`); hasta 32 entradas por mapa y claves de 1 a 64
caracteres `[A-Za-z0-9_.:-]`; un valor sin entrada usa el tono `default`, el
valor crudo y ningún icono. `format: "relative_date"` ya es válido en el
contrato; quien lo aplica es el renderer del SDK. El vector de referencia es
`genui/v2/testdata/conformance/valid/table-live-feed-badge.json`.

```json
{
  "version": "genui/v2",
  "data": { "events": { "q": "t:hub.Event?limit=50" } },
  "blocks": [
    {
      "type": "table",
      "title": { "$t": "feed.title" },
      "source": "data.events",
      "max_height": "md",
      "density": "compact",
      "columns": [
        { "key": "created_at", "label": { "$t": "feed.at" }, "format": "relative_date" },
        { "key": "name", "label": { "$t": "feed.event" } },
        {
          "key": "result",
          "label": { "$t": "feed.result" },
          "variant": "badge",
          "tones": { "ok": "success", "pending": "warning", "error": "danger" },
          "labels": {
            "ok": { "$t": "feed.result.ok" },
            "pending": { "$t": "feed.result.pending" },
            "error": "Error"
          },
          "icons": { "ok": "CheckCircle", "pending": "Clock", "error": "XCircle" }
        }
      ]
    }
  ]
}
```

El perfil del agente (`AgentProfile`) no enumera propiedades por bloque: solo
bloques, verbos, fuentes y límites; las propiedades nuevas ya entran por el
esquema, cuyas descripciones son lo que lee el modelo.

## No cubierto aquí

Bloque `proposal` (F5), «fijar»/`user_blocks` (F6), la implementación de los
puertos en Ops (F3) y el renderer `ui` v2 en el chat (F1).
