sha: 2a3a20d57ba7e17f923d0260ed60edfff3fe27f9
branch: review/fac-652-w4
task: FAC-652
reviewer: review-fac-652-2a3a20d57ba7-w4r2
reviewer-family: anthropic
builder-family: openai
verdict: PASS
reviewed-base: 81b17a5622851532431f4e08ddc1ef6417d84a96
reviewed-head: 2a3a20d57ba7e17f923d0260ed60edfff3fe27f9
---
## Findings and risk

Reviewed range: 81b17a56 .. 2a3a20d5 (one commit, test-only). Independently
re-derived: `git rev-parse HEAD~1` and `origin/fix/fac-652-direct~1` both
resolve to 81b17a5622851532431f4e08ddc1ef6417d84a96 (a commit object, not a
blob). This verdict is scoped to the follow-up on the coordinator-mint fix;
it does not relabel any prior local PASS on 81b17a56 as W4.

Change: `cmd/herd/review_cli_test.go` `TestApproveBroker` is converted to a
table over `{approve, board-done}` subtests. Each subtest builds its own
`approveFixture`, provisions the fence, strips `HERD_FENCE_ATOMIC_SERVER`
and forces `HERD_FENCE_COORDINATOR=1`, runs the entrypoint, requires exactly
one provider PATCH, replays the same entrypoint against the sealed receipt,
and requires the PATCH count to remain 1. The board-done subtest first
flips the fake board's status to `in-progress` under `fk.mu`, matching the
live FAC-756 shape (worker never projected In Review). The replay tolerance
for the "no in-review card matches" exit is now correctly restricted to the
`approve` entrypoint; board-done replay must exit 0.

Verified:
- `fakeKaneo` exposes `mu sync.Mutex` and `status string`; the lock is
  taken around the status write, consistent with the PATCH handler.
- `go vet ./cmd/herd/` clean.
- `go test ./cmd/herd/ -run TestApproveBroker -count=1 -v`: both
  subtests PASS (approve 1.01s, board-done 1.10s).
- `go test ./cmd/herd/ -run 'Approve|BoardDone|Receipt' -count=1`: ok.
- Working tree clean; no production source touched.

Risk: none to production paths. The test now exercises both receipt
reconciliation entrypoints through the stock coordinator broker and pins
idempotence across replay, closing the gap the commit message names.

Non-blocking note: the `t.Fatalf` message "receipt approval through
coordinator broker" is shared by both subtests; subtest names disambiguate
in output, so no change needed.
