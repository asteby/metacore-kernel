# Coherencia de cachés

Cómo el kernel evita que una caché derivada siga sirviendo el mundo de antes de
que un addon cambiara. Cubre cuatro fallos reales de producción (God's, OPS
0.1.17 sobre kernel v0.212.0) y la forma de diagnosticarlos.

| # | Síntoma en producción | Causa | Pieza que lo cura |
|---|---|---|---|
| 1 | `ERROR: cached plan must not change result type (SQLSTATE 0A000)` en cada consulta tras una migración; el POST de dirección del carrito daba 500 hasta reiniciar OPS | el pool de OPS abre GORM con `PrepareStmt: true`: las sentencias preparadas conservan la descripción vieja del resultado y nada las invalida | `database/staleplan` + hook `db.statements` |
| 2 | el navegador mostraba textos sin traducir tras instalar un addon con la pestaña abierta | `Cache-Control: private, max-age=60` en `/api/addons/i18n/:lang` | cabeceras de OPS + `X-Cache-Epoch` + evento `CACHE_EPOCH_CHANGED` |
| 3 | manifests, RBAC, capacidades, rutas, nav y metadatos desfasados hasta reiniciar | cada caché se invalidaba por su cuenta (o nunca) | `cachecoherence` (época + invalidadores ordenados) |
| 4 | `storefront_cart` 0.2.2: manifiesto nuevo con módulo wasm viejo, rutas 501 hasta reinstalar | `wasm.Host.Load` conserva el módulo si los bytes no cambian y solo cambia la spec | verificación de exports + recarga (`installer.verifyBackendExports`) |

## 1. Paquete `cachecoherence`

Una API pequeña:

```go
cachecoherence.Bump("addon.upgrade", cachecoherence.WithOrg(org), cachecoherence.WithAddon(key))
```

- **Época.** Contador monótono por ámbito: global, por organización, por addon.
  `CurrentEpoch(org, addon)` devuelve el vector; `Epoch.Token()` lo serializa
  (`"g.o.a"` en base 36) para un ETag, una cabecera `X-Cache-Epoch` o una clave
  de caché. `NewWatcher(org, addon)` empaqueta la comparación `Stale()/Refresh()`.
  - `Bump()` sin ámbito mueve la época global (y con ella todos los tokens).
  - `WithOrg` mueve la vista de esa organización; `WithAddon` la de ese addon en
    todas las organizaciones que lo observan.
  - La época global arranca en milisegundos desde el inicio del proceso: tras un
    reinicio nunca se repite un token que un cliente viejo pueda tomar por «sin
    cambios».
- **Invalidadores ordenados.** Cada capa se registra una vez
  (`Register(nombre, orden, fn)`); `Bump` los ejecuta de menor a mayor orden:

  | Orden | Constante | Qué cae ahí |
  |---|---|---|
  | 10 | `OrderDatabase` | handles de sentencias preparadas (`db.statements`) |
  | 20 | `OrderSchema` | introspección de esquema: columnas vivas, `deleted_at`/`organization_id` por tabla (`dynamic.schema`) |
  | 30 | `OrderManifest` | registros de manifest/instalaciones (host) |
  | 40 | `OrderRuntime` | módulos wasm / servicios nativos (host) |
  | 50 | `OrderPermission` | capacidades RBAC (`permission.capabilities#…`) |
  | 60 | `OrderCatalog` | catálogo de capacidades, i18n (host) |
  | 70 | `OrderRouting` | rutas de la pasarela (host) |
  | 80 | `OrderNavigation` | nav / sidebar (host) |
  | 90 | `OrderMetadata` | metadatos de modelos dinámicos y su versión (`metadata.tables#…`) |
  | 100 | `OrderPublish` | validadores HTTP, WS/SSE (host) |

  Un invalidador que falla o hace `panic` se registra (`Status`) y **no** detiene
  a los demás: un mundo medio vaciado es peor que uno vaciado con un error.
- **Publicación.** La época nueva se hace visible **después** de que corrieron
  todos los invalidadores. Quien ve el token nuevo y vuelve a pedir datos lee a
  través de cachés limpias.
- **Quién llama a `Bump`.** El instalador: `Install`, `Upgrade`, `Enable`,
  `Disable`, `Uninstall`, siempre ANTES de emitir `ADDON_MANIFEST_CHANGED`
  (`Installer.announce`), de modo que el navegador que reacciona al evento no
  reciba datos de la caché que se iba a invalidar. Si la instalación/upgrade
  falla se hace un `Bump` igualmente (las migraciones pudieron haber corrido).
  `Uninstall` con `dropSchema` hace un segundo `Bump` tras el `DROP SCHEMA`.
- **Hosts con varias réplicas.** El registro es por proceso. Reenvía el evento
  (`Subscribe`) por tu transporte y aplícalo en los pares con `ApplyRemote`.
  El kernel no trae transporte.
