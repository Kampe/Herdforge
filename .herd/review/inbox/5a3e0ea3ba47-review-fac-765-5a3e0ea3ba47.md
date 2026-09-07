sha: 5a3e0ea3ba47838b040d1ef29f331d97814e2bed
branch: unknown
task: FAC-765
reviewer: review-fac-765-5a3e0ea3ba47
reviewer-family: anthropic
builder-family: xai
verdict: FAIL
reviewed-base: 9737258c9ecd7a48da215ea729cceff3cbc1d011
reviewed-head: 5a3e0ea3ba47838b040d1ef29f331d97814e2bed
---
## Findings and risk

Reviewed range: whole range `origin/main..HEAD` (9737258c..5a3e0ea3), in the
leased pool worktree at `.herd/pool-fac765/pool-02`, HEAD detached at the
candidate with no local or remote branch ref naming this exact SHA
(`origin/herd/fac-765` points at 7853dde5, one commit earlier) — hence
`branch: unknown`, a genuine gap and not a placeholder I chose not to fill.

This is a whole-range review, not a slice-B delta: `5a3e0ea3` itself is an
empty (`--allow-empty`, zero files changed — confirmed via
`git diff-tree --no-commit-id --name-only -r 5a3e0ea3`) docs-pin commit whose
message declares `origin/main` as reviewed-base and asserts that 7853dde5's
prior independent Slice-B PASS is not whole-range coverage of host_ingest. I
independently re-derived that same base (`git rev-parse origin/main` =
9737258c) from the actual ref state rather than accepting the commit's or
packet's assertion at face value.

Four commits in range:
- `f88780fa` — host-labelled review-ledger ingest (`pkg/reviewledger/host_ingest.go`,
  `cmd/herd/review_host_ingest.go`). Prior individual FAIL
  (`.herd/review/inbox/f88780fa490c-review-fac-765-f88780fa490c.md`) on a
  vacuous `latestExact` test in `resolveHarvestCandidateWithReconstructionAt`.
- `58e32fe2` — fixes exactly that: merges the off-branch SHA into the target
  branch tip and reorders PASS timestamps so a naive timestamp-only `latest`
  would pick the wrong row, forcing `latestExact`'s branch-label comparison to
  actually do the selecting. I read the full diff
  (`cmd/herd/harvest_branch_scope_test.go`) and confirm this closes the prior
  BLOCKING finding non-vacuously: reverting the `latestExact` branch now would
  fail `TestHostLabelledHarvestSelectsExactBranchBoundPass`.
- `7853dde5` — independent phase contexts for migrate rollback
  (`pkg/deps/migrate_phase.go`, `pkg/deps/migrate.go`, `pkg/deps/gate.go`,
  `cmd/herd/main.go`): readback timeout is now distinguished as UNKNOWN
  (`readbackUnknown` via `provider.IsTimeout`), rollback runs on
  `context.WithoutCancel(op)` plus its own timeout budget instead of the
  already-expired request context, and a rollback that itself times out
  writes `JournalStatusRollbackPending` and returns `RollbackPendingError`.
  `pkg/deps/gate.go` (`ValidateLaunch`) and `cmd/herd/main.go` (`runApprove`)
  both call `deps.RefusePendingRollback("")` to fail closed on a pending
  journal. `pkg/deps/migrate_phase_test.go` exercises both the
  independent-rollback-context path and the rollback-timeout-leaves-pending
  path non-vacuously (asserts a *live* rollback context via
  `rollbackCtxLive`, asserts the journal file's on-disk status, asserts the
  pre-image is left unrestored and `RefusePendingRollback` refuses).
- `5a3e0ea3` — the empty docs-pin commit described above.

### 1. `RefusePendingRollback("")` checks a fixed default directory, not the journal directory the apply run actually used (BLOCKING)

`herd deps migrate apply` takes a user-settable `--journal` flag
(`cmd/herd/deps.go:255`, default `.herd/migrate-journal`) and passes it
through to `ApplyMigrationForRef`/`ApplyMigration`. When a rollback times out,
the pending journal is written under *that* directory (`pkg/deps/migrate.go`
`jPath := filepath.Join(journalDir, ...)`).

But both production call sites of the new fail-closed gate hard-code the
empty string:

