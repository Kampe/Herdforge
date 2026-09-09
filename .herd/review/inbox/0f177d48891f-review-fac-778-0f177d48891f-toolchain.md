sha: 0f177d48891f6492bfbbb4f52c1c23fd70f2b71c
branch: recovery/fac-778-standing-publication
task: FAC-778
reviewer: review-fac-778-0f177d48891f
reviewer-family: anthropic
builder-family: openai
verdict: PASS
reviewed-base: 4180d2b79149bf6fa9041d65be9732e7e8605bfd
reviewed-head: 0f177d48891f6492bfbbb4f52c1c23fd70f2b71c
---

## Toolchain

Prior artifact (`0f177d48891f-review-fac-778-0f177d48891f.md`, reviewer `w4`,
verdict BLOCKED) is retained untouched; this is a separate, new verdict under
my own reviewer identity, admitted with the coordinator-identified existing
host toolchain:

```
$ $HOME/.local/share/mise/installs/go/1.26.4/bin/go version
go version go1.26.4 linux/amd64
```

No install performed, no shell config/PATH modified — the absolute binary
path was invoked directly for every command below.

## Scope

R3 independent review of the exact range
`4180d2b79149bf6fa9041d65be9732e7e8605bfd..0f177d48891f6492bfbbb4f52c1c23fd70f2b71c`,
both commits reviewed as one unit:

- `5fa955d1` fix: reconcile standing branch publication authority
- `0f177d48` fix: refuse publication without protected branch identity

Files touched: `cmd/herd/main.go`, `pkg/goalguard/goalguard.go`,
`pkg/standing/standing.go`, `pkg/standing/standing_test.go`.

## Tests run

Confirmed HEAD before testing:

```
$ git rev-parse HEAD
0f177d48891f6492bfbbb4f52c1c23fd70f2b71c
```

Fresh (uncached) run of every touched package, at the candidate HEAD:

```
$ $HOME/.local/share/mise/installs/go/1.26.4/bin/go test ./pkg/standing/... ./pkg/goalguard/... -v -count=1
...
--- PASS (all subtests) TestRaiseTreatsUnauthorizedSameNamedAgentAsMissing, TestIndexAgentsFailsClosedOnDuplicateAuthorizedIdentity,
    TestStatusReportsUnraiseableWhenHarnessMissing, TestStatusReportsMissingWhenHarnessPresent, TestShutdownTouchesOnlyStanding,
    TestLegacyStandingNamesAreAdopted(+2 subtests), TestRaiseAdoptsLegacyStandingNameWithoutDuplicate,
    TestWorkspaceResolutionFailureBlocks, TestCWDMatchesAssignedWorktree, TestNameHeldGateIsNonVacuous,
    TestStatusUsesDurableLoopModeWhenAgentReportsNone(+4 subtests),
    TestLanePushStandingGrantMatchesRenderedAndDurableAuthority,
    TestLanePushStandingGrantFailsClosedForNonWritersAndProtectedBranches (incl. all unknown-default and known-default subtests)
PASS
ok  	github.com/Kampe/Herdforge/pkg/standing	0.033s

--- PASS (all subtests) TestEvaluateContinuesUntilBoundThenStops, TestEvaluateStopsForEveryTerminalCondition(+4 subtests),
    TestEvaluateStopsAfterExpiry, TestRestartReloadsConsumedBudget, TestCorruptAndStaleEvidenceFailClosed,
    TestContradictoryGoalCannotBePersisted, TestUnboundedGoalContinuesUntilStopCondition
PASS
ok  	github.com/Kampe/Herdforge/pkg/goalguard	0.506s
```

Full-repo affected-package sweep, including `cmd/herd`:

```
$ $HOME/.local/share/mise/installs/go/1.26.4/bin/go test ./pkg/standing/... ./pkg/goalguard/... ./cmd/herd/... -v
ok  	github.com/Kampe/Herdforge/pkg/standing	(cached)
ok  	github.com/Kampe/Herdforge/pkg/goalguard	(cached)
--- FAIL: TestObserveVerifyLandedSquashPreservesCandidate (0.09s)
    landed_observation_test.go:56: squash observation: equivalent-patch proof failed: ordered proof: candidate
    commit 1 (patch 71396b75c02f) has no patch-equivalent counterpart on the landed history; combined proof:
    no candidate-sized landed stack with the reviewed tip patch reproduces the combined reviewed change
FAIL	github.com/Kampe/Herdforge/cmd/herd	103.121s
```

`TestObserveVerifyLandedSquashPreservesCandidate` is the only failure in the
full sweep. It lives in `landed_observation_test.go`, exercises squash/patch
equivalence against landed history, and has no dependency on
`standing.go`/`goalguard.go`/the `main.go` wiring touched by this range.
Verified pre-existing rather than a regression introduced by this candidate
by temporarily checking out the base commit in this leased pool worktree and
re-running it in isolation, then restoring the exact candidate pin:

```
$ git rev-parse --show-toplevel
/home/kampe/Projects/Herdforge/.herd/pool-fac778-standing/pool-01
$ git checkout 4180d2b79149bf6fa9041d65be9732e7e8605bfd
$ $HOME/.local/share/mise/installs/go/1.26.4/bin/go test ./cmd/herd/... -run TestObserveVerifyLandedSquashPreservesCandidate -v
--- FAIL: TestObserveVerifyLandedSquashPreservesCandidate (0.10s)
    (identical failure text, at the base commit)
$ git checkout 0f177d48891f6492bfbbb4f52c1c23fd70f2b71c
$ git rev-parse HEAD
0f177d48891f6492bfbbb4f52c1c23fd70f2b71c
$ git status --porcelain
(clean; the untracked test-cache file the run left behind was removed)
```

