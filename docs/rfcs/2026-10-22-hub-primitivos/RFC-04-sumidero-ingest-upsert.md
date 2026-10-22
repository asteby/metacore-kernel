# RFC-04: sumidero entrante «upsert into model» (`Model.ingest` + `do: "upsert:<Model>"`)

- **Prioridad:** P1 (criterio de salida de M1; M1 puede **empezar** con la API de datos y un token `sk_`).
- **Tamaño:** M-L (contrato S + runtime kernel M + ops S).
- **Depende de:** contrato v0.230.0; usa `WithProtectedWrite` de RFC-02 (mismo PR de runtime o el de
  RFC-02 antes).

## 1. Problema (evidencia)

- Un replicador externo (el núcleo de confianza alimentando `hub_fleet`, un ERP que empuja catálogo, un
  conector que sincroniza pedidos) hoy solo puede escribir por la API de datos con un token `sk_` [S]:
  busca por `f_<campo>`, decide si crea o edita y hace `POST`/`PUT /api/data/<Model>`. Problemas:
  - Dos llamadas no atómicas por fila (carrera si dos entregas llegan juntas), sin orden ni cursor: una
    entrega vieja que llega tarde **pisa** a una nueva.
  - El token `sk_` tiene alcance de API completa de datos de la org (`apitoken/doc.go:6-10`), no «solo
    este modelo, solo estas columnas».
  - Si las columnas de réplica se marcan `protected` (lo correcto, RFC-05), **la API de datos tampoco
    puede escribirlas**: `protected` no tiene excepción (`dynamic/validate.go:163-182`) [V]. Hoy hay que
    elegir entre réplica escribible por cualquier admin o réplica que el replicador no puede llenar.
- Los webhooks entrantes ya existen pero solo enrutan a código: `do` debe ser
  `^(wasm|webhook|compiled):.+$` (schema `InboundWebhook.do`; `manifest/validate.go:1074-1084`) [V]; en
  OPS `webhook:` y `compiled:` son `logSkipDispatcher` (`services/pipeline_runtime.go:165-171`) [V].
- El esquema `hmac-sha256` de entrada firma **solo el cuerpo**, sin timestamp ni nonce
  (`runtime/webhookin/webhookin.go:299-307`) [V]: una entrega capturada se puede reenviar para siempre.
- Piezas reutilizables [V]: la llave natural ya es un concepto del host (ops `models/natural_key.go`:
  primera columna `unique`, `seed.key` o el único índice único sin `where`); `dynamic.NewSystemCaller`
  (`dynamic/system_caller.go`) para escribir como sistema con `SystemActorID` fijo; `security.Signer`
  /`NonceCache` (RFC-01 §1); el montaje `POST /api/webhooks/:org/:addon/:webhook`
  (ops `appserver/server.go:1616`, `handlers/integrations.go:1154`).

## 2. Diseño

### 2.1 Manifest

```json
{
  "connectors": [{
    "key": "fleet_feed", "auth": "token",
    "credentials": [{ "key": "signing_secret", "type": "secret", "required": true }]
  }],
  "models": [{
    "key": "Instance",
    "table": "instances",
    "columns": [
      { "name": "instance_id", "type": "text", "not_null": true },
      { "name": "source_seq", "type": "bigint", "visibility": "list" },
      { "name": "license_status", "type": "text" },
      { "name": "last_seen_at", "type": "timestamp" },
      { "name": "notes", "type": "text" }
    ],
    "indices": [{ "name": "hub_fleet_instances_org_instance_uq",
                  "columns": ["organization_id", "instance_id"], "unique": true,
                  "where": "deleted_at IS NULL" }],
    "ingest": {
      "key": ["instance_id"],
      "cursor": "source_seq",
      "fields": ["name", "customer", "brand_name", "brand_color", "brand_logo_url", "vertical",
                 "deployment", "environment", "channel", "reported_version", "license_status",
                 "last_seen_at", "source_seq"],
      "exclusive": true,
      "on_delete": "soft",
      "max_batch": 500
    }
  }],
  "webhooks": [{
    "key": "fleet_changes",
    "path": "/fleet/changes",
    "verify": "metacore-hmac-v1",
    "secret_ref": "fleet_feed.signing_secret",
    "do": "upsert:Instance"
  }]
}
```

