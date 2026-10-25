# Bus de eventos duradero (R1)

Garantías del bus que conecta a los addons: **at-least-once** hacia el suscriptor y
**una sola fila de libro por `delivery_id`** (idempotencia del lado del kernel). El
suscriptor sigue debiendo ser idempotente frente a efectos externos.

## Emitir al confirmar (`event_emit`)

El guest no publica al llamar: el evento va a un buffer de la invocación y se
confirma solo si la invocación termina bien (ver `docs/wasm-abi.md` § 12.4).
Éxito -> `kernel_event_outbox` -> bus -> libro de entregas. Trap, error del host o
`{"success":false}` -> se descarta. Si el proceso cae entre el outbox y la
publicación, el relay del outbox (`dynamic.Service.RelayOutboxOnce`, cada 30 s)
republica la fila con la **misma identidad** (id, agregado, secuencia, causa, actor,
sucursal, correlación), y el libro la colapsa en las entregas ya registradas.

## Libro de entregas = cola duradera (`dispatch_deliveries` / `event_deliveries`)

Cada fila es el trabajo: lleva `payload`, `next_attempt_at` y un lease
(`lease_until`, `lease_token`).

- **Reclamo**: un `UPDATE … WHERE id IN (SELECT … FOR UPDATE SKIP LOCKED)` (Postgres)
  toma las entregas vencidas y suma un intento; varias réplicas nunca toman la misma.
- **Lease**: un latido lo renueva mientras corre el handler. Si el proceso muere, el
  lease vence y otro proceso retoma la fila. Toda escritura de resultado está
  protegida por el token (un proceso que perdió el lease no pisa al nuevo dueño).
- **Reintentos**: un fallo escribe `next_attempt_at = ahora + backoff` y suelta el
  lease. No hay bucle en memoria: un temporizador solo adelanta el barrido; el barrido
  corre al arrancar y cada `WithPollInterval` (5 s).
- **Arranque**: lo que quedó `pending` se retoma solo. Una fila con todos los intentos
  gastados y lease vencido pasa a `dead` (un evento que mata el proceso no itera para
  siempre). Si la suscripción ya no existe (addon desinstalado/deshabilitado) la
  entrega pasa a `cancelled`.
- **Módulo no listo** (wasm aún cargando): se espera sin gastar intentos hasta
  `WithNotReadyWait`.
- Las filas escritas por un kernel anterior (sin `payload`) no se tocan.

### `retryable` en la respuesta del guest

| Respuesta | Efecto |
|---|---|
| `{"success":false,"retryable":false,…}` | `dead` de inmediato |
| `{"success":false,"retryable":true,"retry_after_ms":N}` | reintento a los N ms (tope 1 h) |
| `{"success":false}` | **sin cambio**: se reintenta hasta `MaxAttempts` |

### `Replay`

`dispatch.Replay(ctx, db, tabla, ReplayFilter{OrgID, AddonKey, Event, From, To})` vuelve
a encolar entregas terminales como la **generación siguiente** de su cadena: nuevo
`delivery_id = sha256(raíz, generación)`, así que no choca con el libro y repetirlo es
idempotente por generación. El suscriptor recibe el mismo payload con
`envelope.generation = N`. Un suscriptor **nuevo** que necesita historia usa los
`backfills` del manifest, no el replay.

## Envelope `metacore.event/1`

Bloque aditivo bajo la clave `envelope` de todo payload objeto JSON (lo estampa el
host; un valor escrito por el emisor se reemplaza). El payload anterior no cambia.

| Campo | Significado |
|---|---|
| `schema` | `metacore.event/1` |
| `id` | id de la publicación (uuidv7); estable ante republicación del relay; base del `delivery_id` |
| `name`, `version` | nombre del evento y versión mayor (hoy 1) |
| `source` | quien publicó, según el bus (addon o `kernel`) |
| `org_id`, `occurred_at` | tenant y cuándo ocurrió |
| `aggregate {type,id}` | entidad del evento: la que declara el guest, o `model`/`id` en un CRUD canónico |
| `sequence` | contador monótono por (org, agregado); ausente si no hay agregado |
| `correlation_id`, `causation_id` | petición de origen / evento cuya entrega produjo este |
| `actor_id`, `branch_id` | actor y sucursal |
| `generation` | 0 original, N en el N-ésimo `Replay` |

Golden: `dispatch/testdata/envelope_v1_*.golden.json` (`go test ./dispatch -run Golden -update`).
