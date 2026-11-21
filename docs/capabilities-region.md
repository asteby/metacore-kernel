# Contratos de capacidad de región

Contratos del paquete `capability` que responden los paquetes de región
(`region_mx`…) desde sus propias tablas. El kernel no trae datos: sin proveedor
instalado la consulta falla con `no_provider` y el consumidor se degrada solo
(p. ej., sin mapa ni distancia). Las reglas generales (cardinalidad, tiempo
límite, salida cerrada, extensiones `x_`, selección por mercado) son las de
[capabilities-commerce.md](capabilities-commerce.md).

## `region.postal.locate`

postal code to the centroid of its area, with city and state. Consulta ·
cardinalidad **one** · tiempo límite por proveedor **2s** · errores
`invalid_input`, `not_found`.

Entrada:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `postal_code` | string | sí | postal code in the country's own format; each region pack validates it (`invalid_input`) |
| `country` | country |  | ISO 3166-1 alpha-2 of the market; a pack of another country answers `not_found` |

Salida (cerrada):

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `lat` | number | sí | WGS84 latitude of the postal code centroid, decimal degrees [-90, 90] |
| `lng` | number | sí | WGS84 longitude of the postal code centroid, decimal degrees [-180, 180] |
| `city` | string |  | city or municipality; omitted when the postal table has none |
| `state` | string | sí | state (name or code, as the region pack publishes it) |
| `state_code` | string |  | short or ISO code of the state (e.g. MX-CMX, CDMX), when the pack has one |
| `country` | country |  | ISO 3166-1 alpha-2 of the pack that answered |
| `neighborhoods` | array<string> |  | colonias / districts of the code, official spelling; omitted when the table has none |
| `cities` | array<string> |  | every municipality the code covers when more than one; `city` is the first |
| `precision` | string: postal_centroid | sí | always postal_centroid: draw it as an area, never as a rooftop pin |

Errores:

| Código | Quién | Cuándo |
|---|---|---|
| `validation_error` / `ErrInvalidInput` | host, antes de invocar | falta `postal_code` o no es una cadena |
| `invalid_input` | proveedor (`NewCodedError`) | un código que no tiene el formato de su país (México: 5 dígitos) o es un rango reservado |
| `not_found` | proveedor (`NewCodedError`) | el código no está en la tabla del proveedor |

Notas:

- El punto es el **centroide** del área del código postal, nunca un domicilio.
  Úsese para mapas aproximados, envío por distancia o el selector de tienda
  más cercana; no para rutas de entrega puerta a puerta.
- `city` es opcional porque las tablas postales (SEPOMEX) no traen ciudad para
  muchos códigos rurales; `state` siempre viene.
- El **formato** del código lo valida cada paquete de región (el host ya no
  impone `^[0-9]{5}$`, que era de México): lo declara `region_address_fields` y el
  proveedor responde `invalid_input`. El código viaja como cadena (un número pierde
  el cero inicial de `06600`).
- `neighborhoods`, `cities`, `state_code` y `country` son **opcionales y aditivos**: un
  proveedor anterior sigue siendo válido y el consumidor degrada a lo que sí venga.
  Esta consulta, con el catálogo oficial del paquete, es la única fuente
  **autoritativa** de estado, municipio y colonia de una dirección; los
  sugeridores de calle (`geo.address.suggest`) nunca la sustituyen.

## `documents.enrich`

enrich one printable document (record + children) before its template is
hydrated. Consulta · cardinalidad **one** · tiempo límite por proveedor **5s** ·
errores `invalid_input`, `not_applicable`.

El host (ops) carga el registro y sus hijos, pregunta al proveedor por
`doc_kind` (el `DocumentDef.kind` del manifest), mezcla la respuesta y
hidrata la plantilla. El host no trae código de ningún país: lo provee el
paquete de región o fiscal del país. Sin proveedor,
o con `not_applicable`, se imprime el documento genérico.

Entrada:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `doc_kind` | string | sí | `DocumentDef.kind` (snake_case, p. ej. `invoice`, `credit_note`); un proveedor que no lo conoce responde `not_applicable` |
| `model` | string | sí | modelo del registro que se imprime |
| `document_key` | string |  | `DocumentDef.key` |
| `record` | object | sí | el registro como lo cargó el host (columna → valor) |
| `children` | object |  | relaciones hijas precargadas: nombre → arreglo de filas |
| `branding` | object |  | campos de marca de la org (string → string) |
| `locale` | string |  | etiqueta BCP 47 (en, es, pt-BR) |

Salida (cerrada, todo opcional):

| Campo | Tipo | Notas |
|---|---|---|
| `record` | object | parche que se mezcla sobre el registro; las claves ausentes se quedan, nunca se borran columnas |
| `children` | object | conjuntos hijos que se REEMPLAZAN por nombre; los nombres ausentes se quedan |
| `branding` | object | parche sobre la marca de la org solo para esta impresión (string → string); p. ej. el emisor que nombra un comprobante timbrado |
| `line_items` | array | filas de `{{#line_items}}`; omitido = las del host |

Notas:

- Aditivo (R2-b): `DocumentDef.kind` es opcional; un manifest sin `kind` se
  comporta como antes.
- `not_applicable` no es un fallo: el consumidor no reintenta ni alerta.
- El proveedor no emite dinero con coma flotante: las cantidades viajan como
  el host las cargó (decimales) y el redondeo es el del país del proveedor.
