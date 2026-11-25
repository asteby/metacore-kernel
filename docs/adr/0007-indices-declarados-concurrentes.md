# ADR 0007: índices declarados, construidos con CONCURRENTLY fuera de transacción

- Estado: aceptado. Implementado en este cambio; **apagado por defecto** (el host decide cuándo llamarlo).
- Fecha: 2026-11-25
- Paquetes: `manifest` (`FromV3`), `dynamic` (`EnsureDeclaredIndexes`, nuevo). Versión: **minor, aditiva**.

## Contexto

Un manifest declara índices en `models[].indices[]`. Hasta ahora:

- `manifest.FromV3` descartaba los **compuestos no únicos sin `where`**. En el corpus de addons son 205 de 445 índices declarados, y ninguno existe en la base.
- El plano DDL (`dynamic.indexStatementsWithPrefix`, que comparten `CreateTable` y `ToDDL`) los hosts lo corren en cada registro, arranque y actualización, con `CREATE INDEX` normal. Ese comando toma un bloqueo `SHARE` sobre la tabla: bloquea INSERT/UPDATE/DELETE durante toda la construcción, en todos los tenants a la vez, al publicar la versión.
- Las migraciones SQL de addon corren dentro de transacción (`ApplyWithOptions`), donde `CREATE INDEX CONCURRENTLY` falla con SQLSTATE 25001.

No había ningún camino para construir un índice declarado sin bloquear escrituras.

## Decisión

1. `FromV3` conserva los compuestos no únicos sin `where` en `ModelDefinition.Indices` (`Name`, `Columns`, `Method`; sin `Unique` ni `Where`). El plano DDL transaccional **sigue sin emitirlos** (salta toda entrada sin `Where`), así que `ToDDL` no cambia y ningún host empieza a correr DDL nuevo al actualizar el kernel. Pruebas: `TestFromV3CompositeNonUniqueIndexCarried`, `TestIndexStatementsSkipCompositeNonUnique`.
2. `dynamic.EnsureDeclaredIndexes(ctx, db, defs, DeclaredIndexOptions)` es el único que los construye. `dynamic.DeclaredIndexPlan(def)` (puro) dice cuáles son.

### Modelo de bloqueo

| Paso | Bloqueo en la tabla | ¿Bloquea escrituras? |
|---|---|---|
| `CREATE INDEX` normal (lo que **no** hacemos) | `SHARE` toda la construcción | **Sí** |
| `CREATE INDEX CONCURRENTLY` | `SHARE UPDATE EXCLUSIVE` toda la construcción; espera a las transacciones anteriores antes de cada fase | No. Bloquea otro DDL, `VACUUM` y otro `CONCURRENTLY` en la misma tabla |
| `DROP INDEX CONCURRENTLY` (INVALID) | `SHARE UPDATE EXCLUSIVE` | No |

Reglas del constructor:

