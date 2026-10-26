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
`payments.confirm`, `payments.status`, `payments.refund`, `payments.submit`,
`payments.client`, `payments.notification.inspect`, `payments.notification.resolve`,
`payments.health`, `shipping.cancel`,
`shipping.track`, `fulfillment.cancel`, `fiscal.invoice.cancel`, `tax.commit` y
`tax.reverse` (`Contract.PinRequired`). `fiscal.invoice.status|document` **no** se fijan: se resuelven por mercado (ver «Lectura de facturas»). Sin `Provider`, `Engine.Query` responde
`ErrProviderRequired` sin invocar a nadie. El proveedor fijado debe estar
instalado y habilitado en la organización y atender los países del mercado
(`SelectPinned`); si no, `ErrNoProvider`. La selección por mercado, el
proveedor por omisión y la ambigüedad no intervienen.

Reglas para el host:

- Guarda la llave del addon en el registro del pago, del envío, de la factura y de la transacción de impuestos al crearlos, y úsala en todo seguimiento y reembolso aunque el proveedor por omisión o la selección del mercado hayan cambiado.
- Las rutas de webhook llevan el proveedor en la ruta (por ejemplo `/webhooks/payments/{addon}`) y lo pasan como `Query.Provider`; el `notify_url` que se envía en `payments.session` ya incluye esa ruta. Un evento de Stripe nunca llega al proveedor por omisión.

## Operaciones mutantes: idempotencia obligatoria

`payments.session`, `payments.submit`, `payments.refund`, `shipping.label`, `fulfillment.create`,
`fiscal.invoice.create`, `tax.commit` y `tax.reverse` mueven dinero o tienen
efectos irreversibles (`Contract.Idempotent`):

- `idempotency_key` es **requerida** (cadena no vacía, estable por operación de negocio, p. ej. `ship-{order_ref}`); el Executor rechaza la llamada con `ErrIdempotencyRequired` sin invocar. Se entrega al proveedor en la entrada mapeada y en `Call.IdempotencyKey`; el proveedor debe deduplicar con ella y devolver el resultado original en un reintento.
- Plazo por omisión **30 s** (`MutatingTimeout`), no 8 s.
- Si vence el plazo (o el llamador cancela con la llamada en vuelo) el resultado es **indeterminado**: el error envuelve `ErrTimeout` (o el error del contexto) y `ErrIndeterminate`, `Result.Indeterminate()` es `true` y el sobre JSON lleva `"indeterminate": true`. La llamada al proveedor puede seguir y terminar con éxito. El host no reintenta con otra llave ni da la operación por fallida: concilia con `payments.status` / `shipping.track` o reintenta con la **misma** llave.

## Pago integrado (flujo embebido): `session.flow`, `client` y `submit`

El comprador paga **en la página de la tienda**, sin salir a la página del
proveedor. Es aditivo: una tienda que no manda `flow` obtiene exactamente lo de
siempre (redirección), y un proveedor que no declara `client` ni `submit` sigue
sirviendo solo ese flujo.

Secuencia (`flow = embedded`):

