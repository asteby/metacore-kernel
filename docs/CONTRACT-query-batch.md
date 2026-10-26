# Contrato del query batch (`/api/q`)

- Estado: **v1 vigente** (descrita tal como la implementa el código); **v2 propuesto** (sección final, aún sin implementar).
- Implementación de referencia: `backend/handlers/query_batch.go` en `asteby-hq/ops` y `packages/runtime-react/src/query-batch.ts` en `metacore-sdk`.
- Este documento es el contrato que citan `query_batch.go` y `docs/PLAN-VELOCIDAD-ABSURDA.md`. Hasta ahora no existía escrito en ningún repo.

## 1. Idea

Un addon (o el shell) describe qué necesita en una cadena y el servidor resuelve el orden: varias lecturas viajan en **una** petición HTTP, con permisos evaluados **por parte** y ETag **por parte**. Solo lecturas; sin expresiones, sin joins arbitrarios, sin escrituras.

## 2. Transporte

| Forma | Petición |
| --- | --- |
| GET | `GET /api/q?b=<plan>[&inm=<nombre>=<etag>;...]` |
| POST | `POST /api/q` con cuerpo `{"parts": ["..."], "inm": {"nombre": "etag"}}` |

- En POST las partes se unen con `;` y forman el mismo plan que `b`. Si el cuerpo no trae partes, se usa `b` de la query.
- `inm` del cuerpo y `inm` de la query se mezclan (la query gana).
- Plan vacío: `400 {"success":false,"message":"empty batch"}`.
- Existe además `TerminalBootstrap` (primer pintado del POS), que arma un plan fijo y lo ejecuta con el mismo motor.

## 3. Gramática del plan

```
plan   = parte { ";" parte }
parte  = [ nombre "=" ] token
nombre = [A-Za-z][A-Za-z0-9]*
token  = prefijo ":" especificación
```

Sin nombre, la parte recibe `p1`, `p2`, … (el primer libre). Un nombre duplicado es error (`duplicate part <n>`). Una parte solo se toma como `nombre=token` si lo de la derecha empieza con un prefijo conocido; si no, todo el trozo es el token (así `=` dentro de una query no se confunde con un nombre).

### 3.1 Prefijos

| Prefijo | Especificación | Devuelve |
| --- | --- | --- |
| `t:` | `Modelo[?query]` | Lista paginada (`data`, `meta`). Pasa por la compuerta de lista y el `kernelListEnvelope`. |
| `m:` | `Modelo` | Metadata de tabla (equivale a `/metadata/table/:model`). |
| `mm:` | `Modelo` | Metadata de modal (equivale a `/metadata/modal/:model`). |
| `o:` | `Modelo?field=&q=&limit=&filter_value=&ctx.<k>=` | Opciones de lookup (`data` = opciones, `meta` = `{type,count}`). |
| `s:` | `clave` | Ajustes de la org para la clave (`addonService.GetSettings`). |
| `r:` | `Modelo/<id>` | Un registro por id (UUID). |
| `i:` | `addon/<lang>` | Mensajes i18n del addon (`lang` = `xx` o `xx-YY`). |

Un prefijo desconocido en `execBatchPart` responde la parte con `success:false, message:"prefix not in this slice"`. Los nombres de modelo, claves y campos validan contra `^[A-Za-z][A-Za-z0-9_]*$`; un id de `r:` o un valor de filtro de `TerminalBootstrap` contra `[A-Za-z0-9_-]{1,80}`.

### 3.2 Filtros en `t:`

- Parámetros reservados (no son filtros): `page, per_page, limit, sortBy, order, search, q, skip_refs, fields, include, count, with, group_by`.
- Cualquier otra clave `col` (o `rel.col`) que cumpla `^[a-z_][a-z0-9_]*(\.[a-z_][a-z0-9_]*)?$` se reescribe a `f_col`. Si ya viene `f_col` explícito, gana el explícito. Los valores usan los operadores de la lista directa (`eq:`, `in:a,b`, …).
- El filtro de organización lo pone el servidor (`stampListScope`): el cliente no lo decide; un `f_organization_id` enviado se descarta.

