# RFC: SchemaEngine — motor de schema declarativo único

**Estado:** Draft · **Fecha:** 2026-07-18 · **Ámbito:** metacore-kernel, ops, hub, metacore-sdk

## Problema

Existen **dos** motores que proyectan `manifest.ModelDefinition → (struct Go, DDL SQL, coerción de input)`:

- **Kernel** (`dynamic/model.go`, `dynamic/schema.go`, `dynamic/coerce.go`) — la fuente de verdad.
- **Ops** (`models/dynamic_model.go`) — una reimplementación completa (`goTypeForColumn`, `buildColumnTag`, `BuildDynamicStructType`, `CreateDynamicTable`, `sqlTypeFor`).

Han derivado. El mismo bug se arregla dos veces (uuid nullable → `*uuid.UUID`, arreglado en ambos por separado; `numeric(p,s)`, `timestamp` opcional, etc.). Y hay **tres allowlists de tipos divergentes**, lo que rompe la promesa del marketplace *"un addon se comporta igual en cualquier host"*:

| Inconsistencia | Efecto |
|---|---|
| `type: vector` pasa el JSON Schema pero el kernel lo rechaza | addon publicable que no instala |
| ops acepta en silencio (`money`/`smallint`/`real` → `TEXT`) lo que el kernel rechaza | instala en ops, revienta en appliance |
| `float` = `numeric(18,4)` (kernel) vs `DOUBLE PRECISION` (ops) | redondeos distintos por host |
| ops inyecta `DEFAULT false`/`DEFAULT '{}'`; el kernel no | estado inicial de filas divergente |
| hub `publish.go` NO valida el plano de columnas | tipos inválidos entran al catálogo |

Bugs de nullability (el zero value se escribe en vez de `NULL`): `uuid`, `timestamp` (arreglados), `bool`, `numeric` (pendientes; cambian semántica de datos vivos).

## Objetivo

Un **único** paquete en el kernel (`metacore-kernel/schema`) que sea la sola fuente de verdad, consumido por todos los hosts y validado por el hub al publicar.

## Diseño

### Superficies del `SchemaEngine`

1. `ColumnType` canónico + `ValidateType(t string) error` — una allowlist única (fusión del enum del JSON Schema + los aliases reales que soporta el DDL). Decide `string` vs `text`, `float` vs `numeric`, e incluye `vector`.
2. `ToReflectType(ModelDefinition) (reflect.Type, error)` — reemplaza `dynamic.BuildStructType` **y** `ops.BuildDynamicStructType`.
3. `ToDDL(ModelDefinition) []Statement` — CREATE/ALTER único. Congela precisión numérica y defaults implícitos.
4. `Coerce(input, ColumnDef) (value, ok)` — ya existe (`coerce.go`); ops ya delega. Extender: el drop de "ref vacío" depende de `Ref != "" && !NotNull`, no del tipo Go.
5. Contrato explícito de nullability de `Ref` — `v3.Column` declara "ref no-`not_null` ⇒ nullable ⇒ engine emite `null`", expuesto en la proyección TS (`generated/manifest-v3.ts`) para que el SDK deje de reimplementar `normalize-submit.ts`.
6. Proyección form-schema para el SDK — descriptor derivado del engine (widget por tipo, required, ref-nullable, options-shape).

### Gate del marketplace

El hub (`internal/api/publish.go`) corre `v3.Validate` **+ `schema.ValidateType`** sobre TODAS las columnas antes de aceptar un addon. Es el candado: un addon publicable = instala idéntico en cualquier host.

## Blockers

- **B1 (crítico, prerequisito de la delegación DDL):** el kernel crea tablas schema-por-addon + RLS; ops crea en `public` con scoping por `WHERE organization_id`. Modelos de aislamiento incompatibles. Opciones: (a) modo "single-schema/public sin RLS" en el kernel, o (b) migrar ops a schema+RLS (invasivo, cambia el runtime de queries). Además el kernel no crea `created_by_id` ni `organization_id` incondicional que ops sí.
- **B2:** los modelos compilados nunca delegan (gate `IsDynamicModel`).
- **B3/B4:** decoración de relaciones/refs (`resolveRelations` etc.) y replace de asociaciones m2m viven solo en ops.

## Plan por fases (incremental, NO big-bang)

**Fase 0 — corrección de paridad (sin romper aislamiento):**
- ✅ uuid opcional → puntero (kernel v0.75.1, ops #835).
- ✅ nil-UUID como vacío en coerce (kernel v0.75.2).
- ✅ `numeric(p,s)`/`varchar(n)` + `timestamp` opcional puntero (ops #836).
- 🟡 `bool`→`*bool`, `numeric`→`*float64` (decidir contrato: `NULL` vs default).

**Fase 1 — fundación del engine (aditiva, no rompe nada):**
- `schema.ValidateType` + allowlist única en el kernel.
- Gate en hub `publish.go`.

**Fase 2 — delegación incremental de ops (sin tocar B1):**
- Show/Get de addons → `Service.Get` (read-only, decoración ya factorizada). **Primer PR.**
- Search unaccent (clause ya cableado, dormant).
- Delete (FileDeleter ya wired).

**Fase 3 — engine de schema unificado (requiere B1):**
- `ToReflectType`/`ToDDL` en el kernel; ops borra su motor y delega.
- Resolver B1 (modo single-schema en el kernel) primero.

**Fase 4 — contrato de nullability + proyección SDK:**
- `Ref` nullable explícito en v3; SDK consume el flag, deja de reimplementar normalización.
