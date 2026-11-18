# RFC: unidades organizacionales genéricas y la identidad fiscal como faceta del addon del país

- Estado: **Propuesto**. Es solo documentación y no hay código en este PR.
- Fecha: 2026-11-18
- Reemplaza en parte a:
  - RFC 0001 «Entidad y alcance». No está en `main`; vive solo en las ramas `origin/docs/rfc-entidad-alcance` y `origin/docs/rfc-estadisticas-grupo`.
  - La propuesta del informe de revisión multi-entidad del 2026-11-18, que pedía una «frontera legal en el kernel».
- Bases revisadas, todas en `origin/main`:
  - `metacore-kernel` eb2720c
  - `asteby-ops` 4a43e65
  - `addons` 7d5e2703
  - `metacore-sdk` (último `fetch` del día)
- Convención:
  - **[H]** hecho verificado en código, con su ruta.
  - **[I]** dato tomado del informe previo, sin reverificar.
  - **[P]** propuesta.

## 0. Resumen

El dueño del producto tiene razón en los dos puntos que cuestionó:

1. **La razón social es un concepto del addon fiscal de cada país, no del kernel ni del shell.** Es un RFC, un régimen, un CSD, unas series y una cuenta del PAC. Su forma cambia por país (CUIT, NIT, EIN, VAT) y hay industrias enteras que no la tienen: una dependencia de gobierno o una clínica de un sistema público. Ponerla como «frontera legal» del árbol del kernel obliga a todas las organizaciones a modelar un nodo fiscal que no existe en su negocio.
2. **«Razón social por sucursal» ya existe y funciona en `fiscal_mexico`.** Está en `main` desde el PR #2090 (`e8784b3f`):
   - una extensión de `locations.Branch` agrega `mx_razon_social`, las series y el lugar de expedición;
   - una cascada resuelve el emisor en el orden sucursal → razón social → global;
   - el documento guarda la cuenta con que se timbró.

   Lo que falta no es inventarla, sino **quitarle tres limitaciones**:
   - los slots fijos `factura_com_rs1..rs4`;
   - el duplicado `locations.Entity`, que ningún consumidor lee;
   - la ausencia de un contrato genérico para que otros addons pregunten «¿qué identidad fiscal aplica aquí?».

**Modelo final [P]:**

- **Kernel:** un árbol genérico de **unidades organizacionales** de N niveles, sin palabras de negocio y sin «frontera legal». Encima, un mecanismo genérico de **facetas** tipadas que los addons adjuntan a las unidades, con herencia por el árbol. Encima, instancias dinámicas de conector, alcance por unidad, contexto `OrgContext` y condiciones de navegación evaluables.
- **Addon fiscal de cada país** (`fiscal_mexico` y equivalentes): dueño del registro de **emisores**, de sus conectores (una instancia por emisor) y de la faceta `fiscal.issuer`, que apunta de una unidad a un emisor. Implementa el contrato agnóstico **`legal_identity.resolve@1`**.
- **`locations`:** dueño de la faceta física y operativa (dirección, horario, recolección, tipo de sucursal, almacén). Deja de ser dueño de la razón social.
- **Ops (shell):** no decide nada por clave, versión ni conteo fijo. Renderiza contribuciones de navegación declaradas en manifiestos, con condiciones evaluadas sobre el `OrgContext`.

---

## A. Hallazgos verificados

### A.1 «Razón social por sucursal» ya existe, y vive en `fiscal_mexico`

[H] **Extensión de la sucursal.** `addons/packages/fiscal_mexico/manifest.json` (0.54.6), `model_extensions` con `target_model: "locations.Branch"` (alrededor de L2362), agrega estas columnas al `fiscal_data` de la sucursal:

| Columna | Qué guarda |
|---|---|
| `mx_razon_social` | Una de las opciones `factura_com_rs1..rs4`. Vacío = la global. |
| `mx_serie_invoice` | Serie de la factura, por sucursal. |
| `mx_serie_credit_note` | Serie de la nota de crédito, por sucursal. |
| `mx_serie_payment_complement` | Serie del complemento de pago, por sucursal. |
| `mx_serie_carta_porte` | Serie de la carta porte, por sucursal. |
| `mx_lugar_expedicion` | Código postal SAT del lugar de expedición. |
| `mx_formato_impresion` | Formato de impresión de los CFDI. |

[H] **Cascada pura.** `packages/fiscal_mexico/backend/wasm/emission_profile.go` hace `buildEmissionProfile(global, rs, rsKey, branchFiscal)`:

- Recorre sucursal → razón social (connector `factura_com_rsN`) → global (connector `factura_com`).
- `accountScopedKeys` impide mezclar las llaves de una cuenta con el RFC o la serie de otra.
- El respaldo con la global es opcional por organización (`emision_fallback_global`). Sin él, el documento queda bloqueado con el motivo.

[H] **Cableado con el host.** `emission_profile_wasm.go` lee la sucursal del documento: `branchFiscalDataForInvoice` (`main.go:1352`) toma `invoice.branch_id`, o el de `sales_orders`, y luego consulta `branches.fiscal_data`.

- `resolveProfileForDependent` obliga a que la nota de crédito y el REP usen **la cuenta con que se timbró la factura**, porque el emisor del documento dependiente no puede cambiar.
- La clave de la cuenta queda guardada en `provider_data` (`emission_profile`, `emission_level`). Es una «foto» mínima del emisor por documento.

[H] **Límite declarado en el propio código** (`emission_profile.go`, encabezado): «Los slots son fijos porque el kernel aún no tiene connectors con instancias dinámicas». Consecuencias:

