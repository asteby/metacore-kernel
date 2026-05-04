# Auditoría `manifest.ColumnDef` — gaps frente a la visión model-driven

- **Fecha:** 2026-05-04
- **Archivo auditado:** [`manifest/manifest.go`](../../manifest/manifest.go) (`type ColumnDef`)
- **Visión de referencia:** un único `Model` declarado en el manifest debe derivar (a) la migración DDL, (b) las rutas dinámicas y (c) los metadatos de UI (table + modal + filtros + búsqueda) sin que el addon escriba nada extra.
- **Alcance:** sólo `manifest.ColumnDef`. No se modifica código en esta tarea.

## Estado actual

`manifest.ColumnDef` (`manifest/manifest.go:279-290`) declara hoy:

```go
type ColumnDef struct {
    Name     string `json:"name"`
    Type     string `json:"type"` // string, uuid, int, bigint, decimal, bool, timestamp, jsonb, text
    Size     int    `json:"size,omitempty"`
    Required bool   `json:"required,omitempty"`
    Index    bool   `json:"index,omitempty"`
    Unique   bool   `json:"unique,omitempty"`
    Default  any    `json:"default,omitempty"`
    Ref      string `json:"ref,omitempty"` // foreign key target: "orders" or "addon_tickets.comments"
}
```

Esto cubre **sólo el plano DDL**. La capa de UI vive en paralelo en `modelbase.ColumnDef` y `modelbase.FieldDef` (ver [`modelbase/metadata.go`](../../modelbase/metadata.go)) y se inyecta a la fuerza vía `ModelDefinition.Table` / `Modal` que están tipados como `interface{}` (opacos al kernel). Resultado: el addon repite la lista de columnas hasta tres veces (migración, table metadata, modal metadata) y el kernel no puede validar coherencia entre ellas.

Existen además precedentes dentro del propio manifest que **ya** resolvieron piezas equivalentes:

- `ToolInputParam` (`manifest.go:205-216`) ya tiene `Validation`, `DefaultValue`, `FormatPattern`, `Normalize`, `Example`.
- `FieldDef` (`manifest.go:232-240`) ya tiene `Label`, `Options`, `Default`.
- `modelbase.ColumnDef` ya tiene `Sortable`, `Filterable`, `Hidden`, `Tooltip`, `RelationPath`, `SearchEndpoint`, `DisplayField`.

La meta de esta auditoría es alinear `manifest.ColumnDef` con esos precedentes para que UN solo `ColumnDef` baste para emitir DDL **y** metadatos **y** validaciones server-side.

## Tabla `presente | falta | propuesta`

Bloque ① — **explícitamente requeridos por la tarea**

| # | Capacidad | Presente hoy | Falta | Propuesta de campo |
|---|-----------|--------------|-------|--------------------|
| 1 | Visibilidad por contexto (list / detail / create / edit / api) | No | Sí | `Visibility ColumnVisibility` con sub-flags `InList`, `InDetail`, `InCreate`, `InEdit`, `InAPI` (todos default `true`). Reemplaza al `Hidden` binario de `modelbase.ColumnDef`. |
| 2 | Searchable explícito | Parcial — sólo `Index bool` (DDL btree) | Sí | `Searchable bool` separado de `Index`; cuando `true`, kernel arma el endpoint `GET /search/<model>` y agrega la columna a `TableMetadata.SearchColumns`. Opcionalmente `SearchWeight int` para FTS futuro. |
| 3 | Reglas de validación server + client | No (sólo `Required`) | Sí | `Validation *ValidationRules` con: `MinLength`, `MaxLength`, `Min`, `Max`, `Pattern` (regex), `Enum []any`, `Format` (`email`, `url`, `phone`, `uuid`, `slug`), `Custom string` (CEL/expr). Reusa naming de `ToolInputParam.Validation` para coherencia. |
| 4 | Widget UI | No | Sí | `Widget string` (enum: `text`, `textarea`, `select`, `multi_select`, `search`, `number`, `date`, `datetime`, `email`, `url`, `boolean`, `image`, `file`, `richtext`, `json`, `relation`, `password`, `slider`, `rating`). Con `WidgetConfig map[string]any` para parámetros del widget (por ej. `rows` en textarea). Si está vacío, el kernel infiere widget desde `Type`. |
| 5 | Relación `OneToMany` | No (sólo `Ref` 1-N inverso implícito) | Sí | `Relation *RelationDef` con `Kind: "one_to_many"`, `Target string` (model destino), `ForeignKey string` (col en target), `OnDelete string` (`cascade` / `restrict` / `set_null`), `Eager bool`. Es **virtual**: no genera columna física, sí endpoint `GET /<model>/:id/<rel>`. |
| 6 | Relación `ManyToMany` | No | Sí | `Relation.Kind: "many_to_many"` + `JoinTable string` (`addon_<key>.<a>_<b>`), `JoinSourceKey`, `JoinTargetKey`. Kernel materializa la tabla pivot y expone `POST/DELETE /<model>/:id/<rel>/:targetId`. |

Bloque ② — **derivados / detectados durante la auditoría**

