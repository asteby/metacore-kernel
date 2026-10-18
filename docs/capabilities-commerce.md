# Contratos de capacidad de comercio y fiscal

Referencia de los contratos bien conocidos del paquete `capability` del kernel
para comercio (`commerce.*`) y facturación (`fiscal.*`). El núcleo de comercio
(carrito, checkout, pedidos) solo nombra estas llaves; qué addon contesta
(`mercadopago`, `stripe`, `skydropx`, `envia`, `emissary`, `region_mx`,
`stripe_tax`, `factura_com`…) es una decisión de instalación y de selección por
mercado (PLAN-HEADLESS H12/H13).

> Las tablas de "Formas compartidas" y "Contratos" se generan del registro
> `capability.Contracts`; si cambian los contratos en Go, regenérelas.

## Modelo

| Concepto | Regla |
|---|---|
| **Tipo** | `action` (dispara al proveedor, sin salida para el consumidor; p. ej. `messaging.whatsapp.send`) o `query` (declara `Output`; el consumidor recibe la salida normalizada). Todos los contratos de esta página son `query`. |
| **Cardinalidad** | `one`: el proveedor seleccionado. `all`: fan-out en paralelo a todos los proveedores elegibles, cada uno con su tiempo límite; devuelve resultados parciales y una lista de errores por proveedor. Nunca falla toda la llamada porque un proveedor falló. |
| **Tiempo límite** | Por proveedor: `Provider.Timeout` > `Contract.Timeout` > `Executor.Timeout` > 8 s. Contratos mutantes 30 s, `commerce.shipping.rates` 4 s, `commerce.search.query` y `commerce.discounts.evaluate` 2 s. |
| **Dinero** | Siempre cadena decimal (`"1499.00"`, regex `^-?(0\|[1-9][0-9]*)(\.[0-9]+)?$`) junto a una moneda ISO 4217 (`"MXN"`). Nunca flotantes ni unidades menores. |
| **Países** | ISO 3166-1 alfa-2 en mayúsculas (`"MX"`). |
| **Coordenadas** | `latitude` / `longitude`: número JSON en grados decimales WGS84, en `[-90, 90]` / `[-180, 180]`; nunca cadena. Ver [capabilities-geo.md](capabilities-geo.md). |
| **Entrada** | Abierta: campos no declarados pasan tal cual (el mapeo del proveedor puede leerlos); los declarados se validan por tipo y los requeridos deben venir y no estar vacíos. |
| **Salida** | Cerrada: tipos, enums y requeridos se validan; campos desconocidos se rechazan salvo extensiones con prefijo `x_` (p. ej. `x_init_point`). Una salida inválida cuenta como error de ese proveedor (`invalid_output`) y no se entrega. |
| **Estados normalizados** | Pago: `pending, requires_action, authorized, paid, failed, cancelled, expired, refunded, partially_refunded`, y desde el estándar de pagos `at_device` (el monto está en una terminal o reciclador esperando al comprador) y `cancelling` (cancelación aceptada sin confirmar: todavía puede cobrarse). Un consumidor que no conoce un estado lo trata como **no final**. Transiciones válidas y reglas en [RFC-PAYMENTS-STANDARD](RFC-PAYMENTS-STANDARD.md) y en el paquete `payments`. Canal (`channel`): `terminal, online, qr, link, cash`. Rastreo: `created, picked_up, in_transit, out_for_delivery, delivered, exception, returned, cancelled`. |

## Resolución por mercado: `(organización, mercado, capacidad) → addon`

El host pasa a `capability.Select` los proveedores instalados y habilitados en
la organización (`[]capability.Provider`) y el mercado de la petición
(`capability.Market{Code, Countries}`, que viene de `apiroute.Request.Market`).

Cardinalidad `one`:

1. el proveedor que la organización eligió para ese mercado (`Provider.Markets` contiene el código);
2. si no hay, el proveedor por omisión de la organización (`Provider.Default`) entre los que atienden los países del mercado;
3. si no hay, el único proveedor que atiende el mercado;
4. si no: `ErrAmbiguousProvider` (varios) o `ErrNoProvider` (ninguno).

Cardinalidad `all`: todos los que atienden los países del mercado; si la
organización eligió explícitamente proveedores para ese mercado, solo esos.

Sin mercado (`Market{}`) se usa la selección a nivel organización y no se
filtra por país.

Un addon declara opcionalmente los países que atiende en su manifiesto:

```jsonc
"provides_capabilities": [{
  "key": "fiscal.invoice.create",
  "label": "Factura.com (CFDI 4.0)",
  "handler": {"type": "wasm", "function": "invoice_create"},
  "countries": ["MX"]
}]
```

El host guarda la selección en los ajustes de la instalación:
`_capability_defaults: ["commerce.payments.session"]` (ya existía en OPS) y
`_capability_markets: {"commerce.payments.session": ["mx"]}`;
`capability.SelectionFromSettings` los lee.

Casos reales:

| Organización / mercado | Pagos | Impuestos | Envío / fulfillment | Fiscal |
|---|---|---|---|---|
| Gods Nutrition `mx` | `mercadopago` | `region_mx` | `emissary` (3PL) | CFDI vía `fiscal_mexico` |
| Gods Nutrition `us` | `stripe` | `stripe_tax` | 3PL por definir | — |
| Protekauto (MX) | `mercadopago` | `region_mx` | `skydropx` (+ `shipping_flat` por omisión) | `factura_com` vía `fiscal_mexico` |

## Seguimiento de una operación: el proveedor se fija, no se adivina

`Select` decide el proveedor de una llamada **nueva** (crear sesión de pago,
comprar guía, emitir factura). Todo lo que sigue sobre algo que un proveedor ya
creó va **fijado a ese proveedor** con `Query.Provider` (llave del addon):
`payments.confirm`, `payments.status`, `payments.refund`, `shipping.cancel`,
`shipping.track`, `fulfillment.cancel`, `fiscal.invoice.cancel`, `tax.commit` y
`tax.reverse` (`Contract.PinRequired`). Sin `Provider`, `Engine.Query` responde
`ErrProviderRequired` sin invocar a nadie. El proveedor fijado debe estar
instalado y habilitado en la organización y atender los países del mercado
(`SelectPinned`); si no, `ErrNoProvider`. La selección por mercado, el
proveedor por omisión y la ambigüedad no intervienen.

Reglas para el host:

