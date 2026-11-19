# RFC: unidades organizacionales, dimensiones de filtro (scope) e identidad fiscal como aspecto del addon del país

- Estado: **Propuesto**. Es solo documentación y no hay código en este PR.
- Fecha: 2026-11-18. Revisión 3, del mismo día: agrega dimensiones y scope, el multi-país verificado de Gods y el benchmark.
- Reemplaza en parte:
  - el RFC 0001 «Entidad y alcance», que solo está en las ramas `origin/docs/rfc-entidad-alcance` y `origin/docs/rfc-estadisticas-grupo`, no en `main`;
  - la propuesta de «frontera legal en el kernel» del informe de revisión multi-entidad del 2026-11-18.
- Bases revisadas en `origin/main`:

  | Repo | Commit |
  |---|---|
  | `metacore-kernel` | eb2720c |
  | `asteby-ops` | 4a43e65 |
  | `addons` | 7d5e2703 |
  | `metacore-sdk` | (último `fetch` del día) |
  | `store-gods-nutrition` | dd6e97d |
  | `hub`, `hub-addons`, `vantora` | copias locales |

- Convención de marcas:
  - **[H]** hecho verificado en código, con su ruta;
  - **[I]** dato tomado de un informe o plan previo, sin reverificar;
  - **[P]** propuesta.
- Documentos relacionados:
  - el plan del tablero del hub (`plan-tablero-hub.md`, *scratchpad* de la sesión), que propone `params`, `?s.*` y `extraWhere` por widget;
  - `store-gods-nutrition/docs/PLAN-REGIONES.md`.

## 0. Resumen

Este RFC responde tres preguntas con una sola respuesta: dónde vive la razón social, qué es una «región» o un «país», y cómo se filtra igual cualquier tablero, lista, exportación o mapa.

1. **El kernel modela tres primitivos genéricos, sin vocabulario de negocio:**
   - **Unidades organizacionales.** Es un árbol de N niveles que responde a *quién* opera, a quién pertenece un registro y a quién se le dan permisos.
   - **Aspectos de unidad.** Son atributos tipados que un addon adjunta a una unidad, con herencia por el árbol. Por ejemplo, la dirección o el país de una sucursal, o el emisor fiscal.
   - **Dimensiones de filtro.** Son ejes declarados por los que se filtra cualquier dato: la unidad, la fecha, el país, el mercado, el territorio, el canal o la versión. Cada modelo declara cómo se mapea cada dimensión a sus columnas, y el servidor aplica el filtro en un solo lugar, con expansión de jerarquías y permisos.
