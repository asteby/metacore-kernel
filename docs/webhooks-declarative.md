# Webhooks declarativos (emisión de eventos, RFC 0003 F2+F3)

Una suscripción de emisión es **un dato**, no un addon: dice qué eventos del bus salen, a qué destino,
con qué filtro, qué cuerpo, con qué firma y con qué llave de idempotencia. Vive en el paquete
`webhooks` (`Webhook.Declarative`) y reutiliza su entrega, reintento, bitácora y repetición.
Un `Webhook` sin `Declarative` se comporta exactamente como antes.

```
evento del bus ──Bridge──▶ filtro ──▶ transformación ──▶ fila de entrega (retenida o en cola)
                                                              │ worker
                                    firma (perfil) + llave de idempotencia ──▶ destino HTTPS
```

## Montaje

```go
svc := webhooks.New(webhooks.Config{
    DB:      db,
    Schemas: schemas,   // rutas de payload por evento (validación al guardar)
    Secrets: resolver,  // ResolveSecret(ctx, ownerType, ownerID, ref): por propietario
    OnAutoDisable: func(ctx context.Context, w webhooks.Webhook) { /* alerta */ },
})
svc.Start(ctx)
_ = webhooks.NewBridge(svc, webhooks.BridgeOptions{}).Attach(bus) // bus *events.Bus
```

`Bridge.Attach` registra un router **durable** en el bus: si no se puede registrar la entrega (base
caída) el bus devuelve `ErrDurableDelivery` y el outbox reintenta; el reintento es seguro porque un
evento republicado trae el mismo `event.id` y se deduplica por suscripción. El puente alimenta **solo**
suscripciones declarativas del organization del evento (`OwnerType` `organization` por omisión).
`Service.Trigger` sigue alimentando webhooks clásicos (y también declarativos).

Requiere la migración `0012_webhooks_declarative` (aditiva: columnas nulas/vacías/falsas).

## Modelo

```json
{
  "key": "v2-clientes",
  "filter": { "all": [ { "path": "payload.branch_id", "in": ["br-1", "br-2"] } ] },
  "transform": {
    "body": { "external_reference": "{{event.aggregate_id}}", "name": "{{payload.name | trim}}",
              "channel": "{{payload.channel | map:channels}}", "origin": "ops" },
    "tables": { "channels": { "web": "b2c", "pos": "retail", "*": "other" } }
  },
  "signing": { "profile": "bearer+hmac_canonical_v1", "secret_ref": "v2.signing_secret", "key_ref": "v2.api_key" },
  "headers": { "X-7L-Contract": "1.0" },
  "idempotency": "{{event.id}}",
  "ordering": "per_aggregate",
  "auto_disable_threshold": 10,
  "enabled": false
}
```

junto con los campos de `Webhook`: `url`, `events` (`["customers.*"]`), `retry_max` (≤ 50),
`timeout_sec` (≤ 120).

### Rutas

`event.*` (lista cerrada): `id, name, occurred_at, org_id, aggregate_type, aggregate_id, sequence,
source, branch_id, correlation_id, actor_id` (se toman del bloque `envelope` del bus,
`docs/events-bus.md`). `payload.a.b.0.c` navega objetos y arreglos del payload (sin `envelope`).
Sin `envelope.id`, el id del evento se deriva de (org, evento, bytes del payload).

### Filtro

`all` (todas) y/o `any` (al menos una) de condiciones con **un** operador: `eq`, `ne`, `in`, `exists`.
Los tipos son estrictos (`31` ≠ `"31"`); `exists` es falso para ausente o `null`.

### Transformación

Sustitución `{{ruta}}` con funciones puras encadenables: `lower`, `upper`, `trim`, `default:texto`,
`map:tabla`. Una expresión sola conserva el tipo (número, objeto); con texto alrededor da cadena.
Ruta ausente → `null` (o `""` dentro de texto). `map` sin entrada y sin `"*"` es un error de render:
la entrega queda **muerta y visible** con el motivo (no se pierde ni se envía mal). Sin
`transform`, el cuerpo es `{"event": {...sobre...}, "payload": {...}}`.