1. **Pintar.** Antes de crear nada, el host llama `commerce.payments.client` **a cada proveedor candidato por su nombre** (`Query.Provider`; no es fan-out: una llamada por proveedor, cada una con su plazo). La salida dice qué opciones existen (`card`, `cash`, `wallet`, `credits`), cuáles salen de la página (`redirect: true`), la `public_key` y el `sdk_url` del SDK del proveedor, los puntos de efectivo, el tope de cuotas, los orígenes para la CSP (`csp`) y el mercado y modo (`market`, `test_mode`). Solo datos **públicos**: el contrato no tiene ningún campo para un secreto.
2. **Crear la sesión.** `commerce.payments.session` con `flow = embedded` (y `method_hint` = la opción elegida). La selección del proveedor es la de siempre: el elegido para ese mercado, luego el de la organización, luego el único. El proveedor contesta con `flow` (el que **realmente** atendió; ausente = `redirect`, un proveedor anterior al flujo) y `client`. `instructions.brick` y `redirect_url` se conservan, y sirven de respaldo si el SDK no carga.
3. **Cobrar.** `commerce.payments.submit`, **fijado** al proveedor dueño de la sesión (`PinRequired`) e **idempotente**. El navegador solo aporta `provider_data` (lo que el SDK del proveedor le entregó: token de tarjeta, id del método, emisor, cuotas, punto de efectivo; nunca un número de tarjeta). **El monto y la moneda los pone el host** desde su propio documento; el proveedor rechaza (`price_mismatch`) cualquier otro monto que venga dentro de `provider_data`, valida el método y las cuotas contra la configuración del mercado (`method_not_allowed`, `installments_exceeded`) y usa la configuración del `market` recibido.

Salida de `submit`: la de `confirm` (`status`, `provider_ref`, `amount`, `currency`, `status_detail`…) más:

| Campo | Qué hace la tienda |
|---|---|
| `action.type = none` | nada: lee `status` |
| `action.type = challenge` | muestra el reto del banco (3DS) **dentro** de la página con `action.challenge`, y sigue con `payments.status` |
| `action.type = voucher` | muestra la ficha (`action.voucher`: punto, referencia, código de barras, CLABE, vencimiento) |
| `action.type = redirect` | abre `action.redirect_url` |
| `decline_code` | motivo neutro de un rechazo (`DeclineCodes`): la tienda tiene el texto; nunca se muestra el código crudo del proveedor |
| `retryable` | el comprador puede intentar de nuevo en el mismo checkout |

Reglas:

- **Idempotencia.** `idempotency_key` es obligatoria y es **una por intento** (no por checkout): un reintento del mismo intento (red caída, doble clic) reusa la llave y obtiene la respuesta original sin cobrar dos veces; otra tarjeta es otro intento y lleva otra llave. Como todo contrato mutante: plazo de 30 s, y un plazo vencido es **indeterminado** (se concilia con `payments.status`, no se reintenta con otra llave).
- **Sin cobro confirmado no hay pedido.** La respuesta de `submit` es una lectura del proveedor (`PaymentEventSources` gana `submit`), pero un `pending`, un `requires_action` o una ficha **no** son un cobro: solo `paid` (verificado como cualquier `confirm`) cierra el pago. Los avisos posteriores (webhook, barrido) siguen siendo la red de seguridad.
- **Política de eventos de dinero, sin cambios.** `commerce.payments.confirmed` lo siguen emitiendo solo quienes proveen alguna capacidad `commerce.payments.*`; los contratos nuevos están en ese mismo dominio y no abren otro camino de emisión.
- **Evolución.** La llave no lleva sufijo de versión (el registro no versiona por llave: `@1` no es una llave válida). Un contrato evoluciona **solo** con campos opcionales de entrada o salida, valores de enum añadidos al final y contratos nuevos; nunca se quita, renombra, cambia de tipo ni se vuelve requerido un campo. Así un proveedor construido contra la forma de hoy sigue validando.

### Salud y notificaciones de pago

`commerce.payments.notification.inspect` (pura: sin red ni escritura) dice si la firma de un aviso es válida (`firma_ok`, `firma_invalida`, `sin_firma`, `sin_secreto`) y qué recurso nombra. `commerce.payments.notification.resolve` lee ese recurso **por id** en la API del proveedor (el cuerpo del aviso nunca se cree) y responde la salida de `status`; valida que el id tenga la forma de los del proveedor (`invalid_input` si no, `not_found` si no existe). `commerce.payments.health` devuelve el autodiagnóstico del conector (`checks[]` con `key`, `ok`, `level`, `message`), sin revelar secretos. Los tres van fijados al proveedor de la ruta del webhook. El diario de avisos, los reintentos y las incidencias viven en el paquete `payments/inbox`, no en `capability`.

