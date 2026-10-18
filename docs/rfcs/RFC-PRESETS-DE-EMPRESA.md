# RFC — Presets de empresa: públicos SaaS y privados por empresa

- **Fecha:** 2026-10-18
- **Estado:** propuesta. Faltan las decisiones del dueño (§12).
- **Tarea:** 7.1 del plan `PLAN-MIGRACION-7LEGUAS-A-OPS-2026-10-18`.
- **Repos afectados:** metacore-kernel (contrato), hub (distribución), ops (instalación, upgrade y export) y addons (presets).
- **Base verificada:** kernel `v0.212.0` (`f3a2392`), ops `73b7a467`, hub `1d1f1c4` y addons `1e9e28126`, todos en `origin/main` del 2026-10-18.

## 0. Resumen

Hoy un preset es una lista de addons más un mapa `defaults` sin esquema, que se instala una sola vez. Este RFC lo convierte en una **plantilla de empresa** con dos capas:

1. **Preset público (SaaS).** Una vertical genérica, como `pitsline`, que cualquier org instala desde el hub.
2. **Preset privado de empresa.** Declara `extends` sobre un preset público y guarda solo sus diferencias (*overlay*): identidad visual, KioskProfiles, pantalla de cliente, terminología, menú, roles, dashboards, plantillas de documentos, configuración fiscal no secreta y addons privados. Solo lo pueden ver e instalar las orgs autorizadas.

Para que esto funcione hacen falta cuatro piezas nuevas:

- `preset.Flatten` en el kernel: une base y overlay con reglas de merge por tipo de dato, publicadas como código.
- Un **ledger de valores aplicados** en ops, para que un upgrade haga un merge a tres bandas y nunca pise lo que editó el admin de la org.
- El **export "convertir mi org en preset"**, que genera el overlay a partir de una org que ya funciona.
- **Grants explícitos** en el hub para lo privado. El comodín `*` de una licencia nunca alcanza un preset privado.

## 1. Estado actual (verificado en código)

### 1.1 Kernel (`v0.212.0`)

- `manifest/v3/types.go`: `KindPreset` y `Preset{Addons []PresetAddon, Defaults map[string]interface{}}`. Cada `PresetAddon` tiene `{key, version, optional, requires}`.
- Schema `manifest-v3.schema.json#/Preset`: `addons` exige `minItems: 1` y `defaults` no tiene esquema (es un objeto libre).
- `validate.go`: un preset no puede declarar `models[]` ni `lifecycle`.
- `preset/preset.go`:
  - `ResolveFromManifest` ordena con un sort topológico sobre `requires` y deja primero los obligatorios.
  - `InstallPreset` recorre los addons y llama al callback de instalación del host.
  - `Resolved.Defaults` es el mapa tal cual viene del manifest.
- **No existen** herencia (`extends`), visibilidad en el manifest, merge ni lectura de archivos `defaults/` del bundle: `bundle.Bundle` solo guarda `Frontend`, `Backend`, `Locales` y `Templates`.

### 1.2 Addons (`verticals/pitsline` 0.9.1)

- 41 addons: 18 obligatorios y 23 opcionales.
- `defaults` trae inline `ui_theme`, `sidebar_layout`, `onboarding` (con preguntas y `exclude_on_no`/`include_on_yes`) y `appliance`.
- **Hallazgo:** `roles_file`, `categories_file`, `dashboards_file`, `branding_file`, `ui_theme_file`, `sidebar_layout_file` y `onboarding_file` **no los lee nadie**. Ni el kernel, ni ops, ni el hub resuelven claves `*_file`; lo admite el propio `tools/manifestcheck/preset_onboarding_inline_test.go`. Consecuencias:
  - En SaaS, los roles, categorías, branding y dashboards de `verticals/pitsline/defaults/*.json` **nunca se aplican**.
  - Solo los usa el appliance, que los embebe con `go:embed` en `cmd/appliance/defaults.go`.

### 1.3 Ops

- `handlers/kernel_marketplace.go` tiene `InstallPreset`, `UpgradePreset`, `UninstallPreset` e `InstallPresetForOrg` (este último lo usa el onboarding).
  - Descargan el bundle del preset por el handshake del hub, resuelven con `kernelpreset.Resolve` e instalan cada addon por el mismo camino que un addon suelto.
  - Al final llaman a `ApplyPresetDefaults` y a `UpsertPresetInstallation`.
- `services/preset_defaults.go` aplica:
  - `categories` y `roles`: crea los que faltan.
  - `branding`: rellena solo los campos vacíos.
  - `ui_theme` y `sidebar_layout`: aplica la plantilla.
  - `dashboards` queda pendiente, porque no hay modelo para ellos.
