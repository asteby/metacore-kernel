# Bus de eventos duradero (R1)

Garantías del bus que conecta a los addons: **at-least-once** hacia el suscriptor y
**una sola fila de libro por `delivery_id`** (idempotencia del lado del kernel). El
suscriptor sigue debiendo ser idempotente frente a efectos externos.

## Emitir al confirmar (`event_emit`)

El guest no publica al llamar: el evento va a un buffer de la invocación y se
confirma solo si la invocación termina bien (ver `docs/wasm-abi.md` § 12.4).
Trap, error del host o `{"success":false}` -> se descarta.

**Sin transacción del llamador** (`Invoke`, `InvokeFor`): éxito -> `kernel_event_outbox`
-> bus -> libro de entregas. Si el proceso cae entre el outbox y la publicación, el
relay del outbox (`dynamic.Service.RelayOutboxOnce`, cada 30 s) republica la fila con la
**misma identidad** (id, agregado, secuencia, causa, actor, sucursal, correlación), y el
libro la colapsa en las entregas ya registradas.

**Dentro de una transacción del llamador** (`InvokeInTx` / `InvokeForInTx`): las filas del
outbox y el avance de la secuencia se escriben **en esa transacción** y **no se publica
en línea**: el evento no existe hasta el `COMMIT`. Un `ROLLBACK` borra filas y secuencia
y no hay entrega. La publicación ocurre después del commit, por un gancho que el
llamador invoca:

```go
ctx, hook := wasm.WithCommitHook(ctx)
out, err := host.InvokeInTx(ctx, tx, ...)
if err == nil { err = tx.Commit().Error }
if err != nil { hook.Discard() } else { host.PublishCommitted(ctx, hook) }
```

Sin gancho cableado, publica solo el relay (con retraso de hasta ~40 s; no se pierde
mientras el outbox esté armado y haya durabilidad). **Sin durabilidad**
(`WithDurableEmits(false)` u outbox sin armar) e `InvokeInTx` sin gancho, los eventos de
la transacción se **descartan** (solo queda un log): en ese caso el gancho es
obligatorio. Con el gancho, el relay queda como respaldo si el proceso cae entre el commit y
`PublishCommitted`. Si el outbox aún no está armado (la migración falló), dentro de una
transacción se publica sin persistir tras el commit (no duradero) y se reintenta armar
con backoff (1 s a 1 min).

**Doble publicación:** si la transacción dura más de ~10 s, el relay y el gancho pueden
publicar la misma fila. El libro de entregas colapsa el duplicado, pero los taps y los
suscriptores planos ven el evento dos veces (at-least-once).

El host arma el outbox al cablearse (`WithBus` / `WithDB` / `WithEventPublisher`), no en
el primer `emit`.

### Quién barre el outbox (`Host.WithDurableEmits`)

Las emisiones duraderas solo se recuperan si algo barre `kernel_event_outbox`:
`dynamic.Service` lo hace (relay de 30 s) **salvo** con `DisableEventOutbox: true`. Un
host que desactiva el outbox del servicio, o que usa el host wasm sin `dynamic.Service`,
debe llamar `host.WithDurableEmits(false)` (publica directo al bus tras el éxito, sin
tabla ni recuperación) o llamar él mismo a `Service.RelayOutboxOnce`. Por defecto es
`true` si el host tiene DB.

### `data.subscribers` es una estimación

`event_emit` responde con el número de suscripciones de la organización que casan con el
evento (`events.Bus.EstimateSubscribers`; los taps `*` del dispatcher y de audit no
cuentan), o `-1` si no se puede saber (un router sin `Estimator`, o falló la consulta de
suscripciones). El reparto real ocurre después de la invocación.

## Un router durable no pierde eventos en silencio

El bus tolera que un handler plano falle (se registra y se sigue: un suscriptor roto
no bloquea a sus hermanos ni la publicación). El **dispatcher** es distinto: es el
router que escribe el libro de entregas, y si no escribe, el evento no quedó en
ninguna parte. Por eso se registra con `Bus.SubscribeDurableRouting` y su error **sí**
se propaga:

- `Publish` / `PublishWithCount` ejecutan a todos los suscriptores y devuelven un error
  agregado (`errors.Is(err, events.ErrDurableDelivery)`) si falló un router durable.
