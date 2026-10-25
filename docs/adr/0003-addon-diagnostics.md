# ADR 0003: diagnóstico de addons como capacidad de plataforma, saneado en un solo punto

- Estado: aceptado. Fase F0 implementada en este cambio.
- Fecha: 2026-10-24
- Paquetes: `diag` (nuevo), `runtime/wasm`, `capability`, `migrations`. Versión: **minor, aditiva**.

## Contexto

Un cobro de Mercado Pago falló con HTTP 400 `property_value` y nadie pudo ver por qué sin entrar al servidor:

1. `http_request`/`doHTTP` (`runtime/wasm/capabilities.go`) devuelve al guest `{status, body}` y el host olvida el 400 del proveedor.
2. `log` del guest solo imprime texto suelto en el log del proceso.
3. Trap, timeout, pool ocupado y memoria (`runtime/wasm/wasm.go`) se vuelven un `error` de Go sin rastro.
4. La capa de capacidades (`capability/query.go`) solo conserva código y mensaje del proveedor.
5. Los hosts reducen el sobre de error a `CodedError` (código ≤64, mensaje ≤160) y lo pierden.

El problema no es de un addon: cualquier addon que falle tiene el mismo agujero.

## Decisión

El kernel captura **automáticamente**, sin que el addon haga nada y sin cambiar el contrato del manifest, un `diag.Diagnostic` saneado por cada invocación que termina en fallo, lo agrupa por huella y lo expone a un administrador por una API gobernada por permiso.

### Saneado: una sola función, allowlist por campo

`diag.Sanitize` es la única que decide qué se guarda. El Store **no tiene escritura pública**: solo el `Recorder` escribe y siempre pasa por `Sanitize`, así que ningún addon ni host puede saltársela. Al leer se vuelve a sanear (defensa en profundidad).

- Códigos `^[a-z][a-z0-9_]{0,47}$` (mayúsculas de origen se pasan a minúsculas, nada más); causas solo dígitos (≤6); rutas de campo con el regex `^[a-z][a-z0-9_]*(\[[0-9]{1,3}\])?(\.…){0,8}$`, ≤80 caracteres, ≤8, sin tramos de más de 5 dígitos.
- **Nunca se extraen valores.** Del cuerpo de error de un proveedor solo salen códigos (claves `code`, `error`, `type`, `reason`…), causas numéricas, rutas de campo y un `shape` (esqueleto `clave:tipo` de las claves de primer nivel, ≤12, snake_case ≤30). Las frases nunca se copian.
- Rutas desde texto libre sin lista de campos de ningún proveedor: se buscan tokens con forma de ruta y se aceptan si pasan el regex, no están pegados a `@ / \ % # & ~ + * -`, no terminan en un TLD o extensión de archivo, cada tramo tiene ≥2 caracteres y, si es un solo tramo, lleva `_` o índice (una palabra suelta es indistinguible de una frase). Las claves que por convención nombran un campo (`field`, `path`, `param`, `pointer`…) se aceptan como ruta aunque sea un solo tramo.
- `upstream_host` es solo el dominio (sin esquema, usuario, puerto, path ni query; las IP se descartan).
- **Borrado por valor** (`Scrubber`): el host conoce los secretos de la invocación (todos los ajustes, las credenciales de conector que el guest leyó con `connector_get` o que resolvieron los grants `connector:`, y los valores de las cabeceras de `http_request` salvo las inocuas) y elimina toda aparición literal, incluidas sus variantes url-escaped, base64 y la parte decodificada de un `Basic`. Para los breadcrumbs (texto libre del guest) además se borran URLs (queda el host), JWT, emails, credenciales en cabecera, PAN con Luhn, runs de ≥6 dígitos y tokens opacos, hasta un punto fijo.
- Garantías probadas: vectores dorados hostiles, idempotencia y fuzz nativo de Go (`FuzzSanitize`, `FuzzExtractBody`, `FuzzScrubText`), que corre en CI.

### Captura (runtime wasm)

