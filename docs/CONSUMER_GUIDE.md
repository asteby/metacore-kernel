# Consumer Guide — metacore-kernel

Guía para apps que consumen el kernel como library Go: `ops`, `link`, `pilot`, `doctores.lat`, `p2p`, `visor`, `operador360`.

---

## 1. Instalación

```bash
go get github.com/asteby/metacore-kernel@v0.1.0
go mod tidy
```

En `go.mod` aparecerá:

```go
require github.com/asteby/metacore-kernel v0.1.0
```

Para traer `latest` durante desarrollo:

```bash
go get github.com/asteby/metacore-kernel@latest
```

---

## 2. Acceso a repo privado

El kernel vive en un repo privado (`github.com/asteby/metacore-kernel`). Configuración única por máquina de desarrollador:

### 2.1. Variables de entorno

```bash
# Añadir al shell rc (~/.zshrc, ~/.bashrc):
go env -w GOPRIVATE="github.com/asteby/*"
go env -w GONOSUMCHECK="github.com/asteby/*"
```

Equivalente temporal en una sesión:

```bash
export GOPRIVATE="github.com/asteby/*"
export GONOSUMCHECK="github.com/asteby/*"
```

### 2.2. Auth vía SSH (recomendado para devs)

```bash
git config --global url."git@github.com:".insteadOf "https://github.com/"
```