- Hay 4 slots y 1 global: **5 RFC por organización como máximo**.
- `profileConnectorGet` es un `switch` sobre constantes, para que el linter de permisos `connector:read` siga validando.

[H] **Datos del emisor.** El RFC, la razón social, el régimen y el código postal son **campos del connector** (`razon_social` en cada uno de los 5 connectors, alrededor de L453, 968, 1177, 1386 y 1595 del manifiesto). Se cifran por organización en `org_integrations`.

- No hay tabla de emisores.
- El CSD lo custodia el PAC. `csd_status_wasm.go` consulta su vigencia en `/v1/current/account`, por cuenta.

[H] **Folios.** Los asigna el PAC dentro de la serie: es el comentario de `mx_serie_invoice`. El kernel tiene `Sequence.Scope "org"|"branch"` (`manifest/v3/types.go`, alrededor de L1464), pero `fiscal_mexico` no lo usa para el folio fiscal.

[H] **Sin relación con `locations`.** `fiscal_mexico` **no lee `locations.Entity` ni `branches.entity_id`**: `git grep entity` en su backend no da ningún uso de entidad organizacional. Ningún otro paquete de `addons` usa `mx_razon_social` ni `factura_com_rs*`.

### A.2 Existe un segundo modelo de razón social, sin consumidores: `locations.Entity`

[H] `packages/locations/manifest.json` (0.4.0, commit 7e2d2ba5, «RFC 0001, F1») agrega:

- **Modelo `Entity`** (`addon_locations.entities`): `code`, `legal_name`, `display_name`, `country`, `fiscal jsonb` («tax_id_type, tax_id, regime, postal_code»), `is_default`, `source local|erp`, `external_ref`.
- **`Branch.entity_id`**, con `ref locations.Entity`.
- **Migración `migrations/005_entities.up.sql`**: entidad `main` por omisión, un trigger que llena `entity_id` y la vista `branch_entities`.
- **Eventos** `locations.entity_created`, `locations.entity_updated` y `locations.branch_assigned`.

[H] **Nadie consume `Entity`:**

- `fiscal_mexico` no la lee.
- Las coincidencias de `entity_id` en `calendar`, `documents` y `mail` se refieren a «entidad» en el sentido de registro genérico, no a la razón social.

Con esto hay **dos «razones sociales» por sucursal que pueden divergir**: `Branch.entity_id` en `locations` y `Branch.fiscal_data.mx_razon_social` en `fiscal_mexico`.

[H] `locations` también es dueño de lo operativo y físico: `kind`, `branch_type physical|channel|virtual_warehouse`, `fulfillment_warehouse_id`, horario y recolección. Su menú (`contributions.navigation`) declara el grupo `locations.nav.group` con «Sucursales» (`model: Branch`) y «Entidades» (`model: Entity`), con `target: sidebar.operations`.

### A.3 El «grupo» no existe como dato

- [H] `asteby-ops/frontend/src/features/org-hierarchy/page.tsx`: `group: { code: user.organization_id, name: user.organization_name }`. El grupo es la organización con otro nombre.
- [H] No hay `parent_id` entre sucursales ni nodos intermedios (región, división). `hierarchy-view.ts` fija tres niveles: grupo → entidades → sucursales.
- [H] El core de Ops tiene además `backend/core/base/branch.go` (`Branch{OrganizationID, Name, Code, Address, Phone, IsActive}`). Es un segundo modelo de sucursal, aparte del de `locations`.

### A.4 Qué hace «Estructura del grupo» y por qué la inyecta el shell

[H] La ruta `/organization/structure` (`OrgStructurePage`):

- Es una vista **de solo lectura**.
- Pide `/data/Entity/me` y `/data/Branch/me`, que son modelos del addon `locations`.
- Dibuja Grupo → Entidad → Sucursal con `HierarchyView`.
- Si no hay addon, permiso o tabla, devuelve `[]` y muestra «una sola entidad».

[H] `frontend/src/components/layout/groups-section.ts` arma la sección «Empresas y grupos» así:

- Conoce las claves i18n del addon: `GROUPS_KEYS = { entities: ['locations.nav.entities'], branches: ['locations.nav.branches'] }`.
- **Inyecta** dos hojas que no vienen de ningún manifiesto: `/organization/structure` y `/integrations` (Conexiones por entidad, que es la tarjeta `ScopedConnections` montada en `pages/integrations/index.tsx:213`).
- Decide si se muestra con `shouldShowGroupsSection(entityCount, locationsVersion)`, que devuelve `entityCount >= 2 || versionAtLeast(locationsVersion, '0.4.0')`. Es decir, **conoce la versión de un addon**.

[H] `frontend/src/components/layout/ops-app-sidebar.tsx` (alrededor de L671) la llama como `shouldShowGroupsSection(0, locations?.version)`, con el conteo **fijo en 0**. La rama «≥2 entidades» nunca se cumple, y la sección aparece en cuanto `locations ≥ 0.4.0` está instalado.

El porqué de la inyección es **histórico, no de diseño**. La pantalla de estructura y la de conexiones con alcance se construyeron en Ops para la F1 del RFC 0001, y no hay contrato en el manifiesto para que una superficie del core se declare con condición. Por eso el shell la cablea en código.

### A.5 Por qué la sección sale casi vacía

Es una explicación verificada leyendo código. No la reproduje en un navegador.