### 3.3 Referencias `@nombre.campo`

- `@p1.id` se sustituye por los valores de la columna `id` de las filas de la parte `p1`, **únicos y en orden**, separados por coma. Ej.: `p2=t:Stock?product_id=in:@p1.id`.
- Regex: `@([A-Za-z][A-Za-z0-9]*)\.([A-Za-z_][A-Za-z0-9_]*)`. Se aplican a todo el token (en v1 no se restringen a posiciones de valor; v2 sí, ver §8).
- Dependencias: cada referencia crea una arista. Las partes se agrupan en **niveles** (orden topológico por rondas; las partes de un nivel no dependen entre sí). Referencia a una parte inexistente: `400 unknown part <n>`; ciclo: `400 cycle in batch`.
- Si la parte referida falló (`success:false`), la dependiente falla con `part <n> is not available`.
- Si la referencia no devuelve ids y el filtro queda `in:` vacío, la parte se responde `success:true, data:[], meta:{total:0}` **sin consultar**.
- En v1 las partes se ejecutan secuencialmente, nivel por nivel, en el orden del plan.

## 4. Respuesta

```json
{
  "success": true,
  "parts": {
    "p1": { "success": true, "data": [...], "meta": {...}, "etag": "\"qh-1a2b…\"" },
    "p2": { "success": true, "not_modified": true, "etag": "\"qh-…\"", "meta": {...} },
    "p3": { "success": false, "message": "forbidden" }
  }
}
```

- El sobre es `200` con `success:true` siempre que el plan sea válido, aunque partes fallen. Un plan inválido (vacío, duplicado, ciclo, parte desconocida, más de 32 partes) responde `400 {"success":false,"message":...}` sin partes.
- Cada parte: `success`, `data?`, `meta?`, `message?`, `etag?`, `not_modified?`. Los campos vacíos se omiten.

### 4.1 Errores parciales

Una parte con error **no** falla el lote. El `message` es el del gate o del servicio; no hay código numérico por parte en v1.

### 4.2 403 por mensaje

v1 no lleva estado por parte. Una denegación (permiso, addon no instalado, org requerida) se expresa solo como `success:false` con `message`. Distintas denegaciones usan mensajes como `forbidden`, `Se requiere una organización activa` o `El módulo '<x>' no está instalado.`. El cliente (`batchGet`) trata como 403 el mensaje que coincide con `/forbidden|permiso/i`: lanza un error con `status: 403` (no hace fallback). Cualquier otro `success:false` cae al GET directo equivalente. Es un contrato frágil por basarse en texto; v2 lo reemplaza por `status` numérico.

### 4.3 Compuertas por prefijo (v1)

- `t:`: `checkAddonAccess`, `checkInboxListPermission`, lectura del addon (`addonReadAllowed`), compuerta de delegación de la lista (`listDelegationGateQuery`; si la lista sigue en la ruta heredada, la parte responde `list stays on the legacy path` y el cliente cae al GET), alcance de lista por org/sucursal y vistas bloqueadas. El resultado de las compuertas que dependen de la **forma** del filtro (no de los ids dentro de `in:`) se cachea por `(org, usuario, forma)`; solo se cachean denegaciones 403.
- `m:`/`mm:`: acceso del addon, metadata del idioma de la petición, contextualización por org, filtro de acciones por addon (solo tabla) y permisos `canCreate/canEdit/canDelete/canExport/canImport` + filtro de acciones por permiso.
- `o:`: permiso de lookup (con excepción para consumidores de referencias con `field=id`).
- `r:`: acceso del addon, permiso de fila, lectura del addon, compuerta de delegación del `show`.
- `s:`: requiere org y servicio de addons.
- `i:`: el addon debe estar habilitado para la org.

