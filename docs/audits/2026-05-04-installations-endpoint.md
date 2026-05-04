# Auditoría endpoint `GET /api/installations` — manifests + frontend.entry para AddonLoader dinámico

- **Fecha:** 2026-05-04
- **Pregunta:** ¿Existe un endpoint `GET /api/installations` que devuelva manifests + `frontend.entry` para el host (necesario para AddonLoader dinámico)?
- **Respuesta corta:** **NO existe ese path literal, pero SÍ existe la capacidad** — la sirve `GET /api/metacore/manifests`. No hace falta crear un endpoint nuevo; alcanza con que el SDK apunte el AddonLoader a esa ruta.
- **Archivos auditados:**
  - [`httpx/metacore/handler.go`](../../httpx/metacore/handler.go) (`ListManifests`, `Install`, `ServeAddonFrontend`)
  - [`httpx/metacore/catalog.go`](../../httpx/metacore/catalog.go) (`Catalog`)
  - [`host/host.go`](../../host/host.go) (`InstalledManifests`)
  - [`host/app.go`](../../host/app.go) (montado del subrouter `/api/metacore`)
  - [`installer/installer.go`](../../installer/installer.go) (`type Installation`, tabla `metacore_installations`)
  - [`marketplace/handler.go`](../../marketplace/handler.go) (`GET /marketplace/installs` — endpoint distinto, ver más abajo)
  - [`manifest/manifest.go`](../../manifest/manifest.go) (`type FrontendSpec`)

## Hallazgos

### 1. Endpoint que cubre el caso de uso: `GET /api/metacore/manifests`

`httpx/metacore/handler.go:113` declara la ruta `GET /api/metacore/manifests` y la implementa en `Handler.ListManifests` (`httpx/metacore/handler.go:120-137`):

```go
func (h *Handler) ListManifests(c fiber.Ctx) error {
    c.Set("X-Metacore-Kernel-Version", h.deps.Bridge.KernelVersion())
    orgID, ok := orgIDFromCtx(c)
    if !ok {
        return c.JSON([]manifest.Manifest{})
    }
    manifests, err := h.deps.Bridge.Host().InstalledManifests(orgID)
    ...
    return c.JSON(manifests)
}
```

Detrás está `host.Host.InstalledManifests` (`host/host.go:100-116`), que filtra por `organization_id = ? AND status = 'enabled'` sobre la tabla `metacore_installations` y devuelve `[]manifest.Manifest`.

Cada `manifest.Manifest` incluye el campo `Frontend *FrontendSpec` (`manifest/manifest.go:60-61`) cuyo struct (`manifest/manifest.go:111-127`) es exactamente el contrato que el AddonLoader necesita:

```go
type FrontendSpec struct {
    Entry     string `json:"entry"`              // URL del remoteEntry.js / bundle
    Format    string `json:"format"`             // "federation" | "script"
    Expose    string `json:"expose,omitempty"`   // módulo federado a importar
    Integrity string `json:"integrity,omitempty"`// SRI hash
    Container string `json:"container,omitempty"`// nombre global del container
}
```

Es decir: **el AddonLoader dinámico ya tiene todo lo que pide la tarea** (manifests + `frontend.entry`) llamando a `GET /api/metacore/manifests`. La ruta es idempotente, scope-org via JWT, devuelve `[]` si no hay org context (boot anónimo del SDK), y emite `X-Metacore-Kernel-Version` para que el frontend pueda invalidar cache cuando el kernel rota.

### 2. Endpoints relacionados que **no** sirven para este caso

- `POST /api/metacore/installations/:key` (`httpx/metacore/handler.go:182`, `Install`) — **mutación**, no lectura. Crea/instala. No aplica.
- `GET /api/metacore/addons/:key/frontend/*path` (`httpx/metacore/handler.go:322`, `ServeAddonFrontend`) — sirve los assets físicos (`remoteEntry.js`, chunks, css). El `Manifest.Frontend.Entry` apunta acá cuando el bundle vive on-disk en el host. Es la "siguiente capa" del loader, no la lista.
- `GET /api/metacore/catalog` (`httpx/metacore/catalog.go`) — lista de bundles disponibles (en `CatalogDir`) anotados con su estado de instalación. Devuelve metadatos del bundle para una vista de marketplace, **no** los manifests completos con `Frontend`. Sólo expone las claves instaladas vía un join contra `metacore_installations`. No aplica para AddonLoader.
- `GET /marketplace/installs` (`marketplace/handler.go:111`) — endpoint de la "lite mode" del marketplace. Devuelve filas de `marketplace_installations` (intent log: `addonKey`, `version`, `bundleURL`, `status="requested"`). Es un registro de "el usuario clickeó Instalar", **no** una lista de manifests con frontend. Tabla distinta a `metacore_installations` (la del installer real). No aplica.

### 3. Naming: ¿por qué la tarea preguntó `/api/installations`?

El kernel tiene dos tablas que comparten parte del nombre y conviene desambiguar para futuras tareas:

| Tabla                       | Backed by                            | Endpoint de lectura                  | Qué representa                                              |
| --------------------------- | ------------------------------------ | ------------------------------------ | ----------------------------------------------------------- |
| `metacore_installations`    | `installer.Installation`             | `GET /api/metacore/manifests` (proyectada)   | Instalaciones reales (lifecycle, secret, status="enabled") |
| `marketplace_installations` | `marketplace.Installation`           | `GET /marketplace/installs`          | Intents de instalación capturados por el botón del Hub      |

El path "natural" `/api/installations` no se mapeó nunca: el kernel decidió exponer la lectura como `/api/metacore/manifests` porque el frontend necesita el manifest entero (modules, slots, navigation, frontend spec, capabilities), no solo la fila de install. La fila del installer (org_id, secret, status, timestamps) no se devuelve en este endpoint y tampoco hace falta para el AddonLoader.

## Recomendación

**No agregar un endpoint nuevo.** El AddonLoader dinámico debe consumir `GET /api/metacore/manifests` y leer `manifest.Frontend.Entry` / `Container` / `Expose` / `Integrity` / `Format` de cada item. Acciones concretas:

1. **SDK frontend:** asegurar que `@asteby/metacore-app-providers` (o el equivalente que orqueste el bootstrap) llame a `/api/metacore/manifests` al boot, deduplique con la `X-Metacore-Kernel-Version` para cache busting, y entregue el array al AddonLoader. Probable que ya esté hecho — verificar en `metacore-sdk/packages/runtime-react` antes de duplicar.
2. **Documentar el rename implícito** en `docs/embedding-quickstart.md` y en el README del SDK: cualquier referencia a "installations endpoint" → `GET /api/metacore/manifests`.
3. **(Opcional, follow-up separado)** Si en el futuro se quiere exponer también el estado de install (`installed_at`, `disabled_at`, `version`) al frontend para una vista admin, agregar `GET /api/metacore/installations` que devuelva `[]{installation, manifest}`. Hoy no es necesario para AddonLoader.
4. **Envelope:** `ListManifests` devuelve `[]manifest.Manifest` directo, no el envelope `{success, data, meta}` que adopta el resto del kernel desde v0.8. Es deliberado (es una colección plana sin metadata) pero conviene revisarlo en una pasada general de consistencia, no en esta tarea.

## Conclusión

**SÍ existe** la capacidad pedida, en `GET /api/metacore/manifests` — `httpx/metacore/handler.go:120` (handler) + `host/host.go:100` (data source). El path literal `/api/installations` no existe y no hace falta crearlo: el endpoint actual ya devuelve manifests con `Frontend.Entry` para el AddonLoader dinámico.
