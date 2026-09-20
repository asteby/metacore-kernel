# RFC: contexto de ejecución para addons wasm (`ctx_get`) y folios en `data_mutate`

- Estado: implementado en el PR que trae este archivo (kernel), pendiente de tag + bumps.
- Fecha: 2026-09-20
- Motivación estratégica: un addon wasm debe ser ciudadano de primera como un módulo
  Odoo (`env.user`, `env.company`, secuencias automáticas). Ver PROPUESTA-NIVEL-ODOO.md
  ("Odoo con sandbox real").

## 1. Problema (hallazgos del ensayo 7Leguas→Pitsline)

| # | Síntoma | Causa raíz |
|---|---------|------------|
| a | `inventory.approve_adjustment` deja `approved_by` NULL | el guest no sabe quién ejecuta la acción |
| b | impuesto 0 en facturación (F-11) | `tax_rate`/`tax_included` viven en `organizations`; `data_query` exige `organization_id` y el guest no puede leerla |
| c | `currency_code` vacío en anticipos/wallet | la moneda de la org es inaccesible al guest |
| d | folios vacíos en `create_work_order`, `create_sales_order`, venta creada por `deliver` (N3-07); `rma_number` resuelto a mano con `sequence_next` | `data_mutate` create no ejecuta `assignSequences`; sólo `POST /data` (`dynamic.Service.Create`) lo hace |

(a), (b), (c) son la misma carencia: el guest no tiene "entorno". (d) es una asimetría entre
dos caminos de escritura que deberían ser equivalentes.

## 2. Diseño mínimo

### 2.1 Import de sólo lectura `ctx_get`

`metacore_host.ctx_get(reqPtr, reqLen) -> i64` (ABI 1.11, envelope v1 `{success,data,meta}`).
Respuesta:

```json
{"org_id":"…","user_id":"…|null","user_email":"…","roles":["admin"],
 "org":{"currency_code":"MXN","tax_rate":16,"tax_included":true,"locale":"es-MX","timezone":"America/Mexico_City"}}
```

Petición opcional `{"scopes":["user","org_config"]}` para acotar (vacía = todo lo declarado).

**Capabilities** (nuevas en el enum cerrado de manifest v3; `target` ignorado, convención `"*"`):

- `ctx:user` → `user_id`, `user_email`
- `ctx:roles` → `roles`
- `ctx:org_config` → `org.*`

Mínimo privilegio:

1. Sin capability declarada el guest sólo recibe `org_id` (que ya conoce). No hay grant implícito
   ni comodín entre kinds.
2. Pedir explícitamente un scope no declarado da `forbidden` (fallo ruidoso en el primer test, no un
   null misterioso en prod). Scope desconocido → `invalid_request`.
3. Gate duro (como `connector_get`), independiente del modo shadow del enforcer: el import es nuevo,
   ningún guest legacy depende de permisividad.
4. `org` es una **lista cerrada** (`currency_code`, `tax_rate`, `tax_included`, `locale`,
   `timezone`), no una vista de la fila `organizations`: nunca `stripe_customer_id`, `billing_*`,
   `fiscal_data` ni columnas arbitrarias. Ampliarla exige cambio de kernel + RFC.
5. `user_id` sale del actor ya presente en el `context` (`dynamic.WithActorID`, el mismo valor que
   `data_mutate` estampa en `created_by_id`); el guest no lo elige. Trabajo sin actor (entrega de
   evento desatendida) → `user_id: null`, nunca un usuario inventado.
6. Puerto del embedder (`Host.WithContextProvider`) recibe sólo los scopes pedidos *y* concedidos para
   que no consulte lo demás.

`tax_rate`/`tax_included` son punteros: `null` = "la org no lo configuró", `0` = exenta.

### 2.2 Folios en `data_mutate` / `data_batch` create

`Host.WithSequenceStamp(dynamicService.StampSequences)`: en cada `create`, dentro de la transacción y
antes del INSERT, se rellenan las columnas ligadas a una secuencia declarada que el guest dejó vacías.