## Cotización y guía de envíos: `rates` y `label` (aditivo)

Los dos contratos conservan su llave (`commerce.shipping.rates`, `commerce.shipping.label`; el sufijo `@1` no es una llave válida) y evolucionan **solo** con campos opcionales, como el pago integrado. Un proveedor construido contra la forma anterior (`envia`, `skydropx`, `noventa9minutos`, `shipping_flat`) sigue validando sin cambios, y el host puede ignorar todo campo nuevo. Los eventos `commerce.shipping.tracking_updated` y `commerce.fulfillment.updated` no cambian. Decisión y lista exacta de campos: `docs/adr/0001-contratos-envios-aditivos.md`.

Reparto de responsabilidades (el kernel solo fija el vocabulario):

- **El proveedor** responde el precio de **lista** (`list_cost`), su propio motivo de gratuidad (`free_reason`: `none`, `rate_free`, `rate_free_over`, más `staff_override` y `pickup_free`), la regla que lo produjo (`rule_ref`), el plazo (`eta_days_min/max`, y `delivery_window` si sabe las fechas), el punto de recogida, el seguro, los paquetes y el peso facturable que cotizó, y la vigencia (`quoted_at`, `expires_at`).
- **El carrito** decide el precio final: los motivos `coupon`, `market_policy` y `shop_threshold` **no** los puede contestar un proveedor (el esquema los rechaza en `rates[].free_reason`); `ShippingFreeReasons` es la enumeración cerrada completa que el pedido registra (precedencia: cupón, política de mercado, umbral de tienda, tarifa con «gratis desde», tarifa gratis). También congela la foto inmutable de la cotización; `quote_hash` de la guía (`sha256:<64 hex>`) es el hash de esa foto y el proveedor, como mucho, lo registra.
- **Base de «gratis desde».** `subtotal_basis` es el subtotal después de descuentos y cupones; el proveedor lo usa (y no `declared_value`, que es el valor asegurado) para su regla de «gratis desde». Ausente = el comportamiento anterior.
- **Tipo de entrega.** `DeliveryTypes` (`door`, `branch`, `same_day`, `on_demand`, `pickup`) es el vocabulario del kernel; los anfitriones traducen a él (`home` y `pickup_point` no son valores del kernel). `delivery_types` en la entrada filtra.
- **Dinero y fechas.** Todo importe es `decimal` en la moneda de la tarifa (nunca número). Las fechas prometidas son `YYYY-MM-DD`; los instantes, RFC 3339. `list_cost >= cost` y `cost >= 0` son invariantes que el esquema no puede expresar: las verifica la suite de contrato de los proveedores.
- **Selección, sin cambios.** `rates` es fan-out (todos los proveedores que sirven el mercado, o el subconjunto que la organización eligió para él; plazo de 4 s por proveedor; uno inválido o lento se aísla y el resto responde). `label` es de un solo proveedor (el elegido para el mercado, luego el predeterminado de la organización, luego el único), idempotente, y el host lo dirige al proveedor de la tarifa elegida (`rate_id` es de ese proveedor).
- **Identificadores opacos.** `rates[].id` y `quote_ref` se pasan tal cual; nunca se parten por `:`.

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

## Lectura de facturas: `fiscal.invoice.status` y `fiscal.invoice.document`

Lado de **lectura** de la facturación, para la cuenta del cliente (ADR 0005). La
cuenta no conoce al PAC: lee estado y descarga archivos de forma normalizada, y
qué proveedor contesta (factura.com hoy, otro PAC u otro país mañana) es la
selección habitual de `Select`.