### Validación AL GUARDAR (`Create` / `Update`)

Falla con `ErrUnknownPath` (ruta inexistente en el esquema del evento, o campo `event.*` que no existe),
`ErrNoSchema` (rutas `payload.*` sin `Config.Schemas`, evento sin esquema, patrón que no casa con
ningún evento), `ErrInvalidDeclarative` (función/tabla/operador/cabecera/perfil inválidos),
`ErrBlockedTarget` (destino), `ErrNoSecretResolver` (firma sin `Config.Secrets`). Con un patrón
(`customers.*`) la ruta debe existir en **todos** los eventos que casan. Esquemas:

```go
schemas := webhooks.NewSchemas().Register("customers.customer.created", "id", "name", "email", "address.city", "items.*.sku")
```

### Firma (`webhooks/signing`)

| Perfil | Qué envía |
|---|---|
| `hmac_simple` | `hex(HMAC-SHA256(secret, body))` en `header` (omisión `X-Signature`, `prefix` opcional) |
| `hmac_canonical_v1` | `X-7L-Timestamp`, `X-7L-Nonce`, `X-7L-Signature = hex(HMAC(secret, "{ts}.{nonce}.{METHOD}.{path}.{sha256hex(body)}"))` |
| `bearer` | `Authorization: Bearer <token>` |
| `bearer+hmac_canonical_v1` | ambos (lo que exige V2) |

Es la construcción de `ChannelSignature.php` de 7 Leguas V2 (ventana ±300 s, nonce de 16–64
`[A-Za-z0-9_-]` no repetible); el vector del contrato está fijado en `signing_test.go`. El nonce y el
timestamp se regeneran en **cada intento**; la llave de idempotencia, no. Sin `signing`, se usa la
firma histórica `X-Metacore-*` con `Webhook.Secret`.

**Secretos por referencia:** `secret_ref` / `key_ref` nombran el secreto (`<conector>.<clave>`); el host
los resuelve con `Config.Secrets` en cada intento (rotación sin tocar la suscripción). Nunca se guardan
ni se registran; los mensajes de error solo nombran la referencia. Una cabecera constante con nombre de
secreto (`*token*`, `*key*`, `authorization`…) se rechaza al guardar.

### Idempotencia

`idempotency` (plantilla, omisión `{{event.id}}`) se guarda en la entrega y viaja en
`X-Idempotency-Key` (configurable con `idempotency_header`) en **todos** los intentos, también en
`Replay`/`Requeue`. El cuerpo es idéntico byte a byte entre intentos y `X-Metacore-Body-SHA256`
(`signing.Fingerprint`) lo huella: el receptor distingue "misma llave, mismo cuerpo" (`replayed`) de
"misma llave, otro cuerpo" (`idempotency_conflict`).

### Orden

`ordering: per_aggregate`: las entregas con el mismo `aggregate_type:aggregate_id` salen en secuencia;
una espera a que la anterior se entregue. Una anterior **agotada** (dead-letter) deja de bloquear.
`none` (omisión): sin espera. Un evento sin agregado no se ordena. La secuencia es por proceso
(nanosegundos monótonos).

### Apagado por omisión y retención

`enabled: false` → cada evento que llega crea una entrega **retenida** (`Held`), no se pierde. `SetEnabled(true)`
(o `Update` con `enabled: true`; HTTP `POST /:id/enable`) las libera en orden. Apagar con entregas en
cola las retiene en el siguiente ciclo del worker.

### Salud, clasificación y dead-letter