1. [H] `features/metacore/use-addon-sidebar.ts:652`: `CORE_PROMOTED_NAV_GROUPS = new Set(['locations.nav.group'])` **descarta el grupo de `locations`** de los ítems de módulos, en `useAddonModuleItems`.
2. [H] `backend/services/navigation.go` (alrededor de L144-156) publica «Sucursales» en el grupo Sistema, con `URL: "/m/branches", Model: "Branch"` y el título `sidebar.branches`, no `locations.nav.branches`.
3. Entonces `extractGroupsSection(eco.rest, …)` busca `locations.nav.entities` y `locations.nav.branches` entre los ítems de módulos y **no encuentra ninguno**:
   - el grupo ya se filtró en el punto 1;
   - Sucursales vive en Sistema con otra clave.
4. Resultado: con `locations` 0.4.0 instalado, la sección muestra solo «Estructura del grupo» y «Conexiones por entidad», dos pantallas que el shell inyecta.
   - «Entidades» **desaparece** del menú. Solo queda el enlace dentro de la página de estructura.
   - «Sucursales» sigue en Sistema.
5. [H] Si la organización tiene un layout propio de sidebar (`orgLayoutTree`), `ops-app-sidebar.tsx` retorna antes (alrededor de L650) y la sección no se arma.

Hay **tres lugares que deciden la misma navegación**: el manifiesto de `locations`, `navigation.go` y `groups-section.ts`, más el filtro de `use-addon-sidebar.ts`.

### A.6 Kernel: la fontanería de alcance existe y usa la palabra «entity»

- [H] `connectors/scope.go`: `Scope{EntityID, BranchID}`, `WithScope` y `ScopedStore.GetScoped`, con la cadena sucursal → entidad → org. El comentario dice «el kernel nunca aprende qué es una entidad».
- [H] Ajustes: `manifest/manifest.go:756-763` define `SettingScopeOrg|Branch|Market|Entity` y `ResolveSettingScopedEntity`. La migración es `migrations/sql/0011_addon_settings_entity_scope`.
- [H] Alcance en WASM: `runtime/wasm/entity_scope.go`, `runtime/entityscope/entityscope.go` y `dynamic/entity_scope.go`.
- [H] Navegación: `NavGroup.Condition *ConditionDef` (`manifest/manifest.go:500-508`). `ConditionDef` (L1979) admite `addon_installed`, `connector_connected` y `unmet hide|disable`, pero **ningún predicado organizacional**.
- [H] Conectores: no hay instancias dinámicas. `git grep instances` en `connectors/` y `manifest/` solo encuentra la concurrencia de WASM.
- [H] Ops no conecta esa fontanería:
  - `git grep` de `X-Entity-ID` y `WithEntityScope` en `backend/` da vacío;
  - `ResolveScopedIntegration` (`services/org_integration_scope.go:77`) no tiene llamadores fuera de las pruebas.
- [H] Ops lee de forma directa la tabla interna de un addon, `addon_locations.branch_members`, en `appserver/server.go:1649` y `edgesync/access.go:125`.

### A.7 SDK: no hay contexto organizacional

- [H] `metacore-sdk/packages/sdk/src/plugin.ts`: `AddonAPI = {manifest, settings, registry, client, kernelVersion, telemetry, log, realtime?}`. No trae contexto organizacional ni evento de cambio.
- [H] `git grep` de `OrgContext`, `useOrgContext` y `org.context.changed` en `packages/` da vacío.
- [I] Hay tres almacenes de «sucursal activa» (Ops, starter-core y runtime-react) y un `lib/api.ts` copiado por cada addon para poner `X-Branch-ID`.

### A.8 Lo que no verifiqué

- [I] Vantora usa el **territorio** como eje (`appserver/server.go`, `/gobiernos/mapa/territorio/...`). No lo reverifiqué en esta pasada.
- Si en una base real existen a la vez `public.branches` (core) y `addon_locations.branches`. La consulta sin esquema en `handlers/integrations_scoped.go` podría leer la tabla equivocada. Hay que medirlo sobre una copia.
- [I] El estado del refactor sin commit `wt-ops-consistency` (`nav-contributions.ts`). No lo leí; este RFC solo lo cita.
- Los datos reales de 7 Leguas: cuántos RFC hay y qué sucursal usa qué slot.

---

## B. Dónde vive cada cosa

### B.1 Decisión

**El kernel no modela la razón social ni ninguna «frontera legal».** Modela tres cosas:

- un árbol de unidades;
- facetas tipadas, con herencia;
- y referencias de alcance genéricas: instancias de conector, alcance de datos, ajustes y secuencias.

La identidad fiscal es una **faceta de referencia** que pertenece al addon fiscal del país. Su semántica se expone a los demás addons mediante un **contrato de capability versionado**, `legal_identity.resolve@1`. El kernel transporta y valida la forma del contrato, pero no conoce RFC, CUIT ni CSD.

### B.2 Criterios (frontera legal en el kernel vs. faceta del addon fiscal)