Requiere clave SSH cargada en GitHub (`ssh-keygen -t ed25519 -C "you@example.com"` + añadir `.pub` en https://github.com/settings/keys).

### 2.3. Auth vía token (CI / headless)

```bash
cat > ~/.netrc <<EOF
machine github.com
  login x-access-token
  password $GITHUB_TOKEN
EOF
chmod 600 ~/.netrc
```

En GitHub Actions de consumidoras, usar un PAT con scope `repo:read`:

```yaml
- name: Configure netrc
  run: |
    cat > ~/.netrc <<EOF
    machine github.com
      login x-access-token
      password ${{ secrets.METACORE_READ_TOKEN }}
    EOF
    chmod 600 ~/.netrc
```

---

## 3. Quickstart

Ejemplo mínimo montando auth + metadata + dynamic CRUD con Fiber:

```go
package main

import (
    "log"

    "github.com/gofiber/fiber/v2"
    "gorm.io/driver/postgres"
    "gorm.io/gorm"

    "github.com/asteby/metacore-kernel/auth"
    "github.com/asteby/metacore-kernel/dynamic"
    "github.com/asteby/metacore-kernel/metadata"
)

func main() {
    db, err := gorm.Open(postgres.Open(mustEnv("DATABASE_URL")), &gorm.Config{})
    if err != nil {
        log.Fatalf("db: %v", err)
    }

    app := fiber.New()

    // 1. Auth: login/refresh/logout + middleware JWT
    authSvc := auth.NewService(auth.Config{
        DB:           db,
        JWTSecret:    mustEnv("JWT_SECRET"),
        RefreshTTLh:  720,
        AccessTTLmin: 15,
    })
    authSvc.MountRoutes(app.Group("/auth"))

    // 2. Metadata: registro de entidades + catálogo expuesto
    md := metadata.New(db)
    md.Register(metadata.Entity{
        Name:   "invoice",
        Fields: []metadata.Field{{Name: "amount", Type: "decimal"}},
    })
    md.MountRoutes(app.Group("/metadata"))

    // 3. Dynamic: CRUD genérico sobre las entidades registradas
    dyn := dynamic.New(db, md)
    dyn.MountRoutes(app.Group("/api"), authSvc.Middleware())

    log.Fatal(app.Listen(":3000"))
}
```

Ver `ARCHITECTURE.md` en la raíz del kernel para el catálogo completo de módulos (`host`, `events`, `navigation`, `permission`, `query`, `security`, `workflows`).

---

## 4. Renovate — template de config

Copia `docs/consumer-renovate-template.json` del kernel a la raíz de tu repo consumidor como `renovate.json`. Snippet clave:

```json
{
  "$schema": "https://docs.renovatebot.com/renovate-schema.json",
  "extends": ["config:recommended", ":semanticCommits"],
  "schedule": ["before 6am on monday"],
  "packageRules": [
    {
      "matchManagers": ["gomod"],
      "matchPackagePatterns": ["^github.com/asteby/metacore-kernel"],
      "matchUpdateTypes": ["patch", "minor"],
      "automerge": true,
      "platformAutomerge": true,
      "groupName": "metacore-kernel"
    },
    {
      "matchManagers": ["gomod"],
      "matchPackagePatterns": ["^github.com/asteby/metacore-kernel"],
      "matchUpdateTypes": ["major"],
      "automerge": false,
      "labels": ["breaking", "review-required"]
    }
  ]
}
```

### Requisitos en el repo consumidor

1. **Instalar Renovate GitHub App** con acceso al repo.
2. **Habilitar `Allow auto-merge`** en Settings -> General del repo (para que `platformAutomerge` funcione).
3. **Branch protection** en `main` con "Require status checks to pass before merging" -> CI debe pasar antes del auto-merge.
4. **GITHUB_TOKEN / RENOVATE_GITHUB_TOKEN** con acceso al módulo privado. En Renovate Cloud, configurar `hostRules` con token PAT.

### Dispatch on-demand

El workflow de release del kernel dispara `repository_dispatch` con `event_type=metacore-kernel-released` en cada consumidora. Añade en tu repo `.github/workflows/renovate-trigger.yml`:

```yaml
name: Renovate on kernel release
on:
  repository_dispatch:
    types: [metacore-kernel-released]
jobs:
  trigger:
    runs-on: ubuntu-latest
    steps:
      - uses: renovatebot/github-action@v40
        with:
          token: ${{ secrets.RENOVATE_TOKEN }}
          configurationFile: renovate.json
```

---

## 5. Política SemVer — cómo leer el changelog

Cuando Renovate abra un PR bumpeando el kernel:

### Patch (`v0.1.0` -> `v0.1.1`)

- Sólo bug fixes. Auto-merge es seguro siempre que el CI pase.
- Revisar el changelog toma <30s; busca "Bug fixes".

### Minor (`v0.1.0` -> `v0.2.0`)

- Nuevas features, nuevos símbolos públicos. Backward-compatible.
- Auto-merge es seguro **si tu CI cubre las rutas de integración con kernel** (auth, metadata, dynamic).
- Si tu repo usa kernel de forma superficial (sólo un módulo), revisa si añadieron deprecaciones (`// Deprecated:`).

### Major (`v1.x` -> `v2.x`)

- **NO auto-merge**. Breaking changes en API.
- El import path cambia: `github.com/asteby/metacore-kernel/v2`. Renovate no puede hacer ese rewrite automáticamente — requiere intervención manual con `gopls rename` o `go mod edit -replace`.
- Revisar `docs/MIGRATIONS.md` en el kernel (si existe) antes de hacer el upgrade.

### Señales de riesgo en un PR de Renovate

- **Falla el CI de la consumidora** -> no hacer merge, abrir issue upstream.
- **Changelog menciona "schema change"** -> revisar si tu app corre migraciones automáticas.
- **Bump cruza un major minor (`v0.5` -> `v0.6`)** en pre-1.0 -> tratar como posible breaking aunque sea técnicamente minor.

---

## 6. Flujo completo esperado

```
[Kernel] git tag v0.1.1 && git push --tags
       |
       v
[Kernel] Release workflow: tests -> proxy ping -> GoReleaser -> dispatch
       |
       v
[Consumer] repository_dispatch recibido -> Renovate corre
       |
       v
[Consumer] PR "chore(deps): update github.com/asteby/metacore-kernel to v0.1.1"
       |
       v
[Consumer] CI pasa -> Renovate auto-merge -> main actualizado
       |
       v
[Consumer] Deploy automatizado (si aplica)
```

Tiempo típico end-to-end: 5-15 minutos entre `git push --tags` y `main` actualizado en todas las consumidoras.

---

## 7. FAQ

**Q: ¿Puedo saltar el proxy y usar el repo directo?**
A: Sí — `GOPROXY=direct go get ...`. Útil para probar branches sin tag.

**Q: ¿Cómo uso un commit específico?**
A: `go get github.com/asteby/metacore-kernel@<commit-sha>` -> `go.mod` usa una pseudo-version (`v0.0.0-YYYYMMDDhhmmss-<sha12>`).

**Q: ¿Y si necesito una feature que todavía no está tageada?**
A: Pide al maintainer que release. Para desarrollo local, usa `replace`: `go mod edit -replace github.com/asteby/metacore-kernel=../metacore-kernel`.

**Q: ¿Puedo forkear el kernel?**
A: Técnicamente sí, pero rompe Renovate (dejarías de recibir bumps upstream). Preferir contribuir un PR o abrir issue.
