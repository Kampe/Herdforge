sha: 7767a0b613d8449f36f4722cbd43356664d8f1bf
branch: herd/fac-631
task: FAC-631
reviewer: review-fac-631-r3-grok-7767a0b
reviewer-family: xai
builder-family: openai
verdict: PASS
reviewed-base: e08b2227821d642c24608a46312eb776aeb6b8ec
reviewed-head: 7767a0b613d8449f36f4722cbd43356664d8f1bf
retry-of: review-fac-631-r2-grok-7767a0b
---

## Findings

No unresolved findings. Fresh independent review of the exact three-commit/two-file range supports PASS.

The shipped ingest path now classifies the candidate before writing the durable record row, using the same `diffStat` three-dot range and `classify.Classify` policy as `review-classify`. When the artifact declares `reviewed-base`, that SHA is the classifier base; `origin/main` remains only the documented fallback for older artifacts that declare none. The record row's `Tier` is what `reviewledger.Tier` / `AdmitReduced` require, and reduced `ReconcileLanded` copies that value onto the completion receipt. Fail-closed: a `diffStat` error refuses the artifact rather than recording an empty tier.

The end-to-end test is non-vacuous for the mutability bug the second commit left and the third commit closed. The `reviewed_base_survives_origin_main_advance` case plants control-plane then docs commits, advances `origin/main` onto the first commit, asserts that the mutable remainder classifies as a different (lower) tier than the declared reviewed range, then requires both the ingested record and the reconcile receipt to keep the reviewed-range R3. Independent classification of those same path sets produced `reviewed=R3 mutable=R0 docs=R0 control=R3`. Deleting the `RecordOpts.Tier` write, or classifying against live `origin/main` after it advanced, would fail that subtest.

Residual, not a defect in this candidate: `ValidatePassDiff` still diffs `origin/main...sha` (pre-existing empty-PASS gate), and `CheckReviewedRange` remains unwired. Neither is introduced by this range, and the advance fixture still has a non-empty remainder against `origin/main`, so ingest-to-reconcile for the claimed FAC-631 behavior is intact.

## Verification

Worktree at review start and finish: detached HEAD, porcelain empty, `HEAD=7767a0b613d8449f36f4722cbd43356664d8f1bf`.

Range readback vs immutable base `e08b2227821d642c24608a46312eb776aeb6b8ec`:

- `323169d6258525034f61566967e4d94aa45290b7` chore: anchor FAC-631 worktree (FAC-106 reap-safe) — empty tree change
- `0550681fc4897a345719b79e03f66b09cba93984` fix: record ingest risk tier for reconciliation FAC-631
- `7767a0b613d8449f36f4722cbd43356664d8f1bf` fix: bind ingest tier to reviewed base FAC-631

`git diff --name-status e08b2227821d642c24608a46312eb776aeb6b8ec..7767a0b613d8449f36f4722cbd43356664d8f1bf` → `M cmd/herd/reviewingest.go`, `A cmd/herd/reviewingest_tier_test.go`.

Commands, all from the candidate worktree with the specified mise Go shim (`go version go1.26.4 linux/amd64`):

- `git diff --check e08b2227821d642c24608a46312eb776aeb6b8ec 7767a0b613d8449f36f4722cbd43356664d8f1bf` → exit 0
- `git diff --check` (worktree) → exit 0
- `go test ./cmd/herd -count=1 -timeout 8m -run TestReviewIngestRiskTierReachesReceiptReconcile -v` → PASS, all three subtests (`documentation`, `control_plane`, `reviewed_base_survives_origin_main_advance`), `ok github.com/Kampe/Herdforge/cmd/herd` 1.136s, exit 0
- `go test ./pkg/reviewledger -count=1 -timeout 5m` → `ok github.com/Kampe/Herdforge/pkg/reviewledger` 0.142s, exit 0
- `go build ./...` → exit 0
- `go run ./scripts/hermeticity/` → exit 0 (no findings)
- Independent non-vacuity: `classify.Classify` on the fixture path sets → `reviewed=R3 mutable=R0 docs=R0 control=R3`, and `reviewed == mutable` is rejected; exit 0

code-review-graph 2.3.7 status at this SHA: 16532 nodes / 1394 files, `built_at_commit=7767a0b613d8449f36f4722cbd43356664d8f1bf`. `detect-changes` vs the reviewed base: 2 files, 2 changed functions/classes, graph risk 0.55; the reported `Untested: runReviewIngest` gap is a linking miss — `runReviewIngest` now calls `Classify`, and `TestReviewIngestRiskTierReachesReceiptReconcile` drives the shipped `review-ingest` binary through ledger record + `ReconcileLanded`.

## Scope/readback

Reviewed only this candidate against the stated base. Did not edit source, Git, Kaneo, receipts, ledgers, lifecycle state, or either prior refused artifact.

Read: `cmd/herd/reviewingest.go` (`runReviewIngest` classify/record write), `cmd/herd/reviewingest_tier_test.go`, `cmd/herd/reviewclassify.go` (`diffStat` three-dot `base...target`), `pkg/classify` default policy (`cmd/herd/` → R3, `docs/*.md` → R0), `pkg/reviewingest` parse of `reviewed-base` into `ReadBase` plus `VerificationDigest`, `pkg/reviewledger` `Record`/`Tier`/`AdmitReduced` missing-tier refusal, `pkg/mergeadmit` reduced `ReconcileLanded` receipt `RiskTier`. Intermediate commit `0550681fc489` still classified against `origin/main`; HEAD `7767a0b613d8` binds `a.ReadBase` first. Worktree remained clean at `7767a0b613d8449f36f4722cbd43356664d8f1bf` after verification.
