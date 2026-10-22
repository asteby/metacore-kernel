# Nota de release: ola 0 de plataforma para el addon de correo

Estado: **preparada, sin publicar**. No se creó tag ni GitHub Release; rama
`feat/mail-platform-wave0`, sin push.

## Qué tag corresponde

Todos los commits son `feat:`/`docs:` sin `!` ni `BREAKING CHANGE:` →
`auto-release.yml` calcula el siguiente **minor** (`v0.x+1.0`). No agregar
`BREAKING CHANGE:` al fusionar (en `v0.x` lo convertiría en `v1.0.0`; ver
`docs/RELEASE.md`). Todo es aditivo: ningún campo ni símbolo existente cambia
de comportamiento.

## Quién debe subir el pin del kernel (en este orden)

El schema v3 está embebido (`go:embed`) en el kernel y es estricto
(`additionalProperties:false`): un manifest con `contributions.setup`,
`contributions.health`, `settings[].visible_when|write_only`,
`ingress.listeners` o la capability `domain:manage` lo rechaza cualquier
validador con un kernel anterior.

1. **kernel**: mergear a `main` → tag.
2. **hub** (`hub/backend/go.mod`): mergear **y desplegar** ANTES de publicar el
   addon. Es el que falla tarde: el PR del addon pasa en verde y `publish-addons`
   recibe `400 additional properties '...' not allowed`; el hub se queda en la
   versión anterior.
   Además `runtime/wasm/importpolicy` lista `dns_resolve`: el scanner del hub
   y `addon-preflight` rechazan un guest que lo importe si su kernel no la
   conoce (`forbidden import`), así que el pin del hub y de addon-preflight es
   obligatorio para cualquier addon que use `dns_resolve`, no solo para el
   schema.
3. **ops** (`ops/backend/go.mod`): sin él, el addon revienta al instalarse o
   actualizarse en una instancia (`v3.Parse`/`manifest.Validate`). Además
   necesita el trabajo de runtime propio (abajo).
4. **addons/tools/manifestcheck** (`go.mod`) y
   **addons/tools/addon-preflight** (`go.mod`, pin propio y separado): sin
   ellos CI rechaza el manifest (`schema validation failed at ...`).
   `addons/tools/check-hub-kernel-compat.sh` compara hub vs manifestcheck.
5. **metacore-sdk**: no consume el módulo Go; necesita los tipos TS
   equivalentes (`SetupFlow`, `HealthResult`, `CheckResult`, `Fix`,
   `RecordSetCheck`, `Setting.visible_when/write_only`).

Orden de la cadena: kernel → tag → hub (merge + deploy) → ops re-pineado →
manifestcheck/addon-preflight → addons.

## Qué NO hace el kernel (queda en ops / SDK)

- Tabla de progreso (`addon_setup_progress`), rutas `/setup/<addon>`, banner,
  tarjeta en Instalados, ejecución de `check`/`records`/`health` (implementar
  `setupflow.Invoker` sobre los dispatchers wasm/native) y programar el health.
- Tabla `org_domains` generalizada (migrar `kind` → `purposes[]`, `verified` →
  `status`), handlers de alta/verificación sobre `domains.*`, almacenamiento
  cifrado write-only del token de Cloudflare, `DNSAuthorizeFunc` para
  `Host.WithDNS`.
- Forwarder L4 de `ingress.listeners` (PROXY protocol, asignación de
  `METACORE_NATIVE_LISTENER_<NAME>`), admisión de conflictos de puerto y
  enforcement del egress con `NetworkPolicy.Allows` + `netguard.IsBlockedIP`.
- Dispatcher de la capability `messaging.email.send` (cualquier proveedor que
  la declare en `provides_capabilities`).