- `dispatch.handle` devuelve error si falla `SubscriptionsForOrg`; `enqueue` devuelve
  error si falla el INSERT del libro. Una suscripción que falla no impide insertar las
  demás.
- El outbox (`publishCanonical`, `OutboxPublisher`, relay) **no estampa `published_at`**
  cuando `Publish` devuelve error; el relay reintenta la fila con los mismos bytes.

### Qué se ejecuta una vez y qué puede repetirse

- **El dispatcher** deduplica: el libro tiene `delivery_id` estable, así que un reintento
  solo inserta lo que faltaba y cada suscripción de addon se entrega una vez.
- **Los suscriptores planos** (tap de auditoría `*`, puentes de reglas, bitácora,
  tiempo real, impresión, imágenes, render...) **no deduplican**, y por eso el reintento
  no los vuelve a llamar. Cuando el intento en línea falla solo en el libro, esos
  hermanos *ya corrieron* en ese mismo `Publish` (corren antes de devolver el error): el
  outbox marca la fila `fanned_out = true` y el relay la reintenta con
  `Bus.PublishDurableOnly`, que invoca **únicamente** a los routers durables. Los planos
  se ejecutan una vez.
- **Excepción, a propósito:** una fila *sin* marca (el proceso murió antes o durante el
  fan-out, o es anterior a la columna) recibe la publicación completa, planos incluidos:
  no se sabe si alguno corrió. Ahí sí puede haber una segunda ejecución de los planos
  (at-least-once); un suscriptor plano con efectos no idempotentes debe tolerarlo.
- Un `Publisher` que envuelva al bus y no exponga `PublishDurableOnly`
  (`dynamic.DurableOnlyPublisher`) recibe la publicación completa en el reintento (el
  comportamiento anterior).

### Reintentos, agotamiento y errores permanentes

- El relay espacia los reintentos con backoff exponencial (`next_attempt_at`: 30 s, 60 s,
  ... tope 30 min) en lugar de un ticker fijo.
- Un nombre de evento de más de `dynamic.MaxEventNameLen` (512) caracteres no cabe en el
  libro (`size:512`): se **rechaza antes de persistir** con un log de error
  `kernel.outbox.rejected` (y el dispatcher lo descarta sin error), en vez de gastar los
  25 intentos.
- Al agotar `outboxMaxAttempts` (25) la fila queda sin `published_at` (visible para
  repetirla a mano) y **en ese momento** se registra a nivel error
  `kernel.outbox.exhausted` (evento, org, fila, último error) y se llama a
  `dynamic.Config.OnOutboxExhausted`, donde el host abre el incidente (el paquete
  `incidents` requiere un `Store`, org y política de huella que `dynamic` no posee, así que
  el kernel emite log + hook).
- Quien publica sin outbox (p. ej. `data_mutate`) ve el error en su log; un `Publish`
  que antes devolvía siempre `nil` ahora puede devolver este error.

`event_emit` con un `*gorm.DB` que es un pool (acciones Native: `actx.DB`) no cuenta
como transacción: se persiste y publica en línea. Solo una transacción abierta difiere
la publicación al commit hook.

El índice `..._due` del libro se crea en Postgres con `CREATE INDEX CONCURRENTLY` (en
segundo plano desde `Wire`, con reintentos y backoff que se cancelan con el `Wire`) y se
recrea si quedó inválido. Crear/borrar el índice exige un candado de aviso de sesión
(`pg_try_advisory_lock`): un índice inválido porque *otra réplica lo está construyendo* no
se toca (esa réplica lo termina); uno huérfano de un constructor caído (candado libre) sí
se recrea. Un arranque nuevo no puede ver el candado de una versión anterior del kernel
(sin él, el comportamiento previo).

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
| `sequence` | contador monótono por (org, agregado) en CRUD canónico; en eventos del guest es por (org, emisor, agregado): dos addons con el mismo `SalesOrder/123` reciben ambos `seq=1`, así que los consumidores ordenan por (`source`, agregado). Ausente si no hay agregado |
| `correlation_id`, `causation_id` | petición de origen / evento cuya entrega produjo este |
| `actor_id`, `branch_id` | actor y sucursal |
| `generation` | 0 original, N en el N-ésimo `Replay` |

Golden: `dispatch/testdata/envelope_v1_*.golden.json` (`go test ./dispatch -run Golden -update`).
