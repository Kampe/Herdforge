sha: 7767a0b613d8449f36f4722cbd43356664d8f1bf
branch: herd/fac-631
task: FAC-631
reviewer: review-fac-631-r4-grok-7767a0b
reviewer-family: xai
builder-family: openai
verdict: PASS
reviewed-base: e08b2227821d642c24608a46312eb776aeb6b8ec
reviewed-head: 7767a0b613d8449f36f4722cbd43356664d8f1bf
retry-of: review-fac-631-r3-grok-7767a0b
---

## Findings

No findings.

## Verification

Exact tree at review time: `git rev-parse HEAD` = `7767a0b613d8449f36f4722cbd43356664d8f1bf`; `git symbolic-ref HEAD` failed (detached); `git status --porcelain=v1` empty. Immutable base `e08b2227821d642c24608a46312eb776aeb6b8ec` is an ancestor of that pin. Range is three commits and two files: empty FAC-106 anchor `323169d62`, ingest-tier write `0550681fc`, reviewed-base bind `7767a0b61`. Files: `./cmd/herd/reviewingest.go` and `./cmd/herd/reviewingest_tier_test.go`. `git diff --check e08b2227821d642c24608a46312eb776aeb6b8ec..7767a0b613d8449f36f4722cbd43356664d8f1bf` exit 0.

code-review-graph 2.3.7 status: built_at_commit = reviewed head, 16532 nodes / 1394 files. `detect-changes` against the immutable base: 2 files, overall risk 0.55. `Classify` callers include `runReviewIngest`; `diffStat` callers are `runReviewClassify` and `runReviewIngest`; `ReconcileLanded` production caller is `runHarvestVerifyLanded`, with the new ingest-to-reconcile test as the shipped coverage for this card. Graph `tests_for(runReviewIngest)` is empty because the new test drives the CLI binary rather than a direct function call; that is not an untested path.

Commands used the mise `go` shim, `go version go1.26.4 linux/amd64`:

- `go test ./cmd/herd -count=1 -timeout 180s -run TestReviewIngestRiskTierReachesReceiptReconcile -v` exit 0. Subtests PASS: `documentation` (R0), `control_plane` (R3), `reviewed_base_survives_origin_main_advance` (R3).
- `go test ./pkg/reviewledger -count=1 -timeout 120s -run 'TestRefusalNamesAMissingRiskTier|TestTier|TestTierEmptyWhenNotSet' -v` exit 0. Missing-tier admission still names the absent risk tier; `Ledger.Tier` returns the recorded value and stays empty when unset.
- `go test ./pkg/mergeadmit -count=1 -timeout 120s -run TestReconcileLanded -v` exit 0, including reduced-provenance landed reconcile.
- `go build ./...` exit 0.
- `go run ./scripts/hermeticity/` exit 0.

Non-vacuity: parent `0550681fc` classifies with `diffStat("origin/main", a.SHA)`. Head uses `riskBase := strings.TrimSpace(a.ReadBase)` and only falls back to `origin/main` when that field is empty, then writes `RecordOpts.Tier = string(riskTier)`. An independent temp git fixture matching the third table case produced reviewed-base paths `cmd/herd/fac631-risk.go` plus `docs/fac-631-followup.md` and a post-advance `origin/main` leftover of only the docs file. `go run ./cmd/herd review-classify --paths` on those two path sets returned inferred `R3` versus inferred `R0`. Restoring the parent classifier base would therefore ingest R0 for that candidate and fail `reviewed_base_survives_origin_main_advance`, which asserted R3 after a real `review-ingest` plus `ReconcileLanded`. `AdmitReduced` still skips a PASS whose launch record has no tier (`TestRefusalNamesAMissingRiskTier`), so deleting the shipped tier write is also non-vacuous.

Candidate range itself classifies R3 (`cmd/herd/` core orchestration; 2 paths, +187/-1). Reviewer family xai differs from builder family openai.

## Scope/readback

Read the entire `e08b2227821d642c24608a46312eb776aeb6b8ec..7767a0b613d8449f36f4722cbd43356664d8f1bf` range, including the empty anchor, the first ingest-tier write, and the reviewed-base repair. Inspected `runReviewIngest` around the classify/record handoff, `diffStat` three-dot numstat, artifact `reviewed-base` parse (`ReadBase`), `reviewledger.Record`/`Tier`/`AdmitReduced`, and `Gate.ReconcileLanded` requiring a non-empty admitted tier on the full path and copying `result.Tier` onto the reduced completion receipt. Fail-closed: a `diffStat` error refuses that artifact and increments refused. Empty/unknown classify scope still fails upward inside `classify.Classify`; this change does not invent a silent empty tier.

Residual, not a finding: artifacts that omit `reviewed-base` still classify against mutable `origin/main` by explicit legacy fallback. `CheckReviewedRange` is not invoked on this ingest path (separate range-coverage gate). RETIRED artifacts still execute classify before the retire rewrite; the computed tier is then discarded. None of those residuals is a regression against the FAC-631 ingest-to-reconcile contract on this pin.

Final reconfirm before this file: HEAD `7767a0b613d8449f36f4722cbd43356664d8f1bf`, detached, clean, no source or prior-artifact edits.
