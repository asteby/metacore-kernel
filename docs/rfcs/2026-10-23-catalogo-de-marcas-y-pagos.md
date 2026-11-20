# RFC: catálogo de marcas (paqueterías y medios de pago) y pagos bien hechos en las tiendas

- Estado: **propuesto**. Solo documentación; no cambia código.
- Fecha: 2026-10-23
- Alcance: kernel (contrato y servicio de assets), SDK (`@asteby/storefront`, `@asteby/storefront-react`), addons (`brands` nuevo, `region_mx`, `mercadopago`, `skydropx`, `envia`, `storefront_cart`, contrato Storefront API v1), OPS (servir assets de addons), hub (distribución), starter `local` y tiendas (`store-protekauto`).
- Pedidos del dueño (literales): «plan para hacerlo pro, escalable, en local next level, reusable en MX; no sé si los addons almacenan la imagen y dónde; pero que quede bien», «para pagos haz las cosas bien», «para próximas plataformas dejemos buena arquitectura». Antes había rechazado las insignias tipográficas del SDK: «son mockups en vez de logos reales, te pedí logos».
- Se alinea con (no contradice): `RFC-DISTRIBUCION-UNIFICADA-2026-10-20-v2.md`, `RFC-RELEASE-AUTONOMO-2026-10-21.md`, `RFC-LICENCIAS-VENDEDORES-ONBOARDING-2026-10-20.md`, `RFC-PRESETS-DE-EMPRESA-Y-DISTRIBUCION.md` (capas defaults < preset < cliente), `docs/rfcs/2026-10-23-atributos-fisicos-y-envio.md` (asteby/metacore-kernel#540) y `region_mx/docs/REGION-PACKS.md` (decisión H9: nada de supuestos de país en el núcleo).
- Bases leídas (`origin/main`): kernel `41d9a8e`, SDK `2af879e6` (v0.10.0 de `storefront-react`, tras #1218/#1219), addons `d7b6b4bd`, hub `ac38370`, ops `d6a6a4dd`, store-protekauto `8f0940f` (0.3.0), local `d2c7457`. Trabajo en curso leído sin tocarlo: worktree `_worktrees/sdk-logos-reales2`, rama `feat/storefront-logos-reales-v2` (cambios sin commit al 2026-10-23).

Marcas: **[V]** verificado en código; **[S]** suposición o estimación; **[NV]** no verificado (dice qué falta).

---

## 0. Decisión en una frase

Las marcas de terceros (paqueterías, redes de tarjeta, billeteras, puntos de efectivo y bancos) pasan a ser **datos versionados de plataforma**: un **catálogo con procedencia** (clave canónica, nombre, alias, categoría, regiones, assets claro/oscuro con sha256, fuente, fecha y estado) que se publica como **un addon de datos `brands`** por el mismo plano de distribución que cualquier addon, que OPS sirve con **URLs inmutables por contenido** y que la Storefront API v1 expone con **campos opcionales**; los addons proveedores solo traducen su dialecto a la **clave canónica** (no cargan logos), `region_mx` decide **qué marcas espera el comprador mexicano y en qué orden**, el SDK **solo dibuja** (con la insignia tipográfica como respaldo para lo desconocido) y cada tienda puede ocultar o sustituir marcas por configuración, nunca copiando código.

---

## 1. Hallazgos (respuestas con evidencia)

### 1.1 ¿Los addons almacenan imágenes y dónde?

| Dónde | Qué hay | Evidencia |
|---|---|---|
| `addons/packages/<addon>/assets/logo.svg` | **Un** logo por addon: el de la **integración** (Skydropx, Envia.com, Mercado Pago, Clip, Conekta, PayPal, Stripe, 99minutos, Ysell…), no los de las marcas que esa integración expone (FedEx, OXXO…). Tamaños: skydropx 842 B, envia 833 B, mercadopago 4 544 B. Algunos traen `assets/BRAND.md` con procedencia en texto libre (titular, fuente, licencia, «revisado por Claude, pendiente de revisión humana»). | [V] `git ls-tree origin/main packages/*/assets`; `packages/skydropx/assets/BRAND.md`, `packages/envia/assets/BRAND.md`; commit addons `de9681e6` (#2190) «logos oficiales correctos en la tarjeta del Hub y auditoría de logos» |
| `manifest.json` → `metadata.icon` | `{"type":"svg","slug":"assets/logo.svg","color":"#00B1EA"}` (mercadopago) o `{"type":"lucide","slug":"Flag"}` (region_mx). Tipos válidos del kernel: `brand` (slug de simple-icons), `lucide`, `svg`, `url`. No existe ningún campo para declarar marcas de terceros. | [V] kernel `manifest/manifest.go:32-35`, `manifest/v3/validate.go:1449-1453`, `docs/abi/v1.md:89` |
| Hub (marketplace) | Al publicar, `resolveIconSVG` extrae `metadata.icon.slug` del bundle, exige ≤ 64 KiB y que empiece por `<svg`, y lo guarda **como texto en la fila del addon** (`IconSVG`). Se sirve en `GET /v1/addons/{key}/icon.svg` con CSP `sandbox`, `nosniff` y `Cache-Control: public, max-age=3600` (URL por clave, **no** por contenido). El catálogo devuelve `icon_url`. No hay sanitización más allá de esa comprobación estructural. | [V] hub `backend/internal/api/publish.go:639-700`, `bundle.go:450-480`, `addons_list.go:479-491`, `catalog.go:513-517`, `router.go:630` |
| Hub (marca de clientes) | Ya existe un **almacén direccionado por contenido** para la marca de cada cliente: `GET /v1/brand/assets/{sha256}` con `immutable`, ETag = sha, CSP sandbox, CORP cross-origin; el «brand pack» viaja **firmado** (Ed25519 con separación de dominio `metacore-brand-pack/v1`). Es el patrón a reutilizar. | [V] hub `backend/internal/api/brand_profiles.go:1-95, 988-1040`, `docs/BRAND-PACK-CONTRACT.md` |
| OPS | No sirve assets de addons al público: solo `GET /api/addons/:key/frontend.js` (`max-age=3600`) e i18n. El bundle en disco sí conserva la carpeta `assets/` (`addonbundle.AssetsDir`). La marca **del producto** (OPS, Pitsline) va embebida (`backend/productbrand`) y la de la instancia en `GET /api/platform/branding`; las imágenes de producto van por `dataload_media_*` (por organización, por sha256, con derivados). | [V] ops `backend/handlers/addon.go:796-811`, `services/addonbundle/bundle.go:45,252-261`, `productbrand/handler.go`, `services/catalogimport/media_sink.go` |
| `asteby-media-library` | **No es un repo ni un servicio**: es la carpeta local de la fábrica de contenido de video (SFX, fuentes, b-roll). No participa en runtime. | [V] `/home/user/projects/asteby-media-library/README.md`; `git` responde «not a git repository» |
| `region_mx` | Addon WASM de **datos** por organización: `region_settings`, subdivisiones, campos de dirección, CP, impuestos, catálogos SAT y **`RegionPaymentMethod`** (`MX:card`, `MX:mercadopago`, `MX:oxxo`, `MX:spei`, `MX:transfer`, `MX:cash_on_pickup`) con `kind`, `flow`, `providers`, MSI, montos, vigencia, `sort_order` e **`icon` lucide** (`CreditCard`, `Wallet`, `Store`, `Landmark`, `Banknote`). Lo publica `GET /storefront/region` (`cache: max_age 3600, swr 86400`). **No tiene assets ni marcas de terceros.** | [V] `packages/region_mx/manifest.json` (modelo `RegionPaymentMethod`, seeds en ~L2660-2770), `docs/REGION-PACKS.md` §1.1-1.3 |
| SDK v0.10.0 (`main`) | `checkout/brands.ts`: 45 marcas con color y «wordmark» corto + 24 alias; `<BrandMark>` dibuja **SVG con texto** (la insignia que el dueño rechazó) o `<img src=logo_url>` si el proveedor manda uno `https:`. | [V] SDK `packages/storefront-react/src/checkout/brands.ts`, `brand-mark.tsx` |
| Otro agente (en curso) | 28 logos reales (27 SVG + 1 PNG de Mercado Pago, 93 KB en disco) en `src/checkout/brands/assets/`, `manifest.json` con procedencia por logo (fuente, origen, licencia, fecha, color/mono), `LOGOS.md` generado, script `build-logos.mjs` que valida (sin `<script>`, sin URLs externas, con `viewBox`) y genera `logos.generated.ts` con **data URIs** (104 KB; **≈ 37 KB gzip**) importado de forma estática por `brands.ts`. `<BrandMark>` pone el logo sobre una **placa blanca con filete** (no recolorea; se ve igual en claro y oscuro). Quedan **pendientes** Paquetexpress, Redpack, SPEI/STP, Telecomm, Carnet, Correos de México, Casa Ley, Calimax, Extra, Kiosko, Farmacias Guadalajara/del Ahorro, Bodega Aurrerá y otras. | [V] lectura de `_worktrees/sdk-logos-reales2` (`git status`, `manifest.json`, `scripts/build-logos.mjs`, `gzip -c logos.generated.ts \| wc -c` = 36 818) |

**Respuesta corta:** los addons solo guardan **su propio logo** (el de la integración, para la tarjeta del Hub), como archivo en `assets/` que el hub copia a una columna de texto y sirve por clave con caché de una hora. **Nadie** guarda hoy los logos de FedEx, OXXO o Visa salvo el trabajo en curso dentro del SDK. `region_mx` es el lugar natural de **qué** medios de pago y marcas espera el comprador mexicano (ya lo hace con iconos lucide), pero **no** de los archivos de imagen.

### 1.2 Qué datos llegan hoy al renderer

| Campo | Quién lo llena y con qué | Evidencia |
|---|---|---|
| `ShippingRate.carrier` | Skydropx: `strings.ToLower(firstNonEmpty(provider_name, provider_display_name))`; el valor exacto depende de Skydropx («paquetexpress», «fedex»…) **[NV en vivo]**. Envía: `strings.ToLower(r.Carrier)`, por omisión `fedex, dhl, estafeta, paquetexpress`. | [V] addons `skydropx/backend/wasm/domain.go:587-596`, `envia/backend/wasm/envia_api.go:436-454`, `envia/backend/wasm/domain.go:36-39` |
| `ShippingRate.service_code`, `title`, `provider` | Código del servicio de la paquetería; `provider` = addon (`skydropx`, `envia`). | [V] mismos archivos; OpenAPI `contract/storefront/v1/openapi.json` (`ShippingRate`) |
| `PaymentPoint.id` / `payment_point` / `label` / `group` | Mercado Pago: catálogo fijo de 15 filas (`stp`, `oxxo`, `7eleven`, `circlek`, `soriana`, `extra`, `kiosko`, `calimax`, `farmaciasyza`, `santander`, `bbva`, `casaley`, `citibanamex`, `telecomm`, `chedraui`); `payment_point` solo cuando el método es compartido (`paycash`, `bancomer`, `banamex`). Etiquetas como «7 Eleven» y «STP (transferencia SPEI)». | [V] addons `mercadopago/backend/wasm/mpcheckout/client.go:76-111, 167-190` |
| `PaymentPoint.logo_url` | Declarado en el tipo Go y en el SDK, **nunca se llena** (`add()` no lo asigna). | [V] `client.go:55` vs `client.go:177-180` |
| `PaymentOption` | `card` (con `max_installments`), `cash` (con `points`), `wallet` y `credits` (redirect); sin redes de tarjeta aceptadas ni `label_max`. En el OpenAPI es `additionalProperties: true` (sin esquema propio). | [V] `client.go:236-258`; OpenAPI `PaymentMethods.methods[].options` |
| Redes de tarjeta | **Las escribe la tienda a mano**: `CARD_BRANDS = ["visa","mastercard","amex"]` «mientras el addon de pagos no las publique». | [V] store-protekauto `app/lib/checkout-ui.ts` |
| Normalización | En el SDK: `brandKey()` (NFD, minúsculas, solo `[a-z0-9]`, `&`→`and`, quita sufijo `mexico`) + tabla `ALIASES` (`paquetexpres`/`paqueteexpress`→`paquetexpress`, `seveneleven`/`711`→`7eleven`, `bancomer`→`bbva`…). En Mercado Pago: `pointKey()` equivalente («7-Eleven» = `7eleven`). **Dos normalizadores independientes sin vectores compartidos.** «Paquete Express» → `paqueteexpress` → alias → `paquetexpress`; «7-Eleven» → `7eleven`. | [V] SDK `brands.ts:92-133`; addons `client.go:157-165` |

### 1.3 Duplicación entre apps (regla del dueño: nunca copiar entre apps)

- `parseDelivery` vive **dos veces** y el mismo bug (`split(":", 2)` con `rate:skydropx:<id>`) se corrigió **dos veces**: store-protekauto `bba4460` (`app/lib/checkout-flow.server.ts:17-26`) y local `d2c7457` (`app/lib/delivery-choice.ts`). [V]
- El starter `local` **va detrás** de la tienda: su checkout no usa `PaymentMethodSection` ni `DeliveryOptions` (radios propios, sin pago integrado ni marcas). [V] local `app/routes/checkout.tsx:1-13, 248-275`. «Reusable en MX» exige que el starter sea el primer consumidor del SDK, no una copia vieja.
- `instructionRows`, `safeEmbedUrl`, `paymentLabel`, `CARD_BRANDS`, `HIDDEN_PAYMENT_METHODS` están en `app/lib/checkout-ui.ts` de cada app. [V]

---

## 2. Requisitos

1. **Logos reales**, sin alterar (no recolorear, no deformar), sobre placa neutra que funcione en claro y oscuro; variante para fondo oscuro solo si el titular la publica.
2. **Rendimiento**: cero peticiones a terceros en runtime (salvo el formulario del proveedor de pagos, que ya las hace); SSR sin parpadeo; **CLS 0** (caja con ancho y alto fijos desde el servidor); el JS del checkout no crece con el número de marcas.
3. **Escala**: cientos de marcas y varias regiones sin tocar código; carga solo de lo que se muestra.
4. **Contrato estable**: clave canónica única por marca, alias auditables, vectores dorados compartidos Go ↔ TS (como `money`).
5. **Actualizar y retirar sin redeploy de cada tienda**; retirada de una marca en horas, y desactivación local por tienda en minutos.
6. **Procedencia auditable**: fuente, fecha, titular, base legal (uso nominativo), guía del titular, revisor humano.
7. **Accesibilidad**: el nombre comercial siempre en texto; el logo decorativo o con nombre accesible, nunca anunciado dos veces.
8. **Multi-idioma y multi-región**: nombres por idioma cuando el titular los cambia; regiones por marca.
9. **Personalización por tienda solo como configuración**: defaults (catálogo) < preset < overrides del cliente.
10. **Nada copiado entre apps**: lo compartido va a kernel, SDK o addons.

---

## 3. Alternativas evaluadas

| | Opción | A favor | En contra | Veredicto |
|---|---|---|---|---|
| (a) | Logos **dentro del SDK** (lo que hace el otro agente) | Cero peticiones (data URI); SSR estable; rápido de entregar; ya existe con procedencia | El JS del checkout crece ≈ 37 KB gzip con 28 logos y crecería lineal con cientos; las claves llegan del servidor en runtime, así que **no hay tree-shaking posible**; retirar o actualizar un logo exige release del SDK **y** redeploy de cada tienda; mete marcas registradas regionales en una librería Apache/MIT genérica (contra «SDK general, no de nicho» y H9); el data URI se repite en el HTML cada vez que la marca aparece | **Puente** de la fase 1, con tope de tamaño; no es el destino |
| (b) | Paquete npm por región (`@asteby/brands-mx`) con catálogo y assets, tree-shakeable | Versionado claro; cero runtime; SSR estable; procedencia en el paquete | Mismo problema de fondo: las marcas que devuelve Skydropx/Envía se conocen en runtime, así que o se empaqueta todo o se hace `import()` por marca (peticiones tras hidratar, parpadeo); retirar sigue exigiendo redeploy de todas las tiendas; duplica el canal de distribución (npm) para algo que también necesitan OPS, POS y el panel | Descartada como canal principal; útil solo para **pruebas y mocks** (`@asteby/storefront/mock`) |
| (c) | Cada addon proveedor declara sus marcas en su manifest con assets propios | La integración conoce su dialecto | FedEx viviría en `skydropx` **y** en `envia`; OXXO en `mercadopago`, `stripe`, `openpay`, `conekta`…; logos inconsistentes; retirar una marca exige N releases; obliga a cada addon a duplicar logos (justo lo que el dueño no quiere) | Descartada como fuente de assets. Se conserva **solo el mapeo de dialecto → clave canónica** en cada proveedor |
| (d) | Servicio central de marcas en el hub con CDN, consumido en runtime por las tiendas | Una sola fuente; retiro inmediato | Dependencia de runtime de cada tienda hacia el hub (hoy VPS frágil, ADR hub H5); rompe self-host y air-gapped; añade un origen externo a la CSP; telemetría involuntaria | Descartada en runtime. El hub sí es la **fuente de distribución** (como para cualquier addon) |
| (e) | **Combinación**: catálogo + assets como **addon de datos `brands`** distribuido por el hub; OPS lo instala y sirve assets inmutables por contenido; la Storefront API expone claves y descriptores; `region_mx` referencia claves; proveedores emiten claves; SDK dibuja; tienda configura | Una fuente, una procedencia, una retirada; cero peticiones a terceros (todo sale del propio OPS o del origen de la tienda); JS del checkout sin assets; reusa firma, contrafirma, yank, revocaciones, visibilidad y compat del marketplace; sirve igual a tienda, POS y panel | Más piezas (addon nuevo, ruta de assets en OPS, campos en la API); la retirada depende de que las instancias actualicen el addon (ver §7) | **Elegida** |

---

## 4. Arquitectura elegida: dónde vive qué

```
            fuente (git, revisión humana)                 distribución                    runtime (por instancia)
addons/packages/brands/ ── CI: valida SVG, tamaño, ── publish ──► hub (firma dev + ──► OPS instala el addon
  catalog/*.json             procedencia, vectores                contrafirma, yank,     ├─ tabla brand_marks (seed, por org)
  assets/<key>/*.svg                                              revocaciones)          ├─ GET /api/addon-assets/{sha256}.svg (inmutable)
  PROVENANCE (por marca)                                                                 └─ GET /storefront/brands (catálogo filtrado)
                                                                                                │
addons proveedores (skydropx, envia, mercadopago…) ── emiten brand:"fedex" ───────────────────►│ Storefront API v1 (campos opcionales)
region_mx ── RegionPaymentMethod.brand / orden esperado en MX ────────────────────────────────►│
                                                                                                ▼
SDK @asteby/storefront (tipos, brandKey + vectores) · @asteby/storefront-react (<BrandMark>, ruta proxy same-origin, respaldo tipográfico)
                                                                                                ▼
starter local / tiendas: solo configuración (ocultar, ordenar, sustituir por un logo licenciado de su biblioteca de medios)
```

| Pieza | Vive en | Qué contiene | Qué NO contiene |
|---|---|---|---|
| **Contrato `asteby.com/brand/v1`** | addons `contract/brands/v1/` (junto al contrato Storefront v1): JSON Schema + `aliases.golden.json` | Gramática de clave (`^[a-z0-9]{1,40}$`), categorías (`carrier`, `card_network`, `wallet`, `cash_point`, `bank`, `bnpl`), forma del descriptor, vectores de normalización | Assets |
| **Normalizador** | Go: `addons/shared/brandspec` (stdlib, importable por guests WASM, como `shipspec` del RFC de envíos). TS: `@asteby/storefront` (`brandKey`, hoy en `storefront-react`, se mueve al paquete sin React). Ambos pasan los mismos vectores | `brandKey()` y resolución de alias contra un catálogo dado | Lista de marcas fija en código |
| **Catálogo + assets** | **Addon de datos `brands`** (addons monorepo, `packages/brands`) | Por marca: `key`, `name` (+ `name_i18n` opcional), `category`, `regions` (`["MX"]`, `["*"]`), `aliases`, `colors` (solo para el respaldo), `assets.mark` y `assets.mark_dark` opcional (`sha256`, `width`, `height`, `format`), `plate` (`light` por omisión), `provenance` (`source_url`, `origin`, `retrieved_at`, `owner`, `guidelines_url`, `basis: "nominative"`, `reviewed_by`, `reviewed_at`), `status` (`active`, `pending_review`, `withdrawn`), `withdrawn_reason` | Qué marcas ofrece una tienda (eso lo decide el servidor con proveedores y región) |
| **Qué espera el comprador MX** | `region_mx`: nueva columna `brand` en `RegionPaymentMethod` (`MX:oxxo` → `oxxo`, `MX:spei` → `spei`, `MX:card` → redes `visa,mastercard,amex,carnet`) y, si hace falta, `RegionBrandPreference` (orden y agrupación por categoría) | Claves y orden | Assets |
| **Mapeo de dialecto** | Cada addon proveedor (`skydropx`, `envia`, `mercadopago`, `stripe`…) | Su tabla `proveedor → clave canónica` (p. ej. `provider_display_name: "Paquete Express"` → `paquetexpress`; MP `paycash/7eleven` → `7eleven`; MP `clabe` → `spei`) validada contra el contrato | Logos de terceros |
| **Servir assets** | Kernel: handler `addonassets` (net/http, como `kernel/brand`): sirve `assets/**` **declarados públicos** de un addon instalado, por sha256, `immutable`, `nosniff`, CSP `sandbox`. OPS lo monta | Bytes de los assets | Lógica de marcas |
| **API** | Storefront API v1.x (aditivo, gate `oasdiff`): `GET /storefront/brands?keys=…` (catálogo filtrado por organización, región e idioma, `max_age 3600, swr 86400`, ETag) + campos opcionales `ShippingRate.brand`, `PaymentPoint.brand`, `PaymentOption.brands[]` (redes de tarjeta), `PaymentMethod.brand` | Claves canónicas y descriptores con URL de asset | URLs de terceros |
| **Dibujo** | SDK `@asteby/storefront-react/checkout`: `<BrandMark>` recibe un **descriptor** (por contexto o prop) y dibuja `<img width height>` sobre placa; el **respaldo tipográfico** solo para clave desconocida, `pending_review` o `withdrawn`. Helper de servidor `brandAssetRoute` para la tienda: proxy same-origin `/_sf/brand/{sha256}.svg` con `immutable` | Render, accesibilidad, CLS 0 | Lista de marcas ni assets |
| **Personalización** | Configuración de la organización en OPS (tabla `brand_marks` editable por la CRUD automática): `is_active`, `sort_order`, `override_media_sha256` (logo licenciado subido a la biblioteca de medios de la org) | Overrides | Código |

### 4.1 Por qué un **addon de datos** y no una tabla en `region_mx`

- Visa, Mastercard, DHL, FedEx o PayPal son de **varias regiones**; poner todo en `region_mx` obliga a copiarlas en `region_co`, `region_us`… (copiar entre paquetes de región es la deuda conocida de §4 de REGION-PACKS).
- La **gobernanza** (alta, revisión legal, retirada) es una sola para todas las regiones.
- `region_mx` sigue siendo el dueño de **qué** marcas espera el comprador mexicano y **en qué orden** (referencia claves). Si mañana hay `region_co`, solo cambia datos de región: PSE, Nequi, Efecty se dan de alta una vez en `brands` con `regions: ["CO"]`.
- Si el catálogo crece demasiado (≫ 500 marcas o > 5 MB de assets [S]), se parte en `brands` (global) + `brands_<cc>` sin cambiar el contrato.

### 4.2 Por qué la Storefront API y un proxy same-origin

- La tienda ya llama a OPS desde su servidor (`storefront(request)` con token). El navegador **no** necesita alcanzar OPS: la tienda sirve `/_sf/brand/{sha256}.svg` desde su propio origen (cabe en `img-src 'self'` de la CSP de `/pagar`, que hoy construye `payCsp` con los orígenes que declara el proveedor). [V] store `app/routes/checkout.tsx:41-48`
- La URL lleva el sha256: caché `immutable` en el CDN de la tienda y en el navegador; un logo nuevo es otra URL (sin invalidaciones).
- El loader del checkout pide el catálogo **filtrado por las claves que va a pintar** (o la API lo incrusta); el SSR dibuja `<img width height>` con las dimensiones del descriptor: sin parpadeo, CLS 0.
- Si el catálogo no responde, el SDK pinta el respaldo con el nombre comercial: **nunca bloquea el checkout**.
- Alternativa admitida para tiendas sin rutas de servidor: el descriptor puede traer `data_uri` para SVG ≤ 4 KB (decisión D5).

### 4.3 Modo claro/oscuro, tamaños y accesibilidad

- Se conserva la decisión del otro agente: **placa blanca con filete** (`--sf-brand-mark-plate`, `--sf-brand-mark-ring`), logo sin recolorear. Se añade `mark_dark` solo cuando el titular publica versión invertida; entonces la placa sigue el tema.
- Caja fija por categoría [S]: redes de tarjeta 38×24 (proporción habitual de iconos de pago), puntos y paqueterías alto fijo y ancho por proporción con tope (como `logoBox` del otro agente).
- Nombre comercial siempre en texto junto al logo; el logo `aria-hidden` o `role="img"` con nombre, nunca ambos (ya lo hace `BrandMark`).

### 4.4 Personalización por tienda (capas)

`brands` (defaults) < preset de la empresa (solo configuración: activar/ocultar, orden, qué categoría mostrar en el pie «aceptamos») < overrides de la organización (subir un logo licenciado a la biblioteca de medios y asociarlo a una clave; ocultar una marca). Una actualización de `brands` **no pisa** overrides. Las tiendas no tienen listas propias de marcas ni de redes de tarjeta.

---

## 5. Qué se hace con el trabajo del otro agente (`feat/storefront-logos-reales-v2`)

| Pieza | Decisión | Por qué |
|---|---|---|
| Los 28 assets SVG/PNG ya recortados y optimizados | **Se conservan y se mueven** (fase 2) a `addons/packages/brands/assets/<key>/mark.svg`. El PNG de Mercado Pago se sustituye por SVG oficial si existe (PNG no escala ni pasa la validación SVG) | Son el trabajo valioso; el destino es el catálogo |
| `brands/manifest.json` (fuente, origen, licencia, fecha, color/mono, nota) | **Se conserva** como semilla del campo `provenance` del contrato; se completan `owner`, `guidelines_url`, `basis`, `reviewed_by` | Ya es procedencia estructurada |
| `LOGOS.md` generado | **Se conserva** como vista generada; la fuente de verdad pasa al catálogo | |
| `scripts/build-logos.mjs` (validación anti-script/URL externa, viewBox) | **Se mueve** al CI del addon `brands` y se endurece (lista blanca de elementos SVG, tamaño máximo, sha256) | La validación debe correr donde se publica |
| `logos.generated.ts` con data URIs importado estáticamente por `brands.ts` | **Se acepta como puente** en la fase 1 **solo con** presupuesto de tamaño en CI (p. ej. ≤ 40 KB gzip el subpath `/checkout` y `/payments` [S]) y **se reemplaza** en la fase 3 por el catálogo en runtime; después queda opcional o se retira | Hace crecer el JS del checkout con cada marca y la retirada exige redeploy |
| Placa blanca, `logoBox`, `data-sf-brand-source`, accesibilidad | **Se conserva** en el SDK de forma definitiva | Es render, que sí es responsabilidad del SDK |
| Precedencia `logo_url` del proveedor > logo empaquetado > insignia | **Se cambia** en la fase 3 a: override de la organización > catálogo `brands` > `logo_url` del proveedor **solo si la tienda lo permite** (es una petición a un tercero) > insignia | Requisito de cero terceros por omisión |
| Alias nuevos (`aurrera`, `oxxopay`, `scotia`…) | **Se conservan** y pasan a `aliases` del catálogo y a los vectores dorados | |
| Pendientes (Paquetexpress, SPEI, Redpack, Telecomm, Carnet…) | Entran por el proceso de alta (§8) con revisión humana; mientras, insignia | Paquetexpress es una de las cuatro paqueterías por omisión de Envía: prioridad 1 del alta |

---

## 6. Pagos bien hechos (de punta a punta)

### 6.1 Lo que ya está bien (no se toca)

- **Idempotencia**: id de intento por carrito en `sessionStorage`, reutilizado en reintentos de red y rotado solo tras respuesta definitiva; Mercado Pago deriva `X-Idempotency-Key` estable; el efectivo con 5xx reintenta con la misma llave («no se duplicará tu pedido»). [V] SDK `use-embedded-payment.ts:130-200`; addons `mpcheckout/idempotency.go`, `online_orders.go:207, 418-423`; store `checkout.tsx:96-104`
- **Total del servidor**: el SDK nunca suma; `syncDelivery` corre con cualquier forma de pago (#1218); `price_mismatch` relee el carrito. [V] `payment-method.tsx:186-215, 236-238`
- **3DS y revisión**: sondeo por `statusUrl`, `checkoutToAttempt` traduce estados sin decidir el pago. [V] `choices.ts:124-145`
- **Rechazos**: `decline.go` reduce los códigos de Mercado Pago a motivos neutros (riesgo nunca se revela) y el SDK los muestra en `role="alert"` con foco. [V] `mpcheckout/decline.go:17-40`; SDK `payment-brick.tsx:143,176`
- **Tarjeta solo dentro del Brick** (`cardPayment`, botón oculto, el botón es de la tienda); sin JS, todo cae al flujo de redirección. [V] SDK `mercadopago.ts:53-100`

### 6.2 «Aunque ya elegí Mercado Pago pide elegir de nuevo»: qué dice el código

Hay **dos causas reales**, ninguna reproducida en navegador en esta entrega **[NV]**:

1. **La selección no persiste fuera de la vida del componente.** `PaymentMethodSection` arranca con `useState(props.defaultChoice ?? choices.defaultValue)` (solo en el montaje) y la tienda pasa `defaultChoice={defaultPayment}`, donde el loader fija `defaultPayment: payChoices.defaultValue` (el primer método `default` del servidor). La acción de la fase de domicilio (`addressFirst`) guarda contacto y dirección y redirige a `/pagar` **sin guardar la forma de pago**. Con JavaScript y navegación del router el componente sigue montado y conserva la elección; pero tras **recargar**, **volver desde Mercado Pago** con «atrás», abrir de nuevo `/pagar`, o **sin JavaScript**, la fila vuelve al valor por omisión del servidor. [V] SDK `payment-method.tsx:165`; store `checkout.tsx:44-47, 84, 89, 272`
2. **La redirección a Checkout Pro no respeta la opción elegida.** Al elegir «Cuenta de Mercado Pago» (`wallet`) o cuando la tarjeta cae a redirección, la preferencia solo fija `default_payment_method_id` para `oxxo` y créditos (`methodDefault`), sin `purpose: "wallet_purchase"` para la cuenta ni exclusión de efectivo/transferencia para la tarjeta: Checkout Pro muestra **todos** los medios y el comprador **vuelve a elegir**. [V] addons `mpcheckout/preference.go:54-60, 229-258`

### 6.3 Mejoras priorizadas

Impacto: A alto, M medio, B bajo. Esfuerzo: S días, M 1-3 semanas.

| # | Mejora | Impacto | Esfuerzo | Dónde vive |
|---|---|---|---|---|
| P1 | **Persistir la forma de pago elegida**: el SDK la guarda por `scope` (carrito) en `sessionStorage` y la reusa si sigue entre las filas publicadas; el servidor la recuerda en el carrito (`payment_choice` opcional en `PUT carts/{id}/buyer` o campo nuevo, aditivo) para recargas, otro dispositivo y sin JS. La tienda pasa `defaultChoice={cart.payment_choice ?? payChoices.defaultValue}` | A | S (SDK) + S (cart + OpenAPI) | SDK `payments/payment-method.tsx`; addons `storefront_cart`, `contract/storefront/v1`; starter |
| P2 | **Checkout Pro respeta la opción**: `wallet` → `purpose: "wallet_purchase"`; tarjeta en redirección → excluir `ticket`, `atm`, `bank_transfer`; efectivo con punto → `default_payment_method_id` del punto. **Verificar en sandbox antes de producción** (regla del dueño) | A | S | addons `mercadopago/backend/wasm/mpcheckout/preference.go` |
| P3 | **SPEI no es efectivo**: hoy STP/SPEI aparece dentro de «Efectivo» (`cashCatalog` con tipo `bank_transfer`). Fila propia «Transferencia SPEI» con CLABE y vigencia (`region_mx` ya tiene `MX:spei` con `flow: instructions`, 24 h) | A | M (opción nueva `transfer` en `PaymentOptionKey`, aditiva; revisar `oasdiff` con enums en respuestas) | addons `mercadopago`, contrato v1, SDK `choices.ts` |
| P4 | **Redes de tarjeta las publica el proveedor** (`PaymentOption.brands`), filtradas por la configuración (`excluded_payment_types`) y, si hace falta, por `GET /v1/payment_methods` de Mercado Pago cacheado; la tienda deja de escribir `CARD_BRANDS` | M | S | addons `mercadopago`; SDK; quitar de store y starter |
| P5 | **Orden y agrupación desde la región**: tarjeta, cuenta Mercado Pago, efectivo, SPEI, pagos sin tarjeta, según `RegionPaymentMethod.sort_order` (hoy el orden es el de las opciones del proveedor) y overrides del preset | M | S-M | `region_mx`, `storefront_cart`, SDK |
| P6 | **MSI honestos**: mostrar «hasta N meses sin intereses» solo si el proveedor lo confirma para el monto (promociones del emisor); si no, «a meses» sin prometer «sin intereses». `label_max` hoy no se llena | M | S-M | addons `mercadopago` (`label_max`), SDK |
| P7 | **Ficha de efectivo útil**: fecha límite en la **zona horaria de la tienda** (`voucherRows` hoy formatea en UTC y agrega «UTC»), botón copiar referencia/CLABE, código de barras, enlace a la ficha del proveedor (`external_resource_url`) y correo con la ficha | M | S | SDK `choices.ts:162-180`; store/starter `checkout-done.tsx`; `region_mx.timezone` |
| P8 | **Un solo mensaje de confianza**: hoy conviven `secureCard` del SDK, `paymentNote` y un `<p>` de la tienda con el mismo texto. Una línea con candado y «Pagos procesados por Mercado Pago» (con su logo del catálogo); sin sellos inventados | B-M | S | SDK `payment-method.messages.ts`; store `checkout.tsx` (quitar el `<p>` duplicado) |
| P9 | **Checkout del starter sobre el SDK**: `local` adopta `PaymentMethodSection`, `DeliveryOptions` y `BrandMark`; `parseDelivery`, `missingFields`, `applyCartDetails`, `buildReceipt`, `instructionRows`, `safeEmbedUrl` pasan a `@asteby/storefront-react/checkout/server` con pruebas; las tiendas borran sus copias | A (reusable en MX) | M | SDK, local, store-protekauto |
| P10 | **Móvil**: botón de pago fijo abajo con el total, `minHeight` del Brick reservado (sin salto al montar), áreas táctiles de 44 px (las filas ya cumplen en «ver más») | M | S | SDK `payments.css`, `brickProps.minHeight` |
| P11 | **Estados del botón accionables**: cuando falta entrega, el botón dice qué falta («Elige la entrega») en vez de solo deshabilitarse; tras un rechazo, foco en el aviso y el Brick listo para otra tarjeta (ya rota el intento) | M | S | SDK |
| P12 | **Telemetría de embudo** (sin datos personales): elección de fila, re-elección, rechazo por motivo, abandono en 3DS, tiempo hasta `form_ready` — para medir P1-P11 | M | S | SDK `onEvent` + tienda/OPS |

---

## 7. Arquitectura para próximas plataformas

### 7.1 Estado verificado de los RFC de distribución (en `origin/main`, 2026-10-23)

| RFC / fase | Qué decía | Estado verificado |
|---|---|---|
| Release autónomo **F0** (dejar de promover a ciegas) | Salud por instancia, `MinAdopters` fail-closed, pausa, congelación | **Implementado**: hub #680, #681, #684 (contrato de canal, cohortes, silencio), #682 (vector dorado de telemetría). [V] `gh pr view` |
| Release autónomo **F1** (la telemetría llega) | `events.jsonl` del updater, bloque `updater`/`health` | **Implementado** en OPS: #2025; retención por defecto en canary #2024; promoción vigilada #2052. [V] |
| Release autónomo F2-F5 (clasificación, frenos, IA, escala con addons) | — | **Propuesta**. [V] (sin PRs) |
| Distribución v2 **Fase 0** | Blobs inmutables, 409 al re-publicar, anti-rollback, manifest firmado del updater | **Implementado en buena parte**: ops #2003 (blobs inmutables y alias atómico), hub #669 (nginx con blobs versionados), ops #2012 (updater con manifest firmado, anti-rollback, `min_core` encadenado), hub #668 (descarga ligada a licencia), hub #686 (`HUB_BUNDLE_GATE`, **apagado en prod** según el ADR del hub, H1). [V] |
| Distribución v2 **F1'** (`kind` en `appliance_releases` + manifest canónico firmado por pipeline) | Columna `kind`, manifest `asteby.com/release/v1` | **No implementado**: no hay migración con `kind` en `appliance_releases` (la última es `0082_core_fleet_changes.sql`). [V] |
| Distribución v2 **F2** (raíz + key set) | Roles de clave, `key_id`, puentes | **Base parcial tras flags apagados**: hub #672 (sobre `asteby1.` con kid, lista de estado, contador de lease, cadena de auditoría), #691 (extraer `trustcore`, sin cambio de comportamiento). [V] `backend/internal/api/signing_v2.go:1-16` |
| Distribución v2 F4b', F4, F5', F6 | — | **Propuesta**. Visibilidad `public/unlisted/private` por addon y versión sí está (#694, cercana a F5'). [V] |
| Licencias **Fase 1** | Plazo + lease, trial sin re-emisión, activación, vendedores | **Tras flags apagados**: hub #673, #674, #675, #676. [V] Encendido en prod **[NV]**. |
| ADR «Hub sobre OPS» | Núcleo de confianza aparte; plano de control como instancia OPS (`preset hub` + `hub_fleet`) | **Propuesto**, con M1/H1 en marcha (#698, #701, #704, #705). [V] |

### 7.2 Cómo encaja el catálogo de marcas en el plano de distribución

| Pregunta | Respuesta |
|---|---|
| ¿Es un artefacto versionado más? | **Sí, y no necesita tipo nuevo**: es un **addon** (`kind: Addon`, solo datos y assets). Entra por el flujo de marketplace que el RFC v2 ya cubre (`addon`, `.mcpkg` sin código fuente), con firma del desarrollador + contrafirma central. Cuando exista F1', recibe el mismo manifest canónico que cualquier addon. |
| ¿Cómo se firma? | Igual que los addons hoy (clave de contrafirma; en F2, rol `bundle-countersign`). Los assets van dentro del bundle; su sha256 en el catálogo queda cubierto por la firma del bundle. |
| ¿Canales y rollout? | Los de addons (hoy sin canales; F5 del RFC de release autónomo los trae a addons). Una actualización de `brands` es clase `feature`; una **retirada por petición de un titular** es `hotfix` con instalación automática (§5.2 de distribución v2: clases y modos). |
| ¿Retirada? | Tres velocidades: (1) **minutos**: la organización oculta la marca (override); (2) **horas**: nueva versión de `brands` con `status: withdrawn` y sin el asset, instalación automática; (3) **emergencia**: `yanked_at` de la versión anterior + entrada en la lista de revocaciones firmada (`revocations.go`) para que ninguna instancia la reinstale. Las tiendas no redeployan porque no empaquetan logos (tras la fase 3). |
| ¿Visibilidad y licencias? | `public` (comodín `*`), porque es catálogo general. Un pack con logos licenciados para un cliente concreto sería `private` con grant (#649/#694), sin sistema paralelo. |
| ¿Assets inmutables? | Mismo principio que §4.3 del RFC v2 y que `/v1/brand/assets/{sha256}`: URL por contenido, `immutable`, sin invalidaciones; en OPS por el handler `addonassets` del kernel. |
| ¿Self-host / air-gapped? | Funciona: los assets viajan dentro del bundle instalado; el bundle offline (F6) los lleva sin cambios. |
| ¿Hub sobre OPS? | Compatible: el hub sigue siendo catálogo y almacén de bundles; el núcleo de confianza firma. Nada del catálogo de marcas depende del panel actual del hub. |
| ¿Tarjeta del Hub de cada addon? | Fase 4 opcional: `metadata.icon` con `{"type":"brand","slug":"mercadopago"}` podría resolverse contra el catálogo `brands` antes que simple-icons, para no tener el logo de Mercado Pago dos veces (`packages/mercadopago/assets/logo.svg` y el catálogo). Requiere los tres bumps de un primitivo nuevo del manifest. |

### 7.3 Reglas generales que deja este RFC para futuras plataformas

1. **Marcas de terceros = datos con procedencia**, nunca código ni copias por app.
2. **Assets por contenido** (sha256) en todo el ecosistema: hub (`/v1/brand/assets`), OPS (`addonassets`), tiendas (proxy). Un archivo cambiado es otra URL.
3. **Las integraciones traducen, no ilustran**: emiten claves canónicas de un contrato con vectores dorados Go ↔ TS.
4. **La región decide qué espera el comprador**; el catálogo dice cómo se ve; el proveedor dice qué ofrece; la tienda solo configura.
5. **Todo artefacto viaja por el mismo plano de control** (addon/preset/binario) con firma, yank y revocación; no se abren canales paralelos (npm para datos de runtime, CDN de terceros).

---

## 8. Gobernanza: alta y retirada de logos

**Alta** (PR en `addons/packages/brands`):
1. Fuente en este orden: kit de prensa o guía del titular > archivo que el titular publica en su sitio > Wikimedia Commons con licencia explícita > Simple Icons (CC0, monocromo) solo como último recurso. (Es el orden que ya usa el otro agente.)
2. Sin alterar: solo recorte de márgenes, `svgo` y quitar `width/height`. Nada de recolorear.
3. Ficha obligatoria: `source_url`, `retrieved_at`, `owner`, `guidelines_url` (si existe), `basis: "nominative"`, `notes`. Sin ficha, el CI falla.
4. CI: lista blanca de elementos SVG (sin `script`, `foreignObject`, `image` externo, `@import`, `url(http…)`), ≤ 32 KB por asset [S], `viewBox` obligatorio, sha256 calculado, vectores de alias verdes, clave única, `regions` válidas.
5. **Revisión humana** (`reviewed_by`, `reviewed_at`); hasta entonces `status: pending_review` y el SDK pinta la insignia. Los BRAND.md actuales dicen «revisado por Claude, pendiente de revisión humana»: no basta.
6. Uso: solo para identificar el servicio que la tienda ofrece («aceptamos», «envío con»), sin sugerir patrocinio; tamaño mínimo y espacio de respeto según la guía del titular (Visa y Mastercard publican reglas de marca de aceptación [S]).

**Retirada**: ver §7.2 (minutos / horas / emergencia). Registro en `CHANGELOG` del addon y en `withdrawn_reason`. Responsable y plazo objetivo: ≤ 24 h desde la petición del titular [S].

**Auditoría**: el CI genera `LOGOS.md` (vista) desde el catálogo; una tarea mensual revisa `retrieved_at` > 12 meses y fuentes caídas.

---

## 9. Fases (mergeables por separado)

### Fase 1 — puente de alto valor (S, ≈ 1 semana)

| PR | Repo | Entrega | Depende de | Prueba |
|---|---|---|---|---|
| 1a | SDK | Mergear el trabajo del otro agente (logos reales sobre placa) **con** presupuesto de tamaño en CI para `/checkout` y `/payments`, `manifest.json` de procedencia como dato exportado y `status` por logo; Paquetexpress y SPEI priorizados en pendientes | — | snapshot SSR por marca, prueba de tamaño, vectores de `brandKey` |
| 1b | SDK | **P1 (lado navegador)**: la elección se guarda por `scope` en `sessionStorage` y se reusa si sigue publicada; `defaultChoice` solo si no hay guardada | — | prueba: recargar conserva «Efectivo/OXXO»; fila desaparecida → valor por omisión |
| 1c | addons | `contract/brands/v1/` (schema + `aliases.golden.json` sembrado con las 45+ claves del SDK) — solo datos y documentación | — | prueba de esquema en CI |
| 1d | addons | **P2** en `mercadopago` (sandbox primero) | — | prueba de la preferencia + verificación manual en sandbox |
| 1e | addons | Mercado Pago: `pointKey` y etiquetas alineadas a claves canónicas (`7eleven` → «7-Eleven»), sin cambiar `id` | 1c | vectores dorados |

### Fase 2 — catálogo como addon y servicio de assets (M, 2-3 semanas)

| PR | Repo | Entrega | Depende de |
|---|---|---|---|
| 2a | kernel | Handler `addonassets` (sha256, `immutable`, CSP sandbox, `nosniff`, `If-None-Match`) + convención de assets públicos | — |
| 2b | ops | Montar `addonassets` y servir assets de addons instalados; bump de kernel | 2a |
| 2c | addons | Addon `brands` (modelo `BrandMark` con seed, assets movidos desde el SDK con su procedencia, CI de §8) + `addons/shared/brandspec` | 1c |
| 2d | addons | `GET /storefront/brands?keys=` y campos opcionales (`ShippingRate.brand`, `PaymentPoint.brand`, `PaymentOption.brands`, `PaymentMethod.brand`) en el OpenAPI v1.x; mock de `@asteby/storefront` al día | 2c |

### Fase 3 — migración de proveedores, región, SDK y tiendas (M)

| PR | Repo | Entrega | Depende de |
|---|---|---|---|
| 3a | addons | `skydropx`, `envia`, `mercadopago` emiten `brand` canónico (mapa de dialecto validado con vectores); MP publica `PaymentOption.brands` (P4) | 2c, 2d |
| 3b | addons | `region_mx`: columna `brand` en `RegionPaymentMethod`, orden esperado (P5); `storefront_cart` lo aplica | 2c |
| 3c | SDK | `<BrandMark>` consume descriptores (contexto `BrandCatalogProvider` + `brandAssetRoute`); precedencia nueva (§5); logos empaquetados quedan opcionales; `brandKey` pasa a `@asteby/storefront`; helpers de servidor del checkout (P9) | 2d |
| 3d | local | Starter sobre el SDK (P9), sin listas propias | 3c |
| 3e | store-protekauto | Borra `CARD_BRANDS`, `parseDelivery`, `instructionRows`… y usa el SDK; P8 | 3c |
| 3f | addons/SDK | P1 servidor (`payment_choice` en el carrito), P3, P6, P7 | 2d |

### Fase 4 — gobernanza y cierre (S-M)

- Proceso de §8 en `packages/brands/CONTRIBUTING.md`, revisión humana de los 28 logos actuales y de los BRAND.md de addons, tarea mensual de auditoría.
- Retiro de los logos empaquetados del SDK (versión menor con aviso; queda la insignia).
- Opcional: `metadata.icon.type = "brand"` resuelto contra el catálogo (tres bumps de kernel).

### 9.1 Riesgos y mitigación

| Riesgo | Mitigación |
|---|---|
| Uso de marca registrada sin permiso | Uso nominativo, sin alteraciones, guía del titular, revisión humana, retirada en ≤ 24 h, `pending_review` hasta revisar |
| SVG con contenido activo | Lista blanca en CI, servir solo como `<img>`, CSP `sandbox`, `nosniff` |
| Seeds idempotentes que no actualizan filas vivas (REGION-PACKS: una fila borrada reaparece) | `status: withdrawn` como columna que la actualización **sí** escribe; prueba de actualización de seed **[NV]**: confirmar si el instalador hace upsert de columnas en filas vivas |
| Retirada lenta si las instancias no actualizan addons solas | Clase `hotfix` + revocación; override por organización como vía inmediata |
| Ruptura de contrato | Solo campos opcionales; `oasdiff` en CI; enums nuevos (`transfer`) revisados aparte |
| Servir binarios desde un handler WASM | Los assets los sirve el host (kernel/OPS), no el guest |
| Bundle del checkout crece en la fase 1 | Presupuesto en CI; fase 3 lo devuelve a ≈ 0 |

### 9.2 Métricas de éxito

- CLS = 0 en `/pagar` (web-vitals, móvil y escritorio).
- JS de `/checkout` + `/payments`: fase 1 ≤ presupuesto; fase 3 ≤ +2 KB gzip respecto a v0.10.0 [S].
- 0 peticiones de imagen a terceros desde el checkout (fuera del Brick).
- Cobertura de logo real: ≥ 95 % de las tarifas y puntos mostrados en ProtekAuto (medido por `data-sf-brand-source`).
- Re-elección de forma de pago tras recargar/volver: ≈ 0 (P1, P2).
- Tiempo de retirada de una marca: ≤ 5 min por override, ≤ 24 h por catálogo.
- Cero listas de marcas o de redes de tarjeta en el código de las tiendas.

---

## 10. Decisiones del dueño

| # | Decisión | Opciones | Recomendación |
|---|---|---|---|
| D1 | Canal de los logos en runtime | A) catálogo en OPS (addon `brands`); B) npm empaquetado en cada tienda | **A**: actualizar y retirar sin redeploy, cero peso en el JS |
| D2 | Dónde vive el catálogo | A) addon `brands` (global + regiones); B) dentro de `region_mx` | **A**; `region_mx` solo referencia claves y orden |
| D3 | Política legal y revisor | Quién firma `reviewed_by`; ¿escribir a los titulares (Skydropx, Envía, Paquetexpress, OXXO) para kits oficiales? | Uso nominativo + revisión humana del dueño o de quien designe; pedir kits a los cuatro con más tráfico |
| D4 | SPEI | A) fila propia «Transferencia SPEI»; B) dentro de «Efectivo» | **A** |
| D5 | Assets en SSR | A) proxy same-origin con URL inmutable; B) data URI en el HTML | **A** por omisión, B como opción para tiendas sin rutas de servidor |
| D6 | `logo_url` del proveedor en runtime | A) ignorado salvo que la tienda lo permita; B) siempre | **A** (es una petición a un tercero) |
| D7 | Persistencia de la forma de pago | A) navegador + carrito; B) solo navegador | **A** |
| D8 | Fase 1 con logos dentro del SDK | A) aceptar como puente con tope de tamaño; B) esperar a la fase 2 | **A**: el dueño ve logos reales ya |