- **Registrar tu propia caché** (host): 

  ```go
  cachecoherence.Register("ops.nav", cachecoherence.OrderNavigation,
      func(ctx context.Context, ev cachecoherence.Event) error { navCache.Clear(); return nil })
  ```

  Registrar con el mismo nombre reemplaza la anterior (reconstrucciones y tests).

### Diagnóstico

`cachecoherence.SnapshotStatus()` devuelve, por invalidador, cuántas veces corrió,
la última secuencia que procesó, si está al día (`up_to_date`), el último error,
las épocas por organización/addon, los últimos 32 `Bump` y los contadores de
`staleplan`. `all_up_to_date=false` significa que alguna capa falló en el último
`Bump`: ahí está la caché sospechosa. OPS lo expone en `GET /api/system/cache-status`.

Eventos de log: `cachecoherence.bump` (info), `cachecoherence.invalidate_failed`
(warn), `staleplan.recovered` (warn), `staleplan.retry_failed` (error),
`staleplan.not_retried` (warn), `wasm.export_drift` /
`installer.backend_export_drift*`.

## 2. `database/staleplan`: SQLSTATE 0A000 y compañía

### Por qué fallaba para siempre

PostgreSQL guarda por conexión la descripción (tipos de parámetros y de las
columnas del RESULTADO) de cada sentencia preparada con nombre. Una migración que
cambia lo que produce `SELECT *` / `RETURNING *` hace que la siguiente ejecución
falle con `0A000`. La caché de sentencias de pgx se cura sola tras un fallo; los
`*sql.Stmt` explícitos que retiene GORM con `PrepareStmt: true` no, y pgx nombra
la sentencia con su propio SQL, así que re-preparar el mismo texto en una conexión
que aún conserva la vieja devuelve la descripción vieja. Reproducido (kernel,
Postgres 17 desechable, un conexión, `PrepareStmt: true`):

```
BEFORE attempt 0: ERROR: cached plan must not change result type (SQLSTATE 0A000)
BEFORE attempt 1: ERROR: cached plan must not change result type (SQLSTATE 0A000)
BEFORE attempt 2: ERROR: cached plan must not change result type (SQLSTATE 0A000)
BEFORE attempt 3: ERROR: cached plan must not change result type (SQLSTATE 0A000)
BEFORE attempt 4: ERROR: cached plan must not change result type (SQLSTATE 0A000)
```

(`TestBefore_PrepareStmtPoolFailsForever`; el mismo fallo por la ruta del host
wasm en `TestWasmDataHost_BeforeFixFailsWith0A000`, que golpea el probe
`SELECT * FROM <tabla> LIMIT 0` de `data_query`.)

### Qué hace ahora (tres capas, de preventiva a red de seguridad)

1. **Preventiva: `Bump` vacía los handles.** El invalidador `db.statements`
   (orden 10) cierra el caché de sentencias preparadas de GORM y adelanta la
   *generación* del pool. Normalmente el error ya no ocurre.
2. **Por conexión: `staleplan.Dialector(dsn)`** (opcional, recomendado) abre el
   pool con un `ResetSession` de database/sql: toda conexión cuya generación sea
   anterior ejecuta `DEALLOCATE ALL` antes de su próximo uso. Cubre las
   conexiones que estaban ocupadas durante el `Bump`. Sin `Dialector`, el reset
   vacía además las conexiones ociosas (`scrubIdle`).
3. **Red de seguridad: reintento transparente.** `staleplan.Install(db)` envuelve
   el `ConnPool` de GORM. Si una sentencia falla con `0A000 cached plan must not
   change result type`, o con `26000 prepared statement … does not exist`
   (tras `DISCARD ALL`/`DEALLOCATE` de un pooler), o `42P05 … already exists`, se
   vacían los handles y se reintenta **una** vez. Métrica Prometheus
   `metacore_staleplan_events_total{kind,outcome}`, `metacore_staleplan_resets_total`,
   logs `staleplan.*` y `staleplan.GetStats()`.
   - Los puntos de entrada de los guests wasm (`db_query`, `db_exec`,
     `data_query`, `data_mutate`, `data_batch`) y `installer.New` instalan la
     capa solos (`wasm.Host.WithDB`, `installer.New`); es idempotente y no hace
     nada fuera de Postgres.

### Garantía de «sin efectos duplicados»

Léelo antes de tocar esto. Un reintento nunca duplica un efecto porque:

- `0A000` se lanza al revalidar el plan, **antes** de que el ejecutor toque una
  fila; `26000`, en el `Bind`, antes de ejecutar. La sentencia fallida no tuvo
  efecto.
- Solo se reintenta (a) una sentencia en *autocommit* (atómica: si falló no dejó
  nada), o (b) una transacción cuyas sentencias previas fueron **solo de
  configuración** (`SET LOCAL …`, `set_config`): se hace `ROLLBACK`, se abre otra
  transacción, se repite esa configuración y se corre otra vez la sentencia. Es
  exactamente la forma del host wasm (`SET LOCAL search_path` y luego la
  consulta) y de la transacción por defecto de GORM alrededor de una escritura.
