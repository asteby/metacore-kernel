# Auditoría — eventos canónicos en `dynamic.Service`

- **Fecha:** 2026-05-04
- **Archivos auditados:**
  - [`dynamic/service.go`](../../dynamic/service.go) — engine CRUD transport-agnostic.
  - [`dynamic/hooks.go`](../../dynamic/hooks.go) — `HookRegistry` (puntos de extensión actuales post-mutación).
  - [`events/events.go`](../../events/events.go) — `Bus` in-process con `Publish/Subscribe` y capability checks (`event:emit`, `event:subscribe`).
  - [`host/app.go`](../../host/app.go) — wiring de `dynamic.New(...)` (no inyecta `*events.Bus`).
- **Visión de referencia:** todo CRUD genérico ejecutado por el kernel emite un evento canónico con nombre `<addonKey>.<model>.created|updated|deleted` post-commit, para que el bus interno lo propague a otros add-ons (suscritos por capability) sin cada autor tener que enchufar el `Bus.Publish` a mano dentro de un `AfterCreate` hook.
- **Alcance:** sólo inventario + propuesta de inserción. No se modifica código en esta tarea.

## TL;DR

> **`dynamic.Service` no emite ningún evento canónico hoy.** Las únicas señales post-mutación son los hooks `runAfterCreate / runAfterUpdate / runAfterDelete` que cualquier app puede registrar contra un modelo concreto vía `HookRegistry`. El `events.Bus` existe (`events/events.go:50`) y expone `Publish(ctx, addonKey, event, orgID, payload)`, pero **no está cableado** al engine dynamic ni en `Config` ni en el constructor `host/app.go:240`.

Para llegar al contrato `<addonKey>.<model>.<action>` faltan tres piezas:

1. Resolver el `addonKey` que es dueño del `model` (hoy `dynamic.Service` no lo conoce).
2. Inyectar `*events.Bus` en `dynamic.Config`.
3. Llamar `bus.Publish` después del commit GORM, antes/después/junto al `runAfter*Hook`.

## Estado actual — qué hace `dynamic.Service` post-commit

GORM `Create / Save / Delete` se ejecutan en autocommit (no hay `db.Transaction(...)` envolvente); por construcción los hooks `runAfter*` corren *después* del commit aunque el código no lo nombre así. Las llamadas pertinentes:

| Operación | Punto donde corre el side-effect post-DB           | Archivo:Línea                    |
|-----------|----------------------------------------------------|----------------------------------|
| Create    | `s.hooks.runAfterCreate(ctx, hc, instance)`        | `dynamic/service.go:224`         |
| Update    | `s.hooks.runAfterUpdate(ctx, hc, instance)`        | `dynamic/service.go:261`         |
| Delete    | `s.hooks.runAfterDelete(ctx, hc, id.String())`     | `dynamic/service.go:287`         |

Antes de cada una, el `Create/Save/Delete` GORM:

| Operación | Línea de la mutación DB                                                  |
|-----------|--------------------------------------------------------------------------|
| Create    | `s.db.WithContext(ctx).Create(instance)` — `dynamic/service.go:220`     |
| Update    | `s.db.WithContext(ctx).Save(instance)` — `dynamic/service.go:257`       |
| Delete    | `db.Delete(instance, "id = ?", id)` — `dynamic/service.go:283`           |

Notas relevantes:

- Los `runAfter*` swallowean errores (`_ = s.hooks.runAfter...`): un evento que falle al publicar no debe revertir la mutación, pero **sí** debería loguearse vía `kernel/log`. El `Bus.Publish` ya hace esto internamente (`events/events.go:128`).
- No hay batching/coalescing — cada `Create/Update/Delete` es individual, así que la relación es 1 mutación → 1 evento.
- Hay paths `BulkExport / Import` en `dynamic/handler_export.go` que NO pasan por `Service.Create/Update/Delete`. Quedan **fuera** de este contrato; documentarlo explícitamente para que un add-on que sólo escuche `<addonKey>.<model>.created` sepa que no recibe filas importadas en bulk salvo que el handler también las publique.

## Estado actual — qué ofrece `events.Bus`

`events/events.go:106-133`, firma:

```go
func (b *Bus) Publish(ctx context.Context, addonKey, event string, orgID uuid.UUID, payload any) error
```

