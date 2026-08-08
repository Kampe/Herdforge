# FAC-200 integration status: criterion still OPEN (reasoning superseded)

> **Updated after FAC-198 landed.** The original blocker below was "no code on
> main creates a hermetic verification container at all". That is no longer
> true: `pkg/verifier/hermetic_docker_runner.go` is on main and creates one at
> `hermetic_docker_runner.go:664`. The historical reasoning is kept beneath for
> provenance, but do not act on it — verify against main.

## Current state

**Criterion:** "Every Herdforge hermetic/containerized verification owns a
durable container lifecycle receipt ... register the container ID immediately
after create, before start or any later failure."

**Still not met, for a different reason than originally recorded.**
`pkg/containerlifecycle` now HAS production callers — `cmd/herd/containers.go`
uses `NewStore`, `Status` and `LabelFAC174Baseline` — so the package is no
longer dead. But the hermetic runner does not use it: it creates a container
and calls `Register` zero times. Verified on main:

```
$ grep -c Register pkg/verifier/hermetic_docker_runner.go
0
$ grep -n '"create"' pkg/verifier/hermetic_docker_runner.go
664:  args := []string{"create", "--pull", "never", ...}
```

So a hermetic verification container can be created and then orphaned by a
crash between create and cleanup, with no durable receipt naming it. Tracked as
FAC-231 rather than left as a stale BLOCKED note on a closed card.

## Historical record (superseded)

### Evidence

```
$ git show main:pkg/verifier/hermetic_docker_runner.go
fatal: path 'pkg/verifier/hermetic_docker_runner.go' does not exist in 'main'

$ git branch -a --list '*fac-198*'
  task/fac-198-hermetic-receipt-r1
  task/fac-198-hermetic-receipt-r2

$ git merge-base HEAD main
dbab4ef87742de6964146cac0e7073145d9f090c   # == main's own tip; no FAC-198 commits anywhere in this history

$ grep -rln '"docker create"\|"docker", "create"\|"docker", "run"\|exec.*"docker"' --include='*.go' . | grep -v _test.go | grep -v /containerlifecycle/
(no output)

$ grep -rl '"docker"' --include='*.go' . | grep -v /containerlifecycle/
pkg/activate/activate.go   # `docker compose` for standing product services — unrelated, not hermetic verification
```

No hermetic/containerized verification launch exists anywhere in this
branch's tree outside `task/fac-198-hermetic-receipt-r1`/`-r2`, which are
not ancestors of `herd/fac-200` or `main`.

### What IS delivered on this branch, and holds regardless of FAC-198

- `pkg/containerlifecycle`: the full durable-receipt store, atomic state
  machine, `EnsureCleanup` compensation path, and `Reconcile` sweep — all
  independently unit- and race-tested against a swappable docker exec
  seam, with no dependency on FAC-198's code.
- `herd containers reconcile [--apply]`: a real, production, currently
  runnable coordinator-recovery caller of `Reconcile` (this is not
  blocked — it works today against whatever receipts exist, which is
  legitimately zero until FAC-198 lands and starts writing them).
- `herd containers` / `herd containers --json`: real status reporting,
  plus the FAC-174 one-time reconciliation plan
  (`docs/fac-200-fac174-reconciliation-plan.md`).

### Integration checklist for whoever lands FAC-198

When `pkg/verifier/hermetic_docker_runner.go` (or its eventual
replacement) merges, wiring it to this package is meant to be small and
mechanical:

1. Construct one `*containerlifecycle.Store` per process (or share one),
   e.g. `containerlifecycle.NewStore(filepath.Join(root, ".herd", "container-lifecycle.db"))`.
2. Immediately after `docker create` returns a container ID — before
   `start`, before any error path — call
   `store.Register(containerlifecycle.Receipt{ContainerID: id, TaskRef: taskRef, Generation: leaseGeneration, ImageDigest: digest, ExpectedTerminalState: ""})`.
3. After `docker start` succeeds, call `store.MarkStarted(id)`.
4. Set up exactly ONE deferred call, right after step 2 (so it covers
   every path: success, failed test, timeout, cancellation, panic-via-
   defer):
   `defer func() { _ = containerlifecycle.EnsureCleanup(teardownCtx, store, id, outcome, containerlifecycle.DockerRemove, containerlifecycle.DockerAbsent) }()`
   — `teardownCtx` must be a context independent of the run's own
   (possibly already-cancelled) context, matching the runner's existing
   `teardownCtx` pattern. `outcome` is whatever short string the runner
   already has for how the run ended ("success", "test_failed",
   "timeout", "cancelled") — `EnsureCleanup` records it via
   `MarkAwaitingCleanup` before removing.
5. On coordinator startup/recovery, run
   `containerlifecycle.Reconcile(ctx, store, liveFn, containerlifecycle.DockerRemove, containerlifecycle.DockerAbsent)`
   (or invoke `herd containers reconcile --apply`) where `liveFn` is
   backed by the coordinator's real lease/session authority once one
   exists — `containerlifecycle.StaleGenerationLive` is the fallback if
   nothing more specific is available yet.

No other change to `pkg/containerlifecycle` should be needed for this
wiring; if one turns out to be, that's this package's bug to fix, not a
sign the design doesn't fit.
