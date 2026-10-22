# Configuración guiada y salud de addons (`contributions.setup` / `contributions.health`)

Todo addon con piezas externas (credenciales, DNS, un webhook que registrar al
otro lado, un emparejamiento) necesita la misma página: "termina de
configurarme". Este contrato la declara **una vez en el manifest** y el host
(ops) y el SDK la dibujan igual para todos los addons. Es 100% opcional: un
addon sin `setup`/`health` se comporta como siempre.

Código: `manifest/v3/setup.go` (contrato + validación), paquete `setupflow/`
(resultado del handler, ejecutor, planner). Ejemplo completo:
`docs/spec/v3/examples/addon-setup-example.json`.

## Manifest

```jsonc
"contributions": {
  "setup": [{
    "key": "main",
    "title": "mailer.setup.title",
    "trigger": "on_install",            // on_install | manual (default)
    "permission": "mailer.setup.manage",// debe estar en rbac.permissions si es del propio addon
    "scope": "org",                     // org (default) | instance
    "steps": [
      { "key": "node", "title": "…", "kind": "check",
        "check": { "handler": { "type": "native", "operation": "preflight" }, "timeout": "20s" } },
      { "key": "settings", "title": "…", "kind": "form", "depends_on": ["node"],
        "form": { "settings": ["dns_mode", "relay_password"] } },       // o { "connector": "cloudflare" }
      { "key": "apply", "title": "…", "kind": "action", "optional": true,
        "action": { "action": "apply_dns_cloudflare" },                 // clave de contributions.actions
        "skip_when": { "setting": "dns_mode", "equals": "manual" } },
      { "key": "records", "title": "…", "kind": "records",
        "records": { "handler": { "type": "wasm", "function": "ListRequiredDnsRecords" } } },
      { "key": "dns_check", "title": "…", "kind": "check", "depends_on": ["records"],
        "check": { "handler": { "type": "wasm", "function": "VerifyDns" },
                   "poll": { "every": "30s", "max_every": "1h", "factor": 2, "give_up_after": "72h" },
                   "timeout": "45s" } },
      { "key": "help", "title": "…", "kind": "custom", "custom": { "slot": "mailer.setup_dns" } }
    ]
  }],
  "health": { "handler": { "type": "wasm", "function": "HealthSummary" }, "every": "5m", "permission": "mailer.health.read" }
}
```

Reglas (las aplican **los dos validadores**, `manifest/v3` y
`manifest.Manifest.Validate`, con la misma función `v3.ValidateSetup`):

- `kind` elige **exactamente un** cuerpo: `check | form | action | records | custom`.
- `depends_on`: claves del mismo flujo, sin repetirse, sin autoreferencia y
  **sin ciclos**. Un paso `optional` nunca bloquea a sus dependientes ni la
  finalización del flujo.
- `form`: exactamente uno de `settings` (claves de `settings[]` del addon) o
  `connector` (clave de `connectors[]`).
- `action.action` debe existir en `contributions.actions`. `custom.slot` debe
  estar contribuido en `contributions.slots[].slot_kind` (esta comprobación y la
  del permiso propio solo existen en el validador v3: el portador legacy no
  proyecta RBAC ni slots).
- `skip_when`: `setting` existente y exactamente uno de `equals` / `in`.
- Handlers: `type` `wasm` (con `function`) o `native` (con `operation`, exige
  `runtime.native_service`). Los `wasm` de `check`, `records` y `health` se
  suman a `backend.exports` (`deriveBackend`): "declarado = existe" lo verifica
  el scanner del hub igual que con las acciones.
- A lo más **un** flujo con `trigger: on_install`.
- `poll`: `every` 1s..24h, `max_every >= every`, `factor` 1..10 (default 2),
  `give_up_after` hasta 30d (default 24h). `check.timeout` hasta 5m (default 30s).
  `health.every` 10s..24h (default 5m).
- Límites: 8 flujos, 40 pasos por flujo, 16 `depends_on` por paso.

### `Setting.visible_when` y `Setting.write_only`

También en `connectors[].credentials[]`:

```json
{ "key": "relay_password", "type": "string", "write_only": true,
  "visible_when": { "field": "dns_mode", "equals": "manual" } }
```

- `visible_when` reutiliza `VisibleWhen` (`field` + `equals` | `in`), **solo
  contra settings hermanos** de la misma lista; `class` no aplica. Sin
  autorreferencia ni ciclos de visibilidad.
- `write_only`: el host acepta el valor y **nunca** lo devuelve (solo si está
  puesto). Implícito para `type: secret`. No admite `default`.