- **El upgrade re-aplica los defaults en modo "crea lo que falta".** Eso tiene dos efectos:
  - Nunca pisa una edición, lo cual es correcto.
  - Tampoco propaga una mejora del preset a una fila que ya existe. Si pitsline cambia la etiqueta de un rol o una plantilla de menú, las orgs instaladas no se enteran.
- `models.PresetInstallation` guarda `(org, preset_key)`, la versión y la lista de addons. No guarda linaje ni versión de base.
- Ola 1, aún sin mergear, ya trae dos patrones que este RFC generaliza:
  - **Terminología** (`ops-terminologia`): `org_terminology_overrides.source = admin|template`. Una fila del admin nunca se reemplaza; una fila de plantilla sí se refresca.
  - **Roles** (`ops-preset-role-grants`): los permisos del preset se otorgan como plantilla en `role_template_grants`, de forma aditiva. Un permiso que el admin quitó no vuelve en el siguiente upgrade.
- Ya hay modelos por org que forman la "identidad" de una empresa:
  - `OrgBranding`, `OrgUiTheme`, `OrgSidebarLayout`, `OrgDocumentTemplate`, `OrgCustomStage`, `OrgStageOverride`, `OrgStageAutomation` y `OrgModelExtensionField`.
  - `addon_installations.settings` y `addon_branch_settings`.
  - `KioskProfile`, modelo del addon `kiosk`, que está en curso.

### 1.4 Hub

- `marketplace_addons.visibility public|private` (migración 0015) e `is_preset` (0030).
- **El gate de lo privado vive en el handshake de instalación, y solo cuando `LICENSING_ENFORCE` está apagado.** Así funciona hoy:
  - Con `LICENSING_ENFORCE` apagado, la org necesita un entitlement que liste la clave del preset.
  - Con `LICENSING_ENFORCE` encendido, manda la licencia de instancia, que lista `presets`, `addons` o `"*"`.
  - **Riesgo:** con `LICENSING_ENFORCE` encendido, una licencia `"*"` (la que usan las instancias SaaS multi-tenant) instala cualquier addon privado, incluido el de otra empresa.
- Revendedores: `partners`, con `allowed_products` y cupos (0059), y `partner_id` en licencias, instancias y cuentas (0060).
- Firma: la firma Ed25519 central del hub (`X-Asteby-Marketplace-Signature`) es el ancla de confianza. La firma del developer se guarda para procedencia.
- `publish.go` detecta `kind: Preset` sobre el `RawManifest`. El hub valida el v3 **estricto**, así que cualquier campo nuevo obliga a subir el kernel del hub. Hoy el hub va en `v0.210.0`.

### 1.5 Qué falta

| Necesidad | Estado |
|---|---|
| Heredar un preset público y sobreescribir | ❌ no existe |
| Defaults con esquema y reglas de merge | ❌ mapa libre |
| `*_file` resueltos desde el bundle | ❌ claves sin efecto |
| Upgrade que propaga mejoras sin pisar ediciones | ⚠ solo "crea lo que falta" |
| Visibilidad privada robusta con `LICENSING_ENFORCE` | ⚠ `"*"` la atraviesa |
| Export org → preset | ❌ no existe |
| Dashboards, settings, KioskProfiles y documentos en el preset | ❌ |

## 2. Modelo de capas

```
  addon (seed/defaults propios del addon)       ← kernel: Model.Seed
    └─ preset público  pitsline@0.9.x           ← vertical SaaS
         └─ preset privado sieteleguas@1.x      ← overlay de empresa (extends)
              └─ org (lo que edita el admin)    ← siempre gana, protegida por el ledger
                   └─ runtime (secretos, datos, transacciones) ← nunca entra a un preset
```

Reglas:

1. **Gana la capa más baja.** La org gana al preset privado, y el preset privado gana al público.
2. **Un preset privado solo contiene diferencias.** Copiar pitsline dentro de sieteleguas está prohibido: haría *drift* desde el primer día.
3. **Un preset nunca lleva secretos, datos transaccionales ni datos personales.** Ver §4.
4. **Profundidad máxima de `extends`: 2 niveles.** Un público puede extender un público, por ejemplo `pitsline` → `retail-base`, y un privado extiende uno de esos. Se rechazan los ciclos y una profundidad mayor.

## 3. Contrato en el manifest (kernel)

### 3.1 Campos nuevos