Opt-in a nivel de host: `Host.WithDiagnostics(*diag.Recorder)`. Sin él, el comportamiento es idéntico al anterior. `KERNEL_DIAGNOSTICS=off` deja el Recorder inerte.

- `invokeImpl`: un defer registra el fallo de la invocación como `kind=runtime` (códigos `trap`, `timeout`, `pool_busy`, `memory_limit`, `canceled`, `addon_not_loaded`, `export_missing`, `module_closed`, `panic`, `runtime_error`). El código lo fija un `faultError` transparente (mismo mensaje, `Unwrap` intacto). Un panic se registra y se vuelve a lanzar.
- Si el guest devuelve el sobre `{success:false,error:{code,message}}`, `diag.FromEnvelope` lo lee (`kind=envelope`) y, **si hubo una llamada saliente fallida, se promueve a `kind=http`** con status, dominio, método y los hechos saneados del cuerpo. Esto es lo que hace visible el 400 del proveedor **sin cambiar el addon**.
- `doHTTP` guarda en una bolsa por invocación (`diag.Bag`) cada status ≥400, error de transporte (`upstream_timeout`, `upstream_unreachable`) y salida denegada por capacidad o guarda SSRF (`egress_denied`). La bolsa solo se vuelca si la invocación termina en fallo: un 404 que el guest manejó no deja nada.
- Breadcrumbs: las últimas 20 líneas de `log` del guest, saneadas, que solo se persisten si la invocación falla.
- `Indeterminate`: se envió una petición que muta (POST/PUT/PATCH/DELETE) y después hubo trap/timeout o error de transporte; el resultado en el proveedor es desconocido.
- `capability.Executor.OnFailure` (`diag.Recorder.OnCapabilityFailure`): timeout, panic, `invalid_output`, cancelación y el código que el proveedor reportó (aunque el contrato no lo declare: `Failure.Reported`).
- Webhooks y eventos entran por `Host.InvokeFor`, así que ya quedan capturados como invocaciones wasm. Los `kind` `webhook`, `event` y `native` existen para que un host registre otros caminos (dead letters, addons nativos) con `Recorder.Record`; no se cablean en F0.

### Escritura asíncrona, agregada y acotada

Fuera de la transacción del guest (si fuera dentro, el rollback de `InvokeForInTx` borraría el diagnóstico). `Recorder.Record` solo toma un mutex y actualiza un mapa: nunca bloquea ni falla. Cada ~5 s `Flush` escribe el agregado en una transacción propia.

- Huella: `hash(addon, versión mayor, operación, kind, code, http_status, provider_code ordenados, props ordenadas)`; el id del grupo es determinista (org + huella), lo que permite un upsert atómico `INSERT … ON CONFLICT … count = count + excluded.count` correcto con varias réplicas.
- Control de inundación: token bucket por org × addon (60/min). Sobre el tope solo sube el contador del grupo. Hasta 5 muestras por flush, máximo 20 por grupo más la primera. Tope de grupos pendientes en memoria (2000).
- Un grupo resuelto que reaparece se reabre; uno silenciado sigue silenciado.
- Retención: eventos 14 días (la primera muestra vive con su grupo), grupos 90 días; purga diaria en el bucle del Recorder.
- Si la escritura falla, el agregado vuelve a la cola (con tope) y el guest nunca se entera.

### Almacenamiento y aislamiento

Migración **0009** (`addon_diagnostic_groups`, `addon_diagnostic_events`), tablas aisladas, sin claves foráneas ni modelo declarativo (no las exponen `/options` ni el CRUD genérico). La RLS es decorativa en los hosts (ops nunca llama `SetRequestOrg`), así que **el filtro por org vive en la capa de app**: todo método del `Store` exige org y no existe ninguno sin ella. `subject_type/subject_id` y `diag_ref` (`dg_` + 12 hex, derivable con `diag.RefFor(org, invocationID)`) correlacionan con la entidad de negocio sin duplicar sus datos.

### API y permisos