- Capability check `event:emit` por addonKey (líneas 110-112). El `addonKey == "kernel"` es trusted (línea 166-168), o sea: si el kernel publica directamente desde `dynamic.Service` con `addonKey="kernel"`, **se salta** el enforcement; si publica con el `<addonKey>` del owner del modelo, **debe** estar registrado en el enforcer con `event:emit:<addonKey>.<model>.*`. Decisión a tomar abajo.
- Subscribers se ejecutan **sincrónicamente** en el goroutine que llamó `Publish` (líneas 126-131). Hot-path de cualquier `POST /api/dynamic/:model` quedará en el camino crítico de los handlers; documentar y aconsejar a los autores que `go func() { ... }()` desde el handler si su trabajo es pesado.
- No hay persistencia (es in-process). Si un add-on cae entre el commit GORM y la entrega, el evento se pierde. Si se quiere durabilidad hay que correr la Publish también contra `eventlog.Service.Emit` (`eventlog/service.go:121`) — fuera de scope para esta tarea.

## Convención canónica propuesta

| Componente   | Valor                                                                                       |
|--------------|---------------------------------------------------------------------------------------------|
| `addonKey`   | el dueño del modelo (`tickets`, `inventory`, …). `"kernel"` para modelos core (`User`, `Organization`). |
| `model`      | el `model` recibido por `Service.Create/Update/Delete` — case sensitive, idéntico al que vio el handler HTTP. |
| `action`     | uno de `created`, `updated`, `deleted`.                                                     |
| `event` final | `fmt.Sprintf("%s.%s.%s", addonKey, model, action)` — ej. `tickets.Ticket.created`.         |
| `payload`    | `map[string]any{"id": ..., "record": <map del Service>, "actor": <user.ID>, "model": model}` para `created/updated`; `{"id": ..., "actor": ..., "model": model}` para `deleted` (no hay record post-delete sin sql logical decoding). |
| `orgID`      | `user.GetOrganizationID()` — ya disponible en `HookContext.User` (`dynamic/hooks.go:15`).  |

## Dónde habría que insertar `Publish`

### 1. Wiring (no es código de `service.go`, pero condiciona los puntos 2-4)

**`dynamic/service.go:18-53` — `Config`**

Agregar dos campos:

```go
// Bus is the in-process event bus where the service emits canonical
// "<addonKey>.<model>.<action>" events post-commit. nil disables emission.
Bus *events.Bus

// AddonKeyForModel resolves the addon owner of a model name. If nil or
// it returns "", the service falls back to "kernel". Apps with an addon
// registry plug their lookup here; apps without addons can leave it nil.
AddonKeyForModel func(ctx context.Context, model string) string
```

**`dynamic/service.go:73-83` — `Service`**

Reflejar los dos campos en el struct, copiar en `New` (`service.go:99-110`).

**`host/app.go:240-244` — wiring del host**

Pasar `Bus` y, cuando exista un addon-registry, `AddonKeyForModel`. Para apps que aún no tienen addon-registry, nil → "kernel".

### 2. `Create` — `dynamic/service.go:220-225`

Insertar **entre** `db.Create` y `runAfterCreate`. La razón de ese orden:

- post-commit garantizado (la línea 220 ya commiteó por autocommit GORM).
- los `AfterCreateHook` que existen hoy podrían querer reaccionar al evento — si publicamos *antes* del hook, el hook lee un mundo donde el evento ya está en vuelo; si publicamos *después*, el hook puede vetar el evento. La política recomendada es publicar **antes** del hook para que el bus sea siempre la verdad y los hooks se mantengan como atajo legacy.

```go
// nuevo, justo después de la línea 222:
s.publishCanonical(ctx, model, "created", hc.User, toMap(instance))
_ = s.hooks.runAfterCreate(ctx, hc, instance)
```

`publishCanonical` es un helper privado nuevo en `service.go` (ver §5).

### 3. `Update` — `dynamic/service.go:257-262`

Insertar entre `db.Save` y `runAfterUpdate`:

```go
// nuevo, justo después de la línea 259:
s.publishCanonical(ctx, model, "updated", hc.User, toMap(instance))
_ = s.hooks.runAfterUpdate(ctx, hc, instance)
```

### 4. `Delete` — `dynamic/service.go:283-288`

Insertar entre `db.Delete` y `runAfterDelete`. El payload no lleva `record` (soft-delete; lo que queda en DB tiene `deleted_at` setteado pero re-leerlo en este handler agrega un round-trip de DB). Si más adelante se quiere snapshot, hacerlo en un PR aparte.