## Contrato del handler (`setupflow`)

Un handler de `check` responde el sobre del kernel `{success,data,meta}`:

```json
{"success":true,"data":{
  "status":"pending",
  "message_key":"mail.dns.propagating",
  "params":{"missing":2},
  "detail":"MX mail.example.com aún no visible",
  "retry_after_s":60,
  "fix":{"label_key":"mail.dns.apply","action":"apply_dns_cloudflare"}}}
```

| Campo | Regla |
|---|---|
| `status` | `pending` · `running` · `ok` · `warn` · `error` · `blocked` |
| `message_key` | clave i18n (`^[A-Za-z][A-Za-z0-9_.-]*$`, ≤200) |
| `params` | objeto JSON ≤8 KiB, rellena el mensaje |
| `detail` | texto técnico ≤4 KiB (nunca el titular) |
| `retry_after_s` | 0..86400, sugerencia para el siguiente poll |
| `fix` | `{label_key, action \| step \| url(https), params?}`, exactamente un destino; no con `ok`/`warn` |

`pending`/`running` hacen que el ejecutor reintente; `ok`, `warn`, `error` y
`blocked` **cierran** el paso. `ok` y `warn` cuentan como "hecho". Un
`{success:false}` o un fallo de transporte **no es un estado**: es transitorio y
se reintenta (máx. 5 seguidos). Se rechazan campos desconocidos para que un typo
del handler no pierda la sugerencia en silencio.

Salud (`contributions.health`) devuelve `HealthResult`:
`{status: ok|warn|error, summary_key, params?, metrics:[{key,value,unit?}], issues:[{severity: warn|error, message_key, params?, fix?}]}`.
Un `status` mejor que su peor `issue` se rechaza. `unknown` lo asigna el host
cuando aún no hay resultado o la sonda falló.

## Ejecutor

```go
exec := &setupflow.Executor{
    Invoker: hostInvoker,                       // despacha wasm/native con el principal
    BaseContext: func(r setupflow.Request) context.Context { // ctx del handler, SOLO desde el request
        return hostAuthContext(r.Principal)     // vida independiente: el executor pone el timeout
    },
}
res, err := exec.Run(ctx, *step.Check, setupflow.Request{
    AddonKey: "mailer", FlowKey: "main", StepKey: "dns_check", Scope: flow.Scope,
    Handler: step.Check.Handler, Principal: setupflow.Principal{UserID: u, OrgID: o, Roles: roles},
})
res, err = exec.Poll(ctx, *step.Check, req, func(attempt int, r setupflow.CheckResult) { /* persistir */ })
```

- **Principal del usuario**: obligatorio (`ErrNoPrincipal`), y `OrgID` también
  cuando el flujo es de scope `org` (`Request.Scope` vacío u `org`); solo un
  flujo `instance` corre sin organización. El check corre con lo que ese usuario
  puede hacer, no con una cuenta de servicio del host.
- **Contexto del handler**: la ejecución es compartida y sobrevive a cada
  llamador, así que NUNCA hereda el `ctx` de la petición (ni sus valores de
  auth, transacciones o deadlines). Se construye con `Executor.BaseContext(req)`
  (solo desde el principal) o, sin él, `context.Background()`; el executor añade
  el timeout.
- **Timeout** por ejecución (`ErrTimeout`), aunque el handler ignore su contexto:
  al vencer se libera el slot en vuelo y la siguiente corrida arranca limpia (la
  goroutine colgada se descarta al volver; hasta `MaxAbandoned`=64 abandonadas,
  después `ErrBusy`).
- **Dedupe**: ejecuciones concurrentes de la misma `(org, addon, flow, step,
  usuario)` comparten **una** sola corrida; un llamador que cancela no aborta a
  los demás. Usuarios distintos nunca comparten resultado.
- **Poll con backoff**: `every × factor^n` hasta `max_every`; `retry_after_s`
  reemplaza el cálculo (acotado a [1s, `max_every`]); `give_up_after` →
  `ErrGaveUp` con el último resultado. `PollDelay` es la función pura.
- `setupflow.Plan(flow, statuses, settingLookup)` calcula `Next`, progreso
  (`Percent`, `Complete`) y qué pasos esperan a quién, respetando `skip_when`
  y `optional`.

El host persiste el progreso (ops: `addon_setup_progress`), aplica `permission`
y `scope`, y decide cuándo mostrar la tarjeta/banner. Nada de eso vive en el
kernel.