`diag.Handler.Mount(router, mw...)` registra `GET /issues`, `GET /issues/:id`, `GET /events`, `POST /issues/:id/{resolve,mute}` y `GET /export` (support bundle JSON re-saneado, con versiones de addons y del kernel y **sin ajustes**). El acceso lo gobierna el permiso `platform.diagnostics.view` (`diag.PermView`), pero el kernel no registra permisos por el host: se inyecta como middleware (`permission.Gate(svc, lookup, permission.Cap("platform.diagnostics", "view"))`, o el `RequireAdmin` del host como respaldo). **`Mount` falla cerrado**: sin al menos un middleware devuelve error. El acceso entre organizaciones (`?org_id=`) solo si `HandlerOptions.IsSuperAdmin` lo permite.

### Alertas

Un grupo nuevo de severidad `error` o más, o ≥5 fallos en 10 minutos, llama a un `diag.Alerter` inyectable. `diag.IncidentsAlerter` abre `incidents.Open(domain="integrations", fingerprint="diag:<huella>")` (huella con contador, campana y evento del paquete `incidents`).

## Alternativas descartadas

- **Host function `diag_report`** para que el addon reporte: exige cambiar cada addon, y justo los addons que fallan son los que no lo harán. Descartada.
- **Guardar el cuerpo del proveedor y sanearlo al mostrarlo:** un solo olvido de saneado en lectura filtra datos para siempre. Se sanea al escribir, con allowlist, y no se guarda el cuerpo.
- **Lista de campos conocidos por proveedor:** acopla el kernel a proveedores. Se usa la forma de ruta del token.
- **Escribir dentro de la tx del guest:** el rollback borra el diagnóstico justo cuando hace falta.
- **Una escritura por fallo:** una caída del proveedor se convertiría en una caída de la base. Agregado + bucket.

## Qué necesita el host (ops)

1. `diag.Migrate(ctx, db)` o la migración goose 0009.
2. `rec := diag.NewRecorder(diag.NewStore(db), diag.Options{Alerter: diag.IncidentsAlerter{Store: incStore}, AddonVersion: …})`, `rec.Start(ctx)` y `defer rec.Close()`.
3. `wasmHost.WithDiagnostics(rec)` y `executor.OnFailure = rec.OnCapabilityFailure`.
4. Montar `diag.NewHandler(store, opts).Mount(group, authMW, gateMW)`.
5. **Correlación:** `actionInvocationCtx` en ops arranca de `context.Background()`; hasta que propague la solicitud, hay que envolver el contexto con `diag.WithCorrelation`, `diag.WithSubject` y (opcional) `diag.WithInvocationID` antes de `InvokeFor*`. Sin ello el `correlation_id` es el id de invocación.

## Fases

- **F0 (este cambio, aditivo y reversible):** `diag`, migración 0009, captura wasm y capability, API, alertas inyectables.
- **F1:** `error.details` en el sobre (dentro de `error`, nunca en el nivel superior: `envelopeKeys` es estricto), captura de dead letters y addons nativos, correlación desde dispatch.
- **F2 (ops/SDK):** montaje, permiso, pantalla de diagnóstico, enlace desde la entidad de negocio con `diag_ref`.

## Riesgos y decisiones abiertas

- La extracción de rutas es heurística: un token con forma de ruta (con `_` o con punto) que en realidad sea un valor (`john_doe`) pasa. Mitigaciones: regex estricto, tope de 8, sin runs de dígitos, borrado por valor de secretos conocidos. Un valor no secreto con forma de identificador es el residuo asumido.
- Los breadcrumbs son texto libre del guest; el saneado es por patrones y por valores conocidos, no una garantía semántica.
- Costo en el camino caliente: con el Recorder activo cada invocación asigna una `Bag` y cada `log`/`http_request` toma un mutex corto. Con `KERNEL_DIAGNOSTICS=off` o sin `WithDiagnostics` no hay costo.
- Con varias réplicas la ráfaga se cuenta por réplica; la incidencia se deduplica por huella.