---

## 11. Alternativas descartadas (resumen)

- **Logos en el SDK como destino** (a): crece el JS del checkout con cada marca, sin tree-shaking posible, y la retirada exige redeploy de todo el ecosistema.
- **Paquete npm por región** (b) como canal principal: mismas limitaciones de retirada y de claves en runtime; se usa solo en mocks y pruebas.
- **Logos en cada addon proveedor** (c): duplica FedEx/OXXO/Visa en N addons y N retiradas.
- **Servicio central del hub en runtime** (d): acopla cada tienda al hub, rompe self-host y añade un origen externo.
- **Imágenes de terceros por URL del proveedor** (`logo_url` de Mercado Pago o `secure_thumbnail`): petición a un tercero en cada checkout, sin control de disponibilidad ni procedencia.
- **Biblioteca de medios de cada organización como fuente**: sirve para overrides de un cliente, pero convertiría un catálogo general en N copias por organización.
- **Insignias tipográficas como solución**: rechazadas por el dueño; quedan solo como respaldo.

---

## 12. Hechos verificados vs. pendientes

- [V] Todo lo citado con ruta y archivo en §1, §5, §6.1, §6.2 y §7.1.
- [NV] Valores exactos de `provider_name` de Skydropx en producción (no se consultó la API ni la base).
- [NV] Reproducción en navegador de «pide elegir de nuevo»; las dos causas de §6.2 salen del código.
- [NV] Comportamiento de `purpose: wallet_purchase` y exclusiones en Checkout Pro de México: verificar en sandbox antes de producción.
- [NV] Si el instalador de OPS actualiza columnas de filas de seed vivas (necesario para `withdrawn`).
- [NV] Si `HUB_BUNDLE_GATE` y los flags de licencias v2 ya están encendidos en producción (el ADR del hub dice que el gate no).
- [S] Tamaños, presupuestos y plazos marcados como estimación.
