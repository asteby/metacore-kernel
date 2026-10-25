# ADR 0004: Query batch v2: metaniveles, planes por ruta y compuerta única

- Estado: propuesto (solo diseño; sin código en este cambio).
- Fecha: 2026-10-25
- Contrato: `docs/CONTRACT-query-batch.md` (v1 vigente, §8 v2 propuesto).
- Repos afectados al implementarse: `asteby-hq/ops` (handler `/api/q`), `metacore-sdk` (`runtime-react`), kernel (primitivo de grafo).

## Contexto

El query batch v1 (`/api/q`, prefijos `t: m: mm: o: s: r: i:`) existe, pero el cliente casi no lo usa donde más pesa. Cifras medidas:

- **Arranque del shell: ~22-25 llamadas HTTP y ninguna pasa por `/api/q`.**
- **Pantalla de lista: ~5 + F llamadas** (F = filtros que piden opciones).
- **Modal/formulario: ~3-4 + K llamadas** (K = campos con opciones/referencias).
- **`m:`/`mm:` difieren de la ruta directa** en idioma, permisos y proyecciones; por eso el cliente no puede sustituir una por otra con seguridad y la ruta rápida queda sin usar.
- **Las cachés del cliente no separan usuario ni idioma** (solo la org): un cambio de usuario o idioma en la misma pestaña puede servir metadata ajena.
- **`/metadata/all` dispara un N+1 hasta 3 veces por instalación** de addon.

Además v1 expresa la denegación solo por texto de mensaje, no tiene límites de profundidad, tamaño ni plazo, y ejecuta secuencialmente.

## Decisión

Evolucionar el batch a v2, **aditivo** sobre v1:

1. **Sobre v2** `{v:2, plan, args, parts, inm}` → `{success, v, epoch, parts:{n:{success,status,data,meta,etag,retry,ms}}}` con estados numéricos 200/304/400/403/404/413/424/429/500/503/504.
2. **Metaniveles**: `q:caps` → `mi:` (índice de metadata de la org con ETag por clave) → `m:`/`mm:` → datos. El cliente pide solo la metadata cuyo ETag cambió.
3. **Tokens nuevos** `mi:`, `me:`, `p:`, `nav:`, `ad:`, `i:*/<lang>`, `x:<clave>`.
4. **Planes por ruta** con argumentos validados: `shell.boot`, `model.list`, `model.form`, `dashboard.home`, `integrations.installed`. Una pantalla = una petición.
5. **Compuerta única por recurso**: `m:`/`mm:`/`t:`/`r:`/`o:` usan el mismo código de autorización, idioma y proyección que su ruta directa, verificado por una **suite de conformidad byte a byte** en CI. Falla cerrado.
6. **Límites explícitos**: 64 partes tras expandir, profundidad 4, cuerpo 16 KB, 500 valores por `in:`, respuesta 2 MB (413), plazo total 3 s, por parte 1,5 s, concurrencia 4 por nivel.
7. **Referencias `@`** solo en valores de filtro y solo hacia partes 200; la org solo del JWT/`Locals`.
8. **Cachés de cliente** con clave que incluye usuario, org e idioma, invalidadas por `epoch`.

Antes de implementar se mide el estado actual con contadores en `/metrics` (`q_*`, ver PR de medición en ops) para validar cuánto se gana.

## Consecuencias

- Positivas: el arranque pasa de ~22-25 llamadas a 1-2; lista y modal a 1; desaparece el N+1 de `/metadata/all`; el 403 deja de depender de texto; el cliente puede usar el batch sin riesgo de divergencia; los límites acotan el abuso.
- Costos: trabajo de refactor para que el batch y la ruta directa compartan compuerta (el mayor riesgo y la mayor ganancia); la suite de conformidad es un mantenimiento permanente; los planes son contrato público que hay que versionar; un servidor con `q:caps` obliga a clientes a negociar.
- Riesgo de compatibilidad: bajo; `v` solo sube con cambios de semántica y el fallback a GET directo se conserva.
- Falta medir del lado del cliente (llamadas HTTP reales por pantalla, p. ej. con Playwright); las métricas del servidor solo ven lo que llega a `/api/q`.

## Alternativas descartadas

- **Más endpoints bootstrap a medida** (`/api/bootstrap/shell`, `/api/bootstrap/pos`…): resuelven una pantalla cada uno, duplican autorización y proyección (el mismo defecto que hoy tiene `m:` frente a la ruta directa) y se multiplican con cada addon.
- **GraphQL**: sustituye el contrato REST completo, exige esquema, resolver y control de costo de consultas, y reabre autorización por campo; desproporcionado para un conjunto cerrado de lecturas.
- **Solo HTTP/2**: elimina el costo de conexiones pero no la cascada de dependencias (la metadata antes de los datos, los ids de una lista antes de su stock), ni las divergencias de autorización, ni el N+1 del servidor; la latencia de ida y vuelta secuencial permanece.
