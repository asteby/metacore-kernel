# Escribir un módulo con nativekit y ledger

Guía para addons genéricos del hub y módulos compilados: transacción por operación, idempotencia en la base, saldos
con libro mayor, eventos atómicos y observabilidad. Todo es genérico: el módulo pone sus tablas y reglas.

| Necesito | Uso | Paquete |
|---|---|---|
| Errores con código estable | `nativekit.E(code, msg, meta)`, `errors.Is` por código, `Code.HTTPStatus()` | `nativekit` |
| Una transacción por operación + efectos post-commit | `nativekit.Run(ctx, db, func(op *Op) error)`, `op.AfterCommit` | `nativekit` |
| Reintento ante deadlock / serialización | `nativekit.RunRetry` | `nativekit` |
| "Aplica una sola vez" (reintentos del cliente) | `nativekit.Once(tx, org, scope, key, fn)` | `nativekit` |
| Bloqueo de filas sin deadlocks | `nativekit.LockRows[T]`, `SortedIDs` | `nativekit` |
| Cantidades/dinero comparables (`==`, `<`) | `dec.D` (6 decimales, `int64`) | `nativekit/dec` |
| Saldos con historial, reverso, conciliación | `ledger.New(...)` | `nativekit/ledger` |
| Evento atómico con los datos | `nkevents.Emitter.EmitTx` | `nativekit/nkevents` |
| Métricas, traza, auditoría | `observe.Observer` | `nativekit/observe` |
| Pruebas con SQLite y Postgres | `nktest.Each` | `nativekit/nktest` |

## 1. Una operación

```go
func (m *Module) Topup(ctx context.Context, db *gorm.DB, org uuid.UUID, in TopupInput) (TopupResult, error) {
    return observe.Track(ctx, m.obs, observe.KindNative, "wallet", "topup", func() (TopupResult, error) {
        var out TopupResult
        err := nativekit.RunRetry(ctx, db, 3, func(op *nativekit.Op) error {
            res, replayed, err := nativekit.Once(op.Tx(), org, "wallet.topup", in.IdempotencyKey,
                func() (TopupResult, error) { return m.apply(op, org, in) },
                nativekit.WithFingerprint(nativekit.Fingerprint(in)))
            out = res
            _ = replayed // la respuesta puede indicar "ya aplicado"
            return err
        })
        return out, err
    })
}
```

Reglas:
- Todas las escrituras van por `op.Tx()`. Lo que no deba ocurrir si la operación falla (publicar, encolar, cachear)
  va en `op.AfterCommit`; un pánico en un hook se registra y no convierte un commit en error.
- `Once` reclama la llave en la MISMA transacción: si `fn` falla no queda llave; si dos peticiones llegan a la vez,
  el índice único serializa y la segunda recibe el resultado de la primera. El resultado se guarda como JSON: el tipo
  debe ser serializable. Reusar la llave con otro payload (`WithFingerprint`) da `idempotency.key_reused`.
- Errores: devuelve `nativekit.E("wallet.frozen", "wallet is frozen", meta)`. El código es API (llega al cliente y al
  catálogo i18n); `Message` es un respaldo en inglés. `Meta` sin datos personales.
- Nunca compares `money.Decimal` con `==`/`<` (es un struct con puntero). Para saldos usa `dec.D`; `FromMoney` /
  `ToMoney` hacen de puente sin redondeo silencioso.

Crea las tablas de idempotencia al arrancar: `nativekit.Migrate(ctx, db)`.

## 2. Libro mayor

```go
wallet := ledger.New("wallet.balance", ledger.Config{
    Emit:   func(ctx context.Context, ev ledger.Event) { /* post-commit */ },
    OnPost: func(ctx context.Context, tx *gorm.DB, ev ledger.Event) error { return nil }, // dentro de la transacción
    // TablePrefix: "wallet_", // tablas propias: wallet_cells|postings|entries
})
_ = wallet.Migrate(ctx, db)

err := wallet.Run(ctx, db, func(s *ledger.Session) error {
    _, err := s.Post(ledger.NewPosting{
        Org: org, Key: nativekit.Key("topup", in.ID), Kind: "topup", Actor: actor,
        Lines: []ledger.Line{{Account: customerID, Delta: amount}},
    })
    return err
})
```

