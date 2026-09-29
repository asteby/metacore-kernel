# Permisos y políticas de acceso

Cómo restringir, por modelo y por acción, quién puede usar el CRUD dinámico
(`/api/dynamic/:model`), cómo declarar modelos *singleton* (una fila por
organización) y cómo devolver errores de validación por campo desde modelos Go.

Complementa a [`permissions.md`](permissions.md) (capabilities de usuario y de
addon) y a [`dynamic-api.md`](dynamic-api.md) (contrato HTTP del CRUD).

---

## Contenido

- [El problema](#el-problema)
- [Política de acceso por modelo](#política-de-acceso-por-modelo)
  - [Declararla](#declararla)
  - [Reglas y presets](#reglas-y-presets)
  - [De dónde salen los roles: `RoleResolver`](#de-dónde-salen-los-roles-roleresolver)
  - [Qué rutas cubre](#qué-rutas-cubre)
  - [Respuesta 403](#respuesta-403)
- [Modelos singleton](#modelos-singleton)
- [Validación por campo desde modelos Go](#validación-por-campo-desde-modelos-go)
- [Resolvers del CRUD en `host.AppConfig`](#resolvers-del-crud-en-hostappconfig)
- [Compatibilidad](#compatibilidad)

---

## El problema

`host.NewApp` monta el CRUD dinámico para todo modelo registrado. Sin
`PermissionStore`, cualquier usuario autenticado puede crear, editar y borrar
filas de cualquier modelo **dentro de su organización**. Con un
`PermissionStore`, el rol `owner` es super-rol por defecto y pasa todas las
capabilities.

En un storefront cada comprador recibe una organización propia con rol
`owner`, así que podía crear promociones, reseñas o listings que luego la
tienda leía. La política de acceso por modelo cierra ese hueco de forma
declarativa, sin quitar los modelos del CRUD dinámico.

## Política de acceso por modelo

### Declararla

Hay tres formas. Si un modelo tiene varias, gana la primera de esta lista:

1. `dynamic.Config.AccessPolicyResolver` (o `host.AppConfig.AccessPolicyResolver`)
   para modelos que no están en el registro Go, como los de addons.
2. Al registrar: `app.RegisterModel(key, factory, host.WithAccess(policy))`,
   que llama a `modelbase.SetAccessPolicy`.
3. En el propio modelo, con la interfaz opcional `modelbase.HasAccessPolicy`:

```go
func (Promotion) DefineAccess() modelbase.AccessPolicy {
    return modelbase.AccessPublicReadStaffWrite("store.admin")
}
```

### Reglas y presets

```go
type AccessPolicy struct {
    List, Get, Create, Update, Delete *AccessRule
    Default *AccessRule // para las acciones sin regla propia
}

type AccessRule struct {
    Public       bool     // cualquier usuario autenticado
    Roles        []string // alguno de estos roles
    Capabilities []string // alguna de estas capabilities (permission.Service)
}
```

- Si la regla de la acción es `nil`, se usa `Default`. Si `Default` también es
  `nil`, esa acción no tiene restricción de política (comportamiento previo).
- Una regla sin cláusulas (`modelbase.AccessNobody()`) no deja pasar a nadie
  por HTTP. El código del servidor sigue teniendo sus caminos: hooks,
  endpoints propios y `dynamic.NewSystemCaller`, que ignora las políticas.
- Los roles se comparan sin distinguir mayúsculas y **sin super-roles**: `owner`
  solo coincide si la regla lo nombra.
- Las capabilities se consultan en el `permission.Service` con su semántica
  completa, super-roles incluidos (por defecto `owner` tiene `*`). Si no hay
  servicio de permisos, la cláusula nunca coincide. En un storefront donde
  todos son `owner`, usa **roles** (vía `RoleResolver`), o configura
  `permission.Config.SuperRoles`.
- La política **solo restringe**. Una petición debe cumplir la política **y**
  la capability que el CRUD ya exigía (`<model>.read|create|update|delete`).

| Preset                                          | list / get     | create / update / delete |
| ----------------------------------------------- | -------------- | ------------------------ |
| `modelbase.AccessPublicReadStaffWrite(roles...)` | autenticado    | `roles`                  |
| `modelbase.AccessStaffOnly(roles...)`           | `roles`        | `roles`                  |
| `modelbase.AccessReadOnly()`                    | autenticado    | nadie                    |

Constructores de reglas: `AccessPublic()`, `AccessRoles(...)`,
`AccessCapabilities(...)` y `AccessNobody()`, más los combinadores
`rule.OrRoles(...)` y `rule.OrCapabilities(...)`.

> "Público" significa *cualquier usuario autenticado*, dentro del alcance de
> tenant del CRUD (`organization_id` del usuario). El CRUD dinámico no sirve
> lecturas anónimas ni entre organizaciones. Un catálogo que ven visitantes
> anónimos se sirve desde los endpoints propios de la app.

### De dónde salen los roles: `RoleResolver`

El motor compara cada regla con la unión de:

1. el rol del JWT (`AuthUser.GetRole()`);
2. los roles de un principal `modelbase.RolesProvider`. `host.AppConfig.RoleResolver`
   lo construye con `modelbase.WithRoles`;
3. `dynamic.Config.ActorRolesResolver`, el mismo resolver que usan las
   aprobaciones.

```go
app := host.NewApp(host.AppConfig{
    DB: db, JWTSecret: secret,
    RoleResolver: func(c fiber.Ctx) []string {
        if isAdminEmail(auth.GetEmail(c)) || auth.GetOrganizationID(c) == houseOrgID {
            return []string{"store.admin"}
        }
        return nil
    },
})
```

`RoleResolver` es perezoso: se ejecuta como mucho una vez por petición, y solo
cuando hay que evaluar una regla de política. Si un hook hace type-assert sobre
`*modelbase.BaseUser`, con `RoleResolver` recibe el envoltorio, así que debe
usar la interfaz `AuthUser`, o `Unwrap()`.

Si la app arma su propio `dynamic.Handler`, puede envolver al usuario en su
`UserResolver` con `modelbase.WithRoles(user, func() []string { ... })`.

### Qué rutas cubre

| Acción   | Rutas                                                                                   |
| -------- | --------------------------------------------------------------------------------------- |
| `list`   | `GET /dynamic/:model`, `/aggregate`, `/facets`, `/export`, `GET /options/:model`, `GET /search/:model` |
| `get`    | `GET /dynamic/:model/:id`, `GET /dynamic/:model/current`                                 |
| `create` | `POST /dynamic/:model`, `POST /dynamic/:model/import`                                    |
| `update` | `PUT /dynamic/:model/:id`, `POST /dynamic/:model/:id/action/:key`                        |
| `delete` | `DELETE /dynamic/:model/:id`                                                             |

Para mostrar u ocultar botones, o en endpoints propios, usa
`svc.Can(ctx, user, model, modelbase.AccessCreate)`. No tiene efectos
secundarios.

### Respuesta 403

```json
{
  "success": false,
  "code": "access_denied",
  "model": "promotions",
  "action": "create",
  "message": "access denied: create on promotions is not allowed for this user"
}
```

En Go, el error es un `*dynamic.AccessDeniedError` que envuelve `dynamic.ErrForbidden`.

## Modelos singleton

Un modelo singleton guarda **una fila por organización**, como la configuración
de la tienda. Para declararlo:

```go
type StoreSettings struct {
    modelbase.BaseUUIDModel
    modelbase.SingletonModel // marcador sin campos: GORM y JSON lo ignoran
    StoreName string `json:"store_name"`
    Currency  string `json:"currency"`
}

// Opcional: valores de la primera fila.
func (StoreSettings) SingletonDefaults(ctx context.Context) map[string]any {
    return map[string]any{"currency": "MXN"}
}
```

También se puede usar `app.RegisterModel(key, f, host.AsSingleton())`, que
llama a `modelbase.MarkSingleton`, o `TableMetadata.Singleton = true`.

Con esto:

- `POST /dynamic/:model` responde **409** si la organización ya tiene fila:
  `{"success":false,"code":"singleton_exists","data":{"id":"…"}}`. Antes
  respondía 500 por el índice único, o creaba una segunda fila.
- `GET /dynamic/:model/current` devuelve la fila de la organización. Si no
  existe y el usuario puede crearla, la crea con `SingletonDefaults` y responde
  `meta.persisted: true`. Si no puede crearla, responde los defaults **sin
  guardarlos**, con `meta.persisted: false`.
- `PUT /dynamic/:model/current` hace upsert: actualiza la fila o la crea sobre
  los defaults. Cada camino aplica su propia autorización.
- La metadata servida lleva `"singleton": true`, así que la UI muestra un
  formulario de ajustes en lugar de una tabla.

Si dos peticiones materializan la fila a la vez, la segunda recupera la fila
de la primera (409 interno → lectura). Para una garantía dura, crea un índice
único parcial: `UNIQUE (organization_id) WHERE deleted_at IS NULL`.

## Validación por campo desde modelos Go

Todas estas vías responden **422** con el mismo contrato que la validación
declarativa:

```json
{"success": false, "message": "validation.failed",
 "errors": {"discount": [{"code": "invalid", "message": "must be between 0 and 100"}]}}
```

`FieldError` ahora lleva un `message` opcional. La validación declarativa del
kernel lo deja vacío, y el SDK sigue localizando `code`.

1. **El modelo se valida a sí mismo.** Implementa `modelbase.Validatable`
   (`Validate() map[string]string`) o `modelbase.WriteValidator`
   (`ValidateWrite(ctx, op string) error`, con `op` igual a `"create"` o
   `"update"`). El CRUD los llama justo antes de escribir, sobre la instancia
   completa: en un update, la fila guardada con los cambios aplicados.
2. **Desde un hook** o un endpoint propio, devuelve `modelbase.FieldErrors{"campo": "mensaje"}`
   o el acumulador ahora público:

   ```go
   ve := dynamic.NewValidationError()
   ve.AddMessage("title", "ya existe una promoción con ese título")
   ve.Add("stars", validate.CodeMax, map[string]any{"max": 5})
   return ve.Err() // nil si no hay errores
   ```
3. **Reglas de la metadata Go.** `FieldDef.Validation` es un
   `*modelbase.ValidationRule` estructurado (`Regex`, `Min`, `Max`, `Custom`).
   En JSON acepta el string heredado (`"required|min:2"`). Hay helpers:
   `modelbase.Range(1, 5)`, `MinValue`, `MaxValue`, `Pattern(re)`,
   `.WithPattern()` y `.WithCustom()`. Con
   `host.AppConfig.ValidateModelMetadata: true`, o con
   `dynamic.MetadataValidationSchema(meta)`, el servidor aplica `Required`, las
   `Options` de un select, el tipo `number` y `Validation` del `FieldDef`. Si
   un campo no declara `Validation`, se usa la `ColumnDef.Validation` de la
   misma clave, así que el formulario no tiene que fusionar nada.

## Resolvers del CRUD en `host.AppConfig`

Campos nuevos, todos opcionales:

| Campo                      | Efecto                                                             |
| -------------------------- | ------------------------------------------------------------------ |
| `RoleResolver`             | Roles de plataforma adicionales para las políticas                 |
| `AccessPolicyResolver`     | Políticas de modelos fuera del registro Go                         |
| `Scoper`                   | `dynamic.TenantScoper` propio (por defecto `organization_id`)      |
| `ValidationSchemaResolver` | Reglas declarativas por columna (manifest)                         |
| `ValidateModelMetadata`    | Aplica también las reglas de la metadata Go (va detrás del anterior) |
| `CustomValidatorResolver`  | Validadores con nombre (`ValidationRule.Custom`)                   |
| `ConfigureDynamic`         | Recibe el `*dynamic.Config` antes de `dynamic.New` (cualquier otro resolver) |

## Compatibilidad

Un modelo sin política, sin marcador singleton y sin `Validate` se comporta
exactamente como antes. `RegisterModel` añade un parámetro variádico de
opciones, así que las llamadas existentes compilan sin cambios. Las rutas
`/current` solo responden para modelos singleton; para el resto dan 404.
