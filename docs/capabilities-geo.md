# Contratos de capacidad geo

Referencia de los contratos `geo.*` del paquete `capability`. **Solo contrato**:
el kernel no trae geocodificador ni enrutador. Un addon proveedor los declara en
`provides_capabilities` (centroides de código postal de `region_mx`, Nominatim u
OSRM/Valhalla autoalojados…) y el host lo selecciona por organización y mercado
como cualquier otra capacidad (`capability.Select`, ver
[capabilities-commerce.md](capabilities-commerce.md)).

Sin proveedor, el consumidor degrada por su cuenta: puntos por código postal y
líneas rectas entre paradas. Nunca falla una pantalla porque falte geo.

## Reglas

| Concepto | Regla |
|---|---|
| **Coordenadas** | WGS84 en grados decimales, `lat` y `lng` como números JSON separados. Tipos `latitude` (`[-90, 90]`) y `longitude` (`[-180, 180]`); una cadena o un valor fuera de rango es salida inválida (`invalid_output`). Solo dentro de GeoJSON el orden es `[lng, lat]`. |
| **Precisión** | Cada resultado dice qué tan exacto es: `rooftop, street, neighborhood, postal_centroid, locality, region, country`. Un centroide de CP es `postal_centroid`: la UI no lo pinta como pin de domicilio. |
| **Tiempo límite** | `geo.address.suggest` 1 s, `geo.geocode` 3 s, `geo.route` 5 s por proveedor (rutas interactivas: formularios de dirección, paneles de mapa). |
| **Cardinalidad** | `one` en ambos. No son mutantes ni requieren fijar proveedor. |
| **Privacidad** | El proveedor recibe direcciones de clientes; preferir proveedores autoalojados. El host no debe mandar nombre ni teléfono del cliente, solo la dirección. |

## Formas compartidas

### GeoPoint

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `lat` | latitude | sí | WGS84 latitude |
| `lng` | longitude | sí | WGS84 longitude |

### GeoPlace

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `lat` | latitude | sí | WGS84 latitude |
| `lng` | longitude | sí | WGS84 longitude |
| `precision` | string: rooftop \| street \| neighborhood \| postal_centroid \| locality \| region \| country | sí | how exact the point is; a postal code centroid is postal_centroid |
| `label` | string |  | formatted address / place name |
| `postal_code` | string |  |  |
| `street` | string |  | street name without the house number |
| `house_number` | string |  | house or building number as written (a string: 12-B, S/N) |
| `neighborhood` | string |  | colonia / district |
| `city` | string |  | city / municipio |
| `state` | string |  | state / province (code or name) |
| `country` | country |  | ISO 3166-1 alpha-2 |
| `place_id` | string |  | opaque provider id of the match; stable enough to re-resolve it, never to be parsed |
| `confidence` | number |  | 0..1, provider-defined |
| `bbox` | array<number> |  | [west, south, east, north] in degrees, when the match is an area |

### RouteLeg

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `distance_m` | number | sí | metres |
| `duration_s` | number | sí | seconds |

## Contratos

### `geo.geocode`

text or postal code to coordinates (send query and/or postal_code). Consulta · cardinalidad **one** · tiempo límite por proveedor **3s**.

Entrada (abierta; manda `query`, `postal_code` o ambos):

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `query` | string |  | free-text address or place; one of query / postal_code is needed |
| `postal_code` | string |  | postal code; alone it resolves to the code's centroid |
| `neighborhood` | string |  |  |
| `city` | string |  |  |
| `state` | string |  |  |
| `country` | country |  | ISO 3166-1 alpha-2; restricts the search |
| `limit` | integer |  | max results (provider default 1, at most 10) |
| `locale` | string |  | language of the labels (es, en…) |

Salida:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `results` | array<GeoPlace> | sí | best match first; empty when nothing matched |

### `geo.address.suggest`

address candidates for a partially typed street (type-ahead of an address form). Consulta · cardinalidad **one** · tiempo límite por proveedor **1s** (`capability.AddressSuggestTimeout`).

Es el autocompletado de un formulario de dirección: una llamada por pausa al
teclear. Va **separado de `geo.geocode`** porque su plazo, su volumen y su caché son
otros (una sugerencia que tarda más de un segundo estorba más de lo que ayuda).

Entrada:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `query` | string, patrón `(?s)^\s*\S.+\S\s*$` | sí | lo que el comprador escribió: al menos 3 caracteres sin contar los espacios de los extremos (`capability.MinAddressSuggestQuery`); el host rechaza menos sin llamar a nadie |
| `country` | country |  | ISO 3166-1 alpha-2; acota la búsqueda (el país del mercado) |
| `postal_code` | string |  | código ya elegido en el formulario; sesga y filtra los candidatos |
| `near` | GeoPoint |  | punto de sesgo (p. ej. el centroide del CP); nunca un filtro |
| `limit` | integer |  | máximo de candidatos, 1..8 (`capability.MaxAddressSuggestResults`; el proveedor recorta; por omisión 5) |
| `locale` | string |  | idioma de las etiquetas (es, en…) |

Salida:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `results` | array<GeoPlace> | sí | mejor candidato primero, a lo sumo 8; vacío si nada coincide |

Reglas de uso:

- **Sugiere, no decide.** El catálogo del paquete de región (`region.postal.locate`)
  es la única fuente autoritativa de estado, municipio y colonia. Un candidato que
  contradice al catálogo (otro país, otro CP, otro estado) lo descarta el consumidor.
- **Privacidad.** Solo se manda lo escrito, el país, el CP y un punto de sesgo: nunca
  nombre, teléfono, correo ni identificador del comprador. Preferir proveedores
  autoalojados; la caché del proveedor es por (país, texto normalizado), nunca por comprador.
- Sin proveedor (`no_provider`) o con uno caído, el formulario sigue siendo de captura
  manual: ni el pago ni los campos dependen de esta capacidad.

### `geo.route`

road distance, duration and optional geometry through ordered waypoints. Consulta · cardinalidad **one** · tiempo límite por proveedor **5s**.

Entrada:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `waypoints` | array<GeoPoint> | sí | origin first, destination last; 2..25 points (`capability.MaxRouteWaypoints`) |
| `profile` | string: driving \| truck \| cycling \| walking |  | default driving |
| `geometry` | boolean |  | return the route geometry (default false: distance and duration only) |

Salida:

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `distance_m` | number | sí | total metres |
| `duration_s` | number | sí | total seconds |
| `geometry` | object |  | GeoJSON LineString {type, coordinates: [[lng, lat], …]}, only when requested |
| `legs` | array<RouteLeg> |  | one per consecutive waypoint pair |
| `approximate` | boolean |  | true when the provider fell back to straight lines (no road network) |

## Coordenadas en rastreo de envíos

`TrackingEvent` (salida de `commerce.shipping.track`) acepta `lat` / `lng`
opcionales para paqueterías que reportan coordenadas. Un proveedor que no las
manda sigue siendo válido; si las manda, deben ser números en rango. Cuando no
hay coordenadas, el consumidor geocodifica `location` o el CP destino con
`geo.geocode` (si hay proveedor instalado) y guarda la precisión obtenida.
