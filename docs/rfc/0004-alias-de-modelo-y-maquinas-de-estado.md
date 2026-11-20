# RFC 0004 — Alias de modelo y máquinas de estado declarativas (manifiesto v3)

Estado: borrador (Draft). Cambio ADITIVO y opcional: ningún manifiesto existente cambia de significado.
Motivación: los módulos nativos de una migración (clientes con rutas y slugs heredados del front) y cualquier
addon que renombre un modelo necesitan (a) resolver varios slugs hacia un mismo modelo y (b) declarar sus
estados y transiciones en lugar de repetir CHECK, lookups y guardas en Go.

## 1. `models[].aliases`

```json
{ "key": "Order", "table": "orders", "aliases": ["order", "sale", "venta"] }
```

- Lista opcional de slugs adicionales con los que `/metadata/{table|modal}/{slug}`, `/data/{slug}` y
  `/options/{slug}` resuelven al modelo. Se comparan sin distinguir mayúsculas ni separadores (`branch-cash-desk`
  == `branch_cash_desks` == `BranchCashDesk`).
- Validación al instalar: un alias no puede coincidir con la `key`, `table` o alias de OTRO modelo instalado en la
  misma organización (conflicto = el instalador rechaza, nunca elige uno en silencio).
- Resolución: el resolvedor de modelo por slug consulta primero key/tabla (comportamiento actual) y solo si no
  encuentra nada consulta el índice de alias. Costo cero para quien no declara alias.
- No cambia permisos: el permiso sigue siendo el del modelo canónico.

## 2. `models[].state_machine`

```json
{
  "key": "Order", "table": "orders",
  "state_machine": {
    "column": "status",
    "initial": "draft",
    "states": [
      {"key": "draft",     "label": "Borrador"},
      {"key": "confirmed", "label": "Confirmada"},
      {"key": "cancelled", "label": "Cancelada", "terminal": true}
    ],
    "transitions": [
      {"from": ["draft"], "to": "confirmed", "action": "confirm", "permission": "orders.confirm"},
      {"from": ["draft", "confirmed"], "to": "cancelled", "action": "cancel", "permission": "orders.cancel"}
    ]
  }
}
```

Efectos (todos derivados, nada duplicado):
1. **CHECK**: el instalador añade `CHECK (status IN (...))` (idempotente) y, si la columna no declara opciones,
   las deriva de `states` para metadata/UI (hoy `options` y CHECK se escriben a mano en dos sitios).
2. **Guarda de transición**: el servicio dinámico rechaza un UPDATE que cambie `column` fuera de `transitions`
   (mismo punto donde hoy actúan `requires_state`/guardas de acción) y exige `permission` cuando se declara.
3. **UI**: `TableMetadata` sirve `state_machine`; el SDK pinta la insignia de estado y solo ofrece las acciones
   cuyas transiciones parten del estado actual (reutiliza `requires_state` de las acciones).
4. **Eventos**: cada transición emite `<addon>.<model>.<action>` por el bus (post-commit, outbox).

Compatibilidad: sin `state_machine` todo queda como hoy. `requires_state` en acciones sigue válido y se
considera la forma imperativa del mismo dato.

## 3. Outbox para eventos de módulos nativos

OPS ya tiene el relé de outbox (`handlers/event_outbox_relay*`). `NativeEnv.Events` (ops#2408) debe escribir en
la MISMA tabla de outbox dentro de la transacción del módulo y dejar que el relé entregue (at-least-once,
reintentos), en vez de emitir tras el commit sin respaldo. El módulo recibe `Events` y no sabe si hay outbox.
Pendiente de implementar en OPS: variante transaccional `EmitTx(tx, org, event, payload)`.

## 4. Lo que NO se propone
- Máquinas de estado jerárquicas, paralelas o con tiempos: fuera de alcance.
- Alias a nivel de acción o campo.
