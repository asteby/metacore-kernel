# RFC-02: acción declarativa que cambia campos (`set_fields`) con confirmación escrita

- **Prioridad:** P1 (M2). Útil ya en M1 para «Retirar instancia» sin wasm.
- **Tamaño:** M (contrato S en v0.230.0 + runtime kernel M + ops S + SDK S).
- **Depende de:** contrato v0.230.0. RFC-01 reutiliza su ejecutor para `on_success.set`.

## 1. Problema (evidencia)

- Cambiar un campo desde una acción («Retirar» → `status = retired`) exige hoy un handler wasm [S]:
  `Handler.type` admite `wasm|webhook|compiled|connector|native|capability`
  (`manifest/v3/types.go:2783-2805`, enum del schema) y nada que escriba campos [V].
- Existen piezas sueltas que **no** cubren el caso:
  - `on_transition[].set` estampa campos al **entrar** en una etapa, pero solo reacciona a un `Update`
    que ya cambió la etapa; no es una acción con formulario, motivo ni confirmación
    (`manifest/v3/types.go:900-923`) [V].
  - `Column.protected` bloquea la escritura genérica y su propia documentación dice que la única vía
    legítima es «una acción declarada», que hoy solo puede ser wasm (`types.go:1238-1257`) [V].
  - OPS tiene un `ActionRegistration.ToState` que **nadie** usa (`models/models.go:172`) [V].