- Misma semántica que `POST /data`: valor explícito gana (no consume contador) → los guests que ya
  llaman `sequence_next` (`rma_number`) o importan folios históricos no cambian.
- **Idempotente**: el UPSERT del contador comparte transacción con el INSERT. Si el create falla o se
  reintenta con el `id` determinista y choca con la PK, el rollback devuelve el contador: sin folios
  quemados y un replay no avanza la serie.
- `scope: branch`: el ABI no lleva sucursal; se toma de `branch_id` de la propia fila; sin él, cae a
  ámbito org (mismo fallback que un usuario sin sucursal en `POST /data`).
- Clave = `model` (ModelKey) del request, igual que `sequence_next`.
- Sin `WithSequenceStamp` el comportamiento es el previo (aditivo).

## 3. Alternativas evaluadas

**A. Inyectar el contexto en el envelope del evento / payload de la acción** (rechazada).
Pro: cero cambios de ABI. Contras: (1) sólo cubre invocaciones con envelope; los caminos
`Host.InvokeFor` directos, lifecycle hooks y `data_batch` en cascada no tienen; (2) el contexto viaja
*siempre*, sin capability: se filtraría email/roles a cada guest aunque no los use, violando mínimo
privilegio; (3) cada addon re-parsea un contrato ad hoc; (4) los datos viajan congelados al inicio
(un rol cambiado durante una cadena de eventos no se refleja) y engordan cada evento en el outbox;
(5) los eventos son el contrato de dominio, no un lugar para config de org.

**B. Permitir a `data_query` leer `organizations`** (rechazada). Abre columnas arbitrarias (incluye
secretos) y rompe la invariante "toda lectura filtra `organization_id = ?`" (aquí la PK es `id`).

**C. Que cada addon reciba `tax_rate` como setting de instalación** (rechazada). Duplica la fuente de
verdad y se desincroniza al editar la org.

**D. Import `ctx_get` con capability por slice** (elegida): bajo demanda, auditable, versionado por
ABI, sin costo para guests que no lo usan, y el mismo mecanismo cubre futuros campos.

Sobre folios: alternativa "obligar a cada guest a llamar `sequence_next`" es la práctica actual y la
causa de N3-07 (se olvida). Estampar en el host hace que ambos caminos de escritura sean equivalentes.

## 4. Compatibilidad hacia atrás y versión del ABI

- ABI 1.10 → **1.11**, estrictamente aditivo. Un guest que no importa `ctx_get` no ve diferencia.
  Un guest compilado contra 1.11 en un host viejo falla al instanciar (import faltante): por eso el
  addon debe declarar `kernel: ">=X"` con el tag que traiga esto, y la instalación lo rechaza en hosts
  anteriores.
- Los kinds `ctx:*` entran al enum cerrado de `Capability.kind` del schema v3: **un manifest que los
  declare no valida en hubs/ops con kernel anterior** (doble validación, ver plan de bumps).
- `data_mutate` create: guests que ya pasan el folio → sin cambio. Guests que dejaban la columna vacía
  *a propósito* (folio nulo) pasarán a recibir folio: es la semántica de `POST /data`, y sólo aplica
  si la columna está ligada a una secuencia declarada.

## 5. Decisiones controvertidas (opción conservadora elegida)

1. **Exponer roles**: capability propia `ctx:roles`, separada de `ctx:user`. Sólo claves de rol
   (no permisos, no ids de grants). Si el hub decide no aprobar `ctx:roles` en first-party por
   política, el resto sigue funcionando. Para *autorizar* acciones el guest debería usar el sistema de
   permisos declarativo (rol×módulo×acción), no leer roles a mano; `ctx:roles` es para presentación y
   ramas de negocio no de seguridad.
2. **Email del usuario**: dato personal; va con `ctx:user`, no con `org_id`. Auditoría: el denegado de
   un scope pedido se registra (`audit ctx_get … decision=denied`).
3. **`locale`**: no todas las orgs lo tienen; devuelve `""`. No se inventa un valor por defecto en el
   kernel.
