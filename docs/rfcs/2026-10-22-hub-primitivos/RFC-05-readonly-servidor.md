# RFC-05: `readonly` aplicado en servidor (campos de réplica)

- **Veredicto:** **no se crea un primitivo nuevo.** Ya existe (`Column.protected`); se ajusta con
  `ingest.exclusive` (RFC-04) y un lint.
- **Prioridad:** lint P0 (M1, sin kernel); `exclusive` P1 con RFC-04.
- **Tamaño:** S.

## 1. Evidencia

- El spike marcó `license_status` y `last_seen_at` como `readonly` y un `PUT {"license_status":"revoked"}`
  se aceptó [S] (G4).
- Es el comportamiento documentado: `readonly` es **solo UI** — *«The DDL and write planes ignore it»*
  (`manifest/v3/types.go:1226-1236`) [V].
- El gate de servidor existe desde kernel v0.136.0 (#356): `Column.protected`
  (`types.go:1238-1257`), aplicado en `dynamic.Service.validateColumns` sin excepción por rol ni permiso;
  un valor que no cambia nada se descarta en silencio para que el formulario de edición no falle
  (`dynamic/validate.go:163-182`, `protectedNoop`) [V]. El SDK ya lo pinta como solo lectura
  (`dynamic/derive.go:466`: `Readonly: c.Readonly || c.Protected`) [V]. Todos los consumidores lo
  tienen (manifestcheck v0.222.0, hub v0.225.0, ops v0.229.4 > v0.136.0) [V].

## 2. Ajuste

1. **Campos de réplica:** declararlos en `ingest.fields` con `ingest.exclusive: true` (RFC-04), que los
   vuelve `protected` para la ruta genérica y deja escribir solo al sumidero y a acciones `set_fields`
   que los listen. Mientras RFC-04 no exista: `protected: true` **impide** que el replicador por API de
   datos escriba esos campos; por eso en M1 se elige una de dos y se documenta:
   - (a) réplica por API `sk_` sin `protected` + RBAC: el rol `hub_operator` no recibe `update` sobre
     `Instance` (los admin/owner sí lo tienen por bypass de super-rol: riesgo aceptado y temporal), o
   - (b) esperar RFC-04 (recomendado como criterio de **salida** de M1).
2. **Lint** (en `manifestcheck` de addons / hub-addons, sin bump de kernel):
   - `readonly: true` sin `protected: true` en un modelo que declara `ingest` → error (cuando exista) o
     aviso con el texto «readonly es solo UI; use protected o ingest.exclusive».
   - Columna con `readonly` en un modelo cuyo `metadata.visibility` es `private` y no tiene ningún
     handler que la escriba → aviso.
3. **Documentación:** el comentario del schema de `readonly` ya lo dice; añadir a `docs/spec/v3/README.md`
   una tabla «readonly vs protected vs ingest.exclusive».

## 3. Compatibilidad, pruebas, seguridad

- Sin cambio de contrato; el lint corre solo en CI de autoría.
- Prueba de regresión del caso G4 en RFC-04 §5.
- Seguridad: `protected` es la defensa; el lint evita el error de autoría que el spike encontró.