- Lo que sí existe y se reutiliza [V]: `requires_state` (servidor, 422 `invalid_state`, ops
  `handlers/dynamic.go:3184`), `Model.reason_required.actions` + `CanonicalEvent.Reason`
  (`types.go:806,1382-1391`, ops `handlers/dynamic.go:3045`), `supervisor_policy`, `approval`, la
  máquina de etapas (`stage_field`/`transitions`, valida el movimiento en `Update`), `Model.locking: row`,
  `constraints`, `rules` y los eventos de dominio (despachados desde kernel #216).
- Falta la **confirmación escrita** («escribe el nombre del cliente»): el SDK solo tiene `ConfirmDialog`
  con botón (`packages/ui/src/dialogs/confirm-dialog.tsx`), sin campo de texto [V]. Es requisito explícito
  para Revocar y Retirar (hub `feat/panel-ux`, `docs/HUB-OPS-UX-REQUISITOS.md`).

## 2. Diseño

### 2.1 Manifest

```json
{
  "models": [{
    "key": "Instance",
    "reason_required": { "actions": ["retire"], "min_length": 3 },
    "columns": [
      { "name": "status", "type": "text", "protected": true, "display": "status",
        "options": [{ "value": "active", "color": "green" }, { "value": "retired", "color": "gray" }] },
      { "name": "retired_at", "type": "timestamp", "protected": true, "display": "relative_date" },
      { "name": "retired_by_id", "type": "uuid", "protected": true, "display": "creator" },
      { "name": "retired_reason", "type": "text", "protected": true }
    ]
  }],
  "contributions": {
    "actions": [{
      "key": "retire",
      "label": "hub_fleet.action.retire",
      "icon": "Archive",
      "target_model": "Instance",
      "placement": "row",
      "tone": "danger",
      "requires_state": ["active"],
      "confirm": true,
      "confirm_message": "hub_fleet.action.retire_impact",
      "confirm_typed": { "field": "name" },
      "fields": [{ "key": "reason", "type": "string", "widget": "textarea", "required": true }],
      "handler": {
        "type": "set_fields",
        "set": {
          "status": "retired",
          "retired_at": "$now",
          "retired_by_id": "$actor.id",
          "retired_reason": "$payload.reason"
        }
      }
    }]
  }
}
```

| Campo | Dónde | Regla |
|---|---|---|
| `handler.type: "set_fields"` | `Handler` | **Nuevo** valor del enum. Solo en `contributions.actions[]` con `target_model` y `placement` `row` (no `create`/`table`). |
| `handler.set` | `Handler` | **Nuevo.** Mapa `columna → valor`. Columnas del modelo destino; prohibidas las gestionadas (`id`, `organization_id`, `created_*`, `updated_*`, `deleted_*`), las `generated`, las `sequence` y las de `snapshots`. Prohibido en modelos `append_only`. Al menos una clave. |
| valores de `set` | | Escalar literal, `null`, o un **token**: `$now`, `$actor.id`, `$payload.<campo>` (el campo debe estar declarado en `fields`/`steps`), `$record.<columna>`. Un literal que empiece por `$` se escribe `$$…`. Cualquier otro `$…` es error de validación. |
| `confirm_typed` | `Action` | **Nuevo.** `{ "field": "<columna>" }`: la UI pide escribir el valor actual de esa columna; el servidor lo comprueba (`confirm_text` en el payload). Implica `confirm: true`. Vale para **cualquier** tipo de handler (también webhook). |
| `tone` | `Action` | **Nuevo.** `"default" \| "danger"`. Pura UI: el SDK separa y colorea las acciones destructivas (requisito transversal de UX). Por omisión `default`. |
| `confirm_message` | `Action` | Existente. El SDK interpola `{{record.<columna>}}` para el «resumen de impacto» (cambio solo de SDK). |

### 2.2 Semántica en el servidor (orden)

1. Cargar el registro filtrado por org (igual que hoy).
2. `requires_state` (existente).
3. `confirm_typed`: `strings.TrimSpace(payload.confirm_text)` debe ser igual a la columna (comparación
   exacta tras `TrimSpace`; Unicode NFC). Si no: 422 `confirmation_mismatch`. `confirm_text` se elimina
   del payload antes de seguir.
4. Validación del payload (`fields`), motivo (`reason_required`), `supervisor_policy`, `approval`
   (existentes, mismo orden que hoy).
5. Resolver tokens de `set` (`$now` = `time.Now().UTC()` del servidor, nunca del cliente).
6. **Escritura** por la ruta normal de `dynamic.Service.Update` dentro de la transacción de la acción,
   con dos opciones nuevas del kernel:
   - `dynamic.WithProtectedWrite(ctx, cols...)`: `validateColumns` permite escribir **solo** las columnas
     `protected` listadas en `set` (`dynamic/validate.go:176`). Ninguna otra.
   - `dynamic.WithExpectedState(ctx, field, allowed...)`: la fila se relee con `SELECT … FOR UPDATE`
     y, si el estado cambió entre el paso 2 y el 6, 409 `state_changed` (cierra el TOCTOU de dos
     operadores que retiran a la vez).
   Todo lo demás del `Update` aplica igual: transición de etapa validada si `set` toca `stage_field`,
   `on_transition` (con su `set`), `constraints`, `rules`, `locking`, estampado de `updated_by_id` con el
   actor y el evento canónico `<addon>.<Model>.updated` con `Reason`.
7. Evento de acción `<addon>.<Model>.<action>` con `{actor, reason, set, before, after}` (solo las
   claves de `set`). Es un evento de dominio normal: cualquier addon puede suscribirse (despachado desde
   kernel #216).
8. Respuesta 200 `{success:true, data:<fila actualizada>}`; errores de validación como hoy (422).

### 2.3 Auditoría

No hace falta tabla nueva: quién/cuándo/motivo quedan en `updated_by_id`/`updated_at`, en el
`CanonicalEvent.Reason` del `…updated` y en el evento de acción, que el historial unificado
(`/api/activity`) ya muestra. Si el addon quiere el dato **en la fila** (para filtrar «Revocadas por
X»), lo declara con tokens (`retired_by_id: "$actor.id"`), como en el ejemplo.

## 3. Cambios por repo

| Repo | Archivos | Cambio |
|---|---|---|
| kernel (contrato, v0.230.0) | `manifest/v3/types.go` (`Handler.Set`, `Action.ConfirmTyped`, `Action.Tone`), schema ×2, `manifest/v3/validate.go`, `manifest/manifest.go` (`ActionTrigger.Set`, `ActionDef.ConfirmTyped`, `ActionDef.Tone`), `manifest/from_v3.go` (`mapActions`, `handlerToTrigger`), `manifest/validate.go` (`validateActionTrigger` caso `set_fields`), `modelbase/metadata.go` (`ActionMetadata.ConfirmTyped`, `Tone`; `ActionTrigger.Set` **no** se sirve) | Campos y reglas de §2.1 en ambos validadores (memoria: la validación dual del hub). |
| kernel (runtime) | `dynamic/action_set.go` (nuevo): `ResolveSet(set, row, payload, actor, now)`, `ApplyActionSet(ctx, tx, model, user, id, def, payload)`; `dynamic/validate.go` (`WithProtectedWrite`); `dynamic/service.go` (`Update`: `WithExpectedState`); `dynamic/action.go` (dispatcher `set_fields` para `ExecAction`) | Ejecutor reutilizable por cualquier host. |
| ops | `services/addon_service.go` (rama `Trigger.Type == "set_fields"` → `kdynamic.ApplyActionSet` con la tx de `ExecuteAction`); `handlers/dynamic.go` (`confirm_typed` antes del handler, para todos los tipos; borrar `ToState`/`FromStates` muertos o mapearlos aquí) | Wiring. |
| SDK `runtime-react` | `action-modal-dispatcher.tsx` / confirm: campo «Escribe *X* para confirmar» con el botón deshabilitado hasta que coincida; envía `confirm_text`; `tone: danger` separa la acción en el menú y usa variante destructiva; interpolación `{{record.x}}` en `confirmMessage`. `types.ts`: `ActionMetadata.confirmTyped`, `tone`. | Render. |

## 4. Compatibilidad hacia atrás

- Todo es aditivo; acciones existentes no cambian. `confirm_typed` en un host viejo: el schema estricto
  lo rechaza → bump previo del hub/validadores (PLAN §2). Un **ops** viejo con un manifest nuevo: la
  Fase 0 de RFC-01 hace que `set_fields` sin despachador falle con 501 en vez de dar falso éxito.
- Un SDK viejo ignora `confirmTyped`, pero el servidor exige `confirm_text`: la acción falla con
  `confirmation_mismatch` (fail-closed, nunca un retiro sin confirmación). Por eso ops sube el SDK en el
  mismo release que el kernel de runtime.

## 5. Pruebas

- Validación: columna inexistente, columna gestionada/generada/sequence, token desconocido,
  `$payload.x` sin campo declarado, `set_fields` en `placement: create`, en modelo `append_only`,
  `confirm_typed.field` inexistente.
- Runtime kernel: escribe una columna `protected` listada y rechaza una no listada; transición de etapa
  inválida → 422 `invalid_transition`; `on_transition.set` se encadena; dos ejecuciones concurrentes →
  una 200 y otra 409 `state_changed`; eventos `…updated` (con `Reason`) y `….retire` emitidos una vez.
- OPS (integración): `confirm_text` incorrecto → 422 sin escribir; correcto → fila actualizada y
  entrada en `/api/activity` con actor y motivo; PUT genérico sobre `status` sigue → 422 `protected_field`.
- SDK: el botón no se habilita hasta escribir el valor exacto; `tone: danger` agrupa al final.

## 6. Seguridad

- La acción es la **única** ruta que escribe esas columnas `protected`; el gate de permisos de la acción
  (`<tabla>.<acción>` o `action.<acción>`, ops `handlers/dynamic.go:3001-3005`) decide quién puede.
- `$now` y `$actor.id` los pone el servidor; el cliente no puede fijarlos.
- La confirmación escrita se valida en el servidor: un script con token también tiene que nombrar el
  recurso (protege contra el «revocar el equivocado» por id).
- RLS: carga y escritura con el filtro por org de siempre; `set` no puede tocar `organization_id`.