Same failure, same message, at the unmodified base commit — pre-existing,
unrelated to this range. Not treated as a blocking finding for FAC-778.

Compiling negative mutation, to prove the new fail-closed tests are
non-vacuous (done in this leased pool worktree only, restored before
continuing):

```
$ git show 5fa955d1:pkg/standing/standing.go > <scratch>/standing_pre_fix.go
$ cp pkg/standing/standing.go <scratch>/standing_post_fix_backup.go
$ cp <scratch>/standing_pre_fix.go pkg/standing/standing.go
$ git status --porcelain
 M pkg/standing/standing.go
$ $HOME/.local/share/mise/installs/go/1.26.4/bin/go test ./pkg/standing/... -run TestLanePushStandingGrantFailsClosedForNonWritersAndProtectedBranches -v
--- FAIL: TestLanePushStandingGrantFailsClosedForNonWritersAndProtectedBranches (0.00s)
    standing_test.go:294: unknown default must refuse "master" publication: {... AllowedBranch:master
    ForbiddenActions:[open or update a PR merge self-review change standing policy or authority] ...}
$ cp <scratch>/standing_post_fix_backup.go pkg/standing/standing.go
$ git status --porcelain
(clean)
$ git rev-parse HEAD
0f177d48891f6492bfbbb4f52c1c23fd70f2b71c
```

Against the pre-fix (`5fa955d1`) code, an unknown default branch (`""`) with
a lane-push policy granted `AllowedBranch:master` — the exact defect this
range fixes, and the exact new test catches it (compiles, fails pre-fix,
passes post-fix). The test is not vacuous.

`go vet` over the touched packages, clean:

```
$ $HOME/.local/share/mise/installs/go/1.26.4/bin/go vet ./pkg/standing/... ./pkg/goalguard/... ./cmd/herd/...
(no output, exit 0)
```

## Static review

- `5fa955d1` introduced `safePublicationBranch(branch, defaultBranch)` gating
  lane-push branch-publication authority. Its guard was
  `defaultBranch != "" && branch == defaultBranch` — when `defaultBranch` is
  empty (unknown), that comparison is always false, so an unknown protected
  branch did not block publication; only the literal string `"main"` was
  hard-refused. A lane could be granted push authority to `master` or
  `trunk` when the repository's actual default branch was unknown to the
  caller. Reproduced live above via the negative mutation.
- `0f177d48` closes exactly that gap by adding `defaultBranch == ""` as an
  unconditional refusal in `safePublicationBranch`, with a comment explaining
  why an unknown default cannot be assumed to be `main` (real repos may use
  `master`, `trunk`, `develop`, etc.). Minimal, correctly-scoped fix that does
  not touch any other branch of the gating logic.
- `standingEnvelope` (added in `5fa955d1`) resolves the lane's native
  worktree branch via `opts.WorktreeHead` only when the policy actually
  requests lane-push, and fails closed (returns an error, no envelope) if
  `WorktreeHead` is nil, errors, or `safePublicationBranch` rejects the
  resolved branch; `runRaise` propagates that as `OutcomeFailed` rather than
  silently downgrading to no-push.
- `canPublish` requires five conjunctive conditions (policy is `lane-push`,
  lane authority is `write`, lane has `git-write` capability, lane role is
  not `reviewer` case-insensitively, `safePublicationBranch` passes) —
  weakening any one fails closed to no-push, and `ForbiddenActions` always
  carries `"open or update a PR"`, `"merge"`, `"self-review"`,
  `"change standing policy or authority"` regardless of `canPublish`, so a
  lane-push grant never implies PR/merge/self-review authority.
- Rendered vs. durable consistency: `withContinuationGoalAndEnvelope` now
  takes the same resolved `envelope` persisted via `SetGoalWithAuthority`,
  fixing a pre-existing rendered/durable divergence where
  `withContinuationGoal` re-derived a policy-free envelope independently of
  what was just persisted. `TestLanePushStandingGrantMatchesRenderedAndDurableAuthority`
  (fresh PASS above) asserts rendered prompt text and durable
  `AllowedBranch`/`ForbiddenActions` agree.
- New/extended tests in `0f177d48` directly cover the fixed defect: unknown
  default branch (`""`) refuses publication for `master`, `trunk`, and an
  arbitrary feature branch alike; a known default of `main` still allows an
  assigned feature branch; a known default of `master` still refuses
  publishing `master` itself. This matches the task's requirement to cover
  both the new unknown-default refusal and the known-default positive case,
  and non-vacuity is now confirmed live (not just by static reading).
- No caller-supplied flag or config path was found that can force
  `canPublish` true independent of the five conjunctive conditions, and no
  path defaults an unknown protected branch to `main` — the opposite is now
  enforced and test-covered.

## Isolation / source-path

- `git rev-parse --show-toplevel` resolved to this leased pool worktree
  (`/home/kampe/Projects/Herdforge/.herd/pool-fac778-standing/pool-01`)
  throughout, never the canonical shared checkout.
- The only tracked-file swap performed (`pkg/standing/standing.go`, for the
  negative-mutation proof) was done and restored inside this pool worktree;
  confirmed clean (`git status --porcelain`) and back at the exact candidate
  pin (`git rev-parse HEAD` = `0f177d48891f6492bfbbb4f52c1c23fd70f2b71c`)
  before writing this artifact.
- No git write, mutation, or board/lease action was taken from this surface.

## Residual risk

None identified for this range. The one failing test in the full sweep is
pre-existing on the base commit and unrelated to the files this range
touches.

## Verdict

PASS.