```jsonc
{
  "apiVersion": "asteby.com/v3",
  "kind": "Preset",
  "metadata": {
    "key": "sieteleguas",
    "version": "1.0.0",
    "visibility": "private",           // NUEVO: public (default) | private
    "owner": "sieteleguas"              // NUEVO, opcional: slug de la cuenta dueña en el hub (informativo; la autoridad es el hub)
  },
  "preset": {
    "extends": {                        // NUEVO
      "key": "pitsline",
      "version": ">=0.9.1 <1.0.0"       // rango semver; el host elige la más alta publicada que cumpla
    },
    "addons": [                         // con extends: puede venir vacío (minItems 0)
      { "key": "fiscal_savings", "version": ">=0.1.0 <1.0.0" },   // addon nuevo (privado)
      { "key": "caja", "version": ">=0.4.0 <1.0.0", "optional": false } // parche: vuelve obligatorio un opcional
    ],
    "remove_addons": ["vulcanizado"],   // NUEVO: quita addons de la base
    "defaults": { /* overlay, §3.2 */ }
  }
}
```

- `metadata.visibility` es una **afirmación** del autor. El hub la hace cumplir al publicar (§7): un bundle `private` nunca se publica como público. El allowlist de orgs **no** va en el manifest, porque cambia sin republicar y expondría la lista de clientes.
- Validación nueva:
  - `addons` puede ir vacío cuando hay `extends`.
  - `remove_addons` solo con `extends`.
  - `extends.key` no puede ser la propia clave.
  - `visibility` debe ser `public` o `private`.

### 3.2 Defaults con esquema y estrategia de merge

`preset.defaults` sigue aceptando claves libres por compatibilidad. Las claves **conocidas** pasan a tener forma y estrategia de merge, y el kernel las publica en una sola tabla en código (`preset.MergeStrategies`) que usan ops, el hub y manifestcheck.

| Clave | Contenido | Estrategia de merge (overlay sobre base) | Aplicador en ops |
|---|---|---|---|
| `branding` | nombre comercial, colores, logo/favicon (ruta a `assets/`), mascota | merge-patch de objeto (RFC 7386: `null` borra) | `OrgBranding` |
| `ui_theme` | `theme_key`, fuente, densidad, radio | merge-patch | `OrgUiTheme` |
| `theme_pack` | ThemePack (tokens, variantes); compartido con la tienda | merge-patch | futuro `@asteby/metacore-theme` |
| `sidebar_templates[]` | plantillas de menú (árbol + terminología) | lista por `key` | `OrgSidebarTemplate` (s4) |
| `sidebar_layout` | `template_key` activa | merge-patch | `OrgSidebarLayout` |
| `terminology` | `{lang: {i18n_key: valor}}` | merge-patch por idioma | `org_terminology_overrides` (s4) |
| `roles[]` | `{slug, name, icon, permissions[]}` | lista por `slug`; `permissions` se une, y `"-perm"` la quita | roles + `role_template_grants` (s5) |
| `categories[]` | categorías y subcategorías | lista por `name` | products |
| `dashboards[]` | widgets por rol | lista por `key` | pendiente: modelo de dashboard |
| `document_templates[]` | `{addon, document_key, html_file, paper}` | lista por `(addon, document_key)` | `OrgDocumentTemplate` |
| `stages[]` | etapas propias y overrides por modelo | lista por `(model, key)` | `OrgCustomStage` / `OrgStageOverride` |
| `extension_fields[]` | campos propios por modelo | lista por `(model, field_key)` | `OrgModelExtensionField` |
| `settings` | `{addon: {clave: valor}}` y `{"addon@branch": …}` | merge-patch por addon | `addon_installations.settings` |
| `records` | filas semilla de modelos de addon: `{"kiosk.KioskProfile": {"key": "code", "rows": [...]}}` | por `(modelo, valor de key)` | genérico, igual que `Model.Seed` |
| `onboarding` | contexto, chips y preguntas | **reemplazo** completo si el overlay lo trae | wizard |
| `appliance` | perfil del appliance | merge-patch | `cmd/appliance` |
| `*_file` | ruta a un JSON del bundle | se **inlinea** antes del merge; si ya hay inline, gana el inline | kernel (§3.3) |

Detalles:

- **`records`** es la pieza genérica para todo lo que es "configuración como filas" de un addon: `KioskProfile`, perfiles de pantalla de cliente, plantillas de ticket, métodos de pago o reglas B8. Así ops no necesita un aplicador nuevo por cada tipo, y el kernel valida que el modelo exista en algún addon del preset ya aplanado.
- **`settings`** rechaza al publicar cualquier clave que el manifest del addon declare con `type: "secret"`, y cualquier credencial de connector.
- Una clave desconocida en un **objeto** hace merge-patch. Una clave desconocida en una **lista** se reemplaza entera y genera un aviso en manifestcheck.