| Regla | Detalle |
|---|---|
| Tipo y ruteo | `query`, cardinalidad `one`, **sin** `Query.Provider` (no son de seguimiento fijado): `Select` por mercado/país, `Provider.Priority` y `StatusNotConfigured` igual que `commerce.shipping.*`. Sin proveedor que atienda el mercado (p. ej. EE. UU.) = `ErrNoProvider`: la cuenta responde «facturación no disponible». |
| Dominio y permiso | Dominio `fiscal.invoice`. El addon invocador (el de cuenta del cliente) declara `capability:query fiscal.invoice.status` y `capability:query fiscal.invoice.document` (o el glob `fiscal.invoice.*`); el proveedor los declara en `provides_capabilities`. Ningún campo nuevo de manifest v3. |
| Privacidad | La organización es la **de la invocación** (`Query.Org`, la pone el host); no hay `org` en la entrada. El proveedor DEBE comprobar que el documento pertenece a `order_ref` y a esa organización; si no, `not_found` (nunca distingue «no existe» de «es de otro»). El addon de cuenta, a su vez, solo pasa un `order_ref` que ya comprobó que es del cliente autenticado. |
| Sin URLs del PAC | La salida de `status` no tiene campos de URL. `document` devuelve bytes o una URL **relativa** firmada (empieza con un solo `/`, validado por patrón) con caducidad ≤ 15 min (`InvoiceDocumentMaxExpiry`); nunca una URL del PAC. Las extensiones `x_` son del proveedor y el consumidor no las reenvía. |
| Límites | Archivo decodificado ≤ 5 MiB (`InvoiceDocumentMaxBytes`); `size_bytes` debe coincidir. `CheckInvoiceDocument(raw, now)` valida lo que el esquema no expresa (exactamente uno de `content_base64`/`url`, tamaño, `content_type` según `kind`, caducidad). El host la aplica tras `ValidateOutput`. |
| Errores (`Result.ErrorCode()`) | `not_found` (orden/documento desconocido o ajeno), `not_ready` (aún sin timbrar o sin ese archivo: reintentar luego), `provider_unavailable` (el PAC no contesta), `not_configured` (instalado sin credenciales; el resolvedor da el mismo código vía `ErrNotConfigured` si todos los candidatos lo están). Más `timeout`/`invalid_output` genéricos. |
| Modo de prueba | `test_mode` y `environment` (`sandbox`/`production`): un documento de sandbox no es un documento fiscal válido y la UI no lo presenta como tal. |
| Notas de crédito | Misma lista: `kind = credit_note` y `related_to` = `document_id` de su factura. |

Contrato de esquema fijado por `capability/testdata/fiscal_invoice_read.golden.json`
(solo aditivo; cambiarlo exige `go test ./capability -run Golden -update` y revisión).

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
| `weight_kg` | decimal |  | weight of ONE unit, kg (quote and label by item; parcels stay the source of truth when present) |
| `length_cm` | decimal |  | length of one unit, cm |
| `width_cm` | decimal |  | width of one unit, cm |
| `height_cm` | decimal |  | height of one unit, cm |
| `requires_shipping` | boolean |  | false: a service / gift card that adds no weight and needs no delivery (default true) |

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

### `commerce.payments.client`

what this provider declares so the storefront paints its payment in the page (options, public key, SDK, CSP origins, market, mode); read only, no secrets. Consulta · requiere `Query.Provider` · cardinalidad **one** · tiempo límite por proveedor **8s**.

Entrada:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `market` | string | sí | market code |
| `currency` | currency | sí |  |
| `amount` | decimal |  | cart total, when the options depend on it (installments) |
| `locale` | string |  | preferred locale (es-MX) |

