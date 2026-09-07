sha: 2a3a20d57ba7e17f923d0260ed60edfff3fe27f9
branch: fix/fac-652-direct
task: FAC-652
reviewer: review-fac-652-2a3a20d57ba7
reviewer-family: anthropic
builder-family: openai
verdict: PASS
reviewed-head: 2a3a20d57ba7e17f923d0260ed60edfff3fe27f9
---

# FAC-652 review evidence

Candidate is test-only: `cmd/herd/review_cli_test.go` (+37/-26), no production
code changed. It sits on `fix/fac-652-direct` directly on top of `81b17a56`
("fix: bind receipt approval to coordinator mint identity (FAC-652)"), which
this review also read for context but does not re-review (already merged
provenance on this branch, not part of this diff).

## What changed

`TestApproveBroker` was a single-case test exercising only the `approve` CLI
entrypoint through the stock coordinator broker (`HERD_FENCE_COORDINATOR=1`).
The candidate converts it to a table test over `{"approve", "board-done"}`.
The `board-done` case seeds the fake board's fixture status as `in-progress`
(via `fk.mu.Lock/Unlock`, consistent with the existing locking pattern used
elsewhere in this file, e.g. the fixture at line ~1360) instead of the default
`in-review`, matching the live FAC-756 recovery shape described in the commit
message: recovery closes a card via `board-done` from `in-progress`, not via
`approve` from `in-review`.

Both subtests require exactly one `fk.patches` mutation on the first call and
no additional mutation on replay. The replay assertion differs correctly per
entrypoint: `approve` replay tolerates the known `"no in-review card matches
FAC-1"` fence rejection (status has already moved off `in-review`), while
`board-done` replay must return no error at all — the candidate does not
special-case an error string for that arm, so a regression there fails loudly.

## Non-vacuity (code-path inspection, not a tracked-file swap)

`grep` over `cmd/herd/main.go` confirms `runBoardDone` and `runApprove` both
call the single shared `approveOne` (line ~3896), and the FAC-652 mint-identity
bind added in `81b17a56` lives inside `approveOne`, gated only on
`provider.UnwrapTaskProvider(tp).(*provider.KaneoProvider)` + `stack.Minter !=
nil` — not on which CLI subcommand invoked it. So the `board-done` subtest
genuinely exercises the same bind the `approve` subtest does; this is not two
copies of one assertion. I did not swap `cmd/herd/main.go` to its parent blob
to force a RED run — `.herd/prompts/reviewer.md`'s Safety section is explicit
("Never edit, commit, merge, rebase, push, remove worktrees, prune refs, or
mutate board state") for a read-only reviewer, and I judged the shared-callsite
proof above sufficient for a test-only, low-risk commit. Flagging this choice
explicitly rather than silently skipping the check.

## Verification run (read-only)

```
go test ./cmd/herd/... -run 'TestApproveBroker' -v         # PASS, both subtests
go test ./cmd/herd/... -run 'TestApproveBroker' -race -v   # PASS, no races
go vet ./cmd/herd/...                                       # clean
git status --porcelain                                      # clean before and after
```

`fakeKaneo.status` mutation in the new subtest setup is guarded by `fk.mu`,
matching the struct's existing concurrency contract (`mu sync.Mutex` guards
`status`/`patches`/etc., read from the fake HTTP handler goroutine).

## Risk tier

R1 (bounded, test-only; extends coverage of a security/consistency-relevant
receipt-mutation path but touches no production code in this diff).

## Findings

No findings.

## Residual risk

- I did not independently re-verify the `81b17a56` production fix itself in
  this review (out of scope for this diff, which only touches the test file);
  a prior PASS for the same candidate SHA already exists in this inbox
  (`2a3a20d57ba7-review-fac-652-2a3a20d57ba7-e2624fb531c84a55.md`, reviewer
  xai, pool-01) with a fuller production-side trace including a pre-fix RED
  run. This review corroborates that verdict from an independent family
  without repeating the tracked-file swap.
- I initially read the packet's `verdict-push` instruction as conflicting with
  `.herd/prompts/reviewer.md`'s "do not use repository `bin/herd-*`
  orchestration scripts." Reading `cmd/herd/verdictpush.go` resolved that:
  `verdict-push` (FAC-619/FAC-638) is a real subcommand of the compiled `herd`
  CLI itself, plumbing-only (`hash-object`/`mktree`/`commit-tree` to a
  per-verdict ref), never mutates the reviewer's working tree, and exists
  specifically to replace the broken git recipe the contract text predates.
  Reporting home through it as instructed.
- Did not reproduce the pre-fix RED run myself (see above); relying on the
  prior independent reviewer's RED/GREEN trace for that half of the evidence.