### 3.3 API nueva del paquete `preset`

```go
// Lee un bundle de preset y también sus archivos defaults/** y assets/** (Bundle.PresetFiles).
func ResolveBundle(b *bundle.Bundle) (Resolved, error) // inlinea *_file

// Une la base ya resuelta con el overlay. Es pura y determinista.
func Flatten(base Resolved, overlay *v3.Manifest, files map[string][]byte) (Resolved, error)

type Resolved struct {
    // ...campos actuales...
    Lineage []LineageEntry // [{Key:"pitsline",Version:"0.9.3"}, {Key:"sieteleguas",Version:"1.0.0"}]
    Assets  map[string][]byte // assets/** de toda la cadena; el overlay gana por ruta
}
```

Reglas de `Flatten` para addons:

- Se hace la unión por `key`.
- Si el overlay repite una clave, la versión resultante es la **intersección** de rangos. Si queda vacía, error `ErrIncompatibleRange`.
- `optional` lo decide el overlay.
- `requires` se une.
- `remove_addons` se aplica al final. Si quitas un addon del que otro depende (`requires`), error.

`InstallPreset` no cambia: recibe el `Resolved` ya aplanado.

## 4. Qué entra al preset y qué es runtime

Regla general: **entra lo que sirve para clonar la operación en otra org sin llevarse datos de nadie.** El export trabaja con un **allowlist** por tipo, nunca con un denylist.

| Dato | ¿En preset? | Nota |
|---|---|---|
| Addons y versiones (públicos y privados) | ✅ | |
| Branding, logos, favicon, ThemePack y `ui_theme` | ✅ | Los logos van como archivos en `assets/`, con su sha256 |
| KioskProfiles, perfiles de pantalla de cliente y plantillas de ticket | ✅ | vía `records` |
| Terminología, plantillas de menú y layout | ✅ | |
| Roles por puesto y sus permisos | ✅ | Los usuarios **no** entran |
| Dashboards por rol | ✅ | |
| Plantillas de documentos (HTML) | ✅ | |
| Etapas, campos propios y reglas/aprobaciones (B8/B4) | ✅ | |
| Settings no secretos de cada addon | ✅ | Ejemplos: uso CFDI por defecto, política de autofactura, periodicidad de la factura global, piso de precio |
| Categorías y catálogos de configuración (métodos de pago) | ✅ | |
| Preguntas de onboarding | ✅ | |
| **Razón social, RFC, régimen, CSD y credenciales del PAC** | ❌ runtime | Se piden en el onboarding. El perfil de emisión es de la org y de la sucursal (tarea 4.1) |
| Credenciales de connectors (MP, Clip, WhatsApp, SMTP) y tokens | ❌ runtime | Secretos cifrados |
| Sucursales reales (dirección, lugar de expedición, series y folio actual) | ❌ runtime | El preset puede traer una **plantilla** de sucursal: nombres de almacén por defecto y tipos de estación |
| Usuarios, empleados, dispositivos emparejados, estaciones y dominios | ❌ runtime | |
| Clientes, productos, precios, existencias, saldos, documentos y CFDI | ❌ datos | Si se quiere clonar un catálogo de muestra, va como **import-bundle aparte**, nunca dentro del preset |
| Secuencias y folios | ❌ runtime | |

Los datos fiscales siguen la regla "configurable vs. regla de la autoridad":

- El preset puede traer **lo configurable por la empresa**: cuándo se factura, la relación por defecto de la refactura o las series sugeridas.
- **Nunca trae lo que dicta el SAT**, que vive en `fiscal_mexico`, ni la identidad del emisor.

## 5. Versionado y upgrade sin pisar overrides

### 5.1 Versiones

- El preset privado tiene su propio semver.
- `PresetInstallation` suma `base_key`, `base_version` y `lineage` (jsonb). La versión efectiva se muestra como `sieteleguas@1.2.0 (pitsline 0.9.3)`.
- **Hay "actualización disponible" en dos casos:**
  - Sale una versión nueva del privado.
  - Sale una versión nueva del público que **cae en el rango `extends`** del privado instalado. El hub la anuncia en el catálogo.
- Si el público sube de mayor (sale del rango), el dueño del privado republica con el rango nuevo. Mientras tanto, la org se queda en la última base compatible.

### 5.2 Merge a tres bandas con ledger

Tabla nueva en ops: `preset_applied_values`.

