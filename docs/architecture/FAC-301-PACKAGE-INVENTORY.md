# FAC-301: Graph-Backed Package Inventory

## Objective

Maintain a durable, evidence-backed classification of every Go package in the
Herdforge module by reachability from the primary binary (`cmd/herd`). The
inventory prevents silent wiring regressions and tracks unwired packages
intentionally, without speculatively deleting designed-but-not-yet-integrated
code.

## Tooling

| Artifact | Purpose |
| :--- | :--- |
| `scripts/packageinventory/inventory.go` | Enumerates packages via `go list`, computes transitive reachability, classifies, and checks/generates baseline |
| `scripts/packageinventory/baseline.json` | Checked-in baseline inventory (regenerate with `--generate`) |
| `scripts/packageinventory/inventory_test.go` | Unit tests for classification, drift detection, and JSON round-trip |
| `make package-inventory` | CI guardrail target (runs `--check` against baseline) |

## Classifications

| Class | Meaning |
| :--- | :--- |
| `production` | Reachable from `cmd/herd` non-test build |
| `secondary` | A `main` package that is not the primary binary (e.g. `cmd/herd-hook-inventory`) |
| `test-helper` | Not production-reachable but imported by tests of production packages |
| `internal` | Under `internal/` — preserved test infrastructure, excluded from drift |
| `unwired` | Not wired into any binary; has own tests but no production importer |

## Drift Detection

The `--check` mode fails (exit 1) on two conditions:

1. **Regression**: a package that was `production` in the baseline is no longer
   production-reachable (broken wiring).
2. **Unintended growth**: a new `unwired` package appears that was not recorded
   in the baseline. Intentionally adding an unwired package requires updating
   the baseline with `--generate`.

Removed packages produce a warning (not a hard failure) since deletion is a
deliberate act, but the baseline should be regenerated to stay current.

## Regenerating the Baseline

```sh
go run ./scripts/packageinventory/ --generate scripts/packageinventory/baseline.json
```

Review the diff before committing. The baseline is the source of truth for CI
drift detection — an incorrect baseline silences the guardrail.

## Current Snapshot (as of this commit)

- **112** total packages
- **87** production
- **1** secondary (`cmd/herd-hook-inventory`)
- **1** internal (`internal/testgit`)
- **23** unwired

The 23 unwired packages are designed-but-not-yet-integrated. They have their
own test suites and are tracked in the baseline so their status is intentional.
Do not delete them speculatively; wire them into `cmd/herd` or update the
baseline if the design intent changes.

## CI Integration

The inventory check runs as part of `make lint` (via `make package-inventory`),
which is executed in the `gate` job of `.github/workflows/ci.yml`. A drift
failure fails the lint step, blocking the PR.