| # | Capacidad | Presente hoy | Falta | Propuesta de campo |
|---|-----------|--------------|-------|--------------------|
| 7 | Etiqueta i18n y descripción | No | Sí | `Label string`, `Description string`, `Placeholder string`, `Tooltip string`. Resolución por `Manifest.I18n[locale]` con clave `column.<model>.<col>.label`. |
| 8 | Opciones para enum/select | No (`Type:"string"` + `Default`) | Sí | `Options []Option` (reusa `manifest.Option`) o referencia `OptionsSource string` (endpoint relativo) cuando son dinámicas. Hoy se duplica en el `modal` opaco. |
| 9 | Sortable explícito | No | Sí | `Sortable bool`. Hoy se infiere por `Index` lo cual mezcla intención DDL con intención UI. |
| 10 | Filterable explícito + tipo de filtro | No | Sí | `Filterable bool` + `FilterType string` (`exact`, `range`, `contains`, `in`). Kernel registra el `f_<col>=` correspondiente. |
| 11 | Computed / generated columns | No | Sí | `Computed *ComputedSpec` con `Expression string` (SQL `GENERATED ALWAYS AS`) o `Resolver string` (símbolo WASM exportado). Read-only en API y UI. |
| 12 | Default function vs literal | Parcial — `Default any` literal sólo | Sí | `DefaultExpr string` (`now()`, `gen_random_uuid()`, `current_setting('app.current_org')`). Hoy se cuela mezclando comillas (`"'open'"`) — frágil. |
| 13 | Soft cascade & FK behaviour | Parcial — `Ref` sin política | Sí | `OnDelete string`, `OnUpdate string` para el FK del campo `Ref`. Defaults `restrict`. |
| 14 | PII / encriptación / secret | No | Sí | `Sensitive bool` (oculta en logs / audit), `Encrypted bool` (cifrado at-rest con KMS). Necesario para compliance que motivó el rewrite de auth. |
| 15 | Audit trail por columna | No | Sí | `Audited bool` — cuando `true`, las mutaciones se loggean en `metacore_audit`. |
| 16 | Permisos a nivel campo | No | Sí | `ReadCapability string`, `WriteCapability string` (claves de `Capability.Kind`). Permite columnas read-only por rol sin código. |
| 17 | Orden / agrupación visual | No | Sí | `Order int`, `Group string` (sección del modal: "general", "advanced"). Estabiliza el render sin requerir `modal.fields[]` aparte. |
| 18 | Format de display | No | Sí | `Format string` (ej. `currency:USD`, `decimal:2`, `date:YYYY-MM-DD`). Frontend lo aplica sin custom code. |
| 19 | Inmutabilidad post-create | No | Sí | `Immutable bool` — kernel rechaza `PUT` que toque la columna. Útil para `external_id`, claves naturales. |
| 20 | Help link / docs | No | Nice-to-have | `HelpURL string` para `?` icon. |
| 21 | Polymorphic / discriminator | No | Nice-to-have | `Polymorphic *PolyDef` (`TypeColumn`, `IDColumn`). No bloquea, marcar para v2. |

## Recomendación de roadmap

1. **v1 (mínimo viable, no breaking):** agregar `Label`, `Description`, `Visibility`, `Searchable`, `Sortable`, `Filterable`, `Widget`, `WidgetConfig`, `Validation`, `Options`, `OnDelete`, `Immutable`. Todos opcionales → addons existentes siguen funcionando.
2. **v1.1:** `Relation` (one-to-many, many-to-many) + materialización de tabla pivot en el installer. Implica cambios en `dynamic.CreateTable` / `dynamic.SyncSchema`.
3. **v1.2:** `Sensitive`, `Encrypted`, `Audited`, `ReadCapability`, `WriteCapability` — depende de capability service y del audit log, ambos ya existen.
4. **v2 (breaking, MAJOR bump APIVersion 3.0.0):** deprecar `ModelDefinition.Table` / `Modal` opacos; el kernel deriva `TableMetadata` y `ModalMetadata` a partir de `[]ColumnDef`. Apps mantienen escape-hatch sólo para overlays per-org.

## Impactos en consumers

- **TS SDK:** `@asteby/metacore-sdk` debe regenerar tipos (script `npm run gen:manifest`). Campos opcionales no rompen.
- **`modelbase.ColumnDef` / `FieldDef`:** quedan como representación interna del kernel para responder `/metadata/...`; eventualmente se convierten en proyección derivada del nuevo `manifest.ColumnDef` (no se borran en v1).
- **Validación (`manifest/validate.go`):** sumar reglas — `Validation` y `Type` deben ser consistentes; `Relation.Kind="many_to_many"` requiere `JoinTable`; `Widget` debe estar en el enum permitido para el `Type` declarado.
- **`dynamic.BuildStructType`:** los campos virtuales (`Relation`, `Computed`) NO deben aparecer como columnas físicas en el struct GORM.
- **Renovate / Changesets:** los pasos 1-3 son `minor`; el paso 4 es `major` y dispara nota de migración para todas las apps.

## Próximos pasos sugeridos (no en esta tarea)

- RFC corto en `docs/rfcs/` con ejemplo completo `tickets` reescrito usando el nuevo `ColumnDef` para validar ergonomía.
- PoC del paso 1 en una rama `feature/columndef-v1` con tests en `manifest/validate_test.go` cubriendo cada campo nuevo.
- Coordinar con `link` para asegurar que el bug `col.name` vs `col.key` (ver memoria SDK) no se replique con la nueva forma — `Name` sigue siendo el identificador canónico.