- Un fallo en una sentencia posterior a una que leyó o escribió datos **no** se
  reintenta en silencio (sus predecesoras se revirtieron con el aborto): sube al
  llamador (`staleplan.not_retried`). Para una unidad completa y repetible:
  `staleplan.RetryTx(db, fn)` repite `fn` entera una vez en una transacción
  nueva. `fn` solo puede tener efectos sobre el `tx` que recibe.
- `data_mutate`/`data_batch` (transacciones de varias sentencias propias del
  host) usan `staleplan.RetryEnvelope`: reejecuta la función completa una vez, lo
  cual es seguro porque cumplen el contrato (todo error hace `ROLLBACK`; los
  eventos se publican solo tras el `COMMIT`; los callbacks del embedder trabajan
  sobre el `tx` que reciben). No uses `RetryEnvelope` en una función que no lo
  cumpla.
- Máximo un reintento por sentencia (y uno por función de host), nunca si el
  `context` ya terminó, nunca para otros errores (`40001`, `40P01`, pérdida de
  conexión: ahí el resultado es incierto).

### Límites

- Una DDL que se confirma **entre** el `Describe` y el `Execute` de una consulta
  en vuelo puede agotar el único reintento; es la carrera de migrar con tráfico,
  no de caché. `Bump` al terminar la migración la cierra.
- `staleplan.Install` debe llamarse sobre el `*gorm.DB` raíz, justo tras
  `gorm.Open`: las sesiones derivadas antes conservan el pool sin envolver.
- Sin `Dialector`, las conexiones ocupadas durante un `Bump` solo se curan por el
  reintento.
- Solo Postgres (pgx). Otros dialectos: la capa no hace nada.
- **Los scripts de migración no pasan por el reintento.** `dynamic.execScript`
  (Apply, DryRunMigrations, ReplayAfterModels) desenvuelve en bucle todo
  `gorm.ConnPool` envuelto (caché de `PreparedStmt`, `staleplan.pool`/`txPool`,
  vía `dynamic.ConnPoolUnwrapper`) y ejecuta el archivo por el protocolo simple:
  un archivo con varios comandos o un `DO $$ … $$` no se puede preparar
  (SQLSTATE 42601). Por eso ese camino no tiene reintento por plan desfasado, y
  `txPool.UnwrapConnPool` marca la transacción como no reiniciable para que un
  0A000 posterior no la reinicie y pierda el script. Todo wrapper nuevo de
  `ConnPool` debe implementar `UnwrapConnPool()`.

## 3. Módulo wasm vs manifiesto

`wasm.Host.Load` conserva el módulo compilado si los bytes no cambian y solo
cambia la spec: un manifiesto que ganó `exports` (rutas/triggers nuevos) queda
delante de un módulo que no los exporta y las rutas responden 501. Ahora:

- `Host.MissingExports(addon)` / `Host.ExportDrift()` comparan los `exports`
  declarados con los que el módulo compilado realmente exporta; `Load` avisa
  (`wasm.export_drift`).
- Tras cargar en `Install`/`Upgrade`, `installer.verifyBackendExports` verifica;
  si falta algo, **recarga el módulo una vez** desde el bundle (`Host.Reload`:
  descarta lo compilado y los pools y recompila), vuelve a verificar y avisa. Si
  persiste, el bundle mismo no exporta lo que su manifiesto declara: se registra
  `installer.backend_export_drift_persists`, se cuenta
  (`wasm_export_drift_total{addon,outcome=reloaded|persisting}`) y se deja la
  decisión al operador, salvo `Installer.StrictBackendExports = true`, que hace
  fallar la instalación.
- Interfaces opcionales del loader: `BackendRuntimeVerifier`,
  `BackendRuntimeReloader`, `BackendDriftNotifier` (las implementa el adaptador
  wasm de `host`).

## 4. Hacia el navegador

- `ManifestChangeEvent.CacheEpoch` / payload `cacheEpoch` de
  `ADDON_MANIFEST_CHANGED`: el token ya posterior al vaciado.
- `bridge.WireCacheEpochBroadcast(hub, db, usersFn, nil)` publica
  `CACHE_EPOCH_CHANGED` (`{epoch, reason, seq, orgId?, addonKey?}`) a los usuarios
  de la organización (o a todos, en un `Bump` global).
- Cabecera `X-Cache-Epoch` en las respuestas: la pone el host (OPS) con
  `CurrentEpoch(org, addon).Token()`.

## 5. Cómo probarlo localmente

```sh
docker run -d -p 127.0.0.1:55447:5432 -e POSTGRES_PASSWORD=pw -e POSTGRES_DB=t pgvector/pgvector:pg17
export TEST_POSTGRES_DSN="host=127.0.0.1 port=55447 user=postgres password=pw dbname=t sslmode=disable"
GOWORK=off go test ./cachecoherence ./database/staleplan ./runtime/wasm ./installer -race
```

Las pruebas con Postgres se saltan solas sin `TEST_POSTGRES_DSN`.
