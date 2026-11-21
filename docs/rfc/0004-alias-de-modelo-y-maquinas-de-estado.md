# RFC 0004 — Alias de modelo y máquinas de estado declarativas (manifiesto v3)

Estado: alias IMPLEMENTADOS (aditivo, opcional); máquinas de estado: YA EXISTÍAN (ver §2), este RFC solo propone los
huecos que quedan. Ningún manifiesto existente cambia de significado.

Motivación: los módulos nativos de una migración (clientes con rutas y slugs heredados del front) y cualquier addon
que renombre un modelo necesitan (a) resolver varios slugs hacia un mismo modelo y (b) declarar sus estados y
transiciones en lugar de repetir CHECK, lookups y guardas en Go.

## 1. `models[].aliases` (implementado)

```json
{ "key": "Order", "table": "orders", "aliases": ["sale", "order-items"] }
```

- Lista opcional de slugs adicionales con los que `/metadata/table|modal/{slug}` y las rutas de `dynamic`
  (`/data`, `/options`, acciones, exportación) resuelven al modelo. Se comparan sin distinguir mayúsculas ni
  separadores y tolerando singular/plural: `branch-cash-desk` == `branch_cash_desks` == `BranchCashDesk`.
- **Dónde se resuelve** (aprendido): el kernel no posee el índice de modelos (lo tiene el host mediante
  `ModelResolver`), así que la resolución ocurre UNA vez en el borde HTTP: `dynamic.Service.CanonicalModel` y
  `metadata.Service.CanonicalModel` traducen el slug a la clave canónica antes de que corra nada más. Permisos,
  auditoría, eventos y caché ven siempre la clave canónica. Se activa con `Config.ModelAliases`
  (`modelalias.Resolver`, normalmente un `*modelalias.Index`); sin él, comportamiento idéntico al anterior.
- **Prioridad**: si el slug ya es un modelo conocido, gana (los alias no cuestan nada a quien no los usa y nunca
  pueden sombrear un modelo real). Solo si no se conoce se consulta el índice.
- **Conservador**: coincidencia exacta normalizada primero; una variante singular/plural solo se acepta si apunta a
  UN solo modelo. Nunca adivina entre dos.
- **Validación**:
  - En el manifiesto (`v3.Validate`): un alias no puede repetir la `key`/`table` propia, ni la `key`/`table`/alias de
    otro modelo del mismo manifiesto, ni otro alias propio (comparación normalizada).
  - Entre manifiestos: `modelalias.Index.Add(key, table, aliases)` devuelve `*ConflictError` y no registra nada si un
    nombre ya pertenece a OTRO modelo. El host lo llama al instalar; un conflicto rechaza la instalación, nunca elige
    uno en silencio. Reinstalar la misma clave reemplaza sus nombres (upgrade); `Remove` los libera.
- No cambia permisos: el permiso sigue siendo el del modelo canónico.
- Fuera de alcance: alias a nivel de acción o campo; alias por organización.

## 2. `models[].state_machine`: ya existe como `stage_field` + `stages` + `transitions` + `on_transition`

Hallazgo al implementar: el kernel ya trae la máquina de estados declarativa (no con ese nombre):

| Propuesta del borrador | Lo que ya existe |
|---|---|
| `state_machine.column` | `models[].stage_field` (columna real, validada) |
| `states[]` (key, label, terminal) | `models[].stages[]` (key, label, color, order, `isFinal`, `locked`, `editableWhenLocked`) |
| `transitions[]` from/to | `models[].transitions[]` from/to + `guard`, `roles`, `requires_fields`, `approval`, `reopen`, `sla` |
| guarda en Update | `dynamic.StageMachine.Allows` aplicada por `Service.Update` (422 `invalid_transition`) |
| hooks | `models[].on_transition[]` (`set` sobre la fila, `do: wasm:|webhook:|compiled:`, `required`) |
| `requires_state` en acciones | ya válido y verificado contra los `stages` (`stage_authority` para acciones que mueven estado) |
| UI de estado | `TableMetadata` deriva el `status` de la columna desde `stages` |

Por tanto NO se añade un bloque `state_machine` paralelo: sería un segundo vocabulario para lo mismo. Un módulo
nativo declara sus estados con `stage_field`/`stages`/`transitions` y obtiene todo lo anterior.

### Huecos reales (propuestos, no implementados)
1. **CHECK derivado** (IMPLEMENTADO como `models[].stage_check`, opt-in, `NOT VALID`; `dynamic.StageCheckValidateStatement` valida las filas viejas) (`CHECK (col IN (stages...))`): hoy los `stages` no generan restricción en DB; un valor fuera
   de la máquina solo se rechaza por la ruta dinámica, no por escrituras directas de módulos nativos. Propuesta:
   `stage_check: true` opt-in por modelo (no por defecto: una tabla con datos previos fuera de la máquina fallaría
   al instalar); el instalador lo crea `NOT VALID` y valida en una fase aparte.
2. **Permiso por transición** (IMPLEMENTADO: `transitions[].permission`, una capacidad que sustituye al gate de `roles` para esa jugada; requires_fields, guard y approval siguen aplicando; la ruta wasm/guest sigue usando `stage_authority`): hoy se autoriza por `roles` o por la acción que mueve
   (`stage_authority`). Un permiso nombrado permitiría delegar una transición concreta sin crear un rol.
3. **Eventos de transición por el outbox** (IMPLEMENTADO, opt-in con `dynamic.Config.EmitTransitionEvents`: acción canónica `transitioned` con `stage_from`/`stage_to`, por el mismo outbox persist-first + relay que created/updated/deleted): `on_transition` ya despacha; falta que el evento canónico
   `<addon>.<model>.<action>` salga por el outbox transaccional (ver §3) en vez de post-commit sin respaldo.

## 3. Outbox para eventos de módulos nativos

Ver `nativekit` (primitivos), `nativekit/ledger` (`Config.OnPost` corre dentro de la transacción para encolar un job
atómico) y el paquete `outbox` del kernel (`EnqueueTx`). OPS ya tiene el relé de outbox
(`handlers/event_outbox_relay*`); `NativeEnv.Events` debe escribir en la MISMA tabla dentro de la transacción del
módulo (variante transaccional `EmitTx(tx, org, event, payload)`), entrega at-least-once.

## 4. Lo que NO se propone
- Máquinas de estado jerárquicas, paralelas o con tiempos.
- Alias a nivel de acción o campo.