2. **La razón social no es un primitivo del kernel.** Es un aspecto de referencia que pertenece al addon fiscal del país. Con esto el dueño tiene razón: «razón social por sucursal» **ya existe** en `fiscal_mexico` (PR #2090). Hay que generalizarla, quitándole los slots fijos `rs1..rs4` y el duplicado `locations.Entity`, no rediseñarla.
3. **Regla de clasificación.**
   - Si la cosa *es la organización* (quién opera o quién manda), es una **unidad**.
   - Si *describe a una unidad*, es un **aspecto**, que además puede exponerse como dimensión derivada.
   - Si *describe al dato* y no a la organización, es una **dimensión independiente**.

   Aplicada a los casos verificados:
   - En 7 Leguas, la sucursal es una unidad y el estado o país de la sucursal es un aspecto.
   - En Gods Nutrition, la «región» es el **mercado** (mx/us) del pedido, una dimensión. No son sucursales: Gods no tiene sucursales.
   - En Vantora, el **territorio** (estado, municipio, localidad) es una dimensión jerárquica de catálogo.
   - En el Hub, `country_iso` es una dimensión del registro de instancia.
4. **SDK:** un solo contexto (`useScope`, que amplía `OrgContext`) y el estado de página `?s.scope.*` / `?s.range`. Lo consumen solos todos los widgets, bloques genui, consultas y exportaciones. El evento es `scope.changed`, y un componente `ScopeBar` se dibuja a partir de las dimensiones del tenant.
5. **Ops** generaliza la función que ya es «el único lugar» del filtro por sucursal (`scopeParamsToActiveBranch`) a `ApplyScope(model, scope)`. La aplica también donde hoy **no se aplica**: los widgets del tablero (A.10). `X-Branch-ID` y `X-Entity-ID` quedan como alias.
6. **Mercado y estructura organizacional son ejes distintos que se cruzan.**
   - El **mercado** (`org_markets`) ya existe en Ops y es de primera clase: agrupa países, moneda, idioma, lista de precios, dominios y proveedores. Gods ya vende hoy en mx y us con él.
   - La **estructura** (sucursal o región) es el otro eje.
   - El scope combina `market × org_unit × canal × rango`, y `market` se **reutiliza** como dimensión: no se crea otra (C.1 bis).
   - Hueco verificado: el pedido no guarda el mercado en una columna. Por eso hoy no se pueden filtrar ventas por país ni por mercado en listas o tableros (A.8).
7. **Estructura «de nueva generación»** (sección K, con comparación frente a SAP, Oracle, Dynamics, Salesforce, NetSuite, Workday, Odoo y Bitrix24):
   - **varias jerarquías por propósito** sobre las mismas unidades;
   - **vigencia temporal** con consultas «a fecha» (*as-of*);
   - **seguridad como alcance declarativo** (ABAC), evaluada igual en Go y en TS;
   - tabla de **cierre transaccional**;
   - **un solo contrato de scope**.

   La jerarquía y el scope van en el **kernel y no en un addon**. Son seguridad transversal, y todo addon los necesita.

---

## A. Hallazgos verificados

### A.1 «Razón social por sucursal» ya existe, y vive en `fiscal_mexico`

**Extensión de la sucursal** [H]. `addons/packages/fiscal_mexico/manifest.json` (0.54.6) declara `model_extensions` sobre `target_model: "locations.Branch"` (alrededor de la L2362). Agrega al `fiscal_data` de la sucursal:

- `mx_razon_social`, con las opciones `factura_com_rs1..rs4`; vacío significa la global;
- `mx_serie_invoice`, `mx_serie_credit_note`, `mx_serie_payment_complement` y `mx_serie_carta_porte`;
- `mx_lugar_expedicion`;
- `mx_formato_impresion`.

**Cascada** [H]. `backend/wasm/emission_profile.go` hace `buildEmissionProfile(global, rs, rsKey, branchFiscal)`:

- Resuelve en el orden sucursal → `factura_com_rsN` → `factura_com`.
- `accountScopedKeys` impide mezclar las llaves de una cuenta con el RFC o la serie de otra.
- El respaldo con la cuenta global es opcional: lo activa `emision_fallback_global`.

**Lectura de la sucursal** [H]. `emission_profile_wasm.go`, con `branchFiscalDataForInvoice` en `main.go:1352`:

- Toma `invoice.branch_id`, o el de `sales_orders`, y consulta `branches.fiscal_data`.
- La nota de crédito y el REP reutilizan la cuenta con que se timbró la factura (`resolveProfileForDependent`).
- Esa cuenta queda en `provider_data.emission_profile`.

**Límite declarado por el propio código** [H]. El comentario dice: «Los slots son fijos porque el kernel aún no tiene connectors con instancias dinámicas». Eso deja 5 RFC por organización como máximo.

**Dónde viven los datos del emisor** [H]. El RFC, la razón social, el régimen y el código postal son **campos de cada connector** y se guardan cifrados en `org_integrations`.

- No hay tabla de emisores.
- El CSD lo custodia el PAC; `csd_status_wasm.go` consulta su vigencia por cuenta.
- El folio lo asigna el PAC dentro de la serie.

**Aislamiento** [H]. `fiscal_mexico` no lee `locations.Entity`. Fuera de `fiscal_mexico`, ningún paquete usa `mx_razon_social` ni `factura_com_rs*`.

### A.2 `locations.Entity` es una segunda razón social sin consumidores

[H] `packages/locations/manifest.json` 0.4.0 (commit 7e2d2ba5) agrega:

- el modelo `Entity`, con `code`, `legal_name`, `country` y `fiscal jsonb`;
- `Branch.entity_id`;
- la migración `005_entities.up.sql`, que crea la entidad `main`, un trigger y la vista `branch_entities`;
- los eventos `locations.entity_*`.

Ningún addon consume `Entity`. Las coincidencias de `entity_id` en `calendar`, `documents` y `mail` son de «entidad» en el sentido de registro, no de razón social.

`locations` también es dueño de los datos físicos de la sucursal:

- `kind` y `branch_type`;
- `city`, `state` y `postal_code` (alrededor de la L134);
- horario, recolección y almacén.

### A.3 El «grupo» no existe como dato

[H] En `asteby-ops/frontend/src/features/org-hierarchy/page.tsx`, el grupo se arma con el `organization_id` y el nombre de la organización.

- Las sucursales no tienen `parent_id`.
- `hierarchy-view.ts` fija tres niveles.
- El core de Ops tiene otro `Branch` propio (`backend/core/base/branch.go`).

### A.4 «Estructura del grupo» la inyecta el shell

[H] `/organization/structure` es una vista de solo lectura sobre `/data/Entity/me` y `/data/Branch/me`.

[H] `frontend/src/components/layout/groups-section.ts` hace tres cosas que no le corresponden al shell:

- conoce las claves i18n de un addon: `GROUPS_KEYS = locations.nav.entities|branches`;
- inyecta `/organization/structure` y `/integrations`, que muestra «Conexiones por entidad» (`ScopedConnections`, en `pages/integrations/index.tsx:213`);
- decide si se muestra con `entityCount >= 2 || versionAtLeast(locationsVersion, '0.4.0')`.

[H] `ops-app-sidebar.tsx` (alrededor de la L671) la llama así: `shouldShowGroupsSection(0, …)`.

El motivo es histórico. Ningún manifiesto permite declarar una pantalla del core con una condición organizacional.

### A.5 Por qué la sección sale casi vacía

Esto se dedujo leyendo el código; no se reprodujo en el navegador.

1. [H] `features/metacore/use-addon-sidebar.ts:652`: `CORE_PROMOTED_NAV_GROUPS = new Set(['locations.nav.group'])` descarta el grupo de `locations` de los ítems de módulos.
2. [H] `backend/services/navigation.go` (alrededor de las L144-156) publica «Sucursales» en Sistema con la clave `sidebar.branches`.
3. Por eso `extractGroupsSection` no encuentra ninguna de las dos hojas. Quedan solo las dos inyectadas.
   - «Entidades» desaparece del menú.
   - Con `orgLayoutTree`, la función retorna antes (alrededor de la L650) y la sección ni siquiera se arma.

### A.6 La fontanería de alcance del kernel usa la palabra «entity» y Ops no la conecta

Lo que hay en el kernel [H]:

- `connectors/scope.go`: `Scope{EntityID, BranchID}` y `ScopedStore`.
- `manifest/manifest.go:756-763`: `SettingScopeEntity`.
- `runtime/wasm/entity_scope.go`.
- `ConditionDef` (L1979), que solo acepta `addon_installed` y `connector_connected`.
- No hay instancias dinámicas de conector.

Lo que falta en Ops [H]:

- No envía ni lee `X-Entity-ID`.
- `ResolveScopedIntegration` no tiene llamadores.
- Lee `addon_locations.branch_members` directamente, en `appserver/server.go:1649` y `edgesync/access.go:125`.

### A.7 El SDK no tiene contexto organizacional

[H] `metacore-sdk/packages/sdk/src/plugin.ts`: `AddonAPI` no trae un contexto organizacional. `git grep OrgContext|useOrgContext|org.context.changed` no da resultados.

[I] Existen tres almacenes de sucursal activa, y cada addon copia su propio `lib/api.ts` con `X-Branch-ID`.

### A.8 Multi-país de Gods Nutrition: cómo funciona hoy

Es un **mercado comercial**, y funciona.

#### A.8.1 Qué es un mercado

- [H] `asteby-ops/backend/models/org_market.go` y la migración `database/migrations/20261017100000_org_markets.sql` definen `OrgMarket{Code, Name, Countries[], Currency, Locale, PriceList, RegionPackage, IsDefault, Active}`.
- El comentario del modelo dice: «Gods Nutrition: "mx" (MXN, godsnutrition.mx, mercadopago + region_mx) and "us" (USD, godsnutrition.com, stripe + stripe_tax)».
- También dice que el catálogo, el stock y los clientes son de la organización, y que el mercado **solo filtra** la disponibilidad, el precio, la moneda y los proveedores.
- **Un mercado ya agrupa varios países** (`countries jsonb`).

#### A.8.2 Qué decide el mercado de una petición

Lo decide la petición de la tienda, **no la sucursal**:

1. [H] La tienda enruta por prefijo (`/mx`, `/us`).
   - El comprador elige con un selector o una cookie (`market`, válida un año).
   - Si no eligió, se le *sugiere* un mercado según el país del CDN y `Accept-Language` (`app/lib/market-detect.ts`).
2. [H] La tienda envía `X-Storefront-Domain: host#mercado` (`app/lib/storefront.server.ts:151`).
3. [H] `backend/middleware/domain.go` (`DomainResolver`, `SplitMarketSelector`, `MarketSelectorSep`) valida que ese mercado exista y esté activo.
   - Si no, cae al `org_domains.market_code` del dominio verificado.
   - Y si tampoco, al `is_default` (`OPS_MARKET_DEFAULT_FALLBACK`).
4. [H] El gateway lo pone en el sobre de la petición: `appserver/api_gateway.go:158-160` (`apiroute.WithMarketResolver`). En los addons llega como `_market`.
5. [H] Con varios mercados activos, `services/market_region_dispatcher.go` enruta al `region_package` del mercado.
6. [H] La respuesta lleva `X-Storefront-Market` (`storefront_api/backend/wasm/api_domain.go:117-124`) y `Vary` (`apigateway/gateway.go:128`). Es la nota de versión «La tienda ahora recibe en cada respuesta el mercado con el que se calculó la página» (`services/releasenotes/release-notes.json:8`).

#### A.8.3 Qué depende del mercado

- [H] **Proveedores por capacidad:**
  - `capability/capability.go:21-22` del kernel dice «Providers are resolved per (organization, market, capability)».
  - El kernel tiene `capability.Market{Code, Countries}` (`capability/query.go:16`).
  - Ops guarda la selección en `org_market_providers` (`models/org_market_provider.go`; según su comentario, en la F3 1/2 solo se escribe) y la proyecta con `ResyncOrgMarketProviders` (`services/org_market_providers.go:117`).
- [H] **Precios y promociones:**
  - `customers.PriceList.market` y `DiscountRule.market` (migración `054_market_scope`), filtrados por el `_market` del sobre (`customers/backend/wasm/pricing.go:394-403`);
  - `storefront_cart` cobra con la lista de precios del mercado (`cartsvc/marketprice.go`).
- [H] **Envío, pago, geocodificación y CP:** `storefront_cart` pasa `Market` a cada `CapabilityQuery` (`cartsvc/delivery.go:421`, `address.go:152`, `checkout.go`, `admin.go:321`).

#### A.8.4 Dónde queda guardado el mercado

[H] Estas son todas las tablas que guardan `market` o `country`:

| Tabla / campo | Uso |
|---|---|
| `org_markets` | Catálogo de mercados |
| `org_domains.market_code` | Mercado de cada dominio |
| `org_market_providers.market_code` | Proveedores por mercado |
| `customers.price_lists.market` | Lista de precios del mercado |
| `customers.discount_rules.market` | Promociones del mercado |
| `storefront_cart.carts.market` | Mercado del carrito (`manifest.json:365`; si no coincide, `409 cart_market_mismatch`) |
| `storefront_cart` `CheckoutSession.market` | Mercado de la sesión de pago (`manifest.json:1244`; se escribe en `cartsvc/checkout.go:400`) |
| `shipping.market` del pedido de canal | `market` **anidado** en el bloque `shipping` que se manda a `channel_orders` (`cartsvc/shippingblock.go:88`) |
| `locations.entities.country` / `branches.state` | Atributos de la sucursal (otro eje) |
| `SalesOrder.shipping_address` (jsonb) | País o estado del envío |

[H] **No hay columna `market` ni `country` en `customers.SalesOrder`** (manifiesto: `market` solo aparece en `PriceList` L1433 y en `DiscountRule` L2056). Tampoco en `channel_orders`: en su manifiesto, `git grep market` solo encuentra «marketplace». Lo que sí tienen es `currency`.

#### A.8.5 Cómo se filtra hoy por país o por mercado en listas, reportes y tableros

[H] **No hay manera.**

- `git grep market` sobre `services/bi`, el tablero y las exportaciones de Ops no da ningún filtro de ventas por mercado.
- `qualifyMarketViewTitles` (`features/metacore/use-addon-sidebar.ts:761`) solo pone el título a las vistas de *ajustes* por país de los addons de región.
- Las únicas aproximaciones posibles son la moneda (MXN o USD), la sesión de pago, o la ruta jsonb `shipping_address`.

La afirmación del dueño es **cierta para la operación de la tienda** (precios, moneda, impuestos, métodos de pago, contenido y dominio por mercado). **No se cumple para el análisis de ventas por país**: falta sellar el mercado en el pedido como dato de primera clase.

#### A.8.6 Gods y las sucursales

- [H] Los *mocks* de la tienda (`app/mock/gods.ts:288`, `gods-us.ts:267`) traen `branches: []`.
- [H] La sucursal solo aparece en la recolección en tienda (`checkout-flow.server.ts:78`). `channel_orders` lleva esa sucursal a `SalesOrder.branch_id` (`shippingblock` / `checkout.go`, comentario «channel_orders carries a locations.Branch to SalesOrder.branch_id»).

**Relación con lo fiscal.**

- [H] El mercado elige el **addon de país**: `OrgMarket.region_package` (`region_mx`) y los proveedores de `fiscal.invoice` por mercado (`org_market_providers`, dominio de capacidad).
- [H] Dentro de `fiscal_mexico`, el **emisor** lo elige la **sucursal** (A.1).
- Para un pedido web sin sucursal, el emisor sale del global.

Así que mercado y sucursal ya actúan como ejes distintos: el mercado elige *qué país o proveedor* atiende, y la unidad elige *qué emisor* dentro de ese proveedor.

#### A.8.7 Lo que no verifiqué de Gods

- Los datos de producción: cuántos mercados activos tiene y si los pedidos traen `shipping.market`.
- Si `channel_orders` o `customers` copian `shipping.market` a alguna columna al materializar el pedido. Solo busqué `market` en sus manifiestos y backends.
- Qué quiere decir exactamente el dueño con «regiones» (decisión H.9).
- El archivo `/opt/ops-hub/current/release-notes.json` del VPS. Usé la copia del repo.

### A.9 7 Leguas, Vantora y Hub

**7 Leguas.**

- [H] Las sucursales son filas de `locations.Branch`, con `state` y `city`.
- [I] Según el commit del RFC 0001, tiene «RFC 1 con 3 sucursales, RFC 2 con la sucursal de canal de Mercado Libre».
- No verifiqué en datos cuántos estados o países tiene. Supongo [S] que solo opera en México.

**Vantora.**

- [H] El territorio es un **catálogo** con niveles estado, municipio y localidad (`backend/core/gobiernos/territory.go:69`, `/territorio/:nivel/:clave` en `appserver/server.go:1328`, INEGI en `core/gobiernos/inegi.go`).
- [H] Los registros lo referencian por columna. Por ejemplo, `Municipio string` en `core/gobiernos/intel.go:98`, o `territorio` en los payloads de alerta (`automations_wire.go:155`).
- El territorio **describe el dato** (dónde ocurrió el hallazgo), no a la organización que lo opera.

**Hub.**

- [H] `hub/backend/internal/geo/geo.go:29` y `internal/api/telemetry.go:334` toman `CF-IPCountry` (y como respaldo `X-Country-Code` y `X-Vercel-IP-Country`).
- [H] `hub-addons/packages/hub_fleet/manifest.json:434` guarda el resultado en `HubInstance.country_iso`.
- [I] Según el plan del tablero, se filtra además por `vertical`, `channel` y `ops_version`.
- Son atributos **del registro instancia**. Las instancias no son unidades del operador del Hub.

### A.10 El filtro por sucursal tiene un «único lugar» en Ops, pero el tablero lo salta

[H] `asteby-ops/backend/handlers/branch_scope.go:154-190`: `scopeListToActiveBranch` / `scopeParamsToActiveBranch` es «the ONE place» que convierte `c.Locals("branch_id")` en filtros. Lo usan `/data/:model`, `aggregate`, `query_batch` (`/api/q`, en `appserver/server.go:2945`) y las exportaciones («so exports follow exactly the same rule as the list»).

- Si el modelo no tiene `branch_id`, no inyecta nada. La columna se detecta por nombre (`modelHasField(…, "branch_id")`, y `modelHasBranchID` usa reflexión en `export.go:1726`).
- Si el usuario está restringido, o el modelo es estricto, aplica `f_branch_id = <activa>`.
- En otro caso aplica `branch_id = X OR branch_id IS NULL`.
- `lookupBranchContext` cubre las opciones, la búsqueda y las facetas.

[H] `services/branch_visibility.go`: los modelos «estrictos» salen de variables de entorno (`OPS_BRANCH_STRICT_MODELS`, `OPS_BRANCH_UNASSIGNED_VISIBLE`), no del manifiesto.

[H] `middleware/branch_scope.go`: el filtro admite **una sola** sucursal activa.

- Un usuario restringido con «Todas» recibe su sucursal por omisión; no puede ver la unión de sus sucursales.
- No existe la noción de un conjunto ni de un subárbol.

**[H] Los widgets del tablero no aplican el alcance por sucursal.**

- `handlers/dashboard_widgets.go:42-60`, `GetWidgetsData`, pasa solo `orgID` y `userID`.
- `services/dashboard_catalog.go:298`, `computeWidget(…, extraWhere)`, no menciona `branch` en ningún punto. `extraWhere` solo se llena desde `view_chart.go:59`, el panel de detalle, con igualdades.
- Resultado: un usuario restringido a una sucursal ve KPIs de toda la organización en el Tablero, mientras sus listas y exportaciones sí se filtran.

**[H] Del lado del kernel:**

- `query/aggregate.go:54`: `AggregateSpec.Where map[string]any` solo admite igualdades y no tiene noción de alcance.
- `dynamic/facets.go:64`: `Service.Facets` ya devuelve los valores distintos de una columna con su conteo, «org/branch-scoped». Es la base natural para los valores de una dimensión. También provoca un **choque de nombres**: «faceta» ya significa eso en el kernel (ver J.3).

[H] Frontend: `features/metacore/page-url-state.ts` ya reserva el prefijo `?s.` para el estado de página genui.

[I] Según el plan del tablero, las fuentes genui tienen `args` escalares fijos sin `$ref`, y el plan propone `params: ["filters.country_iso", …]` por widget.

---

## B. Dónde vive cada cosa

### B.1 Decisiones

1. **La razón social no es parte del kernel.** El kernel no tiene «frontera legal». La identidad fiscal es un **aspecto de referencia** que pertenece al addon fiscal del país, y se expone a los demás addons con el contrato `legal_identity.resolve@1`.
2. **Los filtros no son parte del contexto organizacional: son dimensiones.** La unidad organizacional es *una* dimensión más, la única que además está **ligada a permisos**.
3. **El servidor aplica el filtro en un solo lugar,** para todos los consumidores: listas, agregados, widgets, `/api/q`, exportaciones, mapas, búsquedas y facetas.

### B.2 Tabla de responsabilidades

| Pieza | Kernel | SDK | Ops | `locations` | Addon fiscal del país | Preset de industria / addon de dominio |
|---|---|---|---|---|---|---|
| Árbol de unidades (`org_units`, cierre, N niveles) | **Dueño** (tablas, servicio, eventos) | Tipos TS | Puertos y API `/org/units` | Declara el tipo `branch` | — | Declara los tipos de unidad y su vocabulario (región, planta, dependencia) |
| Jerarquías por propósito, vigencia y bitácora de cambios (`org_hierarchies`, `org_unit_edges`, `org_structure_log`) | **Dueño** | Componente de árbol con línea de tiempo | Interfaz de administración generada | — | — | Declara los propósitos (operational, management, geographic) |
| Políticas de alcance (ABAC, `scope_policies`) | **Lenguaje y evaluador (Go)**; caché de `allowed` | Evaluador TS de solo lectura | Persiste las políticas del administrador; simulador «¿qué ve este usuario?» | — | — | Declaran políticas por omisión |
| Mercado (`org_markets`, proveedores por mercado) | `capability.Market` (ya existe) | Lo expone como dimensión `market` | **Dueño** (ya existe), y declara la dimensión | — | Su `region_package` lo elige el mercado | `storefront_cart` y `channel_orders` sellan `market_code` |
| Aspectos de unidad (esquema, herencia) | **Mecanismo** | Editor genérico | — | Aspecto `locations.site`: dirección, `country`, `state`, horario, `branch_type` | Aspecto `fiscal.issuer`: referencia al emisor, series y lugar de expedición | — |
| Emisor / razón social | — | — | — | (deja de tener `Entity`) | **Dueño**: `Issuer` y conectores | — |
| `legal_identity.resolve@1` | Forma del contrato | Tipos y hook | Proveedor por omisión (la organización) | — | Proveedor | — |
| Instancias de conector | **Mecanismo** `instances` | Superficie de conexiones | `org_integrations.instance_ref` | — | `factura_com` con `per_record(Issuer)` | Pagos: `per_scope` |
| **Dimensiones** (declaración, tipos y fuentes de valores) | **Dueño del contrato y del compilador de filtros**; dimensiones base `org_unit`, `date` y `country` | Tipos, `useScope` y `ScopeBar` | Endpoint de catálogo `/api/scope/dimensions` | Dimensión derivada `org_unit.country` / `org_unit.state` desde su aspecto | — | Declaran sus dimensiones (`market` en el core de Ops; `territory` en Vantora; `vertical`, `channel` y `ops_version` en `hub_fleet`) |
| **Mapeo modelo → dimensión** | Valida el mapeo y compila el SQL | — | — | Declara el de sus modelos | Declara el de sus modelos | Cada addon declara el de sus modelos |
| **Aplicación del filtro y de los permisos** | `scope.Compile(model, ScopeFilter, Allowed)` | Lo envía en cada consulta | **`ApplyScope`**, único, en todos los *endpoints* | — | — | — |
| Estado de filtro de la página | — | **Dueño**: `?s.scope.*`, `?s.range` y `scope.changed` | Adaptador de *router* (ya existe) | — | — | — |
| Navegación con condición | `ConditionDef` con predicados organizacionales | Evaluador TS | Solo renderiza | Contribuye «Sucursales» | Contribuye «Razones sociales» | Cada uno contribuye lo suyo |

---

## C. Dimensiones y filtros (scope)

### C.1 Modelo: ¿nivel del árbol, aspecto o dimensión?

Para decidir dónde va una cosa se aplican cuatro preguntas, en este orden:

1. **¿Se le asignan personas o permisos? ¿Es dueña de registros (un registro «pertenece» a ella)?** Si la respuesta es sí, es una **unidad** del árbol. Ejemplos: sucursal, planta, dependencia, y una región administrativa con su gerente.
2. **¿Describe a una unidad y la usa la configuración** (qué emisor, qué dirección, qué país de operación)? Entonces es un **aspecto** de la unidad. Si además sirve para filtrar, se publica como **dimensión derivada**: se filtra el dato a través de la unidad a la que está anclado.
3. **¿Describe al dato y no a la organización?** Es decir, cosas como dónde se vendió, a qué mercado, de qué territorio trata o qué versión reporta. Entonces es una **dimensión independiente**, con su propia fuente de valores.
4. **¿Es un intervalo continuo** (fechas, montos)? Entonces es una dimensión de tipo `date` o `range`.

**Aplicación a los casos reales:**

| Caso | Concepto | Clasificación | Por qué | Evidencia |
|---|---|---|---|---|
| 7 Leguas | Sucursal | **Unidad** (`branch`) | Tiene miembros (`branch_members`), es dueña de los documentos (`branch_id`) y define el emisor | A.1, A.10 |
| 7 Leguas | Estado / país de la sucursal | **Aspecto** `locations.site` → dimensión derivada `org_unit.state` / `org_unit.country` | Describe a la sucursal; nadie «pertenece» a un estado | `locations` `state` (A.2) |
| 7 Leguas | Zona o región comercial con gerente (si existe) | **Unidad** de agrupación (tipo `region` del preset) | El gerente de zona ve el subárbol | No verificado en datos |
| Gods | «Región» = mercado mx/us, cada uno con sus países, moneda, precios, proveedores y dominio | **Dimensión `market` de primera clase, que ya existe** (`org_markets`), jerárquica mercado → país | El catálogo, el stock y los clientes son de la organización; el mercado solo filtra (comentario de `OrgMarket`), y no hay sucursales | A.8 |
| Gods | País o estado del envío | **Dimensión independiente** `country` (y `ship_state`, si se necesita) mapeada a `shipping_address` | Describe al pedido | A.8 |
| Vantora | Territorio estado → municipio → localidad | **Dimensión jerárquica de catálogo** `territory` | Describe al hallazgo o a la encuesta; la dependencia que lo analiza es otra cosa | A.9 |
| Vantora | Dependencia / área | **Unidad** (tipo del preset gobierno) | Personas y permisos | [I] |
| Hub | `country_iso` de la instancia | **Dimensión** `country` mapeada a una columna | Atributo del registro, salido de `CF-IPCountry` | A.9 |
| Hub | `vertical`, `channel`, `ops_version` | **Dimensiones categóricas** del addon `hub_fleet` | Atributos del registro | [I] plan del tablero |
| Todos | Fecha | **Dimensión** `date` (rango) | Intervalo; cada modelo declara su columna | — |
| Todos | Razón social | **Aspecto** `fiscal.issuer`, que también puede servir como dimensión derivada `org_unit.issuer` | Describe a la unidad y no es dueña de personas en v1 (H.3) | A.1 |

Con esto, **«regiones o sucursales por país»** queda formulado sin ambigüedad:

- **Sucursal** es una unidad.
- **Región de la organización** es una unidad de agrupación, *solo* si tiene responsables y permisos.
- **País de una sucursal** es un aspecto, y se filtra como `org_unit.country`.
- **Región o país de venta** (el mercado, o el destino del envío) es una dimensión del dato.

Una misma palabra, «región», puede significar cualquiera de los cuatro según la industria. Por eso **el vocabulario lo pone el preset** y el kernel solo ve claves de dimensión.

### C.1 bis Matriz: mercado × estructura × canal × rango

**Los ejes son independientes y se cruzan.** Una venta tiene a la vez:

- un **mercado**: dónde y en qué condiciones comerciales se vendió (país, moneda, lista de precios, proveedores, dominio);
- una **unidad**: quién la operó (sucursal, región o zona; `branch_id`);
- un **canal**: `web`, `pos`, `mercadolibre` o `amazon` (`channel_orders`);
- una **fecha**.

Algunos ejemplos de cómo se cruzan:

- 7 Leguas vende en un solo mercado (mx) desde varias sucursales y canales.
- Gods vende en dos mercados (mx y us) sin sucursales, salvo la de recolección.
- Una cadena futura podría tener sucursales en dos países: entonces cada sucursal tendría un **mercado por omisión** como aspecto (D.2), y la venta en mostrador heredaría ese mercado.

**Regla de combinación** (en `scope.Compile`):

- `AND` entre dimensiones distintas y `OR` entre los valores de una misma dimensión.
- Las jerárquicas se expanden. `market` es jerárquica: mercado → sus `countries`, tomados de `org_markets.countries`.
- Filtrar `market=us` usa la columna `market_code` del dato.
- Filtrar `country=US` usa la columna de país mapeada (por ejemplo, `shipping_address.country`).
- Si un modelo solo mapea `market`, un filtro por país se traduce al mercado que contiene ese país, **solo** si el país pertenece a un único mercado. Si no, el filtro queda en `meta.ignored`. Esto asume la regla «un país en un solo mercado», que no está confirmada (decisión H.14).

**Permisos por eje.**

- `org_unit` siempre está ligada a permisos.
- `market` **puede** estarlo, mediante una política ABAC (K.4). Por ejemplo, «el gerente de EE. UU. solo ve `market=us`».
- Las demás dimensiones son, por omisión, solo de filtro.

**Qué son las «regiones» de Gods.** Por omisión, se tratan como **el mercado** (H.9): ya es un grupo de países con su comercio propio, igual que un *market* de Shopify o una *region* de Medusa. Esa comparación viene de las referencias de `asteby-ops/docs/plan-next-gen/DISENO-MERCADOS.md` y es [S].

Hay otros dos casos posibles, cada uno con su forma:

- Si Gods necesita **agrupar mercados** (por ejemplo, «Norteamérica» = us + ca), se agrega `parent_code` a `org_markets`, o una jerarquía de propósito `commercial` sobre los mercados (K.4). La dimensión `market` sigue siendo una sola.
- Si necesita **regiones dentro de un país** (Norte o Bajío según el estado del envío), se crea la dimensión `ship_region`: un catálogo de la organización que asigna estados a regiones, mapeado a `shipping_address.state`.

Ninguna de las dos es una sucursal. Solo si Gods abriera tiendas físicas con gerentes aparecerían unidades de tipo `region` del preset retail.

### C.2 Contrato de dimensión (kernel)

La **declaración** vive en el manifiesto de un addon, de un preset o del core de Ops:

```json
"dimensions": [
  {
    "key": "market",
    "type": "categorical",
    "label": "dim.market",
    "values": { "source": "catalog", "model": "OrgMarket", "value": "code", "label": "name" },
    "multi": true,
    "vocabulary": { "retail": "dim.market", "ecommerce": "dim.region" }
  },
  {
    "key": "territory",
    "type": "hierarchical",
    "label": "gobiernos.dim.territory",
    "values": { "source": "catalog", "model": "Territorio", "value": "clave", "label": "nombre",
                "parent": "clave_padre", "levels": ["estado", "municipio", "localidad"] }
  }
]
```

Los tipos son:

- **`hierarchical`**: valores en árbol. Seleccionar un nodo **expande su subárbol** en el servidor.
- **`categorical`**: valores discretos.
- **`range`**: numérico, `{gte, lte}`.
- **`date`**: `{preset: "7d"} | {from, to}`, con intervalo derivado.

Las fuentes de valores (`values.source`) son:

- **`org_units`**: el árbol. Es la fuente de la dimensión base `org_unit`, que **está ligada a permisos**.
- **`aspect`**: valores de un aspecto de unidad, por ejemplo `locations.site.country`. Produce dimensiones derivadas como `org_unit.country`.
- **`enum`**: las `options` de una columna.
- **`catalog`**: un modelo que hace de catálogo, como `OrgMarket` o el territorio INEGI. Admite `parent` si la dimensión es jerárquica.
- **`distinct`**: valores distintos de la columna mapeada, vía `dynamic.Facets`. Es el caso de `ops_version`.

El kernel trae **dimensiones base**, cuyas claves son estables entre addons para que el selector y la URL sean los mismos en todas partes:

- `org_unit` (jerárquica, desde `org_units`, ligada a permisos);
- `date` (fecha);
- `country` (categórica, catálogo ISO 3166-1 que trae el kernel).

Cualquier otra dimensión es de un addon o de un preset y lleva su prefijo de addon cuando es propia (`hub_fleet.vertical`).

**`market` no se crea de nuevo: se reutiliza.**

- Lo declara el **core de Ops** como dimensión de plataforma, con fuente `catalog: OrgMarket` (A.8). Es jerárquica: mercado → `countries`.
- El kernel ya conoce `capability.Market{Code, Countries}` y lo resuelve por petición. El scope solo lo *expone* para filtrar datos.
- `channel` (`web`, `pos`, `mercadolibre`, `amazon`) lo declara `channel_orders` con fuente `enum`.

**Consulta «a fecha».** `ScopeFilter` admite `{"$as_of": "transaction" | "current" | "<fecha>"}`:

- `transaction`, el valor por omisión, agrupa cada dato con la estructura vigente **cuando ocurrió**;
- `current` lo reagrupa con la estructura de hoy, por ejemplo después de una reorganización de regiones (K.4).

El **mapeo por modelo** también va en el manifiesto, dentro de `models[]`:

```json
"scope": {
  "org_unit": { "column": "branch_id", "unassigned": "visible" },
  "date":     { "column": "created_at", "default": true },
  "market":   { "column": "market_code" },
  "country":  { "path": "shipping_address.country" },
  "org_unit.country": { "via": "org_unit" }
}
```

- **`column`**: columna directa.
- **`path`**: ruta en `jsonb`, validada con la misma regla que `dynamic.Facets`.
- **`via: org_unit`**: el filtro se resuelve a un conjunto de unidades, cuyo aspecto coincide, y se aplica sobre la columna de `org_unit`. Así «ventas de sucursales de Jalisco» no exige que cada venta tenga un campo `state`.
- **`unassigned: visible|hidden`**: reemplaza `OPS_BRANCH_STRICT_MODELS` y `BranchUnassignedVisible`, que hoy salen de variables de entorno.
- Si un modelo **no mapea** una dimensión activa, el filtro **no aplica** y el resultado lo dice: `meta.applied` y `meta.ignored`. Nunca se rellena con un valor inventado. Un widget que ignora un filtro muestra la marca «no aplica a este dato».

El **filtro** es un valor tipado:

```ts
type ScopeFilter = {
  [dimension: string]:
    | { in: string[]; subtree?: boolean /* hierarchical: default true */ }
    | { gte?: number; lte?: number }
    | { preset?: string; from?: string; to?: string; interval?: 'minute'|'hour'|'day'|'week'|'month' }
}
```

El **compilador** es puro y vive en el kernel:

```text
scope.Compile(model, filter, allowed) → []Clause, Meta
```

1. **Permisos primero.** `allowed` es el conjunto de unidades que el usuario puede ver, que el host resuelve con las membresías y su herencia.
   - El filtro efectivo de `org_unit` es `filter.in` expandido, intersectado con `allowed`.
   - Si la intersección queda vacía, hay dos casos: si el usuario la pidió explícitamente, se responde `403 scope_forbidden`; si viene de una URL compartida, el resultado es vacío y lo indica `meta.clamped`.
   - Si el usuario no tiene restricción, `allowed` vale `*`.
2. **Expansión jerárquica** con la tabla de cierre (`org_unit_closure`, o un cierre del catálogo). Se hace con `EXISTS`/`JOIN` contra el cierre, no con listas `IN` gigantes.
3. Una cláusula por dimensión mapeada, con columnas y rutas validadas (`safeColumn`).
4. `Meta{applied[], ignored[], clamped bool, cache_key}`.

`AggregateSpec` gana un campo `Scope []Clause`. `Where` se queda como está, con igualdades fijas del widget.

### C.3 SDK

**Contexto único.** `useScope()` amplía `OrgContext`:

```ts
interface ScopeContext {
  org: OrgContext['org']
  dimensions: DimensionInfo[]        // las disponibles para el tenant (catálogo del host)
  allowed: { org_unit: string[] | '*' }
  filter: ScopeFilter                // efectivo (URL ∪ defaults de página ∪ preferencia del usuario)
  set(dim: string, value: ScopeFilter[string] | null): void
  reset(): void
}
```

**Estado de página.**

- `?s.scope.<dim>=<valor>` y `?s.range=7d`, sobre el prefijo `?s.` que ya existe (`page-url-state.ts`). Ejemplo: `?s.scope.market=us`, o `?s.scope.org_unit=<id>`.
- La **unidad activa** del selector de sucursal es solo `s.scope.org_unit` con persistencia por usuario. El branch-switcher deja de ser un mecanismo aparte.

**Consumo sin código por tablero.**

- Toda fuente genui (`q`, `widget`, `query`), `DynamicTable`, exportación y mapa recibe el `ScopeFilter` efectivo **de forma implícita**.
- Un bloque puede declarar `scope: "ignore"`, por ejemplo un KPI que compara contra toda la organización, o `scope: {only: ["date"]}`.
- Con esto se reemplaza el `params: ["filters.country_iso", …]` *por widget* del plan del tablero. Ese plan sigue siendo válido para la **selección** cruzada, como `state.selection.instance`.

**Evento.**

- `scope.changed {prev, next, changed: string[]}`, en el bus del runtime.
- Las consultas se vuelven a pedir solo si alguna de las dimensiones cambiadas está entre sus `meta.applied`.
- `org.context.changed` se mantiene para el cambio de organización, de membresía o de estructura.

**Selector genérico.**

- `<ScopeBar dimensions? compact?>` vive en `metacore-ui` / `runtime-react`.
- Se dibuja a partir de `GET /api/scope/dimensions`:
  - una jerárquica se muestra como árbol con búsqueda;
  - una categórica, como un combo múltiple con conteos (`dynamic.Facets`);
  - una de fecha, con los rangos del tablero;
  - los filtros activos aparecen como *chips* con ✕.
- Solo muestra las dimensiones con 2 o más valores visibles para el usuario. Con una sola sucursal y un solo mercado, la barra queda vacía y no ocupa espacio.

**Cabeceras.** El cliente del SDK envía `X-Scope` (el `ScopeFilter` en JSON compacto, o un *hash* si es largo) en las consultas que no lo llevan en el cuerpo. `X-Branch-ID` se sigue enviando durante la transición.

### C.4 Ops

**`ApplyScope`, en un solo lugar.**

- `scopeParamsToActiveBranch` y `lookupBranchContext` (A.10) se convierten en `ApplyScope(c, model) → ctx con []Clause`.
- La consumen:
  - `/data/:model`;
  - `aggregate`;
  - `/api/q` (`QueryBatch`);
  - las exportaciones síncronas y asíncronas (el trabajo guarda el `ScopeFilter` **y el `allowed` evaluado al crearse**);
  - opciones, búsqueda y facetas;
  - **`/dashboard/widgets/data`** (nuevo: corrige A.10);
  - `view_chart` y las consultas de mapa.
- El cuerpo de `widgets/data` acepta `{keys, scope}`, y `computeWidget` pasa `spec.Scope = Compile(…)`. `extraWhere` queda para el panel de detalle (una igualdad por clave foránea), que es para lo que existe.

**Alias.**

- `X-Branch-ID: <id>` equivale a `scope.org_unit = {in:[id], subtree:false}`.
- `"all"` o la cabecera ausente equivalen a no filtrar dentro de `allowed`. Esto **corrige** que hoy un usuario restringido no pueda ver la unión de sus sucursales.
- `X-Entity-ID: <id>` equivale a `scope["org_unit.issuer"] = {in:[id]}`, con `via: org_unit`. Se depreca.
- `c.Locals("branch_id")` se sigue llenando, para el código de Ops que lo lee, mientras dure la migración.

**Permisos.**

- `allowed` sale de `org_unit_members`, con herencia hacia abajo (`inherit_down`). Mientras F6 no esté lista, sale de `branch_members`.
- El cálculo falla cerrado, igual que hoy: si no se puede resolver, responde `503 scope_unavailable`.
- Los dueños y administradores tienen `allowed = *`.

**Caché y cardinalidad.**

- La clave de caché es `(org, hash(allowed), hash(filter canónico), clave, versión de estructura)`. La versión de estructura sale de `org.structure.changed` e invalida las expansiones.
- TTL de 10-15 s con *singleflight* en los widgets, como propone el plan del tablero [I].
- Las expansiones de subárbol se cachean por `(org, nodo, versión)`.
- Con más de 200 valores, la dimensión pasa a búsqueda incremental (`dynamic.Facets` con `Q`).
- Una selección de más de 500 valores se rechaza con `scope_too_wide`, salvo que sea un subárbol: los subárboles se resuelven por `JOIN` con el cierre.
- El conteo de cada valor en `ScopeBar` es opcional y perezoso.

**Catálogo.** `GET /api/scope/dimensions` responde con:

- la unión de las dimensiones declaradas por los addons instalados;
- las dimensiones base;
- para cada una: tipo, etiqueta (con vocabulario del preset y `org_terminology_overrides`), si tiene 2 o más valores visibles, y los modelos que la mapean.

### C.5 Addons: participar en una dimensión y añadir dimensiones propias

Un addon **participa** declarando `scope` en sus modelos (C.2). Un addon **añade** dimensiones declarando `dimensions[]`.

**7 Leguas** (sucursal y país): con `locations`, `customers` y `fiscal_mexico` instalados no hace falta código propio.

```json
// locations
"dimensions": [
  { "key": "org_unit.country", "type": "categorical", "values": { "source": "aspect", "aspect": "locations.site", "field": "country" } },
  { "key": "org_unit.state",   "type": "categorical", "values": { "source": "aspect", "aspect": "locations.site", "field": "state" } }
]
```

```json
// customers.SalesOrder
"scope": { "org_unit": { "column": "branch_id" }, "date": { "column": "created_at", "default": true },
           "org_unit.country": { "via": "org_unit" }, "org_unit.state": { "via": "org_unit" } }
```

Si 7 Leguas tiene zonas con gerente, el preset declara `org_unit_types: [{key:"region", allowed_parents:["root"]}]` y las sucursales cuelgan de ellas. Al filtrar por la zona, entran todas sus sucursales.

**Gods** («región» = mercado):

```json
// core de Ops (manifiesto del core; OrgMarket es del core)
"dimensions": [{ "key": "market", "type": "categorical",
  "values": { "source": "catalog", "model": "OrgMarket", "value": "code", "label": "name" },
  "vocabulary": { "ecommerce": "dim.region" } }]
```

```json
// customers.SalesOrder y channel_orders.ChannelOrder: columna nueva market_code. La toma
// channel_orders del shipping.market que ya llega (cartsvc/shippingblock.go:88) o, en POS, del
// aspecto de mercado por omisión de la sucursal.
"scope": { "market": { "column": "market_code" }, "country": { "path": "shipping_address.country" },
           "org_unit": { "column": "branch_id" }, "channel": { "column": "channel" },
           "date": { "column": "created_at", "default": true } }
```

```json
// storefront_cart.Cart y CheckoutSession: la columna ya existe (A.8.4); solo se declara el mapeo.
"scope": { "market": { "column": "market" }, "date": { "column": "created_at", "default": true } }
```

Gods ve el selector «Región: México / United States». Si no hay sucursales, la dimensión `org_unit` no aparece, porque tiene menos de 2 valores. Un tablero de Gods filtra así: `?s.scope.market=us&s.scope.channel=web&s.range=28d`.

**Hub:**

```json
// hub_fleet
"dimensions": [
  { "key": "hub_fleet.vertical", "type": "categorical", "values": { "source": "distinct" } },
  { "key": "hub_fleet.channel",  "type": "categorical", "values": { "source": "enum" } },
  { "key": "hub_fleet.ops_version", "type": "categorical", "values": { "source": "distinct" } }
],
"models": [{ "key": "HubInstance", "scope": {
  "country": { "column": "country_iso" }, "date": { "column": "last_report_at", "default": true },
  "hub_fleet.vertical": { "column": "vertical" }, "hub_fleet.channel": { "column": "channel" },
  "hub_fleet.ops_version": { "column": "ops_version" } } }]
```

Con este mapeo, un clic en «MX» del mapa escribe `s.scope.country=MX`. Los KPIs, la tabla, el mapa y la exportación se recalculan solos.

**Vantora:** el preset gobierno declara `territory` (jerárquica de catálogo) y las dependencias como tipos de unidad. Sus modelos mapean `territory: {column: "municipio_clave"}`. Hoy guardan el **nombre** en texto (`intel.go:98`), así que hay que pasar a la clave INEGI; queda como decisión H.10.

### C.6 Compatibilidad con `branch_id`

- `branch_id` **se queda** como columna en todas las tablas. Es la columna mapeada a `org_unit`, y su valor es el id de la unidad, porque los ids no cambian.
- `X-Branch-ID` y `ctx.branch_id` (en la ABI de WASM y en los eventos sellados) se mantienen como alias de la unidad activa.
- Un modelo que **no declara** `scope`, pero tiene la columna `branch_id`, recibe un mapeo implícito `org_unit → branch_id`, con el `unassigned` que hoy le da la variable de entorno. Así nada cambia antes de que cada addon declare el suyo. El validador lo avisa como *deprecation*.

---

## D. Contratos organizacionales [P]

### D.1 Unidad organizacional (kernel)

```text
org_unit_types     (key, org_id NULL=global, i18n_key, icon, declared_by, version)
org_units          (id uuid, organization_id, type_key, code, name, status, source, external_ref,
                    valid_from, valid_to, timestamps, deleted_at)          -- los nodos, sin padre
org_hierarchies    (key, organization_id, purpose, i18n_key, allowed_edges[{parent_type,child_type}],
                    declared_by, is_primary)                               -- jerarquías por propósito
org_unit_edges     (hierarchy_key, parent_id, child_id, valid_from, valid_to, changed_by, reason)
org_unit_closure   (hierarchy_key, ancestor_id, descendant_id, depth, valid_from, valid_to)
org_unit_members   (organization_id, hierarchy_key, unit_id, user_id, role_key NULL,
                    inherit_down bool, valid_from, valid_to)
org_structure_log  (append-only: quién, cuándo, qué arista o aspecto, vigencia, motivo)
```

- **Las unidades no tienen padre: las aristas son por jerarquía.** Una sucursal puede estar a la vez en la jerarquía `operational` (región → zona → sucursal), en `management` (territorio del gerente comercial) y en `geographic` (país → estado → sucursal).
  - Los propósitos los declaran el preset o un addon.
  - La primaria (`is_primary`, por omisión `operational`) es la que usan el selector y `branch_id`.
- **Vigencia.** Las aristas, los valores de aspectos y las membresías tienen `[valid_from, valid_to)`.
  - Una reorganización se programa a una fecha futura.
  - La consulta `as_of` reconstruye la estructura de cualquier fecha.
  - `org_structure_log` deja la auditoría.
- **Cierre transaccional por jerarquía.** Se mantiene en la misma transacción que la arista (*trigger* o servicio), sin trabajos de «aplanado» nocturnos.
  - Índices: `(hierarchy_key, ancestor_id, valid_from)` y `(descendant_id)`.
  - Un subárbol se expande en **una sola consulta** con `JOIN` al cierre.
- No hay `boundary` ni `legal_unit_id`.
- El id de una sucursal es el id de su unidad.
- La raíz es implícita.

### D.2 Aspectos de unidad (antes «facetas»)

Se renombran para no chocar con `dynamic.Facets` (J.3). Un addon los declara así:

```json
"org_unit_aspects": [{
  "key": "fiscal.issuer", "version": 1, "applies_to": ["*"],
  "kind": "reference", "ref": "fiscal_mexico.Issuer", "inherit": "nearest",
  "extra": { "$ref": "schemas/aspects/issuer_overrides.json" }
}]
```

- `kind` puede ser `attributes` (con un esquema propio) o `reference` (apunta a un registro del addon dueño).
- `inherit` puede ser `none`, `nearest` o `merge`.
- Valores en `org_unit_aspect_values(unit_id, aspect_key, version, value, valid_from)`.
- Se resuelve con `AspectResolve(unit_id, key) → {value, holder_unit_id, inherited}`.
- Un aspecto puede publicar campos como dimensión derivada, con `values.source: aspect` (C.2).

### D.3 Identidad fiscal aplicable: `legal_identity.resolve@1`

Es un contrato de capability con varios proveedores:

- **Entrada:** `{unit_id, market?, document?: {model, id?, override_issuer_id?}, purpose, at?}`. `market` va según la decisión H.18.
- **Salida:** `{provider, issuer_id|null, holder_unit_id, level: document|unit|inherited|organization, legal_name, tax_id?: {type, value}, country?, ready, blocked_reason?, connector_instance?}`.

Se resuelve en este orden:

1. el *override* por documento, si el proveedor lo permite para ese `purpose`;
2. el aspecto de la propia unidad;
3. el aspecto heredado;
4. la identidad de la organización, que es el proveedor por omisión en Ops.

El kernel elige al proveedor por el dueño del aspecto. Cada documento emitido guarda una foto inmutable `LegalIdentitySnapshot`; `fiscal_mexico` ya guarda la cuenta en `provider_data.emission_profile`.

### D.4 Instancias de conector (kernel)

- `connectors[].instances` admite:
  - `single`, que es el comportamiento por omisión;
  - `per_record`, una instancia por registro de un modelo (para `factura_com`, una por `Issuer`). **Reemplaza `rs1..rs4`.**
  - `per_scope` (`resolve: "aspect:fiscal.issuer" | "org_unit"`).
- `instances.strict` reemplaza la lista fija de proveedores estrictos.
- El permiso `connector:read` se da por clave de conector.

### D.5 `OrgContext` v1

Es el contexto organizacional: `{org, unit (activa = s.scope.org_unit), path, allowed, structure: {version, unit_count, depth, types[], aspects[]}, roles}`.

- **No contiene filtros.** Los filtros viven en `ScopeContext` (C.3).
- En el servidor, `ctx_get` suma `org_unit`.

### D.6 Eventos

Todos son esquemas JSON versionados.

| Evento | Emisor | Para qué sirve |
|---|---|---|
| `org_unit.created`, `.updated`, `.archived`, `.moved` | kernel | Ciclo de vida de una unidad |
| `org_unit.member.granted`, `.revoked` | kernel | Invalida `allowed` y el caché |
| `org_unit_type.registered` | kernel | Alta de un tipo de unidad |
| `org_unit.aspect.changed` | kernel | Reemplaza el semántico de `locations.branch_assigned` |
| `org.structure.changed` | kernel | Invalida las expansiones de subárbol y las condiciones de navegación |
| `org.context.changed` | SDK, en el cliente | Cambio de organización, de membresía o de estructura |
| `scope.changed` | SDK, en el cliente | `{prev, next, changed[]}`; reconsulta selectiva |
| `legal_identity.changed` | el proveedor fiscal | Cambio en la identidad fiscal |

### D.7 Navegación con condición

`ConditionDef` suma predicados nuevos:

- `org.units.count {type?, gte}`;
- `org.aspect.holders {key, gte}`;
- `org.unit.type {in}`;
- `scope.dimension.available {key}`;
- `capability`;
- `setting`.

Se evalúan igual en Go y en TS, contra un *golden* JSON compartido.

Cada ítem lo declara su dueño:

| Ítem | Lo declara | Condición |
|---|---|---|
| Estructura | Manifiesto del core de Ops | `org.units.count ≥ 2` |
| Sucursales | `locations` | — |
| Razones sociales | `fiscal_mexico` | — |
| Conexiones por unidad | Superficie de conectores del core | `exists connector instances != single` |

Se borran `groups-section.ts`, `CORE_PROMOTED_NAV_GROUPS`, la promoción en `navigation.go` y `versionAtLeast(…,'0.4.0')`.

---

### D.8 Políticas de alcance declarativas (ABAC)

Los permisos sobre los datos se declaran en el manifiesto o los configura el administrador. No se programan por pantalla.

```json
"scope_policies": [{
  "key": "sales.by_assignment", "version": 1,
  "applies_to": { "permission": "customers.sales_order.read" },
  "subject": { "roles": ["sales_manager", "cashier"] },
  "allow": { "all": [
    { "org_unit": { "in": "$subject.units", "hierarchy": "operational", "subtree": true } },
    { "market":   { "in": "$subject.attr.markets", "unless_empty": true } }
  ]}
}]
```

- **Un solo lenguaje de expresión.** Es un subconjunto JSON con `all`, `any`, `not`, `in`, `eq`, `gte`, rutas `$subject.*` y `$record.*`, y `subtree`. Es el mismo que usan `ConditionDef` (navegación), `visible_when` (formularios) y `ScopeFilter`.
- **Dónde se evalúa.** En Go, en el servidor; es el que **decide**. En TS se evalúa solo para no ofrecer en la interfaz lo que el servidor negaría. Comparten un juego de casos *golden*.
- **Fórmula del resultado.** Lo que el usuario ve es `filtro ∩ (unión de las políticas que le aplican)`. Sin política, rige el comportamiento de hoy: dueños y administradores ven todo, y los usuarios con membresías ven sus unidades.
- **Caché de pertenencia.** Por `(org, user, versión de estructura, versión de políticas)` se guarda el conjunto `allowed` por dimensión, como un *bitmap* o una lista de ids. Se invalida con `org_unit.member.*`, `org.structure.changed` y `scope_policy.changed`.

## E. Qué cambia en el sidebar y en el tablero

**Sidebar.** Hoy es casi vacío por tres causas (A.5): el grupo de `locations` se filtra, la promoción usa otra clave y el conteo es fijo. Con el contrato propuesto:

- cada addon declara su menú;
- la sección «Estructura» aparece solo con estructura real;
- con una sola unidad, el menú queda como hoy;
- Vantora ve sus tipos de unidad (dependencias) sin nada fiscal.

**Tablero.**

- `ScopeBar` aparece arriba en el tablero, en las listas y en las páginas genui, con las dimensiones que el tenant tiene en uso:
  - en 7 Leguas, Sucursal y Estado;
  - en Gods, Región (el mercado);
  - en el Hub, País, Vertical, Canal y Versión;
  - en Vantora, Territorio.
- Los KPIs respetan los permisos por unidad. Es la corrección de A.10.

---

## F. Migración incremental y compatible

Toda migración con datos se ensaya **primero sobre una copia de la base de 7 Leguas**, nunca sobre producción. En Vantora no se toca nada hasta que su preset declare `territory` y las dependencias.

1. **Árbol desde las sucursales.**
   - Se crea `org_units(id = branch.id, type = 'branch')`.
   - Antes se audita la tabla `public.branches` del core.
   - `branch_id` sigue siendo válido.
2. **Mapeo implícito.** Toda tabla con `branch_id` recibe `org_unit → branch_id` (C.6). Las variables de entorno de modelos estrictos se leen como el `unassigned` por omisión hasta que el manifiesto lo declare.
3. **Tablero.** `widgets/data` pasa a aplicar `ApplyScope`.
   - Es un cambio de comportamiento **intencional**: un usuario restringido deja de ver los totales de la organización.
   - Se anuncia en el *changelog* y se protege con la bandera `OPS_SCOPE_WIDGETS=log|enforce`.
4. **Emisores desde los slots.**
   - Cada `factura_com` / `factura_com_rsN` con `api_key` se convierte en un `Issuer` y en una instancia `record:fiscal_mexico.Issuer:<id>`. La operación la hace el host y copia la fila sin descifrarla afuera.
   - La clave vieja queda como alias de lectura para los documentos ya timbrados.
5. **Aspecto desde la sucursal.**
   - `mx_razon_social` se convierte en el aspecto `fiscal.issuer`.
   - Las series, el lugar de expedición y el formato pasan a `extra`.
   - Durante una versión se lee también `fiscal_data`, en modo dual.
6. **`locations.Entity`.** Se ofrece como borrador de `Issuer`, se archiva, y `entity_id` queda en solo lectura durante una versión.
7. **Membresías.** `branch_members` pasa a `org_unit_members`, y Ops deja de leer el esquema del addon.
8. **Mercado en el pedido (Gods).**
   - Se agregan las columnas `channel_orders.ChannelOrder.market_code` y `customers.SalesOrder.market_code` (y la misma en `Invoice` y `Payment` donde aplique).
   - `storefront_cart` ya envía `shipping.market`. `channel_orders` lo copia a la columna al materializar el pedido.
   - **Backfill sin suposiciones.** `CheckoutSession` guarda juntos `sales_order_id` y `market` (`cartsvc/checkout.go:398-400`), así que los pedidos web históricos se rellenan uniendo por `sales_order_id`. Los demás (POS, ML, Amazon) quedan como «sin mercado» y `meta` lo informa.
   - Para 7 Leguas, que tiene un solo mercado, no hay *backfill*.
9. **Alias del kernel.** `SettingScopeEntity|Branch`, `connectors.Scope{EntityID, BranchID}` y `EntityScope` pasan a la unidad o al aspecto durante una versión mayor.

---

## G. Fases

| Fase | Contenido | Criterio de aceptación | Tamaño |
|---|---|---|---|
| **F0** | Aprobar este RFC. Retiro del síntoma: la sección de grupos se muestra por el conteo real de emisores y no por la versión del addon. | Con un solo RFC no aparece la sección. | S |
| **F1** | Kernel: `org_units`, cierre, tipos, aspectos, `AspectResolve` y eventos `org_unit.*`. Ops crea el árbol desde las sucursales. | Los ids de sucursal y de unidad son iguales en la copia de 7 Leguas. Pasan las pruebas de herencia y de movimiento. | L |
| **F2** | **Kernel: dimensiones.** `dimensions[]`, `models[].scope`, dimensiones base (`org_unit`, `date`, `country`), `ScopeFilter`, `scope.Compile` con expansión por cierre e intersección con `allowed`, y `AggregateSpec.Scope`. | *Golden tests* del compilador: subárbol, `via`, `path` jsonb, sin mapeo (`ignored`), intersección vacía (`clamped` / 403). No hay SQL armado con valores del usuario. | L |
| **F3** | **Ops: `ApplyScope` único.** Se aplica en `/data`, `aggregate`, `/api/q`, exportaciones, búsqueda y facetas, **`widgets/data`**, `view_chart` y mapas. Se agrega `GET /api/scope/dimensions` y los alias `X-Branch-ID` / `X-Entity-ID`. | Prueba de paridad: la misma `ScopeFilter` da el mismo conjunto en la lista, el widget de conteo y la exportación. Un usuario restringido ve en el tablero solo sus sucursales, y con «Todas» ve la unión de ellas. | M |
| **F4** | **SDK: `useScope`, `?s.scope.*`, `scope.changed` y `ScopeBar`.** Inyección implícita en fuentes genui, `DynamicTable` y exportaciones. `OrgContext`, y retiro de los almacenes de sucursal duplicados. | Un tablero sin una sola línea de filtro propio se filtra por `market` y por `org_unit` desde la URL. Al compartir la URL se ve lo mismo, dentro de los permisos de quien la abre. | M |
| **F5** | Addons: mapeos en `customers`, `pos`, `receivables`, `channel_orders` y `hub_fleet`. `market_code` en `SalesOrder`. Dimensiones de `locations` (`org_unit.country` / `state`) y de `hub_fleet`. | Gods filtra ventas por región (mercado). El Hub filtra KPIs, mapa y tabla por país con un clic. 7 Leguas filtra por estado de la sucursal. | M |
| **F6** | Kernel y Ops: `connectors[].instances` (`per_record`, `per_scope`, `strict`). | Un addon de prueba con 7 instancias lee la correcta. | M |
| **F7** | `fiscal_mexico`: `Issuer`, aspecto `fiscal.issuer`, `legal_identity.resolve@1` y migración de los slots. | En la copia de 7 Leguas, cada documento sale con el mismo RFC y la misma serie que antes. Se pueden dar de alta 6 o más emisores. Los documentos dependientes usan la cuenta original. | L |
| **F8** | Navegación con condición. Borrado de `groups-section.ts`, `CORE_PROMOTED_NAV_GROUPS` y la promoción en `navigation.go`. | `git grep "locations.nav\|versionAtLeast("` sobre el shell da vacío. | M |
| **F9** | Membresías en `org_unit_members`, `UnitScope` en WASM (en modo `log` y luego `enforce`). Se depreca `locations.Entity`. | La prueba de aislamiento por subárbol pasa tanto en la API como en WASM. | M |
| **F10** | Jerarquías por propósito y vigencia: `org_hierarchies`, aristas con `valid_from/valid_to`, cierre por jerarquía, `as_of` y `org_structure_log`. Puede adelantarse dentro de F1 si se decide (H.15). | Una reorganización programada se ve en `current` desde su fecha, y `transaction` conserva la estructura anterior. Expandir un subárbol de 10 000 unidades tarda menos de 50 ms con el índice. | L |
| **F11** | `scope_policies` (ABAC) con evaluadores Go y TS y casos *golden* compartidos, simulador en Ops, y caché de `allowed`. | El simulador coincide con lo que devuelve la API en todos los casos *golden*. Sin políticas, el comportamiento es idéntico al de hoy. | M |
| **F12** | Mercado como dimensión: el core declara `market` y se agrega la columna `market_code` en `ChannelOrder` y `SalesOrder`, con *backfill* desde `CheckoutSession`. | El tablero de Gods filtra por `market=us` y cuadra con la suma de las sesiones de pago de us. | S |

F2-F5 (dimensiones) y F6-F7 (fiscal) son **independientes** entre sí, y las dos dependen de F1. El tablero del hub puede adoptar F2-F4 sin esperar lo fiscal.

---

## H. Decisiones abiertas

Todas son **propuestas, pendientes de confirmación del dueño**.

1. **Emisor como registro.** Se recomienda un registro `Issuer` referenciado desde el aspecto, no un atributo de cada sucursal.
2. **Override de emisor por documento.** Se recomienda permitirlo solo para los `purpose` que el proveedor declare, con permiso y foto del documento. No en el POS.
3. **Aislamiento por razón social.** Para v1 se recomienda aislar solo por subárbol. Si una organización necesita aislar por empresa, modela las empresas como nodos del árbol.
4. **Organización sin addon fiscal.** Se recomienda que Ops responda el contrato con los datos de la organización.
5. **Folio interno por emisor.** Se recomienda `Sequence.Scope: "aspect:<key>"`.
6. **Cambio de emisor en el tiempo.** Se recomienda pasar `at` en la consulta y guardar `valid_from` en los valores del aspecto.
7. **Nombre del contrato.** Se recomienda `legal_identity`.
8. **Forma del contrato en el catálogo de capabilities del kernel.** Se recomienda definir ahí solo la forma, sin lógica.
9. **Qué significa «regiones» en Gods.** Se recomienda tratarla como la **dimensión `market`**, con la etiqueta «Región» por vocabulario del preset de e-commerce. Esto es lo que dicen el modelo `OrgMarket` y `PLAN-REGIONES.md`. Si el dueño se refiere a regiones de envío dentro de un país (Norte, Bajío…), es otra dimensión `ship_region`, categórica, con un catálogo propio de la organización que mapea estados a regiones, y se filtra por `shipping_address.state`. En ningún caso son sucursales.
10. **Territorio de Vantora.** Se recomienda guardar la clave INEGI además del nombre, para que la dimensión jerárquica pueda expandir estado → municipio sin comparar textos.
11. **Rango por omisión de cada modelo.** Se recomienda `date.default` en el mapeo. Sin él, el modelo ignora el rango y `meta.ignored` lo informa; no se adivina la columna.
12. **Persistencia del filtro.** Se recomienda que la URL mande sobre las vistas guardadas, y estas sobre la preferencia del usuario (por ejemplo, la última sucursal elegida). La URL nunca amplía permisos.
13. **Dimensiones base del kernel.** Se recomienda que sean solo `org_unit`, `date` y `country`. `market` es del core de Ops porque `OrgMarket` es de Ops. Si otros hosts además de Ops necesitan mercados, `OrgMarket` debería subir al kernel, que ya tiene `capability.Market`; queda pendiente.
14. **¿Un país pertenece a un solo mercado?** Se recomienda que sí, validándolo en el alta. `DISENO-MERCADOS.md` §4 ya propone primero un reporte de solapes. Con esa regla, el filtro por país se puede traducir a mercado sin ambigüedad.
15. **¿Jerarquías múltiples y vigencia desde F1, o después (F10)?** Se recomienda **desde F1**, al menos en el esquema: las tablas `edges` y `closure` con `hierarchy_key` y vigencia. Agregarlas después obligaría a migrar el cierre. La interfaz y el `as_of` pueden llegar más tarde.
16. **Mercado por omisión de una sucursal** (para ventas en mostrador). Se recomienda un aspecto `commerce.market` en la unidad, que hereda el mercado por omisión de la organización.
17. **¿El mercado restringe permisos?** Se recomienda que sí, pero solo por política ABAC explícita (D.8), nunca de forma implícita.
18. **Mercado en la resolución de identidad fiscal.** Se recomienda agregar `market` a la entrada de `legal_identity.resolve@1`, para que un pedido web sin sucursal resuelva su emisor por mercado si el proveedor lo configura.

---

## I. Corrección respecto al informe previo

1. **Se retira la «frontera `legal`» del kernel.** El informe proponía un `boundary` en cada unidad, un `legal_unit_id` y el invariante «exactamente un ancestro legal». Eso llevaba un concepto fiscal al primitivo genérico, obligaba a crear nodos vacíos en gobierno o salud y ataba la topología operativa a la fiscal.
2. **El informe no reconoció que la razón social por sucursal ya está implementada** en `fiscal_mexico` (PR #2090). El modelo paralelo es `Entity`, no el de `fiscal_mexico`.
3. **La razón social pasa a ser un aspecto del addon fiscal** y se expone por `legal_identity.resolve@1`.
4. **«Estructura» y «Conexiones» pasan a ser contribuciones declaradas.**
5. **El diagnóstico del sidebar se precisa** (A.5).

**Se conserva del informe:**

- el árbol de N niveles con el vocabulario del preset;
- los ids estables y `branch_id` como alias;
- `OrgContext`;
- los evaluadores en Go y TS;
- el alcance de conectores y ajustes;
- la foto del documento;
- el catálogo `org_unit.*`;
- la crítica a los almacenes duplicados.

---

## J. Autocrítica: lo que estaba «medio raro» en la revisión 1 de este RFC

1. **Mezclaba filtrar con estar autorizado.** `OrgContext.scope {mode: unit|subtree|all, unit_ids}` usaba la misma estructura para dos cosas: lo que el usuario *puede* ver (permisos, que el servidor impone) y lo que *eligió* ver (un filtro de la página). Una URL compartida podía parecer que daba acceso, y un filtro no tenía dónde vivir. **Corrección:** `OrgContext.allowed` lleva los permisos y `ScopeContext.filter` lleva el filtro. El servidor siempre aplica `filtro ∩ allowed`.
2. **La unidad era el único eje de filtro.** Una sola cabecera `X-Org-Unit-ID` y una «unidad activa» no expresan «mercado US, últimos 7 días, sucursales de Jalisco». Sin un contrato de dimensiones, cada tablero termina declarando `params: ["filters.country_iso"]` y `extraWhere` a mano, que es justo la duplicación que se quería evitar. **Corrección:** las dimensiones son un primitivo del kernel y la unidad es una dimensión más, ligada a permisos (sección C).
3. **Usaba el nombre «faceta», que ya está tomado.** El kernel ya usa «facets» para los valores distintos de una columna con su conteo (`dynamic/facets.go`). Llamar igual a los atributos de una unidad iba a confundir, y además las facetas son justo lo que alimenta al selector de filtros. **Corrección:** se renombran a **aspectos** (`org_unit_aspects`).
4. **Daba a entender que «región» era un nivel del árbol para todos.** El preset podía declarar «región» como tipo de unidad y eso parecía la respuesta universal. En Gods, la región es el mercado del pedido (A.8); en Vantora, es el territorio del dato (A.9). Ninguno de los dos es una unidad. **Corrección:** el criterio de C.1, de cuatro preguntas, con casos verificados.
5. **No vio que el tablero se salta el alcance.** Los widgets no aplican el filtro de sucursal (A.10). Sin este dato, un contrato de filtros parecía una mejora estética, cuando en realidad corrige una inconsistencia de permisos. **Corrección:** F3 lo pone dentro de `ApplyScope`, con bandera.
6. **Mantenía comportamientos implícitos.** La detección de `branch_id` por nombre de columna y los modelos estrictos por variable de entorno seguían como estaban. **Corrección:** los dos pasan al mapeo declarado (`scope.org_unit.column`, `unassigned`). La convención por nombre queda solo como compatibilidad, con aviso del validador.
7. **Tenía un solo árbol y no tenía historial.** `org_units.parent_id` reproducía el dolor común de SAP, Dataverse y Odoo: una jerarquía única para la operación, la gestión y la seguridad, y cambios sin vigencia. **Corrección:** jerarquías por propósito, aristas con vigencia, consulta *as-of* y bitácora (D.1 y K).
8. **Trataba el mercado como algo por crear.** Proponía «una dimensión `market`» como si fuera nueva, y suponía que el pedido ya sabía su mercado. En realidad el mercado ya es de primera clase en Ops (A.8), pero **no está sellado en el pedido**. **Corrección:** se reutiliza `org_markets`, se agrega `market_code` al pedido con *backfill* determinista desde `CheckoutSession`, y se deja explícita la matriz mercado × unidad (C.1 bis).
9. **El contrato fiscal no cambia de fondo.** `legal_identity` y las instancias de conector siguen igual. Ahora están claramente **fuera** del filtro: la razón social puede ser una dimensión derivada (`org_unit.issuer`, vía la unidad), pero no es la manera de modelar la organización.

---

## K. Benchmark: cómo lo resuelven los grandes y qué diseño los supera

Esta sección sale de **lo que se sabe de los modelos públicos** de cada producto. En esta sesión no se consultó documentación oficial, así que **todo lo de K.1 y K.2 es [S]**: un resumen fiel según el conocimiento del autor, con nombres de objetos que conviene confirmar antes de citarlos hacia afuera.

### K.1 Cómo modela cada uno la estructura [S]

**SAP S/4HANA**

- **Estructura:** fija y configurada en *customizing*: mandante → sociedad (*company code*, la unidad legal con libros propios), área de control, centro (*plant*), almacén, organización de ventas × canal de distribución × sector (área de ventas), organización de compras, centros de beneficio y de coste (con jerarquías estándar y alternativas por grupos).
- **Seguridad:** objetos de autorización con campos de nivel organizacional (`BUKRS`, `WERKS`, `VKORG`…) en roles de PFCG, más roles derivados por valor de organización.
- **Tiempo:** HCM/OM sí tiene vigencias (infotipos con inicio y fin). El resto de la estructura logística y financiera no.

**Oracle Fusion**

- **Estructura:** entidades legales, unidades de reporte legal, unidades de negocio con funciones de negocio, *ledgers* con segmentos del plan de cuentas, y organizaciones de inventario.
- **Árboles:** versionados y con fechas efectivas, pero hay que **aplanarlos** (*row/column flattening*) con procesos para que rindan.
- **Seguridad:** roles de datos o contextos de seguridad (rol + BU / ledger / org de inventario), con *data access sets*.

**Microsoft Dynamics 365**

- **Finance & Operations:** entidades legales (`DataAreaId` en los registros, una partición dura), unidades operativas (departamentos, centros de coste, unidades de negocio) y **jerarquías organizativas por propósito**, publicadas con fecha efectiva. Las dimensiones financieras son transversales. La seguridad es por roles, más políticas XDS.
- **Dataverse / CE:** árbol de unidades de negocio, roles con niveles de acceso (usuario, BU, BU y descendientes, organización), equipos dueños y, más recientemente, propiedad de registros en una BU distinta a la del usuario («modernized business units»).

**Salesforce**

- **Visibilidad:** jerarquía de roles (la visibilidad sube por la jerarquía), valores por omisión de toda la organización (OWD) y reglas de *sharing* (por dueño o por criterio).
- **Territorios:** *Enterprise Territory Management*, con modelos, tipos, jerarquía y reglas de asignación. Una cuenta puede estar en varios territorios, pero **solo hay un modelo activo**.
- **Divisiones** para partir datos.

**NetSuite OneWorld**

- **Legal:** árbol de subsidiarias.
- **Segmentos de clasificación:** departamento, clase y ubicación, todos jerárquicos, más **segmentos personalizados** con impacto contable.
- **Seguridad:** restricciones de rol por subsidiaria y por segmento.

**Workday**

- **Estructura:** organizaciones supervisoras (gestión y *staffing*), compañías (legal), centros de coste, regiones y organizaciones personalizadas, con tipos de organización.
- **Dimensiones:** **worktags**, etiquetas transversales que van en cada transacción.
- **Seguridad:** grupos de seguridad basados en rol asignados a la organización (heredan hacia abajo), restringidos o no, de intersección y segmentados.
- **Tiempo:** **fecha efectiva en todo**, con reporte a fecha.

**Odoo**

- **Estructura:** `res.company` multi-compañía, con `parent_id`. En versiones recientes hay sucursales como compañías hijas.
- **Registros:** `company_id` en cada registro.
- **Seguridad:** el selector multi-compañía pone `allowed_company_ids` en el contexto, y las *record rules* (`ir.rule`) se escriben por modelo como dominios (`company_id in company_ids`).
- **Jerarquías:** `_parent_store` con un `parent_path` materializado y los operadores `child_of` / `parent_of`.
- **Dimensiones:** planes analíticos múltiples.

**Bitrix24**

- **Estructura:** un árbol de departamentos con jefes.
- **Seguridad en el CRM:** roles con niveles «propios / propios + departamento / + subdepartamentos / todos».
- **Legal:** sin entidades legales de verdad, más allá de varios «datos de mi empresa» (requisitos) para facturar.

### K.2 Aciertos y dolores [S]

| Producto | Aciertos que adoptamos | Dolores que evitamos |
|---|---|---|
| SAP | El nivel organizacional como **campo de autorización** del dato; varias jerarquías de controlling | Estructura rígida en *customizing*: reorganizar es un proyecto. Explosión de roles derivados. Sin vigencia fuera de HCM. Vocabulario industrial fijo. |
| Oracle | Árboles **versionados con fecha efectiva**; seguridad por contexto (rol × alcance) | Aplanado por procesos batch, con estructura desfasada entre corridas. Configuración pesada. Seguridad atada a BU/ledger (la estructura legal mezclada con la seguridad). |
| Dynamics F&O | **Jerarquías por propósito** con fecha efectiva; dimensiones financieras | `DataAreaId` como partición dura: los reportes cruzados entre empresas cuestan. La seguridad sigue a la entidad legal. |
| Dataverse | Niveles de acceso por subárbol; propiedad desacoplada de la BU del usuario | Un solo árbol de BU para todo. Esa jerarquía mezcla legal, operación y seguridad. |
| Salesforce | **Territorios** separados de la jerarquía de roles; asignación por reglas | La jerarquía de roles mezcla gestión con visibilidad. Recálculo de *sharing* lento y con bloqueos. Un solo modelo de territorios activo. |
| NetSuite | **Segmentos de primera clase** (departamento, clase, ubicación y personalizados) | Un solo árbol de subsidiarias. Restricciones por segmento gruesas. |
| Workday | **Worktags** transversales, **fecha efectiva universal**, seguridad heredada por organización | Implementación y configuración pesadas, propietarias y poco extensibles por terceros. |
| Odoo | `parent_path` materializado; alcance multi-selección en el contexto (`allowed_company_ids`) | La compañía es a la vez legal y sucursal. *Record rules* escritas por modelo. Errores de «compañías incompatibles». Sin vigencias. |
| Bitrix24 | Alcance simple y comprensible («departamento + subdepartamentos») | Un solo árbol, solo de RR. HH., sin dimensiones ni entidades legales. |

**Patrón común de los dolores:**

1. una **jerarquía única** que tiene que servir para lo legal, la operación, la gestión y la seguridad a la vez;
2. la **seguridad atada a la estructura legal**;
3. **configuración pesada** y cambios sin historial, o con historial solo en un módulo;
4. **rendimiento** que depende de aplanados batch o de recálculos de *sharing*;
5. **filtros y seguridad reimplementados por módulo o por pantalla**.

### K.3 Por qué la jerarquía y el scope no van en un addon

1. **Es seguridad transversal.** El alcance tiene que aplicarse en todos los *endpoints* del host (listas, agregados, widgets, `/api/q`, exportaciones, `data_query` de WASM, eventos sellados), y también sobre datos de **otros** addons. Un addon no puede imponer reglas sobre datos que no son suyos sin que el host confíe en él; eso invierte la dirección de dependencias.
2. **Todo addon depende de ella.** POS, `customers`, `fiscal_mexico`, `hub_fleet` y los de gobierno necesitan unidad, alcance y filtros. Si vivieran en un addon, todos los demás dependerían de él y de su versión, que es justo el problema de hoy con `locations ≥ 0.4.0` en el shell (A.4).
3. **Es neutral al dominio.** Un árbol, aristas con vigencia, un cierre, dimensiones y políticas no contienen ninguna palabra de negocio. Lo que sí es de negocio son el vocabulario, los tipos y los aspectos, y eso queda en presets y addons.
4. **La fontanería ya está en el kernel.** `EntityScope`, `ScopedStore`, los ajustes con alcance y `dynamic.Facets` ya viven en el kernel (A.6, A.10). Dejar ahí solo la mitad es el «peor punto medio» que el informe criticaba.
5. **Lo que sí es de un addon:** el tipo `branch` y la faceta física (`locations`), el emisor fiscal (addon de cada país), el vocabulario por industria (presets), y las dimensiones propias de cada dominio (`hub_fleet.vertical`, `territory`).

### K.4 Diseño de nueva generación

| # | Principio | Cómo se resuelve aquí | Supera a |
|---|---|---|---|
| 1 | **Varias jerarquías por propósito** sobre las mismas unidades | `org_hierarchies` + `org_unit_edges` por jerarquía (D.1). Con eso se separan la operación, la gestión o el territorio comercial y la geografía. Las dimensiones transversales (canal, vertical, mercado, región comercial) son ciudadanas de primera clase (C.2), como los *worktags* | Al árbol único de Dataverse, Odoo y Bitrix, y al modelo de territorio único de Salesforce |
| 2 | **Vigencia y consultas a fecha** | `valid_from` y `valid_to` en aristas, aspectos y membresías; `as_of: transaction\|current\|fecha`; `org_structure_log` *append-only*; reorganizaciones programadas | A SAP fuera de HCM, a Odoo y a Bitrix. Se iguala a Workday y Oracle |
| 3 | **Seguridad como alcance declarativo (ABAC)** | `scope_policies` (D.8), con un solo lenguaje evaluado en Go (decide) y en TS (orienta la interfaz). No hay código por pantalla | A las *record rules* por modelo de Odoo, a los roles derivados de SAP y al *sharing* de Salesforce |
| 4 | **Rendimiento** | Cierre **transaccional** por jerarquía, sin aplanado batch; índices compuestos; expansión de subárbol en una consulta; caché de `allowed` por versión de estructura y de políticas; `scope_too_wide` y búsqueda incremental en dimensiones grandes | Al aplanado de Oracle y al recálculo de *sharing* de Salesforce |
| 5 | **Un solo contrato de filtro** | `ScopeFilter` + `ApplyScope` para listas, widgets, exportaciones, mapa, reportes y WASM (C.2-C.4) | A los filtros por módulo de todos |
| 6 | **Vocabulario y niveles por industria** | Presets que declaran tipos, jerarquías, etiquetas y dimensiones (retail: sucursal o región; gobierno: dependencia o territorio; salud: red, hospital o servicio; manufactura: planta, línea o celda). El core no trae vocabulario de ninguna vertical | Al vocabulario fijo de SAP y Bitrix |
| 7 | **Declarativo, extensible y tipado** | Manifiesto con `dimensions`, `models[].scope`, `org_unit_aspects`, `org_hierarchies`, `scope_policies` y `org_unit_types`, versionados; eventos `org_unit.*`, `org.context.changed` y `scope.changed`; **interfaz de administración generada** (árbol por jerarquía con línea de tiempo, editor de aspectos desde el esquema, simulador de políticas «¿qué ve este usuario?») | A la configuración propietaria y pesada de todos |
| 8 | **Migración sin romper a nadie** | Ids estables, mapeo implícito de `branch_id`, alias de cabeceras, banderas `log` y luego `enforce` (F) | — |

**Decisión.** El kernel adopta los principios 1 a 5 y 7. El SDK aporta el cliente del 5 y del 7. Ops implementa los puertos y la interfaz generada. Los presets y addons ponen el 6 y los aspectos de dominio. La identidad fiscal sigue en el addon de cada país (D.3).

### K.5 Comparación por criterio

Las celdas de los productos de terceros son [S].

| Criterio | SAP | Oracle | D365 | Salesforce | NetSuite | Workday | Odoo | Bitrix24 | **Propuesta** |
|---|---|---|---|---|---|---|---|---|---|
| Jerarquías múltiples por propósito | Parcial (controlling) | Árboles por tipo | Sí (F&O) | Roles + 1 modelo de territorio | No (subsidiarias + segmentos) | Sí (tipos de organización) | No | No | **Sí, por manifiesto** |
| Legal separado de la operación y de la seguridad | No (la sociedad manda) | No (BU/ledger) | No (`DataAreaId`) | N/A | No | Sí | No (compañía = todo) | N/A | **Sí: la legal es un aspecto del addon fiscal** |
| Dimensiones transversales | Parcial (ACDOCA) | Segmentos del plan de cuentas | Dimensiones financieras | Campos | Sí (segmentos) | Sí (worktags) | Planes analíticos | No | **Sí: `dimensions[]` para todo el dato, no solo el contable** |
| Vigencia y a fecha | Solo HCM | Sí (árboles) | Sí (jerarquías) | No | Limitada | Sí | No | No | **Sí: aristas, aspectos y membresías** |
| Seguridad declarativa por alcance | Objetos + roles | Contextos de datos | Roles + XDS | *Sharing rules* | Restricciones | Grupos | *Record rules* en código | Niveles fijos | **ABAC JSON, Go = TS** |
| Rendimiento del subárbol | — | Aplanado batch | — | Recálculo de *sharing* | — | — | `parent_path` | — | **Cierre transaccional + caché versionada** |
| Un filtro para listas, KPI, exportación y mapa | No | No | No | No | No | Parcial | No | No | **Sí (`ScopeFilter`)** |
| Vocabulario por industria | Fijo | Fijo | Fijo | Configurable | Fijo | Fijo | Fijo | Fijo | **Presets** |
| Extensible por terceros con contratos | ABAP | Limitado | Extensiones | Apex/metadata | SuiteScript | Muy limitado | Módulos Python | REST | **Manifiesto versionado + eventos tipados** |
| Mercado comercial como eje propio | Organización de ventas | BU | Entidad o canal | N/A | Subsidiaria o moneda | N/A | Compañía o lista de precios | N/A | **`market` ⟂ `org_unit` (C.1 bis)** |

## Referencias de código

**`addons`**

- `packages/fiscal_mexico/backend/wasm/emission_profile.go`
- `packages/fiscal_mexico/backend/wasm/emission_profile_wasm.go`
- `packages/fiscal_mexico/backend/wasm/main.go:1352`
- `packages/fiscal_mexico/manifest.json`
- `packages/locations/manifest.json`
- `packages/locations/migrations/005_entities.up.sql`
- `packages/customers/manifest.json` (L1433, L2056 y L2862)
- `packages/storefront_cart/backend/wasm/cartspec/`
- `packages/storefront_cart/backend/wasm/cartsvc/` (`checkout.go:398-400`, `shippingblock.go:88`, `handlers.go:540-550`, `marketprice.go`)
- `packages/storefront_cart/manifest.json` (L365 y L1244)
- `packages/customers/backend/wasm/pricing.go:394`
- `packages/customers/migrations/054_market_scope.up.sql`
- `packages/storefront_api/backend/wasm/api_domain.go:117`

**`asteby-ops`**

- `backend/middleware/domain.go`
- `backend/appserver/api_gateway.go:158`
- `backend/models/org_market_provider.go`
- `backend/services/org_market_providers.go:117`
- `backend/services/market_region_dispatcher.go`
- `backend/database/migrations/20261017100000_org_markets.sql`
- `backend/services/releasenotes/release-notes.json`
- `docs/plan-next-gen/DISENO-MERCADOS.md`
- `backend/handlers/branch_scope.go:154-230`
- `backend/handlers/dashboard_widgets.go:42-60`
- `backend/services/dashboard_catalog.go:298`
- `backend/services/view_chart.go:59`
- `backend/services/branch_visibility.go`
- `backend/middleware/branch_scope.go`
- `backend/handlers/export.go:1726`
- `backend/appserver/server.go:2945`
- `backend/models/org_market.go`
- `backend/services/navigation.go`
- `frontend/src/components/layout/groups-section.ts`
- `frontend/src/components/layout/ops-app-sidebar.tsx`
- `frontend/src/features/metacore/use-addon-sidebar.ts:652`
- `frontend/src/features/metacore/page-url-state.ts`
- `frontend/src/features/org-hierarchy/page.tsx`

**`metacore-kernel`**

- `query/aggregate.go:54`
- `capability/query.go:16` (`Market`)
- `capability/capability.go:21`
- `dynamic/facets.go:64`
- `connectors/scope.go`
- `manifest/manifest.go` (L500-523, L756-820 y L1979)
- `manifest/v3/types.go` (`Sequence.Scope`)
- `runtime/wasm/entity_scope.go`

**`metacore-sdk`**

- `packages/sdk/src/plugin.ts`

**`store-gods-nutrition`**

- `app/lib/markets.ts`
- `app/lib/storefront.server.ts:151`
- `app/lib/market-detect.ts`
- `docs/PLAN-REGIONES.md`
- `app/mock/gods.ts:288`

**`hub`**

- `backend/internal/geo/geo.go:29`
- `backend/internal/api/telemetry.go:334`

**`hub-addons`**

- `packages/hub_fleet/manifest.json:434`

**`vantora`**

- `backend/core/gobiernos/territory.go:69`
- `backend/core/gobiernos/intel.go:98`
- `backend/appserver/server.go:1328`