- Una **cuenta** y un **bucket** son cadenas del módulo (inventario: cuenta `producto@almacén`, buckets `quantity` y
  `reserved`; monedero: una cuenta por cliente; caja: cuenta por caja y bucket por medio de pago).
- El **piso** por línea es 0 por defecto (`ledger.insufficient` sin escribir nada). Un límite de crédito es
  `Floor: &(-limite)`; un ledger con sobregiro usa `Config.AllowNegative`.
- `Key` hace el posting idempotente: repetirlo devuelve `Replayed: true` con el resultado original.
- `Reverse` publica el contra-asiento exacto y enlaza ambos; un posting se revierte una sola vez y un reverso no se
  revierte. Sin `AllowNegative` respeta los pisos.
- `Reconcile` compara cada celda con la suma de sus entradas. Un saldo importado se carga como posting `opening`,
  no escribiendo la celda.
- Tablas: por defecto las compartidas `metacore_ledger_*` (particionadas por el nombre del ledger); con `TablePrefix`
  el módulo conserva sus propias tablas.

## 3. Eventos atómicos

```go
em := nkevents.New(dynamic.NewOutboxPublisher(db, bus), "wallet")
em.Arm() // al arrancar

nativekit.Run(ctx, db, func(op *nativekit.Op) error {
    // ... escrituras ...
    return em.EmitTx(op, nkevents.Event{Org: org, Name: "wallet.topup.posted",
        Payload: map[string]any{"id": id}, AggregateType: "wallet", AggregateID: customerID})
})
```

Entrega at-least-once y ordenada por agregado: la fila del outbox y la secuencia del agregado entran en la
transacción; el fan-out ocurre tras el commit; si el proceso muere en medio, el relay republica. Los consumidores
deduplican por el id del evento (el libro de entregas ya lo hace). Dentro de un ledger, `Config.OnPost` es el lugar
natural para llamar a `EmitTx` con el `Op` de la operación.

## 4. Observabilidad

```go
obs := observe.New(
    observe.PrometheusSink(registry.Prometheus),
    observe.AuditSink(func(s observe.Sample) bool { return s.Kind == observe.KindNative && s.Action != "read" },
        func(ctx context.Context, r observe.AuditRecord) { /* en OPS: addonbundle.Audit(...) */ }),
)
compiledDispatcher.Observe(obs) // acciones compiled: una Sample por invocación
```

Métricas estables: `metacore_action_total{kind,module,action,outcome}` y `metacore_action_duration_seconds{kind,module,action}`.
`outcome` ∈ ok, declined (una regla de negocio dijo no, incluye un `{success:false}` de un handler), not_installed,
invalid, forbidden, error. `module` y `action` son acotados (módulos instalados, handlers declarados): nunca pongas ids,
organización ni texto libre. La organización y el actor viajan en la `Sample` para sinks que no son métricas.

Auditoría: el kernel no escribe un activity log propio. `AuditSink` entrega la `Sample` a una función del host; en OPS esa
función llama a `addonbundle.Audit`, de modo que hay un solo escritor. Traza: un `Sink` propio que cree el span a
posteriori con `time.Now().Add(-s.Duration)` (sin dependencia de OpenTelemetry en el kernel).

## 5. Manifiesto: alias y máquinas de estado

- `models[].aliases` (RFC 0004): slugs adicionales que resuelven al modelo; el host registra cada modelo en un
  `modelalias.Index` (rechaza colisiones entre manifiestos) y lo pasa como `Config.ModelAliases` a `dynamic` y `metadata`.
- Máquina de estados: `stage_field` + `stages` + `transitions` (+ `on_transition`). Opcionales: `stage_check: true` (CHECK
  `NOT VALID` derivado), `transitions[].permission` (capacidad que delega una transición) y
  `dynamic.Config.EmitTransitionEvents` (evento `transitioned` por el outbox).

## 6. Pruebas

```go
func TestTopupIsIdempotent(t *testing.T) {
    nktest.Each(t, func(t *testing.T, db *gorm.DB) { // SQLite siempre, Postgres con TEST_POSTGRES_DSN
        ...
    })
}
```

Las pruebas de concurrencia (misma llave desde 8 goroutines, transferencias en orden opuesto) solo tienen sentido en
Postgres: usa `nktest.Postgres(t)` y añade un paso a CI como el de `./nativekit/...`.
