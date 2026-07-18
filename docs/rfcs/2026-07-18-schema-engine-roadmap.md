# Roadmap: consolidación SchemaEngine — checklist

Doc vivo. Se va tachando. Complementa el RFC `2026-07-18-schema-engine.md`.

## Fase 0 — paridad de tipos/nullability (sin tocar aislamiento)
- [x] uuid opcional → puntero (kernel v0.75.1, ops #835)
- [x] nil-UUID tratado como vacío en coerce (kernel v0.75.2)
- [x] parametrizados numeric(p,s)/varchar(n) (ops #836, kernel ya tenía)
- [x] timestamp opcional → puntero (ops #836, kernel v0.77.0)
- [x] numeric opcional-sin-default → puntero (ops #843, kernel v0.77.0)
- [x] **DECISIÓN** bool opcional: se queda valor + default (NULL rompería filtros; opt-in por columna si algún día se necesita tri-estado)
- [x] paridad timestamptz + escala numeric(18,4) (ops #844; safe — Sync no hace ALTER de tipo sobre existentes)
- [ ] paridad restante menor: float/double → numeric(18,4) (ops sigue DOUBLE PRECISION), defaults implícitos bool/jsonb

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
- [x] **F3.1** `SchemaEngine` facade en kernel: `ToReflectType`/`ToDDL`/`ValidateType` públicos (kernel v0.78.1 #198)
- [x] **F3.2** modo DDL opt-in `SingleSchemaDDLOptions` (public, sin RLS, created_by_id, sin tz) — OFF por defecto (kernel v0.78.1 #198)
- [x] **F3.3** dual-run: ops compara su DDL vs el del kernel (`ToDDL` single-schema), flag `SCHEMA_ENGINE_DUALRUN`, OFF por defecto, sin escribir (ops #847)
  - **Divergencias reveladas (a resolver antes del cutover F3.5):**
    1. `float`/`double`: ops `DOUBLE PRECISION` vs kernel `numeric(18,4)`
    2. `bool` sin default: ops `BOOLEAN DEFAULT false` (implícito) vs kernel `boolean` pelón
    3. `jsonb` sin default: ops `JSONB DEFAULT '{}'` vs kernel `jsonb` pelón
    4. default string bareword (`default:"draft"`): ops lo cita a `'draft'`; kernel `manifest.DefaultLiteral` lo rechaza → NO emite DEFAULT
    5. índice único: ops `idx_<t>_<col>` vs kernel `uidx_<t>_<col>`
  - Todo lo demás COINCIDE bajo el preset (id/org/base, varchar(n), numeric(18,4), timestamptz, defaults citados).
- [x] **5 divergencias CERRADAS** (kernel v0.78.2 #201, opciones ops-compat en `SingleSchemaDDLOptions`) → **dual-run da MATCH byte-idéntico** (ops #848, test `MatchesOpsCompatPreset`)
- [ ] **F3.4** switch de ops a `ToReflectType` del kernel (borra `BuildDynamicStructType`/`goTypeForColumn`) — struct-type only, no toca DB; verificar con el dual-run de reflect
- [ ] **F3.5** switch de ops a `ToDDL` del kernel (borra `CreateDynamicTable`/`sqlTypeFor`) — de-riskeado (dual-run MATCH ✅); queda SOLO: **(a) ventana de migración + backup, (b) ejecutar el cutover con un humano mirando. NO automatizable por diseño.**
- [ ] **F3.6** borrar el motor local de ops; una sola fuente de verdad

## Fase 4 — nullability explícita + contrato SDK
- [x] backend `FieldDef.Nullable` en metadata (kernel v0.77.1 #196)
- [x] SDK consume `field.nullable` con fallback (sdk #591)
- [ ] publicar runtime-react (version-packages PR #587) + bump ops
- [ ] contrato `Ref` nullable explícito en v3 (que el SDK deje de necesitar fallback)

## Reglas de ejecución
- Todo lo aditivo/opt-in → PR normal, review, merge, deploy.
- **F3.5 (cutover DDL en prod)**: NUNCA merge-and-deploy directo. Requiere ventana + backup + dual-run verificado. Es lo único big-bang.
