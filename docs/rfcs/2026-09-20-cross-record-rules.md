# RFC — Reglas declarativas entre registros (`models[].rules`)

Estado: implementado en este PR (kernel). Consumo por addons: pendiente (ver "Orden de merge").

## Problema

Las `constraints[]` de columna evalúan UNA fila. Tres reglas de caja/gastos que
Odoo resuelve con constraints Python + transacción no son expresables:

1. Un `POSSalePayment` sobre una `POSSession` cerrada (F-VEN-06).
2. El sobrepago: `sum(POSSalePayment.amount) > SalesOrder.total` (F-VEN-06).
3. Dos sesiones abiertas sobre la misma caja (F-VEN-10).
4. Abono mayor al préstamo (`expense_loan_payments` vs `expense_loans`).

El terminal escribe por CRUD `/api/data` y por `data_mutate`/`data_batch`; el
subscriber `.created` corre DESPUÉS de escribir, así que no puede rechazar.

## Alternativas

**A. Cablear `before_create` wasm.** El kernel tiene `lifecycle_hooks.before_create`
pero es opt-in del host y ningún addon lo usa. Más flexible (lógica arbitraria),
pero: (i) el guest corre en su propia invocación/transacción — no puede compartir
la tx del CRUD, así que "misma transacción" y bloqueo de la fila padre no se
sostienen; (ii) añade una invocación wasm (latencia, timeouts, panics como
F-VEN-08) a CADA escritura; (iii) la regla queda opaca (no auditable ni
validable en manifest, no la ve el hub); (iv) cada addon reimplementa lo mismo.

**B. Reglas declarativas (elegida).** Dos `kind` cerrados, evaluados en Go dentro
de la tx de la escritura. Cubre 1, 2 y 4 sin código de addon; es validable
estáticamente (hub, `manifestcheck`), determinista y auditable. Costo: solo lo
que el vocabulario expresa. Si aparece un caso fuera del vocabulario se añade un
`kind` (cambio de kernel revisado), no código arbitrario.

**Caso 3** no necesita regla: es un índice único parcial (`indices[].where`,
kernel#372): `UNIQUE (cash_register_id) WHERE state = 'open'`. La BD lo garantiza
mejor que la aplicación.

## Diseño

```jsonc
"models": [{ "key": "POSSalePayment", ...,
  "rules": [
    { "kind": "ref_state", "error_key": "pos.session_closed",
      "ref": "session_id", "parent": "POSSession",
      "require": { "state": "open" } },
    { "kind": "sum_lte", "error_key": "pos.payment_exceeds_total",
      "ref": "sales_order_id", "parent": "SalesOrder",
      "sum": "amount", "max": "total",
      "where": { "status": ["completed"] } }
  ]}]
```

- `ref_state`: el padre (`ref` → `parent`) debe cumplir `require` (columna →
  escalar o lista, igualdad/IN). Se evalúa en create y cuando cambia `ref`; un
  update que no toca la relación nunca se bloquea porque el padre cambió después.
- `sum_lte`: `sum(sum)` sobre las filas hermanas (mismo `ref`, que cumplan
  `where`) más la fila que se escribe debe ser `<= parent.max`. Bloquea la fila
  padre `FOR UPDATE` antes de sumar: dos escrituras concurrentes se serializan y
  no pueden pasar ambas bajo el tope. En update solo se re-evalúa si cambió
  `ref`, `sum` o una columna de `where`.
- Errores: `*dynamic.ConstraintError` (→ 422 con `error_key`, mismo camino que las
  constraints de columna). En wasm: `constraint_violation`.
- Seguridad: sin SQL libre; identificadores validados por regex (manifest y
  runtime), valores como parámetros; el padre se filtra por `organization_id`
  y `deleted_at` cuando la tabla los tiene (un padre de otro tenant = no existe
  = violación, falla cerrado).
- Sin `request_approval` en esta versión (solo rechazo).

## Dónde se evalúa

| Ruta | Mecanismo |
|---|---|
| `dynamic.Service.Create` | `Transaction`: reglas + INSERT. Sin reglas: INSERT simple como antes. |
| `dynamic.Service.Update` | fuerza el camino transaccional (`core(tx,true)`), reglas sobre la fila fusionada. |
| wasm `data_mutate` / `data_batch` | `dynamic.CrossRecordCompute` se encadena en `Host.WithMutationCompute` (recibe la tx abierta); el error se clasifica `constraint_violation` (antes `db_error`) y revierte tx/batch. Deletes exentos. |

El host (ops) debe: poblar `ModelConstraints.Rules` desde `ModelDefinition.Rules`
en su `ConstraintResolver`, y encadenar `CrossRecordCompute` en su
`MutationComputeFn`. Sin ese cableado las reglas son inertes (comportamiento
previo).

## Límites conocidos

- En wasm no hay `before`: las reglas se re-evalúan en cada update (un update que
  no toca la relación puede fallar si el padre ya está en violación).
- `sum_lte` no convierte monedas: sumar solo filas de la misma unidad (usar `where`).
- La suma cuenta filas físicas del hijo; borrados (soft) no cuentan.
- Concurrencia: el `FOR UPDATE` depende de Postgres; SQLite (tests) lo ignora.
