# Validation compatibility window

**Policy: tightening validation = warning for one minor release, then a hard
error.** A validator change must never invalidate an addon that is already
published. (Incident 2026-10-04: kernel #453 made `requires_state` outside the
model's stages a hard error; workshop and customers, published before it,
became invalid overnight and `installer.Upgrade` failed in production with
"manifest validation failed".)

## Mechanism

- `manifest/v3`: `ValidateWithOptions(raw, Options{Strict})` /
  `ParseWithOptions`. `Validate` / `Parse` keep their signature and are
  **strict** (every rule is an error).
- A new rule that rejects manifests that used to pass is reported through
  `tolerate(ruleID, msg)` and listed in `manifest/v3/compat.go`. Non-strict
  mode returns it as a warning `"[rule_id] message"`; strict mode as an error.
  Structural / schema errors are never downgraded.
- After one minor release, drop the rule from `legacyTolerated` and change its
  call site back to a plain `errs = append(...)`.
- Currently tolerated: `requires_state_in_stages`.

## Who calls which mode

| Caller | When | Mode |
| --- | --- | --- |
| hub | publishing a bundle | strict (`bundle.Read`, `v3.Validate`) |
| CI / `addonc` | authoring | strict |
| ops, installer | install / upgrade of an already-published bundle | non-strict: `bundle.ReadWithOptions(r, max, bundle.ReadOptions{})`; findings land in `Bundle.ValidationWarnings` and `Installer.Upgrade` logs them (`manifest.advisory`) |

`bundle.Read` stays strict. Hosts that fetch bundles to install them must move
to `ReadWithOptions` with `Strict: false`.

## Upgrade pre-flight

`Installer.Upgrade` first validates, then **rehearses** the schema step and the
pending SQL migrations inside one Postgres transaction that is always rolled
back (`defaultSchemaApplier.DryRunForUpgrade`, `dynamic.DryRunMigrations`),
before the BEFORE hook fires or anything is persisted. The real apply is not
atomic across migrations (each commits on its own), hence the rehearsal.
A failure leaves the installed version, ledger and schema untouched and returns
`*installer.UpgradeError{Addon, FromVersion, Version, Phase, Preflight, Err}`
with `Phase` in `fetch | validate | schema | migrations` (`fetch` is raised by
hosts via `NewUpgradeError`). `errors.Is/As` see through `Err`.
Limits: Postgres only (other dialects skip the rehearsal); host
`MaterializeModels` callbacks run on the rehearsal transaction, so they must
only touch the database through the `*gorm.DB` they receive.

## Runtime

A tolerated manifest runs: `dynamic.checkRequiresState` compares the record's
state against the declared list; a value that is not a stage simply never
matches, so the action is not eligible (`ErrInvalidState`). It never panics.