Salida:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `kind` | string | sí | storefront adapter that knows the provider SDK (e.g. mercadopago_bricks, stripe_elements) |
| `sdk_url` | string |  | SDK script to load, https; absent when the options need no script |
| `public_key` | string |  | public key / publishable identifier of the SDK |
| `locale` | string |  | locale the SDK is painted in (es-MX) |
| `market` | string |  | market code the answer is for |
| `options` | array<object> | sí | payment options, in the order to paint them |
| `options[].key` | string: card \| cash \| wallet \| credits | sí | which option |
| `options[].family` | string |  | method family as in commerce.payments.capabilities methods (card, msi, oxxo, spei, wallet, bnpl...) |
| `options[].redirect` | boolean | sí | true: the option leaves the page (create the session with flow redirect and method_hint = key) |
| `options[].label` | string |  | provider default label; the storefront owns the final text |
| `options[].max_installments` | integer |  | card: highest installment count offered (the provider re-checks it on submit) |
| `options[].points` | array<object> |  | cash: selectable points |
| `options[].points[].id` | string | sí | stable id of the row the buyer picks |
| `options[].points[].payment_method_id` | string | sí | provider method id sent back in submit provider_data |
| `options[].points[].payment_point` | string |  | provider point id when several points share one method id; absent when the method is the point |
| `options[].points[].label` | string | sí | name shown to the buyer |
| `options[].points[].logo_url` | string |  |  |
| `options[].points[].group` | string: main \| more |  | main (default): first list; more: behind the 'more' toggle |
| `options[].label_max` | string |  | credits: the provider's own wording of the maximum term, when it knows it |
| `csp` | object |  | origins the SDK needs |
| `csp.script` | array<string> |  | script-src origins |
| `csp.frame` | array<string> |  | frame-src origins |
| `csp.connect` | array<string> |  | connect-src origins |
| `csp.img` | array<string> |  | img-src origins |
| `csp.form` | array<string> |  | form-action origins |
| `test_mode` | boolean |  | the public key belongs to the provider sandbox |

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
| `also_captured` | array<string> |  | provider refs of OTHER captured payments of the same order_ref (double charge); the one answered stays in provider_ref |

### `commerce.payments.health`

connector self-diagnosis (credentials, mode, webhook secret, notification URL) without revealing any secret. Consulta · requiere `Query.Provider` · cardinalidad **one** · tiempo límite por proveedor **8s**.

Entrada:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `market` | string |  | market code, when the configuration differs per market |

Salida:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `checks` | array<object> | sí |  |
| `checks[].key` | string | sí | stable check id (credentials, mode, webhook_secret, notification_url...) |
| `checks[].ok` | boolean | sí |  |
| `checks[].level` | string: info \| warning \| critical |  | severity when ok is false (default warning) |
| `checks[].message` | string |  | what is wrong or how to fix it; never a secret |
| `test_mode` | boolean |  | the configured credentials are sandbox credentials |

### `commerce.payments.notification.inspect`

read a provider notification WITHOUT network or writes: verdict on its signature and which resource it names; the host calls it right after persisting the notification. Consulta · requiere `Query.Provider` · cardinalidad **one** · tiempo límite por proveedor **8s**.

Entrada:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `method` | string | sí | HTTP method of the notification |
| `query` | object |  | query-string parameters |
| `headers` | object |  | request headers, lowercase keys |
| `raw_body_b64` | string |  | exact body bytes, base64 (the signature may cover them) |

Salida:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `auth` | string: firma_ok \| firma_invalida \| sin_firma \| sin_secreto | sí | signature verdict |
| `auth_reason` | string |  | why (signature_mismatch, timestamp_out_of_tolerance, matched_previous...) |
| `resource_kind` | string |  | what the notification names (payment, refund, dispute...) |
| `resource_id` | string |  | id of that resource at the provider |
| `event_id` | string |  | provider event id (dedupe) |
| `livemode` | boolean |  | false = sandbox notification |

### `commerce.payments.notification.resolve`

read the resource a notification names through the provider API, by id (the notification body is never trusted); same output as commerce.payments.status; the host rate-limits and dedupes it when auth is not firma_ok. Consulta · requiere `Query.Provider` · cardinalidad **one** · tiempo límite por proveedor **8s**.

Códigos de error del proveedor: `invalid_input`, `not_found`.

