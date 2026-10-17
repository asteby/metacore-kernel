# Capabilities enforcement

Every privileged operation an addon attempts (DB read/write, HTTP fetch, event
emit/subscribe) is checked by `security.Enforcer` against the capabilities the
addon declared in its manifest.

## Default: ENFORCE (BREAKING since the D5 hardening)

`security.NewEnforcer` starts in `ModeEnforce`. A call not covered by a
declared capability is logged (`metacore.capability.violation`) **and**
rejected; callers map the error to HTTP 403. An unknown addon is also denied.

Previously the default was `ModeShadow` (log only). Hosts whose addons make
undeclared calls will now see those calls fail.

## Opting back into shadow

```bash
METACORE_ENFORCE=shadow   # also: 0, false, no, off (case-insensitive)
```

Shadow logs every violation but lets the call proceed. Use it only as a
temporary migration aid while fixing manifests.

Any other value (unset, empty, `1`, `true`, `enforce`, or a typo) enforces:
the parser fails closed.

## Migrating

1. Before upgrading, run the previous kernel with `METACORE_ENFORCE=shadow` and
   collect `metacore.capability.violation` lines (or wire
   `Enforcer.OnViolation` to a metric).
2. Declare the missing capabilities in each addon manifest, or fix the addon.
3. Upgrade the kernel; the default now enforces. Keep `METACORE_ENFORCE=shadow`
   only for addons you cannot fix yet, and remove it afterwards.

The mode is still switchable at runtime with `Enforcer.SetMode`.
See also [permissions.md](permissions.md).

## Política de emisión de eventos

`security.EmitPolicy` decide qué nombres de evento puede publicar un addon
**desde la frontera del guest** (import WASM `event_emit`; un host puede usarla
también en el canal de eventos de un native service). No se conecta al bus: el
host publica los eventos CRUD canónicos con la clave del addon dueño y los
suyos como `kernel`, y ninguno de esos pasa por la política.

Reglas, en orden:

| # | Regla (`EmitRule`) | Grupo | Qué deniega |
|---|---|---|---|
| 1 | `kernel_namespace` | reserved | `kernel` y `kernel.*` desde un guest |
| 2 | `contract_namespace` | reserved | un evento bajo un dominio reservado por un evento de contrato (`commerce.payments.*`, ver [capabilities-commerce.md](capabilities-commerce.md#eventos-de-contrato-commercepaymentsconfirmed)) emitido por quien no provee ninguna capacidad de ese dominio |
| 3 | `foreign_namespace` | reserved | un evento cuyo prefijo es la clave de **otro** addon instalado (`storefront_cart.checkout_paid` emitido por `evil`), salvo que el dueño liste al emisor en `extension_points.events[].emitters` |
| 4 | `undeclared` | declared | un evento que no cubre ningún `event:emit` del manifiesto del emisor (`Capabilities.CanEmit`) |
| — | `facts_unavailable` | reserved | no se pudieron cargar los datos de la organización y el evento está fuera del espacio propio del emisor |

Un espacio que no es de ningún addon (`shipping.*`, `stock.*`, `fiscal.*`)
solo pasa por la regla 4.

### Modos

Cada grupo tiene su propio modo, **independiente de `METACORE_ENFORCE`**:

```bash
METACORE_EVENT_EMIT_RESERVED=warn   # reglas 1-3: warn (por omisión) | enforce | off
METACORE_EVENT_EMIT_DECLARED=warn   # regla 4:    warn (por omisión) | enforce | off
```

- `warn`: la violación se registra
  (`metacore.event_emit.violation mode= action=warned org= addon= event= rule= owner= reason=`),
  se cuenta en `wasm_event_emit_violations_total{addon,rule,action}`, se publica
  `kernel.event_emit.violation` (payload `security.EmitViolation`) y el evento
  **sí** se publica.
- `enforce`: lo mismo, y el guest recibe `forbidden`; el evento no se publica.
- `off`: el grupo no se evalúa.

Un valor desconocido (o un error de dedo) cae en `warn` con aviso en la
bitácora: la política se despliega observando primero. `EmitPolicy.SetMode`
cambia el modo en caliente.

### Conexión en el host

```go
policy := security.NewEmitPolicy(security.EmitFactsFunc(
    func(ctx context.Context, org uuid.UUID, emitter string) (security.EmitFacts, error) {
        return security.EmitFacts{
            Installed: installedAddonKeys(org),             // regla 3
            Provides:  providedCapabilities(org, emitter),  // regla 2
            Grants:    emitterGrants(org),                  // extension_points.events[].emitters
            Caps:      addonCaps(emitter),                  // regla 4; nil = no evaluar
        }, nil
    }))
wasmHost.WithEmitPolicy(policy.Check)
```

Si `EmitFacts` falla, la regla 1 se sigue aplicando, un evento del espacio propio
del emisor pasa y cualquier otro cuenta como `facts_unavailable`.

### Emisor en el sobre

Con o sin política, cada entrega lleva el emisor real: `events.PublishWithCount`
registra la clave con la que se llamó al bus (`events.EmitterFromContext`) y el
despachador la estampa como `emitter` en todo payload objeto JSON (sobrescribe
lo que haya escrito el guest) y la guarda en `event_deliveries.emitter`. Un
suscriptor que actúa sobre un evento ajeno (p. ej. `channel_orders` con
`storefront_cart.checkout_paid`) debe comprobar `emitter`.

### Despliegue sugerido

1. Kernel con ambos grupos en `warn`; revisar `kernel.event_emit.violation`.
2. Declarar en los manifiestos las emisiones que falten y los `emitters` de los
   dueños.
3. Con cero violaciones de las reglas 1-3 durante el plazo acordado:
   `METACORE_EVENT_EMIT_RESERVED=enforce`. La regla 4 pasa a `enforce` cuando
   todos los addons publicados estén al día.
