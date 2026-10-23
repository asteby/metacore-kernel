# RFC: atributos físicos y de envío de producto, multiindustria

- Estado: **propuesto**. La fase 1 está implementada en PRs independientes (§11); las fases 2-4 son diseño.
- Fecha: 2026-10-23
- Alcance: kernel (`measure`, `capability`), SDK, addons `products` / `storefront_cart` / `shipping_flat` / paqueterías, OPS (importador, ajustes), starter `local` y tiendas.
- Pedido del dueño: «las unidades de medida dependen del tipo (de producto); hazlo pro como los mejores, multiindustria».
- Se alinea con (no contradice): `DISENO-SISTEMA-DE-ENVIOS-2026-10-20.md` (ops, rama `docs/ux-instalados`; §4.3 peso y medidas, §4.8 contrato), ADR 0001 de este repo (`docs/adr/0001-contratos-envios-aditivos.md`), `RFC-PRESETS-DE-EMPRESA-Y-DISTRIBUCION.md` (capas defaults < preset < cliente).

## 0. Resumen

Hoy el peso de un envío es un parche en el carrito (`storefront_cart`, asteby-hq/addons#2198): cadena `metadata.weight_g > metadata.weight_kg > columna weight_kg (variante→producto) > default_parcel_weight_kg`, medidas solo si el carrito es 1 unidad de 1 producto, y unidades implícitas (kg y cm). Incidente real: en ProtekAuto el peso llegaba 0 y Skydropx rechazaba (`parcels[0].weight_kg debe ser un decimal mayor a 0`) → checkout sin envíos.

Propuesta en cinco piezas:

1. **Unidades como primitivo general del kernel** (`measure`): catálogo fijo y exacto por magnitud (masa, longitud, volumen, área, conteo), enteros en milésimas de la unidad base, conversión exacta y vectores dorados compartidos con el SDK (`lib/measure.ts`), igual que `money`. Las unidades físicas son código; lo que la organización configura es en qué unidad **vende** y en qué sistema **muestra**.
2. **Atributos físicos por SKU** en las columnas que ya existen (`weight_kg`, `length_cm`, `width_cm`, `height_cm`, `requires_shipping`, `ships_separately`, `hs_code`), siempre **por una unidad de venta**, más un **perfil de envío** de primera clase (`ShippingProfile`) que se asigna a categorías, clases de atributo («tipo de producto») y a la organización, con **herencia explícita y auditable** (`weight_source`).
3. **Empaques** (`product_packagings`, estilo Odoo) para «caja c/4»: la caja tiene su propio peso, medidas y código de barras; la pieza sigue siendo la unidad de venta.
4. **Armado de bultos** en un motor puro compartido (`addons/shared/shipspec`): suma exacta, bultos por separado, servicios fuera, medidas del bulto por reglas simples y, después, cajas estándar configurables y bin-packing opcional; **peso volumétrico** con divisor configurable por tarifa/paquetería.
5. **Validación accionable** en tres puertas (captura, importación, cotización): nunca se llama a un proveedor con 0 kg; el error dice qué SKU y cómo corregirlo.

Fase 1 (ya en PRs, mergeables por separado): normalización de unidades + procedencia + bultos en el carrito (sin cambiar su comportamiento por omisión), relleno de columnas desde `metadata`, importador que escribe columnas, perfil de ProtekAuto corregido y aviso correcto en el checkout del starter.

## 1. Hallazgos

### 1.1 Datos de producción (ProtekAuto, solo `SELECT`, 2026-10-23)

Base `ops` del VPS (contenedor `ops-pg`), tablas en `public` (las de `addon_products.*` / `addon_inventory.*` existen pero están vacías: son las sombras muertas que documenta products/migrations/001).

| Dato | Cifra |
|---|---|
| Organizaciones / productos vivos | 1 / **3 796** (3 795 importados en una sola transacción el 2026-10-22 23:16 UTC + 1 de prueba `TEST-PAY-5`) |
| `products.weight_kg` (columna) > 0 | **1** (el de prueba) |
| `metadata.weight_kg` numérico > 0 | **3 795** (mín 0.02, mediana 0.9, máx 11 kg) |
| `length_cm/width_cm/height_cm` (columnas) | **1** (el de prueba) |
| `metadata.attributes.{largo,ancho,alto}_cm` numéricos > 0 | **3 795** (medidas reales, en la ficha, donde el carrito no las lee) |
| `metadata.attributes.caja_piezas` | 40 (12 → 18, 4 → 17, 20 → 4, 3 → 1); todos lubricantes: «ACEITE … 5L CAJA C/4» con **peso 4.6 kg y precio $976.72 de UNA pieza** |
| `product_type` / `unit_of_measure` | 3 796 × `storable` / `unit` |
| `sold_by_measure`, `has_variants`, `ships_separately`, `hs_code` | 0 en todos |
| `requires_shipping` | 3 796 `true` (default) |
| `sales_uom_id` / `purchase_uom_id` / `stock_uom_id` | todos `NULL` |
| `unit_of_measures` / `uom_conversions` (tablas del host) | **0 / 0 filas** |
| `product_variants` | 0 |
| `product_specs`, `fiscal_data` (producto) | vacíos (`{}`) en todos: **ninguna clave SAT de producto aún** |
| Categorías | 4, sin jerarquía, `attribute_classes = {}`, `fiscal_data = {}`: Suspensión y dirección 2 046, Frenos 1 475, Filtros 226, Lubricantes y fluidos 48 |
| Piezas más comunes (atributo `pieza`) | BALATA DISCO 748 (prom 1.77 kg), TERMINAL DIRECCION 302 (0.62), BUJE 299 (0.37), ROTULA 254 (0.66), AMORTIGUADOR 231 (2.97), FILTRO ACEITE 226 (0.28), HORQUILLA 189 (4.24), CAJA DIRECCION 31 (7.89) |
| Ajustes del carrito | `storefront_cart` **0.6.3** (anterior a #2198, cuyo changeset aún no se publica); `default_parcel_weight_kg` sin definir; `skydropx` 0.2.5 |
| OPS en el VPS | 0.1.28, kernel **v0.229.4** (≥ v0.226.0: `items[].weight_kg/length_cm/requires_shipping` ya están en el contrato) |

Lectura: **el dato existe pero está en el lugar equivocado.** El peso está en `metadata` (por eso 0.6.3, que lee `metadata.weight_kg`, hoy sí tiene peso para 3 795 productos), y las medidas están en la ficha (`metadata.attributes`), así que las paqueterías cotizan con su caja por omisión de 20×20×20 cm aunque la caja real de una horquilla de 4.24 kg sea otra. Todas las filas tienen el mismo `updated_at` (2026-10-23 03:38:04 UTC, una actualización masiva): es **compatible** con que el peso en `metadata` se haya cargado después del incidente, pero desde la base no se puede probar qué cambió (hipótesis, no hecho). Los registros de `ops-web` de las últimas 120 h no contienen el texto del rechazo de Skydropx (el backend corre como servicio aparte, cuyo log no se leyó).

### 1.2 Código

| # | Hallazgo | Dónde | Consecuencia |
|---|---|---|---|
| C1 | El importador escribe el peso del **producto** siempre en `metadata.weight_kg`, aunque `products` ≥ 0.23 tiene la columna (la variante sí usa columna con `setOrMeta`). No tiene campos de medidas ni de unidad. | ops `backend/services/catalogimport/bundle.go` (`meta["weight_kg"] = *p.WeightKg`), `ir.go` | Catálogos importados quedan «sin peso» en pantalla y sin medidas. La migración 011 de `products` rellenó la columna **una vez**; lo importado después no. |
| C2 | El perfil de ProtekAuto manda las medidas a `attributes.*_cm` y la unidad (`U.Med.`: `ST`→`pieza`, `KIT`→`kit`) a un campo que el importador descarta. | store-protekauto `store/import/protek-lista.profile.json` | Medidas invisibles para el envío; unidad perdida. |
| C3 | Precedencia del peso en el carrito: `metadata` **antes** que la columna (prueba `TestWeightMetadataBeatsColumn`), contra lo que documenta el manifest de `products` («variante → producto → metadata (compatibilidad) → por omisión»). | addons `storefront_cart/.../store.go`, `products/manifest.json` | Cuando exista UI para corregir el peso en la columna, la corrección se ignora si `metadata` trae otro. Se arregla en fase 2 **después** de rellenar columnas (§6), para que el cambio sea un no-op. |
| C4 | Dos modelos de UoM que no se hablan: `products.unit_of_measure` (texto con enum `unit/gram/kilogram/liter/milliliter/meter`, lo usa el POS para cantidades decimales) y las tablas del host `unit_of_measures` + `uom_conversions` (GORM, `float64`, «Box» con factor 1, registradas como `units_of_measure` pero la tabla es `unit_of_measures`; 0 filas en ProtekAuto; solo las referencian los addons de datos `manufacturing` / `supply_chain` vía `sales_uom_id`). | ops `backend/core/system/uom.go`, `database/seeders/h_uom_seeder.go` | No hay base sobre la cual construir: las tablas del host se congelan (§5.4). |
| C5 | El contrato `commerce.shipping.rates` ya trae casi todo: `ParcelSchema {weight_kg req, length_cm, width_cm, height_cm, quantity, content}`, `ShipItemSchema {…, weight_kg, length_cm…, requires_shipping, product_code}`, salida `billable_weight_kg` y `parcels`. Falta: mercancía peligrosa, fracción arancelaria por artículo, cantidad decimal (venta por medida), si un peso es estimado. El validador rechaza campos desconocidos salvo prefijo `x_`. | kernel `capability/commerce.go`, `capability/schema.go:205` | Todo lo nuevo es **aditivo** (ADR 0002, §5.1); un addon no puede mandarlo antes de que OPS pinee el kernel que lo declara. |
| C6 | Skydropx, Envía y 99minutos validan `weight_kg > 0` y rellenan medidas faltantes con su ajuste `default_*_cm` (20). `shipping_flat` suma `weight_kg × quantity`. Ninguno calcula peso volumétrico propio: lo hace la API de la paquetería. | addons `*/backend/wasm/domain.go` | El carrito debe mandar **peso real + medidas reales**; mandar peso «facturable» duplicaría el volumétrico. El volumétrico propio solo sirve para tablas internas (`shipping_flat`, tabla del carrito). |
| C7 | `ShipItemSchema.quantity` y `LineSchema.quantity` son enteros. | kernel `capability/commerce.go` | La venta por medida (1.5 kg, 2.75 m) no cabe en el contrato de envío; hoy el carrito web es entero (el POS no cotiza paquetería). |
| C8 | La tienda (starter `local`) trata cualquier `unavailable` como «intenta de nuevo en unos minutos», también `missing_weight`, donde reintentar no sirve. | local `app/routes/checkout.tsx` | Comprador atorado sin salida. |

## 2. Qué hacen los mejores (fuentes al final)

| Plataforma | Unidades | Atributos | Agrupación / herencia | Empaque |
|---|---|---|---|---|
| **Shopify** | peso como objeto `{value, unit}` con enum `WeightUnit` (g, kg, oz, lb) en `InventoryItemMeasurement`; unidad de visualización de la tienda | peso por variante; medidas no son nativas del producto | **perfiles de envío**: uno general (todos los productos) + hasta 99 personalizados con productos asignados y tarifas por ubicación/zona | paquetes personalizados (`CustomShippingPackageInput`) con peso y medidas |
| **Odoo** | categorías de UoM con unidad de referencia y razón; solo se convierte dentro de la categoría | `weight`, `volume` en la plantilla/variante | — (rutas y transportistas por regla) | `product.packaging` (cantidad contenida, código de barras, venta/compra) y **tipos de paquete** con medidas y peso máximo |
| **Medusa** | sin unidades declaradas | `weight`, `length`, `height`, `width` en producto y variante (no verificado en v2 para esta revisión) | **shipping profile** = conjunto de opciones de envío que cumple un conjunto de productos | — |
| **WooCommerce** | una unidad de peso y una de medidas **por tienda** (`woocommerce_weight_unit`, `woocommerce_dimension_unit`: kg/g/lbs/oz; m/cm/mm/in/yd) | peso y medidas por producto/variación | **clases de envío** (una por producto, asignación masiva) que el método de tarifa usa | por plugin |
| **Magento** | una unidad de peso por alcance de configuración (`general/locale/weight_unit`: kgs o lbs) | peso por producto | — | — |
| **Shippo** | `mass_unit` (g, kg, lb, oz) y `distance_unit` (cm, in, ft, m, mm, yd) **por bulto** | — | — | plantillas de paquete por paquetería |
| **EasyPost** | onzas y pulgadas fijas | — | — | `predefined_package` (solo peso) |
| **Paqueterías** | peso volumétrico = L×A×H / divisor (5000 cm³/kg el más común; 139 in³/lb), cada lado redondeado al entero superior; se cobra el mayor | — | — | — |

Conclusiones que adoptamos: (a) la unidad viaja **con** el número o es fija por contrato (Shippo/Shopify), nunca implícita por tienda (Woo/Magento: cambiar la unidad de la tienda reinterpreta todos los productos); (b) la agrupación para no editar miles de SKUs es un **perfil** asignable (Shopify, Medusa, clases de Woo); (c) la caja del proveedor es un **empaque** con su propia cantidad y medidas (Odoo), no la unidad de venta; (d) el volumétrico es por paquetería/tarifa y lo calcula quien cobra.

## 3. Principios

1. **Almacenamiento canónico SI, exacto.** Masa en g, longitud en mm, volumen en ml (área mm², conteo unidad), como enteros en milésimas de la base (mg, µm, µl) cuando se calcula; nada de `float`. Las columnas existentes (`weight_kg numeric(10,3)` = gramo exacto, `*_cm numeric(8,2)` = 0.1 mm exacto) **ya son SI exacto** a la resolución que un envío necesita: **no se migran** a g/mm (romperían POS, importadores, Storefront y fiscal). La conversión por locale es de visualización y captura.
2. **Una unidad física es código; la unidad de venta es dato.** El catálogo de unidades y sus factores vive en el kernel. La organización (o su preset de región) elige el sistema de visualización (métrico/imperial) y cada producto su unidad de venta.
3. **Todo atributo físico es por UNA unidad de venta.** «Caja c/4» se modela como empaque, no como unidad de venta, salvo que la tienda venda la caja (entonces es otro SKU o un empaque vendible).
4. **Herencia con procedencia.** Cada valor sale de una capa con nombre (`weight_source`), en un orden escrito una sola vez; las estimaciones se distinguen de los datos del SKU.
5. **Nunca 0 a un proveedor.** Dato faltante → error accionable por SKU antes de llamar; política configurable (bloquear, estimar con el perfil, permitir).
6. **Lo general al kernel/SDK/shared; lo de la tienda a su configuración.** Nada de autopartes, llantas ni claves SAT en el kernel; las claves fiscales viven en `fiscal_data` (extensiones del addon fiscal) y son dato de la organización.
7. **Solo aditivo.** Campos opcionales en contratos y Storefront API v1 (gate `oasdiff`); ajustes nuevos con el comportamiento anterior por omisión.

## 4. Modelo propuesto

### 4.1 Unidades (`measure`, kernel)

- Magnitudes y base: `mass` (g), `length` (mm), `volume` (ml), `area` (mm²), `count` (unidad). Catálogo fijo con factores legales exactos (1 lb = 453.59237 g; 1 oz = 28.349523125 g; 1 in = 25.4 mm; 1 gal US = 3785.411784 ml; 1 fl oz US = 29.5735295625 ml; galón imperial ausente hasta que un mercado lo pida: un código, un significado).
- Tipo `Milli` (int64, milésimas de la base), `ToMilli`, `FromMilli(m, unit, scale)`, `Convert`, redondeo mitad-lejos-de-cero (el de `money`), errores estables (`unknown_unit`, `bad_number`, `kind`, `overflow`), alias (enum de `products`, español/inglés, códigos de Shopify/Woo/Magento, SAP `ST`, SAT `H87/KGM/LTR/MTR`).
- Vectores dorados `testdata/units.golden.json`; el SDK (`lib/measure.ts`) y la copia de `addons/shared/shipspec` corren **el mismo archivo** comparado por sha256 (patrón de `money/golden.json`).
- **Unidad de venta por tipo**: `products.unit_of_measure` dice la magnitud de la cantidad vendida (conteo → entero; masa/volumen/longitud/área → decimal si `sold_by_measure`). Lo que el dueño llama «la unidad depende del tipo» se resuelve con el **perfil** (§4.3): el perfil de «Lubricantes» propone `liter`, el de «Telas» `meter`, el de «Llantas» `unit`; el producto lo puede cambiar. El enum se amplía de forma aditiva (`square_meter`, `centimeter`, `pound`, `ounce`, `gallon`, `pair`, `dozen`) con etiquetas es/en.
- **Visualización**: ajuste de organización `measurement_system` (`metric` | `imperial`, opcional `display_units` por magnitud) que fija el pack de región (`region_mx` → métrico, `region_us` → imperial) como preset y la organización puede sobrescribir; el SDK formatea con `Intl.NumberFormat(locale, {style: "unit"})`.

### 4.2 Atributos físicos por SKU (addon `products`)

Se quedan las columnas de 0.23 (producto y variante): `weight_kg`, `length_cm`, `width_cm`, `height_cm`, `requires_shipping`, `ships_separately`, `hs_code`. Aditivo en fase 2-3:

| Columna | Dónde | Para qué |
|---|---|---|
| `shipping_profile_id` (uuid, nullable, `ref` a `ShippingProfile`) | `products`, `categories` | herencia (§4.3) |
| `net_content` (numeric) + `net_content_unit` (texto, código de `measure`) | `products` (variante hereda) | contenido neto (946 ml, 500 mg, 5 L): precio por unidad de medida y peso derivado; distinto del peso de envío |
| `density_g_per_ml` (numeric, nullable) | `ShippingProfile` y `products` | líquidos vendidos por volumen: peso = volumen × densidad (+ tara del envase del perfil) |

`weight_kg` es **siempre por una unidad de venta**. Venta por medida: si la unidad de venta es de masa, el peso por unidad se deriva exacto (1 kg por kg vendido, más tara opcional del perfil); si es de volumen, `volumen × densidad`; si es longitud/área, se exige `weight_kg` por unidad (kg por metro, kg por m²).

### 4.3 Perfil de envío (`ShippingProfile`, addon `products`)

Modelo nuevo, org-scoped, con CRUD automático: `name`, `code` (único por organización entre vivos: índice parcial **solo por SQL**, ver §8 riesgos), y valores por omisión **por unidad de venta**:

`weight_kg`, `length_cm`, `width_cm`, `height_cm`, `tare_kg` (envase/embalaje), `density_g_per_ml`, `requires_shipping` (tri-estado: vacío = hereda), `ships_separately` (tri-estado), `package_type` (`box`, `envelope`, `tube`, `bag`, `pallet`, `tire`, `none`), `max_units_per_parcel`, `default_sales_unit` (código de `measure`), `dangerous_goods` (`un_number`, `class`, `packing_group`: estándar ONU, sirve para cualquier país), `hs_code`, `fiscal_data` (jsonb para las extensiones fiscales: p. ej. la clave `ClaveProdServCP`/material peligroso SAT de Carta Porte la declara el addon fiscal como `ModelExtension`, igual que hoy en `products`/`categories`; **el modelo no conoce claves SAT**).

**Herencia y precedencia** (primera capa con valor gana, peso y medidas independientes; medidas siempre las tres juntas):

```
variante (columnas) → producto (columnas) → [compat: metadata de variante/producto, solo lectura]
  → perfil del producto (products.shipping_profile_id)
  → perfil de la categoría (categories.shipping_profile_id; sube por parent_id hasta 5 niveles)
  → perfil de la clase de atributo (Category.attribute_classes: el addon vertical declara el
    perfil sugerido de su clase en su preset, p. ej. clase "tire" → perfil "Llanta")
  → perfil por omisión de la organización (ajuste products.default_shipping_profile)
  → estimación de la tienda (storefront_cart.default_parcel_weight_kg, compatibilidad)
```

`requires_shipping`: el `false` del producto siempre gana; un `false` de su perfil aplica cuando el producto está en el `true` por omisión (la columna es `NOT NULL DEFAULT true` y no distingue «no capturado»). `product_type = service` propone `requires_shipping = false` en el formulario (valor por omisión del formulario, no regla oculta).

**`weight_source`** se calcula al leer (no se guarda): `variant`, `product`, `variant_metadata`, `product_metadata`, `category`, `product_type`, `organization`, `shop_default`, `none` (`shipspec.Source`; `Estimated()` para las cuatro últimas antes de `none`). Viaja en la foto de la cotización del pedido (`request.weight_sources`) y en el informe «Listo para vender»; nunca al comprador.

Para ProtekAuto esto convierte «editar 3 795 SKUs» en «crear 4-6 perfiles y asignarlos a 4 categorías», aunque en su caso **ya hay peso y medidas por SKU** y lo que falta es moverlos a las columnas (§6).

### 4.4 Empaques (`ProductPackaging`, addon `products`, fase 3)

`product_id`, `name` («Caja c/4»), `quantity` (unidades de venta contenidas, decimal), `barcode` (GTIN-14 típico), `weight_kg`, `length_cm/width_cm/height_cm`, `sales`, `purchase`, `ships_as_is` (la caja sale cerrada tal cual). Lo usan compras (comprar por caja), POS (escanear la caja = N piezas), inventario (recibir por caja) y el empacador: si el carrito pide ≥ `quantity` unidades y el empaque `ships_as_is`, esas unidades viajan en la caja con su peso y medidas. ProtekAuto: los 40 `caja_piezas` se vuelven 40 filas (importador, §6); el nombre «CAJA C/4» deja de engañar al comprador cuando la tienda lo decida.

### 4.5 Peso volumétrico y bultos (`addons/shared/shipspec`)

- **v1 (fase 1, hecho)**: bulto combinado con la suma exacta; un bulto por unidad de lo que va `ships_separately` (`quantity` = unidades); `requires_shipping = false` fuera; medidas del combinado `single_item` (por omisión, 1 unidad de 1 producto) o `stack` (cada unidad sobre su cara mayor, apiladas: largo = máx. lado mayor, ancho = máx. lado medio, alto = suma de lados menores; sobrestima, nunca se queda corto). `Volumetric(dims, divisor, roundUpCm)` y `Billable = max(real, volumétrico)` con pruebas, **sin aplicarse a precios** todavía.
- **v2 (fase 2)**: volumétrico opt-in en las tablas propias: `shipping_flat` (`volumetric_divisor` por tarifa, vacío = apagado) y la tabla del carrito; las paqueterías siguen recibiendo peso real + medidas (calculan su volumétrico). `max_units_per_parcel` y peso máximo por bulto del perfil parten el combinado.
- **v3 (fase 4)**: cajas estándar por almacén (`locations`: medidas internas, tara, peso máximo) y empacado 3D *first-fit decreasing* por volumen con verificación de orientación; empaques `ships_as_is` primero. Opcional y con el modo anterior como respaldo.

### 4.6 Validación (qué y dónde)

| Puerta | Qué valida | Resultado |
|---|---|---|
| Captura (manifest de `products`) | positivos, rangos (`validation.min` ya existe), medidas las tres o ninguna (regla de formulario, fase 2) | error de campo |
| Importación (OPS `catalogimport` + `dataload`) | peso ≤ 0 o ausente, medida parcial, unidad no mapeada (`unit_not_mapped`, fase 1), unidad de peso/medida declarada en el origen (fase 2) | aviso por fila en la revisión; el dato se conserva en `metadata` |
| Listo para vender (`storefront_cart` readiness) | N productos publicados y que se envían sin peso efectivo (ni propio ni heredado); N sin medidas cuando hay paqueterías en vivo | aviso con enlace a la lista filtrada (fase 2) |
| Cotización (`storefront_cart`) | peso total 0 → no se llama a nadie (`missing_weight`, #2198); nada que enviar → `nothing_to_ship` (fase 1); algunas líneas sin peso → hoy se cotiza y se registra; fase 2: ajuste `missing_weight_policy` = `block` \| `estimate` (perfil/estimación) \| `allow` (actual) | `unavailable[].code` estable + log con SKUs, sin datos personales |
| Proveedor (paqueterías) | siguen validando `weight_kg > 0` (defensa en profundidad) | `invalid_input` |

## 5. Capa por capa: qué cambia y qué NO

### 5.1 Kernel (`asteby/metacore-kernel`)

**Cambia (fase 2):**
- Paquete nuevo `measure` (stdlib, puro) con los vectores dorados de §4.1. No toca el manifest.
- `capability` (ADR 0002, minor aditiva, mismo patrón que ADR 0001), todos opcionales:
  - `ShipItemSchema`: `quantity_measure` (decimal) + `measure_unit` (código de `measure`) para líneas vendidas por medida (el `quantity` entero sigue siendo requerido: 1 línea); `ships_separately` (bool); `hs_code` (texto); `dangerous_goods` (obj `{un_number, class, packing_group}`); `weight_source` (enum de §4.3) por si un proveedor quiere distinguir estimaciones.
  - `ParcelSchema`: `weight_estimated` (bool), `package_type` (enum de §4.3).
  - Sin campos nuevos en la salida (ya existen `billable_weight_kg` y `parcels`).
- Orden obligatorio: kernel tag → OPS pinea → recién entonces un addon manda los campos (el validador rechaza campos desconocidos; `capability/schema.go`).

**Cambia (fase 4, solo si dos o más addons lo necesitan):** widget de columna `display: "measure"` con `display_config {kind, unit}` para que cualquier columna numérica se capture/muestre en la unidad del usuario sin código propio. Es un **primitivo de manifest**: exige actualizar las **dos** validaciones (`manifest/v3` y la legada `manifest/validate.go` con `validWidgets`) y los **tres bumps** (addons `tools/manifestcheck` + `tools/addon-preflight`, ops, hub; el hub primero, porque falla tarde y en silencio) antes de publicar un addon que lo use (`addons/tools/check-hub-kernel-compat.sh` lo vigila).

**NO cambia:** llaves de capacidades, campos requeridos, `money`, ningún primitivo de manifest en fases 1-3, nada de giros (autopartes, llantas) ni claves fiscales.

### 5.2 SDK (`asteby/metacore-sdk`)

**Cambia (fase 2-3):**
- `@asteby/lib`: `measure.ts` (gemelo de `measure`, mismos vectores, `bigint`), `formatMeasure(value, unit, {locale, system})` con `Intl.NumberFormat` `style: "unit"`.
- `runtime-react`: `MeasureInput` (captura en la unidad del usuario, guarda en la de la columna, sin `float`), `MeasureText`, `DimensionsInput` (las tres o ninguna), `ShippingProfileFields` (peso, medidas, tipo de paquete, se envía/por separado) reutilizado en la ficha de producto, en el perfil y en la edición masiva.
- `DynamicTable`: la acción masiva «Asignar perfil de envío» usa `extraBulkActions` (ya existe); «Editar envío en lote» reutiliza la acción declarativa *set fields* cuando aterrice (`docs/rfcs/2026-10-22-hub-primitivos/RFC-02-accion-set-fields.md`), sin primitivo propio.
- `@asteby/storefront` (tipos + mock): campos opcionales de §5.6.

**NO cambia:** el modal (header/footer fijos ya resueltos), `DynamicTable` base, autenticación ni nada de la API pública existente. Ningún componente de nicho: no hay «input de llanta».

### 5.3 Addon `products` (y `inventory` / `warehouse`)

**Cambia:**
- Fase 1: migración **012** (relleno desde `metadata`, §6).
- Fase 2: modelo `ShippingProfile` + `products.shipping_profile_id` + `categories.shipping_profile_id` (manifest; el kernel agrega columnas nullable sin default), migración **013** solo para lo que el manifest no expresa (índice parcial único de `code` entre vivos, si se decide), ajuste `default_shipping_profile`, opciones nuevas aditivas del enum `unit_of_measure`, etiquetas es/en, navegación «Perfiles de envío» bajo Catálogo, filtro guardado «Sin peso propio» (`weight_kg` vacío) en la lista de productos.
- Fase 3: `ProductPackaging`, `net_content`/`net_content_unit`, `density_g_per_ml`.

**NO cambia:** tipos ni nombres de columnas existentes (`weight_kg` en kg, medidas en cm), `metadata` (sigue de solo lectura, respaldo), `product_specs` legado de llantas, `fiscal_data` (lo sigue escribiendo el addon fiscal). `inventory` y `warehouse` **no** comparten UoM con `products` hoy (cantidades en milésimas de la unidad de venta, `shared/availability`): no cambian en fases 1-2; en fase 3 la recepción por empaque convierte `empaque × quantity` a unidades de venta al registrar el movimiento (el ledger sigue en unidades de venta).

### 5.4 OPS (`asteby-hq/ops`, host)

**Cambia:**
- Fase 1: importador `catalogimport` escribe `weight_kg`, `length_cm/width_cm/height_cm` (producto y variante) y `unit` → `unit_of_measure` validado contra las opciones del manifest (asteby-hq/ops#2114).
- Fase 2: tras el bump del kernel, el registro `catalog_row` acepta `weight` + `weight_unit` y `length/width/height` + `dimension_unit` y convierte con `measure` (Woo/Shopify en lb/in ya no requieren transformar en el perfil); avisos de revisión `missing_weight`, `partial_dimensions`; `caja_piezas` genérico → `packagings[]` del registro (fase 3).
- Ajuste de organización `measurement_system` en la configuración de región (lo fija el preset de región; capa defaults < preset < cliente).

**NO cambia:** las tablas del host `unit_of_measures` / `uom_conversions` y `products.sales_uom_id/purchase_uom_id/stock_uom_id` se **congelan** (sin consumidores en addons; 0 filas en ProtekAuto; `float64`); no se construye sobre ellas y su retiro se decide aparte tras verificar `manufacturing`/`supply_chain`. API gateway, permisos y Storefront API del host: sin cambios (la API v1 la sirve el addon `storefront_api`).

### 5.5 `storefront_cart`, `shipping_flat` y paqueterías (addons)

**Cambia:**
- Fase 1 (asteby-hq/addons#2203): `shared/shipspec` + precedencia única con procedencia, unidades en `metadata`, bultos por separado, servicios sin peso, `nothing_to_ship`, ajuste `parcel_dimensions`, `items[]` con datos físicos por unidad, foto con `parcels` y `weight_sources`.
- Fase 2: capas del perfil en `physicalOf`; **voltear la precedencia a columna antes que `metadata`** cuando `products` ≥ la versión con 012 esté instalada en todas las organizaciones (gate por `requires` de versión; con 012 aplicada el cambio no altera ningún peso); `missing_weight_policy`; aviso de «Listo para vender»; `shipping_flat` con `volumetric_divisor` opt-in.
- Fase 4: cajas estándar y bin-packing.

**NO cambia:** `missing_weight` cuando el total es 0, el ajuste `default_parcel_weight_kg`, la forma de `parcels` que ya entienden las paqueterías, el orden de precios (lista → … → envío → impuestos).

### 5.6 Starter `local` y tiendas

**Cambia:**
- Fase 1: aviso del checkout según el código (asteby-hq/local#29): `missing_weight` ofrece recoger en sucursal o escribir; `nothing_to_ship` no avisa.
- Fase 3: Storefront API v1 **solo campos opcionales** (pasa `oasdiff`): `Product.shipping?: { weight?: {value, unit}, dimensions?: {length, width, height, unit}, requires_shipping?: boolean }` y `ProductVariant.shipping?` igual; `Product.net_content?: {value, unit}` y `Product.unit_price_measure?` (precio por kg/L, útil para PROFECO y comparadores). El renderer los muestra en la ficha con `formatMeasure` según el locale del mercado; checkout sin dirección cuando todo es `nothing_to_ship` (productos digitales/servicios).
- Las tiendas derivadas (p. ej. ProtekAuto) **no copian** el cambio del starter: lo reciben al actualizar del starter, o el componente vive en `@asteby/storefront-react` si dos tiendas lo necesitan.

**NO cambia:** rutas, autenticación, tipos existentes de la API.

## 6. Migración y relleno seguros (ProtekAuto y cualquier organización)

1. **products 012** (asteby-hq/addons#2204): al actualizar `products`, copia `metadata.weight_g/weight_kg` → `weight_kg` donde la columna está vacía, y `metadata.length_cm/width_cm/height_cm` → columnas si las tres están vacías. Idempotente, nunca pisa lo capturado, solo claves genéricas. En ProtekAuto llenaría el peso de **3 795** productos; medidas, ninguna (están en `attributes.*`, que es clave de esa tienda y no se lee).
2. **Importador** (ops#2114) + **perfil de la tienda** (store-protekauto#28, draft): las medidas van a `length_cm/width_cm/height_cm` y la unidad `ST`/`KIT` a `unit`. Tras desplegar OPS, **una reimportación** de la lista (idempotente por `external_id`, la que el dueño ya corre) llena las medidas de los 3 795 productos en las columnas. No se ejecutó ninguna reimportación.
3. **Fase 2 — perfiles**: el preset de la tienda (solo configuración) crea los perfiles y los asigna a sus 4 categorías; un perfil de organización con peso estimado reemplaza a `default_parcel_weight_kg`. Ninguna fila de producto se reescribe.
4. **Fase 2 — voltear precedencia** (columna antes que `metadata`): solo cuando 1 esté aplicado; comprobable con una consulta de solo lectura `count(*) where weight_kg is distinct from metadata weight`.
5. **Fase 3 — empaques**: `caja_piezas` → `ProductPackaging` por el importador (registro `packagings[]`), no por SQL con claves de la tienda.

Nada de esto escribe en producción desde este trabajo: las migraciones corren cuando el dueño actualiza el addon.

## 7. UI de administración (siguiendo el SDK)

- **Lista de productos**: columnas Peso y Medidas formateadas por locale (`MeasureText`), indicador «sin peso» (sin peso propio; con tooltip «heredado de: Categoría Frenos» cuando aplica), filtro guardado «Sin peso propio», acciones masivas «Asignar perfil de envío» y «Editar envío».
- **Categorías**: campo «Perfil de envío» y acción «Aplicar a N productos» (solo asigna el perfil; no copia valores).
- **Perfiles de envío** (Catálogo → Perfiles de envío): CRUD automático con `ShippingProfileFields`, contador de productos que lo usan y vista previa «un carrito de 1 unidad pesa X, mide Y».
- **Ficha de producto**: sección «Envío» (ya existe) con `MeasureInput` en la unidad del usuario, procedencia visible («Peso: 1.8 kg — de la categoría Frenos; captura uno propio para este producto»).
- **Listo para vender**: «N productos publicados sin peso» con enlace a la lista filtrada.
- Copy sin eco de instrucciones; modales del primitivo compartido.

## 8. Fases, dependencias, PRs, riesgos y pruebas

### Fase 1 — hecho en esta entrega (todo mergeable por separado)

| PR | Repo | Depende de | Riesgo |
|---|---|---|---|
| addons#2203 `shared/shipspec` + carrito | addons | nada (los campos de `items[]` ya están en el kernel v0.226+; producción tiene v0.229.4) | bajo: por omisión mismo peso y mismas medidas que #2198; cambia la cotización solo con `ships_separately`, `requires_shipping=false` o `parcel_dimensions=stack` |
| addons#2204 migración 012 | addons | nada | bajo: solo rellena NULL con copia exacta de `metadata` |
| ops#2114 importador | ops | nada | bajo: columnas solo con valores válidos; sin columnas, comportamiento anterior |
| store-protekauto#28 perfil (draft) | tienda | ops#2114 **desplegado** | medio si se adelanta: con OPS viejo las medidas se ignoran |
| local#29 aviso del checkout | starter | nada | bajo |

Pruebas: vectores dorados y unitarias de `shipspec`; suite completa del carrito + `physical_test.go`; `go vet` wasip1 y guardia de imports WASI; `manifestcheck`; PG real desechable para 012; suite de `catalogimport`; JS y e2e con el `dataload` real para el perfil; `vitest` + `tsc` en el starter. No probado: paqueterías reales, una instancia con los addons nuevos, navegador.

### Fase 2 — perfiles y unidades como primitivo

Orden: (1) kernel: `measure` + ADR 0002 (`capability` aditivo) → tag; (2) ops pinea el kernel (y hub, por costumbre, aunque no hay primitivo de manifest); (3) SDK `lib/measure.ts` + `MeasureInput`/`MeasureText`/`ShippingProfileFields` → release; (4) addons, en PRs separados: `products` (`ShippingProfile`, FKs, migración **013** reservada para esto, ajuste por omisión, enum ampliado), `storefront_cart` (capas del perfil, `missing_weight_policy`, readiness, volteo de precedencia con gate de versión), `shipping_flat` (`volumetric_divisor`), copia de `measure` en `shared/shipspec` verificada por sha256; (5) ops: importador con unidades; (6) presets de región: `measurement_system`.

Riesgos: colisión de número de migración en PRs paralelos de `products` (reservar 013/014 en el plan); índice único de `code` parcial con borrado lógico (no declarar `uniqueIndex` por etiqueta GORM con el mismo nombre que un índice parcial por SQL: gana el completo y bloquea re-altas; aquí el modelo lo materializa el instalador del kernel, pero la regla aplica si algún día se espeja en el host); campos de capacidad mandados antes del bump de OPS (rechazados por el validador); volteo de precedencia antes del relleno (cambiaría cotizaciones).

Pruebas por capa: kernel (vectores, esquema de capacidad con casos válidos/ inválidos), SDK (mismos vectores en TS, pruebas de componentes con `vitest`), addons (PG real para 013, `manifestcheck`/`addon-preflight`, carrito con perfiles en las 7 capas), ops (importador con lb/in), e2e (`tools/payment-sim` / `ship-sim` del diseño de envíos cuando exista).

### Fase 3 — empaques, venta por medida, Storefront

`ProductPackaging`, `net_content`, densidad, `quantity_measure` en el contrato, Storefront API v1 opcional (`oasdiff`), ficha del starter, edición masiva, importador `packagings[]`.

### Fase 4 — empacado avanzado y primitivo de UI

Cajas por almacén y bin-packing; multiorigen (diseño de envíos §4.7); `display: "measure"` solo con dos consumidores (tres bumps + doble validación); Carta Porte (`waybill-cartaporte` lee peso resuelto → `PesoEnKg`, mercancía peligrosa del perfil, clave de producto de `fiscal_data` en cascada producto → categoría → perfil).

## 9. Alternativas descartadas

| Alternativa | Por qué no |
|---|---|
| Migrar columnas a gramos y milímetros enteros | ya son SI exactos a la resolución necesaria; romperían POS, importadores, Storefront, fiscal y presets sin ganar precisión útil |
| Unidad de peso por tienda (Woo/Magento) | cambiarla reinterpreta todos los productos; multiorganización y multimercado la vuelven ambigua |
| Unidad por fila (`weight` + `weight_unit` en cada producto) | cada lector tendría que convertir; el número canónico + unidad de captura en la UI da lo mismo sin riesgo |
| Reusar las tablas del host `unit_of_measures` / `uom_conversions` | `float64`, factor de «Box» incorrecto, nombre de registro distinto a la tabla, vacías, fuera del contrato de addons; las unidades físicas no son dato de la organización |
| Clases de envío tipo Woo (solo etiqueta) | no llevan datos físicos: siguen exigiendo capturar el peso en cada SKU |
| Perfil = solo `jsonb` en la categoría (como `categories.fiscal_data`) | sirve para un escalón, pero no para reutilizar un perfil entre categorías, asignarlo a productos ni editarlo en lote con tipos y validación |
| Herencia copiando valores al producto (backfill por categoría) | pierde la procedencia, desincroniza al cambiar el perfil y reescribe miles de filas |
| Mandar peso facturable (volumétrico) a las paqueterías | duplicaría su propio volumétrico y cobraría de más |
| Bin-packing 3D desde el día uno | complejidad y sorpresas en precio sin cajas configuradas; las reglas simples cubren el 90 % y el modo anterior queda como respaldo |
| Leer `attributes.largo_cm` en el carrito o en una migración | clave de una tienda en código general; se corrige en el perfil de importación de la tienda |
| Primitivo de manifest `measure` ya en fase 1 | tres bumps y doble validación por un solo consumidor; los componentes del SDK cubren la necesidad |

## 10. Decisiones del dueño

1. **Volteo de precedencia** columna > `metadata` en fase 2 (tras 012): ¿de acuerdo? (recomendado: sí).
2. **`missing_weight_policy` por omisión** para tiendas nuevas: `allow` (actual), `estimate` (perfil/estimación) o `block`. Recomendado: `estimate` si hay perfil, si no `block`.
3. **ProtekAuto**: ¿se vende por pieza aunque el nombre diga «CAJA C/4»? (los datos dicen que sí: precio y peso de una pieza). ¿Se renombran esos 40 productos o se modela la caja como empaque vendible?
4. **`parcel_dimensions = stack`** para ProtekAuto una vez que haya medidas en columnas: sube la precisión de la cotización con varias piezas pero puede subir el precio cotizado frente a la caja de 20×20×20 por omisión.
5. **Unidades de visualización**: ¿métrico fijo para MX y imperial para US vía preset de región, con override por organización?
6. **Tablas del host de UoM**: ¿congelar y planear su retiro?
7. **Reimportación de ProtekAuto** tras desplegar ops#2114 y mergear store-protekauto#28 (operación del dueño).

## 11. Entregado en la fase 1

- asteby-hq/addons#2203 — `shared/shipspec` + `storefront_cart` (atributos físicos con unidades y procedencia).
- asteby-hq/addons#2204 — `products` migración 012 (relleno desde `metadata`).
- asteby-hq/ops#2114 — importador: peso, medidas y unidad de venta a columnas.
- asteby-hq/store-protekauto#28 (draft) — perfil de importación: medidas y unidad.
- asteby-hq/local#29 — checkout: aviso según el código de envío no disponible.

## Fuentes

- Shopify, objeto `Weight` / `WeightUnit`: https://shopify.dev/docs/api/admin-graphql/latest/objects/weight · `InventoryItemMeasurement`: https://shopify.dev/docs/api/admin-graphql/2025-01/objects/InventoryItemMeasurement · `WeightInput` / `CustomShippingPackageInput`: https://shopify.dev/docs/api/admin-graphql/latest/input-objects/WeightInput · Perfiles de envío: https://help.shopify.com/en/manual/shipping/setting-up-and-managing-your-shipping/shipping-profiles
- Odoo, unidades de medida (categorías, unidad de referencia, razón): https://www.cybrosys.com/odoo/odoo-books/v18-ce/sales/unit-of-measure/ · Empaques y tipos de paquete: https://www.cybrosys.com/blog/how-to-manage-your-product-packages-and-packagings-in-odoo-17 · Peso máximo por tipo de paquete (OCA): https://pypi.org/project/odoo-addon-delivery-carrier-max-weight-constraint/16.0.1.0.1.2
- Medusa, Shipping Profile (v1): https://docs.medusajs.com/v1/references/entities/classes/ShippingProfile
- WooCommerce, clases de envío: https://woocommerce.com/document/product-shipping-classes/ · unidades de la tienda (`woocommerce_weight_units`): https://wp-kama.com/plugin/woocommerce/hook/woocommerce_weight_units
- Magento, unidad de peso (`general/locale/weight_unit`): https://community.magento.com/t5/Magento-2-x-Programming/How-to-change-weight-units/m-p/92757
- Shippo, parcels (`mass_unit`, `distance_unit`): https://docs.goshippo.com/api-reference/parcels/create-a-new-parcel
- EasyPost, parcels (oz/in, `predefined_package`): https://docs.easypost.com/docs/parcels
- FedEx, peso dimensional (5000 cm / 139 in): https://www.fedex.com/en-mo/customer-support/faq/invoices-and-payments/fees-and-charges/calculate-dimensional-weight.html
- SAT Carta Porte (fuentes secundarias; verificar contra el catálogo oficial): https://sovos.com/mx/cambios-regulatorios/iva/mexico-nuevo-catalogo-complemento-carta-porte/ · https://developers.sw.com.mx/wp-content/uploads/2024/06/Resumen-de-Cambios-Carta-Porte-3.1.pdf