```
organization_id | preset_key | target   | target_key                 | applied_hash | applied_from        | applied_at
----------------+------------+----------+----------------------------+--------------+---------------------+-----------
<org>           | sieteleguas| role     | cajero                     | sha256(...)  | pitsline@0.9.3      | ...
<org>           | sieteleguas| record   | kiosk.KioskProfile:llantera| sha256(...)  | sieteleguas@1.0.0   | ...
<org>           | sieteleguas| terminology | es:sales.SalesOrder     | sha256(...)  | sieteleguas@1.0.0   | ...
```

Cuando un aplicador escribe un valor, registra el hash del valor **tal como lo escribió el preset**. En cada upgrade se aplana `base@nueva + privado@vigente` y, para cada elemento, se comparan tres valores:

- **B** = `applied_hash`, lo último que escribió el preset.
- **M** = el hash actual en la org.
- **T** = el valor nuevo del preset aplanado.

| Caso | Acción |
|---|---|
| No hay ledger y la fila no existe | crear y registrar el hash |
| `M == B` (nadie la tocó) | actualizar a T y registrar el hash nuevo |
| `M != B` y `T == B` (la org editó, el preset no cambió) | dejar M |
| `M != B`, `T != B` y `M != T` | **conflicto:** se deja M y se reporta en el resultado del upgrade y en la pantalla "Cambios del preset", donde el admin acepta o descarta |
| Hay ledger y la fila ya no existe (el admin la borró) | no recrearla; tombstone |
| T ya no trae el elemento, hay ledger y `M == B` | borrar solo en tipos de plantilla (terminología de plantilla, plantillas de menú). Los roles y los `records` se quedan, marcados como huérfanos |

Este ledger generaliza los dos patrones de la ola 1:

- `source=admin|template` de la terminología.
- Los grants aditivos que respetan lo que el admin quitó.

Ambos pasan a implementarse sobre el ledger, sin cambiar su comportamiento visible. La protección aplica dos veces:

1. **El privado gana al público** dentro de `Flatten`.
2. **La org gana a ambos** gracias al ledger.

### 5.3 Compatibilidad hacia adelante

Cuando el hub publica una versión nueva de un **público**, corre `Flatten` contra la última versión de cada **privado** que lo extiende y avisa al dueño (partner o Asteby) si falla. Los fallos posibles son:

- rango vacío;
- un addon quitado del que todavía depende el privado;
- `records` de un modelo que ya no existe.

La publicación del público **no** se bloquea.

## 6. Export: "convertir mi org en preset"

**Endpoint:** `POST /api/admin/presets/export` en ops, solo para superadmin o partner con permiso.

```json
{ "key": "sieteleguas", "name": "Siete Leguas", "base": "pitsline", "version": "1.0.0",
  "include": ["branding","theme","terminology","sidebar","roles","dashboards","documents","stages","extension_fields","settings","records"] }
```

Pasos:

1. **Leer** solo las tablas del allowlist (§4) para la org.
2. **Normalizar:**
   - Quitar ids, `organization_id`, timestamps y `updated_by`.
   - Cambiar las referencias internas por claves naturales: el rol por `slug`, el addon por `key`, el modelo por `addon.Model`.
3. **Assets:** copiar logos, favicon e imágenes del kiosko (attract, fondos) a `assets/` con sha256. Las URLs absolutas de `/storage` se reescriben como rutas relativas del bundle.
4. **Diff contra la base:** `Flatten(base@instalada)` contra lo leído. Solo se emite lo que difiere, y eso es el overlay.
5. **Addons:**
   - Si el set instalado tiene addons que la base no trae, entran en `addons[]` con `>=instalada <siguiente mayor`.
   - Si a la org le falta un addon obligatorio de la base, entra en `remove_addons`.
6. **Escaneo de secretos:**
   - Se rechazan claves `type: secret` y patrones (`sk_`, `APP_USR-`, PEM, `.cer`/`.key`) en cualquier valor.
   - Se rechazan RFC y CURP fuera de los campos permitidos.
   - Si algo coincide, el export **falla** y nombra el campo.
7. **Salida:** un `.tar.gz` con la fuente del preset (`manifest.json`, `defaults/*.json` y `assets/`), listo para revisarse en un PR del repo de presets privados (decisión 2). Ops **no** publica directo al hub: la publicación pasa por revisión y firma (§8).

**Clonar una empresa** consiste en exportar la org A, ajustar el overlay (colores, logo, nombre) y publicarlo como preset privado de B. Después se crea la org de B, se instala el preset y se corre el onboarding de runtime (razón social, RFC, CSD, sucursales y usuarios). El catálogo, si se clona, viaja en un import-bundle aparte.

## 7. Visibilidad, allowlist y revendedor (hub)