| Criterio | Frontera `legal` en el kernel (informe previo) | Faceta fiscal del addon (este RFC) |
|---|---|---|
| Acoplamiento | El kernel exige el invariante «toda unidad operativa tiene exactamente un ancestro legal». La topología del árbol queda atada a la fiscalidad. | El árbol refleja la operación. La fiscalidad se adjunta donde aplique y se hereda. |
| Multipaís | Un tipo de nodo `legal` común a todos los países. Los atributos por país quedan como facetas de todos modos. | Cada país trae su addon con su registro de emisores y su faceta. Una organización con sucursales en MX y en US resuelve a proveedores distintos por unidad. |
| Gobierno, salud y ONG sin razón social | Obliga a crear un nodo legal «implícito» que no significa nada para el negocio. | Sin addon fiscal no hay faceta: el contrato responde con la identidad de la organización o «ninguna». |
| Realidad de la asignación | En 7 Leguas, la RS2 tiene la sucursal de canal (Mercado Libre) y la RS1 tiene tres físicas. Agrupar por región o por zona cruzaría la frontera legal, y el árbol tendría que elegir entre la vista fiscal y la operativa. | La asignación fiscal es ortogonal al árbol operativo: se fija por sucursal, se hereda de un nodo superior o se sobrescribe por documento. |
| Evolución | Cambiar a una sucursal de razón social implica **mover** el nodo en el árbol y recalcular ancestros, alcance y caché. | Cambiar el valor de una faceta emite `org_unit.facet.changed`; el árbol no se toca. |
| Migración | Hay que convertir `locations.Entity` en nodos y reubicar las sucursales debajo. | Las sucursales ya son las hojas. `mx_razon_social` se convierte en una referencia a un emisor. `Entity` se migra a emisores, o se archiva si no tiene datos. |
| Lo que ya existe | Se contradice con `fiscal_mexico`, que ya resuelve el emisor por sucursal y no por entidad. | Generaliza lo que `fiscal_mexico` ya hace (sucursal → razón social → global) y le quita los slots fijos. |

Lo que se conserva del informe previo, porque es correcto con o sin frontera legal:

- el alcance por unidad (`EntityScope` y `ScopedStore`, generalizados);
- un `OrgContext` único y el evento `org.context.changed`;
- condiciones `visible_when` evaluables en Go y en TS;
- `branch_id` y `X-Branch-ID` como alias del id de unidad;
- ids estables;
- la foto inmutable del emisor en el documento;
- el catálogo `org_unit.*`.

Ver la sección H.

### B.3 Tabla de responsabilidades

| Pieza | Kernel | SDK | Ops (host/shell) | `locations` | Addon fiscal del país (`fiscal_mexico`…) | Preset de industria |
|---|---|---|---|---|---|---|
| Árbol de unidades (`org_units`, cierre, N niveles) | **Dueño**: tablas, servicio y eventos | Tipos TS generados | Implementa los puertos (persistencia, API `/org/units`) | — | — | — |
| Tipos de unidad (sucursal, planta, dependencia, región…) | Registro genérico (`org_unit_types`) | — | — | Declara `branch` y `warehouse_site` | — | Declara vocabulario y niveles (retail, gobierno, salud) |
| Facetas (esquema, herencia, validación) | **Mecanismo** genérico | Componentes de edición genéricos | — | Faceta `locations.site`: dirección, horario, recolección, `branch_type`, almacén | Faceta `fiscal.issuer`: referencia a un emisor, más los ajustes MX por unidad (series, lugar de expedición, formato) | — |
| Emisor / razón social (RFC, régimen, CP, CSD, cuenta del PAC) | — | — | — | Deja de existir (`Entity` se depreca) | **Dueño**: modelo `Issuer` y conectores | — |
| Contrato `legal_identity.resolve@1` | Forma tipada y versionada del contrato de capability | Tipos y `useLegalIdentity(unit)` | **Proveedor por omisión** con los datos de facturación de la organización | — | **Proveedor**: resuelve por unidad, documento y override | — |
| Instancias de conector (N cuentas por proveedor) | **Mecanismo** `instances` + `ScopedStore` por referencia | Superficie genérica de conexiones | Almacena en `org_integrations` (`instance_ref`) | — | Declara `factura_com` con `instances: per_record(Issuer)` | — |
| Alcance de datos por unidad (subárbol) | `UnitScope`, generaliza `EntityScope` | Cabecera `X-Org-Unit-ID` (alias `X-Branch-ID`) | Implementa el resolver y las membresías | `BranchMember` migra a `org_unit_members` | — | — |
| Ajustes y secuencias por unidad | `scope: unit` (más los alias) | `addon-config-surface` por unidad | — | — | Declara sus ajustes con `scope: unit` | — |
| `OrgContext` + `org.context.changed` | Resolución en el servidor (`ctx_get.org`) | **Dueño en el cliente**: `useOrgContext`, bus y cabeceras | Selector de unidad (reemplaza el branch-switcher) | — | Lo consume | — |
| Navegación | `ConditionDef` con predicados organizacionales; evaluador Go | Evaluador TS del mismo predicado | **Solo renderiza**, sin claves ni versiones fijas | Contribuye «Sucursales» | Contribuye «Razones sociales» y «Series por sucursal» | — |
| Pantalla «Estructura» | — | Componente `OrgUnitTree` (`HierarchyView` v2) | Contribución **declarada** en el manifiesto del core | Aporta columnas o insignias por faceta | Aporta la insignia «emisor» por faceta | — |

---

## C. Contratos [P]

### C.1 Unidad organizacional (kernel)

```text
org_unit_types   (key, org_id NULL = global, allowed_parents[], i18n_key, icon,
                  declared_by (addon|preset), version)
org_units        (id uuid, organization_id, parent_id NULL, type_key, code (único por org),
                  name, status active|archived, position, source local|external:<sys>,
                  external_ref, created_at, updated_at, deleted_at)
org_unit_closure (ancestor_id, descendant_id, depth)
org_unit_members (organization_id, unit_id, user_id, role_key NULL, inherit_down bool)
```

- No hay columna `boundary` ni `legal_unit_id`. El kernel no distingue «legal».
- `id` de una sucursal existente = id de su unidad. `branch_id` en tablas, eventos y cabeceras **es** un id de unidad: no se reescribe ningún dato.
- La raíz es implícita (la organización). Una organización sin árbol funciona como hoy.

