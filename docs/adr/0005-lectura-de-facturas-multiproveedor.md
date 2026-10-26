# ADR 0005: lectura de facturas sin proveedor fiscal concreto (`fiscal.invoice.status` y `fiscal.invoice.document`)

- Estado: aceptado (fase F2 del diseño de la cuenta del cliente, B.4).
- Paquete: `capability`. Versión: minor, aditiva (dos llaves nuevas, sin tocar las existentes).
- Referencia: `docs/capabilities-commerce.md`, sección «Lectura de facturas».

## Contexto

La cuenta del cliente leía `fiscal_documents` y servía enlaces del PAC (factura.com), atándose a un proveedor. Hoy solo existen `fiscal.invoice.create` y `fiscal.invoice.cancel` (escritura). Se necesita leer estado y descargar PDF/XML con otro PAC, otro país, o con «facturación no disponible» (EE. UU.).

## Decisión

1. Dos capacidades `KindQuery`, cardinalidad `one`, **sin** `PinRequired`: se resuelven por mercado con `Select` (país, `Default`, `Priority`, `StatusNotConfigured`). Si nadie atiende el mercado, `ErrNoProvider` = facturación no disponible.
2. `status` (`order_ref` → `documents[]` de facturas y notas de crédito) devuelve estado normalizado (`issued|pending|cancelled|error`, `kind`, folio/uuid, total decimal + moneda, `emitted_at`, `related_to`, `test_mode`/`environment`, `error_reason` neutral) y **ningún campo de URL**.
3. `document` (`order_ref` + `document_id` + `kind`) devuelve bytes en base64 (≤ 5 MiB) o una URL **relativa** firmada de ≤ 15 min; nunca una URL del PAC. Lo que el esquema no expresa lo comprueban `CheckInvoiceDocument` / `CheckInvoiceStatus`, y lo hace **el host**: el contrato los declara en `Contract.CheckOutput` y un tope `Contract.MaxOutputBytes` previo a decodificar, que el `Executor` aplica a todo proveedor (no queda en manos de cada consumidor).
4. Privacidad: la organización es la de la invocación (`Query.Org`), no un campo de entrada. El proveedor valida que el documento pertenece al pedido y a la organización; si no, `not_found` indistinguible de «no existe». El addon de cuenta solo pasa pedidos que ya verificó del cliente autenticado.
5. Errores normalizados en `Contract.Errors`: `not_found`, `not_ready`, `provider_unavailable`, `not_configured`.
6. Permiso/dominio: dominio `fiscal.invoice`; el invocador declara `capability:query fiscal.invoice.status|document` y el proveedor lo declara en `provides_capabilities`. **No hay campos nuevos de manifest v3.**

## Consecuencias

- Un PAC nuevo es otro proveedor de las mismas llaves; la cuenta y las tiendas no cambian.
- El esquema queda fijado por un golden (`testdata/fiscal_invoice_read.golden.json`): evolución solo aditiva.
- Limitaciones conocidas: el esquema no tiene longitud máxima por campo ni «uno de»; por eso las verificaciones del contrato (`Contract.CheckOutput`, campo Go del kernel, no de manifest). Las extensiones `x_` no pueden traer enlaces y el consumidor no las reenvía al navegador. Los mensajes de error (`Envelope.errors[].message`) pueden traer texto del proveedor: el consumidor no los reenvía.

## Cadena de adopción

Release del kernel, bump en ops (resolvedor y `capability:query` ya existen), y luego los addons: `fiscal_mexico` provee ambas llaves y el addon de cuenta las consume.