Entrada:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `resource_kind` | string | sí | as inspect answered |
| `resource_id` | string | sí | must have the shape of the provider's ids, else invalid_input |
| `auth` | string: firma_ok \| firma_invalida \| sin_firma \| sin_secreto |  | inspect verdict, for the provider's own logging |

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
| `also_captured` | array<string> |  | provider refs of OTHER captured payments of the same order_ref (double charge); the one answered stays in provider_ref |

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
| `payments[].also_captured` | array<string> |  | provider refs of OTHER captured payments of the same order_ref (double charge); the one answered stays in provider_ref |
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

### `commerce.payments.refund.status`

re-read a refund by its provider refund id (read only; never refunds again); the output is a commerce.payments.refund output. Consulta · requiere `Query.Provider` · cardinalidad **one** · tiempo límite por proveedor **8s**.

Entrada:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `refund_id` | string | sí | provider refund id (commerce.payments.refund output) |
| `provider_ref` | string |  | provider payment id the refund belongs to |
| `idempotency_key` | string |  | the refund's key, for providers that look it up by key |

Salida: la misma de `commerce.payments.refund`.

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
| `method_hint` | string |  | preselected method family or option (card, cash, wallet and credits are the embedded-payment options) |
| `flow` | string: redirect \| embedded |  | redirect (default): hosted checkout, the buyer leaves the page; embedded: the provider declares in client what the storefront paints and the host charges with commerce.payments.submit. A provider that does not support embedded answers redirect |
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
| `flow` | string: redirect \| embedded |  | flow the provider actually honored; absent = redirect (a provider that predates embedded payment) |
| `client` | object |  | public data to paint the provider form in the storefront (same shape as commerce.payments.client; provider extras allowed); instructions.brick stays for older stores |

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
| `also_captured` | array<string> |  | provider refs of OTHER captured payments of the same order_ref (double charge); the one answered stays in provider_ref |

### `commerce.payments.submit`

charge what the buyer's browser tokenized with the provider SDK (embedded payment); the host sets amount and currency, the provider rejects any other amount; pinned to the session's provider and idempotent. Consulta · requiere `Query.Provider` · cardinalidad **one** · tiempo límite por proveedor **30s** · mutante (idempotente).

Códigos de error del proveedor: `invalid_input`, `price_mismatch`, `method_not_allowed`, `installments_exceeded`.

