# Motivo obligatorio y ajustes por sucursal (PER-4, POS-1)

Dos primitivos de manifest v3 que llegan juntos en una misma versión del kernel.

## `Model.reason_required` — motivo obligatorio al eliminar / cancelar

```json
{
  "key": "Sale",
  "reason_required": { "delete": true, "actions": ["cancel"], "min_length": 5 }
}
```

- `delete`: `DELETE /dynamic/:model/:id` exige un motivo.
- `actions`: claves de `contributions.actions[]` con `target_model` = este modelo. El motivo va en el payload (`reason`).
- `min_length`: largo mínimo del motivo recortado (default 3).

El motivo viaja en `?reason=` o en el cuerpo JSON `{"reason": "..."}` (DELETE) y en `payload.reason` (acciones).
Sin motivo, o demasiado corto, la operación **no se ejecuta** y responde **422** con el mismo contrato de error por campo que
el resto de la validación: `errors.reason: [{code: "required"}]` o `[{code: "min", params: {min: N}}]`.

**Bitácora.** El motivo se estampa en `CanonicalEvent.Reason`:
- borrado: evento `<addon>.<Model>.deleted` (con `before`);
- acción con motivo: evento `<addon>.<Model>.<clave de la acción>` (con `before`) solo si la acción tuvo éxito.

El activity log universal ya consume `CanonicalEvent`, así que quién/qué/cuándo/por qué queda sin código extra.

Wiring del host: `dynamic.Config.ReasonPolicyResolver` (patrón de `ConstraintResolver`). Sin resolver, el primitivo está apagado.
Los borrados que no pasan por `Service.Delete` (wasm `data_mutate`) no lo aplican todavía.

## `Setting.scope` — ajustes por sucursal

```json
{ "key": "stock_policy", "type": "select", "scope": "branch", "default": "block", "options": [...] }
```

`scope`: `org` (default, un valor por organización) o `branch` (cada sucursal puede sobreescribirlo).
Valor efectivo, en orden: override de la sucursal → valor de la org → `default`. Lo resuelve `manifest.ResolveSetting(def, orgValues, branchValues)`;
un override `null` no tapa el valor de la org (para volver a heredar se borra la clave).

El kernel solo declara y resuelve; el almacenamiento de los overrides es del host (ops).

## Cadena de bumps (primitivo nuevo de manifest)
kernel → tag → `addons/tools/manifestcheck` + `addons/tools/addon-preflight` + `hub/backend` + `ops/backend` → addons.