### C.2 Facetas (kernel, mecanismo genérico)

Un addon declara en su manifiesto:

```json
"org_unit_facets": [{
  "key": "fiscal.issuer",
  "version": 1,
  "applies_to": ["*"],
  "kind": "reference",
  "ref": "fiscal_mexico.Issuer",
  "inherit": "nearest",
  "schema": { "issuer_id": "uuid" },
  "extra": { "$ref": "schemas/facets/issuer_overrides.json" },
  "unmet": "inherit"
}]
```

- **`kind: attributes`**: un esquema JSON propio, guardado en `org_unit_facet_values(unit_id, facet_key, version, value jsonb)`. Hoy se resuelve con `model_extensions` sobre `locations.Branch`; esto lo eleva a la unidad sin depender de `locations`.
- **`kind: reference`**: apunta a un registro del addon dueño. El kernel valida que exista mediante `data_query` del dueño.
- **`inherit`**:
  - `none`: solo la unidad;
  - `nearest`: el ancestro más cercano, incluida la propia unidad;
  - `merge`: fusión por clave de la raíz a la hoja.
- **Resolución genérica:** `FacetResolve(unit_id, key) → {value, holder_unit_id, inherited bool}`. Es pura, usa la tabla de cierre y se cachea por `org.structure.version`.
- El kernel **no interpreta** el contenido. Solo valida el esquema, resuelve la herencia y emite eventos.

### C.3 Identidad fiscal aplicable: `legal_identity.resolve@1`

Es un contrato de capability, del mismo tipo que los de `capability/commerce.go`. Tiene varios proveedores y cada uno declara su cobertura:

```ts
// Entrada
interface LegalIdentityQuery {
  unit_id: string | null          // unidad del documento (branch_id hoy)
  document?: { model: string; id?: string; override_issuer_id?: string } // override por documento
  purpose: 'invoice' | 'credit_note' | 'payment' | 'receipt' | 'contract' | string
  at?: string                     // fecha efectiva (para cambios de emisor en el tiempo)
}
// Salida (agnóstica del país)
interface LegalIdentity {
  provider: string                // 'fiscal_mexico' | 'ops.organization' | …
  issuer_id: string | null        // id del registro del proveedor; null = identidad de la organización
  holder_unit_id: string | null   // dónde estaba adjunta la faceta (herencia)
  level: 'document' | 'unit' | 'inherited' | 'organization'
  legal_name: string
  tax_id?: { type: string; value: string }   // 'MX-RFC', 'AR-CUIT', 'US-EIN'… (opaco para el kernel)
  country?: string
  ready: boolean                  // ¿puede emitir? (cuenta, certificado, datos completos)
  blocked_reason?: string
  connector_instance?: string     // instance_ref de la cuenta que emite / cobra
}
```

**Reglas de resolución.** Las define el proveedor, sobre el mecanismo genérico:

1. **Override por documento**, si el proveedor lo permite para ese `purpose` y la unidad lo autoriza.
2. **Faceta en la unidad** (`level: unit`).
3. **Faceta heredada** de un ancestro (`level: inherited`).
4. **Proveedor por omisión de Ops:** la identidad de la organización (`billing_tax_id`, etc.), igual que el `{{org.billing_tax_id}}` de hoy. Si el proveedor exige un emisor explícito, devuelve `ready: false` y el motivo. Es la misma política opcional que hoy tiene `emision_fallback_global`.

**Elección del proveedor.** El kernel elige el proveedor así:

- si la unidad tiene una faceta de identidad, el proveedor es el dueño de esa faceta;
- si no la tiene, se usa el proveedor por omisión.

No hay ninguna regla escrita por país.

**Foto inmutable.** Todo documento emitido guarda la foto `LegalIdentitySnapshot{provider, issuer_id, tax_id, legal_name, connector_instance, captured_at}`. `fiscal_mexico` ya guarda la clave de la cuenta en `provider_data.emission_profile`; la foto la amplía.

**Consumidores esperados:**

- POS (encabezado del ticket);
- cobros (qué cuenta de Mercado Pago o Stripe se usa);
- `channel_orders` (operación intercompañía cuando el emisor del pedido difiere del del almacén);
- storefront (razón social en el checkout).

Ninguno conoce el RFC.

### C.4 Instancias de conector (kernel)

```json
"connectors": [{ "key": "factura_com", "instances": { "mode": "per_record", "model": "Issuer" } }]
```

```json
"connectors": [{ "key": "mercadopago", "instances": { "mode": "per_scope", "resolve": "facet:fiscal.issuer" } }]
```

- **`single`** (por omisión): el comportamiento de hoy.
- **`per_record`**: una instancia por registro del modelo dueño. El `instance_ref` es `record:<addon>.<Model>:<id>`. **Reemplaza `factura_com_rs1..rs4` y quita el tope de 5 RFC.**
- **`per_scope`**: una instancia por unidad, o por el titular de una faceta. Se resuelve por herencia y se guarda en `org_integrations.instance_ref`. Generaliza `ScopedStore` y `org_integration_scopes`.
- **Permisos.** `connector:read` autoriza **la clave del conector**, no cada slot. Con eso el `switch` de constantes de `profileConnectorGet` deja de ser necesario sin perder el linter.
- **Política estricta por conector.** `instances.strict: true` reemplaza `defaultStrictScopeProviders` en Ops. Ahí la política es una lista fija de proveedores [I].

### C.5 Contexto organizacional (`OrgContext` v1)

