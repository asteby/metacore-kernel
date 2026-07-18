# Roadmap: consolidación SchemaEngine — checklist

Doc vivo. Se va tachando. Complementa el RFC `2026-07-18-schema-engine.md`.

## Fase 0 — paridad de tipos/nullability (sin tocar aislamiento)
- [x] uuid opcional → puntero (kernel v0.75.1, ops #835)
- [x] nil-UUID tratado como vacío en coerce (kernel v0.75.2)
- [x] parametrizados numeric(p,s)/varchar(n) (ops #836, kernel ya tenía)
- [x] timestamp opcional → puntero (ops #836, kernel v0.77.0)
- [x] numeric opcional-sin-default → puntero (ops #843, kernel v0.77.0)
- [x] **DECISIÓN** bool opcional: se queda valor + default (NULL rompería filtros; opt-in por columna si algún día se necesita tri-estado)
- [ ] paridad restante: timestamptz vs TIMESTAMP (ops sin tz), escala numeric(18,4) vs NUMERIC, defaults implícitos bool/jsonb

## Fase 1 — fundación del engine + gate marketplace
- [x] `dynamic.ValidateColumnType` + allowlist única + `vector` (kernel v0.76.0 #193)
- [x] JSON Schema v3 acepta parametrizados vía anyOf (kernel v0.76.1 #194)
- [x] gate del hub valida columnas al publicar (hub #303)

## Fase 2 — delegación incremental ops→kernel
- [x] Show/Get de addons → `Service.Get` (ops #838)
- [x] Delete → `Service.Delete` con probe de 404 (ops #842)
- [x] search-unaccent (ops #842; sub-caso relación en legacy)
- [ ] delegar relaciones many2many (Update gate las excluye) — requiere B4
- [ ] modelos con relaciones gorm en Delete (legacy hoy) — requiere cascade en kernel

## Fase 3 — engine de schema UNIFICADO (el trabajo grande, por etapas)
- [ ] **F3.1** `SchemaEngine` facade en kernel: `ToReflectType`/`ToDDL` públicos (aditivo, envuelve lo existente)
- [ ] **F3.2** modo DDL opt-in "single-schema/public, sin RLS, con created_by_id" en el kernel (OFF por defecto; iguala lo que ops emite hoy)
- [ ] **F3.3** ops delega DDL al kernel en modo single-schema detrás de un feature flag (dual-run: compara salida vs su motor, sin escribir)
- [ ] **F3.4** switch de ops a `ToReflectType` del kernel (borra `BuildDynamicStructType`/`goTypeForColumn`)
- [ ] **F3.5** switch de ops a `ToDDL` del kernel (borra `CreateDynamicTable`/`sqlTypeFor`) — **requiere ventana de migración, NO automatizable**
- [ ] **F3.6** borrar el motor local de ops; una sola fuente de verdad

## Fase 4 — nullability explícita + contrato SDK
- [x] backend `FieldDef.Nullable` en metadata (kernel v0.77.1 #196)
- [x] SDK consume `field.nullable` con fallback (sdk #591)
- [ ] publicar runtime-react (version-packages PR #587) + bump ops
- [ ] contrato `Ref` nullable explícito en v3 (que el SDK deje de necesitar fallback)

## Reglas de ejecución
- Todo lo aditivo/opt-in → PR normal, review, merge, deploy.
- **F3.5 (cutover DDL en prod)**: NUNCA merge-and-deploy directo. Requiere ventana + backup + dual-run verificado. Es lo único big-bang.
