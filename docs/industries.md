# Gate de multiindustria

Plan v2 (M3) exige que un preset de industria opere su flujo completo con **0 lineas de nicho** en kernel/SDK. Este gate lo mide en cada CI del kernel (job `go`, sin Postgres):

- Manifests v3 de PRUEBA: `testdata/industries/restaurante.manifest.json` y `consultorio.manifest.json`.
- Pruebas: `industries/industries_test.go` (vive fuera de `testdata/` porque `go test ./...` ignora esas carpetas).
- Cada industria se valida con el validador v3 (`Strict`, sin avisos) y el legacy (`manifest.FromV3(...).Validate`), y se EJECUTA sobre el `dynamic.Service` real con SQLite en memoria. Los manifests no pueden contener tokens de nicho (MXN, CFDI, RFC, llanta, es-MX...).

Para agregar una industria: un manifest en `testdata/industries/`, sus modelos en el test y un flujo feliz + rechazos. Si necesita algo que no se puede expresar, **no** se fuerza un hack: se agrega al backlog (`TestBacklog`) con `t.Skip("backlog: <primitivo>")`.

## Que SI se puede expresar hoy (y el gate ejecuta)

| Primitivo | Restaurante (Comanda) | Consultorio (Cita / Expediente) |
|---|---|---|
| K3 stages + transitions | mesa -> comanda -> cocina -> cuenta -> pagada / cancelada | solicitada -> confirmada -> atendida -> cobrada / cancelada |
| `roles` | mesero, gerente | recepcion, medico |
| `requires_fields` | `mesa` para abrir | `consentimiento_firmado_at` para atender |
| `guard` numerico | `paid_total >= total` para cobrar; `total > 0` para ir a cocina | `honorario > 0` para cobrar |
| `approval` + `reason_required` | cancelar (gerente, con motivo) | - |
| `reopen` + approval | reabrir `pagada` | - |
| `locked` / `editable_when_locked` | pagada y cancelada locked | cobrada permite editar `notas_pago` |
| K4 `scope: column` + `period` | comanda por turno y dia | - |
| K4 `gapless` + `assign_on: transition:<stage>` | ticket `T-000001..` al entrar a `cuenta` | - |
| K4 `gapless` + `scope: column` | - | expediente por paciente |
| K5 `request_approval` (rule) | `discount_pct > 10` -> gerente | `descuento_honorario_pct > 20` -> director |

Verificado por el gate: aparcado de la aprobacion (nada se escribe), replay unico al aprobar, rechazo de aprobador sin rol y de aprobacion sin motivo, segunda aprobacion rechazada, folios sin huecos bajo concurrencia, INSERT fallido que no quema folio.

Matices observados al armar el gate:

- `requires_fields` trata un numerico en 0 como presente (la columna existe): "total > 0" se expresa con `guard`, no con `requires_fields`.
- Una serie `gapless` no se puede pedir suelta (`NextSequence*` la rechaza): solo sale por su columna.
- El host cablea los resolvers desde `manifest.FromV3` (stage machine, secuencias, `dynamic.BuildModelConstraints`); sin `BuildModelConstraints` las reglas `request_approval` NO se aplican (el gate llama `UnwiredRuleGuards`).

## Que NO se puede expresar hoy (backlog de primitivos)

Cada punto esta en `TestBacklog` como `t.Skip("backlog: ...")`.

1. **Split de cuenta, propinas y pagos multiples sobre un documento**: no hay lineas de pago hijas con reparto ni propina como concepto; solo un `paid_total` plano.
2. **Ruteo a estacion de cocina (KDS)**: no hay estado ni destino por linea.
3. **Modificadores y combos**: sin lineas hijas parametrizadas con precio/receta.
4. **Agenda con slots/recursos y exclusion de empalmes** (citas, mesas, reservas): no hay constraint de intervalo/recurso.
5. **Recordatorios relativos** (T-24h de la cita): no hay disparadores relativos a una fecha del registro.
6. **`guard` / `when` solo numerico**: K3 usa `evalConstraintExpr` sobre `mergedNumericEnv` (`dynamic/stage_policy.go:69-75`); no compara strings, fechas ni `now()`. `RecordExpr` (`manifest/v3/recordexpr.go`) existe aparte y no esta conectado a las transiciones.
7. **Lotes con caducidad y recetas** (insumos, FEFO, explosion de receta).
8. **Impuestos multiples o por pais**: `orgconfig.TaxRate` es unico.
9. **Moneda por documento / tipo de cambio**.
10. **Consentimiento con vigencia**: hoy solo se exige que el campo exista (`requires_fields`), no que este vigente (depende de 6).