Nota: `m:`/`mm:` **no** son idénticos a la ruta directa (idioma, permisos, proyecciones). Esa divergencia es parte del problema que v2 corrige (ver ADR).

## 5. ETags

- Cada parte con datos lleva `etag = "qh-<fnv64a hex>"`: hash del **contenido** (semilla + JSON de `data`/`meta`). Un `etag` basado en conteo o longitud ya no existe: una edición del mismo largo devolvía `not_modified` con datos viejos.
- El cliente envía `inm` (`{nombre: etag}`). Si la parte se ejecutó con éxito y su `etag` coincide, la respuesta es `{success:true, not_modified:true, etag, meta}` **sin `data`**. La parte igual se ejecuta (el ahorro es de bytes, no de consultas).
- Formato en query: `inm=p1="qh-…";p2=…` (comillas opcionales; se normalizan).
- El cliente solo confía en etags `"qh-…"` para filas y opciones; para `m:`, `mm:` e `i:` confía en cualquier etag.
- Semillas: `t:` = modelo; `m:` = `m:<Modelo>`; `mm:` = `mm:<Modelo>`; `o:` = `o:<Modelo>.<campo>`; `r:` = `r:<Modelo>/<id>`; `i:` = `i:<addon>/<lang>`; `s:` = clave.

## 6. Límites v1

| Límite | Valor |
| --- | --- |
| Partes por lote | 32 (`batch too large`, 400) |
| Ventana de coalescing del cliente | 8 ms |
| TTL de partes en memoria (cliente, solo `m:`, `mm:`, `i:`) | 30 s |
| TTL de entidades `in:` (cliente) | 30 s |
| Profundidad, tamaño de cuerpo, plazo total | sin límite explícito |
| Concurrencia | ninguna: secuencial |

## 7. Cliente (`runtime-react/query-batch.ts`)

- Encola tokens y los envía juntos en `POST /q` tras 8 ms con nombres `q1…qN` por token distinto.
- Cachea `m:`, `mm:` e `i:` en memoria 30 s (clave: org del `localStorage.auth_user` + token). Filas, registros y opciones siempre viajan; el `inm` evita reenviar bytes.
- Caché por entidad para `t:` con `in:` concreto y sin `page=`: pide solo los ids que faltan y mezcla.
- `invalidateQueryBatchData()` tras cada mutación (conserva metadata e i18n); `invalidateQueryBatchAll()` cuando cambia el epoch (addon instalado/actualizado/habilitado/deshabilitado).
- Si `/q` responde 404/405 el cliente queda marcado como no soportado y usa GET directo. Cualquier parte fallida (salvo 403) cae al GET original.
- La caché de cliente **no separa usuario ni idioma** (solo org): limitación conocida que v2 corrige.

## 8. v2 propuesto

> Diseño aprobado para implementar; nada de esto existe aún. Debe ser **aditivo**: un servidor v2 sigue atendiendo el sobre v1 sin cambios.

### 8.1 Sobre

```
POST /api/q
{ "v": 2, "plan": "shell.boot", "args": { ... }, "parts": ["..."], "inm": { "n": "etag" } }

→ { "success": true, "v": 2, "epoch": "<n>",
    "parts": { "n": { "success": true, "status": 200, "data": ..., "meta": ..., "etag": "...", "retry": false, "ms": 3.2 } } }
```

- `plan` es opcional y nombra una receta por ruta (§8.4); `args` son sus argumentos validados. `parts` sigue siendo la forma libre de v1 (el cliente puede mezclar ambas; el servidor expande el plan y concatena).
- Cada parte lleva `status` numérico, `retry` (si reintentar tiene sentido) y `ms` (tiempo de ejecución de la parte).
- `epoch` es el contador de cambios de catálogo de la org (addons, metadata); el cliente invalida cachés al verlo cambiar.

### 8.2 Estados por parte