- Contadores `success_count` / `failure_count`, `consecutive_failures`, `last_http_status`, `last_error`,
  `last_failed_at`. Al llegar a `auto_disable_threshold` fallos seguidos (omisión 10, negativo = nunca)
  la suscripción se apaga sola (`enabled=false`, `auto_disabled_at`; los eventos se retienen) y llama a
  `Config.OnAutoDisable`. Se **reanuda a mano** con `SetEnabled(true)`, que limpia el contador.
- 2xx: entregada. **4xx salvo 408/409/423/425/429: permanente** (dead-letter sin reintentos).
  5xx, 408, 409, 423, 425, 429 y errores de red: reintento con backoff exponencial (cota 1 h) y jitter ±20 %.
- Repetición manual desde dead-letter: `Service.Requeue(id)` (conserva id, llave y orden;
  `POST /logs/:id/requeue`) o `Service.Replay(id)` (nueva fila, intento síncrono).

### Seguridad

- **Destino:** al guardar y al conectar, con la política de `runtime/webhookout` (sobre `netguard`):
  solo https, un dominio (nunca una IP, ni formas numéricas, ni sin punto), sin credenciales en la URL,
  sin espacios internos (`.internal`, `.local`, `svc`…), sin redes privadas/loopback/metadatos
  (comprobado sobre la IP resuelta en cada conexión), puerto 443 salvo `Config.AllowHosts`;
  `AllowHosts` acota a una lista. Sin redirecciones. El cliente por omisión es el de `webhookout`.
- **Datos personales:** el cuerpo retenido se **borra al entregarse** (`payload_purged`); los muertos con
  `PurgeDeadPayloads(webhookID, antesDe)`. Un `Replay`/`Requeue` de una entrega ya purgada devuelve
  `ErrPayloadPurged`. Las respuestas del destino se guardan con claves secretas redactadas
  (`webhookout.RedactJSON`) o como extracto de 256 bytes si no son JSON.
- **Aislamiento:** el puente solo entrega a suscripciones del `OwnerType/OwnerID` = organización del evento.

## Verificación simétrica (recepción)

```go
v := &signing.Verifier{
    Spec: signing.Spec{Profile: signing.HMACCanonicalV1},
    Secrets: func(ctx context.Context, org uuid.UUID) ([]signing.Secrets, error) {
        return []signing.Secrets{{HMACKey: nuevo}, {HMACKey: anterior}}, nil // solape de rotación
    },
}
err := v.Verify(ctx, signing.VerifyRequest(req)) // req: inbound.VerifyRequest (mismos campos y orden)
switch signing.CodeOf(err) { /* unauthenticated | invalid_signature | stale_timestamp | nonce_reused | bad_request */ }
```

`signing.Verifier` cumple `Verify(ctx, VerifyRequest) error`; `VerifyRequest` es idéntico campo a campo a
`inbound.VerifyRequest`, así que se convierte con `signing.VerifyRequest(req)`. Verifica en tiempo
constante, valida ventana (±300 s) y formato del nonce antes de registrarlo, y registra el nonce (por
organización) solo con firma válida. Ante un nonce cache lleno falla cerrado. Para pruebas o recepciones
sin organización existe la función `signing.Verify(spec, secrets, request, header, opts)`.

## Qué no cubre

Límite de tasa por destino/organización, rotación con solape del lado emisor (el emisor resuelve la
referencia en cada intento; el solape lo ofrece el verificador), cola por agregado separada del
`webhook_deliveries` (la llave `ordering_key` + `seq` hace de partición) y extracción a servicio de
entrega (RFC §2.7).

## Endurecimiento (revisión adversarial de #606)

