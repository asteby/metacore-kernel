# Columnas snapshot y autorizaciones (manifest v3)

Dos primitivos declarativos de la fase de stamp de la creación. Corren igual en
`POST /data`, en `data_mutate` / `data_batch` (wasm) y en las acciones, antes de
las fórmulas y los guards.

## `models[].snapshots`

```json
"snapshots": [
  {"from": "product_id", "fields": {"min_price": "min_price_at_sale", "cost_price": "unit_cost"}},
  {"from": "sales_order_id", "fields": {"branch_id": "branch_id", "seller_id": "seller_id", "order_date": "sold_at"}}
]
```

`from` es una columna con `ref`; `fields` va de columna origen a columna destino.
Los destinos DEBEN ser `protected: true`: el cliente no los escribe y, en wasm,
el valor del servidor pisa lo que mande el guest. La lectura SIEMPRE filtra por
`organization_id` del que escribe; sin org, o con una tabla origen sin
`organization_id`, la escritura falla cerrado (nunca copia sin acotar). Solo al crear:
el snapshot congela el valor del momento (el costo y el piso de la venta no
cambian si el producto se edita después).

## `columns[].grant`

```json
{"name": "price_approval_id", "type": "uuid", "grant": {
  "policy": "price_below_min",
  "sets": "price_authorized",
  "context_eq": {"product_id": "product_id"},
  "context_gte": {"unit_shortfall": "min_price_at_sale - unit_price"}
}}
```

Si la columna trae un id de autorización PIN y alguna expresión de
`context_gte` es positiva (se necesita autorización), el kernel canjea el grant
(`ConsumeApproval`: misma org, `kind=pin` de esa política, pedido por el mismo
usuario, vigente, de un solo uso, producto y monto cubiertos) y deja `sets` en
1; si el canje falla, la escritura se rechaza (`approval_invalid`). Si no se
necesita, el id se ignora y no se quema. `sets` debe ser `protected`.

## Piso de precio derivado

```json
"formulas": [{"target": "price_shortfall",
  "expr": "(min_price_at_sale - unit_price) * (1 - price_authorized)"}],
"columns": [{"name": "price_shortfall", "protected": true, "constraints": [
  {"expr": "price_shortfall <= 0", "error_key": "...price_below_floor",
   "on_violation": "request_approval", "approval": {"roles": ["supervisor"]}}]}]
```

Negativo = holgura sobre el piso; autorizada = 0. El cliente no puede escribir
ninguna de las columnas involucradas.

## Ciclo de vida (create y update)

- **Origen inexistente = escritura rechazada.** Si `from` trae valor y la fila
  no existe en la org del que escribe (id ajeno, inexistente o con
  `deleted_at`), la escritura falla (`ErrSnapshotSource`, 422 /
  `validation_error`); nunca se limpian los destinos, porque un destino vacío
  saltaría los controles que lo leen (el piso de precio).
- **Update.** El stamp también corre al actualizar (`PATCH /data`, `data_mutate`
  y `data_batch` con `Host.WithUpdateStamp`). Los destinos y los `sets` nunca
  vienen del cliente. Si el parche cambia una columna `from`, el snapshot se
  vuelve a tomar; si cambia algo que cubría una autorización (`product_id`,
  `unit_price`, o cualquier columna que lean `context_eq` / `context_gte`, aun
  vía fórmula), el flag `sets` vuelve a 0 y hace falta un id de autorización
  NUEVO en el parche: el que traía la fila ya se gastó con los valores
  anteriores. Un parche que no toca nada de eso conserva la autorización.
- **El canje se hace al final.** El stamp solo VALIDA la grant (el guard lee el
  flag); se gasta dentro de la misma transacción que el INSERT/UPDATE, después
  de los guards. Un guard que rechaza o estaciona la escritura (`request_approval`)
  no quema nada y el replay puede canjear; un rollback deja la grant vigente.
- `context_gte` se evalúa sobre la fila ya con las columnas derivadas
  recalculadas (con el flag en 0, es decir, el faltante real).
