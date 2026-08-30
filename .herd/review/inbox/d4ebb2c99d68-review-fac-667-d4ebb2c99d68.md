sha: d4ebb2c99d686024a6e516197770aa2334a427ed
branch: herd/fac-667
task: FAC-667
reviewer: review-fac-667-d4ebb2c99d68
reviewer-family: anthropic
builder-family: openai
verdict: PASS
reviewed-head: d4ebb2c99d686024a6e516197770aa2334a427ed
---

Scope: `pkg/dispatch/runstate_recovery.go` (+/- refactor of `receiptLiveLaunchLookup.HasLiveLaunch`), plus new tests in `pkg/dispatch/runstate_recovery_test.go` and `cmd/herd/envplan_test.go`. No prod code elsewhere touched. Risk tier R1 (bounded dispatch-recovery correctness fix; no auth/secrets/board-mutation surface).

Intent, verified against the code: this is the third commit in the FAC-667 arc (after 8b30c905d and the anchor commit). The prior `HasLiveLaunch` treated any accepted receipt lacking TabID/PaneID as an "incomplete identity" hard error, with no way to represent a legitimate accepted-but-not-yet-tabbed launch record. The new code introduces `receiptLaunchPhase` (preEdit vs postLaunch) via `classifyReceiptLaunchEvidence`: a pre-edit receipt must carry full provenance (CreatedAt, Branch, Provider, Model, BuilderFamily) with no tab/pane/session; a post-launch receipt must carry a complete, workspace-qualified tab/pane/session set (`workspaceFromHerdrIDs`), otherwise it is UNKNOWN and errors rather than silently matching.

Matching logic (verified by reading, not just trusting names): for each live Herdr agent, receipts are related by exact Name + `sameWorktree` (filepath.Clean-equal CWD). A pre-edit-phase relation is conservatively treated as an admitted match (fail toward "still live" when the receipt can't yet prove a specific tab/pane), a post-launch relation must match tab/pane/workspace/session exactly (`postLaunchIdentityMatches`). More than one distinct live identity matching → ambiguous → error, not a guess. This is materially safer than the prior version, which could silently return `false` (safe-to-recover) even when Name+Tab+Pane matched an unrelated identity, because it never checked worktree at all — `TestReceiptLiveLaunchLookupDoesNotConfuseSameNameWrongTaskWorktree` now pins that down explicitly.

One behavior change worth flagging for the ledger, not a defect: zero accepted receipts for the exact task ref now returns an error ("no accepted launch receipt proves exact task") instead of the old `false, nil`. That flips "no evidence" from an implicit approval to a fail-closed refusal, consistent with this repo's fail-closed invariant and with `TestReceiptLiveLaunchLookupRequiresExactTaskEvidenceAndHerdrRead`, which also asserts Herdr is never even queried in that case. `--recover-stale-run` is an explicit, operator-supplied, single-task CLI recovery path (`cmd/herd/envplan.go`, no bulk/automatic caller), so a stricter refusal here trades a rare manual-intervention case for eliminating a blind-approval path when the receipt ledger is empty/rotated/missing. Acceptable tradeoff for this call site; noting it so it's a documented decision rather than a silent behavior drift.

Verification performed (read-only, no source edits, no tracked-file swaps):
- `go build ./...` — clean.
- `gofmt -l` and `go vet` on all three touched files — clean, no output.
- `go test ./pkg/dispatch/... ./cmd/herd/...` — both packages pass (14.9s / 100.6s).
- `go test -race -run "TestReceiptLiveLaunchLookup|TestRecoverStaleRun" ./pkg/dispatch/... -v` — all 8 test functions (18 subtests) pass under the race detector, including `TestRecoverStaleRunWithPreEditReceiptHasOneConcurrentWinner`, which launches two concurrent `Recover` calls against the same stale run with a synchronized Herdr-list rendezvous and asserts exactly one success + one `runstate.ErrConcurrent`, that the unrelated row is untouched, and that Herdr was read exactly once per attempt (no double-read/leak).
- Confirmed idempotency and non-mutation properties directly in code: `TestRecoverStaleRunWithGenuinePreEditReceiptIsIdempotentAndPreservesEvidence` re-runs `Recover` and checks revision/UpdatedAt are unchanged on retry, the unrelated run's row and the launch-receipts file bytes are byte-identical before/after (append-only evidence guarantee actually enforced, not just asserted).
- Confirmed authority-failure paths (malformed ledger, partial post-launch identity, Herdr listing error, live identity present) all refuse via `err != nil` and leave both the target and unrelated runstate rows unmutated (`TestRecoverStaleRunReceiptAuthorityFailuresDoNotMutateRun`).
- Path handling uses `filepath.Clean` consistently for worktree comparison (`sameWorktree`, `exactHerdrAgentIdentity`) — no string-prefix or naive equality that could be spoofed by trailing slashes or `./` noise.
- No secrets, no shell-string construction, no board/lease mutation in this diff; `StaleRunRecovery.Recover`'s guard composition (claims lookup → HasLiveLaunch → CAS recovery) is unchanged in shape, only the launch-liveness proof got stricter.

reviewer-family independence: builder-family is `openai` per the launch record (prefilled, not re-derived here); this review runs under Claude/anthropic, satisfying the R1 cross-family requirement.

No blocking findings. Verdict: PASS.
