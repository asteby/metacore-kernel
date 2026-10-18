# dataload

Motor genérico de carga masiva: lee una fuente (CSV, JSON, JSON líneas o un
bundle) con un **perfil declarativo**, calcula un **plan** (dry-run y diff
contra el destino), lo **aplica** de forma idempotente y asíncrona dejando
historial, y **respalda los medios** como originales inmutables por sha256.

El paquete no conoce giros, plataformas de origen ni esquemas de base de datos.
Lo específico vive en datos (un `Profile` JSON, su documento `extract` y los
`Overrides` de cada organización) y la persistencia la cablea quien lo usa
(ops, un addon) implementando interfaces pequeñas.

## Flujo

```
Source ──► Profile (+Overrides, +Extract) ──► Plan ──► Runner ──► Target
                                               │           │
                                               │           ├─► Ledger (historial, llave de idempotencia, lease)
                                               │           ├─► ReviewStore (filas "revisar") ─► Decide/ApplyApproved (Authorizer)
                                               │           └─► MediaQueue ─► Backuper ─► BackupStore + Manifest (+ MediaSink)
                                               └─ create / update / unchanged / invalid / duplicate / review
```

- `BuildPlan` / `Runner.DryRun`: qué pasaría, sin escribir ni descargar.
- `Runner.Start`: aplica en segundo plano, por lotes con checkpoint; misma
  entrada + mismo perfil = *replay* sin escribir.
- `Runner.Recover`: retoma corridas huérfanas (lease vencido) sin duplicar.
- `MediaQueue.ProcessOnce` / `Run`: descarga, respalda y liga medios con reintentos.

## Qué implementa quien lo usa

| Interfaz | Obligatoria | Para qué |
| --- | --- | --- |
| `Target` | sí | `Lookup` y `Apply` (upsert idempotente y atómico por lote) |
| `MarkingTarget` | no | marca por lote en la misma transacción: conteo exacto al reanudar |
| `Ledger` | sí | historial de corridas |
| `LeaseLedger` | no | arrendamiento + latido + cerco: `Recover` y no duplicar entre procesos |
| `ReviewStore` | no | guardar filas en revisión para decidir después |
| `Authorizer` | sí para revisiones | `Decide` / `ApplyApproved` (acciones `ActionReviewDecide`, `ActionReviewApply`) |
| `MediaJobStore` | con medios | cola persistente de descargas |
| `LimitedJobStore` | no | tope global de medios por organización entre procesos |
| `BackupStore`, `Manifest` | con medios | originales por sha256 y su registro verificable |
| `PartialStore` | no | bytes de descargas cortadas entre reintentos (reanudación con Range) |
| `MediaSink`, `Deriver` | no | ligar medios al registro; generar derivados aparte |

Todas tienen versión en memoria (`NewMemory*`) para pruebas y como referencia
de la semántica.

## Perfil

Ejemplo genérico en [`examples/catalogo.profile.json`](examples/catalogo.profile.json)
con sus overrides en [`examples/catalogo.overrides.json`](examples/catalogo.overrides.json);
`Example` (en `example_test.go`) los carga y planifica en streaming.

- `fields`: `target`, `from` (alias de encabezado), `type`, `required`,
  `clean`/`finish`, `values`, `split`, `derive` (`first`, `extract`, `gtin`),
  `media`.
- `key`: identidad natural (upsert por ella). `applyOrder`: orden de escritura
  por rangos (p. ej. padres antes que variantes) sin cambiar el plan.
- `maxRows` (en memoria, 50 000 por omisión) y `maxStreamRows` (streaming,
  5 000 000).
- `Overrides` por organización: `rename`, `fields`, `drop`, `defaults`,
  `clean`, `cleanAdd`, `valuesAdd`, `ignoreHeaders`. Entran en la huella.
- `ParseExtract`: documento `metacore.dataload.extract/v1` con reglas RE2,
  vocabularios, comprobaciones y códigos de revisión.
- `Registry` + `ProfileStore`: versiones inmutables por organización, base
  compartida y derivados.