- Guarda la llave del addon en el registro del pago, del envío, de la factura y de la transacción de impuestos al crearlos, y úsala en todo seguimiento y reembolso aunque el proveedor por omisión o la selección del mercado hayan cambiado.
- Las rutas de webhook llevan el proveedor en la ruta (por ejemplo `/webhooks/payments/{addon}`) y lo pasan como `Query.Provider`; el `notify_url` que se envía en `payments.session` ya incluye esa ruta. Un evento de Stripe nunca llega al proveedor por omisión.

## Operaciones mutantes: idempotencia obligatoria

`payments.session`, `payments.refund`, `shipping.label`, `fulfillment.create`,
`fiscal.invoice.create`, `tax.commit` y `tax.reverse` mueven dinero o tienen
efectos irreversibles (`Contract.Idempotent`):

- `idempotency_key` es **requerida** (cadena no vacía, estable por operación de negocio, p. ej. `ship-{order_ref}`); el Executor rechaza la llamada con `ErrIdempotencyRequired` sin invocar. Se entrega al proveedor en la entrada mapeada y en `Call.IdempotencyKey`; el proveedor debe deduplicar con ella y devolver el resultado original en un reintento.
- Plazo por omisión **30 s** (`MutatingTimeout`), no 8 s.
- Si vence el plazo (o el llamador cancela con la llamada en vuelo) el resultado es **indeterminado**: el error envuelve `ErrTimeout` (o el error del contexto) y `ErrIndeterminate`, `Result.Indeterminate()` es `true` y el sobre JSON lleva `"indeterminate": true`. La llamada al proveedor puede seguir y terminar con éxito. El host no reintenta con otra llave ni da la operación por fallida: concilia con `payments.status` / `shipping.track` o reintenta con la **misma** llave.

## Reglas de `commerce.payments.confirm` (webhooks)

El webhook es entrada no confiable. El proveedor y el host deben:

- Verificar la firma exactamente como lo documenta el proveedor (algoritmo, cadena firmada, `raw_body` byte a byte, comparación en tiempo constante) y rechazar si falta o no coincide.
- Aplicar la ventana de repetición (timestamp firmado dentro de la tolerancia del proveedor) para rechazar eventos reenviados.
- Consultar el pago a la API del proveedor con su id y devolver de ahí `status`, `amount` y `currency`; nunca confiar en monto, moneda o estado del cuerpo del webhook.
- El host deduplica por `provider + event_id` (los reintentos del proveedor son normales) y compara `amount`/`currency` con el total del pedido antes de marcarlo pagado; una diferencia no marca el pedido.

## Eventos de contrato: `commerce.payments.confirmed`, `commerce.shipping.tracking_updated`, `commerce.fulfillment.updated`

Además de consultas, un dominio de capacidad puede declarar **eventos de
contrato** (`capability.EventContracts`, `RegisterEvent`, `LookupEvent`). El
consumidor se suscribe al nombre normalizado y nunca a un proveedor
(`stripe.payment_updated`), igual que consulta una capacidad sin nombrar quién
responde.

| Evento | Lo emite | Payload |
|---|---|---|
| `commerce.payments.confirmed` | quien provee alguna capacidad `commerce.payments.*` (las pasarelas, desde su propio webhook; `payments_manual`, desde la acción del personal) | `{provider, result}`; `result` es la salida de `commerce.payments.confirm` y `result.kind` ∈ `payment \| refund \| dispute` |
| `commerce.shipping.tracking_updated` | quien provee alguna capacidad `commerce.shipping.*` (las paqueterías: `skydropx`, `envia`, `noventa9minutos`), desde su webhook o su sondeo | cambio de estado normalizado de un envío (`TrackingUpdatedSchema`, abajo) |
| `commerce.fulfillment.updated` | quien provee alguna capacidad `commerce.fulfillment.*` (almacenes y 3PL: `emissary`) | cambio de estado normalizado de un surtido (`FulfillmentUpdatedSchema`, abajo) |