```ts
interface OrgContext {
  version: 1
  org: { id: string; name: string; country?: string; currency: string; locale: string; timezone: string }
  unit: OrgUnitRef | null                 // unidad activa (null = toda la organización)
  path: OrgUnitRef[]                      // ancestros de la unidad activa
  scope: { mode: 'unit' | 'subtree' | 'all'; unit_ids: string[]; restricted: boolean }
  structure: { version: number; unit_count: number; depth: number;
               types: { key: string; count: number }[];
               facets: { key: string; holders: number }[] }   // p.ej. fiscal.issuer con 2 titulares
  roles: Record<string, string[]>         // unidad → roles
}
interface OrgUnitRef { id: string; type: string; code: string; name: string }
```

- **Servidor:** `ctx_get` (ABI de WASM) suma `org_unit`. El host sella `org_unit_id` en los eventos, igual que hoy sella `branch_id`.
- **Cliente:** el SDK es dueño de un único almacén. Pone `X-Org-Unit-ID` y, como alias, `X-Branch-ID`. Expone `useOrgContext()` y lo agrega a `AddonAPI` como `org`.
  - Se eliminan los `lib/api.ts` copiados en los addons.
  - Se eliminan los tres almacenes de sucursal.

### C.6 Catálogo de eventos

| Evento | Emisor | Payload |
|---|---|---|
| `org_unit.created` / `.updated` / `.archived` | kernel | `{unit: OrgUnitRef, parent_id}` |
| `org_unit.moved` | kernel | `{unit, from_parent, to_parent}` |
| `org_unit.member.granted` / `.revoked` | kernel | `{user_id, unit_id, role_key, inherit_down}` |
| `org_unit_type.registered` | kernel, al instalar un addon o un preset | `{type, declared_by}` |
| `org_unit.facet.changed` | kernel | `{unit_id, facet_key, version, prev?, next?}`. Reemplaza el semántico `locations.branch_assigned`. |
| `org.structure.changed` | kernel, derivado | `{version, unit_count, types, facets}` |
| `org.context.changed` | SDK, en el cliente | `{prev, next, reason: 'user_switch' \| 'membership' \| 'structure'}` |
| `legal_identity.changed` | el proveedor fiscal | `{issuer_id, provider, fields[]}`. No reescribe las fotos. |

Todos son esquemas JSON versionados (`schemas/events/*.json`), igual que los de los addons.

### C.7 Navegación por contribuciones con condición

`ConditionDef` gana predicados organizacionales. Los mismos evaluadores corren en Go (filtro del servidor de `/navigation`) y en TS (al recibir `org.context.changed`). Comparten un caso de prueba de *golden* JSON.

```json
{ "all": [
  { "addon_installed": "fiscal_mexico" },
  { "org.facet.holders": { "key": "fiscal.issuer", "gte": 2 } }
]}
```

Predicados v1:

- `addon_installed` y `connector_connected` (ya existen);
- `org.units.count {type?, gte}`;
- `org.facet.holders {key, gte}`;
- `org.unit.type {in}` (unidad activa);
- `capability` y `setting`.

**Secciones como destino.** El shell publica los destinos (`sidebar.organization`, `sidebar.operations`…), pero no los llena. Cada superficie la aporta su dueño:

| Ítem | Lo declara | Condición propuesta |
|---|---|---|
| Estructura | Manifiesto del **core** de Ops (contribución declarada, no inyectada) | `org.units.count ≥ 2` o `org.facet.holders(any) ≥ 2` |
| Sucursales | `locations` | `addon_installed: locations` (se elimina la promoción de `navigation.go` y `CORE_PROMOTED_NAV_GROUPS`) |
| Razones sociales (emisores) | `fiscal_mexico` | `addon_installed: fiscal_mexico` |
| Conexiones por unidad | Superficie genérica de conectores del core | `exists connector with instances != single` |
| Entidades | Ya nadie. Se depreca con `locations.Entity`. | — |

`groups-section.ts`, `shouldShowGroupsSection`, `versionAtLeast(..., '0.4.0')` y `GROUPS_KEYS` **se borran**. Lo mismo vale para `extractEcommerceSection`, que cablea las claves `integration_mercadolibre` e `integration_amazon`: queda pendiente y fuera del alcance de este RFC, pero debe seguir el mismo contrato.

---

## D. Qué cambia en el sidebar

**Hoy** (A.5) la sección «Empresas y grupos» muestra solo dos pantallas inyectadas. Hay tres causas:

- el shell filtra el grupo de `locations`;
- el backend promueve Sucursales con otra clave;
- la condición se evalúa con un conteo fijo en 0.

**Con el contrato:**

1. Cada addon declara su menú y su destino. El shell no conoce claves, versiones ni conteos.
2. «Estructura» aparece solo cuando hay estructura que mostrar: 2 o más unidades, o 2 o más titulares de faceta. Lo decide un predicado evaluable, no una versión de addon.
3. Con una sola razón social y una sola sucursal, el menú queda como hoy, sin sección vacía.
4. En Vantora o en salud, el preset declara sus tipos (dependencia, área, sede). «Estructura» aparece con ese vocabulario y no se muestra nada fiscal mientras no haya un addon fiscal instalado.
5. Los layouts propios de organización (`orgLayoutTree`) reciben las contribuciones como cualquier otra hoja, sin un `return` anticipado que las salte.

---

## E. Migración incremental y compatible

Principios:

- es aditiva;
- los ids de sucursal no cambian;
- toda migración con datos se ensaya **primero sobre una copia de la base de 7 Leguas**, nunca sobre producción;
- en Vantora no se toca nada hasta que su preset declare `territory` como tipo de unidad.

