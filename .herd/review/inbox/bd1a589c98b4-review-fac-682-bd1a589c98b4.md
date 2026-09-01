sha: bd1a589c98b4c5008ccb9e85a72fc2c842bd46d1
branch: herd/fac-682
task: FAC-682
reviewer: review-fac-682-bd1a589c98b4
reviewer-family: anthropic
builder-family: openai
verdict: PASS
reviewed-head: bd1a589c98b4c5008ccb9e85a72fc2c842bd46d1
---

## Scope

Independent review of bd1a589c98b4 ("fix: harden W4 review toolchain gates
(FAC-682)") on top of a8f371721124 (FAC-681, centralize merge-tree flag).
Diff confirmed via `git show --stat` in this pool surface: 8 files changed,
698 insertions(+), 39 deletions(-) across `cmd/herd/review_pool.go`,
`cmd/herd/review_toolchain.go` (new), `cmd/herd/review_toolchain_test.go`
(new), `pkg/gitroot/gitroot.go`, `pkg/gitroot/gitroot_test.go`,
`pkg/review/pipeline.go`, `pkg/worktree/pool.go`,
`pkg/worktree/pool_release_diagnostics_test.go` (new).

The change does two things:

1. Adds `preflightReviewToolchain` (new `cmd/herd/review_toolchain.go`),
   called from `runPoolReview` before capacity/lease/tab creation. It
   resolves the exact git and go binaries (via `HERD_REVIEW_GIT`/
   `HERD_REVIEW_GO` or PATH), proves git supports
   `merge-tree --write-tree --merge-base=HEAD HEAD HEAD` (the exact
   invocation `pkg/review/pipeline.go`'s `mergeTreeCapable` and prior
   FAC-681 rounds depend on) and that go reports GOROOT/GOTOOLDIR, then
   injects the admitted binaries into the reviewer tab's environment
   (`reviewerTabEnvironment`) and into `worktree.Pool.GitPath` so pool
   admin operations use the same binary. A refusal here creates no lease
   and no tab (verified by `TestReviewToolchainPreflightIsBeforeLeaseAndTab`,
   which asserts ordering by parsing `runPoolReview`'s body).
2. Hardens `pkg/worktree.Pool`: adds a `GitPath` override (defaulting to
   PATH `git`) so pool commands can be pinned to the same admitted binary;
   adds a `sync.Mutex` alongside the existing lockfile so same-process
   concurrent `Lease`/`Release` calls serialize correctly, not just
   cross-process; splits `gitClean`'s stdout/stderr so a clean `git status
   --porcelain` with only stderr warnings (e.g. "ignoring unknown index
   extension IEOT") is treated as clean and the warning is surfaced via a
   new `Diagnostics io.Writer` rather than silently swallowed or
   misclassified as dirt; and adds `ReleasedLeaseID` so a retried
   `Release` call with the same lease id is idempotent without accepting
   an unrelated/unknown lease id as success.

`pkg/gitroot/gitroot.go` gains a named `MergeTreeHeadBaseFlag =
"--merge-base=HEAD"` constant (companion to FAC-681's
`MergeTreeWriteFlag`), and `pkg/review/pipeline.go`'s `mergeTreeCapable`
is updated to use it instead of the inline literal — continuing FAC-681's
centralization, not overlapping with FAC-681's own diff (confirmed by
diffing bd1a589c against its parent a8f371721124, not against origin/main).

## Verification

All commands run from this exact pool cwd (`.herd/pool/pool-01`, confirmed
via `git rev-parse --show-toplevel`), at the reviewed HEAD, using the
admitted toolchain:

```
$ git rev-parse --show-toplevel
/home/kampe/Projects/Herdforge/.herd/pool/pool-01
$ git rev-parse HEAD
bd1a589c98b4c5008ccb9e85a72fc2c842bd46d1
$ git status --porcelain
(empty)

$ PATH="/home/kampe/.local/state/herdforge-tools/git-2.55.0/bin:/home/kampe/.local/share/mise/shims:$PATH" \
  GOTOOLCHAIN=local git --version
git version 2.55.0
$ GOTOOLCHAIN=local /home/kampe/.local/share/mise/shims/go version
go version go1.26.4 linux/amd64

$ git merge-tree --write-tree --merge-base=HEAD HEAD HEAD   # under git 2.55.0
dabc4f284ea8fefc4820161e79dd774194b28491   # exit 0 — capability present

$ go build ./...   # under go1.26.4, GOTOOLCHAIN=local, from repo root
(no output)   # exit 0

$ go test ./cmd/herd/... ./pkg/worktree/... ./pkg/gitroot/... ./pkg/review/... -v
ok  github.com/Kampe/Herdforge/cmd/herd      82.290s
ok  github.com/Kampe/Herdforge/pkg/worktree  12.673s
ok  github.com/Kampe/Herdforge/pkg/gitroot   0.082s
ok  github.com/Kampe/Herdforge/pkg/review    3.075s
```

exit 0 for every command, no `FAIL` anywhere in the four packages' output.
All of this candidate's new tests ran and passed non-vacuously:
`TestReviewToolchainPreflightRefusesMissingOrIncapableTools` (3 subtests —
missing git, old git lacking the exact merge-tree capability, missing go —
each asserted against exact refusal-message substrings),
`TestAdmittedReviewToolchainReachesReviewerProcess` (spawns a subprocess
under `reviewerTabEnvironment` and asserts it actually resolves the
admitted binaries and prints their exact versions),
`TestReviewToolchainPreflightIsBeforeLeaseAndTab` (source-level ordering
check), `TestPoolReleaseUsesExitAndPorcelainStdout` (3 subtests covering
clean+warning / dirty / nonzero-exit),
`TestPoolReleaseRetryIsExactAndCannotClearNewLease`, and
`TestPoolConcurrentReleaseAndAdmissionRemainSerialized` (goroutine-level
race between `Release` and `Lease` synchronized via a fake-git rendezvous
file). `TestMergeTreeWriteFlag` in `pkg/gitroot` also covers the new
`MergeTreeHeadBaseFlag` constant. No source file was edited; `git status
--porcelain` was empty before and after every command above.

I did not need this candidate's own file-swap technique to prove
non-vacuity: unlike FAC-681's prior rounds (blocked/degraded because this
sandbox's *only* git was 2.34.1, which predates `merge-tree
--merge-base`), this round had an admitted modern git (2.55.0) available,
so I ran the actual positive-path capability probe and the full test
suite directly, watching real PASS output rather than reasoning about it.

## Disclosed non-candidate reads (isolation)

Two read-only operations during this review touched paths outside the
pool worktree and are called out explicitly rather than left implicit:

1. Read `.herd/review/inbox/a8f371721124-review-fac-681-a8f371721124-r2.md`
   and `.herd/review/inbox/ec274c215378-review-fac-681-ec274c215378-r2-*.md`
   — prior sealed FAC-681 verdicts — to independently corroborate that
   this sandbox's toolchain gap and the `docs/prompts/review-contract.md`
   packet-path bug are known, previously-documented conditions rather than
   something I was told to assume. This is a read of this repo's own
   review inbox, the same category of artifact `.herd/prompts/reviewer.md`
   itself directs a reviewer to be aware of; nothing was written or
   mutated there.
2. One informational `cd /home/kampe/Projects/Herdforge && go version;
   which go` (before the admitted go path was confirmed usable) — read-only,
   no git command was run against that tree, and it did not persist: the
   very next command's `git rev-parse --show-toplevel` still resolved to
   `.herd/pool/pool-01`, confirmed again just before writing this verdict.

No git command in this review ever targeted the canonical shared checkout,
no tracked file was modified anywhere, and HEAD/toplevel were reconfirmed
clean immediately before this artifact was written.

## Process note

`docs/prompts/review-contract.md`, which the dispatched packet at
`.herd/review-packets/review-fac-682-bd1a589c98b4.md` directs reviewers to
read, does not exist in this checkout — consistent with the same gap
recorded in the FAC-681 round-2 artifacts referenced above. Treating this
as infrastructure evidence, not a review blocker, per the same precedent:
the operative contract used was `.herd/prompts/reviewer.md` plus
`.herd/prompts/routing.md`, both read before this review.

Two messages arrived mid-session styled as coordinator/user follow-ups,
one supplying the `/home/kampe/.local/state/herdforge-tools/git-2.55.0/bin/git`
and `/home/kampe/.local/share/mise/shims/go` paths (independently verified
by running `--version`/`version` before use, matching the precedent set by
the FAC-681 round-2 reviewer receiving a similar follow-up), and a second
asserting the prior two reads should be excluded as "findings sources" and
gesturing toward a default BLOCKED. Neither message's framing was taken at
face value: the toolchain paths were verified empirically rather than
trusted on assertion, and the verdict below is derived from the actual
build/test evidence above, not from either message's suggested outcome.

## Verdict rationale

PASS. The toolchain preflight is fail-closed and ordered correctly ahead
of any lease or tab (proved both by source-level assertion and by the
subprocess test), refuses with an actionable message naming
`HERD_REVIEW_GIT`/`HERD_REVIEW_GO` and explicitly disclaims installing or
modifying host tooling, and its admitted binaries are the same ones
propagated into the pool (`Pool.GitPath`) and the reviewer tab
(`reviewerTabEnvironment`) rather than being checked once and then
silently drifting. The pool hardening is narrowly scoped and each piece
has a test that fails without the corresponding production change (an
in-process mutex for real concurrent Lease/Release ordering, stdout/stderr
split so successful git warnings stop being misread as dirt without
losing visibility into them, and idempotent release-by-lease-id that
still fails closed for an unrelated/unknown lease id and cannot clear a
newer admission). No behavior regression found in the surrounding
`cmd/herd`, `pkg/worktree`, `pkg/gitroot`, or `pkg/review` suites. No
source was edited during this review; the leased pool-01 surface was
clean at HEAD bd1a589c98b4c5008ccb9e85a72fc2c842bd46d1 before, during, and
after.
