# ADR 0002: almacén de ajustes de addon con alcance y revisión, y un solo contrato de resolución

- Estado: aceptado (opción C, revisada). Fase F1 implementada en este cambio.
- Fecha: 2026-10-24
- Paquetes: `settings` (nuevo), `runtime/wasm`, `migrations`. Versión: **minor, aditiva**.

## Contexto

Los ajustes de un addon viven hoy en `metacore_installations.settings` (jsonb de la fila de instalación). Eso mezcla tres cosas que cambian a ritmos distintos: la fila de ciclo de vida del addon (versión, estado, secreto HMAC), los valores que el administrador fija, y claves de sistema con prefijo `_` (`_capability_defaults`, `_capability_markets`). Consecuencias medidas en ops:

1. **Tres reimplementaciones divergentes del merge** de ajustes: `resolveWasmSettings`, `mergeInstallSettings` y `GetEffectiveSettings`. Cada una decide distinto qué gana y qué se ignora.
2. **El camino wasm ignora los overrides por sucursal.** `env_get` sirve el mapa que le pasa el llamador; los `scope: branch` del manifest solo se respetan en la ruta HTTP.
3. **Sin revisión.** Dos pestañas pisan los valores del otro sin enterarse; no hay etag ni evento de cambio.
4. **Los defaults se materializan** en la fila al instalar (`defaultSettings`) y se re-mezclan al actualizar (`mergeSettings`), de modo que un default nuevo del manifest no llega a quien ya tenía el valor viejo copiado.
5. **Retirar la tabla de instalaciones** (`installer/dualwrite.go`, F1.6) arrastraría los ajustes con ella.

## Decisión

Los ajustes no secretos salen de la fila de instalación a una tabla del kernel, `metacore_addon_settings`, con **alcance** y **revisión**, y se resuelven por **un único contrato**: `settings.Resolver`.

- Clave primaria `(org_id, addon_key, scope_kind, scope_id)`; `scope_kind` es `org` (`scope_id = ''`) o `branch` (`scope_id` = id de sucursal). Sin claves foráneas hacia tablas legacy.
- `rev` sube en cada cambio real; `Write` usa concurrencia optimista (`expectedRev`). Un parche que no cambia nada no sube `rev` ni emite evento.
- **Los defaults nunca se materializan.** Se leen del manifest instalado al resolver.
- **Secretos fuera:** una clave `type: secret`, `write_only` o `secret` se rechaza en `Write` con un error que indica usar `org_integrations`. `Resolve` tampoco las devuelve.
- **Claves `_` reservadas:** se rechazan en `Write` de usuario. Siguen en su lugar actual hasta que se decida su destino (ver abiertas).
- `Write` valida cada valor por tipo contra el manifest instalado y rechaza claves no declaradas. En alcance `branch` solo admite claves `scope: branch`.
- `Write` publica `settings.changed` (org, addon, alcance, rev, nombres de clave; nunca valores) si hay `Publisher` (`*events.Bus` ya lo cumple).

### Precedencia (probada)

```
default del manifest instalado  <  valor de org  <  override de sucursal
```

El override de sucursal aplica solo a claves que el manifest instalado declara con `scope: branch` y solo si el contexto trae sucursal. Una clave almacenada que el manifest ya no declara se ignora con log, no falla. La regla por clave es `manifest.ResolveSetting`, que se reutiliza sin copiarla. Un valor `nil` nunca tapa al de abajo; borrar un override es enviar `nil` en el parche.

### Gancho en el host wasm

`wasm.SettingsResolver` (interfaz mínima, opcional vía `Host.WithSettingsResolver`). Con resolver y org, `invokeImpl` resuelve con la sucursal del contexto (`dynamic.BranchIDFromContext`). El mapa `settings` de `Invoke*` queda como base y fallback (obsoleto como fuente única). Sin resolver, o sin org, o si el resolver falla, el comportamiento es idéntico al anterior. `settings.WasmResolver` capa así: lo **almacenado** (org/sucursal) pisa la base; los **defaults** solo rellenan claves ausentes en la base; lo que solo está en la base (secretos, claves `_`, valores aún no migrados) pasa intacto. Es lo que hace segura la convivencia durante F2.

## Alternativas descartadas

- **Espejo permanente** (escribir siempre en ambos sitios y mantenerlos sincronizados): dos fuentes de verdad para siempre; la divergencia actual con una capa más. Solo se admite doble escritura transitoria durante F2, con fecha de retiro.
- **B′ (dejar los ajustes en la fila de instalación y añadir `rev` y un mapa por sucursal dentro del jsonb):** no resuelve que la fila de instalación se retira, mezcla secretos, claves de sistema y ajustes en un mismo documento con un solo `rev` (cualquier cambio de ciclo de vida invalida la edición del administrador) y deja el merge en el instalador.
- **Una tabla por addon / columnas tipadas:** el kernel es general; el esquema lo declara el manifest, no la migración.

## Plan de fases

- **F0** Diseño y este ADR.
- **F1 (este cambio, aditivo y reversible):** migración 0008, paquete `settings`, gancho wasm. Nada en producción lee ni escribe la tabla.
- **F2** (ops): backfill idempotente de `metacore_installations.settings` (solo claves declaradas, no secretas, no `_`) a filas `org`, y de los overrides de sucursal existentes a filas `branch`; doble escritura transitoria detrás de un flag; `WithSettingsResolver` en el host, también tras flag, con comparación en sombra (resolver nuevo vs. mapa legacy, log de diferencias).
- **F3** (ops): las tres reimplementaciones pasan a llamar a `Resolver`/`Store`; HTTP y wasm leen igual. Se apaga la doble escritura.
- **F4** Se dejan de leer los ajustes de `metacore_installations.settings`; se decide el destino de las claves `_`; se retiran las implementaciones viejas.

## Criterios de borrado de lo viejo

La columna `settings` de `metacore_installations` y las tres funciones de merge se pueden borrar cuando se cumplan **todas**:

1. Cero diferencias en la comparación en sombra durante al menos 14 días con tráfico real, en cada entorno.
2. Ninguna lectura de `installation.Settings` fuera de las claves `_` y los secretos (auditado con grep en kernel y en hosts).
3. La doble escritura lleva apagada al menos un ciclo de despliegue sin reversiones.
4. Backup de la columna tomado y restauración probada.
5. Las claves `_` tienen destino definido (tabla propia o `org_integrations`).

Nada se borra en F1.

## Decisiones abiertas

- Destino de las claves `_capability_*` (siguen en la fila de instalación; `capability.SelectionFromSettings` no cambia).
- Si `rev` combinado (suma org + sucursal) basta como etag HTTP o se expone cada capa por separado (`Effective.OrgRev`/`BranchRev` ya están).
- Si el borrado de una sucursal debe limpiar sus filas (hoy no hay FK; lo haría un suscriptor).
- Vocabulario de tipos: la validación es estricta para los tipos conocidos y permisiva para el resto, porque el manifest no enumera los tipos de ajuste.
