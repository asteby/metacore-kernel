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
| `postal_code` | string, patrón `^[0-9]{5}$` | sí | 5-digit postal code (MX código postal) |

Salida (cerrada):

| Campo | Tipo | Req. | Notas |
|---|---|---|---|
| `lat` | number | sí | WGS84 latitude of the postal code centroid, decimal degrees [-90, 90] |
| `lng` | number | sí | WGS84 longitude of the postal code centroid, decimal degrees [-180, 180] |
| `city` | string |  | city or municipality; omitted when the postal table has none |
| `state` | string | sí | state (name or code, as the region pack publishes it) |
| `precision` | string: postal_centroid | sí | always postal_centroid: draw it as an area, never as a rooftop pin |

Errores:

| Código | Quién | Cuándo |
|---|---|---|
| `validation_error` / `ErrInvalidInput` | host, antes de invocar | falta `postal_code` o no son 5 dígitos |
| `invalid_input` | proveedor (`NewCodedError`) | 5 dígitos que no son un código válido para su país (rango reservado) |
| `not_found` | proveedor (`NewCodedError`) | el código no está en la tabla del proveedor |

Notas:

- El punto es el **centroide** del área del código postal, nunca un domicilio.
  Úsese para mapas aproximados, envío por distancia o el selector de tienda
  más cercana; no para rutas de entrega puerta a puerta.
- `city` es opcional porque las tablas postales (SEPOMEX) no traen ciudad para
  muchos códigos rurales; `state` siempre viene.
- La entrada son cinco dígitos (México). Otro país con otro formato necesitará
  su propio contrato o un campo `country` con patrón por país.
