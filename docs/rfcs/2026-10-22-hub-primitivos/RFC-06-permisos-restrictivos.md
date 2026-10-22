# RFC-06: permisos restrictivos por omisión

- **Prioridad:** §A P0 (M1, lint sin kernel) + regla tolerada en el contrato v0.230.0; §B P1 (M2).
- **Tamaño:** S + S.

## 1. Evidencia: G10 es otra cosa de lo que parece

El spike reporta «`rbac` sin `permissions` → *legacy unrestricted mode*» [S]. Leyendo el código hay
**dos** sistemas de permisos y el log viene del que no es el de usuarios:

1. **RBAC de usuarios** (quién puede ver/editar/ejecutar): ops `handlers/dynamic.go:627-663` →
   `modelPermissionAllowed` (`handlers/model_permission_gate.go:28-77`). Para un usuario no admin es
   **deny-by-default**: sin grant que resuelva a `<tabla>.<acción>` (o a una capacidad del addon mapeada
   por `AnyCapabilityGrantsModelAction`) responde 403 [V]. Admin/owner/superadmin pasan por super-rol.
   Esto ya es restrictivo.
2. **Permisos del addon como principal** (qué modelos puede tocar el *código* del addon: interceptores,
   handlers): `services/addon_permissions.go`. Si `manifest.Permissions` está vacío el addon queda en
   *legacy unrestricted mode* (`:71-77`) — ese es el log del spike [V]. Y **todo addon v3** cae ahí:
   `FromV3` nunca llena `manifest.Permissions` (no hay asignación en `manifest/from_v3.go`), y ops lo copia
   tal cual (`services/metacore_ports.go:755-761`) [V]. Las `capabilities` (`db:read`/`db:write`) del v3
   solo se aplican al guest wasm (`security.Compile`, `security/context.go:36-76`).

Huecos reales:

- **A. Claves de permiso que no resuelven, sin aviso.** El spike usa `nav.items[].permission:
  "hub_fleet.instance.read"` y un rol con `["hub_fleet.instance.read","hub_fleet.instance.write"]`, sin
  declararlas en `rbac.permissions` [S]. `v3/validate.go` solo valida las claves de
  `columns[].read_permission` y `rbac.permissions[].models` (`manifest/v3/validate.go:2055-2110`) [V]. Si
  la convención `<addon>.<modelo>.<verbo>` no resuelve al modelo (p. ej. un typo, o un verbo que no es
  CRUD), el rol **no concede nada** y nadie se entera: falla cerrado pero en silencio, y el staff termina
  usando admin para todo (que es lo contrario de mínimo privilegio).
- **B. El addon como principal sin restricción.** Para un addon declarativo sin código (como `hub_fleet`)
  el impacto es nulo; para addons con wasm/interceptores que tocan modelos de otros, el enforcer de ops no
  restringe nada.

## 2. Diseño

### A. Validar las claves (lint ya, regla del kernel después)

Regla nueva `rbac_permission_resolves`: cada clave usada en `rbac.roles[].permissions`,
`navigation[].items[].permission`, `contributions.dashboard[].permission` y `columns[].read_permission` debe:

- estar declarada en `rbac.permissions[].key` (con o sin `models`), **o**
- ser canónica: `<tabla>.<acción>` de un modelo propio o de un `requires`, `screen.<slug>.access`,
  `general.approve_<policy>`, **o**
- seguir `<addon_key>.<modelo>.<read|write|create|update|delete|index|show|export|import|<acción>>` con
  `<modelo>` resoluble a un modelo propio (misma regla que `AnyCapabilityGrantsModelAction`).

Implementación en dos pasos (política de `docs/validation-compat-window.md`):

1. **Ya (P0, sin bump):** el mismo chequeo como lint en `addons/tools/manifestcheck` (y en el de
   hub-addons si D5 lo crea). Los addons privados del Hub nacen limpios.
2. **Contrato v0.230.0:** `tolerate(RuleRBACPermissionResolves, …)` en `manifest/v3/validate.go` y
   registro en `compat.go`: aviso al instalar, error al publicar (modo estricto) tras una minor.

```json
"rbac": {
  "permissions": [
    { "key": "hub_fleet.instance.read",  "models": [{ "model": "Instance", "actions": ["index", "show", "export"] }] },
    { "key": "hub_fleet.instance.retire", "models": [{ "model": "Instance", "actions": ["retire"] }] }
  ],
  "roles": [
    { "key": "hub_fleet_viewer",   "label": "…", "permissions": ["hub_fleet.instance.read"] },
    { "key": "hub_fleet_operator", "label": "…", "permissions": ["hub_fleet.instance.read", "hub_fleet.instance.retire"] }
  ]
}
```

### B. Derivar los permisos del addon desde `capabilities`

- Kernel `manifest/from_v3.go`: `Permissions` = `db:read <t>` → `{model: t, scope: "read"}`,
  `db:write <t>` → `read` + `write` (+ `action` sobre los modelos propios); el esquema propio
  (`addon_<key>.*`) siempre. Sin cambio de schema (solo código) → no exige bump del hub, sí de ops.
- Ops `services/addon_permissions.go`: modo por variable `ADDON_PERMISSION_ENFORCE=audit|enforce`
  (por omisión `audit` una release: registra «habría denegado» con addon, modelo y scope); `enforce`
  después de revisar los logs de la flota. El caso «Permissions vacío = legacy» queda solo para v2/compiled.
- Riesgo: addons con interceptores sobre modelos ajenos que no declararon `db:write` → por eso el modo
  auditoría primero.

## 3. Pruebas

- Lint/validación: clave no declarada y no canónica → aviso/error; canónica `instances.index` → ok;
  `hub_fleet.instance.read` con modelo `Instance` → ok; `hub_fleet.instanse.read` → error.
- `from_v3`: capabilities → `Permissions` esperados; addon sin capabilities → solo su esquema.
- Ops: en `audit` no cambia ninguna respuesta y registra; en `enforce` un interceptor sobre un modelo no
  declarado se omite (comportamiento actual de `Allow` = false, `handlers/dynamic.go:3211-3226`).

## 4. Seguridad

Mínimo privilegio verificable: los roles que el addon trae concederán exactamente lo que dicen, y el
código del addon dejará de tener acceso implícito a toda la base vía interceptores.