- `pkg/deps/gate.go:96` — `RefusePendingRollback("")` inside `ValidateLaunch`
- `cmd/herd/main.go:3039` — `deps.RefusePendingRollback("")` inside
  `runApprove`

`RefusePendingRollback("")` resolves through `migrateJournalDir("")`
(`pkg/deps/migrate_phase.go`) to `HERD_MIGRATE_JOURNAL` or else the hard-coded
`DefaultMigrateJournalDir` (`.herd/migrate-journal`). Neither call site has
any way to know, or attempts to read, the `--journal` value an operator
actually passed to a prior `apply` invocation.

Concretely: an operator runs `herd deps migrate apply --journal
/some/other/dir`, a description write's rollback times out, and
`RollbackPendingError` is written to `/some/other/dir/apply-<ts>.json` with
`Status: "rollback_pending"`. A subsequent `herd deps dispatch` (via
`ValidateLaunch`) or `herd approve` (via `runApprove`) calls
`RefusePendingRollback("")`, which lists `.herd/migrate-journal` (or whatever
`HERD_MIGRATE_JOURNAL` happens to be in that process's environment), finds it
empty or missing, and returns `nil` — dispatch and approval proceed with an
unreconciled, unrestored task description sitting in `/some/other/dir`. This
is exactly the scenario the commit message says it prevents ("Rollback
timeout leaves rollback_pending and blocks dispatch/approval until the
journal is reconciled") and it is silent: no error, no log, `ValidateLaunch`
and `runApprove` both succeed.

The new test (`TestApplyMigrationRollbackTimeoutLeavesRollbackPending`) does
not catch this because it calls `RefusePendingRollback(journalDir)` with the
*same* `t.TempDir()` it passed to `ApplyMigrationForRef` — it never exercises
the production call sites' hard-coded `""`, so the mismatch between "where
the journal actually lands" and "where the gate actually looks" has zero
coverage anywhere in this range.

Correction: either (a) have `ValidateLaunch`/`runApprove` scan the same
directory the operator's `apply` run used (which requires persisting or
discovering that path — e.g. record the last-used journal dir, or always
force the default and remove the `--journal` flag's ability to diverge from
it), or (b) have `RefusePendingRollback` scan a small fixed set of known
journal roots rather than a single directory, or (c) at minimum make
`--journal` refuse non-default values so the flag cannot silently defeat the
gate it was built alongside.

## Tests run

No commands executed — this host has no `go` binary on PATH (`command -v go`
fails), matching the same limitation noted in the prior `f88780fa` review of
this range. The finding above is derived from static reading of
`pkg/deps/gate.go:96`, `cmd/herd/main.go:3039`, `cmd/herd/deps.go:255`, and
`pkg/deps/migrate_phase.go`'s `migrateJournalDir`/`RefusePendingRollback`, and
from tracing the one test that exercises `RefusePendingRollback` to confirm
it does not call it the way production does. No RED/GREEN mutation proof
exists in this review; per the pool-review contract I did not swap or revert
any tracked file in place to demonstrate it (and did not use it as a
"green" claim — a builder patch validating this finding would need its own
independent test run once a Go toolchain is available).

## Residual risk

Independent of the finding above: `58e32fe2` genuinely and non-vacuously
closes the prior `f88780fa` FAIL on `latestExact` branch-scoping — I traced
the fixture's merge/timestamp construction and confirm the old vacuous
selection can no longer explain the test's outcome. `host_ingest.go`'s own
append-only/non-rewrite guarantees (append-only ledger, `preservePrefix`,
`refuseTestDelta`, receipt/branch-reachability authentication) are unchanged
from the individually-passed `f88780fa` review and I found no new interaction
between the whole range's four commits that weakens them; the docs-pin
`5a3e0ea3` touches no code.

If the `RefusePendingRollback("")` gap above ships, the residual risk is that
the entire rollback-pending safety mechanism this range adds is bypassable
by any `apply` invocation using a non-default `--journal` path (or a process
environment where `HERD_MIGRATE_JOURNAL` differs from the one the apply run
saw), with no error surfaced anywhere — dispatch/approval will look
fail-closed in the default-journal-dir case tested here and silently fail
open in the non-default case that the flag exists to support.