| Código | Significado |
| --- | --- |
| 200 | Datos. |
| 304 | `etag` coincide con `inm`; sin `data`. |
| 400 | Token o argumentos inválidos. |
| 403 | Sin permiso o addon no instalado (reemplaza la inferencia por mensaje de v1). |
| 404 | Modelo o registro inexistente. |
| 413 | Respuesta excede 2 MB. |
| 424 | Dependencia fallida (la parte referida no fue 200). |
| 429 | Límite de tasa de la org/usuario. |
| 500 | Error interno. |
| 503 | Dependencia no disponible (`retry: true`). |
| 504 | Plazo de la parte o del lote agotado (`retry: true`). |

### 8.3 Metaniveles y tokens nuevos

Los datos dependen de metadata y la metadata de un índice. v2 lo hace explícito, en este orden de niveles:

```
q:caps → mi: → m:/mm: → datos
```

- `q:caps`: capacidades del servidor (versión `v`, prefijos, planes y límites). Permite negociar sin probar.
- `mi:`: índice de metadata de la org: por cada modelo visible, su ETag. Con ETag por clave el cliente decide qué `m:`/`mm:` pedir (reemplaza el N+1 de `/metadata/all`).
- `m:` / `mm:` como en v1 pero por la misma compuerta que la ruta directa.

Tokens nuevos:

| Token | Contenido |
| --- | --- |
| `mi:` | Índice de metadata de la org con ETag por clave. |
| `me:` | Metadata del entorno/org (países, moneda, contexto de org). |
| `p:` | Permisos efectivos del usuario en la org. |
| `nav:` | Navegación/menú del usuario. |
| `ad:` | Addons instalados/habilitados de la org. |
| `i:*/<lang>` | i18n de todos los addons habilitados para un idioma. |
| `x:<clave>` | Extensión registrada por un addon (clave en lista cerrada). |

### 8.4 Planes por ruta

Recetas con nombre, con `args` validados contra un esquema; el servidor las expande en partes y niveles:

| Plan | Para |
| --- | --- |
| `shell.boot` | Arranque del shell (sesión, org, permisos, navegación, addons, i18n, metadata de entorno). |
| `model.list` | Lista de un modelo: metadata de tabla + primera página + opciones de filtros. |
| `model.form` | Formulario/modal: metadata de modal + registro + opciones necesarias. |
| `dashboard.home` | Inicio: widgets y sus datos. |
| `integrations.installed` | Integraciones instaladas y su estado. |

### 8.5 Límites v2

| Límite | Valor |
| --- | --- |
| Partes tras expandir planes | 64 |
| Profundidad de dependencias | 4 niveles |
| Cuerpo de la petición | 16 KB |
| Valores por `in:` | 500 |
| Respuesta | 2 MB (excedida: 413) |
| Plazo total del lote | 3 s |
| Plazo por parte | 1,5 s |
| Concurrencia | 4 partes por nivel |

### 8.6 Seguridad

- **Una compuerta por recurso**, compartida con la ruta directa: `m:`/`mm:`/`t:`/`r:`/`o:` llaman al mismo código de autorización y proyección que su GET. Ningún atajo propio del batch.
- **Suite de conformidad byte a byte**: para cada recurso, la parte del batch y la ruta directa (mismo usuario, org e idioma) deben producir el mismo `data`. Falla el CI si divergen.
- **Falla cerrado**: ante error de la compuerta o dato ambiguo la parte es 403/500, nunca datos.
- **Referencias `@`** solo en posiciones de valor de filtro y solo hacia partes con estado 200; en otro caso la parte es 424.
- **La org sale solo del JWT/`Locals`**; nunca de `args`, tokens ni cabeceras del cliente.

### 8.7 Compatibilidad y versionado

- `v` sube **solo** con cambios de semántica; añadir prefijos, planes o campos es aditivo y se descubre con `q:caps`.
- Un servidor v1 ignora `v`, `plan` y `args` (campos desconocidos): el cliente detecta la ausencia de `v` en la respuesta y usa solo partes v1.
- Fallback: si `/q` o una parte falla de forma no recuperable, el cliente cae al GET directo equivalente (comportamiento actual).