- **Solo Postgres, nunca en transacción.** Si `db` es una transacción devuelve `ErrDeclaredIndexInTx` y no hace nada. **Nunca** cae en un `CREATE INDEX` normal (a diferencia de `MaterializeUniqueRules` y `dispatch.ensureDueIndex`, que sí lo hacen dentro de una transacción).
- **Uno a la vez, en una conexión dedicada** (`gorm.DB.Connection`), con `SET lock_timeout` (por omisión 5 s) y `SET statement_timeout` (por omisión 0, sin límite: así un `statement_timeout` corto del pool no mata una construcción que no bloquea). Se hace `RESET` de los dos antes de devolver la conexión al pool.
- **Candado consultivo por índice**: `pg_try_advisory_lock(hashtextextended('metacore.declared_index:<schema>.<nombre>', 0))` de sesión. Si lo tiene otra réplica, el índice queda `busy` y no se toca: así nadie confunde con un fallo el INVALID que el otro está construyendo y nadie se lo borra. Si el que construía muere, su conexión suelta el candado y la siguiente corrida sana el INVALID.
- **Esquema explícito** (`Options.Schema`): `public` en un host que sirve los modelos de addon desde `public` (ops), `SharedSchemaName(key)` (`addon_<key>`) en un host solo-kernel. La tabla se busca **solo** ahí (`pg_class`, `relkind` `r`/`p`) y debe tener todas las columnas; una tabla homónima en otro esquema nunca se toca. `Options.CoreTables` nombra tablas del esquema que no son del addon (tablas del núcleo del host con el mismo nombre, como `public.conversations` en ops): se saltan siempre.
- **Identificadores validados y citados** (`^[a-z_][a-z0-9_]{0,62}$`, sin expresiones ni SQL libre); método en una lista cerrada (`btree` por omisión, `hash`, `gin`, `gist`, `brin`). Un nombre ya usado por otra relación o por un índice de **otra** tabla es un fallo (con `IF NOT EXISTS` se ignoraría en silencio).
- **INVALID**: si el índice existe con `indisvalid = false`, `DROP INDEX CONCURRENTLY IF EXISTS` y se recrea (`rebuilt`). Si una construcción termina INVALID o falla (p. ej. `lock_timeout`), se borra el resto y se reporta `failed`.
- **Nunca falla al que llama.** Devuelve un `DeclaredIndexReport` con un resultado por índice (`planned|exists|built|rebuilt|busy|skipped|failed|canceled` y `reason`) y registra una línea `dynamic.declared_index.*`. Un contexto cancelado detiene la corrida antes del siguiente índice; si cortó una construcción, el INVALID que queda lo sana la siguiente corrida (no se intenta limpiar con un contexto ya muerto).
- `OnlyIndexes` (lista de permitidos, para encenderlos por nombre) y `DryRun` (lee el catálogo y devuelve las sentencias sin candados ni DDL).

## Por qué CONCURRENTLY y fuera de transacción

Es la única forma de Postgres de crear un índice sin detener las escrituras de una tabla viva. No puede ir en una transacción (ni en un bloque implícito de varias sentencias: se ejecuta una sentencia por llamada), y una construcción interrumpida deja un índice INVALID que `IF NOT EXISTS` saltaría para siempre, por eso se revisa `indisvalid` antes y después.

## Límites

- `lock_timeout` cubre también las esperas por transacciones viejas: con una transacción abierta más larga que el `lock_timeout`, la construcción falla y se reintenta en la siguiente corrida. Es a propósito (no retener `SHARE UPDATE EXCLUSIVE` indefinidamente); súbelo si hay transacciones largas habituales.
- Un índice INVALID sigue manteniéndose en cada escritura hasta que se borra.
- `CONCURRENTLY` lee la tabla dos veces: más I/O y más tiempo que el normal. Uno a la vez y por proceso; varias réplicas se reparten por el candado, no construyen en paralelo el mismo índice, pero sí índices distintos.
- Un índice válido con el nombre declarado se da por bueno; no se comparan sus columnas.
- No cubre las migraciones SQL de addon (siguen en transacción) ni los únicos/parciales (siguen en el plano DDL y en `MaterializeUniqueRules`).
- El kernel no sabe qué tablas son del núcleo del host: `CoreTables` lo pone el host.

## Rollback

- Detenerlo: el host deja de llamarlo (en ops, `METACORE_DECLARED_INDEXES=off`, el valor por omisión). Los índices ya construidos se quedan y son inofensivos.
- Quitar un índice construido: `DROP INDEX CONCURRENTLY IF EXISTS <schema>.<nombre>;` fuera de transacción, y quitar la declaración del manifest (si no, se vuelve a construir). Ningún camino del kernel borra un índice VÁLIDO.
- Revertir el código: `FromV3` vuelve a descartar esas entradas; nada físico que deshacer.

## Nota para ops

El primer release de ops que suba este kernel debe dejarlo **apagado por defecto** (`METACORE_DECLARED_INDEXES=off`) y encenderlo por instancia (`concurrent`), primero con `METACORE_DECLARED_INDEXES_ONLY` con pocos nombres, en segundo plano después de `registerWithHost`, nunca en el arranque ni en la transacción de instalación, y con `CoreTables` = las tablas de los modelos del núcleo. Parche propuesto: `ops/docs/DECLARED-INDEXES.md`.