## Archivos grandes (streaming)

`NewCSVStreamSource` y `NewJSONLinesSource` reciben un `Opener` y se recorren
fila por fila (`StreamSource.Each`). Con ellas:

- `StreamPlan` hace dos pasadas: la primera da la huella, los duplicados y los
  valores de las comprobaciones `unique`; la segunda decide contra el destino
  por páginas (`StreamOptions.OnPage`). Acciones, diffs, motivos, `Counts`,
  `InputHash` y `PlanHash` son idénticos a `BuildPlan` (lo verifica
  `TestStream_PlanMatchesBuildPlanRowByRow`). El `Plan` devuelto no trae `Rows`.
- `Runner.Start`/`DryRun`/`Recover` toman ese camino solos: escriben página por
  página (`Runner.PageSize`, redondeado a `BatchSize`), guardan revisiones y
  encolan medios por página. Con `applyOrder` hay una pasada de escritura por
  rango. Si la fuente cambia entre pasadas, la corrida falla
  (`dataload.source.changed`).
- En memoria queda una entrada por identidad (y por valor `unique`), no las
  filas. Medido en esta rama: 300 000 filas (36 MiB) aplicadas con pico de
  heap ~59 MiB en streaming contra ~801 MiB en memoria, a cambio de unas 2×
  de tiempo (tres lecturas del archivo).

## Medios

- `MediaQueue.Limits`: `MaxConcurrent` (por proceso), `MaxConcurrentGlobal`
  (entre procesos, lo cuenta un `LimitedJobStore`; sin él vale por proceso) y
  `MaxAttempts` por organización; reparto por turnos entre organizaciones.
- Reintentos con espera creciente; los errores definitivos no se reintentan.
- `importer.FetchMedia` reanuda con `Range` + `If-Range` (solo validador
  fuerte) dentro de un intento; con `Backuper.Partials` también entre
  reintentos y procesos. Nunca se mezclan versiones.
- `RevalidatePolicy` / `MediaQueue.Revalidate`: ETag/Last-Modified con 304; si
  el contenido cambió se guarda otro original y el anterior queda en `Revisions`.

## Recuperación

Con un `LeaseLedger` cada corrida tiene arrendamiento con latido; `Recover`
toma las vencidas, verifica que perfil, extracción y entrada sean los mismos y
reanuda: lo ya escrito sale `unchanged`. Si además el destino es
`MarkingTarget`, las marcas posteriores al último checkpoint devuelven el
conteo exacto de `Created`/`Updated` aunque el proceso muriera entre escribir y
guardar el checkpoint (`Checkpoint.Recovered`).

## Mensajes

El motor no escribe textos fijos: cada error, problema de fila (`RowIssue.Code`)
y motivo por omisión (`ReviewItem.DetailCode`) lleva un código estable
(`code.go`, `importer.IssueCodes()`) y parámetros. Los textos están en
`locales/es.json` y `locales/en.json` (`Messages()`, componible con
`i18n.Compose`); `Localize`, `LocalizeIssue` y `ReviewItem.Localize` traducen
con el idioma del contexto. `errors.Is` compara por código. La corrida guarda
`ErrorCode`/`ErrorParams` y el trabajo de medios `LastErrorCode`.

## Pruebas

```
go test -race ./dataload ./importer ./netguard
```

Cada pieza tiene pruebas con nombre propio (`TestStream_*`, `TestMediaGlobal_*`,
`TestMediaPartial_*`, `TestMarks_*`, `TestAuth_*`, `TestMessages_*`, además de
las de plan, corrida, recuperación, revisión, registro y medios).

## Límites conocidos

- Streaming: el mapa de identidades crece con el archivo (una entrada por fila
  única); con decenas de millones de filas hace falta más memoria o partir el
  archivo.
- Los textos de los errores de red y del origen (`importer.FetchMedia`) no
  tienen código: se guardan tal cual en `LastError`.
- `AllowAnyActor` existe solo para pruebas y desarrollo.