```go
// nuevo, justo después de la línea 285:
s.publishCanonical(ctx, model, "deleted", hc.User, map[string]any{"id": id.String()})
_ = s.hooks.runAfterDelete(ctx, hc, id.String())
```

### 5. Helper privado nuevo — `dynamic/service.go` (al final, antes de `mapToStruct`)

```go
func (s *Service) publishCanonical(ctx context.Context, model, action string, user modelbase.AuthUser, record map[string]any) {
    if s.bus == nil {
        return
    }
    addonKey := "kernel"
    if s.addonKeyForModel != nil {
        if k := s.addonKeyForModel(ctx, model); k != "" {
            addonKey = k
        }
    }
    payload := map[string]any{
        "model":  model,
        "actor":  user.GetID(),
        "record": record,
    }
    if action == "deleted" {
        delete(payload, "record")
        payload["id"] = record["id"]
    }
    event := fmt.Sprintf("%s.%s.%s", addonKey, model, action)
    _ = s.bus.Publish(ctx, addonKey, event, user.GetOrganizationID(), payload)
}
```

Errores: `Publish` puede fallar por capability check (`event:emit` denegado para el addonKey). El service traga el error porque la mutación ya commiteó — política consistente con `runAfter*Hook`. El `Bus` mismo loguea la falla (`events/events.go:128`), así que no hay ceguera operacional.

## Decisiones que el operador tiene que firmar

1. **Política de capability con `addonKey="kernel"` para modelos core.** El kernel publica trusted (skip enforcement, `events.go:166-168`). Si en el futuro queremos que add-ons sólo reciban `kernel.User.created` con capability explícita, hay que cambiar `Bus.check` para que `addonKey="kernel"` también pase por el enforcer.
2. **¿Envolver Create/Update/Delete en `db.Transaction`?** Hoy no lo están. Si en el futuro un hook `BeforeCreate` necesitara mutaciones derivadas atómicas, conviene envolver y mover el `Publish` al callback de éxito de la transacción. Para esta v1 mantener el comportamiento autocommit y publicar inmediatamente después de la mutación es más simple.
3. **Forma del payload**. Esta auditoría propone `{model, actor, record}` (o `{model, actor, id}` para delete). Si el equipo prefiere un envelope `{success, data, meta}` consistente con los handlers (ver `feedback_kernel_handler_response_shape` en memoria), la firma de `payload any` lo soporta sin cambios al `Bus`.
4. **Eventos de import/export**. `dynamic/handler_export.go` no pasa por `Service.Create`. O se envuelve esa ruta también o se documenta la asimetría en `docs/dynamic-system.md`.
5. **Naming**. `<addonKey>.<model>.<action>` ya es lo que usa el ecosistema (ver `webhooks/doc.go:19` con `order.created`, `events/events_test.go` con `ticket.created`). Pero esos ejemplos **omiten** el prefijo `<addonKey>` — son `<model>.<action>`. Decidir si:
   - (a) el contrato canónico nuevo es `<addonKey>.<model>.<action>` (3 niveles) y los webhooks legacy se migran, o
   - (b) seguimos `<model>.<action>` (2 niveles) y agregamos el `addonKey` sólo en el header de telemetría.
   La task del watchdog pide explícitamente la forma de 3 niveles, así que esta auditoría propone (a) y deja la migración de webhooks legacy como follow-up.

## Resumen de líneas tocables

| Inserción                   | Archivo                  | Línea anchor |
|-----------------------------|--------------------------|--------------|
| `Bus`/`AddonKeyForModel` en `Config` | `dynamic/service.go` | 19-53        |
| Mismo en `Service` struct + `New` | `dynamic/service.go` | 73-110       |
| Publish en `Create`         | `dynamic/service.go`    | 222 (después) |
| Publish en `Update`         | `dynamic/service.go`    | 259 (después) |
| Publish en `Delete`         | `dynamic/service.go`    | 285 (después) |
| Helper `publishCanonical`   | `dynamic/service.go`    | después de 289 |
| Wiring en host              | `host/app.go`           | 240-244      |

Una sola PR — sin breaking changes para apps que no inyecten `Bus`/`AddonKeyForModel`: ambos campos son opcionales y default a `nil` → no-op.
