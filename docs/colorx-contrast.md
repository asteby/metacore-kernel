# colorx: contraste dinámico en el servidor

Paquete `github.com/asteby/metacore-kernel/colorx` (solo biblioteca estándar).
Espejo en Go del primitivo de contraste del SDK (`@asteby/metacore-theme/contrast`):
un solo algoritmo, dos lenguajes, un archivo de vectores compartido.

Úsalo cuando la decisión deba tomarse en el servidor (PDF, correos, miniaturas,
proyecciones del hub como `brand_on_color` / `logo_tone` calculadas **una vez**
al subir el logo, sin re-muestrear en cada cliente).

## API

| Función | Qué hace |
|---|---|
| `ParseColor(s)` | `#rgb`, `#rgba`, `#rrggbb`, `#rrggbbaa`, `rgb()/rgba()`, `hsl()/hsla()`, `oklch()`, `white`/`black`/`transparent`. Otra cosa: `ErrInvalidColor`. |
| `RelativeLuminance(c)` | Luminancia relativa WCAG (0..1). |
| `ContrastRatio(fg, bg)` / `Contrast(fg, bg string)` | Razón WCAG 1..21. |
| `OnColor(bg, OnOptions{Light, Dark, Min})` | Texto legible sobre `bg`. |
| `EnsureContrast(fg, bg, min)` | Ajusta la luminosidad OKLCH de `fg` hasta `min`. |
| `PickBacking(fg, candidates)` | El fondo candidato con mejor razón contra `fg`. |
| `ImageTone(r io.Reader)` | Color medio, luminancia y `Tone` de los píxeles opacos de una imagen. |

## Algoritmo

- Colores en sRGB codificado con gamma (0..1). Un `fg` translúcido se compone
  sobre el fondo (source-over en espacio gamma, como los navegadores); un
  fondo translúcido se aplana primero sobre blanco.
- `RelativeLuminance = 0.2126 R + 0.7152 G + 0.0722 B` sobre canales linealizados.
- `ContrastRatio = (Lmax + 0.05) / (Lmin + 0.05)`.
- `OnColor`: elige entre `Light` (def. `#ffffff`) y `Dark` (def. `#000000`) el de
  mayor razón (empate: `Light`). Si el mejor no llega a `Min` (def. **4.5**),
  se ajusta con `EnsureContrast` y devuelve un `oklch()`.
- `EnsureContrast`: si ya cumple, lo devuelve recodificado como `oklch(L C H)`
  (4/4/2 decimales; el chequeo se hace sobre la cadena redondeada). Si no,
  mueve L de a 0.01 (hasta 100 pasos) alejándolo del fondo (aclara si la
  luminancia del fondo < 0.18, oscurece si no), conservando croma y matiz;
  si es inalcanzable devuelve el extremo (L 0 o 1).
- `PickBacking`: máxima razón; empate gana el primero; candidatos no
  parseables se omiten (ninguno válido: `ErrInvalidColor`).
- `ImageTone`: PNG/JPEG/GIF (stdlib). Promedia canales gamma sin premultiplicar
  de los píxeles con alfa > 40/255, muestreando cada N píxeles para que la
  rejilla sea de ≤ 256 px por lado. Tope de 16 MiB leídos y 64 Mpx declarados
  (`ErrImageTooLarge`, se rechaza antes de decodificar). `Tone`: luminancia
  ≥ 0.6 `light`, ≤ 0.2 `dark`, si no `mid`.
  WebP y SVG no tienen decodificador en el kernel: `ErrUnsupportedImage`
  (el host debe rasterizar antes o saltarse el tono). Imagen totalmente
  transparente: `ErrNoOpaquePixels`.

## Garantías

- Con los candidatos por defecto (blanco/negro), `OnColor` da **≥ 4.5:1**
  (en realidad ≥ 4.58) sobre cualquier fondo opaco; la prueba lo recorre en
  una rejilla de 52³ colores.
- `EnsureContrast` llega a `min` o devuelve el extremo L 0/1.
- Borde AA verificado: `#767676` sobre blanco 4.54 (pasa), `#777777` 4.48 (no).

## Vectores de conformidad

`colorx/testdata/contrast-vectors.json`; el SDK copia el archivo tal cual.
Tolerancia de razón: 0.01. Cadenas (`expect`) exactas.

```json
{
  "onColor":        [{"bg":"#2f7fd6","min":4.5,"expect":"#000000","ratio":5.7}],
  "ensureContrast": [{"fg":"#777777","bg":"#ffffff","min":4.5,"expect":"oklch(0.5593 0.0000 89.88)","ratio":4.668}],
  "pickBacking":    [{"fg":"#ffffff","candidates":["#ffffff","#2f7fd6"],"expect":"#2f7fd6","ratio":4.2}]
}
```

`onColor` usa los candidatos por defecto. Los valores se generaron con esta
implementación (con las mismas constantes OKLCH que `contrast.ts`); si el SDK
discrepa en un vector, se corrige el código divergente, no el vector, salvo
que el error esté en la definición de este documento.

## Punto de integración propuesto (no implementado)

`brand.Manifest` (`/api/brand`) es identidad de **producto**; no se tocó. Para
hub, proponer en `HubInstance` los campos aditivos `brand_logo_tone`
(`light|dark|mid`) y `brand_on_color` (hex), calculados con `ImageTone` y
`OnColor` al subir el logo y proyectados a los clientes.
