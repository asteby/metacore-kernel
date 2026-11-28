# RFC 0008 — Canales de un contrato: despacho de operaciones y registro declarativo de proveedores

- **Estado:** propuesto. Solo diseño: este documento no cambia código.
- **Fecha:** 2026-11-24
- **Repos afectados:** `metacore-kernel` (capability, runtime/wasm, manifest v3), `ops` (host), `addons` (`link_inbox`, `customer_support`, `connector_whatsapp`).
- **Relacionado:** `docs/capabilities-commerce.md` (contratos y selección), `manifest/v3/role_validate.go` (R-A..R-D), `capability/declared.go`, `docs/spec/v3/slots.md`, RFC 0005 (Loom: contribuciones de UI).
- **Primer consumidor:** addons `integrations/link-inbox/docs/CANALES.md` (asteby-hq/addons#2984) (contrato `inbox.channel` v1) y `packages/customer_support/docs/INBOX.md`.

## 0. Resumen

El patrón base + sucesores del kernel ya modela "un addon base con canales": el Inbox (`link_inbox`,
rol `contract`) declara `contracts: [{key: "inbox.channel", writable_models, states, selection:
{scopes: ["channel"]}}]` y cada canal (`customer_support`, mañana correo, formularios, tickets de
terceros, WhatsApp) declara `metadata.role: "provider"` + `implements`. La validación (R-B
escritura limitada a `writable_models`, R-D sin navegación propia) funciona hoy.

Faltan dos piezas para que un canal sea **solo declarativo**:

1. **Despacho de `operations` de un contrato declarado.** `capability_query` corre solo contratos
   `query` del registro del kernel (`not_query` para acciones) y no conoce los contratos
   declarados por addons. El Inbox no puede decir "entrega esta respuesta" a su proveedor: v1 usa
   el evento CRUD `link_inbox.Message.created` y cada canal filtra lo suyo.
2. **Lectura de los proveedores de un contrato por el dueño.** El Inbox necesita la definición de
   cada canal (identidades, campos de contexto, acciones). v1 la guarda como dato
   (`Device.definition`, escrito por el canal). No hay forma de declararla en el manifiesto del
   proveedor y leerla desde el dueño.

## 1. Propuesta

### 1.1 `implements[].config` (manifiesto, aditivo)

```jsonc
"implements": [{
  "contract": "inbox.channel", "version": "^1.0.0", "scope": ["channel"],
  "config": { "$ref": "inbox_channel.json" }        // o el objeto en línea
}]
```

- El contrato del dueño declara `config_schema` (ruta a un JSON Schema de su paquete). El
  validador del kernel valida `config` contra él al instalar (como `props_schema` de
  `slot_kinds`).
- Público: viaja en el manifiesto; nunca secretos (igual que `slots[].props`).

### 1.2 `GET /api/metacore/contracts/{key}/providers` (host)

Devuelve, para la organización, los proveedores instalados y habilitados del contrato:
`[{addon, version, scope, config}]`. Lo usa el frontend del dueño (el Inbox) para dibujar los
canales. Reusa la resolución de instalación/habilitación de `capability.Select`. Cuando RFC 0005
(Loom) sirva `GET /api/ui/grafts`, esta lista puede ser una lente de solo lectura del seam
`link_inbox.channel`; el contrato de datos es el mismo.

### 1.3 `capability_invoke` para operaciones `kind: action` de contratos declarados (wasm ABI, aditivo)

```
capability_invoke(reqPtr, reqLen) -> i64
{"contract":"inbox.channel","operation":"deliver","provider":"customer_support","input":{…},"idempotency_key":"<Message.id>"}
```

- Requiere `capability:invoke inbox.channel.*` en el llamador y que `provider` implemente el
  contrato (`implements`) en la organización. Sin `provider` → `provider_required` (las
  operaciones de canal siempre van fijadas, como `PinRequired`).
- El proveedor declara el handler con `provides_capabilities[{key: "inbox.channel.deliver",
  handler}]` (R-C ya exige que esté dentro del contrato implementado).
- Contratos mutantes: `idempotency_key` obligatoria, plazo 30 s, resultado indeterminado si vence
  (mismas reglas que `capabilities-commerce.md`).
- Con esto el Inbox deja de depender del evento CRUD: `send_message` llama
  `deliver` al `owner_addon` del `Device`, y el canal ya no se suscribe a mensajes de otros.

### 1.4 Lint de roles con el catálogo

El informe de roles del repo de addons (`TestRoleLintReport`) marca como "tabla ajena" las
escrituras que el contrato abre (`writable_models`). Debe consultar el catálogo de contratos como
ya hace `CheckRoles` con `RoleCatalog`.

## 2. Compatibilidad

Todo es aditivo. Un canal v1 (evento CRUD + `Device.definition`) sigue funcionando; cuando el
host tenga 1.2/1.3, el dueño prefiere `config` y `capability_invoke` y conserva la ruta v1 una
versión menor para los canales que no se hayan actualizado.

## 3. Preguntas abiertas

- ¿`config` por instalación (ajustes) además de la del manifiesto? (p. ej. nombre visible del canal
  por organización). Propuesta: `provider_settings` del contrato, que ya existe.
- ¿El dueño puede vetar un proveedor (versión de `config` no soportada)? Propuesta: `version` del
  contrato + `config_schema` versionado; fuera de rango no se lista.
