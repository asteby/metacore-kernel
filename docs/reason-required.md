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

## `Action.supervisor_policy` — autorización de supervisor en acciones (POS-2 / PER-2)

```json
{ "key": "cancel_fiscal", "target_model": "Invoice", "supervisor_policy": "cancel_cfdi", "handler": {...} }
```

La acción exige la autorización en el momento de un supervisor para la política `general.approve_<policy>`
(descuento, precio bajo el mínimo, reembolso, cancelar CFDI, ajuste de inventario…).

- Quien **tiene la capacidad** (o es admin) ejecuta directo: `Config.SupervisorBypass`.
- Quien no, obtiene una autorización con PIN (`POST /approvals/pin-grant`) y manda su id como `approval_id` en el payload.
- `dynamic.ConsumePINGrant` canjea la autorización: de la misma org, `kind=pin` de esa política, pedida por el **mismo usuario**,
  con menos de `PINGrantMaxAge` (15 min), del mismo registro si quedó anclada a uno, y **de un solo uso** (atómico).
  Sin autorización canjeable: 403 `approval_grant_required`.
- La metadata servida lleva `supervisorPolicy`: el SDK pide el PIN por sí solo antes de despachar.
- Distinta de `Action.approval`, que estaciona la solicitud para una decisión asíncrona.