Payload (`CommercePaymentsConfirmed`):

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `provider` | string | sí | clave del addon proveedor (informativa: el suscriptor confía en `emitter`) |
| `result` | object ([salida de confirm](#commercepaymentsconfirm)) | sí | `kind` distingue pago, reembolso y disputa |
| `organization_id` | string |  | informativo (el bus lleva la org) |
| `occurred_at` | string |  | RFC 3339 de la observación |
| `source` | string: webhook \| poll \| reconcile \| staff |  | cómo se observó |
| `payment_id` | string |  | id canónico en el ledger de pagos, cuando ya está registrado |
| `document_type`, `document_id` | string |  | documento pagado en OPS |

Un suscriptor en Go valida la entrega con `payments.DecodeConfirmed`: contrato,
`emitter` estampado presente e igual a `provider` (un proveedor no habla por
otro). Un proveedor solo de terminal (`commerce.payments.terminal.*`, p. ej. el
reciclador de efectivo) también es proveedor del dominio y puede emitirlo; un
canal de venta (POS, caja, kiosko) no.

Payload (`CommerceShippingTrackingUpdated`, `capability.TrackingUpdatedSchema`):

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `provider` | string | sí | clave del addon (informativa: el suscriptor confía en `emitter`) |
| `event_id` | string | sí | estable por cambio observado; el suscriptor deduplica por `emitter + event_id` |
| `status` | string | sí | `TrackingStatuses`: `created`, `picked_up`, `in_transit`, `out_for_delivery`, `delivered`, `exception`, `returned`, `cancelled` |
| `occurred_at` | string | sí | RFC 3339 del escaneo |
| `order_ref` | string |  | el `order_ref` de `commerce.shipping.label`; ausente si la paquetería no lo sabe |
| `shipment_id` | string |  | `shipment_id` de `commerce.shipping.label` |
| `carrier`, `tracking_number`, `tracking_url` | string |  |  |
| `label_url` | string |  | cuando ya hay guía (las guías asíncronas llegan después) |
| `label_ready` | boolean |  | este cambio es el que dejó lista la guía |
| `provider_status`, `description`, `location` | string |  | texto o código del proveedor, sin traducir |
| `test_mode` | boolean |  | evento del sandbox |

Payload (`CommerceFulfillmentUpdated`, `capability.FulfillmentUpdatedSchema`):

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `provider` | string | sí | clave del addon (informativa: el suscriptor confía en `emitter`) |
| `event_id` | string | sí | estable por cambio observado |
| `fulfillment_id` | string | sí | el `fulfillment_id` de `commerce.fulfillment.create` |
| `order_ref` | string | sí | el `order_ref` de `commerce.fulfillment.create` |
| `status` | string | sí | `FulfillmentStatuses`: los de `commerce.fulfillment.create` (`accepted`, `on_hold`, `rejected`) seguidos de `TrackingStatuses` |
| `occurred_at` | string | sí | RFC 3339 |
| `previous_status` | string |  | mismo vocabulario |
| `warehouse`, `carrier`, `tracking_number`, `tracking_url` | string |  |  |
| `provider_status`, `description` | string |  | texto o código del proveedor, sin traducir |
| `test_mode` | boolean |  | evento del sandbox |

Los dos esquemas son cerrados: un campo propio del proveedor va con prefijo
`x_`. Un 3PL que solo provee `commerce.fulfillment.*` publica el estado del
paquete en `commerce.fulfillment.updated`, no en `commerce.shipping.*` (no es
proveedor de ese dominio).

Reglas:

- **Espacio reservado.** Todo evento bajo `EmittedBy` (`commerce.payments.*`,
  `commerce.shipping.*`, `commerce.fulfillment.*`)
  solo lo puede emitir un addon que **provee** al menos una capacidad de ese
  dominio (`capability.ReservedEventDomain`, `capability.ProvidesDomain`). El
  host lo aplica en la frontera del guest (`security.EmitPolicy`, regla
  `contract_namespace`; ver [capabilities.md](capabilities.md#política-de-emisión-de-eventos)).
- **Emisor estampado.** El despachador escribe `emitter` (la clave con la que se
  llamó al bus) en el payload entregado y sobrescribe lo que haya escrito el
  guest. El suscriptor usa `emitter`, no `provider`; si ambos vienen y no
  coinciden, rechaza.
- **No es prueba de pago.** El consumidor vuelve a leer el pago con
  `commerce.payments.status` fijado al proveedor de la sesión antes de marcar el
  pedido; un evento falso solo provoca una consulta.
- `EventContract.ValidatePayload` valida el payload e ignora las claves que
  estampa el host (`event`, `emitter`, `actor_id`, `branch_id`). El kernel no
  la aplica al publicar; es para emisores y suscriptores.
- Los eventos propios de cada pasarela (`<key>.payment_updated`,
  `<key>.refund_created`) siguen existiendo; el evento normalizado se emite
  junto a ellos. Igual con los envíos: el nombre heredado
  `shipping.tracking_updated` (sin reservar: cualquier addon que lo declare lo
  puede emitir) y `emissary.shipment_status_changed` se siguen emitiendo por
  compatibilidad, pero un suscriptor que dispare avisos o mueva pedidos se
  suscribe solo al contrato.
- **No es prueba de entrega.** Igual que con los pagos, quien vaya a mover un
  pedido con el evento puede volver a leer el estado con
  `commerce.shipping.track` fijado al proveedor del envío.

## Errores con código del proveedor

Un contrato puede declarar los códigos de error que sus proveedores reportan
(`Contract.Errors`, p. ej. `invalid_input`, `not_found`). El invocador del host
devuelve `capability.NewCodedError(code, message)` cuando el proveedor contestó
un error tipado (en WASM, el sobre `{success:false, error:{code, message}}` del
guest), y el consumidor lo ve en `Result.ErrorCode()` y en
`errors[].code` del sobre JSON. Un código que el contrato no declara se reporta
como `provider_error` con el mismo mensaje. `timeout`, `invalid_output`,
`panic`, `canceled` y `provider_error` están reservados al ejecutor.

Un campo de texto puede fijar `Field.Pattern` (RE2, con anclas): la entrada que
no cumple se rechaza antes de invocar (`ErrInvalidInput`; `validation_error` en
`capability_query`) y una salida que no cumple es `invalid_output`.

## API Go (host-neutral)

```go
// Consumidor: cualquier servicio del host.
var q capability.Querier = &capability.Engine{
    Source: myProviderSource, // manifiestos + ajustes de instalación → []Provider
    Invoke: myInvoker,        // despacha Call.Payload al export wasm / operación native
    Executor: capability.Executor{MaxParallel: 8},
}
res, err := q.Query(ctx, capability.Query{
    Org: orgID, Market: capability.Market{Code: "mx", Countries: []string{"MX"}},
    Capability: capability.CommerceShippingRates, Input: input,
})
// err != nil solo antes de invocar: contrato no-query, entrada inválida,
// sin proveedor o ambiguo.
for _, r := range res.Outputs() { /* r.Provider, r.Output (json válido) */ }
for _, r := range res.Failed()  { /* r.ErrorCode(): timeout | invalid_output | panic | canceled | código de Contract.Errors | provider_error */ }
out, err := res.One() // cardinalidad one

// Seguimiento: fijar el proveedor que creó el registro (ver más abajo).
_, err = q.Query(ctx, capability.Query{
    Org: orgID, Capability: capability.CommercePaymentsRefund,
    Provider: payment.Addon, Input: refundInput, // incluye idempotency_key
})
```

Sobre JSON para consumidores (`json.Marshal(res)` o `res.Envelope()`):

```json
{"capability":"commerce.shipping.rates","market":"mx",
 "results":[{"provider":"skydropx","output":{"rates":[…]}}],
 "errors":[{"provider":"envia","code":"timeout","message":"…"}]}
```

`Executor.Run` aplica el mapeo propio del proveedor
(`provides_capabilities[].input`) a la entrada del contrato antes de invocar,
corre una goroutine por proveedor con `context.WithTimeout`, aísla pánicos,
deja de esperar al vencer el plazo aunque el invocador lo ignore y valida cada
salida contra `Contract.Output`.

Reglas de H12 que siguen vigentes: nada de WASM por pulsación en rutas
calientes (`commerce.search.query` debe servirse de un índice que el host
consulta); siempre hay un proveedor por omisión instalado (`search_basic`,
`shipping_flat`, `manual`).

## Formas compartidas

### Address

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `name` | string |  | contact name |
| `company` | string |  | company name |
| `line1` | string |  | street and number |
| `line2` | string |  | interior, suite |
| `neighborhood` | string |  | colonia / district |
| `city` | string |  | city / municipio |
| `state` | string |  | state / province (code or name) |
| `postal_code` | string | sí | postal code |
| `country` | country | sí | ISO 3166-1 alpha-2 |
| `phone` | string |  | E.164 preferred |
| `email` | string |  | contact email |
| `reference` | string |  | delivery reference / between streets |

### Buyer

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `id` | string |  | customer id in OPS |
| `email` | string |  |  |
| `name` | string |  |  |
| `phone` | string |  |  |
| `tax_id` | string |  | RFC / VAT id |

### Card

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `type` | string: credit \| debit \| prepaid \| other |  | fiscal payment form follows it (credit vs debit) |
| `brand` | string |  | visa / master / amex / debmaster / ... |
| `last4` | string |  | last four digits, as a string (keeps leading zeros) |
| `auth_code` | string |  | issuer authorization code |
| `entry_mode` | string |  | chip / contactless / swipe / manual / online |

### CashTender

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `tendered` | decimal |  | cash the buyer inserted / handed over |
| `change` | decimal |  | change returned |
| `operation_id` | string |  | device operation id (e.g. the cash recycler operation), kept for reconciliation |

### Line

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `id` | string | sí | line id, echoed back in per-line outputs |
| `sku` | string |  |  |
| `title` | string |  |  |
| `quantity` | integer | sí |  |
| `unit_price` | decimal | sí | unit price in the request currency |
| `discount` | decimal |  | line discount amount (total, not per unit) |
| `tax_code` | string |  | provider/fiscal product tax code |
| `product_ref` | string |  | product/variant id |

### Parcel

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `weight_kg` | decimal | sí |  |
| `length_cm` | decimal |  |  |
| `width_cm` | decimal |  |  |
| `height_cm` | decimal |  |  |
| `quantity` | integer |  | identical parcels (default 1) |
| `content` | string |  | content description (customs/carrier) |

### Seller

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `account_ref` | string | sí | seller account id at the provider (OAuth-linked account / connected account) |
| `seller_id` | string |  | seller id in OPS |
| `application_fee` | decimal |  | platform commission retained from this payment, in the payment currency |

### ShipItem

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `sku` | string | sí |  |
| `quantity` | integer | sí |  |
| `title` | string |  |  |
| `id` | string |  | order line id |
| `unit_price` | decimal |  |  |
| `product_code` | string |  | fiscal product key (SAT ClaveProdServ, carta porte) |

### TaxLine

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `name` | string | sí | e.g. IVA, State tax |
| `rate` | decimal | sí | 0.16 |
| `amount` | decimal | sí |  |
| `jurisdiction` | string |  |  |

### TrackingEvent

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `at` | string | sí | RFC 3339 timestamp |
| `status` | string: created \| picked_up \| in_transit \| out_for_delivery \| delivered \| exception \| returned \| cancelled | sí |  |
| `description` | string |  | carrier wording |
| `location` | string |  |  |
| `lat` | latitude |  | WGS84 latitude of the scan, only when the carrier reports coordinates |
| `lng` | longitude |  | WGS84 longitude of the scan, only when the carrier reports coordinates |

## Contratos

### `commerce.discounts.evaluate`

every discount engine proposes adjustments for a cart. Consulta · cardinalidad **all** · tiempo límite por proveedor **2s**.

Entrada:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `currency` | currency | sí |  |
| `lines` | array<Line> | sí |  |
| `codes` | array<string> |  | coupon codes entered by the buyer |
| `buyer` | Buyer |  |  |
| `subtotal` | decimal |  |  |

Salida:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `discounts` | array<object> | sí |  |
| `discounts[].id` | string | sí |  |
| `discounts[].title` | string | sí |  |
| `discounts[].amount` | decimal | sí | total discount |
| `discounts[].currency` | currency | sí |  |
| `discounts[].code` | string |  | coupon code that produced it |
| `discounts[].line_id` | string |  | line it applies to; absent = order level |
| `discounts[].combinable` | boolean |  |  |
| `rejected_codes` | array<object> |  |  |
| `rejected_codes[].code` | string | sí |  |
| `rejected_codes[].reason` | string |  |  |

### `commerce.fulfillment.cancel`

cancel a fulfillment that has not shipped. Consulta · requiere `Query.Provider` · cardinalidad **one** · tiempo límite por proveedor **8s**.

Entrada:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `fulfillment_id` | string | sí |  |
| `order_ref` | string |  |  |
| `reason` | string |  |  |

Salida:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `status` | string: cancelled \| rejected | sí |  |
| `message` | string |  | why it could not be cancelled |

### `commerce.fulfillment.create`

send an order to a warehouse / 3PL for picking and shipping. Consulta · cardinalidad **one** · tiempo límite por proveedor **30s** · mutante (idempotente).

Entrada:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `order_ref` | string | sí |  |
| `items` | array<ShipItem> | sí |  |
| `destination` | Address | sí |  |
| `service_level` | string |  | standard / express / … or a rate id |
| `carrier` | string |  |  |
| `warehouse` | string |  | provider warehouse code |
| `buyer` | Buyer |  |  |
| `declared_value` | decimal |  |  |
| `currency` | currency |  |  |
| `notes` | string |  |  |
| `idempotency_key` | string | sí | required; stable per business operation (reuse it on every retry); the provider must dedupe on it |

Salida:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `fulfillment_id` | string | sí |  |
| `status` | string: accepted \| on_hold \| rejected | sí |  |
| `warehouse` | string |  |  |
| `tracking_number` | string |  |  |
| `carrier` | string |  |  |
| `tracking_url` | string |  |  |
| `message` | string |  | reason when on_hold/rejected |

### `commerce.payments.cancel`

cancel a payment that is not captured yet (open session, terminal order, authorization); re-read the status afterwards. Consulta · requiere `Query.Provider` · cardinalidad **one** · tiempo límite por proveedor **30s** · mutante (idempotente).

Códigos de error del proveedor: `not_found`.

Entrada:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `provider_ref` | string |  | provider payment / terminal order id (one of provider_ref or session_id) |
| `session_id` | string |  | provider session id when no payment exists yet |
| `order_ref` | string |  |  |
| `device_id` | string |  | terminal holding the order, when the provider needs it |
| `reason` | string |  |  |
| `idempotency_key` | string | sí | required; stable per business operation (reuse it on every retry); the provider must dedupe on it |

Salida:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `status` | string: cancelled \| cancelling \| not_cancellable | sí | cancelling: accepted but not confirmed (the device may still capture it): poll commerce.payments.status |
| `payment_status` | string: pending \| requires_action \| authorized \| paid \| failed \| cancelled \| expired \| refunded \| partially_refunded \| at_device \| cancelling |  | normalized payment status after the call |
| `message` | string |  | why it could not be cancelled |

### `commerce.payments.capabilities`

what this provider supports (channels, refunds, cancellation, limits, marketplace); channels read it instead of hard-coding a provider. Consulta · requiere `Query.Provider` · cardinalidad **one** · tiempo límite por proveedor **8s**.

Entrada:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `market` | string |  | market code, when support differs per market |
| `channel` | string: terminal \| online \| qr \| link \| cash |  | restrict the answer to one channel |

Salida:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `channels` | array<string> | sí | subset of terminal / online / qr / link / cash |
| `methods` | array<string> |  | method families: card / msi / oxxo / spei / wallet / bnpl / bank_transfer / cash |
| `currencies` | array<string> |  | ISO 4217 codes |
| `min_amount` | decimal |  | smallest chargeable amount (e.g. a terminal minimum) |
| `max_amount` | decimal |  |  |
| `partial_refunds` | boolean |  |  |
| `multiple_refunds` | boolean |  | several partial refunds on one payment |
| `refund_window_days` | integer |  | 0 = no limit |
| `cancel` | boolean |  | commerce.payments.cancel is supported |
| `cancel_is_async` | boolean |  | cancellation may answer cancelling (confirmed later) |
| `installments` | boolean |  | interest-free installments (MSI) |
| `terminal_expires_seconds` | integer |  | lifetime of a terminal order when not paid |
| `reconcile` | boolean |  | commerce.payments.reconcile is supported |
| `marketplace_split` | boolean |  | charges on behalf of a seller account (SellerSchema) |
| `seller_oauth` | boolean |  | seller accounts are linked through the provider OAuth |
| `test_mode` | boolean |  | the configured credentials are sandbox credentials |

### `commerce.payments.confirm`

verify and normalize a provider webhook/notification into a payment status. Consulta · requiere `Query.Provider` · cardinalidad **one** · tiempo límite por proveedor **8s**.

Entrada:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `payload` | any | sí | webhook body (decoded JSON, or the raw string) |
| `headers` | object |  | request headers, lowercase keys (signature verification) |
| `query` | object |  | query-string parameters |
| `raw_body` | string |  | exact body bytes as text, when the signature covers them |

Salida:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `order_ref` | string | sí | the order_ref sent to commerce.payments.session |
| `status` | string: pending \| requires_action \| authorized \| paid \| failed \| cancelled \| expired \| refunded \| partially_refunded \| at_device \| cancelling | sí |  |
| `amount` | decimal | sí | amount of this status (paid/refunded) |
| `currency` | currency | sí |  |
| `provider_ref` | string | sí | provider payment id |
| `event_id` | string |  | provider event id (dedupe on provider+event_id) |
| `kind` | string: payment \| refund \| dispute |  | default payment |
| `method` | string |  | card / msi / oxxo / spei / wallet / bnpl / bank_transfer / cash |
| `installments` | integer |  |  |
| `paid_at` | string |  | RFC 3339 |
| `occurred_at` | string |  | RFC 3339 |
| `amount_refunded` | decimal |  |  |
| `fee` | decimal |  | provider fee |
| `net_amount` | decimal |  | amount minus fee |
| `test_mode` | boolean |  | event from the provider sandbox |
| `channel` | string: terminal \| online \| qr \| link \| cash |  | how the buyer paid; default online |
| `session_id` | string |  | session / preference / terminal order id the payment belongs to |
| `device_id` | string |  | terminal / cash recycler that took the payment |
| `card` | Card |  | card details when the method is a card |
| `cash` | CashTender |  | cash device details |
| `seller` | Seller |  | marketplace seller the payment was charged for |
| `provider_status` | string |  | provider wording / code of the status, untranslated |
| `status_detail` | string |  | provider reason (rejection code, expiry, ...) |

### `commerce.payments.reconcile`

list the provider's payments updated in a time window (reconciliation job); items are commerce.payments.status outputs. Consulta · requiere `Query.Provider` · cardinalidad **one** · tiempo límite por proveedor **8s**.

Entrada:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `from` | string | sí | RFC 3339, inclusive |
| `to` | string | sí | RFC 3339, exclusive |
| `cursor` | string |  | next_cursor of the previous page |
| `limit` | integer |  | page size hint |
| `seller` | Seller |  | marketplace: only this seller's payments |

Salida:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `payments` | array<object> | sí | normalized payments, any order |
| `payments[].order_ref` | string | sí | the order_ref sent to commerce.payments.session |
| `payments[].status` | string: pending \| requires_action \| authorized \| paid \| failed \| cancelled \| expired \| refunded \| partially_refunded \| at_device \| cancelling | sí |  |
| `payments[].amount` | decimal | sí | amount of this status (paid/refunded) |
| `payments[].currency` | currency | sí |  |
| `payments[].provider_ref` | string | sí | provider payment id |
| `payments[].event_id` | string |  | provider event id (dedupe on provider+event_id) |
| `payments[].kind` | string: payment \| refund \| dispute |  | default payment |
| `payments[].method` | string |  | card / msi / oxxo / spei / wallet / bnpl / bank_transfer / cash |
| `payments[].installments` | integer |  |  |
| `payments[].paid_at` | string |  | RFC 3339 |
| `payments[].occurred_at` | string |  | RFC 3339 |
| `payments[].amount_refunded` | decimal |  |  |
| `payments[].fee` | decimal |  | provider fee |
| `payments[].net_amount` | decimal |  | amount minus fee |
| `payments[].test_mode` | boolean |  | event from the provider sandbox |
| `payments[].channel` | string: terminal \| online \| qr \| link \| cash |  | how the buyer paid; default online |
| `payments[].session_id` | string |  | session / preference / terminal order id the payment belongs to |
| `payments[].device_id` | string |  | terminal / cash recycler that took the payment |
| `payments[].card` | Card |  | card details when the method is a card |
| `payments[].cash` | CashTender |  | cash device details |
| `payments[].seller` | Seller |  | marketplace seller the payment was charged for |
| `payments[].provider_status` | string |  | provider wording / code of the status, untranslated |
| `payments[].status_detail` | string |  | provider reason (rejection code, expiry, ...) |
| `next_cursor` | string |  | absent on the last page |

### `commerce.payments.refund`

refund (fully or partially) a captured payment. Consulta · requiere `Query.Provider` · cardinalidad **one** · tiempo límite por proveedor **30s** · mutante (idempotente).

Entrada:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `provider_ref` | string | sí | provider payment id |
| `amount` | decimal | sí |  |
| `currency` | currency | sí |  |
| `order_ref` | string |  |  |
| `reason` | string |  |  |
| `idempotency_key` | string | sí | required; stable per business operation (reuse it on every retry); the provider must dedupe on it |
| `seller` | Seller |  | marketplace: the seller the payment was charged for |
| `refund_application_fee` | boolean |  | marketplace: also return the platform commission |

Salida:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `refund_id` | string | sí |  |
| `status` | string: pending \| succeeded \| failed \| manual_required | sí | manual_required: cash/BNPL refunds the merchant settles by hand |
| `amount` | decimal | sí |  |
| `currency` | currency | sí |  |
| `instructions` | string |  | what the merchant must do when manual_required |
| `provider_ref` | string |  | provider payment id the refund belongs to |
| `provider_status` | string |  | provider wording / code of the refund status, untranslated |

### `commerce.payments.session`

create a payment session (checkout/preference/intent) for an order. Consulta · cardinalidad **one** · tiempo límite por proveedor **30s** · mutante (idempotente).

Entrada:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `order_ref` | string | sí | order id/number in OPS (external_reference) |
| `amount` | decimal | sí | total to charge |
| `currency` | currency | sí |  |
| `return_url` | string | sí | where the buyer lands after paying |
| `cancel_url` | string |  |  |
| `notify_url` | string |  | webhook URL the host exposes for commerce.payments.confirm |
| `buyer` | Buyer |  |  |
| `methods` | array<string> |  | allowed method families: card / msi / oxxo / spei / wallet / bnpl / bank_transfer / cash |
| `method_hint` | string |  | preselected method family |
| `breakdown` | object |  | amount components (all decimals) |
| `breakdown.subtotal` | decimal |  |  |
| `breakdown.shipping` | decimal |  |  |
| `breakdown.discount` | decimal |  |  |
| `breakdown.tax` | decimal |  |  |
| `shipping_address` | Address |  |  |
| `market` | string |  | market code the order was placed in |
| `lines` | array<Line> |  |  |
| `description` | string |  | statement / title |
| `installments` | integer |  | max interest-free installments (MSI) |
| `expires_at` | string |  | RFC 3339 |
| `idempotency_key` | string | sí | required; stable per business operation (reuse it on every retry); the provider must dedupe on it |
| `metadata` | object |  |  |
| `channel` | string: terminal \| online \| qr \| link \| cash |  | online (default), qr or link; terminals use commerce.payments.terminal.create |
| `seller` | Seller |  | marketplace: charge on behalf of this seller |

Salida:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `session_id` | string | sí | provider session/preference/intent id |
| `status` | string: created \| pending \| requires_action \| authorized \| paid \| failed | sí |  |
| `action_type` | string: redirect \| embed \| voucher \| none |  | what the storefront does next |
| `redirect_url` | string |  | hosted checkout URL |
| `client_secret` | string |  | token for an embedded (iframe) widget |
| `embed_url` | string |  | isolated iframe URL |
| `expires_at` | string |  | RFC 3339 |
| `provider_ref` | string |  | payment id when known already |
| `instructions` | object |  | offline payment instructions (OXXO reference, SPEI CLABE, barcode_url, deadline) |
| `test_mode` | boolean |  | session created against the provider sandbox |
| `qr_data` | string |  | payload to render as a QR (channel qr) |
| `qr_image_url` | string |  | ready-made QR image |

### `commerce.payments.status`

poll a payment's normalized status (return-URL fallback, reconciliation); same output as confirm. Consulta · requiere `Query.Provider` · cardinalidad **one** · tiempo límite por proveedor **8s**.

Entrada:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `provider_ref` | string |  | provider payment/order id |
| `order_ref` | string |  | lookup by external reference when provider_ref is unknown |
| `session_id` | string |  |  |

Salida:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `order_ref` | string | sí | the order_ref sent to commerce.payments.session |
| `status` | string: pending \| requires_action \| authorized \| paid \| failed \| cancelled \| expired \| refunded \| partially_refunded \| at_device \| cancelling | sí |  |
| `amount` | decimal | sí | amount of this status (paid/refunded) |
| `currency` | currency | sí |  |
| `provider_ref` | string | sí | provider payment id |
| `event_id` | string |  | provider event id (dedupe on provider+event_id) |
| `kind` | string: payment \| refund \| dispute |  | default payment |
| `method` | string |  | card / msi / oxxo / spei / wallet / bnpl / bank_transfer / cash |
| `installments` | integer |  |  |
| `paid_at` | string |  | RFC 3339 |
| `occurred_at` | string |  | RFC 3339 |
| `amount_refunded` | decimal |  |  |
| `fee` | decimal |  | provider fee |
| `net_amount` | decimal |  | amount minus fee |
| `test_mode` | boolean |  | event from the provider sandbox |
| `channel` | string: terminal \| online \| qr \| link \| cash |  | how the buyer paid; default online |
| `session_id` | string |  | session / preference / terminal order id the payment belongs to |
| `device_id` | string |  | terminal / cash recycler that took the payment |
| `card` | Card |  | card details when the method is a card |
| `cash` | CashTender |  | cash device details |
| `seller` | Seller |  | marketplace seller the payment was charged for |
| `provider_status` | string |  | provider wording / code of the status, untranslated |
| `status_detail` | string |  | provider reason (rejection code, expiry, ...) |

### `commerce.payments.terminal.create`

send an amount to an in-person device (card terminal, cash recycler); the result is followed with commerce.payments.status. Consulta · requiere `Query.Provider` · cardinalidad **one** · tiempo límite por proveedor **30s** · mutante (idempotente).

Códigos de error del proveedor: `amount_below_minimum`, `device_not_found`, `device_offline`, `device_busy`, `invalid_input`.

Entrada:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `device_id` | string | sí | provider device id (payment_terminals / cash_recyclers record) |
| `order_ref` | string | sí | document id/number in OPS (external_reference) |
| `amount` | decimal | sí | amount to collect |
| `currency` | currency | sí |  |
| `description` | string |  | text shown on the device / statement |
| `installments` | integer |  | interest-free installments; a provider that cannot preset them retries without and says so in message |
| `card_type` | string: credit \| debit \| prepaid \| other |  | preselect credit / debit when the device supports it |
| `expires_in_seconds` | integer |  | order lifetime on the device (provider default when absent) |
| `print_ticket` | boolean |  | ask the device to print its voucher |
| `idempotency_key` | string | sí | required; stable per business operation (reuse it on every retry); the provider must dedupe on it |
| `metadata` | object |  |  |

Salida:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `provider_ref` | string | sí | provider terminal order / payment id; pin follow-ups to it |
| `status` | string: pending \| at_device \| failed | sí | pending (queued), at_device (shown on the device) or failed |
| `device_id` | string |  |  |
| `expires_at` | string |  | RFC 3339 |
| `installments` | integer |  | installments actually preset (0/1 when the provider dropped them) |
| `message` | string |  | provider message (e.g. installments not accepted, retried without) |
| `test_mode` | boolean |  |  |

### `commerce.payments.terminal.devices`

list the provider account's in-person devices (to link a payment_terminals record without typing ids). Consulta · requiere `Query.Provider` · cardinalidad **one** · tiempo límite por proveedor **8s**.

Entrada:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `device_id` | string |  | only this device |

Salida:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `devices` | array<object> | sí |  |
| `devices[].device_id` | string | sí |  |
| `devices[].name` | string |  |  |
| `devices[].model` | string |  |  |
| `devices[].mode` | string |  | provider operating mode (e.g. integrated / standalone) |
| `devices[].status` | string: online \| offline \| busy \| unknown |  |  |
| `devices[].test_mode` | boolean |  |  |

### `commerce.search.query`

product search / suggestions (hot path: providers should answer from an index, not per-keystroke WASM). Consulta · cardinalidad **one** · tiempo límite por proveedor **2s**.

Entrada:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `q` | string |  | free text; empty = browse |
| `filters` | object |  | facet -> value(s) |
| `sort` | string |  | relevance / price_asc / price_desc / newest / … |
| `limit` | integer |  |  |
| `cursor` | string |  |  |
| `locale` | string |  |  |
| `collection` | string |  | restrict to a collection/category handle |
| `suggest` | boolean |  | type-ahead mode |

Salida:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `hits` | array<object> | sí |  |
| `hits[].id` | string | sí | product id |
| `hits[].handle` | string |  |  |
| `hits[].title` | string |  |  |
| `hits[].variant_id` | string |  |  |
| `hits[].score` | number |  |  |
| `hits[].highlight` | object |  | field -> highlighted snippet |
| `total_hits` | integer |  | total matches (may be an estimate) |
| `next_cursor` | string |  |  |
| `facets` | array<object> |  |  |
| `facets[].field` | string | sí |  |
| `facets[].values` | array<object> | sí |  |
| `facets[].values[].value` | string | sí |  |
| `facets[].values[].count` | integer | sí |  |
| `suggestions` | array<string> |  | query completions |

### `commerce.shipping.cancel`

void a label / cancel a shipment not yet picked up. Consulta · requiere `Query.Provider` · cardinalidad **one** · tiempo límite por proveedor **8s**.

Entrada:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `shipment_id` | string | sí |  |
| `tracking_number` | string |  |  |
| `order_ref` | string |  |  |
| `reason` | string |  |  |

Salida:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `status` | string: cancelled \| rejected | sí |  |
| `refund_amount` | decimal |  | label cost credited back |
| `currency` | currency |  |  |
| `message` | string |  | why it could not be cancelled |

### `commerce.shipping.label`

buy a label for a quoted rate. Consulta · cardinalidad **one** · tiempo límite por proveedor **30s** · mutante (idempotente).

Entrada:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `rate_id` | string | sí | id of a commerce.shipping.rates rate from this provider |
| `order_ref` | string | sí |  |
| `origin` | Address |  |  |
| `destination` | Address |  |  |
| `parcels` | array<Parcel> |  |  |
| `items` | array<ShipItem> |  |  |
| `declared_value` | decimal |  |  |
| `currency` | currency |  |  |
| `label_format` | string: pdf \| zpl \| png |  |  |
| `label_size` | string |  | 4x6 / letter |
| `cod_amount` | decimal |  |  |
| `idempotency_key` | string | sí | stable per shipment; a duplicate label costs money |

Salida:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `label_status` | string: created \| label_pending | sí | label_pending resolves by webhook/poll; tracking_number and label_url are set once created |
| `carrier` | string | sí |  |
| `tracking_number` | string |  |  |
| `label_url` | string |  |  |
| `shipment_id` | string |  |  |
| `label_format` | string |  |  |
| `tracking_url` | string |  |  |
| `cost` | decimal |  |  |
| `currency` | currency |  |  |
| `status` | string: created \| picked_up \| in_transit \| out_for_delivery \| delivered \| exception \| returned \| cancelled |  | tracking status |

### `commerce.shipping.rates`

quote delivery options; every eligible carrier addon answers in parallel. Consulta · cardinalidad **all** · tiempo límite por proveedor **4s**.

Entrada:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `origin` | Address | sí |  |
| `destination` | Address | sí |  |
| `parcels` | array<Parcel> | sí |  |
| `currency` | currency | sí | currency of declared_value and of the wanted rates |
| `declared_value` | decimal |  | insured / declared value |
| `cod_amount` | decimal |  | cash on delivery to collect |
| `items` | array<ShipItem> |  |  |
| `order_ref` | string |  | cart/order id |

Salida:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `rates` | array<object> | sí |  |
| `rates[].id` | string | sí | opaque rate id, passed to commerce.shipping.label |
| `rates[].carrier` | string | sí | e.g. fedex, estafeta, dhl, local |
| `rates[].service` | string | sí | human service name |
| `rates[].cost` | decimal | sí | price to the buyer |
| `rates[].currency` | currency | sí |  |
| `rates[].service_code` | string |  |  |
| `rates[].eta_days_min` | integer |  |  |
| `rates[].eta_days_max` | integer |  |  |
| `rates[].pickup` | boolean |  | store/branch pickup option |
| `rates[].delivery_type` | string: door \| branch \| same_day \| on_demand \| pickup |  |  |
| `rates[].taxes_included` | boolean |  | cost already includes VAT/IVA |
| `rates[].expires_at` | string |  | RFC 3339 |

### `commerce.shipping.track`

normalized tracking of a shipment. Consulta · requiere `Query.Provider` · cardinalidad **one** · tiempo límite por proveedor **8s**.

Entrada:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `tracking_number` | string | sí |  |
| `carrier` | string |  |  |
| `shipment_id` | string |  |  |

Salida:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `status` | string: created \| picked_up \| in_transit \| out_for_delivery \| delivered \| exception \| returned \| cancelled | sí |  |
| `carrier` | string |  |  |
| `tracking_url` | string |  |  |
| `eta` | string |  | RFC 3339 date |
| `delivered_at` | string |  | RFC 3339 |
| `events` | array<TrackingEvent> |  | newest first |

### `commerce.tax.calculate`

compute taxes for cart lines and shipping. Consulta · cardinalidad **one** · tiempo límite por proveedor **8s**.

Entrada:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `currency` | currency | sí |  |
| `lines` | array<Line> | sí |  |
| `ship_to` | Address |  |  |
| `ship_from` | Address |  |  |
| `shipping_amount` | decimal |  | shipping charged to the buyer |
| `prices_include_tax` | boolean |  | unit prices already include tax |
| `customer` | Buyer |  |  |
| `order_ref` | string |  |  |

Salida:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `currency` | currency | sí |  |
| `tax_included` | boolean | sí | true when line prices already contain the tax |
| `total_tax` | decimal | sí |  |
| `lines` | array<object> | sí |  |
| `lines[].id` | string | sí | input line id |
| `lines[].tax` | decimal | sí | tax amount of the line |
| `lines[].taxable_amount` | decimal |  |  |
| `lines[].breakdown` | array<TaxLine> |  |  |
| `shipping_tax` | decimal |  |  |
| `calculation_id` | string |  | provider calculation id, passed to commerce.tax.commit |

### `commerce.tax.commit`

record the taxes of a paid order with the provider (no-op for stateless providers). Consulta · requiere `Query.Provider` · cardinalidad **one** · tiempo límite por proveedor **30s** · mutante (idempotente).

Entrada:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `order_ref` | string | sí |  |
| `calculation_id` | string |  |  |
| `currency` | currency |  |  |
| `lines` | array<Line> |  |  |
| `total_tax` | decimal |  |  |
| `ship_to` | Address |  |  |
| `paid_at` | string |  | RFC 3339 |
| `idempotency_key` | string | sí | required; stable per business operation (reuse it on every retry); the provider must dedupe on it |

Salida:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `status` | string: committed \| skipped | sí |  |
| `transaction_id` | string |  |  |

### `commerce.tax.reverse`

reverse (fully or partially) a committed tax transaction after a refund/return. Consulta · requiere `Query.Provider` · cardinalidad **one** · tiempo límite por proveedor **30s** · mutante (idempotente).

Entrada:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `order_ref` | string | sí |  |
| `transaction_id` | string |  |  |
| `lines` | array<Line> |  | partial reversal; absent = full |
| `amount` | decimal |  |  |
| `currency` | currency |  |  |
| `reason` | string |  |  |
| `idempotency_key` | string | sí | required; stable per business operation (reuse it on every retry); the provider must dedupe on it |

Salida:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `status` | string: reversed \| skipped | sí |  |
| `transaction_id` | string |  |  |

### `fiscal.invoice.cancel`

cancel an issued fiscal invoice. Consulta · requiere `Query.Provider` · cardinalidad **one** · tiempo límite por proveedor **8s**.

Entrada:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `invoice_id` | string | sí |  |
| `reason` | string |  | e.g. SAT motivo 01 / 02 / 03 / 04 |
| `substitution_id` | string |  | replacing invoice fiscal_uuid (motivo 01) |

Salida:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `status` | string: cancelled \| pending \| rejected | sí |  |
| `message` | string |  |  |

### `fiscal.invoice.create`

issue a fiscal invoice for an order (CFDI 4.0 in MX via the fiscal_mexico family). Consulta · cardinalidad **one** · tiempo límite por proveedor **30s** · mutante (idempotente).

Entrada:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `order_ref` | string | sí |  |
| `currency` | currency | sí |  |
| `customer` | object | sí |  |
| `customer.tax_id` | string | sí | RFC / VAT id |
| `customer.name` | string | sí | legal name |
| `customer.email` | string |  |  |
| `customer.postal_code` | string |  | fiscal domicile postal code |
| `customer.tax_regime` | string |  | e.g. SAT régimen fiscal |
| `customer.use` | string |  | e.g. SAT uso CFDI |
| `customer.country` | country |  |  |
| `lines` | array<object> | sí |  |
| `lines[].id` | string | sí |  |
| `lines[].description` | string | sí |  |
| `lines[].quantity` | decimal | sí | decimal: fiscal quantities can be fractional |
| `lines[].unit_price` | decimal | sí | before tax |
| `lines[].discount` | decimal |  |  |
| `lines[].product_code` | string |  | fiscal product key (SAT ClaveProdServ) |
| `lines[].unit_code` | string |  | fiscal unit key (SAT ClaveUnidad) |
| `lines[].sku` | string |  |  |
| `lines[].taxes` | array<TaxLine> |  |  |
| `payment_form` | string |  | e.g. SAT forma de pago (04, 03, 01) |
| `payment_method` | string |  | e.g. PUE / PPD |
| `series` | string |  |  |
| `subtotal` | decimal |  |  |
| `total` | decimal |  |  |
| `issued_at` | string |  | RFC 3339 |
| `notes` | string |  |  |
| `idempotency_key` | string | sí | required; stable per business operation (reuse it on every retry); the provider must dedupe on it |

Salida:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `invoice_id` | string | sí | provider id |
| `status` | string: issued \| pending \| failed | sí |  |
| `fiscal_uuid` | string |  | folio fiscal (CFDI UUID) |
| `series` | string |  |  |
| `folio` | string |  |  |
| `total` | decimal |  |  |
| `currency` | currency |  |  |
| `pdf_url` | string |  |  |
| `xml_url` | string |  |  |
| `message` | string |  | authority/provider message when failed |

## Pendiente / decisiones abiertas

- `commerce.payments.confirm` devuelve una sola actualización normalizada. Los
  webhooks de temas ajenos los rechaza el proveedor con error; la forma del
  acuse por proveedor (Kueski/Aplazo/Openpay) y varias actualizaciones por
  webhook quedan para el contrato de webhooks entrantes (K3).
- `commerce.shipping.label` con `label_status = created` debe traer
  `tracking_number` y `label_url`; hoy es convención documentada, no se valida
  condicionalmente.
- Retirar los nombres heredados `shipping.tracking_updated`,
  `shipping.label_ready` y `emissary.shipment_status_changed` cuando ningún
  suscriptor los use (hoy solo se emiten por compatibilidad).
- Sin contrato aún: `commerce.shipping.pickup`, `commerce.fulfillment.inventory.sync`,
  `marketing.events`, `marketing.feed`.
- Función de host WASM para que un addon invitado llame `Querier.Query`
  (`capability_query`): pendiente en `runtime/wasm`.
