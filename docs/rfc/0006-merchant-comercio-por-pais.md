# RFC 0006 — Merchant (comercio): cuentas de varios países en una sola organización, bien divididas

- **Estado:** propuesto. Solo diseño: este documento no cambia código.
- **Fecha:** 2026-11-21
- **Repos afectados:** `metacore-kernel`, `metacore-sdk`, `ops`, `addons` (`locations`, `customers`, `inventory`, `storefront_*`, `payments`/`mercadopago`, `region_*`, `fiscal_*`, `channel_orders`), tiendas (`store-*`, `local`).
- **Relacionado:** RFC 0001 (entidad y alcance multi-tenant), RFC 0002 (estadísticas de grupo con aislamiento estricto), [ADR 0002](../adr/0002-addon-settings-scoped-store.md) (ajustes con alcance), [RFC de pagos](../RFC-PAYMENTS-STANDARD.md).

## 0. Resumen

Una empresa que vende en varios países (God's: México y EE. UU.) quiere **un solo lugar** —un equipo, un catálogo, un panel— y, a la vez, **cuentas bien divididas por país**: su propia razón social, moneda, impuestos, facturación, cuentas de cobro y de canal, inventario, caja y permisos. Hoy la plataforma resuelve la mitad: hay mercados (`org_markets`), sucursales atadas a un mercado (`branches.market_code`) y un contrato de lectura por mercado en el kernel. Falta la pieza que **junta país + razón social + cuentas** en una unidad con dueño, y que hace exigible la separación.

Este RFC define esa unidad: el **merchant (comercio)** = *una razón social que vende en un mercado*. No crea un concepto paralelo: **compone** lo que ya existe (entidad del RFC 0001, mercado, sucursal) y agrega lo que no existe (cuentas por comercio, políticas de alcance declaradas, permisos por comercio, consolidado de grupo).

| # | Decisión |
|---|---|
| D1 | **Merchant = (entidad × mercado).** Una organización tiene N merchants. Ej.: `gods-mx` (God's México, S.A. de C.V., MXN, CFDI, Mercado Pago MX) y `gods-us` (God's LLC, USD, sales tax, Stripe US). |
| D2 | **La organización sigue siendo la frontera de tenant** (aislamiento duro). El merchant es una **frontera de operación, fiscal y de autorización** dentro de ella, igual que la entidad del RFC 0001. No es un tenant por país: se perdería el catálogo, los clientes y el panel únicos. |
| D3 | El merchant vive en el addon `locations` (como la entidad). **El kernel no aprende palabras de negocio:** solo aporta mecanismos genéricos (alcance, cadena de ajustes/credenciales, portadores de contexto, política de alcance por modelo). |
| D4 | Los registros **no duplican `merchant_id`**: se **deriva de la sucursal** (`branch_id` ya viaja en contexto, eventos y datos), como la entidad. Los documentos fiscales y cobros cerrados guardan una **foto** inmutable del merchant. |
| D5 | **Cada modelo declara su política de alcance** en el manifiesto —`scope: merchant \| market \| shared \| group`— y el CI falla si un modelo con `branch_id` no la declara. Reemplaza la convención implícita de hoy (`markets` jsonb). |
| D6 | **Credenciales, ajustes y proveedores** ganan el nivel merchant en su cadena de herencia: `sucursal → merchant → organización`. Cada país tiene su propia cuenta de cobro, su PAC, su paquetería y sus cuentas de canal. |
| D7 | **Permisos por merchant** (`merchant_members`): el equipo de EE. UU. no ve México. El administrador de grupo ve todo. |
| D8 | **Nunca se convierten monedas en tiendas ni en documentos.** Cada documento lleva la moneda de su merchant. El **consolidado de grupo** (RFC 0002) convierte solo en la capa de reporte, con una tabla de tipo de cambio explícita (fuente y fecha). |
| D9 | **Sin configuración, nada cambia (D7 del RFC 0001):** toda organización tiene un merchant implícito por defecto; una con un solo mercado se comporta como hoy. |
| D10 | Rollout por fases con banderas; cada fase trae su **prueba de aislamiento** que corre en CI. |

## 1. Lo que ya existe (evidencia)

- **Mercados y sucursales:** `org_markets` (código, moneda, idioma) y `branches.market_code` (`locations` 0.5.0). La sucursal activa (`X-Branch-ID`) fija el mercado de la sesión.
- **Contrato de lectura por mercado (kernel v0.314.0):** `dynamic.WithMarket` / `MarketFromContext`, `MarketScoper`, `ApplyMarketScope`, `ColumnDataType`. Un modelo con columna `markets` jsonb (NULL o `[]` = todos) solo muestra sus filas en los mercados listados. Ops lo aplica en listas, búsquedas, facetas, tablero y agente (`BranchAwareScoper`, `DataScope`).
- **Productos, descuentos y clientes por mercado:** `products.markets`, `discount_rules.markets`, `customers.markets` (derivado de las ventas, migración 065) y almacenes/existencias que heredan la sucursal por trigger. Verificado en gods: productos 12/12/5, clientes 5/4/2, ventas 12/14.
- **Ajustes por mercado:** `wasm.SettingsResolver` ya resuelve ajustes con mercado en contexto (`MarketSettingsResolver`, `ResolveSettingsInMarket`).
- **Proveedores por mercado:** `org_market_providers` y el paquete `routing` (gana prioridad y luego más claves de coincidencia).
- **Tiendas:** el mercado viaja en `X-Storefront-Domain: host#mercado`; precios, impuestos y envío por mercado los decide Ops, nunca la IP.
- **Entidad y alcance (RFC 0001, propuesto):** `loc_entities`, `branches.entity_id`, `dynamic.EntityScope` (conjunto cerrado de sucursales) y su resolución en kernel.

## 2. Lo que falta (problema)

1. **No hay una unidad que junte país + razón social + cuentas.** Hoy el mercado es una etiqueta de la sucursal y la entidad es otra; nada obliga a que coincidan ni a que tengan sus propias cuentas.
2. **Las credenciales son por organización** (`org_integrations`, único por `(org, provider)`): no se puede tener Mercado Pago MX y Stripe US, ni dos cuentas de Mercado Libre, sin hacks.
3. **La convención de alcance es implícita.** Un modelo nuevo con `branch_id` que no declara `markets` se ve igual en todos los mercados y nadie se entera. Hoy: pedidos de canal (26/26/26) y traspasos (6/5/5) no se dividen por región.
4. **Existen filas sin sucursal** (almacén original, movimientos históricos) que se ven en todas las regiones y falsean existencias y kárdex.
5. **Sin permisos por país.** Quien entra al panel ve lo que su rol permite en toda la organización.
6. **Sin consolidado serio.** El tablero de grupo no existe; convertir monedas «a ojo» sería un error fiscal.
7. **Fiscal por país sin dueño:** CFDI (MX) y sales tax (EE. UU.) se eligen por configuración dispersa, no por el merchant.

## 3. Diseño

### 3.1 Modelo (addon `locations`)

```
organización ──┬── entidad A (razón social) ──┬── merchant gods-mx  (mercado mx)
               │                               └── merchant gods-es  (mercado es)   ← misma entidad, otro país
               └── entidad B ───────────────────── merchant gods-us  (mercado us)
                                                      └── sucursales / almacenes / canales
```

| Tabla / columna | Contenido |
|---|---|
| `loc_merchants` | `id`, `org_id`, `code` (estable), `entity_id`, `market_code`, `currency`, `settlement_currency`, `status`, `is_default`, `source` (`ops` \| `external:<sistema>`), `external_ref` |
| `branches.merchant_id` | no nulo tras la migración; índice `(org_id, merchant_id)`. `branches.market_code` y `entity_id` quedan como **derivados** del merchant (compatibilidad) |
| `merchant_members` | `(org_id, merchant_id, user_id, role)`; sin membresías = acceso de grupo según rol |

Un mismo país puede tener **varios merchants** (dos razones sociales en México) y una misma entidad **varios países**: por eso es (entidad × mercado) y no una columna más.

### 3.2 Cuentas por merchant

| Cuenta | Hoy | Con merchant |
|---|---|---|
| Cobro (Mercado Pago, Stripe…) | una por org | una por merchant; la tienda de ese mercado usa la suya |
| Canal (Mercado Libre, Amazon) | una por org | `channel_account` pertenece a **una** sucursal de canal → un merchant |
| Facturación / PAC | por org | por entidad (serie y folios propios) |
| Paquetería | por org | por merchant (tarifas y remitente propios) |
| Cuenta bancaria / liquidación | no existe | `settlement_currency` + cuenta por merchant |
| Ajustes de addons | `org` y `branch` | `org` → `merchant` → `branch` (ADR 0002, `scope_kind = merchant`) |

`connectors.Store.Get(ctx, orgID, key)` gana un parámetro de alcance (`scope_kind`, `scope_id`) con la misma resolución en cadena; sin alcance sigue leyendo el de la organización (compatible).

### 3.3 Política de alcance declarada (el cambio que hace escalable la separación)

Cada modelo del manifiesto v3 declara `scope`:

| `scope` | Significa | Ejemplos |
|---|---|---|
| `merchant` | Pertenece a un merchant (por su `branch_id`); solo visible en él | pedidos, cobros, facturas, movimientos, caja, transferencias |
| `market` | Maestro compartido, **visible según `markets`** | productos, reglas de descuento, listas de precio |
| `shared` | Maestro global de la organización | clientes (con `markets` derivado de la actividad), categorías, marcas |
| `group` | Solo con vista de grupo | consolidados, configuración de merchants |

- **Kernel:** valida el manifiesto, expone la política al `DataScope` y rechaza en arranque un modelo con `branch_id` sin `scope` (con ruta de adopción gradual: advertencia → error).
- **Prueba de aislamiento obligatoria (CI):** recorre **todos** los modelos con `scope: merchant|market`, crea datos en dos merchants y comprueba que ninguna ruta (lista, por id, opciones, búsqueda, facetas, agregados, tablero, agente, exportación, WASM) cruza la frontera. Esto convierte «¿ya se divide todo?» en una prueba, no en una revisión manual.

### 3.4 Alcance y permisos

- El merchant **se expande a un conjunto de sucursales** y reutiliza `EntityScope` y el alcance por sucursal (D4 del RFC 0001); no se inventa otro mecanismo.
- `merchant_members` restringe qué merchants puede activar una persona; el selector de sucursal del panel agrupa por merchant (bandera + moneda).
- Filas **sin sucursal** pertenecen al merchant por defecto, con política explícita por modelo y **migración de relleno** (almacén original → su sucursal; hoy es el `UPDATE` pendiente en gods).

### 3.5 Moneda y consolidado

- Cada documento guarda la moneda de su merchant; el tablero por merchant formatea en esa moneda (ya hecho para el tablero de gods).
- El **consolidado de grupo** (RFC 0002) agrega por merchant y convierte **solo en el reporte** con `fx_rates(date, from, to, rate, source)`; el reporte muestra siempre la tasa y la fecha. Las tiendas y los documentos nunca convierten.

### 3.6 Fiscal por país

El merchant elige el **motor fiscal** por su país vía los addons de región (`region_mx`: RFC, régimen SAT, CFDI; `region_us`: sales tax por estado; otros países aportan su identificador y reglas). El núcleo no asume RFC ni IVA. Los documentos emitidos guardan la foto de entidad y merchant.

### 3.7 Tiendas

La resolución `X-Storefront-Domain: host#mercado` pasa a resolver **merchant**: el mercado de la ruta (`/mx`, `/us`) elige el merchant, que fija cuenta de cobro, impuestos, envío y catálogo (`markets`). Una tienda nueva solo declara qué mercados vende; no escribe lógica por país.

### 3.8 SDK y Ops

- **SDK:** tipo `Merchant`, hook `useMerchant()`, selector de comercio en el shell, formateo de moneda por merchant (ya existe `useMarketCurrency`).
- **Ops:** pantalla de merchants (alta guiada: entidad + mercado + moneda + cuentas), selector agrupado, tablero por merchant y de grupo, auditoría de acceso entre merchants.

## 4. Fases y criterios de aceptación

| Fase | Entrega | Aceptación |
|---|---|---|
| **P0 (hecha)** | Contrato de mercado en kernel, sucursal por mercado, productos/descuentos/clientes por mercado, tablero en la moneda del mercado | gods: 12/12/5, clientes 5/4/2 |
| **P1** | `loc_merchants` + `branches.merchant_id`, backfill 1 merchant por mercado existente, selector agrupado. Solo lectura de lo existente | org de un solo mercado idéntica a hoy |
| **P2** | `scope` declarado en el manifiesto v3 + validación en el kernel + **prueba de aislamiento en CI** + relleno de filas sin sucursal | ningún modelo con `branch_id` sin política; prueba verde con 2 merchants |
| **P3** | Cadena `merchant` en ajustes y credenciales (`connectors`), cobros por merchant, cuentas de canal por sucursal de canal | dos cuentas de cobro distintas operando a la vez, sin filtrarse |
| **P4** | `merchant_members` + permisos + auditoría | usuario de EE. UU. recibe 404 en registros de México (por id, lista, búsqueda, WASM) |
| **P5** | Motor fiscal por merchant, series y folios por entidad | CFDI solo para merchants MX; EE. UU. sin CFDI |
| **P6** | Consolidado de grupo con `fx_rates` y tablero de grupo | totales del grupo = suma de merchants convertidos con tasa visible |
| **P7** | Tiendas: dominio+mercado → merchant | `local`, God's y ProtekAuto sin lógica por país |

Cada fase se publica con bandera y es reversible; ninguna cambia el comportamiento de una organización que no configura merchants (D9).

## 5. Alternativas descartadas

- **Un tenant (organización) por país.** Aísla bien pero rompe el catálogo, los clientes y el panel únicos que la empresa pide, y obliga a sincronizar maestros. Se queda como opción para clientes que exigen residencia de datos distinta (fuera de alcance).
- **Solo una columna `country` en cada tabla.** Duplica la verdad (sucursal y registro pueden discrepar) y no resuelve cuentas, permisos ni fiscal. Contradice D4.
- **Una tabla de «merchants» en el kernel.** El kernel no debe conocer palabras de negocio; vive en `locations` y el kernel aporta solo el mecanismo (D3).

## 6. Riesgos y preguntas abiertas (con recomendación)

1. **Nombre.** «Merchant» es el término de la industria de pagos; en la interfaz en español se propone **«Comercio»**. Recomendado: tabla `loc_merchants`, etiqueta «Comercio».
2. **Rendimiento de derivar el merchant por sucursal.** Recomendado: sin denormalizar; si una lista supera p95 > 300 ms con datos reales, agregar `merchant_id` a esa tabla con trigger (misma regla que el RFC 0001 §3.3).
3. **Mismo país, varias razones sociales.** Soportado por el modelo; la tienda necesita una regla de enrutamiento (`org_market_providers` ya enruta por prioridad).
4. **Operaciones intercompañía** (un merchant surte a otro): se marcan (`intercompany = true`) y se entregan al ERP, como en el RFC 0001 §3.2.1; no se contabilizan en Ops.
5. **Migración de filas sin sucursal.** Recomendado: relleno desde su almacén, con reporte de lo que no se pudo asignar, y política explícita por modelo.
6. **Adopción de `scope`.** Recomendado: advertencia durante un ciclo de release y error después, para no romper addons de terceros.
