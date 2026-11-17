# Vectores de conformidad de genui

Casos JSON compartidos entre el validador TypeScript (este paquete) y el espejo
Go del kernel (`metacore-kernel/genui`, paquete `genui/v2`). Un vector que una
implementación no cumple rompe su CI: así no se desalinean los dos lados.

Contrato: `../schema/genui-v2.schema.json` (fuente única) y el ADR 0001 del SDK
(`docs/adr/0001-genui-ui-declarativa-universal.md`, §2.5-2.7).

## Formato

Un archivo por caso, `{ "description", "input", "options"?, "expect" }`.

| Carpeta | `input` | `expect` |
|---|---|---|
| `valid/` | documento genui/v2 (objeto) | `{ "ok": true }` |
| `invalid/` | documento (objeto o cadena JSON) | `{ "ok": false, "code": "genui.…" }` |
| `upgrade/` | documento genui/v1 | `{ "ok": true, "output": <v2> }` o `{ "ok": false, "code" }` |
| `downgrade/` | `{ "spec": <v2>, "caps"?: { "versions", "blocks"?, "verbs"? } }` | `{ "ok": true, "output": <v1 o v2> }` o `{ "ok": false, "code" }` |

`options.allowExternalLinks` (por defecto `false`) corresponde a la opción del
mismo nombre del validador. Los `code` son los de `GENUI_ERROR_CODES`
(`src/errors.ts`); la comparación es exacta, igual que `output` (igualdad
profunda de JSON). `downgrade` sin `caps` asume `{"versions":["genui/v1"]}`.

## Reglas para escribir y consumir vectores

- **Una sola violación por vector.** Un vector inválido debe romper exactamente
  una regla: con varias violaciones el código dependería del orden en que cada
  implementación recorre el documento. `src/conformance.test.ts` lo comprueba en
  parte: el mismo documento con las claves en orden inverso debe dar el mismo
  código, y cada código de `GENUI_ERROR_CODES` debe tener al menos un vector.
- **Orden de las fases** (el primer fallo gana; el kernel debe seguirlo):
  1. bytes UTF-8 del texto (`genui.limit.bytes`);
  2. anidamiento del texto JSON, sin parsear (`genui.limit.depth`, tope 64);
  3. `JSON.parse` (`genui.invalid_json`);
  4. recorrido iterativo del objeto: anidamiento, números no finitos como `1e400`
     (`genui.invalid_json`) y claves `__proto__`/`constructor`/`prototype`
     (`genui.forbidden_key`). Para un objeto (no texto) se trabaja sobre la
     COPIA `JSON.parse(JSON.stringify(v))`, que es la que se valida y se devuelve
     (getters, prototipos ajenos y `toJSON` no sobreviven);
  5. bytes UTF-8 del objeto serializado;
  6. es objeto y `version` (`genui.invalid_document`, `genui.unsupported_version`);
  7. esquema y reglas por posición, en este orden de claves: ordenadas
     **por bytes UTF-8** (equivale a puntos de código y es el `sort.Strings` de
     Go; NO el orden por unidades UTF-16 de `Array.prototype.sort()`, que difiere
     para caracteres fuera del plano básico frente a U+E000..U+FFFF; vector
     `invalid/key-order-utf8-bytes`), con `blocks` al final de la raíz (así `data`, `state` y
     `overlays` ya están validados cuando un bloque los referencia). Dentro de un
     objeto: `required`, luego cada propiedad;
  8. reglas globales: `submit.form` debe apuntar a un `form`/`wizard`.
- **Claves de prototipo**: nunca se aceptan, ni como dato. Las búsquedas por
  nombre (tipos de bloque, fuentes, overlays, códigos de límite) deben ser de
  claves propias. Además, ningún SEGMENTO (separado por `.`) de una ruta de
  vinculación (`bindingPath`), de `set.path` ni de una clave de campo/columna/
  entrada (`key`) puede ser `__proto__`, `constructor` ni `prototype`
  (`genui.forbidden_key`, antes de comprobar el patrón). El esquema lo repite
  con un lookahead negativo y la marca `x-no-proto`; el kernel Go (RE2, sin
  lookahead) debe hacer la comprobación por segmentos en código.
- **`x-no-proto` por segmentos**: la marca `x-no-proto` (en un valor string o en
  `propertyNames`) y su lookahead `(?!(?:.*[.:@])?(?:__proto__|constructor|prototype)(?:[.:@]|$))`
  parten el texto con `[.:@]` (punto, dos puntos y arroba) y rechazan
  cualquier segmento igual a una clave de prototipo con `genui.forbidden_key`,
  ANTES de comprobar el patrón. Cubre `key` y claves de fila
  (`table.rows[]`, `data.*.inline[]`; `propertyNames`), `bindingPath`, `set.path`,
  `$t` (`a.__proto__.b`, `ns:__proto__`), `formField.depends_on` (vía `key`) y los
  identificadores que el renderer usará como clave de registro: `id` de bloque,
  `name`, `formField.widget`, `dataSource.widget`, `icon`, `block_settings.addon`,
  `invoke.action`, `emit.id`, `confirm.actions[].id`, `slot.name` y `ext`
  (`a.constructor@1`). `media.src` con `asset:<id>` lleva el lookahead solo en el
  `<id>` (da `genui.pattern`; las rutas `/…` no se parten). Go/RE2 (sin
  lookahead) debe repetir esta comprobación por segmentos en código con los
  mismos separadores y el mismo orden (antes del patrón).
- **`key` sin segmentos vacíos**: `[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*`,
  1 a 64 caracteres; `a..b` y `a.` dan `genui.pattern`. Las claves de fila usan
  el mismo patrón.