1. **Al publicar:**
   - Si el manifest dice `visibility: private`, el hub fuerza `marketplace_addons.visibility = 'private'`.
   - Pasarlo a público exige republicar con `public` **y** la acción del admin. Si las dos cosas no coinciden, el publish falla.
2. **Grants explícitos.** Tabla `private_grants(addon_key, subject_type org|license|partner, subject_id, granted_by, expires_at, revoked_at)`. Hoy el grant se infiere de los entitlements. Lo nuevo:
   - Los grants valen **también con `LICENSING_ENFORCE`**.
   - El comodín `"*"` de una licencia **no** cubre lo privado. Solo lo cubre la clave explícita en la licencia o un grant.
3. **Cascada:** un grant sobre un preset privado P concede también los **addons privados del mismo dueño** que P lista (`fiscal_savings`). El hub lo resuelve en el handshake de cada addon.
4. **Dueño y revendedor:**
   - `marketplace_addons.owner_account_id` (la empresa) y `partner_id` (opcional).
   - Un partner puede publicar presets privados para sus clientes. Solo puede darles grant a orgs o licencias **de su cartera** (scoping por `partner_id`, mismo patrón que la Partner API) y dentro de sus `allowed_products`.
   - Asteby (admin del hub) puede dar grant a cualquiera.
5. **Catálogo:** un privado solo aparece para orgs con grant. En el onboarding ("elige tu giro"), una org con grant ve primero su preset privado.
6. **Claves:** las claves privadas comparten espacio con las públicas. El hub rechaza claves reservadas y colisiones. `notAPresetMessage` de ops ya documenta el daño de una colisión.

## 8. Firma, publicación y onboarding

- **Fuente:** los presets privados **no** viven en `addons/verticals/`, porque el repo lo leen todos los agentes y partners. Van en un repo privado, por ejemplo `asteby-hq/presets-empresa/<key>/` (decisión 2), con la misma CI de manifestcheck más el escaneo de secretos.
- **Firma:** es la misma cadena que la de los addons. La firma del developer o partner da procedencia, y la firma central Ed25519 del hub es el ancla. El bundle del privado **no** incluye la base: la referencia por rango y la base se descarga y verifica por separado.
- **Validación al publicar:** schema estricto, `extends` resoluble contra lo publicado, `Flatten` sin error, settings sin secretos, `records` contra modelos existentes y tamaño máximo de `assets/` (5 MB).
- **Instalación (ops):**
  1. `InstallPreset` o `InstallPresetForOrg(sieteleguas)` pide el bundle del privado; el handshake comprueba el grant.
  2. Lee `extends`, pide el bundle de `pitsline` en la versión más alta del rango y ejecuta `ResolveBundle` sobre los dos y luego `Flatten`.
  3. `InstallPreset` instala los addons aplanados.
  4. Los aplicadores escriben los defaults y registran el ledger.
  5. Se guarda una sola fila en `PresetInstallation`, la del privado, con su linaje. Esa fila es la que usan el sidebar, Instalados y el upgrade.
- **Onboarding:** con preset privado, el wizard no hace las preguntas del giro, porque el overlay ya decidió los addons. Solo pide el **runtime**: razón social, RFC, CSD, PAC, sucursales, usuarios por rol, impresoras y terminales. Las preguntas que el overlay deja (`onboarding`) siguen funcionando.

## 9. Ejemplo completo: `sieteleguas` sobre `pitsline`

```
presets-empresa/sieteleguas/
├── manifest.json
├── assets/
│   ├── logo.svg               # logo Siete Leguas
│   ├── favicon.png
│   └── kiosk/attract-1.webp   # pantalla de espera del kiosko
├── defaults/
│   ├── terminology.es.json
│   ├── sidebar_7leguas.json   # menú clásico de 7leguas (tarea 5.1)
│   ├── roles.json             # 7 puestos (tarea 5.2)
│   ├── dashboards.json
│   ├── kiosk_profiles.json    # flujo llantera con el tema Siete Leguas (2.6)
│   ├── display_profiles.json  # pantalla de cliente (2.7)
│   └── templates/
│       ├── ticket-80mm.html
│       └── cotizacion.html
└── locales/es-MX.json
```