1. **Árbol desde las sucursales.**
   - Por cada fila de `addon_locations.branches` se inserta `org_units(id = branch.id, type = 'branch', parent = raíz)`.
   - El `public.branches` del core se audita antes: si tiene filas, se mapea por `code`, y si está vacío, se depreca.
   - `branch_id` sigue siendo válido en todas partes.
2. **Emisores desde los slots.**
   - Por cada connector `factura_com` y `factura_com_rsN` con `api_key`, `fiscal_mexico` crea un `Issuer` con RFC, razón social, régimen y CP tomados del connector.
   - Sus credenciales se re-asocian a la instancia `record:fiscal_mexico.Issuer:<id>` **sin descifrarlas fuera del host**, como una operación del host que copia la fila de `org_integrations`.
   - La clave vieja queda como alias de lectura, para cancelar, consultar el REP y refacturar los documentos ya timbrados (`profileKeyOfDoc`).
3. **Faceta desde la sucursal.**
   - `mx_razon_social = rsN` pasa a la faceta `fiscal.issuer = <issuer de rsN>`.
   - Vacío equivale a no tener faceta, y la unidad hereda la de la raíz, que es el `Issuer` del global.
   - Las series, el lugar de expedición y el formato pasan a la parte `extra` de la faceta. Durante una versión se mantienen **leídas también** desde `fiscal_data`, en modo dual.
4. **`locations.Entity`.**
   - Si una organización tiene entidades con datos fiscales, se ofrecen como borradores de `Issuer` que el usuario confirma.
   - Después se archivan.
   - `Branch.entity_id` y la vista `branch_entities` quedan de solo lectura durante una versión y luego se eliminan.
   - Hoy ningún addon las consume (A.2), así que el riesgo es bajo.
5. **Membresías.**
   - `addon_locations.branch_members` pasa a `org_unit_members`.
   - Ops deja de leer el esquema del addon (A.6).
6. **Alias del kernel:**
   - `SettingScopeEntity`, `SettingScopeBranch` y `connectors.Scope{EntityID, BranchID}` se mapean a `scope: unit` por resolución de herencia;
   - `EntityScope` se mapea a `UnitScope`.
   - Se mantienen una versión mayor.
7. **Fotos.** Los documentos existentes no se reescriben. La foto nueva solo se escribe en los documentos que se emitan desde la fase F3.

---

## F. Fases

| Fase | Contenido | Criterio de aceptación | Tamaño |
|---|---|---|---|
| **F0** | Este RFC aprobado. Retiro inmediato del síntoma en Ops: `shouldShowGroupsSection` deja de usar la versión del addon y se calcula con el conteo real de razones sociales, o se oculta la sección, mientras llega F4. | Con una sola razón social no aparece la sección. Con dos o más aparece con «Sucursales» y «Razones sociales». | S |
| **F1** | Kernel: `org_units`, cierre, tipos, facetas (`attributes`/`reference`, `inherit`), `FacetResolve` y eventos `org_unit.*`. Ops implementa los puertos y crea el árbol desde las sucursales. | Las pruebas de herencia y de movimiento pasan. Los ids de sucursal son iguales a los ids de unidad en la copia de 7 Leguas. El evento `org.structure.changed` se emite. | L |
| **F2** | Kernel: `connectors[].instances` (`per_record`, `per_scope`, `strict`) y `connector:read` por clave. Ops guarda `instance_ref`. | Un addon de prueba con 7 instancias lee la correcta. Sin `instances`, el comportamiento es idéntico al de hoy. | M |
| **F3** | `fiscal_mexico`: modelo `Issuer`, faceta `fiscal.issuer`, proveedor `legal_identity.resolve@1`, migración de los slots y modo dual con `fiscal_data`. Ops publica el proveedor por omisión de la organización. | En la copia de 7 Leguas, cada documento se timbra con el mismo RFC y la misma serie que antes, con prueba de oro sobre los perfiles. Las notas de crédito y los REP de documentos viejos usan la cuenta original. Se pueden dar de alta 6 o más emisores. | L |
| **F4** | `ConditionDef` con predicados organizacionales, evaluadores Go y TS con *golden* compartido, contribuciones declaradas, y borrado de `groups-section.ts`, `CORE_PROMOTED_NAV_GROUPS` y de la promoción de Sucursales en `navigation.go`. | `git grep` de `locations.nav` y `versionAtLeast(` en el shell da vacío. El sidebar es igual con y sin layout propio. | M |
| **F5** | SDK: `OrgContext`, `useOrgContext`, `AddonAPI.org`, cabeceras únicas y `org.context.changed`. Se eliminan los `lib/api.ts` copiados. | Cambiar la unidad activa refresca las tablas y las páginas declarativas sin recargar. No queda `active_branch_id` ni `current_branch` en el código. | M |
| **F6** | `UnitScope` en WASM con el resolver en Ops (modo `log` y luego `enforce`), y membresías en `org_unit_members`. | La prueba de aislamiento pasa: un usuario de un subárbol no lee documentos de otro, ni por API ni por WASM. | M |
| **F7** | `locations.Entity` deprecada y archivada. Faceta `locations.site` sobre la unidad. Consumidores de `legal_identity` en cobros, `channel_orders` y POS. | Ningún addon lee `entity_id` ni `mx_razon_social`. | M |

---

## G. Decisiones abiertas

Todas son **propuestas, pendientes de confirmación del dueño**.

