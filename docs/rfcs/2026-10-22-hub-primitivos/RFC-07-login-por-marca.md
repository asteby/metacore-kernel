# RFC-07: copy y hero del login configurables por preset/marca

- **Prioridad:** P0 (M1: la instancia del plano de control se presenta al staff).
- **Tamaño:** S-M (kernel contrato S, ops S-M, SDK S).
- **Depende de:** contrato v0.230.0 solo para el camino «preset» (`branding_file`). El camino
  `brand.json` de la instancia es solo de ops y puede ir antes.

## 1. Problema (evidencia)

- El login de OPS tiene el copy **escrito en el componente**: «Plataforma ERP integral», «Todo tu negocio,
  un solo lugar», módulos ERP fijos y cifras «2,400+ empresas activas / 150K+ / 99.9%»
  (ops `frontend/src/features/auth/sign-in/index.tsx:25-40,141-166,212-240`) [V].
- Solo un appliance con módulos sembrados cambia el panel derecho (`useVerticalShowcase`:
  `isAppliance && verticalModules.length > 0`, `:46-51`) [V]: una instancia SaaS/self-host con preset
  privado (el plano de control) no puede.
- La marca no tiene dónde decirlo:
  - Preset: `preset.defaults.branding(_file)` se decodifica con `DisallowUnknownFields` contra
    `v3.Branding` (`manifest/v3/branding.go`, `ParseBranding`) [V]; no hay campo de copy de login.
  - Instancia: `storage/brand/brand.json` rechaza claves desconocidas (ops
    `docs/BRAND-JSON-POR-ARCHIVO.md`, `services/brandfile`) [V].
- El SDK ya tiene los bloques: `@asteby/metacore-auth/showcases/blocks` (`HeroPanel`, `TileGrid`, …) y
  `SignInPage` con ranura derecha (`packages/auth/README.md:23,70-110`) [V]; ops no los usa.
- Logo ancho roto en el spike [S]: el isotipo sí carga. `BrandMark` mide el logo al cargar
  (`naturalWidth/naturalHeight`) para decidir si es «wordmark» (`frontend/src/components/brand-mark.tsx:52-66`).
  Hipótesis a confirmar [?]: (1) SVG sin `width`/`height` (solo `viewBox`) → `naturalHeight` 0 en
  algunos navegadores y la medición no se completa; (2) la ruta del preset (`./assets/hub-logo.svg`) y la
  de `brand.json` (`assets/hub-logo.svg`) no resuelven al mismo asset servido.

## 2. Diseño

Bloque opcional `login` en **ambas** fuentes de marca (mismo shape):

```json
{
  "platform_name": "Asteby Hub",
  "logo_url": "./assets/hub-logo.svg",
  "logo_mark_url": "./assets/hub-mark.svg",
  "login": {
    "eyebrow": "Plano de control",
    "title": "La flota de Asteby,",
    "title_accent": "en un solo lugar.",
    "subtitle": "Instancias, licencias, catálogo y actualizaciones.",
    "highlights": [
      { "icon": "Server", "label": "Flota" },
      { "icon": "KeyRound", "label": "Licencias" },
      { "icon": "Package", "label": "Catálogo" },
      { "icon": "RefreshCw", "label": "Actualizaciones" }
    ],
    "stats": [],
    "signup": false,
    "i18n": { "en": { "eyebrow": "Control plane", "title": "Asteby's fleet,", "title_accent": "in one place." } }
  }
}
```

| Campo | Regla |
|---|---|
| `eyebrow`, `title`, `title_accent`, `subtitle` | Texto plano, ≤ 80/80/80/200 caracteres. Sin HTML. |
| `highlights[]` | ≤ 8; `icon` = nombre Lucide PascalCase (misma regla que `Role.icon`), `label` ≤ 24. Sustituye a los módulos ERP. |
| `stats[]` | ≤ 3 `{label, value}`. **Vacío o ausente = no se muestran cifras.** Nunca más cifras inventadas por omisión: el default de ops pasa a no mostrar `stats` salvo que la marca las declare. |
| `signup` | `false` oculta «¿Nuevo aquí? Crear cuenta». Por omisión el comportamiento actual. |
| `i18n` | Por locale (`es`, `en`, …), mismas claves; cae a la raíz. |

Cascada (la misma de hoy para el resto de la marca): organización → `instance_brand` (`brand.json`) →
preset → valores genéricos de OPS. Para cada campo, vacío cae al siguiente.

## 3. Cambios por repo

| Repo | Cambio |
|---|---|
| kernel (contrato v0.230.0) | `manifest/v3/branding.go`: `Branding.Login *LoginCopy` + validación (longitudes, iconos, ≤ 8/≤ 3). **Sí** cambia el schema: `Preset.defaults.branding` es `additionalProperties: false` [V], así que `login` se añade en ambas copias (`manifest/v3/schema/` y `docs/spec/v3/`). El `branding_file` se valida con el mismo `ParseBranding`. |
| ops backend | `services/brandfile` (aceptar `login`), `services/preset_branding.go` (`presetBranding.Login`), persistencia en `instance_brand` (columna `login jsonb`) y en `org_brandings` (ídem, migración), y exponerlo en `/api/org/branding` y `/api/brand/boot.js` (antes del login). |
| ops frontend | `features/auth/sign-in/index.tsx`: leer `login` del `PlatformConfig`; si existe, panel derecho con `HeroPanel` + `TileGrid` del SDK; quitar los `stats` por omisión. Arreglar el logo ancho (reproducir con el SVG del spike; si es la hipótesis 1, medir con `getBBox`/`viewBox` o usar `decode()` + fallback por proporción declarada `logo_aspect`). |
| SDK `auth` | `showcases/blocks`: `BrandLoginShowcase({login})` que compone `HeroPanel`/`TileGrid`/`StatRow` desde el JSON (sirve a cualquier app, no solo a ops). Changeset minor. |
| addons (preset `hub`) | `defaults/branding.json` con `login`. |

## 4. Compatibilidad

Aditivo. Sin `login`, el SaaS genérico queda igual salvo las cifras, que dejan de mostrarse por omisión
(decisión de producto explícita: un preset/una instancia que las quiera las declara). Un hub viejo
rechazaría un preset con `login` → bump previo (PLAN §2).

## 5. Pruebas

Kernel: `ParseBranding` acepta/rechaza (`login` con HTML, icono inválido, 9 highlights). Ops: brandfile
con `login`, cascada org → instancia → preset; snapshot del login con y sin `login`; logo SVG sin
`width/height` se ve como wordmark. SDK: test del showcase.

## 6. Seguridad

Texto plano renderizado como texto (React escapa); sin URLs en el copy; los assets siguen las reglas de
tamaño/tipo ya vigentes de `brand.json`.