```jsonc
{
  "apiVersion": "asteby.com/v3",
  "kind": "Preset",
  "metadata": {
    "key": "sieteleguas",
    "name": "Siete Leguas",
    "version": "1.0.0",
    "visibility": "private",
    "owner": "sieteleguas",
    "author": "Asteby",
    "icon": { "type": "svg", "slug": "assets/logo.svg", "color": "#C8102E" }
  },
  "compatibility": { "requires": [{ "key": "kernel", "version": ">=3.0.0 <4.0.0" }] },
  "preset": {
    "extends": { "key": "pitsline", "version": ">=0.9.1 <1.0.0" },
    "addons": [
      { "key": "caja",               "version": ">=0.4.0 <1.0.0", "optional": false },
      { "key": "kiosk",              "version": ">=0.3.0 <1.0.0", "optional": false },
      { "key": "customer_display",   "version": ">=0.3.0 <1.0.0", "optional": false },
      { "key": "workstations",       "version": ">=0.2.0 <1.0.0", "optional": false },
      { "key": "cash_recycler",      "version": ">=0.2.0 <1.0.0", "optional": false },
      { "key": "payment_terminals",  "version": ">=0.2.0 <1.0.0", "optional": false },
      { "key": "mercadopago",        "version": ">=0.9.0 <1.0.0", "optional": false },
      { "key": "receivables",        "version": ">=0.8.0 <1.0.0", "optional": false },
      { "key": "collections",        "version": ">=0.6.0 <1.0.0", "optional": false },
      { "key": "credit_approval",    "version": ">=0.3.0 <1.0.0", "optional": false },
      { "key": "fiscal_savings",     "version": ">=0.1.0 <1.0.0", "optional": true,
        "requires": ["fiscal_mexico"] }
    ],
    "remove_addons": [],
    "defaults": {
      "branding": {
        "platform_name": "Siete Leguas",
        "logo_url": "assets/logo.svg",
        "favicon_url": "assets/favicon.png",
        "primary_color": "#C8102E",
        "accent_color": "#1B1B1B"
      },
      "ui_theme": { "theme_key": "classic", "font": "manrope", "density": "comfortable" },
      "terminology_file": "./defaults/terminology.es.json",
      "sidebar_templates_file": "./defaults/sidebar_7leguas.json",
      "sidebar_layout": { "template_key": "siete_leguas" },
      "roles_file": "./defaults/roles.json",
      "dashboards_file": "./defaults/dashboards.json",
      "document_templates": [
        { "addon": "pos",    "document_key": "ticket",    "html_file": "./defaults/templates/ticket-80mm.html", "paper": "80mm" },
        { "addon": "quotes", "document_key": "quote_pdf", "html_file": "./defaults/templates/cotizacion.html",   "paper": "letter" }
      ],
      "settings": {
        "pos":           { "require_customer_on_credit": true, "price_floor_requires_approval": true },
        "fiscal_mexico": { "invoice_trigger": "autofactura_or_flag", "credit_note_on_invoiced_return": "always",
                           "global_invoice_periodicity": "daily" },
        "kiosk":         { "default_profile": "llantera" }
      },
      "records": {
        "kiosk.KioskProfile":                     { "key": "code", "rows_file": "./defaults/kiosk_profiles.json" },
        "customer_display.CustomerDisplayProfile": { "key": "code", "rows_file": "./defaults/display_profiles.json" }
      }
    }
  }
}
```

`terminology.es.json` (fragmento):

```json
{ "es": { "sales.SalesOrder": "Orden", "workshop.Bay": "Rampa",
          "warehouse.menu.pending": "En espera", "inventory.menu.low_stock": "Stock Bajo" } }
```

`roles.json` (fragmento):

```json
[
  { "slug": "vendedor", "name": "Vendedor", "icon": "BadgeDollarSign",
    "permissions": ["sales.salesorder.create", "quotes.quote.create", "customers.customer.index"] },
  { "slug": "cajero", "name": "Cajero", "icon": "Wallet",
    "permissions": ["pos.session.open", "caja.cashcut.create", "-sales.salesorder.delete"] }
]
```

Resultado de `Flatten(pitsline@0.9.3, sieteleguas@1.0.0)`:

- Hay 42 addons (los 41 de pitsline más `fiscal_savings`). `caja`, `kiosk`, `customer_display`, `workstations`, `cash_recycler`, `payment_terminals`, `mercadopago`, `receivables`, `collections` y `credit_approval` pasan a obligatorios.
- `onboarding` se hereda de pitsline. Con preset privado, el wizard salta las preguntas de addons que ya son obligatorios.
- `defaults.categories` también se hereda de pitsline.

**Ahorros (`fiscal_savings`)** es un addon privado del mismo dueño. Entra como opcional porque está pendiente la decisión 3 del plan. Llega gracias al grant en cascada (§7.3).

## 10. Cambios por repo y cadena de versiones

Se sigue la regla de HANDOFF-FACTURACION §1.3: un campo v3 nuevo obliga a subir **todos** los validadores (kernel, manifestcheck y hub) antes de publicar un manifest que lo use.