1. **¿El emisor es un registro del addon o un atributo en la unidad?** Recomendación: **un registro** (`Issuer`) referenciado desde la faceta. Varias sucursales comparten una razón social, y la cuenta, el certificado y las series son de la razón social, no de cada sucursal.
2. **Override de emisor por documento.** Recomendación: permitido solo para los `purpose` que el proveedor declare, por ejemplo una factura manual desde la administración. Requiere permiso y queda en la foto. En POS no se permite.
3. **Aislamiento de datos por razón social.** Recomendación v1: el alcance se define **solo por subárbol de unidades**. Quien necesite aislar por razón social modela «empresas» como nodos del árbol (con un tipo del preset) y adjunta ahí la faceta. Un alcance por «titular de faceta» queda para una v2, si aparece un caso real.
4. **Organización sin addon fiscal.** Recomendación: Ops es el proveedor por omisión del contrato y responde con los datos de facturación de la organización. `ready` depende de que tengan RFC/tax id. El kernel no impone nada.
5. **Folio interno por emisor.** Recomendación: agregar `Sequence.Scope: "facet:<key>"`, un contador por titular de faceta, para los folios internos que no asigna el PAC, como un ticket con serie por razón social. En MX, el folio fiscal lo sigue dando el PAC.
6. **Cambio de emisor en el tiempo.** Recomendación: `at` en la consulta y el historial en `org_unit_facet_values` (con `valid_from`), para refacturar con el emisor vigente en la fecha original.
7. **Nombre del contrato.** Recomendación: `legal_identity`, que es neutro y sirve para una persona moral, una física o un ente público, en vez de `fiscal_identity`.
8. **Ubicación de la forma del contrato.** Recomendación: en el catálogo de capabilities del kernel, junto a `commerce`. Ahí solo se define la forma y la versión, sin lógica ni datos. Es la misma tensión que ya existe con `tax_regime` en `capability/commerce.go`; se documenta y no se agrava.

---

## H. Corrección respecto al informe previo

**Qué cambió:**

1. **Se retira la «frontera `legal`» del kernel** (`boundary ENUM(legal|operational|grouping)`, `legal_unit_id` por trigger, y el invariante «exactamente un ancestro legal»). Ese diseño trasladaba un concepto fiscal al primitivo genérico:
   - obligaba a gobierno, salud y ONG a crear nodos legales vacíos;
   - ataba la topología operativa a la fiscal;
   - convertía un cambio de razón social en un movimiento del árbol.
2. **El informe no reconoció que «razón social por sucursal» ya estaba implementada** en `fiscal_mexico` (PR #2090), con su cascada, su foto mínima y su respaldo opcional. La presentó como «slots fijos» y como un modelo paralelo a `locations.Entity`, cuando es **el modelo vivo**: `Entity` es el paralelo sin consumidores. Por eso este RFC **generaliza `fiscal_mexico`**, en lugar de mover la razón social al kernel o a `locations`.
3. **La razón social pasa a ser una faceta de referencia propiedad del addon fiscal del país.** Se expone por el contrato `legal_identity.resolve@1`, y la plataforma la consume sin conocer el RFC.
4. **La pantalla «Estructura» y «Conexiones» dejan de ser del shell.** Son contribuciones declaradas con condición. El informe proponía `visible_when` pero conservaba el registro de contribuciones en código de Ops.
5. **El diagnóstico del sidebar se precisa.** Además del conteo fijo en 0, la sección sale vacía porque `CORE_PROMOTED_NAV_GROUPS` y `navigation.go` quitan las hojas que `groups-section.ts` intenta mover.

**Qué se conserva del informe:**

- el árbol de N niveles con vocabulario por preset;
- las facetas;
- los ids estables y `branch_id` como alias;
- `OrgContext` y `org.context.changed`;
- los evaluadores `visible_when` en Go y TS;
- el alcance de conectores y ajustes por unidad;
- la foto inmutable en los documentos;
- el catálogo `org_unit.*`;
- la crítica a los almacenes de sucursal duplicados y a los `lib/api.ts` copiados.

---

## Referencias de código

**`addons`:**

- `packages/fiscal_mexico/backend/wasm/emission_profile.go`
- `packages/fiscal_mexico/backend/wasm/emission_profile_wasm.go`
- `packages/fiscal_mexico/backend/wasm/main.go:1352`
- `packages/fiscal_mexico/backend/wasm/csd_status_wasm.go`
- `packages/fiscal_mexico/manifest.json` (connectors `factura_com*`, extensión de `locations.Branch`)
- `packages/locations/manifest.json`
- `packages/locations/migrations/005_entities.up.sql`

**`asteby-ops`:**

- `frontend/src/components/layout/groups-section.ts`
- `frontend/src/components/layout/ops-app-sidebar.tsx` (alrededor de L650-674)
- `frontend/src/features/metacore/use-addon-sidebar.ts:652`
- `frontend/src/features/org-hierarchy/page.tsx`
- `frontend/src/pages/integrations/index.tsx:213`
- `backend/services/navigation.go` (alrededor de L144-156)
- `backend/services/org_integration_scope.go:77`
- `backend/core/base/branch.go`
- `backend/edgesync/access.go:125`

**`metacore-kernel`:**

- `connectors/scope.go`
- `manifest/manifest.go` (L500-523 `NavGroup`/`NavItem`, L756-820 ajustes con alcance, L1979 `ConditionDef`)
- `manifest/v3/types.go` (`Sequence.Scope`)
- `runtime/wasm/entity_scope.go`
- `runtime/entityscope/entityscope.go`

**`metacore-sdk`:**

- `packages/sdk/src/plugin.ts` (`AddonAPI`)