| Campo | Regla |
|---|---|
| `ingest.key` | Columnas de la llave natural (1..4). Debe existir un índice único que cubra `organization_id` + exactamente esas columnas (parcial `deleted_at IS NULL` permitido). Error de validación si no. |
| `ingest.cursor` | Opcional. Columna `bigint` o `timestamp(tz)` monotónica por fila. Si viene, una entrada con cursor `<=` al guardado se **omite** (`stale`). Sin cursor: última escritura gana. |
| `ingest.fields` | Opcional. Columnas que el sumidero puede escribir (además de la llave). Por omisión todas las no gestionadas. Nunca `id`, `organization_id`, `created_*`, `updated_*`, `deleted_*`, `generated`, `sequence`. |
| `ingest.exclusive` | Opcional, por omisión `false`. `true` = las columnas de `fields` se comportan como `protected` en la ruta genérica (UI, API de datos, import): **solo** el sumidero y las acciones declaradas (RFC-02, columna listada en `set`) las escriben. Columnas fuera de `fields` (p. ej. `notes`) siguen editables por el staff. Crear y borrar filas por la ruta genérica también se rechaza (`ingest_exclusive`). |
| `ingest.on_delete` | `soft` (por omisión: `deleted_at` + `deleted_by_id = SystemActorID`) \| `ignore`. Nunca borrado físico (la telemetría es evidencia, ADR §5.6). |
| `ingest.max_batch` | 1..1000, por omisión 500. Además, cuerpo ≤ 1 MiB. |
| `webhooks[].do: "upsert:<Model>"` | **Nuevo prefijo.** `<Model>` debe ser un modelo **propio** con bloque `ingest`. Exige `verify` distinto de vacío y de `addon` (el kernel verifica; no hay guest). |
| `webhooks[].verify: "metacore-hmac-v1"` | **Nuevo esquema** (sirve a cualquier ruta, no solo a `upsert`): `security.Signer.VerifyWithNonce` sobre `<ts>.<METHOD>.<path>.<nonce>.<sha256(body)>`, ventana 5 min, nonce obligatorio. |

### 2.2 Cuerpo y respuesta

```json
POST /api/webhooks/<org>/hub_fleet/fleet/changes
{
  "items": [
    { "op": "upsert", "cursor": 18231, "data": { "instance_id": "inst_123", "name": "Tienda A",
      "license_status": "active", "last_seen_at": "2026-10-22T18:00:00Z" } },
    { "op": "delete", "cursor": 18232, "key": { "instance_id": "inst_099" } }
  ]
}
```

```json
200 { "success": true, "data": {
  "applied": 1, "deleted": 1, "skipped_stale": 0, "rejected": 0,
  "errors": [],
  "high_water": 18232 } }
```

- `cursor` por ítem: si el modelo declara `ingest.cursor`, se escribe en esa columna y se usa para la
  regla de obsolescencia. `high_water` = máximo cursor aplicado **en este lote**.
- `GET` sobre la misma ruta (misma firma, cuerpo vacío) devuelve `{ "high_water": <max(cursor) de la
  org> }`: el replicador **reanuda** desde ahí tras un respaldo restaurado o una réplica borrada
  (ADR, plan de datos M1: «borrar la réplica y volver a llenarla desde cero»).
- Errores por ítem (`{index, code, field?}`: `validation`, `unknown_field`, `missing_key`,
  `constraint_violation`) no abortan el lote; un error de firma, de tamaño o de org sí (401/413/404).

### 2.3 Semántica de escritura

Por lote, una transacción; por ítem, un `SAVEPOINT`:

1. Descartar del `data` cualquier clave fuera de `key ∪ fields` → `unknown_field` (no se ignora en
   silencio).
2. `SELECT … FOR UPDATE` por `organization_id` (de la **ruta**, nunca del cuerpo) + llave, `deleted_at
   IS NULL`.
3. No existe → `dynamic.Service.Create` con `NewSystemCaller(org)`, `WithProtectedWrite(fields)` y
   procedencia `ingest:<addon>.<webhook>` (paquete `provenance`).
   Existe → si hay cursor y `nuevo <= guardado` → `skipped_stale`; si no → `Service.Update` igual.
4. Toda la validación del modelo aplica (`validation`, `constraints`, `rules`, `formulas`, `stage
   machine` si la llave toca la etapa) y se emiten los eventos canónicos `…created/updated/deleted` con
   actor sistema: las suscripciones y notificaciones del addon reaccionan a la réplica como a cualquier
   escritura.
5. `on_delete: soft` → `Service.Delete` (soft) con el mismo principal.

## 3. Cambios por repo