- **Aislamiento por propietario.** Cada ruta de `Handler` resuelve el propietario y carga el webhook con `Service.GetOwned` (las entregas con `DeliveryOwned`, uniendo con `webhooks`): lo ajeno es **404**. El cuerpo de `POST`/`PUT` se lee a una lista blanca (`name, url, events, active, retry_max, timeout_sec, declarative`); `id`, `owner_*`, `organization_id`, `secret`, contadores, salud y `auto_disabled_at` se ignoran. `declarative` se fusiona campo a campo en `PUT` y no se puede poner a `null` (no se degrada a clásico).
- **Secretos por propietario.** `SecretResolver.ResolveSecret(ctx, ownerType, ownerID, ref)`: el resolvedor es la autoridad de alcance y debe fallar si la referencia no es del propietario. Se valida **al guardar** (error `ErrSecretRef`; `SecretRefChecker` opcional para no leer el valor) y se vuelve a exigir al enviar (falla cerrado, sin enviar). Un fallo del almacén de secretos reintenta pero no cuenta como fallo de salud.
- **Cuerpo mínimo.** Se exige `transform` o `payload_fields` (rutas escalares); nunca se vuelca el payload entero. Las rutas deben ser un campo escalar declarado (no `{{payload}}` ni un subárbol). Límite de 256 KB al payload entrante y al cuerpo renderizado; lo que lo excede queda como fila muerta visible. La llave de idempotencia solo admite rutas `event.*` (nada de datos personales en ella) y se valida (ASCII imprimible, ≤255 B).
- **Equidad.** El worker reclama hasta `BatchSize` (10) entregas por ciclo y las envía concurrentemente, con tope por suscripción (`PerWebhookConcurrency`, 2) y por propietario (`PerOwnerConcurrency`, 4). Las bloqueadas por el orden se excluyen en SQL, así que no ocupan la ventana. Tope de `MaxSubscriptionsPerOwner` (100) suscripciones declarativas.
- **Borrado y huérfanas.** `Delete` cancela las entregas pendientes y borra su payload. Si el webhook ya no existe, `drainOnce` marca la entrega muerta y purgada (no se reencola).
- **SSRF.** Las entregas declarativas usan SIEMPRE el cliente protegido de `webhookout` (no `Config.HTTPClient`); el destino se revalida antes de cada envío (`AllowHosts`, política vigente); sin query ni fragmento en la URL; los errores de red no incluyen la URL. Los destinos internos legítimos se autorizan por `Config.AllowInternalHosts`. Los webhooks clásicos usan por omisión ese mismo cliente protegido (sin redirecciones; un `HTTPClient` propio sigue siendo responsabilidad del host).
- **Deduplicación.** Índice único parcial `(webhook_id, event_id) WHERE event_id <> ''` + `ON CONFLICT DO NOTHING`. Sin `envelope.id` el id se deriva de (org, evento, bytes, cubeta de 10 min): un reintento rápido del relay deduplica y un evento idéntico posterior es otra ocurrencia.
- **Texto seguro.** Cuerpos de respuesta y errores se guardan como UTF-8 válido, sin NUL y truncados por runas; si la BD aun así rechaza el guardado se reintenta sin el texto (la entrega nunca queda en vuelo sin registrar el intento).
- **Salud.** Solo cuentan como fallo consecutivo los errores de transporte, 5xx, 408 y 429. 4xx permanentes, 409/423/425, secretos no resolubles, destino bloqueado y filas no renderizables **no** cuentan; `Test` tampoco. Los contadores son `UPDATE` atómicos y la autodesactivación solo fija `auto_disabled_at` (no reescribe la configuración): `Update` y `PUT enabled:true` limpian la salud. `Update` escribe solo las columnas de configuración.
- **Retención.** `HeldRetention` (14 d) y `DeadRetention` (30 d) borran el payload retenido/muerto (`PurgeExpired`, periódico en el pool de workers). Las filas no renderizables quedan `payload_purged` y no se pueden repetir ni reencolar; `Replay` con la suscripción apagada se retiene.

Supuestos conocidos: `hmac_canonical_v1` firma método, ruta (sin query), cuerpo, `ts` y `nonce`, **no** el host, la query ni las cabeceras (incluida la de idempotencia): el destino debe fijar su host y el cuerpo ya viene huellado; la secuencia `per_aggregate` es por proceso; el puente consulta las suscripciones del propietario en cada evento (sin caché).
