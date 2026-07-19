# Backlog de dependencias y deuda — seguimiento

Doc vivo. Captura POR QUÉ este trabajo importa y el plan para cerrarlo sin romper. Complementa el roadmap del SchemaEngine.

## El problema (ahora)
~20 PRs abiertos across repos, atascados por 3 causas:
1. **Deuda de dependencias**: bumps de librerías atrasados; varios *majors* (cambian API).
2. **Chocan con la consolidación**: el cutover del SchemaEngine reescribió archivos centrales (`dynamic_listupdate_kernel.go`, `appliance/main.go`), así que los PRs que los tocan dan conflicto.
3. **Features sin entregar** de otras sesiones (telemetry, chat filter, related-lists) que se pudren (conflictan más cada día).

## Por qué es necesario a largo plazo
- **Seguridad**: bumps de Stripe/jsonschema traen parches de CVEs. Dejarlos = exposición acumulada.
- **Costo compuesto**: un salto v82→v86 diferido es más difícil que hacerlo por partes. La deuda de deps es interés compuesto — cuanto más esperás, más grande y riesgosa cada migración.
- **Ruido tapa señal**: pila de PRs de Renovate → el equipo deja de mirar → los reales se pierden.
- **Fricción CI/onboarding**: lockfiles viejos y ramas en conflicto pudren el CI.

## Por qué NO se blastea
Force-merge sin build rompe (probado: ops#274 dio 3 conflictos en el boot del appliance). Regla: rebase → resolver conflicto correcto → adaptar API en majors → build verde → merge. Uno por uno.

## Inventario y plan

### A) Dep bumps seguros (Renovate self-rebase; merge con build verde)
- [x] ops kernel v0.79.2 (bump limpio #855; cerró Renovate #854 divergido)
- [x] ops #853 — chromedp v0.16.0
- [x] sdk #595 — version packages (publicado)
- [ ] addons #341 — metacore platform · #174 lockfile · #5 pnpm v11 · #337 version packages (pnpm, pendientes)

### B) Majors — pasada dedicada cada uno (adaptar API, build verde, o hold documentado)
- [x] kernel jsonschema v5→v6 (MIGRADO #209, API adaptada; cerró Renovate #79)
- [x] hub stripe-go v82→v86 (MIGRADO #304; cerró #249) — + limpieza gitlinks #306
- [x] sdk stripe-go v85→v86 (MIGRADO #599; cerró #390)
- [ ] hub #47 — tailwindcss v4 (frontend, pesado — pendiente)
- [ ] sdk #403 — react ecosystem (frontend, riesgo alto — pendiente)
- [ ] hub #191 — metacore platform (major) — CUIDADO: no bajar/subir kernel a algo incompatible con lo integrado

### C) Features de otras sesiones (rebase contra la consolidación + review por su autor)
- [ ] ops #731 — filtro chat legacy (TOMADO por worktree ops-inbox-clean, otra sesión)
- [ ] ops #274 — appliance telemetry (conflicto en main.go boot)
- [ ] hub #279 test · #188 bundle-default-latest
- [ ] addons #196 — related-lists (sub-tablas en detalle)

## Orden recomendado
1. A) primero (bajo riesgo, desbloquea): ops#854 (kernel bump) → sdk#595 (release) → addons deps.
2. C) features, rebaseadas contra la consolidación (yo resuelvo los conflictos en archivos que reescribí).
3. B) majors, uno por uno, con ventana mental para adaptar API.
