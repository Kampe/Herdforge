# FAC-118 salvage ledger — orphan patch recovery

Disposition of the two orphan patches named by FAC-118. Baseline: `origin/main`
at `15642b0`. Retained set is **empty**: every recoverable hunk is already
upstream or is a strict ancestor of upstream. No code change is carried by this
card.

## 0. Named inputs do not exist

Both ticket inputs are absent:

- `.herd/salvage/fac-58-orphaned-work.patch`
- `.herd/salvage/fac-110-orphaned-work.patch`

`.herd/salvage/` exists in no worktree and in no tree in the object database —
verified over every ref plus every dangling commit and stash:

```sh
{ git for-each-ref --format='%(objectname)'
  git fsck --lost-found | awk '/dangling commit/ {print $3}'
  git stash list --format='%H'; } | sort -u |
while read c; do git ls-tree -r --name-only "$c" -- .herd/salvage; done
# no output over 394 commits
```

Classification below is therefore against the surviving orphan *lineage* (a live
branch for FAC-110, dangling commits for FAC-58's review-ledger component), not
against the patch files. Where no lineage survives, that is stated instead of
guessed.

## 1. fac-110-orphaned-work — preflight

Surviving lineage: branch `fac-110-clean` (also on `origin`), single commit
`96a9dff` "feat: worktree isolation guard — detect agent root-bleed (FAC-110)",
2 files / 3 hunks. Not an ancestor of `origin/main`; content landed separately
via the `herd/fac-110` merge.

| # | File | Hunk | Class | Evidence |
|---|------|------|-------|----------|
| 1 | `pkg/preflight/preflight.go` | `@@ -3,6 +3,7 @@` — add `os/exec` import | already shipped | blob identical |
| 2 | `pkg/preflight/preflight.go` | `@@ -56,3 +57,79 @@` — `CheckAgentStayedInWorktree`, `gitStatusPorcelain`, `isUnder`, `runCmd` | already shipped | blob identical |
| 3 | `pkg/preflight/worktree_isolation_test.go` | `@@ -0,0 +1,154 @@` — new test file | superseded | main's copy has the same cases plus `internal/testgit` hermetic git |

```sh
git rev-parse origin/main:pkg/preflight/preflight.go \
              fac-110-clean:pkg/preflight/preflight.go
# ffd3caee795b3741ea67dc5a0f6f0814faccea79  (twice)

git diff fac-110-clean:pkg/preflight/worktree_isolation_test.go \
         origin/main:pkg/preflight/worktree_isolation_test.go
# only: mustRun routes git through internal/testgit.Command; the per-invocation
# -c commit.gpgSign=false is dropped because testgit supplies it. Every test
# case body is unchanged — no assertion was narrowed or removed.
```

Replaying hunk 3 would *reduce* coverage quality (non-hermetic git). Rejected on
that basis.

## 2. fac-58-orphaned-work — park, reset-safe, review-ledger

No commit, branch, stash, or dangling object anywhere in this repository
references FAC-58. The patch's three subject areas are all present and
CLI-wired on `origin/main`:

| Area | On main | CLI |
|------|---------|-----|
| park | `pkg/park/` — `park.go`, `list.go`, `reap.go`, `audit.go`, `hygiene.go`, `park_test.go` | `cmd/herd/main.go` `case "park"` |
| reset-safe | `pkg/resetsafe/` — `reset.go`, `reset_test.go` | `cmd/herd/main.go` `case "reset-safe"` |
| review-ledger | `pkg/reviewledger/` — `ledger.go`, `operations.go`, `types.go`, `admission.go` + tests | `pkg/harvest/integration.go` calls `Ledger.Admit` |

The only surviving orphan lineage for any of the three is the review-ledger:
dangling commits `7c4408f`, `989dda0` (package) and `d3d951a` (harvest wiring).
That lineage is a strict ancestor of main — it exports nothing main lacks:

```sh
# normalized exported-symbol sets over ledger.go + operations.go + types.go
comm -23 orphan-symbols main-symbols   # empty
comm -13 orphan-symbols main-symbols   # 10 symbols, main-only
```

Class for the whole patch: **already shipped** for park and reset-safe (by
package + CLI presence, no lineage available to diff at hunk granularity);
**superseded** for review-ledger (ancestor of main, zero symbols lost).

Hunk-level classification of park and reset-safe is not possible and is not
claimed — the patch is unrecoverable. If the file resurfaces, re-run section 1's
blob-identity method against it.

## 3. Artifact preservation

The original `.patch` files cannot be preserved because they never reached this
repository. The durable equivalents are recorded here:

- FAC-110: branch `fac-110-clean` @ `96a9dff`, present on `origin` — durable.
- FAC-58 review-ledger: dangling `7c4408f`, `989dda0`, `d3d951a`. Unreferenced
  and therefore gc-eligible. They are fully superseded, so they are recorded
  rather than anchored; `git branch <name> <sha>` before a `git gc --prune` if
  a future card needs them.

## 4. Out of scope (informational)

`preflight.CheckAgentStayedInWorktree` is on main with zero production callers —
the FAC-110 guard shipped unwired. Wiring it is a behavior change with its own
blast radius and belongs to its own card, not to this recovery pass. Recorded,
not actioned.
