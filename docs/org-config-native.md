# Org config para handlers nativos

Moneda/fiscal son config de la org, nunca hardcode. La ruta wasm lee `ctx_get`
scope `org_config`; los handlers nativos (Go compilado) usan la misma fuente:

```go
// host, una vez por request/evento/accion (misma ContextProviderFn que wasm.Host.WithContextProvider):
ctx = orgconfig.WithProvider(ctx, wasm.OrgConfigProvider(ctxProviderFn))

// handler:
cfg, err := actionCtx.OrgConfig(ctx)      // bridge.ActionContext
cfg, err := actionCtx.OrgConfig()         // lifecycle.ActionContext (usa .Ctx)
if errors.Is(err, orgconfig.ErrUnavailable) { /* host sin provider */ }
if cfg.CurrencyCode == "" { /* no configurada: decide el llamador, no hay default */ }
```

- La org sale de `ActionContext.OrgID`; el provider recibe solo ese id.
- `ErrUnavailable` (sin provider), `ErrNoOrg` (OrgID nil).
- Los addons que definen su propio `models.ActionContext` (accounting-lite, pos) deben exponer el `context.Context` y `OrgID` y llamar `orgconfig.Resolve(ctx, orgID)`.