Entrada:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `session_id` | string | sí | commerce.payments.session session_id |
| `order_ref` | string | sí | order id/number in OPS (external_reference) |
| `amount` | decimal | sí | total to charge, set by the host from its own document |
| `currency` | currency | sí |  |
| `method` | string: card \| cash \| bank_transfer | sí | family the buyer chose |
| `provider_data` | object | sí | opaque: what the provider SDK handed the browser (card token, method id, issuer, installments, payer id, cash point). Never a card number |
| `idempotency_key` | string | sí | required; stable per business operation (reuse it on every retry); the provider must dedupe on it; one per attempt, not per checkout |
| `device_session_id` | string |  | anti-fraud device fingerprint from the provider SDK; pass through, never store or log |
| `buyer` | Buyer |  | from the host's record, not the browser |
| `shipping_address` | Address |  | from the host's record, not the browser |
| `lines` | array<Line> |  |  |
| `market` | string |  | market code the order was placed in; the provider uses that market's configuration |
| `metadata` | object |  |  |
| `metadata.checkout_id` | string |  | checkout id in OPS |
| `metadata.attempt` | integer |  | attempt number of the checkout |
| `expires_at` | string |  | RFC 3339 expiry of a cash / transfer voucher |
| `return_url` | string |  | where a challenge returns the buyer |
| `notify_url` | string |  | webhook URL the host exposes (as in commerce.payments.session) |

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
| `also_captured` | array<string> |  | provider refs of OTHER captured payments of the same order_ref (double charge); the one answered stays in provider_ref |
| `action` | object |  | what the storefront does next; absent = none |
| `action.type` | string: none \| challenge \| voucher \| redirect | sí | none: nothing to do; challenge: show the bank challenge (3DS) inside the page; voucher: show the cash / transfer voucher; redirect: send the buyer to redirect_url |
| `action.challenge` | object |  | type challenge: what the provider's SDK needs to mount the bank challenge |
| `action.challenge.url` | string |  | challenge URL (external_resource_url) |
| `action.challenge.creq` | string |  | challenge request, as the provider returns it |
| `action.voucher` | object |  | type voucher: the offline payment slip (never a card number) |
| `action.voucher.payment_point` | string |  | cash point or bank (oxxo, 7eleven, spei...) |
| `action.voucher.reference` | string |  | payment reference to give at the point |
| `action.voucher.voucher_url` | string |  | hosted voucher page (https) |
| `action.voucher.barcode` | string |  | barcode content |
| `action.voucher.clabe` | string |  | bank transfer account (CLABE) |
| `action.voucher.amount` | decimal |  | exact amount to pay |
| `action.voucher.currency` | currency |  |  |
| `action.voucher.expires_at` | string |  | RFC 3339; after it the voucher no longer pays |
| `action.redirect_url` | string |  | type redirect: hosted page the buyer must open |
| `decline_code` | string: insufficient_funds \| invalid_cvc \| invalid_expiry \| invalid_card_data \| call_issuer \| card_disabled \| duplicate \| max_attempts \| authentication_failed \| declined |  | neutral rejection reason when status is failed |
| `retryable` | boolean |  | failed: the buyer may try again with the same checkout (another card, fixing a field) |

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
| `purpose` | string: outbound \| return |  | outbound (default) or return label |
| `shipment_ref` | string |  | id of the host shipment record this label belongs to |
| `insurance_value` | decimal |  | value to insure, in currency; absent = not insured |
| `quote_ref` | string |  | rates[].quote_ref of the chosen rate |
| `quote_hash` | string (`sha256:<64 hex>`) |  | hash of the host's immutable quote snapshot; provider may log it, never rely on it |
| `origin_location_id` | string |  | location the parcel ships from |

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
| `cost_tax` | decimal |  | tax included in cost, when the carrier invoices it |
| `estimated_delivery` | string (`YYYY-MM-DD`) |  | promised delivery date |
| `insurance_cost` | decimal |  | part of cost that is insurance |
| `billable_weight_kg` | decimal |  | weight the carrier billed |
| `parcels` | array<Parcel> |  | packages as the carrier registered them |

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
| `market` | string |  | market code the quote is for (the host also selects providers by it) |
| `subtotal_basis` | decimal |  | cart subtotal AFTER line / amount discounts and coupons, in currency: the one basis a provider must use for its own 'free over' rule (declared_value stays the insured value) |
| `origins` | array<object> |  | candidate ship-from locations (multi-origin): Address + `location_id` (required); origin stays the primary one |
| `delivery_types` | array<string: door \| branch \| same_day \| on_demand \| pickup> |  | only these delivery types are wanted |
| `origin_configured` | boolean |  | false: the host has no configured origin and origin is a placeholder; the provider quotes with its own default or answers no rates |

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
| `rates[].list_cost` | decimal |  | price before any 'free' rule or discount of the provider; list_cost >= cost; absent = same as cost |
| `rates[].free_reason` | string: none \| rate_free \| rate_free_over \| staff_override \| pickup_free |  | why cost is below list_cost or zero (provider side only) |
| `rates[].rule_ref` | object |  | `kind`, `id` (required), `version`: the provider rule behind the price |
| `rates[].delivery_window` | object |  | `from`, `to` (`YYYY-MM-DD`, both required): promised dates |
| `rates[].pickup_point` | object |  | `id`, `name` (required), `address` (Address), `hours`, `lat`, `lng` |
| `rates[].insurance` | object |  | `available`, `included` (boolean), `cost` (decimal) |
| `rates[].fallback` | boolean |  | a backup rate offered because live carriers failed or no zone matched |
| `rates[].quote_ref` | string |  | provider quote id that label needs besides rate_id |
| `rates[].quoted_at` | string |  | RFC 3339 time the provider computed the price |
| `rates[].quote_hash` | string (`sha256:<64 hex>`) |  | provider digest of the priced facts (id, cost, list_cost, currency, expires_at) |
| `rates[].origin_location_id` | string |  | origins[].location_id this rate ships from |
| `rates[].billable_weight_kg` | decimal |  | weight the price is based on (max of actual and volumetric) |
| `rates[].parcels` | array<Parcel> |  | packages the quote assumed |

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

