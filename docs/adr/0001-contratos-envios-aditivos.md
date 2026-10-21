# ADR 0001: contratos de envíos, evolución aditiva de `commerce.shipping.rates` y `commerce.shipping.label`

- Estado: aceptado (carril 0-E4 de la ola 0 del diseño del sistema de envíos).
- Fecha: 2026-10-20
- Paquete: `capability`. Versión propuesta: **v0.226.0** (minor, aditiva).
- Referencia: `DISENO-SISTEMA-DE-ENVIOS-2026-10-20.md` (repo ops), §3.2, §4.8, §5.1, §8.2; `docs/capabilities-commerce.md`.

## Contexto

El carrito sabe cotizar bien (proveedor, tarifa, transportista, servicio, plazo, si es gratis y por qué), pero el contrato del kernel solo transportaba `cost`, el plazo en días y el tipo de entrega. Sin costo de lista, motivo de gratuidad, fecha prometida, seguro, origen ni vigencia, el pedido no puede guardar el envío como un hecho estructurado ni comprar la guía con la tarifa elegida, y cada paquetería inventa campos propios (`x_...`).

## Decisión

1. **Las llaves no cambian ni llevan sufijo** (`commerce.shipping.rates`, `commerce.shipping.label`; `...@1` no es una llave válida para `ValidKey`). El diseño habla de `rates@1` como la versión actual del contrato, no como llave. La evolución es la del pago integrado (`capability/payments_embedded.go`): **solo** campos opcionales de entrada y de salida, valores de enum añadidos al final y formas compartidas nuevas. No se quita, renombra, cambia de tipo ni se vuelve requerido nada.
2. **El kernel fija el vocabulario; el carrito decide el precio.** Un proveedor solo puede contestar los motivos de gratuidad que le corresponden. `coupon`, `market_policy` y `shop_threshold` son decisiones del carrito (paso 5 del orden de precios) y el esquema los rechaza en la salida de un proveedor.
3. **La foto inmutable de la cotización es del anfitrión.** El kernel solo define el formato del hash (`sha256:` + 64 hex en minúsculas, `QuoteHashPattern`) y dónde viaja; no define el JSON de la foto.
4. **Fuera de este ADR** (ola 2, carril 2-E6, dependen de otros carriles del kernel): la capacidad `commerce.shipments.status`, el evento `commerce.shipments.updated@1`, el refuerzo de `EmitPolicy` para `commerce.shipments` y el dominio `shipping` de `incidents`. No se tocan `commerce.shipping.tracking_updated` ni `commerce.fulfillment.updated`.

## Campos nuevos (todos opcionales)

`commerce.shipping.rates`, entrada:

| Campo | Tipo | Para qué |
|---|---|---|
| `market` | string | mercado de la cotización |
| `subtotal_basis` | decimal | subtotal después de descuentos y cupones: base única de «gratis desde» (`declared_value` sigue siendo el valor asegurado) |
| `origins[]` | array de Address + `location_id` (requerido dentro) | orígenes candidatos (multiorigen); `origin` sigue siendo el principal |
| `delivery_types[]` | array de `DeliveryTypes` | filtro de tipos de entrega |
| `origin_configured` | boolean | el anfitrión no tiene origen configurado |
| `items[].weight_kg`, `items[].length_cm`, `items[].width_cm`, `items[].height_cm` | decimal | peso y medidas por unidad (en `ShipItemSchema`, compartido con `label` y `fulfillment.create`) |
| `items[].requires_shipping` | boolean | falso: no suma peso ni requiere entrega |

`commerce.shipping.rates`, salida (`rates[]`):

| Campo | Tipo | Para qué |
|---|---|---|
| `list_cost` | decimal | precio antes de gratis o descuento del proveedor (`>= cost`) |
| `free_reason` | enum `ShippingRateFreeReasons`: `none`, `rate_free`, `rate_free_over`, `staff_override`, `pickup_free` | motivo de gratuidad del proveedor |
| `rule_ref` | `{kind, id, version?}` | regla que produjo el precio |
| `delivery_window` | `{from, to}` (`YYYY-MM-DD`) | fechas prometidas |
| `pickup_point` | `{id, name, address?, hours?, lat?, lng?}` | punto de recogida |
| `insurance` | `{available?, included?, cost?}` | seguro |
| `fallback` | boolean | tarifa de respaldo |
| `quote_ref` | string | id de cotización del proveedor para la guía |
| `quoted_at` | string RFC 3339 | cuándo calculó el precio (`expires_at` ya existía) |
| `quote_hash` | string `sha256:<64 hex>` | huella del proveedor de lo cotizado |
| `origin_location_id` | string | origen del que sale |
| `billable_weight_kg` | decimal | peso facturable |
| `parcels[]` | array de `Parcel` | paquetes cotizados (peso y medidas) |

Ya existían y no cambian: `service`, `service_code`, `carrier`, `eta_days_min/max`, `delivery_type`, `pickup`, `taxes_included`, `expires_at`.

`commerce.shipping.label`, entrada: `purpose` (`outbound`|`return`), `shipment_ref`, `insurance_value` (decimal), `quote_ref`, `quote_hash`, `origin_location_id`.
`commerce.shipping.label`, salida: `cost_tax`, `estimated_delivery` (`YYYY-MM-DD`), `insurance_cost`, `billable_weight_kg`, `parcels[]`.

Listas y formas nuevas exportadas: `DeliveryTypes` (los valores que ya aceptaba el contrato), `ShippingFreeReasons` (los 8 valores del pedido: `none`, `rate_free`, `rate_free_over`, `market_policy`, `shop_threshold`, `coupon`, `staff_override`, `pickup_free`), `ShippingRateFreeReasons`, `ShippingPurposes`, `DatePattern`, `QuoteHashPattern`, `ShippingOriginSchema`, `ShippingRuleRefSchema`, `DeliveryWindowSchema`, `PickupPointSchema`, `RateInsuranceSchema`.

## Consecuencias

- **Retrocompatibilidad.** Una entrada, una salida y una guía del esquema anterior siguen validando (prueba `TestShippingLegacyShapesStillValid`); ningún campo anterior cambia de tipo ni de obligatoriedad (`TestShippingEvolutionIsAdditiveOnly`).
- **Selección y fan-out sin cambios.** `rates` sigue siendo fan-out con plazo de 4 s; `label` sigue siendo de un solo proveedor, idempotente. Un proveedor viejo y uno nuevo conviven en la misma consulta; el que devuelve un campo nuevo mal tipado se aísla como `invalid_output`.
- **Lo que el esquema no puede expresar** (`list_cost >= cost`, `cost >= 0`, `delivery_window.from <= to`, fechas de calendario reales) lo verifica la suite de contrato de los proveedores (`shared/shipspec/contracttest` en `addons`), no el kernel.
- **Entradas abiertas.** Como todas las entradas del paquete, las de estos contratos aceptan campos no declarados y no exigen los requeridos anidados; la forma de `origins[]` se valida con `ShippingOriginSchema`.
- **Consumidores.** Los proveedores adoptan los campos cuando quieran (`shipping_flat` primero, 0-E2); los anfitriones los ignoran hasta entonces.
- **Riesgo conocido.** Añadir campos a `ShipItemSchema` los hace visibles también en `fulfillment.create` y en la entrada de `label`; es aditivo y opcional en ambos.
