---
artifact_kind: review
task: FAC-631
task-id: rm82b4ojm6cpb8ls3id0lp95
project-id: b939c5jzixruza3vvywrg1hs
branch: herd/fac-631
candidate: 7767a0b613d8449f36f4722cbd43356664d8f1bf
reviewed-base: e08b2227821d642c24608a46312eb776aeb6b8ec
reviewed-head: 7767a0b613d8449f36f4722cbd43356664d8f1bf
builder-family: openai
reviewer-family: xai
reviewer: review-fac-631-r1-grok-7767a0b
verdict: PASS
---

Independent xAI/Grok review of candidate `7767a0b613d8449f36f4722cbd43356664d8f1bf` against immutable base `e08b2227821d642c24608a46312eb776aeb6b8ec`. Builder family is OpenAI. Detached worktree was clean at the candidate SHA for the entire review.

## Findings

No findings.

## Verification

Inspected the entire three-commit range, not only the tip:

- `323169d6258525034f61566967e4d94aa45290b7` — empty FAC-106 anchor; no source change.
- `0550681fc4897a345719b79e03f66b09cba93984` — records ingest risk tier, but classified with `diffStat("origin/main", a.SHA)`.
- `7767a0b613d8449f36f4722cbd43356664d8f1bf` — binds classification to artifact `reviewed-base` (`a.ReadBase`), falling back to `origin/main` only when that field is empty.

Tree diff vs the immutable base is exactly:

- `./cmd/herd/reviewingest.go`
- `./cmd/herd/reviewingest_tier_test.go`

Shipped ingest now classifies with `classify.Classify` (same deterministic floor as `review-classify`) over `diffStat(riskBase, a.SHA)` and writes `RecordOpts.Tier`. `diffStat` failure refuses the artifact (`classify candidate risk`) rather than recording an empty tier. `classify.Classify` always returns a non-empty R0–R3 value; empty/unknown scope fails upward to R2. `ensureRecord` remains SHA+reviewer idempotent and does not backfill historical empty-tier rows. Admission still refuses a genuinely missing tier by name (`./pkg/reviewledger/admission.go` and `TestRefusalNamesAMissingRiskTier`).

Focused commands, using the required mise-shim `go` (go1.26.4 linux/amd64):

- `git diff --check <base> <candidate>` — exit 0
- `go test -count=1 ./cmd/herd/ -run 'TestReviewIngest'` — exit 0
- `go test -count=1 -v ./cmd/herd/ -run 'TestReviewIngestRiskTierReachesReceiptReconcile'` — exit 0; all three subtests PASS (`documentation` R0, `control_plane` R3, `reviewed_base_survives_origin_main_advance` R3)
- `go test -count=1 ./pkg/reviewledger/ -run 'TestRefusalNamesAMissingRiskTier|TestAdmit'` — exit 0
- `go test -count=1 ./pkg/classify/` — exit 0
- `go build ./...` — exit 0
- `go run ./scripts/hermeticity/` — exit 0 (no findings)

Non-vacuity / mutation (throwaway copies only; worktree source untouched):

- Deleting the classify block and `RecordOpts.Tier` write turned `TestReviewIngestRiskTierReachesReceiptReconcile/documentation` RED on the shipped assertion: `ingested record tier = "", want "R0"`.
- Forcing `riskBase := "origin/main"` turned `reviewed_base_survives_origin_main_advance` RED on the same assertion: `ingested record tier = "R0", want "R3"`.

Live FAC-631 acceptance criteria:

- Allowlisted launch family + independent PASS + verification digest is admitted through the shipped ingest path and `ReconcileLanded` reduced receipt path (new test drives both).
- A candidate genuinely missing a tier is still refused with a message containing `risk tier` (existing ledger tests, re-run here; no-backfill preserved).
- Existing tier semantics are unchanged for an already-recorded SHA+reviewer pair (`ensureRecord` no-ops).
- Deleting the tier write turns the new regression RED with `-count=1`.

Graph (rebuilt at the candidate SHA; 16532 nodes / 1394 files): `detect-changes` risk 0.55 on the two scoped files; `tests_for runReviewIngest` reported a gap because the new coverage execs the binary rather than calling the function. Direct read of the test confirms it drives `herd review-ingest` then `ReconcileLanded`.

Residual risks, not findings: `a.ReadBase` is used as a git revision without a 40-hex commit-id gate (invalid refs already fail closed; empty remains the documented legacy fallback). `CheckReviewedRange` is still unwired, which is FAC-704 and out of this card's scope.

## Scope/readback

Intended change: record a deterministic risk tier during `review-ingest`, using the artifact's immutable `reviewed-base` instead of mutable `origin/main`, so a valid independent PASS can later satisfy receipt reconciliation.

Read back: that is exactly what landed. Classification is bound to `a.ReadBase` when present, uses the same `diffStat` + `classify.Classify` floor as review-classify, writes the tier onto the durable record row, refuses when the range cannot be classified, and the new test proves R0, R3, mutable-`origin/main` survival, ingest→ledger, and receipt reconcile. Scope stayed inside the two named files. No source, git, Kaneo, receipt, ledger, or lifecycle state was mutated by this review.