### `fiscal.invoice.document`

one file (pdf or xml) of a fiscal document: bytes or a short-lived RELATIVE signed url, never a provider URL; read by the customer account. Consulta · cardinalidad **one** · tiempo límite por proveedor **15s** · errores `not_found`, `not_ready`, `provider_unavailable`, `not_configured`.

Entrada:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `order_ref` | string | sí | order the document must belong to (ownership check) |
| `document_id` | string | sí | from fiscal.invoice.status |
| `kind` | string: pdf \| xml | sí |  |

Salida:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `kind` | string: pdf \| xml | sí |  |
| `filename` | string | sí | suggested file name, without path |
| `content_type` | string: application/pdf \| application/xml | sí |  |
| `size_bytes` | integer | sí | decoded size; at most InvoiceDocumentMaxBytes (5 MiB) |
| `content_base64` | string |  | the file; exactly one of content_base64 or url |
| `url` | string |  | RELATIVE signed path served by the provider addon (starts with a single /); never an absolute URL |
| `expires_at` | string |  | RFC 3339; required with url; at most 15 minutes ahead |
| `test_mode` | boolean |  | see test_mode above |

### `fiscal.invoice.status`

state of the fiscal documents (invoices and credit notes) of an order, normalized and free of provider URLs; read by the customer account. Consulta · cardinalidad **one** · tiempo límite por proveedor **8s** · errores `not_found`, `not_ready`, `provider_unavailable`, `not_configured`.

Entrada:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `order_ref` | string | sí | order id/number in OPS; the provider checks that the documents belong to it and to the invoking org |
| `document_id` | string |  | narrow the answer to one document of that order |

Salida:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `documents` | array<object> | sí | empty = nothing issued yet for a known order |
| `documents[].document_id` | string | sí | opaque id of the document inside the provider; pass it back to fiscal.invoice.document |
| `documents[].status` | string: issued \| pending \| cancelled \| error | sí |  |
| `documents[].kind` | string: invoice \| credit_note | sí |  |
| `documents[].fiscal_uuid` | string |  | folio fiscal (CFDI UUID) or the country's equivalent |
| `documents[].series` | string |  |  |
| `documents[].folio` | string |  |  |
| `documents[].total` | decimal |  |  |
| `documents[].currency` | currency |  |  |
| `documents[].emitted_at` | string |  | RFC 3339; absent while pending |
| `documents[].related_to` | string |  | document_id of the invoice a credit_note applies to |
| `documents[].available_files` | array<string> |  | subset of pdf / xml that fiscal.invoice.document can serve now |
| `documents[].test_mode` | boolean |  | the answer comes from the provider sandbox / test credentials (true) or from production (false); absent = the provider does not say |
| `documents[].environment` | string: sandbox \| production |  | absent = the provider does not say |
| `documents[].error_reason` | string: invalid_customer_data \| invalid_tax_data \| rejected_by_authority \| provider_rejected \| unknown |  | neutral reason when status=error |
| `documents[].error_message` | string |  | short human text for the store owner, never provider internals, URLs or credentials |

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