- **`depends_on`** es una `key` y debe ser la clave de un campo del mismo
  `form`/`wizard` (`genui.unknown_target`, igual que los pasos).
- **Registros del renderer**: lo que el esquema deja pasar sigue pudiendo
  coincidir con nombres heredados no listados (`toString`, `hasOwnProperty`,
  `valueOf`). Todo registro del renderer (bloques, widgets, iconos, addons,
  acciones, slots, extensiones, overlays, fuentes) debe ser un `Map` o buscar con
  `Object.hasOwn`, nunca `registry[name]`; y el resolvedor de vinculaciones
  (`state.toString`, `data.x.hasOwnProperty`) debe usar `Object.hasOwn` en cada
  segmento, no solo en el primero.
- **Mapas abiertos**: en un objeto con `additionalProperties` (params, values,
  filters, args, props…) una clave que empiece por `$` es `genui.ref_not_allowed`:
  `{"$ref": …}` solo es un valor donde el esquema lo declara.
- **Unidades**: los límites de texto cuentan **puntos de código Unicode** (un emoji
  es 1), no unidades UTF-16 ni bytes; el tamaño del documento cuenta bytes UTF-8.
  `null` no equivale a ausente (`title: null` es inválido). Con claves duplicadas
  en el texto JSON gana la última.
- **Reglas por posición**: `$ref`, enlaces, `source` y acciones se interpretan por
  su posición en el esquema, nunca por el nombre de la clave: una fila de datos
  `{"url": "...", "do": "open", "ref": "A-1"}` es un dato.

Los límites que los vectores ejercitan están en `GENUI_V2_LIMITS`
(`src/validate-v2.ts`) y deben ser idénticos en Go.

## Cómo los toma el kernel (PR aparte)

Hasta que exista `scripts/sync-genui-vectors.mjs` (ADR §2.7), la copia es
manual y de una sola vía, SDK → kernel:

```sh
SDK=/ruta/a/metacore-sdk/packages/genui
KERNEL=/ruta/a/metacore-kernel
mkdir -p $KERNEL/genui/schema $KERNEL/genui/testdata/conformance
cp $SDK/schema/genui-v2.schema.json $KERNEL/genui/schema/
rsync -a --delete $SDK/conformance/{valid,invalid,upgrade,downgrade} $KERNEL/genui/testdata/conformance/
```

El kernel anota el commit del SDK de donde copió (archivo de bloqueo con el
hash del contenido) y su `go test` recorre las cuatro carpetas. Si el kernel
pasa a ser la fuente del esquema (ADR §2.7), el sentido de la copia se invierte
y el SDK trae los vectores fijados a la versión del kernel; el formato no cambia.

## Reglas que el esquema no expresa (escritas a mano) y su vector

| Regla | Vector |
|---|---|
| Total de bloques (200), profundidad de bloques (6), hijos | `invalid/blocks-201`, `depth-7`, `children-25` |
| Acciones: 6 por bloque (contadas por posición), 32 en total | `invalid/actions-7-in-list`, `actions-total-33` |
| `id` único; clave de pestaña y de campo únicas | `invalid/duplicate-id`, `tabs-duplicate-key`, `form-duplicate-field` |
| Vinculaciones: máx. 200; raíz `data.<nombre>` declarada; `ctx` solo 4 claves | `invalid/too-many-bindings`, `ref-data-*`, `bad-ref-*` |
| `$ref` fuera de una posición de valor (url, link, src, route.path, enums, `set`, params de `emit`) o como mapa abierto (filters, params, values, args, props) | `invalid/ref-in-*`, `set-ref-value`, `map-ref-*` |
| Enlaces: seguros y externos solo con `allowExternalLinks` | `invalid/unsafe-link-*`, `external-link-disabled` |
| `source`, `refresh.source` y `$ref` a fuentes declaradas | `invalid/unknown-source`, `refresh-undeclared-source`, `source-constructor` |
| `open`/`close` a un overlay existente | `invalid/open-unknown-overlay`, `open-overlay-constructor` |
| `submit.form` apunta a un `form`/`wizard` | `invalid/submit-unknown-form`, `submit-form-is-text` |
| `route`: `path` o `name`, y lo que exige cada `name` | `invalid/route-*` |
| `table`/`chart`: `rows` xor `source`; `stats`/`timeline`: `items` xor `source`; mínimo uno | `invalid/table-rows-and-source`, `chart-rows-and-source`, `stats-*` |
| Tabla en línea 12 columnas x 200 filas; con `source`, 24 columnas | `valid/table-inline-12x200`, `table-source-24-columns`, `invalid/table-*` |
| Pasos de `wizard`/`form` refieren campos existentes; `wizard` sin contenido | `invalid/wizard-*`, `form-steps-without-steps` |
| Claves de prototipo prohibidas (también por segmento de ruta/clave/i18n/identificador); anidamiento JSON 64; números finitos | `invalid/proto-key*`, `set-path-*`, `ref-*-proto*`, `field-key-*`, `i18n-key-proto-*`, `table-row-key-*`, `inline-row-key-proto`, `*-constructor`, `json-depth-*`, `number-out-of-range` |
| `formField.depends_on` apunta a un campo del mismo form | `invalid/depends-on-*`, `valid/depends-on-valid` |

Todo lo demás (campos permitidos por bloque y por verbo, requeridos, enums,
patrones, rangos, longitudes, `maxItems`, forma de `state`, `submit` por destino)
vive en `schema/genui-v2.schema.json`.

## Regenerar

Los JSON se editan a mano (son la especificación); no los regeneres borrando la carpeta, que contiene este README. Un vector nuevo debe
añadirse en el mismo PR que la regla que ejercita, y correr en ambos lenguajes.