| Repo | Archivos | Cambio |
|---|---|---|
| kernel (contrato, v0.230.0) | `manifest/v3/types.go` (`Model.Ingest`, `IngestSpec`), schema ×2 (`Model.ingest`, enum `InboundWebhook.verify` + `metacore-hmac-v1`, patrón `do` + `upsert`), `manifest/v3/validate.go` (reglas §2.1, índice único que cubre la llave), `manifest/manifest.go` (`ModelDefinition.Ingest`), `manifest/from_v3.go`, `manifest/validate.go` (`validHookPrefixes` + `upsert`, `validateDoRef`) | Contrato en ambos validadores. |
| kernel (runtime) | `dynamic/ingest.go` (nuevo: `Service.Ingest(ctx, org, model, items, IngestOptions) (IngestResult, error)` y `HighWater`); `dynamic/validate.go` (`ingest.exclusive` → rechazo en la ruta genérica, salvo `WithProtectedWrite`); `runtime/webhookin/webhookin.go` (esquema `metacore-hmac-v1` con `NonceStore` inyectable; prefijo `upsert` resuelto por un `Dispatcher` que el host registra); `runtime/webhookin/ingest.go` (`IngestDispatcher{svc}` que decodifica el lote y llama a `Service.Ingest`) | Host-neutral. |
| ops | `services/pipeline_runtime.go` (`whDispatchers["upsert"] = kwebhookin.NewIngestDispatcher(kdynamicSvc)`; `NonceStore` en Postgres o Redis, no en memoria: hay varias réplicas de proceso); `handlers/integrations.go` (`GET` en la misma ruta para `high_water`; comprobar que el addon está **habilitado** para la org de la ruta antes de despachar [?]; límite de cuerpo 1 MiB para rutas `upsert`; rate limit por org+ruta) | Wiring. |
| hub / núcleo (M1) | replicador del núcleo: `GET /core/v1/fleet/changes?since=` → lotes firmados `metacore-hmac-v1` al sumidero; reanuda con `high_water` | Consumidor. |
| addons | `hub_fleet`: `ingest` + `webhooks[]` + columnas de réplica sin `readonly` suelto | Adopción. |

## 4. Compatibilidad

- Aditivo. Los `do` existentes no cambian. Un ops viejo con un manifest nuevo: `upsert:` no tiene
  despachador → con la Fase 0 de RFC-01 responde 501 (no falso éxito).
- `ingest.exclusive: false` (por omisión) no cambia la ruta genérica.
- La API de datos con `sk_` sigue funcionando para modelos sin `exclusive`: M1 puede arrancar así y
  migrar al sumidero sin cambiar el modelo (solo se añade el bloque y el webhook).

## 5. Pruebas

- Validación: llave sin índice único, cursor de tipo texto, `fields` con columna gestionada, `upsert:`
  hacia modelo ajeno o sin `ingest`, `verify` vacío con `upsert:`.
- Runtime: crear, actualizar, `stale` con cursor menor o igual, borrado soft, `unknown_field`, lote mixto
  con un ítem inválido (los demás se aplican), dos lotes concurrentes con la misma llave (sin duplicados
  gracias al `FOR UPDATE` + índice), `high_water` tras restaurar.
- Firma: válida, timestamp fuera de ventana, nonce repetido (rechazado aunque el cuerpo sea idéntico),
  ruta distinta con la misma firma.
- Exclusividad: `PUT /api/data/Instance {"license_status":"revoked"}` → 422 `protected_field` (el caso
  exacto del spike, G4); `PUT {"notes":"…"}` → 200; acción `set_fields` que lista `license_status` → 200.
- Aislamiento: el `organization_id` del cuerpo se rechaza; la org de la ruta sin el addon habilitado → 404.

## 6. Seguridad

- **Autenticación:** firma con ts + nonce + ruta + hash; secreto por conector, rotable sin republicar.
  Sin `verify` no hay `upsert` (regla de validación).
- **Replay:** nonce en almacén compartido; además el cursor hace idempotente el reenvío de un lote ya
  aplicado.
- **RLS:** la org sale de la ruta y de la instalación habilitada; el filtro por org es el de Go
  (`OrganizationScoper`), que sigue siendo la única defensa real (la RLS de Postgres es decorativa,
  ADR §5.3). Pruebas de aislamiento obligatorias (§5).
- **Alcance mínimo:** el sumidero escribe solo `key ∪ fields` de **un** modelo; sustituye al token `sk_`
  con alcance de toda la API de datos.
- **DoS:** tamaño de cuerpo, `max_batch`, rate limit y una transacción por lote con tiempo máximo.