4. **Sucursal**: `branch_id` NO se expone en v1.11 (el actor no lo lleva por el ABI). Queda como
   ampliación posible (`ctx:user` + `branch_id`) cuando ops lo propague.

## 6. Puerto que debe implementar ops

`Host.WithContextProvider(fn)` en `backend/appserver/server.go`, junto a `WithSequenceNext`:

```go
w.WithContextProvider(func(ctx context.Context, orgID, userID uuid.UUID, want wasm.HostContextScopes) (*wasm.HostContext, error) {
    hc := &wasm.HostContext{}
    if want.OrgConfig {
        var o models.Organization
        if err := db.WithContext(ctx).Select("currency_code", "tax_rate", "tax_included", "timezone").
            First(&o, "id = ?", orgID).Error; err != nil { return nil, err }
        rate, incl := o.TaxRate, o.TaxIncluded
        hc.Org = wasm.OrgConfig{CurrencyCode: o.CurrencyCode, TaxRate: &rate, TaxIncluded: &incl, Timezone: o.TimeZone}
    }
    if (want.User || want.Roles) && userID != uuid.Nil {
        var u models.User
        if err := db.WithContext(ctx).Preload("Roles").Where("id = ? AND organization_id = ?", userID, orgID).
            First(&u).Error; err != nil { return nil, err }   // filtro de org: nunca cruzar tenants
        hc.UserEmail = u.Email
        for _, r := range u.Roles { hc.Roles = append(hc.Roles, r.Name) }
    }
    return hc, nil
})
w.WithSequenceStamp(dynamicHandler.SequenceStamp()) // adapta ensureDynamicCRUDService().StampSequences
```

Puntos a verificar en ops: (1) que el camino action→wasm ponga `dynamic.WithActorID(ctx, userID)`
(`approval_request` y `created_by_id` ya dependen de ello; si falta, `user_id` llega `null`);
(2) `organizations` no tiene columna de locale → `locale` vacío hasta que exista; (3) cachear por
(org,user) dentro de la invocación si un guest llama `ctx_get` en bucle.
Bloqueado hasta el tag de kernel (ops#… bump `metacore-kernel` en `backend/go.mod`).

## 7. Plan de bumps / orden de merge

1. **kernel**: merge del PR + tag (siguiente minor; incluye enum v3 y `StampSequences`).
2. **hub** (validador de manifest vendoriza el schema del kernel): bump al tag → sin esto un manifest con
   `ctx:*` es rechazado al publicar (regla "primitivo nuevo = 3 bumps"). Verificar que su lista de
   kinds aprobables first-party (auto-approve) incluya `ctx:*`.
3. **ops**: bump `backend/go.mod` + cablear `WithContextProvider` y `WithSequenceStamp` (§6) + tests
   de humo (approve_adjustment con `approved_by`, venta con impuesto de la org).
4. **addons**: recién entonces declarar `ctx:*` y `kernel >= tag`, y consumir `ctx_get` en inventory
   (`approved_by`), facturación (F-11), wallet/anticipos (moneda); quitar los `sequence_next` manuales
   redundantes. Con changeset, no bump manual. (Fuera del alcance de este PR: no se tocan addons.)

Los folios (2.2) sólo requieren pasos 1 y 3: no necesitan cambio de manifest ni de hub.

## 8. Pruebas

- `security/ctxcap_test.go`: mínimo privilegio de `CanReadContext`.
- `runtime/wasm/ctxget_test.go`: slices por capability, `forbidden` por scope no declarado, actor
  ausente → `user_id null`, sin provider/sin org, y un **guest wasm real** (módulo mínimo que importa
  `ctx_get`) de punta a punta vía `Host.InvokeFor`.
- `TestDataMutate_CreateStampsDeclaredSequences`: el INSERT lleva el folio estampado en la misma tx.
- `dynamic/sequence_test.go`: `StampSequences` numera igual que Create, valor explícito gana sin
  consumir contador, **rollback no quema folio**, scope branch por `branch_id` de la fila.
