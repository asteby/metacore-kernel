# RFC 0004 — Contratos y cadena de addons: el schema único del tren R2-b

- **Estado:** propuesto. Solo diseño: este documento no cambia código.
- **Fecha:** 2026-11-19
- **Repos afectados:** `metacore-kernel` (`manifest/v3`, `manifest`, `installer`, `capability`), `metacore-sdk` (codegen de tipos y editor de manifest), `hub` (validación al publicar, catálogo, certificación), `ops` (installer host, bindings, desinstalación), `addons` (`tools/manifestcheck`, manifests, addons base nuevos).
- **Relacionado:**
  - Plan maestro de Pitsline v2, `ops/docs/plan-next-gen/PLAN-PITSLINE-PRO-ESCALABLE-2026-11-19-v2.md`, ola R2-b y §5 (este RFC es el "RFC de schema nº 2" que esa ola exige).
  - [RFC 0003](0003-emision-y-recepcion-de-eventos.md) (catálogo `events{emits,consumes}`).
  - RFC 0001 y 0002 (kernel #595, #597): no tocan el schema de este tren; ver §9.
  - RFC de unidades organizacionales (kernel #633) y estándar genui (kernel #641): ver §9, hay una dependencia de orden con #633.
  - `docs/spec/v3/slots.md`, `docs/validation-compat-window.md`, `docs/conformance-payments.md`.
  - kernel #550 (`contributions.inject[]`): este RFC lo **sustituye** reusando su código (§2).
  - kernel #547 (`EXTENSIBILITY.md`, guía de autores): debe actualizarse con este RFC al implementarse.

Convención de evidencia: [V] verificado en `origin/main` al 2026-11-19 (kernel v0.292.0; addons `5400a649`); [I] inferencia mía; [NV] no verificado.

---

## 0. Resumen

R2-b congela, **en un solo release de schema**, todo lo que un addon necesita para ser base, sub-base, proveedor o extensión de otro. Es el primitivo P21 "Contratos y cadena" del plan v2, fusionado con eventos versionados (P9), `lifecycle.uninstall` (P12), bindings (P22b) y `inject` (P9).

| # | Decisión |
|---|---|
| D1 | **Un solo release de schema** con todas las claves de §1. Lo que no está en §1 espera a R3. Ningún bump suelto antes ni después dentro del tren. |
| D2 | **Composición, no herencia de código.** "Herencia" es herencia de contratos, esquema y puntos de extensión por `extends_addon` (padre único, profundidad ≤ 3). El WASM nunca comparte código. |
| D3 | **Un catálogo de slots, no dos:** se extiende `ops.host/v1` (nombres **singulares**, `<superficie>.<parte>`). Se descarta `slots@1` de #550 y sus nombres en plural (§1.9). |
| D4 | **Un catálogo de eventos, no tres:** el bloque `events` de RFC 0003 es el sitio canónico y gana `version`. `extension_points.events[]` queda como alias deprecado 2 releases (§1.7). |
| D5 | **`sealed` es booleano** y vive en lo que protege (modelo, contrato, operación, regla, acción). Se descarta la lista `extension_points.sealed[]` de #550. |
| D6 | **`primary_from[]` se descarta.** Un tope de prioridad por rol se logra con bandas de `order` y una regla del host, sin primitivo nuevo (§2.2). |
| D7 | **`ResolveChain` único** en `manifest/v3`, reutilizado por hub, installer (envuelve el `ResolveOrder` de H11), `manifestcheck` y el resolvedor de presets (§3). |
| D8 | **Compatibilidad:** `provides_capabilities` se lee 2 releases; sin `role` se infiere con aviso 1 release; toda regla nueva que pueda invalidar un manifest publicado nace en `warnOnlyWindow` (§1.13). |
| D9 | **Puerta de salida:** el corpus de 99 manifests de addons pasa sin errores y la proyección legacy de los que no usan claves nuevas es **idéntica byte a byte** a la anterior (§2.4). |
| D10 | **Sin renombres salvo `fiscal_mexico`** (plan v2 C8/D11). Las claves reales de pagos y correo no cambian (§4). |

Hallazgos que cambian el plan, verificados en el corpus (§2.4, §4):

1. **13 manifests ya violan R-A** (requieren a un proveedor), casi todos con `optional: true`. R-A no puede ser solo "prohibir `requires`": hace falta `requires` **por contrato** (`contract:<key>`), que está en este RFC (§1.4).
2. **Los addons de pagos no se llaman `payments_<p>`**: son `stripe`, `mercadopago`, `openpay`, `conekta`, `paypal`, `clip`, `payments_manual`. No se renombran.
3. **`messaging_core` no existe**; hoy hay **dos proveedores** de `messaging.whatsapp.send` (`link` y `connector_whatsapp`) y `connector_email` + `mail`. El contrato de correo necesita un addon base pequeño nuevo, sin renombrar a nadie.
4. **El plan habla de 7 roles pero su tabla §4A.1 tiene 8** (`localization` no está en la lista de P21). `l10n_mx` lo necesita; este RFC adopta los 8.
5. **`lifecycle.uninstall` hoy es un `string`** (nombre del hook) [V `types.go`]: `lifecycle.uninstall.data` como lo escribe el plan lo volvería unión de tipos. Se propone clave hermana (§1.11).
6. **`subscriptions[].ordering.phase: pre` no es implementable** para handlers WASM hoy (la ABI no tiene transacción, ver memoria `reference_wasm_abi_no_transaction_forupdate`): se reserva (§1.8).

---

## 1. Alcance cerrado: las claves de R2-b

Todas son **opcionales**. Un manifest que no las usa valida y se comporta igual que hoy.

| § | Clave | Dónde vive | Reemplaza / fusiona |
|---|---|---|---|
| 1.1 | `metadata.role` | `Metadata` | Taxonomía de roles (8) |
| 1.2 | `contracts[]` | raíz | `capability.Contract` abierto, P18/P10/P11/P23b |
| 1.3 | `implements[]` | raíz | `provides_capabilities[]` (se lee 2 releases) |
| 1.4 | `extends_addon`, `compatibility.requires[].key = "contract:<k>"` | raíz / `Requirement` | herencia simple; dependencia por contrato |
| 1.5 | `replaces[]` | raíz | renombres (solo `fiscal_mexico`) |
| 1.6 | `metadata.deprecated`, `contracts[].deprecated` | `Metadata`, `Contract` | ciclo de vida de claves |
| 1.7 | `events.emits[].version` (+ `deprecated`) | `EventsDecl` (RFC 0003) | `extension_points.events[]` |
| 1.8 | `contributions.subscriptions[].ordering` | `Subscription` | P9 `ordering` |
| 1.9 | `contributions.inject[]` y catálogo de slots unificado | `Contributions`, `ops.host/v1` | #550, `slots@1` |
| 1.10 | `sealed` (booleano) | `Model`, `Contract`, `ContractOperation`, `Rule`, `Action` | #550 `extension_points.sealed[]` |
| 1.11 | `lifecycle.uninstall_data` | `Lifecycle` | P12 |
| 1.12 | `Model.relates_to`, `extends_addon.reexport_writable[]`, `contracts[].writable_models[]` | `Model`, `ExtendsAddon`, `Contract` | herencia de esquema |
| 1.13 | `capability_bindings` | **tabla del kernel, no manifest** | `org_market_providers` (P22b) |

Nota: `capability_bindings` está en el plan como clave de schema, pero es una **tabla por organización**, no algo que un autor escriba. Su "schema" es el modelo GORM + migración SQL (§1.13), sin copias de JSON schema.

**Añadido respecto al plan v2** (sujeto a veto del dueño, §7 pregunta N4): `actions[].target_model` acepta `"<addon>.<Model>"` o `"@dominio.entidad"` (de #550). Sin él, `inject` resuelve la mitad del problema cross-addon (paneles) y deja la otra (botones) sin canal. Es un cambio de validación, no de forma (el campo ya existe como string).

**Descartado de #550 y no incluido:** `DashboardWidget.kind: map`, `extension_points.primary_from[]`, `extension_points.sealed[]`, el catálogo `slots@1`. Razones en §2.

### 1.0 Dónde se toca cada cambio (las "4 copias")

Cada clave se implementa **siempre** en estos sitios, en el mismo PR (hay prueba de paridad que falla si faltan):

1. `manifest/v3/types.go` (tipo Go autoritativo).
2. `docs/spec/v3/manifest-v3.schema.json` **y** `manifest/v3/schema/manifest-v3.schema.json` (idénticas; `schema_parity_test.go` [V]).
3. `manifest/v3/validate.go` + `manifest/v3/compat.go` (reglas y ventana de tolerancia).
4. Portadora legacy: `manifest/manifest.go` (struct `Manifest`), `manifest/from_v3.go` (proyección) y `manifest/validate.go` (el "validador legacy" que ve ops/installer). Hoy el installer y `capability.Select` consumen `manifest.Manifest`, no el v3 [V `installer/requires.go:322`].

Y fuera del kernel: codegen del SDK, validador del hub y `manifestcheck` (§2.3).

### 1.1 `metadata.role`

```go
// types.go
type Metadata struct {
    // ...campos existentes...
    // Role is the addon's place in the chain. One of RoleFoundation|RoleContract|
    // RoleProvider|RoleExtension|RoleLocalization|RoleVertical|RoleApp|RolePreset.
    Role string `json:"role,omitempty"`
    // Deprecated: see §1.6.
    Deprecated *Deprecation `json:"deprecated,omitempty"`
}
```

```json
"role": { "type": "string", "enum": ["foundation","contract","provider","extension","localization","vertical","app","preset"] }
```

Los 8 roles son los de la tabla 4A.1 del plan v1 (el plan v2 dice "7" y omite `localization`). `kind: Preset` implica `role: preset`; el valor explícito debe coincidir (error si no).

**Inferencia (ventana de 1 release).** Si `role` falta, el validador infiere y emite el aviso `[role_inferred] role "<x>" inferido de <motivo>`:

| Condición (en este orden) | Rol inferido |
|---|---|
| `kind: Preset` | `preset` |
| tiene `contracts[]` | `contract` |
| tiene `implements[]` o `provides_capabilities[]` | `provider` |
| tiene `extends_addon` | `extension` |
| `metadata.countries` no vacío y sin modelos | `localization` |
| tiene `extension_points.model_extensions_accepted` o modelos | `foundation` |
| otro | `app` |

La inferencia es heurística y **solo sirve para avisar**: el release siguiente `role_missing` es error en modo strict. El hub rechaza al publicar sin `role` desde ese release.

### 1.2 `contracts[]`

Un contrato es **una API versionada que un addon base publica y que varios proveedores implementan**. Es la generalización cerrada de `capability.Contract` (hoy abierto y sin versión, `capability/capability.go:62-117` [V]).

```go
type Contract struct {
    Key        string              `json:"key"`     // "commerce.payments", "fiscal.document"
    Version    string              `json:"version"` // semver "1.0.0"
    Operations []ContractOperation `json:"operations"`
    // Events son REFERENCIAS "name@version" a events.emits de ESTE manifest.
    Events []string `json:"events,omitempty"`
    // States liga el contrato a la máquina de etapas de un modelo propio.
    States *ContractStates `json:"states,omitempty"`
    // Models proyecta roles de lectura a modelos propios: "@commerce.order" -> SalesOrder.
    Models []ContractModel `json:"models,omitempty"`
    // WritableModels son los modelos propios que el contrato ABRE a sus implementadores (R-B).
    WritableModels []string `json:"writable_models,omitempty"`
    ProviderSettings []string `json:"provider_settings,omitempty"` // keys de settings[] del base
    Selection  *ContractSelection `json:"selection,omitempty"`
    Failover   string             `json:"failover,omitempty"` // never|allowed (default por operación)
    Conformance string            `json:"conformance,omitempty"` // "CONF-PAYMENTS@1"
    DocumentTypes []string        `json:"document_types,omitempty"` // fiscal: ["invoice","credit_note","transport"]
    Sealed     bool               `json:"sealed,omitempty"`
    Deprecated *Deprecation       `json:"deprecated,omitempty"`
}

type ContractOperation struct {
    Key         string   `json:"key"`   // "issue"
    Kind        string   `json:"kind"`  // "action" | "query"
    // Capability es la clave `capability.Contract` LEGACY que esta operación
    // hereda ("fiscal.invoice.create"); por omisión "<contract.key>.<op.key>".
    Capability  string   `json:"capability,omitempty"`
    Required    *bool    `json:"required,omitempty"` // default true
    Idempotent  bool     `json:"idempotent,omitempty"`
    TimeoutS    int      `json:"timeout_s,omitempty"`
    Failover    string   `json:"failover,omitempty"`
    Errors      []string `json:"errors,omitempty"`
    Input       string   `json:"input,omitempty"`  // ruta a JSON schema del bundle
    Output      string   `json:"output,omitempty"`
    Sealed      bool     `json:"sealed,omitempty"`
}
```

Reglas de nombres (la regla que une el registro viejo con el nuevo):

- `contracts[].key`: `^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*){1,2}$`, único en el **registro cerrado** (§3.3).
- La clave de capability de una operación es `<contract.key>.<op.key>` salvo que `operations[].capability` diga otra cosa. Así `commerce.payments` + `session` = `commerce.payments.session` [V en 6 proveedores del corpus]. Para fiscal, el contrato nuevo `fiscal.document` (op `issue`) conserva la clave legacy con `capability: "fiscal.invoice.create"` (hoy publicada por `fiscal_mexico`); el alias se mantiene los mismos 2 releases.
- Las claves bajo el prefijo `<addon_key>.` del propio addon (p. ej. `mercadopago.payments.brick_submit`) quedan **fuera** del contrato y de R-C: son privadas del addon.

**Qué es un contrato y qué no.** `states`, `provider_settings`, `selection`, `failover` y `health` no son lógica: son datos que el host y la pantalla genérica `contract.providers` leen. No hay `phases` (diferido, §6.2).

### 1.3 `implements[]`

```go
type Implements struct {
    Contract  string   `json:"contract"`            // "commerce.payments"
    Version   string   `json:"version"`             // rango semver "^1.0"
    Countries []string `json:"countries,omitempty"` // ISO-3166 alpha-2 (hoy ProvidedCapability.Countries)
    DocumentTypes []string `json:"document_types,omitempty"` // subconjunto del contrato
    Operations []ImplementedOperation `json:"operations"`
    Events []string `json:"events,omitempty"` // "name@version" que este proveedor emite/consume por contrato
}

// ImplementedOperation es el ProvidedCapability de hoy con otro nombre: QUÉ operación y CÓMO se atiende.
type ImplementedOperation struct {
    Key     string            `json:"key"`
    Handler Handler           `json:"handler"`         // wasm | native (igual que ProvidedCapability)
    Input   map[string]string `json:"input,omitempty"`
}
```

`implements` es **múltiple** (como interfaces): un addon puede implementar contratos de ancestros distintos (`messaging.email@1` y `documents.enrich@1`). `Countries` sube de la operación al contrato porque hoy se repite igual en todas las capabilities de un proveedor [I].

**Puente (2 releases):** `FromV3` proyecta `implements[].operations[]` al `ProvidedCapability` legacy que `capability.Select` ya entiende, así **ops no cambia en R2-b**. En el sentido inverso, un manifest con `provides_capabilities[]` y sin `implements[]` recibe una `implements` implícita agrupando por contrato conocido y el aviso `[provides_capabilities_deprecated]`. Si una clave no pertenece a ningún contrato registrado, se proyecta tal cual (registro abierto) con el aviso `capability_key_unregistered`; R-C la vuelve error recién en la fase enforce.

### 1.4 `extends_addon` y dependencia por contrato

```go
type ExtendsAddon struct {
    Key     string `json:"key"`     // padre ÚNICO
    Version string `json:"version"` // rango semver ("^1")
    // ReexportWritable son modelos del addon RAÍZ de la cadena que este nivel
    // vuelve a abrir a sus descendientes (§1.12). Subconjunto de lo que su padre le abrió.
    ReexportWritable []string `json:"reexport_writable,omitempty"`
}
```

- **Padre único, profundidad ≤ 3, sin diamantes por construcción** (un solo campo, no lista). Los errores de forma (ciclo, profundidad, padre inexistente) son **errores desde el primer día**: ningún manifest publicado puede violarlos porque la clave es nueva.
- `extends_addon` implica `compatibility.requires` del padre (el validador lo añade a la proyección; el autor no lo repite). El instalador instala padre antes que hijo.
- `implements` **no** exige `extends_addon`: un proveedor que solo implementa un contrato (p. ej. `connector_email` → `messaging.email@1`) no hereda esquema ni puntos de extensión.

**Requires por contrato.** `Requirement.key` acepta `contract:<contract.key>` (ejemplo `contract:commerce.payments`), con `version` como rango del contrato. Se satisface con **cualquier addon instalado que implemente** ese contrato. Es la resolución de R-A: hoy `storefront` requiere `payments_manual` y `shipping_flat` (no opcionales) y otros 12 manifests requieren proveedores concretos con `optional: true` [V corpus]. `enables` sigue funcionando igual. Sin esto R-A sería "borrar dependencias" y no "cambiar de objeto".

### 1.5 `replaces[]`

```go
type Replaces struct {
    Key      string `json:"key"`      // clave que este addon sucede
    Versions string `json:"versions"` // rango de la clave vieja que este addon adopta: "<=0.54.99"
}
```

Semántica (la ejecuta el installer, §3.4): si la org tiene instalado `Key` en `Versions`, el installer **migra** esa instalación al addon nuevo conservando credenciales cifradas, bindings, permisos y layouts; deja alias de rutas públicas de la clave vieja ≥ 1 major (webhooks, descargas de CFDI de 5 años) y no permite dejar ambos instalados. **No es lo mismo que `compatibility.provides[]`** (26 manifests lo usan [V]): `provides` dice "cubro la clave X para fines de catálogo" (`hr` cubre `hr_lite`); `replaces` dice "soy el sucesor de X y migro sus datos". Se mantienen los dos.

Solo el plan v2 C8 autoriza `replaces` en `fiscal_mexico`. El validador del hub exige que `replaces` solo lo publique el mismo `owner` que la clave vieja.

### 1.6 `deprecated`

```go
type Deprecation struct {
    Since      string `json:"since"`                // versión del addon/contrato
    ReplacedBy string `json:"replaced_by,omitempty"` // clave o "contrato@versión"
    Sunset     string `json:"sunset,omitempty"`      // fecha ISO-8601; informativa
}
```

Vale para addon, contrato, evento y operación. El host **no bloquea** nada: emite telemetría de uso (plan D19) y el hub lo muestra. Un `implements` de un contrato deprecado da aviso, no error.

### 1.7 Eventos: un solo catálogo (`events`) con versión

Hoy hay **tres** sitios que declaran eventos [V]:

| Sitio | Forma | Uso en el corpus |
|---|---|---|
| `events.emits[]/consumes[]` (RFC 0003, `manifest/v3/event_catalog.go`) | `{name, description, aggregate, payload_schema (objeto), sensitivity, since}` | 5 manifests |
| `extension_points.events[]` | `{name, payload_schema (string), description}`, sin versión | **50 manifests** |
| `contributions.subscriptions[].event` | nombre libre | 25 manifests |

El plan quiere "un solo `events[]` con `version`". **Decisión:** el sitio canónico es `events.emits[]` (RFC 0003), porque ya trae agregado, sensibilidad, límites de tamaño y un validador, y lo consume el panel de integraciones y genui; `extension_points.events[]` es el que tiene menos estructura. Se desvía de la redacción del plan (que nombraba `extension_points.events[]`) pero cumple su intención (un esquema, con versión).

```go
type EmittedEvent struct {
    // ...campos de RFC 0003...
    // Version es el major del contrato del payload: entero >= 1, default 1.
    Version    int          `json:"version,omitempty"`
    Deprecated *Deprecation `json:"deprecated,omitempty"`
}
type ConsumedEvent struct {
    Name    string `json:"name"`
    Version int    `json:"version,omitempty"` // 0 = cualquiera
    // ...
}
```

- **Forma de referencia:** `name@N` (`fiscal.document.settled@1`) en `contracts[].events`, `implements[].events` y `subscriptions[].event`. **El nombre en el bus no cambia** (`fiscal.document.settled`); la versión viaja en el sobre del evento (`event.version`). Así el despacho, el outbox y los 50 manifests actuales siguen funcionando.
- Una suscripción sin `@N` recibe todas las versiones (comportamiento actual). Con `@N` recibe solo esa. Durante una deprecación el emisor declara **dos entradas** con la misma `name` y `version` distinta y publica ambas; el kernel no hace doble emisión automática.
- **Puente (2 releases):** `extension_points.events[]` se proyecta a `events.emits[]{version:1}` y da aviso `[extension_points_events_deprecated]`. Si el mismo `name` está en ambos, gana `events.emits[]`.
- Un evento de contrato (`contracts[].events`) debe existir en `events.emits[]` del mismo manifest (error). Un proveedor que lo lista en `implements[].events` debe declararlo también en su `events.emits[]` con `payload_schema` **compatible** (toda propiedad `required` del contrato está presente con el mismo `type`; comprobación acotada, el resto lo cubre conformidad).
- Un sub-base puede emitir eventos propios namespaced (`l10n_mx.cfdi.stamped`); solo el base re-emite los canónicos (el consumidor nunca depende del país).

Requiere un cambio **sin schema** en `dispatch`: filtrar por `event.version` cuando la suscripción trae `@N`. Va en el PR de installer/dispatch (§8).

### 1.8 `contributions.subscriptions[].ordering`

```go
type Subscription struct {
    // ...existente...
    Ordering *Ordering `json:"ordering,omitempty"`
}
type Ordering struct {
    Phase string   `json:"phase,omitempty"` // "post" (default) | "pre" (RESERVADO)
    After []string `json:"after,omitempty"` // claves de addon cuya entrega del MISMO evento debe terminar antes
}
```

- `post` (default): el despachador no entrega el evento a este suscriptor hasta que cada addon de `after[]` tenga la entrega del mismo `event_id` confirmada en `event_deliveries`. Sin `after`, no hay orden entre hermanos. `after` solo puede nombrar al **padre o a un ancestro** del addon o a un `foundation`/`contract` del que dependa (R-A: nunca hacia abajo ni a un hermano); ciclos de `after` son error.
- `pre` (síncrono dentro de la transacción) **se acepta en el schema y el validador lo rechaza como reservado**: los handlers WASM no tienen transacción compartida (ABI sin tx/FOR UPDATE) y su semántica es parte del spike de `phases` (plan RS5, §6.2). Reservarlo ahora evita un bump de schema después.

### 1.9 `contributions.inject[]` y el catálogo de slots unificado

**Qué se reusa de #550.** El tipo `Inject` y sus cuerpos (`InjectColumn`, `InjectBadge`, `InjectFilter`), `MaxInjectPerAddon = 64`, las bandas de `order` (dueño del modelo `0..199`, otro addon `200+`), la condición server-side, `ValidateInject`/`InjectEnv` (una sola regla compartida por el validador v3 y el legacy), `FromV3` para `Inject`, y los 3 archivos de prueba (`inject_test.go`, `inject_review_test.go`, `from_v3_inject_test.go`). Se **rebasa** el código de `inject.go` en el PR de R2-b; #550 se cierra con enlace aquí (§8, K-3).

```go
type Contributions struct {
    // ...existente...
    Inject []Inject `json:"inject,omitempty"`
}
```

`Inject` queda como en #550 con tres cambios:

1. `Slot` toma nombres del catálogo unificado (abajo).
2. `Target` con rol `@dominio.entidad` se resuelve **por `contracts[].models[]`** del addon dueño (§1.2): `{"role":"commerce.order","model":"SalesOrder"}`. En #550 el rol decía resolverse al "read contract `commerce.order@1`", pero ese contrato no existía en el schema; aquí sí.
3. Se elimina la referencia a `extension_points.primary_from`.

**Un catálogo.** Se extiende `ops.host/v1` (`v3.HostSlotCatalogOpsV1`, `docs/spec/v3/slots.md`) y se retira `slots@1`.

*Regla de nombres:* `<superficie>.<parte>`, minúsculas, un solo punto, **singular** siempre, superficie ∈ {`dashboard`, `record`, `list`, `form`, `settings`, `topbar`, `page`}. Un slot es un **lugar**, no un tipo de cuerpo: el catálogo declara qué cuerpos acepta cada lugar. El catálogo se versiona con el identificador `ops.host/v1`: agregar nombres es aditivo dentro de v1 (la regla `slot_kind_unknown` ya los trata como aviso); renombrar o quitar es `ops.host/v2`. No hay un segundo identificador de versión.

| `ops.host/v1` (unificado) | #550 `slots@1` | Cuerpos aceptados | Contexto |
|---|---|---|---|
| `dashboard.widget` (existe) | `dashboard.widgets` | componente federado (`contributions.slots`), `widget` (`inject`) | ninguno |
| `record.panel` (existe) | `record.panels` | componente, `panel` | `{model, record_id}` |
| `record.tab` (**nuevo**) | `record.tabs` | `tab`, `panel` | `{model, record_id}` |
| `record.badge` (**nuevo**) | `record.header.badges` | `badge` | `{model, record_id}` |
| `record.action` (existe) | (fuera de `slots@1`) | componente; `contributions.actions[]` | `{model, record_id}` |
| `list.toolbar` (existe) | — | componente | `{model}` |
| `list.column` (**nuevo**) | `list.columns` | `column` | `{model}` |
| `list.filter` (**nuevo**) | `list.filters` | `filter` | `{model}` |
| `settings.section`, `topbar.item`, `page.header` | — | componente | como hoy |
| `form.section` (reservado, sin cuerpos inject) | `form.sections` | `models[].extensions[]` (no inject) | — |

Cuerpo por defecto de `inject[].slot` si se omite: `panel→record.panel`, `tab→record.tab`, `column→list.column`, `badge→record.badge`, `filter→list.filter`, `widget→dashboard.widget`. Como #550 no está mergeado, **no hay alias** para los nombres plurales. En Go: `HostSlotCatalogOpsV1 []string` se conserva (compat) y se añade `HostSlots() []HostSlot{Name, Bodies}` como fuente única; el schema JSON referencia la lista generada.

`contributions.slots[]` (componente federado) e `inject[]` (cuerpo declarativo) **no se fusionan**: el primero monta código remoto, el segundo es dato servido por el host. Comparten lugares, no semántica. Hoy `contributions.slots` tiene **0 usos en el corpus** [V], lo que vuelve gratis acordar el catálogo ahora.

### 1.10 `sealed` (booleano)

`sealed: true` en: `models[]`, `contracts[]`, `contracts[].operations[]`, `models[].rules[]`, `contributions.actions[]`. Significa "ningún otro addon contribuye sobre esto":

| Objeto | Efecto de `sealed: true` |
|---|---|
| Modelo | `inject[]` con ese `target` se sirve inerte y se rechaza al validar en el hub; `actions[].target_model` ajeno, `extends` y `relates_to` sobre él, también. Reemplaza la lista `extension_points.sealed[]` de #550 (que vivía lejos del modelo y podía quedar con nombres obsoletos). |
| Contrato | Ningún `implements` puede añadir operaciones fuera de la lista ni `events` fuera de `contracts[].events`. |
| Operación | Los proveedores no pueden suscribirse con `ordering.phase: pre` a sus eventos (hoy reservado) ni añadir `rules` con `target` sobre ella. |
| Regla (`Model.rules[]`) | `contributions.rules[].target` ajeno no puede debilitarla ni eliminarla. |
| Acción | `target_model` ajeno no puede ocultarla ni reemplazarla. |

Invariantes de dinero, idempotencia y "indeterminado → `pending`" se marcan `sealed` en el base. Sin `overridable` en este tren.

### 1.11 `lifecycle.uninstall_data` (P12)

El plan lo escribe `lifecycle.uninstall.data`, pero `Lifecycle.Uninstall` es hoy un `string` (nombre del hook) y 17 manifests tienen `lifecycle` [V]. Hacerlo objeto exige unión `string|object` en el schema, en el unmarshal y en el codegen del SDK. **Decisión técnica:** clave hermana:

```go
type Lifecycle struct {
    // ...existente...
    UninstallData *UninstallData `json:"uninstall_data,omitempty"`
}
type UninstallData struct {
    Policy string   `json:"policy"` // "retain" (default si se omite) | "purge"
    Models []string `json:"models,omitempty"` // purga selectiva; vacío = todos los propios
}
```

`retain`: se desinstala el addon, las filas de `addon_<key>.*` se conservan (no se borra el schema; reinstalar las reencuentra). `purge`: exige confirmación tipada en el host y `Uninstall(..., dropSchema=true)`. Si el addon tiene **dependientes instalados** (`Dependents`, H11), la desinstalación se bloquea salvo cascada explícita que desinstala primero las hojas. Las filas del base nunca se borran por desinstalar una hoja (`extends` usa `ON DELETE CASCADE` del hijo al padre [V `dynamic/extends_ddl.go`], no al revés). Artefactos canónicos (XML/PDF) viven en el base.

### 1.12 Esquema compartido: `relates_to`, `writable_models`, `reexport_writable`

**Hoy [V]:** `Model.extends: "<addon>.<Model>"` es 1:1 (id = PK = FK, `ON DELETE CASCADE`), exige que el dueño publique el modelo en `extension_points.model_extensions_accepted`, y **no puede apuntar a otra extensión** (`extends_validate.go`). Cada nivel de la cadena extiende al **mismo modelo raíz** (estrella), lo cual ya respeta esa regla.

```go
type Model struct {
    // ...existente...
    RelatesTo *RelatesTo `json:"relates_to,omitempty"`
    Sealed    bool       `json:"sealed,omitempty"`
}
type RelatesTo struct {
    Model    string `json:"model"`     // "fiscal_core.FiscalDocument"
    Column   string `json:"column"`    // columna FK de ESTE modelo (obligatoria, tipo ref)
    OnDelete string `json:"on_delete"` // "cascade" | "restrict" (default restrict)
}
```

`relates_to` es la 1:N con FK para tablas hijas que no son 1:1 (eventos de timbrado por documento). Misma validación que `extends`: el dueño debe aceptarla en `model_extensions_accepted` y el modelo destino no debe ser `sealed`.

**Escritura (R-B):** conjunto escribible de un addon en la cadena:

```
writable(A) = tablas propias(A)  ∪  (opens(parent(A)) ∩ writable_recibido_de_su_padre)
opens(base)  = contracts[].writable_models      (del base)
opens(medio) = extends_addon.reexport_writable  (subconjunto de lo recibido)
```

La apertura es transitiva hacia abajo **solo si el intermedio la reexporta**. Enforcement doble: manifest (los `capabilities` `db:write` de `provider`/`extension` ⊆ `writable`) y host (`CheckCapability db:write`, `runtime/wasm/databatch.go:135`, `datamutate.go:169` [V]; este RFC no cambia el host, solo el conjunto contra el que valida, que hoy es "lo declarado").

**Migraciones por cadena:** orden base → sub-base → hoja (toposort de `ResolveChain`); una migración de hijo cuyo DDL toque otro schema es error de validación; la migración del hijo exige el schema mínimo del padre (`extends_addon.version`).

### 1.13 `capability_bindings` (tabla, no manifest)

Reemplaza a `org_market_providers` (`organization_id, market_code, domain, addon_key, priority, enabled` [V en ops]) 1:1:

```go
// package capability
type Binding struct {
    ID         uint      `gorm:"primaryKey"`
    OrgID      uuid.UUID `gorm:"type:uuid;not null;uniqueIndex:ux_cap_binding,priority:1"`
    Contract   string    `gorm:"not null;uniqueIndex:ux_cap_binding,priority:2"` // contracts[].key
    ScopeKind  string    `gorm:"not null;uniqueIndex:ux_cap_binding,priority:3"` // org|market|branch|country
    ScopeValue string    `gorm:"not null;uniqueIndex:ux_cap_binding,priority:4"`
    Provider   string    `gorm:"not null;uniqueIndex:ux_cap_binding,priority:5"` // addon key
    Priority   int       `gorm:"not null;default:100"`
    Mode       string    `gorm:"not null;default:'primary'"` // primary|failover
    Rule       *string   // reservado, JSON; sin uso en R2-b
    Enabled    bool      `gorm:"not null;default:true"`
}
```

Migración SQL en el kernel con la regla de numeración de la carpeta (colisiones, memoria `feedback_migration_number_collision`): `market_code → (scope_kind='market', scope_value=market_code)`, `domain → contract` por el registro de contratos (`payments → commerce.payments`), filas con `domain` desconocido **no se migran y se reportan** (conteo en la migración). `capability.Select` v2 lee esta tabla como única fuente; ops borra su resolvedor propio en su PR. No hay JSON schema ni validador de manifest asociados. El índice es total (no parcial), por lo que no aplica el problema de soft-delete de `uniqueIndex` [V memoria `feedback_gorm_partial_index_softdelete`]: la tabla no usa soft-delete.

---

## 2. Qué se reusa de #550 y qué se descarta (con argumentos)

| Pieza de #550 | Decisión | Argumento |
|---|---|---|
| `Inject` + cuerpos, `ValidateInject`/`InjectEnv`, `MaxInjectPerAddon`, bandas de `order` | **Reusar** | Son 2.021 líneas añadidas en 15 archivos, con pruebas, ya separan validación compartida v3/legacy y no dependen de nada descartado. Reescribir viola "copy+edit sobre rewrite". |
| `FromV3` de `Inject` (+ `from_v3_inject_test.go`) | **Reusar** | La proyección ya está probada; solo cambia el nombre del campo de sealed. |
| `RelationPanel.via` | **Reusar** | Es parte del cuerpo `panel` de `inject`. |
| `actions[].target_model` calificado o por rol (`ActionTargetProblem`) | **Reusar, con veto posible (N4)** | Mismo patrón de direccionamiento que `inject.target`, sin esto los botones cross-addon no tienen canal. Acepta clave desnuda (compat). |
| Catálogo `slots@1` (plural) | **Descartar** | Ya existe `ops.host/v1` en `docs/spec/v3/slots.md` y en `HostSlotCatalogOpsV1`; dos catálogos obligan al autor a saber cuál aplica a cada clave y al host a mantener dos listas. Mapa de equivalencias en §1.9. |
| `extension_points.sealed[]` (lista de modelos) | **Descartar por `sealed` booleano** | La lista vive lejos del objeto y puede nombrar modelos que ya no existen; el booleano en el objeto es válido por construcción y también cubre contrato, regla y acción (el plan v2 lo pide así). |
| `extension_points.primary_from[]` | **Descartar** | (a) No está en el plan: es un primitivo nuevo (3 bumps) sin caso real, **0 manifests del corpus lo necesitarían** hoy; (b) el problema ("nadie roba el botón principal") se resuelve con la banda de `order` de #550 (dueño `0..199`, otros `200+`) más una regla del host: una acción contribuida desde otro addon se limita a `secondary`, sin lista blanca. Si aparece un caso real (p. ej. `pos` pidiendo botón primario sobre un modelo ajeno), se agrega aditivo en R3 con el caso en mano. |
| `DashboardWidget.kind: map` | **Descartar de R2-b** | Es de genui, no de cadena. El mapa ya existe como bloque genui v2 (#634, #636 mergeados) y el estándar #641 mueve los widgets hacia `kind: block` sobre bloques con nombre. Un `kind: map` distinto del bloque `map` duplicaría el primitivo. Se discute en #641. |
| Pruebas (`inject_test`, `inject_review_test`, `from_v3_inject_test`) | **Reusar**, ajustando nombres de slots y la sección de sealed | Cubren 618 líneas de reglas. |

### 2.1 Cómo se entrega

No se mergea #550: se **rebasa en un PR nuevo** (K-3, §8) tomando `manifest/v3/inject.go`, sus pruebas y la proyección `FromV3` con los cambios de la tabla, y #550 se cierra enlazando ese PR. Razón: #550 mezcla en un PR cosas descartadas (`primary_from`, `slots@1`, widget `map`) que obligarían a revertir tras el merge, y un merge parcial rompe su historial `X2.1a`.

### 2.2 Qué no se pierde al descartar `primary_from`

La garantía "otro addon no puede quitar el botón principal de mi modelo" queda con tres capas: bandas de `order`, tope `secondary` por el host para acciones ajenas, y `sealed` en la acción o modelo cuando el dueño quiere prohibirlo del todo.

### 2.3 Cadena de bumps (una sola, en este orden)

| Paso | Repo | Qué hace | Puerta |
|---|---|---|---|
| 1 | kernel | Schema + validadores + `ResolveChain` + installer + `capability_bindings`. Release minor `vX.Y.0`. | CI kernel + corpus (abajo) |
| 2 | sdk | Bump de kernel; codegen de tipos desde el JSON schema (nuevas interfaces `Contract`, `Implements`…); editor de manifest. | `pnpm install --frozen-lockfile` (memoria `feedback_pnpm_lockfile_verify_frozen`) y build |
| 3 | hub | Bump de kernel; el publicador ejecuta `ResolveChain` contra el catálogo; indexa `role/contracts/implements`; la certificación `CONF-*` se guarda por (addon, versión, cadena). Acepta publicar manifests sin y con las claves nuevas. | `check-hub-kernel-compat.sh` |
| 4 | ops | Bump de kernel + SDK; lee `capability_bindings`; installer con orden por cadena y bloqueo por dependientes; UI `contract.providers` y desinstalación con política. | smoke con el corpus |
| 5 | addons | `manifestcheck` bump (modo `role_lint`), `role` en los 99 manifests (script), addons base nuevos, división de `fiscal_mexico`. | corpus verde |

Todo lo ejecuta `release-train` (plan S4); el merge de cada PR sigue requiriendo autorización explícita del turno.

### 2.4 Puerta de salida: el corpus de 99 manifests

Medido en `addons` `origin/main` (`5400a649`): **86** en `packages/*/manifest.json`, **5** en `verticals/*`, **6** en `integrations/*`, **1** en `appliance/ops` = **98**, más el fixture `tools/manifestcheck/testdata/privatelint/hub_fleet_spike` = **99**. [V] El plan y el dueño hablan de "99"; la cifra cuadra si se cuenta el fixture. Los 91 de `packages/` + `verticals/` están en `apiVersion: asteby.com/v3`, ninguno declara ya `role`, `contracts`, `implements` ni `capability_bindings`, 22 declaran `provides_capabilities`, 50 `extension_points.events`, 26 `compatibility.provides`, 25 `contributions.subscriptions`, 17 `lifecycle` y **0** `contributions.slots`.

El release de kernel no se publica hasta que un job de CI (kernel PR K-2, contra `addons` `main`) cumpla **las tres**:

1. **Sin regresión de validación:** los 99 manifests pasan `ValidateWithOptions` en modo no-strict con 0 errores nuevos; en strict solo aparecen avisos de reglas nuevas (`role_inferred`, `provides_capabilities_deprecated`, `extension_points_events_deprecated`, R-A/R-B/R-C) y ningún error.
2. **Proyección legacy idéntica:** `FromV3` de cada manifest que no usa claves nuevas produce JSON **idéntico byte a byte** al del kernel anterior (golden generado con el commit previo). Es la prueba de que "claves opcionales" es cierto.
3. **Inventario de avisos** como artefacto del job (por regla y por addon), para dimensionar la fase enforce.

Y para pasar de aviso a error (§5): 99/99 con `role` explícito y 0 violaciones R-A/R-C en lo migrado (gate R2-b del plan), medido por `manifestcheck --role-lint`.

---

## 3. Cómo se resuelve la cadena en runtime

### 3.1 `ResolveChain` único

Se crea `manifest/v3/chain.go` (paquete `v3`; `manifest` ya lo importa, no hay ciclo). Opera sobre una proyección mínima, no sobre el manifest completo, para servir al v3 **y** a la portadora legacy que usa el installer:

```go
type ChainNode struct {
    Key      string
    Version  string
    Role     string
    Extends  *ExtendsRef    // {Key, Range}
    Requires []ChainRequire // {Key, Range, Optional}; Key puede ser "contract:<k>"
    Implements []string     // contract keys
    Replaces []string
    Provides []string       // compatibility.provides (equivalencias)
    Contracts []string      // contract keys que publica
}

type ChainResult struct {
    Order      []string            // base -> hoja, determinista (empates por clave)
    Ancestors  map[string][]string // key -> [padre, abuelo...]
    Depth      map[string]int      // 1 = base
    Dependents map[string][]string // inverso de requires ∪ extends
    Providers  map[string][]string // contract key -> implementadores del conjunto
    Findings   []ChainFinding      // {Code, Severity, Addon, Msg}
}

// Lookup resuelve nodos fuera del conjunto: instalados (installer), catálogo (hub), corpus (CI).
func ResolveChain(set []ChainNode, lookup func(key string) (ChainNode, bool)) (ChainResult, error)
func (m *Manifest) ChainNode() ChainNode         // v3
// manifest.Manifest (legacy) expone el mismo método, vía from_v3.
```

Códigos de error (todos duros desde el día 1, porque solo son alcanzables con claves nuevas): `chain_cycle`, `chain_depth` (> 3 niveles de addon), `chain_parent_missing`, `chain_parent_version`, `chain_replaces_conflict`, `chain_contract_unresolved` (un `contract:<k>` no obligatorio sin implementador solo avisa). Reglas R-A..R-E son `Findings` con severidad según la ventana (§3.3).

El **orden** es toposort por `extends_addon` ∪ `requires` ∪ `contract:` resuelto. Una arista opcional que cerraría un ciclo se descarta (semántica actual de H11).

### 3.2 Quién lo llama

| Llamador | Lookup | Modo | Qué obtiene |
|---|---|---|---|
| **hub**, al publicar | catálogo publicado (`role` indexado) | strict | rechaza ciclos/profundidad/padre inexistente; avisa o rechaza R-A..R-E según ventana; un público no puede extender a un `private` |
| **installer** (kernel) | addons instalados de la org ∪ lote | no-strict | `installer.ResolveOrder` y `ResolveInstall` **pasan a envolver `ResolveChain`** (hoy ordenan `requires` sobre `manifest.Manifest`, `installer/requires.go:322` [V]); `Dependents` sale del mismo resultado; migraciones en `Order` |
| **`manifestcheck`** (addons CI) | corpus del repo | strict | reemplaza `devctl orderByRequires`; `--role-lint=warn|enforce` |
| **resolvedor de presets** | addons del preset ∪ catálogo | no-strict | expande el preset a su cierre por cadena y lo ordena; un preset (`role: preset`) no entra a la cadena de otros |

`ResolveOrder`/`ResolveInstall` conservan su firma pública: la mitad de H11 (#651) queda intacta y su suite de pruebas es la primera prueba de regresión de `ResolveChain`.

### 3.3 Reglas R-A a R-E con N niveles

| Regla | Enunciado | Ventana |
|---|---|---|
| **R-A** | Un addon solo depende hacia **arriba** (padre, ancestros, `foundation`, `contract`). Ningún no-`extension` hace `requires` de un descendiente suyo ni de un `provider` concreto: usa `contract:<k>`. `pos` no depende de ningún `provider`. | Aviso → error. **13 manifests del corpus la violan hoy** (§4). |
| **R-B** | `db:write` de un `provider`/`extension` ⊆ `writable(A)` (§1.12) ∪ `addon_<key>.*`. | Aviso → error. `fiscal_mexico` declara 13 destinos de escritura ajenos al schema propio [V]. |
| **R-C** | Capabilities que publica un descendiente ⊆ unión de los contratos de su cadena (más el prefijo propio `<addon_key>.`). Clave desconocida = error. | Aviso → error |
| **R-D** | La navegación de un `provider`/`extension` solo entra por el slot que publica el ancestro más cercano (`contract.providers`, `record.panel`). | Aviso → error (el corpus no usa slots aún) |
| **R-E** | Ningún hijo redeclara una clave (modelo, acción, evento, setting, permiso, slot) que ya exista en un ancestro. Sin `overrides` en este tren (`overridable` está diferido). | Error desde el día 1 (solo alcanzable con `extends_addon`) |

### 3.4 Installer: reemplazo y desinstalación

1. `Install(batch)` → `ResolveChain` → instala en `Order`. Falla atómica del lote ante `chain_*`.
2. `replaces`: antes de instalar `fiscal_core`+hijos, para cada org con `fiscal_mexico` en rango, `Replace` mueve (a) `connectors` credentials sin descifrar fuera del servicio de secretos; (b) `capability_bindings`; (c) permisos y layouts por clave; (d) deja alias de rutas `webhookin` de la clave vieja ≥ 1 major; (e) registra la reversión (instalar v2, volver a v1, reinstalar v2 con datos, plan §8.1). Un fallo deja la instalación vieja intacta.
3. `Uninstall`: política de §1.11; bloqueado por `Dependents` salvo cascada.

---

## 4. Migración de los casos reales

### 4.1 Fiscal: `fiscal_mexico` → `fiscal_core` + `l10n_mx` + `fiscal_mx_facturacom`

Nivel (profundidad): `fiscal_core` (N1, `contract`) → `l10n_mx` (N2, `localization`, `extends_addon: fiscal_core`) → `fiscal_mx_facturacom` (N3, `provider`, `extends_addon: l10n_mx`).

**Corrección vs el plan v1:** `fiscal_mx_cartaporte` no es un cuarto nivel. Hoy el addon real es `waybill-cartaporte` (v0.8.0, `requires: fiscal_mexico`) [V]. La carta porte es un **tipo de documento** (`transport`) declarado en `fiscal_core` (`contracts[].document_types`) y cada proveedor lo anuncia en `implements[].document_types`. El addon de carta porte pasa a `extends_addon: l10n_mx` (3.er nivel, hermano de la hoja proveedor, **sin atarse a un proveedor**) y su `requires: fiscal_mexico` se vuelve `contract:fiscal.document`.

`fiscal_core` (base, parcial):

```json
{
  "apiVersion": "asteby.com/v3", "kind": "Addon",
  "metadata": { "key": "fiscal_core", "name": "Fiscal", "version": "1.0.0", "role": "contract", "category": "fiscal" },
  "compatibility": { "requires": [ { "key": "kernel", "version": ">=3.0.0 <4.0.0" } ] },
  "contracts": [{
    "key": "fiscal.document", "version": "1.0.0",
    "operations": [
      { "key": "issue",     "kind": "action", "capability": "fiscal.invoice.create",   "idempotent": true, "timeout_s": 180, "failover": "never", "sealed": true },
      { "key": "status",    "kind": "query",  "capability": "fiscal.invoice.status",   "failover": "allowed" },
      { "key": "download",  "kind": "query",  "capability": "fiscal.invoice.document", "required": false },
      { "key": "cancel",    "kind": "action", "idempotent": true, "failover": "never" }
    ],
    "events": [ "fiscal.document.settled@1" ],
    "states": { "model": "FiscalDocument", "field": "stage", "indeterminate": "pending" },
    "models": [ { "role": "commerce.invoice", "model": "FiscalDocument" } ],
    "writable_models": [ "FiscalDocument" ],
    "document_types": [ "invoice", "credit_note", "transport" ],
    "selection": { "cardinality": "one", "scopes": ["org", "branch", "country"] },
    "conformance": "CONF-FISCAL@1"
  }],
  "events": { "emits": [ { "name": "fiscal.document.settled", "version": 1, "aggregate": "fiscal_document",
                           "payload_schema": { "type": "object", "required": ["id", "uuid"], "properties": { "id": {"type":"string"}, "uuid": {"type":"string"} } } } ] },
  "extension_points": { "model_extensions_accepted": [ "FiscalDocument" ] },
  "models": [ { "key": "FiscalDocument", "sealed": false } ],
  "lifecycle": { "uninstall_data": { "policy": "retain" } }
}
```

`l10n_mx` (hijo, sub-base, reglas de la autoridad como datos):

```json
{
  "metadata": { "key": "l10n_mx", "version": "1.0.0", "role": "localization", "countries": ["MX"] },
  "extends_addon": { "key": "fiscal_core", "version": "^1", "reexport_writable": [ "FiscalDocument" ] },
  "models": [ { "key": "MxFiscalDocument", "extends": "fiscal_core.FiscalDocument",
                "columns": [ { "key": "uso_cfdi" }, { "key": "regimen" }, { "key": "exportacion" } ] } ],
  "events": { "emits": [ { "name": "l10n_mx.cfdi.stamped", "version": 1 } ] },
  "lifecycle": { "uninstall_data": { "policy": "retain" } }
}
```

`fiscal_mx_facturacom` (hoja proveedor, parcial):

```json
{
  "metadata": { "key": "fiscal_mx_facturacom", "version": "1.0.0", "role": "provider" },
  "extends_addon": { "key": "l10n_mx", "version": "^1" },
  "implements": [{
    "contract": "fiscal.document", "version": "^1.0", "countries": ["MX"],
    "document_types": [ "invoice", "credit_note", "transport" ],
    "operations": [
      { "key": "issue",  "handler": { "type": "wasm", "function": "cfdi_issue" } },
      { "key": "status", "handler": { "type": "wasm", "function": "cfdi_status" } }
    ],
    "events": [ "fiscal.document.settled@1" ]
  }],
  "replaces": [ { "key": "fiscal_mexico", "versions": "<=0.54.99" } ],
  "connectors": [ { "key": "facturacom" } ]
}
```

Hecho verificable que obliga a esta forma: hoy `fiscal_mexico` escribe en `suppliers`, `purchase_orders`, `sales_orders`, `invoices`, `customers`, `sales_returns`… (R-B) y publica `fiscal.invoice.create|status|document`. Tras la división, el único escritor ajeno pasa a ser lo que `writable_models` abra: `FiscalDocument`. Las escrituras a `sales_orders`/`customers` se reemplazan por **eventos** (`fiscal.document.settled@1`) y proyecciones; ese rediseño es del PR de la división, no de este RFC.

**Orden de las migraciones** (§3.4): `fiscal_core` → `l10n_mx` → `fiscal_mx_facturacom`; `CONF-FISCAL@1` y los vectores REG-FAC tienen que dar el mismo resultado antes y después (gate R2-b). El plan v2 D12 decide **cuándo** (durante el piloto); este RFC solo garantiza que el schema ya lo soporta.

### 4.2 Pagos: `payments_core` + proveedores, sin renombres

Claves reales del corpus: `stripe`, `mercadopago`, `openpay`, `conekta`, `paypal`, `clip`, `payments_manual`, `cash-recycler`, `payment-terminals` [V]. **No se renombran** (plan v2 C8; los webhooks de MP/Stripe llevan la clave en la ruta, riesgo RS3). Lo único nuevo es el addon base **`payments_core`**, sin WASM, dueño del contrato; los proveedores ganan `role: provider` + `implements`:

```json
// payments_core (nuevo)
"metadata": { "key": "payments_core", "role": "contract", "version": "1.0.0" },
"contracts": [{ "key": "commerce.payments", "version": "1.0.0", "conformance": "CONF-PAYMENTS@1",
  "operations": [
    { "key": "session", "kind": "action", "idempotent": true },
    { "key": "confirm", "kind": "action", "idempotent": true },
    { "key": "refund",  "kind": "action", "idempotent": true, "failover": "never" },
    { "key": "status",  "kind": "query" },
    { "key": "client", "kind": "query", "required": false }, { "key": "submit", "kind": "action", "required": false },
    { "key": "cancel", "kind": "action", "required": false }, { "key": "capabilities", "kind": "query", "required": false },
    { "key": "reconcile", "kind": "action", "required": false },
    { "key": "terminal.create", "kind": "action", "required": false }, { "key": "terminal.devices", "kind": "query", "required": false } ],
  "selection": { "cardinality": "one", "scopes": ["org", "branch", "market"] } }]
```

```json
// stripe (proveedor existente; claves intactas)
"metadata": { "key": "stripe", "role": "provider" },
"implements": [{ "contract": "commerce.payments", "version": "^1.0",
  "operations": [ { "key": "session", "handler": { "type": "wasm", "function": "..." } } ] }]
```

- **CONF-PAYMENTS@1 ya existe** (`payments/conformance`, `docs/conformance-payments.md`) y usa exactamente las claves `commerce.payments.session|confirm|status|refund`: `contracts[].conformance: "CONF-PAYMENTS@1"` solo lo enlaza; no hay cambio de la suite.
- `mercadopago.payments.brick_submit` queda **fuera del contrato** por el prefijo propio (§1.2), no como violación R-C.
- `payments_manual` es proveedor aunque lo requieran como no-opcional (`storefront`): pasa a `contract:commerce.payments`.
- `pos` (hoy `mercadopago?` opcional) y `caja`/`kiosk`/`storefront_cart`: `requires` → `contract:commerce.payments` con `enables` equivalente.
- Un `extension` (terminal `Point`) se modela, si hace falta, como `extends_addon: mercadopago` (2 niveles); hoy `payment-terminals` y `cash-recycler` ya implementan subconjuntos del contrato [V], sin ser extensiones de un proveedor concreto.
- `capability_bindings` migra `org_market_providers`: el `domain` "payments" se mapea a `commerce.payments`.

### 4.3 Mensajería: `messaging_core` → proveedores de correo (y WhatsApp)

Hoy [V]: `connector_email` v0.8.0 (sin `requires` más que `kernel`, sin `provides_capabilities`), `mail` v0.3.1 (bandeja, modelos propios) y **dos proveedores del mismo contrato** `messaging.whatsapp.send`: `link` y `connector_whatsapp`.

- Se crea **`messaging_core`** (nuevo, `contract`, sin WASM) con `messaging.email` y `messaging.whatsapp`. Se justifica por el segundo consumidor ya existente (`link` y `connector_whatsapp` son proveedores del mismo contrato) y por el plan C8 ("separar proveedores solo bajo demanda").
- `connector_email` **conserva su clave** y gana `role: provider` + `implements: messaging.email@1` + `provider_settings`. **No** se crean `email_sendgrid` ni `email_*` en este tren: nacen cuando entre un segundo proveedor real, ya con el schema soportándolos.
- `mail` es `app`; `storefront_notifications`, `quotes`, `customers` (hoy `connector_email?`) pasan a `contract:messaging.email`.
- `link` y `connector_whatsapp`: `implements: messaging.whatsapp@1`; el binding por org decide cuál usar (hoy `capability.Select` elige el "default de la org").

### 4.4 El resto de familias

`shipping` (`skydropx`, `envia`, `emissary`, `noventa9minutos`, `shipping_flat`: 5 proveedores de `commerce.shipping.*` [V]), `tax` (`stripe_tax`), `fx` (`fx_providers`), `geo` (`geo_photon`), `search` (`search_basic`): **rol + contrato, sin renombre**, con contratos base pequeños (`shipping_core`, etc.) cuando se priorice. No son parte del gate de R2-b; solo prueban que el schema las cubre. `billing-sat` se elimina en M0.8 (ya en plan).

---

## 5. Compatibilidad y modo warn/enforce

Política vigente (`docs/validation-compat-window.md`): una regla que puede invalidar un manifest publicado nace como aviso un minor, luego error. Mecanismo: `tolerate(ruleID, msg)` + `legacyTolerated` + `warnOnlyWindow` en `manifest/v3/compat.go` [V].

| Regla | Release N (R2-b) | Release N+1 | Release N+2 |
|---|---|---|---|
| `role_inferred` / `role_missing` | aviso (ambos modos) | `role_missing` error en strict | error |
| `provides_capabilities_deprecated` | aviso | aviso | se **retira** la lectura (se lee 2 releases) |
| `extension_points_events_deprecated` | aviso | aviso | se retira |
| `capability_key_unregistered` (R-C) | aviso (ambos) | error en strict (publicar) | error |
| R-A (`requires` a provider) | aviso (ambos) | error en strict | error |
| R-B (`db:write` fuera de lo abierto) | aviso (ambos) | error en strict | error |
| R-D | aviso (ambos) | error en strict | error |
| `chain_*`, R-E, `sealed` violado, forma de cualquier clave nueva | **error** | error | error |
| `slot_kind_unknown` para los slots nuevos | aviso (existente) | igual | promoción ya planeada en `slots.md` |
| `ordering.phase: pre` | error ("reservado") | error | decide el spike |

"Ambos modos" = `warnOnlyWindow`: se reporta aviso incluso con `Options.Strict`, de modo que **el hub no rechaza publicaciones por reglas nuevas en el release que las introduce** (lección del incidente de 2026-10-04 y del bump v0.230.0). Quien consume bundles ya publicados (ops, installer) sigue en no-strict.

`manifestcheck --role-lint=warn|enforce` (flag nuevo en addons): `warn` por defecto en el release N; `enforce` se enciende por **repo**, no por kernel, cuando el inventario del job de §2.4 reporta 0 violaciones en lo migrado.

Compatibilidad N-1 (plan §6): el kernel lee manifests del release anterior durante 2 releases (la proyección `FromV3` es la que lo garantiza); el hub acepta publicar ambos; el corpus lo prueba en CI de los 4 repos.

---

## 6. Riesgos y alternativas descartadas

### 6.1 Riesgos

| # | Riesgo | Mitigación |
|---|---|---|
| R1 | La proyección legacy cambia bytes y rompe `ops`/`capability.Select` sin que lo vea una prueba. | Golden byte a byte del corpus (§2.4, punto 2). |
| R2 | El corpus real tiene 13 violaciones R-A con `optional`; sin `contract:` el enforce rompe el piloto. | `contract:<k>` en el mismo release; enforce solo tras 0 violaciones. |
| R3 | `replaces` rompe rutas públicas, credenciales, entitlements, layouts (RS3). | Solo `fiscal_mexico`; alias de rutas ≥ 1 major; migración con conteos antes/después; prueba de reversión; credenciales sin descifrar fuera del servicio de secretos. |
| R4 | Dos catálogos de slots (`ops.host/v1` y `slots@1`) divergen si #550 se mergea. | Se rebasa, no se mergea (§2.1); un solo `HostSlots()`. |
| R5 | El hub valida cadenas cruzadas con un catálogo incompleto y rechaza hijos válidos. | `Lookup` por catálogo con rango de versión; en caso de duda es aviso (`chain_parent_unverifiable`), no error, hasta que el índice del hub sea fiable. |
| R6 | Un solo release es grande (3 bumps encadenados, plan estima ≈ 5 semanas, 2 agentes). | PR por capa (§8), cada uno revisable; flags en modo aviso; el release solo se corta cuando el corpus pasa. |
| R7 | `pre` síncrono prometido y no entregable. | Reservado y rechazado por el validador (§1.8). |
| R8 | La comprobación de compatibilidad de `payload_schema` entre contrato y proveedor da falsos positivos. | Acotada (`required` + `type`); el resto lo cubre conformidad (§1.7). |
| R9 | Cruce con #633: su fase F7 reescribe `fiscal_mexico` (aspecto `fiscal.issuer`, `legal_identity.resolve@1`). | Orden: F7 de #633 se implementa sobre `fiscal_core` (el contrato `legal_identity.resolve@1` es un `contracts[]` más), no sobre `fiscal_mexico` (§9). |

### 6.2 Fuera de este tren (decisión del plan, no se reabre aquí)

`phases` con modos `one/first/chain/all` y super-call por host, `overridable`, `min_parent_schema`, `contract_event` en suscripciones, `read_models`/`contract:read`: R3 o después del spike de M0.13 (plan C9, RS5, D17). Mientras tanto la extensión de comportamiento es **eventos + `inject` + `rules`** (sin cadena síncrona). `sealed` en operaciones ya deja la puerta alineada con esas fases.

### 6.3 Alternativas descartadas

| Alternativa | Por qué no |
|---|---|
| Fusionar `provides_capabilities` y `implements` en el mismo campo | Obliga a releer 22 manifests sin una vía de aviso; `implements` también nombra contrato, versión y tipos de documento. |
| `extends_addon` como lista (multi-herencia) | Diamantes y conflictos de claves (R-E) no decidibles; `implements[]` múltiple ya cubre "varios contratos". |
| `lifecycle.uninstall` como objeto con `data` | Unión `string|object` en schema/unmarshal/codegen por una sola clave (§1.11). |
| Poner `capability_bindings` en el manifest | Es dato por organización, lo cambia el operador, no el autor; el plan ya lo define como tabla. |
| Renombrar toda la taxonomía (v1 §4A.6, ~40 addons) | Plan v2 C8/D11: riesgo alto, valor técnico nulo. |
| Mantener `extension_points.events[]` como sitio canónico | Menos estructura que RFC 0003 y habría dos catálogos con versión. |
| `slots@1` plural con alias hacia `ops.host/v1` | Mantener dos nombres por lugar para siempre; #550 no está mergeado, el alias no protege a nadie. |
| `primary_from[]` | Ver §2. |
| Mergear #550 tal cual y arreglar después | Arrastra claves descartadas al corpus y a 4 repos. |
| Poner `ResolveChain` en `installer` | El hub y `manifestcheck` tendrían que importar el installer (con GORM y runtime); por eso va en `manifest/v3`. |

---

## 7. Preguntas abiertas

Marcadas **[T]** técnica (la decide el equipo con este RFC), **[N]** **no técnica: decide el dueño**.

| # | Pregunta | Tipo | Recomendación |
|---|---|---|---|
| N1 | **Quién puede publicar un `contract` y un `provider` de contratos de primera parte** (`commerce.*`, `fiscal.*`, `messaging.*`): ¿solo first-party, o un tercero puede publicar un proveedor certificado? Afecta el modelo de negocio del marketplace y la auto-aprobación del hub. | **N** | Contratos solo first-party (registro cerrado); proveedores de terceros permitidos, con `CONF-*` verde como requisito de listado. |
| N2 | **Precios, entitlements y licencias** al dividir `fiscal_mexico` en 3 addons: ¿se cobra un paquete, cada addon, o `fiscal_core`+`l10n_mx` incluidos y el proveedor aparte? `replaces` conserva el entitlement, pero el esquema comercial debe definirse antes de publicar. | **N** | Un SKU "Fiscal México" que instala los tres; el proveedor facturable por separado cuando exista un segundo PAC. |
| N3 | **Marca y nombres visibles** en el marketplace (`fiscal_core`, `l10n_mx`, `payments_core`, `messaging_core`): ¿se muestran como addons instalables al cliente final o ocultos como dependencias de un preset? | **N** | Ocultos (`metadata.visibility`/categoría "base") salvo en modo avanzado. |
| N4 | **`actions[].target_model` calificado** (añadido al plan, §1): ¿se aprueba ampliar el alcance de R2-b con él? | **N** (alcance) | Sí; sin él, `inject` queda a medias. |
| N5 | **Ventana de deprecación:** hoy "2 releases". Con releases semanales equivale a ~2 semanas; los clientes appliance pueden tardar meses en actualizar. ¿Se define un mínimo en **tiempo** (p. ej. 90 días) además de releases? | **N** (política con clientes) | Mayor de las dos medidas. |
| N6 | **Momento de dividir `fiscal_mexico`** (plan D12) y **política de ruptura del preset privado `sieteleguas`** (D14): este RFC los habilita pero no los decide. | **N** | Los de D12/D14 del plan. |
| N7 | **Telemetría de claves/contratos deprecados** (plan D19) para decidir cuándo retirar los puentes. | **N** | Sí, agregada y por org. |
| T1 | `events` como sitio canónico de eventos (desviación de la redacción del plan, §1.7). | T | Sí. |
| T2 | `lifecycle.uninstall_data` en vez de `lifecycle.uninstall.data` (§1.11). | T | Sí. |
| T3 | `ordering.phase: pre` reservado en el schema (§1.8). | T | Sí; se decide con el spike. |
| T4 | Esquema de `contract:<key>` en `requires` (§1.4) vs. alternativa de campo nuevo `requires_contracts[]`. | T | `contract:` (un solo campo, `enables` sigue igual). |
| T5 | ¿`contracts[].models[]` (roles de lectura) basta para resolver `@dominio.entidad` o debe ser un contrato de lectura aparte (`contract:read` del plan, diferido)? | T | Basta en R2-b; `contract:read` queda para R3. |

---

## 8. Plan de implementación (PRs en orden)

Esfuerzo: S ≤ 1 día, M 2–3 días, L 4–5 días, de un agente. Quién: *kernel*, *sdk*, *hub*, *ops*, *addons*. Los de schema (K-2, K-3) los hace un solo agente de schema para no divergir en las 4 copias; el resto en paralelo. Cada PR se abre contra `main` en una rama propia y se encola (memoria "cola de merges"); el merge exige autorización del turno.

| PR | Repo | Contenido | Dep. | Esf. |
|---|---|---|---|---|
| **K-1** | kernel | `ChainNode`/`ResolveChain` + `ChainResult` + `Lookup` (sin schema, sin uso); `installer.ResolveOrder`/`ResolveInstall`/`Dependents` envuelven `ResolveChain`; reglas `chain_cycle/depth/...`. Toda la suite de H11 pasa sin cambios. | — | M |
| **K-2** | kernel | Schema (2 copias + `schema_parity_test`) y tipos de §1.1–1.8, 1.10–1.12: `role`, `contracts`, `implements`, `extends_addon`, `replaces`, `deprecated`, `events.version`, `ordering`, `sealed`, `uninstall_data`, `relates_to`, `contract:`; portadora legacy + `from_v3.go` + `validate.go`; puentes y avisos de §5; golden del corpus (§2.4). | K-1 | L |
| **K-3** | kernel | `contributions.inject[]` rebasado de #550 con catálogo unificado (`HostSlots()`), `sealed` booleano, `target_model` calificado, resolución de `@rol` por `contracts[].models`. Cierra #550 enlazando. | K-2 | M |
| **K-4** | kernel | `capability_bindings` (modelo, migración SQL desde `org_market_providers`, `capability.Select` v2 que la lee), `dispatch` filtra por `event.version` y respeta `ordering.after`. | K-2 | L |
| **K-5** | kernel | Installer: orden por cadena en migraciones, `Replace` (credenciales, bindings, alias de rutas), `Uninstall` con `uninstall_data` y bloqueo por dependientes; documentación (`docs/`, `EXTENSIBILITY.md` de #547, `slots.md`, `CHANGELOG`). | K-2, K-4 | L |
| **K-6** | kernel | Release minor del tren (tag), con el job de corpus de §2.4 en verde. | K-1…K-5 | S |
| **S-1** | sdk | Bump kernel; codegen de tipos (`Contract`, `Implements`, `ExtendsAddon`, `Inject`…); editor de manifest con `role`/`contracts`; `--frozen-lockfile`. | K-6 | M |
| **H-1** | hub | Bump kernel; publicación ejecuta `ResolveChain` contra el catálogo; indexa `role/contracts/implements`; catálogo filtra por rol; certificación por (addon, versión, cadena); `check-hub-kernel-compat.sh`. | K-6 | L |
| **O-1** | ops | Bump kernel+SDK; borra `pickCapabilityProviderIn`/`dropUnfitImplicit` y usa `Select` v2; lee `capability_bindings`; instalación/desinstalación con política; pantalla `contract.providers`. | K-6, S-1 | L |
| **A-1** | addons | `manifestcheck` bump: `--role-lint`, `ResolveChain` en lugar de `devctl orderByRequires`. | K-6 | S |
| **A-2** | addons | Script `manifest-role-stamp` + `role` explícito y `contract:` en los `requires` a proveedores de los 99 manifests (cambio mecánico, un PR por familia). Meta: inventario R-A/R-C a 0 en lo migrado. | A-1 | M |
| **A-3** | addons | `payments_core`, `messaging_core` y `implements` en los proveedores existentes (sin renombres). | A-2 | M |
| **A-4** | addons | División de `fiscal_mexico` → `fiscal_core` + `l10n_mx` + `fiscal_mx_facturacom` con `replaces`, alias de rutas y migración de org de prueba; carta porte como tipo `transport`. **Timing según plan D12.** | A-2, K-5, O-1 | L (×2) |

Ruta crítica de schema: K-1 → K-2 → K-3/K-4 → K-5 → K-6 (≈ 3 semanas); las cuatro capas aguas abajo en paralelo ≈ 1–2 semanas; A-4 aparte. Coherente con las ≈ 5 semanas del plan.

**Gate del tren (plan, ola R2-b):** `role_lint` en error con 0 violaciones R-A/R-C en lo migrado; `CONF-FISCAL@1` y REG-FAC idénticos antes y después de la división; migración de una org de prueba con credenciales y entitlements del hub verificada; `customers` ≤ 10 grants `db:*` ajenos (hoy 49 `db:*` en total [V]; ese gate es de la ola, no de este schema).

---

## 9. Compatibilidad con los demás RFC del kernel

| RFC | Relación |
|---|---|
| **0003** eventos | Se **extiende, no se contradice**: `events.emits[]` gana `version`/`deprecated`; `consumes[]` gana `version`; RFC 0003 §2.4 (catálogo, panel de integraciones, límites de `payload_schema`) queda intacto. El bloque que RFC 0003 llamó `events` es el sitio canónico. |
| **0001** entidad y alcance | Sin cruce de schema: trabaja con `entity_id` derivado de sucursal y `X-Entity-ID`. Sin conflicto con `writable_models` (es por modelo, no por entidad). |
| **0002** estadísticas de grupo | Añade `scope`, `group_by`, `permission` a **widgets** del tablero (claves de `DashboardWidget`); R2-b no toca esa estructura (el widget `map` de #550 se descartó justamente para no colisionar). Un `inject` de `kind: widget` reutiliza el mismo `DashboardWidget` y heredará esas claves cuando 0002 aterrice. |
| **#633** unidades organizacionales | Es el tren R3 (campos por org). Sus claves (`dimensions[]`, `Model.scope`, `org_unit_types`, aspectos) no están en este RFC. **Dependencia de orden:** su F7 (identidad fiscal como aspecto de `fiscal_mexico`, `legal_identity.resolve@1`) debe implementarse sobre `fiscal_core` — el contrato `legal_identity.resolve@1` se declara en `contracts[]` — y no sobre `fiscal_mexico`; de lo contrario se reescribe dos veces. Hay que reflejarlo en su §G. |
| **#641** estándar genui | Añade `contributions.value_maps/assets/custom_blocks` y fuentes `live`; no solapa con `inject` (usa "inyectable" para activos, otra cosa). El cuerpo `widget` de `inject` apunta al tipo `DashboardWidget`, que #641 evoluciona sin que R2-b lo copie. |
| **`docs/spec/v3/slots.md`** | Se actualiza (K-5): tabla del catálogo ampliada (§1.9), nota de que el identificador es `ops.host/v1` para siempre dentro de v1. |