| # | Repo | Cambio | Versión |
|---|---|---|---|
| 1 | **metacore-kernel** | `Metadata.Visibility` y `Owner`; `Preset.Extends` y `RemoveAddons`; schema: `addons` con `minItems 0` si hay `extends`; validaciones de §3.1; `Bundle.PresetFiles` (`defaults/**` y `assets/**`); `preset.ResolveBundle` (inlinea `*_file` y `rows_file`/`html_file`); `preset.Flatten`; `preset.MergeStrategies`; `Resolved.Lineage` y `Assets`. Tests de merge, de rangos, de ciclos y de profundidad | **v0.213.0** |
| 2 | **addons** `tools/manifestcheck` | Subir a v0.213; reglas de `*_file` (el archivo debe existir dentro del preset); escaneo de secretos en `settings` y `records` | con 1 |
| 3 | **hub** | Subir de **v0.210 a v0.213**; publish acepta los campos nuevos y valida `Flatten`; forzar `visibility` desde el manifest; tabla `private_grants` y migración; `"*"` no cubre lo privado (también con `LICENSING_ENFORCE`); grant en cascada a los addons privados del dueño; `owner_account_id`; grants scoped por partner; aviso de "base nueva compatible" en el catálogo; re-flatten de los privados al publicar un público | después de 1 |
| 4 | **ops** | Subir a v0.213; `InstallPreset*` y `UpgradePreset` resuelven `extends` (descargan la base) y aplanan; migración `preset_applied_values` más `PresetInstallation.base_key`, `base_version` y `lineage`; aplicadores nuevos (`terminology`, `sidebar_templates` sobre s4, `document_templates`, `stages`, `extension_fields`, `settings`, `records` y `dashboards` cuando haya modelo); merge a tres bandas; pantalla "Cambios del preset"; `POST /api/admin/presets/export` | después de 1 |
| 5 | **addons** `verticals/pitsline` | Subir a 0.10.0: los `*_file` actuales **empiezan a aplicarse** en SaaS. Hay que revisar roles, categorías y branding antes, porque las orgs existentes los recibirán en el próximo upgrade (con el ledger, sin pisar) | después de 3 y 4 |
| 6 | **repo privado de presets** | `sieteleguas` 1.0.0 (§9) y CI | después de 5 |
| 7 | **addons** `fiscal_savings` (privado) | tarea 4.9, si el dueño la confirma | independiente |

Dependencias con la ola 1:

- s4 (terminología y plantillas de menú) y s5 (roles con permisos) **son aplicadores de este RFC**. Conviene que se mergeen primero y que el ledger los envuelva después, sin cambiar su comportamiento.
- s2 (`KioskProfile`) y s3 (perfil de pantalla de cliente) deben declarar una **clave natural** (`code`) para que `records` pueda hacer upsert.
- o2 (multi razón social) define el perfil de emisión, que es **runtime** y no entra al preset.

## 11. Alternativas descartadas

- **Aplanar al publicar**, es decir, que el hub guarde el privado ya unido con la base. Se descarta porque una mejora de pitsline no llegaría a sieteleguas sin republicar, y se pierde el linaje para el merge a tres bandas.
- **Copiar pitsline dentro del privado.** Se descarta por el *drift* inmediato: es justo lo que este RFC evita.
- **Guardar la identidad solo como configuración de la org, sin preset.** Se descarta porque no se puede clonar, versionar ni vender.
- **Allowlist dentro del manifest firmado.** Se descarta porque cambia sin republicar y expone la lista de clientes.

## 12. Decisiones del dueño

1. **Revendedores:** ¿un partner puede publicar presets privados para sus clientes (§7.4), o solo Asteby los publica?
2. **¿Dónde viven los presets privados?** Repo privado `asteby-hq/presets-empresa` (recomendado) o carpeta dentro de `addons`.
3. **Pitsline 0.10.0** activa los `*_file` que hoy no tienen efecto (roles, categorías, branding y dashboards). ¿Se aplican a las orgs pitsline existentes en su próximo upgrade, o solo a instalaciones nuevas?
4. **Límite de herencia:** ¿2 niveles alcanzan, o quieres una tercera capa "preset de sucursal"? La recomendación es no: lo de sucursal es runtime o setting `@branch`.
5. **Ahorros en el preset de 7leguas:** opcional (hoy) u obligatorio. Es la decisión 3 del plan.
6. **Conflictos en el upgrade:** ¿el admin de la org decide (recomendado), o el dueño del preset privado puede forzar un valor (`"lock": true`)?
